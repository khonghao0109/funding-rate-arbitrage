package hyperliquid

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestParseHyperliquidInstruments_Golden(t *testing.T) {
	var resp hyperliquidMetaResponse
	exchangestest.LoadJSON(t, "instruments_hyperliquid_futures.json", &resp)
	got, err := parseHyperliquidInstruments(resp, "hyperliquid_futures",
		[]exchanges.Symbol{{Standard: "BTCUSDT", Venue: "BTC"}, {Standard: "XRPUSDT", Venue: "XRP"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d instruments, want 2", len(got))
	}
	exchangestest.AssertInstrument(t, got[0], exchanges.Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTC", Source: "hyperliquid_futures",
		MarketType: "perp", Status: exchanges.StatusTrading, BaseAsset: "BTC", QuoteAsset: "USD",
		TickSizeQuote: 0, StepSizeCoin: 0.00001, MinQtyCoin: 0.00001,
		MinNotionalQuote: 10, ContractSizeCoin: 1, MaxLeverageX: 40,
	})
	// szDecimals 0 → whole-XRP steps.
	exchangestest.AssertInstrument(t, got[1], exchanges.Instrument{
		Symbol: "XRPUSDT", NativeSymbol: "XRP", Source: "hyperliquid_futures",
		MarketType: "perp", Status: exchanges.StatusTrading, BaseAsset: "XRP", QuoteAsset: "USD",
		TickSizeQuote: 0, StepSizeCoin: 1, MinQtyCoin: 1,
		MinNotionalQuote: 10, ContractSizeCoin: 1, MaxLeverageX: 20,
	})
}
