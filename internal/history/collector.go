package history

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/store"
)

const (
	// TopUpOverlap is how far BEFORE the newest stored settlement an
	// incremental run starts. It is not tuning: finishFundingHistory measures a
	// venue's cadence from the spacing of the rows it was given, so a window
	// containing one new settlement carries no cadence and would be refused.
	//
	// 26 hours is the smallest value that still gives three settlements at the
	// slowest cadence here (8h). It is kept small because one venue pays for it
	// per hour rather than per row: Paradex is sampled one HTTP request per
	// hour boundary, so every extra day of overlap is another 24 requests per
	// symbol per top-up. Re-fetching the overlap costs nothing in the database
	// — the primary key turns it into ignored inserts.
	//
	// A gap longer than this is still covered: the window starts at the newest
	// STORED settlement, so an outage of any length is filled on the next run.
	TopUpOverlap = 26 * time.Hour

	// BootstrapWindow is what a series with nothing stored asks for. It is
	// deliberately short: filling a year is cmd/backfill's job, run once and
	// watched, not something a scanner should start doing on its own at
	// startup against seven venues' rate limits.
	BootstrapWindow = 7 * 24 * time.Hour
)

// Job is one source and the symbols to collect for it.
type Job struct {
	// Source is the wire id (binance_futures); Connector selects the fetcher,
	// exactly as it does for the connectors and the instrument registry. They
	// are separate so two sources can share one venue's code.
	Source    string
	Connector string
	Symbols   []exchanges.Symbol
}

// Result is what one (source, symbol) collection did.
//
// Fetched and Inserted differ by design and the difference is informative:
// re-running a backfill over the same window fetches everything and inserts
// nothing, which is what "restart loses nothing" looks like from here.
type Result struct {
	Source string
	Symbol string

	RequestedFromMs int64
	Fetched         int
	Inserted        int
	OldestAtMs      int64
	NewestAtMs      int64
	Gaps            exchanges.FundingGapReport

	// Err is this series' failure. One venue refusing must never stop the
	// others: a collection run reports per series and the caller decides.
	Err error
}

// ReachedRequestedStart reports whether the venue answered as far back as it
// was asked. False is normal for OKX, Gate and Paradex and is the fact phase 3
// needs to see rather than infer.
func (r Result) ReachedRequestedStart() bool {
	if r.Err != nil || r.Fetched == 0 {
		return false
	}
	// One interval of slack: a venue that settles every 8h cannot have a row
	// exactly at an arbitrary requested instant.
	slackMs := int64(0)
	if r.Gaps.ModalGapSec > 0 {
		slackMs = r.Gaps.ModalGapSec * 1000
	}
	return r.OldestAtMs-r.RequestedFromMs <= slackMs
}

// Collector fetches funding history for a set of jobs and stores it.
type Collector struct {
	store    *store.Store
	jobs     []Job
	fetchers map[string]exchanges.FundingHistoryFetchFunc
	now      func() time.Time
}

// New builds a collector. Jobs whose connector has no history fetcher are
// dropped here, once, with a line saying so — a spot source has no funding and
// an oracle has neither, and silently skipping them would look identical to a
// venue that answered with nothing.
func New(st *store.Store, jobs []Job) *Collector {
	return newCollector(st, jobs, exchanges.FundingHistoryFetchers())
}

// newCollector is New with the fetcher table supplied. The tests use it to run
// the whole collection path — resume points, overlap, per-series failure — with
// no socket open, which is what keeps `go test ./...` offline.
func newCollector(st *store.Store, jobs []Job, fetchers map[string]exchanges.FundingHistoryFetchFunc) *Collector {
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
	return &Collector{store: st, jobs: kept, fetchers: fetchers, now: time.Now}
}

// Jobs is the list actually collectable, after the connectors without a history
// fetcher were dropped.
func (c *Collector) Jobs() []Job { return c.jobs }

// Collect fetches a fixed window for every job and stores what came back.
//
// Sources are walked one at a time rather than in parallel. Backfill is a
// background errand with no deadline, and seven venues hit at once is a burst
// against the same rate limits the live feeds are spending.
func (c *Collector) Collect(ctx context.Context, window exchanges.FundingWindow) []Result {
	var results []Result
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

// TopUp fetches only what is missing since each series' newest stored
// settlement, so a restart or an outage costs one short window rather than a
// full refetch.
func (c *Collector) TopUp(ctx context.Context) []Result {
	nowMs := c.now().UnixMilli()

	var results []Result
	for _, job := range c.jobs {
		for _, symbol := range job.Symbols {
			if ctx.Err() != nil {
				return results
			}
			newestMs, err := c.store.LatestFundingAt(ctx, job.Source, symbol.Standard)
			if err != nil {
				results = append(results, Result{Source: job.Source, Symbol: symbol.Standard, Err: err})
				continue
			}
			startMs := nowMs - BootstrapWindow.Milliseconds()
			if newestMs > 0 {
				startMs = newestMs - TopUpOverlap.Milliseconds()
			}
			// EndMs is exclusive and nowMs is "now", so the settlement running
			// right now is not in the window — which is correct: it has not
			// settled, and a venue that reports it early would be publishing a
			// prediction into a table of realized rates.
			window := exchanges.FundingWindow{StartMs: startMs, EndMs: nowMs}
			results = append(results, c.collectOne(ctx, job, symbol, window))
		}
	}
	return results
}

func (c *Collector) collectOne(ctx context.Context, job Job, symbol exchanges.Symbol, window exchanges.FundingWindow) Result {
	result := Result{Source: job.Source, Symbol: symbol.Standard, RequestedFromMs: window.StartMs}

	fetch, ok := c.fetchers[job.Connector]
	if !ok {
		result.Err = fmt.Errorf("connector %q has no funding history fetcher", job.Connector)
		return result
	}
	entries, err := fetch(ctx, job.Source, symbol, window)
	if err != nil {
		result.Err = err
		return result
	}
	result.Fetched = len(entries)
	if len(entries) == 0 {
		return result
	}
	result.OldestAtMs = entries[0].SettledAtMs
	result.NewestAtMs = entries[len(entries)-1].SettledAtMs
	result.Gaps = exchanges.FundingGaps(entries)

	inserted, err := c.store.PutFundingHistory(ctx, entries)
	if err != nil {
		result.Err = err
		return result
	}
	result.Inserted = inserted
	return result
}

// Run tops up on a schedule until ctx ends. It is what keeps the corpus current
// inside the scanner process; a failure is logged per series and retried at the
// next tick, because a venue being down is not a reason to stop collecting from
// the other six.
func (c *Collector) Run(ctx context.Context, every time.Duration) {
	if len(c.jobs) == 0 || every <= 0 {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		results := c.TopUp(ctx)
		// A cancelled context makes TopUp return nothing, and logging that
		// would put "0 series, 0 rows" in the log on every shutdown — a line
		// that reads like a failed collection.
		if ctx.Err() != nil {
			return
		}
		LogResults("funding history top-up", results)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// LogResults prints one line per series that did something, and one per series
// that failed. A run where every venue returned only rows already stored prints
// a single summary — the normal case once the corpus is warm, and one that must
// not fill the log.
func LogResults(job string, results []Result) {
	var inserted, fetched, failed int
	for _, result := range results {
		switch {
		case result.Err != nil:
			failed++
			log.Printf("%s: %s %s: %v", job, result.Source, result.Symbol, result.Err)
		case result.Inserted > 0:
			log.Printf("%s: %s %s: +%d rows (%d fetched), newest %s, cadence %ds",
				job, result.Source, result.Symbol, result.Inserted, result.Fetched,
				time.UnixMilli(result.NewestAtMs).UTC().Format(time.RFC3339),
				result.Gaps.ModalGapSec)
		}
		inserted += result.Inserted
		fetched += result.Fetched
	}
	log.Printf("%s: %d series, %d rows fetched, %d new, %d failed",
		job, len(results), fetched, inserted, failed)
}
