package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// statistics_test.go verifies observable spending estimates against wallet changes.
// Explicit sample times cover recharge, identity changes, gaps, and calendar
// boundaries without relying on the machine's current timezone or floating money.

// TestDailyStatsDSTUsesCalendarBoundaries spans a local day containing the autumn clock change.
// Coverage must reflect twenty-five elapsed hours rather than imposing a fixed twenty-four-hour bucket.
func TestDailyStatsDSTUsesCalendarBoundaries(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, location)
	a := statSample(start, "100")
	b := statSample(start.AddDate(0, 0, 1), "75")
	a.Timezone = "America/New_York"
	b.Timezone = a.Timezone
	a.IntervalMinutes = 1440
	b.IntervalMinutes = 1440
	stats := DailyStats([]Observation{a, b})
	var found bool
	for _, day := range stats {
		if day.Date == "2026-11-01" {
			found = true
			require.Equal(t, int64(90000), day.CoveredSeconds)
			require.Equal(t, "25", *day.EstimatedSpend)
		}
	}
	require.True(t, found)
}

// TestDailyStatsRechargeAndGap records a drop, recharge, and another wallet drop.
// Recharges cannot cancel observed consumption, while long gaps remain excluded from estimates.
func TestDailyStatsRechargeAndGap(t *testing.T) {
	start := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	samples := []Observation{}
	for i, value := range []string{"100", "80", "130", "120"} {
		samples = append(samples, statSample(start.Add(time.Duration(i)*time.Hour), value))
	}
	stats := DailyStats(samples)
	require.Len(t, stats, 1)
	require.Equal(t, "30", *stats[0].EstimatedSpend)
	require.Equal(t, "20", *stats[0].NetChange)
	samples = append(samples, statSample(start.Add(8*time.Hour), "90"))
	stats = DailyStats(samples)
	require.Equal(t, "30", *stats[0].EstimatedSpend)
	require.True(t, stats[0].Gap)
	samples[len(samples)-1].WalletEpoch = 2
	stats = DailyStats(samples)
	require.Len(t, stats, 2)
}

// TestDailyStatsMidnightAndUnknown allocates one interval across the site's local midnight.
// A single sample remains unknown and split consumption is explicitly marked estimated.
func TestDailyStatsMidnightAndUnknown(t *testing.T) {
	start := time.Date(2026, 11, 12, 23, 50, 0, 0, time.UTC)
	a := statSample(start, "100")
	b := statSample(start.Add(30*time.Minute), "88")
	require.Nil(t, DailyStats([]Observation{a})[0].EstimatedSpend)
	stats := DailyStats([]Observation{b, a})
	require.Len(t, stats, 2)
	require.Equal(t, "8", *stats[0].EstimatedSpend)
	require.Equal(t, "4", *stats[1].EstimatedSpend)
	require.True(t, stats[0].CrossDay)
	require.True(t, stats[1].CrossDay)
}

// statSample constructs one complete successful wallet observation for statistics tests.
func statSample(at time.Time, value string) Observation {
	return Observation{Source: "balance", Outcome: Healthy, At: at, IntervalMinutes: 60, WalletEpoch: 1, StatisticsEpoch: 1, Timezone: "UTC", Balances: []Balance{{Unit: "USD", Available: value}}}
}

func TestDailyStatsBalanceAndPricedGap(t *testing.T) {
	start := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	price := WalletPrice{Multiplier: "2", Source: walletPriceConfigured}
	var samples []Observation
	for i, value := range []string{"100", "80", "130", "120"} {
		samples = append(samples, pricedStatSample(start.Add(time.Duration(i)*time.Hour), value, price))
	}

	row := onlyDailyStat(t, DailyStats(samples))
	require.Equal(t, "100", *row.OpenBalance)
	require.Equal(t, "120", *row.CloseBalance)
	require.Equal(t, "80", *row.MinBalance)
	require.Equal(t, "130", *row.MaxBalance)
	require.Equal(t, start, *row.FirstSampleAt)
	require.Equal(t, start.Add(3*time.Hour), *row.LastSampleAt)
	require.Equal(t, "50", *row.BalanceIncrease)
	require.Equal(t, "30", *row.UpstreamCost)
	require.Equal(t, "20", *row.NetChange)
	require.Equal(t, "15", *row.EstimatedCostCNY)
	require.Equal(t, "0", *row.GapCostCNY)
	require.Equal(t, "priced", row.PricingStatus)
	require.Equal(t, 3, row.ComparableIntervals)
	require.Equal(t, 3, row.PricedIntervals)
	require.Equal(t, int64(10800), row.CoveredSeconds)

	recovery := pricedStatSample(start.Add(8*time.Hour), "90", price)
	for i := range samples {
		samples[i].Balances[0].Used = new(string)
		*samples[i].Balances[0].Used = []string{"1", "3", "5", "7"}[i]
	}
	recovery.Balances[0].Used = new(string)
	*recovery.Balances[0].Used = "15"
	row = onlyDailyStat(t, DailyStats(append(samples, recovery)))
	require.Equal(t, "30", *row.EstimatedSpend)
	require.Equal(t, "60", *row.UpstreamCost)
	require.Equal(t, "50", *row.BalanceIncrease)
	require.Equal(t, "-10", *row.NetChange)
	require.Equal(t, "30", *row.EstimatedCostCNY)
	require.Equal(t, "15", *row.GapCostCNY)
	require.Equal(t, "6", *row.AccountSpend)
	require.Equal(t, "90", *row.CloseBalance)
	require.Equal(t, start.Add(8*time.Hour), *row.LastSampleAt)
	require.True(t, row.Gap)
	require.Equal(t, 4, row.ComparableIntervals)
	require.Equal(t, 4, row.PricedIntervals)
	require.Equal(t, int64(10800), row.CoveredSeconds)
}

func TestDailyStatsZeroAndFractionalMultipliers(t *testing.T) {
	start := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	price := WalletPrice{Multiplier: "0.5", Source: walletPriceConfigured}
	zero := pricedStatSample(start.Add(time.Hour), "0", price)
	row := onlyDailyStat(t, DailyStats([]Observation{pricedStatSample(start, "3", price), zero}))
	require.Equal(t, "6", *row.EstimatedCostCNY)
	require.Equal(t, "3", *row.UpstreamCost)
	require.Equal(t, "0", *row.CloseBalance)
	require.Equal(t, "0", *row.MinBalance)

	row = onlyDailyStat(t, DailyStats([]Observation{pricedStatSample(start, "0", price), zero}))
	require.Equal(t, "0", *row.EstimatedCostCNY)
	require.Equal(t, "0", *row.UpstreamCost)
	require.Equal(t, "0", *row.NetChange)
	require.Equal(t, "0", *row.BalanceIncrease)
	require.Equal(t, 1, row.PricedIntervals)
	require.Equal(t, "priced", row.PricingStatus)

	row = onlyDailyStat(t, DailyStats([]Observation{zero}))
	require.Equal(t, "0", *row.CloseBalance)
	require.Nil(t, row.EstimatedCostCNY)
	require.Nil(t, row.UpstreamCost)
	require.Nil(t, row.BalanceIncrease)
	require.Nil(t, row.GapCostCNY)
	require.Nil(t, row.UnpricedDrop)
	require.Equal(t, "unknown", row.PricingStatus)
	require.Zero(t, row.ComparableIntervals)
	require.Empty(t, DailyStats(nil))

	price.Multiplier = "3"
	row = onlyDailyStat(t, DailyStats([]Observation{
		pricedStatSample(start, "2", price), pricedStatSample(start.Add(time.Hour), "0", price),
	}))
	require.Equal(t, "0.6666666667", *row.EstimatedCostCNY)
}

func TestDailyStatsPriceVersionsKeepHistoricalCost(t *testing.T) {
	start := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	old := WalletPrice{Multiplier: "2", Source: walletPriceConfigured, Version: 1}
	next := WalletPrice{Multiplier: "4", Source: walletPriceConfigured, Version: 2}
	samples := []Observation{
		pricedStatSample(start, "100", old),
		pricedStatSample(start.Add(time.Hour), "80", old),
		pricedStatSample(start.Add(2*time.Hour), "72", next),
		pricedStatSample(start.Add(3*time.Hour), "64", next),
	}
	row := onlyDailyStat(t, DailyStats(samples))
	require.Equal(t, "36", *row.UpstreamCost)
	require.Equal(t, "8", *row.UnpricedDrop)
	require.Equal(t, "12", *row.EstimatedCostCNY)
	require.Equal(t, "partial", row.PricingStatus)
	require.Equal(t, 2, row.PricedIntervals)
	require.Equal(t, 3, row.ComparableIntervals)
	require.Equal(t, map[string]int{"price_changed": 1}, row.UnpricedReasons)

	old.Version = 3
	row = onlyDailyStat(t, pairDaily(&samples[1], pricedStatSample(start.Add(4*time.Hour), "76", old)))
	require.Equal(t, "4", *row.UpstreamCost)
	require.Equal(t, "4", *row.UnpricedDrop)
	require.Nil(t, row.EstimatedCostCNY)
	require.Equal(t, "unknown", row.PricingStatus)
	require.Equal(t, map[string]int{"price_changed": 1}, row.UnpricedReasons)
}

func TestDailyStatsDefaultAndLegacySnapshots(t *testing.T) {
	start := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	a, b := statSample(start, "10"), statSample(start.Add(time.Hour), "8")
	a.Balances, b.Balances = nil, nil
	site := Site{}
	for _, unit := range []string{"CNY", "USD", "TOKENS"} {
		price := site.PriceFor(unit)
		a.Balances = append(a.Balances, Balance{Unit: unit, Available: "10", Price: &price})
		b.Balances = append(b.Balances, Balance{Unit: unit, Available: "8", Price: &price})
	}
	stats := DailyStats([]Observation{a, b})
	require.Len(t, stats, 3)
	require.Equal(t, "CNY", stats[0].Unit)
	require.Equal(t, "2", *stats[0].EstimatedCostCNY)
	require.Equal(t, "priced", stats[0].PricingStatus)
	for _, row := range stats[1:] {
		require.Equal(t, "2", *row.UpstreamCost)
		require.Equal(t, "2", *row.UnpricedDrop)
		require.Equal(t, "unknown", row.PricingStatus)
		require.Equal(t, 1, row.ComparableIntervals)
		require.Zero(t, row.PricedIntervals)
		require.Nil(t, row.EstimatedCostCNY)
		require.Equal(t, map[string]int{"missing_multiplier": 1}, row.UnpricedReasons)
	}

	a.Balances, b.Balances = a.Balances[:1], b.Balances[:1]
	configured := WalletPrice{Multiplier: "1.0", Source: walletPriceConfigured}
	b.Balances[0].Price = &configured
	row := onlyDailyStat(t, DailyStats([]Observation{a, b}))
	require.Equal(t, "2", *row.EstimatedCostCNY)

	a.Balances[0].Price = nil
	for _, price := range []*WalletPrice{&configured, nil} {
		b.Balances[0].Price = price
		row = onlyDailyStat(t, DailyStats([]Observation{a, b}))
		require.Equal(t, "2", *row.UpstreamCost)
		require.Equal(t, "2", *row.UnpricedDrop)
		require.Equal(t, "unknown", row.PricingStatus)
		require.Nil(t, row.EstimatedCostCNY)
		require.Equal(t, map[string]int{"missing_price_snapshot": 1}, row.UnpricedReasons)
	}

	a.Balances[0].Price = &configured
	for _, multiplier := range []string{"0", "-1", "0.00000000001", "1e0", "invalid"} {
		b.Balances[0].Price = &WalletPrice{Multiplier: multiplier}
		row = onlyDailyStat(t, DailyStats([]Observation{a, b}))
		require.Nil(t, row.EstimatedCostCNY, multiplier)
		require.Equal(t, map[string]int{"invalid_multiplier": 1}, row.UnpricedReasons, multiplier)
	}
}

func TestDailyStatsNewAPIBasisChange(t *testing.T) {
	start := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	settings := defaultNewAPISettings()
	price := WalletPrice{Multiplier: "2", Source: walletPriceConfigured}
	self := func(at time.Time, quota string) Observation {
		sample := statSample(at, "")
		var err error
		sample.Balances, err = newAPISelf([]byte(`{"data":{"quota":`+quota+`}}`), settings, "")
		require.NoError(t, err)
		sample.Balances[0].Price = &price
		return sample
	}
	a := self(start, "5000000")
	b := self(start.Add(time.Hour), "4000000")
	row := onlyDailyStat(t, DailyStats([]Observation{a, b}))
	require.Equal(t, "10", *row.OpenBalance)
	require.Equal(t, "8", *row.CloseBalance)
	require.Equal(t, "2", *row.UpstreamCost)
	require.Equal(t, "1", *row.EstimatedCostCNY)

	settings.factor = decimal.NewFromInt(1000000)
	changed := self(start.Add(time.Hour), "5000000")
	row = onlyDailyStat(t, DailyStats([]Observation{a, changed}))
	require.Equal(t, "10", *row.OpenBalance)
	require.Equal(t, "5", *row.CloseBalance)
	require.Nil(t, row.UpstreamCost)
	require.Nil(t, row.NetChange)
	require.Nil(t, row.BalanceIncrease)
	require.Nil(t, row.EstimatedCostCNY)
	require.Zero(t, row.ComparableIntervals)
	require.Equal(t, "unknown", row.PricingStatus)

	recovered := self(start.Add(2*time.Hour), "4000000")
	row = onlyDailyStat(t, DailyStats([]Observation{a, changed, recovered}))
	require.Equal(t, "1", *row.UpstreamCost)
	require.Equal(t, "0.5", *row.EstimatedCostCNY)
	require.Equal(t, 1, row.ComparableIntervals)

	changed.Balances[0].Basis = ""
	row = onlyDailyStat(t, pairDaily(&a, changed))
	require.Nil(t, row.UpstreamCost)
	require.Equal(t, "5", *row.OpenBalance)

	b.Balances[0].Basis = settings.basis("token.data.user_balance", false)
	a.Balances[0].Basis = settings.basis("self.data.quota", false)
	row = onlyDailyStat(t, pairDaily(&a, b))
	require.Nil(t, row.UpstreamCost)
	require.Nil(t, row.EstimatedCostCNY)
	require.Zero(t, row.ComparableIntervals)
}

func TestDailyStatsFrozenBalanceAndAccountSpend(t *testing.T) {
	start := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	price := WalletPrice{Multiplier: "2", Source: walletPriceConfigured}
	total, oldUsed, newUsed, resetUsed := "100", "9", "12", "1"
	a := pricedStatSample(start, "100", price)
	b := pricedStatSample(start.Add(time.Hour), "60", price)
	a.Balances[0].Total, a.Balances[0].Used = &total, &oldUsed
	b.Balances[0].Total, b.Balances[0].Used = &total, &newUsed
	failed := statSample(start.Add(20*time.Minute), "1")
	failed.Outcome, failed.HTTPStatus = Authentication, 401
	probe := statSample(start.Add(40*time.Minute), "0")
	probe.Source = "probe"
	samples := []Observation{probe, b, failed, a}
	row := onlyDailyStat(t, DailyStats(samples))
	require.Equal(t, "100", *row.OpenBalance)
	require.Equal(t, "60", *row.CloseBalance)
	require.Equal(t, "60", *row.MinBalance)
	require.Equal(t, "100", *row.MaxBalance)
	require.Equal(t, "0", *row.UpstreamCost)
	require.Equal(t, "0", *row.EstimatedCostCNY)
	require.Equal(t, "3", *row.AccountSpend)
	require.False(t, row.AvailableOnly)
	require.Equal(t, 1, row.PricedIntervals)
	require.Equal(t, b.At, *row.LastSampleAt)
	require.Empty(t, pairDaily(&a, failed))
	require.Empty(t, pairDaily(&a, probe))
	require.Nil(t, onlyDailyStat(t, pairDaily(&failed, b)).UpstreamCost)

	for _, used := range []*string{nil, &resetUsed} {
		b.Balances[0].Used = used
		row = onlyDailyStat(t, DailyStats(samples))
		require.Nil(t, row.AccountSpend)
		require.Equal(t, "0", *row.UpstreamCost)
		require.Equal(t, "0", *row.EstimatedCostCNY)
	}

	b.Balances[0].Total = nil
	row = onlyDailyStat(t, DailyStats(samples))
	require.True(t, row.AvailableOnly)
	require.Equal(t, "60", *row.CloseBalance)
	require.Nil(t, row.UpstreamCost)
	require.Nil(t, row.EstimatedCostCNY)
}

func TestDailyStatsCrossDayTailAndIntervalCounts(t *testing.T) {
	start := time.Date(2026, 11, 12, 23, 50, 0, 0, time.UTC)
	price := WalletPrice{Multiplier: "2", Source: walletPriceConfigured}
	a := pricedStatSample(start, "100", price)
	b := pricedStatSample(start.Add(30*time.Minute), "88", price)
	rows := DailyStats([]Observation{a, b})
	require.Len(t, rows, 2)
	require.Equal(t, "2026-11-13", rows[0].Date)
	require.Equal(t, "4", *rows[0].EstimatedCostCNY)
	require.Equal(t, "8", *rows[0].UpstreamCost)
	require.Equal(t, int64(1200), rows[0].CoveredSeconds)
	require.Equal(t, "2026-11-12", rows[1].Date)
	require.Equal(t, "2", *rows[1].EstimatedCostCNY)
	require.Equal(t, "4", *rows[1].UpstreamCost)
	require.Equal(t, int64(600), rows[1].CoveredSeconds)
	for _, row := range rows {
		require.True(t, row.CrossDay)
		require.Equal(t, 1, row.ComparableIntervals)
		require.Equal(t, 1, row.PricedIntervals)
		require.Equal(t, "priced", row.PricingStatus)
	}

	pair := pairDaily(&a, b)
	require.Len(t, pair, 2)
	require.Equal(t, "88", *pair[0].OpenBalance)
	require.Equal(t, "88", *pair[0].CloseBalance)
	require.Equal(t, b.At, *pair[0].FirstSampleAt)
	require.Equal(t, b.At, *pair[0].LastSampleAt)
	require.Nil(t, pair[1].OpenBalance)
	require.Nil(t, pair[1].CloseBalance)
	require.Nil(t, pair[1].MinBalance)
	require.Nil(t, pair[1].MaxBalance)
	require.Nil(t, pair[1].FirstSampleAt)
	require.Nil(t, pair[1].LastSampleAt)

	price.Multiplier = "1"
	a = pricedStatSample(start, "1", price)
	b = pricedStatSample(start.Add(30*time.Minute), "0.9999999999", price)
	rows = DailyStats([]Observation{a, b})
	require.Equal(t, "0", *rows[1].UpstreamCost)
	require.Equal(t, "0", *rows[1].EstimatedCostCNY)
	require.Equal(t, "0.0000000001", *rows[0].UpstreamCost)
	require.Equal(t, "0.0000000001", *rows[0].EstimatedCostCNY)
	require.Equal(t, "-0.0000000001", *rows[0].NetChange)

	b.Balances[0].Price = nil
	rows = DailyStats([]Observation{a, b})
	require.Equal(t, "0", *rows[1].UnpricedDrop)
	require.Equal(t, "0.0000000001", *rows[0].UnpricedDrop)
	for _, row := range rows {
		require.Equal(t, "unknown", row.PricingStatus)
		require.Equal(t, 1, row.ComparableIntervals)
		require.Zero(t, row.PricedIntervals)
		require.Nil(t, row.EstimatedCostCNY)
		require.Equal(t, map[string]int{"missing_price_snapshot": 1}, row.UnpricedReasons)
	}

	b = pricedStatSample(start.Add(10*time.Minute), "0", price)
	rows = DailyStats([]Observation{a, b})
	require.Equal(t, "0", *rows[0].OpenBalance)
	require.Zero(t, rows[0].ComparableIntervals)
	require.Nil(t, rows[0].EstimatedCostCNY)
	require.Equal(t, "1", *rows[1].EstimatedCostCNY)
	require.Equal(t, 1, rows[1].ComparableIntervals)
}

func TestDailyStatsAllocatesByActualLocalDayLength(t *testing.T) {
	for _, tt := range []struct {
		zone                      string
		year                      int
		month                     time.Month
		day, hour, minute         int
		duration                  time.Duration
		startBalance, endBalance  string
		firstSeconds, lastSeconds int64
		firstCost, lastCost       string
	}{
		{"America/New_York", 2026, time.November, 1, 0, 0, 26 * time.Hour, "100", "74", 90000, 3600, "12.5", "0.5"},
		{"America/Sao_Paulo", 2018, time.November, 3, 23, 30, time.Hour, "100", "88", 1800, 1800, "3", "3"},
	} {
		t.Run(tt.zone, func(t *testing.T) {
			loc, err := time.LoadLocation(tt.zone)
			require.NoError(t, err)
			start := time.Date(tt.year, tt.month, tt.day, tt.hour, tt.minute, 0, 0, loc)
			price := WalletPrice{Multiplier: "2", Source: walletPriceConfigured}
			a, b := pricedStatSample(start, tt.startBalance, price), pricedStatSample(start.Add(tt.duration), tt.endBalance, price)
			a.Timezone, b.Timezone = tt.zone, tt.zone
			a.IntervalMinutes = 1440
			rows := DailyStats([]Observation{a, b})
			require.Len(t, rows, 2)
			require.Equal(t, start.Format(time.DateOnly), rows[1].Date)
			require.Equal(t, b.At.Format(time.DateOnly), rows[0].Date)
			require.Equal(t, tt.firstSeconds, rows[1].CoveredSeconds)
			require.Equal(t, tt.lastSeconds, rows[0].CoveredSeconds)
			require.Equal(t, tt.firstCost, *rows[1].EstimatedCostCNY)
			require.Equal(t, tt.lastCost, *rows[0].EstimatedCostCNY)
		})
	}
}

func TestDailyStatsDayWithoutSamplesStillHasAllocatedCost(t *testing.T) {
	start := time.Date(2026, 11, 12, 23, 30, 0, 0, time.UTC)
	price := WalletPrice{Multiplier: "2", Source: walletPriceConfigured}
	a := pricedStatSample(start, "100", price)
	b := pricedStatSample(start.Add(25*time.Hour), "50", price)
	a.IntervalMinutes = 1440
	oldUsed, newUsed := "5", "30"
	a.Balances[0].Used, b.Balances[0].Used = &oldUsed, &newUsed
	rows := DailyStats([]Observation{a, b})
	require.Len(t, rows, 3)
	require.Equal(t, "2026-11-13", rows[1].Date)
	require.Equal(t, "48", *rows[1].UpstreamCost)
	require.Equal(t, "24", *rows[1].AccountSpend)
	require.Equal(t, "24", *rows[1].EstimatedCostCNY)
	require.Equal(t, "0.5", *rows[0].EstimatedCostCNY)
	require.Equal(t, "0.5", *rows[2].EstimatedCostCNY)
	require.Equal(t, int64(86400), rows[1].CoveredSeconds)
	require.Equal(t, 1, rows[1].PricedIntervals)
	require.Equal(t, 1, rows[1].ComparableIntervals)
	require.Nil(t, rows[1].OpenBalance)
	require.Nil(t, rows[1].CloseBalance)
	require.Nil(t, rows[1].MinBalance)
	require.Nil(t, rows[1].MaxBalance)
	require.Nil(t, rows[1].FirstSampleAt)
	require.Nil(t, rows[1].LastSampleAt)
}

func TestDailyStatsGapOnlyTouchesRecoveryDay(t *testing.T) {
	start := time.Date(2026, 11, 12, 23, 30, 0, 0, time.UTC)
	price := WalletPrice{Multiplier: "2", Source: walletPriceConfigured}
	a := pricedStatSample(start, "100", price)
	b := pricedStatSample(start.Add(12*time.Hour), "80", price)
	b.IntervalMinutes = 1440
	row := onlyDailyStat(t, pairDaily(&a, b))
	require.Equal(t, "2026-11-13", row.Date)
	require.Equal(t, "20", *row.UpstreamCost)
	require.Equal(t, "10", *row.GapCostCNY)
	require.Equal(t, "10", *row.EstimatedCostCNY)
	require.Equal(t, "0", *row.BalanceIncrease)
	require.Nil(t, row.EstimatedSpend)
	require.True(t, row.Gap)
	require.False(t, row.CrossDay)
	require.Zero(t, row.CoveredSeconds)
	require.Equal(t, 1, row.ComparableIntervals)

	b.Balances[0].Available = "140"
	row = onlyDailyStat(t, pairDaily(&a, b))
	require.Equal(t, "40", *row.BalanceIncrease)
	require.Equal(t, "40", *row.NetChange)
	require.Equal(t, "0", *row.UpstreamCost)
	require.Equal(t, "0", *row.GapCostCNY)

	b = pricedStatSample(start.Add(2*time.Hour), "90", price)
	rows := pairDaily(&a, b)
	require.Len(t, rows, 2)
	require.False(t, rows[0].Gap)
	require.False(t, rows[1].Gap)
	require.Equal(t, int64(5400), rows[0].CoveredSeconds)
	require.Equal(t, int64(1800), rows[1].CoveredSeconds)
}

func TestDailyStatsWalletCalendarAndUnitSegments(t *testing.T) {
	start := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	price := WalletPrice{Multiplier: "2", Source: walletPriceConfigured}
	a := pricedStatSample(start, "100", price)
	for name, change := range map[string]func(*Observation){
		"wallet":     func(o *Observation) { o.WalletEpoch++ },
		"calendar":   func(o *Observation) { o.StatisticsEpoch++ },
		"timezone":   func(o *Observation) { o.Timezone = "Asia/Shanghai" },
		"unit":       func(o *Observation) { o.Balances[0].Unit = "CNY" },
		"same time":  func(o *Observation) { o.At = start },
		"prior time": func(o *Observation) { o.At = start.Add(-time.Hour) },
	} {
		t.Run(name, func(t *testing.T) {
			b := pricedStatSample(start.Add(time.Hour), "70", price)
			change(&b)
			row := onlyDailyStat(t, pairDaily(&a, b))
			require.Nil(t, row.UpstreamCost)
			require.Nil(t, row.EstimatedCostCNY)
			require.Zero(t, row.ComparableIntervals)
			require.Equal(t, "70", *row.OpenBalance)
			require.Equal(t, b.At, *row.FirstSampleAt)
		})
	}
}

func TestMergeDailyStatMatchesHistoryAndJSON(t *testing.T) {
	start := time.Date(2026, 11, 12, 22, 50, 0, 0, time.UTC)
	price := WalletPrice{Multiplier: "2", Source: walletPriceConfigured}
	samples := []Observation{
		pricedStatSample(start, "100", price),
		pricedStatSample(start.Add(time.Hour), "80", price),
		statSample(start.Add(90*time.Minute), "90"),
		statSample(start.Add(2*time.Hour), "85"),
	}
	var deltas []DailyStat
	var previous *Observation
	for i, sample := range samples {
		deltas = append(deltas, pairDaily(previous, sample)...)
		previous = &samples[i]
	}
	merged := map[string]DailyStat{}
	for i := len(deltas) - 1; i >= 0; i-- {
		delta := deltas[i]
		row := merged[delta.Date]
		mergeDailyStat(&row, delta)
		merged[delta.Date] = row
	}
	rows := DailyStats(samples)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.Equal(t, row, merged[row.Date])
	}
	require.Equal(t, map[string]int{"missing_price_snapshot": 1}, deltas[len(deltas)-1].UnpricedReasons)
	require.Equal(t, map[string]int{"missing_price_snapshot": 2}, rows[0].UnpricedReasons)
	require.Equal(t, "partial", rows[1].PricingStatus)
	require.Equal(t, "0", *rows[1].UnpricedDrop)
	require.Equal(t, "100", *rows[1].OpenBalance)
	require.Equal(t, "80", *rows[1].CloseBalance)
	require.Equal(t, "90", *rows[0].OpenBalance)
	require.Equal(t, "85", *rows[0].CloseBalance)

	raw, err := json.Marshal(rows[0])
	require.NoError(t, err)
	require.JSONEq(t, `{
		"algorithm_version":2, "date":"2026-11-13", "timezone":"UTC", "unit":"USD",
		"wallet_epoch":1, "statistics_epoch":1, "estimated_spend":"5", "account_spend":null,
		"net_change":"1.6666666667", "upstream_cost":"5", "estimated_cost_cny":null,
		"gap_cost_cny":null, "unpriced_drop":"5", "balance_increase":"6.6666666667",
		"open_balance":"90", "close_balance":"85", "min_balance":"85", "max_balance":"90",
		"first_sample_at":"2026-11-13T00:20:00Z", "last_sample_at":"2026-11-13T00:50:00Z",
		"pricing_status":"unknown", "priced_intervals":0, "comparable_intervals":2,
		"unpriced_reasons":{"missing_price_snapshot":2}, "covered_seconds":3000,
		"gap":false, "cross_day":true, "available_only":true
	}`, string(raw))
}

func pricedStatSample(at time.Time, value string, price WalletPrice) Observation {
	sample := statSample(at, value)
	sample.Balances[0].Price = &price
	return sample
}

func onlyDailyStat(t *testing.T, rows []DailyStat) DailyStat {
	t.Helper()
	require.Len(t, rows, 1)
	return rows[0]
}
