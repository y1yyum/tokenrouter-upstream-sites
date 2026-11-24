package main

import (
	"time"

	"github.com/shopspring/decimal"
)

// legacyDailyEvidence 复现 1.1.1 发布日报的计算口径，仅用于判断历史
// 是否足以回建。分段独立舍入，并像旧版一样截掉每段覆盖时长的小数秒。
func legacyDailyEvidence(samples []Observation) map[dailyStatKey]*DailyStat {
	rows := make(map[dailyStatKey]*DailyStat)
	for i, current := range samples {
		loc, err := time.LoadLocation(current.Timezone)
		if err != nil {
			continue
		}
		end := current.At.In(loc)
		for _, balance := range current.Balances {
			row := legacyDailyEntry(rows, current, balance, end)
			if i == 0 {
				continue
			}
			previous := samples[i-1]
			if previous.WalletEpoch != current.WalletEpoch || previous.StatisticsEpoch != current.StatisticsEpoch ||
				previous.Timezone != current.Timezone || !current.At.After(previous.At) {
				continue
			}
			for _, prior := range previous.Balances {
				if prior.Unit == balance.Unit {
					addLegacyInterval(rows, row, previous, current, prior, balance, previous.At.In(loc), end)
					break
				}
			}
		}
	}
	return rows
}

func legacyDailyEntry(rows map[dailyStatKey]*DailyStat, o Observation, b Balance, at time.Time) *DailyStat {
	date := at.Format(time.DateOnly)
	// 旧版按统计分段区分时区，键内省略时区名。
	key := dailyStatKey{date: date, unit: b.Unit, walletEpoch: o.WalletEpoch, statisticsEpoch: o.StatisticsEpoch}
	if rows[key] == nil {
		rows[key] = &DailyStat{
			AlgorithmVersion: 1, Date: date, Timezone: o.Timezone, Unit: b.Unit,
			WalletEpoch: o.WalletEpoch, StatisticsEpoch: o.StatisticsEpoch, AvailableOnly: b.Total == nil,
		}
	}
	return rows[key]
}

func addLegacyInterval(rows map[dailyStatKey]*DailyStat, row *DailyStat, previous, current Observation, prior, balance Balance, start, end time.Time) {
	if (prior.Total == nil) != (balance.Total == nil) {
		return
	}
	before, after := prior.Available, balance.Available
	if prior.Total != nil {
		before, after = *prior.Total, *balance.Total
	}
	oldValue, oldErr := Amount(before)
	newValue, newErr := Amount(after)
	if oldErr != nil || newErr != nil {
		return
	}
	net := newValue.Sub(oldValue)
	elapsed := current.At.Sub(previous.At)
	if elapsed > 2*time.Duration(previous.IntervalMinutes)*time.Minute {
		row.Gap = true
		addDecimal(&row.NetChange, net)
		return
	}

	spend := decimal.Max(decimal.Zero, net.Neg())
	var used *decimal.Decimal
	if prior.Used != nil && balance.Used != nil {
		oldUsed, oldErr := Amount(*prior.Used)
		newUsed, newErr := Amount(*balance.Used)
		if oldErr == nil && newErr == nil && !newUsed.LessThan(oldUsed) {
			delta := newUsed.Sub(oldUsed)
			used = &delta
		}
	}
	cross := start.Format(time.DateOnly) != end.Format(time.DateOnly)
	for start.Before(end) {
		nextDay := time.Date(start.Year(), start.Month(), start.Day()+1, 0, 0, 0, 0, start.Location())
		stop := end
		if nextDay.Before(stop) {
			stop = nextDay
		}
		if !stop.After(start) {
			return
		}

		part := legacyDailyEntry(rows, current, balance, start)
		seconds := stop.Sub(start).Seconds()
		fraction := decimal.NewFromFloat(seconds).Div(decimal.NewFromFloat(elapsed.Seconds()))
		addDecimal(&part.EstimatedSpend, spend.Mul(fraction).Round(10))
		addDecimal(&part.NetChange, net.Mul(fraction).Round(10))
		if used != nil {
			addDecimal(&part.AccountSpend, used.Mul(fraction).Round(10))
		}
		part.CoveredSeconds += int64(seconds)
		part.CrossDay = part.CrossDay || cross
		start = stop
	}
}
