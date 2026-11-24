package main

import (
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const (
	dailyAlgorithmVersion = 2
	pricingPriced         = "priced"
	pricingPartial        = "partial"
	pricingUnknown        = "unknown"
)

// DailyStat 按钱包、统计时区分段、日历日和单位汇总。
type DailyStat struct {
	AlgorithmVersion    int            `json:"algorithm_version"`
	Date                string         `json:"date"`
	Timezone            string         `json:"timezone"`
	Unit                string         `json:"unit"`
	WalletEpoch         int64          `json:"wallet_epoch"`
	StatisticsEpoch     int64          `json:"statistics_epoch"`
	EstimatedSpend      *string        `json:"estimated_spend"`
	AccountSpend        *string        `json:"account_spend"`
	NetChange           *string        `json:"net_change"`
	UpstreamCost        *string        `json:"upstream_cost"`
	EstimatedCostCNY    *string        `json:"estimated_cost_cny"`
	GapCostCNY          *string        `json:"gap_cost_cny"`
	UnpricedDrop        *string        `json:"unpriced_drop"`
	BalanceIncrease     *string        `json:"balance_increase"`
	OpenBalance         *string        `json:"open_balance"`
	CloseBalance        *string        `json:"close_balance"`
	MinBalance          *string        `json:"min_balance"`
	MaxBalance          *string        `json:"max_balance"`
	FirstSampleAt       *time.Time     `json:"first_sample_at"`
	LastSampleAt        *time.Time     `json:"last_sample_at"`
	PricingStatus       string         `json:"pricing_status"`
	PricedIntervals     int            `json:"priced_intervals"`
	ComparableIntervals int            `json:"comparable_intervals"`
	UnpricedReasons     map[string]int `json:"unpriced_reasons,omitempty"`
	HasUnknownCoverage  bool           `json:"has_unknown_coverage,omitempty"`
	CoveredSeconds      int64          `json:"covered_seconds"`
	Gap                 bool           `json:"gap"`
	CrossDay            bool           `json:"cross_day"`
	AvailableOnly       bool           `json:"available_only"`
}

type dailyStatKey struct {
	date, timezone, unit         string
	walletEpoch, statisticsEpoch int64
}

type dailyRows map[dailyStatKey]DailyStat

func (row DailyStat) key() dailyStatKey {
	return dailyStatKey{row.Date, row.Timezone, row.Unit, row.WalletEpoch, row.StatisticsEpoch}
}

func (rows dailyRows) add(delta DailyStat) {
	key := delta.key()
	row := rows[key]
	mergeDailyStat(&row, delta)
	rows[key] = row
}

func (rows dailyRows) sorted() []DailyStat {
	result := make([]DailyStat, 0, len(rows))
	for _, row := range rows {
		result = append(result, row)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Date != b.Date {
			return a.Date > b.Date
		}
		if a.Unit != b.Unit {
			return a.Unit < b.Unit
		}
		if a.WalletEpoch != b.WalletEpoch {
			return a.WalletEpoch < b.WalletEpoch
		}
		if a.StatisticsEpoch != b.StatisticsEpoch {
			return a.StatisticsEpoch < b.StatisticsEpoch
		}
		return a.Timezone < b.Timezone
	})
	return result
}

func walletSample(o Observation) bool {
	return o.Source == "balance" && o.Outcome == Healthy
}

// DailyStats 从任意顺序的历史汇总，仅成功的余额查询能成为区间端点。
func DailyStats(history []Observation) []DailyStat {
	samples := make([]Observation, 0, len(history))
	for _, o := range history {
		if walletSample(o) {
			samples = append(samples, o)
		}
	}
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].At.Before(samples[j].At) })
	rows := dailyRows{}
	var previous *Observation
	for i := range samples {
		for _, delta := range pairDaily(previous, samples[i]) {
			rows.add(delta)
		}
		previous = &samples[i]
	}
	return rows.sorted()
}

// pairDaily 只记入 current 的实测可用余额，previous 仅作为区间端点。
// 正常区间在每个有经过时长的日行计一次，原因计数采用相同口径。
// 跨日汇总这些计数得到日行上的覆盖次数。缺测区间只计入恢复日。
func pairDaily(previous *Observation, current Observation) []DailyStat {
	if !walletSample(current) {
		return nil
	}
	location, err := time.LoadLocation(current.Timezone)
	if err != nil {
		return nil
	}
	if previous != nil && (!walletSample(*previous) ||
		previous.WalletEpoch != current.WalletEpoch ||
		previous.StatisticsEpoch != current.StatisticsEpoch ||
		previous.Timezone != current.Timezone ||
		!current.At.After(previous.At)) {
		previous = nil
	}

	end := current.At.In(location)
	rows := dailyRows{}
	for _, balance := range current.Balances {
		available, err := Amount(balance.Available)
		if err != nil {
			continue
		}
		row := DailyStat{
			AlgorithmVersion: dailyAlgorithmVersion,
			Date:             end.Format(time.DateOnly),
			Timezone:         current.Timezone,
			Unit:             balance.Unit,
			WalletEpoch:      current.WalletEpoch,
			StatisticsEpoch:  current.StatisticsEpoch,
			PricingStatus:    pricingUnknown,
			AvailableOnly:    balance.Total == nil,
		}
		sample := row
		at, value := current.At.UTC(), decimalText(available)
		sample.FirstSampleAt, sample.LastSampleAt = &at, &at
		sample.OpenBalance, sample.CloseBalance = value, value
		sample.MinBalance, sample.MaxBalance = value, value
		rows.add(sample)

		if previous != nil {
			for _, prior := range previous.Balances {
				if prior.Unit == balance.Unit {
					addWalletInterval(rows, row, prior, balance, previous.At.In(location), end, previous.IntervalMinutes)
					break
				}
			}
		}
	}
	return rows.sorted()
}

func addWalletInterval(rows dailyRows, row DailyStat, prior, current Balance, start, end time.Time, cadenceMinutes int) {
	if prior.Basis != current.Basis || (prior.Total == nil) != (current.Total == nil) {
		return
	}
	oldAmount, newAmount := prior.Available, current.Available
	if prior.Total != nil {
		oldAmount, newAmount = *prior.Total, *current.Total
	}
	oldValue, oldErr := Amount(oldAmount)
	newValue, newErr := Amount(newAmount)
	if oldErr != nil || newErr != nil {
		return
	}

	net := newValue.Sub(oldValue)
	drop := decimal.Max(decimal.Zero, net.Neg())
	row.NetChange = decimalText(net)
	row.UpstreamCost = decimalText(drop)
	row.BalanceIncrease = decimalText(decimal.Max(decimal.Zero, net))
	row.ComparableIntervals = 1
	multiplier, reason := intervalMultiplier(prior.Price, current.Price)
	if reason == "" {
		row.PricedIntervals = 1
		row.PricingStatus = pricingPriced
		row.EstimatedCostCNY = decimalText(drop.DivRound(multiplier, 10))
		row.GapCostCNY = decimalText(decimal.Zero)
	} else {
		row.UnpricedDrop = row.UpstreamCost
		row.UnpricedReasons = map[string]int{reason: 1}
	}

	elapsed := end.Sub(start)
	if elapsed > 2*time.Duration(cadenceMinutes)*time.Minute {
		row.Gap = true
		row.GapCostCNY = row.EstimatedCostCNY
		rows.add(row)
		return
	}

	row.EstimatedSpend = row.UpstreamCost
	if prior.Used != nil && current.Used != nil {
		oldUsed, oldErr := Amount(*prior.Used)
		newUsed, newErr := Amount(*current.Used)
		if oldErr == nil && newErr == nil && !newUsed.LessThan(oldUsed) {
			row.AccountSpend = decimalText(newUsed.Sub(oldUsed))
		}
	}

	boundary := nextLocalDay(start)
	row.CrossDay = !boundary.After(end)
	for from := start; from.Before(end); {
		to := end
		if boundary.Before(to) {
			to = boundary
		}
		part := row
		part.Date = from.Format(time.DateOnly)
		before, after := from.Sub(start), to.Sub(start)
		part.CoveredSeconds = int64(after/time.Second - before/time.Second)
		for _, field := range part.amountFields() {
			*field = elapsedShare(*field, before, after, elapsed)
		}
		// 历史时区可能在午夜向前或向后跳，同一日期可出现多段。
		if rows[part.key()].ComparableIntervals != 0 {
			part.ComparableIntervals, part.PricedIntervals, part.UnpricedReasons = 0, 0, nil
		}
		rows.add(part)
		from = to
		if from.Before(end) {
			boundary = nextLocalDay(from)
		}
	}
}

// 价格版本依据有效倍率更新。来源字段可随默认值改成等值的显式设置。
func intervalMultiplier(prior, current *WalletPrice) (decimal.Decimal, string) {
	if prior == nil || current == nil {
		return decimal.Zero, "missing_price_snapshot"
	}
	if prior.Multiplier == "" || current.Multiplier == "" {
		return decimal.Zero, "missing_multiplier"
	}
	oldValue, oldErr := Amount(prior.Multiplier)
	newValue, newErr := Amount(current.Multiplier)
	if oldErr != nil || newErr != nil || !oldValue.IsPositive() || !newValue.IsPositive() ||
		strings.ContainsAny(prior.Multiplier, "eE") || strings.ContainsAny(current.Multiplier, "eE") {
		return decimal.Zero, "invalid_multiplier"
	}
	if prior.Version != current.Version || !oldValue.Equal(newValue) {
		return decimal.Zero, "price_changed"
	}
	return newValue, ""
}

// nextLocalDay 找到下一个实际的日界线，也处理午夜跳过或重复的情况。
func nextLocalDay(at time.Time) time.Time {
	location := at.Location()
	for {
		year, month, day := at.Date()
		_, offset := at.Zone()
		midnight := time.Date(year, month, day+1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(offset) * time.Second)
		_, zoneEnd := at.ZoneBounds()
		if zoneEnd.IsZero() || midnight.Before(zoneEnd) {
			return midnight.In(location)
		}
		at = zoneEnd.In(location)
		y, m, d := at.Date()
		if y != year || m != month || d != day {
			return at
		}
	}
}

// 每段等于两次累计分摊的差。末段收尾差，所有分段之和等于整段金额。
func elapsedShare(raw *string, before, after, elapsed time.Duration) *string {
	if raw == nil || (before == 0 && after == elapsed) {
		return raw
	}
	amount := decimal.RequireFromString(*raw)
	divisor := decimal.NewFromInt(int64(elapsed))
	cumulative := func(duration time.Duration) decimal.Decimal {
		return amount.Mul(decimal.NewFromInt(int64(duration))).DivRound(divisor, 10)
	}
	return decimalText(cumulative(after).Sub(cumulative(before)))
}

func (row *DailyStat) amountFields() [8]**string {
	return [8]**string{
		&row.EstimatedSpend, &row.AccountSpend, &row.NetChange, &row.UpstreamCost,
		&row.EstimatedCostCNY, &row.GapCostCNY, &row.UnpricedDrop, &row.BalanceIncrease,
	}
}

// mergeDailyStat 累加同一日行的增量。调用方负责去重和按日行键归组。
func mergeDailyStat(target *DailyStat, delta DailyStat) {
	if target.Date == "" {
		*target = delta
		target.UnpricedReasons = maps.Clone(delta.UnpricedReasons)
		return
	}

	values := delta.amountFields()
	for i, field := range target.amountFields() {
		if value := *values[i]; value != nil {
			addDecimal(field, decimal.RequireFromString(*value))
		}
	}
	if delta.FirstSampleAt != nil && (target.FirstSampleAt == nil || delta.FirstSampleAt.Before(*target.FirstSampleAt)) {
		target.FirstSampleAt, target.OpenBalance = delta.FirstSampleAt, delta.OpenBalance
	}
	if delta.LastSampleAt != nil && (target.LastSampleAt == nil || !delta.LastSampleAt.Before(*target.LastSampleAt)) {
		target.LastSampleAt, target.CloseBalance = delta.LastSampleAt, delta.CloseBalance
	}
	if delta.MinBalance != nil && (target.MinBalance == nil ||
		decimal.RequireFromString(*delta.MinBalance).LessThan(decimal.RequireFromString(*target.MinBalance))) {
		target.MinBalance = delta.MinBalance
	}
	if delta.MaxBalance != nil && (target.MaxBalance == nil ||
		decimal.RequireFromString(*delta.MaxBalance).GreaterThan(decimal.RequireFromString(*target.MaxBalance))) {
		target.MaxBalance = delta.MaxBalance
	}
	if len(delta.UnpricedReasons) > 0 {
		if target.UnpricedReasons == nil {
			target.UnpricedReasons = make(map[string]int)
		}
		for reason, count := range delta.UnpricedReasons {
			target.UnpricedReasons[reason] += count
		}
	}
	target.PricedIntervals += delta.PricedIntervals
	target.ComparableIntervals += delta.ComparableIntervals
	switch {
	case target.PricedIntervals == 0:
		target.PricingStatus = pricingUnknown
	case target.PricedIntervals < target.ComparableIntervals:
		target.PricingStatus = pricingPartial
	default:
		target.PricingStatus = pricingPriced
	}
	target.CoveredSeconds += delta.CoveredSeconds
	target.Gap = target.Gap || delta.Gap
	target.CrossDay = target.CrossDay || delta.CrossDay
	target.AvailableOnly = target.AvailableOnly || delta.AvailableOnly
}

func decimalText(value decimal.Decimal) *string {
	text := value.String()
	return &text
}

// addDecimal 保留空值，直至有数值可累计。
func addDecimal(target **string, value decimal.Decimal) {
	if *target != nil {
		old, _ := decimal.NewFromString(**target)
		value = value.Add(old)
	}
	*target = decimalText(value)
}
