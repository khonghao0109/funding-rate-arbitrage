package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"futures-arbitrage-scanner/internal/strategy"
)

// The sweep's axes, one comma-separated list per flag. The DEFAULTS are the
// step-3.3 grid exactly — 4 × 3 × 2 = 24 sets in the same order — so a sweep
// run with no grid flags still diffs against the CSV the step-3.3 verdict was
// delivered on (grid_test.go pins that).
const (
	defaultMinRateBps  = "0.3,0.5,0.8,1.2"
	defaultPersist     = "2,3,6"
	defaultMinNetAPR   = "0.02"
	defaultExitNetAPR  = "0.005"
	defaultExitPersist = "1,3"
	defaultNotional    = "50000"
	defaultHoldDays    = "30"
	// Sign-flip exit gates (strategy.Params.ExitNegative*): the defaults are
	// the step-3.2 rule, close on any settled negative print.
	defaultExitNegBps     = "0"
	defaultExitNegPeriods = "1"
	defaultExitNegCum     = "0"
	// Minimum hold (strategy.Params.MinHoldRecoveredCostFrac): 0 is off, and
	// off is the rule as it stood before the axis existed.
	defaultMinHold = "0"
	// Margin on the perp leg: off, exactly as every run before it existed.
	defaultPerpMargin = "0"
	defaultLiqBuffer  = "0"
)

// gridSpec is every axis of a sweep, named as strategy.Params names them.
type gridSpec struct {
	MinRatePer8hBps        []float64
	PersistencePeriods     []int
	MinNetAPRFrac          []float64
	ExitNetAPRFrac         []float64
	ExitPersistencePeriods []int
	NotionalQuote          []float64
	HoldingDays            []float64

	ExitNegativeMinBps      []float64
	ExitNegativePeriods     []int
	ExitNegativeCumCostFrac []float64

	MinHoldRecoveredCostFrac []float64

	PerpMarginFrac          []float64
	MinLiquidationBufferPct []float64
}

func parseGridSpec(minRate, persist, minNet, exitNet, exitPersist, notional, hold string) (gridSpec, error) {
	var spec gridSpec
	var err error
	if spec.MinRatePer8hBps, err = parseFloatList(minRate); err != nil {
		return spec, fmt.Errorf("-min-rate-bps: %w", err)
	}
	if spec.PersistencePeriods, err = parseIntList(persist); err != nil {
		return spec, fmt.Errorf("-persist: %w", err)
	}
	if spec.MinNetAPRFrac, err = parseFloatList(minNet); err != nil {
		return spec, fmt.Errorf("-min-net-apr: %w", err)
	}
	if spec.ExitNetAPRFrac, err = parseFloatList(exitNet); err != nil {
		return spec, fmt.Errorf("-exit-net-apr: %w", err)
	}
	if spec.ExitPersistencePeriods, err = parseIntList(exitPersist); err != nil {
		return spec, fmt.Errorf("-exit-persist: %w", err)
	}
	if spec.NotionalQuote, err = parseFloatList(notional); err != nil {
		return spec, fmt.Errorf("-notional: %w", err)
	}
	if spec.HoldingDays, err = parseFloatList(hold); err != nil {
		return spec, fmt.Errorf("-hold-days: %w", err)
	}
	// Gates default to the 3.2 rule until withNegativeGates says otherwise.
	spec.ExitNegativeMinBps, spec.ExitNegativePeriods, spec.ExitNegativeCumCostFrac = []float64{0}, []int{1}, []float64{0}
	// The minimum-hold floor defaults to off for the same reason.
	spec.MinHoldRecoveredCostFrac = []float64{0}
	spec.PerpMarginFrac, spec.MinLiquidationBufferPct = []float64{0}, []float64{0}
	return spec, nil
}

// withMargin sets the two perp-margin axes. They move together because a
// margin fraction with no buffer only leaves once the venue has ALREADY
// liquidated, which is a report and not a rule (config.Strategy.validate says
// the same thing about the live block).
func (g gridSpec) withMargin(marginFrac, bufferPct string) (gridSpec, error) {
	var err error
	if g.PerpMarginFrac, err = parseFloatList(marginFrac); err != nil {
		return g, fmt.Errorf("-perp-margin: %w", err)
	}
	if g.MinLiquidationBufferPct, err = parseFloatList(bufferPct); err != nil {
		return g, fmt.Errorf("-liq-buffer: %w", err)
	}
	return g, nil
}

// withMinHold sets the minimum-hold axis from its flag.
func (g gridSpec) withMinHold(minHold string) (gridSpec, error) {
	var err error
	if g.MinHoldRecoveredCostFrac, err = parseFloatList(minHold); err != nil {
		return g, fmt.Errorf("-min-hold: %w", err)
	}
	return g, nil
}

// withNegativeGates sets the three sign-flip axes from their flags.
func (g gridSpec) withNegativeGates(minBps, periods, cum string) (gridSpec, error) {
	var err error
	if g.ExitNegativeMinBps, err = parseFloatList(minBps); err != nil {
		return g, fmt.Errorf("-exit-neg-bps: %w", err)
	}
	if g.ExitNegativePeriods, err = parseIntList(periods); err != nil {
		return g, fmt.Errorf("-exit-neg-periods: %w", err)
	}
	if g.ExitNegativeCumCostFrac, err = parseFloatList(cum); err != nil {
		return g, fmt.Errorf("-exit-neg-cum: %w", err)
	}
	return g, nil
}

// params expands the axes into every combination, nested so the step-3.3
// order survives while the extra axes hold one value each: notional, hold,
// entry floor, exit floor outermost, then rate threshold, persistence, exit
// persistence exactly as before.
//
// A combination whose exit floor is at or above its entry floor is one
// config.yaml refuses to load (config.Strategy validation), so the live path
// could never run it. It is dropped and COUNTED — the count is returned and
// printed — never run quietly and never dropped quietly.
func (g gridSpec) params() ([]strategy.Params, int, error) {
	if err := g.check(); err != nil {
		return nil, 0, err
	}
	var grid []strategy.Params
	dropped := 0
	for _, notional := range g.NotionalQuote {
		for _, hold := range g.HoldingDays {
			for _, minNet := range g.MinNetAPRFrac {
				for _, exitNet := range g.ExitNetAPRFrac {
					if exitNet >= minNet {
						dropped += len(g.MinRatePer8hBps) * len(g.PersistencePeriods) * len(g.ExitPersistencePeriods) *
							len(g.ExitNegativeMinBps) * len(g.ExitNegativePeriods) * len(g.ExitNegativeCumCostFrac) *
							len(g.MinHoldRecoveredCostFrac) * len(g.PerpMarginFrac) * len(g.MinLiquidationBufferPct)
						continue
					}
					for _, minBps := range g.MinRatePer8hBps {
						for _, periods := range g.PersistencePeriods {
							for _, exitPeriods := range g.ExitPersistencePeriods {
								for _, negBps := range g.ExitNegativeMinBps {
									for _, negPeriods := range g.ExitNegativePeriods {
										for _, negCum := range g.ExitNegativeCumCostFrac {
											for _, minHold := range g.MinHoldRecoveredCostFrac {
												for _, marginFrac := range g.PerpMarginFrac {
													for _, buffer := range g.MinLiquidationBufferPct {
														p := baseParams(notional, hold)
														p.MinRatePer8hBps = minBps
														p.PersistencePeriods = periods
														p.MinNetAPRFrac = minNet
														p.ExitNetAPRFrac = exitNet
														p.ExitPersistencePeriods = exitPeriods
														p.ExitNegativeMinBps = negBps
														p.ExitNegativePeriods = negPeriods
														p.ExitNegativeCumCostFrac = negCum
														p.MinHoldRecoveredCostFrac = minHold
														p.PerpMarginFrac = marginFrac
														p.MinLiquidationBufferPct = buffer
														grid = append(grid, p)
													}
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if len(grid) == 0 {
		return nil, dropped, fmt.Errorf("every combination has its exit floor at or above its entry floor (%d dropped) — nothing to sweep", dropped)
	}
	return grid, dropped, nil
}

// check refuses values the strategy could not mean anything by. A negative
// rate threshold would enter on funding that pays the other side; a zero
// persistence never observes a settlement; a zero notional prices no fill.
func (g gridSpec) check() error {
	for _, v := range g.MinRatePer8hBps {
		if v < 0 {
			return fmt.Errorf("-min-rate-bps %g: a negative threshold enters on funding that pays the other side", v)
		}
	}
	for _, v := range g.PersistencePeriods {
		if v < 1 {
			return fmt.Errorf("-persist %d: persistence needs at least one settled period", v)
		}
	}
	for _, v := range g.MinNetAPRFrac {
		if v < 0 {
			return fmt.Errorf("-min-net-apr %g: a negative entry floor accepts a losing projection", v)
		}
	}
	for _, v := range g.ExitNetAPRFrac {
		if v < 0 {
			return fmt.Errorf("-exit-net-apr %g: a negative exit floor holds through a losing projection", v)
		}
	}
	for _, v := range g.ExitPersistencePeriods {
		if v < 1 {
			return fmt.Errorf("-exit-persist %d: the decay exit needs at least one settled period", v)
		}
	}
	for _, v := range g.NotionalQuote {
		if v <= 0 {
			return fmt.Errorf("-notional %g: a position needs a positive size", v)
		}
	}
	for _, v := range g.HoldingDays {
		if v <= 0 {
			return fmt.Errorf("-hold-days %g: the round trip is amortized over a positive hold", v)
		}
	}
	for _, v := range g.ExitNegativeMinBps {
		if v < 0 {
			return fmt.Errorf("-exit-neg-bps %g: the gate is a depth below zero, so it cannot be negative", v)
		}
	}
	for _, v := range g.ExitNegativePeriods {
		if v < 1 {
			return fmt.Errorf("-exit-neg-periods %d: the sign-flip exit needs at least one negative settlement", v)
		}
	}
	for _, v := range g.ExitNegativeCumCostFrac {
		if v < 0 {
			return fmt.Errorf("-exit-neg-cum %g: a fraction of the round trip cannot be negative", v)
		}
	}
	for _, v := range g.MinHoldRecoveredCostFrac {
		if v < 0 {
			return fmt.Errorf("-min-hold %g: a fraction of the round trip cannot be negative", v)
		}
	}
	for _, v := range g.PerpMarginFrac {
		if v < 0 || v > 1 {
			return fmt.Errorf("-perp-margin %g: collateral as a fraction of notional must be in [0,1]; 0.1 is 10x", v)
		}
	}
	for _, v := range g.MinLiquidationBufferPct {
		if v < 0 {
			return fmt.Errorf("-liq-buffer %g: a distance to the liquidation price cannot be negative", v)
		}
	}
	for _, m := range g.PerpMarginFrac {
		if m <= 0 {
			continue
		}
		for _, b := range g.MinLiquidationBufferPct {
			if b <= 0 {
				return fmt.Errorf("-perp-margin %g with -liq-buffer 0: the position would only leave once "+
					"the venue had ALREADY liquidated it, which is not a rule", m)
			}
		}
	}
	return nil
}

func parseFloatList(s string) ([]float64, error) {
	var out []float64
	for _, piece := range strings.Split(s, ",") {
		piece = strings.TrimSpace(piece)
		if piece == "" {
			return nil, fmt.Errorf("empty element in %q", s)
		}
		v, err := strconv.ParseFloat(piece, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("%q is not a finite number", piece)
		}
		out = append(out, v)
	}
	return out, nil
}

func parseIntList(s string) ([]int, error) {
	var out []int
	for _, piece := range strings.Split(s, ",") {
		piece = strings.TrimSpace(piece)
		if piece == "" {
			return nil, fmt.Errorf("empty element in %q", s)
		}
		v, err := strconv.Atoi(piece)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", piece)
		}
		out = append(out, v)
	}
	return out, nil
}

// sweepOnlyFlagsTouched reports whether a flag only -sweep reads was given a
// non-default value, so a plain run can refuse it instead of ignoring it.
func sweepOnlyFlagsTouched(minRate, persist, minNet, exitNet, exitPersist, negBps, negPeriods, negCum, minHold,
	perpMargin, liqBuffer string, top int) bool {
	return minRate != defaultMinRateBps || persist != defaultPersist || minNet != defaultMinNetAPR ||
		exitNet != defaultExitNetAPR || exitPersist != defaultExitPersist ||
		negBps != defaultExitNegBps || negPeriods != defaultExitNegPeriods || negCum != defaultExitNegCum ||
		minHold != defaultMinHold || perpMargin != defaultPerpMargin || liqBuffer != defaultLiqBuffer || top != 0
}
