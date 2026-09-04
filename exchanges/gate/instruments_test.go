package gate

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestParseGateInstrument_Golden(t *testing.T) {
	var resp gateContractResponse
	exchangestest.LoadJSON(t, "instruments_gate_futures.json", &resp)
	got, err := parseGateInstrument(resp, "gate_futures", exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTC_USDT"})
	if err != nil {
		t.Fatal(err)
	}
	exchangestest.AssertInstrument(t, got, exchanges.Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTC_USDT", Source: "gate_futures",
		MarketType: "perp", Status: exchanges.StatusTrading, BaseAsset: "BTC", QuoteAsset: "USDT",
		TickSizeQuote: 0.1, StepSizeCoin: 0.0001, MinQtyCoin: 0.0001, MaxQtyCoin: 1200,
		IsContract: true, ContractSizeCoin: 0.0001, MaxLeverageX: 200,
	})
}
