package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
)

// The store's job is to lose nothing and to invent nothing. These tests are
// written around the two ways it could fail at that quietly: a re-fetch that
// duplicates a settlement instead of ignoring it, and a retention pass that
// deletes more than it was told to.

func openTemp(t *testing.T) *Store {
	t.Helper()
	// A real file, not :memory: — "restart loses nothing" is the acceptance
	// criterion of this step, and an in-memory database cannot be reopened.
	db, err := Open(filepath.Join(t.TempDir(), "scanner.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func entry(source string, atMs int64, rate float64) exchanges.FundingHistoryEntry {
	return exchanges.FundingHistoryEntry{
		Symbol: "BTCUSDT", Source: source, Model: exchanges.FundingDiscrete,
		SettledAtMs: atMs, RawRate: rate, RawRateField: "fundingRate",
		RatePerIntervalFrac: rate, IntervalSec: 28800, GapPrevSec: 28800,
		RatePer8hFrac: rate, APRFrac: rate * 1095,
	}
}

func TestOpenSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "scanner.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := first.PutFundingHistory(ctx, []exchanges.FundingHistoryEntry{
		entry("binance_futures", 1788480000000, 0.0001),
	}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// This is the acceptance criterion of step 2.6, exercised rather than
	// asserted: a new process opening the same file sees what the last one
	// wrote, and applying the schema again is a no-op.
	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	rows, err := second.FundingHistory(ctx, "BTCUSDT", 0, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SettledAtMs != 1788480000000 {
		t.Fatalf("after reopen got %+v, want the one row written before the close", rows)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scanner.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// An old binary opening a new file would keep writing, silently leaving
	// every column it does not know about empty.
	if _, err := Open(path); err == nil {
		t.Fatal("a database from a newer schema version was opened without complaint")
	}
}

func TestPutFundingHistoryIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	entries := []exchanges.FundingHistoryEntry{
		entry("binance_futures", 1788422400000, 0.00005),
		entry("binance_futures", 1788451200000, 0.00008),
		entry("binance_futures", 1788480000000, 0.00009),
	}
	inserted, err := db.PutFundingHistory(ctx, entries)
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 3 {
		t.Fatalf("first pass inserted %d, want 3", inserted)
	}

	// Overlapping windows are the NORMAL case: every top-up re-fetches its
	// overlap and the backfill can be re-run at will. If this ever inserts
	// again, the corpus grows a duplicate settlement per run and every
	// settlement count in phase 3 is wrong.
	inserted, err = db.PutFundingHistory(ctx, entries)
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 0 {
		t.Fatalf("second pass inserted %d, want 0", inserted)
	}

	rows, err := db.FundingHistory(ctx, "BTCUSDT", 0, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("stored %d rows, want 3", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].SettledAtMs <= rows[i-1].SettledAtMs {
			t.Fatalf("rows are not oldest-first: %d after %d", rows[i].SettledAtMs, rows[i-1].SettledAtMs)
		}
	}
	if rows[0].Model != exchanges.FundingDiscrete {
		t.Errorf("Model = %q, want discrete — the column decides whether a row counts as a payment", rows[0].Model)
	}
	if rows[0].RecordedAtMs == 0 {
		t.Error("RecordedAtMs is 0; a row nobody can date")
	}
}

func TestPutFundingHistoryRefusesUnusableRows(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	for _, tc := range []struct {
		name  string
		entry exchanges.FundingHistoryEntry
	}{
		{"no source", exchanges.FundingHistoryEntry{Symbol: "BTCUSDT", SettledAtMs: 1, IntervalSec: 28800}},
		{"no symbol", exchanges.FundingHistoryEntry{Source: "binance_futures", SettledAtMs: 1, IntervalSec: 28800}},
		{"no settlement", exchanges.FundingHistoryEntry{Source: "binance_futures", Symbol: "BTCUSDT", IntervalSec: 28800}},
		{"no interval", exchanges.FundingHistoryEntry{Source: "binance_futures", Symbol: "BTCUSDT", SettledAtMs: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.PutFundingHistory(ctx, []exchanges.FundingHistoryEntry{tc.entry}); err == nil {
				t.Fatal("accepted a row that cannot be used; everything downstream divides by IntervalSec")
			}
		})
	}
}

func TestFundingHistoryWindowAndSymbolFilter(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	eth := entry("binance_futures", 1788451200000, 0.0002)
	eth.Symbol = "ETHUSDT"
	if _, err := db.PutFundingHistory(ctx, []exchanges.FundingHistoryEntry{
		entry("binance_futures", 1788422400000, 0.00005),
		entry("binance_futures", 1788451200000, 0.00008),
		entry("bybit_futures", 1788451200000, 0.00007),
		eth,
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := db.FundingHistory(ctx, "BTCUSDT", 1788451200000, 1788480000000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want the two BTC venues at the window's start (closed-open)", len(rows))
	}
	for _, row := range rows {
		if row.Symbol != "BTCUSDT" {
			t.Errorf("row for %s leaked past the symbol filter", row.Symbol)
		}
	}

	all, err := db.FundingHistory(ctx, "", 0, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("an empty symbol read %d rows, want all 4", len(all))
	}
}

func TestLatestFundingAt(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	// Nothing stored: MAX over no rows is SQL NULL, and scanning it into an
	// int64 is how this would fail as a type error rather than as a 0.
	newest, err := db.LatestFundingAt(ctx, "binance_futures", "BTCUSDT")
	if err != nil {
		t.Fatalf("empty table: %v", err)
	}
	if newest != 0 {
		t.Fatalf("got %d for an empty series, want 0", newest)
	}

	if _, err := db.PutFundingHistory(ctx, []exchanges.FundingHistoryEntry{
		entry("binance_futures", 1788422400000, 0.00005),
		entry("binance_futures", 1788480000000, 0.00009),
		entry("bybit_futures", 1788508800000, 0.00007),
	}); err != nil {
		t.Fatal(err)
	}
	newest, err = db.LatestFundingAt(ctx, "binance_futures", "BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	// The other venue's newer row must not be the answer, or a top-up would
	// start after settlements this venue never delivered.
	if newest != 1788480000000 {
		t.Fatalf("got %d, want this source's own newest settlement", newest)
	}
}

func TestFundingCoverage(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	continuous := entry("paradex_futures", 1788480000000, 0.00008)
	continuous.Model = exchanges.FundingContinuous
	if _, err := db.PutFundingHistory(ctx, []exchanges.FundingHistoryEntry{
		entry("binance_futures", 1788422400000, 0.00005),
		entry("binance_futures", 1788480000000, 0.00009),
		continuous,
	}); err != nil {
		t.Fatal(err)
	}

	coverage, err := db.FundingCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(coverage) != 2 {
		t.Fatalf("got %d series, want 2", len(coverage))
	}
	bySource := map[string]Coverage{}
	for _, c := range coverage {
		bySource[c.Source] = c
	}
	binance := bySource["binance_futures"]
	if binance.Rows != 2 || binance.OldestAtMs != 1788422400000 || binance.NewestAtMs != 1788480000000 {
		t.Errorf("binance coverage = %+v", binance)
	}
	// The model has to survive into the report: a continuous series is not a
	// shorter discrete one, and phase 3 must be able to tell them apart before
	// counting settlements.
	if bySource["paradex_futures"].Model != string(exchanges.FundingContinuous) {
		t.Errorf("paradex coverage model = %q", bySource["paradex_futures"].Model)
	}
}

func TestPriceSnapshotsRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	samples := []PriceSample{
		{Source: "binance_futures", Symbol: "BTCUSDT", SampledAtMs: 1000,
			MidPriceQuote: 80000, BestBidQuote: 79999, BestAskQuote: 80001,
			BestBidQtyCoin: 1.5, BestAskQtyCoin: 2, RecvAtMs: 950},
		// A contract-denominated venue: 0 quantity means NOT KNOWN and must
		// round-trip as 0 rather than being dropped.
		{Source: "okx_futures", Symbol: "BTCUSDT", SampledAtMs: 1000,
			MidPriceQuote: 80010, BestBidQuote: 80009, BestAskQuote: 80011, RecvAtMs: 900},
	}
	if _, err := db.PutPriceSamples(ctx, samples); err != nil {
		t.Fatal(err)
	}

	got, err := db.PriceSnapshots(ctx, "BTCUSDT", 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d samples, want 2", len(got))
	}
	if got[1].BestBidQtyCoin != 0 || got[1].RecvAtMs != 900 {
		t.Errorf("okx sample = %+v, want zero quantities and its own recv stamp", got[1])
	}

	// Same key, better measurement: a re-sample in the same millisecond must
	// replace, not fail and not duplicate.
	samples[0].MidPriceQuote = 80500
	if _, err := db.PutPriceSamples(ctx, samples); err != nil {
		t.Fatal(err)
	}
	got, err = db.PriceSnapshots(ctx, "BTCUSDT", 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].MidPriceQuote != 80500 {
		t.Fatalf("after re-sampling got %d rows, first mid %g", len(got), got[0].MidPriceQuote)
	}
}

func TestInstrumentSnapshotRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	instruments := []exchanges.Instrument{{
		Symbol: "BTCUSDT", NativeSymbol: "BTC-USDT-SWAP", Source: "okx_futures", MarketType: "perp",
		BaseAsset: "BTC", QuoteAsset: "USDT", Status: exchanges.StatusTrading,
		TickSizeQuote: 0.1, StepSizeCoin: 0.0001, MinQtyCoin: 0.0001,
		IsContract: true, ContractSizeCoin: 0.01, MaxLeverageX: 100,
	}}
	written, err := db.PutInstrumentSnapshot(ctx, "2026-09-04", instruments)
	if err != nil {
		t.Fatal(err)
	}
	if written != 1 {
		t.Fatalf("wrote %d, want 1", written)
	}

	got, err := db.InstrumentSnapshot(ctx, "2026-09-04")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != instruments[0] {
		t.Fatalf("round trip changed the instrument:\n got %+v\nwant %+v", got, instruments)
	}

	// An empty registry means every fetch failed. Recording that as the day's
	// snapshot would assert something about the venues instead of about us.
	written, err = db.PutInstrumentSnapshot(ctx, "2026-09-05", nil)
	if err != nil {
		t.Fatalf("an empty snapshot must not be an error: %v", err)
	}
	if written != 0 {
		t.Fatalf("wrote %d rows for an empty registry", written)
	}
}

func TestPruneRespectsRetention(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	db.SetClock(func() time.Time { return now })

	dayMs := int64(24 * time.Hour / time.Millisecond)
	nowMs := now.UnixMilli()
	if _, err := db.PutFundingHistory(ctx, []exchanges.FundingHistoryEntry{
		entry("binance_futures", nowMs-400*dayMs, 0.0001), // older than a year
		entry("binance_futures", nowMs-10*dayMs, 0.0002),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutPriceSamples(ctx, []PriceSample{
		{Source: "binance_futures", Symbol: "BTCUSDT", SampledAtMs: nowMs - 100*dayMs, MidPriceQuote: 1, RecvAtMs: 1},
		{Source: "binance_futures", Symbol: "BTCUSDT", SampledAtMs: nowMs - 10*dayMs, MidPriceQuote: 1, RecvAtMs: 1},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := db.Prune(ctx, Retention{FundingDays: 365, PriceDays: 90})
	if err != nil {
		t.Fatal(err)
	}
	if result.FundingRows != 1 || result.PriceRows != 1 {
		t.Fatalf("pruned %+v, want one row of each", result)
	}
	rows, err := db.FundingHistory(ctx, "", 0, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SettledAtMs != nowMs-10*dayMs {
		t.Fatalf("kept %+v, want only the recent settlement", rows)
	}
}

func TestPruneKeepsEverythingAtZero(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	db.SetClock(func() time.Time { return now })
	dayMs := int64(24 * time.Hour / time.Millisecond)

	if _, err := db.PutFundingHistory(ctx, []exchanges.FundingHistoryEntry{
		entry("binance_futures", now.UnixMilli()-4000*dayMs, 0.0001),
	}); err != nil {
		t.Fatal(err)
	}

	// 0 must mean "keep everything". The opposite reading — a cutoff of "now" —
	// would let an unset field in a YAML file delete a corpus that three of the
	// seven venues cannot refill.
	result, err := db.Prune(ctx, Retention{})
	if err != nil {
		t.Fatal(err)
	}
	if result.FundingRows != 0 || result.PriceRows != 0 {
		t.Fatalf("a zero retention deleted %+v", result)
	}
	rows, err := db.FundingHistory(ctx, "", 0, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("an eleven-year-old row was deleted by a zero retention")
	}
}

func TestOpenRefusesAPathTheDriverWouldMisread(t *testing.T) {
	// The pragmas ride in the DSN as a query string. A '?' in the path would be
	// read as the start of them, and the store would open a different, shorter
	// file than the one configured — with no error and no missing data until
	// someone went looking for last month's funding.
	if _, err := Open(filepath.Join(t.TempDir(), "scan?ner.db")); err == nil {
		t.Fatal("a path containing '?' was accepted")
	}
}
