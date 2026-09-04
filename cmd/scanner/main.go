// Command scanner runs the arbitrage scanner: it reads config.yaml, wires the
// venue connectors it names into the scanner engine (internal/scanner), and
// serves the dashboard.
//
// All behavior lives in internal/scanner; this file only assembles it. Run from
// the repository root so ./static and ./config.yaml resolve:
//
//	go run ./cmd/scanner
//
// A different config file can be given with -config. Interrupt it (Ctrl-C) and
// every connector and every internal goroutine stops before the process exits.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/instruments"
	"futures-arbitrage-scanner/internal/scanner"
	"futures-arbitrage-scanner/internal/store"

	"github.com/joho/godotenv"
)

// shutdownBudget is how long the whole shutdown may take. The acceptance
// criterion for step 1.5 is that cancelling the context stops every connector
// within 5 seconds, so exceeding this is a defect and gets logged as one rather
// than silently waited out.
const shutdownBudget = 5 * time.Second

func main() {
	configPath := flag.String("config", "config.yaml", "path to the configuration file")
	flag.Parse()

	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using system environment variables")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		// Refusing to start beats starting with no sources, which on the
		// dashboard is indistinguishable from every venue being down.
		log.Fatalf("configuration: %v", err)
	}

	// One context for everything the process starts. Interrupting cancels it,
	// which is what stops the connectors, the scanner's goroutines and the HTTP
	// server - in that order, and without any of them polling a flag.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	scanner.Configure(cfg)
	s := scanner.New(cfg.SymbolNames())
	s.Run(ctx)

	connectors := startConnectors(ctx, cfg, s)
	// One registry, threaded through. Step 2.4 reads it for the hedge mapping,
	// 2.6 for the daily snapshots, and 2.7 for the funding table's hedge column
	// — do NOT build a second one elsewhere.
	registry := startInstrumentRegistry(ctx, cfg, s)

	// Persistence (step 2.6). Its jobs get their own WaitGroup: the store is
	// closed after they stop, and closing it underneath a job mid-transaction
	// is how a WAL file ends up needing recovery.
	var recorders sync.WaitGroup
	db := startStore(ctx, cfg, s, registry, &recorders)

	// Depth (step 2.7b) shares the recorders' WaitGroup: it writes to the same
	// store, so it has to finish before main closes it.
	startDepth(ctx, cfg, s, registry, db, func(job func()) {
		recorders.Add(1)
		go func() {
			defer recorders.Done()
			job()
		}()
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.HandleWebSocket)
	// Settled funding history for the chart (step 2.7). Read-only, and the only
	// route that touches the store — the push contract stays on /ws.
	mux.HandleFunc("/api/funding/history", newFundingHistoryHandler(db, cfg))
	mux.Handle("/", http.FileServer(http.Dir("./static/")))

	port := os.Getenv("PORT")
	if port == "" {
		port = cfg.Server.Port
	}
	server := &http.Server{Addr: ":" + port, Handler: mux}

	go func() {
		log.Printf("Server starting on http://localhost:%s", port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP server stopped: %v", err)
			stop() // a scanner nobody can reach is not worth keeping alive
		}
	}()

	<-ctx.Done()

	// The clock for the acceptance criterion starts HERE, at the cancellation -
	// not inside shutdown. Everything below is what the budget has to cover.
	cancelledAt := time.Now()
	log.Printf("Shutting down...")
	shutdown(server, connectors, cancelledAt)

	// After the budget, not inside it: the 5s acceptance criterion of step 1.5
	// measures the connectors, and a recording job finishing a transaction must
	// not be able to spend that budget or to be abandoned by it.
	closeStore(db, &recorders)
}

// storeShutdownBudget bounds the wait for the recording jobs.
//
// Bounded rather than open-ended for the same reason the connector wait is: a
// process that will not exit is worse than one that leaves a transaction
// uncommitted, and an uncommitted SQLite transaction costs the rows of one tick
// and nothing else. Every job selects on the context, so reaching this is a
// defect and says so.
const storeShutdownBudget = 5 * time.Second

func closeStore(db *store.Store, recorders *sync.WaitGroup) {
	if db == nil {
		return
	}
	stopped := make(chan struct{})
	go func() {
		recorders.Wait()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(storeShutdownBudget):
		log.Printf("WARNING: storage jobs still running %s after cancellation, closing anyway", storeShutdownBudget)
	}
	if err := db.Close(); err != nil {
		log.Printf("storage: close: %v", err)
	}
}

// shutdown closes the HTTP server and waits for every connector to return.
//
// Both happen under ONE budget measured from the cancellation. Giving each its
// own would allow twice the documented 5s, and starting the clock here would
// leave the HTTP shutdown out of the number the acceptance criterion is read
// from.
func shutdown(server *http.Server, connectors *sync.WaitGroup, cancelledAt time.Time) {
	deadline := cancelledAt.Add(shutdownBudget)

	closeCtx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	// The connectors were cancelled before this was called, so waiting for them
	// only means waiting for them to notice. Run it alongside the HTTP shutdown
	// rather than after it: they are independent, and serialising them would
	// spend the budget twice.
	stopped := make(chan struct{})
	go func() {
		connectors.Wait()
		close(stopped)
	}()

	if err := server.Shutdown(closeCtx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
	}

	// Checked before the timed wait below, and not merged into it: once
	// server.Shutdown has used up the budget the remaining time is negative, and
	// a select between an expired timer and an already-closed channel picks at
	// RANDOM - which would print the warning about connectors that stopped in
	// microseconds, on the one line the acceptance criterion is read from.
	select {
	case <-stopped:
		reportStopped(cancelledAt)
		return
	default:
	}

	remaining := time.Until(deadline)
	if remaining < 0 {
		remaining = 0
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()

	select {
	case <-stopped:
		reportStopped(cancelledAt)
	case <-timer.C:
		// Saying so is the point: this is the step's acceptance criterion, and a
		// connector that has to be abandoned is a bug, not a slow exit.
		log.Printf("WARNING: connectors still running %s after cancellation, exiting anyway", shutdownBudget)
	}
}

// reportStopped logs the number the step 1.5 acceptance criterion is read from.
//
// Microseconds, not milliseconds: cancellation closes each socket underneath its
// blocked reader, so the whole shutdown lands well under a millisecond and
// rounding to ms would print a meaningless "0s" against a 5s budget.
func reportStopped(cancelledAt time.Time) {
	log.Printf("All connectors stopped in %s", time.Since(cancelledAt).Round(time.Microsecond))
}

// startConnectors launches one goroutine per configured source and returns a
// WaitGroup that completes when they have all stopped.
//
// This replaced ten hardcoded calls at step 1.4. The list of venues, and which
// symbols each one is asked for, is data: adding a source that uses an existing
// connector is a config.yaml edit and nothing else.
func startConnectors(ctx context.Context, cfg config.Config, s *scanner.Scanner) *sync.WaitGroup {
	available := exchanges.Connectors()
	feeds := s.Feeds(ctx)

	var running sync.WaitGroup

	for _, source := range cfg.Sources {
		connect, ok := available[source.Connector]
		if !ok {
			// Only reachable if config.yaml names a connector nobody wrote.
			// cmd/scanner's tests catch this before it can happen in
			// production, so failing loudly here is right.
			log.Fatalf("source %s names connector %q, which does not exist",
				source.Source, source.Connector)
		}

		symbols := venueSymbols(cfg, source)
		if len(symbols) == 0 {
			// config.Validate already rejects this; belt and braces, because a
			// connector subscribed to nothing looks healthy forever.
			log.Printf("source %s serves none of the configured symbols, not starting it", source.Source)
			continue
		}

		log.Printf("Starting %s (%s) for %d symbols", source.Source, source.Connector, len(symbols))

		running.Add(1)
		go func(source config.Source, symbols []exchanges.Symbol) {
			defer running.Done()
			connect(source.Source, symbols, feeds)
		}(source, symbols)
	}

	return &running
}

// venueSymbols translates the configured pairs into the identifiers this source
// uses, dropping the ones it does not serve.
func venueSymbols(cfg config.Config, source config.Source) []exchanges.Symbol {
	symbols := make([]exchanges.Symbol, 0, len(cfg.Symbols))
	for _, symbol := range cfg.Symbols {
		venueSymbol, ok := source.VenueSymbol(symbol)
		if !ok {
			continue
		}
		symbols = append(symbols, exchanges.Symbol{
			Standard: symbol.Symbol,
			Venue:    venueSymbol,
		})
	}
	return symbols
}

// startInstrumentRegistry assembles the instrument registry from the same
// config the connectors use and keeps it fresh once a day (step 2.3). A
// failed refresh is logged and retried at the next cycle with yesterday's
// rules still served — the scanner's own data path does not depend on it, so
// it must never take the process down.
func startInstrumentRegistry(ctx context.Context, cfg config.Config, s *scanner.Scanner) *instruments.Registry {
	fetchers := exchanges.InstrumentFetchers()
	var sources []instruments.Source
	for _, source := range cfg.Sources {
		fetch, ok := fetchers[source.Connector]
		if !ok {
			// An oracle has no instruments; anything else without a fetcher
			// is a gap worth seeing in the log once at startup.
			if source.MarketType != "oracle" {
				log.Printf("source %s (%s) has no instrument fetcher — its trading rules stay unknown",
					source.Source, source.Connector)
			}
			continue
		}
		sources = append(sources, instruments.Source{
			Name:    source.Source,
			Symbols: venueSymbols(cfg, source),
			Fetch:   fetch,
		})
	}

	registry := instruments.New(sources)

	// The hedge mapping (step 2.4) is a pure function of the registry plus
	// the config's declarations, so it is rebuilt after every refresh and
	// logged when it CHANGED — the 5-minute failure-retry cycle must not
	// repeat an unchanged table.
	pairs := make([]instruments.PairAssets, 0, len(cfg.Symbols))
	for _, s := range cfg.Symbols {
		pairs = append(pairs, instruments.PairAssets{Symbol: s.Symbol, BaseAsset: s.Base})
	}
	claims := make([]instruments.SourceClaim, 0, len(cfg.Sources))
	for _, s := range cfg.Sources {
		claims = append(claims, instruments.SourceClaim{
			Source:     s.Source,
			MarketType: s.MarketType,
			QuoteAsset: s.QuoteAsset,
			Tradable:   s.Tradable,
		})
	}
	var lastMapping instruments.HedgeMapping
	go registry.Run(ctx, func() {
		mapping := instruments.BuildHedgeMapping(registry.Snapshot(), pairs, claims)
		// Pushed on EVERY refresh, including one that changed nothing: the log
		// below is deduplicated for a human reading it, but the scanner's copy
		// is state and must not depend on whether the last refresh happened to
		// differ. Cheap enough - it is a map of a few dozen entries.
		s.SetHedges(hedgeLegs(cfg, mapping))
		// The same refresh carries the contract→coin multipliers the price
		// path needs (step 2.7b): three venues publish top-of-book size in
		// contracts, and until now the wire showed 0 for all of them.
		s.SetContractSizes(contractSizes(registry.Snapshot()))

		// Compared as a STRUCTURE, not as its rendered text: the log lines
		// carry only symbol and source names, so a venue revising a step or
		// contract size would render identically and go unlogged.
		if reflect.DeepEqual(mapping, lastMapping) { // only Run's goroutine touches lastMapping
			return
		}
		lastMapping = mapping
		log.Printf("hedge mapping: %d pairs, %d refusals\n  %s",
			len(mapping.Pairs), len(mapping.Rejections), strings.Join(mapping.LogLines(), "\n  "))
	})
	return registry
}
