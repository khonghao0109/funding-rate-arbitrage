package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/scanner"
	"futures-arbitrage-scanner/internal/store"
	"futures-arbitrage-scanner/internal/strategy"
)

func signalsFixtureConfig() config.Config {
	return config.Config{
		Strategy: config.Strategy{
			Enabled: true, EvaluateEveryMin: 10, MaxBookAgeMin: 120,
			MinRatePer8hBps: 0.5, PersistencePeriods: 3, MinNetAPRFrac: 0.02,
			NotionalQuote: 50_000, HoldingDays: 30,
			ExitNetAPRFrac: 0.005, ExitPersistencePeriods: 3, MaxBasisPct: 1, MaxBasisWidenPct: 0.5,
		},
		Symbols: []config.Symbol{{Symbol: "BTCUSDT", Base: "BTC", Quote: "USDT"}},
		Sources: []config.Source{
			{Source: "binance_futures", MarketType: "perp", QuoteAsset: "USDT", Tradable: true,
				Fee: config.Fee{TakerBps: 5, Verified: true}},
			{Source: "binance_spot", MarketType: "spot", QuoteAsset: "USDT", Tradable: true,
				Fee: config.Fee{TakerBps: 10, Verified: true}},
			{Source: "kraken_futures", MarketType: "perp", QuoteAsset: "USD", Tradable: true,
				Fee: config.Fee{TakerBps: 5, Verified: true}},
		},
	}
}

func rowsFor(source string, n int) []store.FundingRow {
	out := make([]store.FundingRow, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, store.FundingRow{FundingHistoryEntry: exchanges.FundingHistoryEntry{
			Symbol: "BTCUSDT", Source: source, Model: exchanges.FundingDiscrete,
			SettledAtMs: int64(1_757_000_000_000 + i*28_800_000), IntervalSec: 28800,
			RatePerIntervalFrac: 0.0001, RatePer8hFrac: 0.0001,
		}})
	}
	return out
}

// The evaluator must hand strategy exactly what the backtest hands it — same
// history slice per venue, the leg's chosen spot, the config's fees, the
// scanner's newest books — and, unlike the backtest, LIVE prices, so the basis
// exit is evaluable here and only here.
func TestBuildCandidates_AssemblesOnePerHedgeableLegWithLivePrices(t *testing.T) {
	cfg := signalsFixtureConfig()
	legs := []scanner.HedgeLeg{
		{Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot"},
		{Symbol: "BTCUSDT", PerpSource: "kraken_futures", NoteVI: "no spot shares quote USD"},
	}
	// Mixed venues, deliberately unordered, so the filter and the sort are both exercised.
	rows := append(rowsFor("kraken_futures", 2), rowsFor("binance_futures", 5)...)
	rows[len(rows)-1], rows[2] = rows[2], rows[len(rows)-1]
	books := []depth.Summary{
		{Symbol: "BTCUSDT", Source: "binance_spot", SampledAtMs: 1, MidPriceQuote: 100},
		{Symbol: "BTCUSDT", Source: "binance_futures", SampledAtMs: 1, MidPriceQuote: 100},
	}
	prices := []scanner.PriceReading{
		{Symbol: "BTCUSDT", Source: "binance_spot", PricePoint: scanner.PricePoint{Price: 81050}},
		{Symbol: "BTCUSDT", Source: "binance_futures", PricePoint: scanner.PricePoint{Price: 81043}},
	}

	got := buildCandidates(cfg, legs, rows, books, prices, nil)
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2 (one per perp leg, refusals included)", len(got))
	}

	var binance, kraken strategy.Candidate
	for _, c := range got {
		switch c.PerpSource {
		case "binance_futures":
			binance = c
		case "kraken_futures":
			kraken = c
		}
	}
	if binance.SpotSource != "binance_spot" {
		t.Errorf("spot leg = %q, want the mapping's choice", binance.SpotSource)
	}
	if len(binance.Settled) != 5 {
		t.Errorf("settled = %d rows, want only binance_futures' 5", len(binance.Settled))
	}
	for i := 1; i < len(binance.Settled); i++ {
		if binance.Settled[i].SettledAtMs < binance.Settled[i-1].SettledAtMs {
			t.Fatal("settled history must be oldest first — that is what strategy and the backtest both assume")
		}
	}
	if binance.SpotFee.TakerFeeBps != 10 || !binance.SpotFee.Verified || binance.PerpFee.TakerFeeBps != 5 {
		t.Errorf("fees not taken from config: spot %+v perp %+v", binance.SpotFee, binance.PerpFee)
	}
	if binance.SpotBook.Source != "binance_spot" || binance.PerpBook.Source != "binance_futures" {
		t.Errorf("books not matched by source: spot %q perp %q", binance.SpotBook.Source, binance.PerpBook.Source)
	}
	if binance.SpotPriceQuote != 81050 || binance.PerpPriceQuote != 81043 {
		t.Errorf("live prices not attached: spot %v perp %v", binance.SpotPriceQuote, binance.PerpPriceQuote)
	}
	if kraken.SpotSource != "" || kraken.HedgeNoteVI == "" {
		t.Error("a refused leg must reach strategy as a refusal with its note, so the log says why")
	}
}

func TestJournalRecord_KeepsEveryCheckAndTheParams(t *testing.T) {
	at := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	d := strategy.Decision{
		At: at, Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot",
		Action: strategy.ActionSkip,
		Checks: []strategy.Check{{Name: "hedge_leg", Passed: true, DetailVI: "ok"},
			{Name: "persistence", Passed: false, DetailVI: "2/3 mốc"}},
		NetAPR: strategy.NetAPRResult{OK: true, NetAPRFrac: 0.0571},
		Cost:   strategy.RoundTrip{OK: true, TotalPct: 0.301},
	}
	p := strategy.Params{MinRatePer8hBps: 0.5, PersistencePeriods: 3}

	rec := journalRecord(d, p)
	if rec.EvaluatedAtMs != at.UnixMilli() || rec.Action != "skip" || rec.NetAPRFrac != 0.0571 || rec.CostTotalPct != 0.301 {
		t.Errorf("record lost a field: %+v", rec)
	}
	var checks []map[string]any
	if err := json.Unmarshal([]byte(rec.ChecksJSON), &checks); err != nil || len(checks) != 2 {
		t.Fatalf("checks_json must be a JSON list of every check: %v %q", err, rec.ChecksJSON)
	}
	if checks[1]["name"] != "persistence" || checks[1]["passed"] != false || checks[1]["detail_vi"] != "2/3 mốc" {
		t.Errorf("check lost its reasoning: %v", checks[1])
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(rec.ParamsJSON), &params); err != nil || params["min_rate_per_8h_bps"] != 0.5 {
		t.Errorf("params_json must carry the unit-suffixed thresholds: %v %q", err, rec.ParamsJSON)
	}
}

// A journal row with net_apr_ok false must carry 0, never a stale number.
func TestJournalRecord_RefusedNetAPRIsZeroNotANumber(t *testing.T) {
	d := strategy.Decision{At: time.Unix(1, 0), Action: strategy.ActionSkip,
		NetAPR: strategy.NetAPRResult{OK: false, NetAPRFrac: 0.99}}
	rec := journalRecord(d, strategy.Params{})
	if rec.NetAPROK || rec.NetAPRFrac != 0 {
		t.Errorf("refused net APR leaked as a number: %+v", rec)
	}
}

// A restart must continue the same paper book: the newest journal row per
// market decides whether a position is still open, and the OpenedAtMs comes
// from the row that entered it, not from the restart.
func TestPaperBook_SeedsOpenPositionsFromTheJournal(t *testing.T) {
	db := openTempStore(t)
	ctx := context.Background()
	entered := int64(1_757_000_000_000)
	rows := []store.SignalRecord{
		{EvaluatedAtMs: entered, Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot",
			Action: "enter", ParamsJSON: `{"notional_quote":50000}`, ChecksJSON: "[]"},
		{EvaluatedAtMs: entered + 1, Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot",
			Action: "hold", ParamsJSON: `{"notional_quote":50000}`, ChecksJSON: "[]"},
		{EvaluatedAtMs: entered, Symbol: "ETHUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot",
			Action: "enter", ParamsJSON: `{"notional_quote":50000}`, ChecksJSON: "[]"},
		{EvaluatedAtMs: entered + 1, Symbol: "ETHUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot",
			Action: "exit", ParamsJSON: `{"notional_quote":50000}`, ChecksJSON: "[]"},
		{EvaluatedAtMs: entered, Symbol: "SOLUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot",
			Action: "skip", ParamsJSON: `{}`, ChecksJSON: "[]"},
	}
	if _, err := db.PutSignalDecisions(ctx, rows); err != nil {
		t.Fatal(err)
	}
	book := newPaperBook()
	if err := book.seed(ctx, db); err != nil {
		t.Fatal(err)
	}
	pos, open := book.position("BTCUSDT", "binance_futures")
	if !open || pos.OpenedAtMs != entered || pos.NotionalQuote != 50000 {
		t.Errorf("BTC (enter→hold) must resume OPEN from the enter row: open=%v %+v", open, pos)
	}
	if _, open := book.position("ETHUSDT", "binance_futures"); open {
		t.Error("ETH (enter→exit) must resume CLOSED")
	}
	if _, open := book.position("SOLUSDT", "binance_futures"); open {
		t.Error("SOL (skip) was never open")
	}
}

func openTempStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "scanner.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The journal's params_json is the authority on which rule a row was judged
// by (PLAN 3.5 ③), so every selection key is in it — the absolute floor, its
// horizon, and the cost-crossing fraction added 2026-09-10.
func TestJournalRecord_CarriesEverySelectionKey(t *testing.T) {
	d := strategy.Decision{At: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC), Symbol: "BTCUSDT",
		PerpSource: "binance_futures", SpotSource: "binance_spot", Action: strategy.ActionSkip}
	p := strategy.Params{MinTrailingMeanBps: 0.5, TrailingMeanDays: 90, TrailingMeanMinCostFrac: 1.0}
	var params map[string]any
	if err := json.Unmarshal([]byte(journalRecord(d, p).ParamsJSON), &params); err != nil {
		t.Fatal(err)
	}
	if params["min_trailing_mean_bps"] != 0.5 || params["trailing_mean_days"] != 90.0 || params["trailing_mean_min_cost_frac"] != 1.0 {
		t.Errorf("params_json must carry the three selection keys: %v", params)
	}
}
