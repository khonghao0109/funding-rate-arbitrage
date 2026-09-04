package gate

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestGateFundingHistoryGolden(t *testing.T) {
	symbol := exchangestest.BTCSymbol(t, "gate_futures")
	var raw []gateFundingHistoryRow
	exchangestest.LoadJSON(t, "funding_history_gate.json", &raw)

	rows, oldestSec, err := parseGateFundingHistory(raw, symbol, exchangestest.WideWindow)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := exchanges.FinishFundingHistory("gate_futures", symbol, exchanges.FundingDiscrete, rows)
	if err != nil {
		t.Fatal(err)
	}
	exchangestest.AssertHistorySane(t, entries, "gate_futures", exchanges.FundingDiscrete)

	if oldestSec*exchanges.MsPerSecond != entries[0].SettledAtMs {
		t.Errorf("paging cursor = %ds, want the oldest stamp %dms", oldestSec, entries[0].SettledAtMs)
	}
	// This venue stamps SECONDS. A parser that stored them as milliseconds
	// would date every settlement to January 1970 and the corpus would look
	// empty for the last fifty years rather than wrong.
	newest := entries[len(entries)-1]
	if newest.SettledAtMs < 1_500_000_000_000 {
		t.Fatalf("newest SettledAtMs = %d — seconds were not converted to milliseconds", newest.SettledAtMs)
	}
	// And they are not on the hour: the recording has settlements one to three
	// seconds past the boundary. Rounding them would invent a stamp the venue
	// never published, and the primary key would stop matching on a re-fetch.
	if newest.SettledAtMs%(exchanges.SecPerHour*exchanges.MsPerSecond) == 0 {
		t.Errorf("SettledAtMs %d landed exactly on the hour; the recording's stamps do not", newest.SettledAtMs)
	}
}
