package config

import (
	"fmt"
	"sort"
)

// CrossRadar is the cross-venue funding radar (PLAN 4.5i, direction 1): it
// compares the FORMING funding rate of the same perpetual on two venues and
// logs how long a wide spread lasts. It is read-only and places nothing.
//
// Every number here is a DECISION the operator makes about how to read the
// market, not a venue fact, which is why it lives in this file beside the
// measurements that motivated it (docs/reports/walkthrough-pillar-2-bybit.md
// §5: median spread 3.2% APR, ≥ 15% on 4.8% of coin-days, median run 1 day).
type CrossRadar struct {
	// Enabled false leaves the scanner exactly as it was: no radar, no events.
	Enabled bool `yaml:"enabled"`

	// SourceA and SourceB are the two perpetual sources compared. The spread is
	// A − B, so a positive spread means A pays more and the trade is short A,
	// long B.
	SourceA string `yaml:"source_a"`
	SourceB string `yaml:"source_b"`

	// LeverageXPerLeg is the leverage on EACH leg. Capital is 2N/K because the
	// margin sits on two venues that do not offset each other, so a return on
	// capital is K/2 times the return on notional — never K.
	LeverageXPerLeg float64 `yaml:"leverage_x_per_leg"`

	// PlannedHoldDays spreads the round-trip cost over a holding period to put
	// it in APR terms. It is an assumption, and the radar says so beside every
	// figure: on the three-year corpus a spread ≥ 15% lasted a median of ONE day.
	PlannedHoldDays float64 `yaml:"planned_hold_days"`

	// Status thresholds, in percent a year.
	GoodMinAfterCostAPRCapitalPct float64 `yaml:"good_min_after_cost_apr_capital_pct"`
	// GoodMaxBreakevenDays is how long the CURRENT spread must be able to repay
	// the round trip within for a pair to be called good. The after-cost APR
	// rests on PlannedHoldDays; this gate rests on how long wide spreads have
	// actually lasted (three-year corpus: median 1 day, P90 2–3 days), so a
	// spread that needs a week to pay its costs is not green however high its
	// seven-day APR reads.
	GoodMaxBreakevenDays   float64 `yaml:"good_max_breakeven_days"`
	MaxTouchSpreadBps      float64 `yaml:"max_touch_spread_bps"`
	NormalBelowGrossAPRPct float64 `yaml:"normal_below_gross_apr_pct"`

	// EventThresholdsGrossAPRPct are the gross spreads whose episodes are
	// logged, each on its own. EventEndBelowSec is how long a spread must stay
	// under a threshold before its episode is closed: forming rates wobble
	// around a threshold between two readings, and without it one real episode
	// is logged as dozens of five-second ones. The episode's end is still
	// stamped at the FIRST reading below, so the grace does not lengthen it.
	EventThresholdsGrossAPRPct []float64 `yaml:"event_thresholds_gross_apr_pct"`
	EventEndBelowSec           int64     `yaml:"event_end_below_sec"`

	// SampleEverySec is how often the radar is evaluated for the event log.
	SampleEverySec int64 `yaml:"sample_every_sec"`
}

const (
	defaultCrossRadarLeverageX      = 2
	defaultCrossRadarHoldDays       = 7
	defaultCrossRadarGoodAPRPct     = 20
	defaultCrossRadarBreakevenDays  = 3
	defaultCrossRadarMaxTouchBps    = 5
	defaultCrossRadarNormalAPRPct   = 10
	defaultCrossRadarEndBelowSec    = 60
	defaultCrossRadarSampleEverySec = 5
)

func (r *CrossRadar) applyDefaults() {
	if r.LeverageXPerLeg == 0 {
		r.LeverageXPerLeg = defaultCrossRadarLeverageX
	}
	if r.PlannedHoldDays == 0 {
		r.PlannedHoldDays = defaultCrossRadarHoldDays
	}
	if r.GoodMinAfterCostAPRCapitalPct == 0 {
		r.GoodMinAfterCostAPRCapitalPct = defaultCrossRadarGoodAPRPct
	}
	if r.GoodMaxBreakevenDays == 0 {
		r.GoodMaxBreakevenDays = defaultCrossRadarBreakevenDays
	}
	if r.MaxTouchSpreadBps == 0 {
		r.MaxTouchSpreadBps = defaultCrossRadarMaxTouchBps
	}
	if r.NormalBelowGrossAPRPct == 0 {
		r.NormalBelowGrossAPRPct = defaultCrossRadarNormalAPRPct
	}
	if len(r.EventThresholdsGrossAPRPct) == 0 {
		r.EventThresholdsGrossAPRPct = []float64{15, 25}
	}
	if r.EventEndBelowSec == 0 {
		r.EventEndBelowSec = defaultCrossRadarEndBelowSec
	}
	if r.SampleEverySec == 0 {
		r.SampleEverySec = defaultCrossRadarSampleEverySec
	}
	sort.Float64s(r.EventThresholdsGrossAPRPct)
}

// validate needs the sources: both named sources must exist and be perps.
func (r CrossRadar) validate(sources []Source) error {
	if !r.Enabled {
		return nil
	}
	if r.SourceA == "" || r.SourceB == "" || r.SourceA == r.SourceB {
		return fmt.Errorf("cross_radar needs two different sources, got %q and %q", r.SourceA, r.SourceB)
	}
	quotes := map[string]string{}
	for _, name := range []string{r.SourceA, r.SourceB} {
		found := false
		for _, s := range sources {
			if s.Source != name {
				continue
			}
			found = true
			if s.MarketType != "perp" {
				return fmt.Errorf("cross_radar source %s is %q; the radar compares perpetual funding", name, s.MarketType)
			}
			quotes[name] = s.QuoteAsset
		}
		if !found {
			return fmt.Errorf("cross_radar names source %s, which is not configured", name)
		}
	}
	if quotes[r.SourceA] != quotes[r.SourceB] {
		// A USD perp against a USDT perp is open in USDT/USD, and nothing on the
		// radar would say so (the Quote bridging trap).
		return fmt.Errorf("cross_radar sources quote %s and %s; the radar compares same-quote perps only",
			quotes[r.SourceA], quotes[r.SourceB])
	}
	switch {
	case r.LeverageXPerLeg < 1:
		return fmt.Errorf("cross_radar.leverage_x_per_leg is %v; below 1 is not leverage", r.LeverageXPerLeg)
	case r.PlannedHoldDays <= 0:
		return fmt.Errorf("cross_radar.planned_hold_days must be > 0, got %v", r.PlannedHoldDays)
	case r.MaxTouchSpreadBps <= 0 || r.NormalBelowGrossAPRPct <= 0 || r.GoodMinAfterCostAPRCapitalPct <= 0 || r.GoodMaxBreakevenDays <= 0:
		// Only a NEGATIVE value reaches here: Load applies defaults before
		// validating, and a plain number cannot tell "absent" from "0", so a
		// written 0 means "use the default" (documented in config.yaml).
		return fmt.Errorf("cross_radar status thresholds must not be negative (0 or absent takes the default)")
	case r.EventEndBelowSec < 1:
		return fmt.Errorf("cross_radar.event_end_below_sec is %d; a negative grace is not a grace (0 or absent takes the default)", r.EventEndBelowSec)
	case r.SampleEverySec < 1:
		return fmt.Errorf("cross_radar.sample_every_sec must be at least 1, got %d", r.SampleEverySec)
	}
	for i, t := range r.EventThresholdsGrossAPRPct {
		if t <= 0 || (i > 0 && t == r.EventThresholdsGrossAPRPct[i-1]) {
			return fmt.Errorf("cross_radar.event_thresholds_gross_apr_pct must be positive and distinct, got %v", r.EventThresholdsGrossAPRPct)
		}
	}
	return nil
}
