package fees

import (
	"math"
	"testing"
)

// The schedules themselves live in config.yaml since step 1.4; internal/config
// validates them (a verified fee needs a citation, an unverified one must carry
// no numbers, a taker fee outside 0-30 bps is a misplaced decimal point). What
// is left here is the calculation.

func verified(taker float64) Schedule {
	return Schedule{Source: "v", MakerFeeBps: taker / 3, TakerFeeBps: taker, Verified: true}
}

// The cost of a spread is FOUR fills, not two: the spread is only realised by
// unwinding, so entry alone understates what the trade costs by exactly the
// exit.
func TestRoundTripTakerPct_ChargesEntryAndExit(t *testing.T) {
	// 4.5 bps + 5.0 bps entry, and the same again on exit = 19 bps = 0.19%.
	got, ok := RoundTripTakerPct(verified(4.5), verified(5.0))
	if !ok {
		t.Fatal("both venues are verified; the cost must be computable")
	}
	if math.Abs(got-0.19) > 1e-9 {
		t.Errorf("round trip = %g%%, want 0.19%%", got)
	}

	// Half the fills would be exactly half the cost, which is the mistake this
	// guards against.
	if math.Abs(got-0.095) < 1e-9 {
		t.Error("only entry was charged; the exit fills are missing")
	}
}

// A venue with no verified schedule must produce no number at all. Treating the
// missing fee as zero would report the full gross spread as if it were free to
// capture - and zero is a real fee on some venues, so the flag is the only thing
// that can tell the two apart.
func TestRoundTripTakerPct_UnverifiedVenueYieldsNoNumber(t *testing.T) {
	unverified := Schedule{Source: "u"}

	if _, ok := RoundTripTakerPct(verified(5), unverified); ok {
		t.Error("a pair with an unverified sell venue must not produce a cost")
	}
	if _, ok := RoundTripTakerPct(unverified, verified(5)); ok {
		t.Error("a pair with an unverified buy venue must not produce a cost")
	}
	if _, ok := RoundTripTakerPct(unverified, unverified); ok {
		t.Error("two unverified venues must not produce a cost")
	}
}

// A venue that really does charge nothing is verified with zero fees, and that
// is a computable cost - unlike an unverified one.
func TestRoundTripTakerPct_AVerifiedZeroFeeIsStillANumber(t *testing.T) {
	free := Schedule{Source: "free", Verified: true}

	got, ok := RoundTripTakerPct(free, free)
	if !ok {
		t.Fatal("a verified zero fee is data, not a missing value")
	}
	if got != 0 {
		t.Errorf("round trip = %g, want 0", got)
	}
}

// The real schedules are not whole basis points - Hyperliquid's maker leg is
// 1.5 bps and Paradex's is 0.3 bps - so the arithmetic has to carry fractions
// through. See docs/CONVENTIONS.md §1.2.
func TestRoundTripTakerPct_CarriesFractionalBasisPoints(t *testing.T) {
	got, ok := RoundTripTakerPct(verified(4.5), verified(4.5))
	if !ok {
		t.Fatal("expected a cost")
	}
	if want := 0.18; math.Abs(got-want) > 1e-9 {
		t.Errorf("round trip = %g%%, want %g%%", got, want)
	}
}

// Basis points to percent is a factor of 100, and getting it wrong by an order
// of magnitude is the kind of error that makes every spread look profitable.
func TestRoundTripTakerPct_ConvertsBasisPointsToPercent(t *testing.T) {
	// 100 bps a leg, four legs = 400 bps = 4%.
	got, _ := RoundTripTakerPct(
		Schedule{Verified: true, TakerFeeBps: 100},
		Schedule{Verified: true, TakerFeeBps: 100},
	)
	if math.Abs(got-4.0) > 1e-9 {
		t.Errorf("round trip = %g%%, want 4%%", got)
	}
}
