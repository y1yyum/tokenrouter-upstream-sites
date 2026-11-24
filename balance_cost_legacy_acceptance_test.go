package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/plugin-sdk/go/sdk"
	"github.com/stretchr/testify/require"
)

func putAcceptanceLegacyHistory(t *testing.T, kv sdk.KV, siteID int64, sample Observation) {
	t.Helper()
	key := fmtSource(siteID, sample.At.UTC().Format(time.RFC3339Nano)+":"+sample.RunID)
	_, err := kv.Put(t.Context(), "history", key, sample, sdk.IfAbsent())
	require.NoError(t, err)
}

func putAcceptanceLegacyDay(t *testing.T, kv sdk.KV, site Site, date, spend, net string, covered int64, crossDay bool) {
	t.Helper()
	legacy := []map[string]any{{
		"algorithm_version": 1, "date": date, "timezone": site.Config.Timezone, "unit": "USD",
		"wallet_epoch": site.WalletEpoch, "statistics_epoch": site.StatisticsEpoch,
		"estimated_spend": spend, "account_spend": nil, "net_change": net,
		"covered_seconds": covered, "gap": false, "cross_day": crossDay, "available_only": true,
	}}
	_, err := kv.Put(t.Context(), "daily", fmtSource(site.ID, date), legacy)
	require.NoError(t, err)
}

func TestBalanceCostAcceptanceLegacyOnlyDailyBalances(t *testing.T) {
	p, h := testPlugin(t)
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"isValid":true,"balance":"%d","unit":"USD"}`, 130-10*calls.Add(1))
	}))
	t.Cleanup(upstream.Close)
	site := acceptanceSite(t, p, upstream.URL, "sub2api", acceptanceTimezone(), BalanceMultipliers{"USD": "2"})
	loc, err := time.LoadLocation(site.Config.Timezone)
	require.NoError(t, err)
	date := time.Now().In(loc).Format(time.DateOnly)
	putAcceptanceLegacyDay(t, h.Env.Host.KV, site, date, "30", "20", 10_800, false)
	p = &Plugin{}
	require.NoError(t, p.Init(t.Context(), h.Env))
	p.client = upstream.Client()
	acceptanceQuery(t, p, site.ID)
	last := acceptanceQuery(t, p, site.ID)
	row := acceptanceDay(t, acceptanceDaily(t, p, site.ID), date, "USD", 1, 1)
	require.Equal(t, new("40"), row.EstimatedSpend)
	require.Equal(t, new("5"), row.EstimatedCostCNY)
	require.Equal(t, "partial", row.PricingStatus)
	require.Nil(t, row.OpenBalance)
	require.Nil(t, row.FirstSampleAt)
	require.Nil(t, row.MinBalance)
	require.Nil(t, row.MaxBalance)
	require.Equal(t, new("110"), row.CloseBalance)
	require.Equal(t, &last.At, row.LastSampleAt)
	week := readPricingResponse[overviewResponse](t, call(t, p, http.MethodGet, "/statistics/overview?period=7d", nil))
	require.Len(t, week.Periods, 1)
	require.Equal(t, row.PricingStatus, week.Periods[0].PricingStatus)
	require.Equal(t, row.EstimatedCostCNY, week.Periods[0].EstimatedCostCNY)
}

func TestBalanceCostAcceptanceRebuildsLegacyCrossDayRounding(t *testing.T) {
	p, h := testPlugin(t)
	site := acceptanceSite(t, p, "https://example.com", "sub2api", "UTC", nil)
	midnight := time.Now().UTC().AddDate(0, 0, -2).Truncate(24 * time.Hour)
	for i, amount := range []string{"1.0000000001", "1"} {
		at := midnight.Add(time.Duration(2*i-1) * 10 * time.Minute)
		sample := Observation{
			RunID: fmt.Sprint(i), At: at, Source: "balance", Outcome: Healthy, Code: "WALLET_OK", HTTPStatus: http.StatusOK,
			ConfigVersion: 1, WalletEpoch: 1, StatisticsEpoch: 1, Timezone: "UTC", IntervalMinutes: 60,
			Balances: []Balance{{Unit: "USD", Available: amount}},
		}
		putAcceptanceLegacyHistory(t, h.Env.Host.KV, site.ID, sample)
		// 1.1.1 将两个半段各自四舍五入，导致两天都记录了一个最小单位。
		putAcceptanceLegacyDay(t, h.Env.Host.KV, site, at.Format(time.DateOnly), "0.0000000001", "-0.0000000001", 600, true)
	}
	p = &Plugin{}
	require.NoError(t, p.Init(t.Context(), h.Env))
	rows := acceptanceDaily(t, p, site.ID)
	before := acceptanceDay(t, rows, midnight.AddDate(0, 0, -1).Format(time.DateOnly), "USD", 1, 1)
	after := acceptanceDay(t, rows, midnight.Format(time.DateOnly), "USD", 1, 1)
	require.Equal(t, new("0.0000000001"), before.UpstreamCost)
	require.Equal(t, new("0"), after.UpstreamCost)
	require.Equal(t, new("1.0000000001"), before.OpenBalance)
	require.Equal(t, new("1"), after.OpenBalance)
	require.Nil(t, before.EstimatedCostCNY)
	require.Nil(t, after.EstimatedCostCNY)
}

func TestBalanceCostAcceptanceReopensPricingAfterLegacyFailure(t *testing.T) {
	p, h := testPlugin(t)
	var amount atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"isValid":true,"balance":"%d","unit":"USD"}`, amount.Load())
	}))
	t.Cleanup(upstream.Close)
	p.client = upstream.Client()
	site := acceptanceSite(t, p, upstream.URL, "sub2api", acceptanceTimezone(), BalanceMultipliers{"USD": "2"})
	query := func(value int64) Observation {
		t.Helper()
		amount.Store(value)
		return acceptanceQuery(t, p, site.ID)
	}
	first, last := query(100), query(80)
	loc, err := time.LoadLocation(site.Config.Timezone)
	require.NoError(t, err)
	date := last.At.In(loc).Format(time.DateOnly)
	daily := func() DailyStat {
		t.Helper()
		return acceptanceDay(t, acceptanceDaily(t, p, site.ID), date, "USD", 1, 1)
	}
	require.Equal(t, new("10"), daily().EstimatedCostCNY)

	// 1.1.1 写入 401 历史、旧日报并用旧结构重写站点。成功余额仍是 80，旧结构不含任何价格快照。
	failed := last
	failed.At, failed.RunID = time.Now().UTC(), "legacy-401"
	failed.Outcome, failed.Code, failed.HTTPStatus = Authentication, "UPSTREAM_AUTHENTICATION_FAILED", http.StatusUnauthorized
	failed.Balances = nil
	last.Balances = []Balance{{Unit: "USD", Available: "80"}}
	kv := h.Env.Host.KV
	putAcceptanceLegacyHistory(t, kv, site.ID, failed)
	putAcceptanceLegacyDay(t, kv, site, date, "20", "-20", int64(last.At.Sub(first.At)/time.Second), false)
	key := fmt.Sprint(site.ID)
	current, err := kv.Get(t.Context(), "sites", key)
	require.NoError(t, err)
	require.True(t, current.Found)
	var stored struct {
		Ciphertext string `json:"ciphertext"`
		Site       struct {
			State State `json:"state"`
		} `json:"site"`
	}
	require.NoError(t, json.Unmarshal(current.ValueJson, &stored))
	legacyConfig := site.Config
	legacyConfig.BalanceMultipliers = nil
	legacy := map[string]any{
		"ciphertext": stored.Ciphertext,
		"site": map[string]any{
			"id": site.ID, "config": legacyConfig, "config_version": site.ConfigVersion,
			"wallet_epoch": site.WalletEpoch, "statistics_epoch": site.StatisticsEpoch,
			"archived": false, "has_key": true, "has_proxy": false, "provider_count": 0, "today": nil,
			"state": Evaluate(legacyConfig, stored.Site.State, failed), "latest": last, "last_observation": failed,
			"next_due": map[string]time.Time{"balance": failed.At.Add(time.Hour)},
		},
	}
	_, err = kv.Put(t.Context(), "sites", key, legacy, sdk.IfVersion(current.Version))
	require.NoError(t, err)

	h.Env.PreviousVersion = "1.1.1"
	p = &Plugin{}
	require.NoError(t, p.Init(t.Context(), h.Env))
	p.client = upstream.Client()
	path := fmt.Sprintf("/sites/%d", site.ID)
	readPricingResponse[Site](t, call(t, p, http.MethodPut, path, SaveRequest{Config: site.Config, Version: site.ConfigVersion}))
	query(72)
	row := daily()
	require.Equal(t, new("28"), row.UpstreamCost)
	require.Equal(t, new("10"), row.EstimatedCostCNY)
	require.Equal(t, new("8"), row.UnpricedDrop)
	query(64)
	row = daily()
	require.Equal(t, new("36"), row.UpstreamCost)
	require.Equal(t, new("14"), row.EstimatedCostCNY)
	require.Equal(t, new("8"), row.UnpricedDrop)
	history := readPricingResponse[struct {
		Items []Observation `json:"items"`
		Total int           `json:"total"`
	}](t, call(t, p, http.MethodGet, path+"/history", nil))
	require.Equal(t, 5, history.Total)
	require.Len(t, history.Items, 5)
	require.Equal(t, failed, history.Items[2])
}
