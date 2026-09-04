package exchanges

import (
	"context"
	"time"
)

// Feeds is everything a connector needs to deliver data and be told to stop.
//
// It replaces the four separate parameters every connector used to take. The
// point is not brevity: it is that adding a channel later becomes one new
// field and zero changes to the ten connector signatures. Steps 1.5 and 2.2
// were both scheduled to rewrite those signatures, which is why the refactor
// was pulled forward into 1.5 — and 2.2 then added Funding exactly that way.
//
// Holding a context in a struct is normally wrong. This is the documented
// exception: Feeds is a short-lived parameter object handed to a function that
// runs for the process's lifetime, not a long-lived object that outlives the
// call. See docs/PLAN.md step 1.5.
type Feeds struct {
	Ctx       context.Context
	Price     chan<- PriceData
	Orderbook chan<- OrderbookData
	Trade     chan<- TradeData

	// Funding carries normalized funding readings (step 2.2). Every value on
	// it has already been through one of the normalize<Venue>Funding
	// builders, so the interval is in seconds and the rates are fractions —
	// the scanner never sees venue units.
	Funding chan<- FundingData

	// Conn carries what the connector knows about its own socket. Until step
	// 1.5 the scanner could only infer connection state from silence, which
	// cannot tell a venue that is unreachable from one that is merely quiet.
	Conn chan<- ConnEvent
}

// SendPrice, SendOrderbook, SendTrade and SendFunding deliver one message, or
// give up if the context is cancelled.
//
// The give-up half is what makes shutdown bounded. A plain `ch <- data` blocks
// when the buffer is full - which is exactly the situation during a shutdown,
// once the scanner's consumers have stopped draining - and a connector blocked
// there would never see the cancellation. Each returns false when the message
// was dropped, so a caller can stop parsing the rest of a batch.
func (f Feeds) SendPrice(data PriceData) bool {
	select {
	case f.Price <- data:
		return true
	case <-f.Ctx.Done():
		return false
	}
}

func (f Feeds) SendOrderbook(data OrderbookData) bool {
	select {
	case f.Orderbook <- data:
		return true
	case <-f.Ctx.Done():
		return false
	}
}

func (f Feeds) SendTrade(data TradeData) bool {
	select {
	case f.Trade <- data:
		return true
	case <-f.Ctx.Done():
		return false
	}
}

func (f Feeds) SendFunding(data FundingData) bool {
	// A select on a nil channel blocks until cancellation — a harness that
	// builds Feeds without a Funding channel would hang its whole read loop
	// on the first funding frame. Report the drop instead.
	if f.Funding == nil {
		return false
	}
	select {
	case f.Funding <- data:
		return true
	case <-f.Ctx.Done():
		return false
	}
}

// ReportConn publishes a connection-state transition.
//
// It never blocks: a connector must not stall on a socket event, and losing one
// is harmless because the scanner also infers state from silence. A nil channel
// is legal so a connector can be exercised without one.
func (f Feeds) ReportConn(source string, state ConnState) {
	if f.Conn == nil {
		return
	}
	select {
	case f.Conn <- ConnEvent{Source: source, State: state, At: time.Now()}:
	default:
	}
}
