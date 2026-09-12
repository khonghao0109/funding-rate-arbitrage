package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/fees"
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

	rec := journalRecord(d, strategy.Candidate{}, p)
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
	rec := journalRecord(d, strategy.Candidate{}, strategy.Params{})
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
	if err := json.Unmarshal([]byte(journalRecord(d, strategy.Candidate{}, p).ParamsJSON), &params); err != nil {
		t.Fatal(err)
	}
	if params["min_trailing_mean_bps"] != 0.5 || params["trailing_mean_days"] != 90.0 || params["trailing_mean_min_cost_frac"] != 1.0 {
		t.Errorf("params_json must carry the three selection keys: %v", params)
	}
	// And what the row was decided on (2026-09-10): the newest settlement
	// stamp and the usable row count the evaluator saw.
	d.NewestSettledAtMs, d.SettledRows = 1_757_000_000_000, 42
	if err := json.Unmarshal([]byte(journalRecord(d, strategy.Candidate{}, p).ParamsJSON), &params); err != nil {
		t.Fatal(err)
	}
	in, _ := params["inputs"].(map[string]any)
	if in["newest_settled_at_ms"] != 1_757_000_000_000.0 || in["settled_rows"] != 42.0 {
		t.Errorf("params_json.inputs must carry the newest stamp and the row count: %v", params["inputs"])
	}
}

// PLAN 3.5 ④: the journal process loads its fee schedules ONCE at start-up
// and the strategy parameters alone cannot show that the file on disk has
// since verified a venue. So every row carries the fee state of BOTH legs it
// was judged with — source, taker bps, verified — under params_json.fees,
// and the gate's step 2 can catch a fee drift by machine.
func TestJournalRecord_CarriesTheFeeStateOfBothLegs(t *testing.T) {
	d := strategy.Decision{At: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC), Symbol: "BTCUSDT",
		PerpSource: "bybit_futures", SpotSource: "binance_spot", Action: strategy.ActionSkip}
	c := strategy.Candidate{Symbol: "BTCUSDT", PerpSource: "bybit_futures", SpotSource: "binance_spot",
		SpotFee: fees.Schedule{Source: "binance_spot", TakerFeeBps: 10, Verified: true},
		PerpFee: fees.Schedule{Source: "bybit_futures", TakerFeeBps: 5.5, Verified: false}}
	var params map[string]any
	if err := json.Unmarshal([]byte(journalRecord(d, c, strategy.Params{}).ParamsJSON), &params); err != nil {
		t.Fatal(err)
	}
	fs, ok := params["fees"].(map[string]any)
	if !ok {
		t.Fatalf("params_json must carry a fees object: %v", params)
	}
	spot, perp := fs["spot"].(map[string]any), fs["perp"].(map[string]any)
	if spot["source"] != "binance_spot" || spot["taker_bps"] != 10.0 || spot["verified"] != true {
		t.Errorf("spot leg fee state lost: %v", spot)
	}
	if perp["source"] != "bybit_futures" || perp["taker_bps"] != 5.5 || perp["verified"] != false {
		t.Errorf("perp leg fee state lost: %v", perp)
	}

	// No spot leg → no spot schedule: null, never a zero-fee schedule.
	c.SpotSource, c.SpotFee = "", fees.Schedule{}
	d.SpotSource = ""
	if err := json.Unmarshal([]byte(journalRecord(d, c, strategy.Params{}).ParamsJSON), &params); err != nil {
		t.Fatal(err)
	}
	if params["fees"].(map[string]any)["spot"] != nil {
		t.Errorf("a missing spot leg must journal null, got %v", params["fees"])
	}
}

// The seed reads params_json for the notional it re-opens a position with;
// since 2026-09-10 that JSON also carries a nested fees object and booleans.
// encoding/json's best-effort decode kept the notional even into the old
// map[string]float64 (the object was skipped with an ignored error), so this
// guards the shape against a stricter decoder rather than pinning a fix.
func TestPaperBook_SeedReadsTheNotionalBesideNonNumericParams(t *testing.T) {
	db := openTempStore(t)
	ctx := context.Background()
	rows := []store.SignalRecord{{EvaluatedAtMs: 1_757_000_000_000, Symbol: "BTCUSDT", PerpSource: "binance_futures",
		SpotSource: "binance_spot", Action: "enter", ChecksJSON: "[]",
		ParamsJSON: `{"notional_quote":50000,"fees":{"spot":{"source":"binance_spot","taker_bps":10,"verified":true},"perp":null}}`}}
	if _, err := db.PutSignalDecisions(ctx, rows); err != nil {
		t.Fatal(err)
	}
	book := newPaperBook()
	if err := book.seed(ctx, db); err != nil {
		t.Fatal(err)
	}
	if pos, open := book.position("BTCUSDT", "binance_futures"); !open || pos.NotionalQuote != 50000 {
		t.Errorf("the notional must survive non-numeric siblings: open=%v %+v", open, pos)
	}
}

// The live path reads back as much settled history as the trailing-mean
// horizon needs, never less than the 30-day floor (PLAN 3.5, debt of
// 2026-09-10): with the floor alone, trailing_mean_days above 30 was refused
// live ("chưa phủ") and judged in the replay.
func TestSettledLookbackFor_FollowsTheTrailingHorizon(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		name string
		days float64
		want time.Duration
	}{
		{"selection off keeps the floor", 0, 30 * day},
		{"a horizon inside the floor keeps the floor", 20, 30 * day},
		{"a horizon at the floor still gets its week of slack", 30, 37 * day},
		{"a 90-day horizon reads 97 days", 90, 97 * day},
		{"the 180-day horizon the grid offers reads 187 days", 180, 187 * day},
		{"fractional days are honoured", 45.5, time.Duration(45.5*24*float64(time.Hour)) + 7*day},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := settledLookbackFor(config.Strategy{TrailingMeanDays: tc.days})
			if got != tc.want {
				t.Fatalf("lookback for %g days = %s, want %s", tc.days, got, tc.want)
			}
		})
	}
}

// End to end on a temporary store: with trailing_mean_days 60 the live path
// must read 60 days back and JUDGE the mean, not refuse for lack of
// coverage — which is exactly what the 30-day floor alone produced.
func TestEvaluateOnce_ReadsEnoughHistoryForTheTrailingMean(t *testing.T) {
	ctx := context.Background()
	db := openTempStore(t)
	at := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	// 70 days of 8h settlements ending an hour before the evaluation.
	var rows []exchanges.FundingHistoryEntry
	for i := 0; i < 70*3; i++ {
		rows = append(rows, exchanges.FundingHistoryEntry{
			Symbol: "BTCUSDT", Source: "binance_futures", Model: exchanges.FundingDiscrete,
			SettledAtMs: at.Add(-time.Hour).Add(-time.Duration(i) * 8 * time.Hour).UnixMilli(),
			IntervalSec: 28800, GapPrevSec: 28800, RatePerIntervalFrac: 0.0001, RatePer8hFrac: 0.0001, APRFrac: 0.1095,
			RawRateField: "fundingRate", RawRate: 0.0001,
		})
	}
	if _, err := db.PutFundingHistory(ctx, rows); err != nil {
		t.Fatal(err)
	}

	cfg := signalsFixtureConfig()
	cfg.Strategy.MinTrailingMeanBps, cfg.Strategy.TrailingMeanDays = 0.1, 60
	s := scanner.New([]string{"BTCUSDT"})
	s.SetHedges([]scanner.HedgeLeg{{Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot"}})

	evaluateOnce(ctx, cfg, paramsFrom(cfg.Strategy), s, db, newPaperBook(), at)

	journal, err := db.SignalDecisions(ctx, 0, at.Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if len(journal) != 1 {
		t.Fatalf("journal has %d rows, want 1", len(journal))
	}
	var checks []struct {
		Name     string `json:"name"`
		Passed   bool   `json:"passed"`
		DetailVI string `json:"detail_vi"`
	}
	if err := json.Unmarshal([]byte(journal[0].ChecksJSON), &checks); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range checks {
		if c.Name != "trailing_mean" {
			continue
		}
		found = true
		if strings.Contains(c.DetailVI, "chưa phủ") {
			t.Fatalf("the live path still refuses a 60-day horizon for lack of coverage: %s", c.DetailVI)
		}
		if !c.Passed || !strings.Contains(c.DetailVI, "180 mốc trong 60 ngày") {
			t.Fatalf("trailing_mean should be judged over the 180 settlements of the last 60 days and pass at 1.0 bps: passed=%v %s", c.Passed, c.DetailVI)
		}
	}
	if !found {
		t.Fatal("no trailing_mean check in the journal row")
	}
}
