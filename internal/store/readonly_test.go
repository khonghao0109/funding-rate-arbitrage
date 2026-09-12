package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/depth"
)

// The paper ledger (step 4.3) is a SECOND process reading the file the
// step-3.5 scanner writes. This pins the two properties that make that safe:
// the reader cannot write, and under WAL the writer's rows reach it without
// either side waiting for the other.
func TestOpenReadOnly_ReadsBesideAWriterAndCannotWrite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "scanner.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.PutFundingHistory(ctx, []exchanges.FundingHistoryEntry{entry("binance_futures", 1_000, 0.0001)}); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer reader.Close()

	rows, err := reader.FundingHistory(ctx, "BTCUSDT", 0, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("reader saw %d rows, want 1", len(rows))
	}

	// The writer keeps writing while the reader holds its handle …
	if _, err := writer.PutFundingHistory(ctx, []exchanges.FundingHistoryEntry{entry("binance_futures", 2_000, 0.0002)}); err != nil {
		t.Fatalf("writer blocked by a read-only reader: %v", err)
	}
	// … and the new row is visible to the reader's next query.
	rows, err = reader.FundingHistory(ctx, "BTCUSDT", 0, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("reader saw %d rows after the writer's insert, want 2", len(rows))
	}

	// Every write path is refused by SQLite itself, not by convention.
	if _, err := reader.PutFundingHistory(ctx, []exchanges.FundingHistoryEntry{entry("binance_futures", 3_000, 0.0003)}); err == nil {
		t.Fatal("a read-only store accepted a write")
	} else if !strings.Contains(err.Error(), "readonly") {
		t.Fatalf("write refused for the wrong reason: %v", err)
	}
	if _, err := reader.DB().ExecContext(ctx, "PRAGMA user_version = 99"); err == nil {
		t.Fatal("a read-only store let the schema version be stamped")
	}
	var jm string
	if err := reader.DB().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&jm); err != nil {
		t.Fatal(err)
	}
	if jm != "wal" {
		t.Fatalf("reader sees journal_mode %q, want wal — the writer's DSN sets it and the reader must find it", jm)
	}
}

func TestOpenReadOnly_RefusesMissingNewerAndForeignFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenReadOnly(filepath.Join(dir, "absent.db")); err == nil {
		t.Fatal("opened a file that does not exist (and must not have created it)")
	}

	newer := filepath.Join(dir, "newer.db")
	raw, err := sql.Open("sqlite", newer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := OpenReadOnly(newer); err == nil {
		t.Fatal("read a file at a schema version this binary does not know")
	}

	foreign := filepath.Join(dir, "foreign.db")
	raw, err = sql.Open("sqlite", foreign)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("CREATE TABLE funding_history (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := OpenReadOnly(foreign); err == nil {
		t.Fatal("read a version-0 file this code never created")
	}
}

// The two "newest at or before" lookups the paper ledger prices on. Never
// after: a sample from the future is a price the instant had not seen.
func TestPriceSampleAtAndDepthSnapshotAt_NewestAtOrBeforeNeverAfter(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	samples := []PriceSample{
		{Source: "binance_spot", Symbol: "BTCUSDT", SampledAtMs: 1_000, MidPriceQuote: 100, RecvAtMs: 999},
		{Source: "binance_spot", Symbol: "BTCUSDT", SampledAtMs: 2_000, MidPriceQuote: 200, RecvAtMs: 1_999},
		{Source: "binance_spot", Symbol: "BTCUSDT", SampledAtMs: 3_000, MidPriceQuote: 300, RecvAtMs: 2_999},
		{Source: "bybit_spot", Symbol: "BTCUSDT", SampledAtMs: 2_500, MidPriceQuote: 250, RecvAtMs: 2_499},
	}
	if _, err := db.PutPriceSamples(ctx, samples); err != nil {
		t.Fatal(err)
	}
	books := []depth.Summary{
		{Source: "binance_spot", Symbol: "BTCUSDT", SampledAtMs: 1_000, MidPriceQuote: 100, AskDepthWithinWideQuote: 1},
		{Source: "binance_spot", Symbol: "BTCUSDT", SampledAtMs: 2_000, MidPriceQuote: 200, AskDepthWithinWideQuote: 2},
		{Source: "binance_spot", Symbol: "BTCUSDT", SampledAtMs: 3_000, MidPriceQuote: 300, AskDepthWithinWideQuote: 3},
	}
	if _, err := db.PutDepthSnapshots(ctx, books); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		atMs    int64
		wantMid float64
		wantOK  bool
	}{
		{"exactly on a sample", 2_000, 200, true},
		{"between two samples takes the earlier one", 2_999, 200, true},
		{"after the newest takes the newest", 9_000, 300, true},
		{"before the oldest has nothing", 999, 0, false},
	}
	for _, tc := range cases {
		t.Run("price "+tc.name, func(t *testing.T) {
			got, ok, err := db.PriceSampleAt(ctx, "binance_spot", "BTCUSDT", tc.atMs)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.wantOK || (ok && got.MidPriceQuote != tc.wantMid) {
				t.Fatalf("got ok=%v mid=%v, want ok=%v mid=%v", ok, got.MidPriceQuote, tc.wantOK, tc.wantMid)
			}
			if ok && got.SampledAtMs > tc.atMs {
				t.Fatalf("returned a sample from %d, AFTER the instant %d", got.SampledAtMs, tc.atMs)
			}
		})
		t.Run("depth "+tc.name, func(t *testing.T) {
			got, ok, err := db.DepthSnapshotAt(ctx, "binance_spot", "BTCUSDT", tc.atMs)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.wantOK || (ok && got.MidPriceQuote != tc.wantMid) {
				t.Fatalf("got ok=%v mid=%v, want ok=%v mid=%v", ok, got.MidPriceQuote, tc.wantOK, tc.wantMid)
			}
			if ok && got.SampledAtMs > tc.atMs {
				t.Fatalf("returned a snapshot from %d, AFTER the instant %d", got.SampledAtMs, tc.atMs)
			}
		})
	}
	// Another source's sample must never answer for this one.
	got, ok, err := db.PriceSampleAt(ctx, "bybit_spot", "BTCUSDT", 9_000)
	if err != nil || !ok || got.MidPriceQuote != 250 {
		t.Fatalf("bybit_spot lookup got ok=%v mid=%v err=%v", ok, got.MidPriceQuote, err)
	}
}

// The ledger's cheaper journal reads: the tally counts without loading, the
// position rows carry no checks_json, and both honour the window.
func TestSignalActionTallyAndPositionRows(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	rows := []SignalRecord{
		{EvaluatedAtMs: 1_000, Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot", Action: "skip", ChecksJSON: "[huge]", ParamsJSON: "{}"},
		{EvaluatedAtMs: 2_000, Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot", Action: "enter", NetAPRFrac: 0.05, NetAPROK: true, CostTotalPct: 0.3, ChecksJSON: "[huge]", ParamsJSON: `{"notional_quote":50000}`},
		{EvaluatedAtMs: 3_000, Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot", Action: "hold", ChecksJSON: "[huge]", ParamsJSON: "{}"},
		{EvaluatedAtMs: 4_000, Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot", Action: "exit", ChecksJSON: "[huge]", ParamsJSON: "{}"},
		{EvaluatedAtMs: 9_000, Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot", Action: "enter", ChecksJSON: "[huge]", ParamsJSON: "{}"},
	}
	if _, err := db.PutSignalDecisions(ctx, rows); err != nil {
		t.Fatal(err)
	}
	tally, err := db.SignalActionTally(ctx, 0, 9_000)
	if err != nil {
		t.Fatal(err)
	}
	if tally["skip"] != 1 || tally["enter"] != 1 || tally["hold"] != 1 || tally["exit"] != 1 || len(tally) != 4 {
		t.Fatalf("tally %v, want one of each inside [0, 9000)", tally)
	}
	got, err := db.SignalPositionRows(ctx, 0, 9_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Action != "enter" || got[1].Action != "exit" {
		t.Fatalf("position rows %+v, want the enter then the exit", got)
	}
	if got[0].ChecksJSON != "" || got[0].ParamsJSON != `{"notional_quote":50000}` || !got[0].NetAPROK || got[0].NetAPRFrac != 0.05 || got[0].CostTotalPct != 0.3 {
		t.Fatalf("enter row %+v: checks_json must be empty and everything else loaded", got[0])
	}
}

// Quote assets come from each market's OWN newest snapshot day.
func TestInstrumentQuoteAssets_UsesEachMarketsNewestDay(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	if _, err := db.PutInstrumentSnapshot(ctx, "2026-09-09", []exchanges.Instrument{
		{Source: "kraken_futures", Symbol: "BTCUSDT", QuoteAsset: "USD"},
		{Source: "binance_spot", Symbol: "BTCUSDT", QuoteAsset: "USDT"},
	}); err != nil {
		t.Fatal(err)
	}
	// The newer day lacks kraken (its fetch failed that day).
	if _, err := db.PutInstrumentSnapshot(ctx, "2026-09-11", []exchanges.Instrument{
		{Source: "binance_spot", Symbol: "BTCUSDT", QuoteAsset: "USDT"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.InstrumentQuoteAssets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got["kraken_futures|BTCUSDT"] != "USD" || got["binance_spot|BTCUSDT"] != "USDT" {
		t.Fatalf("quote assets %v: kraken must still answer from its last successful day", got)
	}
	if _, present := got["okx_futures|BTCUSDT"]; present {
		t.Fatal("a market never snapshotted must be absent, not empty")
	}
}
