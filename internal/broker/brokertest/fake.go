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
	"math"
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
	//
	// It applies to LIMIT orders only. A MARKET order always fills completely
	// here, because a market order that rests is not modelling any venue that
	// exists, and a test needing a partial market fill would be testing this
	// fake rather than the code under test. Use RejectWith for a market order
	// that fails, or Fill for one that is driven by hand.
	FillFractionOnPlace float64

	// TimeoutTimes bounds how many places the timeout knobs affect. 0 means
	// EVERY place — a permanently broken link — while N means the first N and
	// no more. N is how a test reaches the case that matters most: the send
	// timed out, we asked the venue, it had never arrived, we resent, and the
	// resend worked.
	TimeoutTimes int

	// RejectWith, when set, is returned by PlaceOrder instead of accepting.
	RejectWith error

	// CancelRacesAFill makes CancelOrder find the order already fully filled,
	// which is the race a two-leg unwind must survive.
	CancelRacesAFill bool

	// MarketFillFraction makes a MARKET order fill only part of its quantity.
	// 0 means the whole of it, which is the default and what a liquid venue
	// does.
	//
	// It exists for the CLOSE (step 4.5), where a partial market fill is a
	// real venue state rather than a modelling curiosity: a thin book runs out,
	// and a reduceOnly order is clamped to the position that is actually there.
	// A close that half-fills is exactly the case where a pair can be left
	// unbalanced, so it has to be reachable on demand.
	MarketFillFraction float64
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
	timeouts  int            // how many places have already been failed by the knobs
	orders    []broker.Order // in placement order, so OpenOrders is stable
	positions map[string]broker.Position
	balances  map[broker.Market][]broker.Balance

	// nowMs is the clock, injectable so a test is not at the mercy of one.
	nowMs int64

	// funding is what FundingIncome answers, and trade commissions are
	// synthesized from the orders — see SetFundingIncome and SetCommission.
	funding        []broker.FundingIncome
	commissionRate float64
	commissionAss  string
	markPrice      broker.MarkPrice

	// baseAsset, when set, makes the fake move a SPOT balance as fills happen,
	// the way a venue does. Without it the fake reports whatever SetBalance
	// last wrote, which is a venue that never settles anything.
	baseAsset string

	// stepSizeCoin is the venue's quantity grid. Fills are floored onto it,
	// because a real venue never reports a fill BETWEEN two points of its own
	// grid — quantities there are multiples of stepSize by construction.
	//
	// It defaults to 0, meaning "do not round", which keeps the fake's
	// arithmetic exact for tests that do not care. Tests that drive a partial
	// fill DO care: an off-grid fill cannot be closed by an order rounded onto
	// the grid, so a fake without this produces a stuck remainder that no
	// venue would ever create, and the code under test gets blamed for it.
	stepSizeCoin float64
}

// New returns an empty fake with a well-behaved venue.
func New() *Fake {
	return &Fake{
		positions: map[string]broker.Position{},
		balances:  map[broker.Market][]broker.Balance{},
		nowMs:     1_789_000_000_000,
	}
}

var (
	_ broker.Broker          = (*Fake)(nil)
	_ broker.TradeReader     = (*Fake)(nil)
	_ broker.FundingReader   = (*Fake)(nil)
	_ broker.MarkPriceReader = (*Fake)(nil)
)

// SetBehaviour replaces the knobs.
func (f *Fake) SetBehaviour(b Behaviour) {
	if b.FillFractionOnPlace < 0 || b.FillFractionOnPlace > 1 {
		panic(fmt.Sprintf("brokertest: FillFractionOnPlace must be in [0,1], got %v", b.FillFractionOnPlace))
	}
	if b.MarketFillFraction < 0 || b.MarketFillFraction > 1 {
		panic(fmt.Sprintf("brokertest: MarketFillFraction must be in [0,1], got %v", b.MarketFillFraction))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.behaviour = b
}

// SetStepSizeCoin makes fills land on the venue's quantity grid.
func (f *Fake) SetStepSizeCoin(step float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stepSizeCoin = step
}

// onGrid floors a quantity onto the venue's grid. Caller holds the lock.
func (f *Fake) onGrid(qtyCoin float64) float64 {
	if f.stepSizeCoin <= 0 {
		return qtyCoin
	}
	steps := math.Floor(qtyCoin/f.stepSizeCoin + 1e-9)
	return math.Round(steps*f.stepSizeCoin*1e12) / 1e12
}

// SetBaseAsset makes the fake keep a SPOT balance of this asset and move it as
// orders fill, and keep a FUTURES position and move it the same way.
//
// It is what makes rule 7 testable: the code under test asks the venue what it
// holds, and the venue's answer changes because of the orders it filled rather
// than because a test remembered to write it down.
func (f *Fake) SetBaseAsset(asset string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.baseAsset = asset
}

// settle moves the venue's own position and balance by one fill. Caller holds
// the lock.
func (f *Fake) settle(o broker.Order, qtyCoin float64) {
	if qtyCoin <= 0 {
		return
	}
	signed := qtyCoin
	if o.Side == broker.SideSell {
		signed = -qtyCoin
	}
	switch o.Market {
	case broker.MarketFuturesUSDM:
		key := string(o.Market) + "|" + o.Symbol
		p, ok := f.positions[key]
		if !ok {
			p = broker.Position{Market: o.Market, Symbol: o.Symbol}
		}
		p.QtyCoin = roundGrid(p.QtyCoin + signed)
		p.UpdatedAtMs = f.nowMs
		f.positions[key] = p
	case broker.MarketSpot:
		if f.baseAsset == "" {
			return
		}
		list := f.balances[broker.MarketSpot]
		for i := range list {
			if list[i].Asset == f.baseAsset {
				list[i].FreeQtyCoin = roundGrid(list[i].FreeQtyCoin + signed)
				f.balances[broker.MarketSpot] = list
				return
			}
		}
		f.balances[broker.MarketSpot] = append(list, broker.Balance{
			Market: broker.MarketSpot, Asset: f.baseAsset, FreeQtyCoin: roundGrid(signed)})
	}
}

// roundGrid removes the float dust that repeated addition leaves, so a balance
// that returned to where it started reads as equal rather than as 1e-17.
func roundGrid(v float64) float64 { return math.Round(v*1e12) / 1e12 }

// SetFundingIncome replaces what FundingIncome answers for the futures market.
func (f *Fake) SetFundingIncome(rows ...broker.FundingIncome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.funding = append([]broker.FundingIncome(nil), rows...)
}

// SetCommission makes every fill report a commission of rateFrac x the fill's
// notional, denominated in asset.
//
// The asset is NOT converted anywhere, on purpose: a real venue charges spot
// commission in the asset received (BTC on a buy) or in BNB, and a caller that
// silently priced those in USDT would be inventing a rate. An empty asset means
// the venue stated no commission, which is not the same as zero.
func (f *Fake) SetCommission(rateFrac float64, asset string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commissionRate, f.commissionAss = rateFrac, asset
}

// SetMarkPrice replaces what MarkPrice answers.
func (f *Fake) SetMarkPrice(m broker.MarkPrice) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markPrice = m
}

// OrderTrades implements broker.TradeReader: one synthesized fill per order
// that filled anything.
func (f *Fake) OrderTrades(ctx context.Context, q broker.OrderQuery) ([]broker.Trade, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := q.Validate(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.indexOf(q)
	if i < 0 {
		return nil, fmt.Errorf("%w: %s", broker.ErrOrderNotFound, describe(q))
	}
	o := f.orders[i]
	if o.FilledQtyCoin <= 0 {
		return nil, nil
	}
	t := broker.Trade{
		Market: o.Market, Symbol: o.Symbol, TradeID: o.VenueOrderID + "-1",
		VenueOrderID: o.VenueOrderID, Side: o.Side,
		QtyCoin: o.FilledQtyCoin, PriceQuote: o.AvgFillPriceQuote,
		CommissionAsset: f.commissionAss, TimeMs: o.UpdatedAtMs,
	}
	if f.commissionAss != "" {
		t.CommissionQtyInAsset = f.commissionRate * o.FilledQtyCoin * o.AvgFillPriceQuote
	}
	return []broker.Trade{t}, nil
}

// FundingIncome implements broker.FundingReader.
func (f *Fake) FundingIncome(ctx context.Context, market broker.Market, symbol string, startMs, endMs int64) ([]broker.FundingIncome, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if market != broker.MarketFuturesUSDM {
		return nil, fmt.Errorf("%w: %q settles no funding", broker.ErrNotSupported, market)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []broker.FundingIncome
	for _, r := range f.funding {
		if symbol != "" && r.Symbol != symbol {
			continue
		}
		if startMs > 0 && r.SettledAtMs < startMs {
			continue
		}
		if endMs > 0 && r.SettledAtMs > endMs {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// MarkPrice implements broker.MarkPriceReader.
func (f *Fake) MarkPrice(ctx context.Context, market broker.Market, symbol string) (broker.MarkPrice, error) {
	if err := ctx.Err(); err != nil {
		return broker.MarkPrice{}, err
	}
	if market != broker.MarketFuturesUSDM {
		return broker.MarkPrice{}, fmt.Errorf("%w: %q publishes no mark price", broker.ErrNotSupported, market)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.markPrice
	m.Market, m.Symbol = market, symbol
	return m, nil
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
		f.settle(*o, qtyCoin)
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
	timeoutArmed := f.behaviour.TimeoutTimes == 0 || f.timeouts < f.behaviour.TimeoutTimes
	if f.behaviour.PlaceTimesOutBeforeAccepting && timeoutArmed {
		// Nothing is recorded: the venue never saw it.
		f.timeouts++
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
	switch {
	case req.Type == broker.OrderTypeMarket:
		// A market order takes liquidity and fills, whole unless a test asks
		// for less. The fake has no book, so it prices the fill at 1 and the
		// arithmetic a test checks is the QUANTITY.
		filled := req.QtyCoin
		if frac := f.behaviour.MarketFillFraction; frac > 0 && frac < 1 {
			filled = f.onGrid(req.QtyCoin * frac)
		}
		if filled > 0 {
			applyFill(&o, filled, 1)
			f.settle(o, filled)
		}
	case f.behaviour.FillFractionOnPlace > 0:
		if filled := f.onGrid(req.QtyCoin * f.behaviour.FillFractionOnPlace); filled > 0 {
			applyFill(&o, filled, req.PriceQuote)
			f.settle(o, filled)
		}
	}
	f.orders = append(f.orders, o)

	if f.behaviour.PlaceTimesOutAfterAccepting && timeoutArmed {
		// Recorded, then the answer is lost. This is the ambiguity.
		f.timeouts++
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
		if rest := o.RemainingQtyCoin(); rest > 0 {
			applyFill(o, rest, o.PriceQuote)
			f.settle(*o, rest)
		}
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
