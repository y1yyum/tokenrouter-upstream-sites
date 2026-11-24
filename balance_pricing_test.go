package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/plugin-sdk/go/sdk"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func readPricingResponse[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var response struct {
		Data T `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	return response.Data
}

func savePricingConfig(t *testing.T, p *Plugin, method, path string, req SaveRequest, multipliers string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(req)
	require.NoError(t, err)
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &body))
	var config map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body["config"], &config))
	if multipliers == "" {
		delete(config, "balance_multipliers")
	} else {
		config["balance_multipliers"] = json.RawMessage(multipliers)
	}
	body["config"], err = json.Marshal(config)
	require.NoError(t, err)
	return call(t, p, method, path, body)
}

func TestBalancePricingConfigHTTP(t *testing.T) {
	p, _ := testPlugin(t)
	config := DefaultConfig()
	config.BaseURL = "https://example.com"
	created := readPricingResponse[Site](t, savePricingConfig(t, p, "POST", "/sites", SaveRequest{Config: config}, `{"USD":"2.0","TOKENS":"0.5"}`))
	require.Equal(t, BalanceMultipliers{"USD": "2", "TOKENS": "0.5"}, created.Config.BalanceMultipliers)
	require.Equal(t, WalletPrice{Multiplier: "2", Source: "configured", Version: 1}, created.PriceFor("USD"))
	require.Equal(t, WalletPrice{Multiplier: "1", Source: "default"}, created.PriceFor("CNY"))
	require.Equal(t, WalletPrice{Multiplier: "0.5", Source: "configured", Version: 1}, created.PriceFor("TOKENS"))
	require.False(t, created.BalancePriceRevisions["USD"].UpdatedAt.IsZero())

	req := SaveRequest{Config: created.Config, Version: created.ConfigVersion}
	changeStarted := time.Now().UTC()
	changed := readPricingResponse[Site](t, savePricingConfig(t, p, "PUT", "/sites/1", req, `{"USD":"2.00","CNY":"1.0","TOKENS":"3"}`))
	require.Equal(t, created.BalancePriceRevisions["USD"], changed.BalancePriceRevisions["USD"])
	require.Equal(t, WalletPrice{Multiplier: "1", Source: "configured"}, changed.PriceFor("CNY"))
	require.NotContains(t, changed.BalancePriceRevisions, "CNY")
	require.Equal(t, int64(2), changed.PriceFor("TOKENS").Version)
	require.WithinRange(t, changed.BalancePriceRevisions["TOKENS"].UpdatedAt, changeStarted, time.Now().UTC())
	require.Equal(t, http.StatusConflict, savePricingConfig(t, p, "PUT", "/sites/1", req, `{}`).Code)

	req = SaveRequest{Config: changed.Config, Version: changed.ConfigVersion}
	req.Config.Notes = "legacy form"
	legacy := readPricingResponse[Site](t, savePricingConfig(t, p, "PUT", "/sites/1", req, ""))
	require.Equal(t, changed.Config.BalanceMultipliers, legacy.Config.BalanceMultipliers)
	require.Equal(t, changed.BalancePriceRevisions, legacy.BalancePriceRevisions)

	req = SaveRequest{Config: legacy.Config, Version: legacy.ConfigVersion}
	for _, invalid := range []string{
		`{"USD":"0"}`, `{"USD":"-2"}`, `{"USD":""}`, `{"USD":"NaN"}`, `{"USD":"1e-1"}`,
		`{"USD":"0.00000000001"}`, `{"EUR":"2"}`, `{"USD":2}`, `{"USD":null}`, `[]`, `null`,
	} {
		t.Run(invalid, func(t *testing.T) {
			w := savePricingConfig(t, p, "PUT", "/sites/1", req, invalid)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), `"field":"config.balance_multipliers"`)
		})
	}
	req.Config.BalanceMultipliers = BalanceMultipliers{}
	clearStarted := time.Now().UTC()
	cleared := readPricingResponse[Site](t, call(t, p, "PUT", "/sites/1", req))
	require.Empty(t, cleared.Config.BalanceMultipliers)
	require.NotNil(t, cleared.Config.BalanceMultipliers)
	require.Equal(t, WalletPrice{Source: "unset", Version: 2}, cleared.PriceFor("USD"))
	require.Equal(t, WalletPrice{Multiplier: "1", Source: "default"}, cleared.PriceFor("CNY"))
	require.Equal(t, WalletPrice{Source: "unset", Version: 3}, cleared.PriceFor("TOKENS"))
	require.WithinRange(t, cleared.BalancePriceRevisions["USD"].UpdatedAt, clearStarted, time.Now().UTC())
}

type pricingTestBridge struct{}

func (pricingTestBridge) Snapshot(context.Context) (json.RawMessage, error) { return nil, nil }
func (pricingTestBridge) Reconcile(context.Context) error                   { return nil }

func TestBalancePricingSamplingAndStatus(t *testing.T) {
	ctx := context.Background()
	p, h := testPlugin(t)
	p.bridge = pricingTestBridge{}
	responses := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/user/balance":
			select {
			case body := <-responses:
				if body == "" {
					w.WriteHeader(http.StatusUnauthorized)
				} else {
					_, _ = w.Write([]byte(body))
				}
			default:
				t.Error("unexpected wallet request")
				w.WriteHeader(http.StatusInternalServerError)
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	p.client = server.Client()
	c := DefaultConfig()
	c.Adapter, c.BaseURL = "deepseek_balance", server.URL
	if hour := time.Now().UTC().Hour(); hour == 0 || hour == 23 {
		c.Timezone = "Asia/Shanghai"
	}
	c.Policy.Mode = "balance"
	c.Policy.Thresholds = []Threshold{{Unit: "USD", Low: "90", Recover: "100"}}
	c.Alerts = AlertPolicy{Unit: "USD", LowBalance: "90", DailySpend: "25"}
	c.Probes = []Probe{{ID: "health", URL: server.URL + "/health", IntervalMinutes: 5, TimeoutSeconds: 5, JSONPath: "ok", Expected: "true"}}
	site := readPricingResponse[Site](t, call(t, p, "POST", "/sites", SaveRequest{Config: c, Secrets: &Secrets{Key: "fixture"}}))
	require.Equal(t, WalletPrice{Multiplier: "1", Source: "default"}, site.PriceFor("CNY"))
	require.Equal(t, WalletPrice{Source: "unset"}, site.PriceFor("USD"))

	var stored StoredSite
	version, err := p.read(ctx, "sites", "1", &stored)
	require.NoError(t, err)
	legacy := Observation{
		RunID: "legacy", Source: "balance", Outcome: Healthy, At: time.Now().UTC().Add(-time.Minute),
		ConfigVersion: site.ConfigVersion, WalletEpoch: site.WalletEpoch, StatisticsEpoch: site.StatisticsEpoch,
		Timezone: c.Timezone, IntervalMinutes: c.IntervalMinutes, Balances: []Balance{{Unit: "USD", Available: "120"}},
	}
	stored.Site.Latest, stored.Site.LastObservation = &legacy, &legacy
	stored.Site.State.Manual = true
	_, err = p.kv.Put(ctx, "sites", "1", stored, sdk.IfVersion(version))
	require.NoError(t, err)
	_, err = p.kv.Put(ctx, "history", fmtSource(1, "legacy"), legacy)
	require.NoError(t, err)

	site = readPricingResponse[Site](t, savePricingConfig(t, p, "PUT", "/sites/1", SaveRequest{Config: site.Config, Version: site.ConfigVersion}, `{"USD":"2"}`))
	require.Nil(t, site.Latest.Balances[0].Price)
	responses <- `{"balance_infos":[{"currency":"USD","total_balance":"100"},{"currency":"CNY","total_balance":"50"},{"currency":"TOKENS","total_balance":"0"}]}`
	manual := readPricingResponse[Observation](t, call(t, p, "POST", "/sites/1/query?source=balance", nil))
	require.Equal(t, Healthy, manual.Outcome)
	require.Equal(t, []Balance{
		{Unit: "USD", Available: "100", Price: &WalletPrice{Multiplier: "2", Source: "configured", Version: 1}},
		{Unit: "CNY", Available: "50", Price: &WalletPrice{Multiplier: "1", Source: "default"}},
		{Unit: "TOKENS", Available: "0", Price: &WalletPrice{Source: "unset"}},
	}, manual.Balances)

	stored = StoredSite{}
	_, err = p.read(ctx, "sites", "1", &stored)
	require.NoError(t, err)
	before := stored.Site
	require.Equal(t, 1, before.State.Counters["balance"].Successes)
	require.Empty(t, before.State.Reasons)
	require.True(t, before.State.Manual)
	changed := readPricingResponse[Site](t, savePricingConfig(t, p, "PUT", "/sites/1", SaveRequest{Config: before.Config, Version: before.ConfigVersion}, `{"USD":"4"}`))
	require.Equal(t, before.State, changed.State)
	require.Equal(t, before.WalletEpoch, changed.WalletEpoch)
	require.Equal(t, before.StatisticsEpoch, changed.StatisticsEpoch)
	require.Equal(t, before.NextDue, changed.NextDue)
	require.Equal(t, before.Latest, changed.Latest)
	require.Equal(t, before.LastBalanceObservation, changed.LastBalanceObservation)
	require.Equal(t, WalletPrice{Multiplier: "4", Source: "configured", Version: 2}, changed.PriceFor("USD"))

	require.Eventually(t, func() bool { return time.Now().UTC().After(manual.At) }, time.Second, time.Millisecond)
	responses <- `{"balance_infos":[{"currency":"USD","total_balance":"80"}]}`
	require.NoError(t, p.RunTask(ctx, "monitor", changed.NextDue["balance"].Add(time.Second)))
	list := readPricingResponse[struct{ Sites []Site }](t, call(t, p, "GET", "/sites", nil))
	scheduled := list.Sites[0]
	require.Equal(t, "80", scheduled.Latest.Balances[0].Available)
	require.Equal(t, changed.PriceFor("USD"), *scheduled.Latest.Balances[0].Price)
	require.Equal(t, scheduled.Latest, scheduled.LastBalanceObservation)
	require.Equal(t, "health", scheduled.LastObservation.Source)
	require.Contains(t, scheduled.State.Reasons, "balance_low")
	require.True(t, scheduled.State.Manual)
	events := readPricingResponse[[]struct{ Kind, Unit, Value string }](t, call(t, p, "GET", "/sites/1/alerts", nil))
	require.Len(t, events, 2)
	values := map[string]string{}
	for _, event := range events {
		require.Equal(t, "USD", event.Unit)
		values[event.Kind] = event.Value
	}
	location, err := time.LoadLocation(c.Timezone)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"low_balance": "80", "daily_" + scheduled.Latest.At.In(location).Format(time.DateOnly): "40"}, values)

	responses <- ""
	failed := readPricingResponse[Observation](t, call(t, p, "POST", "/sites/1/query", nil))
	require.Equal(t, Authentication, failed.Outcome)
	require.Empty(t, failed.Balances)
	probe := readPricingResponse[Observation](t, call(t, p, "POST", "/sites/1/query?source=health", nil))
	require.Equal(t, Healthy, probe.Outcome)
	require.Empty(t, probe.Balances)
	restarted := &Plugin{}
	require.NoError(t, restarted.Init(ctx, h.Env))
	stored = StoredSite{}
	_, err = restarted.read(ctx, "sites", "1", &stored)
	require.NoError(t, err)
	require.Equal(t, failed.RunID, stored.Site.LastBalanceObservation.RunID)
	require.Equal(t, probe.RunID, stored.Site.LastObservation.RunID)
	require.Equal(t, scheduled.Latest.RunID, stored.Site.Latest.RunID)
	history := readPricingResponse[struct{ Items []Observation }](t, call(t, restarted, "GET", "/sites/1/history?source=balance", nil))
	require.Len(t, history.Items, 4)
	balances := make(map[string][]Balance, len(history.Items))
	for _, item := range history.Items {
		balances[item.RunID] = item.Balances
	}
	require.Equal(t, legacy.Balances, balances[legacy.RunID])
	require.Equal(t, manual.Balances, balances[manual.RunID])
	require.Equal(t, scheduled.Latest.Balances, balances[scheduled.Latest.RunID])
	require.Contains(t, balances, failed.RunID)
	require.Empty(t, balances[failed.RunID])

	restarted.bridge = pricingTestBridge{}
	site = readPricingResponse[Site](t, savePricingConfig(t, restarted, "PUT", "/sites/1", SaveRequest{Config: changed.Config, Version: changed.ConfigVersion}, `{"USD":"2"}`))
	require.Equal(t, WalletPrice{Multiplier: "2", Source: "configured", Version: 3}, site.PriceFor("USD"))
	require.Equal(t, changed.WalletEpoch, site.WalletEpoch)
	require.Equal(t, failed.RunID, site.LastBalanceObservation.RunID)
	site = readPricingResponse[Site](t, savePricingConfig(t, restarted, "PUT", "/sites/1", SaveRequest{Config: site.Config, Version: site.ConfigVersion, ClearKey: true}, ""))
	require.Equal(t, WalletPrice{Multiplier: "2", Source: "configured", Version: 3}, site.PriceFor("USD"))
	require.Equal(t, changed.WalletEpoch+1, site.WalletEpoch)
	require.Nil(t, site.LastBalanceObservation)
	require.Nil(t, site.Latest)
	require.Empty(t, site.State.Counters)
	require.True(t, site.State.Manual)
}

func TestBalancePricingRejectsInFlightObservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, _ := testPlugin(t)
	entered, release := make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte(`{"isValid":true,"unit":"USD","balance":"9.99"}`))
	}))
	defer server.Close()
	defer finish()
	p.client = server.Client()
	site := addSite(t, p, server.URL)
	done := make(chan error, 1)
	go func() {
		_, err := p.query(ctx, site.ID, "balance")
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("wallet request did not start")
	}
	req := SaveRequest{Config: site.Config, Version: site.ConfigVersion}
	require.Equal(t, http.StatusOK, savePricingConfig(t, p, "PUT", "/sites/1", req, `{"USD":"0.5"}`).Code)
	finish()
	require.ErrorIs(t, <-done, ErrConflict)
	history, err := p.history(ctx, site.ID)
	require.NoError(t, err)
	require.Empty(t, history)
	var stored StoredSite
	_, err = p.read(ctx, "sites", "1", &stored)
	require.NoError(t, err)
	require.Nil(t, stored.Site.Latest)
	require.Nil(t, stored.Site.LastBalanceObservation)
}

func TestNewAPIBalanceSamplingBasis(t *testing.T) {
	p, _ := testPlugin(t)
	var format atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body string
		switch r.URL.Path {
		case "/api/status":
			body = `{"success":true,"data":{"quota_display_type":"USD","quota_per_unit":"500000.0","usd_exchange_rate":7}}`
			if format.Load() > 0 {
				body = `{"success":true,"data":{"quota_display_type":"USD","quota_per_unit":"1000000.00","usd_exchange_rate":8}}`
			}
		case "/api/usage/token/":
			body = `{"data":{"user_balance":5000000}}`
			if format.Load() == 2 {
				body = `{"data":{"user_balance_display":"5","currency":"USD","user_balance":5000000}}`
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	p.client = server.Client()
	c := DefaultConfig()
	c.Adapter, c.BaseURL = "new_api", server.URL
	req := SaveRequest{Config: c, Secrets: &Secrets{Key: "fixture"}}
	readPricingResponse[Site](t, savePricingConfig(t, p, "POST", "/sites", req, `{"USD":"2"}`))
	for step, want := range []Balance{
		{Unit: "USD", Available: "10", Basis: "token.data.user_balance:raw:quota_per_unit=500000"},
		{Unit: "USD", Available: "5", Basis: "token.data.user_balance:raw:quota_per_unit=1000000"},
		{Unit: "USD", Available: "5", Basis: "token.data.user_balance_display:display"},
	} {
		format.Store(int32(step))
		got := readPricingResponse[Observation](t, call(t, p, "POST", "/sites/1/query", nil))
		require.Equal(t, Healthy, got.Outcome)
		want.Price = &WalletPrice{Multiplier: "2", Source: "configured", Version: 1}
		require.Equal(t, []Balance{want}, got.Balances)
	}
}

func TestNewAPIBalanceBasisUsesOnlyAppliedSettings(t *testing.T) {
	cny := newAPISettings{"CNY", decimal.RequireFromString("500000.0"), decimal.RequireFromString("7.30")}
	token := newAPISettings{"TOKENS", cny.factor, cny.rate}
	for _, tc := range []struct {
		name, body, basis string
		settings          newAPISettings
		parse             func([]byte, newAPISettings) ([]Balance, error)
	}{
		{"root", `{"balance":"4"}`, "compatible.balance:display", cny, newAPICompatible},
		{"root list", `{"balance_infos":[{"balance":"4"}]}`, "compatible.balance_infos.balance:display", cny, newAPICompatible},
		{"nested list", `{"data":{"balance_infos":[{"total_balance":"4"}]}}`, "compatible.data.balance_infos.total_balance:display", cny, newAPICompatible},
		{"nested raw", `{"data":{"quota":500000}}`, "compatible.data.quota:raw:quota_per_unit=500000:usd_exchange_rate=7.3", cny, newAPICompatible},
		{"self display", `{"data":{"quota":"4","currency":"CNY"}}`, "self.data.quota:display", cny, parseSelfBasis},
		{"self token", `{"data":{"quota":"4"}}`, "self.data.quota:raw", token, parseSelfBasis},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.parse([]byte(tc.body), tc.settings)
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Equal(t, tc.basis, got[0].Basis)
		})
	}
}

func parseSelfBasis(body []byte, settings newAPISettings) ([]Balance, error) {
	return newAPISelf(body, settings, "")
}
