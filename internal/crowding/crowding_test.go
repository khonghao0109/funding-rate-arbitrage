package crowding

import (
	"math"
	"testing"
)

// The research package's own tests (test_crowding_reversal_research.py, 161
// lines), ported as table tests where they concern these nine definitions.
// The four that exercise the backtest layer — benchmark_returns,
// trade_episodes, invert_path and the funding bucket — belong to step 6.3
// and are listed at the bottom so their absence here is a decision.

// synthetic_panel: closes as a slow sine random walk, ratios as a bounded
// sine, ETH phase-shifted by 0.2.
func syntheticPanel(n int) Panel {
	p := Panel{Assets: []string{"BTC", "ETH"}, Close: make([][]float64, 2), Ratio: make([][]float64, 2)}
	for a, shift := range []float64{0, 0.2} {
		close, ratio := make([]float64, n), make([]float64, n)
		var cum float64
		for i := 0; i < n; i++ {
			cum += 0.001 * math.Sin(float64(i)/11+shift)
			close[i] = 100 * math.Exp(cum)
			ratio[i] = math.Exp(0.2 * math.Sin(float64(i)/17+shift))
		}
		p.Close[a], p.Ratio[a] = close, ratio
	}
	return p
}

func clonePanel(p Panel) Panel {
	out := Panel{Assets: p.Assets, Close: make([][]float64, 2), Ratio: make([][]float64, 2)}
	for a := 0; a < 2; a++ {
		out.Close[a] = append([]float64(nil), p.Close[a]...)
		out.Ratio[a] = append([]float64(nil), p.Ratio[a]...)
	}
	return out
}

func equalSeries(t *testing.T, name string, a, b []float64) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s: lengths %d vs %d", name, len(a), len(b))
	}
	for i := range a {
		if math.IsNaN(a[i]) && math.IsNaN(b[i]) {
			continue
		}
		if a[i] != b[i] {
			t.Fatalf("%s: bar %d %v vs %v", name, i, a[i], b[i])
		}
	}
}

// test_scores_and_signal_are_prefix_causal
func TestScoresAndSignal_ArePrefixCausal(t *testing.T) {
	panel := syntheticPanel(500)
	const cut = 400
	first := CrowdingScores(panel.Prefix(cut), 90)
	changed := clonePanel(panel)
	for a := 0; a < 2; a++ {
		for i := cut; i < 500; i++ {
			changed.Ratio[a][i] *= 9
		}
	}
	second := CrowdingScores(changed, 90)
	for a := 0; a < 2; a++ {
		equalSeries(t, "score "+panel.Assets[a], first[a], second[a][:cut])
	}
	s1, err := HysteresisSignal(first, 1.0, 0.25, SideBoth)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := HysteresisSignal(second, 1.0, 0.25, SideBoth)
	if err != nil {
		t.Fatal(err)
	}
	for a := 0; a < 2; a++ {
		equalSeries(t, "signal "+panel.Assets[a], s1[a], s2[a][:cut])
	}
}

// test_weights_are_prefix_causal_and_bounded
func TestCausalWeights_ArePrefixCausalAndBounded(t *testing.T) {
	panel := syntheticPanel(500)
	signal, err := HysteresisSignal(CrowdingScores(panel, 90), 1.0, 0.25, SideBoth)
	if err != nil {
		t.Fatal(err)
	}
	prefixClose := [][]float64{panel.Close[0][:400], panel.Close[1][:400]}
	prefixSignal := [][]float64{signal[0][:400], signal[1][:400]}
	first, err := CausalWeights(prefixClose, prefixSignal, 0.24, 1.0, 90)
	if err != nil {
		t.Fatal(err)
	}
	changed := clonePanel(panel)
	for a := 0; a < 2; a++ {
		for i := 400; i < 500; i++ {
			changed.Close[a][i] *= 3
		}
	}
	second, err := CausalWeights(changed.Close, signal, 0.24, 1.0, 90)
	if err != nil {
		t.Fatal(err)
	}
	nonzero := 0
	for a := 0; a < 2; a++ {
		equalSeries(t, "weights "+panel.Assets[a], first[a], second[a][:400])
	}
	for i := 0; i < 400; i++ {
		gross := math.Abs(first[0][i]) + math.Abs(first[1][i])
		if gross > 1.0+1e-12 {
			t.Fatalf("bar %d gross %v exceeds 1", i, gross)
		}
		if gross > 0 {
			nonzero++
		}
	}
	if nonzero == 0 {
		t.Fatal("the synthetic panel produced no exposure at all — the assertion above tested nothing")
	}
}

// test_missing_ratio_fails_closed_without_future_fill, strengthened: the
// state held BEFORE the gap must not survive it.
func TestHysteresisSignal_MissingScoreResetsTheState(t *testing.T) {
	cases := []struct {
		name  string
		score []float64
		want  []float64
	}{
		{"enter short, NaN resets, below entry stays flat", []float64{0, 1.5, 0.8, math.NaN(), 0.8, 0.9}, []float64{0, -1, -1, 0, 0, 0}},
		{"enter long, +Inf resets", []float64{0, -1.2, -0.5, math.Inf(1), -0.5}, []float64{0, 1, 1, 0, 0}},
		{"exactly at entry enters, exactly at exit leaves", []float64{1.0, 0.26, 0.25, 0.5}, []float64{-1, -1, 0, 0}},
		{"side long ignores crowded longs", []float64{2, -2, 0}, []float64{0, 1, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			side := SideBoth
			if tc.name == "side long ignores crowded longs" {
				side = SideLong
			}
			got, err := HysteresisSignal([][]float64{tc.score}, 1.0, 0.25, side)
			if err != nil {
				t.Fatal(err)
			}
			equalSeries(t, tc.name, got[0], tc.want)
		})
	}
	// The research test as written: a NaN score at bar 200 → signal 0 there.
	panel := syntheticPanel(250)
	score := CrowdingScores(panel, 90)
	score[0][200] = math.NaN()
	signal, err := HysteresisSignal(score, 1.0, 0.25, SideBoth)
	if err != nil {
		t.Fatal(err)
	}
	if signal[0][200] != 0 {
		t.Fatalf("signal at the missing bar is %v, want 0", signal[0][200])
	}
}

// test_invalid_hysteresis_rejected
func TestHysteresisSignal_RejectsInvalidThresholdsAndSides(t *testing.T) {
	score := [][]float64{{0, 2}, {0, -2}}
	if _, err := HysteresisSignal(score, 1.0, 1.0, SideBoth); err == nil {
		t.Fatal("exit_z == entry_z accepted")
	}
	if _, err := HysteresisSignal(score, 1.0, -0.1, SideBoth); err == nil {
		t.Fatal("negative exit_z accepted")
	}
	if _, err := HysteresisSignal(score, 1.0, 0.25, Side("sideways")); err == nil {
		t.Fatal("unknown side accepted")
	}
}

// test_ensemble_is_prefix_causal_and_bounded
func TestEnsembleTargets_IsPrefixCausalAndBounded(t *testing.T) {
	panel := syntheticPanel(500)
	cfg := DefaultConfig()
	members := []Member{{90, 1.0, 0.25}, {90, 1.25, 0.50}}
	first, err := EnsembleTargets(panel.Prefix(400), cfg, members)
	if err != nil {
		t.Fatal(err)
	}
	changed := clonePanel(panel)
	for a := 0; a < 2; a++ {
		for i := 400; i < 500; i++ {
			changed.Close[a][i] *= 2
			changed.Ratio[a][i] *= 5
		}
	}
	second, err := EnsembleTargets(changed, cfg, members)
	if err != nil {
		t.Fatal(err)
	}
	for a := 0; a < 2; a++ {
		equalSeries(t, "ensemble target "+panel.Assets[a], first.TargetFracOfEquity[a], second.TargetFracOfEquity[a][:400])
		equalSeries(t, "ensemble signal "+panel.Assets[a], first.MeanMemberSignal[a], second.MeanMemberSignal[a][:400])
	}
	for i := 0; i < 400; i++ {
		if g := math.Abs(first.TargetFracOfEquity[0][i]) + math.Abs(first.TargetFracOfEquity[1][i]); g > 1.0+1e-12 {
			t.Fatalf("bar %d gross %v exceeds 1", i, g)
		}
	}
}

// test_trend_confirmation_is_causal_and_fails_closed_during_warmup: two
// votes are required, so 30d alone is insufficient and 30d + 60d can act —
// nothing before 60 × 6 bars may be non-zero, and the last bar passes.
func TestApplyTrendConfirmation_FailsClosedDuringWarmup(t *testing.T) {
	const n = 1000
	close := [][]float64{make([]float64, n), make([]float64, n)}
	target := [][]float64{make([]float64, n), make([]float64, n)}
	for i := 0; i < n; i++ {
		close[0][i] = float64(i + 1) // BTC rising: agrees with a long
		close[1][i] = float64(n - i) // ETH falling: agrees with a short
		target[0][i], target[1][i] = 0.5, -0.5
	}
	confirmed, err := ApplyTrendConfirmation(close, target, []int{30, 60, 120}, 2)
	if err != nil {
		t.Fatal(err)
	}
	for a := 0; a < 2; a++ {
		for i := 0; i < 60*BarsPerDay; i++ {
			if confirmed[a][i] != 0 {
				t.Fatalf("asset %d bar %d confirmed %v before two horizons had history", a, i, confirmed[a][i])
			}
		}
		if confirmed[a][60*BarsPerDay] != target[a][60*BarsPerDay] {
			t.Fatalf("asset %d: the first bar with 30d+60d history must pass, got %v", a, confirmed[a][60*BarsPerDay])
		}
		if confirmed[a][n-1] != target[a][n-1] {
			t.Fatalf("asset %d last bar %v, want %v", a, confirmed[a][n-1], target[a][n-1])
		}
	}
	// The availability mask, isolated: a falling close with a SHORT target
	// is aligned, but before 30 × 6 bars the 30d horizon has no start and
	// must not vote — "no history" is not a bearish vote.
	votes := TrendConfirmationVotes(close, target, []int{30})
	if votes[1][30*BarsPerDay-1] != 0 || votes[1][30*BarsPerDay] != 1 {
		t.Fatalf("ETH votes around the 30d boundary: %d then %d, want 0 then 1", votes[1][30*BarsPerDay-1], votes[1][30*BarsPerDay])
	}
	if _, err := ApplyTrendConfirmation(close, target, []int{30, 60}, 3); err == nil {
		t.Fatal("required votes above the horizon count accepted")
	}
	if _, err := ApplyTrendConfirmation(close, target, nil, 1); err == nil {
		t.Fatal("no horizons accepted")
	}
}

// min_periods counts FINITE values: lookback 90 → 72 needed; a window with
// 71 finite values is NaN and one with 72 is a number.
func TestRollingZScore_MinPeriodsCountsFiniteValues(t *testing.T) {
	const lookback = 90
	minimum := zscoreMinPeriods(lookback)
	if minimum != 72 {
		t.Fatalf("min_periods for 90 is %d, want int(0.8·90) = 72", minimum)
	}
	if zscoreMinPeriods(20) != 20 || zscoreMinPeriods(24) != 20 || zscoreMinPeriods(25) != 20 || zscoreMinPeriods(26) != 20 || zscoreMinPeriods(30) != 24 {
		t.Fatal("the floor of 20 must apply below lookback 25")
	}
	ratio := make([]float64, lookback)
	for i := range ratio {
		ratio[i] = 1 + 0.01*float64(i%7)
	}
	// Poison 19 bars: 71 finite values remain in the full window.
	for i := 0; i < 19; i++ {
		ratio[i] = math.NaN()
	}
	z := RollingZScore(ratio, lookback)
	if !math.IsNaN(z[lookback-1]) {
		t.Fatalf("71 finite values in a window needing 72 gave %v, want NaN", z[lookback-1])
	}
	ratio[18] = 1.05 // 72 finite values now
	z = RollingZScore(ratio, lookback)
	if math.IsNaN(z[lookback-1]) {
		t.Fatal("72 finite values in a window needing 72 gave NaN")
	}
	// A non-positive ratio is NaN, not a log of a negative number, and
	// the bar's own NaN makes its score NaN even with a valid window.
	ratio[lookback-1] = 0
	z = RollingZScore(ratio, lookback)
	if !math.IsNaN(z[lookback-1]) {
		t.Fatalf("ratio 0 gave score %v, want NaN", z[lookback-1])
	}
	// Sample std (ddof = 1), checked on a hand-computable window.
	small := []float64{math.E, math.E * math.E, math.E * math.E * math.E} // ln → 1, 2, 3
	got := RollingZScore(small, 3)
	// min_periods floors at 20, so a 3-bar window can never be valid.
	for _, v := range got {
		if !math.IsNaN(v) {
			t.Fatalf("a window shorter than min_periods produced %v", v)
		}
	}
	mean, std, ok := rollingMeanStd([]float64{1, 2, 3}, 2, 3, 2)
	if !ok || mean != 2 || std != 1 {
		t.Fatalf("mean %v std %v ok %v, want 2, 1 (sample std of 1,2,3), true", mean, std, ok)
	}
}

// Rolling covariance is pandas': pairwise mean of the product minus the
// product of the marginal means, scaled n/(n−1).
func TestRollingCov_MatchesTheSampleDefinition(t *testing.T) {
	x := []float64{1, 2, 3, 4}
	y := []float64{2, 4, 6, 9}
	got, ok := rollingCov(x, y, 3, 4, 2)
	if !ok {
		t.Fatal("not ok")
	}
	// Sample covariance of (1,2,3,4) and (2,4,6,9): mean x 2.5, mean y 5.25,
	// Σ(x−x̄)(y−ȳ) = (−1.5)(−3.25)+(−0.5)(−1.25)+(0.5)(0.75)+(1.5)(3.75) = 11.5; /3 = 3.8333…
	if math.Abs(got-11.5/3) > 1e-12 {
		t.Fatalf("cov %v, want %v", got, 11.5/3)
	}
	// Variance through the same path equals the sample variance.
	v, _ := rollingVar(x, 3, 4, 2)
	if math.Abs(v-5.0/3) > 1e-12 {
		t.Fatalf("var %v, want %v", v, 5.0/3)
	}
	if _, ok := rollingCov(x, []float64{math.NaN(), 4, 6, 9}, 3, 4, 4); ok {
		t.Fatal("a window with a NaN pair below min_periods must be invalid")
	}
	// Asymmetric NaNs: the marginal means are over each series' OWN values
	// and the product mean over the pairs — a pairwise-only rewrite gives a
	// different number. x = {1, NaN, 3, 4}, y = {2, 4, 6, 9}: mean x 8/3
	// (3 values), mean y 21/4 (4 values), mean xy 56/3 (3 pairs), n = 3 →
	// (56/3 − (8/3)(21/4)) × 3/2 = 7.
	got, ok = rollingCov([]float64{1, math.NaN(), 3, 4}, y, 3, 4, 2)
	if !ok || math.Abs(got-7) > 1e-12 {
		t.Fatalf("asymmetric-NaN cov %v ok=%v, want pandas' marginal/pairwise 7", got, ok)
	}
}

// A window of identical ratios has variance exactly 0 and z = NaN, as in
// pandas, and the hysteresis state RESETS on it: a stuck feed goes flat.
func TestRollingZScore_IdenticalWindowIsNaNAndResetsTheState(t *testing.T) {
	const lookback = 90
	ratio := make([]float64, 200)
	for i := range ratio {
		ratio[i] = math.Exp(0.2 * math.Sin(float64(i)/5)) // varying
	}
	for i := 110; i < 200; i++ {
		ratio[i] = 1.05 // stuck for a full window from bar 110
	}
	z := RollingZScore(ratio, lookback)
	if !math.IsNaN(z[199]) {
		t.Fatalf("90 identical ratios gave z = %v, want NaN (0/0)", z[199])
	}
	if math.IsNaN(z[150]) {
		t.Fatal("a window still holding varying values must have a score")
	}
	signal, err := HysteresisSignal([][]float64{z}, 1.0, 0.25, SideBoth)
	if err != nil {
		t.Fatal(err)
	}
	if signal[0][199] != 0 {
		t.Fatalf("state %v at the stuck bar, want 0 (reset)", signal[0][199])
	}
	// The same rule inside rollingMeanVar, on a hand-made window.
	mean, variance, ok := rollingMeanVar([]float64{1.05, 1.05, 1.05}, 2, 3, 2)
	if !ok || mean != 1.05 || variance != 0 {
		t.Fatalf("identical window mean %v var %v ok %v, want 1.05, exactly 0, true", mean, variance, ok)
	}
}

func TestEnsembleTargets_RefusesNoMembersAndBadConfig(t *testing.T) {
	panel := syntheticPanel(300)
	if _, err := EnsembleTargets(panel, DefaultConfig(), nil); err == nil {
		t.Fatal("zero members accepted — Python raises ZeroDivisionError here")
	}
	bad := DefaultConfig()
	bad.TargetVolAnnualFrac = -0.24
	if _, err := EnsembleTargets(panel, bad, EnsembleMembers); err == nil {
		t.Fatal("a negative volatility target (which flips every sign) was accepted")
	}
	bad = DefaultConfig()
	bad.TrendRequiredVotes = 4
	if _, err := EnsembleTargets(panel, bad, EnsembleMembers); err == nil {
		t.Fatal("4 required votes over 3 horizons accepted")
	}
	if panel.Prefix(10_000).Bars() != 300 || panel.Prefix(-1).Bars() != 0 {
		t.Fatal("Prefix must clamp to the panel")
	}
}

// The four research tests NOT ported here, by name, and where they go:
//   - test_benchmark_is_true_buy_and_hold_not_periodically_rebalanced → 6.3 (benchmark_returns is a backtest benchmark).
//   - test_episode_costs_include_entry_resize_and_exit               → 6.3 (trade_episodes reads a simulated path).
//   - test_funding_millisecond_timestamp_belongs_to_settlement_bucket → 6.2 (funding ingestion and bucketing).
//   - test_direction_flip_keeps_cost_and_negates_directional_pnl     → 6.3 (invert_path is a P&L diagnostic).
// And from test_crowding_reversal_pre_rust_audit.py: the historical-cutoff
// check is TestEnsembleTargets_PrefixCausalityAtTheManifestCutoffs above;
// the one-bar lag / accounting identity is 6.3.
