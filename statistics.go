package main

import (
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// statistics.go derives daily wallet observations without inventing exact bills.
// Only comparable successful samples contribute differences. A recharge contributes
// no estimated consumption, while a long gap, changed wallet, changed calendar,
// or missing currency breaks continuity. Total funds take precedence over available
// funds so an upstream freeze is not counted as spending when totals are known.
//
// Intervals crossing a calendar boundary are allocated by elapsed seconds and
// explicitly marked estimated. Calendar boundaries use the sample's IANA timezone,
// including daylight-saving changes. Account cumulative spending remains a separate
// metric and is never inferred from key usage or from a wallet's balance decline.

// DailyStat describes one wallet, calendar, day, and currency observation segment.
type DailyStat struct {
	AlgorithmVersion int     `json:"algorithm_version"` // immutable aggregation algorithm identifier
	Date             string  `json:"date"`              // local calendar date
	Timezone         string  `json:"timezone"`          // immutable segment timezone
	Unit             string  `json:"unit"`              // wallet currency denomination
	WalletEpoch      int64   `json:"wallet_epoch"`      // account identity segment
	StatisticsEpoch  int64   `json:"statistics_epoch"`  // timezone identity segment
	EstimatedSpend   *string `json:"estimated_spend"`   // null until comparable observations
	AccountSpend     *string `json:"account_spend"`     // null without trustworthy account counter
	NetChange        *string `json:"net_change"`        // observed net wallet change
	CoveredSeconds   int64   `json:"covered_seconds"`   // accepted observation interval coverage
	Gap              bool    `json:"gap"`               // excluded long observation interval
	CrossDay         bool    `json:"cross_day"`         // proportional calendar boundary allocation
	AvailableOnly    bool    `json:"available_only"`    // frozen funds can affect estimate
}

// DailyStats aggregates comparable intervals from an immutable observation history.
// The caller supplies successful and failed rounds in any order; only validated
// wallet observations become samples. The result keeps wallet and calendar epochs
// separate and leaves insufficiently observed days unknown. Cross-midnight intervals
// use actual elapsed seconds, while gaps over twice the earlier sample's cadence
// contribute only net change and can never trigger estimated daily spending.
func DailyStats(history []Observation) []DailyStat {
	samples := make([]Observation, 0, len(history))
	for _, o := range history {
		if o.Source == "balance" && o.Outcome == Healthy {
			samples = append(samples, o)
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].At.Before(samples[j].At) })
	stats := map[string]*DailyStat{}
	for i, current := range samples {
		location, err := time.LoadLocation(current.Timezone)
		if err != nil {
			continue
		}
		for _, balance := range current.Balances {
			entry := dailyEntry(stats, current, balance, current.At.In(location))
			if i == 0 {
				continue
			}
			previous := samples[i-1]
			if previous.WalletEpoch != current.WalletEpoch || previous.StatisticsEpoch != current.StatisticsEpoch || previous.Timezone != current.Timezone || !current.At.After(previous.At) {
				continue
			}
			var prior *Balance
			for j := range previous.Balances {
				if previous.Balances[j].Unit == balance.Unit {
					prior = &previous.Balances[j]
					break
				}
			}
			if prior == nil || (prior.Total == nil) != (balance.Total == nil) {
				continue
			}
			oldAmount, newAmount := prior.Available, balance.Available
			if prior.Total != nil {
				oldAmount = *prior.Total
				newAmount = *balance.Total
			}
			oldValue, e1 := Amount(oldAmount)
			newValue, e2 := Amount(newAmount)
			if e1 != nil || e2 != nil {
				continue
			}
			net := newValue.Sub(oldValue)
			elapsed := current.At.Sub(previous.At)
			if elapsed > time.Duration(2*previous.IntervalMinutes)*time.Minute {
				entry.Gap = true
				addDecimal(&entry.NetChange, net)
				continue
			}
			spend := decimal.Max(decimal.Zero, net.Neg())
			var used *decimal.Decimal
			if prior.Used != nil && balance.Used != nil {
				a, e1 := Amount(*prior.Used)
				b, e2 := Amount(*balance.Used)
				if e1 == nil && e2 == nil && !b.LessThan(a) {
					delta := b.Sub(a)
					used = &delta
				}
			}
			start := previous.At.In(location)
			end := current.At.In(location)
			cross := start.Format(time.DateOnly) != end.Format(time.DateOnly)
			for start.Before(end) {
				nextDay := time.Date(start.Year(), start.Month(), start.Day()+1, 0, 0, 0, 0, location)
				stop := end
				if nextDay.Before(stop) {
					stop = nextDay
				}
				part := dailyEntry(stats, current, balance, start)
				seconds := stop.Sub(start).Seconds()
				// Allocation describes observed elapsed time, not the unknown instant
				// upstream spending happened; every split day retains this quality marker.
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
	}
	result := make([]DailyStat, 0, len(stats))
	for _, value := range stats {
		result = append(result, *value)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Date == result[j].Date {
			return result[i].Unit < result[j].Unit
		}
		return result[i].Date > result[j].Date
	})
	return result
}

// dailyEntry creates an unknown day until a valid interval supplies spending evidence.
func dailyEntry(stats map[string]*DailyStat, o Observation, b Balance, at time.Time) *DailyStat {
	date := at.Format(time.DateOnly)
	key := fmtSource(o.WalletEpoch, fmtSource(o.StatisticsEpoch, date+":"+b.Unit))
	if stats[key] == nil {
		stats[key] = &DailyStat{AlgorithmVersion: 1, Date: date, Timezone: o.Timezone, Unit: b.Unit, WalletEpoch: o.WalletEpoch, StatisticsEpoch: o.StatisticsEpoch, AvailableOnly: b.Total == nil}
	}
	return stats[key]
}

// addDecimal accumulates exact decimal values while retaining unknown before first evidence.
func addDecimal(target **string, value decimal.Decimal) {
	if *target != nil {
		old, _ := decimal.NewFromString(**target)
		value = value.Add(old)
	}
	text := value.String()
	*target = &text
}
