package main

import (
	"context"
	"log"
	"strings"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/venues"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/instruments"
	"futures-arbitrage-scanner/internal/scanner"
	"futures-arbitrage-scanner/internal/store"
)

// Order book depth wiring (step 2.7b).
//
// The collector is pure — it fetches and measures — so the two things that make
// it useful are joined here: the instrument registry, which supplies the
// contract→coin multiplier, and the store, which keeps the series phase 3 will
// need and can never re-fetch.

// depthWarmup is how long the first sweep waits.
//
// Long enough for the instrument registry to have finished its first refresh:
// without a multiplier the three contract venues produce no figures at all, and
// a first sweep that reports "unknown" for a third of the table on every
// restart is a worse first impression than one that starts a minute late.
const depthWarmup = 90 * time.Second

// startDepth launches the depth sweep. db may be nil (persistence off), in
// which case the dashboard still gets its liquidity columns and nothing is
// recorded for phase 3.
func startDepth(ctx context.Context, cfg config.Config, s *scanner.Scanner,
	registry *instruments.Registry, db *store.Store, start func(func())) {

	if !cfg.Depth.Enabled {
		log.Printf("depth: disabled; no order book is fetched and liquidity stays unknown")
		return
	}

	collector := depth.New(depthJobs(cfg), venues.DepthFetchers(), contractSizeFrom(registry), cfg.Depth.Levels)
	if len(collector.Jobs()) == 0 {
		log.Printf("depth: no configured source has a depth fetcher")
		return
	}

	every := time.Duration(cfg.Depth.RefreshEveryMin) * time.Minute
	log.Printf("depth: %d sources every %s, %d levels, windows %g%%/%g%%",
		len(collector.Jobs()), every, cfg.Depth.Levels, depth.WindowTightPct, depth.WindowWidePct)

	start(func() {
		tickLoop(ctx, depthWarmup, every, func(time.Time) {
			summaries := collector.CollectOnce(ctx)
			if ctx.Err() != nil {
				return
			}
			// The scanner first: the dashboard is what a person is looking at,
			// and a slow disk must not hold the table back.
			s.SetDepth(summaries)
			recordDepth(ctx, db, summaries)
			logDepth(summaries)
		})
	})
}

// recordDepth writes the sweep, when there is anywhere to write it.
func recordDepth(ctx context.Context, db *store.Store, summaries []depth.Summary) {
	if db == nil {
		return
	}
	written, err := db.PutDepthSnapshots(ctx, summaries)
	if err != nil {
		log.Printf("depth: store: %v", err)
		return
	}
	log.Printf("depth: stored %d of %d measurements", written, len(summaries))
}

// logDepth names what could not be read.
//
// Only the failures, and only once per sweep: a healthy sweep is 36 lines
// nobody reads, and the whole point of the line is that a venue quietly
// dropping out of the liquidity table should not be quiet.
func logDepth(summaries []depth.Summary) {
	var failed []string
	for _, summary := range summaries {
		if !summary.OK() {
			failed = append(failed, summary.Source+"/"+summary.Symbol)
		}
	}
	if len(failed) > 0 {
		log.Printf("depth: %d of %d series unreadable: %s",
			len(failed), len(summaries), strings.Join(failed, ", "))
	}
}

// depthJobs builds one sweep job per configured source.
func depthJobs(cfg config.Config) []depth.Job {
	jobs := make([]depth.Job, 0, len(cfg.Sources))
	for _, source := range cfg.Sources {
		symbols := venueSymbols(cfg, source)
		if len(symbols) == 0 {
			continue
		}
		jobs = append(jobs, depth.Job{
			Source: source.Source, Connector: source.Connector, Symbols: symbols,
		})
	}
	return jobs
}

// contractSizeFrom adapts the instrument registry to the collector's lookup.
//
// It reports ok=false for a market the registry does not know, and the
// collector then publishes no liquidity figure at all for it. That is the
// important half: treating an unknown multiplier as 1 would report Gate, whose
// BTC contract is 0.0001 BTC, as ten thousand times the deepest venue in the
// table — a wrong number that nothing downstream could tell from a right one.
func contractSizeFrom(registry *instruments.Registry) depth.ContractSizeFn {
	return func(symbol, source string) (float64, bool) {
		for _, inst := range registry.Snapshot() {
			if inst.Symbol == symbol && inst.Source == source {
				return inst.ContractSizeCoin, inst.ContractSizeCoin > 0
			}
		}
		return 0, false
	}
}

// contractSizes is the same information as a map, for the scanner's top-of-book
// conversion. Rebuilt after every registry refresh and pushed in whole.
func contractSizes(insts []exchanges.Instrument) map[string]float64 {
	sizes := make(map[string]float64, len(insts))
	for _, inst := range insts {
		if inst.ContractSizeCoin > 0 {
			sizes[inst.Symbol+"|"+inst.Source] = inst.ContractSizeCoin
		}
	}
	return sizes
}
