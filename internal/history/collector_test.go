package history

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/store"
)

// These run the whole collection path with fake fetchers and a real SQLite
// file. No socket is opened: `go test ./...` stays offline, and what is being
// tested here is not the venues but the two decisions this package makes —
// where a resumed window starts, and what happens to the other six venues when
// one of them fails.

func openTemp(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "scanner.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// settlements builds an 8-hourly series covering a window, the way a venue
// would answer.
func settlements(source string, window exchanges.FundingWindow) []exchanges.FundingHistoryEntry {
	const intervalMs = 8 * 3600 * 1000
	var out []exchanges.FundingHistoryEntry
	// Start on a settlement boundary at or after the window start.
	for atMs := ((window.StartMs + intervalMs - 1) / intervalMs) * intervalMs; atMs < window.EndMs; atMs += intervalMs {
		out = append(out, exchanges.FundingHistoryEntry{
			Symbol: "BTCUSDT", Source: source, Model: exchanges.FundingDiscrete,
			SettledAtMs: atMs, RawRate: 0.0001, RawRateField: "fundingRate",
			RatePerIntervalFrac: 0.0001, IntervalSec: 28800, GapPrevSec: 28800,
			RatePer8hFrac: 0.0001, APRFrac: 0.1095,
		})
	}
	return out
}

// recordingFetcher answers with a generated series and remembers every window
// it was asked for.
func recordingFetcher(windows *[]exchanges.FundingWindow) exchanges.FundingHistoryFetchFunc {
	return func(_ context.Context, source string, _ exchanges.Symbol, window exchanges.FundingWindow) ([]exchanges.FundingHistoryEntry, error) {
		*windows = append(*windows, window)
		return settlements(source, window), nil
	}
}

func btcJob(source, connector string) Job {
	return Job{Source: source, Connector: connector, Symbols: []exchanges.Symbol{
		{Standard: "BTCUSDT", Venue: "BTCUSDT"},
	}}
}

func TestNewDropsJobsWithoutAFetcher(t *testing.T) {
	db := openTemp(t)
	var windows []exchanges.FundingWindow

	collector := newCollector(db, []Job{
		btcJob("binance_futures", "binance_futures"),
		// A spot source and an oracle have no funding history to fetch.
		btcJob("binance_spot", "binance_spot"),
		btcJob("pyth", "pyth"),
		// A perp source that serves none of the configured pairs.
		{Source: "gate_futures", Connector: "gate_futures"},
	}, map[string]exchanges.FundingHistoryFetchFunc{
		"binance_futures": recordingFetcher(&windows),
		"gate_futures":    recordingFetcher(&windows),
	})

	jobs := collector.Jobs()
	if len(jobs) != 1 || jobs[0].Source != "binance_futures" {
		t.Fatalf("kept %+v, want only the perp source that serves a pair", jobs)
	}
}

func TestCollectStoresAndIsRepeatable(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	var windows []exchanges.FundingWindow

	collector := newCollector(db, []Job{btcJob("binance_futures", "binance_futures")},
		map[string]exchanges.FundingHistoryFetchFunc{"binance_futures": recordingFetcher(&windows)})

	window := exchanges.FundingWindow{StartMs: 1788000000000, EndMs: 1788480000000}
	results := collector.Collect(ctx, window)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("collect = %+v", results)
	}
	first := results[0]
	if first.Fetched == 0 || first.Inserted != first.Fetched {
		t.Fatalf("first pass fetched %d and inserted %d, want them equal and non-zero",
			first.Fetched, first.Inserted)
	}
	if first.Gaps.ModalGapSec != 28800 {
		t.Errorf("cadence = %ds, want 28800", first.Gaps.ModalGapSec)
	}
	if !first.ReachedRequestedStart() {
		t.Errorf("a venue that answered from the window start reported it did not: %+v", first)
	}

	// Re-running is the demonstration that the corpus survives a restart: the
	// same rows come back from the venue and none of them is stored twice.
	second := collector.Collect(ctx, window)
	if second[0].Fetched != first.Fetched || second[0].Inserted != 0 {
		t.Fatalf("second pass fetched %d, inserted %d — want the same fetch and no new rows",
			second[0].Fetched, second[0].Inserted)
	}
}

func TestCollectReportsAVenueThatCouldNotReachBack(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	// OKX's real behaviour: it answers, but only about three months deep.
	const retentionMs = 90 * 24 * 3600 * 1000
	shallow := func(_ context.Context, source string, _ exchanges.Symbol, window exchanges.FundingWindow) ([]exchanges.FundingHistoryEntry, error) {
		clamped := window
		if oldest := window.EndMs - retentionMs; clamped.StartMs < oldest {
			clamped.StartMs = oldest
		}
		return settlements(source, clamped), nil
	}
	collector := newCollector(db, []Job{btcJob("okx_futures", "okx_futures")},
		map[string]exchanges.FundingHistoryFetchFunc{"okx_futures": shallow})

	endMs := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC).UnixMilli()
	results := collector.Collect(ctx, exchanges.FundingWindow{
		StartMs: endMs - 365*24*3600*1000, EndMs: endMs,
	})
	if results[0].Err != nil {
		t.Fatalf("a venue's retention limit must not read as an error: %v", results[0].Err)
	}
	if results[0].Fetched == 0 {
		t.Fatal("nothing fetched")
	}
	// This is the fact phase 3 has to see: the corpus is not as deep as it was
	// asked for, and nothing about the fetch failing says so.
	if results[0].ReachedRequestedStart() {
		t.Errorf("a series that reached only %s of a %s request reported success",
			time.UnixMilli(results[0].OldestAtMs), time.UnixMilli(results[0].RequestedFromMs))
	}
}

func TestTopUpResumesFromTheNewestStoredSettlement(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	var windows []exchanges.FundingWindow

	collector := newCollector(db, []Job{btcJob("binance_futures", "binance_futures")},
		map[string]exchanges.FundingHistoryFetchFunc{"binance_futures": recordingFetcher(&windows)})
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	collector.now = func() time.Time { return now }

	// Nothing stored yet: the first run asks for the bootstrap window, not a
	// year — filling a year is cmd/backfill's job, run once and watched.
	collector.TopUp(ctx)
	if len(windows) != 1 {
		t.Fatalf("%d windows requested, want 1", len(windows))
	}
	bootstrapDays := time.Duration(now.UnixMilli()-windows[0].StartMs) * time.Millisecond
	if bootstrapDays != BootstrapWindow {
		t.Fatalf("bootstrap asked for %v, want %v", bootstrapDays, BootstrapWindow)
	}

	newestMs, err := db.LatestFundingAt(ctx, "binance_futures", "BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if newestMs == 0 {
		t.Fatal("the bootstrap stored nothing")
	}

	// The second run resumes from the newest stored settlement, minus the
	// overlap. Without the overlap the window would hold one new settlement,
	// and a single row carries no measurable cadence.
	windows = nil
	results := collector.TopUp(ctx)
	if len(windows) != 1 {
		t.Fatalf("%d windows requested, want 1", len(windows))
	}
	if want := newestMs - TopUpOverlap.Milliseconds(); windows[0].StartMs != want {
		t.Errorf("resumed at %d, want %d (newest stored minus the overlap)", windows[0].StartMs, want)
	}
	if results[0].Inserted != 0 {
		t.Errorf("the overlap inserted %d rows; it must be ignored, not duplicated", results[0].Inserted)
	}
}

func TestTopUpKeepsGoingWhenOneVenueFails(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	var windows []exchanges.FundingWindow

	broken := func(context.Context, string, exchanges.Symbol, exchanges.FundingWindow) ([]exchanges.FundingHistoryEntry, error) {
		return nil, errors.New("okx: HTTP 503")
	}
	collector := newCollector(db, []Job{
		btcJob("binance_futures", "binance_futures"),
		btcJob("okx_futures", "okx_futures"),
	}, map[string]exchanges.FundingHistoryFetchFunc{
		"binance_futures": recordingFetcher(&windows),
		"okx_futures":     broken,
	})

	results := collector.TopUp(ctx)
	if len(results) != 2 {
		t.Fatalf("got %d results, want one per series", len(results))
	}
	byPair := map[string]Result{}
	for _, result := range results {
		byPair[result.Source] = result
	}
	// One venue being down must cost only that venue's rows. Aborting the run
	// would mean a single flaky exchange stops the corpus growing at all.
	if byPair["okx_futures"].Err == nil {
		t.Error("the failing venue reported success")
	}
	if byPair["binance_futures"].Err != nil || byPair["binance_futures"].Inserted == 0 {
		t.Errorf("the healthy venue collected nothing: %+v", byPair["binance_futures"])
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	db := openTemp(t)
	var windows []exchanges.FundingWindow
	collector := newCollector(db, []Job{btcJob("binance_futures", "binance_futures")},
		map[string]exchanges.FundingHistoryFetchFunc{"binance_futures": recordingFetcher(&windows)})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		collector.Run(ctx, time.Hour)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
