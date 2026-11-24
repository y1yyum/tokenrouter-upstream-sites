package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/TokenFlux/TokenRouter/plugin-sdk/go/pluginpb/v1"
	"github.com/TokenFlux/TokenRouter/plugin-sdk/go/sdk"
	"github.com/stretchr/testify/require"
)

type reportKV struct {
	KVStore
	writes       int
	historyLists int
}

func (kv *reportKV) Put(ctx context.Context, collection, key string, value any, options ...sdk.PutOption) (int64, error) {
	kv.writes++
	return kv.KVStore.Put(ctx, collection, key, value, options...)
}

func (kv *reportKV) Delete(ctx context.Context, collection, key string, version int64) (bool, error) {
	kv.writes++
	return kv.KVStore.Delete(ctx, collection, key, version)
}

func (kv *reportKV) List(ctx context.Context, request *pb.KVListRequest) (*pb.KVListResponse, error) {
	if request.Collection == "history" {
		kv.historyLists++
	}
	return kv.KVStore.List(ctx, request)
}

func acceptanceSite(t *testing.T, p *Plugin, base, adapter, timezone string, multipliers BalanceMultipliers) Site {
	t.Helper()
	config := DefaultConfig()
	config.BaseURL, config.Adapter, config.Timezone = base, adapter, timezone
	config.BalanceMultipliers = multipliers
	return readPricingResponse[Site](t, call(t, p, http.MethodPost, "/sites", SaveRequest{Config: config, Secrets: &Secrets{Key: "acceptance-wallet"}}))
}

func acceptanceQuery(t *testing.T, p *Plugin, siteID int64) Observation {
	t.Helper()
	row := readPricingResponse[Observation](t, call(t, p, http.MethodPost, fmt.Sprintf("/sites/%d/query", siteID), nil))
	require.Equal(t, Healthy, row.Outcome)
	return row
}

func acceptanceDaily(t *testing.T, p *Plugin, siteID int64) []DailyStat {
	t.Helper()
	return readPricingResponse[[]DailyStat](t, call(t, p, http.MethodGet, fmt.Sprintf("/sites/%d/daily", siteID), nil))
}

func acceptanceDay(t *testing.T, rows []DailyStat, date, unit string, wallet, statistics int64) DailyStat {
	t.Helper()
	for _, row := range rows {
		if row.Date == date && row.Unit == unit && row.WalletEpoch == wallet && row.StatisticsEpoch == statistics {
			return row
		}
	}
	t.Fatalf("daily row missing for %s %s wallet=%d statistics=%d: %+v", date, unit, wallet, statistics, rows)
	return DailyStat{}
}

func acceptanceTimezone() string {
	if hour := time.Now().UTC().Hour(); hour < 2 || hour >= 21 {
		return "Pacific/Honolulu"
	}
	return "UTC"
}

func TestBalanceCostAcceptanceReportsAreReadOnly(t *testing.T) {
	p, h := testPlugin(t)
	var usd, cny, requests atomic.Int64
	usd.Store(120)
	cny.Store(60)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/usd/v1/usage":
			fmt.Fprintf(w, `{"isValid":true,"balance":"%d","unit":"USD"}`, usd.Load())
		case "/cny/v1/users/me/balance":
			fmt.Fprintf(w, `{"data":{"available_balance":"%d"}}`, cny.Load())
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)
	p.client = upstream.Client()
	zone := acceptanceTimezone()
	first := acceptanceSite(t, p, upstream.URL+"/usd", "sub2api", zone, BalanceMultipliers{"USD": "2"})
	second := acceptanceSite(t, p, upstream.URL+"/cny", "kimi_balance", zone, nil)
	unknown := acceptanceSite(t, p, upstream.URL, "sub2api", zone, nil)
	_, err := h.Env.Host.KV.Put(t.Context(), "meta", "bindings", map[string]int64{"11": first.ID, "12": first.ID})
	require.NoError(t, err)
	const report = "/statistics/overview?period=7d"
	before := readPricingResponse[overviewResponse](t, call(t, p, http.MethodGet, report, nil))
	require.Nil(t, before.Current.EstimatedBalanceCNY)
	require.Equal(t, 3, before.Current.UnknownSites)
	acceptanceQuery(t, p, first.ID)
	acceptanceQuery(t, p, second.ID)
	usd.Store(100)
	cny.Store(50)
	last := acceptanceQuery(t, p, first.ID)
	acceptanceQuery(t, p, second.ID)
	loc, err := time.LoadLocation(zone)
	require.NoError(t, err)
	date := last.At.In(loc).Format(time.DateOnly)
	inspected := &reportKV{KVStore: h.Env.Host.KV}
	p.kv = inspected
	readReports := func() overviewResponse {
		t.Helper()
		get := readPricingResponse[overviewResponse](t, call(t, p, http.MethodGet, report, nil))
		row := acceptanceDay(t, acceptanceDaily(t, p, first.ID), date, "USD", 1, 1)
		require.Equal(t, new("20"), row.UpstreamCost)
		require.Equal(t, new("10"), row.EstimatedCostCNY)
		require.Empty(t, acceptanceDaily(t, p, unknown.ID))
		list := call(t, p, http.MethodGet, "/sites", nil)
		require.Equal(t, http.StatusOK, list.Code, list.Body.String())
		require.Zero(t, inspected.writes)
		require.Zero(t, inspected.historyLists)
		require.EqualValues(t, 4, requests.Load())
		return get
	}
	result := readReports()
	require.Equal(t, []overviewCurrentUnit{{Unit: "CNY", Amount: "50", Sites: 1}, {Unit: "USD", Amount: "100", Sites: 1}}, result.Current.ByUnit)
	require.Equal(t, new("100"), result.Current.EstimatedBalanceCNY)
	require.Equal(t, 2, result.Current.PricedSites)
	require.Equal(t, 1, result.Current.UnknownSites)
	require.Len(t, result.Periods, 1)
	require.Equal(t, new("20"), result.Periods[0].EstimatedCostCNY)
	secondRead := readReports()
	require.Equal(t, result.Periods, secondRead.Periods)
	p.kv = h.Env.Host.KV
	first.Config.BalanceMultipliers = BalanceMultipliers{}
	readPricingResponse[Site](t, call(t, p, http.MethodPut, "/sites/1", SaveRequest{Config: first.Config, Version: first.ConfigVersion}))
	p.kv = inspected
	cleared := readReports()
	require.Equal(t, result.Current.ByUnit, cleared.Current.ByUnit)
	require.Equal(t, new("50"), cleared.Current.EstimatedBalanceCNY)
	require.Equal(t, 1, cleared.Current.UnpricedSites)
	require.Equal(t, result.Periods, cleared.Periods)
}

func TestBalanceCostAcceptanceNewAPINormalization(t *testing.T) {
	p, _ := testPlugin(t)
	var quota, factor atomic.Int64
	quota.Store(5_000_000)
	factor.Store(500_000)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status":
			fmt.Fprintf(w, `{"success":true,"data":{"quota_display_type":"USD","quota_per_unit":%d}}`, factor.Load())
		case "/api/usage/token/":
			fmt.Fprintf(w, `{"data":{"user_balance":%d}}`, quota.Load())
		default:
			t.Errorf("unexpected New API path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)
	p.client = upstream.Client()
	zone := acceptanceTimezone()
	loc, err := time.LoadLocation(zone)
	require.NoError(t, err)
	site := acceptanceSite(t, p, upstream.URL, "new_api", zone, BalanceMultipliers{"USD": "2"})
	first := acceptanceQuery(t, p, site.ID)
	require.Equal(t, "10", first.Balances[0].Available)
	date := first.At.In(loc).Format(time.DateOnly)
	day := func() DailyStat {
		t.Helper()
		return acceptanceDay(t, acceptanceDaily(t, p, site.ID), date, "USD", 1, 1)
	}
	require.Nil(t, day().EstimatedCostCNY)
	factor.Store(1_000_000)
	require.Equal(t, "5", acceptanceQuery(t, p, site.ID).Balances[0].Available)
	require.Nil(t, day().UpstreamCost)
	require.Nil(t, day().EstimatedCostCNY)
	quota.Store(4_000_000)
	require.Equal(t, "4", acceptanceQuery(t, p, site.ID).Balances[0].Available)
	require.Equal(t, new("0.5"), day().EstimatedCostCNY)

	second := acceptanceSite(t, p, upstream.URL, "new_api", zone, BalanceMultipliers{"USD": "2"})
	factor.Store(500_000)
	quota.Store(5_000_000)
	acceptanceQuery(t, p, second.ID)
	quota.Store(4_000_000)
	require.Equal(t, "8", acceptanceQuery(t, p, second.ID).Balances[0].Available)
	secondDay := acceptanceDay(t, acceptanceDaily(t, p, second.ID), date, "USD", 1, 1)
	require.Equal(t, new("2"), secondDay.UpstreamCost)
	require.Equal(t, new("1"), secondDay.EstimatedCostCNY)
}

func TestBalanceCostAcceptanceKeepsPriceWalletAndCalendarBoundaries(t *testing.T) {
	p, _ := testPlugin(t)
	var amount atomic.Int64
	var cny atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unit := "USD"
		if cny.Load() {
			unit = "CNY"
		}
		fmt.Fprintf(w, `{"is_available":true,"balance":"%d","currency":%q}`, amount.Load(), unit)
	}))
	t.Cleanup(upstream.Close)
	p.client = upstream.Client()
	zone := acceptanceTimezone()
	site := acceptanceSite(t, p, upstream.URL, "zivv", zone, BalanceMultipliers{"USD": "2"})
	query := func(balance int64) Observation {
		t.Helper()
		amount.Store(balance)
		return acceptanceQuery(t, p, site.ID)
	}
	save := func(secrets *Secrets) {
		t.Helper()
		site = readPricingResponse[Site](t, call(t, p, http.MethodPut, "/sites/1", SaveRequest{Config: site.Config, Version: site.ConfigVersion, Secrets: secrets}))
	}
	get := func(sample Observation, unit string) DailyStat {
		t.Helper()
		loc, err := time.LoadLocation(sample.Timezone)
		require.NoError(t, err)
		return acceptanceDay(t, acceptanceDaily(t, p, site.ID), sample.At.In(loc).Format(time.DateOnly), unit, sample.WalletEpoch, sample.StatisticsEpoch)
	}
	query(100)
	query(80)
	site.Config.BalanceMultipliers = BalanceMultipliers{"USD": "4"}
	save(nil)
	query(72)
	usd := query(64)
	row := get(usd, "USD")
	require.Equal(t, new("36"), row.UpstreamCost)
	require.Equal(t, new("12"), row.EstimatedCostCNY)
	require.Equal(t, new("8"), row.UnpricedDrop)
	require.Equal(t, 2, row.PricedIntervals)
	require.Equal(t, 3, row.ComparableIntervals)
	require.Equal(t, map[string]int{"price_changed": 1}, row.UnpricedReasons)
	cny.Store(true)
	require.Nil(t, get(query(50), "CNY").EstimatedCostCNY)
	require.Equal(t, new("0"), get(query(50), "CNY").EstimatedCostCNY)
	if zone == "UTC" {
		site.Config.Timezone = "Africa/Johannesburg"
	} else {
		site.Config.Timezone = "America/Los_Angeles"
	}
	save(nil)
	calendar := query(40)
	require.EqualValues(t, 2, calendar.StatisticsEpoch)
	require.Nil(t, get(calendar, "CNY").UpstreamCost)
	save(&Secrets{Key: "replacement-wallet"})
	wallet := query(30)
	require.EqualValues(t, 2, wallet.WalletEpoch)
	require.Nil(t, get(wallet, "CNY").UpstreamCost)
	require.Equal(t, new("1"), get(query(29), "CNY").EstimatedCostCNY)
	require.Equal(t, row, get(usd, "USD"))
	overview := readPricingResponse[overviewResponse](t, call(t, p, http.MethodGet, "/statistics/overview?period=7d", nil))
	require.Len(t, overview.Periods, 1)
	require.Equal(t, site.Config.Timezone, overview.Periods[0].Timezone)
	require.Equal(t, new("1"), overview.Periods[0].EstimatedCostCNY)
	require.Len(t, overview.Periods[0].ByUnit, 1)
	require.Equal(t, "CNY", overview.Periods[0].ByUnit[0].Unit)
}

func TestBalanceCostAcceptancePreservesPartialLegacyDay(t *testing.T) {
	p, h := testPlugin(t)
	site := acceptanceSite(t, p, "https://example.com", "sub2api", "UTC", BalanceMultipliers{"USD": "2"})
	day := time.Now().UTC().AddDate(0, 0, -2).Truncate(24 * time.Hour)
	for i, amount := range []string{"130", "120"} {
		sample := Observation{
			RunID: amount, At: day.Add(time.Duration(i+3) * time.Hour), Source: "balance", Outcome: Healthy,
			ConfigVersion: 1, WalletEpoch: 1, StatisticsEpoch: 1, Timezone: "UTC", IntervalMinutes: 60,
			Balances: []Balance{{Unit: "USD", Available: amount}},
		}
		_, err := h.Env.Host.KV.Put(t.Context(), "history", fmtSource(site.ID, sample.At.Format(time.RFC3339Nano)+":"+sample.RunID), sample)
		require.NoError(t, err)
	}
	date := day.Format(time.DateOnly)
	legacy := json.RawMessage(fmt.Sprintf(`[{"algorithm_version":1,"date":%q,"timezone":"UTC","unit":"USD","wallet_epoch":1,"statistics_epoch":1,"estimated_spend":"30","account_spend":null,"net_change":"20","covered_seconds":10800,"gap":false,"cross_day":false,"available_only":true}]`, date))
	_, err := h.Env.Host.KV.Put(t.Context(), "daily", fmtSource(site.ID, date), legacy)
	require.NoError(t, err)
	for range 2 {
		p = &Plugin{}
		require.NoError(t, p.Init(t.Context(), h.Env))
		row := acceptanceDay(t, acceptanceDaily(t, p, site.ID), date, "USD", 1, 1)
		require.Equal(t, new("30"), row.EstimatedSpend, "the surviving 130 → 120 cannot replace the complete legacy day")
		require.Equal(t, new("20"), row.NetChange)
		require.EqualValues(t, 10_800, row.CoveredSeconds)
		require.Nil(t, row.EstimatedCostCNY)
		require.Nil(t, row.OpenBalance)
		require.Nil(t, row.MinBalance)
	}
}

func TestBalanceCostAcceptanceKeepsUncountedLegacyCoverageOnSameDay(t *testing.T) {
	p, h := testPlugin(t)
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"isValid":true,"balance":"%d","unit":"USD"}`, 130-10*requests.Add(1))
	}))
	t.Cleanup(upstream.Close)
	zone := acceptanceTimezone()
	site := acceptanceSite(t, p, upstream.URL, "sub2api", zone, BalanceMultipliers{"USD": "2"})
	loc, err := time.LoadLocation(zone)
	require.NoError(t, err)
	date := time.Now().In(loc).Format(time.DateOnly)
	legacy := DailyStat{
		AlgorithmVersion: 1, Date: date, Timezone: zone, Unit: "USD",
		WalletEpoch: site.WalletEpoch, StatisticsEpoch: site.StatisticsEpoch,
		EstimatedSpend: new("30"), NetChange: new("20"), CoveredSeconds: 10800,
	}
	_, err = h.Env.Host.KV.Put(t.Context(), "daily", fmtSource(site.ID, date), []DailyStat{legacy})
	require.NoError(t, err)
	p = &Plugin{}
	require.NoError(t, p.Init(t.Context(), h.Env))
	p.client = upstream.Client()
	acceptanceQuery(t, p, site.ID)
	acceptanceQuery(t, p, site.ID)
	row := acceptanceDay(t, acceptanceDaily(t, p, site.ID), date, "USD", site.WalletEpoch, site.StatisticsEpoch)
	require.Equal(t, new("40"), row.EstimatedSpend)
	require.Equal(t, new("5"), row.EstimatedCostCNY)
	require.Equal(t, pricingPartial, row.PricingStatus)
	require.True(t, row.HasUnknownCoverage)
	require.Equal(t, 1, row.PricedIntervals)
	require.Equal(t, 1, row.ComparableIntervals)

	overview := readPricingResponse[overviewResponse](t, call(t, p, http.MethodGet, "/statistics/overview", nil))
	require.Len(t, overview.Sites, 1)
	require.NotNil(t, overview.Sites[0].Period)
	require.Len(t, overview.Periods, 1)
	for _, period := range []overviewPeriod{*overview.Sites[0].Period, overview.Periods[0]} {
		require.Equal(t, row.PricingStatus, period.PricingStatus)
		require.Equal(t, row.EstimatedCostCNY, period.EstimatedCostCNY)
		require.True(t, period.HasUnknownCoverage)
		require.Len(t, period.ByUnit, 1)
		unit := period.ByUnit[0]
		require.Equal(t, row.Unit, unit.Unit)
		require.Equal(t, row.PricingStatus, unit.PricingStatus)
		require.Equal(t, row.EstimatedCostCNY, unit.EstimatedCostCNY)
		require.Equal(t, row.PricedIntervals, unit.PricedIntervals)
		require.Equal(t, row.ComparableIntervals, unit.ComparableIntervals)
		require.Equal(t, row.UnpricedDrop, unit.UnpricedDrop)
		require.True(t, unit.HasUnknownCoverage)
	}
	require.EqualValues(t, 2, requests.Load())
}

func TestBalanceCostAcceptanceRetainsCrossDayPrecisionWithoutRawHistory(t *testing.T) {
	p, h := testPlugin(t)
	site := acceptanceSite(t, p, "https://example.com", "sub2api", "UTC", BalanceMultipliers{"USD": "2", "TOKENS": "1"})
	midnight := time.Now().UTC().AddDate(0, 0, -2).Truncate(24 * time.Hour)
	keys := []string{}
	for _, sample := range []Observation{
		{At: midnight.Add(-10 * time.Minute), RunID: "before", Balances: []Balance{
			{Unit: "USD", Available: "100", Price: &WalletPrice{Multiplier: "2", Version: 1}},
			{Unit: "TOKENS", Available: "0.0000000001", Price: &WalletPrice{Multiplier: "1", Version: 1}},
		}},
		{At: midnight.Add(20 * time.Minute), RunID: "after", Balances: []Balance{
			{Unit: "USD", Available: "88", Price: &WalletPrice{Multiplier: "2", Version: 1}},
			{Unit: "TOKENS", Available: "0", Price: &WalletPrice{Multiplier: "1", Version: 1}},
		}},
	} {
		sample.Source, sample.Outcome, sample.Timezone, sample.IntervalMinutes = "balance", Healthy, "UTC", 60
		sample.ConfigVersion, sample.WalletEpoch, sample.StatisticsEpoch = 1, 1, 1
		key := fmtSource(site.ID, sample.At.Format(time.RFC3339Nano)+":"+sample.RunID)
		_, err := h.Env.Host.KV.Put(t.Context(), "history", key, sample)
		require.NoError(t, err)
		keys = append(keys, key)
	}
	p = &Plugin{}
	require.NoError(t, p.Init(t.Context(), h.Env))
	days := acceptanceDaily(t, p, site.ID)
	previous, current := midnight.AddDate(0, 0, -1).Format(time.DateOnly), midnight.Format(time.DateOnly)
	require.Equal(t, new("2"), acceptanceDay(t, days, previous, "USD", 1, 1).EstimatedCostCNY)
	require.Equal(t, new("4"), acceptanceDay(t, days, current, "USD", 1, 1).EstimatedCostCNY)
	require.Equal(t, new("0"), acceptanceDay(t, days, previous, "TOKENS", 1, 1).EstimatedCostCNY)
	require.Equal(t, new("0.0000000001"), acceptanceDay(t, days, current, "TOKENS", 1, 1).EstimatedCostCNY)
	for _, key := range keys {
		deleted, err := h.Env.Host.KV.Delete(t.Context(), "history", key, 0)
		require.NoError(t, err)
		require.True(t, deleted)
	}
	p = &Plugin{}
	require.NoError(t, p.Init(t.Context(), h.Env))
	require.Equal(t, days, acceptanceDaily(t, p, site.ID))
	overview := readPricingResponse[overviewResponse](t, call(t, p, http.MethodGet, "/statistics/overview?period=7d", nil))
	require.Len(t, overview.Periods, 1)
	require.Equal(t, new("6.0000000001"), overview.Periods[0].EstimatedCostCNY)
}
