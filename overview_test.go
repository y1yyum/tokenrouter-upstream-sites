package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/plugin-sdk/go/manifest"
	pb "github.com/TokenFlux/TokenRouter/plugin-sdk/go/pluginpb/v1"
	"github.com/TokenFlux/TokenRouter/plugin-sdk/go/sdk"
	"github.com/stretchr/testify/require"
)

func TestOverviewValuesLatestAndKeepsHistoricalPrices(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	oldPrice := WalletPrice{Multiplier: "2", Source: walletPriceConfigured, Version: 1}
	newPrice := WalletPrice{Multiplier: "4", Source: walletPriceConfigured, Version: 2}
	usd := []Observation{
		pricedStatSample(now.Add(-2*time.Hour), "136", oldPrice),
		pricedStatSample(now.Add(-90*time.Minute), "116", oldPrice),
		pricedStatSample(now.Add(-time.Hour), "108", newPrice),
		pricedStatSample(now.Add(-30*time.Minute), "100", newPrice),
	}
	first := overviewFixtureSite(1, "UTC", &usd[3])
	first.ProviderCount = 2
	first.Config.BalanceMultipliers = BalanceMultipliers{"USD": "2"}

	start, end := statSample(now.Add(-4*time.Hour), ""), statSample(now, "")
	cnyPrice := WalletPrice{Multiplier: "1", Source: walletPriceDefault}
	start.Balances = []Balance{{Unit: "CNY", Available: "60", Price: &cnyPrice}, {Unit: "TOKENS", Available: "13"}}
	end.Balances = []Balance{{Unit: "CNY", Available: "50", Price: &cnyPrice}, {Unit: "TOKENS", Available: "10"}}
	second := overviewFixtureSite(2, "UTC", &end)
	q, err := newOverviewQuery("", "")
	require.NoError(t, err)
	get := func() overviewResponse {
		o := newOverviewTotals(now, q)
		o.addSite(first, DailyStats(usd))
		o.addSite(second, DailyStats([]Observation{start, end}))
		return o.result()
	}

	before := get()
	require.Equal(t, "today", before.Period)
	require.Equal(t, now, before.AsOf)
	require.Equal(t, []overviewCurrentUnit{{"CNY", "50", 1}, {"TOKENS", "10", 1}, {"USD", "100", 1}}, before.Current.ByUnit)
	require.Equal(t, new("100"), before.Current.EstimatedBalanceCNY)
	require.Equal(t, 2, before.Current.FreshSites)
	require.Equal(t, 2, before.Current.PricedSites)
	require.Equal(t, 1, before.Current.UnpricedSites)
	require.Equal(t, pricingPartial, before.Sites[1].BalancePricingStatus)
	require.Equal(t, []overviewBalance{
		{Unit: "CNY", Available: "50", Multiplier: new("1"), Source: walletPriceDefault, BalanceCNY: new("50")},
		{Unit: "TOKENS", Available: "10", Source: walletPriceUnset},
	}, before.Sites[1].Balances)

	first.Config.BalanceMultipliers["USD"] = "4"
	after := get()
	require.Equal(t, new("75"), after.Current.EstimatedBalanceCNY)
	require.Equal(t, new("25"), after.Sites[0].Balances[0].BalanceCNY)
	require.Equal(t, before.Periods, after.Periods)
	require.Equal(t, before.Sites[0].Period, after.Sites[0].Period)
	require.Len(t, after.Periods, 1)
	period := after.Periods[0]
	require.Equal(t, new("22"), period.EstimatedCostCNY)
	require.Equal(t, new("10"), period.GapCostCNY)
	require.Equal(t, pricingPartial, period.PricingStatus)
	require.Equal(t, new("12"), after.Sites[0].Period.EstimatedCostCNY)
	require.Equal(t, new("10"), after.Sites[1].Period.EstimatedCostCNY)
	require.Equal(t, pricingPartial, after.Sites[1].Period.PricingStatus)
	require.Equal(t, overviewUnitCost{
		Unit: "USD", UpstreamCost: new("36"), UnpricedDrop: new("8"), PricedIntervals: 2, ComparableIntervals: 3,
		UnpricedReasons: map[string]int{"price_changed": 1}, CoveredSeconds: 5400,
		overviewCost: overviewCost{EstimatedCostCNY: new("12"), GapCostCNY: new("0"), PricingStatus: pricingPartial},
	}, period.ByUnit[2])
	require.True(t, period.ByUnit[0].Gap)
	require.Equal(t, new("3"), period.ByUnit[1].UnpricedDrop)
	require.Nil(t, period.ByUnit[1].GapCostCNY)

	delete(first.Config.BalanceMultipliers, "USD")
	unset := get()
	require.Equal(t, before.Current.ByUnit, unset.Current.ByUnit)
	require.Equal(t, new("50"), unset.Current.EstimatedBalanceCNY)
	require.Equal(t, 1, unset.Current.PricedSites)
	require.Equal(t, 2, unset.Current.UnpricedSites)
	require.Equal(t, before.Periods, unset.Periods)
}

func TestOverviewSeparatesFreshnessTimezoneAndArchival(t *testing.T) {
	now := time.Date(2026, 11, 24, 17, 0, 0, 0, time.UTC)
	staleSample := statSample(now.Add(-2*time.Hour-time.Second), "20")
	freshSample := statSample(now.Add(-2*time.Hour), "50")
	stale, fresh := overviewFixtureSite(1, "UTC", &staleSample), overviewFixtureSite(2, "Asia/Shanghai", &freshSample)
	stale.Config.BalanceMultipliers = BalanceMultipliers{"USD": "2"}
	fresh.Config.BalanceMultipliers = BalanceMultipliers{"USD": "2"}
	fresh.LastBalanceObservation = &Observation{Source: "balance", At: now.Add(-time.Minute), Outcome: Authentication, Code: "WALLET_AUTH", Diagnostic: "GROUP_DISABLED"}
	fresh.LastObservation = &Observation{Source: "health", At: now, Outcome: Healthy}
	archived := overviewFixtureSite(3, "UTC", &freshSample)
	archived.Archived = true
	unknown := overviewFixtureSite(4, "UTC", nil)
	unknown.LastObservation = &Observation{Source: "balance", At: now, Outcome: Unreachable}

	get := func(timezone string) overviewResponse {
		q, err := newOverviewQuery("today", timezone)
		require.NoError(t, err)
		o := newOverviewTotals(now, q)
		o.addSite(stale, []DailyStat{overviewFixtureDay("2026-11-24", "UTC", "8")})
		o.addSite(fresh, []DailyStat{overviewFixtureDay("2026-11-24", "Asia/Shanghai", "99"), overviewFixtureDay("2026-11-25", "Asia/Shanghai", "3")})
		o.addSite(archived, []DailyStat{overviewFixtureDay("2026-11-24", "UTC", "99")})
		o.addSite(unknown, nil)
		return o.result()
	}
	all := get("")
	require.Equal(t, overviewCurrent{
		ByUnit: []overviewCurrentUnit{{"USD", "50", 1}}, EstimatedBalanceCNY: new("25"),
		FreshSites: 1, PricedSites: 1, StaleSites: 1, UnknownSites: 1, ArchivedSites: 1,
	}, all.Current)
	require.Len(t, all.Sites, 3)
	require.Equal(t, "stale", all.Sites[0].Status)
	require.Equal(t, new("10"), all.Sites[0].Balances[0].BalanceCNY)
	require.Equal(t, "fresh", all.Sites[1].Status)
	require.Equal(t, &freshSample.At, all.Sites[1].SampledAt)
	require.Equal(t, Authentication, all.Sites[1].LastBalanceObservation.Outcome)
	require.Equal(t, "GROUP_DISABLED", all.Sites[1].LastBalanceObservation.Diagnostic)
	require.Equal(t, int64(4), all.Sites[2].SiteID)
	require.Equal(t, "unknown", all.Sites[2].Status)
	require.Equal(t, Unreachable, all.Sites[2].LastBalanceObservation.Outcome)
	require.Empty(t, all.Sites[2].Balances)
	require.Nil(t, all.Sites[2].Period.EstimatedCostCNY)
	require.Len(t, all.Periods, 2)
	require.Equal(t, "Asia/Shanghai", all.Periods[0].Timezone)
	require.Equal(t, new("2026-11-25"), all.Periods[0].From)
	require.Equal(t, new("3"), all.Periods[0].EstimatedCostCNY)
	require.Equal(t, "UTC", all.Periods[1].Timezone)
	require.Equal(t, new("2026-11-24"), all.Periods[1].To)
	require.Equal(t, new("8"), all.Periods[1].EstimatedCostCNY)

	filtered := get("UTC")
	require.Equal(t, all.Current, filtered.Current)
	require.Equal(t, []overviewPeriod{all.Periods[1]}, filtered.Periods)
	require.Nil(t, filtered.Sites[1].Period)
	require.Equal(t, all.Sites[1].Balances, filtered.Sites[1].Balances)
	missing := get("America/New_York")
	require.Equal(t, all.Current, missing.Current)
	require.Empty(t, missing.Periods)
}

func TestOverviewCalendarWindowsAndEpochs(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	site := overviewFixtureSite(1, "UTC", nil)
	rows := []DailyStat{
		overviewFixtureDay("2026-10-20", "UTC", "32"),
		overviewFixtureDay("2026-11-24", "UTC", "1"),
		overviewFixtureDay("2026-11-18", "UTC", "2"),
		overviewFixtureDay("2026-11-17", "UTC", "4"),
		overviewFixtureDay("2026-10-26", "UTC", "8"),
		overviewFixtureDay("2026-10-25", "UTC", "16"),
		overviewFixtureDay("2026-11-23", "Asia/Shanghai", "999"),
		overviewFixtureDay("2026-11-25", "UTC", "999"),
	}
	oldWallet, oldStatistics := overviewFixtureDay("2026-11-24", "UTC", "999"), overviewFixtureDay("2026-11-24", "UTC", "999")
	oldWallet.WalletEpoch--
	oldStatistics.StatisticsEpoch--
	rows = append(rows, oldWallet, oldStatistics)
	for _, tt := range []struct{ period, from, cost string }{
		{"today", "2026-11-24", "1"}, {"7d", "2026-11-18", "3"},
		{"30d", "2026-10-26", "15"}, {"retained", "2026-10-20", "63"},
	} {
		t.Run(tt.period, func(t *testing.T) {
			q, err := newOverviewQuery(tt.period, "")
			require.NoError(t, err)
			o := newOverviewTotals(now, q)
			o.addSite(site, rows)
			out := o.result()
			require.Len(t, out.Periods, 1)
			period := out.Periods[0]
			require.Equal(t, &period, out.Sites[0].Period)
			require.Equal(t, &tt.from, period.From)
			require.Equal(t, new("2026-11-24"), period.To)
			require.Equal(t, &tt.cost, period.EstimatedCostCNY)
		})
	}
	q, err := newOverviewQuery("retained", "")
	require.NoError(t, err)
	o := newOverviewTotals(now, q)
	o.addSite(site, rows[:1])
	period := o.result().Periods[0]
	require.Equal(t, new("2026-10-20"), period.From)
	require.Equal(t, period.From, period.To)
}

func TestOverviewPreservesUnknownLegacyCoverage(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	q, err := newOverviewQuery("7d", "")
	require.NoError(t, err)
	first, second := overviewFixtureSite(1, "UTC", nil), overviewFixtureSite(2, "UTC", nil)
	legacyBaseline := DailyStat{AlgorithmVersion: 1, Date: "2026-11-21", Timezone: "UTC", Unit: "USD", WalletEpoch: 1, StatisticsEpoch: 1}
	legacy := legacyBaseline
	legacy.Date, legacy.EstimatedSpend, legacy.NetChange = "2026-11-22", new("0"), new("0")
	legacyGap := legacyBaseline
	legacyGap.Date, legacyGap.NetChange, legacyGap.Gap = "2026-11-23", new("-8"), true
	modernBaseline := statSample(now.AddDate(0, 0, -2), "0")
	priced := overviewFixtureDay("2026-11-24", "UTC", "7")

	assertCoverage := func(t *testing.T, period overviewPeriod, status string, unknown bool, intervals int, cost *string) {
		t.Helper()
		require.Equal(t, status, period.PricingStatus)
		require.Equal(t, unknown, period.HasUnknownCoverage)
		require.Equal(t, cost, period.EstimatedCostCNY)
		require.Len(t, period.ByUnit, 1)
		unit := period.ByUnit[0]
		require.Equal(t, status, unit.PricingStatus)
		require.Equal(t, unknown, unit.HasUnknownCoverage)
		require.Equal(t, intervals, unit.PricedIntervals)
		require.Equal(t, intervals, unit.ComparableIntervals)
	}
	t.Run("only legacy intervals", func(t *testing.T) {
		o := newOverviewTotals(now, q)
		o.addSite(first, []DailyStat{legacyBaseline, legacy, legacyGap})
		out := o.result()
		assertCoverage(t, out.Periods[0], pricingUnknown, true, 0, nil)
		assertCoverage(t, *out.Sites[0].Period, pricingUnknown, true, 0, nil)
		require.Nil(t, out.Periods[0].ByUnit[0].UnpricedDrop)
		require.True(t, out.Periods[0].ByUnit[0].Gap)
	})
	t.Run("baseline rows only", func(t *testing.T) {
		o := newOverviewTotals(now, q)
		o.addSite(first, append(DailyStats([]Observation{modernBaseline}), legacyBaseline, priced))
		out := o.result()
		assertCoverage(t, out.Periods[0], pricingPriced, false, 1, new("7"))
		assertCoverage(t, *out.Sites[0].Period, pricingPriced, false, 1, new("7"))
	})
	t.Run("across days", func(t *testing.T) {
		o := newOverviewTotals(now, q)
		o.addSite(first, []DailyStat{legacy, priced})
		out := o.result()
		assertCoverage(t, out.Periods[0], pricingPartial, true, 1, new("7"))
		assertCoverage(t, *out.Sites[0].Period, pricingPartial, true, 1, new("7"))

		q, err := newOverviewQuery("today", "")
		require.NoError(t, err)
		o = newOverviewTotals(now, q)
		o.addSite(first, []DailyStat{legacy, priced})
		assertCoverage(t, o.result().Periods[0], pricingPriced, false, 1, new("7"))
	})
	t.Run("explicit partial with equal counts", func(t *testing.T) {
		merged := priced
		merged.AlgorithmVersion = 1
		merged.EstimatedSpend = new("40")
		merged.PricingStatus = pricingPartial
		o := newOverviewTotals(now, q)
		o.addSite(first, []DailyStat{merged})
		out := o.result()
		assertCoverage(t, *out.Sites[0].Period, pricingPartial, true, 1, new("7"))
		assertCoverage(t, out.Periods[0], pricingPartial, true, 1, new("7"))
	})
	t.Run("across sites and midnight", func(t *testing.T) {
		start := time.Date(2026, 11, 23, 23, 50, 0, 0, time.UTC)
		price := WalletPrice{Multiplier: "2", Source: walletPriceConfigured}
		rows := DailyStats([]Observation{pricedStatSample(start, "100", price), pricedStatSample(start.Add(30*time.Minute), "88", price)})
		o := newOverviewTotals(now, q)
		o.addSite(first, []DailyStat{legacyGap})
		o.addSite(second, rows)
		out := o.result()
		require.Len(t, out.Periods, 1)
		assertCoverage(t, out.Periods[0], pricingPartial, true, 2, new("6"))
		assertCoverage(t, *out.Sites[0].Period, pricingUnknown, true, 0, nil)
		assertCoverage(t, *out.Sites[1].Period, pricingPriced, false, 2, new("6"))
		require.Equal(t, int64(1800), out.Periods[0].ByUnit[0].CoveredSeconds)
	})
}

func TestOverviewKeepsZeroDistinctFromUnknown(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	q, err := newOverviewQuery("retained", "")
	require.NoError(t, err)
	site := overviewFixtureSite(1, "UTC", nil)
	unknown := newOverviewTotals(now, q)
	unknown.addSite(site, nil)
	out := unknown.result()
	raw, err := json.Marshal(out.Periods[0])
	require.NoError(t, err)
	require.JSONEq(t, `{"timezone":"UTC","from":null,"to":null,"by_unit":[],"estimated_cost_cny":null,"gap_cost_cny":null,"pricing_status":"unknown","has_unknown_coverage":false}`, string(raw))
	require.Nil(t, out.Current.EstimatedBalanceCNY)

	price := WalletPrice{Multiplier: "0.5", Source: walletPriceConfigured}
	sample := pricedStatSample(now, "0", price)
	site.Latest = &sample
	site.Config.BalanceMultipliers = BalanceMultipliers{"USD": "0.5"}
	first := newOverviewTotals(now, q)
	first.addSite(site, DailyStats([]Observation{sample}))
	out = first.result()
	require.Equal(t, new("0"), out.Current.EstimatedBalanceCNY)
	require.Equal(t, []overviewCurrentUnit{{"USD", "0", 1}}, out.Current.ByUnit)
	require.Nil(t, out.Periods[0].ByUnit[0].UpstreamCost)
	require.Nil(t, out.Periods[0].EstimatedCostCNY)

	prior := pricedStatSample(now.Add(-time.Hour), "0", price)
	known := newOverviewTotals(now, q)
	known.addSite(site, DailyStats([]Observation{prior, sample}))
	out = known.result()
	require.Equal(t, new("0"), out.Periods[0].ByUnit[0].UpstreamCost)
	require.Equal(t, new("0"), out.Periods[0].EstimatedCostCNY)
	require.Equal(t, new("0"), out.Periods[0].GapCostCNY)
	require.Equal(t, pricingPriced, out.Periods[0].PricingStatus)
}

func TestOverviewHTTPReadsOnlyMatchingSitesAndDailyBuckets(t *testing.T) {
	p, _ := testPlugin(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(server.Close)
	p.client = server.Client()
	var err error
	p.env.Config, err = sdk.NewConfig([]byte(`{"hostURL":` + strconv.Quote(server.URL) + `}`))
	require.NoError(t, err)
	ctx := context.Background()
	now := time.Now().UTC()
	usd, cny := statSample(now, "100"), statSample(now, "")
	cny.Balances = []Balance{{Unit: "CNY", Available: "50"}}
	first := overviewFixtureSite(1, "UTC", &usd)
	first.Config.BalanceMultipliers = BalanceMultipliers{"USD": "2"}
	second := overviewFixtureSite(2, "Asia/Shanghai", &cny)
	archived := overviewFixtureSite(3, "UTC", &usd)
	archived.Archived = true
	for _, site := range []Site{first, second, archived} {
		site.Config.BaseURL = server.URL
		_, err := p.kv.Put(ctx, "sites", strconv.FormatInt(site.ID, 10), StoredSite{Site: site, Ciphertext: "private-fixture"})
		require.NoError(t, err)
	}
	_, err = p.kv.Put(ctx, "meta", "bindings", map[string]int64{"41": 1, "42": 1})
	require.NoError(t, err)
	date := now.Format(time.DateOnly)
	legacy := DailyStat{AlgorithmVersion: 1, Date: date, Timezone: "UTC", Unit: "USD", WalletEpoch: 1, StatisticsEpoch: 1, NetChange: new("-99")}
	current := overviewFixtureDay(date, "UTC", "7")
	_, err = p.kv.Put(ctx, "daily", fmtSource(first.ID, date), []DailyStat{legacy})
	require.NoError(t, err)
	legacy.Date = now.AddDate(0, 0, -1).Format(time.DateOnly)
	_, err = p.kv.Put(ctx, "daily", fmtSource(first.ID, legacy.Date), []DailyStat{legacy})
	require.NoError(t, err)
	_, err = p.kv.Put(ctx, dailyCollection, dailyKey(first.ID, current), dailyBucket{Sequence: 2, Stat: current})
	require.NoError(t, err)
	observed := &overviewReadOnlyKV{KVStore: p.kv, t: t, plugin: p}
	p.kv = observed

	w := call(t, p, http.MethodGet, "/statistics/overview?period=7d&timezone=UTC", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	require.NotContains(t, w.Body.String(), "private-fixture")
	var res struct {
		Data overviewResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	require.Equal(t, "7d", res.Data.Period)
	require.Equal(t, new("100"), res.Data.Current.EstimatedBalanceCNY)
	require.Equal(t, []overviewCurrentUnit{{"CNY", "50", 1}, {"USD", "100", 1}}, res.Data.Current.ByUnit)
	require.Equal(t, 1, res.Data.Current.ArchivedSites)
	require.Len(t, res.Data.Sites, 2)
	require.Len(t, res.Data.Periods, 1)
	require.Equal(t, new("7"), res.Data.Periods[0].EstimatedCostCNY)
	require.Equal(t, pricingPartial, res.Data.Periods[0].PricingStatus)
	require.True(t, res.Data.Periods[0].HasUnknownCoverage)
	require.Equal(t, 1, res.Data.Periods[0].ByUnit[0].ComparableIntervals)
	require.Equal(t, 1, res.Data.Periods[0].ByUnit[0].PricedIntervals)
	require.True(t, res.Data.Periods[0].ByUnit[0].HasUnknownCoverage)
	require.Equal(t, new("7"), res.Data.Sites[0].Period.EstimatedCostCNY)
	require.Nil(t, res.Data.Sites[1].Period)
	require.Equal(t, []string{"sites/", "daily/" + fmtSource(first.ID, ""), dailyCollection + "/" + fmtSource(first.ID, "")}, observed.listed)

	observed.listed = nil
	w = call(t, p, http.MethodGet, "/statistics/overview", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	require.Equal(t, "today", res.Data.Period)
	require.Len(t, res.Data.Periods, 2)
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	utcDay := res.Data.AsOf.UTC().Format(time.DateOnly)
	shanghaiDay := res.Data.AsOf.In(shanghai).Format(time.DateOnly)
	require.Equal(t, &utcDay, res.Data.Sites[0].Period.From)
	require.Equal(t, &shanghaiDay, res.Data.Sites[1].Period.From)
	require.False(t, res.Data.Sites[0].Period.HasUnknownCoverage)
	require.Equal(t, []string{
		"sites/",
		"daily/" + fmtSource(first.ID, utcDay), dailyCollection + "/" + fmtSource(first.ID, utcDay+":"),
		"daily/" + fmtSource(second.ID, shanghaiDay), dailyCollection + "/" + fmtSource(second.ID, shanghaiDay+":"),
	}, observed.listed)

	observed.listed = nil
	for _, query := range []string{"period=week", "timezone=unknown-zone", "period=today&period=7d", "timezone=%ZZ"} {
		w := call(t, p, http.MethodGet, "/statistics/overview?"+query, nil)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	}
	require.Zero(t, requests.Load())
	require.Empty(t, observed.listed)
}

func TestOverviewRouteRequiresHostAdmin(t *testing.T) {
	raw, err := os.ReadFile("plugin.json")
	require.NoError(t, err)
	m, err := manifest.Parse(raw)
	require.NoError(t, err)
	for _, route := range m.Backend.Routes {
		if route.Matches(http.MethodGet, "/statistics/overview") {
			require.Equal(t, "admin", route.Auth)
			return
		}
	}
	t.Fatal("总览路由未由宿主清单声明")
}

type overviewReadOnlyKV struct {
	KVStore
	t      *testing.T
	plugin *Plugin
	listed []string
}

func (kv *overviewReadOnlyKV) List(ctx context.Context, req *pb.KVListRequest) (*pb.KVListResponse, error) {
	require.True(kv.t, kv.plugin.mu.TryLock(), "报表读取时持有插件全局锁")
	kv.plugin.mu.Unlock()
	kv.listed = append(kv.listed, req.Collection+"/"+req.Prefix)
	return kv.KVStore.List(ctx, req)
}

func (kv *overviewReadOnlyKV) Get(context.Context, string, string) (*pb.KVGetResponse, error) {
	kv.t.Fatal("报表不需要点读 KV")
	return nil, nil
}

func (kv *overviewReadOnlyKV) Put(context.Context, string, string, any, ...sdk.PutOption) (int64, error) {
	kv.t.Fatal("报表不应写入 KV")
	return 0, nil
}

func (kv *overviewReadOnlyKV) Delete(context.Context, string, string, int64) (bool, error) {
	kv.t.Fatal("报表不应删除 KV")
	return false, nil
}

func overviewFixtureSite(id int64, timezone string, latest *Observation) Site {
	return Site{ID: id, Config: Config{Timezone: timezone, IntervalMinutes: 60}, WalletEpoch: 1, StatisticsEpoch: 1, Latest: latest}
}

func overviewFixtureDay(date, timezone, cost string) DailyStat {
	return DailyStat{
		AlgorithmVersion: dailyAlgorithmVersion, Date: date, Timezone: timezone, Unit: "USD", WalletEpoch: 1, StatisticsEpoch: 1,
		UpstreamCost: &cost, EstimatedCostCNY: &cost, GapCostCNY: new("0"), PricingStatus: pricingPriced,
		ComparableIntervals: 1, PricedIntervals: 1,
	}
}
