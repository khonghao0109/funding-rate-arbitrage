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
		got.Source != want.Source || got.MarketType != want.MarketType || got.Status != want.Status {
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
	assertInstrument(t, got[0], Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTCUSDT", Source: "binance_futures",
		MarketType: "perp", Status: StatusTrading,
		TickSizeQuote: 0.10, StepSizeCoin: 0.001, MinQtyCoin: 0.001, MaxQtyCoin: 1000,
		MinNotionalQuote: 50, ContractSizeCoin: 1,
	})
	assertInstrument(t, got[1], Instrument{
		Symbol: "XRPUSDT", NativeSymbol: "XRPUSDT", Source: "binance_futures",
		MarketType: "perp", Status: StatusTrading,
		TickSizeQuote: 0.0001, StepSizeCoin: 0.1, MinQtyCoin: 0.1, MaxQtyCoin: 10_000_000,
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
	assertInstrument(t, gotSpot[0], Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTCUSDT", Source: "binance_spot",
		MarketType: "spot", Status: StatusTrading,
		TickSizeQuote: 0.01, StepSizeCoin: 0.00001, MinQtyCoin: 0.00001, MaxQtyCoin: 9000,
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
		MarketType: "perp", Status: StatusTrading,
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
		MarketType: "spot", Status: StatusTrading,
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
	assertInstrument(t, got, Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTC-USDT-SWAP", Source: "okx_futures",
		MarketType: "perp", Status: StatusTrading,
		TickSizeQuote: 0.1, StepSizeCoin: 0.0001, MinQtyCoin: 0.0001,
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
		MarketType: "perp", Status: StatusTrading,
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
		MarketType: "perp", Status: StatusTrading,
		TickSizeQuote: 1, StepSizeCoin: 0.0001,
		IsContract: true, ContractSizeCoin: 1, MaxLeverageX: 100,
	})
	// precision 0 → WHOLE 1-XRP contracts; initialMargin 2% → 50×.
	assertInstrument(t, got[1], Instrument{
		Symbol: "XRPUSDT", NativeSymbol: "PF_XRPUSD", Source: "kraken_futures",
		MarketType: "perp", Status: StatusTrading,
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
		MarketType: "perp", Status: StatusTrading,
		TickSizeQuote: 0, StepSizeCoin: 0.00001, MinQtyCoin: 0.00001,
		MinNotionalQuote: 10, ContractSizeCoin: 1, MaxLeverageX: 40,
	})
	// szDecimals 0 → whole-XRP steps.
	assertInstrument(t, got[1], Instrument{
		Symbol: "XRPUSDT", NativeSymbol: "XRP", Source: "hyperliquid_futures",
		MarketType: "perp", Status: StatusTrading,
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
		MarketType: "perp", Status: StatusTrading,
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
}
