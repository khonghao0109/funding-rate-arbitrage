package store

import (
	"context"
	"testing"
)

func TestCrossSpreadEvents_OpenRewriteCloseAndRestart(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	open := CrossSpreadEvent{Writer: "scanner:8085", Symbol: "SUIUSDT", ThresholdAPRPct: 15, StartedAtMs: 1_000,
		ShortSource: "binance_futures", LongSource: "bybit_futures",
		PeakGrossAPRPct: 16, PeakAtMs: 1_000, LastSeenAboveMs: 1_000, SampleEverySec: 5, RateModel: "forming_gross"}
	if err := s.PutCrossSpreadEvents(ctx, []CrossSpreadEvent{open}); err != nil {
		t.Fatal(err)
	}
	open.PeakGrossAPRPct, open.LastSeenAboveMs = 22, 9_000
	if err := s.PutCrossSpreadEvents(ctx, []CrossSpreadEvent{open}); err != nil {
		t.Fatal(err)
	}
	second := open
	second.Symbol, second.StartedAtMs = "LTCUSDT", 2_000
	second.EndedAtMs, second.DurationSec, second.EndReason = 7_000, 5, "below"
	if err := s.PutCrossSpreadEvents(ctx, []CrossSpreadEvent{second}); err != nil {
		t.Fatal(err)
	}

	// Another writer's live episode on the same file.
	other := open
	other.Writer = "scanner:8088"
	if err := s.PutCrossSpreadEvents(ctx, []CrossSpreadEvent{other}); err != nil {
		t.Fatal(err)
	}
	n, err := s.CloseOpenCrossSpreadEvents(ctx, "scanner:8085")
	if err != nil || n != 1 {
		t.Fatalf("closed %d, %v — want exactly this writer's one left open", n, err)
	}
	if theirs, _ := s.CrossSpreadEvents(ctx, "scanner:8088", 0, 10); len(theirs) != 1 || theirs[0].EndedAtMs != 0 {
		t.Fatalf("another writer's live episode was touched: %+v", theirs)
	}
	rows, err := s.CrossSpreadEvents(ctx, "scanner:8085", 0, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("%d rows, %v — a rewrite must replace, not duplicate", len(rows), err)
	}
	for _, r := range rows {
		if r.Symbol == "SUIUSDT" && (r.EndReason != "restart" || r.EndedAtMs != 9_000 || r.DurationSec != 8 || r.PeakGrossAPRPct != 22) {
			t.Errorf("restart close must end at last_seen_above_ms: %+v", r)
		}
		if r.Symbol == "LTCUSDT" && r.EndReason != "below" {
			t.Errorf("an already closed episode was touched: %+v", r)
		}
	}
	if rows[0].StartedAtMs < rows[1].StartedAtMs {
		t.Error("newest first")
	}

	bad := open
	bad.EndedAtMs = 5
	if err := s.PutCrossSpreadEvents(ctx, []CrossSpreadEvent{bad}); err == nil {
		t.Error("an end with no reason was accepted")
	}
}
