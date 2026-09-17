package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"futures-arbitrage-scanner/internal/scanner"
	"futures-arbitrage-scanner/internal/store"
)

func TestCrossRadarHandlers_GetOnlyAndStorageOff(t *testing.T) {
	s := scanner.New([]string{"BTCUSDT"})
	for _, h := range []http.HandlerFunc{newCrossRadarHandler(s), newCrossEventsHandler(apiStore(t), s, "scanner:test")} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/api/cross-radar", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST answered %d", rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	newCrossEventsHandler(nil, s, "scanner:test")(rec, httptest.NewRequest(http.MethodGet, "/api/cross-radar/events", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no store answered %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	newCrossEventsHandler(apiStore(t), s, "scanner:test")(rec, httptest.NewRequest(http.MethodGet, "/api/cross-radar/events?days=500", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("days=500 answered %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	newCrossRadarHandler(s)(rec, httptest.NewRequest(http.MethodGet, "/api/cross-radar", nil))
	var snap scanner.CrossRadarSnapshot
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &snap) != nil || snap.Pairs == nil {
		t.Errorf("radar answered %d %s", rec.Code, rec.Body.String())
	}
}

// Durations are summarized over episodes the SPREAD ended; a stale or restart
// cut is counted by reason but kept out of the statistics.
func TestSummarizeCrossEvents(t *testing.T) {
	row := func(sym string, th, dur float64, reason string, ended int64) store.CrossSpreadEvent {
		return store.CrossSpreadEvent{Symbol: sym, ThresholdAPRPct: th, StartedAtMs: 1, EndedAtMs: ended,
			DurationSec: dur, EndReason: reason, RateModel: "forming_gross"}
	}
	rows := []store.CrossSpreadEvent{
		row("A", 15, 10, "below", 2), row("A", 15, 30, "flip", 2), row("B", 15, 50, "below", 2),
		row("B", 15, 9999, "stale", 2), row("C", 15, 0, "", 0), row("A", 25, 5, "below", 2),
		row("D", 20, 7, "below", 2), // logged under a threshold no longer configured
	}
	got := summarizeCrossEvents(rows, []float64{15, 25})
	if len(got) != 3 || got[1].ThresholdAPRPct != 20 || got[1].Episodes != 1 {
		t.Fatalf("a threshold present in the rows must be summarized: %+v", got)
	}
	got = []apiCrossSummary{got[0], got[2]}
	s15 := got[0]
	if s15.Episodes != 5 || s15.Open != 1 || s15.MeasuredCount != 3 || *s15.MedianSec != 30 || *s15.MeanSec != 30 ||
		*s15.MaxSec != 50 || s15.ByReason["stale"] != 1 || s15.BySymbol["A"] != 2 ||
		s15.CensoredCount != 1 || *s15.CensoredMedianLowerSec != 9999 {
		t.Errorf("15%%: %+v median %v", s15, *s15.MedianSec)
	}
	if !nearly(*s15.P90Sec, 46) {
		t.Errorf("p90 of [10 30 50] is 46, got %v", *s15.P90Sec)
	}
	if got[1].Episodes != 1 || *got[1].MedianSec != 5 {
		t.Errorf("25%%: %+v", got[1])
	}
}

func nearly(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
