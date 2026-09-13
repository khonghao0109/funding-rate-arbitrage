package execution

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"futures-arbitrage-scanner/internal/broker"
)

// Opener holds the two venues and opens one pair at a time.
//
// It is deliberately not concurrent-safe for a single intent: two goroutines
// opening the same intent would place the same derived ClientOrderIDs twice,
// and the venue's duplicate-id refusal is the only thing that would stop them.
// One caller, one intent at a time.
type Opener struct {
	spot broker.Broker
	perp broker.Broker
	cfg  Config
	rec  Recorder
}

// NewOpener wires the two brokers. A zero field in cfg is filled from
// DefaultConfig, EXCEPT MaxEntryCostWidenBps, where zero is a real setting
// ("tolerate no widening at all") and defaulting it would silently loosen a
// caller's risk limit.
func NewOpener(spotBroker, perpBroker broker.Broker, cfg Config, rec Recorder) (*Opener, error) {
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
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxResendPerLeg < 0 {
		cfg.MaxResendPerLeg = 0
	}
	if rec == nil {
		rec = nopRecorder{}
	}
	return &Opener{spot: spotBroker, perp: perpBroker, cfg: cfg, rec: rec}, nil
}

// Open opens one delta-neutral pair, and returns having satisfied the
// invariant: both legs open, or both flat. See doc.go.
//
// The error is non-nil for every outcome that is not a completed hedge. A
// caller must branch on the RESULT's Outcome for what the account holds, and on
// the error for why — in particular ErrUnwindIncomplete, which is the one error
// meaning the invariant may NOT hold and a human is needed.
func (o *Opener) Open(ctx context.Context, intent Intent) (Result, error) {
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

	// LEG 1 — spot, and the order is deliberate: if the second leg fails we
	// hold the first naked until the unwind completes, and a naked spot long
	// cannot be liquidated while a naked perp short can.
	spotLeg, err := o.workLeg(ctx, intent, LegSpot, o.spot, broker.PlaceOrderRequest{
		Market: broker.MarketSpot, Symbol: intent.Symbol, Side: broker.SideBuy,
		Type: broker.OrderTypeLimitGTC, ClientOrderID: res.Spot.ClientOrderID,
		QtyCoin: plan.SpotOrder.QtyCoin, PriceQuote: plan.SpotOrder.PriceQuote,
	}, plan.QtyCoin)
	res.Spot = mergeLeg(res.Spot, spotLeg)
	if err != nil || !reachedTarget(res.Spot.FilledQtyCoin, plan.QtyCoin) {
		return o.unwind(ctx, intent, inv, res, reasonFor("chân spot", err, res.Spot.FilledQtyCoin, plan.QtyCoin))
	}

	// LEG 2 — perp.
	perpLeg, err := o.workLeg(ctx, intent, LegPerp, o.perp, broker.PlaceOrderRequest{
		Market: broker.MarketFuturesUSDM, Symbol: intent.Symbol, Side: broker.SideSell,
		Type: broker.OrderTypeLimitGTC, ClientOrderID: res.Perp.ClientOrderID,
		QtyCoin: plan.PerpOrder.QtyCoin, PriceQuote: plan.PerpOrder.PriceQuote,
	}, plan.QtyCoin)
	res.Perp = mergeLeg(res.Perp, perpLeg)
	if err != nil || !reachedTarget(res.Perp.FilledQtyCoin, plan.QtyCoin) {
		return o.unwind(ctx, intent, inv, res, reasonFor("chân perp", err, res.Perp.FilledQtyCoin, plan.QtyCoin))
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

// workLeg places one leg and works it to its deadline, returning what the
// VENUE says happened — never what this process believes.
func (o *Opener) workLeg(ctx context.Context, intent Intent, leg LegName, b broker.Broker,
	req broker.PlaceOrderRequest, targetQtyCoin float64) (broker.Order, error) {

	q := broker.OrderQuery{Market: req.Market, Symbol: req.Symbol, ClientOrderID: req.ClientOrderID}
	deadline := o.cfg.Now().Add(o.cfg.LegTimeout)
	o.record(ctx, Event{IntentID: intent.ID, Kind: EventLegPlacing, Leg: leg,
		ClientOrderID: req.ClientOrderID, QtyCoin: req.QtyCoin, PriceQuote: req.PriceQuote})

	order, err := o.placeResolving(ctx, intent, leg, b, req, q, deadline)
	if err != nil {
		return order, err
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
		return order, nil
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
			return broker.Order{ClientOrderID: req.ClientOrderID, Status: broker.OrderStatusCanceled}, nil
		}
		// We cannot say what the venue holds. Report the last thing it told us
		// and let the caller unwind on that, which is the conservative
		// direction: unwinding a quantity that turns out not to exist fails
		// loudly, whereas assuming zero leaves a naked leg silently.
		o.record(safe, Event{IntentID: intent.ID, Kind: EventLegReadBack, Leg: leg, Err: err})
		return order, fmt.Errorf("không đọc lại được %s từ sàn sau khi huỷ: %w", leg, err)
	}
	o.record(safe, Event{IntentID: intent.ID, Kind: EventLegReadBack, Leg: leg,
		ClientOrderID: readBack.ClientOrderID, VenueOrderID: readBack.VenueOrderID,
		FilledQtyCoin: readBack.FilledQtyCoin, DetailVI: string(readBack.Status)})
	return readBack, nil
}

// placeResolving sends one order and resolves an ambiguous failure the way the
// step-4.2 contract says, never by guessing.
func (o *Opener) placeResolving(ctx context.Context, intent Intent, leg LegName, b broker.Broker,
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
func (o *Opener) resolveByID(ctx context.Context, b broker.Broker, q broker.OrderQuery, deadline time.Time) (broker.Order, error) {
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
func (o *Opener) unwind(ctx context.Context, intent Intent, inv pairInvariant, res Result, reasonVI string) (Result, error) {
	res.ReasonVI = reasonVI
	startedAt := o.cfg.Now()
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
	o.record(safe, Event{IntentID: intent.ID, Kind: EventUnwindDone,
		DetailVI: fmt.Sprintf("mất %s", res.UnwindDuration), FilledQtyCoin: res.Spot.FilledQtyCoin})

	// Confirm against the VENUE, not against our own arithmetic (rule 7).
	if err := o.confirmFlat(safe, intent, &res); err != nil {
		failures = append(failures, err.Error())
	}

	outcome, err := inv.classify(res.Spot.FilledQtyCoin, res.Perp.FilledQtyCoin)
	if err != nil || len(failures) > 0 {
		res.Outcome = OutcomeBothFlat // best effort label; the error is what matters
		detail := reasonVI
		if len(failures) > 0 {
			detail += " | " + joinVI(failures)
		}
		res.ReasonVI = detail
		o.record(safe, Event{IntentID: intent.ID, Kind: EventResolved, Err: ErrUnwindIncomplete, DetailVI: detail})
		return res, fmt.Errorf("%w: %s", ErrUnwindIncomplete, detail)
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
func (o *Opener) closeLeg(ctx context.Context, intent Intent, leg LegName, b broker.Broker,
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

	order, err := b.PlaceOrder(ctx, req)
	if err != nil {
		// Same ambiguity contract as the open: ask before concluding.
		q := broker.OrderQuery{Market: market, Symbol: intent.Symbol, ClientOrderID: req.ClientOrderID}
		found, qErr := o.resolveByID(ctx, b, q, o.cfg.Now().Add(o.cfg.UnwindTimeout))
		if qErr != nil {
			return 0, fmt.Errorf("lệnh đóng %s hỏng và không xác nhận được: %w", leg, err)
		}
		order = found
	}
	if order.FilledQtyCoin <= 0 {
		return 0, fmt.Errorf("lệnh đóng %s không khớp được gì (trạng thái %s)", leg, order.Status)
	}
	return order.FilledQtyCoin, nil
}

// confirmFlat asks the VENUE what it holds (rule 7).
//
// The two legs are NOT symmetric, and pretending they were would be a lie:
//
//   - The PERP leg has a position, so GetPosition answers directly and the
//     invariant is asserted on the venue's own number.
//   - The SPOT leg has only a balance, and the account may hold the asset for
//     reasons that predate this intent. An absolute balance assertion there
//     would be meaningless, so the balance is FETCHED AND REPORTED but the
//     proof is that the closing order filled what the opening order did.
func (o *Opener) confirmFlat(ctx context.Context, intent Intent, res *Result) error {
	pos, err := o.perp.GetPosition(ctx, broker.MarketFuturesUSDM, intent.Symbol)
	if err != nil && !errors.Is(err, broker.ErrNotSupported) {
		return fmt.Errorf("không đọc được vị thế perp từ sàn để xác nhận phẳng: %s", err.Error())
	}
	if err == nil && !pos.Flat() {
		return fmt.Errorf("sàn vẫn báo vị thế perp %v coin sau khi gỡ", pos.QtyCoin)
	}
	// Reported, not asserted on — see the doc comment.
	if _, balErr := o.spot.GetBalance(ctx, broker.MarketSpot); balErr != nil {
		o.record(ctx, Event{IntentID: intent.ID, Kind: EventUnwindDone, Leg: LegSpot, Err: balErr,
			DetailVI: "không đọc được số dư spot để báo cáo (không phải bằng chứng phẳng)"})
	}
	return nil
}

// unwindClientOrderID derives the closing order's id. Distinct from the
// opening leg's, and derived the same way, so a restarted process can ask about
// the CLOSE as well as the open.
func unwindClientOrderID(intentID string, leg LegName) string {
	return LegClientOrderID(intentID+"|unwind", leg)
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
func mergeLeg(base LegResult, order broker.Order) LegResult {
	if order.ClientOrderID != "" {
		base.ClientOrderID = order.ClientOrderID
	}
	base.VenueOrderID = order.VenueOrderID
	base.Status = order.Status
	base.FilledQtyCoin = order.FilledQtyCoin
	base.AvgFillPriceQuote = order.AvgFillPriceQuote
	return base
}

func reasonFor(legVI string, err error, filled, target float64) string {
	if err != nil {
		return fmt.Sprintf("%s hỏng: %s", legVI, err.Error())
	}
	return fmt.Sprintf("%s chỉ khớp %.10g/%.10g coin trong hạn", legVI, filled, target)
}

// sleep waits, or returns when the context dies.
func (o *Opener) sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (o *Opener) record(ctx context.Context, ev Event) {
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
