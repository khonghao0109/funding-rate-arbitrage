package okx

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestOKXFundingHistorySeparatesFailureFromAbsence(t *testing.T) {
	symbol := exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTC-USDT-SWAP"}

	// 51001 is the one OKX code that means "instrument does not exist".
	rows, _, err := parseOKXFundingHistory(okxFundingHistoryResponse{
		Code: "51001", Msg: "Instrument ID does not exist",
	}, symbol, exchangestest.WideWindow)
	if err != nil || len(rows) != 0 {
		t.Errorf("51001 = %d rows, %v; want absent and no error", len(rows), err)
	}
	// Everything else stays loud: "system busy" read as absence would truncate
	// the corpus without a word.
	if _, _, err := parseOKXFundingHistory(okxFundingHistoryResponse{
		Code: "50011", Msg: "Requests too frequent",
	}, symbol, exchangestest.WideWindow); err == nil {
		t.Error("a rate-limit response was read as an empty history")
	}
}

func TestOKXFundingHistoryGolden(t *testing.T) {
	symbol := exchangestest.BTCSymbol(t, "okx_futures")
	var resp okxFundingHistoryResponse
	exchangestest.LoadJSON(t, "funding_history_okx.json", &resp)

	rows, oldestMs, err := parseOKXFundingHistory(resp, symbol, exchangestest.WideWindow)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := exchanges.FinishFundingHistory("okx_futures", symbol, exchanges.FundingDiscrete, rows)
	if err != nil {
		t.Fatal(err)
	}
	exchangestest.AssertHistorySane(t, entries, "okx_futures", exchanges.FundingDiscrete)

	if oldestMs != entries[0].SettledAtMs {
		t.Errorf("paging cursor = %d, want the oldest stamp %d", oldestMs, entries[0].SettledAtMs)
	}
	// realizedRate is the rate actually charged and is what the recording
	// carries; falling back to fundingRate is for rows that state no realized
	// one, not the normal path.
	for i, entry := range entries {
		if entry.RawRateField != "realizedRate" {
			t.Errorf("entry %d: RawRateField = %q, want realizedRate", i, entry.RawRateField)
		}
	}
}

func TestOKXFundingHistoryFallsBackToFundingRate(t *testing.T) {
	// An empty realizedRate must never parse as 0 — that would record a
	// settlement that paid nothing.
	var resp okxFundingHistoryResponse
	resp.Code = "0"
	resp.Data = append(resp.Data, struct {
		InstID       string `json:"instId"`
		FundingRate  string `json:"fundingRate"`
		RealizedRate string `json:"realizedRate"`
		FundingTime  string `json:"fundingTime"`
		Method       string `json:"method"`
	}{InstID: "BTC-USDT-SWAP", FundingRate: "0.0001", RealizedRate: "", FundingTime: "1788480000000"})

	rows, _, err := parseOKXFundingHistory(resp, exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTC-USDT-SWAP"}, exchangestest.WideWindow)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RawRateField != "fundingRate" || rows[0].RateFrac != 0.0001 {
		t.Fatalf("got %+v, want one row from fundingRate = 0.0001", rows)
	}
}
