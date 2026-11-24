package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/TokenFlux/TokenRouter/plugin-sdk/go/sdk"
)

// http.go exposes administrator-only site management through the plugin namespace.
// Provider credentials are projected away before responding, and host requests only
// use GET. Binding records belong to the plugin rather than provider schema extensions.

// ProviderSummary contains only fields required for reviewing site membership.
type ProviderSummary struct {
	ID          int64  `json:"id"`          // Existing host provider ID.
	Name        string `json:"name"`        // Existing administrator label.
	BaseURL     string `json:"base_url"`    // Existing forwarding root.
	Status      string `json:"status"`      // Read-only manual lifecycle.
	Schedulable bool   `json:"schedulable"` // Read-only scheduling switch.
}

// SaveRequest carries a complete config revision and optional credential replacements.
type SaveRequest struct {
	Config     Config   `json:"config"`            // Complete editable configuration.
	Version    int64    `json:"config_version"`    // Expected public revision.
	Secrets    *Secrets `json:"secrets,omitempty"` // Blank fields retain credentials.
	ClearKey   bool     `json:"clear_key"`         // Explicit wallet key deletion.
	ClearProxy bool     `json:"clear_proxy"`       // Explicit monitor proxy deletion.
	Archived   bool     `json:"archived"`          // Disable site monitoring lifecycle.
}

// Handler registers only admin-authenticated routes declared by the manifest.
func (p *Plugin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sites", p.listHTTP)
	mux.HandleFunc("POST /sites", p.saveHTTP)
	mux.HandleFunc("PUT /sites/{id}", p.saveHTTP)
	mux.HandleFunc("POST /sites/{id}/query", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		source := r.URL.Query().Get("source")
		if source == "" {
			source = "balance"
		}
		o, e := p.query(r.Context(), id, source)
		reply(w, o, e)
	})
	mux.HandleFunc("GET /sites/{id}/history", p.historyHTTP)
	mux.HandleFunc("GET /sites/{id}/daily", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		p.mu.Lock()
		defer p.mu.Unlock()
		var s StoredSite
		_, e := p.read(r.Context(), "sites", strconv.FormatInt(id, 10), &s)
		if e != nil {
			reply(w, nil, e)
			return
		}
		d, e := p.rebuild(r.Context(), s.Site)
		reply(w, d, e)
	})
	mux.HandleFunc("GET /sites/{id}/alerts", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		rows, e := p.records(r.Context(), "events", fmtSource(id, ""))
		out := []json.RawMessage{}
		for _, v := range rows {
			out = append(out, json.RawMessage(v.ValueJson))
		}
		reply(w, out, e)
	})
	mux.HandleFunc("GET /providers", func(w http.ResponseWriter, r *http.Request) { data, e := p.providers(r.Context()); reply(w, data, e) })
	mux.HandleFunc("GET /bindings", func(w http.ResponseWriter, r *http.Request) {
		var bindings map[string]int64
		version, e := p.read(r.Context(), "meta", "bindings", &bindings)
		if errors.Is(e, ErrNotFound) {
			bindings = map[string]int64{}
			e = nil
		}
		reply(w, map[string]any{"bindings": bindings, "version": version}, e)
	})
	mux.HandleFunc("PUT /bindings", p.bindHTTP)
	mux.HandleFunc("GET /control", func(w http.ResponseWriter, r *http.Request) {
		if p.bridge == nil {
			reply(w, map[string]any{"available": false}, nil)
			return
		}
		status, e := p.bridge.Snapshot(r.Context())
		reply(w, map[string]any{"available": e == nil, "status": status}, e)
	})
	return mux
}

// reply emits stable diagnostics without reflecting secret-bearing upstream errors.
func reply(w http.ResponseWriter, data any, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		status := 500
		message := "操作失败，请检查插件运行日志和配置"
		switch {
		case errors.Is(err, errBalanceMultipliers):
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"message": "余额倍率须为正十进制数，最多 10 位小数，单位须为 USD、CNY 或 TOKENS",
				"field":   "config.balance_multipliers",
			})
			return
		case errors.Is(err, ErrInvalid):
			status = 400
			message = "配置无效，请检查阈值、模式和必需探针"
		case errors.Is(err, ErrBridge):
			status = 503
			message = "提供商控制连接不可用；已保存的策略会在连接恢复后重试，请刷新确认状态"
		case errors.Is(err, ErrConflict) || errors.Is(err, sdk.ErrVersionConflict):
			status = 409
			message = "数据已变化，请刷新后重试"
		case errors.Is(err, ErrNotFound):
			status = 404
			message = "站点不存在"
		case errors.Is(err, ErrBusy):
			status = 409
			message = "站点正在查询，请稍后刷新"
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": message})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// listHTTP returns public metadata with current plugin-owned membership counts.
func (p *Plugin) listHTTP(w http.ResponseWriter, r *http.Request) {
	values, err := p.sites(r.Context())
	if err != nil {
		reply(w, nil, err)
		return
	}
	bindings := map[string]int64{}
	_, e := p.read(r.Context(), "meta", "bindings", &bindings)
	if e != nil && !errors.Is(e, ErrNotFound) {
		reply(w, nil, e)
		return
	}
	counts := map[int64]int{}
	for _, id := range bindings {
		counts[id]++
	}
	out := []Site{}
	for _, v := range values {
		v.Site.ProviderCount = counts[v.Site.ID]
		out = append(out, v.Site)
	}
	reply(w, map[string]any{"sites": out, "control_available": p.bridge != nil, "version": "1.2.0"}, nil)
}

// saveHTTP applies optimistic edits while retaining omitted write-only secrets.
func (p *Plugin) saveHTTP(w http.ResponseWriter, r *http.Request) {
	var req SaveRequest
	req.Config = DefaultConfig()
	req.Config.Timezone = "Asia/Shanghai"
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		if errors.Is(err, errBalanceMultipliers) {
			reply(w, nil, errBalanceMultipliers)
		} else {
			reply(w, nil, ErrInvalid)
		}
		return
	}
	if err := req.Config.Validate(); err != nil {
		reply(w, nil, err)
		return
	}
	if req.Config.Policy.Mode != "off" {
		if p.bridge == nil {
			reply(w, nil, ErrBridge)
			return
		}
		if _, e := p.bridge.Snapshot(r.Context()); e != nil {
			reply(w, nil, e)
			return
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var stored StoredSite
	version := int64(-1)
	var secret Secrets
	if id > 0 {
		v, e := p.read(r.Context(), "sites", strconv.FormatInt(id, 10), &stored)
		if e != nil {
			reply(w, nil, e)
			return
		}
		version = v
		if req.Version != stored.Site.ConfigVersion {
			reply(w, nil, ErrConflict)
			return
		}
		raw, e := p.cipher.Decrypt(stored.Ciphertext)
		if e != nil {
			reply(w, nil, e)
			return
		}
		if json.Unmarshal([]byte(raw), &secret) != nil {
			reply(w, nil, ErrInvalid)
			return
		}
	} else {
		all, e := p.sites(r.Context())
		if e != nil {
			reply(w, nil, e)
			return
		}
		id = 1
		for _, s := range all {
			if s.Site.ID >= id {
				id = s.Site.ID + 1
			}
		}
		stored.Site = Site{ID: id, WalletEpoch: 1, StatisticsEpoch: 1, NextDue: map[string]time.Time{}, State: State{Reasons: map[string]time.Time{}, Counters: map[string]Counter{}, Version: 1}}
	}
	previousSecret := secret
	previousSecret.ProbeKeys = maps.Clone(secret.ProbeKeys)
	walletChanged := version > 0 && (req.Config.BaseURL != stored.Site.Config.BaseURL || req.Config.Adapter != stored.Site.Config.Adapter)
	if req.Secrets != nil {
		v := req.Secrets
		if v.Key != "" && v.Key != secret.Key {
			secret.Key = v.Key
			walletChanged = version > 0
		}
		if v.WalletToken != "" && v.WalletToken != secret.WalletToken {
			secret.WalletToken = v.WalletToken
			walletChanged = version > 0
		}
		if v.WalletUserID != "" && v.WalletUserID != secret.WalletUserID {
			secret.WalletUserID = v.WalletUserID
			walletChanged = version > 0
		}
		if v.ProxyURL != "" {
			u, e := url.Parse(v.ProxyURL)
			if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
				reply(w, nil, ErrInvalid)
				return
			}
			secret.ProxyURL = v.ProxyURL
		}
		if len(v.ProbeKeys) > 0 {
			if secret.ProbeKeys == nil {
				secret.ProbeKeys = map[string]string{}
			}
			for k, x := range v.ProbeKeys {
				if x != "" {
					secret.ProbeKeys[k] = x
				}
			}
		}
	}
	if req.ClearKey {
		secret.Key = ""
		walletChanged = version > 0
	}
	if req.ClearProxy {
		secret.ProxyURL = ""
	}
	if req.Config.Adapter == "new_api" {
		if _, err := newAPIUserID(secret.WalletUserID); err != nil {
			reply(w, nil, ErrInvalid)
			return
		}
	}
	if walletChanged {
		stored.Site.WalletEpoch++
		stored.Site.Latest = nil
		stored.Site.LastObservation = nil
		stored.Site.LastBalanceObservation = nil
	}
	if version > 0 && req.Config.Timezone != stored.Site.Config.Timezone {
		stored.Site.StatisticsEpoch++
	}
	if req.Archived {
		var bindings map[string]int64
		_, e := p.read(r.Context(), "meta", "bindings", &bindings)
		if e != nil && !errors.Is(e, ErrNotFound) {
			reply(w, nil, e)
			return
		}
		for _, sid := range bindings {
			if sid == id {
				reply(w, nil, ErrInvalid)
				return
			}
		}
	}
	raw, e := json.Marshal(secret)
	if e != nil {
		reply(w, nil, e)
		return
	}
	stored.Ciphertext, e = p.cipher.Encrypt(string(raw))
	if e != nil {
		reply(w, nil, e)
		return
	}
	pricingOnly := false
	if req.Config.BalanceMultipliers == nil {
		req.Config.BalanceMultipliers = stored.Site.Config.BalanceMultipliers
	} else {
		pricingOnly = version > 0 && !walletChanged && req.Archived == stored.Site.Archived &&
			sameConfigExceptBalanceMultipliers(req.Config, stored.Site.Config) && reflect.DeepEqual(secret, previousSecret)
		stored.Site.updateBalancePriceRevisions(req.Config, time.Now().UTC())
	}
	if !pricingOnly {
		stored.Site.State = Reconfigure(req.Config, stored.Site.State)
	}
	stored.Site.Config = req.Config
	stored.Site.ConfigVersion++
	stored.Site.HasKey = secret.Key != "" || secret.WalletToken != ""
	stored.Site.HasProxy = secret.ProxyURL != ""
	stored.Site.Archived = req.Archived
	_, e = p.kv.Put(r.Context(), "sites", strconv.FormatInt(id, 10), stored, sdk.IfVersion(version))
	if e == nil {
		e = p.reconcile(r.Context())
	}
	reply(w, stored.Site, e)
}

func sameConfigExceptBalanceMultipliers(a, b Config) bool {
	a.BalanceMultipliers, b.BalanceMultipliers = nil, nil
	return reflect.DeepEqual(a, b)
}

// historyHTTP provides bounded pages while retaining exact imported observations.
func (p *Plugin) historyHTTP(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	all, e := p.history(r.Context(), id)
	if e != nil {
		reply(w, nil, e)
		return
	}
	items := []Observation{}
	source := r.URL.Query().Get("source")
	for i := len(all) - 1; i >= 0; i-- {
		if source == "" || all[i].Source == source {
			items = append(items, all[i])
		}
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	if offset > len(items) {
		offset = len(items)
	}
	end := offset + 100
	if end > len(items) {
		end = len(items)
	}
	reply(w, map[string]any{"items": items[offset:end], "total": len(items)}, nil)
}

// providers reads the official management API and removes all credential fields.
func (p *Plugin) providers(ctx context.Context) ([]ProviderSummary, error) {
	base := p.env.Config.String("hostURL")
	if !validHostURL(base) {
		return nil, ErrInvalid
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	out := []ProviderSummary{}
	for page := 1; page <= 1000; page++ {
		target := fmt.Sprintf("%s/api/v1/admin/providers?page=%d&page_size=100", strings.TrimRight(base, "/"), page)
		req, e := http.NewRequestWithContext(ctx, "GET", target, nil)
		if e != nil {
			return nil, e
		}
		req.Header.Set("x-api-key", p.env.Config.Secret("adminKey"))
		resp, e := client.Do(req)
		if e != nil {
			return nil, e
		}
		var envelope struct {
			Data struct {
				Items []struct {
					ID          int64  `json:"id"`
					Name        string `json:"name"`
					Status      string `json:"status"`
					Schedulable bool   `json:"schedulable"`
					Credentials struct {
						BaseURL string `json:"base_url"`
					} `json:"credentials"`
				} `json:"items"`
				Total int `json:"total"`
			} `json:"data"`
		}
		e = json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 8<<20)).Decode(&envelope)
		resp.Body.Close()
		if e != nil || resp.StatusCode != 200 {
			return nil, errors.New("host provider read failed")
		}
		for _, item := range envelope.Data.Items {
			out = append(out, ProviderSummary{item.ID, item.Name, item.Credentials.BaseURL, item.Status, item.Schedulable})
		}
		if len(out) >= envelope.Data.Total || len(envelope.Data.Items) == 0 {
			return out, nil
		}
	}
	return nil, errors.New("provider pagination limit")
}

// bindHTTP atomically replaces a reviewed mapping without touching host providers.
func (p *Plugin) bindHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Bindings map[string]int64 `json:"bindings"`
		Version  int64            `json:"version"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || req.Bindings == nil {
		reply(w, nil, ErrInvalid)
		return
	}
	providers, e := p.providers(r.Context())
	if e != nil {
		reply(w, nil, e)
		return
	}
	allowed := map[string]bool{}
	for _, v := range providers {
		allowed[strconv.FormatInt(v.ID, 10)] = true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	sites, e := p.sites(r.Context())
	if e != nil {
		reply(w, nil, e)
		return
	}
	valid := map[int64]bool{}
	for _, s := range sites {
		valid[s.Site.ID] = !s.Site.Archived
	}
	for id, site := range req.Bindings {
		if !allowed[id] || !valid[site] {
			reply(w, nil, ErrInvalid)
			return
		}
	}
	version := req.Version
	if version == 0 {
		version = -1
	}
	v, e := p.kv.Put(r.Context(), "meta", "bindings", req.Bindings, sdk.IfVersion(version))
	if e == nil {
		e = p.reconcile(r.Context())
	}
	reply(w, map[string]any{"version": v}, e)
}
