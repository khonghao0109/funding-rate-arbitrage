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
	"futures-arbitrage-scanner/internal/depth"

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

// fundingLogEvery is how often the collected funding table is printed. Funding
// moves on a schedule measured in hours, so this is an operational heartbeat —
// proof the feeds are alive — not a data feed. It is the only visibility step
// 2.5 has: the dashboard arrives at 2.7.
const fundingLogEvery = 60 * time.Second

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
	// the source's own quote asset; quantities are in base COIN, converted from
	// contracts where the venue quotes them that way (step 2.7b, via the
	// instrument registry). 0 still means "not known", never "no liquidity":
	// Paradex publishes no size at all, and a market the registry does not know
	// keeps 0 rather than being published unconverted.
	BestBid        float64
	BestAsk        float64
	BestBidQtyCoin float64
	BestAskQtyCoin float64
}

type Scanner struct {
	symbols []string
	// symbolSet answers "is this symbol configured?" without scanning the
	// slice; updateFunding fences on it so an all-market stream at step 2.5
	// cannot grow the funding map with symbols nobody asked for.
	symbolSet   map[string]struct{}
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
	sourceConn map[string]sourceConn
	connMutex  sync.RWMutex

	// late is what the entrypoint's periodic jobs reported about their own
	// scheduling: how many ticks fired late, and the newest one. Under
	// connMutex — it is published beside the connection records.
	late          lateTicks
	wsClients     map[*websocket.Conn]bool
	clientsMutex  sync.RWMutex
	wsWriteMutex  sync.Mutex // Protects WebSocket writes
	upgrader      websocket.Upgrader
	priceChan     chan exchanges.PriceData
	orderbookChan chan exchanges.OrderbookData
	tradeChan     chan exchanges.TradeData
	fundingChan   chan exchanges.FundingData
	connChan      chan exchanges.ConnEvent

	// funding holds the latest normalized reading per symbol per source. It is
	// the collection point steps 2.5–2.7 build on: connectors fill it through
	// fundingChan, persistence (2.6) and the funding dashboard (2.7) read it.
	// Values are already normalized by exchanges' normalize<Venue>Funding builders — the
	// scanner never sees venue units.
	funding      map[string]map[string]exchanges.FundingData
	fundingMutex sync.RWMutex

	// hedges says which spot market each perpetual can be hedged against, keyed
	// by symbol|perp source. It is SET from outside (step 2.7): the mapping is
	// derived from the instrument registry, which refreshes daily, and the wire
	// layer has no business knowing how venue trading rules are fetched.
	//
	// Empty until the first registry refresh lands, and a missing entry reads as
	// "not known yet" rather than "no hedge exists" - the dashboard says so.
	hedges     map[string]HedgeLeg
	hedgeMutex sync.RWMutex

	// depthSummaries is the latest order book measurement per symbol per source
	// (step 2.7b), pushed in from outside on the collector's schedule for the
	// same reason hedges are: the conversion from contracts to coin needs the
	// instrument registry, which the wire layer must not import.
	depthSummaries map[string]map[string]depth.Summary
	depthMutex     sync.RWMutex

	// contractSizeCoin is how many base coins one contract represents, keyed by
	// symbol|source. It converts the TOP-OF-BOOK quantities on the price path,
	// which OKX, Gate and Kraken publish in contracts and which have therefore
	// been 0 on the wire since step 1.2. A market absent from this map keeps 0,
	// which the contract defines as "not known" and never as "no liquidity".
	contractSizes     map[string]float64
	contractSizeMutex sync.RWMutex

	lastOpportunity  map[string]time.Time // Track last alert per symbol
	opportunityMutex sync.RWMutex

	// pendingSpreads is the newest matrix per symbol waiting for the broadcast
	// ticker. See spreadsBroadcastEvery for why it is not sent on the tick that
	// produced it.
	pendingSpreads map[string]wireSpreads
	pendingMutex   sync.Mutex

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
		// Funding changes once per venue-symbol per seconds at worst, so the
		// buffer only has to absorb a reconnect burst — connChan-style sizing,
		// not the 1000 of the price firehose.
		fundingChan:    make(chan exchanges.FundingData, 256),
		funding:        make(map[string]map[string]exchanges.FundingData),
		hedges:         make(map[string]HedgeLeg),
		depthSummaries: make(map[string]map[string]depth.Summary),
		contractSizes:  make(map[string]float64),
		// Connection events are rare and must never block a connector, so the
		// buffer only has to absorb every source flapping at once.
		connChan:        make(chan exchanges.ConnEvent, 256),
		sourceConn:      make(map[string]sourceConn),
		lastOpportunity: make(map[string]time.Time),
		lastUsableSet:   make(map[string]string),
		pendingSpreads:  make(map[string]wireSpreads),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
	s.symbolSet = make(map[string]struct{}, len(symbols))
	for _, symbol := range symbols {
		s.symbolSet[symbol] = struct{}{}
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

			// Top-of-book quantities arrive in the venue's OWN order unit.
			// OKX, Gate and Kraken publish contracts, so they have been 0 on
			// the wire since step 1.2 — converting needed the instrument
			// registry. Step 2.7b is where the number is first consumed, and
			// therefore where the conversion belongs.
			bidQtyCoin, askQtyCoin := s.qtyInCoin(orderbookData)

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
				BestBidQtyCoin: bidQtyCoin,
				BestAskQtyCoin: askQtyCoin,
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

func (s *Scanner) processFunding(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case fundingData, ok := <-s.fundingChan:
			if !ok {
				return
			}
			s.updateFunding(fundingData)
		}
	}
}

// updateFunding keeps the latest reading per symbol per source. Funding is a
// slowly-changing value read at display/persistence cadence, so last-write-
// wins is the whole story — history is step 2.6's job, in SQLite, not here.
// Consumers (2.6 persistence, 2.7 dashboard) must judge freshness from the
// stored RecvAt; nothing here evicts a reading whose source went quiet.
func (s *Scanner) updateFunding(data exchanges.FundingData) {
	// A symbol nobody configured is dropped at the door: an all-market
	// funding stream (a cheap choice at step 2.5) must not grow this map —
	// and later SQLite and the dashboard — with the venue's whole universe.
	if _, configured := s.symbolSet[data.Symbol]; !configured {
		return
	}

	// Normalize the receive stamp exactly as updatePrice does, so the stored
	// reading and the liveness record can never disagree about the clock: a
	// zero RecvAt stored verbatim would read as a ~56-year age downstream.
	recvAt := s.receivedAt(data.RecvAt)
	data.RecvAt = recvAt

	s.fundingMutex.Lock()
	if s.funding[data.Symbol] == nil {
		s.funding[data.Symbol] = make(map[string]exchanges.FundingData)
	}
	s.funding[data.Symbol][data.Source] = data
	s.fundingMutex.Unlock()

	// A funding message arriving proves the socket is alive, exactly like a
	// trade does — without this, a source whose only subscribed stream is
	// funding would look disconnected in a quiet market.
	s.markSourceAlive(data.Source, recvAt)
}

// FundingSnapshot returns every funding reading held, newest value per symbol
// per source, sorted by symbol then source.
//
// This is the shape the production readers want (2.6 persistence, 2.7
// dashboard): one locked pass rather than a point lookup per cell. Freshness
// is the CALLER's judgement — nothing here evicts a reading whose source went
// quiet, so a consumer that ignores RecvAt will happily show an hour-old rate
// as current (docs/PLAN.md, step 2.7's warning).
func (s *Scanner) FundingSnapshot() []exchanges.FundingData {
	s.fundingMutex.RLock()
	total := 0
	for _, bySource := range s.funding {
		total += len(bySource)
	}
	out := make([]exchanges.FundingData, 0, total)
	for _, bySource := range s.funding {
		for _, data := range bySource {
			out = append(out, data)
		}
	}
	s.fundingMutex.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Source < out[j].Source
	})
	return out
}

// SetHedges installs the spot↔perp mapping the funding table reads.
//
// It is pushed in rather than pulled: the mapping comes from the instrument
// registry, which refreshes on its own daily cycle, and the entrypoint already
// rebuilds it after every refresh (step 2.4). Calling this from there keeps the
// registry out of the wire layer entirely.
//
// The whole table is replaced, never merged. A market a venue has delisted must
// disappear from the mapping, and merging would keep yesterday's answer alive
// for a pair that no longer exists.
func (s *Scanner) SetHedges(legs []HedgeLeg) {
	hedges := make(map[string]HedgeLeg, len(legs))
	for _, leg := range legs {
		hedges[hedgeKey(leg.Symbol, leg.PerpSource)] = leg
	}
	s.hedgeMutex.Lock()
	s.hedges = hedges
	s.hedgeMutex.Unlock()
}

// SetDepth installs the latest order book measurements and pushes them out.
//
// It broadcasts immediately rather than waiting for a ticker: a sweep happens
// once an hour, so the alternative is a dashboard showing an hour-old book for
// however long the next tick is away. There is nothing to throttle — this is
// the only thing that changes the table.
func (s *Scanner) SetDepth(summaries []depth.Summary) {
	byRow := make(map[string]map[string]depth.Summary, len(summaries))
	for _, summary := range summaries {
		if byRow[summary.Symbol] == nil {
			byRow[summary.Symbol] = make(map[string]depth.Summary)
		}
		byRow[summary.Symbol][summary.Source] = summary
	}

	s.depthMutex.Lock()
	s.depthSummaries = byRow
	s.depthMutex.Unlock()

	if s.hasClients() {
		s.broadcast(s.depthMessage())
	}
}

// DepthSnapshot returns the newest book per symbol per source, sorted by symbol
// then source.
//
// Written for the step-3.5 live signal path, which prices fills on the same
// book the dashboard shows. Freshness is the caller's judgement, exactly as for
// FundingSnapshot and PriceSnapshot: SampledAtMs travels with every summary and
// strategy.RoundTripInput.MaxBookAge is where the limit is applied.
func (s *Scanner) DepthSnapshot() []depth.Summary {
	s.depthMutex.RLock()
	out := make([]depth.Summary, 0, len(s.depthSummaries))
	for _, bySource := range s.depthSummaries {
		for _, summary := range bySource {
			out = append(out, summary)
		}
	}
	s.depthMutex.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Source < out[j].Source
	})
	return out
}

// depthMessage builds the current depth table.
func (s *Scanner) depthMessage() wireDepth {
	s.depthMutex.RLock()
	summaries := make([]depth.Summary, 0, len(s.depthSummaries))
	for _, bySource := range s.depthSummaries {
		for _, summary := range bySource {
			summaries = append(summaries, summary)
		}
	}
	s.depthMutex.RUnlock()
	return newWireDepth(summaries, s.now())
}

// qtyInCoin resolves the top-of-book quantities to base coin.
//
// A connector fills exactly one pair of fields. The ...Coin pair is already in
// coin and passes through untouched — Binance, Bybit and Hyperliquid have
// published real quantities there since step 1.2 and must keep doing so even
// before the instrument registry has refreshed.
//
// The ...Contracts pair needs the registry. When the multiplier is unknown the
// result stays 0, which the contract defines as "not known": publishing an
// unconverted contract count would report Gate — whose BTC contract is 0.0001
// BTC — as ten thousand times deeper than it is, and a wrong number here is
// worse than a missing one because nothing downstream can tell it from a
// measurement. Paradex publishes no size at all and stays 0 either way.
func (s *Scanner) qtyInCoin(book exchanges.OrderbookData) (bidQtyCoin, askQtyCoin float64) {
	if book.BestBidQtyContracts == 0 && book.BestAskQtyContracts == 0 {
		return book.BestBidQtyCoin, book.BestAskQtyCoin
	}
	sizeCoin, ok := s.contractSizeCoin(book.Symbol, book.Source)
	if !ok {
		return 0, 0
	}
	return book.BestBidQtyContracts * sizeCoin, book.BestAskQtyContracts * sizeCoin
}

// SetContractSizes installs the contract→coin multipliers for the price path.
//
// Keyed symbol|source, from the instrument registry, refreshed with it. This is
// what finally fills best_bid_qty_coin for OKX, Gate and Kraken — reserved on
// the wire at step 1.0, left at 0 since 1.2 because converting needed a
// registry that did not exist, and consumed for the first time here.
func (s *Scanner) SetContractSizes(sizes map[string]float64) {
	copied := make(map[string]float64, len(sizes))
	for key, size := range sizes {
		copied[key] = size
	}
	s.contractSizeMutex.Lock()
	s.contractSizes = copied
	s.contractSizeMutex.Unlock()
}

// contractSizeCoin is the multiplier for one market, and whether it is known.
//
// ok=false must never be treated as 1: Gate's BTC contract is 0.0001 BTC, so a
// missing multiplier applied as one reports ten thousand times the real size.
// The caller leaves the quantity at 0 — "not known" — instead.
func (s *Scanner) contractSizeCoin(symbol, source string) (float64, bool) {
	s.contractSizeMutex.RLock()
	defer s.contractSizeMutex.RUnlock()
	size, ok := s.contractSizes[hedgeKey(symbol, source)]
	return size, ok && size > 0
}

// hedgeSnapshot copies the mapping for one message build.
// Hedges returns the installed spot↔perp legs, refusals included, sorted by
// symbol then perp source. The live signal path (step 3.5) reads it to know
// which perps can be acted on and against which spot market; a refused leg is
// returned as a refusal so the evaluator can say "no hedge" in the venue's own
// words instead of silently skipping the pair.
func (s *Scanner) Hedges() []HedgeLeg {
	snapshot := s.hedgeSnapshot()
	out := make([]HedgeLeg, 0, len(snapshot))
	for _, leg := range snapshot {
		out = append(out, leg)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].PerpSource < out[j].PerpSource
	})
	return out
}

func (s *Scanner) hedgeSnapshot() map[string]HedgeLeg {
	s.hedgeMutex.RLock()
	defer s.hedgeMutex.RUnlock()
	out := make(map[string]HedgeLeg, len(s.hedges))
	for key, leg := range s.hedges {
		out[key] = leg
	}
	return out
}

// PriceReading is one venue's latest price for one symbol, with the identity
// the map keys carry attached to it.
type PriceReading struct {
	Symbol string
	Source string
	PricePoint
}

// PriceSnapshot returns the latest price held per symbol per source, sorted by
// symbol then source.
//
// Written for the step-2.6 sampler, which needs the whole table on a fixed
// schedule rather than a value per tick. Freshness is the CALLER's judgement,
// exactly as for FundingSnapshot: nothing here drops a point whose source went
// quiet, which is why RecvAt travels with every reading and is stored beside
// the sample instant.
func (s *Scanner) PriceSnapshot() []PriceReading {
	s.pricesMutex.RLock()
	total := 0
	for _, bySource := range s.prices {
		total += len(bySource)
	}
	out := make([]PriceReading, 0, total)
	for symbol, bySource := range s.prices {
		for source, point := range bySource {
			out = append(out, PriceReading{Symbol: symbol, Source: source, PricePoint: point})
		}
	}
	s.pricesMutex.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Source < out[j].Source
	})
	return out
}

// logFundingSummary prints the funding table on a slow schedule: what was
// collected, in the cross-venue comparable unit, with the age of each reading.
//
// It exists because step 2.5 collects funding that nothing displays yet — the
// dashboard is 2.7 — and a feed nobody can see is a feed nobody knows is
// broken. The age is printed from RecvAt for the same reason the dashboard
// will have to: a subscription that died silently keeps its last reading
// forever, and only the age says so.
func (s *Scanner) logFundingSummary(ctx context.Context) {
	ticker := time.NewTicker(fundingLogEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			readings := s.FundingSnapshot()
			if len(readings) == 0 {
				log.Printf("funding: no readings yet")
				continue
			}
			var b strings.Builder
			// "gross" is not decoration: these figures have no fee, no
			// slippage and no borrow deducted, and a round trip costs about
			// 0.19% (internal/fees). CLAUDE.md rule 2 — a figure with nothing
			// deducted is never presented as profit.
			fmt.Fprintf(&b, "funding: %d readings (GROSS — no fees, slippage or borrow deducted)", len(readings))
			for _, data := range readings {
				// Bps per 8h is the cross-venue comparison figure; the interval
				// is printed beside it because the same bps at 1h and at 8h are
				// eight different annual returns.
				fmt.Fprintf(&b, "\n  %-9s %-20s %+8.4f bps/8h  APR_gross %+7.2f%%  every %4ds  age %3.0fs",
					data.Symbol, data.Source, data.RatePer8hFrac*10000, data.APRFrac*100,
					data.IntervalSec, now.Sub(data.RecvAt).Seconds())
			}
			log.Print(b.String())
		}
	}
}

// latestFunding returns the most recent funding reading for one symbol on one
// source, if any has arrived. Unexported on purpose: the production readers
// (2.6 persistence, 2.7 dashboard) want FundingSnapshot; this point lookup
// exists for tests.
func (s *Scanner) latestFunding(symbol, source string) (exchanges.FundingData, bool) {
	s.fundingMutex.RLock()
	defer s.fundingMutex.RUnlock()
	data, ok := s.funding[symbol][source]
	return data, ok
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

// lateTicks is the process-level scheduling record: a tick that fired later
// than its schedule allows means the process was not running its loops —
// the machine slept or the host stalled — and nothing was journalled,
// sampled or evaluated in the gap. It is NOT a property of any one source,
// which is why it travels beside source_status rather than inside it.
type lateTicks struct {
	Count   int
	LastJob string
	LastAt  time.Time
	LastBy  time.Duration
}

// NoteLateTick records one late tick from one of the entrypoint's jobs.
func (s *Scanner) NoteLateTick(job string, at time.Time, lateBy time.Duration) {
	s.connMutex.Lock()
	defer s.connMutex.Unlock()
	s.late.Count++
	s.late.LastJob, s.late.LastAt, s.late.LastBy = job, at, lateBy
}

// snapshotLateTicks copies the late-tick record for one broadcast.
func (s *Scanner) snapshotLateTicks() lateTicks {
	s.connMutex.RLock()
	defer s.connMutex.RUnlock()
	return s.late
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
// The connector stamps it there, before parsing and before queueing. Stamping at
// this end instead - what step 1.1 did - restarted every message's clock at the
// dequeue, so a scanner backed up behind a 1000-deep channel reported every
// venue as freshly updated while serving prices seconds old. The age it produced
// measured our dispatch lag, not the data.
//
// The consequence of the move is deliberate and worth knowing: under a real
// backlog ages now climb past the staleness thresholds and sources drop out of
// comparison together. That is the honest answer - those prices ARE stale - and
// it is visible rather than hidden.
//
// Zero means the data never crossed a socket (a test injecting straight into a
// channel), and only then does the scanner fall back to its own clock. See
// CLAUDE.md rule 13.
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
	s.queueSpreads(newWireSpreads(symbol, sourcePrices, excluded, snapshotAt.UnixMilli()))
}

// spreadsBroadcastEvery bounds how often one symbol's matrix goes out.
//
// It fires on every price tick from every venue, and step 2.7 measured what
// that costs on the wire: 58 of 60 consecutive frames to a connected dashboard
// were `spreads`. The frontend already keeps only the newest snapshot and
// redraws at 300ms, so all but the last of those were encoded, sent and thrown
// away — and they were crowding out the messages that carry new information.
//
// 200ms matches the price ticker, which is the cadence the dashboard was built
// around. Alerts are NOT queued: an opportunity still broadcasts the instant it
// is found. PLAN §7.3 item 1.
const spreadsBroadcastEvery = 200 * time.Millisecond

// queueSpreads keeps the newest matrix per symbol for the next flush.
//
// Newest by the SNAPSHOT time in the message, not by arrival: checkArbitrage
// and refreshStaleness both publish per symbol from different goroutines, so a
// matrix computed earlier can be queued later, and taking it would replace a
// fresher picture with a staler one.
func (s *Scanner) queueSpreads(message wireSpreads) {
	s.pendingMutex.Lock()
	defer s.pendingMutex.Unlock()
	if existing, ok := s.pendingSpreads[message.Symbol]; ok && existing.ServerTimeMs > message.ServerTimeMs {
		return
	}
	s.pendingSpreads[message.Symbol] = message
}

// flushSpreads sends at most one matrix per symbol per tick.
func (s *Scanner) flushSpreads(ctx context.Context) {
	ticker := time.NewTicker(spreadsBroadcastEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		s.pendingMutex.Lock()
		pending := s.pendingSpreads
		if len(pending) == 0 {
			s.pendingMutex.Unlock()
			continue
		}
		s.pendingSpreads = make(map[string]wireSpreads, len(pending))
		s.pendingMutex.Unlock()

		// Configured order, so a client watching two symbols sees them in a
		// stable sequence rather than in Go map order.
		for _, symbol := range s.symbols {
			if message, ok := pending[symbol]; ok {
				s.broadcast(message)
				delete(pending, symbol)
			}
		}
		// Anything the configured list does not name still goes out; dropping
		// it would silently lose a symbol the scanner is tracking.
		for _, message := range pending {
			s.broadcast(message)
		}
	}
}

// hasClients reports whether any dashboard is connected, so the broadcast path
// can skip building a message nobody will receive.
func (s *Scanner) hasClients() bool {
	s.clientsMutex.RLock()
	defer s.clientsMutex.RUnlock()
	return len(s.wsClients) > 0
}

// fundingBroadcastEvery is how often the funding table is pushed.
//
// Far slower than the 200ms price ticker because funding is a far slower
// number: the fastest venue here republishes about once a second and the
// slowest goes minutes without a message by design. Five seconds keeps the
// countdown to the next settlement honest to the second it is rendered at,
// without sending 28 rows of unchanged rates twenty times a minute.
const fundingBroadcastEvery = 5 * time.Second

// broadcastFunding pushes the funding table on its own slow schedule.
func (s *Scanner) broadcastFunding(ctx context.Context) {
	ticker := time.NewTicker(fundingBroadcastEvery)
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
		s.broadcast(s.fundingMessage())
	}
}

// fundingMessage builds the current funding table.
//
// It is sent even when empty: a table with no rows at all says the funding
// feeds are down, and skipping the message would leave the dashboard showing
// the last good one indefinitely.
func (s *Scanner) fundingMessage() wireFunding {
	return newWireFunding(s.FundingSnapshot(), s.hedgeSnapshot(), s.now())
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
		s.broadcast(newWirePrices(pricesCopy, lastMsgCopy, connCopy, s.startedAt, s.now(), s.snapshotLateTicks()))
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

	// The funding table follows meta immediately, before this client joins the
	// broadcast set. Its ticker is five seconds, and a dashboard that opens on
	// an empty funding panel for that long reads as "this venue publishes no
	// funding" - the same misreading the contract avoids by keeping stale
	// prices on screen instead of dropping them.
	//
	// A failure here is NOT fatal, unlike a failed meta: the client can render
	// everything else without it and the next tick will carry the table.
	if failed, err := s.writeToClients([]*websocket.Conn{conn}, s.fundingMessage()); err != nil || len(failed) > 0 {
		log.Printf("WebSocket funding write failed for %s: %v", r.RemoteAddr, err)
	}

	// Depth follows for the same reason and more urgently: it is refreshed once
	// an HOUR, so a client that connected a minute after a sweep would otherwise
	// see an empty liquidity column for fifty-nine of them.
	if failed, err := s.writeToClients([]*websocket.Conn{conn}, s.depthMessage()); err != nil || len(failed) > 0 {
		log.Printf("WebSocket depth write failed for %s: %v", r.RemoteAddr, err)
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
	go s.processFunding(ctx)
	go s.processConnEvents(ctx)
	go s.broadcastPrices(ctx)
	go s.broadcastFunding(ctx)
	go s.flushSpreads(ctx)
	go s.refreshStaleness(ctx)
	go s.logFundingSummary(ctx)
}

// Feeds is what the venue connectors write into, and what tells them to stop.
//
// It replaced the three separate channel accessors this had until step 1.5.
// The gain showed up on schedule at step 2.2: the funding feed was one new
// field here and no change to any of the ten connector signatures.
func (s *Scanner) Feeds(ctx context.Context) exchanges.Feeds {
	return exchanges.Feeds{
		Ctx:       ctx,
		Price:     s.priceChan,
		Orderbook: s.orderbookChan,
		Trade:     s.tradeChan,
		Funding:   s.fundingChan,
		Conn:      s.connChan,
	}
}
