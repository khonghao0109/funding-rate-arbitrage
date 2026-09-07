package backtest

import (
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
	if !strings.Contains(got.Trades[0].ExitReasonVI, "MẤT VỐN") {
		t.Error("the reason must say this is a capital loss, not an ordinary exit")
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
	for _, want := range []string{"MÔ HÌNH KÝ QUỸ", "ĐỈNH nến", "CHƯA mô hình hoá", "funding đã thu"} {
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
