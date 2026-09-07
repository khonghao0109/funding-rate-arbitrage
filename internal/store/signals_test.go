package store

import (
	"context"
	"testing"
)

// The signal journal is step 3.5's paper-trading record: every decision the
// live path makes, with the reasoning, so the gate can compare it against a
// backtest over the same window. A journal that dropped the reasoning would be
// a list of verdicts nobody can argue with two weeks later.
func TestSignalJournal_RoundTripsADecisionWithItsReasoning(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	in := []SignalRecord{{
		EvaluatedAtMs: 1_757_000_000_000,
		Symbol:        "BTCUSDT",
		PerpSource:    "binance_futures",
		SpotSource:    "binance_spot",
		Action:        "skip",
		NetAPRFrac:    0.0571,
		NetAPROK:      true,
		CostTotalPct:  0.3010,
		ChecksJSON:    `[{"name":"persistence","passed":false,"detail_vi":"2/6 mốc"}]`,
		ParamsJSON:    `{"min_rate_per_8h_bps":0.8}`,
	}}
	n, err := s.PutSignalDecisions(ctx, in)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if n != 1 {
		t.Fatalf("wrote %d rows, want 1", n)
	}

	out, err := s.SignalDecisions(ctx, 0, 2_000_000_000_000)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("read %d rows, want 1", len(out))
	}
	got := out[0]
	if got.Symbol != "BTCUSDT" || got.PerpSource != "binance_futures" || got.Action != "skip" {
		t.Errorf("identity/action lost: %+v", got)
	}
	if got.ChecksJSON != in[0].ChecksJSON {
		t.Errorf("reasoning lost: %q", got.ChecksJSON)
	}
	if got.NetAPRFrac != 0.0571 || !got.NetAPROK || got.CostTotalPct != 0.3010 {
		t.Errorf("figures lost: %+v", got)
	}
	if got.RecordedAtMs <= 0 {
		t.Error("the store must stamp when it wrote the row")
	}
}

// Re-evaluating the same instant is a corrected reading of one moment, not a
// second decision: REPLACE, so a restart mid-window cannot double-count.
func TestSignalJournal_SameInstantReplacesRatherThanDuplicates(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	rec := SignalRecord{EvaluatedAtMs: 1, Symbol: "BTCUSDT", PerpSource: "p", SpotSource: "s", Action: "skip"}
	if _, err := s.PutSignalDecisions(ctx, []SignalRecord{rec}); err != nil {
		t.Fatal(err)
	}
	rec.Action = "enter"
	if _, err := s.PutSignalDecisions(ctx, []SignalRecord{rec}); err != nil {
		t.Fatal(err)
	}
	out, err := s.SignalDecisions(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Action != "enter" {
		t.Errorf("want one row carrying the later action, got %+v", out)
	}
}

func TestSignalJournal_RefusesAnUnidentifiedRow(t *testing.T) {
	s := openTemp(t)
	if _, err := s.PutSignalDecisions(context.Background(), []SignalRecord{{Symbol: "BTCUSDT"}}); err == nil {
		t.Error("a row with no evaluation instant or source must be refused, not stored as zeros")
	}
}
