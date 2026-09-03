// Command scanner runs the arbitrage scanner: it reads config.yaml, wires the
// venue connectors it names into the scanner engine (internal/scanner), and
// serves the dashboard.
//
// All behavior lives in internal/scanner; this file only assembles it. Run from
// the repository root so ./static and ./config.yaml resolve:
//
//	go run ./cmd/scanner
//
// A different config file can be given with -config.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/scanner"

	"github.com/joho/godotenv"
)

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

	scanner.Configure(cfg)
	s := scanner.New(cfg.SymbolNames())
	s.Run()

	startConnectors(cfg, s)

	http.HandleFunc("/ws", s.HandleWebSocket)
	http.Handle("/", http.FileServer(http.Dir("./static/")))

	port := os.Getenv("PORT")
	if port == "" {
		port = cfg.Server.Port
	}

	log.Printf("Server starting on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// startConnectors launches one goroutine per configured source.
//
// This replaced ten hardcoded calls. The list of venues, and which symbols each
// one is asked for, is now data: adding a source that uses an existing connector
// is a config.yaml edit and nothing else.
func startConnectors(cfg config.Config, s *scanner.Scanner) {
	available := exchanges.Connectors()
	prices, orderbooks, trades := s.PriceFeed(), s.OrderbookFeed(), s.TradeFeed()

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
		go connect(source.Source, symbols, prices, orderbooks, trades)
	}
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
