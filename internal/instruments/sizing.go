package instruments

import (
	"fmt"
	"math"

	"futures-arbitrage-scanner/exchanges"
)

// SizingRequest names every number SizeDeltaNeutral consumes. Three adjacent
// bare float64 parameters would let a transposed spot/perp price compile and
// size the position off the wrong leg (CONVENTIONS §6.2: >4 parameters →
// struct; §1: the field names carry the units).
type SizingRequest struct {
	SpotPriceQuote float64
	PerpPriceQuote float64
	NotionalQuote  float64
}

// DeltaNeutralSize is a validated order size for the two legs of one
// spot-long / perp-short position. QtyCoin is identical on both legs by
// construction — that equality IS delta neutrality in the base coin.
type DeltaNeutralSize struct {
	QtyCoin float64

	// SpotQtyUnits is the spot order size in the venue's native unit (coin).
	// PerpQtyUnits is the perp order size in the venue's native unit —
	// CONTRACTS when the venue is contract-denominated, coin otherwise.
	SpotQtyUnits float64
	PerpQtyUnits float64

	SpotNotionalQuote float64
	PerpNotionalQuote float64
}

// gridTolerance absorbs float64 representation error when checking that a
// quantity sits on a venue's step grid (128 × 0.0001 is not exactly 0.0128).
// It is a tolerance on the STEP COUNT, so it does not scale with quantity.
const gridTolerance = 1e-6

// SizeDeltaNeutral turns a target notional into a valid equal-coin size for
// both legs, or refuses with the reason. The rule set is docs/PLAN.md step
// 2.3: round DOWN to the COARSER step of the two legs, then require the
// result to clear the venue minimums on BOTH legs before anything is placed —
// the two venues' steps differ (docs/DATA-REQUIREMENTS.md §5), and rounding
// each leg separately is how a "delta-neutral" position starts life
// unbalanced.
//
// The notional is anchored to the SPOT leg: that is the capital that actually
// buys coin; the perp leg's notional then differs by the basis, which is
// reported back, not hidden.
func SizeDeltaNeutral(spot, perp exchanges.Instrument, req SizingRequest) (DeltaNeutralSize, error) {
	if req.NotionalQuote <= 0 {
		return DeltaNeutralSize{}, fmt.Errorf("notional %v is not positive", req.NotionalQuote)
	}
	if req.SpotPriceQuote <= 0 || req.PerpPriceQuote <= 0 {
		return DeltaNeutralSize{}, fmt.Errorf("price not positive (spot %v, perp %v)", req.SpotPriceQuote, req.PerpPriceQuote)
	}

	type leg struct {
		name       string
		inst       exchanges.Instrument
		priceQuote float64
	}
	legs := []leg{
		{"spot", spot, req.SpotPriceQuote},
		{"perp", perp, req.PerpPriceQuote},
	}

	// A contract-denominated SPOT venue does not exist in this system; the
	// return path would silently emit its native quantity in coin, so refuse
	// loudly instead of shipping a 1/ContractSizeCoin× order.
	if spot.IsContract {
		return DeltaNeutralSize{}, fmt.Errorf("spot leg %s/%s is contract-denominated — not supported", spot.Source, spot.Symbol)
	}
	for _, l := range legs {
		if l.inst.Status != exchanges.StatusTrading {
			return DeltaNeutralSize{}, fmt.Errorf("%s leg %s/%s is not trading (status %q)",
				l.name, l.inst.Source, l.inst.Symbol, l.inst.Status)
		}
		if l.inst.StepSizeCoin <= 0 {
			return DeltaNeutralSize{}, fmt.Errorf("%s leg %s/%s has no known step size",
				l.name, l.inst.Source, l.inst.Symbol)
		}
		if l.inst.IsContract && l.inst.ContractSizeCoin <= 0 {
			return DeltaNeutralSize{}, fmt.Errorf("%s leg %s/%s is contract-denominated with unknown contract size",
				l.name, l.inst.Source, l.inst.Symbol)
		}
	}

	// Round DOWN on the coarser grid. The tolerance keeps a target that is
	// exactly n steps (up to float representation) from losing a step. A
	// result of zero steps is refused here — MinQtyCoin cannot be relied on
	// for that floor, because two venues publish no minimum at all and their
	// field is 0 = "not stated".
	coarserStepCoin := math.Max(spot.StepSizeCoin, perp.StepSizeCoin)
	targetQtyCoin := req.NotionalQuote / req.SpotPriceQuote
	qtyCoin := math.Floor(targetQtyCoin/coarserStepCoin+gridTolerance) * coarserStepCoin
	if qtyCoin <= 0 {
		return DeltaNeutralSize{}, fmt.Errorf(
			"notional %v at spot price %v is below one step (%v coin, the coarser of the two legs) — minimum viable notional not reached",
			req.NotionalQuote, req.SpotPriceQuote, coarserStepCoin)
	}

	// Max-size first: beyond ~1e10 steps the grid check's float tolerance
	// degrades, and an absurd notional should be refused as "too big", not
	// misdiagnosed as an incommensurable grid.
	for _, l := range legs {
		if l.inst.MaxQtyCoin > 0 && qtyCoin > l.inst.MaxQtyCoin {
			return DeltaNeutralSize{}, fmt.Errorf(
				"%v coin exceeds the %s leg's maximum order size %v on %s — split the order instead of capping silently",
				qtyCoin, l.name, l.inst.MaxQtyCoin, l.inst.Source)
		}
	}

	// The floor result must sit on BOTH venues' grids. With the usual
	// decimal steps the coarser is a multiple of the finer and this always
	// holds; when it does not (incommensurable steps), refusing beats
	// shipping a size one venue would reject or round for us.
	for _, l := range legs {
		steps := qtyCoin / l.inst.StepSizeCoin
		if math.Abs(steps-math.Round(steps)) > gridTolerance {
			return DeltaNeutralSize{}, fmt.Errorf(
				"%v coin is not a whole number of %s steps (%v) on %s — the legs' step sizes are incommensurable",
				qtyCoin, l.name, l.inst.StepSizeCoin, l.inst.Source)
		}
	}

	// Venue minimums and maximums on BOTH legs. A zero MinQtyCoin or
	// MinNotionalQuote means the venue publishes none — nothing to check,
	// never "zero is acceptable".
	for _, l := range legs {
		if l.inst.MinQtyCoin > 0 && qtyCoin < l.inst.MinQtyCoin-gridTolerance*l.inst.StepSizeCoin {
			return DeltaNeutralSize{}, fmt.Errorf(
				"%v coin is below the %s leg's minimum quantity %v on %s (notional %v at price %v)",
				qtyCoin, l.name, l.inst.MinQtyCoin, l.inst.Source, req.NotionalQuote, l.priceQuote)
		}
		if legNotional := qtyCoin * l.priceQuote; l.inst.MinNotionalQuote > 0 && legNotional < l.inst.MinNotionalQuote {
			return DeltaNeutralSize{}, fmt.Errorf(
				"notional %v on the %s leg is below %s's minimum notional %v",
				legNotional, l.name, l.inst.Source, l.inst.MinNotionalQuote)
		}
	}

	perpQtyUnits := qtyCoin
	if perp.IsContract {
		perpQtyUnits = qtyCoin / perp.ContractSizeCoin
	}
	return DeltaNeutralSize{
		QtyCoin:           qtyCoin,
		SpotQtyUnits:      qtyCoin,
		PerpQtyUnits:      perpQtyUnits,
		SpotNotionalQuote: qtyCoin * req.SpotPriceQuote,
		PerpNotionalQuote: qtyCoin * req.PerpPriceQuote,
	}, nil
}
