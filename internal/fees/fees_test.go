package fees

import (
	"math"
	"strings"
	"testing"
)

// A venue whose fee has not been verified must not look free. Zero is a
// legitimate fee on some venues, so the absence of data needs its own flag - the
// same mistake the top-of-book quantities avoided at step 1.2.
func TestFor_UnverifiedVenueIsFlaggedNotZeroed(t *testing.T) {
	unknown := For("some_venue_nobody_surveyed")
	if unknown.Verified {
		t.Error("an unknown source must not report a verified fee")
	}
	if unknown.TakerFeeBps != 0 || unknown.MakerFeeBps != 0 {
		t.Errorf("an unknown source must report 0, got %+v", unknown)
	}

	// And a venue in the table that was not verified is the same case.
	bybit := For("bybit_futures")
	if bybit.Verified {
		t.Error("bybit_futures was not verifiable from documentation; it must not claim to be")
	}
	if bybit.NoteVI == "" {
		t.Error("an unverified venue must say why it has no number")
	}
}

// The cost of a spread is FOUR fills, not two: the spread is only realised by
// unwinding, so entry alone understates what the trade costs by exactly the
// exit.
func TestRoundTripTakerPct_ChargesEntryAndExit(t *testing.T) {
	// hyperliquid taker 4.5 bps, kraken taker 5.0 bps.
	// (4.5 + 5.0) bps entry + the same again on exit = 19 bps = 0.19%.
	got, ok := RoundTripTakerPct(For("hyperliquid_futures"), For("kraken_futures"))
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

func TestRoundTripTakerPct_UnverifiedVenueYieldsNoNumber(t *testing.T) {
	if _, ok := RoundTripTakerPct(For("binance_futures"), For("bybit_futures")); ok {
		t.Error("a pair with an unverified venue must not produce a cost")
	}
	if _, ok := RoundTripTakerPct(For("bybit_futures"), For("okx_futures")); ok {
		t.Error("two unverified venues must not produce a cost")
	}
}

// Every number in the table has to be traceable to the venue's own published
// schedule. A fee remembered rather than read is exactly the plausible-but-wrong
// number CLAUDE.md rule 5 forbids.
func TestSchedules_EveryVerifiedEntryCitesItsSource(t *testing.T) {
	for _, s := range All() {
		if !s.Verified {
			continue
		}
		if s.DocURL == "" {
			t.Errorf("%s reports a verified fee with no citation", s.Source)
		}
		if !strings.HasPrefix(s.DocURL, "https://") {
			t.Errorf("%s citation is not a URL: %q", s.Source, s.DocURL)
		}
	}
}

// A decimal point in the wrong place turns 0.05% into 0.5% or 0.005%. These
// bounds do not verify the numbers, they catch a slip.
func TestSchedules_AreWithinAPlausibleRange(t *testing.T) {
	for _, s := range All() {
		if !s.Verified {
			continue
		}
		if s.TakerFeeBps <= 0 || s.TakerFeeBps > 30 {
			t.Errorf("%s taker = %g bps, outside the plausible 0-30 bps range", s.Source, s.TakerFeeBps)
		}
		if s.MakerFeeBps < 0 || s.MakerFeeBps > 30 {
			t.Errorf("%s maker = %g bps, outside the plausible 0-30 bps range", s.Source, s.MakerFeeBps)
		}
		if s.MakerFeeBps > s.TakerFeeBps {
			t.Errorf("%s maker %g > taker %g; a venue does not charge more to add liquidity",
				s.Source, s.MakerFeeBps, s.TakerFeeBps)
		}
	}
}

// The real schedules are not whole basis points - Hyperliquid's maker leg is
// 1.5 bps and Paradex's is 0.3 bps. Rounding them to integers would misstate the
// cost by up to 100%, which is why the field is fractional. See
// docs/CONVENTIONS.md §1.2.
func TestSchedules_FractionalBpsAreRepresented(t *testing.T) {
	hyperliquid := For("hyperliquid_futures")
	if math.Abs(hyperliquid.MakerFeeBps-1.5) > 1e-9 {
		t.Errorf("hyperliquid maker = %g bps, want 1.5", hyperliquid.MakerFeeBps)
	}
	if hyperliquid.MakerFeeBps == math.Trunc(hyperliquid.MakerFeeBps) {
		t.Error("a fractional fee was rounded to a whole basis point")
	}
}

// An oracle has no fee because nothing trades on it. That is a different fact
// from "we have not looked it up", and the note has to say which.
func TestFor_OracleHasNoFeeForAReason(t *testing.T) {
	pyth := For("pyth")
	if pyth.Verified {
		t.Error("an oracle must not report a tradable fee")
	}
	if !strings.Contains(strings.ToLower(pyth.NoteVI), "oracle") {
		t.Errorf("pyth note should say it is an oracle, got %q", pyth.NoteVI)
	}
}
