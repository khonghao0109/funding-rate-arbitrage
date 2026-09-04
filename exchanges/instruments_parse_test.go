package exchanges

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// Golden tests: every instrument parser against the real venue responses
// recorded 2026-09-03 (testdata/instruments_*.json, re-record with
// CAPTURE_TESTDATA=1). The expected numbers were read off the venues
// independently during the step-2.3 survey — the test pins that the parser
// reproduces them, unit conversions included.

func loadInstrumentTestdata(t *testing.T, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("missing recording: %v", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("%s does not decode: %v", name, err)
	}
}

func assertInstrument(t *testing.T, got, want Instrument) {
	t.Helper()
	if got.Symbol != want.Symbol || got.NativeSymbol != want.NativeSymbol ||
		got.Source != want.Source || got.MarketType != want.MarketType || got.Status != want.Status ||
		got.BaseAsset != want.BaseAsset || got.QuoteAsset != want.QuoteAsset {
		t.Errorf("identity = %+v,\nwant %+v", got, want)
		return
	}
	fields := []struct {
		name      string
		got, want float64
	}{
		{"TickSizeQuote", got.TickSizeQuote, want.TickSizeQuote},
		{"StepSizeCoin", got.StepSizeCoin, want.StepSizeCoin},
		{"MinQtyCoin", got.MinQtyCoin, want.MinQtyCoin},
		{"MaxQtyCoin", got.MaxQtyCoin, want.MaxQtyCoin},
		{"MinNotionalQuote", got.MinNotionalQuote, want.MinNotionalQuote},
		{"ContractSizeCoin", got.ContractSizeCoin, want.ContractSizeCoin},
		{"MaxLeverageX", got.MaxLeverageX, want.MaxLeverageX},
	}
	for _, f := range fields {
		if math.Abs(f.got-f.want) > 1e-12 {
			t.Errorf("%s %s: %s = %v, want %v", got.Source, got.NativeSymbol, f.name, f.got, f.want)
		}
	}
	if got.IsContract != want.IsContract {
		t.Errorf("%s %s: IsContract = %v, want %v", got.Source, got.NativeSymbol, got.IsContract, want.IsContract)
	}
}

func TestParseBinanceInstruments_Golden(t *testing.T) {
	var futures binanceExchangeInfo
	loadInstrumentTestdata(t, "instruments_binance_futures.json", &futures)
	got, err := parseBinanceInstruments(futures, "binance_futures", "perp",
		[]Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}, {Standard: "XRPUSDT", Venue: "XRPUSDT"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d instruments, want 2", len(got))
	}
	// MaxQtyCoin is the SMALLER of LOT_SIZE.maxQty (limit cap) and
	// MARKET_LOT_SIZE.maxQty (market cap) — in the recording BTCUSDT is
	// 1000 vs 120 and XRPUSDT 10,000,000 vs 2,000,000: the market cap wins,
	// and it is the one the taker fills of this strategy are governed by.
	assertInstrument(t, got[0], Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTCUSDT", Source: "binance_futures",
		MarketType: "perp", Status: StatusTrading, BaseAsset: "BTC", QuoteAsset: "USDT",
		TickSizeQuote: 0.10, StepSizeCoin: 0.001, MinQtyCoin: 0.001, MaxQtyCoin: 120,
		MinNotionalQuote: 50, ContractSizeCoin: 1,
	})
	assertInstrument(t, got[1], Instrument{
		Symbol: "XRPUSDT", NativeSymbol: "XRPUSDT", Source: "binance_futures",
		MarketType: "perp", Status: StatusTrading, BaseAsset: "XRP", QuoteAsset: "USDT",
		TickSizeQuote: 0.0001, StepSizeCoin: 0.1, MinQtyCoin: 0.1, MaxQtyCoin: 2_000_000,
		MinNotionalQuote: 5, ContractSizeCoin: 1,
	})

	var spot binanceExchangeInfo
	loadInstrumentTestdata(t, "instruments_binance_spot.json", &spot)
	gotSpot, err := parseBinanceInstruments(spot, "binance_spot", "spot",
		[]Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotSpot) != 1 {
		t.Fatalf("parsed %d spot instruments, want 1", len(gotSpot))
	}
	// Spot's MARKET_LOT_SIZE cap is dynamic (a rolling average the venue
	// updates); the value here is whatever the recording froze, and far
	// tighter than LOT_SIZE's 9000.
	assertInstrument(t, gotSpot[0], Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTCUSDT", Source: "binance_spot",
		MarketType: "spot", Status: StatusTrading, BaseAsset: "BTC", QuoteAsset: "USDT",
		TickSizeQuote: 0.01, StepSizeCoin: 0.00001, MinQtyCoin: 0.00001, MaxQtyCoin: 149.14262137,
		MinNotionalQuote: 5, ContractSizeCoin: 1,
	})
}

func TestParseBybitInstruments_Golden(t *testing.T) {
	var linear bybitInstrumentsResponse
	loadInstrumentTestdata(t, "instruments_bybit_futures.json", &linear)
	got, ok, err := parseBybitInstrument(linear, "bybit_futures", "perp", Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"})
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	assertInstrument(t, got, Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTCUSDT", Source: "bybit_futures",
		MarketType: "perp", Status: StatusTrading, BaseAsset: "BTC", QuoteAsset: "USDT",
		TickSizeQuote: 0.10, StepSizeCoin: 0.001, MinQtyCoin: 0.001, MaxQtyCoin: 1500,
		MinNotionalQuote: 5, ContractSizeCoin: 1, MaxLeverageX: 150,
	})

	var spot bybitInstrumentsResponse
	loadInstrumentTestdata(t, "instruments_bybit_spot.json", &spot)
	gotSpot, ok, err := parseBybitInstrument(spot, "bybit_spot", "spot", Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"})
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	assertInstrument(t, gotSpot, Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTCUSDT", Source: "bybit_spot",
		MarketType: "spot", Status: StatusTrading, BaseAsset: "BTC", QuoteAsset: "USDT",
		TickSizeQuote: 0.1, StepSizeCoin: 0.000001, MinQtyCoin: 0.000001, MaxQtyCoin: 230,
		MinNotionalQuote: 5, ContractSizeCoin: 1,
	})
}

func TestParseOKXInstrument_Golden(t *testing.T) {
	var resp okxInstrumentsResponse
	loadInstrumentTestdata(t, "instruments_okx_futures.json", &resp)
	got, ok, err := parseOKXInstrument(resp, "okx_futures", Symbol{Standard: "BTCUSDT", Venue: "BTC-USDT-SWAP"})
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	// lotSz 0.01 ct × (ctVal 0.01 BTC × ctMult 1) = 0.0001 BTC per step —
	// trap: lotSz alone looks like a plausible coin step and is 100× off.
	// MaxQtyCoin = min(maxLmtSz 100,000,000, maxMktSz 35,000) contracts
	// × 0.01 BTC = 350 BTC: the market-order ceiling, which the strategy's
	// taker fills are governed by.
	assertInstrument(t, got, Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTC-USDT-SWAP", Source: "okx_futures",
		MarketType: "perp", Status: StatusTrading, BaseAsset: "BTC", QuoteAsset: "USDT",
		TickSizeQuote: 0.1, StepSizeCoin: 0.0001, MinQtyCoin: 0.0001, MaxQtyCoin: 350,
		IsContract: true, ContractSizeCoin: 0.01, MaxLeverageX: 100,
	})
}

func TestParseGateInstrument_Golden(t *testing.T) {
	var resp gateContractResponse
	loadInstrumentTestdata(t, "instruments_gate_futures.json", &resp)
	got, err := parseGateInstrument(resp, "gate_futures", Symbol{Standard: "BTCUSDT", Venue: "BTC_USDT"})
	if err != nil {
		t.Fatal(err)
	}
	assertInstrument(t, got, Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTC_USDT", Source: "gate_futures",
		MarketType: "perp", Status: StatusTrading, BaseAsset: "BTC", QuoteAsset: "USDT",
		TickSizeQuote: 0.1, StepSizeCoin: 0.0001, MinQtyCoin: 0.0001, MaxQtyCoin: 1200,
		IsContract: true, ContractSizeCoin: 0.0001, MaxLeverageX: 200,
	})
}

func TestParseKrakenInstruments_Golden(t *testing.T) {
	var resp krakenInstrumentsResponse
	loadInstrumentTestdata(t, "instruments_kraken_futures.json", &resp)
	got, err := parseKrakenInstruments(resp, "kraken_futures",
		[]Symbol{{Standard: "BTCUSDT", Venue: "PF_XBTUSD"}, {Standard: "XRPUSDT", Venue: "PF_XRPUSD"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d instruments, want 2", len(got))
	}
	// contractSize 1 BTC, precision 4 → 0.0001-BTC steps; initialMargin 1% →
	// 100×. MinQtyCoin stays 0: Kraken publishes no minimum order size.
	assertInstrument(t, got[0], Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "PF_XBTUSD", Source: "kraken_futures",
		MarketType: "perp", Status: StatusTrading, BaseAsset: "BTC", QuoteAsset: "USD",
		TickSizeQuote: 1, StepSizeCoin: 0.0001,
		IsContract: true, ContractSizeCoin: 1, MaxLeverageX: 100,
	})
	// precision 0 → WHOLE 1-XRP contracts; initialMargin 2% → 50×.
	assertInstrument(t, got[1], Instrument{
		Symbol: "XRPUSDT", NativeSymbol: "PF_XRPUSD", Source: "kraken_futures",
		MarketType: "perp", Status: StatusTrading, BaseAsset: "XRP", QuoteAsset: "USD",
		TickSizeQuote: 0.0001, StepSizeCoin: 1,
		IsContract: true, ContractSizeCoin: 1, MaxLeverageX: 50,
	})
}

func TestParseHyperliquidInstruments_Golden(t *testing.T) {
	var resp hyperliquidMetaResponse
	loadInstrumentTestdata(t, "instruments_hyperliquid_futures.json", &resp)
	got, err := parseHyperliquidInstruments(resp, "hyperliquid_futures",
		[]Symbol{{Standard: "BTCUSDT", Venue: "BTC"}, {Standard: "XRPUSDT", Venue: "XRP"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d instruments, want 2", len(got))
	}
	assertInstrument(t, got[0], Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTC", Source: "hyperliquid_futures",
		MarketType: "perp", Status: StatusTrading, BaseAsset: "BTC", QuoteAsset: "USD",
		TickSizeQuote: 0, StepSizeCoin: 0.00001, MinQtyCoin: 0.00001,
		MinNotionalQuote: 10, ContractSizeCoin: 1, MaxLeverageX: 40,
	})
	// szDecimals 0 → whole-XRP steps.
	assertInstrument(t, got[1], Instrument{
		Symbol: "XRPUSDT", NativeSymbol: "XRP", Source: "hyperliquid_futures",
		MarketType: "perp", Status: StatusTrading, BaseAsset: "XRP", QuoteAsset: "USD",
		TickSizeQuote: 0, StepSizeCoin: 1, MinQtyCoin: 1,
		MinNotionalQuote: 10, ContractSizeCoin: 1, MaxLeverageX: 20,
	})
}

func TestParseParadexInstrument_Golden(t *testing.T) {
	var resp paradexMarketsResponse
	loadInstrumentTestdata(t, "instruments_paradex_futures.json", &resp)
	got, ok, err := parseParadexInstrument(resp, "paradex_futures", Symbol{Standard: "BTCUSDT", Venue: "BTC-USD-PERP"})
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	// MinQtyCoin stays 0: Paradex publishes no separate minimum size.
	assertInstrument(t, got, Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTC-USD-PERP", Source: "paradex_futures",
		MarketType: "perp", Status: StatusTrading, BaseAsset: "BTC", QuoteAsset: "USD",
		TickSizeQuote: 0.1, StepSizeCoin: 0.00001, MaxQtyCoin: 100,
		MinNotionalQuote: 10, ContractSizeCoin: 1,
	})
}

// Every tradable connector must have an instrument fetcher and vice versa —
// the same two-way agreement cmd/scanner enforces between config and
// Connectors(). Pyth is the deliberate exception: an oracle has no
// instruments.
func TestInstrumentFetchersMatchConnectors(t *testing.T) {
	connectors := Connectors()
	fetchers := InstrumentFetchers()
	for name := range connectors {
		if name == "pyth" {
			continue
		}
		if _, ok := fetchers[name]; !ok {
			t.Errorf("connector %q has no instrument fetcher — its trading rules would stay unknown", name)
		}
	}
	for name := range fetchers {
		if _, ok := connectors[name]; !ok {
			t.Errorf("instrument fetcher %q names a connector that does not exist", name)
		}
	}
}

// A symbol the venue does not list must come back absent, not as an error and
// never as a zero-valued instrument a consumer might trust.
func TestParseInstruments_UnlistedSymbolIsAbsent(t *testing.T) {
	var okx okxInstrumentsResponse // empty data
	if _, ok, err := parseOKXInstrument(okx, "okx_futures", Symbol{Standard: "DOGEUSDT", Venue: "DOGE-USDT-SWAP"}); ok || err != nil {
		t.Fatalf("empty OKX response: ok=%v err=%v, want absent without error", ok, err)
	}
	var bybit bybitInstrumentsResponse
	if _, ok, err := parseBybitInstrument(bybit, "bybit_futures", "perp", Symbol{Standard: "DOGEUSDT", Venue: "DOGEUSDT"}); ok || err != nil {
		t.Fatalf("empty Bybit response: ok=%v err=%v, want absent without error", ok, err)
	}
	var paradex paradexMarketsResponse
	if _, ok, err := parseParadexInstrument(paradex, "paradex_futures", Symbol{Standard: "DOGEUSDT", Venue: "DOGE-USD-PERP"}); ok || err != nil {
		t.Fatalf("empty Paradex response: ok=%v err=%v, want absent without error", ok, err)
	}

	// The wrapped "not listed" shapes, measured live 2026-09-03: OKX answers
	// HTTP 200 + code 51001, Bybit linear answers HTTP 200 + retCode 10001
	// "symbol invalid" (its spot category uses the empty list above instead).
	// Both must read as absent — one unsupported pair must not blank a whole
	// source — while every OTHER venue error stays loud.
	okx.Code, okx.Msg = "51001", "Instrument ID doesn't exist."
	if _, ok, err := parseOKXInstrument(okx, "okx_futures", Symbol{Standard: "XLMUSDT", Venue: "XLM-USDT-SWAP"}); ok || err != nil {
		t.Fatalf("OKX code 51001: ok=%v err=%v, want absent without error", ok, err)
	}
	okx.Code, okx.Msg = "50011", "rate limited"
	if _, _, err := parseOKXInstrument(okx, "okx_futures", Symbol{Standard: "XLMUSDT", Venue: "XLM-USDT-SWAP"}); err == nil {
		t.Fatal("OKX code 50011 must stay an error, not read as absent")
	}
	// retMsg is prose, not contract: the match must survive Bybit's other
	// spellings of the same thing (it uses title case elsewhere in v5).
	for _, msg := range []string{
		"params error: symbol invalid",
		"params error: Symbol Is Invalid",
		"symbol not exist",
	} {
		bybit.RetCode, bybit.RetMsg = 10001, msg
		if _, ok, err := parseBybitInstrument(bybit, "bybit_futures", "perp", Symbol{Standard: "XLMUSDT", Venue: "XLMUSDT"}); ok || err != nil {
			t.Fatalf("Bybit 10001 %q: ok=%v err=%v, want absent without error", msg, ok, err)
		}
	}
	bybit.RetCode, bybit.RetMsg = 10001, "params error: category invalid"
	if _, _, err := parseBybitInstrument(bybit, "bybit_futures", "perp", Symbol{Standard: "XLMUSDT", Venue: "XLMUSDT"}); err == nil {
		t.Fatal("a non-symbol Bybit 10001 must stay an error, not read as absent")
	}
}

// A symbol_map typo pointing PF_XBTUSD's slot at a dated future must go
// ABSENT (so Refresh names it), not sail through stamped "perp" — Kraken's
// list carries every product family and the fetcher used to stamp perp
// unconditionally. flexible_futures is the one type a PF_ perpetual carries
// (measured 2026-09-03, all four configured markets).
func TestParseKrakenInstruments_SkipsNonPerpetualTypes(t *testing.T) {
	var resp krakenInstrumentsResponse
	loadInstrumentTestdata(t, "instruments_kraken_futures.json", &resp)
	resp.Instruments[0].Type = "futures_inverse" // a dated/inverse product under the wanted symbol

	got, err := parseKrakenInstruments(resp, "kraken_futures",
		[]Symbol{{Standard: "BTCUSDT", Venue: resp.Instruments[0].Symbol}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a %q product parsed as %d instrument(s), want absent", "futures_inverse", len(got))
	}
}

func TestSmallerPositiveCap(t *testing.T) {
	cases := []struct{ a, b, want float64 }{
		{0, 0, 0},     // neither stated → stays "not stated", never invented
		{0, 120, 120}, // 0 must not win over a real cap
		{1000, 0, 1000},
		{1000, 120, 120}, // the tighter ceiling governs
		{120, 1000, 120},
	}
	for _, c := range cases {
		if got := smallerPositiveCap(c.a, c.b); got != c.want {
			t.Errorf("smallerPositiveCap(%g, %g) = %g, want %g", c.a, c.b, got, c.want)
		}
	}
}
