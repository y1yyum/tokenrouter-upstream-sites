package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	pb "github.com/TokenFlux/TokenRouter/plugin-sdk/go/pluginpb/v1"
	"github.com/TokenFlux/TokenRouter/plugin-sdk/go/sdk"
)

const (
	rollupCollection = "rollup-v2"
	dailyCollection  = "daily-v2"
)

// 站点 CAS 同时提交观测、控制状态和待处理汇总。独立进度镜像在
// 写日桶前保存待办，在清空站点待办前保存已确认进度，供旧版重写站点后恢复。
type rollupProgress struct {
	Sequence int64        `json:"sequence"`
	Previous *Observation `json:"previous,omitempty"`
	Pending  *rollupWork  `json:"pending,omitempty"`
}

type siteRollup struct {
	rollupProgress
	Backfill *rollupBackfill `json:"backfill,omitempty"`
}

type rollupWork struct {
	Sample      Observation          `json:"sample"`
	RawDays     int                  `json:"raw_days"`
	SummaryDays int                  `json:"summary_days"`
	Through     *observationPosition `json:"backfill_through,omitempty"`
	LegacyBases map[string]DailyStat `json:"legacy_bases,omitempty"`
}

type observationPosition struct {
	At    time.Time `json:"at"`
	RunID string    `json:"run_id"`
}

type rollupBackfill struct {
	Through   observationPosition `json:"through"`
	RawCutoff *time.Time          `json:"raw_cutoff,omitempty"`
}

type dailyBucket struct {
	Sequence      int64                `json:"sequence"`
	Stat          DailyStat            `json:"stat"`
	Legacy        *DailyStat           `json:"legacy,omitempty"`
	LegacyThrough *observationPosition `json:"legacy_through,omitempty"`
}

func position(o Observation) observationPosition {
	return observationPosition{At: o.At, RunID: o.RunID}
}

func (a observationPosition) before(b observationPosition) bool {
	if !a.At.Equal(b.At) {
		return a.At.Before(b.At)
	}
	return a.RunID < b.RunID
}

func retentionDays(s Site) (raw, summary int) {
	raw, summary = s.Config.RawRetentionDays, s.Config.SummaryRetentionDays
	if raw == 0 {
		raw = 90
	}
	if summary == 0 {
		summary = 365
	}
	return raw, summary
}

func historyKey(id int64, o Observation) string {
	return fmtSource(id, o.At.UTC().Format(time.RFC3339Nano)+":"+o.RunID)
}

func dailyKey(id int64, row DailyStat) string {
	return fmtSource(id, fmt.Sprintf("%s:%d:%d:%s:%s", row.Date, row.WalletEpoch, row.StatisticsEpoch, row.Timezone, row.Unit))
}

func localDayEnd(date string, timezone string, days int) (time.Time, error) {
	day, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return time.Time{}, err
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, err
	}
	end := day.AddDate(0, 0, days)
	target := end.Format(time.DateOnly)
	anchor := end.AddDate(0, 0, -2)
	at := time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 12, 0, 0, 0, loc)
	for at.Format(time.DateOnly) < target {
		at = nextLocalDay(at)
	}
	return at, nil
}

func (p *Plugin) putRollupHistory(ctx context.Context, id int64, work rollupWork) error {
	ttl := time.Until(work.Sample.At.Add(time.Duration(work.RawDays) * 24 * time.Hour))
	if ttl <= 0 {
		return nil
	}
	_, err := p.kv.Put(ctx, "history", historyKey(id, work.Sample), work.Sample, sdk.IfAbsent(), sdk.TTL(ttl))
	if errors.Is(err, sdk.ErrVersionConflict) {
		return nil
	}
	return err
}

func (p *Plugin) addDailyBucket(ctx context.Context, id, sequence int64, delta DailyStat, expires time.Time, work rollupWork) error {
	key := dailyKey(id, delta)
	for {
		var bucket dailyBucket
		version, err := p.read(ctx, dailyCollection, key, &bucket)
		if errors.Is(err, ErrNotFound) {
			version = -1
			if work.Through == nil {
				bucket.Legacy, err = p.legacyDailyRow(ctx, id, delta)
				if err != nil {
					return err
				}
			} else if legacy, found := work.LegacyBases[key]; found {
				bucket.Legacy, bucket.LegacyThrough = &legacy, work.Through
			}
		} else if err != nil {
			return err
		}
		if bucket.Sequence >= sequence {
			return nil
		}
		ttl := time.Until(expires)
		if ttl <= 0 {
			return nil
		}
		bucket.Sequence = sequence
		if bucket.LegacyThrough == nil || bucket.LegacyThrough.before(position(work.Sample)) {
			mergeDailyStat(&bucket.Stat, delta)
		}
		_, err = p.kv.Put(ctx, dailyCollection, key, bucket, sdk.IfVersion(version), sdk.TTL(ttl))
		if !errors.Is(err, sdk.ErrVersionConflict) {
			return err
		}
	}
}

// 同一序号只有待处理和已确认两个阶段。重复写入及旧副本不能回退进度。
func (p *Plugin) mirrorRollup(ctx context.Context, key string, progress rollupProgress) (bool, error) {
	for {
		var current rollupProgress
		version, err := p.read(ctx, rollupCollection, key, &current)
		if errors.Is(err, ErrNotFound) {
			version = -1
		} else if err != nil {
			return false, err
		}
		if current.Sequence > progress.Sequence || (current.Sequence == progress.Sequence && current.Pending == nil) {
			return false, nil
		}
		if current.Sequence == progress.Sequence && progress.Pending != nil {
			return true, nil
		}
		_, err = p.kv.Put(ctx, rollupCollection, key, progress, sdk.IfVersion(version))
		if !errors.Is(err, sdk.ErrVersionConflict) {
			return err == nil, err
		}
	}
}

func (p *Plugin) finishRollup(ctx context.Context, key string, stored StoredSite, version int64) error {
	progress := &stored.Rollup.rollupProgress
	work := *progress.Pending
	apply, err := p.mirrorRollup(ctx, key, *progress)
	if err != nil {
		return err
	}
	if apply {
		if work.Through == nil {
			if err = p.putRollupHistory(ctx, stored.Site.ID, work); err != nil {
				return err
			}
		}
		if err = p.rollupDays(ctx, stored.Site.ID, *progress, work); err != nil {
			return err
		}
	}
	if walletSample(work.Sample) {
		progress.Previous = &work.Sample
	}
	progress.Pending = nil
	if apply {
		if _, err = p.mirrorRollup(ctx, key, *progress); err != nil {
			return err
		}
	}
	_, err = p.kv.Put(ctx, "sites", key, stored, sdk.IfVersion(version))
	if errors.Is(err, sdk.ErrVersionConflict) {
		return nil
	}
	return err
}

func (p *Plugin) rollupDays(ctx context.Context, id int64, progress rollupProgress, work rollupWork) error {
	for _, delta := range pairDaily(progress.Previous, work.Sample) {
		expires, err := localDayEnd(delta.Date, delta.Timezone, work.SummaryDays)
		if err != nil {
			return err
		}
		if !expires.After(time.Now()) {
			continue
		}
		if err = p.addDailyBucket(ctx, id, progress.Sequence, delta, expires, work); err != nil {
			return err
		}
	}
	return nil
}

func (p *Plugin) needsLegacyCheck(id int64) bool {
	if p.env == nil || p.env.PreviousVersion != "1.1.1" {
		return false
	}
	_, checked := p.legacyChecked.Load(id)
	return !checked
}

func (p *Plugin) prepareRollup(ctx context.Context, id int64) error {
	checkLegacy := p.needsLegacyCheck(id)
	err := p.resumeRollup(ctx, id, checkLegacy)
	if checkLegacy && err == nil {
		p.legacyChecked.Store(id, true)
	}
	return err
}

// 回填按采样时间和 run ID 顺序推进。站点版本条件让副本共用同一待办。
func (p *Plugin) resumeRollup(ctx context.Context, id int64, checkLegacyHistory bool) error {
	key := strconv.FormatInt(id, 10)
	var samples []Observation
	var loaded bool
	var bases map[string]DailyStat
	var plannedThrough observationPosition
	for {
		var stored StoredSite
		version, err := p.read(ctx, "sites", key, &stored)
		if err != nil {
			return err
		}
		state := stored.Rollup
		if state != nil && state.Pending != nil {
			if err = p.finishRollup(ctx, key, stored, version); err != nil {
				return err
			}
			continue
		}
		if state != nil && state.Backfill == nil {
			if !checkLegacyHistory {
				return nil
			}
			// v1 先写 history，再写 sites。崩溃时 sites 可能仍保留 v2 标记。
			found, scanErr := p.hasNewLegacySample(ctx, id, state.Previous)
			if scanErr != nil || !found {
				return scanErr
			}
		}
		if !loaded {
			samples, err = p.backfillSamples(ctx, stored.Site)
			if err != nil {
				return err
			}
			loaded = true
		}
		raw, summary := retentionDays(stored.Site)
		if state == nil {
			var progress rollupProgress
			_, err = p.read(ctx, rollupCollection, key, &progress)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			state = &siteRollup{rollupProgress: progress}
			if len(samples) != 0 {
				state.Backfill = &rollupBackfill{Through: position(samples[len(samples)-1])}
				if errors.Is(err, ErrNotFound) {
					cutoff := time.Now().Add(-time.Duration(raw) * 24 * time.Hour)
					state.Backfill.RawCutoff = &cutoff
				}
			}
			stored.Rollup = state
		} else if state.Backfill == nil {
			if len(samples) == 0 {
				return nil
			}
			through := position(samples[len(samples)-1])
			if state.Previous != nil && !position(*state.Previous).before(through) {
				return nil
			}
			state.Backfill = &rollupBackfill{Through: through}
		} else {
			if bases == nil || plannedThrough != state.Backfill.Through {
				bases, err = p.backfillLegacyBases(ctx, id, samples, state)
				if err != nil {
					return err
				}
				plannedThrough = state.Backfill.Through
			}
			i := 0
			if state.Previous != nil {
				last := position(*state.Previous)
				i = sort.Search(len(samples), func(i int) bool { return last.before(position(samples[i])) })
			}
			if i == len(samples) || state.Backfill.Through.before(position(samples[i])) {
				state.Backfill = nil
			} else {
				state.Sequence++
				work := &rollupWork{Sample: samples[i], SummaryDays: summary, Through: &state.Backfill.Through}
				for _, delta := range pairDaily(state.Previous, work.Sample) {
					key := dailyKey(id, delta)
					if legacy, found := bases[key]; found {
						if work.LegacyBases == nil {
							work.LegacyBases = make(map[string]DailyStat)
						}
						work.LegacyBases[key] = legacy
					}
				}
				state.Pending = work
			}
		}
		_, err = p.kv.Put(ctx, "sites", key, stored, sdk.IfVersion(version))
		if err != nil && !errors.Is(err, sdk.ErrVersionConflict) {
			return err
		}
		if err == nil {
			checkLegacyHistory = false
		}
	}
}

func (p *Plugin) hasNewLegacySample(ctx context.Context, id int64, previous *Observation) (bool, error) {
	request := &pb.KVListRequest{Collection: "history", Prefix: fmtSource(id, ""), Limit: 500}
	if previous != nil {
		// 同一秒内的整数时间和带小数时间在 history 键中顺序不同。
		request.Cursor = fmtSource(id, previous.At.UTC().Format("2006-01-02T15:04:05")+".")
	}
	for {
		page, err := p.kv.List(ctx, request)
		if err != nil {
			return false, err
		}
		for _, row := range page.Items {
			var sample Observation
			if err = json.Unmarshal(row.ValueJson, &sample); err != nil {
				return false, err
			}
			if walletSample(sample) && (previous == nil || position(*previous).before(position(sample))) {
				return true, nil
			}
		}
		if page.NextCursor == "" {
			return false, nil
		}
		request.Cursor = page.NextCursor
	}
}

func (p *Plugin) legacyDailyRow(ctx context.Context, id int64, delta DailyStat) (*DailyStat, error) {
	var legacy []DailyStat
	_, err := p.read(ctx, "daily", fmtSource(id, delta.Date), &legacy)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	for _, row := range legacy {
		if row.key() == delta.key() {
			return &row, nil
		}
	}
	return nil, nil
}

// 覆盖或原币指标对不上时，保留 v1 日桶作为基数。待办随序号记录
// 受影响的基数，跨日写入中断后仍按同一个回填截止点续跑。
func (p *Plugin) backfillLegacyBases(ctx context.Context, id int64, samples []Observation, state *siteRollup) (map[string]DailyStat, error) {
	rows, err := p.records(ctx, "daily", fmtSource(id, ""))
	if err != nil {
		return nil, err
	}
	bases := make(map[string]DailyStat)
	if len(rows) == 0 {
		return bases, nil
	}
	backfill := state.Backfill
	n := sort.Search(len(samples), func(i int) bool { return backfill.Through.before(position(samples[i])) })
	evidence := samples[:n]
	if state.Previous != nil && (n == 0 || position(*state.Previous).before(position(evidence[0]))) {
		evidence = append([]Observation{*state.Previous}, evidence...)
	}
	rebuilt := dailyRows{}
	for _, row := range DailyStats(evidence) {
		rebuilt[row.key()] = row
	}
	for _, item := range rows {
		var legacy []DailyStat
		if err = json.Unmarshal(item.ValueJson, &legacy); err != nil {
			return nil, err
		}
		for _, row := range legacy {
			partial := false
			if backfill.RawCutoff != nil {
				loc, loadErr := time.LoadLocation(row.Timezone)
				partial = loadErr == nil && row.Date <= backfill.RawCutoff.In(loc).Format(time.DateOnly)
			}
			if partial || !matchesLegacyDaily(row, rebuilt[row.key()]) {
				bases[dailyKey(id, row)] = row
			}
		}
	}
	return bases, nil
}

func matchesLegacyDaily(legacy, rebuilt DailyStat) bool {
	equal := func(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
	return rebuilt.Date != "" && legacy.CoveredSeconds == rebuilt.CoveredSeconds &&
		legacy.Gap == rebuilt.Gap && legacy.CrossDay == rebuilt.CrossDay &&
		equal(legacy.EstimatedSpend, rebuilt.EstimatedSpend) &&
		equal(legacy.AccountSpend, rebuilt.AccountSpend) && equal(legacy.NetChange, rebuilt.NetChange)
}

func (bucket dailyBucket) report() DailyStat {
	if bucket.Legacy == nil {
		return bucket.Stat
	}
	row := *bucket.Legacy
	row.PricingStatus = pricingUnknown
	if bucket.Stat.Date != "" {
		mergeDailyStat(&row, bucket.Stat)
		if row.EstimatedCostCNY != nil {
			row.PricingStatus = pricingPartial
		}
	}
	return row
}

func (p *Plugin) backfillSamples(ctx context.Context, s Site) ([]Observation, error) {
	history, err := p.history(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	samples := make([]Observation, 0, len(history)+1)
	var seenLatest bool
	for _, o := range history {
		if walletSample(o) {
			samples = append(samples, o)
			seenLatest = seenLatest || (s.Latest != nil && position(o) == position(*s.Latest))
		}
	}
	if !seenLatest && s.Latest != nil && walletSample(*s.Latest) {
		samples = append(samples, *s.Latest)
	}
	sort.Slice(samples, func(i, j int) bool { return position(samples[i]).before(position(samples[j])) })
	return samples, nil
}

// 日报读取只合并两个版本的现有日桶，单站告警可限定到当天。
func (p *Plugin) readDaily(ctx context.Context, s Site) ([]DailyStat, error) {
	return p.readDailyForDay(ctx, s, "")
}

func (p *Plugin) readDailyForDay(ctx context.Context, s Site, date string) ([]DailyStat, error) {
	rows := dailyRows{}
	_, days := retentionDays(s)
	now := time.Now()
	include := func(row DailyStat) {
		expires, err := localDayEnd(row.Date, row.Timezone, days)
		if err == nil && expires.After(now) {
			rows[row.key()] = row
		}
	}

	legacy, err := p.records(ctx, "daily", fmtSource(s.ID, date))
	if err != nil {
		return nil, err
	}
	for _, item := range legacy {
		var values []DailyStat
		if err = json.Unmarshal(item.ValueJson, &values); err != nil {
			return nil, err
		}
		for _, row := range values {
			if row.PricingStatus == "" {
				row.PricingStatus = pricingUnknown
			}
			include(row)
		}
	}
	if date != "" {
		date += ":"
	}
	current, err := p.records(ctx, dailyCollection, fmtSource(s.ID, date))
	if err != nil {
		return nil, err
	}
	for _, item := range current {
		var bucket dailyBucket
		if err = json.Unmarshal(item.ValueJson, &bucket); err != nil {
			return nil, err
		}
		include(bucket.report())
	}
	return rows.sorted(), nil
}
