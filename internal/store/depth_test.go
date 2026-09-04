package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"futures-arbitrage-scanner/internal/depth"
)

func depthStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "depth.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func sampleSummary(source string, sampledAtMs int64) depth.Summary {
	return depth.Summary{
		Source: source, Symbol: "BTCUSDT",
		SampledAtMs: sampledAtMs, VenueTimeMs: sampledAtMs - 20,
		MidPriceQuote: 81000, BestBidQuote: 80999, BestAskQuote: 81001,
		BestBidQtyCoin: 1.5, BestAskQtyCoin: 2.5, SpreadPct: 0.0025,
		BidDepthWithinTightQuote: 250000, AskDepthWithinTightQuote: 310000,
		BidDepthWithinWideQuote: 1250000, AskDepthWithinWideQuote: 1410000,
		BidLevels: 100, AskLevels: 100,
		BidSpanPct: 0.62, AskSpanPct: 0.58,
		IsContractBook: source == "gate_futures",
	}
}

func TestPutDepthSnapshots_RoundTripsEveryColumn(t *testing.T) {
	db := depthStore(t)
	ctx := context.Background()
	want := sampleSummary("gate_futures", 1788500000000)

	written, err := db.PutDepthSnapshots(ctx, []depth.Summary{want})
	if err != nil || written != 1 {
		t.Fatalf("put: wrote %d, err %v", written, err)
	}

	got, err := db.DepthSnapshots(ctx, "BTCUSDT", 0, 1788500000001)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d rows, want 1", len(got))
	}
	// Compared whole: a column silently not written is exactly the failure
	// PRAGMA user_version exists to prevent, and a spot check would miss it.
	if got[0] != want {
		t.Errorf("round trip changed the row:\n got %+v\nwant %+v", got[0], want)
	}
}

// A row of zeros is indistinguishable from a market with an empty book, and
// phase 3 would read it as "no liquidity" instead of "not measured".
func TestPutDepthSnapshots_SkipsMeasurementsThatFailed(t *testing.T) {
	db := depthStore(t)
	ctx := context.Background()

	failed := depth.Summary{
		Source: "okx_futures", Symbol: "BTCUSDT", SampledAtMs: 1788500000000,
		ErrVI: "Không đọc được sổ lệnh: HTTP 503",
	}
	written, err := db.PutDepthSnapshots(ctx, []depth.Summary{failed, sampleSummary("binance_futures", 1788500000000)})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if written != 1 {
		t.Fatalf("wrote %d rows, want only the successful measurement", written)
	}

	got, err := db.DepthSnapshots(ctx, "", 0, 1788500000001)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, row := range got {
		if row.Source == "okx_futures" {
			t.Error("a failed measurement was stored as zeros")
		}
	}
}

// Unlike a settlement, a depth measurement of the same instant is a corrected
// reading of one moment rather than a second event that must never overwrite
// the first.
func TestPutDepthSnapshots_ARemeasurementReplaces(t *testing.T) {
	db := depthStore(t)
	ctx := context.Background()

	first := sampleSummary("binance_futures", 1788500000000)
	second := first
	second.BidDepthWithinWideQuote = 999999

	if _, err := db.PutDepthSnapshots(ctx, []depth.Summary{first}); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if _, err := db.PutDepthSnapshots(ctx, []depth.Summary{second}); err != nil {
		t.Fatalf("second put: %v", err)
	}

	got, err := db.DepthSnapshots(ctx, "BTCUSDT", 0, 1788500000001)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d rows, want one replaced row", len(got))
	}
	if got[0].BidDepthWithinWideQuote != 999999 {
		t.Errorf("bid depth = %g, want the re-measurement", got[0].BidDepthWithinWideQuote)
	}
}

func TestDepthSnapshots_HonoursTheWindowAndTheSymbol(t *testing.T) {
	db := depthStore(t)
	ctx := context.Background()

	old := sampleSummary("binance_futures", 1788400000000)
	recent := sampleSummary("binance_futures", 1788500000000)
	other := sampleSummary("binance_futures", 1788500000000)
	other.Symbol = "ETHUSDT"

	if _, err := db.PutDepthSnapshots(ctx, []depth.Summary{old, recent, other}); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := db.DepthSnapshots(ctx, "BTCUSDT", 1788450000000, 1788500000001)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 || got[0].SampledAtMs != 1788500000000 {
		t.Fatalf("got %+v, want only the recent BTCUSDT row", got)
	}
}

// Depth is the one series that can never be re-fetched, so an unset retention
// must keep it rather than delete it — the same asymmetry as funding.
func TestPrune_DepthZeroDaysKeepsEverything(t *testing.T) {
	db := depthStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1788500000000)
	db.SetClock(func() time.Time { return now })

	ancient := sampleSummary("binance_futures", now.AddDate(0, 0, -400).UnixMilli())
	if _, err := db.PutDepthSnapshots(ctx, []depth.Summary{ancient}); err != nil {
		t.Fatalf("put: %v", err)
	}

	if _, err := db.Prune(ctx, Retention{}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	got, err := db.DepthSnapshots(ctx, "", 0, now.UnixMilli())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 {
		t.Fatal("an unset retention deleted depth; 0 means keep everything")
	}

	result, err := db.Prune(ctx, Retention{DepthDays: 30})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if result.DepthRows != 1 {
		t.Errorf("pruned %d depth rows, want 1", result.DepthRows)
	}
}
