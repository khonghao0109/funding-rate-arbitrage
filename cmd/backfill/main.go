// Command backfill fills the SQLite store with historical funding rates
// (PLAN.md step 2.6) — the corpus the phase-3 backtest replays.
//
//	go run ./cmd/backfill                 # 12 months, every configured pair
//	go run ./cmd/backfill -months 6
//	go run ./cmd/backfill -symbol BTCUSDT
//
// It opens real sockets to seven venues and takes minutes, so it is a command
// rather than something the scanner does at startup. Run it once; after that
// the scanner's hourly top-up keeps the corpus current on its own.
//
// It is safe to re-run at any time. Every row is keyed by (source, symbol,
// settlement), so a second pass over the same window inserts nothing — which is
// also how "restart loses nothing" is demonstrated rather than asserted.
//
// ⚠️ The report it prints is the point, not a courtesy. The corpus is NOT
// uniformly deep, and a backtest that assumed it was would be weighting venues
// by their retention policy. Measured on BTCUSDT, 2026-09-04: Binance, Bybit,
// Kraken and Hyperliquid returned a full 365 days; Gate reached 178.7 (its
// 180-day limit); OKX 92.7 (its ~3-month retention); Paradex 83.3, which is
// this tool's own page budget rather than the venue's — it has no settlements
// at all, only an hourly sample of a funding index, one request per hour. The
// COVERAGE table says how deep each series actually is.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/venues"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/history"
	"futures-arbitrage-scanner/internal/store"
)

// main defers everything to run so that an exit code never skips a deferred
// Close: os.Exit does not run defers, and skipping the store's Close leaves the
// WAL uncheckpointed. SQLite recovers it on the next open, but a tool whose
// failure path leaves a file needing recovery teaches the wrong habit.
func main() { os.Exit(run()) }

func run() int {
	configPath := flag.String("config", "config.yaml", "path to the configuration file")
	months := flag.Int("months", 12, "how far back to ask each venue for")
	only := flag.String("symbol", "", "collect only this pair (default: every configured pair)")
	onlySource := flag.String("source", "", "collect only this source (default: every configured source)")
	dbPath := flag.String("db", "", "database file (default: storage.path from the config)")
	check := flag.Int("check", 0, "read back the last N days instead of collecting; opens no socket")
	prices := flag.Bool("prices", false, "collect hourly PRICE candles (price_history) instead of funding rates")
	flag.Parse()

	if *months < 1 {
		log.Printf("-months must be at least 1, got %d", *months)
		return 1
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Printf("configuration: %v", err)
		return 1
	}
	path := cfg.Storage.Path
	if *dbPath != "" {
		path = *dbPath
	}
	if path == "" {
		log.Printf("no database path: set storage.path in %s or pass -db", *configPath)
		return 1
	}

	// Interrupting stops the fetch between requests and leaves everything
	// already written intact — every batch is its own transaction, so there is
	// no half-collected state to clean up.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(path)
	if err != nil {
		log.Printf("storage: %v", err)
		return 1
	}
	defer db.Close()

	if *check > 0 {
		// Read-only: what does the corpus actually answer for the last N days?
		// It goes through store.FundingHistory, the same query the phase-3
		// backtest will use, rather than through SQL typed at a prompt — the
		// point is that the READER works, not that the rows exist.
		return reportWindow(ctx, db, *only, *check)
	}

	if *prices {
		return backfillPrices(ctx, db, cfg, *only, *onlySource, *months, path)
	}

	collector := history.New(db, jobsFrom(cfg, *only, *onlySource), venues.FundingHistoryFetchers())
	if len(collector.Jobs()) == 0 {
		log.Printf("nothing to collect: no configured source has a funding history fetcher for symbol %q source %q",
			*only, *onlySource)
		return 1
	}

	now := time.Now()
	window := exchanges.FundingWindow{
		StartMs: now.AddDate(0, -*months, 0).UnixMilli(),
		EndMs:   now.UnixMilli(),
	}
	log.Printf("backfill: %s → %s into %s",
		time.UnixMilli(window.StartMs).UTC().Format(time.DateOnly),
		time.UnixMilli(window.EndMs).UTC().Format(time.DateOnly), path)

	results := collector.Collect(ctx, window)
	history.LogResults("backfill", results)
	reportReach(results)

	// Cancellation BETWEEN two series produces no Result for the ones never
	// attempted, so counting errored results alone would let a
	// `timeout N go run ./cmd/backfill` read a truncated corpus as success —
	// and the hourly top-up never re-fills history, only the recent edge.
	if ctx.Err() != nil {
		log.Printf("backfill: interrupted with series unattempted; what was written is intact — re-run to complete")
		return 1
	}

	coverage, err := db.FundingCoverage(ctx)
	if err != nil {
		log.Printf("coverage: %v", err)
		return 1
	}
	reportCoverage(coverage, now)

	// A failed series leaves a permanent hole: the scanner's top-up resumes
	// from the newest stored settlement, so it fills the last day, never the
	// missing year. Re-running this command is the fix, and exiting non-zero is
	// what makes a scripted run notice it needs to — the same contract
	// cmd/fundingcheck has.
	if failed := failedSeries(results); failed > 0 {
		log.Printf("backfill: %d series failed and are NOT filled; re-run to complete them", failed)
		return 1
	}
	return 0
}

// backfillPrices fills price_history, the hourly candles the phase-3 basis exit
// reads. It is a separate path from the funding backfill and not a flag on one
// shared loop: the fetchers, the window type, the table and the set of sources
// all differ — the SPOT sources are collected here and never there, because a
// basis needs both legs.
func backfillPrices(ctx context.Context, db *store.Store, cfg config.Config, only, onlySource string, months int, path string) int {
	collector := history.NewPrices(db, jobsFrom(cfg, only, onlySource), venues.PriceHistoryFetchers())
	if len(collector.Jobs()) == 0 {
		log.Printf("nothing to collect: no configured source has a price history fetcher for symbol %q source %q",
			only, onlySource)
		return 1
	}

	now := time.Now()
	window := exchanges.PriceWindow{
		StartMs: now.AddDate(0, -months, 0).UnixMilli(),
		EndMs:   now.UnixMilli(),
	}
	log.Printf("price backfill: %s → %s into %s",
		time.UnixMilli(window.StartMs).UTC().Format(time.DateOnly),
		time.UnixMilli(window.EndMs).UTC().Format(time.DateOnly), path)

	results := collector.Collect(ctx, window)
	failed := 0
	for _, result := range results {
		switch {
		case result.Err != nil:
			failed++
			log.Printf("price backfill %s/%s: %v", result.Source, result.Symbol, result.Err)
		case result.Fetched == 0:
			log.Printf("price backfill %-20s %-8s no candles in the window", result.Source, result.Symbol)
		default:
			reach := "full"
			if !result.ReachedRequestedStart() {
				// NOT a failure. Hyperliquid's hourly candles reach about 208
				// days and it says so with an empty array — the fact a
				// backtest has to see rather than infer from a short series.
				reach = fmt.Sprintf("SHORT by %.0fd",
					float64(result.OldestOpenMs-window.StartMs)/float64(24*3600*1000))
			}
			log.Printf("price backfill %-20s %-8s %6d fetched, %6d new, %s → %s (%s)",
				result.Source, result.Symbol, result.Fetched, result.Inserted,
				time.UnixMilli(result.OldestOpenMs).UTC().Format(time.DateOnly),
				time.UnixMilli(result.NewestOpenMs).UTC().Format(time.DateOnly), reach)
		}
	}

	if ctx.Err() != nil {
		log.Printf("price backfill: interrupted with series unattempted; what was written is intact — re-run to complete")
		return 1
	}

	coverage, err := db.PriceCoverage(ctx, only)
	if err != nil {
		log.Printf("price coverage: %v", err)
		return 1
	}
	for _, c := range coverage {
		log.Printf("price coverage %-20s %-8s %6d candles  %s → %s  (%.0f days)",
			c.Source, c.Symbol, c.Candles,
			time.UnixMilli(c.FirstOpenMs).UTC().Format(time.DateOnly),
			time.UnixMilli(c.LastOpenMs).UTC().Format(time.DateOnly),
			float64(c.LastOpenMs-c.FirstOpenMs)/float64(24*3600*1000))
	}

	if failed > 0 {
		log.Printf("price backfill: %d series failed and are NOT filled; re-run to complete them", failed)
		return 1
	}
	return 0
}

func failedSeries(results []history.Result) int {
	failed := 0
	for _, result := range results {
		if result.Err != nil {
			failed++
		}
	}
	return failed
}

// jobsFrom builds one collection job per configured source, optionally narrowed
// to a single pair and a single source.
//
// Narrowing by source exists because repairing ONE series is the job that
// actually comes up: a venue rate-limited two of twenty-eight series, and
// re-running the pair would spend two thousand requests re-reading a Paradex
// series that was already complete.
func jobsFrom(cfg config.Config, only, onlySource string) []history.Job {
	jobs := make([]history.Job, 0, len(cfg.Sources))
	for _, source := range cfg.Sources {
		if onlySource != "" && source.Source != onlySource {
			continue
		}
		symbols := make([]exchanges.Symbol, 0, len(cfg.Symbols))
		for _, symbol := range cfg.Symbols {
			if only != "" && symbol.Symbol != only {
				continue
			}
			venueSymbol, ok := source.VenueSymbol(symbol)
			if !ok {
				continue
			}
			symbols = append(symbols, exchanges.Symbol{Standard: symbol.Symbol, Venue: venueSymbol})
		}
		jobs = append(jobs, history.Job{
			Source: source.Source, Connector: source.Connector, Symbols: symbols,
		})
	}
	return jobs
}

// reportReach names the series that came back shorter than asked for, and the
// series whose settlement spacing was not one value.
//
// None of it is a failure, and the three cases are reported apart because they
// mean different things. A short reach is retention or a page budget. A spacing
// that wobbles by a few seconds is venue stamp jitter. A spacing with TWO heavy
// modes is a cadence change, which silently misstates the minority era's
// comparison figures — that one gets a warning.
func reportReach(results []history.Result) {
	var short, outages, mixed []string
	for _, result := range results {
		if result.Err != nil || result.Fetched == 0 {
			continue
		}
		if !result.ReachedRequestedStart() {
			short = append(short, fmt.Sprintf("%s/%s reached %s (asked %s)",
				result.Source, result.Symbol,
				time.UnixMilli(result.OldestAtMs).UTC().Format(time.DateOnly),
				time.UnixMilli(result.RequestedFromMs).UTC().Format(time.DateOnly)))
		}
		line := fmt.Sprintf("%s/%s cadence %ds, gaps %s",
			result.Source, result.Symbol, result.Gaps.ModalGapSec, formatGaps(result.Gaps.Counts))
		switch {
		case result.Gaps.CadenceLooksMixed():
			mixed = append(mixed, line)
		case len(result.Gaps.Counts) > 1:
			outages = append(outages, line)
		}
	}
	if len(short) > 0 {
		// The cause is deliberately NOT asserted here. It is the venue's
		// retention for OKX and Gate, but for Paradex it is this tool's own
		// page budget — one request per hour boundary caps the reach at
		// maxFundingHistoryPages hours. Naming one cause would state the wrong
		// one for a third of the cases.
		log.Printf("backfill: %d series did not reach the requested start "+
			"(venue retention, or this tool's page budget — docs/DATA-REQUIREMENTS.md §9):\n  %s",
			len(short), strings.Join(short, "\n  "))
	}
	if len(outages) > 0 {
		// Not always outages: Gate stamps settlements 1-3 seconds past the hour
		// and the offset drifts, and Paradex samples land within a few seconds
		// of the boundary. Measured 2026-09-04, a year of Gate BTCUSDT shows
		// gaps of 28799-28802s. One cadence, noisy stamps.
		log.Printf("backfill: %d series have a spacing that is not one exact value "+
			"(venue stamp jitter, or a missed settlement — the cadence itself is single):\n  %s",
			len(outages), strings.Join(outages, "\n  "))
	}
	if len(mixed) > 0 {
		// This one is not routine. interval_sec is the series' MODAL spacing,
		// so a corpus spanning a cadence change has its minority era annotated
		// with the majority's interval, and rate_per_8h_frac and apr_frac are
		// derived from that — a 2× error over months, in the figure phase 3
		// ranks on. gap_prev_sec holds the truth per row; a backtest crossing
		// such a series has to read it.
		log.Printf("⚠️  backfill: %d series carry TWO settlement cadences with real weight — "+
			"interval_sec is the modal one, so the other era's rate_per_8h_frac and apr_frac are misstated. "+
			"Use gap_prev_sec for those rows:\n  %s",
			len(mixed), strings.Join(mixed, "\n  "))
	}
}

func formatGaps(counts map[int64]int) string {
	gaps := make([]int64, 0, len(counts))
	for gap := range counts {
		gaps = append(gaps, gap)
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })

	parts := make([]string, 0, len(gaps))
	for _, gap := range gaps {
		parts = append(parts, fmt.Sprintf("%ds×%d", gap, counts[gap]))
	}
	return strings.Join(parts, " ")
}

// reportWindow reads the last `days` days back out of the store and prints one
// line per series: how many settlements, over what span, and the mean rate in
// the cross-venue comparison unit.
//
// GROSS, and labelled so. Nothing in this corpus has a fee, a slippage estimate
// or a borrow cost deducted anywhere (CLAUDE.md rule 2).
func reportWindow(ctx context.Context, db *store.Store, symbol string, days int) int {
	now := time.Now()
	fromMs := now.AddDate(0, 0, -days).UnixMilli()

	rows, err := db.FundingHistory(ctx, symbol, fromMs, now.UnixMilli())
	if err != nil {
		// An exit code, never log.Fatalf: main defers everything to run so an
		// exit cannot skip db.Close(), and os.Exit here skipped exactly that.
		log.Printf("read back: %v", err)
		return 1
	}
	if len(rows) == 0 {
		log.Printf("check: nothing stored for the last %d days", days)
		return 0
	}

	type series struct {
		rows        int
		sumPer8h    float64
		oldest      int64
		newest      int64
		intervalSec int64
		model       string
	}
	byKey := map[string]*series{}
	var order []string
	for _, row := range rows {
		key := row.Symbol + " " + row.Source
		s, ok := byKey[key]
		if !ok {
			s = &series{oldest: row.SettledAtMs, model: string(row.Model)}
			byKey[key] = s
			order = append(order, key)
		}
		s.rows++
		s.sumPer8h += row.RatePer8hFrac
		s.newest = row.SettledAtMs
		s.intervalSec = row.IntervalSec
	}
	sort.Strings(order)

	var b strings.Builder
	fmt.Fprintf(&b, "check: last %d days, %d settlements read back through store.FundingHistory "+
		"(GROSS — no fee, slippage or borrow deducted)", days, len(rows))
	for _, key := range order {
		s := byKey[key]
		fmt.Fprintf(&b, "\n  %-32s %-10s %5d rows  every %5ds  mean %+8.4f bps/8h  %s → %s",
			key, s.model, s.rows, s.intervalSec, s.sumPer8h/float64(s.rows)*10000,
			time.UnixMilli(s.oldest).UTC().Format(time.DateOnly),
			time.UnixMilli(s.newest).UTC().Format(time.DateOnly))
	}
	log.Print(b.String())
	return 0
}

// reportCoverage prints what the corpus holds, read back from the database
// rather than from what was just fetched — the difference is what a re-run
// proves.
func reportCoverage(coverage []store.Coverage, now time.Time) {
	if len(coverage) == 0 {
		log.Printf("coverage: the store is empty")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "coverage: %d series (GROSS rates — no fee, slippage or borrow is deducted anywhere in this corpus)", len(coverage))
	for _, c := range coverage {
		days := float64(c.NewestAtMs-c.OldestAtMs) / float64(24*time.Hour/time.Millisecond)
		fmt.Fprintf(&b, "\n  %-9s %-20s %-10s %6d rows  %6.1f days  %s → %s",
			c.Symbol, c.Source, c.Model, c.Rows, days,
			time.UnixMilli(c.OldestAtMs).UTC().Format(time.DateOnly),
			time.UnixMilli(c.NewestAtMs).UTC().Format(time.DateOnly))
	}
	log.Print(b.String())
}
