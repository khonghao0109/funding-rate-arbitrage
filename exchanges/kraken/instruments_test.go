package kraken

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestParseKrakenInstruments_Golden(t *testing.T) {
	var resp krakenInstrumentsResponse
	exchangestest.LoadJSON(t, "instruments_kraken_futures.json", &resp)
	got, err := parseKrakenInstruments(resp, "kraken_futures",
		[]exchanges.Symbol{{Standard: "BTCUSDT", Venue: "PF_XBTUSD"}, {Standard: "XRPUSDT", Venue: "PF_XRPUSD"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d instruments, want 2", len(got))
	}
	// contractSize 1 BTC, precision 4 → 0.0001-BTC steps; initialMargin 1% →
	// 100×. MinQtyCoin stays 0: Kraken publishes no minimum order size.
	exchangestest.AssertInstrument(t, got[0], exchanges.Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "PF_XBTUSD", Source: "kraken_futures",
		MarketType: "perp", Status: exchanges.StatusTrading, BaseAsset: "BTC", QuoteAsset: "USD",
		TickSizeQuote: 1, StepSizeCoin: 0.0001,
		IsContract: true, ContractSizeCoin: 1, MaxLeverageX: 100,
	})
	// precision 0 → WHOLE 1-XRP contracts; initialMargin 2% → 50×.
	exchangestest.AssertInstrument(t, got[1], exchanges.Instrument{
		Symbol: "XRPUSDT", NativeSymbol: "PF_XRPUSD", Source: "kraken_futures",
		MarketType: "perp", Status: exchanges.StatusTrading, BaseAsset: "XRP", QuoteAsset: "USD",
		TickSizeQuote: 0.0001, StepSizeCoin: 1,
		IsContract: true, ContractSizeCoin: 1, MaxLeverageX: 50,
	})
}

// A symbol_map typo pointing PF_XBTUSD's slot at a dated future must go
// ABSENT (so Refresh names it), not sail through stamped "perp" — Kraken's
// list carries every product family and the fetcher used to stamp perp
// unconditionally. flexible_futures is the one type a PF_ perpetual carries
// (measured 2026-09-03, all four configured markets).
func TestParseKrakenInstruments_SkipsNonPerpetualTypes(t *testing.T) {
	var resp krakenInstrumentsResponse
	exchangestest.LoadJSON(t, "instruments_kraken_futures.json", &resp)
	resp.Instruments[0].Type = "futures_inverse" // a dated/inverse product under the wanted symbol

	got, err := parseKrakenInstruments(resp, "kraken_futures",
		[]exchanges.Symbol{{Standard: "BTCUSDT", Venue: resp.Instruments[0].Symbol}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a %q product parsed as %d instrument(s), want absent", "futures_inverse", len(got))
	}
}
