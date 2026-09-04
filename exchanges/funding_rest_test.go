package exchanges

import (
	"testing"
	"time"
)

// A settlement stamp that has already passed is dropped rather than published:
// Hyperliquid settles hourly while its metadata refreshes every few minutes,
// so a cached stamp goes stale between refreshes.
func TestFutureStampMs_DropsThePast(t *testing.T) {
	now := time.Date(2026, 9, 4, 9, 30, 0, 0, time.UTC)
	future := now.Add(30 * time.Minute).UnixMilli()
	past := now.Add(-1 * time.Minute).UnixMilli()

	if got := FutureStampMs(future, now); got != future {
		t.Errorf("a stamp still ahead must survive: got %d, want %d", got, future)
	}
	if got := FutureStampMs(past, now); got != 0 {
		t.Errorf("a stamp already passed must become 0 (not supplied), got %d", got)
	}
	if got := FutureStampMs(0, now); got != 0 {
		t.Errorf("an absent stamp stays absent, got %d", got)
	}
}
