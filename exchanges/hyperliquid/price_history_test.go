package hyperliquid

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// The regression this fixture exists for: the payload carries "t" (open) and
// "T" (close), and Go's case-insensitive JSON fallback let "T" overwrite "t",
// stamping every candle one millisecond before the hour. AssertCandlesSane
// rejects a stamp off the hour, so the decode is pinned here rather than only
// in a comment.
func TestHyperliquidCandlesGolden(t *testing.T) {
	symbol := exchangestest.BTCSymbol(t, "hyperliquid_futures")
	var raw []hyperliquidCandle
	exchangestest.LoadJSON(t, "price_history_hyperliquid.json", &raw)
	if len(raw) < 2 {
		t.Fatalf("the recording has %d candles", len(raw))
	}
	for i, candle := range raw {
		if candle.CloseTimeMs <= candle.OpenTimeMs {
			t.Fatalf("candle %d: close stamp %d is not after open %d — the two keys have been "+
				"decoded into one field again", i, candle.CloseTimeMs, candle.OpenTimeMs)
		}
	}

	rows, err := parseHyperliquidCandles(raw, symbol, "hyperliquid_futures", exchangestest.WidePriceWindow)
	if err != nil {
		t.Fatal(err)
	}
	candles, err := exchanges.FinishPriceHistory("hyperliquid_futures", symbol, rows)
	if err != nil {
		t.Fatal(err)
	}
	exchangestest.AssertCandlesSane(t, candles, "hyperliquid_futures")
	for i, candle := range candles {
		if candle.BaseVolumeCoin <= 0 {
			t.Errorf("candle %d has no volume; this venue publishes one in the base coin", i)
		}
	}
}
