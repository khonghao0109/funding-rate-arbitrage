package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/paper"
	"futures-arbitrage-scanner/internal/store"
)

const (
	t0     = int64(1_789_110_000_000)
	hourMs = int64(3_600_000)
)

func book(source, symbol string, mid float64, at int64) depth.Summary {
	return depth.Summary{
		Source: source, Symbol: symbol, SampledAtMs: at,
		MidPriceQuote: mid, BestBidQuote: mid * 0.9999, BestAskQuote: mid * 1.0001, SpreadPct: 0.02,
		BidDepthWithinTightQuote: 1_000_000, AskDepthWithinTightQuote: 1_000_000,
		BidDepthWithinWideQuote: 5_000_000, AskDepthWithinWideQuote: 5_000_000,
		BidLevels: 100, AskLevels: 100, BidSpanPct: 1, AskSpanPct: 1,
	}
}

func paramsJSON(t *testing.T, withFees bool) string {
	t.Helper()
	p := map[string]any{"notional_quote": 50_000.0, "max_book_age_min": 120.0, "perp_margin_frac": 0.0, "holding_days": 90.0}
	if withFees {
		p["fees"] = map[string]any{
			"spot": map[string]any{"source": "binance_spot", "taker_bps": 10.0, "verified": true},
			"perp": map[string]any{"source": "binance_futures", "taker_bps": 5.0, "verified": true},
		}
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// seed writes a synthetic run-2-shaped window: books an hour before the
// decisions, an enter, holds, one settlement while open, an exit, and 30s
// price samples throughout.
func seed(t *testing.T, withFees bool) *store.Store {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "scanner.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.PutInstrumentSnapshot(ctx, "2026-09-11", []exchanges.Instrument{
		{Source: "binance_spot", Symbol: "BTCUSDT", QuoteAsset: "USDT", MarketType: "spot"},
		{Source: "binance_futures", Symbol: "BTCUSDT", QuoteAsset: "USDT", MarketType: "perp"},
	}); err != nil {
		t.Fatal(err)
	}
	var books []depth.Summary
	for h := int64(-1); h <= 12; h++ { // hourly sweeps, the first one an hour BEFORE the enter
		books = append(books,
			book("binance_spot", "BTCUSDT", 100_000, t0+h*hourMs-60_000),
			book("binance_futures", "BTCUSDT", 100_050, t0+h*hourMs-60_000))
	}
	if _, err := db.PutDepthSnapshots(ctx, books); err != nil {
		t.Fatal(err)
	}
	var samples []store.PriceSample
	for ms := t0 - hourMs; ms <= t0+12*hourMs; ms += 30_000 {
		samples = append(samples,
			store.PriceSample{Source: "binance_spot", Symbol: "BTCUSDT", SampledAtMs: ms, MidPriceQuote: 100_000, RecvAtMs: ms},
			store.PriceSample{Source: "binance_futures", Symbol: "BTCUSDT", SampledAtMs: ms, MidPriceQuote: 100_050, RecvAtMs: ms})
	}
	if _, err := db.PutPriceSamples(ctx, samples); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutFundingHistory(ctx, []exchanges.FundingHistoryEntry{
		// Before the open: must not be credited.
		{Symbol: "BTCUSDT", Source: "binance_futures", Model: exchanges.FundingDiscrete, SettledAtMs: t0 - 30*60_000, RatePerIntervalFrac: 0.0005, IntervalSec: 28800, GapPrevSec: 28800, RatePer8hFrac: 0.0005, MarkPriceQuote: 99_000},
		// While open, mark published by the venue.
		{Symbol: "BTCUSDT", Source: "binance_futures", Model: exchanges.FundingDiscrete, SettledAtMs: t0 + 8*hourMs, RatePerIntervalFrac: 0.0001, IntervalSec: 28800, GapPrevSec: 28800, RatePer8hFrac: 0.0001, MarkPriceQuote: 100_100},
		// After the close: must not be credited.
		{Symbol: "BTCUSDT", Source: "binance_futures", Model: exchanges.FundingDiscrete, SettledAtMs: t0 + 16*hourMs, RatePerIntervalFrac: 0.0001, IntervalSec: 28800, GapPrevSec: 28800, RatePer8hFrac: 0.0001, MarkPriceQuote: 100_100},
	}); err != nil {
		t.Fatal(err)
	}
	pj := paramsJSON(t, withFees)
	rows := []store.SignalRecord{
		{EvaluatedAtMs: t0 - 10*60_000, Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot", Action: "skip", ChecksJSON: "[]", ParamsJSON: pj},
		{EvaluatedAtMs: t0, Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot", Action: "enter", NetAPRFrac: 0.061, NetAPROK: true, CostTotalPct: 0.3011, ChecksJSON: "[]", ParamsJSON: pj},
		{EvaluatedAtMs: t0 + 10*60_000, Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot", Action: "hold", ChecksJSON: "[]", ParamsJSON: pj},
		{EvaluatedAtMs: t0 + 10*hourMs, Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot", Action: "exit", ChecksJSON: "[]", ParamsJSON: pj},
	}
	if _, err := db.PutSignalDecisions(ctx, rows); err != nil {
		t.Fatal(err)
	}
	return db
}

// PLAN 4.3 acceptance (2): the equity curve of a journal window is rebuilt
// from the journal and the corpus alone, and each enter's projected net APR
// stands beside the paper P&L of that position to its exit.
func TestBuild_ReconstructsPositionsFundingAndEquityFromTheJournal(t *testing.T) {
	db := seed(t, true)
	report, err := Build(context.Background(), Inputs{
		DB: db, FromMs: t0 - hourMs, ToMs: t0 + 12*hourMs, NowMs: t0 + 12*hourMs,
		CapitalQuote: 1_000_000, MarkEvery: time.Hour, FromNoteVI: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Mode != "paper" {
		t.Fatalf("mode %q", report.Mode)
	}
	if report.JournalRows != 4 || report.Enters != 1 || report.Exits != 1 || report.Holds != 1 || report.Skips != 1 {
		t.Fatalf("journal tally %+v", report)
	}
	if len(report.Open) != 0 || len(report.Closed) != 1 || report.Refusals != 0 || report.Anomalies != 0 {
		t.Fatalf("open %d closed %d refusals %d anomalies %d", len(report.Open), len(report.Closed), report.Refusals, report.Anomalies)
	}
	p := report.Closed[0]
	if p.OpenedAtMs != t0 || p.ClosedAtMs != t0+10*hourMs {
		t.Fatalf("position window %d → %d", p.OpenedAtMs, p.ClosedAtMs)
	}
	// The book used is the sweep an hour BEFORE the decision, never the one after.
	if p.EntrySpot.BookSampledAtMs != t0-60_000 || p.EntryPerp.BookSampledAtMs != t0-60_000 {
		t.Fatalf("entry books sampled at %d / %d, want the sweep at %d", p.EntrySpot.BookSampledAtMs, p.EntryPerp.BookSampledAtMs, t0-60_000)
	}
	if p.ExitSpot.BookSampledAtMs != t0+10*hourMs-60_000 {
		t.Fatalf("exit book sampled at %d, want %d", p.ExitSpot.BookSampledAtMs, t0+10*hourMs-60_000)
	}
	// Exactly ONE settlement collected: the one while open, at the venue's mark.
	if p.FundingSettlements != 1 || len(p.Credits) != 1 || p.Credits[0].SettledAtMs != t0+8*hourMs || p.Credits[0].MarkPriceQuote != 100_100 {
		t.Fatalf("funding credits %+v", p.Credits)
	}
	wantFunding := 100_100 * p.QtyCoin * 0.0001
	if diff := p.FundingQuote - wantFunding; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("funding %v, want mark × qty × rate = %v", p.FundingQuote, wantFunding)
	}
	// The projected figure and the realized one stand side by side.
	if !p.ProjectedNetAPROK || p.ProjectedNetAPRFrac != 0.061 || p.JournalCostTotalPct != 0.3011 {
		t.Fatalf("projection not carried: %+v", p)
	}
	if p.RealizedPnLQuote >= 0 {
		t.Fatalf("with flat prices one settlement cannot pay four fills: realized %v", p.RealizedPnLQuote)
	}
	// Equity: one mark per hour from the enter to the window end, plus the end.
	if len(report.Equity) < 12 {
		t.Fatalf("equity has %d points, want hourly marks from the enter", len(report.Equity))
	}
	last := report.Equity[len(report.Equity)-1]
	if last.AtMs != t0+12*hourMs || last.OpenPositions != 0 {
		t.Fatalf("last point %+v", last)
	}
	if diff := last.EquityQuote - (1_000_000 + p.RealizedPnLQuote); diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("final equity %v, want capital + realized = %v", last.EquityQuote, 1_000_000+p.RealizedPnLQuote)
	}
	if report.EquityQuote != last.EquityQuote || report.RealizedQuote != p.RealizedPnLQuote || report.FundingQuote != p.FundingQuote {
		t.Fatal("report totals must equal the ledger's")
	}
	for _, pt := range report.Equity {
		if pt.StaleMarks != 0 {
			t.Fatalf("a 30s sampler must never leave a stale mark: %+v", pt)
		}
	}
	// Event order at the close instant: funding never after close, open never before close.
	kinds := []paper.EventKind{}
	for _, e := range report.Events {
		kinds = append(kinds, e.Kind)
	}
	want := []paper.EventKind{paper.EventOpen, paper.EventFunding, paper.EventClose}
	if len(kinds) != len(want) {
		t.Fatalf("events %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("events %v, want %v", kinds, want)
		}
	}
}

// A row without a fee state (the first run's rows) is refused BY NAME —
// never priced at zero, never priced from today's config.yaml.
func TestBuild_RefusesRowsWithoutFeeState(t *testing.T) {
	db := seed(t, false)
	report, err := Build(context.Background(), Inputs{
		DB: db, FromMs: t0 - hourMs, ToMs: t0 + 12*hourMs, NowMs: t0 + 12*hourMs, CapitalQuote: 1_000_000, MarkEvery: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Open) != 0 || len(report.Closed) != 0 || report.Refusals < 1 {
		t.Fatalf("open %d closed %d refusals %d — a fee-less row must open nothing", len(report.Open), len(report.Closed), report.Refusals)
	}
	found := false
	for _, e := range report.Events {
		if e.Kind == paper.EventRefuseOpen && strings.Contains(e.DetailVI, "không ghi biểu phí") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refusal must name the missing fee state: %+v", report.Events)
	}
	if report.EquityQuote != 1_000_000 {
		t.Fatalf("equity %v moved on a refused open", report.EquityQuote)
	}
}

// The mark stand-in for venues that publish no mark: the closest sampled mid
// at or before the stamp, labelled as such.
func TestBuild_UsesSampledMidWhenTheVenuePublishesNoMark(t *testing.T) {
	db := seed(t, true)
	ctx := context.Background()
	if _, err := db.PutFundingHistory(ctx, []exchanges.FundingHistoryEntry{
		{Symbol: "BTCUSDT", Source: "binance_futures", Model: exchanges.FundingDiscrete, SettledAtMs: t0 + 4*hourMs, RatePerIntervalFrac: 0.0002, IntervalSec: 28800, GapPrevSec: 28800, RatePer8hFrac: 0.0002, MarkPriceQuote: 0},
	}); err != nil {
		t.Fatal(err)
	}
	report, err := Build(ctx, Inputs{DB: db, FromMs: t0 - hourMs, ToMs: t0 + 12*hourMs, NowMs: t0 + 12*hourMs, CapitalQuote: 1_000_000, MarkEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	p := report.Closed[0]
	if p.FundingSettlements != 2 {
		t.Fatalf("settlements %d, want 2", p.FundingSettlements)
	}
	c := p.Credits[0]
	if c.SettledAtMs != t0+4*hourMs || c.MarkPriceQuote != 100_050 || !strings.Contains(c.MarkSourceVI, "mid lấy mẫu") {
		t.Fatalf("stand-in mark %+v", c)
	}
}

func TestServer_IsReadOnlyLabelledPaperAndHasNoGoLive(t *testing.T) {
	db := seed(t, true)
	build := func() (Report, error) {
		return Build(context.Background(), Inputs{DB: db, FromMs: t0 - hourMs, ToMs: t0 + 12*hourMs, NowMs: t0 + 12*hourMs, CapitalQuote: 1_000_000, MarkEvery: time.Hour})
	}
	srv := newServer()
	r, err := build()
	if err != nil {
		t.Fatal(err)
	}
	srv.set(r)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/ledger")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("X-Execution-Mode") != "paper" {
		t.Fatal("JSON must be labelled paper on the wire")
	}
	var got Report
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Mode != "paper" || len(got.Closed) != 1 {
		t.Fatalf("served %+v", got.Mode)
	}

	page, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	body, err := io.ReadAll(page.Body)
	if err != nil {
		t.Fatal(err)
	}
	if page.Header.Get("X-Execution-Mode") != "paper" || !bytes.Equal(body, indexHTML) {
		t.Fatal("the page served must be the embedded UI, labelled paper")
	}
	html := string(body)
	if !strings.Contains(html, "PAPER") || strings.Count(html, `class="paper"`) < 5 {
		t.Fatal("the UI must put PAPER beside its figures")
	}
	for _, forbidden := range []string{"go-live", "go live", "golive", "<button", "<form", "method=\"post\""} {
		if strings.Contains(strings.ToLower(html), forbidden) {
			t.Fatalf("the UI carries %q — a demo must not have a control that could act", forbidden)
		}
	}
	for _, path := range []string{"/api/ledger", "/", "/healthz"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			req, _ := http.NewRequest(method, ts.URL+path, nil)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s answered %d, want 405", method, path, res.StatusCode)
			}
		}
	}
}

func TestResolveFromAndParseStamp(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "started_at")
	if _, _, err := resolveFrom("", filepath.Join(dir, "absent")); err != nil {
		t.Fatalf("a missing started_at must fall back, not fail: %v", err)
	}
	if err := writeFile(file, "2026-09-11 14:15:50 +0700\n"); err != nil {
		t.Fatal(err)
	}
	ms, note, err := resolveFrom("", file)
	if err != nil {
		t.Fatal(err)
	}
	if ms != 1_789_110_950_000 || note != file {
		t.Fatalf("started_at read as %d (%s), want 1789110950000 — the run-2 launch", ms, note)
	}
	ms, _, err = resolveFrom("2026-09-11T07:15:50", file)
	if err != nil || ms != 1_789_110_950_000 {
		t.Fatalf("-from UTC form: %d %v", ms, err)
	}
	if _, err := parseStamp("11/09/2026"); err == nil {
		t.Fatal("accepted a stamp form nobody documented")
	}
}

func buildSeeded(t *testing.T, db *store.Store, markEvery time.Duration) Report {
	t.Helper()
	report, err := Build(context.Background(), Inputs{
		DB: db, FromMs: t0 - hourMs, ToMs: t0 + 12*hourMs, NowMs: t0 + 12*hourMs,
		CapitalQuote: 1_000_000, MarkEvery: markEvery, FromNoteVI: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// Tie order at ONE instant (review 2026-09-11): a settlement stamped at the
// exit instant is collected by the closing position, and an enter row at
// that same instant opens a new position that does NOT collect it.
func TestBuild_TieOrderAtOneInstant_FundingThenCloseThenOpen(t *testing.T) {
	db := seed(t, true)
	ctx := context.Background()
	exitAt := t0 + 10*hourMs
	if _, err := db.PutFundingHistory(ctx, []exchanges.FundingHistoryEntry{
		{Symbol: "BTCUSDT", Source: "binance_futures", Model: exchanges.FundingDiscrete, SettledAtMs: exitAt, RatePerIntervalFrac: 0.0003, IntervalSec: 28800, GapPrevSec: 28800, RatePer8hFrac: 0.0003, MarkPriceQuote: 100_100},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutSignalDecisions(ctx, []store.SignalRecord{
		{EvaluatedAtMs: exitAt, Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot", Action: "enter", NetAPRFrac: 0.03, NetAPROK: true, CostTotalPct: 0.3, ChecksJSON: "[]", ParamsJSON: paramsJSON(t, true)},
	}); err != nil {
		t.Fatal("the journal key is (instant, symbol, perp): the exit row at exitAt must have been replaced by the enter — ", err)
	}
	// The exit row was REPLACED by the enter (same key), so re-add the exit
	// one millisecond earlier to keep close-then-open at (nearly) one instant
	// — and put the settlement exactly at the exit's instant.
	if _, err := db.PutSignalDecisions(ctx, []store.SignalRecord{
		{EvaluatedAtMs: exitAt - 1, Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot", Action: "exit", ChecksJSON: "[]", ParamsJSON: paramsJSON(t, true)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutFundingHistory(ctx, []exchanges.FundingHistoryEntry{
		{Symbol: "BTCUSDT", Source: "binance_futures", Model: exchanges.FundingDiscrete, SettledAtMs: exitAt - 1, RatePerIntervalFrac: 0.0004, IntervalSec: 28800, GapPrevSec: 28800, RatePer8hFrac: 0.0004, MarkPriceQuote: 100_100},
	}); err != nil {
		t.Fatal(err)
	}
	report := buildSeeded(t, db, time.Hour)
	if len(report.Closed) != 1 || len(report.Open) != 1 || report.Anomalies != 0 {
		t.Fatalf("closed %d open %d anomalies %d, want 1 / 1 / 0", len(report.Closed), len(report.Open), report.Anomalies)
	}
	closed, opened := report.Closed[0], report.Open[0]
	// Closed at exitAt−1 collected the t0+8h settlement AND the one stamped
	// at its own closing instant (funding before close at equal stamps).
	if closed.FundingSettlements != 2 || closed.Credits[1].SettledAtMs != exitAt-1 {
		t.Fatalf("closed position credits %+v, want the settlement AT the close instant included", closed.Credits)
	}
	// Opened at exitAt: the settlement stamped exactly at exitAt is NOT its
	// (strictly after the open), so it holds no funding at all.
	if opened.OpenedAtMs != exitAt || opened.FundingSettlements != 0 {
		t.Fatalf("re-opened position %+v: a settlement AT the open instant must not be credited", opened)
	}
	if report.Events[len(report.Events)-1].Kind != paper.EventOpen {
		t.Fatalf("the last event must be the re-open, got %v", report.Events[len(report.Events)-1].Kind)
	}
}

// The 15-minute mark age (review 2026-09-11): with no price sample inside
// 15 minutes before a mark-less settlement, the ledger's own last mark
// stands in — labelled — and the hourly equity mark in that gap is stale.
func TestBuild_MarkAgeRuleAndLedgerFallback(t *testing.T) {
	db := seed(t, true)
	ctx := context.Background()
	gapFrom, gapTo := t0+3*hourMs, t0+4*hourMs+20*60_000
	if _, err := db.DB().ExecContext(ctx, `DELETE FROM price_snapshots WHERE sampled_at_ms > ? AND sampled_at_ms <= ?`, gapFrom, gapTo); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutFundingHistory(ctx, []exchanges.FundingHistoryEntry{
		{Symbol: "BTCUSDT", Source: "binance_futures", Model: exchanges.FundingDiscrete, SettledAtMs: t0 + 4*hourMs, RatePerIntervalFrac: 0.0002, IntervalSec: 28800, GapPrevSec: 28800, RatePer8hFrac: 0.0002, MarkPriceQuote: 0},
	}); err != nil {
		t.Fatal(err)
	}
	report := buildSeeded(t, db, time.Hour)
	p := report.Closed[0]
	if p.FundingSettlements != 2 {
		t.Fatalf("settlements %d, want 2 (the fallback still credits)", p.FundingSettlements)
	}
	c := p.Credits[0]
	if c.SettledAtMs != t0+4*hourMs || !strings.Contains(c.MarkSourceVI, "mark cuối") || c.MarkPriceQuote != 100_050 {
		t.Fatalf("fallback credit %+v: want the ledger's last perp mark, labelled", c)
	}
	stale := 0
	for _, pt := range report.Equity {
		if pt.AtMs == t0+4*hourMs {
			stale = pt.StaleMarks
		}
	}
	if stale != 1 {
		t.Fatalf("the mark inside the sample gap must be stale for the one open position, got %d", stale)
	}
}

// The exit sells the spot the POSITION holds even when the exit row names
// no spot source (registry not refreshed at that tick).
func TestBuild_ClosesTheSpotThePositionHoldsWhenTheExitRowNamesNone(t *testing.T) {
	db := seed(t, true)
	if _, err := db.DB().ExecContext(context.Background(), `UPDATE signal_journal SET spot_source = '' WHERE action = 'exit'`); err != nil {
		t.Fatal(err)
	}
	report := buildSeeded(t, db, time.Hour)
	if len(report.Closed) != 1 || report.Refusals != 0 {
		t.Fatalf("closed %d refusals %d — the position's own spot leg must be sold", len(report.Closed), report.Refusals)
	}
	if report.Closed[0].ExitSpot.Source != "binance_spot" {
		t.Fatalf("exit spot leg on %q, want binance_spot", report.Closed[0].ExitSpot.Source)
	}
}

// With no mark grid the closing mark still lands, so equity is never left
// at the starting capital while cash has moved.
func TestBuild_FinalMarkIsUnconditional(t *testing.T) {
	db := seed(t, true)
	report := buildSeeded(t, db, 0)
	if len(report.Equity) != 1 || report.Equity[0].AtMs != t0+12*hourMs {
		t.Fatalf("equity points %+v, want exactly the closing mark", report.Equity)
	}
	if diff := report.EquityQuote - (1_000_000 + report.RealizedQuote); diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("equity %v, want capital + realized %v", report.EquityQuote, 1_000_000+report.RealizedQuote)
	}
}
