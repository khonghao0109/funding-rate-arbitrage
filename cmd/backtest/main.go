// Command backtest replays the production entry and exit rules over the stored
// funding corpus and reports what they would have done (PLAN.md step 3.3).
//
//	go run ./cmd/backtest                          # 6 months, every hedgeable pair
//	go run ./cmd/backtest -months 12 -symbol BTCUSDT
//	go run ./cmd/backtest -sweep -csv out.csv      # parameter sweep to CSV
//
// It reads the SQLite corpus and writes nothing back to it. What it can and
// cannot model is stated by the run itself: depth cannot be backfilled, so the
// cost is one measured book held fixed, and the basis exit is not evaluable at
// all. Those caveats print with every report and are not optional.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/backtest"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/fees"
	"futures-arbitrage-scanner/internal/instruments"
	"futures-arbitrage-scanner/internal/store"
	"futures-arbitrage-scanner/internal/strategy"
)

func main() {
	configPath := flag.String("config", "config.yaml", "configuration file")
	months := flag.Int("months", 6, "how many months back to replay")
	only := flag.String("symbol", "", "replay one symbol only")
	sweep := flag.Bool("sweep", false, "sweep the entry threshold and persistence grid")
	csvPath := flag.String("csv", "", "also write the results to this CSV file")
	notional := flag.Float64("notional", 50000, "position size in the quote asset")
	holdDays := flag.Float64("hold-days", 30, "expected holding period, for amortizing the round trip")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}
	db, err := store.Open(cfg.Storage.Path)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	window := backtest.Window{
		FromMs: now.AddDate(0, -*months, 0).UnixMilli(),
		ToMs:   now.UnixMilli(),
	}

	series, err := buildSeries(ctx, db, cfg, *only, window)
	if err != nil {
		log.Fatalf("build series: %v", err)
	}
	if len(series) == 0 {
		log.Fatalf("no hedgeable series to replay — every perp was refused by the hedge mapping " +
			"(a series with an unverified fee schedule is built, then refused BY NAME inside Run)")
	}

	grid := []strategy.Params{baseParams(*notional, *holdDays)}
	if *sweep {
		grid = sweepGrid(*notional, *holdDays)
	}

	results := backtest.Sweep(series, window, grid)

	fmt.Printf("BACKTEST %d tháng · %s → %s · %d chuỗi × %d bộ tham số\n\n",
		*months, stamp(window.FromMs), stamp(window.ToMs), len(series), len(grid))

	if *sweep {
		printSweep(results)
	} else {
		for _, r := range results {
			for _, line := range r.SummaryLines() {
				fmt.Println(line)
			}
			fmt.Println()
		}
	}
	for _, line := range backtest.AssumptionLines(results) {
		fmt.Println(line)
	}

	if *csvPath != "" {
		file, err := os.Create(*csvPath)
		if err != nil {
			log.Fatalf("create csv: %v", err)
		}
		defer file.Close()
		if err := backtest.WriteCSV(file, results); err != nil {
			log.Fatalf("write csv: %v", err)
		}
		fmt.Printf("\nĐã ghi %d dòng vào %s\n", len(results), *csvPath)
	}
}

// buildSeries assembles one Series per hedgeable (symbol, perp) combination.
//
// The hedge leg is chosen EXACTLY as cmd/scanner chooses it: the same
// instruments.BuildHedgeMapping over the stored instrument snapshot (venue-
// declared base and quote, validated both ways), then config's one leg rule,
// CheapestVerifiedSpot. Step 3.5 compares this replay's positions against the
// live journal, and two commands picking different legs for the same perp
// would compare different trades. A perp the mapping refuses is skipped in
// the venue's words.
func buildSeries(ctx context.Context, db *store.Store, cfg config.Config,
	only string, window backtest.Window) ([]backtest.Series, error) {

	books, err := latestBooks(ctx, db, cfg, only, window.ToMs)
	if err != nil {
		return nil, err
	}
	mapping, err := hedgeMapping(ctx, db, cfg)
	if err != nil {
		return nil, err
	}
	spotFor := map[string]string{} // symbol|perp → chosen spot
	candidates := map[string][]string{}
	for _, pair := range mapping.Pairs {
		k := key(pair.Symbol, pair.Perp.Source)
		candidates[k] = append(candidates[k], pair.Spot.Source)
	}
	for k, spots := range candidates {
		spotFor[k], _ = cfg.CheapestVerifiedSpot(spots)
	}
	for _, r := range mapping.Rejections {
		if src, ok := cfg.SourceByName(r.Source); ok && src.MarketType == "perp" {
			if only == "" || r.Symbol == only {
				log.Printf("bỏ qua %s/%s: %s", r.Symbol, r.Source, r.Reason)
			}
		}
	}

	var out []backtest.Series
	for _, symbol := range cfg.Symbols {
		if only != "" && symbol.Symbol != only {
			continue
		}
		rows, err := db.FundingHistory(ctx, symbol.Symbol, window.FromMs, window.ToMs)
		if err != nil {
			return nil, fmt.Errorf("funding history for %s: %w", symbol.Symbol, err)
		}
		bySource := map[string][]store.FundingRow{}
		for _, row := range rows {
			bySource[row.Source] = append(bySource[row.Source], row)
		}
		for _, perp := range cfg.Sources {
			if perp.MarketType != "perp" || !perp.Tradable {
				continue
			}
			spotSource, ok := spotFor[key(symbol.Symbol, perp.Source)]
			if !ok {
				continue // refused above, by name
			}
			settled := bySource[perp.Source]
			if len(settled) == 0 {
				continue
			}
			spot, _ := cfg.SourceByName(spotSource)
			entries := make([]exchanges.FundingHistoryEntry, 0, len(settled))
			for _, row := range settled {
				entries = append(entries, row.FundingHistoryEntry)
			}
			out = append(out, backtest.Series{
				Symbol: symbol.Symbol, PerpSource: perp.Source, SpotSource: spotSource,
				Settled:  entries,
				SpotFee:  schedule(spot),
				PerpFee:  schedule(perp),
				SpotBook: books[key(symbol.Symbol, spotSource)],
				PerpBook: books[key(symbol.Symbol, perp.Source)],
			})
		}
	}
	return out, nil
}

// hedgeMapping rebuilds the step-2.4 mapping from the NEWEST stored instrument
// snapshot — the same function, over the same declarations, cmd/scanner runs
// after every registry refresh.
func hedgeMapping(ctx context.Context, db *store.Store, cfg config.Config) (instruments.HedgeMapping, error) {
	var day string
	if err := db.DB().QueryRowContext(ctx, `SELECT COALESCE(MAX(snapshot_day), '') FROM instrument_snapshots`).Scan(&day); err != nil {
		return instruments.HedgeMapping{}, fmt.Errorf("newest instrument snapshot: %w", err)
	}
	if day == "" {
		return instruments.HedgeMapping{}, fmt.Errorf("no instrument snapshot stored — run the scanner with storage on first")
	}
	insts, err := db.InstrumentSnapshot(ctx, day)
	if err != nil {
		return instruments.HedgeMapping{}, fmt.Errorf("instrument snapshot %s: %w", day, err)
	}
	pairs := make([]instruments.PairAssets, 0, len(cfg.Symbols))
	for _, s := range cfg.Symbols {
		pairs = append(pairs, instruments.PairAssets{Symbol: s.Symbol, BaseAsset: s.Base})
	}
	claims := make([]instruments.SourceClaim, 0, len(cfg.Sources))
	for _, s := range cfg.Sources {
		claims = append(claims, instruments.SourceClaim{Source: s.Source, MarketType: s.MarketType,
			QuoteAsset: s.QuoteAsset, Tradable: s.Tradable})
	}
	log.Printf("hedge mapping from instrument snapshot %s", day)
	return instruments.BuildHedgeMapping(insts, pairs, claims), nil
}

func schedule(source config.Source) fees.Schedule {
	return fees.Schedule{
		Source: source.Source, MakerFeeBps: source.Fee.MakerBps,
		TakerFeeBps: source.Fee.TakerBps, Verified: source.Fee.Verified,
	}
}

func key(symbol, source string) string { return symbol + "|" + source }

// latestBooks reads the newest USABLE depth measurement per (symbol, source)
// from the last 30 days — bounded, because the scanner appends to this table
// hourly and a read since epoch would grow without limit; usable, because a
// row carrying ErrVI is a failed fetch, not a book, and would price nothing.
//
// ONE measurement, held fixed for the whole replay — depth cannot be
// backfilled, and the engine's assumptions block says so on every report.
func latestBooks(ctx context.Context, db *store.Store, cfg config.Config,
	only string, untilMs int64) (map[string]depth.Summary, error) {

	const lookbackMs = 30 * 24 * 3600 * 1000
	out := map[string]depth.Summary{}
	for _, symbol := range cfg.Symbols {
		if only != "" && symbol.Symbol != only {
			continue
		}
		summaries, err := db.DepthSnapshots(ctx, symbol.Symbol, untilMs-lookbackMs, untilMs+1)
		if err != nil {
			return nil, fmt.Errorf("depth for %s: %w", symbol.Symbol, err)
		}
		for _, s := range summaries {
			if !s.OK() {
				continue
			}
			existing, seen := out[key(s.Symbol, s.Source)]
			if !seen || s.SampledAtMs > existing.SampledAtMs {
				out[key(s.Symbol, s.Source)] = s
			}
		}
	}
	return out, nil
}

func baseParams(notional, holdDays float64) strategy.Params {
	return strategy.Params{
		MinRatePer8hBps: 0.5, PersistencePeriods: 3, MinNetAPRFrac: 0.02,
		NotionalQuote: notional, HoldingDays: holdDays,
		ExitNetAPRFrac: 0.005, ExitPersistencePeriods: 3,
		MaxBasisPct: 1.0, MaxBasisWidenPct: 0.5,
	}
}

func sweepGrid(notional, holdDays float64) []strategy.Params {
	var grid []strategy.Params
	for _, minBps := range []float64{0.3, 0.5, 0.8, 1.2} {
		for _, periods := range []int{2, 3, 6} {
			for _, exitPeriods := range []int{1, 3} {
				p := baseParams(notional, holdDays)
				p.MinRatePer8hBps = minBps
				p.PersistencePeriods = periods
				p.ExitPersistencePeriods = exitPeriods
				grid = append(grid, p)
			}
		}
	}
	return grid
}

func printSweep(results []backtest.Result) {
	// Sorted on a COPY: the CSV written afterwards keeps series-major order,
	// so plain and -sweep runs produce diffable files.
	results = append([]backtest.Result(nil), results...)
	backtest.SortByRealizedAPR(results)
	fmt.Printf("%-8s %-20s %6s %5s %5s %7s %7s %8s %6s\n",
		"cặp", "perp", "ngưỡng", "bền", "thoát", "APR", "tổng", "drawdown", "lệnh")
	refused := map[string]string{}
	for _, r := range results {
		if !r.OK {
			refused[r.Symbol+"/"+r.PerpSource] = r.ReasonVI
			continue
		}
		fmt.Printf("%-8s %-20s %6.2f %5d %5d %+6.2f%% %+6.3f%% %7.3f%% %6d\n",
			r.Symbol, r.PerpSource, r.Params.MinRatePer8hBps, r.Params.PersistencePeriods,
			r.Params.ExitPersistencePeriods, r.RealizedAPRFrac*100, r.TotalReturnFrac*100,
			r.MaxDrawdownFrac*100, len(r.Trades))
	}
	if len(refused) > 0 {
		fmt.Printf("\nTỪ CHỐI (không phát lại, không phải 'không có lệnh'):\n")
		for k, why := range refused {
			fmt.Printf("  %-30s %s\n", k, why)
		}
	}
}

func stamp(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02") }
