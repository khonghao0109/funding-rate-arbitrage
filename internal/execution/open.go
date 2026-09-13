package execution

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/strategy"
)

// Trader holds the two venues and runs one position's whole life: Open (4.4)
// and Close (4.5), against the SAME invariant and the same recorder.
//
// The two halves are one type because they share everything that matters —
// which venues, which deadlines, which clock, which recorder — and because the
// invariant is one sentence that has to mean the same thing in both: when the
// call returns, the two legs hold the same quantity within the coarser step, or
// both hold nothing. Never one leg.
//
// It is deliberately not concurrent-safe for a single intent: two goroutines
// acting on the same intent would place the same derived ClientOrderIDs twice,
// and the venue's duplicate-id refusal is the only thing that would stop them.
// One caller, one intent at a time.
type Trader struct {
	spot broker.Broker
	perp broker.Broker
	cfg  Config
	rec  Recorder
}

// Opener is the name step 4.4a used, kept as an alias so every existing caller
// and test still reads. New code should say Trader: a type called Opener with a
// Close method is a type nobody can guess the shape of.
type Opener = Trader

// NewOpener wires the two brokers. A zero field in cfg is filled from
// DefaultConfig, EXCEPT MaxEntryCostWidenBps, where zero is a real setting
// ("tolerate no widening at all") and defaulting it would silently loosen a
// caller's risk limit.
func NewOpener(spotBroker, perpBroker broker.Broker, cfg Config, rec Recorder) (*Trader, error) {
	if spotBroker == nil || perpBroker == nil {
		return nil, errors.New("execution: both a spot and a perp broker are required")
	}
	d := DefaultConfig()
	if cfg.LegTimeout <= 0 {
		cfg.LegTimeout = d.LegTimeout
	}
	if cfg.UnwindTimeout <= 0 {
		cfg.UnwindTimeout = d.UnwindTimeout
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = d.PollEvery
	}
	if cfg.MaxBookAge <= 0 {
		cfg.MaxBookAge = d.MaxBookAge
	}
	if cfg.OrderSettleTimeout <= 0 {
		cfg.OrderSettleTimeout = d.OrderSettleTimeout
	}
	// One leg may not spend the whole close-out budget waiting to hear about
	// itself: the second leg has to be paid for out of the same clock.
	if cap := cfg.UnwindTimeout / 3; cfg.OrderSettleTimeout > cap {
		cfg.OrderSettleTimeout = cap
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxResendPerLeg < 0 {
		cfg.MaxResendPerLeg = 0
	}
	if cfg.LegOrder == "" {
		cfg.LegOrder = d.LegOrder
	}
	if cfg.LegOrder != LegOrderSequentialSpotFirst && cfg.LegOrder != LegOrderParallel {
		return nil, fmt.Errorf("execution: LegOrder %q is neither %q nor %q", cfg.LegOrder, LegOrderSequentialSpotFirst, LegOrderParallel)
	}
	// NOT defaulted: zero is a real tolerance ("the touch, not a tick beyond").
	// Negative is refused rather than clamped, because it prices a buy below
	// the touch and the symptom is an order that never fills — which reads as
	// a venue fault rather than as a configuration one.
	if cfg.MaxSlippageBps < 0 {
		return nil, fmt.Errorf("execution: MaxSlippageBps is %v; a negative tolerance prices a buy below the touch", cfg.MaxSlippageBps)
	}
	if rec == nil {
		rec = nopRecorder{}
	}
	return &Trader{spot: spotBroker, perp: perpBroker, cfg: cfg, rec: rec}, nil
}

// Open opens one delta-neutral pair, and returns having satisfied the
// invariant: both legs open, or both flat. See doc.go.
//
// The error is non-nil for every outcome that is not a completed hedge. A
// caller must branch on the RESULT's Outcome for what the account holds, and on
// the error for why — in particular ErrUnwindIncomplete, which is the one error
// meaning the invariant may NOT hold and a human is needed.
func (o *Trader) Open(ctx context.Context, intent Intent) (Result, error) {
	res := Result{IntentID: intent.ID, Outcome: OutcomeBothFlat}
	o.record(ctx, Event{IntentID: intent.ID, Kind: EventIntentReceived, QtyCoin: intent.NotionalQuote})

	plan, err := planEntry(intent, o.cfg)
	if err != nil {
		// Nothing was sent, so the account is untouched and flat by
		// construction. This is the cheapest of all the exits.
		res.ReasonVI = err.Error()
		res.BookAgeMs = plan.BookAgeMs
		o.record(ctx, Event{IntentID: intent.ID, Kind: EventRefused, Err: err, DetailVI: err.Error()})
		o.record(ctx, Event{IntentID: intent.ID, Kind: EventResolved, Outcome: OutcomeBothFlat})
		return res, err
	}
	res.TargetQtyCoin, res.BookAgeMs = plan.QtyCoin, plan.BookAgeMs
	inv := plan.invariant(intent)
	o.record(ctx, Event{IntentID: intent.ID, Kind: EventSized, QtyCoin: plan.QtyCoin})
	o.record(ctx, Event{IntentID: intent.ID, Kind: EventBookChecked,
		DetailVI: fmt.Sprintf("chi phí vào %.4f%%, rộng thêm %.2f bps, sổ cũ %d ms", plan.EntryCostPct, plan.WidenBps, plan.BookAgeMs)})

	res.Spot = LegResult{Leg: LegSpot, Market: broker.MarketSpot, Symbol: intent.Symbol,
		ClientOrderID: LegClientOrderID(intent.ID, LegSpot)}
	res.Perp = LegResult{Leg: LegPerp, Market: broker.MarketFuturesUSDM, Symbol: intent.Symbol,
		ClientOrderID: LegClientOrderID(intent.ID, LegPerp)}

	// The first half of the spot leg's flat proof: what the VENUE says this
	// account holds of the base asset before anything is sent. Read now,
	// because after the fact there is nothing to compare against — and a
	// failure to read it is recorded rather than fatal, since the proof has a
	// second, independent half.
	if qty, ok := o.readSpotBaseQtyCoin(ctx, intent); ok {
		res.SpotBaseBalanceBeforeQtyCoin, res.SpotBaseBalanceRead = qty, true
	}

	spotReq := broker.PlaceOrderRequest{
		Market: broker.MarketSpot, Symbol: intent.Symbol, Side: broker.SideBuy,
		Type: broker.OrderTypeLimitGTC, ClientOrderID: res.Spot.ClientOrderID,
		QtyCoin: plan.SpotOrder.QtyCoin, PriceQuote: plan.SpotOrder.PriceQuote,
	}
	perpReq := broker.PlaceOrderRequest{
		Market: broker.MarketFuturesUSDM, Symbol: intent.Symbol, Side: broker.SideSell,
		Type: broker.OrderTypeLimitGTC, ClientOrderID: res.Perp.ClientOrderID,
		QtyCoin: plan.PerpOrder.QtyCoin, PriceQuote: plan.PerpOrder.PriceQuote,
	}

	if o.cfg.LegOrder == LegOrderParallel {
		spotLeg, perpLeg := o.placeBothAtOnce(ctx, intent, plan, spotReq, perpReq)
		res.Spot = mergeLeg(res.Spot, spotLeg)
		res.Perp = mergeLeg(res.Perp, perpLeg)
		if spotLeg.err != nil && perpLeg.err != nil {
			return o.unwind(ctx, intent, inv, res, joinVI([]string{
				reasonFor("chân spot", spotLeg.err, res.Spot.FilledQtyCoin, plan.QtyCoin),
				reasonFor("chân perp", perpLeg.err, res.Perp.FilledQtyCoin, plan.QtyCoin)}))
		}
	} else {
		// LEG 1 — spot, and the order is deliberate: if the second leg fails we
		// hold the first naked until the unwind completes, and a naked spot long
		// cannot be liquidated while a naked perp short can.
		spotLeg := o.workLeg(ctx, intent, LegSpot, o.spot, spotReq, plan.QtyCoin)
		res.Spot = mergeLeg(res.Spot, spotLeg)
		if spotLeg.err != nil || !reachedTarget(res.Spot.FilledQtyCoin, plan.QtyCoin) {
			// Leg 1 fell short, so leg 2 is never placed and there is nothing
			// to reduce TO. Shrinking the pair needs both legs to hold
			// something; here only one does.
			return o.unwind(ctx, intent, inv, res, reasonFor("chân spot", spotLeg.err, res.Spot.FilledQtyCoin, plan.QtyCoin))
		}

		// LEG 2 — perp.
		perpLeg := o.workLeg(ctx, intent, LegPerp, o.perp, perpReq, plan.QtyCoin)
		res.Perp = mergeLeg(res.Perp, perpLeg)
	}

	res.UnhedgedWindow = unhedgedWindow(res.Spot.FilledAtMs, res.Perp.FilledAtMs)

	shortOfTarget := !reachedTarget(res.Spot.FilledQtyCoin, plan.QtyCoin) ||
		!reachedTarget(res.Perp.FilledQtyCoin, plan.QtyCoin)
	if shortOfTarget {
		// One leg stopped short. Before throwing a good position away, try to
		// SHRINK the pair to what the short leg really holds — see
		// reduceToMatch. Unwinding is the fallback, not the first answer.
		reasonVI := joinVI([]string{
			reasonFor("chân spot", nil, res.Spot.FilledQtyCoin, plan.QtyCoin),
			reasonFor("chân perp", nil, res.Perp.FilledQtyCoin, plan.QtyCoin)})
		if whyNotVI := o.reduceToMatch(ctx, intent, inv, plan, &res); whyNotVI != "" {
			return o.unwind(ctx, intent, inv, res, reasonVI+" | không thu nhỏ được: "+whyNotVI)
		}
	}

	outcome, err := inv.classify(res.Spot.FilledQtyCoin, res.Perp.FilledQtyCoin)
	if err != nil {
		// Both legs think they filled, and the pair is still not hedged. Try
		// to close out rather than return holding it.
		return o.unwind(ctx, intent, inv, res, err.Error())
	}
	res.Outcome = outcome
	res.ResidualQtyCoin = math.Abs(res.Spot.FilledQtyCoin - res.Perp.FilledQtyCoin)
	o.record(ctx, Event{IntentID: intent.ID, Kind: EventResolved, Outcome: outcome,
		FilledQtyCoin: res.Spot.FilledQtyCoin})
	return res, nil
}

// legOutcome is what one leg did: the venue's own account of the order, when
// this process saw it reach that state, and why it stopped if it stopped badly.
type legOutcome struct {
	order      broker.Order
	filledAtMs int64
	err        error
}

// placeBothAtOnce runs the two legs concurrently.
//
// It exists to make the unhedged window measurable at its shortest — the
// difference between two fills rather than the whole of leg 2's deadline. It is
// NOT the default: with both legs live at once, a refusal on one arrives while
// the other is still working, so the unwind has two moving things to catch
// instead of one. 4.4b measures both windows on a real venue and the default
// moves, if it moves, on that measurement.
func (o *Trader) placeBothAtOnce(ctx context.Context, intent Intent, plan entryPlan,
	spotReq, perpReq broker.PlaceOrderRequest) (legOutcome, legOutcome) {

	var spotLeg, perpLeg legOutcome
	done := make(chan struct{})
	go func() {
		defer close(done)
		perpLeg = o.workLeg(ctx, intent, LegPerp, o.perp, perpReq, plan.QtyCoin)
	}()
	spotLeg = o.workLeg(ctx, intent, LegSpot, o.spot, spotReq, plan.QtyCoin)
	<-done
	return spotLeg, perpLeg
}

// unhedgedWindow is how long exactly one leg was open, from two instants on
// this process's own clock. Zero when either leg never filled — the naked
// window of a run that unwound is measured by the unwind instead.
func unhedgedWindow(aMs, bMs int64) time.Duration {
	if aMs <= 0 || bMs <= 0 {
		return 0
	}
	d := aMs - bMs
	if d < 0 {
		d = -d
	}
	return time.Duration(d) * time.Millisecond
}

// reduceToMatch shrinks the pair to the smaller leg instead of unwinding it.
//
// This is the design point the 4.4a report was least comfortable with. When one
// leg reaches 60% of the target, 4.4a closed BOTH legs: it paid a round trip to
// destroy a position that was hedged, merely smaller than asked for. A desk
// reduces the larger leg to match the smaller one and keeps the trade.
//
// The reason 4.4a did not is that shrinking needs three things to be true at
// once, and each of them can fail halfway:
//
//  1. the KEPT size must still be a legal, CLOSEABLE position on both venues —
//     a pair too small for a venue to trade is a pair we cannot get out of,
//     which is worse than not having it;
//  2. the REDUCTION order must itself be placeable on the larger leg's venue;
//  3. the book must be able to absorb that reduction inside MaxSlippageBps.
//
// All three are checked BEFORE anything is sent, and any of them failing falls
// back to the 4.4a behaviour — unwind to flat — rather than leaving the pair as
// it is. The invariant does not move: the outcome is still both open within the
// coarser step, or both flat.
//
// It returns "" on success, and otherwise the reason it did not shrink, in the
// operator's language.
func (o *Trader) reduceToMatch(ctx context.Context, intent Intent, inv pairInvariant, plan entryPlan, res *Result) string {
	spotQty, perpQty := res.Spot.FilledQtyCoin, res.Perp.FilledQtyCoin
	keepQtyCoin := math.Min(spotQty, perpQty)
	if keepQtyCoin <= 0 {
		return "một chân không giữ gì — không có cỡ chung nào để thu về"
	}
	// Already hedged. This happens when a leg stops one step short: the pair
	// satisfies the invariant as it stands, and "reducing" it would place an
	// order to fix something that is not broken — usually an order the venue
	// would refuse anyway, which would then be read as a reason to unwind a
	// perfectly good position.
	if err := inv.check(spotQty, perpQty); err == nil {
		return ""
	}

	// (1) The kept size must be closeable on BOTH venues. RoundOrder answers
	// exactly that question — minQty, the step grid and minNotional together —
	// so the rules are read once, from the venues, and not restated here.
	for _, leg := range []struct {
		nameVI     string
		rules      exchanges.Instrument
		side       broker.Side
		priceQuote float64
	}{
		{"spot", intent.SpotInstrument, broker.SideSell, intent.SpotPriceQuote},
		{"perp", intent.PerpInstrument, broker.SideBuy, intent.PerpPriceQuote},
	} {
		if _, err := broker.RoundOrder(broker.RoundRequest{
			Rules: leg.rules, Side: leg.side, Type: broker.OrderTypeMarket,
			QtyCoin: keepQtyCoin, PriceQuote: leg.priceQuote,
		}); err != nil {
			return fmt.Sprintf("cỡ giữ lại %.10g coin không đóng lại được trên sàn %s: %s", keepQtyCoin, leg.nameVI, err.Error())
		}
	}

	// The book, at the NEW size. On the same snapshot a smaller size can only
	// price better, so this cannot fail today; it is written against the SIZE
	// rather than against the snapshot so that it is already the right check
	// when 4.4b re-reads the book before reducing.
	keptSpot := strategy.EstimateFill(intent.SpotBook, strategy.SideBuy, keepQtyCoin*intent.SpotPriceQuote)
	keptPerp := strategy.EstimateFill(intent.PerpBook, strategy.SideSell, keepQtyCoin*intent.PerpPriceQuote)
	if !keptSpot.Fillable || !keptPerp.Fillable {
		return "sổ không định giá được cặp ở cỡ mới"
	}
	if widenBps := (keptSpot.SlippagePct + keptPerp.SlippagePct - intent.SignalEntryCostPct) * 100; widenBps > o.cfg.MaxEntryCostWidenBps {
		return fmt.Sprintf("ở cỡ mới chi phí vào rộng thêm %.2f bps > %.2f bps cho phép", widenBps, o.cfg.MaxEntryCostWidenBps)
	}

	// Which leg is the larger one, and by how much.
	larger, smallerQty := LegSpot, perpQty
	if perpQty > spotQty {
		larger, smallerQty = LegPerp, spotQty
	}
	excessQtyCoin := math.Max(spotQty, perpQty) - smallerQty
	if excessQtyCoin <= 0 {
		return "" // already equal; nothing to sell back
	}

	legBroker, legResult := o.spot, &res.Spot
	market, side, rules, priceQuote, book, fillSide := broker.MarketSpot, broker.SideSell,
		intent.SpotInstrument, intent.SpotPriceQuote, intent.SpotBook, strategy.SideSell
	if larger == LegPerp {
		legBroker, legResult = o.perp, &res.Perp
		market, side, rules, priceQuote, book, fillSide = broker.MarketFuturesUSDM, broker.SideBuy,
			intent.PerpInstrument, intent.PerpPriceQuote, intent.PerpBook, strategy.SideBuy
	}

	// (2) the reduction must be placeable, and (3) the book must absorb it.
	rounded, err := broker.RoundOrder(broker.RoundRequest{
		Rules: rules, Side: side, Type: broker.OrderTypeMarket,
		QtyCoin: excessQtyCoin, PriceQuote: priceQuote,
	})
	if err != nil {
		return fmt.Sprintf("phần dư %.10g coin trên chân %s không đặt được lệnh: %s", excessQtyCoin, larger, err.Error())
	}
	// The book check belongs to the REDUCTION and not to the unwind, and the
	// asymmetry is deliberate. Reducing is OPTIONAL — the alternative is a
	// perfectly good unwind — so it is allowed to refuse on a bad price.
	// Unwinding is MANDATORY: refusing there would leave the account holding
	// one leg, which is the one outcome this package exists to prevent, so the
	// unwind takes whatever the book gives it and reports what it paid.
	cut := strategy.EstimateFill(book, fillSide, rounded.QtyCoin*priceQuote)
	if !cut.Fillable {
		return fmt.Sprintf("sổ chân %s không hấp thụ được phần cắt %.10g coin: %s", larger, rounded.QtyCoin, cut.ReasonVI)
	}
	if cutBps := cut.SlippagePct * 100; cutBps > o.cfg.MaxSlippageBps {
		return fmt.Sprintf("cắt phần dư trên chân %s trượt %.2f bps > %.2f bps cho phép", larger, cutBps, o.cfg.MaxSlippageBps)
	}

	o.record(ctx, Event{IntentID: intent.ID, Kind: EventReducing, Leg: larger,
		QtyCoin: rounded.QtyCoin, DetailVI: fmt.Sprintf("thu chân %s về %.10g coin cho bằng chân kia", larger, keepQtyCoin)})

	safe, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.cfg.UnwindTimeout)
	defer cancel()
	closed, err := o.closeLeg(safe, intent, larger, legBroker, market, side, rounded.QtyCoin, priceQuote)
	if err != nil {
		return fmt.Sprintf("lệnh thu nhỏ chân %s hỏng: %s", larger, err.Error())
	}
	legResult.UnwoundQtyCoin += closed
	legResult.FilledQtyCoin -= closed
	if legResult.FilledQtyCoin < 0 {
		legResult.FilledQtyCoin = 0
	}

	// The pair has to be hedged AFTER the cut, on the venues' own numbers. If
	// it is not, say nothing was achieved and let the caller unwind: a failed
	// shrink must not leave a worse pair than it found.
	if _, err := inv.classify(res.Spot.FilledQtyCoin, res.Perp.FilledQtyCoin); err != nil {
		return fmt.Sprintf("sau khi thu nhỏ cặp vẫn không phòng hộ: %s", err.Error())
	}
	res.ReducedToMatch = true
	res.ResidualQtyCoin = math.Abs(res.Spot.FilledQtyCoin - res.Perp.FilledQtyCoin)
	_ = plan
	o.record(ctx, Event{IntentID: intent.ID, Kind: EventReduced, Leg: larger,
		FilledQtyCoin: closed, DetailVI: fmt.Sprintf("cặp còn %.10g coin mỗi chân", keepQtyCoin)})
	return ""
}

// workLeg places one leg and works it to its deadline, returning what the
// VENUE says happened — never what this process believes.
func (o *Trader) workLeg(ctx context.Context, intent Intent, leg LegName, b broker.Broker,
	req broker.PlaceOrderRequest, targetQtyCoin float64) legOutcome {

	q := broker.OrderQuery{Market: req.Market, Symbol: req.Symbol, ClientOrderID: req.ClientOrderID}
	deadline := o.cfg.Now().Add(o.cfg.LegTimeout)
	o.record(ctx, Event{IntentID: intent.ID, Kind: EventLegPlacing, Leg: leg,
		ClientOrderID: req.ClientOrderID, QtyCoin: req.QtyCoin, PriceQuote: req.PriceQuote})

	order, err := o.placeResolving(ctx, intent, leg, b, req, q, deadline)
	if err != nil {
		return o.seal(order, err)
	}
	o.record(ctx, Event{IntentID: intent.ID, Kind: EventLegPlaced, Leg: leg,
		ClientOrderID: order.ClientOrderID, VenueOrderID: order.VenueOrderID, FilledQtyCoin: order.FilledQtyCoin})

	// Work it until it is done, or until the deadline.
	for !order.Status.Done() && !reachedTarget(order.FilledQtyCoin, targetQtyCoin) {
		if o.cfg.Now().After(deadline) {
			break
		}
		if err := o.sleep(ctx, o.cfg.PollEvery); err != nil {
			// The context died. Do NOT return here without reading the venue:
			// the order may have filled, and Open's caller must be told the
			// truth about the account, not about our context.
			break
		}
		latest, err := b.GetOrder(ctx, q)
		if err != nil {
			if o.cfg.Now().After(deadline) {
				break
			}
			continue
		}
		order = latest
	}

	if order.Status.Done() && reachedTarget(order.FilledQtyCoin, targetQtyCoin) {
		o.record(ctx, Event{IntentID: intent.ID, Kind: EventLegFilled, Leg: leg,
			ClientOrderID: order.ClientOrderID, VenueOrderID: order.VenueOrderID, FilledQtyCoin: order.FilledQtyCoin})
		return o.seal(order, nil)
	}

	// Out of time, or finished short. Cancel whatever is left — and then read
	// it back, ALWAYS.
	o.record(ctx, Event{IntentID: intent.ID, Kind: EventLegCancelling, Leg: leg,
		ClientOrderID: order.ClientOrderID, FilledQtyCoin: order.FilledQtyCoin})
	safe := context.WithoutCancel(ctx)
	if !order.Status.Done() {
		if _, cancelErr := b.CancelOrder(safe, q); cancelErr != nil && !errors.Is(cancelErr, broker.ErrOrderNotFound) {
			o.record(safe, Event{IntentID: intent.ID, Kind: EventLegCancelling, Leg: leg, Err: cancelErr})
		}
	}

	// THE READ-BACK. A cancel can race a fill: the venue may have filled the
	// order in the microseconds before the cancel arrived, so believing our own
	// cancel would either unwind a position we still hold, or fail to unwind
	// one we do. Everything downstream uses THIS number, not the one we had
	// before cancelling. The property test exists to catch the removal of
	// these five lines.
	readBack, err := b.GetOrder(safe, q)
	if err != nil {
		if errors.Is(err, broker.ErrOrderNotFound) {
			// The venue positively has no such order: nothing filled.
			o.record(safe, Event{IntentID: intent.ID, Kind: EventLegReadBack, Leg: leg, FilledQtyCoin: 0,
				DetailVI: "sàn không có lệnh này — không có gì khớp"})
			return o.seal(broker.Order{ClientOrderID: req.ClientOrderID, Status: broker.OrderStatusCanceled}, nil)
		}
		// We cannot say what the venue holds. Report the last thing it told us
		// and let the caller unwind on that, which is the conservative
		// direction: unwinding a quantity that turns out not to exist fails
		// loudly, whereas assuming zero leaves a naked leg silently.
		o.record(safe, Event{IntentID: intent.ID, Kind: EventLegReadBack, Leg: leg, Err: err})
		return o.seal(order, fmt.Errorf("không đọc lại được %s từ sàn sau khi huỷ: %w", leg, err))
	}
	o.record(safe, Event{IntentID: intent.ID, Kind: EventLegReadBack, Leg: leg,
		ClientOrderID: readBack.ClientOrderID, VenueOrderID: readBack.VenueOrderID,
		FilledQtyCoin: readBack.FilledQtyCoin, DetailVI: string(readBack.Status)})
	return o.seal(readBack, nil)
}

// seal stamps the instant THIS PROCESS settled what the leg holds. It is taken
// on Config.Now, once, at every exit from workLeg, so that the two legs'
// instants are readings of one clock and their difference is a window rather
// than a skew.
func (o *Trader) seal(order broker.Order, err error) legOutcome {
	out := legOutcome{order: order, err: err}
	if order.FilledQtyCoin > 0 {
		out.filledAtMs = o.cfg.Now().UnixMilli()
	}
	return out
}

// readSpotBaseQtyCoin asks the SPOT venue how much of the base asset this
// account holds, free plus locked.
//
// Locked counts: while a buy is resting, part of the holding is committed to
// the order, and a proof that read only the free half would call an account
// with everything on the book empty.
func (o *Trader) readSpotBaseQtyCoin(ctx context.Context, intent Intent) (float64, bool) {
	asset := intent.SpotInstrument.BaseAsset
	if asset == "" {
		return 0, false
	}
	balances, err := o.spot.GetBalance(ctx, broker.MarketSpot)
	if err != nil {
		o.record(ctx, Event{IntentID: intent.ID, Kind: EventLegReadBack, Leg: LegSpot, Err: err,
			DetailVI: "không đọc được số dư " + asset + " từ sàn spot"})
		return 0, false
	}
	for _, b := range balances {
		if b.Asset == asset {
			return b.TotalQtyCoin(), true
		}
	}
	// The venue answered and this asset is not in the answer. That IS zero —
	// a venue that lists balances and omits an asset is saying it holds none.
	return 0, true
}

// placeResolving sends one order and resolves an ambiguous failure the way the
// step-4.2 contract says, never by guessing.
func (o *Trader) placeResolving(ctx context.Context, intent Intent, leg LegName, b broker.Broker,
	req broker.PlaceOrderRequest, q broker.OrderQuery, deadline time.Time) (broker.Order, error) {

	var lastErr error
	for attempt := 0; attempt <= o.cfg.MaxResendPerLeg; attempt++ {
		order, err := b.PlaceOrder(ctx, req)
		if err == nil {
			return order, nil
		}
		lastErr = err

		// A DEFINITE refusal: the venue answered, or this package's own
		// validation refused. Resending changes nothing and asking the venue
		// about an order it never accepted wastes the deadline.
		if definiteRejection(err) {
			o.record(ctx, Event{IntentID: intent.ID, Kind: EventLegResolved, Leg: leg, Err: err,
				DetailVI: "sàn từ chối dứt khoát, không gửi lại"})
			return broker.Order{}, fmt.Errorf("%s bị từ chối: %w", leg, err)
		}

		// AMBIGUOUS. We do not know whether the venue has this order, and both
		// guesses are dangerous: resend and a double fill leaves twice the
		// position unhedged; wait and a leg that never existed is never placed.
		// So ask, by the id we chose before sending.
		o.record(ctx, Event{IntentID: intent.ID, Kind: EventLegAmbiguous, Leg: leg, Err: err,
			ClientOrderID: req.ClientOrderID})
		found, qErr := o.resolveByID(ctx, b, q, deadline)
		switch {
		case qErr == nil:
			o.record(ctx, Event{IntentID: intent.ID, Kind: EventLegResolved, Leg: leg,
				ClientOrderID: found.ClientOrderID, VenueOrderID: found.VenueOrderID,
				FilledQtyCoin: found.FilledQtyCoin, DetailVI: "lệnh ĐÃ tới sàn — không gửi lại"})
			return found, nil
		case errors.Is(qErr, broker.ErrOrderNotFound):
			// The venue positively does not have it. This is the ONLY branch
			// down which a resend is safe.
			if attempt < o.cfg.MaxResendPerLeg {
				o.record(ctx, Event{IntentID: intent.ID, Kind: EventLegResent, Leg: leg,
					ClientOrderID: req.ClientOrderID, DetailVI: "sàn chưa nhận — gửi lại an toàn"})
			}
			continue
		default:
			// STILL AMBIGUOUS. Do not resend. This is the branch that gets
			// deleted by somebody tidying up, and the only one that is always
			// right to be careful in.
			o.record(ctx, Event{IntentID: intent.ID, Kind: EventLegResolved, Leg: leg, Err: qErr,
				DetailVI: "VẪN MƠ HỒ — không gửi lại, chuyển sang gỡ vị thế"})
			return broker.Order{}, fmt.Errorf("%s vẫn mơ hồ sau khi tra sàn: %w", leg, qErr)
		}
	}
	return broker.Order{}, fmt.Errorf("%s không đặt được sau %d lần: %w", leg, o.cfg.MaxResendPerLeg+1, lastErr)
}

// resolveByID asks the venue about one order until it gives an answer that
// means something, or the deadline passes.
func (o *Trader) resolveByID(ctx context.Context, b broker.Broker, q broker.OrderQuery, deadline time.Time) (broker.Order, error) {
	safe := context.WithoutCancel(ctx)
	for {
		order, err := b.GetOrder(safe, q)
		if err == nil || errors.Is(err, broker.ErrOrderNotFound) {
			return order, err
		}
		if o.cfg.Now().After(deadline) {
			return broker.Order{}, err
		}
		if sleepErr := o.sleep(safe, o.cfg.PollEvery); sleepErr != nil {
			return broker.Order{}, err
		}
	}
}

// unwind closes whatever is open and returns having reached the invariant, or
// says loudly that it could not.
func (o *Trader) unwind(ctx context.Context, intent Intent, inv pairInvariant, res Result, reasonVI string) (Result, error) {
	res.ReasonVI = reasonVI
	startedAt := o.cfg.Now()
	// What the spot leg held when the unwind began. It is the second half of
	// the flat proof — "the closing order filled exactly what the opening one
	// did" — and it has to be taken before the loop below starts subtracting.
	openedSpotQtyCoin := res.Spot.FilledQtyCoin
	o.record(ctx, Event{IntentID: intent.ID, Kind: EventUnwindStarted, DetailVI: reasonVI,
		FilledQtyCoin: res.Spot.FilledQtyCoin})

	// The unwind must survive a cancelled context. A caller that gave up is
	// exactly the caller who must not be left holding one leg.
	safe, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.cfg.UnwindTimeout)
	defer cancel()

	var failures []string
	for _, leg := range []struct {
		name   LegName
		b      broker.Broker
		result *LegResult
		rules  func() (market broker.Market, side broker.Side, priceQuote float64)
	}{
		{LegSpot, o.spot, &res.Spot, func() (broker.Market, broker.Side, float64) {
			// Close a long by selling.
			return broker.MarketSpot, broker.SideSell, intent.SpotPriceQuote
		}},
		{LegPerp, o.perp, &res.Perp, func() (broker.Market, broker.Side, float64) {
			// Close a short by buying.
			return broker.MarketFuturesUSDM, broker.SideBuy, intent.PerpPriceQuote
		}},
	} {
		if leg.result.FilledQtyCoin <= 0 {
			continue
		}
		market, side, priceQuote := leg.rules()
		closed, err := o.closeLeg(safe, intent, leg.name, leg.b, market, side, leg.result.FilledQtyCoin, priceQuote)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", leg.name, err.Error()))
			continue
		}
		leg.result.UnwoundQtyCoin = closed
		// The leg is flat by exactly as much as it was open.
		leg.result.FilledQtyCoin -= closed
		if leg.result.FilledQtyCoin < 0 {
			leg.result.FilledQtyCoin = 0
		}
		// A remainder the close could not reach is not dust to be shrugged
		// off: it is a position we hold and cannot close, and the only honest
		// thing to do with it is say so. It happens when the fill sits between
		// two points of the venue's own quantity grid, so rounding the close
		// DOWN — the right direction for an opening order — leaves a sliver
		// behind.
		if leg.result.FilledQtyCoin > 0 {
			failures = append(failures, fmt.Sprintf(
				"%s còn %.10g coin không đóng được (đã đóng %.10g/%.10g; phần dư nằm giữa hai nấc lưới của sàn)",
				leg.name, leg.result.FilledQtyCoin, closed, closed+leg.result.FilledQtyCoin))
		}
	}

	res.UnwindDuration = o.cfg.Now().Sub(startedAt)
	res.ResidualQtyCoin = math.Abs(res.Spot.FilledQtyCoin - res.Perp.FilledQtyCoin)
	// The naked window of a run that unwound: from the one leg that filled to
	// the moment the close-out finished. This is the figure PLAN 4.4's
	// acceptance means by "leg 2 fails and leg 1 closes within a few seconds",
	// and it is a different measurement from the two-fill window above.
	if res.UnhedgedWindow == 0 {
		if firstFillMs := firstFilledAtMs(res.Spot, res.Perp); firstFillMs > 0 {
			if d := o.cfg.Now().UnixMilli() - firstFillMs; d > 0 {
				res.UnhedgedWindow = time.Duration(d) * time.Millisecond
			}
		}
	}
	o.record(safe, Event{IntentID: intent.ID, Kind: EventUnwindDone,
		DetailVI: fmt.Sprintf("mất %s", res.UnwindDuration), FilledQtyCoin: res.Spot.FilledQtyCoin})

	// Confirm against the VENUE, not against our own arithmetic (rule 7).
	flatErr := o.confirmFlat(safe, intent, openedSpotQtyCoin, &res)
	if flatErr != nil {
		failures = append(failures, flatErr.Error())
	}

	outcome, err := inv.classify(res.Spot.FilledQtyCoin, res.Perp.FilledQtyCoin)
	if err != nil || len(failures) > 0 {
		res.Outcome = OutcomeBothFlat // best effort label; the error is what matters
		detail := reasonVI
		if len(failures) > 0 {
			detail += " | " + joinVI(failures)
		}
		res.ReasonVI = detail
		// A conflict between the two flat proofs is its own sentinel: it does
		// not mean the unwind failed, it means we cannot tell whether it
		// succeeded, and those call for different actions.
		wrapped := ErrUnwindIncomplete
		if errors.Is(flatErr, ErrFlatEvidenceConflict) {
			wrapped = ErrFlatEvidenceConflict
		}
		o.record(safe, Event{IntentID: intent.ID, Kind: EventResolved, Err: wrapped, DetailVI: detail})
		return res, fmt.Errorf("%w: %s", wrapped, detail)
	}
	res.Outcome = outcome
	o.record(safe, Event{IntentID: intent.ID, Kind: EventResolved, Outcome: outcome, DetailVI: reasonVI})
	return res, fmt.Errorf("ý định %s đã gỡ về phẳng trong %s: %s", intent.ID, res.UnwindDuration, reasonVI)
}

// closeLeg sends the opposite MARKET order, sized to what ACTUALLY filled.
//
// Sizing an unwind from the intended notional is how a partial fill becomes an
// opposite position, so the quantity here is the one read back from the venue
// and nothing else.
func (o *Trader) closeLeg(ctx context.Context, intent Intent, leg LegName, b broker.Broker,
	market broker.Market, side broker.Side, qtyCoin, priceQuote float64) (float64, error) {

	rules := intent.SpotInstrument
	if leg == LegPerp {
		rules = intent.PerpInstrument
	}
	rounded, err := broker.RoundOrder(broker.RoundRequest{
		Rules: rules, Side: side, Type: broker.OrderTypeMarket,
		QtyCoin: qtyCoin, PriceQuote: priceQuote,
	})
	if err != nil {
		// A partial fill too small for the venue to trade away is a real and
		// nasty state: we hold something we cannot close. Loud, by name.
		return 0, fmt.Errorf("không đóng được %v coin đã khớp: %w", qtyCoin, err)
	}

	req := broker.PlaceOrderRequest{
		Market: market, Symbol: intent.Symbol, Side: side, Type: broker.OrderTypeMarket,
		ClientOrderID: unwindClientOrderID(intent.ID, leg), QtyCoin: rounded.QtyCoin,
		ReduceOnly: market == broker.MarketFuturesUSDM,
	}
	o.record(ctx, Event{IntentID: intent.ID, Kind: EventUnwindLeg, Leg: leg,
		ClientOrderID: req.ClientOrderID, QtyCoin: req.QtyCoin})

	q := broker.OrderQuery{Market: market, Symbol: intent.Symbol, ClientOrderID: req.ClientOrderID}
	order, err := b.PlaceOrder(ctx, req)
	if err != nil {
		// Same ambiguity contract as the open: ask before concluding.
		found, qErr := o.resolveByID(ctx, b, q, o.cfg.Now().Add(o.cfg.UnwindTimeout))
		if qErr != nil {
			return 0, fmt.Errorf("lệnh đóng %s hỏng và không xác nhận được: %w", leg, err)
		}
		order = found
	}
	// An acknowledgement is not a fill — see settleOrder. The unwind is the
	// last place that may believe one.
	order = o.settleOrder(ctx, b, q, order)
	if order.FilledQtyCoin <= 0 {
		return 0, fmt.Errorf("lệnh đóng %s không khớp được gì (trạng thái %s)", leg, order.Status)
	}
	return order.FilledQtyCoin, nil
}

// confirmFlat asks the VENUE what it holds (rule 7), and for the spot leg it
// asks TWICE, in two independent ways.
//
// The two legs are not symmetric, and pretending they were would be a lie:
//
//   - The PERP leg has a position, so GetPosition answers directly and the
//     invariant is asserted on the venue's own number.
//   - The SPOT leg has no position, only a balance — and the account may hold
//     the asset for reasons that predate this intent. 4.4a therefore proved
//     spot flatness from this process's OWN arithmetic, which is exactly what
//     rule 7 warns against, and the 4.4a report listed it as an open hole.
//
// It is closed here with a SECOND, independent piece of evidence rather than a
// better single one:
//
//	A (the venue)  the base-asset balance is back where it was before we
//	               started, within one step size
//	B (our books)  the closing order filled exactly what the opening order did
//
// Neither is sufficient alone. A moves for reasons that have nothing to do with
// us; B is our own belief about our own orders, which is the thing under test.
// Together they are worth having because they FAIL DIFFERENTLY, and when they
// disagree that disagreement is the finding: nothing is reconciled, both
// numbers are reported, and the caller gets ErrFlatEvidenceConflict. Choosing
// the one that matches what we expected is how a wrong belief survives contact
// with evidence.
func (o *Trader) confirmFlat(ctx context.Context, intent Intent, openedSpotQtyCoin float64, res *Result) error {
	pos, err := o.perp.GetPosition(ctx, broker.MarketFuturesUSDM, intent.Symbol)
	if err != nil && !errors.Is(err, broker.ErrNotSupported) {
		return fmt.Errorf("không đọc được vị thế perp từ sàn để xác nhận phẳng: %s", err.Error())
	}
	if err == nil && !pos.Flat() {
		return fmt.Errorf("sàn vẫn báo vị thế perp %v coin sau khi gỡ", pos.QtyCoin)
	}

	// Evidence B, ours: the close filled what the open did.
	step := intent.SpotInstrument.StepSizeCoin
	closedSpotQtyCoin := res.Spot.UnwoundQtyCoin
	ordersAgree := math.Abs(closedSpotQtyCoin-openedSpotQtyCoin) <= step+gridEpsilon

	// Evidence A, the venue's: the balance came back to where it started.
	after, read := o.readSpotBaseQtyCoin(ctx, intent)
	if !read || !res.SpotBaseBalanceRead {
		res.SpotFlatEvidenceVI = fmt.Sprintf(
			"chân spot: SỔ CỦA TA nói mở %.10g coin, đóng %.10g coin (%s); SỐ DƯ SÀN: không đọc được, nên chỉ có một bằng chứng",
			openedSpotQtyCoin, closedSpotQtyCoin, agreementVI(ordersAgree))
		if !ordersAgree {
			return fmt.Errorf("chân spot đóng %.10g coin nhưng đã mở %.10g coin", closedSpotQtyCoin, openedSpotQtyCoin)
		}
		return nil
	}
	res.SpotBaseBalanceAfterQtyCoin = after
	deltaQtyCoin := after - res.SpotBaseBalanceBeforeQtyCoin
	balanceAgrees := math.Abs(deltaQtyCoin) <= step+gridEpsilon

	res.SpotFlatEvidenceVI = fmt.Sprintf(
		"chân spot: SỐ DƯ SÀN %.10g → %.10g (lệch %.10g coin, dung sai một bước %.10g) — %s; SỔ CỦA TA mở %.10g đóng %.10g coin — %s",
		res.SpotBaseBalanceBeforeQtyCoin, after, deltaQtyCoin, step, agreementVI(balanceAgrees),
		openedSpotQtyCoin, closedSpotQtyCoin, agreementVI(ordersAgree))

	switch {
	case balanceAgrees && ordersAgree:
		return nil
	case balanceAgrees != ordersAgree:
		// The two disagree. NOT reconciled, by design.
		return fmt.Errorf("%w: %s", ErrFlatEvidenceConflict, res.SpotFlatEvidenceVI)
	default:
		// Both say not flat, which is not a conflict — it is a failed unwind,
		// and it is reported as one.
		return fmt.Errorf("chân spot chưa phẳng theo cả hai bằng chứng: %s", res.SpotFlatEvidenceVI)
	}
}

// agreementVI renders one piece of evidence's verdict.
func agreementVI(ok bool) string {
	if ok {
		return "KHỚP (phẳng)"
	}
	return "KHÔNG khớp"
}

// firstFilledAtMs is the earlier of the two legs' fill instants, ignoring a leg
// that never filled.
func firstFilledAtMs(a, b LegResult) int64 {
	switch {
	case a.FilledAtMs > 0 && b.FilledAtMs > 0:
		return minInt64(a.FilledAtMs, b.FilledAtMs)
	case a.FilledAtMs > 0:
		return a.FilledAtMs
	default:
		return b.FilledAtMs
	}
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// UnwindClientOrderID derives the unwinding order's id. Distinct from the
// opening leg's and from the close's, and derived the same way, so a restarted
// process — or a diagnostic holding nothing but the intent id — can ask the
// venue about every order this intent could have produced.
func UnwindClientOrderID(intentID string, leg LegName) string {
	return LegClientOrderID(intentID+"|unwind", leg)
}

func unwindClientOrderID(intentID string, leg LegName) string {
	return UnwindClientOrderID(intentID, leg)
}

// definiteRejection reports whether the order was positively refused, as
// opposed to having its fate left unknown.
//
// The rule that works across venues is not "the venue answered" — it is "the
// answer came from the thing that would have executed the order". A 4xx is the
// venue's own matching engine saying no: the order does not exist and never
// will. A 5xx is not. A 502 or a 503 typically comes from a gateway IN FRONT
// of the venue, which may have forwarded the order perfectly well before
// failing to relay the reply — so it carries exactly the same ambiguity as a
// timeout, and must be resolved by asking, not by assuming.
//
// Getting this backwards is the expensive direction: treating a 502 as a
// refusal skips the GetOrder resolution entirely, so the state machine unwinds
// leg 1 while leg 2 may be live at the venue — which is the precise failure
// this package exists to prevent, arrived at through the code meant to prevent
// it.
func definiteRejection(err error) bool {
	// This package's own validation, and venue rules checked before sending.
	// Nothing was transmitted, so nothing can be pending.
	if errors.Is(err, broker.ErrInvalidOrder) || errors.Is(err, broker.ErrBelowMinNotional) ||
		errors.Is(err, broker.ErrBelowMinQty) || errors.Is(err, broker.ErrAboveMaxQty) ||
		errors.Is(err, broker.ErrNotTrading) || errors.Is(err, broker.ErrRulesUnknown) {
		return true
	}
	var httpErr *broker.HTTPError
	if !errors.As(err, &httpErr) {
		return false // no answer at all: ambiguous
	}
	return httpErr.StatusCode >= 400 && httpErr.StatusCode < 500
}

// reachedTarget reports whether a leg filled its whole intended quantity.
func reachedTarget(filledQtyCoin, targetQtyCoin float64) bool {
	return filledQtyCoin >= targetQtyCoin-gridEpsilon
}

// mergeLeg folds what the venue said into the leg result, keeping the
// ClientOrderID this process chose even when the venue answered nothing.
func mergeLeg(base LegResult, leg legOutcome) LegResult {
	order := leg.order
	if order.ClientOrderID != "" {
		base.ClientOrderID = order.ClientOrderID
	}
	base.VenueOrderID = order.VenueOrderID
	base.Status = order.Status
	base.FilledQtyCoin = order.FilledQtyCoin
	base.AvgFillPriceQuote = order.AvgFillPriceQuote
	base.FilledAtMs = leg.filledAtMs
	return base
}

func reasonFor(legVI string, err error, filled, target float64) string {
	if err != nil {
		return fmt.Sprintf("%s hỏng: %s", legVI, err.Error())
	}
	return fmt.Sprintf("%s chỉ khớp %.10g/%.10g coin trong hạn", legVI, filled, target)
}

// sleep waits, or returns when the context dies.
func (o *Trader) sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (o *Trader) record(ctx context.Context, ev Event) {
	if ev.At.IsZero() {
		ev.At = o.cfg.Now()
	}
	// A Recorder that fails must not change what the state machine does:
	// losing the record of an unwind is bad, abandoning the unwind because the
	// record failed is catastrophic.
	_ = o.rec.Record(ctx, ev)
}

func joinVI(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}
