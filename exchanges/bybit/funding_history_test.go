package bybit

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestBybitFundingHistoryGolden(t *testing.T) {
	symbol := exchangestest.BTCSymbol(t, "bybit_futures")
	var resp bybitFundingHistoryResponse
	exchangestest.LoadJSON(t, "funding_history_bybit.json", &resp)

	rows, oldestMs, err := parseBybitFundingHistory(resp, symbol, exchangestest.WideWindow)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := exchanges.FinishFundingHistory("bybit_futures", symbol, exchanges.FundingDiscrete, rows)
	if err != nil {
		t.Fatal(err)
	}
	exchangestest.AssertHistorySane(t, entries, "bybit_futures", exchanges.FundingDiscrete)

	// The venue answers newest first; the entries must come back oldest first
	// (assertHistorySane checks the order) and the paging cursor must be the
	// OLDEST stamp seen, or the next request re-reads the page just read.
	if oldestMs != entries[0].SettledAtMs {
		t.Errorf("paging cursor = %d, want the oldest stamp %d", oldestMs, entries[0].SettledAtMs)
	}
}

func TestBybitFundingHistorySeparatesFailureFromAbsence(t *testing.T) {
	symbol := exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"}

	// Bybit reports both failure and absence inside HTTP 200, and they must not
	// be read the same way. A rate-limit or server error read as an empty list
	// would truncate the corpus silently...
	loud := bybitFundingHistoryResponse{RetCode: 10002, RetMsg: "request not supported"}
	if _, _, err := parseBybitFundingHistory(loud, symbol, exchangestest.WideWindow); err == nil {
		t.Error("a venue error was read as an empty history")
	}

	// ...while an unlisted market read as an error would put a permanent hourly
	// failure in the log for a pair that will never exist (step 2.4, XLMUSDT).
	absent := bybitFundingHistoryResponse{RetCode: 10001, RetMsg: "params error: symbol invalid"}
	rows, _, err := parseBybitFundingHistory(absent, symbol, exchangestest.WideWindow)
	if err != nil {
		t.Errorf("an unlisted market was reported as a failure: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows for an unlisted market", len(rows))
	}
}
