package crossperp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/broker/binance"
	"futures-arbitrage-scanner/internal/broker/bybit"
	"futures-arbitrage-scanner/internal/execution"
)

// Every conversation with a venue. Nothing here decides whether a pair is
// hedged; it establishes what the venues say, and says when it cannot.

// legRun is what one opening leg did, as the VENUE reported it.
type legRun struct {
	req        broker.PlaceOrderRequest
	order      broker.Order
	filledAtMs int64

	// returnedAt is when the opening send RETURNED, on Config.Now. The venue is
	// asked about an ambiguous send for AmbiguousSendQuiet from here — not from
	// before the send, which a slow send would have used up (review 4.5k, N2).
	returnedAt time.Time

	// sendAmbiguous: the send's own answer was lost.
	sendAmbiguous bool

	// confirmed: the venue said the order can no longer change — finished or
	// refused. A "no such order" is never this (confirmLeg).
	confirmed bool
	err       error
}

// pollBudget bounds a polling loop by COUNT as well as by the clock, so a clock
// that does not advance — an injected one — cannot keep a loop alive (review
// 4.5k, m9).
func (x *Executor) pollBudget(d time.Duration) int {
	if d <= 0 {
		return 2
	}
	return int(d/x.cfg.PollEvery) + 2
}

// workLeg places one opening leg and works it to its deadline. stop is closed
// when the OTHER leg can no longer be part of a hedge: this leg's remainder is
// then cancelled at once instead of working the whole LegTimeout while nothing
// hedges it (review 4.5k, m3). abort is how this leg says the same to the other.
//
// A leg aborts on a definite refusal AND on a send whose answer was lost. The
// operator's rule for Engine 2 is that a leg whose partner was refused or cut off
// by the network is taken back to flat within 500 ms; asking the venue about the
// lost send first — for up to AmbiguousSendQuiet, six seconds by default — would
// leave the filled leg naked for all of it. So the lost send is returned
// UNCONFIRMED at once, never resent (review 4.5k, B3), and settleAbortedOpen
// asks about it while it flattens both legs.
func (x *Executor) workLeg(ctx context.Context, intentID string, leg execution.LegName, v Venue,
	req broker.PlaceOrderRequest, stop <-chan struct{}, abort func()) legRun {

	run := legRun{req: req}
	q := broker.OrderQuery{Market: req.Market, Symbol: req.Symbol, ClientOrderID: req.ClientOrderID}
	x.record(ctx, execution.Event{IntentID: intentID, Kind: execution.EventLegPlacing, Leg: leg,
		ClientOrderID: req.ClientOrderID, QtyCoin: req.QtyCoin, PriceQuote: req.PriceQuote, DetailVI: v.Name})

	// The other leg may already have aborted while this goroutine started: then
	// nothing is sent at all (review 4.5k, m-e).
	select {
	case <-stop:
		run.confirmed = true
		run.order = broker.Order{ClientOrderID: req.ClientOrderID, Status: broker.OrderStatusCanceled}
		run.err = fmt.Errorf("%s (%s) không gửi: chân kia đã bị từ chối hoặc mất kết nối", leg, v.Name)
		return x.seal(run)
	default:
	}
	legDeadline := x.cfg.Now().Add(x.cfg.LegTimeout)
	order, err := v.Broker.PlaceOrder(ctx, req)
	run.returnedAt = x.cfg.Now()
	switch {
	case err == nil:
		run.order = order
	case errors.Is(err, broker.ErrIPCoolingDown) || definiteRejection(err):
		run.confirmed, run.err = true, fmt.Errorf("%s (%s) bị từ chối: %w", leg, v.Name, err)
		run.order = broker.Order{ClientOrderID: req.ClientOrderID, Status: broker.OrderStatusRejected}
		abort() // before anything else: every moment here is the other leg unhedged
		x.record(ctx, execution.Event{IntentID: intentID, Kind: execution.EventLegResolved, Leg: leg, Err: err,
			DetailVI: "sàn từ chối dứt khoát (hoặc chưa gửi vì IP đang bị phạt)"})
		return x.seal(run)
	default:
		// AMBIGUOUS, and never resent (review 4.5k, B3). The other leg stops now;
		// this one is asked about by the id we chose, while both are flattened.
		run.sendAmbiguous = true
		run.err = fmt.Errorf("%s (%s) gửi mất câu trả lời: %w", leg, v.Name, err)
		abort()
		x.record(ctx, execution.Event{IntentID: intentID, Kind: execution.EventLegAmbiguous, Leg: leg, Err: err, ClientOrderID: req.ClientOrderID,
			DetailVI: "mất câu trả lời — dừng chân kia, gỡ ngay, KHÔNG gửi lại"})
		return run
	}
	x.record(ctx, execution.Event{IntentID: intentID, Kind: execution.EventLegPlaced, Leg: leg,
		ClientOrderID: run.order.ClientOrderID, VenueOrderID: run.order.VenueOrderID, FilledQtyCoin: run.order.FilledQtyCoin})

	polls := x.pollBudget(x.cfg.LegTimeout)
	stopped := false
	for i := 0; !finished(run.order, req.QtyCoin) && i < polls && !x.cfg.Now().After(legDeadline); i++ {
		if x.sleepOrStop(ctx, stop, x.cfg.PollEvery) {
			select {
			case <-stop:
				stopped = true
			default:
			}
			break // the context died or the other leg stopped: read the venue below
		}
		latest, gerr := v.Broker.GetOrder(ctx, q)
		if gerr == nil {
			run.order = latest
		} else if errors.Is(gerr, broker.ErrIPCoolingDown) {
			break
		}
	}
	if finished(run.order, req.QtyCoin) {
		run.confirmed = true
		if run.order.FilledQtyCoin < req.QtyCoin-gridEpsilon*req.QtyCoin {
			run.err = fmt.Errorf("%s (%s) kết thúc %s sau khi khớp %.10g/%.10g coin", leg, v.Name, run.order.Status, run.order.FilledQtyCoin, req.QtyCoin)
		} else {
			x.record(ctx, execution.Event{IntentID: intentID, Kind: execution.EventLegFilled, Leg: leg,
				ClientOrderID: run.order.ClientOrderID, VenueOrderID: run.order.VenueOrderID, FilledQtyCoin: run.order.FilledQtyCoin})
		}
		return x.seal(run)
	}
	if stopped {
		// The other leg was refused or lost. This one returns UNCONFIRMED at once:
		// settleAbortedOpen cancels and reads it back WHILE it flattens what it
		// filled, instead of leaving that fill unhedged for the whole read-back — a
		// Bybit cancel alone confirms for up to three seconds (review 4.5k round
		// 3, M3).
		run.err = fmt.Errorf("%s (%s) dừng vì chân kia bị từ chối hoặc mất kết nối, lúc đã khớp %.10g/%.10g coin", leg, v.Name, run.order.FilledQtyCoin, req.QtyCoin)
		return x.seal(run)
	}
	run.err = fmt.Errorf("%s (%s) chỉ khớp %.10g/%.10g coin rồi dừng làm việc", leg, v.Name, run.order.FilledQtyCoin, req.QtyCoin)
	return x.seal(x.confirmLeg(ctx, intentID, leg, v, q, run))
}

// finished: the order can no longer change — Done, or filled to its quantity.
func finished(o broker.Order, qtyCoin float64) bool {
	return o.Status.Done() || o.FilledQtyCoin >= qtyCoin-gridEpsilon*qtyCoin
}

// confirmLeg cancels whatever may still be working and reads the order back
// until the venue says it can no longer change. A read-back that merely returns
// is not a final fill: Bybit cancels asynchronously (PLAN 4.5i correction 9).
//
// For a send whose answer was lost it keeps asking until AmbiguousSendQuiet has
// passed since the send RETURNED — not since it left, which a slow send would
// have used up (review 4.5k, N2) — and a "no such order", however late, never
// confirms it: Binance documents -1006/-1007 as "execution status unknown" and a
// 503 as "could have been a success" with no bound on when, and Bybit never
// answers "not found" at all. The execution portal reads an OPENING order it
// cannot see the same way. Such a leg stays unconfirmed, the result is loud, and
// the lock stays until the venue proves the order finished or a person does.
func (x *Executor) confirmLeg(ctx context.Context, intentID string, leg execution.LegName, v Venue, q broker.OrderQuery, run legRun) legRun {
	safe := context.WithoutCancel(ctx)
	deadline := x.cfg.Now().Add(x.cfg.OrderSettleTimeout)
	if quietEnd := run.returnedAt.Add(x.cfg.AmbiguousSendQuiet); run.sendAmbiguous && quietEnd.After(deadline) {
		deadline = quietEnd
	}
	x.record(safe, execution.Event{IntentID: intentID, Kind: execution.EventLegCancelling, Leg: leg,
		ClientOrderID: q.ClientOrderID, FilledQtyCoin: run.order.FilledQtyCoin})
	_, cancelErr := v.Broker.CancelOrder(safe, q)
	cancelAccepted, liveSinceCancel, resent := cancelErr == nil, 0, 0
	polls := x.pollBudget(deadline.Sub(x.cfg.Now()))
	var gerr error
	for i := 0; ; i++ {
		o, err := v.Broker.GetOrder(safe, q)
		gerr = err
		switch {
		case err == nil:
			run.order = o
			if finished(o, run.req.QtyCoin) {
				run.confirmed = true
				x.record(safe, execution.Event{IntentID: intentID, Kind: execution.EventLegReadBack, Leg: leg,
					ClientOrderID: o.ClientOrderID, VenueOrderID: o.VenueOrderID, FilledQtyCoin: o.FilledQtyCoin, DetailVI: string(o.Status)})
				return run
			}
			// Seen WORKING. An order that reached the venue after the cancel was
			// answered "unknown order" — a lost send the backend processed late —
			// would otherwise be watched to the deadline and left resting (review
			// 4.5k round 3, B1). It is cancelled again: at once when no cancel was
			// ever accepted, and after a few live reads when one was.
			liveSinceCancel++
			if (!cancelAccepted || liveSinceCancel >= liveReadsBeforeRecancel) && resent < maxOpeningRecancels {
				resent++
				_, again := v.Broker.CancelOrder(safe, q)
				if again == nil {
					cancelAccepted, liveSinceCancel = true, 0
				} else {
					cancelErr = again
				}
			}
		case errors.Is(err, broker.ErrIPCoolingDown):
			run.err = errors.Join(run.err, fmt.Errorf("đọc lại %s dừng vì IP đang bị sàn phạt: %w", leg, err))
			return run
		}
		if i >= polls || x.cfg.Now().After(deadline) || x.sleep(safe, x.cfg.PollEvery) != nil {
			run.err = errors.Join(run.err, fmt.Errorf("không chứng minh được lệnh %s (%s) đã kết thúc: huỷ → %v, đọc lại → %v, lần cuối thấy %s khớp %.10g",
				leg, v.Name, cancelErr, gerr, run.order.Status, run.order.FilledQtyCoin))
			x.record(safe, execution.Event{IntentID: intentID, Kind: execution.EventLegReadBack, Leg: leg, Err: run.err,
				ClientOrderID: q.ClientOrderID, FilledQtyCoin: run.order.FilledQtyCoin})
			return run
		}
	}
}

// liveReadsBeforeRecancel and maxOpeningRecancels bound how confirmLeg cancels an
// order it keeps reading back as working: again after this many live reads when
// a cancel was already accepted, and no more than this many times in all.
const (
	liveReadsBeforeRecancel = 5
	maxOpeningRecancels     = 5
)

// seal stamps when this process saw the leg holding something, on one clock.
func (x *Executor) seal(run legRun) legRun {
	if run.order.FilledQtyCoin > 0 {
		run.filledAtMs = x.cfg.Now().UnixMilli()
	}
	return run
}

// readBackOpeningOrder reads one OPENING order back by its deterministic id
// until the venue says it is finished, sending one more cancel if it finds the
// order still working. It is how a close proves an intent's opening orders
// cannot still fill (review 4.5k, N3). As everywhere here, "no such order" proves
// nothing: false.
func (x *Executor) readBackOpeningOrder(ctx context.Context, v Venue, q broker.OrderQuery) (broker.Order, bool) {
	safe := context.WithoutCancel(ctx)
	deadline := x.cfg.Now().Add(x.cfg.OrderSettleTimeout)
	polls := x.pollBudget(x.cfg.OrderSettleTimeout)
	cancelledAgain := false
	for i := 0; ; i++ {
		o, err := v.Broker.GetOrder(safe, q)
		switch {
		case err == nil && o.Status.Done():
			return o, true
		case err == nil && !cancelledAgain:
			cancelledAgain = true
			_, _ = v.Broker.CancelOrder(safe, q)
		case errors.Is(err, broker.ErrIPCoolingDown):
			return o, false
		}
		if i >= polls || x.cfg.Now().After(deadline) || x.sleep(safe, x.cfg.PollEvery) != nil {
			return o, false
		}
	}
}

// definiteRejection reports whether an order was positively refused.
//
// It is internal/execution's rule with the exceptions that rule lacks, each
// quoted from the venue's own pages (read 2026-09-17):
//
//   - HTTP 408 "is used when a timeout has occurred while waiting for a response
//     from the backend server" — a 4xx that is NOT the matching engine saying no;
//   - Binance -1000 "An unknown error occurred while processing the request",
//     -1006 and -1007 "execution status unknown" — whatever HTTP status carries
//     them;
//   - a duplicate client id — Binance -4116 DUPLICATED_CLIENT_ORDER_ID, Bybit
//     110072 — which is proof an order under that id EXISTS, not a refusal of
//     it (review 4.5k, B3).
//
// Every one of these is resolved by asking the venue, never by assuming.
func definiteRejection(err error) bool {
	if errors.Is(err, binance.ErrDuplicateClientOrderID) || errors.Is(err, bybit.ErrDuplicateClientOrderID) {
		return false
	}
	var venueErr *binance.VenueError
	if errors.As(err, &venueErr) {
		switch venueErr.Code {
		case -1000, -1006, -1007:
			return false
		}
	}
	if errors.Is(err, broker.ErrInvalidOrder) || errors.Is(err, broker.ErrBelowMinNotional) ||
		errors.Is(err, broker.ErrBelowMinQty) || errors.Is(err, broker.ErrAboveMaxQty) ||
		errors.Is(err, broker.ErrNotTrading) || errors.Is(err, broker.ErrRulesUnknown) {
		return true
	}
	var httpErr *broker.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.StatusCode == http.StatusRequestTimeout {
		return false
	}
	return httpErr.StatusCode >= 400 && httpErr.StatusCode < 500
}

// pendingOrder is a reduce-only order a call sent and could not prove finished.
type pendingOrder struct {
	leg        execution.LegName
	venue      Venue
	q          broker.OrderQuery
	returnedAt time.Time // when its send returned, on Config.Now

	// seen: the venue showed the order, so "not shown" later is no sign of
	// absence; statusUnknown: the venue itself answered "execution status
	// unknown". Either keeps the order pending until it is read back finished.
	seen, statusUnknown bool
}

// pendingSet collects one call's pending orders across its goroutines.
type pendingSet struct {
	mu     sync.Mutex
	orders []pendingOrder
}

func (p *pendingSet) add(o pendingOrder) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.orders = append(p.orders, o)
}

func (p *pendingSet) list() []pendingOrder {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]pendingOrder(nil), p.orders...)
}

func (p *pendingSet) replace(orders []pendingOrder) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.orders = append([]pendingOrder(nil), orders...)
}

// PendingOrder names a reduce-only order that was not proven finished when a call
// returned. It may still execute, and it reduces WHATEVER the venue holds on the
// symbol when it does — another engine's position once the lock is gone — so a
// pair with one keeps its lock (review 4.5k round 3, M1).
type PendingOrder struct {
	Leg              execution.LegName
	Venue            string
	ClientOrderID    string
	SendReturnedAtMs int64
	Seen             bool
	StatusUnknown    bool
}

func exportPending(orders []pendingOrder) []PendingOrder {
	out := make([]PendingOrder, 0, len(orders))
	for _, o := range orders {
		out = append(out, PendingOrder{Leg: o.leg, Venue: o.venue.Name, ClientOrderID: o.q.ClientOrderID,
			SendReturnedAtMs: o.returnedAt.UnixMilli(), Seen: o.seen, StatusUnknown: o.statusUnknown})
	}
	return out
}

// statusUnknownAnswer: the VENUE answered, and said it does not know whether the
// order will execute — Binance -1000/-1006/-1007, HTTP 408 and 503. The gateway
// took such a request, so its recvWindow no longer bounds when it may execute.
func statusUnknownAnswer(err error) bool {
	var venueErr *binance.VenueError
	if errors.As(err, &venueErr) {
		switch venueErr.Code {
		case -1000, -1006, -1007:
			return true
		}
	}
	var httpErr *broker.HTTPError
	return errors.As(err, &httpErr) && (httpErr.StatusCode == http.StatusRequestTimeout || httpErr.StatusCode == http.StatusServiceUnavailable)
}

// notShown: the venue lists no order under the id — Binance's "unknown order",
// Bybit's two empty lists.
func notShown(err error) bool {
	return errors.Is(err, broker.ErrOrderNotFound) || errors.Is(err, bybit.ErrOrderNotVisible)
}

// settleReductions reads every pending reduce-only order back by its id, all at
// once, and returns the ones still not proven finished. It is called only when
// the venues read FLAT, where waiting costs no exposure. An order the venue shows
// finished is proven; one it shows working is cancelled and waited for; one it
// does not show at all counts as absent once AmbiguousSendQuiet has passed since
// its send returned — the execution portal's reading of an order that is not an
// OPENING order (PLAN 4.5j), and a bound on a signed request's recvWindow. It is
// not a proof: an order the venue SHOWED, or whose send the venue answered
// "execution status unknown", is never taken as absent (review 4.5k round 3, M1).
func (x *Executor) settleReductions(ctx context.Context, intentID string, pending []pendingOrder) []pendingOrder {
	if len(pending) == 0 {
		return nil
	}
	safe := context.WithoutCancel(ctx)
	proven := make([]bool, len(pending))
	var wg sync.WaitGroup
	for i, p := range pending {
		wg.Add(1)
		go func() {
			defer wg.Done()
			proven[i] = x.settleReduction(safe, intentID, p)
		}()
	}
	wg.Wait()
	var still []pendingOrder
	for i, p := range pending {
		if !proven[i] {
			still = append(still, p)
		}
	}
	return still
}

func (x *Executor) settleReduction(ctx context.Context, intentID string, p pendingOrder) bool {
	quietEnd := p.returnedAt.Add(x.cfg.AmbiguousSendQuiet)
	deadline := x.cfg.Now().Add(x.cfg.OrderSettleTimeout)
	if quietEnd.After(deadline) {
		deadline = quietEnd
	}
	polls := x.pollBudget(deadline.Sub(x.cfg.Now()))
	cancelled := false
	for i := 0; ; i++ {
		o, err := p.venue.Broker.GetOrder(ctx, p.q)
		switch {
		case err == nil && o.Status.Done():
			x.record(ctx, execution.Event{IntentID: intentID, Kind: execution.EventLegReadBack, Leg: p.leg, ClientOrderID: p.q.ClientOrderID,
				FilledQtyCoin: o.FilledQtyCoin, DetailVI: "lệnh giảm vị thế đã kết thúc: " + string(o.Status)})
			return true
		case err == nil:
			p.seen = true
			if !cancelled {
				cancelled = true
				_, _ = p.venue.Broker.CancelOrder(ctx, p.q)
			}
		case errors.Is(err, broker.ErrIPCoolingDown):
			return false
		case notShown(err) && !p.seen && !p.statusUnknown && !x.cfg.Now().Before(quietEnd):
			x.record(ctx, execution.Event{IntentID: intentID, Kind: execution.EventLegReadBack, Leg: p.leg, ClientOrderID: p.q.ClientOrderID,
				DetailVI: fmt.Sprintf("lệnh giảm vị thế không có trên sàn sau %s lặng kể từ lúc gửi trả về — coi là không tới", x.cfg.AmbiguousSendQuiet)})
			return true
		}
		if i >= polls || x.cfg.Now().After(deadline) || x.sleep(ctx, x.cfg.PollEvery) != nil {
			return false
		}
	}
}

// pairPositions is both venues' answers, SIGNED as the venues report them.
type pairPositions struct {
	longQtyCoin, shortQtyCoin     float64
	longOrders, shortOrders       int
	longErr, shortErr             error
	longOrdersErr, shortOrdersErr error
}

func (p pairPositions) read() bool { return p.longErr == nil && p.shortErr == nil }

func (p pairPositions) evidenceVI(longVenue, shortVenue string) string {
	side := func(name string, qty float64, orders int, err, ordersErr error) string {
		if err != nil {
			return fmt.Sprintf("%s KHÔNG ĐỌC ĐƯỢC (%v)", name, err)
		}
		s := fmt.Sprintf("%s %+.10g", name, qty)
		if ordersErr != nil {
			s += fmt.Sprintf(" (lệnh treo: không đọc được — %v)", ordersErr)
		} else if orders > 0 {
			s += fmt.Sprintf(" (%d lệnh treo)", orders)
		}
		return s
	}
	return side(longVenue, p.longQtyCoin, p.longOrders, p.longErr, p.longOrdersErr) + ", " +
		side(shortVenue, p.shortQtyCoin, p.shortOrders, p.shortErr, p.shortOrdersErr)
}

// readBoth reads both venues at once: the perp position, and — when asked — the
// orders still resting on the symbol.
func (x *Executor) readBoth(ctx context.Context, symbol string, long, short Venue, withOrders bool) pairPositions {
	var out pairPositions
	var wg sync.WaitGroup
	read := func(v Venue, qty *float64, orders *int, perr, oerr *error) {
		defer wg.Done()
		pos, err := v.Broker.GetPosition(ctx, broker.MarketFuturesUSDM, symbol)
		switch {
		case err != nil:
			*perr = err
			return
		case math.IsNaN(pos.QtyCoin) || math.IsInf(pos.QtyCoin, 0):
			*perr = fmt.Errorf("khối lượng %v không phải số", pos.QtyCoin)
			return
		case pos.Symbol != "" && pos.Symbol != symbol:
			*perr = fmt.Errorf("hỏi %s, sàn trả vị thế của %s", symbol, pos.Symbol)
			return
		}
		*qty = pos.QtyCoin
		if !withOrders {
			return
		}
		list, err := v.Broker.OpenOrders(ctx, broker.MarketFuturesUSDM, symbol)
		if err != nil {
			*oerr = err
			return
		}
		for _, o := range list {
			if o.Symbol == symbol && !o.Status.Done() {
				*orders++
			}
		}
	}
	wg.Add(2)
	go read(long, &out.longQtyCoin, &out.longOrders, &out.longErr, &out.longOrdersErr)
	go read(short, &out.shortQtyCoin, &out.shortOrders, &out.shortErr, &out.shortOrdersErr)
	wg.Wait()
	return out
}

// awaitPositions re-reads both venues until each holds what the orders say it
// should (SIGNED coin), or PositionSettleTimeout passes.
func (x *Executor) awaitPositions(ctx context.Context, symbol string, long, short Venue, wantLongQtyCoin, wantShortQtyCoin float64) (pairPositions, bool) {
	deadline := x.cfg.Now().Add(x.cfg.PositionSettleTimeout)
	polls := x.pollBudget(x.cfg.PositionSettleTimeout)
	for i := 0; ; i++ {
		p := x.readBoth(ctx, symbol, long, short, false)
		if p.read() && sameQty(p.longQtyCoin, wantLongQtyCoin) && sameQty(p.shortQtyCoin, wantShortQtyCoin) {
			return p, true
		}
		if i >= polls || x.cfg.Now().After(deadline) || x.sleep(ctx, x.cfg.PollEvery) != nil {
			return p, false
		}
	}
}

func sameQty(a, b float64) bool { return math.Abs(a-b) <= gridEpsilon*math.Max(1, math.Abs(b)) }

// legDrive is what one close-out did to one leg.
type legDrive struct {
	startQtyCoin      float64 // SIGNED, the venue's first answer
	finalQtyCoin      float64 // SIGNED, the venue's last answer
	closedQtyCoin     float64 // what the reduce-only orders reported filled
	avgFillPriceQuote float64 // of the last order that filled
	lastClientOrderID string
	lastVenueOrderID  string
	lastStatus        broker.OrderStatus
	rounds            int
	positionRead      bool

	// firstFillAtMs and lastFillAtMs are when this process saw the first and the
	// last reducing order finish with a fill, on Config.Now.
	firstFillAtMs, lastFillAtMs int64

	// unresolvedOrders counts rounds whose order could not be proven finished.
	unresolvedOrders int
	err              error
}

// driveLegTo sends reduce-only MARKET orders on one leg until the VENUE says it
// holds targetQtyCoin (0 is flat), the rounds run out, or the deadline passes.
//
// Every quantity is read from the venue's position that round (rule 7). Toward
// ZERO, a round whose order is ambiguous is followed by a new round under a new
// id: reduce-only cannot open or flip a position, so the worst a late duplicate
// does at zero is be refused. Toward a size ABOVE zero that is not true — two
// reductions of the same size can both execute — so an unresolved round ends the
// drive with errReductionUnresolved (review 4.5k, B2).
func (x *Executor) driveLegTo(ctx context.Context, intentID, symbol string, leg execution.LegName, spec LegSpec,
	targetQtyCoin float64, purpose Purpose, attempt string, deadline time.Time, pending *pendingSet) legDrive {

	var out legDrive
	step := spec.Rules.StepSizeCoin
	sign, side := 1.0, broker.SideSell // a long leg holds + and shrinks by selling
	if leg == LegShort {
		sign, side = -1, broker.SideBuy
	}
	for round := 1; ; round++ {
		posQtyCoin, err := x.readPositionQtyCoin(ctx, spec.Venue, symbol, deadline)
		if err != nil {
			out.err = errors.Join(out.err, fmt.Errorf("không đọc được vị thế %s (%s): %w", leg, spec.Venue.Name, err))
			return out
		}
		if !out.positionRead {
			out.startQtyCoin, out.positionRead = posQtyCoin, true
		}
		out.finalQtyCoin = posQtyCoin
		heldQtyCoin := sign * posQtyCoin
		if heldQtyCoin < -gridEpsilon*math.Max(1, step) {
			out.err = fmt.Errorf("%w: chân %s trên %s giữ %+.10g — ngược chiều cặp này", ErrPositionDisagrees, leg, spec.Venue.Name, posQtyCoin)
			return out
		}
		excessQtyCoin := heldQtyCoin - targetQtyCoin
		if excessQtyCoin <= gridEpsilon*math.Max(1, step) {
			// The VENUE decides success: an earlier round's refusal — a
			// reduce-only order refused because the position had already gone —
			// is not a failure once the position reads at the target.
			out.err = nil
			return out
		}
		if round > x.cfg.MaxCloseRounds || x.cfg.Now().After(deadline) {
			out.err = errors.Join(out.err, fmt.Errorf("chân %s trên %s còn %.10g coin sau %d vòng, cần về %.10g", leg, spec.Venue.Name, heldQtyCoin, out.rounds, targetQtyCoin))
			return out
		}
		// A part off the venue's grid is sent as far as the grid reaches; the
		// next round then finds the sliver, RoundOrder refuses it by name, and
		// that refusal is what this call returns — reported, never subtracted.
		rounded, rerr := broker.RoundOrder(broker.RoundRequest{
			Rules: spec.Rules, Side: side, Type: broker.OrderTypeMarket,
			QtyCoin: excessQtyCoin, PriceQuote: spec.Book.MidPriceQuote, ReduceOnly: true,
		})
		if rerr != nil {
			out.err = fmt.Errorf("chân %s trên %s: phần %.10g coin không đặt được lệnh: %w", leg, spec.Venue.Name, excessQtyCoin, rerr)
			return out
		}
		out.rounds = round
		req := broker.PlaceOrderRequest{
			Market: broker.MarketFuturesUSDM, Symbol: symbol, Side: side, Type: broker.OrderTypeMarket,
			ClientOrderID: ClientOrderID(intentID, purpose, leg, attempt, round), QtyCoin: rounded.QtyCoin, ReduceOnly: true,
		}
		x.record(ctx, execution.Event{IntentID: intentID, Kind: closingEventKind(purpose), Leg: leg,
			ClientOrderID: req.ClientOrderID, QtyCoin: req.QtyCoin, DetailVI: fmt.Sprintf("%s, vòng %d, sàn giữ %+.10g", spec.Venue.Name, round, posQtyCoin)})
		out.lastClientOrderID = req.ClientOrderID

		q := broker.OrderQuery{Market: req.Market, Symbol: symbol, ClientOrderID: req.ClientOrderID}
		order, returnedAt, definite, perr := x.placeClosing(ctx, spec.Venue, req)
		switch {
		case errors.Is(perr, broker.ErrIPCoolingDown):
			out.err = fmt.Errorf("DỪNG chân %s trên %s: IP đang bị sàn phạt — thử lại trong lệnh phạt chỉ kéo dài nó: %w", leg, spec.Venue.Name, perr)
			return out
		case perr != nil && definite:
			// Refused: the next round re-reads the position, which is what decides
			// whether anything is left to send.
			out.err = perr
			_ = x.sleep(ctx, x.cfg.PollEvery)
			continue
		case perr != nil:
			out.unresolvedOrders++
			pending.add(pendingOrder{leg: leg, venue: spec.Venue, q: q, returnedAt: returnedAt, statusUnknown: statusUnknownAnswer(perr)})
			if targetQtyCoin > 0 {
				out.err = fmt.Errorf("%w: %s trên %s, lệnh %s: %v", errReductionUnresolved, leg, spec.Venue.Name, req.ClientOrderID, perr)
				return out
			}
			out.err = perr
			_ = x.sleep(ctx, x.cfg.PollEvery)
			continue
		}
		order, done := x.settleOrder(ctx, spec.Venue, q, order)
		out.lastVenueOrderID, out.lastStatus = order.VenueOrderID, order.Status
		if !done {
			out.unresolvedOrders++
			pending.add(pendingOrder{leg: leg, venue: spec.Venue, q: q, returnedAt: returnedAt, seen: true})
			if targetQtyCoin > 0 {
				out.err = fmt.Errorf("%w: %s trên %s, lệnh %s dừng ở %s", errReductionUnresolved, leg, spec.Venue.Name, req.ClientOrderID, order.Status)
				return out
			}
		}
		out.err = nil
		if order.FilledQtyCoin > 0 {
			out.lastFillAtMs = x.cfg.Now().UnixMilli()
			if out.firstFillAtMs == 0 {
				out.firstFillAtMs = out.lastFillAtMs
			}
			out.closedQtyCoin += order.FilledQtyCoin
			out.avgFillPriceQuote = order.AvgFillPriceQuote
			// A position can trail the fill that moved it. Wait for it rather
			// than send the same reduction again on a stale answer.
			x.awaitReduced(ctx, spec.Venue, symbol, sign, heldQtyCoin, heldQtyCoin-order.FilledQtyCoin, targetQtyCoin)
		}
	}
}

func closingEventKind(p Purpose) execution.EventKind {
	switch p {
	case PurposeClose:
		return execution.EventCloseLeg
	case PurposeReduce:
		return execution.EventReducing
	}
	return execution.EventUnwindLeg
}

// flattenPair takes a pair to flat ONE LEG AT A TIME: first to zero, then the
// second down to what the first REALLY holds afterwards (review 4.5k, M2). When
// the FIRST venue fails, the two legs stay the same size; when the SECOND fails
// after the first closed, one leg is left — loud, and no better or worse than two
// legs closed at once. What the order buys is the first case, and a close that
// names the stressed venue first. The cost is time: the first leg's whole close is
// unhedged, where two legs at once leave only the gap between two fills.
//
// A first leg whose position cannot be read leaves the second leg untouched: its
// size is not known. A first leg that still has an order that may execute is
// read as it stands: every such order is reduce-only, so the first leg can only
// shrink further, and the second leg reduced to its reading can never undershoot
// it — whereas leaving the second leg whole would leave the whole difference
// naked (review 4.5k, N5).
//
// Each leg gets its OWN close-out budget. With one shared deadline, a first leg
// that took all of it to close leaves the second no time to send anything — the
// one naked leg this ordering exists to prevent. A close can therefore take up to
// twice UnwindTimeout.
func (x *Executor) flattenPair(ctx context.Context, intentID, symbol string, long, short LegSpec, first execution.LegName,
	attempt string, purpose Purpose, pending *pendingSet) (longDrive, shortDrive legDrive, err error) {

	firstSpec, secondSpec, second := long, short, LegShort
	if first == LegShort {
		firstSpec, secondSpec, second = short, long, LegLong
	}
	firstCtx, cancelFirst, firstDeadline := x.closeOutContext(ctx)
	firstDrive := x.driveLegTo(firstCtx, intentID, symbol, first, firstSpec, 0, purpose, attempt, firstDeadline, pending)
	cancelFirst()
	var secondDrive legDrive
	switch {
	case !firstDrive.positionRead:
		err = fmt.Errorf("vị thế chân %s (%s) không đọc được — chân %s KHÔNG được đụng tới: %w", first, firstSpec.Venue.Name, second, firstDrive.err)
	default:
		secondCtx, cancelSecond, secondDeadline := x.closeOutContext(ctx)
		secondDrive = x.driveLegTo(secondCtx, intentID, symbol, second, secondSpec, math.Abs(firstDrive.finalQtyCoin), purpose, attempt, secondDeadline, pending)
		cancelSecond()
		err = errors.Join(firstDrive.err, secondDrive.err)
	}
	if first == LegLong {
		return firstDrive, secondDrive, err
	}
	return secondDrive, firstDrive, err
}

// placeClosing sends one reduce-only order. definite reports a positive refusal,
// and sendReturnedAt when the send itself returned. An ambiguous answer is
// resolved by id; it is never resent under the same id — the caller's next round
// does that under a new one, after reading the position.
func (x *Executor) placeClosing(ctx context.Context, v Venue, req broker.PlaceOrderRequest) (order broker.Order, sendReturnedAt time.Time, definite bool, err error) {
	order, err = v.Broker.PlaceOrder(ctx, req)
	sendReturnedAt = x.cfg.Now()
	switch {
	case err == nil:
		return order, sendReturnedAt, false, nil
	case errors.Is(err, broker.ErrIPCoolingDown), definiteRejection(err):
		return broker.Order{}, sendReturnedAt, true, err
	}
	q := broker.OrderQuery{Market: req.Market, Symbol: req.Symbol, ClientOrderID: req.ClientOrderID}
	safe := context.WithoutCancel(ctx)
	deadline := x.cfg.Now().Add(x.cfg.OrderSettleTimeout)
	polls := x.pollBudget(x.cfg.OrderSettleTimeout)
	for i := 0; ; i++ {
		found, qErr := v.Broker.GetOrder(safe, q)
		switch {
		case qErr == nil:
			return found, sendReturnedAt, false, nil
		case errors.Is(qErr, broker.ErrIPCoolingDown):
			return broker.Order{}, sendReturnedAt, true, qErr
		}
		if i >= polls || x.cfg.Now().After(deadline) || x.sleep(safe, x.cfg.PollEvery) != nil {
			return broker.Order{}, sendReturnedAt, false, fmt.Errorf("lệnh giảm %s trên %s mơ hồ: gửi → %w, tra → %v", req.Side, v.Name, err, qErr)
		}
	}
}

// settleOrder reads an order back until the venue says it is finished. A MARKET
// answer on USDⓈ-M and every Bybit answer is an acknowledgement, not a fill.
func (x *Executor) settleOrder(ctx context.Context, v Venue, q broker.OrderQuery, order broker.Order) (broker.Order, bool) {
	deadline := x.cfg.Now().Add(x.cfg.OrderSettleTimeout)
	polls := x.pollBudget(x.cfg.OrderSettleTimeout)
	for i := 0; !order.Status.Done(); i++ {
		if i >= polls || x.cfg.Now().After(deadline) || x.sleep(ctx, x.cfg.PollEvery) != nil {
			return order, false
		}
		latest, err := v.Broker.GetOrder(ctx, q)
		if errors.Is(err, broker.ErrIPCoolingDown) {
			return order, false
		}
		if err == nil {
			order = latest
		}
	}
	return order, true
}

// readPositionQtyCoin reads one venue's SIGNED position, retrying a failed read
// until the deadline — except inside an IP cool-down, where a retry lengthens the
// ban.
func (x *Executor) readPositionQtyCoin(ctx context.Context, v Venue, symbol string, deadline time.Time) (float64, error) {
	polls := x.pollBudget(deadline.Sub(x.cfg.Now()))
	for i := 0; ; i++ {
		pos, err := v.Broker.GetPosition(ctx, broker.MarketFuturesUSDM, symbol)
		switch {
		case err == nil && (math.IsNaN(pos.QtyCoin) || math.IsInf(pos.QtyCoin, 0)):
			err = fmt.Errorf("khối lượng %v không phải số", pos.QtyCoin)
		case err == nil && pos.Symbol != "" && pos.Symbol != symbol:
			err = fmt.Errorf("hỏi %s, sàn trả vị thế của %s", symbol, pos.Symbol)
		case err == nil:
			return pos.QtyCoin, nil
		case errors.Is(err, broker.ErrIPCoolingDown):
			return 0, err
		}
		if i >= polls || x.cfg.Now().After(deadline) || x.sleep(ctx, x.cfg.PollEvery) != nil {
			return 0, err
		}
	}
}

// awaitHeldAtLeast waits, bounded, for a leg's position to show at least what its
// opening order is known to have filled, so a position that trails the fill is
// not read as "nothing to flatten".
func (x *Executor) awaitHeldAtLeast(ctx context.Context, v Venue, symbol string, sign, filledQtyCoin float64) {
	if filledQtyCoin <= 0 {
		return
	}
	deadline := x.cfg.Now().Add(x.cfg.PositionSettleTimeout)
	polls := x.pollBudget(x.cfg.PositionSettleTimeout)
	for i := 0; ; i++ {
		pos, err := v.Broker.GetPosition(ctx, broker.MarketFuturesUSDM, symbol)
		if err == nil && sign*pos.QtyCoin >= filledQtyCoin-gridEpsilon*math.Max(1, filledQtyCoin) {
			return
		}
		if errors.Is(err, broker.ErrIPCoolingDown) || i >= polls || x.cfg.Now().After(deadline) || x.sleep(ctx, x.cfg.PollEvery) != nil {
			return
		}
	}
}

// awaitReduced waits, bounded, for a leg's position to show a reducing fill: at
// most wantHeldQtyCoin. Toward ZERO it also returns as soon as the position has
// moved at all from heldBeforeQtyCoin — a still-working opening order that fills
// during the flatten makes "held − filled" unreachable, and waiting for it would
// leave that fill naked for the whole PositionSettleTimeout (review 4.5k round 3,
// M4); the caller re-reads and re-sizes, and reduce-only cannot overshoot zero.
// Toward a size above zero it waits for the full reduction, because re-sizing a
// cut on a position that shows only part of a fill cuts below the target.
func (x *Executor) awaitReduced(ctx context.Context, v Venue, symbol string, sign, heldBeforeQtyCoin, wantHeldQtyCoin, targetQtyCoin float64) {
	deadline := x.cfg.Now().Add(x.cfg.PositionSettleTimeout)
	polls := x.pollBudget(x.cfg.PositionSettleTimeout)
	for i := 0; ; i++ {
		pos, err := v.Broker.GetPosition(ctx, broker.MarketFuturesUSDM, symbol)
		if err == nil {
			held := sign * pos.QtyCoin
			tolerance := gridEpsilon * math.Max(1, math.Abs(heldBeforeQtyCoin))
			if held <= wantHeldQtyCoin+gridEpsilon*math.Max(1, wantHeldQtyCoin) || targetQtyCoin == 0 && math.Abs(held-heldBeforeQtyCoin) > tolerance {
				return
			}
		}
		if errors.Is(err, broker.ErrIPCoolingDown) || i >= polls || x.cfg.Now().After(deadline) || x.sleep(ctx, x.cfg.PollEvery) != nil {
			return
		}
	}
}

// watchLegToZero drives one leg to zero now and, until asked is closed, re-reads
// its position every PollEvery and drives it to zero again whenever it is not: a
// lost opening order that reaches the venue and fills while it is still being
// asked about is taken back at once, not when the asking ends (review 4.5k round
// 3, M2). Every later pass that sends uses round ids of its own. A nil asked is a
// single pass.
func (x *Executor) watchLegToZero(ctx context.Context, intentID, symbol string, leg execution.LegName, spec LegSpec,
	filledQtyCoin float64, attempt string, deadline time.Time, pending *pendingSet, asked <-chan struct{}) legDrive {

	sign := 1.0
	if leg == LegShort {
		sign = -1
	}
	x.awaitHeldAtLeast(ctx, spec.Venue, symbol, sign, filledQtyCoin)
	total := x.driveLegTo(ctx, intentID, symbol, leg, spec, 0, PurposeUnwind, attempt, deadline, pending)
	if asked == nil {
		return total
	}
	for pass := 1; ; pass++ {
		t := time.NewTimer(x.cfg.PollEvery)
		select {
		case <-asked:
			t.Stop()
			return total
		case <-ctx.Done():
			t.Stop()
			return total
		case <-t.C:
		}
		if x.cfg.Now().After(deadline) {
			// This close-out's budget is spent; the last pass has one of its own.
			return total
		}
		pos, err := spec.Venue.Broker.GetPosition(ctx, broker.MarketFuturesUSDM, symbol)
		if err != nil || pos.QtyCoin == 0 {
			continue
		}
		more := x.driveLegTo(ctx, intentID, symbol, leg, spec, 0, PurposeUnwind, attempt+"-w"+strconv.Itoa(pass), deadline, pending)
		total.closedQtyCoin += more.closedQtyCoin
		total.rounds += more.rounds
		total.unresolvedOrders += more.unresolvedOrders
		if more.firstFillAtMs > 0 && total.firstFillAtMs == 0 {
			total.firstFillAtMs = more.firstFillAtMs
		}
		if more.lastFillAtMs > 0 {
			total.lastFillAtMs, total.avgFillPriceQuote = more.lastFillAtMs, more.avgFillPriceQuote
		}
		if more.positionRead {
			total.finalQtyCoin, total.positionRead = more.finalQtyCoin, true
		}
		total.err = more.err
	}
}

// sleep waits, or returns when the context dies.
func (x *Executor) sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// sleepOrStop waits one poll. It reports true when the context died or stop was
// closed instead.
func (x *Executor) sleepOrStop(ctx context.Context, stop <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-stop:
		return true
	case <-t.C:
		return false
	}
}

func (x *Executor) record(ctx context.Context, ev execution.Event) {
	if ev.At.IsZero() {
		ev.At = x.cfg.Now()
	}
	// A recorder that fails must not change what the machine does.
	_ = x.rec.Record(ctx, ev)
}
