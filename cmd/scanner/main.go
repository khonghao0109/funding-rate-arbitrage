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
	"sync"
	"syscall"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/scanner"

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

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.HandleWebSocket)
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
