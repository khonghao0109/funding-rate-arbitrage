package history

import (
	"context"
	"fmt"
	"sort"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/store"
)

// Hourly candle collection (step 3.3b), the price twin of the funding
// collector above.
//
// It is a separate type rather than a mode of Collector because the two differ
// in every respect that matters: a different fetcher signature, a different
// window type, a different table, a different set of jobs (SPOT sources
// included, since a basis needs both legs), and a different reach per venue.
// Folding them together would mean a Job whose meaning depends on which method
// was called.

// PriceResult is what one (source, symbol) candle collection did.
type PriceResult struct {
	Source string
	Symbol string

	RequestedFromMs int64
	Fetched         int
	Inserted        int
	OldestOpenMs    int64
	NewestOpenMs    int64

	// Err is this series' failure. One venue refusing must never stop the
	// others — the same rule the funding collector follows.
	Err error
}

// ReachedRequestedStart reports whether the venue answered as far back as it
// was asked.
//
// False is NORMAL for Hyperliquid, whose hourly candles reach about 208 days
// and which signals the boundary with an empty array rather than an error
// (measured 2026-09-07). It is the fact a backtest needs to see rather than
// infer from a series that merely looks short.
func (r PriceResult) ReachedRequestedStart() bool {
	if r.Err != nil || r.Fetched == 0 {
		return false
	}
	// One interval of slack: a candle series cannot have a bar opening exactly
	// at an arbitrary requested instant.
	return r.OldestOpenMs-r.RequestedFromMs <= exchanges.PriceCandleIntervalSec*msPerSecond
}

const msPerSecond = 1000

// PriceCollector fetches hourly candles for a set of jobs and stores them.
type PriceCollector struct {
	store    *store.Store
	jobs     []Job
	fetchers map[string]exchanges.PriceHistoryFetchFunc
	now      func() time.Time
}

// NewPrices builds a candle collector. Jobs whose connector has no price
// fetcher are dropped here, once — Paradex and Pyth have none — because
// silently skipping them looks identical to a venue that answered with nothing.
func NewPrices(st *store.Store, jobs []Job, fetchers map[string]exchanges.PriceHistoryFetchFunc) *PriceCollector {
	kept := make([]Job, 0, len(jobs))
	for _, job := range jobs {
		if _, ok := fetchers[job.Connector]; !ok {
			continue
		}
		if len(job.Symbols) == 0 {
			continue
		}
		kept = append(kept, job)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Source < kept[j].Source })
	return &PriceCollector{store: st, jobs: kept, fetchers: fetchers, now: time.Now}
}

// Jobs is the list actually collectable.
func (c *PriceCollector) Jobs() []Job { return c.jobs }

// Collect fetches a fixed window for every job and stores what came back.
//
// One source at a time, for the reason Collect gives above: a backfill has no
// deadline and eight venues hit at once is a burst against the same rate limits
// the live feeds are spending.
func (c *PriceCollector) Collect(ctx context.Context, window exchanges.PriceWindow) []PriceResult {
	var results []PriceResult
	for _, job := range c.jobs {
		for _, symbol := range job.Symbols {
			if ctx.Err() != nil {
				return results
			}
			results = append(results, c.collectOne(ctx, job, symbol, window))
		}
	}
	return results
}

func (c *PriceCollector) collectOne(ctx context.Context, job Job, symbol exchanges.Symbol, window exchanges.PriceWindow) PriceResult {
	result := PriceResult{Source: job.Source, Symbol: symbol.Standard, RequestedFromMs: window.StartMs}

	fetch, ok := c.fetchers[job.Connector]
	if !ok {
		result.Err = fmt.Errorf("connector %q has no price history fetcher", job.Connector)
		return result
	}
	candles, err := fetch(ctx, job.Source, symbol, window)
	if err != nil {
		result.Err = err
		return result
	}
	result.Fetched = len(candles)
	if len(candles) == 0 {
		return result
	}
	result.OldestOpenMs = candles[0].OpenTimeMs
	result.NewestOpenMs = candles[len(candles)-1].OpenTimeMs

	inserted, err := c.store.PutPriceHistory(ctx, candles)
	if err != nil {
		result.Err = err
		return result
	}
	result.Inserted = inserted
	return result
}
