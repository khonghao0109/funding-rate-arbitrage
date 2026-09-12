package crowding

import (
	"fmt"
	"math"
)

// HysteresisSignal fades extremes and stays in the state until the ratio
// normalises: a score ≥ entryZ opens a SHORT (−1), ≤ −entryZ a LONG (+1),
// and the state returns to 0 once |score| ≤ exitZ. A non-finite score resets
// the state to 0 — a missing observation fails closed, never holds.
//
// Returns one series per asset, values in {−1, 0, +1}.
func HysteresisSignal(score [][]float64, entryZ, exitZ float64, side Side) ([][]float64, error) {
	if !(0 <= exitZ && exitZ < entryZ) {
		return nil, fmt.Errorf("crowding: require 0 <= exit_z (%v) < entry_z (%v)", exitZ, entryZ)
	}
	switch side {
	case SideBoth, SideLong, SideShort:
	default:
		return nil, fmt.Errorf("crowding: unknown side %q", side)
	}
	out := make([][]float64, len(score))
	for a, values := range score {
		result := make([]float64, len(values))
		state := 0.0
		for i, v := range values {
			switch {
			case math.IsNaN(v) || math.IsInf(v, 0):
				state = 0
			case state == 0:
				if v >= entryZ && (side == SideBoth || side == SideShort) {
					state = -1
				} else if v <= -entryZ && (side == SideBoth || side == SideLong) {
					state = 1
				}
			case math.Abs(v) <= exitZ:
				state = 0
			}
			result[i] = state
		}
		out[a] = result
	}
	return out, nil
}
