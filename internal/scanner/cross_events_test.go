package scanner

import (
	"encoding/json"
	"testing"
	"time"
)

func jsonMarshalForTest(v any) (string, error) {
	raw, err := json.Marshal(v)
	return string(raw), err
}

// snapAt is a one-pair radar at t seconds with a gross spread and direction.
func snapAt(sec int64, gross float64, short string, live bool) CrossRadarSnapshot {
	status := statusLive
	if !live {
		status = statusStale
	}
	long := "bybit_futures"
	if short == "bybit_futures" {
		long = "binance_futures"
	}
	p := CrossPair{Symbol: "SUIUSDT", GrossAPRPct: gross, ShortSource: short, LongSource: long,
		A: CrossLeg{FundingStatus: status}, B: CrossLeg{FundingStatus: statusLive}}
	return CrossRadarSnapshot{UpdatedAtMs: sec * 1000, Pairs: []CrossPair{p}}
}

func TestCrossEvents_OpenPeakCloseWithGrace(t *testing.T) {
	tr := NewCrossEventTracker([]float64{25, 15}, 60*time.Second, 5)
	if got := tr.Observe(snapAt(0, 16, "binance_futures", true)); len(got.Opened) != 1 || got.Opened[0].ThresholdAPRPct != 15 {
		t.Fatalf("16%% must open the 15%% episode only: %+v", got)
	}
	if got := tr.Observe(snapAt(5, 30, "binance_futures", true)); len(got.Opened) != 1 || got.Opened[0].ThresholdAPRPct != 25 || len(got.Updated) != 1 {
		t.Fatalf("30%% opens 25%% and updates 15%%: %+v", got)
	}
	// Dips below both at 10s, back above 15 at 30s: inside the grace, one episode.
	tr.Observe(snapAt(10, 12, "binance_futures", true))
	if got := tr.Observe(snapAt(30, 18, "binance_futures", true)); len(got.Closed) != 0 {
		t.Fatalf("a dip shorter than the grace closed an episode: %+v", got)
	}
	// 25%% has been below since 10s; at 70s the grace has passed.
	got := tr.Observe(snapAt(70, 18, "binance_futures", true))
	if len(got.Closed) != 1 || got.Closed[0].ThresholdAPRPct != 25 || got.Closed[0].EndedAtMs != 10_000 ||
		got.Closed[0].EndReason != CrossEndBelow || got.Closed[0].PeakGrossAPRPct != 30 || got.Closed[0].DurationSec() != 5 {
		t.Fatalf("the 25%% episode must end at its FIRST reading below (10s), lasting 5s: %+v", got.Closed)
	}
	// 15%%: below from 80s, closed at 140s, ended at 80s, lasting 80s.
	tr.Observe(snapAt(80, 5, "binance_futures", true))
	got = tr.Observe(snapAt(140, 5, "binance_futures", true))
	if len(got.Closed) != 1 || got.Closed[0].EndedAtMs != 80_000 || got.Closed[0].DurationSec() != 80 || tr.OpenCount() != 0 {
		t.Fatalf("15%% episode: %+v open %d", got.Closed, tr.OpenCount())
	}
}

// A spread that dips under the threshold and comes back the other way within the
// grace ends at its first reading below, not when the flip is seen.
func TestCrossEvents_FlipAfterDipEndsAtFirstBelow(t *testing.T) {
	tr := NewCrossEventTracker([]float64{15}, time.Minute, 5)
	tr.Observe(snapAt(0, 20, "binance_futures", true))
	tr.Observe(snapAt(5, 5, "binance_futures", true))
	got := tr.Observe(snapAt(30, 20, "bybit_futures", true))
	if len(got.Closed) != 1 || got.Closed[0].EndedAtMs != 5_000 || got.Closed[0].EndReason != CrossEndFlip {
		t.Fatalf("%+v", got)
	}
}

func TestCrossEvents_FlipEndsOneAndStartsAnother(t *testing.T) {
	tr := NewCrossEventTracker([]float64{15}, time.Minute, 5)
	tr.Observe(snapAt(0, 20, "binance_futures", true))
	got := tr.Observe(snapAt(5, 20, "bybit_futures", true))
	if len(got.Closed) != 1 || got.Closed[0].EndReason != CrossEndFlip || len(got.Opened) != 1 || got.Opened[0].ShortSource != "bybit_futures" {
		t.Fatalf("%+v", got)
	}
}

func TestCrossEvents_StaleOrVanishedEndsAtLastSeenAbove(t *testing.T) {
	tr := NewCrossEventTracker([]float64{15}, time.Minute, 5)
	tr.Observe(snapAt(0, 20, "binance_futures", true))
	tr.Observe(snapAt(5, 21, "binance_futures", true))
	// One stale tick inside an episode is a blip, not an end (review 2026-09-17).
	if got := tr.Observe(snapAt(10, 40, "binance_futures", false)); len(got.Closed) != 0 || len(got.Opened) != 0 {
		t.Fatalf("a stale blip split the episode: %+v", got)
	}
	if got := tr.Observe(snapAt(15, 22, "binance_futures", true)); len(got.Closed) != 0 || len(got.Updated) != 1 {
		t.Fatalf("the episode did not continue after the blip: %+v", got)
	}
	tr.Observe(snapAt(20, 40, "binance_futures", false))
	got := tr.Observe(snapAt(80, 40, "binance_futures", false))
	if len(got.Closed) != 1 || got.Closed[0].EndReason != CrossEndStale || got.Closed[0].EndedAtMs != 15_000 || len(got.Opened) != 0 {
		t.Fatalf("a silence as long as the grace must end the episode at its last live reading above: %+v", got)
	}

	tr.Observe(snapAt(20, 20, "binance_futures", true))
	got = tr.Observe(CrossRadarSnapshot{UpdatedAtMs: 25_000})
	if len(got.Closed) != 1 || got.Closed[0].EndReason != CrossEndStale || got.Closed[0].EndedAtMs != 20_000 {
		t.Fatalf("a symbol that vanished must end as stale: %+v", got)
	}
}

func TestCrossEvents_BelowThenStaleIsStillBelow(t *testing.T) {
	tr := NewCrossEventTracker([]float64{15}, time.Minute, 5)
	tr.Observe(snapAt(0, 20, "binance_futures", true))
	tr.Observe(snapAt(5, 5, "binance_futures", true))
	tr.Observe(snapAt(55, 5, "binance_futures", false))
	got := tr.Observe(snapAt(65, 5, "binance_futures", false))
	if len(got.Closed) != 1 || got.Closed[0].EndReason != CrossEndBelow || got.Closed[0].EndedAtMs != 5_000 {
		t.Fatalf("a spread below for the whole grace ended, whatever the data did after: %+v", got)
	}
}
