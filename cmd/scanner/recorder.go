package main

import (
	"context"
	"log"
	"sync"
	"time"

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
		history.New(db, fundingHistoryJobs(cfg)).
			Run(ctx, time.Duration(cfg.Storage.FundingTopUpEveryMin)*time.Minute)
	})
	start(func() {
		prune(ctx, db, time.Duration(cfg.Storage.PruneEveryHours)*time.Hour, store.Retention{
			FundingDays: cfg.Storage.RetainFundingDays,
			PriceDays:   cfg.Storage.RetainPriceDays,
			DepthDays:   cfg.Depth.RetainDays,
		})
	})
	return db
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
	tickLoop(ctx, every, every, func(at time.Time) {
		samples := priceSamples(s.PriceSnapshot(), at)
		if len(samples) == 0 {
			return
		}
		if _, err := db.PutPriceSamples(ctx, samples); err != nil && ctx.Err() == nil {
			log.Printf("storage: price samples: %v", err)
		}
	})
}

// tickLoop runs fn after first, then every period, until ctx ends.
//
// The two delays are separate because the jobs below run on periods measured in
// hours: with one delay, a process restarted more often than its period would
// never take an instrument snapshot and never prune, and the database would
// grow forever on a host that reboots nightly. The first delay is a warm-up,
// not a period — long enough for the registry's first fetch to land.
func tickLoop(ctx context.Context, first, every time.Duration, fn func(time.Time)) {
	timer := time.NewTimer(first)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case at := <-timer.C:
			fn(at)
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
	tickLoop(ctx, instrumentSnapshotWarmup, every, func(at time.Time) {
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
	tickLoop(ctx, pruneWarmup, every, func(time.Time) {
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
