package crowding

import (
	"fmt"
	"math"
)

// CausalWeights turns per-asset signals into target fractions of equity:
// inverse-volatility mix across the two assets, then one risk scale from the
// trailing two-asset covariance so the portfolio's annualised volatility
// targets targetVolAnnualFrac, capped so Σ|weight| ≤ maxGrossFrac.
//
// Every statistic is trailing over volWindowBars bars (sample, ddof = 1),
// valid from int(0.8·window) finite observations, and the first
// int(0.8·window) bars are forced to zero — the research package's warm-up
// convention, reproduced rather than improved.
func CausalWeights(close [][]float64, signal [][]float64, targetVolAnnualFrac, maxGrossFrac float64, volWindowBars int) ([][]float64, error) {
	if len(close) != 2 || len(signal) != 2 {
		return nil, fmt.Errorf("crowding: causal weights need exactly two assets (got %d close, %d signal)", len(close), len(signal))
	}
	n := len(close[0])
	if len(close[1]) != n || len(signal[0]) != n || len(signal[1]) != n {
		return nil, fmt.Errorf("crowding: series lengths differ")
	}
	minPeriodsBars := int(0.8 * float64(volWindowBars))

	returns := [2][]float64{pctChange(close[0], 1), pctChange(close[1], 1)}

	// raw = signal / vol, ±Inf → NaN, NaN → 0; mix = raw / Σ|raw|, Σ=0 → 0.
	mix := [2][]float64{make([]float64, n), make([]float64, n)}
	for i := 0; i < n; i++ {
		var raw [2]float64
		var absSum float64
		for a := 0; a < 2; a++ {
			_, std, ok := rollingMeanStd(returns[a], i, volWindowBars, minPeriodsBars)
			vol := math.NaN()
			if ok {
				vol = std * math.Sqrt(BarsPerYear)
			}
			v := signal[a][i] / vol
			if math.IsInf(v, 0) || math.IsNaN(v) {
				v = 0
			}
			raw[a] = v
			absSum += math.Abs(v)
		}
		for a := 0; a < 2; a++ {
			if absSum == 0 {
				mix[a][i] = 0
			} else {
				mix[a][i] = raw[a] / absSum
			}
		}
	}

	weights := [2][]float64{make([]float64, n), make([]float64, n)}
	for i := 0; i < n; i++ {
		varL, okL := rollingVar(returns[0], i, volWindowBars, minPeriodsBars)
		varR, okR := rollingVar(returns[1], i, volWindowBars, minPeriodsBars)
		cov, okC := rollingCov(returns[0], returns[1], i, volWindowBars, minPeriodsBars)
		if !okL {
			varL = math.NaN()
		}
		if !okR {
			varR = math.NaN()
		}
		if !okC {
			cov = math.NaN()
		}
		// NaN propagates through 0 × NaN exactly as it does in pandas: a
		// flat mix beside an undefined variance is still an undefined
		// portfolio variance, and the scale below reads that as 0.
		pv := BarsPerYear * (mix[0][i]*mix[0][i]*varL + mix[1][i]*mix[1][i]*varR + 2.0*mix[0][i]*mix[1][i]*cov)
		var scale float64
		switch {
		case math.IsNaN(pv):
			scale = 0
		default:
			if pv < 0 {
				pv = 0
			}
			pvol := math.Sqrt(pv)
			if pvol == 0 {
				scale = 0 // 0 → NaN → 0 in the research code
			} else {
				scale = targetVolAnnualFrac / pvol
				if scale > maxGrossFrac {
					scale = maxGrossFrac
				}
				if math.IsInf(scale, 0) || math.IsNaN(scale) {
					scale = 0
				}
			}
		}
		if i < minPeriodsBars {
			scale = 0
		}
		var gross float64
		for a := 0; a < 2; a++ {
			w := mix[a][i] * scale
			if math.IsNaN(w) {
				w = 0
			}
			weights[a][i] = w
			gross += math.Abs(w)
		}
		if gross > maxGrossFrac+1e-10 {
			return nil, fmt.Errorf("crowding: gross exposure cap violated at bar %d: %v > %v", i, gross, maxGrossFrac)
		}
	}
	return weights[:], nil
}

// pctChange is pandas pct_change(periodsBars, fill_method=None): NaN for
// the first periodsBars bars and wherever either close is NaN.
func pctChange(close []float64, periodsBars int) []float64 {
	out := make([]float64, len(close))
	for i := range close {
		if i < periodsBars || math.IsNaN(close[i]) || math.IsNaN(close[i-periodsBars]) {
			out[i] = math.NaN()
			continue
		}
		out[i] = close[i]/close[i-periodsBars] - 1
	}
	return out
}

// rollingVar is the SAMPLE variance of the non-NaN values in the window
// ending at i, or ok=false below minPeriodsBars such values.
func rollingVar(values []float64, i, windowBars, minPeriodsBars int) (float64, bool) {
	_, variance, ok := rollingMeanVar(values, i, windowBars, minPeriodsBars)
	return variance, ok
}

// rollingCov is pandas' rolling covariance (ddof = 1): (mean(xy) −
// mean(x)·mean(y)) · n/(n−1), where mean(x) and mean(y) are each over that
// series' OWN non-NaN values in the window (marginal, not pairwise), and
// mean(xy) and n are over the pairs where both are non-NaN; NaN (ok=false)
// when any of the three has fewer than minPeriodsBars observations.
func rollingCov(x, y []float64, i, windowBars, minPeriodsBars int) (float64, bool) {
	start := i - windowBars + 1
	if start < 0 {
		start = 0
	}
	var sumX, sumY, sumXY float64
	var nX, nY, nXY int
	for j := start; j <= i; j++ {
		xv, yv := x[j], y[j]
		okX, okY := !math.IsNaN(xv), !math.IsNaN(yv)
		if okX {
			sumX += xv
			nX++
		}
		if okY {
			sumY += yv
			nY++
		}
		if okX && okY {
			sumXY += xv * yv
			nXY++
		}
	}
	if nX < minPeriodsBars || nY < minPeriodsBars || nXY < minPeriodsBars || nXY < 2 {
		return 0, false
	}
	meanX, meanY, meanXY := sumX/float64(nX), sumY/float64(nY), sumXY/float64(nXY)
	return (meanXY - meanX*meanY) * (float64(nXY) / float64(nXY-1)), true
}
