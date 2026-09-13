// Package brokertest is an in-memory broker.Broker and the contract every real
// implementation must satisfy.
//
// It exists so that step 4.4 — the two-leg open and close, where the expensive
// failures live — can be unit-tested without a network, a credential or a
// venue. The failures worth testing there are not "the order was rejected";
// they are the ambiguous ones: a place that timed out after the venue accepted
// it, a leg that filled halfway, a cancel that raced a fill. None of those can
// be produced on demand against a real venue, which is why they go untested
// against one.
//
// The fake holds no credentials and opens no socket. It deliberately reuses
// broker.PlaceOrderRequest.Validate so that a request the real broker refuses
// is refused here too: a test that passes against a fake with laxer rules than
// production has tested nothing about production.
package brokertest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"futures-arbitrage-scanner/internal/broker"
)

// Behaviour is the set of knobs. The zero value is a well-behaved venue: every
// order is accepted, rests, and fills only when a test says so.
type Behaviour struct {
	// PlaceTimesOutAfterAccepting is THE case this package exists for. The
	// order is recorded exactly as if the venue had taken it, and PlaceOrder
	// then returns ErrTimeout — so the caller has no order, no venue id, and
	// no way to know the difference except to ask by ClientOrderID.
	PlaceTimesOutAfterAccepting bool

	// PlaceTimesOutBeforeAccepting is the other half of the same ambiguity:
	// the request never reached the venue. The caller sees the SAME error, and
	// only GetOrder can tell the two apart.
	PlaceTimesOutBeforeAccepting bool

	// FillFractionOnPlace fills this fraction of the quantity the instant the
	// order is placed: 0 rests, 1 fills, 0.4 leaves a partial fill. Out-of-range
	// values are a test bug and panic rather than silently clamping.
	FillFractionOnPlace float64

	// RejectWith, when set, is returned by PlaceOrder instead of accepting.
	RejectWith error

	// CancelRacesAFill makes CancelOrder find the order already fully filled,
	// which is the race a two-leg unwind must survive.
	CancelRacesAFill bool
}

// ErrTimeout is what an ambiguous failure looks like to a caller. It is
// deliberately NOT broker.ErrOrderNotFound: a timeout says nothing about
// whether the venue has the order, and conflating the two is the bug this
// whole package is built to catch.
var ErrTimeout = errors.New("brokertest: the request timed out; whether the venue received it is unknown")

// Fake is an in-memory broker.Broker.
type Fake struct {
	mu        sync.Mutex
	behaviour Behaviour
	seq       int64
	orders    []broker.Order // in placement order, so OpenOrders is stable
	positions map[string]broker.Position
	balances  map[broker.Market][]broker.Balance

	// nowMs is the clock, injectable so a test is not at the mercy of one.
	nowMs int64
}

// New returns an empty fake with a well-behaved venue.
func New() *Fake {
	return &Fake{
		positions: map[string]broker.Position{},
		balances:  map[broker.Market][]broker.Balance{},
		nowMs:     1_789_000_000_000,
	}
}

var _ broker.Broker = (*Fake)(nil)

// SetBehaviour replaces the knobs.
func (f *Fake) SetBehaviour(b Behaviour) {
	if b.FillFractionOnPlace < 0 || b.FillFractionOnPlace > 1 {
		panic(fmt.Sprintf("brokertest: FillFractionOnPlace must be in [0,1], got %v", b.FillFractionOnPlace))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.behaviour = b
}

// SetBalance replaces one market's balances.
func (f *Fake) SetBalance(m broker.Market, balances ...broker.Balance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balances[m] = balances
}

// SetPosition replaces one symbol's position.
func (f *Fake) SetPosition(p broker.Position) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.positions[string(p.Market)+"|"+p.Symbol] = p
}

// Fill drives a resting order, the way a venue would between two polls. It is
// how a test reaches PARTIALLY_FILLED without guessing at timing.
func (f *Fake) Fill(clientOrderID string, qtyCoin, priceQuote float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.orders {
		o := &f.orders[i]
		if o.ClientOrderID != clientOrderID {
			continue
		}
		if qtyCoin > o.RemainingQtyCoin() {
			return fmt.Errorf("brokertest: filling %v would exceed the %v remaining on %s", qtyCoin, o.RemainingQtyCoin(), clientOrderID)
		}
		applyFill(o, qtyCoin, priceQuote)
		o.UpdatedAtMs = f.nowMs
		return nil
	}
	return fmt.Errorf("%w: %s", broker.ErrOrderNotFound, clientOrderID)
}

// applyFill adds a fill, keeping AvgFillPriceQuote a true quantity-weighted
// average. It is GROSS — no commission is deducted anywhere in this package.
func applyFill(o *broker.Order, qtyCoin, priceQuote float64) {
	total := o.FilledQtyCoin + qtyCoin
	if total > 0 {
		o.AvgFillPriceQuote = (o.AvgFillPriceQuote*o.FilledQtyCoin + priceQuote*qtyCoin) / total
	}
	o.FilledQtyCoin = total
	switch {
	case o.FilledQtyCoin >= o.QtyCoin:
		o.Status = broker.OrderStatusFilled
	case o.FilledQtyCoin > 0:
		o.Status = broker.OrderStatusPartiallyFilled
	}
}

// Orders returns a copy of everything the fake holds, for assertions.
func (f *Fake) Orders() []broker.Order {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]broker.Order(nil), f.orders...)
}

// PlaceOrder implements broker.Broker.
func (f *Fake) PlaceOrder(ctx context.Context, req broker.PlaceOrderRequest) (broker.Order, error) {
	if err := ctx.Err(); err != nil {
		return broker.Order{}, err
	}
	// The same validation the real broker runs, on purpose.
	if err := req.Validate(); err != nil {
		return broker.Order{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.behaviour.RejectWith != nil {
		return broker.Order{}, f.behaviour.RejectWith
	}
	if f.behaviour.PlaceTimesOutBeforeAccepting {
		// Nothing is recorded: the venue never saw it.
		return broker.Order{}, ErrTimeout
	}
	for _, o := range f.orders {
		if o.ClientOrderID == req.ClientOrderID {
			return broker.Order{}, fmt.Errorf("%w: duplicate ClientOrderID %q", broker.ErrInvalidOrder, req.ClientOrderID)
		}
	}

	f.seq++
	o := broker.Order{
		Market: req.Market, Symbol: req.Symbol, Side: req.Side, Type: req.Type,
		Status:         broker.OrderStatusNew,
		ClientOrderID:  req.ClientOrderID,
		VenueOrderID:   strconv.FormatInt(f.seq, 10),
		QtyCoin:        req.QtyCoin,
		PriceQuote:     req.PriceQuote,
		ReduceOnly:     req.ReduceOnly,
		TransactTimeMs: f.nowMs,
		UpdatedAtMs:    f.nowMs,
	}
	if frac := f.behaviour.FillFractionOnPlace; frac > 0 {
		price := req.PriceQuote
		if req.Type == broker.OrderTypeMarket {
			// A market order has no price of its own; the fake prices it at
			// the caller's own reference so the arithmetic stays checkable.
			price = 1
		}
		applyFill(&o, req.QtyCoin*frac, price)
	}
	f.orders = append(f.orders, o)

	if f.behaviour.PlaceTimesOutAfterAccepting {
		// Recorded, then the answer is lost. This is the ambiguity.
		return broker.Order{}, ErrTimeout
	}
	return o, nil
}

// CancelOrder implements broker.Broker.
func (f *Fake) CancelOrder(ctx context.Context, q broker.OrderQuery) (broker.Order, error) {
	if err := ctx.Err(); err != nil {
		return broker.Order{}, err
	}
	if err := q.Validate(); err != nil {
		return broker.Order{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	i := f.indexOf(q)
	if i < 0 {
		return broker.Order{}, fmt.Errorf("%w: %s", broker.ErrOrderNotFound, describe(q))
	}
	o := &f.orders[i]
	if f.behaviour.CancelRacesAFill {
		applyFill(o, o.RemainingQtyCoin(), o.PriceQuote)
	}
	if o.Status.Done() {
		// Cancelling something already finished is not a cancel. The caller
		// unwinding two legs must see the difference.
		return *o, fmt.Errorf("%w: %s is already %s", broker.ErrOrderNotFound, describe(q), o.Status)
	}
	o.Status = broker.OrderStatusCanceled
	o.UpdatedAtMs = f.nowMs
	return *o, nil
}

// GetOrder implements broker.Broker.
func (f *Fake) GetOrder(ctx context.Context, q broker.OrderQuery) (broker.Order, error) {
	if err := ctx.Err(); err != nil {
		return broker.Order{}, err
	}
	if err := q.Validate(); err != nil {
		return broker.Order{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := f.indexOf(q); i >= 0 {
		return f.orders[i], nil
	}
	return broker.Order{}, fmt.Errorf("%w: %s", broker.ErrOrderNotFound, describe(q))
}

// OpenOrders implements broker.Broker.
func (f *Fake) OpenOrders(ctx context.Context, market broker.Market, symbol string) ([]broker.Order, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []broker.Order
	for _, o := range f.orders {
		if o.Market != market || o.Status.Done() {
			continue
		}
		if symbol != "" && o.Symbol != symbol {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}

// GetPosition implements broker.Broker.
func (f *Fake) GetPosition(ctx context.Context, market broker.Market, symbol string) (broker.Position, error) {
	if err := ctx.Err(); err != nil {
		return broker.Position{}, err
	}
	if market != broker.MarketFuturesUSDM {
		return broker.Position{}, fmt.Errorf("%w: %q holds no position", broker.ErrNotSupported, market)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.positions[string(market)+"|"+symbol]
	if !ok {
		// A venue that holds nothing reports flat, and that IS the answer.
		return broker.Position{Market: market, Symbol: symbol, UpdatedAtMs: f.nowMs}, nil
	}
	return p, nil
}

// GetBalance implements broker.Broker.
func (f *Fake) GetBalance(ctx context.Context, market broker.Market) ([]broker.Balance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]broker.Balance(nil), f.balances[market]...), nil
}

// indexOf finds an order by either id. Caller holds the lock.
func (f *Fake) indexOf(q broker.OrderQuery) int {
	for i, o := range f.orders {
		if o.Market != q.Market || o.Symbol != q.Symbol {
			continue
		}
		if q.ClientOrderID != "" && o.ClientOrderID == q.ClientOrderID {
			return i
		}
		if q.VenueOrderID != "" && o.VenueOrderID == q.VenueOrderID {
			return i
		}
	}
	return -1
}

func describe(q broker.OrderQuery) string {
	if q.ClientOrderID != "" {
		return q.Symbol + " clientOrderId=" + q.ClientOrderID
	}
	return q.Symbol + " orderId=" + q.VenueOrderID
}
