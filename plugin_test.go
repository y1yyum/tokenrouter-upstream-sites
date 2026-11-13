package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/plugin-sdk/go/manifest"
	"github.com/TokenFlux/TokenRouter/plugin-sdk/go/plugintest"
	"github.com/TokenFlux/TokenRouter/plugin-sdk/go/sdk"
	"github.com/stretchr/testify/require"
)

// plugin_test.go verifies public plugin behavior against the official SDK fake host.
// HTTP fixtures exercise wallet classification, write-only secrets, optimistic edits,
// restart persistence and provider-read projection without using production accounts.

// testPlugin creates a connected SDK fixture and validates real initialization.
func testPlugin(t *testing.T) (*Plugin, *plugintest.Harness) {
	t.Helper()
	h := plugintest.New(t)
	cfg, err := sdk.NewConfig([]byte(`{"hostURL":"http://127.0.0.1:8080","encryptionKey":"` + strings.Repeat("a", 64) + `","notifyUserID":0}`))
	require.NoError(t, err)
	h.Env.Config = cfg
	p := &Plugin{}
	require.NoError(t, p.Init(context.Background(), h.Env))
	return p, h
}

// call exercises the same handler and response envelope used by the frontend.
func call(t *testing.T, p *Plugin, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.Handler().ServeHTTP(w, r)
	return w
}

// addSite persists a real site through its public creation endpoint.
func addSite(t *testing.T, p *Plugin, base string) Site {
	t.Helper()
	c := DefaultConfig()
	c.BaseURL = base
	c.Name = "fixture"
	w := call(t, p, "POST", "/sites", SaveRequest{Config: c, Secrets: &Secrets{Key: "private-wallet-key"}})
	require.Equal(t, 200, w.Code, w.Body.String())
	var r struct {
		Data Site `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &r))
	return r.Data
}

// TestManifest ensures the package uses the official declaration contract unchanged.
func TestManifest(t *testing.T) {
	raw, e := os.ReadFile("plugin.json")
	require.NoError(t, e)
	_, e = manifest.Parse(raw)
	require.NoError(t, e)
}

// TestWalletPersistenceAndDisabledControls covers secrets, failure evidence and restart.
func TestWalletPersistenceAndDisabledControls(t *testing.T) {
	p, h := testPlugin(t)
	var status atomic.Int32
	status.Store(200)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer private-wallet-key", r.Header.Get("Authorization"))
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(`{"isValid":true,"balance":"12.1234567890","unit":"USD","remaining":999}`))
	}))
	defer server.Close()
	p.client = server.Client()
	site := addSite(t, p, server.URL)
	o, e := p.query(context.Background(), site.ID, "balance")
	require.NoError(t, e)
	require.Equal(t, "12.123456789", o.Balances[0].Available)
	for _, code := range []int{401, 429, 503} {
		status.Store(int32(code))
		o, e = p.query(context.Background(), site.ID, "balance")
		require.NoError(t, e)
		require.Empty(t, o.Balances)
		require.NotEqual(t, Healthy, o.Outcome)
	}
	list := call(t, p, "GET", "/sites", nil)
	require.NotContains(t, list.Body.String(), "private-wallet-key")
	require.NotContains(t, list.Body.String(), "ciphertext")
	require.Contains(t, list.Body.String(), "12.123456789")
	cfg := site.Config
	cfg.Policy.Mode = "balance"
	w := call(t, p, "PUT", "/sites/1", SaveRequest{Config: cfg, Version: site.ConfigVersion})
	require.Equal(t, 400, w.Code)
	cfg.Policy.Mode = "off"
	cfg.Policy.AutoRecover = true
	w = call(t, p, "PUT", "/sites/1", SaveRequest{Config: cfg, Version: site.ConfigVersion})
	require.Equal(t, 400, w.Code)
	restarted := &Plugin{}
	require.NoError(t, restarted.Init(context.Background(), h.Env))
	records, e := restarted.history(context.Background(), site.ID)
	require.NoError(t, e)
	require.Len(t, records, 4)
}

// TestEditingAndStatisticsAreIdempotent rejects stale edits and duplicate rollups.
func TestEditingAndStatisticsAreIdempotent(t *testing.T) {
	p, _ := testPlugin(t)
	s := addSite(t, p, "https://example.com")
	cfg := s.Config
	cfg.Notes = "updated"
	w := call(t, p, "PUT", "/sites/1", SaveRequest{Config: cfg, Version: 1})
	require.Equal(t, 200, w.Code)
	w = call(t, p, "PUT", "/sites/1", SaveRequest{Config: cfg, Version: 1})
	require.Equal(t, 409, w.Code)
	var record StoredSite
	_, e := p.read(context.Background(), "sites", "1", &record)
	require.NoError(t, e)
	raw, e := p.cipher.Decrypt(record.Ciphertext)
	require.NoError(t, e)
	require.Contains(t, raw, "private-wallet-key")
	at := time.Now().UTC().Add(-time.Hour)
	for i, amount := range []string{"20", "18"} {
		o := statSample(at.Add(time.Duration(i)*30*time.Minute), amount)
		o.RunID = amount
		_, e = p.kv.Put(context.Background(), "history", fmtSource(1, amount), o)
		require.NoError(t, e)
	}
	a, e := p.rebuild(context.Background(), record.Site)
	require.NoError(t, e)
	b, e := p.rebuild(context.Background(), record.Site)
	require.NoError(t, e)
	require.Equal(t, a, b)
}

// TestNewAPIRejectsKeyQuotas prevents API-key allowance from becoming account balance.
func TestNewAPIRejectsKeyQuotas(t *testing.T) {
	_, err := newAPICompatible([]byte(`{"data":{"total_available":123,"total_used":4}}`), defaultNewAPISettings())
	require.Error(t, err)
}

// TestProvidersAreReadOnly verifies management key confinement and credential projection.
func TestProvidersAreReadOnly(t *testing.T) {
	p, h := testPlugin(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "GET", r.Method)
		require.Equal(t, "management-test", r.Header.Get("x-api-key"))
		_, _ = w.Write([]byte(`{"data":{"items":[{"id":7,"name":"test","status":"active","schedulable":false,"credentials":{"base_url":"https://example.com","api_key":"never-return"}}],"total":1}}`))
	}))
	defer server.Close()
	cfg, e := sdk.NewConfig([]byte(`{"hostURL":"` + server.URL + `","adminKey":"management-test","encryptionKey":"` + strings.Repeat("a", 64) + `"}`))
	require.NoError(t, e)
	h.Env.Config = cfg
	addSite(t, p, "https://example.com")
	r := call(t, p, "GET", "/providers", nil)
	require.Equal(t, 200, r.Code)
	require.NotContains(t, r.Body.String(), "never-return")
	w := call(t, p, "PUT", "/bindings", map[string]any{"version": 0, "bindings": map[string]int64{"7": 1}})
	require.Equal(t, 200, w.Code)
	w = call(t, p, "PUT", "/bindings", map[string]any{"version": 0, "bindings": map[string]int64{"7": 1}})
	require.Equal(t, 409, w.Code)
}

// TestPublicTransportRejectsPrivateTargets covers loopback credential exfiltration attempts.
func TestPublicTransportRejectsPrivateTargets(t *testing.T) {
	_, e := publicDial(context.Background(), "tcp", "127.0.0.1:1234")
	require.Error(t, e)
	require.False(t, validHostURL("https://example.com"))
}

// TestProbeAndConcurrentEdit separates health evidence from configuration races.
func TestProbeAndConcurrentEdit(t *testing.T) {
	p, _ := testPlugin(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer server.Close()
	p.client = server.Client()
	site := addSite(t, p, server.URL)
	c := site.Config
	c.Probes = []Probe{{ID: "health", URL: server.URL, IntervalMinutes: 1, TimeoutSeconds: 10, JSONPath: "success", Expected: "true", UseWalletKey: true, SuccessCodes: []int{200}}}
	require.Equal(t, 200, call(t, p, "PUT", "/sites/1", SaveRequest{Config: c, Version: 1}).Code)
	done := make(chan error, 1)
	go func() { _, e := p.query(context.Background(), 1, "health"); done <- e }()
	<-entered
	c.Notes = "changed during probe"
	require.Equal(t, 200, call(t, p, "PUT", "/sites/1", SaveRequest{Config: c, Version: 2}).Code)
	close(release)
	require.ErrorIs(t, <-done, ErrConflict)
	history, e := p.history(context.Background(), 1)
	require.NoError(t, e)
	require.Empty(t, history)
	c.Probes[0].URL = "https://different.example"
	require.Error(t, c.Validate())
}

// TestControlNetworkEvidence excludes configuration failures from automatic suspension.
func TestControlNetworkEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		outage bool
	}{
		{"invalid header", &url.Error{Op: "Get", URL: "https://example.com", Err: errors.New("invalid header value")}, false},
		{"TLS verification", &url.Error{Err: &tls.CertificateVerificationError{Err: errors.New("untrusted certificate")}}, false},
		{"cancelled worker", context.Canceled, false},
		{"deadline", &url.Error{Err: context.DeadlineExceeded}, true},
		{"connection refused", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"DNS lookup", &net.DNSError{Err: "no such host", Name: "example.invalid"}, true},
	} {
		t.Run(test.name, func(t *testing.T) { require.Equal(t, test.outage, isNetworkOutage(test.err)) })
	}
	require.Equal(t, "off", DefaultConfig().Policy.Mode)
	require.False(t, DefaultConfig().Policy.AutoRecover)
}
