// Command scanner runs the arbitrage scanner: it wires the venue connectors
// into the scanner engine (internal/scanner) and serves the dashboard.
//
// All behavior lives in internal/scanner; this file only assembles it. Run
// from the repository root so ./static resolves:
//
//	go run ./cmd/scanner
package main

import (
	"log"
	"net/http"
	"os"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/scanner"

	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using system environment variables")
	}

	symbols := []string{"BTCUSDT", "ETHUSDT", "XRPUSDT", "SOLUSDT"}

	s := scanner.New(symbols)
	s.Run()

	prices, orderbooks, trades := s.PriceFeed(), s.OrderbookFeed(), s.TradeFeed()

	// Futures venues.
	go exchanges.ConnectBinanceFutures(symbols, prices, orderbooks, trades)
	go exchanges.ConnectBybitFutures(symbols, prices, orderbooks, trades)
	go exchanges.ConnectHyperliquidFutures(symbols, prices, orderbooks, trades)
	go exchanges.ConnectKrakenFutures(symbols, prices, orderbooks, trades)
	go exchanges.ConnectOKXFutures(symbols, prices, orderbooks, trades)
	go exchanges.ConnectGateFutures(symbols, prices, orderbooks, trades)
	go exchanges.ConnectParadexFutures(symbols, prices, orderbooks, trades)

	// Spot venues.
	go exchanges.ConnectBinanceSpot(symbols, prices, orderbooks, trades)
	go exchanges.ConnectBybitSpot(symbols, prices, orderbooks, trades)

	// Oracle reference feed.
	go exchanges.ConnectPythPrices(symbols, prices, orderbooks, trades)

	http.HandleFunc("/ws", s.HandleWebSocket)
	http.Handle("/", http.FileServer(http.Dir("./static/")))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}

	log.Printf("Server starting on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
