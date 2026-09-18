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
	"futures-arbitrage-scanner/internal/risk"
	"futures-arbitrage-scanner/internal/strategy"
)

// Executor runs Engine 2's opens and closes against two venues. It holds no
// state between calls, so it is safe for concurrent use on DIFFERENT symbols;
// one symbol is one caller at a time (Engine serialises that), because two
// calls on one intent would send the same opening ClientOrderIDs twice.
type Executor struct {
	cfg   Config
	locks LockHolder
	gate  EntryGate
	rec   execution.Recorder
}

// NewExecutor wires the executor. The lock holder and the margin gate are
// REQUIRED: an Engine-2 open that nobody checked against the other engine and
// the account's margin is exactly the open this package exists to refuse.
func NewExecutor(cfg Config, locks LockHolder, gate EntryGate, rec execution.Recorder) (*Executor, error) {
	if locks == nil {
		return nil, errors.New("crossperp: no lock holder — Engine 2 may not open a symbol nobody locked")
	}
	if gate == nil {
		return nil, errors.New("crossperp: no margin gate — Engine 2 may not open on margin nobody read")
	}
	d := DefaultConfig()
	for _, f := range []struct {
		v   *time.Duration
		def time.Duration
	}{
		{&cfg.LegTimeout, d.LegTimeout}, {&cfg.OrderSettleTimeout, d.OrderSettleTimeout},
		{&cfg.PositionSettleTimeout, d.PositionSettleTimeout}, {&cfg.UnwindTimeout, d.UnwindTimeout},
		{&cfg.PollEvery, d.PollEvery}, {&cfg.MaxBookAge, d.MaxBookAge},
		{&cfg.AmbiguousSendQuiet, d.AmbiguousSendQuiet},
	} {
		if *f.v <= 0 {
			*f.v = f.def
		}
	}
	if cfg.MaxCloseRounds <= 0 {
		cfg.MaxCloseRounds = d.MaxCloseRounds
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	// Zero is a real setting for both; negative is refused, never clamped.
	if cfg.MaxSlippageBps < 0 || math.IsNaN(cfg.MaxSlippageBps) {
		return nil, fmt.Errorf("crossperp: MaxSlippageBps %v prices a buy below the touch", cfg.MaxSlippageBps)
	}
	if math.IsNaN(cfg.MaxEntryCostWidenBps) {
		return nil, errors.New("crossperp: MaxEntryCostWidenBps is NaN")
	}
	if rec == nil {
		rec = nopRecorder{}
	}
	return &Executor{cfg: cfg, locks: locks, gate: gate, rec: rec}, nil
}

type nopRecorder struct{}

func (nopRecorder) Record(context.Context, execution.Event) error { return nil }

// closeOutContext is a fresh budget for one close-out phase. Each phase gets its
// OWN: a slow reduction must not spend the time the mandatory unwind after it
// needs (review 4.5k, M1), and a caller that gave up is exactly the caller who
// must not be left holding one leg.
func (x *Executor) closeOutContext(ctx context.Context) (context.Context, context.CancelFunc, time.Time) {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), x.cfg.UnwindTimeout)
	return c, cancel, x.cfg.Now().Add(x.cfg.UnwindTimeout)
}

// Open opens one pair and returns having satisfied the invariant, or with a
// loud error saying it could not (doc.go). The error is non-nil for every
// outcome that is not a clean hedge. Result.Outcome says what the venues hold;
// a loud error says that may still change (Outcome).
func (x *Executor) Open(ctx context.Context, intent Intent) (Result, error) {
	res := Result{IntentID: intent.ID, Symbol: intent.Symbol, Outcome: OutcomeBothFlat, AttemptNonce: newAttemptNonce(),
		Long:  LegResult{Leg: LegLong, Venue: intent.Long.Venue.Name, Side: broker.SideBuy, ClientOrderID: ClientOrderID(intent.ID, PurposeOpen, LegLong, "", 0)},
		Short: LegResult{Leg: LegShort, Venue: intent.Short.Venue.Name, Side: broker.SideSell, ClientOrderID: ClientOrderID(intent.ID, PurposeOpen, LegShort, "", 0)},
	}
	x.record(ctx, execution.Event{IntentID: intent.ID, Kind: execution.EventIntentReceived, QtyCoin: intent.NotionalQuote})
	refuse := func(err error) (Result, error) {
		res.ReasonVI = err.Error()
		x.record(ctx, execution.Event{IntentID: intent.ID, Kind: execution.EventRefused, Err: err, DetailVI: err.Error()})
		x.record(ctx, execution.Event{IntentID: intent.ID, Kind: execution.EventResolved, Outcome: execution.Outcome(OutcomeBothFlat)})
		return res, err
	}

	if err := validateIntent(intent); err != nil {
		return refuse(err)
	}
	if !x.locks.Holds(intent.Symbol, coordinator.EngineCrossPerp, intent.ID) {
		return refuse(fmt.Errorf("%w: %s / ý định %q", ErrLockNotHeld, intent.Symbol, intent.ID))
	}
	if err := x.gate.AllowOpen(risk.OpenRequest{Engine: string(coordinator.EngineCrossPerp), CrossVenuePerp: true,
		Venues: []string{intent.Long.Venue.Name, intent.Short.Venue.Name}}); err != nil {
		return refuse(fmt.Errorf("%w: %v", ErrMarginGate, err))
	}
	plan, err := planOpen(intent, x.cfg)
	res.TargetQtyCoin, res.CommonStepCoin, res.ToleranceQtyCoin = plan.QtyCoin, plan.CommonStepCoin, plan.ToleranceQtyCoin
	res.RefMidQuote, res.BookAgeMs, res.EntryCostPct, res.WidenBps = plan.RefMidQuote, plan.BookAgeMs, plan.EntryCostPct, plan.WidenBps
	if err != nil {
		return refuse(err)
	}
	x.record(ctx, execution.Event{IntentID: intent.ID, Kind: execution.EventSized, QtyCoin: plan.QtyCoin,
		DetailVI: fmt.Sprintf("bước chung %v, chi phí vào %.4f%%, sổ cũ %d ms", plan.CommonStepCoin, plan.EntryCostPct, plan.BookAgeMs)})

	// Both venues, immediately before sending (rule 7): no position and no
	// resting order on the symbol. It narrows, and does not close, the race with
	// an Engine 1 that bypasses the coordinator (coordinator/doc.go).
	pre := x.readBoth(ctx, intent.Symbol, intent.Long.Venue, intent.Short.Venue, true)
	preVI := pre.evidenceVI(intent.Long.Venue.Name, intent.Short.Venue.Name)
	switch {
	case !pre.read() || pre.longOrdersErr != nil || pre.shortOrdersErr != nil:
		return refuse(fmt.Errorf("%w: %s", ErrVenueUnreadable, preVI))
	case pre.longQtyCoin != 0 || pre.shortQtyCoin != 0 || pre.longOrders > 0 || pre.shortOrders > 0:
		return refuse(fmt.Errorf("%w: %s", ErrVenueNotFlat, preVI))
	}
	// On disk before the first order: from here the lock cannot be withdrawn
	// without reading the venues (coordinator.MarkOrdersSent).
	if err := x.locks.MarkOrdersSent(intent.Symbol, coordinator.EngineCrossPerp, intent.ID); err != nil {
		return refuse(fmt.Errorf("%w: %v", ErrOrdersNotMarked, err))
	}

	longReq := broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: intent.Symbol, Side: broker.SideBuy,
		Type: broker.OrderTypeLimitGTC, ClientOrderID: res.Long.ClientOrderID,
		QtyCoin: plan.LongOrder.QtyCoin, PriceQuote: plan.LongOrder.PriceQuote}
	shortReq := broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: intent.Symbol, Side: broker.SideSell,
		Type: broker.OrderTypeLimitGTC, ClientOrderID: res.Short.ClientOrderID,
		QtyCoin: plan.ShortOrder.QtyCoin, PriceQuote: plan.ShortOrder.PriceQuote}
	res.Long.LimitPriceQuote, res.Short.LimitPriceQuote = longReq.PriceQuote, shortReq.PriceQuote

	// Both legs at once. A leg refused outright, or whose send lost its answer,
	// stops the other from working its marketable limit for the rest of
	// LegTimeout.
	stop := make(chan struct{})
	var once sync.Once
	abort := func() { once.Do(func() { close(stop) }) }
	var longRun, shortRun legRun
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		longRun = x.workLeg(ctx, intent.ID, LegLong, intent.Long.Venue, longReq, stop, abort)
	}()
	go func() {
		defer wg.Done()
		shortRun = x.workLeg(ctx, intent.ID, LegShort, intent.Short.Venue, shortReq, stop, abort)
	}()
	wg.Wait()
	res.Long, res.Short = mergeRun(res.Long, longRun), mergeRun(res.Short, shortRun)
	if longRun.filledAtMs > 0 && shortRun.filledAtMs > 0 {
		res.UnhedgedWindow = absDuration(longRun.filledAtMs - shortRun.filledAtMs)
	}
	pending := &pendingSet{}
	if !longRun.confirmed || !shortRun.confirmed {
		return x.settleAbortedOpen(ctx, intent, res, longRun, shortRun, pending)
	}
	return x.settleOpen(ctx, intent, plan, res, longRun, shortRun, pending)
}

// settleAbortedOpen is the open in which an opening order was NOT proven
// finished when both legs stopped: its send lost its answer, it was stopped
// because the other leg was refused or lost, or its cancel was never read back as
// done. Nothing is kept beside an order that may still fill, and a leg whose
// partner was refused or cut off is taken back to flat at once. So, in parallel:
//
//   - each unproven order is cancelled and read back (confirmLeg), for as long as
//     the quiet period after a lost send allows;
//   - each leg is driven to zero NOW, and a leg whose order is still being asked
//     about is re-read every PollEvery and driven to zero again whenever it holds
//     something — a lost order that reaches the venue and fills during the asking
//     is taken back at once (review 4.5k round 3, M2).
//
// When the asking is over both legs are driven to zero once more under ids of
// their own. The flat verdict is loud while any opening order is still unproven
// or any reduce-only order is still pending.
//
// Both legs at once rather than one after the other: this is not a hedge being
// closed but a leg that may already be naked, and reduce-only toward zero can
// neither open nor flip anything, so there is no half-state to protect by order.
func (x *Executor) settleAbortedOpen(ctx context.Context, intent Intent, res Result, longRun, shortRun legRun, pending *pendingSet) (Result, error) {
	startedAt := x.cfg.Now()
	x.record(ctx, execution.Event{IntentID: intent.ID, Kind: execution.EventUnwindStarted,
		DetailVI: "một lệnh mở chưa chứng minh được là đã kết thúc — gỡ cả hai chân NGAY, vừa gỡ vừa hỏi sàn về lệnh đó"})
	longFilled, shortFilled := longRun.order.FilledQtyCoin, shortRun.order.FilledQtyCoin
	var longAsked, shortAsked chan struct{}
	if !longRun.confirmed {
		longAsked = make(chan struct{})
	}
	if !shortRun.confirmed {
		shortAsked = make(chan struct{})
	}

	flatCtx, cancelFlat, flatDeadline := x.closeOutContext(ctx)
	defer cancelFlat()
	var longDrive, shortDrive legDrive
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		if longAsked != nil {
			defer close(longAsked)
			longRun = x.confirmLeg(ctx, intent.ID, LegLong, intent.Long.Venue, openingQuery(intent.Symbol, longRun), longRun)
		}
	}()
	go func() {
		defer wg.Done()
		if shortAsked != nil {
			defer close(shortAsked)
			shortRun = x.confirmLeg(ctx, intent.ID, LegShort, intent.Short.Venue, openingQuery(intent.Symbol, shortRun), shortRun)
		}
	}()
	go func() {
		defer wg.Done()
		longDrive = x.watchLegToZero(flatCtx, intent.ID, intent.Symbol, LegLong, intent.Long, longFilled, res.AttemptNonce, flatDeadline, pending, longAsked)
	}()
	go func() {
		defer wg.Done()
		shortDrive = x.watchLegToZero(flatCtx, intent.ID, intent.Symbol, LegShort, intent.Short, shortFilled, res.AttemptNonce, flatDeadline, pending, shortAsked)
	}()
	wg.Wait()
	firstPass := x.cfg.Now().Sub(startedAt)

	// An order still unproven but last seen WORKING is cancelled once more before
	// the second pass (review 4.5k round 3, B1).
	for _, l := range []struct {
		run legRun
		v   Venue
	}{{longRun, intent.Long.Venue}, {shortRun, intent.Short.Venue}} {
		if !l.run.confirmed && l.run.order.VenueOrderID != "" && !l.run.order.Status.Done() {
			_, _ = l.v.Broker.CancelOrder(context.WithoutCancel(ctx), openingQuery(intent.Symbol, l.run))
		}
	}

	// The second pass, under ids of its own: whatever the unproven orders filled
	// between the last watch and the end of the asking.
	againCtx, cancelAgain, againDeadline := x.closeOutContext(ctx)
	defer cancelAgain()
	var longAgain, shortAgain legDrive
	wg.Add(2)
	go func() {
		defer wg.Done()
		longAgain = x.driveLegTo(againCtx, intent.ID, intent.Symbol, LegLong, intent.Long, 0, PurposeUnwind, res.AttemptNonce+"-2", againDeadline, pending)
	}()
	go func() {
		defer wg.Done()
		shortAgain = x.driveLegTo(againCtx, intent.ID, intent.Symbol, LegShort, intent.Short, 0, PurposeUnwind, res.AttemptNonce+"-2", againDeadline, pending)
	}()
	wg.Wait()

	res.Long, res.Short = mergeRun(res.Long, longRun), mergeRun(res.Short, shortRun)
	res.Long.ClosedQtyCoin += longDrive.closedQtyCoin + longAgain.closedQtyCoin
	res.Short.ClosedQtyCoin += shortDrive.closedQtyCoin + shortAgain.closedQtyCoin
	res.UnwindDuration = x.cfg.Now().Sub(startedAt)
	if firstMs := firstFillAtMs(res); firstMs > 0 && res.UnhedgedWindow == 0 {
		res.UnhedgedWindow = absDuration(x.cfg.Now().UnixMilli() - firstMs)
	}
	x.record(ctx, execution.Event{IntentID: intent.ID, Kind: execution.EventUnwindDone,
		DetailVI: fmt.Sprintf("lượt gỡ đầu (kèm canh) mất %s, tổng %s", firstPass, res.UnwindDuration)})

	finalCtx, cancelFinal, _ := x.closeOutContext(ctx)
	defer cancelFinal()
	confirmErr, still := x.confirmFlatAndSettle(finalCtx, intent, &res, pending)
	driveErr := errors.Join(longDrive.err, shortDrive.err, longAgain.err, shortAgain.err)
	res.EvidenceVI = fmt.Sprintf("LỆNH: long %.10g (%s%s), short %.10g (%s%s); đã gỡ long %.10g, short %.10g",
		res.Long.FilledQtyCoin, res.Long.Status, confirmedVI(longRun.confirmed), res.Short.FilledQtyCoin, res.Short.Status, confirmedVI(shortRun.confirmed),
		res.Long.ClosedQtyCoin, res.Short.ClosedQtyCoin)

	var loud []string
	for _, l := range []struct {
		name string
		run  legRun
	}{{"long", longRun}, {"short", shortRun}} {
		if !l.run.confirmed {
			loud = append(loud, fmt.Sprintf("lệnh mở %s (%v)", l.name, l.run.err))
		}
	}
	if len(still) > 0 {
		loud = append(loud, fmt.Sprintf("%d lệnh giảm vị thế chưa chứng minh được là đã kết thúc", len(still)))
	}
	var loudErr error
	if len(loud) > 0 {
		loudErr = fmt.Errorf("%w: %s — có thể còn khớp muộn; khóa phải được GIỮ", ErrLegAmbiguous, strings.Join(loud, "; "))
	}
	if confirmErr != nil {
		return x.unresolved(ctx, intent, res, errors.Join(loudErr, confirmErr, driveErr))
	}
	res.Outcome = OutcomeBothFlat
	if res.Long.ClosedQtyCoin > 0 || res.Short.ClosedQtyCoin > 0 {
		res.Outcome = OutcomeUnwoundFlat
	}
	if loudErr != nil {
		return x.finish(ctx, intent, res, loudErr)
	}
	reasonVI := joinVI(errVI(longRun.err), errVI(shortRun.err))
	if res.Outcome == OutcomeBothFlat {
		return x.finish(ctx, intent, res, fmt.Errorf("%w: %s", ErrNotOpened, reasonVI))
	}
	return x.finish(ctx, intent, res, fmt.Errorf("%w: %s", ErrOpenUnwound, reasonVI))
}

// confirmFlatAndSettle confirms both venues flat and, when they are, waits for
// every pending reduce-only order to be proven finished or absent
// (settleReductions) and reads the venues once more. A flat pair beside a pending
// order is not released: that order reduces whatever the venue holds when it
// executes (review 4.5k round 3, M1). It returns the confirmation's error and the
// orders still pending, which it also writes into res.
func (x *Executor) confirmFlatAndSettle(ctx context.Context, intent Intent, res *Result, pending *pendingSet) (error, []pendingOrder) {
	confirmErr := x.confirmFlat(ctx, intent, res)
	still := pending.list()
	if confirmErr == nil && len(still) > 0 {
		still = x.settleReductions(ctx, intent.ID, still)
		confirmErr = x.confirmFlat(ctx, intent, res)
	}
	pending.replace(still)
	res.PendingOrders = exportPending(still)
	return confirmErr, still
}

func openingQuery(symbol string, run legRun) broker.OrderQuery {
	return broker.OrderQuery{Market: run.req.Market, Symbol: symbol, ClientOrderID: run.req.ClientOrderID}
}

// settleOpen decides what the two legs amount to, on the venues' evidence, and
// makes it one of the two states. Both opening orders are proven finished here;
// an open with one that is not goes through settleAbortedOpen.
func (x *Executor) settleOpen(ctx context.Context, intent Intent, plan openPlan, res Result, longRun, shortRun legRun, pending *pendingSet) (Result, error) {
	evidenceCtx, cancelEvidence, _ := x.closeOutContext(ctx)
	defer cancelEvidence()
	longV, shortV := intent.Long.Venue, intent.Short.Venue

	// Both venues were flat before sending and the lock keeps every other
	// engine off the symbol, so each venue's position is what these orders did.
	ordersLong, ordersShort := longRun.order.FilledQtyCoin, shortRun.order.FilledQtyCoin
	pos, agreed := x.awaitPositions(evidenceCtx, intent.Symbol, longV, shortV, ordersLong, -ordersShort)
	x.notePositions(&res, pos)
	res.EvidenceVI = fmt.Sprintf("LỆNH: long %.10g (%s%s), short %.10g (%s%s); SÀN: %s",
		ordersLong, longRun.order.Status, confirmedVI(longRun.confirmed), ordersShort, shortRun.order.Status, confirmedVI(shortRun.confirmed),
		pos.evidenceVI(longV.Name, shortV.Name))

	switch {
	case agreed:
	case !pos.read():
		res.EvidenceVI += " — vị thế không đọc được, chỉ có MỘT bằng chứng (lệnh)"
	case pos.longQtyCoin < 0 || pos.shortQtyCoin > 0:
		// A short on the venue this call only bought on, or the reverse: no
		// order of this intent can produce that.
		return x.unresolved(ctx, intent, res, fmt.Errorf("%w: %s — vị thế ngược chiều cặp, không gửi thêm gì", ErrFillEvidenceConflict, res.EvidenceVI))
	default:
		// Both orders are proven finished and the venues disagree with them past
		// the deadline. Nothing more is sent: sizing a close from a position that
		// may be the wrong one can itself open a naked leg (a lagging 0 on one
		// venue closes the other venue's real hedge). Loud, and left to a person
		// (review 4.5k, M4).
		return x.unresolved(ctx, intent, res, fmt.Errorf("%w: %s — quá %s vẫn lệch, không gửi thêm gì", ErrFillEvidenceConflict, res.EvidenceVI, x.cfg.PositionSettleTimeout))
	}
	longHeld, shortHeld := ordersLong, ordersShort
	reasonVI := joinVI(errVI(longRun.err), errVI(shortRun.err))

	switch {
	case longHeld == 0 && shortHeld == 0:
		res.Outcome = OutcomeBothFlat
		return x.finish(ctx, intent, res, fmt.Errorf("%w: %s", ErrNotOpened, reasonVI))

	case longHeld > 0 && shortHeld > 0 && hedgedWithin(intent.Long, intent.Short, plan.ToleranceQtyCoin, longHeld, shortHeld):
		res.Outcome = OutcomeBothOpen
		res.DeltaImbalanceQtyCoin = math.Abs(longHeld - shortHeld)
		if !pos.read() {
			return x.finish(ctx, intent, res, fmt.Errorf("%w: %s", ErrPositionsUnverified, res.EvidenceVI))
		}
		return x.finish(ctx, intent, res, nil)

	case longHeld > 0 && shortHeld > 0:
		newLong, newShort, whyNot := x.reduceToMatch(ctx, intent, plan, &res, longHeld, shortHeld, pending)
		if whyNot == "" {
			res.Outcome, res.ReducedToMatch = OutcomeBothOpen, true
			res.DeltaImbalanceQtyCoin = math.Abs(newLong - newShort)
			return x.finish(ctx, intent, res, nil)
		}
		res, unwindErr := x.unwind(ctx, intent, res, newLong, newShort, pending)
		return x.afterUnwind(ctx, intent, res, unwindErr,
			fmt.Errorf("%w: %s | không thu nhỏ được: %s", ErrOpenUnwound, reasonVI, whyNot), " | không thu nhỏ được: "+whyNot)

	default:
		res, unwindErr := x.unwind(ctx, intent, res, longHeld, shortHeld, pending)
		return x.afterUnwind(ctx, intent, res, unwindErr, fmt.Errorf("%w: %s", ErrOpenUnwound, reasonVI), "")
	}
}

// afterUnwind turns an unwind into the call's verdict: proven flat is flatErr;
// flat venues beside a pending order are loud but still flat; anything else that
// failed is unresolved. unwind sets the Outcome on every path, so a failed unwind
// is never read as the Result's starting OutcomeBothFlat.
func (x *Executor) afterUnwind(ctx context.Context, intent Intent, res Result, unwindErr, flatErr error, detailVI string) (Result, error) {
	switch {
	case unwindErr == nil:
		return x.finish(ctx, intent, res, flatErr)
	case res.Outcome == OutcomeUnwoundFlat || res.Outcome == OutcomeBothFlat:
		return x.finish(ctx, intent, res, errors.Join(unwindErr, flatErr))
	}
	if detailVI != "" {
		unwindErr = fmt.Errorf("%w%s", unwindErr, detailVI)
	}
	return x.unresolved(ctx, intent, res, unwindErr)
}

// hedgedWithin is the BOTH OPEN arm, and the ONE criterion for it — the executor
// keeps a pair on it and Engine.Adopt recovers one on it: two magnitudes within
// toleranceQtyCoin (the coarser step), and a residual too small for either venue
// to trade (internal/execution's two conditions).
func hedgedWithin(long, short LegSpec, toleranceQtyCoin, longQtyCoin, shortQtyCoin float64) bool {
	residual := math.Abs(longQtyCoin - shortQtyCoin)
	if residual > toleranceQtyCoin+gridEpsilon*math.Max(1, toleranceQtyCoin) {
		return false
	}
	if residual == 0 {
		return true
	}
	for _, leg := range []LegSpec{long, short} {
		// With no reference price the residual cannot be shown to be below the
		// venue's minimum notional (review 4.5k round 3, minor 3).
		if !(leg.Book.MidPriceQuote > 0) || leg.Rules.MinNotionalQuote > 0 && residual*leg.Book.MidPriceQuote >= leg.Rules.MinNotionalQuote {
			return false
		}
	}
	return true
}

// reduceToMatch shrinks both legs to the common size the smaller one supports.
// It returns the venue-read sizes afterwards, and "" on success or the reason it
// did not shrink.
func (x *Executor) reduceToMatch(ctx context.Context, intent Intent, plan openPlan, res *Result, longHeld, shortHeld float64, pending *pendingSet) (float64, float64, string) {
	target := broker.FloorToStep(math.Min(longHeld, shortHeld), plan.CommonStepCoin)
	if target <= 0 {
		return longHeld, shortHeld, "không có cỡ chung nào trên lưới chung"
	}
	// The KEPT size must be closeable later on both venues without any
	// reduce-only exemption: Binance documents one, Bybit linear's is unread.
	for _, leg := range []struct {
		spec LegSpec
		side broker.Side
	}{{intent.Long, broker.SideSell}, {intent.Short, broker.SideBuy}} {
		if _, err := broker.RoundOrder(broker.RoundRequest{Rules: leg.spec.Rules, Side: leg.side, Type: broker.OrderTypeMarket,
			QtyCoin: target, PriceQuote: leg.spec.Book.MidPriceQuote}); err != nil {
			return longHeld, shortHeld, fmt.Sprintf("cỡ giữ lại %.10g coin không đóng lại được trên %s: %s", target, leg.spec.Venue.Name, err.Error())
		}
	}
	// Reducing is OPTIONAL, so it may refuse on a bad price; unwinding is not.
	for _, cut := range []struct {
		spec     LegSpec
		excess   float64
		fillSide strategy.Side
	}{{intent.Long, longHeld - target, strategy.SideSell}, {intent.Short, shortHeld - target, strategy.SideBuy}} {
		if cut.excess <= 0 {
			continue
		}
		fill := strategy.EstimateFill(cut.spec.Book, cut.fillSide, cut.excess*cut.spec.Book.MidPriceQuote)
		if !fill.Fillable {
			return longHeld, shortHeld, fmt.Sprintf("sổ %s không hấp thụ được phần cắt %.10g coin: %s", cut.spec.Venue.Name, cut.excess, fill.ReasonVI)
		}
		if bps := fill.SlippagePct * 100; bps > x.cfg.MaxSlippageBps {
			return longHeld, shortHeld, fmt.Sprintf("cắt %.10g coin trên %s trượt %.2f bps > %.2f bps", cut.excess, cut.spec.Venue.Name, bps, x.cfg.MaxSlippageBps)
		}
	}
	x.record(ctx, execution.Event{IntentID: intent.ID, Kind: execution.EventReducing, QtyCoin: target,
		DetailVI: fmt.Sprintf("thu hai chân về %.10g coin (long %.10g, short %.10g)", target, longHeld, shortHeld)})

	reduceCtx, cancel, deadline := x.closeOutContext(ctx)
	defer cancel()
	// Both cuts at once: a failed cut is followed by an unwind of both legs, so
	// a cut on one side alone never stands.
	var longDrive, shortDrive legDrive
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		longDrive = x.driveLegTo(reduceCtx, intent.ID, intent.Symbol, LegLong, intent.Long, target, PurposeReduce, res.AttemptNonce, deadline, pending)
	}()
	go func() {
		defer wg.Done()
		shortDrive = x.driveLegTo(reduceCtx, intent.ID, intent.Symbol, LegShort, intent.Short, target, PurposeReduce, res.AttemptNonce, deadline, pending)
	}()
	wg.Wait()
	res.Long.ClosedQtyCoin += longDrive.closedQtyCoin
	res.Short.ClosedQtyCoin += shortDrive.closedQtyCoin
	newLong, newShort := longHeld, shortHeld
	if longDrive.positionRead {
		newLong = longDrive.finalQtyCoin
		res.Long.VenuePositionQtyCoin, res.Long.VenuePositionRead = longDrive.finalQtyCoin, true
	}
	if shortDrive.positionRead {
		newShort = -shortDrive.finalQtyCoin
		res.Short.VenuePositionQtyCoin, res.Short.VenuePositionRead = shortDrive.finalQtyCoin, true
	}
	if err := errors.Join(longDrive.err, shortDrive.err); err != nil {
		return newLong, newShort, "lệnh thu nhỏ hỏng: " + err.Error()
	}
	if !hedgedWithin(intent.Long, intent.Short, plan.ToleranceQtyCoin, newLong, newShort) {
		return newLong, newShort, fmt.Sprintf("sau khi thu nhỏ vẫn không phòng hộ: long %.10g, short %.10g", newLong, newShort)
	}
	x.record(ctx, execution.Event{IntentID: intent.ID, Kind: execution.EventReduced, FilledQtyCoin: target,
		DetailVI: fmt.Sprintf("cặp còn long %.10g / short %.10g", newLong, newShort)})
	return newLong, newShort, ""
}

// unwind closes every leg the venues say is held — the larger leg first, the
// other to what it really left (flattenPair) — with its own budget, and returns
// flat, flat and loud beside a pending reduce-only order, or unresolved.
func (x *Executor) unwind(ctx context.Context, intent Intent, res Result, longHeld, shortHeld float64, pending *pendingSet) (Result, error) {
	finalCtx, cancelFinal, _ := x.closeOutContext(ctx)
	defer cancelFinal()
	if longHeld <= 0 && shortHeld <= 0 {
		// Nothing was ever held; the positions are still confirmed.
		confirmErr, still := x.confirmFlatAndSettle(finalCtx, intent, &res, pending)
		switch {
		case confirmErr != nil:
			res.Outcome = OutcomeUnresolved
			return res, confirmErr
		case len(still) > 0:
			res.Outcome = OutcomeBothFlat
			return res, x.pendingErr(still)
		}
		res.Outcome = OutcomeBothFlat
		return res, nil
	}
	x.record(ctx, execution.Event{IntentID: intent.ID, Kind: execution.EventUnwindStarted,
		DetailVI: fmt.Sprintf("gỡ: long %.10g, short %.10g", longHeld, shortHeld)})
	startedAt := x.cfg.Now()
	first := LegLong
	if shortHeld > longHeld {
		first = LegShort
	}
	longDrive, shortDrive, err := x.flattenPair(ctx, intent.ID, intent.Symbol, intent.Long, intent.Short, first, res.AttemptNonce, PurposeUnwind, pending)
	res.Long.ClosedQtyCoin += longDrive.closedQtyCoin
	res.Short.ClosedQtyCoin += shortDrive.closedQtyCoin
	res.UnwindDuration = x.cfg.Now().Sub(startedAt)
	if firstMs := firstFillAtMs(res); firstMs > 0 && res.UnhedgedWindow == 0 {
		res.UnhedgedWindow = absDuration(x.cfg.Now().UnixMilli() - firstMs)
	}
	x.record(ctx, execution.Event{IntentID: intent.ID, Kind: execution.EventUnwindDone, DetailVI: fmt.Sprintf("mất %s", res.UnwindDuration)})
	if err != nil {
		after := x.readBoth(finalCtx, intent.Symbol, intent.Long.Venue, intent.Short.Venue, false)
		x.notePositions(&res, after)
		res.Outcome, res.PendingOrders = OutcomeUnresolved, exportPending(pending.list())
		return res, fmt.Errorf("%w: gỡ không xong — %v; SÀN: %s", execution.ErrUnwindIncomplete, err,
			after.evidenceVI(intent.Long.Venue.Name, intent.Short.Venue.Name))
	}
	confirmErr, still := x.confirmFlatAndSettle(finalCtx, intent, &res, pending)
	if confirmErr != nil {
		res.Outcome = OutcomeUnresolved
		return res, confirmErr
	}
	res.Outcome = OutcomeUnwoundFlat
	if len(still) > 0 {
		return res, x.pendingErr(still)
	}
	return res, nil
}

// pendingErr is the loud error for flat venues beside pending reduce-only orders.
func (x *Executor) pendingErr(still []pendingOrder) error {
	ids := make([]string, 0, len(still))
	for _, p := range still {
		ids = append(ids, fmt.Sprintf("%s %s", p.venue.Name, p.q.ClientOrderID))
	}
	return fmt.Errorf("%w: sàn đã phẳng nhưng %d lệnh giảm vị thế chưa chứng minh được là đã kết thúc (%s) — nó sẽ giảm BẤT KỲ vị thế nào trên symbol khi khớp muộn; khóa phải được GIỮ",
		ErrLegAmbiguous, len(still), strings.Join(ids, ", "))
}

// confirmFlat asks both venues (rule 7) and fails loudly unless both read
// exactly zero.
func (x *Executor) confirmFlat(ctx context.Context, intent Intent, res *Result) error {
	pos, agreed := x.awaitPositions(ctx, intent.Symbol, intent.Long.Venue, intent.Short.Venue, 0, 0)
	x.notePositions(res, pos)
	if !agreed {
		return fmt.Errorf("%w: sàn chưa phẳng sau khi gỡ — %s", execution.ErrUnwindIncomplete, pos.evidenceVI(intent.Long.Venue.Name, intent.Short.Venue.Name))
	}
	return nil
}

// finish records the resolution and returns.
func (x *Executor) finish(ctx context.Context, intent Intent, res Result, err error) (Result, error) {
	if err != nil {
		res.ReasonVI = joinVI(res.ReasonVI, err.Error())
	}
	x.record(ctx, execution.Event{IntentID: intent.ID, Kind: execution.EventResolved, Outcome: execution.Outcome(res.Outcome), Err: err, DetailVI: res.EvidenceVI})
	return res, err
}

// unresolved is the one exit that means the invariant may not hold.
func (x *Executor) unresolved(ctx context.Context, intent Intent, res Result, err error) (Result, error) {
	res.Outcome = OutcomeUnresolved
	return x.finish(ctx, intent, res, err)
}

func (x *Executor) notePositions(res *Result, pos pairPositions) {
	res.Long.VenuePositionRead, res.Short.VenuePositionRead = pos.longErr == nil, pos.shortErr == nil
	if pos.longErr == nil {
		res.Long.VenuePositionQtyCoin = pos.longQtyCoin
	}
	if pos.shortErr == nil {
		res.Short.VenuePositionQtyCoin = pos.shortQtyCoin
	}
	if pos.read() {
		res.DeltaImbalanceQtyCoin = math.Abs(pos.longQtyCoin + pos.shortQtyCoin)
	}
}

func mergeRun(base LegResult, run legRun) LegResult {
	if run.order.ClientOrderID != "" {
		base.ClientOrderID = run.order.ClientOrderID
	}
	base.VenueOrderID = run.order.VenueOrderID
	base.Status = run.order.Status
	base.OrderConfirmed = run.confirmed
	base.FilledQtyCoin = run.order.FilledQtyCoin
	base.AvgFillPriceQuote = run.order.AvgFillPriceQuote
	base.FilledAtMs = run.filledAtMs
	return base
}

func firstFillAtMs(res Result) int64 {
	switch {
	case res.Long.FilledAtMs > 0 && res.Short.FilledAtMs > 0:
		return min(res.Long.FilledAtMs, res.Short.FilledAtMs)
	case res.Long.FilledAtMs > 0:
		return res.Long.FilledAtMs
	}
	return res.Short.FilledAtMs
}

func absDuration(ms int64) time.Duration {
	if ms < 0 {
		ms = -ms
	}
	return time.Duration(ms) * time.Millisecond
}

func confirmedVI(ok bool) string {
	if ok {
		return ", đã kết thúc"
	}
	return ", CHƯA xác nhận kết thúc"
}

func errVI(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func joinVI(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " | ")
}
