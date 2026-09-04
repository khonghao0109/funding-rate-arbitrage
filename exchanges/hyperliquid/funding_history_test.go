package hyperliquid

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestHyperliquidFundingHistoryGolden(t *testing.T) {
	symbol := exchangestest.BTCSymbol(t, "hyperliquid_futures")
	var raw []hyperliquidFundingHistoryRow
	exchangestest.LoadJSON(t, "funding_history_hyperliquid.json", &raw)

	rows, err := parseHyperliquidFundingHistory(raw, symbol, exchangestest.WideWindow)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := exchanges.FinishFundingHistory("hyperliquid_futures", symbol, exchanges.FundingDiscrete, rows)
	if err != nil {
		t.Fatal(err)
	}
	exchangestest.AssertHistorySane(t, entries, "hyperliquid_futures", exchanges.FundingDiscrete)

	// Hourly, measured — not assumed. The stamps carry millisecond jitter, so
	// this also proves roundedGapSec absorbs it: truncation would produce a
	// mix of 3599 and 3600 and no single cadence.
	report := exchanges.FundingGaps(entries)
	if report.ModalGapSec != exchanges.SecPerHour {
		t.Fatalf("modal gap = %ds, want %ds (hourly); gaps: %v", report.ModalGapSec, exchanges.SecPerHour, report.Counts)
	}
	for i, entry := range entries {
		if entry.IntervalSec != exchanges.SecPerHour {
			t.Errorf("entry %d: IntervalSec = %d, want %d", i, entry.IntervalSec, exchanges.SecPerHour)
		}
	}
	// The jitter itself must survive into the stored stamp: rounding it to the
	// hour would be inventing a timestamp, and a re-fetch would then miss the
	// primary key and insert a duplicate settlement.
	jittered := false
	for _, entry := range entries {
		if entry.SettledAtMs%(exchanges.SecPerHour*exchanges.MsPerSecond) != 0 {
			jittered = true
		}
	}
	if !jittered {
		t.Error("every stamp landed exactly on the hour; the recording's do not")
	}
}
