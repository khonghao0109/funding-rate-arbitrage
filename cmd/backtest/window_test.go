package main

import (
	"testing"
	"time"
)

// The window is "the last N months" unless -from / -to pin it, and a pinned
// window is what a walk-forward needs: two spans that do not both end today.
func TestReplayWindow_DefaultsToTheLastNMonthsAndPinsOnRequest(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	day := func(y int, m time.Month, d int) int64 { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).UnixMilli() }

	w, err := replayWindow(now, 6, "", "")
	if err != nil || w.FromMs != now.AddDate(0, -6, 0).UnixMilli() || w.ToMs != now.UnixMilli() {
		t.Fatalf("default: %+v, %v", w, err)
	}
	w, err = replayWindow(now, 6, "2023-09-09", "")
	if err != nil || w.FromMs != day(2023, 9, 9) || w.ToMs != now.UnixMilli() {
		t.Fatalf("-from alone must keep today's end: %+v, %v", w, err)
	}
	w, err = replayWindow(now, 12, "", "2025-09-09")
	if err != nil || w.ToMs != day(2025, 9, 9) || w.FromMs != day(2024, 9, 9) {
		t.Fatalf("-to alone must keep the N-month length: %+v, %v", w, err)
	}
	w, err = replayWindow(now, 6, "2023-09-09", "2025-09-09")
	if err != nil || w.FromMs != day(2023, 9, 9) || w.ToMs != day(2025, 9, 9) {
		t.Fatalf("both pinned must ignore -months: %+v, %v", w, err)
	}
	for _, bad := range [][2]string{{"2025-09-09", "2023-09-09"}, {"2025-09-09", "2025-09-09"}, {"09/09/2023", ""}, {"", "yesterday"}} {
		if _, err := replayWindow(now, 6, bad[0], bad[1]); err == nil {
			t.Errorf("-from %q -to %q must be refused", bad[0], bad[1])
		}
	}
}

// The corpus is loaded from BEFORE the window, far enough back for the longest
// trailing-mean horizon the grid offers (180 days) to be evaluable at the
// window's first settlement; the engine still decides only inside the window.
func TestFundingLoadFrom_ReachesBackPastTheLongestTrailingHorizon(t *testing.T) {
	w, _ := replayWindow(time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC), 12, "2025-09-09", "2026-09-09")
	from := fundingLoadFrom(w)
	if days := float64(w.FromMs-from) / (24 * 3600 * 1000); days < 180 || days > 365 {
		t.Fatalf("lookback is %.0f days; want at least the 180-day horizon and less than a year", days)
	}
}
