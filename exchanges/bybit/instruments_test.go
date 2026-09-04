package bybit

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestParseBybitInstruments_Golden(t *testing.T) {
	var linear bybitInstrumentsResponse
	exchangestest.LoadJSON(t, "instruments_bybit_futures.json", &linear)
	got, ok, err := parseBybitInstrument(linear, "bybit_futures", "perp", exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"})
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	exchangestest.AssertInstrument(t, got, exchanges.Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTCUSDT", Source: "bybit_futures",
		MarketType: "perp", Status: exchanges.StatusTrading, BaseAsset: "BTC", QuoteAsset: "USDT",
		TickSizeQuote: 0.10, StepSizeCoin: 0.001, MinQtyCoin: 0.001, MaxQtyCoin: 1500,
		MinNotionalQuote: 5, ContractSizeCoin: 1, MaxLeverageX: 150,
	})

	var spot bybitInstrumentsResponse
	exchangestest.LoadJSON(t, "instruments_bybit_spot.json", &spot)
	gotSpot, ok, err := parseBybitInstrument(spot, "bybit_spot", "spot", exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"})
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	exchangestest.AssertInstrument(t, gotSpot, exchanges.Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTCUSDT", Source: "bybit_spot",
		MarketType: "spot", Status: exchanges.StatusTrading, BaseAsset: "BTC", QuoteAsset: "USDT",
		TickSizeQuote: 0.1, StepSizeCoin: 0.000001, MinQtyCoin: 0.000001, MaxQtyCoin: 230,
		MinNotionalQuote: 5, ContractSizeCoin: 1,
	})
}

// A symbol the venue does not list must come back absent: Bybit linear answers
// HTTP 200 + retCode 10001 "symbol invalid" (its spot category uses an empty
// list instead), measured live 2026-09-03. One unsupported pair must not blank
// a whole source — while a non-symbol 10001 stays a loud error.
func TestParseInstrument_UnlistedSymbolIsAbsent(t *testing.T) {
	var resp bybitInstrumentsResponse // empty list — the spot shape
	if _, ok, err := parseBybitInstrument(resp, "bybit_futures", "perp", exchanges.Symbol{Standard: "DOGEUSDT", Venue: "DOGEUSDT"}); ok || err != nil {
		t.Fatalf("empty response: ok=%v err=%v, want absent without error", ok, err)
	}
	// retMsg is prose, not contract: the match must survive Bybit's other
	// spellings of the same thing (it uses title case elsewhere in v5).
	for _, msg := range []string{
		"params error: symbol invalid",
		"params error: Symbol Is Invalid",
		"symbol not exist",
	} {
		resp.RetCode, resp.RetMsg = 10001, msg
		if _, ok, err := parseBybitInstrument(resp, "bybit_futures", "perp", exchanges.Symbol{Standard: "XLMUSDT", Venue: "XLMUSDT"}); ok || err != nil {
			t.Fatalf("10001 %q: ok=%v err=%v, want absent without error", msg, ok, err)
		}
	}
	resp.RetCode, resp.RetMsg = 10001, "params error: category invalid"
	if _, _, err := parseBybitInstrument(resp, "bybit_futures", "perp", exchanges.Symbol{Standard: "XLMUSDT", Venue: "XLMUSDT"}); err == nil {
		t.Fatal("a non-symbol 10001 must stay an error, not read as absent")
	}
}
