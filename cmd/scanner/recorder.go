package main

import (
	"context"
	"log"
	"sync"
	"time"

	"futures-arbitrage-scanner/exchanges/venues"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/history"
	"futures-arbitrage-scanner/internal/instruments"
	"futures-arbitrage-scanner/internal/scanner"
	"futures-arbitrage-scanner/internal/store"
)

// Persistence wiring (step 2.6).
//
// Four jobs, all of them periodic, none of them on the data path: the scanner's
// price and funding maps are read on a schedule and written to SQLite. Nothing
// here can slow an ingestion channel down, and a database that fails to open
// leaves the scanner running exactly as it did before this step — a read-only
// scanner that persists nothing is the product of phases 0 and 1, and it stays
// a valid one.

// startStore opens the database and launches the recording jobs. It returns the
// store so main can close it, and nil when storage is disabled.
func startStore(ctx context.Context, cfg config.Config, s *scanner.Scanner,
	registry *instruments.Registry, running *sync.WaitGroup) *store.Store {

	if !cfg.Storage.Enabled {
		log.Printf("storage: disabled; nothing is persisted")
		return nil
	}

	db, err := store.Open(cfg.Storage.Path)
	if err != nil {
		// Not fatal, deliberately. Losing persistence costs the phase-3 corpus;
		// refusing to start costs the live scanner too, and the live scanner is
		// what someone is watching right now.
		log.Printf("storage: %v — continuing without persistence", err)
		return nil
	}
	log.Printf("storage: %s (price sample %ds, funding top-up %dm, instruments %dh, retention %dd funding / %dd price)",
		cfg.Storage.Path, cfg.Storage.PriceSampleEverySec, cfg.Storage.FundingTopUpEveryMin,
		cfg.Storage.InstrumentSnapshotEveryHours, cfg.Storage.RetainFundingDays, cfg.Storage.RetainPriceDays)

	// Waited on, not fired and forgotten: main closes the store during
	// shutdown, and closing it underneath a job still inside a transaction is
	// how a WAL file ends up needing recovery on the next start.
	start := func(job func()) {
		running.Add(1)
		go func() {
			defer running.Done()
			job()
		}()
	}

	start(func() {
		samplePrices(ctx, db, s, time.Duration(cfg.Storage.PriceSampleEverySec)*time.Second)
	})
	start(func() {
		snapshotInstruments(ctx, db, registry, time.Duration(cfg.Storage.InstrumentSnapshotEveryHours)*time.Hour)
	})
	start(func() {
		topUpFundingHistory(ctx, history.New(db, fundingHistoryJobs(cfg), venues.FundingHistoryFetchers()),
			time.Duration(cfg.Storage.FundingTopUpEveryMin)*time.Minute)
	})
	start(func() {
		prune(ctx, db, time.Duration(cfg.Storage.PruneEveryHours)*time.Hour, store.Retention{
			FundingDays:      cfg.Storage.RetainFundingDays,
			PriceDays:        cfg.Storage.RetainPriceDays,
			DepthDays:        cfg.Depth.RetainDays,
			PriceHistoryDays: cfg.Storage.RetainPriceHistoryDays,
		})
	})
	return db
}

// fundingTopUpJob is the SCHEDULER's name for this loop — what a late tick is
// reported under, on the wire and in the log. Named once so the test asserts
// the same string the operator reads.
//
// It is deliberately not the same string as the collection report's prefix
// below ("funding history top-up"): that one has been in the log since step
// 2.6 and operators grep it, while this one follows the "storage: …"
// convention the other five scheduled jobs use. Two labels, two different
// things — do not "fix" one into the other.
const fundingTopUpJob = "storage: funding top-up"

// topUpFundingHistory collects settled funding on the SAME scheduler every
// other periodic job here uses.
//
// It used to run history.Collector's own ticker, which meant one loop in the
// process was not counted by tickLoop and so never appeared in
// prices.tick_status — a machine that slept through four hours of settlements
// would show every other job's late tick and say nothing about the one job
// whose whole purpose is to fetch what happened while it was asleep.
//
// Since 2026-09-12 all SIX of this file's and its siblings' fixed-period jobs
// run on tickLoop and are counted: the price sampler, the instrument snapshot,
// the prune, the depth sweep, the strategy evaluation and this. It is NOT the
// only loop in the process — instruments.Registry.Run keeps its own, on
// purpose, because its period is variable (it backs off 2x while a refresh
// keeps failing), so "late against its schedule" would not mean there what it
// means here. That exclusion is written down in WS-CONTRACT §4.3; a job added
// with a FIXED period belongs on tickLoop.
//
// The first delay is zero on purpose: the collector's own loop collected once
// at start-up before waiting, and the corpus wants that — a process restarted
// more often than its period would otherwise never top up at all.
//
// One semantic did change with the scheduler. Collector.Run armed its ticker
// BEFORE the first collection, so top-ups landed on a fixed grid; tickLoop
// takes the next due instant AFTER fn returns, so the period is now measured
// from the end of each collection and the grid drifts forward by however long
// a collection takes (seven venues walked serially under their own rate
// limits). That is lossless rather than merely tolerable: TopUp starts each
// series at its newest STORED settlement minus history.TopUpOverlap, 26 hours,
// so drift of any size short of a day still re-reads every settlement it
// passed. What it buys is that the loop's lateness is measurable at all.
func topUpFundingHistory(ctx context.Context, collector *history.Collector, every time.Duration) {
	// The collector's OWN job list, not the one this process built: it drops
	// every job whose connector has no history fetcher, and arming a timer for
	// a config that names only such venues would wake the process forever with
	// nothing to do. Collector.Run guarded on its filtered list for the same
	// reason, and this is the one place that behaviour could have been lost.
	if collector == nil || len(collector.Jobs()) == 0 || every <= 0 {
		return
	}
	tickLoop(ctx, fundingTopUpJob, 0, every, func(time.Time) {
		results := collector.TopUp(ctx)
		// A cancelled context makes TopUp return nothing, and logging that
		// would put "0 series, 0 rows" in the log on every shutdown - a line
		// that reads like a failed collection.
		if ctx.Err() != nil {
			return
		}
		history.LogResults("funding history top-up", results)
	})
}

// fundingHistoryJobs turns the configured sources into collection jobs. Sources
// whose connector has no history fetcher — every spot market, and the oracle —
// are dropped by history.New.
func fundingHistoryJobs(cfg config.Config) []history.Job {
	jobs := make([]history.Job, 0, len(cfg.Sources))
	for _, source := range cfg.Sources {
		jobs = append(jobs, history.Job{
			Source:    source.Source,
			Connector: source.Connector,
			Symbols:   venueSymbols(cfg, source),
		})
	}
	return jobs
}

// samplePrices writes one cross-section of the price table per tick.
//
// Every row of a round carries the SAME sampled_at_ms, on purpose: a
// cross-venue spread is only meaningful between prices sampled at one instant,
// and stamping each row as it is written would make the query that reconstructs
// a spread a join on approximate times.
//
// A source whose feed died still gets a row, with its old recv_at_ms. That is
// the record of a dead feed, and dropping the row instead would make it
// indistinguishable from the scanner being down.
func samplePrices(ctx context.Context, db *store.Store, s *scanner.Scanner, every time.Duration) {
	tickLoop(ctx, "storage: price sampler", every, every, func(at time.Time) {
		samples := priceSamples(s.PriceSnapshot(), at)
		if len(samples) == 0 {
			return
		}
		if _, err := db.PutPriceSamples(ctx, samples); err != nil && ctx.Err() == nil {
			log.Printf("storage: price samples: %v", err)
		}
	})
}

// lateTickTolerance is how far past its schedule a tick may fire before it
// is a LATE tick. A minute absorbs scheduler jitter and a slow disk; a tick
// later than that means the process was not running its loops — the machine
// slept, or the host stalled — and the live path wrote nothing for that
// long. Run 1 of step 3.5 lost 16 of 32 settlements this way and nothing in
// the process said so (PLAN 3.5, "Nợ mới 2026-09-10"); the log line and the
// counter below are that debt paid.
const lateTickTolerance = time.Minute

// wallNow is the clock lateness is measured on. A variable so the test can
// hand the loop a clock that jumps five hours between arming a timer and
// its firing, which is what a sleeping machine does to the wall clock.
var wallNow = time.Now

// lateTickSink receives every late tick — job, when it fired, how late. main
// points it at the scanner so the count reaches source_status' sibling
// tick_status on the wire; it defaults to a no-op so a job started without a
// scanner (a test) still runs.
var lateTickSink = func(job string, at time.Time, lateBy time.Duration) {}

// tickLoop runs fn after first, then every period, until ctx ends.
//
// The two delays are separate because the jobs below run on periods measured in
// hours: with one delay, a process restarted more often than its period would
// never take an instrument snapshot and never prune, and the database would
// grow forever on a host that reboots nightly. The first delay is a warm-up,
// not a period — long enough for the registry's first fetch to land.
//
// Lateness is measured on WALL readings only, from the instant the timer was
// armed to the instant its firing was RECEIVED. Three facts force that shape
// (review of 2026-09-12): Go's timers run on a monotonic clock that does not
// advance while the machine sleeps (Darwin's CLOCK_UPTIME_RAW), so a
// 10-minute timer armed a minute before a 5-hour sleep fires 9 minutes after
// the wake — on time by its own clock, five hours late by the world's;
// time.Time.Sub uses the monotonic readings whenever both operands carry
// one, which is why the difference is taken on UnixNano; and since Go 1.23
// the value a timer channel delivers is the SCHEDULED instant, backdated, so
// a host that stalled after the deadline would read as on time — the
// receipt instant is what says when the loop actually ran. fn is handed
// that instant too, so a settlement that landed during a stall is stamped
// after it was readable, not before. fn's own running time is excluded: the
// next due instant is taken after fn returns. A forward clock step (NTP
// after a wake) counts as a late tick; that is the same gap by another name.
func tickLoop(ctx context.Context, job string, first, every time.Duration, fn func(time.Time)) {
	due := wallNow().Add(first)
	timer := time.NewTimer(first)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			fired := wallNow()
			if lateBy := time.Duration(fired.UnixNano() - due.UnixNano()); lateBy > lateTickTolerance {
				// Beside it, the monotonic reading of the same gap: ≈0 says the
				// machine slept (or the clock stepped), ≈lateBy says the process
				// was not scheduled.
				log.Printf("%s: tick trễ %s theo đồng hồ tường (đơn điệu %s: ≈0 = máy ngủ hay đồng hồ nhảy, bằng nhau = tiến trình bị treo) — hẹn %s, chạy %s; không có gì được ghi trong khoảng đó",
					job, lateBy.Round(time.Second), fired.Sub(due).Round(time.Second),
					due.UTC().Format(time.RFC3339), fired.UTC().Format(time.RFC3339))
				lateTickSink(job, fired, lateBy)
			}
			fn(fired)
			due = wallNow().Add(every)
			timer.Reset(every)
		}
	}
}

// priceSamples turns one read of the scanner's price table into storable rows.
//
// A reading with no receive stamp is dropped rather than stored with a
// RecvAtMs of -6795364578, which is what a zero time.Time renders as in
// milliseconds — a sample dated 1754 would pass every window filter phase 3
// writes.
func priceSamples(readings []scanner.PriceReading, at time.Time) []store.PriceSample {
	sampledAtMs := at.UnixMilli()
	samples := make([]store.PriceSample, 0, len(readings))
	for _, reading := range readings {
		if reading.RecvAt.IsZero() {
			continue
		}
		samples = append(samples, store.PriceSample{
			Source:         reading.Source,
			Symbol:         reading.Symbol,
			SampledAtMs:    sampledAtMs,
			MidPriceQuote:  reading.Price,
			BestBidQuote:   reading.BestBid,
			BestAskQuote:   reading.BestAsk,
			BestBidQtyCoin: reading.BestBidQtyCoin,
			BestAskQtyCoin: reading.BestAskQtyCoin,
			RecvAtMs:       reading.RecvAt.UnixMilli(),
		})
	}
	return samples
}

// snapshotInstruments records the day's trading rules.
//
// It runs several times a day against a key of (day, source, symbol), so the
// last successful reading of a day is the one kept. Once a day exactly would
// mean a single failed fetch loses that day's rules forever, and the whole
// point of these snapshots is being able to look back.
func snapshotInstruments(ctx context.Context, db *store.Store, registry *instruments.Registry, every time.Duration) {
	tickLoop(ctx, "storage: instrument snapshot", instrumentSnapshotWarmup, every, func(at time.Time) {
		// UTC, so a restart at 23:59 and one at 00:01 land in different
		// snapshots rather than in whichever day the host's zone believes.
		day := at.UTC().Format(time.DateOnly)
		written, err := db.PutInstrumentSnapshot(ctx, day, registry.Snapshot())
		if err != nil && ctx.Err() == nil {
			log.Printf("storage: instrument snapshot: %v", err)
			return
		}
		if written > 0 {
			log.Printf("storage: instrument snapshot %s: %d markets", day, written)
		}
	})
}

const (
	// instrumentSnapshotWarmup is how long the first snapshot waits for the
	// registry's startup fetch of nine venues to land. An empty registry writes
	// nothing rather than an empty day, so being early costs a skipped
	// snapshot, not a wrong one.
	instrumentSnapshotWarmup = 2 * time.Minute

	// pruneWarmup keeps the first retention pass off the startup path, where it
	// would compete with nine venues connecting for the store's single
	// connection.
	pruneWarmup = 5 * time.Minute
)

func prune(ctx context.Context, db *store.Store, every time.Duration, policy store.Retention) {
	tickLoop(ctx, "storage: prune", pruneWarmup, every, func(time.Time) {
		result, err := db.Prune(ctx, policy)
		if err != nil && ctx.Err() == nil {
			log.Printf("storage: prune: %v", err)
			return
		}
		if result.FundingRows > 0 || result.PriceRows > 0 || result.DepthRows > 0 {
			log.Printf("storage: pruned %d funding rows, %d price rows and %d depth rows",
				result.FundingRows, result.PriceRows, result.DepthRows)
		}
	})
}
