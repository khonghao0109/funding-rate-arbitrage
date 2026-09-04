package okx

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestParseOKXInstrument_Golden(t *testing.T) {
	var resp okxInstrumentsResponse
	exchangestest.LoadJSON(t, "instruments_okx_futures.json", &resp)
	got, ok, err := parseOKXInstrument(resp, "okx_futures", exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTC-USDT-SWAP"})
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	// lotSz 0.01 ct × (ctVal 0.01 BTC × ctMult 1) = 0.0001 BTC per step —
	// trap: lotSz alone looks like a plausible coin step and is 100× off.
	// MaxQtyCoin = min(maxLmtSz 100,000,000, maxMktSz 35,000) contracts
	// × 0.01 BTC = 350 BTC: the market-order ceiling, which the strategy's
	// taker fills are governed by.
	exchangestest.AssertInstrument(t, got, exchanges.Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTC-USDT-SWAP", Source: "okx_futures",
		MarketType: "perp", Status: exchanges.StatusTrading, BaseAsset: "BTC", QuoteAsset: "USDT",
		TickSizeQuote: 0.1, StepSizeCoin: 0.0001, MinQtyCoin: 0.0001, MaxQtyCoin: 350,
		IsContract: true, ContractSizeCoin: 0.01, MaxLeverageX: 100,
	})
}

// A symbol the venue does not list must come back absent, not as an error and
// never as a zero-valued instrument a consumer might trust. OKX has two "not
// listed" shapes, measured live 2026-09-03: an empty data array, and HTTP 200
// + code 51001 — while every OTHER venue error stays loud.
func TestParseInstrument_UnlistedSymbolIsAbsent(t *testing.T) {
	var resp okxInstrumentsResponse // empty data
	if _, ok, err := parseOKXInstrument(resp, "okx_futures", exchanges.Symbol{Standard: "DOGEUSDT", Venue: "DOGE-USDT-SWAP"}); ok || err != nil {
		t.Fatalf("empty response: ok=%v err=%v, want absent without error", ok, err)
	}
	resp.Code, resp.Msg = "51001", "Instrument ID doesn't exist."
	if _, ok, err := parseOKXInstrument(resp, "okx_futures", exchanges.Symbol{Standard: "XLMUSDT", Venue: "XLM-USDT-SWAP"}); ok || err != nil {
		t.Fatalf("code 51001: ok=%v err=%v, want absent without error", ok, err)
	}
	resp.Code, resp.Msg = "50011", "rate limited"
	if _, _, err := parseOKXInstrument(resp, "okx_futures", exchanges.Symbol{Standard: "XLMUSDT", Venue: "XLM-USDT-SWAP"}); err == nil {
		t.Fatal("code 50011 must stay an error, not read as absent")
	}
}
