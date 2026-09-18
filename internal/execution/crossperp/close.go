package crossperp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/coordinator"
	"futures-arbitrage-scanner/internal/execution"
)

// CloseRequest is one decision to close one pair. What is closed is what the
// VENUES hold, read at the start and every round (rule 7) — the request names
// the pair, not a size.
type CloseRequest struct {
	IntentID string
	Symbol   string

	// Long and Short are the pair's venues with CURRENT rules; the books are the
	// reference price for rounding and for the hedge test's minimum notional,
	// never a cap.
	Long  LegSpec
	Short LegSpec

	// FirstVenue is the venue whose leg closes FIRST — the margin guard names the
	// venue under stress. Empty closes the larger leg first (long on a tie).
	FirstVenue string

	// OpeningOrdersUnproven: an opening order of this intent was never proven
	// finished — its send lost its answer, its cancel was never read back, or the
	// lock came back from a file under which orders had been sent. Close sends
	// cancels for both opening orders by their derived ids before it reduces
	// anything, reads them back before its verdict, closes whatever a late fill of
	// theirs opened meanwhile, and keeps a flat verdict loud while either is
	// unproven: flat venues beside an order that may still fill are not a closed
	// pair (review 4.5k, N3).
	OpeningOrdersUnproven bool

	// PendingOrders are reduce-only orders an earlier call could not prove
	// finished. Close reads them back before a flat verdict, which stays loud
	// while any is still pending (review 4.5k round 3, M1).
	PendingOrders []PendingOrder

	ReasonVI string
}

// CloseResult is one Close.
type CloseResult struct {
	IntentID string
	Symbol   string

	// Outcome is what the venues' positions read at the end: OutcomeBothFlat when
	// nothing of the pair is left, OutcomeBothOpen when the pair is still hedged
	// at a smaller size (with ErrCloseIncomplete), OutcomeUnresolved when the legs
	// are no longer a hedge or could not be read. Whether that reading is FINAL is
	// the error's to say: a loud one means an order may still change it. EMPTY
	// when the close was refused before it read the venues.
	Outcome Outcome

	// AlreadyFlat: both venues read flat before anything was sent.
	AlreadyFlat bool

	Long  LegResult
	Short LegResult

	// The venues' positions before anything was sent, SIGNED. The positions at
	// the end are on Long/Short.VenuePositionQtyCoin.
	LongBeforeQtyCoin, ShortBeforeQtyCoin float64

	// FirstLeg is the leg that closed first.
	FirstLeg execution.LegName

	// OpeningOrdersProven: both opening orders were read back from the venues as
	// finished during this call. Only meaningful when the request said they were
	// unproven.
	OpeningOrdersProven bool

	// PendingOrders are the reduce-only orders — the request's and this call's —
	// still not proven finished when Close returned.
	PendingOrders []PendingOrder

	// RestingOrders is how many orders on the symbol the venues still showed
	// working at the end; -1 when a venue's list could not be read.
	RestingOrders int

	// UnhedgedWindow is how long the two legs were of DIFFERENT sizes during the
	// close as this process saw it: from a first leg's first reducing fill to the
	// second leg's last, summed over the close's passes. Closing one leg after the
	// other makes the whole first leg's close unhedged, where two legs at once
	// leave only the gap between two fills — the price of the ordering (review
	// 4.5k, m-j). On a fake broker this is microseconds and says nothing about a
	// venue.
	UnhedgedWindow time.Duration

	Rounds       int
	Duration     time.Duration
	AttemptNonce string
	ReasonVI     string
}

// ErrCloseIncomplete: the pair is still hedged, smaller than it was. The
// invariant holds and the position is still there.
var ErrCloseIncomplete = fmt.Errorf("crossperp: đóng chưa xong — cặp vẫn phòng hộ ở cỡ nhỏ hơn, phải đóng nốt (%w)", execution.ErrCloseIncomplete)

// Close closes the pair ONE LEG AT A TIME with reduce-only MARKET orders — the
// first leg to zero, the second to what the first really left (flattenPair) —
// and returns having proven both flat and quiet, or says what is left.
//
// Not both at once, although the design asked for that (review 4.5k, M2): two
// legs closed together leave ONE naked leg whenever the venue that fails is
// either of them; in series, a failing FIRST venue leaves the pair whole, and
// only a SECOND venue failing after the first closed leaves one leg — loud, and
// no worse than two at once. The design's own emergency rule — close on the venue
// with the higher maintenance ratio first — is the same ordering: FirstVenue. What
// the ordering costs is time unhedged (CloseResult.UnhedgedWindow).
//
// The verdict is loud — OutcomeUnresolved with ErrLegAmbiguous — whenever
// something that may still execute stands beside the positions it reads: a
// reduce-only order not proven finished (next to open legs two equal legs are
// not a hedge if one may still shrink, review 4.5k N1; next to flat legs it would
// reduce whatever the venue holds when it executes — another engine's position
// once the lock is gone, round 3 M1), an opening order not proven finished, or
// any order still resting on the symbol.
func (x *Executor) Close(ctx context.Context, req CloseRequest) (CloseResult, error) {
	res := CloseResult{IntentID: req.IntentID, Symbol: req.Symbol, AttemptNonce: newAttemptNonce(),
		Long:  LegResult{Leg: LegLong, Venue: req.Long.Venue.Name, Side: broker.SideSell},
		Short: LegResult{Leg: LegShort, Venue: req.Short.Venue.Name, Side: broker.SideBuy},
	}
	startedAt := x.cfg.Now()
	x.record(ctx, execution.Event{IntentID: req.IntentID, Kind: execution.EventCloseStarted, DetailVI: req.ReasonVI})

	switch {
	case req.IntentID == "" || req.Symbol == "":
		return res, fmt.Errorf("%w: không có id ý định hoặc symbol", ErrCloseRefused)
	case req.Long.Venue.Broker == nil || req.Short.Venue.Broker == nil || req.Long.Venue.Name == "" || req.Short.Venue.Name == "":
		return res, fmt.Errorf("%w: một chân thiếu sàn", ErrCloseRefused)
	case req.Long.Venue.Name == req.Short.Venue.Name:
		return res, fmt.Errorf("%w: hai chân cùng sàn %q", ErrCloseRefused, req.Long.Venue.Name)
	case req.Long.Rules.Symbol != req.Symbol || req.Short.Rules.Symbol != req.Symbol:
		return res, fmt.Errorf("%w: quy tắc sàn là của %q/%q, cặp là %q", ErrCloseRefused, req.Long.Rules.Symbol, req.Short.Rules.Symbol, req.Symbol)
	case req.FirstVenue != "" && req.FirstVenue != req.Long.Venue.Name && req.FirstVenue != req.Short.Venue.Name:
		return res, fmt.Errorf("%w: sàn đóng trước %q không phải một chân của cặp", ErrCloseRefused, req.FirstVenue)
	case !x.locks.Holds(req.Symbol, coordinator.EngineCrossPerp, req.IntentID):
		// Engine 2 closes only what it holds: a close sent on a symbol another
		// engine owns trades THAT engine's position.
		return res, fmt.Errorf("%w: Động cơ 2 không giữ khóa %s cho ý định %q", ErrCloseRefused, req.Symbol, req.IntentID)
	}

	pending, carried := &pendingSet{}, pendingFromRequest(req)
	for _, p := range carried.readable {
		pending.add(p)
	}
	// Every return from here on carries at least the orders the request handed
	// over: a close refused before its verdict must not report an empty list and
	// let its caller forget what is still pending (self-review, round 4).
	res.PendingOrders = append(exportPending(carried.readable), carried.unreadable...)

	// Cancels for the opening orders go out first, so none keeps working while
	// the close reads and sends — and the close does not wait for their answers:
	// Bybit's client confirms a cancel for up to three seconds, and reading the
	// orders back can take OrderSettleTimeout (review 4.5k round 3, minor 1).
	// Close returns only after both cancels answered.
	res.OpeningOrdersProven = !req.OpeningOrdersUnproven
	var cancelsDone <-chan struct{}
	if req.OpeningOrdersUnproven {
		cancelsDone = x.cancelOpeningOrders(ctx, req)
		defer func() { <-cancelsDone }()
	}

	before := x.readBoth(ctx, req.Symbol, req.Long.Venue, req.Short.Venue, false)
	beforeVI := before.evidenceVI(req.Long.Venue.Name, req.Short.Venue.Name)
	if !before.read() {
		return res, fmt.Errorf("%w: không đọc được vị thế sàn — %s", ErrCloseRefused, beforeVI)
	}
	res.LongBeforeQtyCoin, res.ShortBeforeQtyCoin = before.longQtyCoin, before.shortQtyCoin
	if before.longQtyCoin < 0 || before.shortQtyCoin > 0 {
		res.Outcome = OutcomeUnresolved
		return res, fmt.Errorf("%w: %s — chân long phải ≥ 0, chân short phải ≤ 0", ErrPositionDisagrees, beforeVI)
	}
	res.AlreadyFlat = before.longQtyCoin == 0 && before.shortQtyCoin == 0

	var driveErr error
	flatten := func(attempt string, from pairPositions) {
		first := LegLong
		switch {
		case req.FirstVenue == req.Short.Venue.Name:
			first = LegShort
		case req.FirstVenue == "" && -from.shortQtyCoin > from.longQtyCoin:
			first = LegShort
		}
		if res.FirstLeg == "" {
			res.FirstLeg = first
		}
		longDrive, shortDrive, err := x.flattenPair(ctx, req.IntentID, req.Symbol, req.Long, req.Short, first, attempt, PurposeClose, pending)
		driveErr = errors.Join(driveErr, err)
		for _, d := range []struct {
			drive legDrive
			into  *LegResult
		}{{longDrive, &res.Long}, {shortDrive, &res.Short}} {
			d.into.ClosedQtyCoin += d.drive.closedQtyCoin
			if d.drive.lastClientOrderID != "" {
				d.into.ClientOrderID, d.into.VenueOrderID, d.into.Status = d.drive.lastClientOrderID, d.drive.lastVenueOrderID, d.drive.lastStatus
			}
			if d.drive.lastFillAtMs > 0 {
				d.into.AvgFillPriceQuote, d.into.FilledAtMs = d.drive.avgFillPriceQuote, d.drive.lastFillAtMs
			}
		}
		res.Rounds += max(longDrive.rounds, shortDrive.rounds)
		firstDrive, secondDrive := longDrive, shortDrive
		if first == LegShort {
			firstDrive, secondDrive = shortDrive, longDrive
		}
		if firstDrive.firstFillAtMs > 0 {
			end := secondDrive.lastFillAtMs
			if end == 0 {
				end = x.cfg.Now().UnixMilli() // the second leg never moved: still unhedged
			}
			res.UnhedgedWindow += absDuration(end - firstDrive.firstFillAtMs)
		}
	}
	if !res.AlreadyFlat {
		flatten(res.AttemptNonce, before)
	}

	// The verdict reads the venues on a budget of its own (review 4.5k, M1).
	finalCtx, cancelFinal, _ := x.closeOutContext(ctx)
	defer cancelFinal()
	if req.OpeningOrdersUnproven {
		<-cancelsDone
		res.OpeningOrdersProven = x.readBackOpeningOrders(finalCtx, req)
		// A late fill of an opening order — proven by that read-back, or still
		// possible — shows in the positions now. A close closes it too, once,
		// instead of reading it and leaving it naked (review 4.5k round 3, M5).
		again := x.readBoth(finalCtx, req.Symbol, req.Long.Venue, req.Short.Venue, false)
		if again.read() && again.longQtyCoin >= 0 && again.shortQtyCoin <= 0 && (again.longQtyCoin > 0 || again.shortQtyCoin < 0) {
			flatten(res.AttemptNonce+"-2", again)
		}
	}
	after := before
	if !res.AlreadyFlat || req.OpeningOrdersUnproven {
		after, _ = x.awaitPositions(finalCtx, req.Symbol, req.Long.Venue, req.Short.Venue, 0, 0)
	}
	still := pending.list()
	if after.read() && after.longQtyCoin == 0 && after.shortQtyCoin == 0 && len(still) > 0 {
		// Flat, so waiting costs no exposure: prove the reductions finished or
		// absent before a flat verdict can release the lock.
		still = x.settleReductions(finalCtx, req.IntentID, still)
		pending.replace(still)
	}
	quiet := x.readBoth(finalCtx, req.Symbol, req.Long.Venue, req.Short.Venue, true)
	if quiet.read() {
		after.longQtyCoin, after.shortQtyCoin, after.longErr, after.shortErr = quiet.longQtyCoin, quiet.shortQtyCoin, nil, nil
	} else {
		after.longErr, after.shortErr = errors.Join(after.longErr, quiet.longErr), errors.Join(after.shortErr, quiet.shortErr)
	}
	after.longOrders, after.shortOrders, after.longOrdersErr, after.shortOrdersErr = quiet.longOrders, quiet.shortOrders, quiet.longOrdersErr, quiet.shortOrdersErr
	res.RestingOrders = after.longOrders + after.shortOrders
	ordersRead := after.longOrdersErr == nil && after.shortOrdersErr == nil
	if !ordersRead {
		res.RestingOrders = -1
	}
	res.PendingOrders = append(exportPending(still), carried.unreadable...)
	res.Long.VenuePositionRead, res.Short.VenuePositionRead = after.longErr == nil, after.shortErr == nil
	res.Long.VenuePositionQtyCoin, res.Short.VenuePositionQtyCoin = after.longQtyCoin, after.shortQtyCoin
	res.Duration = x.cfg.Now().Sub(startedAt)
	afterVI := after.evidenceVI(req.Long.Venue.Name, req.Short.Venue.Name)

	// What may still execute beside those positions.
	var loud []string
	if n := len(res.PendingOrders); n > 0 {
		loud = append(loud, fmt.Sprintf("%d lệnh giảm vị thế chưa chứng minh được là đã kết thúc", n))
	}
	if !res.OpeningOrdersProven {
		loud = append(loud, "lệnh MỞ của ý định này chưa chứng minh được là đã kết thúc")
	}
	if res.RestingOrders > 0 {
		loud = append(loud, fmt.Sprintf("%d lệnh còn treo trên symbol", res.RestingOrders))
	}
	if !ordersRead {
		loud = append(loud, "không đọc được danh sách lệnh treo")
	}
	loudErr := func() error {
		return fmt.Errorf("%w: %s — %s; %v", ErrLegAmbiguous, strings.Join(loud, "; "), afterVI, driveErr)
	}

	var outErr error
	flat := after.longQtyCoin == 0 && after.shortQtyCoin == 0
	tolerance := math.Max(req.Long.Rules.StepSizeCoin, req.Short.Rules.StepSizeCoin)
	switch {
	case !after.read():
		res.Outcome = OutcomeUnresolved
		outErr = fmt.Errorf("%w: không đọc được vị thế sau khi đóng — %s; %v", execution.ErrUnwindIncomplete, afterVI, driveErr)
	case flat && len(loud) > 0:
		res.Outcome = OutcomeUnresolved
		outErr = loudErr()
	case flat:
		res.Outcome = OutcomeBothFlat
	case after.longQtyCoin > 0 && after.shortQtyCoin < 0 && hedgedWithin(req.Long, req.Short, tolerance, after.longQtyCoin, -after.shortQtyCoin):
		// The executor's and Adopt's one criterion, minimum notional included
		// (review 4.5k round 3, minor 2).
		if len(loud) > 0 {
			res.Outcome = OutcomeUnresolved
			outErr = loudErr()
			break
		}
		res.Outcome = OutcomeBothOpen
		outErr = fmt.Errorf("%w: %s; %v", ErrCloseIncomplete, afterVI, driveErr)
	default:
		// One leg, or two legs no longer a hedge. Nothing re-hedges it (doc.go):
		// it is named, with both venues' numbers.
		res.Outcome = OutcomeUnresolved
		outErr = fmt.Errorf("%w: sau khi đóng hai chân không còn là một cặp phòng hộ — %s; %v", execution.ErrUnwindIncomplete, afterVI, driveErr)
		if len(loud) > 0 {
			outErr = errors.Join(outErr, loudErr())
		}
	}
	switch {
	case outErr != nil:
		res.ReasonVI = joinVI(req.ReasonVI, outErr.Error())
	case res.AlreadyFlat && res.Rounds == 0:
		res.ReasonVI = joinVI(req.ReasonVI, "cả hai sàn đã phẳng, không lệnh nào treo — không gửi lệnh giảm nào")
	default:
		res.ReasonVI = joinVI(req.ReasonVI, "đã đóng phẳng cả hai chân — "+afterVI)
	}
	x.record(finalCtx, execution.Event{IntentID: req.IntentID, Kind: execution.EventCloseDone, Outcome: execution.Outcome(res.Outcome),
		Err: outErr, DetailVI: fmt.Sprintf("mất %s, %d vòng, chân %s trước, lệch cỡ %s: %s", res.Duration, res.Rounds, res.FirstLeg, res.UnhedgedWindow, afterVI)})
	return res, outErr
}

// requestPending is a request's pending orders: the ones on one of the pair's two
// venues, which can be read back, and any other, which cannot and stays pending.
type requestPending struct {
	readable   []pendingOrder
	unreadable []PendingOrder
}

func pendingFromRequest(req CloseRequest) requestPending {
	var out requestPending
	for _, p := range req.PendingOrders {
		var v Venue
		switch p.Venue {
		case req.Long.Venue.Name:
			v = req.Long.Venue
		case req.Short.Venue.Name:
			v = req.Short.Venue
		default:
			out.unreadable = append(out.unreadable, p)
			continue
		}
		out.readable = append(out.readable, pendingOrder{leg: p.Leg, venue: v,
			q:          broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: req.Symbol, ClientOrderID: p.ClientOrderID},
			returnedAt: time.UnixMilli(p.SendReturnedAtMs), seen: p.Seen, statusUnknown: p.StatusUnknown})
	}
	return out
}

// openingQueries are the intent's two opening orders, by their derived ids.
func openingQueries(req CloseRequest) [2]struct {
	leg  execution.LegName
	spec LegSpec
	q    broker.OrderQuery
} {
	query := func(leg execution.LegName) broker.OrderQuery {
		return broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: req.Symbol, ClientOrderID: ClientOrderID(req.IntentID, PurposeOpen, leg, "", 0)}
	}
	return [2]struct {
		leg  execution.LegName
		spec LegSpec
		q    broker.OrderQuery
	}{{LegLong, req.Long, query(LegLong)}, {LegShort, req.Short, query(LegShort)}}
}

// cancelOpeningOrders sends a cancel for both of the intent's opening orders,
// both venues at once, and returns without waiting: the channel closes when both
// have answered.
func (x *Executor) cancelOpeningOrders(ctx context.Context, req CloseRequest) <-chan struct{} {
	safe := context.WithoutCancel(ctx)
	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, l := range openingQueries(req) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := l.spec.Venue.Broker.CancelOrder(safe, l.q)
			x.record(safe, execution.Event{IntentID: req.IntentID, Kind: execution.EventLegCancelling, Leg: l.leg, ClientOrderID: l.q.ClientOrderID, Err: err,
				DetailVI: "huỷ lệnh MỞ trước khi đóng trên " + l.spec.Venue.Name})
		}()
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

// readBackOpeningOrders reads both of the intent's opening orders back, both
// venues at once. It reports whether BOTH were read back finished.
func (x *Executor) readBackOpeningOrders(ctx context.Context, req CloseRequest) bool {
	var ok [2]bool
	var wg sync.WaitGroup
	for i, l := range openingQueries(req) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, proven := x.readBackOpeningOrder(ctx, l.spec.Venue, l.q)
			ok[i] = proven
			detail := fmt.Sprintf("lệnh mở trên %s: %s, khớp %.10g", l.spec.Venue.Name, o.Status, o.FilledQtyCoin)
			if !proven {
				detail = fmt.Sprintf("lệnh mở trên %s CHƯA chứng minh được là đã kết thúc (lần cuối thấy %q)", l.spec.Venue.Name, o.Status)
			}
			x.record(ctx, execution.Event{IntentID: req.IntentID, Kind: execution.EventLegReadBack, Leg: l.leg, ClientOrderID: l.q.ClientOrderID,
				FilledQtyCoin: o.FilledQtyCoin, DetailVI: detail})
		}()
	}
	wg.Wait()
	return ok[0] && ok[1]
}
