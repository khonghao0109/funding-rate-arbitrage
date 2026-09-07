package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
)

func candlesFor(source string, firstOpenMs int64, closes ...float64) []exchanges.PriceCandle {
	out := make([]exchanges.PriceCandle, 0, len(closes))
	for i, close := range closes {
		out = append(out, exchanges.PriceCandle{
			Symbol: "BTCUSDT", Source: source,
			OpenTimeMs: firstOpenMs + int64(i)*3600_000, IntervalSec: 3600,
			OpenPriceQuote: close, HighPriceQuote: close + 1, LowPriceQuote: close - 1,
			ClosePriceQuote: close, BaseVolumeCoin: 1.5,
		})
	}
	return out
}

func TestPriceHistory_RoundTripsAndIsIdempotent(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "scanner.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	const start = int64(1_780_000_000_000)

	candles := candlesFor("binance_spot", start, 100, 101, 102)
	if n, err := db.PutPriceHistory(ctx, candles); err != nil || n != 3 {
		t.Fatalf("first write: %d rows, %v", n, err)
	}
	// A backfill is safe to re-run: the same window written twice is three
	// rows, not six. That is what the primary key is for.
	if _, err := db.PutPriceHistory(ctx, candles); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, err := db.PriceHistory(ctx, "binance_spot", "BTCUSDT", start, start+10*3600_000)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("re-writing the same window produced %d rows, want 3", len(got))
	}
	for i := range got {
		if got[i] != candles[i] {
			t.Errorf("row %d changed on the way through: got %+v want %+v", i, got[i], candles[i])
		}
	}
	// Oldest first, and the window is closed-open.
	if half, _ := db.PriceHistory(ctx, "binance_spot", "BTCUSDT", start, start+2*3600_000); len(half) != 2 {
		t.Errorf("a two-hour window returned %d rows, want 2 (end exclusive)", len(half))
	}
}

func TestPriceHistory_RefusesAnUnidentifiedCandle(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "scanner.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	for _, bad := range []exchanges.PriceCandle{
		{Symbol: "BTCUSDT", OpenTimeMs: 1, IntervalSec: 3600, ClosePriceQuote: 1},          // no source
		{Source: "binance_spot", OpenTimeMs: 1, IntervalSec: 3600, ClosePriceQuote: 1},     // no symbol
		{Source: "binance_spot", Symbol: "BTCUSDT", IntervalSec: 3600, ClosePriceQuote: 1}, // no stamp
		{Source: "binance_spot", Symbol: "BTCUSDT", OpenTimeMs: 1, ClosePriceQuote: 1},     // no interval
	} {
		if _, err := db.PutPriceHistory(context.Background(), []exchanges.PriceCandle{bad}); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// price_history gets its OWN retention number. Sharing the 90-day price_snapshots
// policy would silently delete nine months of a corpus that was backfilled for
// the basis exit.
func TestPrune_PriceHistoryHasItsOwnRetention(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "scanner.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	db.now = func() time.Time { return now }
	old := now.AddDate(0, 0, -200).UnixMilli()
	recent := now.AddDate(0, 0, -10).UnixMilli()
	if _, err := db.PutPriceHistory(ctx, append(
		candlesFor("binance_spot", old, 100),
		candlesFor("binance_spot", recent, 200)...)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A 90-day PRICE SNAPSHOT policy must not touch the candles.
	if _, err := db.Prune(ctx, Retention{PriceDays: 90}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got, _ := db.PriceHistory(ctx, "binance_spot", "BTCUSDT", 0, now.UnixMilli()); len(got) != 2 {
		t.Fatalf("the price_snapshots retention deleted %d candles; the two tables share no number", 2-len(got))
	}

	result, err := db.Prune(ctx, Retention{PriceHistoryDays: 100})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if result.PriceHistoryRows != 1 {
		t.Errorf("pruned %d candles, want 1", result.PriceHistoryRows)
	}
	// And 0 means KEEP EVERYTHING, never "delete everything".
	if _, err := db.Prune(ctx, Retention{PriceHistoryDays: 0}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got, _ := db.PriceHistory(ctx, "binance_spot", "BTCUSDT", 0, now.UnixMilli()); len(got) != 1 {
		t.Errorf("a zero retention removed rows; it must keep everything")
	}
}
