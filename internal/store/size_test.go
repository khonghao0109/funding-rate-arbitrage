package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The price sampler's row count is linear in a number somebody picks in
// config.yaml, which makes the cost per row the only thing standing between
// that number and a disk. This measures it.
//
// Skipped by default — it writes 180,000 rows and takes a few seconds:
//
//	MEASURE_STORE=1 go test -run TestPriceSnapshotRowCost -v ./internal/store/
//
// Measured 2026-09-04 on the schema as shipped: 133.3 bytes per row, which is
// 7.46 GB for the 36 series of config.yaml sampled every 5s and kept 90 days.
// That measurement is why the shipped period is 30s (1.24 GB) rather than the
// 5s the plan's "1-5s" suggested — see docs/PLAN.md 2.6. Re-run it after any
// change to the price_snapshots columns or indexes.
func TestPriceSnapshotRowCost(t *testing.T) {
	if os.Getenv("MEASURE_STORE") != "1" {
		t.Skip("set MEASURE_STORE=1 to measure the on-disk cost of a price sample")
	}
	ctx := context.Background()
	dir := t.TempDir()

	db, err := Open(filepath.Join(dir, "size.db"))
	if err != nil {
		t.Fatal(err)
	}

	// The shipped shape: every configured source × every configured pair, one
	// round per sampling tick. Measuring a narrower table would understate the
	// key, which for a WITHOUT ROWID table is most of the row.
	sources := []string{
		"binance_futures", "binance_spot", "bybit_futures", "bybit_spot", "okx_futures",
		"gate_futures", "kraken_futures", "hyperliquid_futures", "paradex_futures",
	}
	symbols := []string{"BTCUSDT", "ETHUSDT", "XRPUSDT", "SOLUSDT"}
	const rounds = 5000

	for round := 0; round < rounds; round++ {
		sampledAtMs := int64(1788000000000 + round*5000)
		batch := make([]PriceSample, 0, len(sources)*len(symbols))
		for _, symbol := range symbols {
			for _, source := range sources {
				batch = append(batch, PriceSample{
					Source: source, Symbol: symbol, SampledAtMs: sampledAtMs,
					MidPriceQuote: 80123.45, BestBidQuote: 80123.4, BestAskQuote: 80123.5,
					BestBidQtyCoin: 1.234, BestAskQtyCoin: 2.345, RecvAtMs: sampledAtMs - 137,
				})
			}
		}
		if _, err := db.PutPriceSamples(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// The WAL and its index are part of the cost until the last connection
	// closes, so the whole directory is measured rather than the .db file.
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}

	rows := int64(rounds * len(sources) * len(symbols))
	perRow := float64(total) / float64(rows)
	t.Logf("%d rows in %d bytes = %.1f bytes/row", rows, total, perRow)
	for _, everySec := range []int{5, 10, 30, 60} {
		projected := int64(90*86400/everySec) * int64(len(sources)*len(symbols))
		t.Logf("  every %2ds for 90 days: %d rows = %.2f GB",
			everySec, projected, float64(projected)*perRow/1e9)
	}
}
