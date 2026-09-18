package crossperp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/broker/brokertest"
	"futures-arbitrage-scanner/internal/broker/bybit"
	"futures-arbitrage-scanner/internal/coordinator"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/execution"
	"futures-arbitrage-scanner/internal/risk"
)

const (
	venueBinance = "binance_futures"
	venueBybit   = "bybit_linear"
	testMid      = 60_000.0
	testIntentID = "cp-intent-0001"
)

// binanceRules is Binance USDⓈ-M TESTNET BTCUSDT as its own exchangeInfo stated
// it on 2026-09-13 (the figures internal/execution's fixtures use).
func binanceRules() exchanges.Instrument {
	return exchanges.Instrument{Symbol: "BTCUSDT", Source: venueBinance, MarketType: "perp",
		Status: exchanges.StatusTrading, TickSizeQuote: 0.10, StepSizeCoin: 0.0001, MinQtyCoin: 0.0001,
		MaxQtyCoin: 1000, MinNotionalQuote: 50, ContractSizeCoin: 1, BaseAsset: "BTC", QuoteAsset: "USDT"}
}

// bybitRules is Bybit linear BTCUSDT as exchanges/bybit/testdata's recording
// states it: tickSize 0.10, qtyStep 0.001, minOrderQty 0.001, minNotionalValue 5,
// maxMktOrderQty 150.
func bybitRules() exchanges.Instrument {
	return exchanges.Instrument{Symbol: "BTCUSDT", Source: venueBybit, MarketType: "perp",
		Status: exchanges.StatusTrading, TickSizeQuote: 0.10, StepSizeCoin: 0.001, MinQtyCoin: 0.001,
		MaxQtyCoin: 150, MinNotionalQuote: 5, ContractSizeCoin: 1, BaseAsset: "BTC", QuoteAsset: "USDT"}
}

func deepBook(source string, mid float64, sampledAtMs int64) depth.Summary {
	return depth.Summary{Source: source, Symbol: "BTCUSDT", SampledAtMs: sampledAtMs,
		MidPriceQuote: mid, BestBidQuote: mid * 0.99995, BestAskQuote: mid * 1.00005, SpreadPct: 0.01,
		BidDepthWithinTightQuote: 400_000, AskDepthWithinTightQuote: 400_000,
		BidDepthWithinWideQuote: 3_000_000, AskDepthWithinWideQuote: 3_000_000,
		BidLevels: 100, AskLevels: 100, BidSpanPct: 0.8, AskSpanPct: 0.8}
}

// testConfig shrinks every deadline so a test that waits one out costs
// milliseconds. The SHAPE of the machine is what is tested, not its timings.
func testConfig() Config {
	c := DefaultConfig()
	c.LegTimeout = 40 * time.Millisecond
	c.OrderSettleTimeout = 40 * time.Millisecond
	c.PositionSettleTimeout = 40 * time.Millisecond
	c.UnwindTimeout = 3 * time.Second
	c.PollEvery = time.Millisecond
	c.AmbiguousSendQuiet = 30 * time.Millisecond
	return c
}

type allowAll struct{}

func (allowAll) AllowOpen(risk.OpenRequest) error { return nil }

type refuseAll struct{ err error }

func (r refuseAll) AllowOpen(risk.OpenRequest) error { return r.err }

// holdsFor is a lock holder that holds exactly one intent's symbol.
type holdsFor struct{ symbol, intentID string }

func (h holdsFor) Holds(symbol string, engine coordinator.EngineID, intentID string) bool {
	return symbol == h.symbol && intentID == h.intentID && engine == coordinator.EngineCrossPerp
}

func (h holdsFor) MarkOrdersSent(symbol string, engine coordinator.EngineID, intentID string) error {
	if !h.Holds(symbol, engine, intentID) {
		return fmt.Errorf("test: %s is not held by %q", symbol, intentID)
	}
	return nil
}

// scripted wraps a fake venue to produce the failures a real one produces on
// its own schedule: a place that dies before or after the venue took it, an
// order lookup that stays ambiguous, a venue in an IP cool-down.
type scripted struct {
	*brokertest.Fake

	mu sync.Mutex
	// placeBefore returns a non-nil error to fail a place BEFORE the venue sees it.
	placeBefore func(req broker.PlaceOrderRequest) error
	// placeAfter returns a non-nil error to lose the answer AFTER the venue took it.
	placeAfter func(req broker.PlaceOrderRequest) error
	// getOrder returns a non-nil error to answer a lookup with it instead.
	getOrder func(q broker.OrderQuery) error
	// cancel returns a non-nil error to answer a cancel with it instead.
	cancel func(q broker.OrderQuery) error
	// cancelAck, when set, answers a cancel with a bare acknowledgement and
	// hands the cancel to the test, which completes it when it chooses — Bybit's
	// "the acknowledgement ... indicates that the request was successfully
	// accepted", with the order still live on the next read.
	cancelAck func(q broker.OrderQuery)
	// position, when set, replaces the venue's position answer.
	position func(p broker.Position) (broker.Position, error)
	places   []broker.PlaceOrderRequest
}

func (s *scripted) PlaceOrder(ctx context.Context, req broker.PlaceOrderRequest) (broker.Order, error) {
	s.mu.Lock()
	s.places = append(s.places, req)
	before, after := s.placeBefore, s.placeAfter
	s.mu.Unlock()
	if before != nil {
		if err := before(req); err != nil {
			return broker.Order{}, err
		}
	}
	if req.ReduceOnly {
		// What a venue does with reduce-only (brokertest's fake does not):
		// refuse one that cannot reduce — Binance -2022 REDUCE_ONLY_REJECT — and
		// never let one grow or flip the position.
		pos, err := s.Fake.GetPosition(ctx, req.Market, req.Symbol)
		if err != nil {
			return broker.Order{}, err
		}
		if !(req.Side == broker.SideSell && pos.QtyCoin > 0 || req.Side == broker.SideBuy && pos.QtyCoin < 0) {
			return broker.Order{}, fmt.Errorf("%w: test: -2022 ReduceOnly Order is rejected (position %v)", broker.ErrInvalidOrder, pos.QtyCoin)
		}
		if held := math.Abs(pos.QtyCoin); req.QtyCoin > held {
			req.QtyCoin = held
		}
	}
	o, err := s.Fake.PlaceOrder(ctx, req)
	if err == nil && after != nil {
		if lost := after(req); lost != nil {
			return broker.Order{}, lost
		}
	}
	return o, err
}

func (s *scripted) GetOrder(ctx context.Context, q broker.OrderQuery) (broker.Order, error) {
	s.mu.Lock()
	f := s.getOrder
	s.mu.Unlock()
	if f != nil {
		if err := f(q); err != nil {
			return broker.Order{}, err
		}
	}
	return s.Fake.GetOrder(ctx, q)
}

func (s *scripted) CancelOrder(ctx context.Context, q broker.OrderQuery) (broker.Order, error) {
	s.mu.Lock()
	f, ack := s.cancel, s.cancelAck
	s.mu.Unlock()
	if ack != nil {
		ack(q)
		return broker.Order{ClientOrderID: q.ClientOrderID, Status: broker.OrderStatusNew}, nil
	}
	if f != nil {
		if err := f(q); err != nil {
			return broker.Order{}, err
		}
	}
	return s.Fake.CancelOrder(ctx, q)
}

func (s *scripted) GetPosition(ctx context.Context, market broker.Market, symbol string) (broker.Position, error) {
	p, err := s.Fake.GetPosition(ctx, market, symbol)
	s.mu.Lock()
	f := s.position
	s.mu.Unlock()
	if f != nil && err == nil {
		return f(p)
	}
	return p, err
}

func (s *scripted) sent() []broker.PlaceOrderRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]broker.PlaceOrderRequest(nil), s.places...)
}

// sentWith lists the requests of one purpose on one leg, in the order sent.
// Shrinking orders carry a per-call nonce, so they are recognised by the id's
// visible prefix — "cp1" + leg letter + purpose letter; every test harness holds
// one intent.
func (s *scripted) sentWith(intentID string, purpose Purpose, leg execution.LegName) []broker.PlaceOrderRequest {
	var out []broker.PlaceOrderRequest
	for _, r := range s.sent() {
		if isOrderOf(r.ClientOrderID, purpose, leg) {
			out = append(out, r)
		}
	}
	return out
}

func isOrderOf(clientOrderID string, purpose Purpose, leg execution.LegName) bool {
	return strings.HasPrefix(clientOrderID, clientOrderIDPrefix+string(leg)[:1]+string(purpose)[:1])
}

func (s *scripted) set(f func(s *scripted)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// awaitOpeningSent waits, bounded, until this venue has been sent an opening
// order.
func (s *scripted) awaitOpeningSent() {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range s.sent() {
			if isOrderOf(r.ClientOrderID, PurposeOpen, LegLong) || isOrderOf(r.ClientOrderID, PurposeOpen, LegShort) {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
}

// answerAfterOtherOpens makes this venue answer err to every order, and to its
// OPENING order only once the other venue has been sent its own. A venue's answer
// travels a network round trip; an in-memory venue answers before the other leg's
// goroutine has sent anything, and workLeg then — rightly — never sends that leg
// (review 4.5k, m-e). That is a different case from "one leg filled, the other was
// refused or cut off", which is what the tests using this are about.
func (s *scripted) answerAfterOtherOpens(other *scripted, err error) {
	s.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			if !req.ReduceOnly {
				other.awaitOpeningSent()
			}
			return err
		}
	})
}

// errTransport is what a dropped connection looks like: no answer at all.
var errTransport = errors.New("test: read tcp: connection reset by peer")

// errNotVisible is an ambiguous lookup: bybit.ErrOrderNotVisible, the answer the
// real Bybit client gives for an id neither order list shows.
var errNotVisible = fmt.Errorf("test: %w", bybit.ErrOrderNotVisible)

type harness struct {
	t       *testing.T
	binance *scripted
	bybit   *scripted
	rec     *execution.MemoryRecorder
	exec    *Executor
	cfg     Config
}

// newHarness is Binance long, Bybit short, well-behaved venues that fill every
// limit at once, and an executor holding the test intent's lock.
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, cfg: testConfig(), rec: execution.NewMemoryRecorder(),
		binance: &scripted{Fake: brokertest.New()}, bybit: &scripted{Fake: brokertest.New()}}
	h.binance.SetStepSizeCoin(binanceRules().StepSizeCoin)
	h.bybit.SetStepSizeCoin(bybitRules().StepSizeCoin)
	h.binance.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1})
	h.bybit.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1})
	h.build(holdsFor{"BTCUSDT", testIntentID}, allowAll{})
	return h
}

func (h *harness) build(locks LockHolder, gate EntryGate) {
	h.t.Helper()
	x, err := NewExecutor(h.cfg, locks, gate, h.rec)
	if err != nil {
		h.t.Fatal(err)
	}
	h.exec = x
}

func (h *harness) intent() Intent {
	nowMs := time.Now().UnixMilli()
	return Intent{
		ID: testIntentID, Symbol: "BTCUSDT", NotionalQuote: 20_000, SignalEntryCostPct: 1.0,
		Long:  LegSpec{Venue: Venue{Name: venueBinance, Broker: h.binance}, Rules: binanceRules(), Book: deepBook(venueBinance, testMid, nowMs)},
		Short: LegSpec{Venue: Venue{Name: venueBybit, Broker: h.bybit}, Rules: bybitRules(), Book: deepBook(venueBybit, testMid, nowMs)},
	}
}

// positions reads both fake venues — the evidence every assertion is made on,
// never the Result a machine that lost a leg would report with confidence.
func (h *harness) positions() (long, short float64) {
	h.t.Helper()
	pl, err := h.binance.Fake.GetPosition(context.Background(), broker.MarketFuturesUSDM, "BTCUSDT")
	if err != nil {
		h.t.Fatal(err)
	}
	ps, err := h.bybit.Fake.GetPosition(context.Background(), broker.MarketFuturesUSDM, "BTCUSDT")
	if err != nil {
		h.t.Fatal(err)
	}
	return pl.QtyCoin, ps.QtyCoin
}
