package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/TokenFlux/TokenRouter/plugin-sdk/go/pluginpb/v1"
	"github.com/TokenFlux/TokenRouter/plugin-sdk/go/sdk"
	"github.com/stretchr/testify/require"
)

type rollupTestKV struct {
	KVStore
	beforePut func(*pb.KVPutRequest) error
	onList    func(string)
}

func (kv rollupTestKV) Put(ctx context.Context, collection, key string, value any, options ...sdk.PutOption) (int64, error) {
	if kv.beforePut != nil {
		request := &pb.KVPutRequest{Collection: collection, Key: key}
		for _, option := range options {
			option(request)
		}
		if err := kv.beforePut(request); err != nil {
			return 0, err
		}
	}
	return kv.KVStore.Put(ctx, collection, key, value, options...)
}

func (kv rollupTestKV) List(ctx context.Context, request *pb.KVListRequest) (*pb.KVListResponse, error) {
	if kv.onList != nil {
		kv.onList(request.Collection)
	}
	return kv.KVStore.List(ctx, request)
}

func rollupDayStart() time.Time {
	return time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
}

func rollupSample(s Site, at time.Time, id, amount string, priced bool) Observation {
	o := statSample(at, amount)
	o.ConfigVersion = s.ConfigVersion
	o.RunID = id
	if priced {
		o.Balances[0].Price = &WalletPrice{Multiplier: "2", Version: 1}
	}
	return o
}

func acceptRollupSample(t *testing.T, p *Plugin, s Site, o Observation) {
	t.Helper()
	_, accepted, err := p.acceptObservation(context.Background(), s.ID, o)
	require.NoError(t, err)
	require.True(t, accepted)
}

func completeRollupSample(t *testing.T, p *Plugin, s Site, o Observation) {
	t.Helper()
	acceptRollupSample(t, p, s, o)
	require.NoError(t, p.prepareRollup(context.Background(), s.ID))
}

func putLegacyHistory(t *testing.T, p *Plugin, s Site, o Observation) {
	t.Helper()
	_, err := p.kv.Put(context.Background(), "history", historyKey(s.ID, o), o)
	require.NoError(t, err)
}

func putLegacyDaily(t *testing.T, p *Plugin, s Site, o Observation, spend string, seconds int64) {
	t.Helper()
	day := o.At.Format(time.DateOnly)
	net := "-" + spend
	// 只写 1.1.1 保存的字段，模拟由旧运行时重建的日报。
	legacy := []DailyStat{{
		AlgorithmVersion: 1, Date: day, Timezone: o.Timezone, Unit: "USD",
		WalletEpoch: o.WalletEpoch, StatisticsEpoch: o.StatisticsEpoch,
		EstimatedSpend: &spend, NetChange: &net, CoveredSeconds: seconds, AvailableOnly: true,
	}}
	_, err := p.kv.Put(context.Background(), "daily", fmtSource(s.ID, day), legacy)
	require.NoError(t, err)
}

func TestRollupRestartsAfterPartialCrossDayWrite(t *testing.T) {
	ctx := context.Background()
	p, h := testPlugin(t)
	s := addSite(t, p, "https://example.com")
	require.NoError(t, p.prepareRollup(ctx, s.ID))
	start := rollupDayStart().Add(23*time.Hour + 50*time.Minute)
	a := rollupSample(s, start, "a", "100", true)
	b := rollupSample(s, start.Add(30*time.Minute), "b", "88", true)
	completeRollupSample(t, p, s, a)
	acceptRollupSample(t, p, s, b)

	expected := DailyStats([]Observation{a, b})
	failed := errors.New("cross-day interruption")
	before := p.kv
	p.kv = rollupTestKV{KVStore: before, beforePut: func(r *pb.KVPutRequest) error {
		if r.Collection == dailyCollection && r.Key == dailyKey(s.ID, expected[1]) {
			return failed
		}
		return nil
	}}
	require.ErrorIs(t, p.prepareRollup(ctx, s.ID), failed)
	var interrupted StoredSite
	version, err := p.read(ctx, "sites", "1", &interrupted)
	require.NoError(t, err)
	require.Equal(t, b.RunID, interrupted.Rollup.Pending.Sample.RunID)
	require.Equal(t, a.RunID, interrupted.Rollup.Previous.RunID)
	partial, err := p.readDaily(ctx, s)
	require.NoError(t, err)
	require.Equal(t, "4", *partial[0].EstimatedCostCNY)
	require.Nil(t, partial[1].EstimatedCostCNY)

	restarted := &Plugin{}
	require.NoError(t, restarted.Init(ctx, h.Env))
	// 旧副本再次投递同一待办和原始记录。
	require.NoError(t, restarted.finishRollup(ctx, "1", interrupted, version))
	require.NoError(t, restarted.putRollupHistory(ctx, s.ID, rollupWork{Sample: b, RawDays: 90}))
	require.NoError(t, restarted.prepareRollup(ctx, s.ID))
	daily, err := restarted.readDaily(ctx, s)
	require.NoError(t, err)
	require.Equal(t, expected, daily)
	history, err := restarted.history(ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, []Observation{a, b}, history)
}

func TestRollupConcurrentReplicasAndCASRetry(t *testing.T) {
	ctx := context.Background()
	p, h := testPlugin(t)
	other := &Plugin{}
	require.NoError(t, other.Init(ctx, h.Env))
	s := addSite(t, p, "https://example.com")
	at := rollupDayStart().Add(10 * time.Hour)
	a := rollupSample(s, at, "a", "100", false)
	putLegacyHistory(t, p, s, a)
	ready := func() {
		t.Helper()
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for _, replica := range []*Plugin{p, other} {
			wg.Go(func() {
				<-start
				errs <- replica.prepareRollup(ctx, s.ID)
			})
		}
		close(start)
		wg.Wait()
		require.NoError(t, <-errs)
		require.NoError(t, <-errs)
	}
	ready()
	b := rollupSample(s, at.Add(time.Hour), "b", "80", true)
	acceptRollupSample(t, p, s, b)
	var conflicts atomic.Int32
	original := p.kv
	p.kv = rollupTestKV{KVStore: original, beforePut: func(r *pb.KVPutRequest) error {
		if r.Collection == dailyCollection && conflicts.Add(1) == 1 {
			return sdk.ErrVersionConflict
		}
		return nil
	}}
	require.NoError(t, p.prepareRollup(ctx, s.ID))
	require.Equal(t, int32(2), conflicts.Load())

	c := rollupSample(s, at.Add(2*time.Hour), "c", "60", true)
	acceptRollupSample(t, other, s, c)
	ready()
	_, accepted, err := other.acceptObservation(ctx, s.ID, c)
	require.NoError(t, err)
	require.False(t, accepted)
	got, err := p.readDaily(ctx, s)
	require.NoError(t, err)
	require.Equal(t, DailyStats([]Observation{a, b, c}), got)
	var stored StoredSite
	_, err = p.read(ctx, "sites", "1", &stored)
	require.NoError(t, err)
	require.EqualValues(t, 3, stored.Rollup.Sequence)
	require.Nil(t, stored.Rollup.Pending)
}

func TestRollupKeepsCompleteLegacyBucketWithIncompleteRaw(t *testing.T) {
	cases := []struct {
		name        string
		keepTwo     bool
		oldFromDays int
	}{
		{name: "expanded raw retention", keepTwo: true, oldFromDays: 10},
		{name: "restored last snapshot only", oldFromDays: 1},
		{name: "first partially retained local day", keepTwo: true, oldFromDays: 90},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			p, h := testPlugin(t)
			s := addSite(t, p, "https://example.com")
			at := rollupDayStart().AddDate(0, 0, 1-test.oldFromDays).Add(10 * time.Hour)
			before := rollupSample(s, at, "retained-a", "90", false)
			last := rollupSample(s, at.Add(time.Hour), "retained-b", "80", false)
			putLegacyDaily(t, p, s, last, "40", 6*3600)
			if test.keepTwo {
				putLegacyHistory(t, p, s, before)
				putLegacyHistory(t, p, s, last)
			} else {
				var stored StoredSite
				_, err := p.read(ctx, "sites", "1", &stored)
				require.NoError(t, err)
				stored.Site.Latest = &last
				_, err = p.kv.Put(ctx, "sites", "1", stored)
				require.NoError(t, err)
			}

			restarted := &Plugin{}
			require.NoError(t, restarted.Init(ctx, h.Env))
			daily, err := restarted.readDaily(ctx, s)
			require.NoError(t, err)
			require.Len(t, daily, 1)
			require.EqualValues(t, 1, daily[0].AlgorithmVersion)
			require.Equal(t, "40", *daily[0].EstimatedSpend)
			require.EqualValues(t, 6*3600, daily[0].CoveredSeconds)
			require.Nil(t, daily[0].UpstreamCost)
			require.Nil(t, daily[0].EstimatedCostCNY)
			require.Nil(t, daily[0].OpenBalance)
			require.Equal(t, pricingUnknown, daily[0].PricingStatus)

			next := rollupSample(s, last.At.Add(time.Hour), "new-a", "75", true)
			end := rollupSample(s, next.At.Add(time.Hour), "new-b", "65", true)
			completeRollupSample(t, restarted, s, next)
			completeRollupSample(t, restarted, s, end)
			daily, err = restarted.readDaily(ctx, s)
			require.NoError(t, err)
			require.Len(t, daily, 1)
			require.Equal(t, "55", *daily[0].EstimatedSpend)
			require.Equal(t, "-55", *daily[0].NetChange)
			require.EqualValues(t, 8*3600, daily[0].CoveredSeconds)
			require.Equal(t, "15", *daily[0].UpstreamCost)
			require.Equal(t, "5", *daily[0].EstimatedCostCNY)
			require.Equal(t, "5", *daily[0].UnpricedDrop)
			require.Equal(t, pricingPartial, daily[0].PricingStatus)
		})
	}
}

func TestRollupRebuildsCompleteLegacyEvidenceAndKeepsExpiredRawTotals(t *testing.T) {
	ctx := context.Background()
	p, h := testPlugin(t)
	s := addSite(t, p, "https://example.com")
	at := rollupDayStart().Add(10 * time.Hour)
	a := rollupSample(s, at, "a", "100", false)
	b := rollupSample(s, at.Add(time.Hour), "b", "80", false)
	putLegacyHistory(t, p, s, a)
	putLegacyHistory(t, p, s, b)
	putLegacyDaily(t, p, s, b, "20", 3600)
	legacyOnly := rollupSample(s, at.AddDate(0, 0, -100), "expired", "20", false)
	putLegacyDaily(t, p, s, legacyOnly, "9", 3600)
	pastSummary := rollupSample(s, at.AddDate(0, 0, -400), "expired-summary", "20", false)
	putLegacyDaily(t, p, s, pastSummary, "50", 3600)

	restarted := &Plugin{}
	require.NoError(t, restarted.Init(ctx, h.Env))
	for _, sample := range []Observation{a, b} {
		_, err := restarted.kv.Delete(ctx, "history", historyKey(s.ID, sample), 0)
		require.NoError(t, err)
	}
	writes := 0
	historyReads := 0
	restarted.kv = rollupTestKV{KVStore: restarted.kv,
		beforePut: func(*pb.KVPutRequest) error { writes++; return nil },
		onList: func(collection string) {
			if collection == "history" {
				historyReads++
			}
		},
	}
	// 即使配置锁被占用，GET 也能立即从持久日桶读取。
	restarted.mu.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			restarted.mu.Unlock()
		}
	}()
	done := make(chan []byte, 1)
	go func() {
		w := call(t, restarted, http.MethodGet, "/sites/1/daily", nil)
		done <- w.Body.Bytes()
	}()
	var body []byte
	select {
	case body = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("日报阻塞在配置锁")
	}
	restarted.mu.Unlock()
	unlocked = true
	require.Zero(t, writes)
	require.Zero(t, historyReads)
	var result struct{ Data []DailyStat }
	require.NoError(t, json.Unmarshal(body, &result))
	require.Len(t, result.Data, 2)
	require.Equal(t, DailyStats([]Observation{a, b})[0], result.Data[0])
	require.Equal(t, "9", *result.Data[1].EstimatedSpend)
	require.Nil(t, result.Data[1].UpstreamCost)
	require.Nil(t, result.Data[1].EstimatedCostCNY)
}

func TestRollupResumesAfterDowngradeRewritesSites(t *testing.T) {
	ctx := context.Background()
	p, h := testPlugin(t)
	s := addSite(t, p, "https://example.com")
	require.NoError(t, p.prepareRollup(ctx, s.ID))
	at := rollupDayStart().Add(10 * time.Hour)
	a := rollupSample(s, at, "a", "120", true)
	b := rollupSample(s, at.Add(time.Hour), "b", "80", true)
	completeRollupSample(t, p, s, a)
	completeRollupSample(t, p, s, b)

	// 降级查询写入新历史并重写 sites 和 daily。已确认的 v2 历史也已到期。
	for _, sample := range []Observation{a, b} {
		_, err := p.kv.Delete(ctx, "history", historyKey(s.ID, sample), 0)
		require.NoError(t, err)
	}
	c := rollupSample(s, at.Add(2*time.Hour), "legacy-c", "70", false)
	putLegacyHistory(t, p, s, c)
	putLegacyDaily(t, p, s, c, "50", 7200)
	var stored StoredSite
	_, err := p.read(ctx, "sites", "1", &stored)
	require.NoError(t, err)
	stored.Site.Latest = &c
	stored.Site.LastObservation = &c
	stored.Site.LastBalanceObservation = nil
	stored.Rollup = nil
	_, err = p.kv.Put(ctx, "sites", "1", stored)
	require.NoError(t, err)

	restarted := &Plugin{}
	require.NoError(t, restarted.Init(ctx, h.Env))
	d := rollupSample(s, at.Add(3*time.Hour), "d", "60", true)
	e := rollupSample(s, at.Add(4*time.Hour), "e", "40", true)
	completeRollupSample(t, restarted, s, d)
	completeRollupSample(t, restarted, s, e)
	again := &Plugin{}
	require.NoError(t, again.Init(ctx, h.Env))
	daily, err := again.readDaily(ctx, s)
	require.NoError(t, err)
	require.Len(t, daily, 1)
	require.Equal(t, "80", *daily[0].EstimatedSpend)
	require.Equal(t, "80", *daily[0].UpstreamCost)
	require.Equal(t, "30", *daily[0].EstimatedCostCNY)
	require.Equal(t, "20", *daily[0].UnpricedDrop)
	require.Equal(t, pricingPartial, daily[0].PricingStatus)
	history, err := again.history(ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, []Observation{c, d, e}, history)
	var progress rollupProgress
	_, err = again.read(ctx, rollupCollection, "1", &progress)
	require.NoError(t, err)
	require.EqualValues(t, 5, progress.Sequence)
	require.Equal(t, "e", progress.Previous.RunID)
}

func TestRollupFindsDowngradeHistoryWithoutSiteRewrite(t *testing.T) {
	ctx := context.Background()
	p, h := testPlugin(t)
	s := addSite(t, p, "https://example.com")
	require.NoError(t, p.prepareRollup(ctx, s.ID))
	at := rollupDayStart().Add(10 * time.Hour)
	a := rollupSample(s, at, "a", "100", true)
	b := rollupSample(s, at.Add(time.Hour), "b", "80", true)
	completeRollupSample(t, p, s, a)
	completeRollupSample(t, p, s, b)
	c := rollupSample(s, b.At.Add(500*time.Millisecond), "legacy-c", "70", false)
	putLegacyHistory(t, p, s, c)

	h.Env.PreviousVersion = "1.1.1"
	restarted := &Plugin{}
	require.NoError(t, restarted.Init(ctx, h.Env))
	daily, err := restarted.readDaily(ctx, s)
	require.NoError(t, err)
	require.Equal(t, DailyStats([]Observation{a, b, c}), daily)
	require.Equal(t, "10", *daily[0].EstimatedCostCNY)
	require.Equal(t, "10", *daily[0].UnpricedDrop)
}

func TestRollupDailyAlertStillUsesOriginalCurrency(t *testing.T) {
	ctx := context.Background()
	p, _ := testPlugin(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		amount := "100"
		if calls.Add(1) > 1 {
			amount = "80"
		}
		_, _ = w.Write([]byte(`{"isValid":true,"balance":"` + amount + `","unit":"USD"}`))
	}))
	defer server.Close()
	p.client = server.Client()
	s := addSite(t, p, server.URL)
	if time.Now().UTC().Hour() >= 12 {
		s.Config.Timezone = "Etc/GMT+12"
	}
	s.Config.BalanceMultipliers = map[string]string{"USD": "2"}
	s.Config.Alerts.Unit = "USD"
	s.Config.Alerts.DailySpend = "15"
	s.Config.Alerts.DailyMetric = "estimated"
	require.Equal(t, http.StatusOK, call(t, p, http.MethodPut, "/sites/1", SaveRequest{Config: s.Config, Version: s.ConfigVersion}).Code)
	_, err := p.query(ctx, s.ID, "balance")
	require.NoError(t, err)
	_, err = p.query(ctx, s.ID, "balance")
	require.NoError(t, err)
	events, err := p.records(ctx, "events", fmtSource(s.ID, ""))
	require.NoError(t, err)
	require.Len(t, events, 1)
	var event map[string]any
	require.NoError(t, json.Unmarshal(events[0].ValueJson, &event))
	require.Equal(t, "20", event["value"])
	require.Equal(t, "USD", event["unit"])
	rows, err := p.readDaily(ctx, s)
	require.NoError(t, err)
	require.Equal(t, "10", *rows[0].EstimatedCostCNY)
}

func TestRollupDayExpiryUsesLocalCalendar(t *testing.T) {
	start, err := localDayEnd("2026-11-01", "America/New_York", 1)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC), start.UTC())
	request := &pb.KVPutRequest{}
	sdk.TTL(time.Nanosecond)(request)
	require.EqualValues(t, 1, request.TtlSeconds)
}
