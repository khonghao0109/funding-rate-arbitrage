package kraken

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// Kraken takes its request bounds in SECONDS and answers with stamps in
// MILLISECONDS — one asymmetry inside one request/response pair. This recording
// is what pins the response side.
func TestKrakenCandlesGolden(t *testing.T) {
	symbol := exchangestest.BTCSymbol(t, "kraken_futures")
	var resp krakenChartResponse
	exchangestest.LoadJSON(t, "price_history_kraken.json", &resp)
	if len(resp.Candles) < 2 {
		t.Fatalf("the recording has %d candles", len(resp.Candles))
	}
	if resp.Candles[0].Time < 1_500_000_000_000 {
		t.Fatalf("the recording's time = %d looks like seconds; this venue answers in milliseconds "+
			"even though it is ASKED in seconds — re-record before trusting this test", resp.Candles[0].Time)
	}

	rows, err := parseKrakenCandles(resp, "kraken_futures", exchangestest.WidePriceWindow)
	if err != nil {
		t.Fatal(err)
	}
	candles, err := exchanges.FinishPriceHistory("kraken_futures", symbol, rows)
	if err != nil {
		t.Fatal(err)
	}
	exchangestest.AssertCandlesSane(t, candles, "kraken_futures")

	// PF_ contracts are 1 base unit each (settled at step 2.3), so the volume
	// is numerically the coin and IS carried through.
	for i, candle := range candles {
		if candle.BaseVolumeCoin <= 0 {
			t.Errorf("candle %d has no volume; this venue publishes one", i)
		}
	}
}
