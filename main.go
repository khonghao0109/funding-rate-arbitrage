package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
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

// stalenessRefreshInterval is how often every symbol is re-examined even when
// nothing arrives for it.
const stalenessRefreshInterval = time.Second

// PricePoint is one venue's latest price for one symbol, with everything needed
// to decide whether it can still be trusted.
//
// RecvAt is the only basis for staleness. VenueTimeMs is diagnostic: it is 0 for
// the venues that publish no timestamp, and comparing venue clocks with ours
// would measure clock skew, not freshness.
type PricePoint struct {
	Price       float64
	VenueTimeMs int64
	RecvAt      time.Time
}

type FuturesScanner struct {
	symbols     []string
	prices      map[string]map[string]PricePoint
	pricesMutex sync.RWMutex

	// sourceLastMsgAt is the last time anything at all arrived from a source,
	// across every symbol. A venue can be healthy while one thin pair goes
	// quiet, so connection state and price freshness are measured separately.
	//
	// It has its own mutex: trades update it at roughly a thousand messages a
	// second and must not contend with every price read.
	sourceLastMsgAt  map[string]time.Time
	lastMsgMutex     sync.RWMutex
	wsClients        map[*websocket.Conn]bool
	clientsMutex     sync.RWMutex
	wsWriteMutex     sync.Mutex // Protects WebSocket writes
	upgrader         websocket.Upgrader
	priceChan        chan exchanges.PriceData
	orderbookChan    chan exchanges.OrderbookData
	tradeChan        chan exchanges.TradeData
	lastOpportunity  map[string]time.Time // Track last alert per symbol
	opportunityMutex sync.RWMutex

	// lastUsableSet is the set of sources last published per symbol, so the
	// staleness timer can skip republishing an identical matrix.
	lastUsableSet map[string]string
	usableMutex   sync.Mutex

	// onOpportunity receives every raised opportunity. Production leaves it nil
	// and broadcasts; tests set it to observe the alert path directly.
	onOpportunity func(wireOpportunity)

	// now is the scanner's clock, injectable so staleness can be tested without
	// sleeping.
	now func() time.Time

	// startedAt distinguishes "this venue has not sent anything yet" at startup
	// from "this venue never connected".
	startedAt time.Time
}

func NewFuturesScanner(symbols []string) *FuturesScanner {
	s := &FuturesScanner{
		symbols:         symbols,
		now:             time.Now,
		prices:          make(map[string]map[string]PricePoint),
		sourceLastMsgAt: make(map[string]time.Time),
		wsClients:       make(map[*websocket.Conn]bool),
		priceChan:       make(chan exchanges.PriceData, 1000),
		orderbookChan:   make(chan exchanges.OrderbookData, 1000),
		tradeChan:       make(chan exchanges.TradeData, 1000),
		lastOpportunity: make(map[string]time.Time),
		lastUsableSet:   make(map[string]string),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
	s.startedAt = s.now()
	return s
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
			Symbol:      orderbookData.Symbol,
			Source:      orderbookData.Source,
			Price:       midPrice,
			VenueTimeMs: orderbookData.VenueTimeMs,
		}

		s.updatePrice(priceData)
	}
}

func (s *FuturesScanner) processTrades() {
	for tradeData := range s.tradeChan {
		// Trades are not used for pricing, but one arriving proves the socket is
		// alive. Without this, a venue whose book simply has not moved looks
		// disconnected on a change-driven feed in a quiet market.
		s.markSourceAlive(tradeData.Source, s.now())
	}
}

// updatePrice is the single place the scanner stamps a receive time. Every
// staleness decision downstream is measured from it, so it must not be set
// anywhere else - a second stamping site is how the two-meaning Timestamp field
// this step replaces came about.
func (s *FuturesScanner) updatePrice(data exchanges.PriceData) {
	recvAt := s.now()

	s.pricesMutex.Lock()
	if s.prices[data.Symbol] == nil {
		s.prices[data.Symbol] = make(map[string]PricePoint)
	}
	s.prices[data.Symbol][data.Source] = PricePoint{
		Price:       data.Price,
		VenueTimeMs: data.VenueTimeMs,
		RecvAt:      recvAt,
	}
	s.pricesMutex.Unlock()

	s.markSourceAlive(data.Source, recvAt)

	s.checkArbitrage(data.Symbol)
}

// usableSetChanged reports whether the set of sources usable for this symbol
// differs from the last time it was published, and records the new set.
func (s *FuturesScanner) usableSetChanged(symbol string, usable map[string]float64) bool {
	sources := make([]string, 0, len(usable))
	for source := range usable {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	signature := strings.Join(sources, ",")

	s.usableMutex.Lock()
	defer s.usableMutex.Unlock()

	if previous, seen := s.lastUsableSet[symbol]; seen && previous == signature {
		return false
	}
	s.lastUsableSet[symbol] = signature
	return true
}

// markSourceAlive records that something arrived from a source, whatever it was.
func (s *FuturesScanner) markSourceAlive(source string, at time.Time) {
	s.lastMsgMutex.Lock()
	s.sourceLastMsgAt[source] = at
	s.lastMsgMutex.Unlock()
}

func (s *FuturesScanner) snapshotLastMsgAt() map[string]time.Time {
	s.lastMsgMutex.RLock()
	defer s.lastMsgMutex.RUnlock()

	out := make(map[string]time.Time, len(s.sourceLastMsgAt))
	for source, at := range s.sourceLastMsgAt {
		out[source] = at
	}
	return out
}

func (s *FuturesScanner) checkArbitrage(symbol string) {
	s.evaluate(symbol, true)
}

// republishSpreads recomputes and publishes the matrix WITHOUT raising alerts.
//
// refreshStaleness fires on a timer with no new quote behind it. An alert from
// there would be stamped with the current time while describing prices observed
// seconds earlier.
func (s *FuturesScanner) republishSpreads(symbol string) {
	s.evaluate(symbol, false)
}

func (s *FuturesScanner) evaluate(symbol string, raiseAlerts bool) {
	s.pricesMutex.RLock()
	sourcePrices, exists := s.prices[symbol]
	if !exists {
		s.pricesMutex.RUnlock()
		return
	}

	// Create a copy of the prices map to avoid race conditions
	now := s.now()
	pricesCopy := make(map[string]float64)
	var excluded []wireExcludedSource
	for source, point := range sourcePrices {
		// An unusable price is not data. Keeping it would make it the minimum,
		// drive every spread against it and suppress real alerts. Dropping it
		// silently would be just as bad, so say so on the wire.
		if !isUsablePrice(point.Price) {
			excluded = append(excluded, wireExcludedSource{
				Source: source,
				Reason: "no_price",
				NoteVI: "Sàn đang gửi giá không dùng được, đã loại khỏi so sánh",
			})
			continue
		}
		// A venue that stopped sending keeps its last price in state, and that
		// frozen number would go on producing spreads and alerts against a
		// market that may no longer exist.
		if priceStatus(point, staleAfter(source), now) == statusStale {
			excluded = append(excluded, wireExcludedSource{
				Source: source,
				Reason: "stale",
				NoteVI: fmt.Sprintf("Không nhận được dữ liệu quá %s, giá đã đóng băng",
					staleAfter(source)),
			})
			continue
		}
		pricesCopy[source] = point.Price
	}
	s.pricesMutex.RUnlock()

	// The matrix is always republished when a price arrives, including when it
	// shrinks to nothing: returning instead would leave the previous matrix
	// frozen on screen with no field contradicting it.
	//
	// The timer path publishes only when the usable set actually changed. In
	// steady state that is never, and an identical matrix every second would
	// only add contention on the write mutex that ingestion also holds.
	// Called unconditionally: || would short-circuit on the alert path and never
	// record what was published, so the timer would see a change every tick.
	changed := s.usableSetChanged(symbol, pricesCopy)
	if raiseAlerts || changed {
		defer s.broadcastSpreads(symbol, pricesCopy, excluded, now)
	}

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
	if raiseAlerts && grossPct > alertMinSpreadPct {
		opportunityKey := fmt.Sprintf("%s_%s_%s", symbol, minSource, maxSource)

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
		ServerTimeMs: s.now().UnixMilli(),
		Opportunity:  opportunity,
	})
}

// snapshotAt is the moment the prices in this message were read, not the moment
// it is sent. checkArbitrage and refreshStaleness both publish per symbol, so
// stamping at send time would let an older matrix carry a later timestamp.
//
// This orders messages the frontend compares; it does not serialise the two
// producers. Two matrices computed microseconds apart can still be delivered in
// either order, and the older one wins only if it also carries the later
// snapshot time - which this prevents.
func (s *FuturesScanner) broadcastSpreads(symbol string, sourcePrices map[string]float64, excluded []wireExcludedSource, snapshotAt time.Time) {
	// Build the O(n^2) matrix only if there is somebody to send it to.
	if !s.hasClients() {
		return
	}
	s.broadcast(newWireSpreads(symbol, sourcePrices, excluded, snapshotAt.UnixMilli()))
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
		pricesCopy := make(map[string]map[string]PricePoint, len(s.prices))
		for symbol, prices := range s.prices {
			pricesCopy[symbol] = make(map[string]PricePoint, len(prices))
			for source, point := range prices {
				pricesCopy[symbol][source] = point
			}
		}
		s.pricesMutex.RUnlock()

		lastMsgCopy := s.snapshotLastMsgAt()

		// Sent even with no prices at all: the message carries the status of
		// every registered source, and a total outage is precisely when the
		// dashboard needs to be told.
		s.broadcast(newWirePrices(pricesCopy, lastMsgCopy, s.startedAt, s.now()))
	}
}

// refreshStaleness re-evaluates every symbol on a timer.
//
// checkArbitrage only runs when a price arrives, so a symbol whose sources have
// all gone quiet would never be re-examined: the dashboard would keep showing
// the last matrix, and the opportunities in it, for as long as the silence
// lasted. Nothing arriving is exactly the case staleness has to catch.
func (s *FuturesScanner) refreshStaleness() {
	ticker := time.NewTicker(stalenessRefreshInterval)
	defer ticker.Stop()

	for range ticker.C {
		if !s.hasClients() {
			continue
		}

		s.pricesMutex.RLock()
		symbols := make([]string, 0, len(s.prices))
		for symbol := range s.prices {
			symbols = append(symbols, symbol)
		}
		s.pricesMutex.RUnlock()

		for _, symbol := range symbols {
			s.republishSpreads(symbol)
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
	failed, err := s.writeToClients([]*websocket.Conn{conn}, newWireMeta(s.symbols, s.now().UnixMilli()))
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
	go scanner.refreshStaleness()

	http.HandleFunc("/ws", scanner.handleWebSocket)
	http.Handle("/", http.FileServer(http.Dir("./static/")))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}

	log.Printf("Server starting on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
