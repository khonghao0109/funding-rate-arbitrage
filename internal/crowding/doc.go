// Package crowding is the Crowding Reversal strategy's core arithmetic
// (PLAN.md phase 6, step 6.1): a Go port of the frozen Python research
// package docs/research/crowding-reversal-20260904, proven equal to that
// package's own golden fixture and nothing more.
//
// It is DIRECTIONAL — long or short BTC/ETH perps against the crowd — and
// therefore the one strategy in this repository that is not delta-neutral.
// It shares nothing with internal/strategy except the repository's rules:
// normalized inputs only, no clock, no network, every identifier carrying its
// unit, and never the word "net" (CLAUDE.md rule 2) — its output is a target
// FRACTION OF EQUITY, before any cost.
//
// # The nine definitions, as the research package names them
//
//	rolling_zscore            RollingZScore
//	crowding_scores           CrowdingScores
//	hysteresis_signal         HysteresisSignal
//	causal_weights            CausalWeights
//	trend_confirmation_votes  TrendConfirmationVotes
//	apply_trend_confirmation  ApplyTrendConfirmation
//	ensemble_targets          EnsembleTargets
//	StrategyConfig            Config
//	constants                 BarsPerDay, BarsPerYear, EnsembleMembers
//
// The fixture has no column that is the output of ONE of these: its expected
// score/signal/target are the 12-member ensemble's averages, so the whole
// path is ported and compared, not four functions.
//
// # pandas semantics this port reproduces (written before the code)
//
//   - ln(ratio) is taken only where ratio > 0; a missing or non-positive
//     ratio is NaN and is NEVER filled, forward or backward.
//   - Rolling windows are the last `lookback` bars including the current
//     one; a window is valid when it holds at least min_periods non-NaN
//     values, and the mean/std/var/cov are taken over those values.
//     min_periods is max(20, int(0.8·lookback)) for the z-score and
//     int(0.8·window) for the volatility, variance and covariance (the two
//     differ, exactly as in the research code).
//   - std, var and cov are SAMPLE statistics (ddof = 1); the covariance is
//     pandas' (mean(xy) − mean(x)·mean(y)) · n/(n−1) with mean(x) and mean(y)
//     over each series' own non-NaN values and mean(xy), n over the pairs
//     where both are non-NaN.
//   - A window of IDENTICAL values has mean = that value and variance
//     EXACTLY 0 (pandas' online statistics special-case it), so its z-score
//     is 0/0 = NaN and the state machine resets — a member whose ratio feed
//     is stuck goes flat instead of holding on a rounding-error z.
//   - A NaN score resets the hysteresis state to 0 (flat); it does not hold
//     the previous state. A z-score at exactly ±entry enters; |z| at exactly
//     exit leaves.
//   - Returns are pct_change with no fill: NaN at the first bar and wherever
//     either close is NaN.
//   - The risk scale is min(target_vol / portfolio_vol, max_gross), NaN and
//     ±Inf become 0, and the first int(0.8·window) bars are forced to 0
//     whatever the rolling statistics say (the research package's warm-up
//     convention, kept on purpose).
//   - Momentum is pct_change(days × 6) > 0, and a horizon whose start lies
//     before the series exists is NOT a vote against — "not enough history"
//     is masked out, never read as bearish.
//   - The 12 members are averaged FIRST and the trend filter is applied to
//     the average; a member's volatility window is its own lookback.
//   - BarsPerYear is 6 × 365.25 for a 4h grid.
//
// # What parity proves, and what it does not
//
// The fixture is the research package's 4h panel AFTER its own aggregation
// of 1-minute closes and 5-minute account ratios (right-closed, right-
// labelled). Parity therefore proves Go = Python on that already-aggregated
// path: |target error| ≤ 1e-10, signal EXACTLY equal (it is a multiple of
// 1/12 and a difference there is a wrong state machine, not rounding), score
// within a stated tolerance. It does NOT prove the aggregation layer (a
// different boundary convention moved the package's CAGR from 26.94/36.84 to
// 28.84/38.49 — step 6.2 decides the convention before any table is written),
// it does not prove an edge, and bit-exactness is not claimed: Go's math.Log
// differs from glibc's by one ulp on 8.4% of the ratio values. The lag,
// P&L and equity columns of the fixture are the backtest's (step 6.3) and
// are not compared here.
//
// Introduced in: PLAN.md phase 6, step 6.1.
package crowding
