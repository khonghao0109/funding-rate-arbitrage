package broker

import (
	"context"
	"errors"
	"fmt"
)

// The order interface, step 4.2.
//
// Step 4.1 was the transport — a clock, a signature, a weight budget, and two
// read-only endpoints. This file is the shape of an order, and nothing in it
// sends one: the Binance implementation is in internal/broker/binance and the
// in-memory one is in internal/broker/brokertest.
//
// # Every figure here is GROSS (CLAUDE.md rule 2)
//
// FilledQtyCoin and AvgFillPriceQuote are what the VENUE says was filled and at
// what average price. No commission has been taken off, no funding, no
// slippage against a reference. The word "net" appears nowhere in this package
// and must not: only internal/strategy may say it, and only about a figure that
// has had all four fills and the measured book deducted. A caller that wants a
// net figure computes it there, from these numbers plus the fee schedule.
//
// # Units live in the identifiers (CLAUDE.md rule 4)
//
// QtyCoin, PriceQuote, NotionalQuote, FilledQtyCoin, AvgFillPriceQuote. A bare
// `qty` or `price` crossing a function boundary in this package is a defect:
// three of the nine venues denominate orders in CONTRACTS, and the one thing
// that must never be ambiguous at the moment of placing an order is whether the
// number means coins or contracts. Everything in this interface is COIN and
// QUOTE; converting to a venue's contracts is the implementation's job, on the
// far side of the interface, using the registry's ContractSizeCoin.

// Market names which of a venue's markets an order belongs to. They are
// separate systems with separate endpoints, separate rules and — measured
// 2026-09-13 on Binance testnet — separate credentials.
type Market string

const (
	MarketFuturesUSDM Market = "futures_usdm"
	MarketSpot        Market = "spot"
)

// Side of an order.
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// OrderType is deliberately just the two step 4.2 needs.
//
// LIMIT_GTC and MARKET are enough to open and close a delta-neutral position
// and to run the place/cancel acceptance. Every other time-in-force and every
// conditional type is absent on purpose: an enum value that no code path has
// ever sent is a value nobody has checked the venue's spelling of, and rule 5
// says not to guess one.
type OrderType string

const (
	// OrderTypeLimitGTC rests on the book until it fills or is cancelled.
	OrderTypeLimitGTC OrderType = "LIMIT_GTC"
	// OrderTypeMarket takes liquidity immediately.
	OrderTypeMarket OrderType = "MARKET"
)

// OrderStatus is the venue's account of an order, normalized.
//
// OrderStatusUnknown is a real answer and not a zero value to be ignored: it
// means the venue replied with a status this package has not mapped, and a
// caller must treat it as "I do not know what this order is doing" — never as
// "not filled".
type OrderStatus string

const (
	OrderStatusNew             OrderStatus = "NEW"
	OrderStatusPartiallyFilled OrderStatus = "PARTIALLY_FILLED"
	OrderStatusFilled          OrderStatus = "FILLED"
	OrderStatusCanceled        OrderStatus = "CANCELED"
	OrderStatusRejected        OrderStatus = "REJECTED"
	OrderStatusExpired         OrderStatus = "EXPIRED"
	OrderStatusUnknown         OrderStatus = "UNKNOWN"
)

// Done reports whether the order can no longer change at the venue.
func (s OrderStatus) Done() bool {
	switch s {
	case OrderStatusFilled, OrderStatusCanceled, OrderStatusRejected, OrderStatusExpired:
		return true
	}
	return false
}

// PlaceOrderRequest is one order to place.
//
// ClientOrderID is REQUIRED and is the caller's, not the venue's. See the
// Broker interface comment: it is the only thing that makes an ambiguous
// timeout answerable.
type PlaceOrderRequest struct {
	Market        Market
	Symbol        string // normalized, e.g. BTCUSDT
	Side          Side
	Type          OrderType
	ClientOrderID string

	// QtyCoin is in BASE COIN, always, whatever the venue denominates in.
	QtyCoin float64

	// PriceQuote is required for LIMIT_GTC and must be zero for MARKET —
	// sending a price with a market order is how a venue-specific rejection
	// that nobody tested for arrives at three in the morning.
	PriceQuote float64

	// ReduceOnly asks the venue to refuse the order if it would OPEN or grow a
	// position rather than shrink one. It is meaningful on futures only.
	// Step 4.2 never sets it; it exists because closing a leg at 4.4/4.5 must
	// be able to, and adding the field later would change this struct after
	// two implementations had been written against it.
	ReduceOnly bool
}

// Validate checks what can be checked without a venue.
//
// It is here rather than in each implementation so that the fake and the real
// broker refuse the same requests: a test that passes against a fake which
// accepts a zero quantity has tested nothing about the code that matters.
func (r PlaceOrderRequest) Validate() error {
	switch {
	case r.Market != MarketFuturesUSDM && r.Market != MarketSpot:
		return fmt.Errorf("%w: market %q is neither %q nor %q", ErrInvalidOrder, r.Market, MarketFuturesUSDM, MarketSpot)
	case r.Symbol == "":
		return fmt.Errorf("%w: no symbol", ErrInvalidOrder)
	case r.Side != SideBuy && r.Side != SideSell:
		return fmt.Errorf("%w: side %q is neither %q nor %q", ErrInvalidOrder, r.Side, SideBuy, SideSell)
	case r.ClientOrderID == "":
		// The whole recovery story depends on this, so it is refused here
		// rather than defaulted to something generated: an id the caller did
		// not choose is an id the caller cannot look up after a timeout.
		return fmt.Errorf("%w: no ClientOrderID — without one, a request that times out cannot be resolved into 'did it arrive' before resending", ErrInvalidOrder)
	case r.QtyCoin <= 0:
		return fmt.Errorf("%w: QtyCoin is %v, which is not a quantity", ErrInvalidOrder, r.QtyCoin)
	}
	switch r.Type {
	case OrderTypeLimitGTC:
		if r.PriceQuote <= 0 {
			return fmt.Errorf("%w: a %s order needs a PriceQuote, got %v", ErrInvalidOrder, r.Type, r.PriceQuote)
		}
	case OrderTypeMarket:
		if r.PriceQuote != 0 {
			return fmt.Errorf("%w: a %s order must carry no PriceQuote, got %v", ErrInvalidOrder, r.Type, r.PriceQuote)
		}
	default:
		return fmt.Errorf("%w: order type %q is not one this package sends", ErrInvalidOrder, r.Type)
	}
	if r.ReduceOnly && r.Market != MarketFuturesUSDM {
		return fmt.Errorf("%w: ReduceOnly is a futures concept and %q is not futures", ErrInvalidOrder, r.Market)
	}
	return nil
}

// Order is the venue's account of one order. Every quantity is GROSS.
type Order struct {
	Market        Market
	Symbol        string
	Side          Side
	Type          OrderType
	Status        OrderStatus
	ClientOrderID string

	// VenueOrderID is the venue's own identifier. It is a STRING even where a
	// venue sends a number: Binance's orderId is a 64-bit integer, and the
	// moment it travels through anything that speaks JSON numbers it is a
	// float64 and the low digits are gone.
	VenueOrderID string

	QtyCoin    float64
	PriceQuote float64

	// FilledQtyCoin and AvgFillPriceQuote are GROSS — see the file comment.
	// AvgFillPriceQuote is 0 while nothing has filled, which is why the
	// quantity is the field to test, never the price.
	FilledQtyCoin     float64
	AvgFillPriceQuote float64

	// ReduceOnly as the venue reports it back.
	ReduceOnly bool

	TransactTimeMs int64
	UpdatedAtMs    int64
}

// FilledNotionalQuote is what actually traded, GROSS of every cost.
func (o Order) FilledNotionalQuote() float64 { return o.FilledQtyCoin * o.AvgFillPriceQuote }

// RemainingQtyCoin is what the venue still has resting.
func (o Order) RemainingQtyCoin() float64 {
	if r := o.QtyCoin - o.FilledQtyCoin; r > 0 {
		return r
	}
	return 0
}

// OrderQuery identifies one order for GetOrder or CancelOrder.
//
// Either identifier will do, and ClientOrderID is the one that matters: it is
// the only one the caller knows before the venue has answered.
type OrderQuery struct {
	Market        Market
	Symbol        string
	ClientOrderID string
	VenueOrderID  string
}

// Validate checks that the query can identify something.
func (q OrderQuery) Validate() error {
	switch {
	case q.Market != MarketFuturesUSDM && q.Market != MarketSpot:
		return fmt.Errorf("%w: market %q is neither %q nor %q", ErrInvalidOrder, q.Market, MarketFuturesUSDM, MarketSpot)
	case q.Symbol == "":
		return fmt.Errorf("%w: no symbol", ErrInvalidOrder)
	case q.ClientOrderID == "" && q.VenueOrderID == "":
		return fmt.Errorf("%w: neither ClientOrderID nor VenueOrderID", ErrInvalidOrder)
	}
	return nil
}

// Position is one futures position as the VENUE reports it (CLAUDE.md rule 7:
// a position is read from the venue, never from local state).
//
// QtyCoin is SIGNED: positive is long, negative is short, zero is flat. A
// separate side field would allow the two to disagree, and code that reads one
// without the other is code that hedges the wrong way.
type Position struct {
	Market Market
	Symbol string

	QtyCoin         float64
	EntryPriceQuote float64
	MarkPriceQuote  float64

	// UnrealizedPnLQuote is the venue's own figure, GROSS: it deducts no
	// commission, no funding paid or received, and no cost of closing.
	UnrealizedPnLQuote float64

	LiquidationPriceQuote float64
	LeverageX             float64
	UpdatedAtMs           int64
}

// Flat reports whether the venue holds nothing here.
func (p Position) Flat() bool { return p.QtyCoin == 0 }

// Balance is one asset's balance at one market, as the venue reports it.
type Balance struct {
	Market Market
	Asset  string

	// FreeQtyCoin is what can be spent; LockedQtyCoin is what is committed to
	// resting orders or margin. A caller that reads only the free half reports
	// an account with everything on the book as empty.
	FreeQtyCoin   float64
	LockedQtyCoin float64
}

// TotalQtyCoin is free plus locked.
func (b Balance) TotalQtyCoin() float64 { return b.FreeQtyCoin + b.LockedQtyCoin }

// The errors a caller must be able to branch on.
var (
	// ErrInvalidOrder is a request this package refuses before sending it.
	ErrInvalidOrder = errors.New("broker: invalid order request")

	// ErrOrderNotFound is the venue saying it has no such order.
	//
	// This is the most important error in the package, because it is the one
	// that RESOLVES an ambiguous timeout — see the Broker interface comment.
	// An implementation must return it only when the venue positively said the
	// order is unknown, never for a network failure or an unparsed answer.
	ErrOrderNotFound = errors.New("broker: the venue has no order with that id")

	// ErrNotSupported is a capability that does not exist on this market, as
	// opposed to one that failed: spot has no position to read.
	ErrNotSupported = errors.New("broker: not supported on this market")
)

// Broker is what step 4.4 will hold: one venue's order operations.
//
// # Why ClientOrderID is not optional
//
// The expensive failure in this system is not a rejected order, it is an
// AMBIGUOUS one. A PlaceOrder that times out, or dies on a dropped connection,
// leaves the caller unable to say whether the venue received it. Both readings
// are dangerous: assume it failed and resend, and a double fill leaves the
// position twice the intended size and un-hedged; assume it worked and wait,
// and a leg that never existed is never placed while the other leg sits open.
// internal/execution/doc.go states the rule this serves — every path resolves
// to both legs open or both legs closed, and "retry later" is not a resolution.
//
// So the caller chooses the id BEFORE sending, and after any ambiguous failure
// asks the venue:
//
//	order, err := b.GetOrder(ctx, OrderQuery{Market: m, Symbol: s, ClientOrderID: id})
//	switch {
//	case err == nil:                      // it arrived; act on order.Status
//	case errors.Is(err, ErrOrderNotFound): // it did not arrive; safe to resend
//	default:                              // STILL ambiguous — do not resend
//	}
//
// That third branch is the one that gets deleted by someone tidying up, and it
// is the only branch that is always correct to be careful in.
//
// Implementations must therefore send the caller's ClientOrderID to the venue
// verbatim and accept it back in GetOrder and CancelOrder. An implementation
// that generates its own id, or silently truncates one the venue will not
// accept, has removed the only handle the caller had.
type Broker interface {
	// PlaceOrder sends one order. The returned Order carries whatever the
	// venue said about it immediately — which, for a resting LIMIT, is
	// typically status NEW and nothing filled.
	PlaceOrder(ctx context.Context, req PlaceOrderRequest) (Order, error)

	// CancelOrder cancels one resting order. Cancelling an order that is
	// already gone returns ErrOrderNotFound rather than succeeding, because
	// "there is nothing there" and "I removed it" are different facts and the
	// caller of a two-leg unwind needs to know which happened.
	CancelOrder(ctx context.Context, q OrderQuery) (Order, error)

	// GetOrder reads one order by either id. It returns ErrOrderNotFound when
	// the venue positively says there is no such order.
	GetOrder(ctx context.Context, q OrderQuery) (Order, error)

	// OpenOrders lists what is still resting for one symbol. An empty symbol
	// means every symbol on that market.
	OpenOrders(ctx context.Context, market Market, symbol string) ([]Order, error)

	// GetPosition reads one futures position FROM THE VENUE (rule 7). On a
	// market with no position concept it returns ErrNotSupported — never a
	// zero Position, which would read as "flat".
	GetPosition(ctx context.Context, market Market, symbol string) (Position, error)

	// GetBalance reads the account's balances from the venue.
	GetBalance(ctx context.Context, market Market) ([]Balance, error)
}
