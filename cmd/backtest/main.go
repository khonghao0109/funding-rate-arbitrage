// Command backtest replays the production entry and exit rules over the stored
// funding corpus and reports what they would have done (PLAN.md step 3.3).
//
//	go run ./cmd/backtest                          # 6 months, every hedgeable pair
//	go run ./cmd/backtest -months 12 -symbol BTCUSDT
//	go run ./cmd/backtest -sweep -csv out.csv      # parameter sweep to CSV
//	go run ./cmd/backtest -sweep -months 12 -min-rate-bps 0.5,1,2,5 \
//	    -exit-net-apr 0,0.005 -notional 20000,200000 -hold-days 14,60 \
//	    -csv wide.csv -trades-csv trades.csv -top 40    # a wider grid
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
	notional := flag.String("notional", defaultNotional, "position size in the quote asset (a list only with -sweep)")
	holdDays := flag.String("hold-days", defaultHoldDays, "expected holding period in days, for amortizing the round trip (a list only with -sweep)")
	minRateBps := flag.String("min-rate-bps", defaultMinRateBps, "-sweep axis: entry threshold, bps per 8h")
	persist := flag.String("persist", defaultPersist, "-sweep axis: settled periods the rate must persist before entry")
	minNetAPR := flag.String("min-net-apr", defaultMinNetAPR, "-sweep axis: entry floor on projected net APR, fraction")
	exitNetAPR := flag.String("exit-net-apr", defaultExitNetAPR, "-sweep axis: decay-exit floor on net APR, fraction (must stay below the entry floor)")
	exitPersist := flag.String("exit-persist", defaultExitPersist, "-sweep axis: consecutive settled periods under the floor before the decay exit")
	exitNegBps := flag.String("exit-neg-bps", defaultExitNegBps, "-sweep axis: sign-flip exit needs the newest settled rate <= -X bps/8h (0 = any negative)")
	exitNegPeriods := flag.String("exit-neg-periods", defaultExitNegPeriods, "-sweep axis: sign-flip exit needs N consecutive negative settlements")
	exitNegCum := flag.String("exit-neg-cum", defaultExitNegCum, "-sweep axis: sign-flip exit needs the run's paid funding >= C x round-trip cost (0 = no gate)")
	minHold := flag.String("min-hold", defaultMinHold, "-sweep axis: YIELD exits are blocked until the position has collected M x its round-trip cost (0 = off; risk exits are never blocked)")
	tradesCSVPath := flag.String("trades-csv", "", "also write one row per trade to this CSV file")
	top := flag.Int("top", 0, "with -sweep, print only the best N rows (0 = all)")
	flag.Parse()

	spec, err := parseGridSpec(*minRateBps, *persist, *minNetAPR, *exitNetAPR, *exitPersist, *notional, *holdDays)
	if err != nil {
		log.Fatalf("grid: %v", err)
	}
	if spec, err = spec.withMinHold(*minHold); err != nil {
		log.Fatal(err)
	}
	if spec, err = spec.withNegativeGates(*exitNegBps, *exitNegPeriods, *exitNegCum); err != nil {
		log.Fatalf("grid: %v", err)
	}

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

	var grid []strategy.Params
	dropped := 0
	if *sweep {
		grid, dropped, err = spec.params()
		if err != nil {
			log.Fatalf("grid: %v", err)
		}
	} else {
		// A plain run still validates what it uses — a zero hold would make
		// NetAPR refuse every settlement and the run would print "0 trades"
		// as if the strategy had found nothing — and refuses the flags it
		// would otherwise ignore, for the same reason.
		if err := spec.check(); err != nil {
			log.Fatalf("grid: %v", err)
		}
		if len(spec.NotionalQuote) != 1 || len(spec.HoldingDays) != 1 {
			log.Fatalf("-notional and -hold-days take a list only with -sweep")
		}
		if sweepOnlyFlagsTouched(*minRateBps, *persist, *minNetAPR, *exitNetAPR, *exitPersist, *exitNegBps, *exitNegPeriods, *exitNegCum, *minHold, *top) {
			log.Fatalf("-min-rate-bps, -persist, -min-net-apr, -exit-net-apr, -exit-persist, -exit-neg-* and -top " +
				"apply only with -sweep; a plain run uses the step-3.3 base parameters")
		}
		grid = []strategy.Params{plainParams(cfg, spec.NotionalQuote[0], spec.HoldingDays[0],
			*notional != defaultNotional, *holdDays != defaultHoldDays)}
	}

	results := backtest.Sweep(series, window, grid)

	fmt.Printf("BACKTEST %d tháng · %s → %s · %d chuỗi × %d bộ tham số\n",
		*months, stamp(window.FromMs), stamp(window.ToMs), len(series), len(grid))
	if dropped > 0 {
		fmt.Printf("(bỏ %d tổ hợp có sàn thoát ≥ sàn vào — config.yaml cũng từ chối nạp chúng)\n", dropped)
	}
	fmt.Println()

	if *sweep {
		printSweep(results, *top)
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
	if *tradesCSVPath != "" {
		file, err := os.Create(*tradesCSVPath)
		if err != nil {
			log.Fatalf("create trades csv: %v", err)
		}
		defer file.Close()
		if err := backtest.WriteTradesCSV(file, results); err != nil {
			log.Fatalf("write trades csv: %v", err)
		}
		trades := 0
		for _, r := range results {
			trades += len(r.Trades)
		}
		fmt.Printf("Đã ghi %d lệnh vào %s\n", trades, *tradesCSVPath)
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
	// Whether a given (symbol, perp, spot) combination exists only because
	// config declared the two quotes equivalent, keyed per COMBINATION: the
	// chosen leg decides, not whichever pair happened to come first.
	type quotePair struct{ spot, perp string }
	bridged := map[string]quotePair{}
	for _, pair := range mapping.Pairs {
		k := key(pair.Symbol, pair.Perp.Source)
		candidates[k] = append(candidates[k], pair.Spot.Source)
		if pair.QuoteBridged {
			bridged[k+"|"+pair.Spot.Source] = quotePair{pair.SpotQuoteAsset, pair.QuoteAsset}
		}
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
			series := backtest.Series{
				Symbol: symbol.Symbol, PerpSource: perp.Source, SpotSource: spotSource,
				Settled:  entries,
				SpotFee:  schedule(spot),
				PerpFee:  schedule(perp),
				SpotBook: books[key(symbol.Symbol, spotSource)],
				PerpBook: books[key(symbol.Symbol, perp.Source)],
			}
			if q, ok := bridged[key(symbol.Symbol, perp.Source)+"|"+spotSource]; ok {
				series.QuoteBridged, series.SpotQuoteAsset, series.PerpQuoteAsset = true, q.spot, q.perp
			}
			out = append(out, series)
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
	return instruments.BuildHedgeMapping(insts, pairs, claims, cfg.Hedge.QuoteEquivalents), nil
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

// baseParams is the step-3.3 base every sweep axis is varied around. A test
// pins it to config.yaml's strategy block, so the two cannot drift apart.
func baseParams(notional, holdDays float64) strategy.Params {
	return strategy.Params{
		MinRatePer8hBps: 0.3, PersistencePeriods: 6, MinNetAPRFrac: 0.02,
		NotionalQuote: notional, HoldingDays: holdDays,
		ExitNetAPRFrac: 0, ExitPersistencePeriods: 48,
		ExitNegativeMinBps: 2.0, ExitNegativePeriods: 2, ExitNegativeCumCostFrac: 1.0,
		MaxBasisPct: 1.0, MaxBasisWidenPct: 0.5,
	}
}

// plainParams is the parameter set of a plain (non -sweep) run: the config's
// strategy block when it is enabled — through the same StrategyParams the
// live journal uses, so the 3.5 gate compares like with like — and the
// step-3.3 base otherwise. -notional / -hold-days override only when given.
// MaxBookAge is zeroed: the replay prices every historical instant on one
// measured book, which is stale by construction.
func plainParams(cfg config.Config, notional, holdDays float64, notionalGiven, holdGiven bool) strategy.Params {
	if !cfg.Strategy.Enabled {
		return baseParams(notional, holdDays)
	}
	p := cfg.Strategy.StrategyParams()
	p.MaxBookAge = 0
	if notionalGiven {
		p.NotionalQuote = notional
	}
	if holdGiven {
		p.HoldingDays = holdDays
	}
	return p
}

func printSweep(results []backtest.Result, top int) {
	// Sorted on a COPY: the CSV written afterwards keeps series-major order,
	// so plain and -sweep runs produce diffable files.
	results = append([]backtest.Result(nil), results...)
	backtest.SortByRealizedAPR(results)
	fmt.Printf("%-8s %-20s %6s %5s %5s %6s %5s %4s %5s %7s %5s %7s %7s %8s %6s\n",
		"cặp", "perp", "ngưỡng", "bền", "thoát", "sàn-ra", "âm≥", "âmN", "âmC", "vốn", "giữ", "APR", "tổng", "drawdown", "lệnh")
	refused := map[string]string{}
	shown, ran := 0, 0
	for _, r := range results {
		if !r.OK {
			refused[r.Symbol+"/"+r.PerpSource] = r.ReasonVI
			continue
		}
		ran++
		if top > 0 && shown >= top {
			continue
		}
		shown++
		fmt.Printf("%-8s %-20s %6.2f %5d %5d %5.2f%% %5.2f %4d %5.2f %6.0fk %5.0f %+6.2f%% %+6.3f%% %7.3f%% %6d\n",
			r.Symbol, r.PerpSource, r.Params.MinRatePer8hBps, r.Params.PersistencePeriods,
			r.Params.ExitPersistencePeriods, r.Params.ExitNetAPRFrac*100,
			r.Params.ExitNegativeMinBps, r.Params.ExitNegativePeriods, r.Params.ExitNegativeCumCostFrac,
			r.Params.NotionalQuote/1000, r.Params.HoldingDays, r.RealizedAPRFrac*100, r.TotalReturnFrac*100,
			r.MaxDrawdownFrac*100, len(r.Trades))
	}
	if shown < ran {
		fmt.Printf("(hiển thị %d/%d lượt chạy tốt nhất — toàn bộ nằm trong CSV)\n", shown, ran)
	}
	if len(refused) > 0 {
		fmt.Printf("\nTỪ CHỐI (không phát lại, không phải 'không có lệnh'):\n")
		for k, why := range refused {
			fmt.Printf("  %-30s %s\n", k, why)
		}
	}
}

func stamp(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02") }
