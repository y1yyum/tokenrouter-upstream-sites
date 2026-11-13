package main

import (
	"testing"
	"time"

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
