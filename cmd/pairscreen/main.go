// Command pairscreen measures what a candidate pair looks like on the venues
// TODAY, before it earns a place in config.yaml: which sources list it,
// whether a spot leg pairs with each perp under the config's own declarations
// (venue-declared base and quote, validated both ways, exactly as cmd/scanner
// and cmd/backtest pair them), how deep both legs' books are, and the
// round-trip cost internal/strategy prices from those books at the intended
// size. It writes one JSON row per (symbol, perp source) and reads nothing
// from the store: the other half of a screen — the funding the pair actually
// paid over the last year — is the corpus cmd/backfill fills and
// tools/report/pairscreen.py reads.
//
// It is a diagnostic in the family of cmd/fundingcheck and cmd/backfill:
// public REST only, no credentials, and it decides nothing. A pair it cannot
// price is reported with the venue's or the strategy's own words, never
// dropped and never costed at 0.
//
// Run: go run ./cmd/pairscreen -config config-screen.yaml -out screen.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/venues"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/fees"
	"futures-arbitrage-scanner/internal/instruments"
	"futures-arbitrage-scanner/internal/strategy"
)

func main() { os.Exit(run()) }

func run() int {
	configPath := flag.String("config", "config.yaml", "configuration file; its symbols are the candidates")
	only := flag.String("symbol", "", "screen one pair only (default: every configured pair)")
	notional := flag.Float64("notional", 50000, "position size in the quote asset the round trip is priced at")
	out := flag.String("out", "", "write the report as JSON to this file (default: stdout)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Printf("configuration: %v", err)
		return 1
	}
	if *notional <= 0 {
		log.Printf("-notional must be positive, got %g", *notional)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The registry is refreshed ONCE, live, rather than read from the store's
	// daily snapshot: a candidate pair has no snapshot yet, and the point is
	// what the venue says today.
	registry := instruments.New(registrySources(cfg))
	if err := registry.Refresh(ctx); err != nil {
		// A source that failed is reported as unlisted with this reason in
		// the rows; the others are still worth screening.
		log.Printf("instrument refresh: %v (continuing with the sources that answered)", err)
	}
	insts := registry.Snapshot()
	log.Printf("instruments: %d across %d sources", len(insts), len(cfg.Sources))

	mapping := hedgeMapping(cfg, insts)

	collector := depth.New(depthJobs(cfg, registry, *only), venues.DepthFetchers(), contractSizeFrom(registry), cfg.Depth.Levels)
	log.Printf("depth: %d sources, %d levels", len(collector.Jobs()), cfg.Depth.Levels)
	summaries := collector.CollectOnce(ctx)
	if ctx.Err() != nil {
		log.Printf("interrupted before every book was read; nothing written")
		return 1
	}

	report := Report{
		GeneratedAtMs: time.Now().UnixMilli(),
		NotionalQuote: *notional,
		Rows:          screenRows(cfg, insts, mapping, summaries, *notional, time.Now(), *only),
		Rejections:    mapping.Rejections,
	}
	printTable(report)

	encoded, err := json.MarshalIndent(report, "", " ")
	if err != nil {
		log.Printf("encode: %v", err)
		return 1
	}
	if *out == "" {
		fmt.Println(string(encoded))
		return 0
	}
	if err := os.WriteFile(*out, encoded, 0o644); err != nil {
		log.Printf("write %s: %v", *out, err)
		return 1
	}
	log.Printf("wrote %s: %d rows", *out, len(report.Rows))
	return 0
}

// Report is the file this command writes. Every figure that came from a venue
// or from internal/strategy is carried with the words that qualify it.
type Report struct {
	GeneratedAtMs int64
	NotionalQuote float64
	Rows          []Row
	// Rejections is the hedge mapping's own list, verbatim: every instrument
	// it refused to pair and why, spot sources included.
	Rejections []instruments.Rejection
}

// Row is one (symbol, perp source) candidate.
type Row struct {
	Symbol     string
	PerpSource string

	// Listed is whether the perp venue lists the market at all. When false
	// every other field is empty and RefusalVI says why.
	Listed bool
	// SpotSource is the hedge leg config's one rule (CheapestVerifiedSpot)
	// chose among the legs the mapping validated; "" when the mapping refused
	// every candidate, and RefusalVI then carries the refusal.
	SpotSource   string
	SpotNoteVI   string
	QuoteBridged bool
	RefusalVI    string

	Perp InstrumentFacts
	Spot InstrumentFacts

	PerpBook depth.Summary
	SpotBook depth.Summary

	// Cost is the round trip at NotionalQuote, priced by internal/strategy
	// from the two books above, or its refusal.
	Cost CostFacts

	// MinDepthWithinWideQuote is the thinnest of the four sides the trip
	// crosses (spot ask and perp bid to enter, spot bid and perp ask to
	// exit) inside 0.5% of mid — the number a size has to fit under.
	MinDepthWithinWideQuote float64
}

// InstrumentFacts is the slice of exchanges.Instrument a screen reads.
type InstrumentFacts struct {
	Source           string
	NativeSymbol     string
	Status           string
	QuoteAsset       string
	IsContract       bool
	ContractSizeCoin float64
	MinNotionalQuote float64
	MaxQtyCoin       float64
	MaxLeverageX     float64
}

// CostFacts is strategy.RoundTrip reduced to what the screen reports.
type CostFacts struct {
	OK                bool
	TotalPct          float64
	FeesPct           float64
	SlippagePct       float64
	DepthIsLowerBound bool
	ReasonVI          string
	NoteVI            string
}

func facts(inst exchanges.Instrument) InstrumentFacts {
	return InstrumentFacts{
		Source: inst.Source, NativeSymbol: inst.NativeSymbol, Status: inst.Status, QuoteAsset: inst.QuoteAsset,
		IsContract: inst.IsContract, ContractSizeCoin: inst.ContractSizeCoin,
		MinNotionalQuote: inst.MinNotionalQuote, MaxQtyCoin: inst.MaxQtyCoin, MaxLeverageX: inst.MaxLeverageX,
	}
}

// screenRows builds one row per (configured symbol, tradable perp source).
// Pure: everything it needs is passed in, so the shape of a refusal can be
// tested without a socket.
func screenRows(cfg config.Config, insts []exchanges.Instrument, mapping instruments.HedgeMapping,
	summaries []depth.Summary, notional float64, now time.Time, only string) []Row {

	instBy := make(map[string]exchanges.Instrument, len(insts))
	for _, inst := range insts {
		instBy[key(inst.Symbol, inst.Source)] = inst
	}
	bookBy := make(map[string]depth.Summary, len(summaries))
	for _, s := range summaries {
		bookBy[key(s.Symbol, s.Source)] = s
	}
	candidates := map[string][]string{}
	bridged := map[string]bool{}
	for _, pair := range mapping.Pairs {
		k := key(pair.Symbol, pair.Perp.Source)
		candidates[k] = append(candidates[k], pair.Spot.Source)
		if pair.QuoteBridged {
			bridged[k+"|"+pair.Spot.Source] = true
		}
	}
	refusal := map[string]string{}
	for _, r := range mapping.Rejections {
		if _, seen := refusal[key(r.Symbol, r.Source)]; !seen {
			refusal[key(r.Symbol, r.Source)] = r.Reason
		}
	}

	var rows []Row
	for _, symbol := range cfg.Symbols {
		if only != "" && symbol.Symbol != only {
			continue
		}
		for _, perp := range cfg.Sources {
			if perp.MarketType != "perp" || !perp.Tradable {
				continue
			}
			row := Row{Symbol: symbol.Symbol, PerpSource: perp.Source}
			perpInst, listed := instBy[key(symbol.Symbol, perp.Source)]
			if !listed {
				row.RefusalVI = "Sàn không niêm yết market này (instrument registry không trả về nó)."
				if _, ok := perp.VenueSymbol(symbol); !ok {
					row.RefusalVI = "config không ánh xạ cặp này sang ký hiệu của sàn (symbol_map bỏ qua)."
				}
				rows = append(rows, row)
				continue
			}
			row.Listed = true
			row.Perp = facts(perpInst)
			row.PerpBook = bookBy[key(symbol.Symbol, perp.Source)]

			k := key(symbol.Symbol, perp.Source)
			spot, note := cfg.CheapestVerifiedSpot(candidates[k])
			if spot == "" {
				row.RefusalVI = refusal[k]
				if row.RefusalVI == "" {
					row.RefusalVI = "Không có chân spot nào ghép được (không rejection nào được ghi — kiểm tra registry của các nguồn spot)."
				}
				rows = append(rows, row)
				continue
			}
			row.SpotSource, row.SpotNoteVI = spot, note
			row.QuoteBridged = bridged[k+"|"+spot]
			row.Spot = facts(instBy[key(symbol.Symbol, spot)])
			row.SpotBook = bookBy[key(symbol.Symbol, spot)]

			spotSrc, _ := cfg.SourceByName(spot)
			trip := strategy.RoundTripCost(strategy.RoundTripInput{
				NotionalQuote: notional,
				SpotFee:       schedule(spotSrc), PerpFee: schedule(perp),
				SpotBook: row.SpotBook, PerpBook: row.PerpBook,
				At: now, MaxBookAge: 0,
			})
			row.Cost = CostFacts{OK: trip.OK, TotalPct: trip.TotalPct, FeesPct: trip.FeesPct, SlippagePct: trip.SlippagePct,
				DepthIsLowerBound: trip.DepthIsLowerBound, ReasonVI: trip.ReasonVI, NoteVI: trip.NoteVI}
			row.MinDepthWithinWideQuote = minDepth(row.SpotBook, row.PerpBook)
			rows = append(rows, row)
		}
	}
	return rows
}

// minDepth is the thinnest side of the four the trip crosses, inside the
// wide window. 0 when either book is unusable — which reads as "unknown",
// never as "empty", the same convention the scanner's quantities use.
func minDepth(spot, perp depth.Summary) float64 {
	if !spot.OK() || !perp.OK() {
		return 0
	}
	m := spot.AskDepthWithinWideQuote
	for _, v := range []float64{spot.BidDepthWithinWideQuote, perp.BidDepthWithinWideQuote, perp.AskDepthWithinWideQuote} {
		if v < m {
			m = v
		}
	}
	return m
}

func schedule(source config.Source) fees.Schedule {
	return fees.Schedule{
		Source: source.Source, MakerFeeBps: source.Fee.MakerBps,
		TakerFeeBps: source.Fee.TakerBps, Verified: source.Fee.Verified,
	}
}

func key(symbol, source string) string { return symbol + "|" + source }

// registrySources mirrors cmd/scanner's startInstrumentRegistry: every source
// with an instrument fetcher, asked for the symbols config maps onto it.
func registrySources(cfg config.Config) []instruments.Source {
	fetchers := venues.InstrumentFetchers()
	var sources []instruments.Source
	for _, source := range cfg.Sources {
		fetch, ok := fetchers[source.Connector]
		if !ok {
			continue
		}
		sources = append(sources, instruments.Source{Name: source.Source, Symbols: venueSymbols(cfg, source), Fetch: fetch})
	}
	return sources
}

func venueSymbols(cfg config.Config, source config.Source) []exchanges.Symbol {
	symbols := make([]exchanges.Symbol, 0, len(cfg.Symbols))
	for _, symbol := range cfg.Symbols {
		venueSymbol, ok := source.VenueSymbol(symbol)
		if !ok {
			continue
		}
		symbols = append(symbols, exchanges.Symbol{Standard: symbol.Symbol, Venue: venueSymbol})
	}
	return symbols
}

// depthJobs asks each source only for the markets the registry confirmed it
// lists: a candidate list is mostly markets a given venue does NOT carry, and
// a 404 per (venue, pair) would be most of the sweep.
func depthJobs(cfg config.Config, registry *instruments.Registry, only string) []depth.Job {
	jobs := make([]depth.Job, 0, len(cfg.Sources))
	for _, source := range cfg.Sources {
		var symbols []exchanges.Symbol
		for _, symbol := range venueSymbols(cfg, source) {
			if only != "" && symbol.Standard != only {
				continue
			}
			if _, listed := registry.Instrument(source.Source, symbol.Standard); listed {
				symbols = append(symbols, symbol)
			}
		}
		if len(symbols) == 0 {
			continue
		}
		jobs = append(jobs, depth.Job{Source: source.Source, Connector: source.Connector, Symbols: symbols})
	}
	return jobs
}

// contractSizeFrom is cmd/scanner's adapter, repeated here because both are
// package main: unknown multiplier → no liquidity figure, never "1".
func contractSizeFrom(registry *instruments.Registry) depth.ContractSizeFn {
	return func(symbol, source string) (float64, bool) {
		inst, ok := registry.Instrument(source, symbol)
		if !ok {
			return 0, false
		}
		return inst.ContractSizeCoin, inst.ContractSizeCoin > 0
	}
}

func hedgeMapping(cfg config.Config, insts []exchanges.Instrument) instruments.HedgeMapping {
	pairs := make([]instruments.PairAssets, 0, len(cfg.Symbols))
	for _, s := range cfg.Symbols {
		pairs = append(pairs, instruments.PairAssets{Symbol: s.Symbol, BaseAsset: s.Base})
	}
	claims := make([]instruments.SourceClaim, 0, len(cfg.Sources))
	for _, s := range cfg.Sources {
		claims = append(claims, instruments.SourceClaim{Source: s.Source, MarketType: s.MarketType,
			QuoteAsset: s.QuoteAsset, Tradable: s.Tradable})
	}
	return instruments.BuildHedgeMapping(insts, pairs, claims, cfg.Hedge.QuoteEquivalents)
}

func printTable(report Report) {
	fmt.Printf("%-9s %-20s %-13s %-6s %9s %10s  %s\n", "cặp", "perp", "spot", "bridge", "vòng@"+kUSD(report.NotionalQuote), "mỏng nhất", "ghi chú")
	for _, r := range report.Rows {
		switch {
		case !r.Listed:
			fmt.Printf("%-9s %-20s %-13s %-6s %9s %10s  %s\n", r.Symbol, r.PerpSource, "—", "", "", "", "không niêm yết")
		case r.SpotSource == "":
			fmt.Printf("%-9s %-20s %-13s %-6s %9s %10s  %s\n", r.Symbol, r.PerpSource, "—", "", "", "", r.RefusalVI)
		case !r.Cost.OK:
			fmt.Printf("%-9s %-20s %-13s %-6s %9s %10s  %s\n", r.Symbol, r.PerpSource, r.SpotSource, bridge(r), "TỪ CHỐI", kUSD(r.MinDepthWithinWideQuote), r.Cost.ReasonVI)
		default:
			fmt.Printf("%-9s %-20s %-13s %-6s %8.3f%% %10s  %s\n", r.Symbol, r.PerpSource, r.SpotSource, bridge(r), r.Cost.TotalPct, kUSD(r.MinDepthWithinWideQuote), r.Cost.NoteVI)
		}
	}
}

func bridge(r Row) string {
	if r.QuoteBridged {
		return "USD"
	}
	return ""
}

func kUSD(v float64) string {
	if v >= 1_000_000 {
		return fmt.Sprintf("%.1fM", v/1_000_000)
	}
	return fmt.Sprintf("%.0fk", v/1000)
}
