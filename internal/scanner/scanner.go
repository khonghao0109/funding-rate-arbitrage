package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"futures-arbitrage-scanner/exchanges"

	"github.com/gorilla/websocket"
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

	// Top of book behind Price, 0 when the source publishes none. Prices are in
	// the source's own quote asset; quantities are in base coin and are 0 for
	// every venue whose book is denominated in contracts, because converting
	// needs an instrument registry that does not exist yet. 0 therefore means
	// "not known", never "no liquidity" - see exchanges.OrderbookData.
	BestBid        float64
	BestAsk        float64
	BestBidQtyCoin float64
	BestAskQtyCoin float64
}

type Scanner struct {
	symbols     []string
	prices      map[string]map[string]PricePoint
	pricesMutex sync.RWMutex

	// sourceLastMsgAt is the last time anything at all arrived from a source,
	// across every symbol. A venue can be healthy while one thin pair goes
	// quiet, so connection state and price freshness are measured separately.
	//
	// It has its own mutex: trades update it at roughly a thousand messages a
	// second and must not contend with every price read.
	sourceLastMsgAt map[string]time.Time
	lastMsgMutex    sync.RWMutex

	// sourceConn is what each connector reports about its OWN socket, as
	// opposed to what silence implies. The two answer different questions and
	// both are needed: this one knows a venue is unreachable while the last
	// message is still recent, and silence catches a socket that stayed open
	// and stopped delivering. Step 1.1 could only infer.
	sourceConn       map[string]sourceConn
	connMutex        sync.RWMutex
	wsClients        map[*websocket.Conn]bool
	clientsMutex     sync.RWMutex
	wsWriteMutex     sync.Mutex // Protects WebSocket writes
	upgrader         websocket.Upgrader
	priceChan        chan exchanges.PriceData
	orderbookChan    chan exchanges.OrderbookData
	tradeChan        chan exchanges.TradeData
	connChan         chan exchanges.ConnEvent
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

func New(symbols []string) *Scanner {
	s := &Scanner{
		symbols:         symbols,
		now:             time.Now,
		prices:          make(map[string]map[string]PricePoint),
		sourceLastMsgAt: make(map[string]time.Time),
		wsClients:       make(map[*websocket.Conn]bool),
		priceChan:       make(chan exchanges.PriceData, 1000),
		orderbookChan:   make(chan exchanges.OrderbookData, 1000),
		tradeChan:       make(chan exchanges.TradeData, 1000),
		// Connection events are rare and must never block a connector, so the
		// buffer only has to absorb every source flapping at once.
		connChan:        make(chan exchanges.ConnEvent, 256),
		sourceConn:      make(map[string]sourceConn),
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

func (s *Scanner) processPrices(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case priceData, ok := <-s.priceChan:
			if !ok {
				return
			}
			s.updatePrice(priceData)
		}
	}
}

func (s *Scanner) processOrderbooks(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case orderbookData, ok := <-s.orderbookChan:
			if !ok {
				return
			}
			// Calculate mid price from best bid and best ask
			midPrice := (orderbookData.BestBid + orderbookData.BestAsk) / 2

			s.updatePrice(exchanges.PriceData{
				Symbol:      orderbookData.Symbol,
				Source:      orderbookData.Source,
				Price:       midPrice,
				VenueTimeMs: orderbookData.VenueTimeMs,
				// Carried through, not re-stamped: this is when the venue's
				// message came off the socket, and re-taking it here would
				// measure how long the message sat in the channel above.
				RecvAt: orderbookData.RecvAt,
				// The book was parsed and thrown away before step 1.2. It is the
				// first-order liquidity filter, and it costs no extra bandwidth.
				BestBid:        orderbookData.BestBid,
				BestAsk:        orderbookData.BestAsk,
				BestBidQtyCoin: orderbookData.BestBidQtyCoin,
				BestAskQtyCoin: orderbookData.BestAskQtyCoin,
			})
		}
	}
}

func (s *Scanner) processTrades(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case tradeData, ok := <-s.tradeChan:
			if !ok {
				return
			}
			// Trades are not used for pricing, but one arriving proves the socket
			// is alive. Without this, a venue whose book simply has not moved
			// looks disconnected on a change-driven feed in a quiet market.
			s.markSourceAlive(tradeData.Source, s.receivedAt(tradeData.RecvAt))
		}
	}
}

// processConnEvents records what the connectors say about their own sockets.
func (s *Scanner) processConnEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-s.connChan:
			if !ok {
				return
			}
			s.applyConnEvent(event)
		}
	}
}

// applyConnEvent folds one connector-reported transition into the source's
// connection record.
func (s *Scanner) applyConnEvent(event exchanges.ConnEvent) {
	s.connMutex.Lock()
	defer s.connMutex.Unlock()

	current := s.sourceConn[event.Source]
	switch event.State {
	case exchanges.ConnConnected:
		// The first connection is not a reconnection. Counting it would report
		// every venue as having reconnected once before anything went wrong,
		// which makes the number useless for spotting the venue that actually
		// flapped overnight - the reason it is on the wire at all.
		//
		// The test is "has this source ever been CONNECTED", not "has it ever
		// reported": a source whose first dial fails reports reconnecting first,
		// and keying on any prior report would count its first working
		// connection as a reconnect.
		if current.EverConnected {
			current.ReconnectCount++
		}
		current.EverConnected = true
		current.ConnectedSince = event.At
	default:
		// Uptime measures the CURRENT unbroken connection, so it restarts from
		// nothing rather than accumulating across outages.
		current.ConnectedSince = time.Time{}
	}
	current.State = event.State
	s.sourceConn[event.Source] = current
}

// snapshotConn copies the connection records for one broadcast.
func (s *Scanner) snapshotConn() map[string]sourceConn {
	s.connMutex.RLock()
	defer s.connMutex.RUnlock()

	out := make(map[string]sourceConn, len(s.sourceConn))
	for source, conn := range s.sourceConn {
		out[source] = conn
	}
	return out
}

// receivedAt is when a message came off the socket.
//
// The connector stamps it there, before parsing and before queueing, because the
// ingestion channels hold 1000 messages and a stamp taken at this end would
// measure our own backlog rather than the venue's silence - a scanner falling
// behind would report every venue as stale. Zero means the data never crossed a
// socket (a test injecting straight into a channel), and only then does the
// scanner fall back to its own clock. See CLAUDE.md rule 13.
func (s *Scanner) receivedAt(stamped time.Time) time.Time {
	if stamped.IsZero() {
		return s.now()
	}
	return stamped
}

// updatePrice records one venue's latest price.
//
// The receive time comes from the connector, which stamped it at the socket
// read; see receivedAt. Until step 1.5 this function took the stamp itself, at
// which point the message had already been through a 1000-deep channel.
func (s *Scanner) updatePrice(data exchanges.PriceData) {
	recvAt := s.receivedAt(data.RecvAt)

	s.pricesMutex.Lock()
	if s.prices[data.Symbol] == nil {
		s.prices[data.Symbol] = make(map[string]PricePoint)
	}
	s.prices[data.Symbol][data.Source] = PricePoint{
		Price:          data.Price,
		VenueTimeMs:    data.VenueTimeMs,
		RecvAt:         recvAt,
		BestBid:        data.BestBid,
		BestAsk:        data.BestAsk,
		BestBidQtyCoin: data.BestBidQtyCoin,
		BestAskQtyCoin: data.BestAskQtyCoin,
	}
	s.pricesMutex.Unlock()

	s.markSourceAlive(data.Source, recvAt)

	s.checkArbitrage(data.Symbol)
}

// usableSetChanged reports whether the set of sources usable for this symbol
// differs from the last time it was published, and records the new set.
func (s *Scanner) usableSetChanged(symbol string, usable map[string]float64) bool {
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
func (s *Scanner) markSourceAlive(source string, at time.Time) {
	s.lastMsgMutex.Lock()
	s.sourceLastMsgAt[source] = at
	s.lastMsgMutex.Unlock()
}

func (s *Scanner) snapshotLastMsgAt() map[string]time.Time {
	s.lastMsgMutex.RLock()
	defer s.lastMsgMutex.RUnlock()

	out := make(map[string]time.Time, len(s.sourceLastMsgAt))
	for source, at := range s.sourceLastMsgAt {
		out[source] = at
	}
	return out
}

func (s *Scanner) checkArbitrage(symbol string) {
	s.evaluate(symbol, true)
}

// republishSpreads recomputes and publishes the matrix WITHOUT raising alerts.
//
// refreshStaleness fires on a timer with no new quote behind it. An alert from
// there would be stamped with the current time while describing prices observed
// seconds earlier.
func (s *Scanner) republishSpreads(symbol string) {
	s.evaluate(symbol, false)
}

func (s *Scanner) evaluate(symbol string, raiseAlerts bool) {
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
				Reason: reasonNoPrice,
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
				Reason: reasonStale,
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

	if !raiseAlerts {
		return
	}

	// An opportunity only exists INSIDE a group. Taking the minimum and the
	// maximum across every source - what this did before step 1.2 - compares
	// spot with perpetual and a venue with an oracle, and reports a number that
	// cannot be executed by anyone. See grouping.go.
	groups, _ := partitionSources(pricesCopy)
	for _, group := range groups {
		if !group.Tradable {
			continue
		}

		buySource, sellSource, ok := bestPair(group.Sources, pricesCopy)
		if !ok {
			continue
		}
		buyPrice, sellPrice := pricesCopy[buySource], pricesCopy[sellSource]

		// This is a GROSS spread: no fee, funding or slippage has been deducted.
		// Step 1.3 adds the fee model; slippage needs order book depth and does
		// not arrive before phase 2.
		grossPct := spreadGrossPct(buyPrice, sellPrice)
		if grossPct <= alertMinSpreadPct {
			continue
		}

		// Keyed by group as well as by pair: a spot alert must not be swallowed
		// by a perpetual alert's cooldown, and the same two venues can appear in
		// two groups.
		opportunityKey := fmt.Sprintf("%s|%s|%s|%s", symbol, group.GroupID, buySource, sellSource)

		// Claiming the window must be atomic: two ingestion goroutines reaching
		// here together would otherwise both pass the check and emit the same
		// alert twice, with the same derived id.
		s.opportunityMutex.Lock()
		lastAlert, seen := s.lastOpportunity[opportunityKey]
		claimed := !seen || now.Sub(lastAlert) > opportunityCooldown
		if claimed {
			s.lastOpportunity[opportunityKey] = now
		}
		s.opportunityMutex.Unlock()

		if !claimed {
			continue
		}

		opportunity := newWireOpportunity(
			symbol, group.GroupID, buySource, sellSource, buyPrice, sellPrice, now.UnixMilli(),
		)
		if s.onOpportunity != nil {
			s.onOpportunity(opportunity)
		}
		s.broadcastOpportunity(opportunity)
	}
}

// broadcast writes one contract message to every connected client and drops the
// clients that fail. All three message types share it so the fan-out and the
// client cleanup exist in exactly one place.
func (s *Scanner) broadcast(message any) {
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
func (s *Scanner) writeToClients(clients []*websocket.Conn, message any) ([]*websocket.Conn, error) {
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

func (s *Scanner) broadcastOpportunity(opportunity wireOpportunity) {
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
func (s *Scanner) broadcastSpreads(symbol string, sourcePrices map[string]float64, excluded []wireExcludedSource, snapshotAt time.Time) {
	// Build the O(n^2) matrix only if there is somebody to send it to.
	if !s.hasClients() {
		return
	}
	s.broadcast(newWireSpreads(symbol, sourcePrices, excluded, snapshotAt.UnixMilli()))
}

// hasClients reports whether any dashboard is connected, so the broadcast path
// can skip building a message nobody will receive.
func (s *Scanner) hasClients() bool {
	s.clientsMutex.RLock()
	defer s.clientsMutex.RUnlock()
	return len(s.wsClients) > 0
}

func (s *Scanner) broadcastPrices(ctx context.Context) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

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
		connCopy := s.snapshotConn()

		// Sent even with no prices at all: the message carries the status of
		// every registered source, and a total outage is precisely when the
		// dashboard needs to be told.
		s.broadcast(newWirePrices(pricesCopy, lastMsgCopy, connCopy, s.startedAt, s.now()))
	}
}

// refreshStaleness re-evaluates every symbol on a timer.
//
// checkArbitrage only runs when a price arrives, so a symbol whose sources have
// all gone quiet would never be re-examined: the dashboard would keep showing
// the last matrix, and the opportunities in it, for as long as the silence
// lasted. Nothing arriving is exactly the case staleness has to catch.
func (s *Scanner) refreshStaleness(ctx context.Context) {
	ticker := time.NewTicker(stalenessRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

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

func (s *Scanner) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
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

// Run starts the scanner's internal goroutines: channel consumers, the
// connection-event reader, the price broadcaster and the staleness refresher.
// It does not start venue connectors or the HTTP server - wiring those is the
// entrypoint's job (cmd/scanner).
//
// Every one of them returns when ctx is cancelled. Before step 1.5 they ranged
// over channels nobody closed and ran until the process died, which is also why
// a test could not stop the goroutine it started: one leaked into the next test
// and read a package-level registry that test was rewriting.
func (s *Scanner) Run(ctx context.Context) {
	go s.processPrices(ctx)
	go s.processOrderbooks(ctx)
	go s.processTrades(ctx)
	go s.processConnEvents(ctx)
	go s.broadcastPrices(ctx)
	go s.refreshStaleness(ctx)
}

// Feeds is what the venue connectors write into, and what tells them to stop.
//
// It replaced the three separate channel accessors this had until step 1.5. The
// gain is not at this end but at the connectors': a fourth feed - the funding
// data phase 2 collects - becomes one new field here and no change to any of the
// ten connector signatures.
func (s *Scanner) Feeds(ctx context.Context) exchanges.Feeds {
	return exchanges.Feeds{
		Ctx:       ctx,
		Price:     s.priceChan,
		Orderbook: s.orderbookChan,
		Trade:     s.tradeChan,
		Conn:      s.connChan,
	}
}
