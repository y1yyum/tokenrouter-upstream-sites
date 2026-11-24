package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
	_ "time/tzdata"

	pb "github.com/TokenFlux/TokenRouter/plugin-sdk/go/pluginpb/v1"
	"github.com/TokenFlux/TokenRouter/plugin-sdk/go/sdk"
)

// main.go connects the standalone plugin to authenticated host storage and tasks.
// Site control uses a restricted atomic bridge; host HTTP operations remain reads.
// Persistent secrets use authenticated encryption; their ciphertext never enters public responses.

// KVStore isolates persistence so behavior can be tested without a running host.
type KVStore interface {
	Get(context.Context, string, string) (*pb.KVGetResponse, error)
	Put(context.Context, string, string, any, ...sdk.PutOption) (int64, error)
	List(context.Context, *pb.KVListRequest) (*pb.KVListResponse, error)
	Delete(context.Context, string, string, int64) (bool, error)
}

// Plugin owns process-local serialization while the host owns durable state.
type Plugin struct {
	env    *sdk.Env      // Host process context.
	kv     KVStore       // Persistent namespaced records.
	cipher *AESEncryptor // Immutable credential encryption.
	mu     sync.Mutex    // Configuration and completion serialization.
	busy   sync.Map      // Per-site in-flight observations.
	client *http.Client  // Bounded monitoring transport.
	bridge ControlBridge // Atomic, ownership-aware provider scheduling capability.
}

// StoredSite wraps public site metadata and its separately encrypted credentials.
type StoredSite struct {
	Site       Site   `json:"site"`       // Public site configuration.
	Ciphertext string `json:"ciphertext"` // Private authenticated encrypted JSON.
}

// main starts the SDK lifecycle and contributes no standalone network listener.
func main() { sdk.Serve(&Plugin{}) }

// Init validates secrets before accepting either requests or scheduled observations.
func (p *Plugin) Init(ctx context.Context, env *sdk.Env) error {
	key, err := hex.DecodeString(env.Config.Secret("encryptionKey"))
	if err != nil {
		return errors.New("invalid encryption key")
	}
	p.cipher, err = NewAESEncryptor(key)
	if err != nil {
		return err
	}
	p.env = env
	p.kv = env.Host.KV
	p.client = monitorClient("")
	if dsn := env.Config.Secret("controlDSN"); dsn != "" {
		p.bridge, err = openBridge(ctx, dsn)
		if err != nil {
			return err
		}
	}
	if !validHostURL(env.Config.String("hostURL")) {
		return errors.New("host API must use loopback HTTP")
	}
	_, err = p.records(ctx, "sites", "")
	return err
}

// read decodes one namespaced record and returns its optimistic storage version.
func (p *Plugin) read(ctx context.Context, c, k string, out any) (int64, error) {
	r, err := p.kv.Get(ctx, c, k)
	if err != nil {
		return 0, err
	}
	if !r.Found {
		return 0, ErrNotFound
	}
	return r.Version, json.Unmarshal(r.ValueJson, out)
}

// records follows host pagination without interpreting private record contents.
func (p *Plugin) records(ctx context.Context, c, prefix string) ([]*pb.KVItem, error) {
	var out []*pb.KVItem
	cursor := ""
	for {
		r, err := p.kv.List(ctx, &pb.KVListRequest{Collection: c, Prefix: prefix, Cursor: cursor, Limit: 500})
		if err != nil {
			return nil, err
		}
		out = append(out, r.Items...)
		if r.NextCursor == "" {
			return out, nil
		}
		cursor = r.NextCursor
	}
}

// sites lists encrypted aggregates for internal consumers; HTTP projects public fields.
func (p *Plugin) sites(ctx context.Context) ([]StoredSite, error) {
	rows, err := p.records(ctx, "sites", "")
	if err != nil {
		return nil, err
	}
	out := []StoredSite{}
	for _, r := range rows {
		var s StoredSite
		if err = json.Unmarshal(r.ValueJson, &s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Site.ID < out[j].Site.ID })
	return out, nil
}

// fmtSource constructs stable numeric prefixes shared by history and statistics.
func fmtSource(id int64, s string) string { return fmt.Sprintf("%020d:%s", id, s) }

// history returns all retained observations in time order for deterministic rollups.
func (p *Plugin) history(ctx context.Context, id int64) ([]Observation, error) {
	rows, err := p.records(ctx, "history", fmtSource(id, ""))
	if err != nil {
		return nil, err
	}
	out := []Observation{}
	for _, r := range rows {
		var o Observation
		if err = json.Unmarshal(r.ValueJson, &o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// rebuild replaces complete retained calendar days, preserving older daily summaries.
// The first partially retained day is excluded once retention has removed samples.
// Replaying this operation after a process exit cannot add consumption twice.
func (p *Plugin) rebuild(ctx context.Context, s Site) ([]DailyStat, error) {
	history, err := p.history(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	stats := DailyStats(history)
	loc, _ := time.LoadLocation(s.Config.Timezone)
	if loc == nil {
		loc = time.UTC
	}
	cutoff := time.Now().AddDate(0, 0, -s.Config.RawRetentionDays).In(loc).Format(time.DateOnly)
	days := map[string][]DailyStat{}
	for _, d := range stats {
		if d.Date > cutoff {
			days[d.Date] = append(days[d.Date], d)
		}
	}
	for day, values := range days {
		if _, err = p.kv.Put(ctx, "daily", fmtSource(s.ID, day), values, sdk.TTL(time.Duration(s.Config.SummaryRetentionDays)*24*time.Hour)); err != nil {
			return nil, err
		}
	}
	rows, err := p.records(ctx, "daily", fmtSource(s.ID, ""))
	if err != nil {
		return nil, err
	}
	out := []DailyStat{}
	for _, r := range rows {
		var values []DailyStat
		if err = json.Unmarshal(r.ValueJson, &values); err != nil {
			return nil, err
		}
		out = append(out, values...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out, nil
}

// query serializes one site while allowing independent sites to use four workers.
// Configuration revisions invalidate observations started before an edit. History is
// written before summaries so a later rebuild can recover an interrupted completion.
func (p *Plugin) query(ctx context.Context, id int64, source string) (Observation, error) {
	if _, busy := p.busy.LoadOrStore(id, true); busy {
		return Observation{}, ErrBusy
	}
	defer p.busy.Delete(id)
	var stored StoredSite
	_, err := p.read(ctx, "sites", strconv.FormatInt(id, 10), &stored)
	if err != nil {
		return Observation{}, err
	}
	s := stored.Site
	if s.Archived {
		return Observation{}, ErrInvalid
	}
	raw, err := p.cipher.Decrypt(stored.Ciphertext)
	if err != nil {
		return Observation{}, errors.New("credential decryption failed")
	}
	var secret Secrets
	if err = json.Unmarshal([]byte(raw), &secret); err != nil {
		return Observation{}, err
	}
	o := p.observe(ctx, s.Config, secret, source)
	o.ConfigVersion = s.ConfigVersion
	o.WalletEpoch = s.WalletEpoch
	o.StatisticsEpoch = s.StatisticsEpoch
	o.Timezone = s.Config.Timezone
	o.IntervalMinutes = s.Config.IntervalMinutes
	p.mu.Lock()
	defer p.mu.Unlock()
	var current StoredSite
	version, err := p.read(ctx, "sites", strconv.FormatInt(id, 10), &current)
	if err != nil {
		return o, err
	}
	if current.Site.ConfigVersion != s.ConfigVersion || current.Site.Archived {
		return o, ErrConflict
	}
	if source == "balance" && o.Outcome == Healthy {
		for i := range o.Balances {
			price := s.PriceFor(o.Balances[i].Unit)
			o.Balances[i].Price = &price
		}
	}
	_, err = p.kv.Put(ctx, "history", fmtSource(id, o.At.UTC().Format(time.RFC3339Nano)+":"+o.RunID), o, sdk.IfAbsent(), sdk.TTL(time.Duration(s.Config.RawRetentionDays)*24*time.Hour))
	if err != nil {
		return o, err
	}
	current.Site.LastObservation = &o
	current.Site.State = Evaluate(current.Site.Config, current.Site.State, o)
	if source == "balance" {
		current.Site.LastBalanceObservation = &o
		if o.Outcome == Healthy {
			current.Site.Latest = &o
		}
	}
	if current.Site.NextDue == nil {
		current.Site.NextDue = map[string]time.Time{}
	}
	interval := s.Config.IntervalMinutes
	for _, probe := range s.Config.Probes {
		if probe.ID == source {
			interval = probe.IntervalMinutes
		}
	}
	current.Site.NextDue[source] = time.Now().Add(time.Duration(interval) * time.Minute)
	_, err = p.kv.Put(ctx, "sites", strconv.FormatInt(id, 10), current, sdk.IfVersion(version))
	if err != nil {
		return o, err
	}
	if err = p.reconcile(ctx); err != nil {
		return o, err
	}
	stats, err := p.rebuild(ctx, current.Site)
	if err != nil {
		return o, err
	}
	err = p.alert(ctx, current.Site, o, stats)
	return o, err
}

// RunTask retries persisted restrictions and checks each monitoring source's deadline.
func (p *Plugin) RunTask(ctx context.Context, name string, at time.Time) error {
	if name != "monitor" {
		return ErrInvalid
	}
	if err := p.reconcile(ctx); err != nil {
		return err
	}
	sites, err := p.sites(ctx)
	if err != nil {
		return err
	}
	jobs := make(chan StoredSite)
	var wg sync.WaitGroup
	var errsMu sync.Mutex
	var errs []error
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				s := item.Site
				if s.Archived {
					continue
				}
				sources := []string{}
				if s.Config.AutoQuery && !s.NextDue["balance"].After(at) {
					sources = append(sources, "balance")
				}
				for _, probe := range s.Config.Probes {
					if !s.NextDue[probe.ID].After(at) {
						sources = append(sources, probe.ID)
					}
				}
				for _, source := range sources {
					if _, e := p.query(ctx, s.ID, source); e != nil && !errors.Is(e, ErrBusy) {
						errsMu.Lock()
						errs = append(errs, fmt.Errorf("site %d: %w", s.ID, e))
						errsMu.Unlock()
					}
				}
			}
		}()
	}
	for _, s := range sites {
		select {
		case jobs <- s:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	return errors.Join(errs...)
}
