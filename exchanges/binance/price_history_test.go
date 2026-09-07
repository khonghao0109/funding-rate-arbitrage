package binance

import (
	"encoding/json"
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// One parser serves both endpoints, so both recordings run through it: the
// spot and futures klines share a shape that is easy to assume and expensive
// to be wrong about.
func TestBinanceKlinesGolden(t *testing.T) {
	for _, tc := range []struct{ source, file string }{
		{"binance_futures", "price_history_binance_futures.json"},
		{"binance_spot", "price_history_binance_spot.json"},
	} {
		t.Run(tc.source, func(t *testing.T) {
			symbol := exchangestest.BTCSymbol(t, tc.source)
			var raw [][]json.RawMessage
			exchangestest.LoadJSON(t, tc.file, &raw)

			rows, err := parseBinanceKlines(raw, tc.source, exchangestest.WidePriceWindow)
			if err != nil {
				t.Fatal(err)
			}
			candles, err := exchanges.FinishPriceHistory(tc.source, symbol, rows)
			if err != nil {
				t.Fatal(err)
			}
			exchangestest.AssertCandlesSane(t, candles, tc.source)

			// Index 5 is the BASE volume on both endpoints. Reading index 7
			// (quote volume) instead would be off by the price — a number that
			// still looks like a volume.
			for i, candle := range candles {
				if candle.BaseVolumeCoin <= 0 {
					t.Errorf("candle %d has no base volume; this endpoint publishes one", i)
				}
				if candle.BaseVolumeCoin > 1_000_000 {
					t.Errorf("candle %d volume %g BTC in one hour — that is the QUOTE column",
						i, candle.BaseVolumeCoin)
				}
			}
		})
	}
}

// The window is applied by the parser, not by the caller: a page straddling the
// requested edge must contribute only the candles inside it.
func TestBinanceKlinesWindowFilters(t *testing.T) {
	var raw [][]json.RawMessage
	exchangestest.LoadJSON(t, "price_history_binance_futures.json", &raw)

	all, err := parseBinanceKlines(raw, "binance_futures", exchangestest.WidePriceWindow)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 3 {
		t.Fatalf("the fixture has %d candles; this test needs at least 3", len(all))
	}
	window := exchanges.PriceWindow{StartMs: all[1].OpenTimeMs, EndMs: all[len(all)-1].OpenTimeMs}
	got, err := parseBinanceKlines(raw, "binance_futures", window)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(all) - 2; len(got) != want {
		t.Errorf("window kept %d candles, want %d (start inclusive, end exclusive)", len(got), want)
	}
}
