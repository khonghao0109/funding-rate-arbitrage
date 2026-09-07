package backtest

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
)

func TestWriteCSV_EveryColumnCarriesItsUnit(t *testing.T) {
	// Phase 8 reads this file from Python months later, so a bare `rate` or
	// `return` column is how a reader divides a figure that was already
	// divided (CONVENTIONS §1).
	for _, name := range csvHeader {
		switch name {
		case "symbol", "perp_source", "spot_source", "settlements", "trades",
			"funding_reversals", "coverage_short", "ok", "reason_vi", "assumptions_vi",
			"periods_in_position", "basis_not_evaluable", "persistence_periods",
			"exit_persistence_periods", "dropped_special":
			continue // counts and identifiers carry no unit
		}
		if !strings.HasSuffix(name, "_frac") && !strings.HasSuffix(name, "_bps") &&
			!strings.HasSuffix(name, "_pct") && !strings.HasSuffix(name, "_ms") &&
			!strings.HasSuffix(name, "_days") && !strings.HasSuffix(name, "_quote") &&
			!strings.HasSuffix(name, "_share") {
			t.Errorf("column %q carries a unit but does not name it", name)
		}
	}
}

func TestWriteCSV_RoundTripsAResult(t *testing.T) {
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2)
	result := Run(seriesOf(entries), fullWindow(entries), testParams())

	var buf bytes.Buffer
	if err := WriteCSV(&buf, []Result{result}); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want header + 1 row, got %d rows", len(rows))
	}
	if len(rows[1]) != len(csvHeader) {
		t.Errorf("row has %d fields, header has %d", len(rows[1]), len(csvHeader))
	}
	if rows[1][0] != "BTCUSDT" {
		t.Errorf("first column = %q, want the symbol", rows[1][0])
	}
	// The CSV is the artifact that gets detached from everything else, so the
	// cost and the assumptions must be IN it, not beside it.
	byName := map[string]string{}
	for i, name := range rows[0] {
		byName[name] = rows[1][i]
	}
	if byName["round_trip_cost_pct"] == "" || byName["round_trip_cost_pct"] == "0" {
		t.Errorf("round_trip_cost_pct = %q — a traded row must carry the cost it was charged", byName["round_trip_cost_pct"])
	}
	if !strings.Contains(byName["assumptions_vi"], "không backfill") {
		t.Error("assumptions_vi must carry the stated assumptions into the file")
	}
}

// A result printed without its assumptions becomes a number quoted later as a
// measurement of the strategy.
func TestSummary_CarriesTheAssumptionsAndTheBasisCaveat(t *testing.T) {
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2)
	result := Run(seriesOf(entries), fullWindow(entries), testParams())

	joined := strings.Join(result.SummaryLines(), "\n")
	if !strings.Contains(joined, "basis") {
		t.Error("the summary must say the basis exit was never evaluable")
	}
	assumptions := strings.Join(AssumptionLines([]Result{result}), "\n")
	if !strings.Contains(assumptions, "không backfill") {
		t.Error("the assumptions block must state that depth cannot be backfilled")
	}
	if !strings.Contains(assumptions, "căng") {
		t.Error("the assumptions block must warn that today's book is not a stressed book")
	}
}
