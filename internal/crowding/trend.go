package crowding

import (
	"fmt"
	"math"
)

// TrendConfirmationVotes counts, per asset and bar, how many momentum
// horizons agree with the proposed direction: a positive target needs
// pct_change(days × 6) > 0, a negative one needs it ≤ 0 — and a horizon
// whose start lies before the series has data is masked out, never counted
// as a vote against.
func TrendConfirmationVotes(close [][]float64, target [][]float64, horizonsDays []int) [][]int {
	votes := make([][]int, len(target))
	for a := range target {
		votes[a] = make([]int, len(target[a]))
	}
	for _, days := range horizonsDays {
		periodsBars := days * BarsPerDay
		for a := range target {
			momentum := pctChange(close[a], periodsBars)
			for i := range target[a] {
				up := momentum[i] > 0 // false on NaN
				t := target[a][i]
				aligned := (t > 0 && up) || (t < 0 && !up)
				available := i >= periodsBars && !math.IsNaN(close[a][i-periodsBars])
				if aligned && available {
					votes[a][i]++
				}
			}
		}
	}
	return votes
}

// ApplyTrendConfirmation zeroes every target that fewer than `required`
// horizons agree with.
func ApplyTrendConfirmation(close [][]float64, target [][]float64, horizonsDays []int, required int) ([][]float64, error) {
	if len(horizonsDays) == 0 || required < 1 || required > len(horizonsDays) {
		return nil, fmt.Errorf("crowding: invalid trend-confirmation vote: %d of %d horizons", required, len(horizonsDays))
	}
	votes := TrendConfirmationVotes(close, target, horizonsDays)
	out := make([][]float64, len(target))
	for a := range target {
		out[a] = make([]float64, len(target[a]))
		for i, t := range target[a] {
			if votes[a][i] >= required {
				out[a][i] = t
			}
		}
	}
	return out, nil
}
