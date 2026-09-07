package backtest

import (
	"math"
	"strings"
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/risk"
	"futures-arbitrage-scanner/internal/strategy"
)

func verifiedBracket() risk.Bracket {
	return risk.Bracket{Source: "binance_futures", MaintenanceMarginFrac: 0.005, MaxLeverage: 125, Verified: true}
}

// marginSeries is a funding series with hourly candles for both legs, flat
// except for whatever the caller does to the perp highs.
func marginSeries(t *testing.T, perpHighs func(i int, c *exchanges.PriceCandle)) (Series, Window) {
	t.Helper()
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2, 2, 2, 2)
	series := seriesOf(entries)
	series.PerpMargin = verifiedBracket()

	hours := int((entries[len(entries)-1].SettledAtMs-entries[0].SettledAtMs)/(3600*msPerSec)) + 4
	flat := make([]float64, hours)
	for i := range flat {
		flat[i] = 100
	}
	series.SpotCandles = hourlyCandles("binance_spot", flat...)
	series.PerpCandles = hourlyCandles("binance_futures", flat...)
	shift := entries[0].SettledAtMs - epoch
	for i := range series.SpotCandles {
		series.SpotCandles[i].OpenTimeMs += shift
		series.PerpCandles[i].OpenTimeMs += shift
		if perpHighs != nil {
			perpHighs(i, &series.PerpCandles[i])
		}
	}
	return series, fullWindow(entries)
}

func marginParams() strategy.Params {
	p := testParams()
	p.PerpMarginFrac = 0.10         // 10x: liquidation about 9.4% up
	p.MinLiquidationBufferPct = 2.0 // and leave with 2% of room left
	p.MaxBasisPct, p.MaxBasisWidenPct = 100, 100
	return p
}

// The reason the intrabar check exists: a short dies on a SPIKE, and an hourly
// close steps straight over one. A bar that opens and closes at 100 but printed
// a high of 115 liquidated a 10x short, and a close-only model reports a
// profitable hold.
func TestRun_LiquidationIsFoundInTheCANDLEHIGHNotTheClose(t *testing.T) {
	// Hour 20: settlements are 8-hourly and entry fires on the third (hour 16),
	// so this bar is inside the first holding period. Its open and close stay
	// at 100 — only the high moves.
	spike := func(i int, c *exchanges.PriceCandle) {
		if i == 20 {
			c.HighPriceQuote = 115
		}
	}
	series, window := marginSeries(t, spike)

	// Close-only: nothing ever moves, so nothing is liquidated.
	flat, flatWindow := marginSeries(t, nil)
	if got := Run(flat, flatWindow, marginParams()); got.Liquidations != 0 {
		t.Fatalf("a flat series reported %d liquidations", got.Liquidations)
	}

	got := Run(series, window, marginParams())
	if !got.OK {
		t.Fatalf("Run: %s", got.ReasonVI)
	}
	if got.Liquidations != 1 {
		t.Fatalf("a 15%% spike against a 9.4%% liquidation gave %d liquidations", got.Liquidations)
	}
	if len(got.Trades) == 0 || !strings.Contains(got.Trades[0].ExitReasonVI, "THANH LÝ") {
		t.Errorf("the trade does not say it was liquidated: %+v", got.Trades)
	}
	// A liquidation is not a chosen exit and must be countable apart from one.
	//
	// And it must name the RIGHT loss. This assertion first read "MẤT VỐN" — a
	// capital loss — which is wrong and was corrected on 2026-09-07: a short is
	// only liquidated when the price RISES, and at that same instant the spot
	// leg holds an unrealized gain of very nearly the margin the perp leg lost
	// (exactly N(f−m)/(1+m) against a margin of f·N). The combined position is
	// still flat. Charging the margin as a loss would deduct it without
	// crediting the spot side, i.e. count the same move twice — which is why
	// the engine charges the round trip and nothing else.
	//
	// What is really gone is the HEDGE: from that instant the position is naked
	// long spot until it can be sold, and no model here prices that window.
	reason := got.Trades[0].ExitReasonVI
	if !strings.Contains(reason, "MẤT HEDGE") {
		t.Errorf("the reason must name the loss of the hedge, not an ordinary exit: %s", reason)
	}
	if strings.Contains(reason, "MẤT VỐN") {
		t.Errorf("a liquidation is not a capital loss to the COMBINED position; "+
			"the spot leg gained what the perp margin lost: %s", reason)
	}
}

// Off is off: with no margin fraction the engine behaves exactly as it did
// before the model existed, whatever the price did.
func TestRun_MarginModelIsOffByDefault(t *testing.T) {
	series, window := marginSeries(t, func(i int, c *exchanges.PriceCandle) {
		c.HighPriceQuote = 1000 // a tenfold spike in every bar
	})
	got := Run(series, window, testParams())
	if got.Liquidations != 0 {
		t.Errorf("the margin model fired with no margin fraction configured: %d liquidations", got.Liquidations)
	}
	if containsAny(got.AssumptionsVI, "MÔ HÌNH KÝ QUỸ") {
		t.Error("an unconfigured run must not claim a margin model")
	}
}

// A run that models margin has to say so, and say what it does NOT model:
// omitting the exclusions is how a result gets quoted as if the liquidation
// risk were fully priced.
func TestRun_AMarginRunNamesWhatItDoesNotModel(t *testing.T) {
	series, window := marginSeries(t, nil)
	got := Run(series, window, marginParams())
	// "NOTIONAL" and "trần" are load-bearing: every return this engine reports
	// is a fraction of notional, and notional does not change when leverage is
	// switched on — so the figures measure what leverage COSTS and can never
	// measure what it buys. The benefit lives in the capital, where the spot
	// leg is unleveraged and the same size, so the whole ceiling is 2.00x.
	// A reader who takes the reported APR as the answer to "does leverage help"
	// is reading the wrong denominator; the assumptions block has to say so.
	for _, want := range []string{
		"MÔ HÌNH KÝ QUỸ", "ĐỈNH nến", "CHƯA mô hình hoá", "funding đã thu",
		"TRẦN TRỤI", "NOTIONAL", "trần",
	} {
		if !containsAny(got.AssumptionsVI, want) {
			t.Errorf("the assumptions block is missing %q:\n%s", want, strings.Join(got.AssumptionsVI, "\n"))
		}
	}
}

// An unverified bracket cannot state a liquidation price. The run must say so
// loudly rather than silently never liquidating anything.
func TestRun_AnUnverifiedBracketIsNamedInTheAssumptions(t *testing.T) {
	series, window := marginSeries(t, nil)
	series.PerpMargin = risk.Bracket{Source: "binance_futures"} // never looked up
	got := Run(series, window, marginParams())
	if !containsAny(got.AssumptionsVI, "CHƯA XÁC MINH") {
		t.Errorf("an unverified bracket was not named:\n%s", strings.Join(got.AssumptionsVI, "\n"))
	}
}

// The scan must not re-read the whole holding period at every settlement: that
// is O(n²) on the 8,760-settlement hourly venues, the same defect UsableSettled
// had. A spike before the position opened must also not liquidate it.
func TestRun_ASpikeBeforeTheEntryDoesNotLiquidate(t *testing.T) {
	series, window := marginSeries(t, func(i int, c *exchanges.PriceCandle) {
		if i < 4 { // well before entry, which needs three settlements of persistence
			c.HighPriceQuote = 1000
		}
	})
	got := Run(series, window, marginParams())
	if got.Liquidations != 0 {
		t.Errorf("a spike before the position existed liquidated it: %d", got.Liquidations)
	}
}

// The denominator that answers "does leverage help".
//
// This test exists because the leverage sweep of 2026-09-07 was read off
// TotalReturnFrac and concluded "leverage is monotone bad, no optimum in the
// middle". That reading was wrong: notional is constant across a leverage
// sweep, so those figures capture leverage's cost (the extra round trips its
// liquidations force) and structurally cannot capture its benefit, which is
// entirely in the capital.
//
// The capital is what pins the answer down. A delta-neutral position needs
// BOTH legs at full size and the spot leg cannot be levered — buying N of coin
// costs N — so capital is N·(1+f) and the whole ceiling is 2/(1+f), which
// approaches 2.00x and never reaches 10x however high the leverage goes.
func TestRun_CapitalDenominatorIsBothLegsNotJustThePerpMargin(t *testing.T) {
	for _, tc := range []struct {
		nameVI      string
		marginFrac  float64
		wantCapital float64
		wantCeiling float64
	}{
		// Off is not "free": an unlevered short posts its full notional, so the
		// position ties up 2N and the ceiling on improving that is 1.00x.
		{"tắt", 0, 2.00, 1.00},
		{"2x", 0.5, 1.50, 4.0 / 3.0},
		{"10x", 0.1, 1.10, 2.0 / 1.1},
		{"20x", 0.05, 1.05, 2.0 / 1.05},
	} {
		t.Run(tc.nameVI, func(t *testing.T) {
			series, window := marginSeries(t, nil)
			p := marginParams()
			p.PerpMarginFrac = tc.marginFrac
			got := Run(series, window, p)
			if !got.OK {
				t.Fatalf("Run: %s", got.ReasonVI)
			}
			if math.Abs(got.CapitalPerNotional-tc.wantCapital) > 1e-9 {
				t.Errorf("CapitalPerNotional = %v, want %v — the spot leg is unlevered and the "+
					"same size as the perp, so capital is N·(1+f), never f·N alone",
					got.CapitalPerNotional, tc.wantCapital)
			}
			if ceiling := 2 / got.CapitalPerNotional; math.Abs(ceiling-tc.wantCeiling) > 1e-9 {
				t.Errorf("capital-efficiency ceiling = %.4fx, want %.4fx", ceiling, tc.wantCeiling)
			}
			// The two denominators must not be confused for one another.
			want := got.TotalReturnFrac / tc.wantCapital
			if math.Abs(got.TotalReturnOnCapitalFrac-want) > 1e-12 {
				t.Errorf("TotalReturnOnCapitalFrac = %v, want %v", got.TotalReturnOnCapitalFrac, want)
			}
			if got.RealizedAPRFrac != 0 &&
				math.Abs(got.RealizedAPROnCapitalFrac-got.RealizedAPRFrac/tc.wantCapital) > 1e-12 {
				t.Errorf("RealizedAPROnCapitalFrac = %v, want %v",
					got.RealizedAPROnCapitalFrac, got.RealizedAPRFrac/tc.wantCapital)
			}
		})
	}
}

// Twenty times the leverage does not buy twenty times the capital efficiency,
// and the gap is not a detail — it is the reason the whole question has a
// boring answer. One leg of two can at most be removed from the capital.
func TestRun_LeverageCannotBuyMoreThanHalfTheCapitalBack(t *testing.T) {
	series, window := marginSeries(t, nil)
	base := marginParams()
	base.PerpMarginFrac = 0
	unlevered := Run(series, window, base)

	for _, frac := range []float64{0.5, 0.2, 0.1, 0.05, 0.01} {
		p := marginParams()
		p.PerpMarginFrac = frac
		got := Run(series, window, p)
		if !got.OK {
			t.Fatalf("Run at f=%v: %s", frac, got.ReasonVI)
		}
		if ratio := unlevered.CapitalPerNotional / got.CapitalPerNotional; ratio >= 2 {
			t.Errorf("f=%v claims %.3fx capital efficiency; levering ONE of two equal legs "+
				"cannot reach 2x, so the model has stopped charging for the spot leg",
				frac, ratio)
		}
	}
}
