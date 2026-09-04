package binance

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestBinanceFundingHistoryGolden(t *testing.T) {
	symbol := exchangestest.BTCSymbol(t, "binance_futures")
	var raw []binanceFundingHistoryRow
	exchangestest.LoadJSON(t, "funding_history_binance.json", &raw)

	rows, err := parseBinanceFundingHistory(raw, symbol, exchangestest.WideWindow)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := exchanges.FinishFundingHistory("binance_futures", symbol, exchanges.FundingDiscrete, rows)
	if err != nil {
		t.Fatal(err)
	}
	exchangestest.AssertHistorySane(t, entries, "binance_futures", exchanges.FundingDiscrete)

	// rateType is published on this endpoint and nowhere else in the codebase.
	// It is what a phase-3 backtest filters "Special" on, so a parser that
	// dropped it would leave that filter with nothing to read.
	for i, entry := range entries {
		if entry.RateType == "" {
			t.Fatalf("entry %d carries no RateType; the recording has one", i)
		}
		if entry.RawRateField != "fundingRate" {
			t.Errorf("entry %d: RawRateField = %q", i, entry.RawRateField)
		}
		if entry.MarkPriceQuote <= 0 {
			t.Errorf("entry %d: MarkPriceQuote = %g, but this venue publishes one", i, entry.MarkPriceQuote)
		}
	}
}

func TestFundingWindowFiltersRows(t *testing.T) {
	symbol := exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"}
	raw := []binanceFundingHistoryRow{
		{Symbol: "BTCUSDT", FundingTime: 1000, FundingRate: "0.0001", MarkPrice: "1", RateType: "Regular"},
		{Symbol: "BTCUSDT", FundingTime: 2000, FundingRate: "0.0001", MarkPrice: "1", RateType: "Regular"},
		{Symbol: "BTCUSDT", FundingTime: 3000, FundingRate: "0.0001", MarkPrice: "1", RateType: "Regular"},
		{Symbol: "ETHUSDT", FundingTime: 2500, FundingRate: "0.0001", MarkPrice: "1", RateType: "Regular"},
	}
	rows, err := parseBinanceFundingHistory(raw, symbol, exchanges.FundingWindow{StartMs: 2000, EndMs: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SettledAtMs != 2000 {
		t.Fatalf("got %+v, want the single row at 2000 — the window is closed-open and another symbol's row is not ours", rows)
	}
}
