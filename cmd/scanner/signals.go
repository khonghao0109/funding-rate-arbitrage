package main

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/fees"
	"futures-arbitrage-scanner/internal/risk"
	"futures-arbitrage-scanner/internal/scanner"
	"futures-arbitrage-scanner/internal/store"
	"futures-arbitrage-scanner/internal/strategy"
)

// The live signal path (step 3.5).
//
// On a fixed cadence it hands package strategy the same inputs the backtest
// hands it — settled history from the store, fee schedules from config, the
// newest measured books from the scanner — plus the one thing the backtest can
// never have: LIVE spot and perp prices, which is what makes the basis exit
// evaluable here and nowhere else. Every decision is written to the signal
// journal with its full reasoning, and that journal is the paper-trading
// record the step-3.5 gate compares against a backtest over the same window.
//
// Positions here are PAPER positions: nothing is placed (phase 4 has not
// started, and this process holds no credentials). They are seeded from the
// journal at startup so a restart continues the same paper book instead of
// re-entering everything it was already holding.
//
// Nothing in this file blocks the data path: it runs on its own goroutine, on
// its own ticker, and a store error is logged and skipped.

// signalWarmup lets the depth sweep (90s warmup) and the first funding top-up
// land before the first evaluation, so the first journal rows are not a
// column of "no book" and "no history".
const signalWarmup = 3 * time.Minute

// settledLookback bounds how much history each evaluation reads. Thirty days
// covers any persistence window a config would set at hourly cadence (720
// settlements) with room to spare, and keeps the per-tick query on the
// funding_history_by_symbol index rather than a year of rows.
const settledLookback = 30 * 24 * time.Hour

func startSignals(ctx context.Context, cfg config.Config, s *scanner.Scanner, db *store.Store, start func(func())) {
	if !cfg.Strategy.Enabled {
		log.Printf("strategy: disabled; nothing is evaluated and the signal journal stays empty")
		return
	}
	if db == nil {
		log.Printf("strategy: enabled but storage is off — no settled history to decide on and nowhere to journal; not started")
		return
	}
	params := paramsFrom(cfg.Strategy)
	every := time.Duration(cfg.Strategy.EvaluateEveryMin) * time.Minute
	log.Printf("strategy: evaluating every %s · %.2f bps/8h held %d · net APR ≥ %.2f%% · %.0f quote · hold %.0f days · exit < %.2f%% for %d",
		every, params.MinRatePer8hBps, params.PersistencePeriods, params.MinNetAPRFrac*100,
		params.NotionalQuote, params.HoldingDays, params.ExitNetAPRFrac*100, params.ExitPersistencePeriods)

	book := newPaperBook()
	if err := book.seed(ctx, db); err != nil {
		log.Printf("strategy: could not seed paper positions from the journal: %v — starting flat", err)
	}

	start(func() {
		tickLoop(ctx, signalWarmup, every, func(at time.Time) {
			evaluateOnce(ctx, cfg, params, s, db, book, at.UTC())
		})
	})
}

// evaluateOnce runs one pass over every hedge leg and journals every decision.
func evaluateOnce(ctx context.Context, cfg config.Config, params strategy.Params,
	s *scanner.Scanner, db *store.Store, book *paperBook, at time.Time) {

	legs := s.Hedges()
	if len(legs) == 0 {
		log.Printf("strategy: no hedge legs yet (registry has not refreshed) — nothing evaluated")
		return
	}
	rows, err := settledSince(ctx, db, cfg, at.Add(-settledLookback), at)
	if err != nil {
		log.Printf("strategy: read settled history: %v — skipping this tick", err)
		return
	}
	candidates := buildCandidates(cfg, legs, rows, s.DepthSnapshot(), s.PriceSnapshot(), latestFundingVI(s.FundingSnapshot()))

	var records []store.SignalRecord
	for _, c := range candidates {
		var d strategy.Decision
		if pos, holding := book.position(c.Symbol, c.PerpSource); holding {
			d = strategy.EvaluateExit(at, pos, c, params)
			if d.Action == strategy.ActionExit {
				book.close(c.Symbol, c.PerpSource)
			}
		} else {
			d = strategy.EvaluateEntry(at, c, params)
			if d.Action == strategy.ActionEnter {
				book.open(c, at, params)
			}
		}
		records = append(records, journalRecord(d, params))
		log.Printf("strategy:\n  %s", strings.Join(d.LogLines(), "\n  "))
	}
	if _, err := db.PutSignalDecisions(ctx, records); err != nil {
		log.Printf("strategy: journal write failed: %v (decisions above were logged, not stored)", err)
	}
}

// settledSince reads every configured symbol's settled history in one window.
// One query per symbol keeps each read on the (symbol, funding_at_ms) index.
func settledSince(ctx context.Context, db *store.Store, cfg config.Config, from, to time.Time) ([]store.FundingRow, error) {
	var out []store.FundingRow
	for _, symbol := range cfg.Symbols {
		rows, err := db.FundingHistory(ctx, symbol.Symbol, from.UnixMilli(), to.UnixMilli())
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// buildCandidates assembles one strategy.Candidate per perp leg — refusals
// included, so a perp with no hedge reaches strategy as a refusal and the log
// says why in the venue's words rather than the pair vanishing.
func buildCandidates(cfg config.Config, legs []scanner.HedgeLeg, rows []store.FundingRow,
	books []depth.Summary, prices []scanner.PriceReading, latestVI map[string]string) []strategy.Candidate {

	settled := make(map[string][]exchanges.FundingHistoryEntry)
	for _, row := range rows {
		k := key(row.Symbol, row.Source)
		settled[k] = append(settled[k], row.FundingHistoryEntry)
	}
	for k := range settled {
		entries := settled[k]
		sort.Slice(entries, func(i, j int) bool { return entries[i].SettledAtMs < entries[j].SettledAtMs })
	}
	bookBy := make(map[string]depth.Summary, len(books))
	for _, b := range books {
		bookBy[key(b.Symbol, b.Source)] = b
	}
	priceBy := make(map[string]float64, len(prices))
	for _, p := range prices {
		priceBy[key(p.Symbol, p.Source)] = p.Price
	}

	var out []strategy.Candidate
	for _, leg := range legs {
		if !isPerpSource(cfg, leg.PerpSource) {
			continue
		}
		c := strategy.Candidate{
			Symbol: leg.Symbol, PerpSource: leg.PerpSource, SpotSource: leg.SpotSource,
			HedgeNoteVI:    leg.NoteVI,
			Settled:        settled[key(leg.Symbol, leg.PerpSource)],
			LatestVI:       latestVI[key(leg.Symbol, leg.PerpSource)],
			PerpFee:        schedule(cfg, leg.PerpSource),
			PerpBook:       bookBy[key(leg.Symbol, leg.PerpSource)],
			PerpPriceQuote: priceBy[key(leg.Symbol, leg.PerpSource)],
			PerpMargin:     marginBracket(cfg, leg.PerpSource),
		}
		if leg.SpotSource != "" {
			c.SpotFee = schedule(cfg, leg.SpotSource)
			c.SpotBook = bookBy[key(leg.Symbol, leg.SpotSource)]
			c.SpotPriceQuote = priceBy[key(leg.Symbol, leg.SpotSource)]
		}
		out = append(out, c)
	}
	return out
}

// latestFundingVI renders each live reading for the log. It is carried, never
// computed with: the decision runs on settled history (strategy/signal.go).
func latestFundingVI(readings []exchanges.FundingData) map[string]string {
	out := make(map[string]string, len(readings))
	for _, r := range readings {
		state := "đã chốt"
		if r.IsEstimated {
			state = "đang hình thành"
		}
		out[key(r.Symbol, r.Source)] = strings.TrimSpace(strings.Join([]string{
			"live", state, exchangeRateVI(r),
		}, " · "))
	}
	return out
}

func exchangeRateVI(r exchanges.FundingData) string {
	return strings.TrimSpace(strings.Join([]string{
		formatBps(r.RatePer8hFrac), "bps/8h", "(", string(r.Model), ")",
	}, " "))
}

func formatBps(frac float64) string { return jsonNumber(frac * 10000) }

func jsonNumber(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// marginBracket is the perp venue's maintenance bracket as config declares it.
// An unverified one makes the margin condition refuse to state a liquidation
// price rather than assume there is none — see internal/risk.
func marginBracket(cfg config.Config, source string) risk.Bracket {
	src, ok := cfg.SourceByName(source)
	if !ok {
		return risk.Bracket{Source: source}
	}
	return src.Margin.Bracket(source)
}

func schedule(cfg config.Config, source string) fees.Schedule {
	src, _ := cfg.SourceByName(source)
	return fees.Schedule{Source: source, MakerFeeBps: src.Fee.MakerBps,
		TakerFeeBps: src.Fee.TakerBps, Verified: src.Fee.Verified}
}

func key(symbol, source string) string { return symbol + "|" + source }

func paramsFrom(st config.Strategy) strategy.Params { return st.StrategyParams() }

// journalRecord flattens a decision for the store, reasoning included.
func journalRecord(d strategy.Decision, p strategy.Params) store.SignalRecord {
	type checkJSON struct {
		Name     string `json:"name"`
		Passed   bool   `json:"passed"`
		DetailVI string `json:"detail_vi"`
	}
	checks := make([]checkJSON, 0, len(d.Checks))
	for _, c := range d.Checks {
		checks = append(checks, checkJSON{c.Name, c.Passed, c.DetailVI})
	}
	checksJSON, _ := json.Marshal(checks)
	paramsJSON, _ := json.Marshal(map[string]any{
		"min_rate_per_8h_bps": p.MinRatePer8hBps, "persistence_periods": p.PersistencePeriods,
		"min_net_apr_frac": p.MinNetAPRFrac, "notional_quote": p.NotionalQuote, "holding_days": p.HoldingDays,
		"max_book_age_min": p.MaxBookAge.Minutes(), "exit_net_apr_frac": p.ExitNetAPRFrac,
		"exit_persistence_periods": p.ExitPersistencePeriods,
		"exit_negative_min_bps":    p.ExitNegativeMinBps, "exit_negative_periods": p.EffectiveExitNegativePeriods(),
		"exit_negative_cum_cost_frac":  p.ExitNegativeCumCostFrac,
		"min_hold_recovered_cost_frac": p.MinHoldRecoveredCostFrac,
		"max_basis_pct":                p.MaxBasisPct, "max_basis_widen_pct": p.MaxBasisWidenPct,
		"min_trailing_mean_bps": p.MinTrailingMeanBps, "trailing_mean_days": p.TrailingMeanDays,
		"trailing_mean_min_cost_frac": p.TrailingMeanMinCostFrac,
	})
	rec := store.SignalRecord{
		EvaluatedAtMs: d.At.UnixMilli(), Symbol: d.Symbol, PerpSource: d.PerpSource, SpotSource: d.SpotSource,
		Action: string(d.Action), ChecksJSON: string(checksJSON), ParamsJSON: string(paramsJSON),
	}
	// A refused figure stays 0 beside its false flag; it must never be able to
	// read as a rate of zero, let alone as the stale number it computed.
	if d.NetAPR.OK {
		rec.NetAPROK, rec.NetAPRFrac = true, d.NetAPR.NetAPRFrac
	}
	if d.Cost.OK {
		rec.CostTotalPct = d.Cost.TotalPct
	}
	return rec
}

// paperBook is the set of open PAPER positions, keyed by symbol|perp.
type paperBook struct {
	mu        sync.Mutex
	positions map[string]strategy.Position
}

func newPaperBook() *paperBook { return &paperBook{positions: map[string]strategy.Position{}} }

// seed rebuilds the open set from the journal: for each market, the newest row
// decides — an "enter" or "hold" means the paper position is still open.
func (b *paperBook) seed(ctx context.Context, db *store.Store) error {
	rows, err := db.SignalDecisions(ctx, 0, time.Now().Add(time.Hour).UnixMilli())
	if err != nil {
		return err
	}
	latest := map[string]store.SignalRecord{}
	opened := map[string]int64{}
	for _, r := range rows { // oldest first, so the last write per key wins
		k := key(r.Symbol, r.PerpSource)
		if r.Action == string(strategy.ActionEnter) {
			opened[k] = r.EvaluatedAtMs
		}
		latest[k] = r
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, r := range latest {
		if r.Action == string(strategy.ActionEnter) || r.Action == string(strategy.ActionHold) {
			var params map[string]float64
			_ = json.Unmarshal([]byte(r.ParamsJSON), &params)
			b.positions[k] = strategy.Position{
				Symbol: r.Symbol, PerpSource: r.PerpSource, SpotSource: r.SpotSource,
				OpenedAtMs: opened[k], NotionalQuote: params["notional_quote"],
			}
		}
	}
	if len(b.positions) > 0 {
		log.Printf("strategy: resumed %d paper position(s) from the journal", len(b.positions))
	}
	return nil
}

func (b *paperBook) position(symbol, perp string) (strategy.Position, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	pos, ok := b.positions[key(symbol, perp)]
	return pos, ok
}

func (b *paperBook) open(c strategy.Candidate, at time.Time, p strategy.Params) {
	var entryBasisPct float64
	if c.SpotPriceQuote > 0 && c.PerpPriceQuote > 0 {
		entryBasisPct = (c.PerpPriceQuote - c.SpotPriceQuote) / c.SpotPriceQuote * 100
	}
	b.mu.Lock()
	b.positions[key(c.Symbol, c.PerpSource)] = strategy.Position{
		Symbol: c.Symbol, PerpSource: c.PerpSource, SpotSource: c.SpotSource,
		OpenedAtMs: at.UnixMilli(), NotionalQuote: p.NotionalQuote, EntryBasisPct: entryBasisPct,
	}
	b.mu.Unlock()
}

func (b *paperBook) close(symbol, perp string) {
	b.mu.Lock()
	delete(b.positions, key(symbol, perp))
	b.mu.Unlock()
}
