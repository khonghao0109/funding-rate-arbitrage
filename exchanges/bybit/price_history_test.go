package bybit

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// Bybit sends its list NEWEST FIRST. AssertCandlesSane checks the parser's
// output is oldest-first, so this recording is what proves the reversal is
// actually happening rather than the fixture happening to be sorted.
func TestBybitKlinesGolden(t *testing.T) {
	for _, tc := range []struct{ source, file string }{
		{"bybit_futures", "price_history_bybit_futures.json"},
		{"bybit_spot", "price_history_bybit_spot.json"},
	} {
		t.Run(tc.source, func(t *testing.T) {
			symbol := exchangestest.BTCSymbol(t, tc.source)
			var resp bybitKlineResponse
			exchangestest.LoadJSON(t, tc.file, &resp)
			if resp.RetCode != 0 {
				t.Fatalf("the recording carries retCode %d %q", resp.RetCode, resp.RetMsg)
			}
			if len(resp.Result.List) < 2 {
				t.Fatalf("the recording has %d candles", len(resp.Result.List))
			}
			// The premise of the backwards pagination: element 0 is the NEWEST.
			if resp.Result.List[0][0] <= resp.Result.List[len(resp.Result.List)-1][0] {
				t.Error("this recording is not newest-first; the fetcher's paging assumes it is")
			}

			rows, err := parseBybitKlines(resp.Result.List, tc.source, exchangestest.WidePriceWindow)
			if err != nil {
				t.Fatal(err)
			}
			candles, err := exchanges.FinishPriceHistory(tc.source, symbol, rows)
			if err != nil {
				t.Fatal(err)
			}
			exchangestest.AssertCandlesSane(t, candles, tc.source)
			for i, candle := range candles {
				// Index 5 is volume (base); index 6 is turnover (quote), which
				// is larger by roughly the price.
				if candle.BaseVolumeCoin <= 0 || candle.BaseVolumeCoin > 1_000_000 {
					t.Errorf("candle %d volume %g is not a plausible hourly BTC volume — wrong column",
						i, candle.BaseVolumeCoin)
				}
			}
		})
	}
}
