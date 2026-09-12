package crowding

import "math"

// RollingZScore standardises ln(ratio) over a trailing window of lookbackBars
// bars, the current completed bar included; NaN wherever the window holds
// fewer than max(20, int(0.8·lookback)) finite values or the bar's own ratio
// is missing or non-positive. Sample standard deviation (ddof = 1). A window
// of IDENTICAL values has std exactly 0 and z = 0/0 = NaN, as in pandas —
// which is what flattens a member whose ratio feed is stuck (see doc.go).
func RollingZScore(ratio []float64, lookbackBars int) []float64 {
	minPeriodsBars := zscoreMinPeriods(lookbackBars)
	logged := make([]float64, len(ratio))
	for i, r := range ratio {
		if math.IsNaN(r) || r <= 0 {
			logged[i] = math.NaN()
		} else {
			logged[i] = math.Log(r)
		}
	}
	out := make([]float64, len(ratio))
	for i := range logged {
		mean, std, ok := rollingMeanStd(logged, i, lookbackBars, minPeriodsBars)
		if !ok || math.IsNaN(logged[i]) {
			out[i] = math.NaN()
			continue
		}
		// pandas divides regardless: a zero std gives ±Inf or NaN, which the
		// hysteresis state machine reads as "not finite" and resets on.
		out[i] = (logged[i] - mean) / std
	}
	return out
}

// zscoreMinPeriods is max(20, int(0.8·lookback)); int() truncates, as
// Python's does.
func zscoreMinPeriods(lookbackBars int) int {
	minPeriodsBars := int(0.8 * float64(lookbackBars))
	if minPeriodsBars < 20 {
		minPeriodsBars = 20
	}
	return minPeriodsBars
}

// rollingMeanVar is the mean and SAMPLE variance of the non-NaN values in
// the window ending at index i (inclusive) of width windowBars, or ok=false
// when fewer than minPeriodsBars such values are in it.
//
// Identical values are a special case on purpose: pandas' online
// calc_mean/calc_var return the value itself and exactly 0 when every value
// in the window is the same (GH#42064), whereas sum/count is off by an ulp
// for most values and would make z ≈ ±0.99 instead of NaN — a member would
// then HOLD through a stuck feed where the reference goes flat (review of
// 2026-09-12).
func rollingMeanVar(values []float64, i, windowBars, minPeriodsBars int) (mean, variance float64, ok bool) {
	start := i - windowBars + 1
	if start < 0 {
		start = 0
	}
	count := 0
	var sum, first float64
	identical := true
	for j := start; j <= i; j++ {
		if v := values[j]; !math.IsNaN(v) {
			if count == 0 {
				first = v
			} else if v != first {
				identical = false
			}
			sum += v
			count++
		}
	}
	if count < minPeriodsBars || count < 2 {
		return 0, 0, false
	}
	if identical {
		return first, 0, true
	}
	mean = sum / float64(count)
	var ss float64
	for j := start; j <= i; j++ {
		if v := values[j]; !math.IsNaN(v) {
			d := v - mean
			ss += d * d
		}
	}
	return mean, ss / float64(count-1), true
}

// rollingMeanStd is rollingMeanVar with the standard deviation.
func rollingMeanStd(values []float64, i, windowBars, minPeriodsBars int) (mean, std float64, ok bool) {
	mean, variance, ok := rollingMeanVar(values, i, windowBars, minPeriodsBars)
	if !ok {
		return 0, 0, false
	}
	return mean, math.Sqrt(variance), true
}

// CrowdingScores is RollingZScore per asset of the panel's account ratio.
func CrowdingScores(panel Panel, lookbackBars int) [][]float64 {
	out := make([][]float64, len(panel.Ratio))
	for a := range panel.Ratio {
		out[a] = RollingZScore(panel.Ratio[a], lookbackBars)
	}
	return out
}
