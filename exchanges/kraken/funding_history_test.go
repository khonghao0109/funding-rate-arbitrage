package kraken

import (
	"math"
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestKrakenFundingHistoryGolden(t *testing.T) {
	symbol := exchangestest.BTCSymbol(t, "kraken_futures")
	var resp krakenFundingHistoryResponse
	exchangestest.LoadJSON(t, "funding_history_kraken.json", &resp)

	rows, err := parseKrakenFundingHistory(resp, symbol, exchangestest.WideWindow)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := exchanges.FinishFundingHistory("kraken_futures", symbol, exchanges.FundingDiscrete, rows)
	if err != nil {
		t.Fatal(err)
	}
	exchangestest.AssertHistorySane(t, entries, "kraken_futures", exchanges.FundingDiscrete)

	for i, entry := range entries {
		if entry.RawRateField != "relativeFundingRate" {
			t.Fatalf("entry %d reads %q; fundingRate on this venue is an absolute price amount",
				i, entry.RawRateField)
		}
	}

	// THE step-2.3 obligation. kraken_funding.go pins the hourly cadence as a
	// constant because no Kraken funding message carries an interval, and the
	// standing condition on that constant was that every backfill re-measure
	// it. This is the measurement: re-record the fixture and this test says
	// whether the constant is still true.
	report := exchanges.FundingGaps(entries)
	if report.ModalGapSec != exchanges.SecPerHour {
		t.Fatalf("Kraken settles every %ds in the recording, but kraken_funding.go pins %ds — "+
			"the venue changed cadence; gaps observed: %v",
			report.ModalGapSec, exchanges.SecPerHour, report.Counts)
	}
	// The rate itself must be the relative one. The absolute field in the same
	// rows is order 1 (a price amount), so this bound separates them by five
	// orders of magnitude rather than by a field name alone.
	for i, entry := range entries {
		if entry.RatePerIntervalFrac != 0 && math.Abs(entry.RatePerIntervalFrac) > 0.001 {
			t.Errorf("entry %d: rate %g looks like the absolute funding amount, not the relative rate",
				i, entry.RatePerIntervalFrac)
		}
	}
}

func TestKrakenFundingHistoryRefusesMissingRelativeRate(t *testing.T) {
	var resp krakenFundingHistoryResponse
	resp.Result = "success"
	resp.Rates = append(resp.Rates, struct {
		Timestamp           string   `json:"timestamp"`
		FundingRate         float64  `json:"fundingRate"`
		RelativeFundingRate *float64 `json:"relativeFundingRate"`
	}{Timestamp: "2026-09-04T03:00:00Z", FundingRate: 1.81, RelativeFundingRate: nil})

	if _, err := parseKrakenFundingHistory(resp, exchanges.Symbol{Standard: "BTCUSDT", Venue: "PF_XBTUSD"}, exchangestest.WideWindow); err == nil {
		t.Fatal("a row with no relativeFundingRate was accepted; fundingRate would have been used as a rate")
	}
}
