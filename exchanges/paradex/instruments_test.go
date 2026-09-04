package paradex

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestParseParadexInstrument_Golden(t *testing.T) {
	var resp paradexMarketsResponse
	exchangestest.LoadJSON(t, "instruments_paradex_futures.json", &resp)
	got, ok, err := parseParadexInstrument(resp, "paradex_futures", exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTC-USD-PERP"})
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	// MinQtyCoin stays 0: Paradex publishes no separate minimum size.
	exchangestest.AssertInstrument(t, got, exchanges.Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTC-USD-PERP", Source: "paradex_futures",
		MarketType: "perp", Status: exchanges.StatusTrading, BaseAsset: "BTC", QuoteAsset: "USD",
		TickSizeQuote: 0.1, StepSizeCoin: 0.00001, MaxQtyCoin: 100,
		MinNotionalQuote: 10, ContractSizeCoin: 1,
	})
}

// A symbol the venue does not list must come back absent, not as an error and
// never as a zero-valued instrument a consumer might trust (Paradex's other
// shape — HTTP 404 — is recognized by the shared transport).
func TestParseInstrument_UnlistedSymbolIsAbsent(t *testing.T) {
	var resp paradexMarketsResponse
	if _, ok, err := parseParadexInstrument(resp, "paradex_futures", exchanges.Symbol{Standard: "DOGEUSDT", Venue: "DOGE-USD-PERP"}); ok || err != nil {
		t.Fatalf("empty response: ok=%v err=%v, want absent without error", ok, err)
	}
}
