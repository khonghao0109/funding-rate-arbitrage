package paradex

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestParadexFundingHistoryGolden(t *testing.T) {
	symbol := exchangestest.BTCSymbol(t, "paradex_futures")
	var resp paradexFundingHistoryResponse
	exchangestest.LoadJSON(t, "funding_history_paradex.json", &resp)

	rows, err := parseParadexFundingHistory(resp, symbol, exchangestest.WideWindow)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := exchanges.FinishFundingHistory("paradex_futures", symbol, exchanges.FundingContinuous, rows)
	if err != nil {
		t.Fatal(err)
	}
	exchangestest.AssertHistorySane(t, entries, "paradex_futures", exchanges.FundingContinuous)

	// The venue declares the funding period per row, and that declaration is
	// the only thing standing between this and a catastrophic measurement: the
	// samples are five seconds apart, so measuring the interval from their
	// spacing would report a 5-second funding period and an APR 5,760× too
	// large.
	for i, entry := range entries {
		if entry.IntervalSec != 8*exchanges.SecPerHour {
			t.Errorf("entry %d: IntervalSec = %d, want %d from funding_period_hours",
				i, entry.IntervalSec, 8*exchanges.SecPerHour)
		}
		if entry.Model != exchanges.FundingContinuous {
			t.Errorf("entry %d: Model = %q — this venue has no settlements", i, entry.Model)
		}
	}
}

func TestParadexFundingHistoryRefusesMissingPeriod(t *testing.T) {
	var resp paradexFundingHistoryResponse
	resp.Results = append(resp.Results, struct {
		Market             string `json:"market"`
		FundingRate        string `json:"funding_rate"`
		FundingRate8h      string `json:"funding_rate_8h"`
		FundingPeriodHours int64  `json:"funding_period_hours"`
		FundingIndex       string `json:"funding_index"`
		CreatedAt          int64  `json:"created_at"`
	}{Market: "BTC-USD-PERP", FundingRate: "0.00008", FundingPeriodHours: 0, CreatedAt: 1788492012060})

	if _, err := parseParadexFundingHistory(resp, exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTC-USD-PERP"}, exchangestest.WideWindow); err == nil {
		t.Fatal("a row with no funding_period_hours was accepted; the 5s sample spacing would become the interval")
	}
}
