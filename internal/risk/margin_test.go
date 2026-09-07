package risk

import (
	"math"
	"testing"
)

func verifiedBracket(mm float64) Bracket {
	return Bracket{Source: "bybit_futures", MaintenanceMarginFrac: mm, MaxLeverage: 150, Verified: true}
}

// The whole point of the model: a delta-neutral position is flat in the coin
// and its PERP leg is not. The liquidation price depends on the collateral
// posted and the venue's maintenance rate, and on nothing else.
func TestEvaluate_LiquidationPriceIsMarginOverMaintenance(t *testing.T) {
	pos := Position{NotionalQuote: 50_000, EntryPriceQuote: 80_000, MarginFrac: 0.10}
	got := Evaluate(pos, verifiedBracket(0.0033), 80_000)
	if !got.OK {
		t.Fatalf("not OK: %s", got.ReasonVI)
	}
	// 80000 * 1.10 / 1.0033
	want := 80_000 * 1.10 / 1.0033
	if math.Abs(got.LiquidationPriceQuote-want) > 1e-6 {
		t.Errorf("liquidation = %.4f, want %.4f", got.LiquidationPriceQuote, want)
	}
	// At entry the buffer is the whole distance, just under the 10% the
	// leverage suggests — maintenance eats the rest.
	if got.BufferPct <= 9.4 || got.BufferPct >= 10 {
		t.Errorf("buffer at entry = %.4f%%, want just under 10%%", got.BufferPct)
	}
	if got.Liquidated {
		t.Error("liquidated at the entry price")
	}
	// It does not move with the price: the same position priced anywhere has
	// the same liquidation price.
	moved := Evaluate(pos, verifiedBracket(0.0033), 85_000)
	if math.Abs(moved.LiquidationPriceQuote-got.LiquidationPriceQuote) > 1e-9 {
		t.Errorf("the liquidation price moved with the market: %.4f then %.4f",
			got.LiquidationPriceQuote, moved.LiquidationPriceQuote)
	}
	if moved.BufferPct >= got.BufferPct {
		t.Error("the buffer must SHRINK as the price rises against a short")
	}
}

func TestEvaluate_LiquidatesExactlyAtTheComputedPrice(t *testing.T) {
	pos := Position{NotionalQuote: 50_000, EntryPriceQuote: 80_000, MarginFrac: 0.10}
	bracket := verifiedBracket(0.0033)
	liq := Evaluate(pos, bracket, 80_000).LiquidationPriceQuote

	if just := Evaluate(pos, bracket, liq*(1-1e-9)); just.Liquidated {
		t.Error("liquidated just BELOW the liquidation price")
	}
	if at := Evaluate(pos, bracket, liq); !at.Liquidated {
		t.Errorf("not liquidated AT the liquidation price (equity %.6f, maintenance %.6f)",
			at.EquityQuote, at.MaintenanceQuote)
	}
	if above := Evaluate(pos, bracket, liq*1.01); !above.Liquidated || above.BufferPct >= 0 {
		t.Errorf("above the liquidation price: liquidated=%v buffer=%.4f%%", above.Liquidated, above.BufferPct)
	}
}

// An unverified maintenance rate must refuse, never default to 0 — a 0 puts the
// liquidation price further away than any venue would allow, which is the most
// dangerous direction a missing number can round.
func TestEvaluate_RefusesAnUnverifiedBracket(t *testing.T) {
	pos := Position{NotionalQuote: 50_000, EntryPriceQuote: 80_000, MarginFrac: 0.10}
	got := Evaluate(pos, Bracket{Source: "binance_futures", MaintenanceMarginFrac: 0}, 80_000)
	if got.OK {
		t.Errorf("an unverified schedule produced a liquidation price of %.2f", got.LiquidationPriceQuote)
	}
	if got.ReasonVI == "" {
		t.Error("the refusal carries no reason")
	}
}

// Maintenance is a STEP function of size. A position above the tier ceiling
// belongs to a stricter bracket, and answering with this one's number is worse
// than answering with none: it is optimistic by exactly the difference.
func TestEvaluate_RefusesAPositionAboveTheTierCeiling(t *testing.T) {
	bracket := verifiedBracket(0.0033)
	bracket.TierCeilingQuote = 300_000

	inside := Evaluate(Position{NotionalQuote: 50_000, EntryPriceQuote: 80_000, MarginFrac: 0.10}, bracket, 80_000)
	if !inside.OK {
		t.Fatalf("a position inside the tier was refused: %s", inside.ReasonVI)
	}
	outside := Evaluate(Position{NotionalQuote: 400_000, EntryPriceQuote: 80_000, MarginFrac: 0.10}, bracket, 80_000)
	if outside.OK {
		t.Error("a position above the tier ceiling was priced against this tier's rate")
	}
}

func TestEvaluate_RefusesInputsThatDescribeNoPosition(t *testing.T) {
	bracket := verifiedBracket(0.0033)
	good := Position{NotionalQuote: 50_000, EntryPriceQuote: 80_000, MarginFrac: 0.10}
	for name, bad := range map[string]struct {
		pos   Position
		price float64
		b     Bracket
	}{
		"no notional":       {Position{EntryPriceQuote: 80_000, MarginFrac: 0.1}, 80_000, bracket},
		"no entry price":    {Position{NotionalQuote: 50_000, MarginFrac: 0.1}, 80_000, bracket},
		"no current price":  {good, 0, bracket},
		"no margin":         {Position{NotionalQuote: 50_000, EntryPriceQuote: 80_000}, 80_000, bracket},
		"margin over 100%":  {Position{NotionalQuote: 50_000, EntryPriceQuote: 80_000, MarginFrac: 1.5}, 80_000, bracket},
		"maintenance >= 1":  {good, 80_000, Bracket{Source: "x", MaintenanceMarginFrac: 1, Verified: true}},
		"maintenance < 0":   {good, 80_000, Bracket{Source: "x", MaintenanceMarginFrac: -0.1, Verified: true}},
		"price is infinite": {good, math.Inf(1), bracket},
		"price is NaN":      {good, math.NaN(), bracket},
	} {
		if got := Evaluate(bad.pos, bad.b, bad.price); got.OK {
			t.Errorf("%s: accepted, liquidation = %v", name, got.LiquidationPriceQuote)
		}
	}
}

// Fully collateralized is not un-liquidatable: at 1x a short still dies if the
// price roughly doubles, because it owes the whole rise.
func TestEvaluate_OneTimesLeverageStillHasALiquidationPrice(t *testing.T) {
	got := Evaluate(Position{NotionalQuote: 50_000, EntryPriceQuote: 80_000, MarginFrac: 1.0},
		verifiedBracket(0.005), 80_000)
	if !got.OK {
		t.Fatalf("not OK: %s", got.ReasonVI)
	}
	if got.BufferPct < 95 || got.BufferPct > 100 {
		t.Errorf("a 1x short liquidates at +%.2f%%, expected just under +100%%", got.BufferPct)
	}
}
