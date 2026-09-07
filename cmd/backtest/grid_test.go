package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"futures-arbitrage-scanner/internal/config"
)

func TestParseFloatList(t *testing.T) {
	got, err := parseFloatList("0.3, 0.5,0.8")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 3 || got[0] != 0.3 || got[1] != 0.5 || got[2] != 0.8 {
		t.Errorf("got %v", got)
	}
	for _, bad := range []string{"", " ", "0.5,,1", "abc", "NaN", "Inf"} {
		if _, err := parseFloatList(bad); err == nil {
			t.Errorf("parseFloatList(%q) accepted", bad)
		}
	}
}

func TestParseIntList(t *testing.T) {
	got, err := parseIntList("2,3, 6")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 3 || got[0] != 2 || got[1] != 3 || got[2] != 6 {
		t.Errorf("got %v", got)
	}
	for _, bad := range []string{"", "2.5", "x", "1,"} {
		if _, err := parseIntList(bad); err == nil {
			t.Errorf("parseIntList(%q) accepted", bad)
		}
	}
}

// The flag defaults ARE the step-3.3 grid: the same 24 sets in the same order,
// so a sweep run today still diffs against the CSV the step-3.3 verdict was
// delivered on.
func TestDefaultGrid_ReproducesTheStep33Grid(t *testing.T) {
	spec, err := parseGridSpec(defaultMinRateBps, defaultPersist, defaultMinNetAPR,
		defaultExitNetAPR, defaultExitPersist, defaultNotional, defaultHoldDays)
	if err != nil {
		t.Fatalf("defaults must parse: %v", err)
	}
	grid, dropped, err := spec.params()
	if err != nil {
		t.Fatalf("defaults must build: %v", err)
	}
	if dropped != 0 {
		t.Errorf("defaults dropped %d combinations", dropped)
	}
	if len(grid) != 24 {
		t.Fatalf("want 24 parameter sets, got %d", len(grid))
	}
	minBps := []float64{0.3, 0.5, 0.8, 1.2}
	periods := []int{2, 3, 6}
	exitPeriods := []int{1, 3}
	for i, p := range grid {
		wantBps, wantPeriods, wantExit := minBps[i/6], periods[(i/2)%3], exitPeriods[i%2]
		if p.MinRatePer8hBps != wantBps || p.PersistencePeriods != wantPeriods || p.ExitPersistencePeriods != wantExit {
			t.Errorf("grid[%d] = (%.1f, %d, %d), want (%.1f, %d, %d)", i,
				p.MinRatePer8hBps, p.PersistencePeriods, p.ExitPersistencePeriods, wantBps, wantPeriods, wantExit)
		}
		if p.MinNetAPRFrac != 0.02 || p.ExitNetAPRFrac != 0.005 || p.NotionalQuote != 50000 ||
			p.HoldingDays != 30 || p.MaxBasisPct != 1.0 || p.MaxBasisWidenPct != 0.5 {
			t.Errorf("grid[%d] base values changed: %+v", i, p)
		}
	}
}

func TestGrid_CountIsTheProductOfItsAxes(t *testing.T) {
	spec, err := parseGridSpec("0.5,2", "1,3", "0.02", "0.005", "1,6", "20000,200000", "14,60")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	grid, dropped, err := spec.params()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if want := 2 * 2 * 1 * 1 * 2 * 2 * 2; len(grid) != want || dropped != 0 {
		t.Errorf("got %d sets (%d dropped), want %d", len(grid), dropped, want)
	}
}

// config.yaml refuses an exit floor at or above the entry floor; a sweep must
// not quietly run the combinations the live path could never be configured
// with. They are dropped and COUNTED, never silently.
func TestGrid_DropsExitFloorAtOrAboveEntryFloor(t *testing.T) {
	spec, err := parseGridSpec("0.5", "3", "0.02,0.05", "0,0.02,0.05", "1", "50000", "30")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	grid, dropped, err := spec.params()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// entry 0.02: exits 0 ok, 0.02 dropped, 0.05 dropped; entry 0.05: 0 ok, 0.02 ok, 0.05 dropped.
	if len(grid) != 3 || dropped != 3 {
		t.Errorf("got %d sets, %d dropped; want 3 and 3", len(grid), dropped)
	}
	for _, p := range grid {
		if p.ExitNetAPRFrac >= p.MinNetAPRFrac {
			t.Errorf("kept exit %.3f >= entry %.3f", p.ExitNetAPRFrac, p.MinNetAPRFrac)
		}
	}
}

func TestGrid_RefusesValuesTheStrategyCannotUse(t *testing.T) {
	cases := []struct{ name, minRate, persist, minNet, exitNet, exitPersist, notional, hold string }{
		{"persistence 0", "0.5", "0", "0.02", "0.005", "1", "50000", "30"},
		{"exit persistence 0", "0.5", "3", "0.02", "0.005", "0", "50000", "30"},
		{"notional 0", "0.5", "3", "0.02", "0.005", "1", "0", "30"},
		{"negative hold", "0.5", "3", "0.02", "0.005", "1", "50000", "-1"},
		{"negative rate threshold", "-0.5", "3", "0.02", "0.005", "1", "50000", "30"},
		{"negative exit floor", "0.5", "3", "0.02", "-0.01", "1", "50000", "30"},
	}
	for _, c := range cases {
		spec, err := parseGridSpec(c.minRate, c.persist, c.minNet, c.exitNet, c.exitPersist, c.notional, c.hold)
		if err != nil {
			continue // refused at parse — also acceptable
		}
		if _, _, err := spec.params(); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	// Every combination dropped is an error, not an empty sweep.
	spec, err := parseGridSpec("0.5", "3", "0.02", "0.05", "1", "50000", "30")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, _, err := spec.params(); err == nil || !strings.Contains(err.Error(), "exit") {
		t.Errorf("an all-dropped grid must fail naming the exit floor, got %v", err)
	}
}

// The plain (non -sweep) run validates the two axes it uses: a zero or
// negative hold would make NetAPR refuse every settlement and the run would
// report "0 trades" with no reason — the "no opportunity at no cost" reading
// the engine refuses to produce.
func TestPlainRun_ChecksItsAxesAndRefusesSweepOnlyFlags(t *testing.T) {
	spec, err := parseGridSpec(defaultMinRateBps, defaultPersist, defaultMinNetAPR,
		defaultExitNetAPR, defaultExitPersist, "50000", "0")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := spec.check(); err == nil {
		t.Error("-hold-days 0 must be refused by name before the engine sees it")
	}
	if sweepOnlyFlagsTouched(defaultMinRateBps, defaultPersist, defaultMinNetAPR, defaultExitNetAPR, defaultExitPersist, defaultExitNegBps, defaultExitNegPeriods, defaultExitNegCum, defaultMinHold, 0) {
		t.Error("defaults must not count as touched")
	}
	if !sweepOnlyFlagsTouched("1.2", defaultPersist, defaultMinNetAPR, defaultExitNetAPR, defaultExitPersist, defaultExitNegBps, defaultExitNegPeriods, defaultExitNegCum, defaultMinHold, 0) {
		t.Error("-min-rate-bps 1.2 without -sweep must be refused, not silently ignored")
	}
	if !sweepOnlyFlagsTouched(defaultMinRateBps, defaultPersist, defaultMinNetAPR, defaultExitNetAPR, defaultExitPersist, defaultExitNegBps, defaultExitNegPeriods, defaultExitNegCum, defaultMinHold, 40) {
		t.Error("-top without -sweep must be refused")
	}
}

// The sign-flip gates are three more axes; left alone they are the 3.2 rule
// (0 / 1 / 0), so the 24-set grid stays the 24-set grid.
func TestGrid_NegativeGatesDefaultToTheOldRuleAndMultiplyWhenSet(t *testing.T) {
	spec, err := parseGridSpec(defaultMinRateBps, defaultPersist, defaultMinNetAPR,
		defaultExitNetAPR, defaultExitPersist, defaultNotional, defaultHoldDays)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	grid, _, err := spec.params()
	if err != nil || len(grid) != 24 {
		t.Fatalf("defaults: %d sets, %v", len(grid), err)
	}
	for _, p := range grid {
		if p.ExitNegativeMinBps != 0 || p.ExitNegativePeriods != 1 || p.ExitNegativeCumCostFrac != 0 {
			t.Fatalf("default gates changed: %+v", p)
		}
	}
	spec, err = spec.withNegativeGates("0,0.5", "1,3", "0,0.5")
	if err != nil {
		t.Fatalf("gates: %v", err)
	}
	grid, dropped, err := spec.params()
	if err != nil || dropped != 0 || len(grid) != 24*8 {
		t.Errorf("with gates: %d sets, %d dropped, %v — want 192", len(grid), dropped, err)
	}
	if !sweepOnlyFlagsTouched(defaultMinRateBps, defaultPersist, defaultMinNetAPR, defaultExitNetAPR, defaultExitPersist, "0.5", defaultExitNegPeriods, defaultExitNegCum, defaultMinHold, 0) {
		t.Error("-exit-neg-bps without -sweep must be refused, not ignored")
	}
	for _, bad := range [][3]string{{"-0.5", "1", "0"}, {"0", "0", "0"}, {"0", "1", "-1"}} {
		g, err := spec.withNegativeGates(bad[0], bad[1], bad[2])
		if err != nil {
			continue
		}
		if _, _, err := g.params(); err == nil {
			t.Errorf("gates %v accepted", bad)
		}
	}
}

// The sweep's base and the live strategy block are the same numbers by test,
// not by coincidence: config.yaml is what the 3.5 journal runs, baseParams is
// what every sweep axis is varied around, and plainParams is what a plain run
// replays — all three must agree, MaxBookAge aside (the replay zeroes it).
func TestBaseParams_MatchesTheShippedStrategyBlock(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatalf("load config.yaml: %v", err)
	}
	if !cfg.Strategy.Enabled {
		t.Skip("strategy block disabled in the shipped config")
	}
	fromConfig := cfg.Strategy.StrategyParams()
	fromConfig.MaxBookAge = 0
	base := baseParams(fromConfig.NotionalQuote, fromConfig.HoldingDays)
	if !reflect.DeepEqual(base, fromConfig) {
		t.Errorf("baseParams drifted from config.yaml's strategy block:\n base   %+v\n config %+v", base, fromConfig)
	}
	plain := plainParams(cfg, 12345, 7, false, false)
	if !reflect.DeepEqual(plain, fromConfig) {
		t.Errorf("a plain run must replay the config block when flags are left alone:\n plain  %+v\n config %+v", plain, fromConfig)
	}
	plain = plainParams(cfg, 12345, 7, true, true)
	if plain.NotionalQuote != 12345 || plain.HoldingDays != 7 {
		t.Errorf("explicit -notional/-hold-days must apply: %+v", plain)
	}
	if plain.MaxBookAge != 0 {
		t.Error("the replay must not carry a book-age limit: its one book is stale by construction")
	}
}

// The minimum-hold floor is one more axis, and left alone it is off — so the
// default grid stays the step-3.3 grid and every archived sweep still replays.
func TestGrid_MinHoldDefaultsToOffAndMultipliesWhenSet(t *testing.T) {
	spec, err := parseGridSpec(defaultMinRateBps, defaultPersist, defaultMinNetAPR,
		defaultExitNetAPR, defaultExitPersist, defaultNotional, defaultHoldDays)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	grid, _, err := spec.params()
	if err != nil || len(grid) != 24 {
		t.Fatalf("defaults: %d sets, %v", len(grid), err)
	}
	for _, p := range grid {
		if p.MinHoldRecoveredCostFrac != 0 {
			t.Fatalf("the floor must default to off: %+v", p)
		}
	}

	withFloor, err := spec.withMinHold("0,0.5,1")
	if err != nil {
		t.Fatalf("min-hold: %v", err)
	}
	grid, dropped, err := withFloor.params()
	if err != nil || dropped != 0 || len(grid) != 24*3 {
		t.Errorf("with the floor: %d sets, %d dropped, %v — want 72", len(grid), dropped, err)
	}
	if !sweepOnlyFlagsTouched(defaultMinRateBps, defaultPersist, defaultMinNetAPR, defaultExitNetAPR,
		defaultExitPersist, defaultExitNegBps, defaultExitNegPeriods, defaultExitNegCum, "1", 0) {
		t.Error("-min-hold without -sweep must be refused, not ignored")
	}
	if bad, err := spec.withMinHold("-1"); err == nil {
		if _, _, err := bad.params(); err == nil {
			t.Error("a negative floor is not a fraction of anything and must be refused")
		}
	}
}
