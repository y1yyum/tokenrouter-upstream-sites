-- control_bridge.sql provides atomic ownership for plugin scheduling restrictions.
-- The plugin login can execute only reconcile and snapshot, not read credentials
-- or update provider rows directly. Manual and other host writes to schedulable
-- are retained even when they write false to an already disabled provider.
BEGIN;
CREATE SCHEMA IF NOT EXISTS upstream_site_control;
REVOKE ALL ON SCHEMA upstream_site_control FROM PUBLIC;
CREATE TABLE IF NOT EXISTS upstream_site_control.holds (
    provider_id bigint PRIMARY KEY,
    site_id bigint NOT NULL,
    original_schedulable boolean NOT NULL,
    manual_schedulable boolean NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    intervention_at timestamptz
);
CREATE TABLE IF NOT EXISTS upstream_site_control.events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider_id bigint NOT NULL,
    site_id bigint NOT NULL,
    kind text NOT NULL,
    manual_schedulable boolean NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- capture_switch runs while PostgreSQL already owns the provider row lock.
-- Same-value writes are meaningful: pressing Disable during a plugin pause
-- must remain a manual pause after recovery. Host error containment also wins.
CREATE OR REPLACE FUNCTION upstream_site_control.capture_switch()
RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE h upstream_site_control.holds%ROWTYPE;
BEGIN
    IF current_setting('upstream_site_control.writer',true) = 'reconcile' THEN
        RETURN NEW;
    END IF;
    SELECT * INTO h FROM upstream_site_control.holds WHERE provider_id=NEW.id FOR UPDATE;
    IF FOUND THEN
        UPDATE upstream_site_control.holds SET manual_schedulable=NEW.schedulable,
            intervention_at=clock_timestamp() WHERE provider_id=NEW.id;
        INSERT INTO upstream_site_control.events(provider_id,site_id,kind,manual_schedulable)
            VALUES(NEW.id,h.site_id,'external_switch',NEW.schedulable);
        NEW.schedulable := false;
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS upstream_site_control_switch ON public.providers;
CREATE TRIGGER upstream_site_control_switch BEFORE UPDATE OF schedulable ON public.providers
    FOR EACH ROW EXECUTE FUNCTION upstream_site_control.capture_switch();

-- reconcile derives targets exclusively from this plugin's committed KV records.
-- Provider rows are locked before ownership rows, matching capture_switch. Both
-- state and scheduler outbox changes commit together; no caller supplies SQL or
-- provider identifiers. Recovery changes no status, expiry, quota or health field.
CREATE OR REPLACE FUNCTION upstream_site_control.reconcile()
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    bindings jsonb;
    desired jsonb := '{}'::jsonb;
    item record;
    sid bigint;
    p public.providers%ROWTYPE;
    h upstream_site_control.holds%ROWTYPE;
    present boolean;
    changed boolean;
    stopped integer := 0;
    released integer := 0;
    old_writer text;
BEGIN
    PERFORM pg_advisory_xact_lock(710131101);
    -- A share lock prevents configuration or binding publication during this pass.
    PERFORM 1 FROM public.plugin_kv WHERE plugin_id='local.upstream-sites'
        AND collection IN ('sites','meta') ORDER BY collection,key FOR SHARE;
    SELECT value INTO bindings FROM public.plugin_kv WHERE plugin_id='local.upstream-sites'
        AND collection='meta' AND key='bindings';
    FOR item IN SELECT b.key,b.value FROM jsonb_each_text(coalesce(bindings,'{}'::jsonb)) b
        JOIN public.plugin_kv s ON s.plugin_id='local.upstream-sites' AND s.collection='sites'
            AND s.key=b.value
        WHERE coalesce(s.value#>>'{site,config,policy,mode}','off') <> 'off'
          AND NOT coalesce((s.value#>>'{site,archived}')::boolean,false)
          AND jsonb_typeof(s.value#>'{site,state,reasons}')='object'
          AND coalesce(s.value#>'{site,state,reasons}','{}'::jsonb) <> '{}'::jsonb
    LOOP
        IF item.key ~ '^[1-9][0-9]{0,17}$' AND item.value ~ '^[1-9][0-9]{0,17}$' THEN
            desired := desired || jsonb_build_object(item.key,item.value::bigint);
        END IF;
    END LOOP;
    old_writer := current_setting('upstream_site_control.writer',true);
    PERFORM set_config('upstream_site_control.writer','reconcile',true);
    FOR item IN SELECT id FROM (
        SELECT key::bigint AS id FROM jsonb_each(desired)
        UNION SELECT provider_id FROM upstream_site_control.holds
    ) targets ORDER BY id LOOP
        SELECT * INTO p FROM public.providers WHERE id=item.id FOR UPDATE;
        IF NOT FOUND THEN
            DELETE FROM upstream_site_control.holds WHERE provider_id=item.id;
            CONTINUE;
        END IF;
        SELECT * INTO h FROM upstream_site_control.holds WHERE provider_id=item.id FOR UPDATE;
        present := FOUND;
        sid := (desired->>item.id::text)::bigint;
        changed := false;
        IF sid IS NOT NULL AND p.deleted_at IS NULL THEN
            IF NOT present THEN
                INSERT INTO upstream_site_control.holds(provider_id,site_id,original_schedulable,manual_schedulable)
                    VALUES(p.id,sid,p.schedulable,p.schedulable);
                INSERT INTO upstream_site_control.events(provider_id,site_id,kind,manual_schedulable)
                    VALUES(p.id,sid,'suspend',p.schedulable);
                stopped := stopped+1;
            ELSE
                UPDATE upstream_site_control.holds SET site_id=sid WHERE provider_id=p.id;
            END IF;
            IF p.schedulable THEN
                UPDATE public.providers SET schedulable=false,updated_at=now() WHERE id=p.id;
                changed := true;
            END IF;
        ELSIF present THEN
            IF p.schedulable IS DISTINCT FROM h.manual_schedulable THEN
                UPDATE public.providers SET schedulable=h.manual_schedulable,updated_at=now() WHERE id=p.id;
                changed := true;
            END IF;
            INSERT INTO upstream_site_control.events(provider_id,site_id,kind,manual_schedulable)
                VALUES(p.id,h.site_id,'release',h.manual_schedulable);
            DELETE FROM upstream_site_control.holds WHERE provider_id=p.id;
            released := released+1;
        END IF;
        IF changed THEN
            INSERT INTO public.scheduler_outbox(event_type,provider_id,payload,created_at)
                VALUES('provider_changed',p.id,'{}'::jsonb,now());
        END IF;
    END LOOP;
    PERFORM set_config('upstream_site_control.writer',coalesce(old_writer,''),true);
    DELETE FROM upstream_site_control.events WHERE created_at<now()-interval '365 days';
    RETURN jsonb_build_object('suspended',stopped,'released',released,
        'held',(SELECT count(*) FROM upstream_site_control.holds));
END $$;

-- snapshot exposes ownership and audit facts, never host account credentials.
CREATE OR REPLACE FUNCTION upstream_site_control.snapshot()
RETURNS jsonb LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT jsonb_build_object('version',1,
        'trigger_ready',EXISTS(SELECT 1 FROM pg_trigger WHERE tgname='upstream_site_control_switch'
            AND tgrelid='public.providers'::regclass AND tgenabled='O'),
        'holds',coalesce((SELECT jsonb_agg(to_jsonb(h) ORDER BY provider_id) FROM upstream_site_control.holds h),'[]'::jsonb),
        'events',coalesce((SELECT jsonb_agg(to_jsonb(e) ORDER BY id DESC) FROM
            (SELECT * FROM upstream_site_control.events ORDER BY id DESC LIMIT 200) e),'[]'::jsonb))
$$;
REVOKE ALL ON ALL TABLES IN SCHEMA upstream_site_control FROM PUBLIC;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA upstream_site_control FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA upstream_site_control FROM PUBLIC;
-- The deployment script creates a per-database login and grants only USAGE on
-- this schema plus EXECUTE on reconcile() and snapshot(). No table privileges.
COMMIT;
