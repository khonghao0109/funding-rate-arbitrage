package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"futures-arbitrage-scanner/exchanges"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

// wsWriteTimeout bounds a single write to one dashboard client. The write mutex
// is shared by every broadcast, so an unbounded write would let one stalled
// browser stop the whole scanner.
const wsWriteTimeout = 2 * time.Second

// opportunityCooldown throttles repeat alerts for the same pair.
const opportunityCooldown = 10 * time.Second

type FuturesScanner struct {
	symbols          []string
	prices           map[string]map[string]float64
	pricesMutex      sync.RWMutex
	wsClients        map[*websocket.Conn]bool
	clientsMutex     sync.RWMutex
	wsWriteMutex     sync.Mutex // Protects WebSocket writes
	upgrader         websocket.Upgrader
	priceChan        chan exchanges.PriceData
	orderbookChan    chan exchanges.OrderbookData
	tradeChan        chan exchanges.TradeData
	lastOpportunity  map[string]time.Time // Track last alert per symbol
	opportunityMutex sync.RWMutex

	// onOpportunity receives every raised opportunity. Production leaves it nil
	// and broadcasts; tests set it to observe the alert path directly.
	onOpportunity func(wireOpportunity)
}

func NewFuturesScanner(symbols []string) *FuturesScanner {
	return &FuturesScanner{
		symbols:         symbols,
		prices:          make(map[string]map[string]float64),
		wsClients:       make(map[*websocket.Conn]bool),
		priceChan:       make(chan exchanges.PriceData, 1000),
		orderbookChan:   make(chan exchanges.OrderbookData, 1000),
		tradeChan:       make(chan exchanges.TradeData, 1000),
		lastOpportunity: make(map[string]time.Time),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
}

func (s *FuturesScanner) processPrices() {
	for priceData := range s.priceChan {
		s.updatePrice(priceData)
	}
}

func (s *FuturesScanner) processOrderbooks() {
	for orderbookData := range s.orderbookChan {
		// Calculate mid price from best bid and best ask
		midPrice := (orderbookData.BestBid + orderbookData.BestAsk) / 2

		priceData := exchanges.PriceData{
			Symbol:    orderbookData.Symbol,
			Source:    orderbookData.Source,
			Price:     midPrice,
			Timestamp: orderbookData.Timestamp,
		}

		s.updatePrice(priceData)
	}
}

func (s *FuturesScanner) processTrades() {
	for range s.tradeChan {
		// Keep trade data for future use but don't use for pricing
	}
}

func (s *FuturesScanner) updatePrice(data exchanges.PriceData) {
	s.pricesMutex.Lock()
	if s.prices[data.Symbol] == nil {
		s.prices[data.Symbol] = make(map[string]float64)
	}
	s.prices[data.Symbol][data.Source] = data.Price
	s.pricesMutex.Unlock()

	s.checkArbitrage(data.Symbol)
}

func (s *FuturesScanner) checkArbitrage(symbol string) {
	s.pricesMutex.RLock()
	sourcePrices, exists := s.prices[symbol]
	if !exists {
		s.pricesMutex.RUnlock()
		return
	}

	// Create a copy of the prices map to avoid race conditions
	pricesCopy := make(map[string]float64)
	var excluded []wireExcludedSource
	for source, price := range sourcePrices {
		// An unusable price is not data. Keeping it would make it the minimum,
		// drive every spread against it and suppress real alerts. Dropping it
		// silently would be just as bad, so say so on the wire.
		if !isUsablePrice(price) {
			excluded = append(excluded, wireExcludedSource{
				Source: source,
				Reason: "no_price",
				NoteVI: "Sàn đang gửi giá không dùng được, đã loại khỏi so sánh",
			})
			continue
		}
		pricesCopy[source] = price
	}
	s.pricesMutex.RUnlock()

	// The matrix is always republished, including when it shrinks to nothing.
	// Returning here instead would leave the previous matrix frozen on screen
	// with no field contradicting it.
	defer s.broadcastSpreads(symbol, pricesCopy, excluded)

	if len(pricesCopy) < 2 {
		return
	}

	var minPrice, maxPrice float64
	var minSource, maxSource string
	first := true

	for source, price := range pricesCopy {
		if first {
			minPrice = price
			maxPrice = price
			minSource = source
			maxSource = source
			first = false
			continue
		}

		if price < minPrice {
			minPrice = price
			minSource = source
		}
		if price > maxPrice {
			maxPrice = price
			maxSource = source
		}
	}

	// This is a GROSS spread: no fee, funding or slippage has been deducted.
	// Step 1.3 adds the fee model; slippage needs order book depth and does not
	// arrive before phase 2.
	grossPct := spreadGrossPct(minPrice, maxPrice)

	// Only alert if the gross spread is significant and we haven't alerted recently
	if grossPct > alertMinSpreadPct {
		opportunityKey := fmt.Sprintf("%s_%s_%s", symbol, minSource, maxSource)

		now := time.Now()

		// Claiming the window must be atomic: two ingestion goroutines reaching
		// here together would otherwise both pass the check and emit the same
		// alert twice, with the same derived id.
		s.opportunityMutex.Lock()
		lastAlert, exists := s.lastOpportunity[opportunityKey]
		claimed := !exists || now.Sub(lastAlert) > opportunityCooldown
		if claimed {
			s.lastOpportunity[opportunityKey] = now
		}
		s.opportunityMutex.Unlock()

		if claimed {
			opportunity := newWireOpportunity(
				symbol, minSource, maxSource, minPrice, maxPrice, now.UnixMilli(),
			)
			if s.onOpportunity != nil {
				s.onOpportunity(opportunity)
			}
			s.broadcastOpportunity(opportunity)
		}
	}

}

// broadcast writes one contract message to every connected client and drops the
// clients that fail. All three message types share it so the fan-out and the
// client cleanup exist in exactly one place.
func (s *FuturesScanner) broadcast(message any) {
	s.clientsMutex.RLock()
	clients := make([]*websocket.Conn, 0, len(s.wsClients))
	for client := range s.wsClients {
		clients = append(clients, client)
	}
	s.clientsMutex.RUnlock()

	if len(clients) == 0 {
		return
	}

	toRemove, err := s.writeToClients(clients, message)
	if err != nil {
		log.Printf("WebSocket broadcast dropped: %v", err)
		return
	}

	if len(toRemove) > 0 {
		s.clientsMutex.Lock()
		for _, client := range toRemove {
			delete(s.wsClients, client)
		}
		s.clientsMutex.Unlock()
	}
}

// writeToClients holds the shared write mutex for the shortest possible span and
// returns the clients whose write failed.
//
// The deadline bounds one write, so a client that stops reading is dropped
// instead of blocking forever. It does not make this cheap: broadcast still runs
// synchronously on the ingestion path, so N stalled clients cost up to
// N*wsWriteTimeout before the tick completes. Removing that needs a per-client
// send queue, which belongs with the broadcast rework in PLAN.md §7.3.
func (s *FuturesScanner) writeToClients(clients []*websocket.Conn, message any) ([]*websocket.Conn, error) {
	// Encode once. WriteJSON per client would also make an encoding failure look
	// like a transport failure, evicting every connected dashboard over one
	// unencodable value, and sending each a truncated frame first.
	payload, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode %T: %w", message, err)
	}

	s.wsWriteMutex.Lock()
	defer s.wsWriteMutex.Unlock()

	var toRemove []*websocket.Conn
	for _, client := range clients {
		if err := client.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
			log.Printf("WebSocket set deadline error: %v", err)
			client.Close()
			toRemove = append(toRemove, client)
			continue
		}
		if err := client.WriteMessage(websocket.TextMessage, payload); err != nil {
			log.Printf("WebSocket write error: %v", err)
			client.Close()
			toRemove = append(toRemove, client)
		}
	}
	return toRemove, nil
}

func (s *FuturesScanner) broadcastOpportunity(opportunity wireOpportunity) {
	s.broadcast(wireArbitrage{
		Type:         "arbitrage",
		V:            wireVersion,
		ServerTimeMs: time.Now().UnixMilli(),
		Opportunity:  opportunity,
	})
}

func (s *FuturesScanner) broadcastSpreads(symbol string, sourcePrices map[string]float64, excluded []wireExcludedSource) {
	// Build the O(n^2) matrix only if there is somebody to send it to.
	if !s.hasClients() {
		return
	}
	s.broadcast(newWireSpreads(symbol, sourcePrices, excluded, time.Now().UnixMilli()))
}

// hasClients reports whether any dashboard is connected, so the broadcast path
// can skip building a message nobody will receive.
func (s *FuturesScanner) hasClients() bool {
	s.clientsMutex.RLock()
	defer s.clientsMutex.RUnlock()
	return len(s.wsClients) > 0
}

func (s *FuturesScanner) broadcastPrices() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		if !s.hasClients() {
			continue
		}

		s.pricesMutex.RLock()
		pricesCopy := make(map[string]map[string]float64, len(s.prices))
		for symbol, prices := range s.prices {
			pricesCopy[symbol] = make(map[string]float64, len(prices))
			for source, price := range prices {
				pricesCopy[symbol][source] = price
			}
		}
		s.pricesMutex.RUnlock()

		if len(pricesCopy) > 0 {
			s.broadcast(newWirePrices(pricesCopy, time.Now().UnixMilli()))
		}
	}
}

func (s *FuturesScanner) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	log.Printf("WebSocket connection attempt from %s", r.RemoteAddr)

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error from %s: %v", r.RemoteAddr, err)
		return
	}
	defer conn.Close()

	// The dashboard builds its source list, symbol selector and cost disclaimer
	// from this message, so it has to arrive before any data. Sending it before
	// the client joins wsClients guarantees no broadcast can overtake it.
	failed, err := s.writeToClients([]*websocket.Conn{conn}, newWireMeta(s.symbols, time.Now().UnixMilli()))
	if err != nil || len(failed) > 0 {
		// Registering a client that never received meta would leave a blank
		// dashboard reading "Connected".
		if err == nil {
			err = fmt.Errorf("client dropped the connection")
		}
		log.Printf("WebSocket meta write failed for %s, dropping connection: %v", r.RemoteAddr, err)
		return
	}

	s.clientsMutex.Lock()
	s.wsClients[conn] = true
	clientCount := len(s.wsClients)
	s.clientsMutex.Unlock()

	log.Printf("WebSocket client connected from %s. Total clients: %d", r.RemoteAddr, clientCount)

	defer func() {
		s.clientsMutex.Lock()
		delete(s.wsClients, conn)
		log.Printf("WebSocket client disconnected. Total clients: %d", len(s.wsClients))
		s.clientsMutex.Unlock()
	}()

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println("No .env file found, using system environment variables")
	}

	symbols := []string{"BTCUSDT", "ETHUSDT", "XRPUSDT", "SOLUSDT"}

	scanner := NewFuturesScanner(symbols)

	// Start processing goroutines
	go scanner.processPrices()
	go scanner.processOrderbooks()
	go scanner.processTrades()

	// Start exchange connections with orderbook feeds
	go exchanges.ConnectBinanceFutures(symbols, scanner.priceChan, scanner.orderbookChan, scanner.tradeChan)
	go exchanges.ConnectBybitFutures(symbols, scanner.priceChan, scanner.orderbookChan, scanner.tradeChan)
	go exchanges.ConnectHyperliquidFutures(symbols, scanner.priceChan, scanner.orderbookChan, scanner.tradeChan)
	go exchanges.ConnectKrakenFutures(symbols, scanner.priceChan, scanner.orderbookChan, scanner.tradeChan)
	go exchanges.ConnectOKXFutures(symbols, scanner.priceChan, scanner.orderbookChan, scanner.tradeChan)
	go exchanges.ConnectGateFutures(symbols, scanner.priceChan, scanner.orderbookChan, scanner.tradeChan)
	go exchanges.ConnectParadexFutures(symbols, scanner.priceChan, scanner.orderbookChan, scanner.tradeChan)

	// Start spot exchange connections with orderbook feeds
	go exchanges.ConnectBinanceSpot(symbols, scanner.priceChan, scanner.orderbookChan, scanner.tradeChan)
	go exchanges.ConnectBybitSpot(symbols, scanner.priceChan, scanner.orderbookChan, scanner.tradeChan)

	// Start Pyth price feed connection
	go exchanges.ConnectPythPrices(symbols, scanner.priceChan, scanner.orderbookChan, scanner.tradeChan)

	go scanner.broadcastPrices()

	http.HandleFunc("/ws", scanner.handleWebSocket)
	http.Handle("/", http.FileServer(http.Dir("./static/")))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}

	log.Printf("Server starting on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
