package strategy

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"futures-arbitrage-scanner/internal/fees"
)

// --- series selection by the series' OWN cost-crossing (2026-09-10) ---
//
// The three-year study (docs/reports/regime-3y-2026-09-09.html, regularity 2)
// found that the level at which a series pays for its round trip is that
// series' own priced cost divided by the settlements a hold crosses — about
// 0.33 bps/8h at BTC/ETH's 0.30% and 0.8 at NEAR's 0.74% — not one number for
// every series. TrailingMeanMinCostFrac makes that the selection floor.

// crossingBpsFor is the test's OWN arithmetic for the crossing: the priced
// round trip, spread over the settlements a HoldingDays hold crosses at the
// candidate's cadence, expressed per 8h. Written independently of the
// package so the test is a second opinion on the number, not a restatement.
func crossingBpsFor(t *testing.T, c Candidate, p Params, frac float64) float64 {
	t.Helper()
	cost := RoundTripCost(RoundTripInput{NotionalQuote: p.NotionalQuote, SpotFee: c.SpotFee, PerpFee: c.PerpFee,
		SpotBook: c.SpotBook, PerpBook: c.PerpBook, At: evalAt, MaxBookAge: p.MaxBookAge})
	if !cost.OK {
		t.Fatalf("round trip: %s", cost.ReasonVI)
	}
	interval := c.Settled[len(c.Settled)-1].IntervalSec
	settlements := math.Floor(p.HoldingDays * 86400 / float64(interval))
	perInterval := frac * cost.TotalPct / 100 / settlements
	return perInterval * 28800 / float64(interval) * 10000
}

var crossingRe = regexp.MustCompile(`điểm cắt chi phí của chuỗi này ([0-9.]+) bps/8h`)

// crossingIn reads the crossing the check reported, so a test compares
// numbers rather than the formatting of numbers.
func crossingIn(t *testing.T, detail string) float64 {
	t.Helper()
	m := crossingRe.FindStringSubmatch(detail)
	if m == nil {
		t.Fatalf("the detail must name the series' crossing: %q", detail)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func flat(n int, bps float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = bps
	}
	return out
}

// selectionParams lowers the per-print entry floors so the trailing-mean
// condition is the one under test, and sizes the horizon to the ten 8h
// settlements the candidates below carry.
func selectionParams() Params {
	p := entryParams()
	p.MinRatePer8hBps, p.PersistencePeriods, p.MinNetAPRFrac = 0.1, 2, 0.0001
	p.TrailingMeanDays = 10 * 8.0 / 24
	return p
}

// Off is the rule exactly as it stood before the field existed: the zero
// value enters the good candidate, the check passes and says it is off,
// and the decision carries the same conditions in the same order as the
// rule before this field (eight checks, hedge first, margin last).
func TestEvaluateEntry_CostCrossingOffChangesNothing(t *testing.T) {
	p := entryParams()
	p.TrailingMeanMinCostFrac = 0
	d := EvaluateEntry(evalAt, goodCandidate(), p)
	if d.Action != ActionEnter {
		t.Fatalf("the zero value must not stop the good candidate:\n%s", strings.Join(d.LogLines(), "\n"))
	}
	tm := findCheck(t, d, "trailing_mean")
	if !tm.Passed || tm.NotEvaluated || !strings.Contains(tm.DetailVI, "Không xét") {
		t.Errorf("off must pass and say so: %+v", tm)
	}
	names := make([]string, 0, len(d.Checks))
	for _, c := range d.Checks {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, ","); got != "hedge_leg,history_depth,rate_threshold,persistence,trailing_mean,liquidity,net_apr,margin_known" {
		t.Errorf("the entry conditions changed: %s", got)
	}
	// A fraction with no horizon is off too — config refuses the block, but
	// the package must not average an empty window if handed one.
	p.TrailingMeanMinCostFrac, p.TrailingMeanDays = 1.0, 0
	d = EvaluateEntry(evalAt, goodCandidate(), p)
	if d.Action != ActionEnter || !findCheck(t, d, "trailing_mean").Passed {
		t.Errorf("a fraction with a zero horizon must be off, got %v", d.Action)
	}
}

// The floor is the series' own round trip over the hold, per 8h — the
// number crossingBpsFor computes on its own — and the check names it. Ten
// settlements 1% above it enter; 1% below do not; two round trips (2.0)
// double it.
func TestEvaluateEntry_CostCrossingIsTheSeriesOwnCostOverTheHold(t *testing.T) {
	p := selectionParams()
	p.TrailingMeanMinCostFrac = 1.0
	c := goodCandidate()
	want := crossingBpsFor(t, c, p, 1.0)
	if want < 0.2 || want > 0.6 {
		t.Fatalf("sanity: a 0.30%% trip over 90 settlements is about 0.33 bps/8h, got %.4f", want)
	}

	c.Settled = settled("binance_futures", 28800, flat(10, want*1.01)...)
	d := EvaluateEntry(evalAt, c, p)
	tm := findCheck(t, d, "trailing_mean")
	if !tm.Passed {
		t.Fatalf("a mean 1%% above the series' crossing must clear it:\n%s", strings.Join(d.LogLines(), "\n"))
	}
	if got := crossingIn(t, tm.DetailVI); math.Abs(got-want) > 1e-4 {
		t.Errorf("reported crossing %.4f, want %.4f bps/8h", got, want)
	}
	if d.Action != ActionEnter {
		t.Errorf("above the crossing every other condition holds here, want enter:\n%s", strings.Join(d.LogLines(), "\n"))
	}

	c.Settled = settled("binance_futures", 28800, flat(10, want*0.99)...)
	d = EvaluateEntry(evalAt, c, p)
	if tm := findCheck(t, d, "trailing_mean"); tm.Passed {
		t.Errorf("a mean 1%% under the crossing must not clear it: %+v", tm)
	}
	if d.Action != ActionSkip {
		t.Errorf("a failed selection must skip, got %v", d.Action)
	}

	p.TrailingMeanMinCostFrac = 2.0
	c.Settled = settled("binance_futures", 28800, flat(10, want*1.01)...)
	d = EvaluateEntry(evalAt, c, p)
	tm = findCheck(t, d, "trailing_mean")
	if tm.Passed {
		t.Errorf("one crossing is under two round trips: %+v", tm)
	}
	if got := crossingIn(t, tm.DetailVI); math.Abs(got-2*want) > 1e-4 {
		t.Errorf("at 2.0 the crossing is twice the figure: got %.4f, want %.4f", got, 2*want)
	}
}

// The crossing is a function of the priced round trip, so without one — no
// hedge leg, or a fee schedule that refuses to price — the condition reports
// itself NOT EVALUATED instead of inventing a second cause beside the
// hedge/liquidity check that already names the real one.
func TestEvaluateEntry_CostCrossingIsNotEvaluatedWithoutAPricedRoundTrip(t *testing.T) {
	p := selectionParams()
	p.TrailingMeanMinCostFrac = 1.0

	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, flat(10, 1.0)...)
	c.SpotSource = ""
	tm := findCheck(t, EvaluateEntry(evalAt, c, p), "trailing_mean")
	if !tm.NotEvaluated || tm.Passed {
		t.Errorf("no hedge leg → no cost → not evaluated, got %+v", tm)
	}

	c = goodCandidate()
	c.Settled = settled("binance_futures", 28800, flat(10, 1.0)...)
	c.PerpFee = fees.Schedule{Source: "binance_futures"} // Verified false: the round trip refuses
	tm = findCheck(t, EvaluateEntry(evalAt, c, p), "trailing_mean")
	if !tm.NotEvaluated || tm.Passed {
		t.Errorf("an unpriceable round trip → not evaluated, got %+v", tm)
	}
}

// Both floors may be set; the mean has to clear each of them, and the detail
// carries both so the log says which one was binding.
func TestEvaluateEntry_CostCrossingAndTheAbsoluteFloorBothApply(t *testing.T) {
	p := selectionParams()
	p.TrailingMeanMinCostFrac = 1.0
	c := goodCandidate()
	want := crossingBpsFor(t, c, p, 1.0)
	c.Settled = settled("binance_futures", 28800, flat(10, want*1.5)...) // clears the crossing

	p.MinTrailingMeanBps = want * 2 // the absolute floor is higher and binds
	tm := findCheck(t, EvaluateEntry(evalAt, c, p), "trailing_mean")
	if tm.Passed || !strings.Contains(tm.DetailVI, "ngưỡng chọn chuỗi") {
		t.Errorf("the absolute floor must still bind, and be named: %+v", tm)
	}
	p.MinTrailingMeanBps = want // at or under the mean: both clear
	if tm := findCheck(t, EvaluateEntry(evalAt, c, p), "trailing_mean"); !tm.Passed {
		t.Errorf("both floors cleared must pass: %+v", tm)
	}
	// The other way round: the absolute floor clears and the crossing binds.
	p.MinTrailingMeanBps = want * 0.5
	c.Settled = settled("binance_futures", 28800, flat(10, want*0.8)...)
	tm = findCheck(t, EvaluateEntry(evalAt, c, p), "trailing_mean")
	if tm.Passed || math.Abs(crossingIn(t, tm.DetailVI)-want) > 1e-4 {
		t.Errorf("the crossing must bind when it is the higher floor, and be named: %+v", tm)
	}
}

// Per 8h the crossing does not depend on the venue's cadence: a 0.30% trip
// over 30 days is the same money whether it is 90 settlements at 8h or 720 at
// 1h — which is the whole reason the comparison unit is per 8h (rule 3).
func TestEvaluateEntry_CostCrossingPer8hDoesNotDependOnTheCadence(t *testing.T) {
	p := selectionParams()
	p.TrailingMeanMinCostFrac = 1.0
	p.TrailingMeanDays = 1

	c8 := goodCandidate()
	c8.Settled = settled("binance_futures", 28800, flat(3, 1.0)...) // one day of 8h
	c1 := goodCandidate()
	c1.Settled = settled("binance_futures", 3600, flat(24, 1.0)...) // one day of 1h
	want8, want1 := crossingBpsFor(t, c8, p, 1.0), crossingBpsFor(t, c1, p, 1.0)
	if math.Abs(want8-want1) > 1e-9 {
		t.Fatalf("sanity: the test's own crossing differs by cadence: %.6f vs %.6f", want8, want1)
	}
	c8.Settled = settled("binance_futures", 28800, flat(3, want8*1.01)...)
	c1.Settled = settled("binance_futures", 3600, flat(24, want1*1.01)...)
	tm8 := findCheck(t, EvaluateEntry(evalAt, c8, p), "trailing_mean")
	tm1 := findCheck(t, EvaluateEntry(evalAt, c1, p), "trailing_mean")
	if !tm8.Passed || !tm1.Passed {
		t.Fatalf("both cadences must clear the same crossing:\n8h: %+v\n1h: %+v", tm8, tm1)
	}
	if g8, g1 := crossingIn(t, tm8.DetailVI), crossingIn(t, tm1.DetailVI); math.Abs(g8-g1) > 1e-4 {
		t.Errorf("reported crossing differs by cadence: %.4f (8h) vs %.4f (1h)", g8, g1)
	}
}
