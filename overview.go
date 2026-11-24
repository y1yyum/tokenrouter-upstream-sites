package main

import (
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/shopspring/decimal"
)

func (p *Plugin) overviewHTTP(w http.ResponseWriter, r *http.Request) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(values["period"]) > 1 || len(values["timezone"]) > 1 {
		reply(w, nil, ErrInvalid)
		return
	}
	q, err := newOverviewQuery(values.Get("period"), values.Get("timezone"))
	if err != nil {
		reply(w, nil, err)
		return
	}
	ctx := r.Context()
	sites, err := p.sites(ctx)
	if err != nil {
		reply(w, nil, err)
		return
	}
	totals := newOverviewTotals(time.Now(), q)
	for _, stored := range sites {
		var daily []DailyStat
		if q.includesPeriod(stored.Site) {
			if q.days == 1 {
				date := totals.periodGroup(stored.Site.Config.Timezone).window.to
				daily, err = p.readDailyForDay(ctx, stored.Site, date)
			} else {
				daily, err = p.readDaily(ctx, stored.Site)
			}
			if err != nil {
				reply(w, nil, err)
				return
			}
		}
		totals.addSite(stored.Site, daily)
	}
	reply(w, totals.result(), nil)
}

type overviewQuery struct {
	period   string
	days     int
	timezone string
}

func newOverviewQuery(period, timezone string) (overviewQuery, error) {
	q := overviewQuery{period: period, timezone: timezone}
	switch period {
	case "", "today":
		q.period, q.days = "today", 1
	case "7d":
		q.days = 7
	case "30d":
		q.days = 30
	case "retained":
	default:
		return overviewQuery{}, ErrInvalid
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return overviewQuery{}, ErrInvalid
	}
	return q, nil
}

func (q overviewQuery) includesPeriod(site Site) bool {
	return !site.Archived && (q.timezone == "" || q.timezone == overviewTimezone(site.Config.Timezone))
}

func overviewTimezone(timezone string) string {
	if timezone == "" {
		return "UTC"
	}
	return timezone
}

type overviewResponse struct {
	Period  string           `json:"period"`
	AsOf    time.Time        `json:"as_of"`
	Current overviewCurrent  `json:"current"`
	Sites   []overviewSite   `json:"sites"`
	Periods []overviewPeriod `json:"periods"`
}

type overviewCurrent struct {
	ByUnit              []overviewCurrentUnit `json:"by_unit"`
	EstimatedBalanceCNY *string               `json:"estimated_balance_cny"`
	FreshSites          int                   `json:"fresh_sites"`
	PricedSites         int                   `json:"priced_sites"`
	UnpricedSites       int                   `json:"unpriced_sites"`
	StaleSites          int                   `json:"stale_sites"`
	UnknownSites        int                   `json:"unknown_sites"`
	ArchivedSites       int                   `json:"archived_sites"`
}

type overviewCurrentUnit struct {
	Unit   string `json:"unit"`
	Amount string `json:"amount"`
	Sites  int    `json:"sites"`
}

type overviewSite struct {
	SiteID                 int64                   `json:"site_id"`
	Status                 string                  `json:"status"`
	Balances               []overviewBalance       `json:"balances"`
	BalancePricingStatus   string                  `json:"balance_pricing_status"`
	SampledAt              *time.Time              `json:"sampled_at"`
	LastBalanceObservation *overviewBalanceAttempt `json:"last_balance_observation"`
	Period                 *overviewPeriod         `json:"period"`
}

type overviewBalanceAttempt struct {
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"`
	Code    string    `json:"code"`
}

type overviewBalance struct {
	Unit       string  `json:"unit"`
	Available  string  `json:"available"`
	Multiplier *string `json:"multiplier"`
	Source     string  `json:"source"`
	BalanceCNY *string `json:"balance_cny"`
}

type overviewCost struct {
	EstimatedCostCNY   *string `json:"estimated_cost_cny"`
	GapCostCNY         *string `json:"gap_cost_cny"`
	PricingStatus      string  `json:"pricing_status"`
	HasUnknownCoverage bool    `json:"has_unknown_coverage"`
}

type overviewUnitCost struct {
	Unit                string         `json:"unit"`
	UpstreamCost        *string        `json:"upstream_cost"`
	UnpricedDrop        *string        `json:"unpriced_drop"`
	PricedIntervals     int            `json:"priced_intervals"`
	ComparableIntervals int            `json:"comparable_intervals"`
	UnpricedReasons     map[string]int `json:"unpriced_reasons,omitempty"`
	CoveredSeconds      int64          `json:"covered_seconds"`
	Gap                 bool           `json:"gap"`
	overviewCost
}

type overviewPeriod struct {
	Timezone string             `json:"timezone"`
	From     *string            `json:"from"`
	To       *string            `json:"to"`
	ByUnit   []overviewUnitCost `json:"by_unit"`
	overviewCost
}

type overviewTotals struct {
	query        overviewQuery
	response     overviewResponse
	currentUnits map[string]overviewCurrentUnit
	groups       map[string]*overviewPeriodTotals
}

func newOverviewTotals(now time.Time, q overviewQuery) *overviewTotals {
	return &overviewTotals{
		query: q,
		response: overviewResponse{
			Period: q.period, AsOf: now.UTC(),
			Sites: []overviewSite{}, Periods: []overviewPeriod{},
			Current: overviewCurrent{ByUnit: []overviewCurrentUnit{}},
		},
		currentUnits: make(map[string]overviewCurrentUnit),
		groups:       make(map[string]*overviewPeriodTotals),
	}
}

func (o *overviewTotals) addSite(site Site, rows []DailyStat) {
	if site.Archived {
		o.response.Current.ArchivedSites++
		return
	}
	row := overviewSiteBalance(o.response.AsOf, site)
	o.response.Current.addSite(o.currentUnits, row)
	if o.query.includesPeriod(site) {
		group := o.periodGroup(site.Config.Timezone)
		totals := overviewPeriodTotals{window: group.window, units: make(map[string]*overviewUnitCost)}
		for _, daily := range rows {
			if totals.window.includes(site, daily) {
				totals.add(daily)
				group.add(daily)
			}
		}
		period := totals.result()
		row.Period = &period
	}
	o.response.Sites = append(o.response.Sites, row)
}

func (o *overviewTotals) periodGroup(timezone string) *overviewPeriodTotals {
	zone := overviewTimezone(timezone)
	group := o.groups[zone]
	if group == nil {
		group = newOverviewPeriodTotals(o.response.AsOf, o.query.days, zone)
		o.groups[zone] = group
	}
	return group
}

func (o *overviewTotals) result() overviewResponse {
	out := o.response
	for _, unit := range slices.Sorted(maps.Keys(o.currentUnits)) {
		out.Current.ByUnit = append(out.Current.ByUnit, o.currentUnits[unit])
	}
	for _, zone := range slices.Sorted(maps.Keys(o.groups)) {
		out.Periods = append(out.Periods, o.groups[zone].result())
	}
	return out
}

func overviewSiteBalance(now time.Time, site Site) overviewSite {
	out := overviewSite{
		SiteID: site.ID, Status: "unknown", Balances: []overviewBalance{},
		BalancePricingStatus: pricingUnknown,
	}
	attempt := site.LastBalanceObservation
	if attempt == nil && site.LastObservation != nil && site.LastObservation.Source == "balance" {
		attempt = site.LastObservation
	}
	if attempt != nil {
		out.LastBalanceObservation = &overviewBalanceAttempt{At: attempt.At.UTC(), Outcome: attempt.Outcome, Code: attempt.Code}
	}
	latest := site.Latest
	if latest == nil || latest.WalletEpoch != site.WalletEpoch {
		return out
	}
	priced := 0
	for _, balance := range latest.Balances {
		amount, err := Amount(balance.Available)
		if err != nil {
			continue
		}
		price := site.PriceFor(balance.Unit)
		row := overviewBalance{Unit: balance.Unit, Available: amount.String(), Source: price.Source}
		if multiplier, err := Amount(price.Multiplier); err == nil && multiplier.IsPositive() {
			row.Multiplier = decimalText(multiplier)
			row.BalanceCNY = decimalText(amount.DivRound(multiplier, 10))
			priced++
		}
		out.Balances = append(out.Balances, row)
	}
	if len(out.Balances) != 0 {
		at := latest.At.UTC()
		out.SampledAt = &at
		out.Status = "fresh"
		cadence := latest.IntervalMinutes
		if cadence <= 0 {
			cadence = site.Config.IntervalMinutes
		}
		if now.Sub(at) > 2*time.Duration(cadence)*time.Minute {
			out.Status = "stale"
		}
		out.BalancePricingStatus = overviewPricingStatus(priced, priced < len(out.Balances))
	}
	return out
}

func (current *overviewCurrent) addSite(units map[string]overviewCurrentUnit, site overviewSite) {
	switch site.Status {
	case "unknown":
		current.UnknownSites++
		return
	case "stale":
		current.StaleSites++
		return
	}
	current.FreshSites++
	if site.BalancePricingStatus != pricingUnknown {
		current.PricedSites++
	}
	if site.BalancePricingStatus != pricingPriced {
		current.UnpricedSites++
	}
	for _, balance := range site.Balances {
		unit, found := units[balance.Unit]
		if !found {
			unit = overviewCurrentUnit{Unit: balance.Unit, Amount: "0"}
		}
		unit.Amount = decimal.RequireFromString(unit.Amount).Add(decimal.RequireFromString(balance.Available)).String()
		unit.Sites++
		units[balance.Unit] = unit
		addOverviewAmount(&current.EstimatedBalanceCNY, balance.BalanceCNY)
	}
}

type overviewWindow struct {
	timezone string
	from     string
	to       string
}

type overviewPeriodTotals struct {
	window   overviewWindow
	firstDay string
	lastDay  string
	units    map[string]*overviewUnitCost
}

func newOverviewPeriodTotals(now time.Time, days int, timezone string) *overviewPeriodTotals {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	year, month, day := now.In(loc).Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	window := overviewWindow{timezone: timezone, to: today.Format(time.DateOnly)}
	if days != 0 {
		window.from = today.AddDate(0, 0, 1-days).Format(time.DateOnly)
	}
	return &overviewPeriodTotals{window: window, units: make(map[string]*overviewUnitCost)}
}

func (w overviewWindow) includes(site Site, day DailyStat) bool {
	return day.WalletEpoch == site.WalletEpoch && day.StatisticsEpoch == site.StatisticsEpoch &&
		overviewTimezone(day.Timezone) == w.timezone && day.Date != "" && day.Date >= w.from && day.Date <= w.to
}

func (p *overviewPeriodTotals) add(day DailyStat) {
	if p.firstDay == "" || day.Date < p.firstDay {
		p.firstDay = day.Date
	}
	if day.Date > p.lastDay {
		p.lastDay = day.Date
	}
	unit := p.units[day.Unit]
	if unit == nil {
		unit = &overviewUnitCost{Unit: day.Unit}
		p.units[day.Unit] = unit
	}
	addOverviewAmount(&unit.UpstreamCost, day.UpstreamCost)
	addOverviewAmount(&unit.EstimatedCostCNY, day.EstimatedCostCNY)
	addOverviewAmount(&unit.GapCostCNY, day.GapCostCNY)
	addOverviewAmount(&unit.UnpricedDrop, day.UnpricedDrop)
	unit.PricedIntervals += day.PricedIntervals
	unit.ComparableIntervals += day.ComparableIntervals
	// v1 日桶记录了可比区间的金额，但没有记录区间数。首样本日没有这些金额。
	unit.HasUnknownCoverage = unit.HasUnknownCoverage ||
		(day.AlgorithmVersion == 1 && day.ComparableIntervals == 0 && day.EstimatedCostCNY == nil &&
			(day.EstimatedSpend != nil || day.NetChange != nil))
	unit.CoveredSeconds += day.CoveredSeconds
	unit.Gap = unit.Gap || day.Gap
	if len(day.UnpricedReasons) != 0 {
		if unit.UnpricedReasons == nil {
			unit.UnpricedReasons = make(map[string]int)
		}
		for reason, count := range day.UnpricedReasons {
			unit.UnpricedReasons[reason] += count
		}
	}
}

func (p *overviewPeriodTotals) result() overviewPeriod {
	out := overviewPeriod{Timezone: p.window.timezone, ByUnit: []overviewUnitCost{}}
	if p.window.from != "" {
		out.From, out.To = &p.window.from, &p.window.to
	} else if p.firstDay != "" {
		out.From, out.To = &p.firstDay, &p.lastDay
	}
	priced, comparable := 0, 0
	for _, key := range slices.Sorted(maps.Keys(p.units)) {
		unit := p.units[key]
		unit.PricingStatus = overviewPricingStatus(unit.PricedIntervals, unit.PricedIntervals < unit.ComparableIntervals || unit.HasUnknownCoverage)
		out.ByUnit = append(out.ByUnit, *unit)
		priced += unit.PricedIntervals
		comparable += unit.ComparableIntervals
		out.HasUnknownCoverage = out.HasUnknownCoverage || unit.HasUnknownCoverage
		addOverviewAmount(&out.EstimatedCostCNY, unit.EstimatedCostCNY)
		addOverviewAmount(&out.GapCostCNY, unit.GapCostCNY)
	}
	out.PricingStatus = overviewPricingStatus(priced, priced < comparable || out.HasUnknownCoverage)
	return out
}

func overviewPricingStatus(priced int, partial bool) string {
	switch {
	case priced == 0:
		return pricingUnknown
	case partial:
		return pricingPartial
	default:
		return pricingPriced
	}
}

func addOverviewAmount(total **string, amount *string) {
	if amount != nil {
		addDecimal(total, decimal.RequireFromString(*amount))
	}
}
