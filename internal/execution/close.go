package execution

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"futures-arbitrage-scanner/internal/broker"
)

// Step 4.5 — closing the position, and saying what it made.
//
// # The invariant is the same sentence as Open's
//
// When Close returns, the two legs hold the SAME quantity within the coarser of
// the two venues' step sizes: either zero, which is the whole position closed,
// or the same non-zero amount, which is a position that is still hedged and
// smaller than it was. Never one leg.
//
// That is how a close can fail safely. Everything that can be refused is
// refused BEFORE anything is sent, so a refusal leaves both legs exactly as
// they were. Once sending has begun, the spot leg is closed by WHAT THE PERP
// LEG ACTUALLY CLOSED — read back from the venue, not by what was asked for —
// so a perp close that only half-filled shrinks the pair rather than unbalancing
// it. A pair left half-closed is a named, loud error with both quantities in it,
// not a silent state.
//
// # Why the PERP leg closes first
//
// The mirror of Open's argument. If the second close fails we are left holding
// the leg that was not closed, and a naked SPOT LONG cannot be liquidated while
// a naked PERP SHORT can. So the dangerous leg goes first and the safe one is
// the one that can be left behind.
//
// # Rounding a close
//
// A closing order rounds the CLOSING way, and the difference from an opening
// order is the point. broker.RoundOrder floors quantity onto the step grid with
// a tolerance on the STEP COUNT, so a quantity that IS on the grid and merely
// looks off it in float64 — 0.3/0.0001 is 2999.9999999999995 — closes whole
// rather than leaving 0.0001 behind. Anything that genuinely does not sit on the
// grid leaves a remainder, and that remainder is REPORTED by name with the
// quantity: silently subtracting it is how a position nobody believes in
// survives on a venue.
//
// # What RealizedQuote is, and what it is not (CLAUDE.md rule 2)
//
// RealizedQuote is exactly three things, each READ FROM THE VENUE:
//
//	+ FundingReceivedQuote   the settlements that actually paid, signed as the
//	                         venue signs them (rule 6: rows, not an APR times a
//	                         holding time)
//	- CommissionQuote        the commission the venue charged, summed ONLY over
//	                         fills whose commission was taken in the QUOTE asset
//	- SlippageQuote          every one of the four fills against the reference
//	                         mid current when that decision was made
//
// It is NOT "net" and this package does not use that word — only
// internal/strategy may, and only about a figure that has had all four fills and
// the measured book deducted. Three things it deliberately leaves out, each
// reported beside it rather than folded in: PairPriceDriftQuote (the two legs'
// prices moving apart between entry and exit — the basis, which is real money
// and which nothing here deducts), commission charged in an asset that is not
// the quote asset (CommissionOtherVI — converting it needs a price at a moment
// and inventing one is rule 5's "wrong but not obviously wrong"), and the cost
// of capital.

// CloseRequest is one decision to close one pair.
type CloseRequest struct {
	// Intent is the same value Open was given, carrying the ids, the venue
	// rules and the CURRENT books — not the books the position was opened on.
	Intent Intent

	// QtyCoin is what the caller believes is open. 0 means "ask the venue",
	// which is rule 7's answer and the right default; a non-zero value is
	// CHECKED against the venue's own position rather than trusted.
	QtyCoin float64

	// OpenedAtMs bounds the funding window. Settlements before it belong to
	// somebody else's position.
	OpenedAtMs int64

	// The entry fills, for the slippage arithmetic. AvgPrice is what the fills
	// really cost; RefMid is the mid that was current when the ENTRY decision
	// was made. Zero on either side means that leg's entry slippage cannot be
	// priced and is reported as unpriced rather than as zero.
	EntrySpotAvgPriceQuote float64
	EntryPerpAvgPriceQuote float64
	EntrySpotRefMidQuote   float64
	EntryPerpRefMidQuote   float64
}

// CloseResult is what closing did, GROSS except where a name says otherwise.
type CloseResult struct {
	IntentID string

	// Outcome is OutcomeBothFlat when the whole position was closed. A pair
	// that is still hedged at a smaller size reports OutcomeBothOpen together
	// with an error: it is a real state, and calling it "closed" would be a
	// lie the next tick acts on.
	Outcome Outcome

	Spot LegResult
	Perp LegResult

	// RequestedQtyCoin is what was asked for; ClosedQtyCoin is what the venues
	// actually closed; RemainingQtyCoin is what the pair still holds on BOTH
	// legs afterwards.
	RequestedQtyCoin float64
	ClosedQtyCoin    float64
	RemainingQtyCoin float64

	// VenuePositionQtyCoin is the perp position the VENUE reported before
	// anything was sent, SIGNED. It is the number the request was checked
	// against (rule 7).
	VenuePositionQtyCoin float64

	Duration time.Duration

	// The three components, and their sum. See the file comment for what is
	// deliberately not in them.
	FundingReceivedQuote float64
	CommissionQuote      float64
	SlippageQuote        float64
	RealizedQuote        float64

	// SettlementsCounted is how many funding rows the venue listed inside the
	// holding window — settlements CROSSED, not periods elapsed (rule 6).
	SettlementsCounted int

	// PairPriceDriftQuote is the two legs' prices moving apart between entry
	// and exit: (exit spot - entry spot) + (entry perp - exit perp), times the
	// quantity. Reported BESIDE RealizedQuote and never inside it.
	PairPriceDriftQuote float64
	PriceDriftPricedVI  string

	// CommissionOtherVI lists commission charged in an asset that is not the
	// quote asset, unconverted. Empty when there was none.
	CommissionOtherVI string

	// FundingSourceVI and CommissionSourceVI say where each figure came from,
	// or why it is absent. A zero that means "not read" must never be
	// indistinguishable from a zero that means "nothing was charged".
	FundingSourceVI    string
	CommissionSourceVI string

	SpotBaseBalanceBeforeQtyCoin float64
	SpotBaseBalanceAfterQtyCoin  float64
	SpotBaseBalanceRead          bool
	SpotFlatEvidenceVI           string

	ReasonVI string
}

// The close's own refusals.
var (
	// ErrCloseRefused marks every refusal that happened BEFORE anything was
	// sent, so both legs are exactly as they were.
	ErrCloseRefused = errors.New("execution: đóng vị thế bị từ chối trước khi gửi lệnh nào — hai chân còn nguyên")

	// ErrNothingToClose is the venue holding nothing to close.
	ErrNothingToClose = fmt.Errorf("%w: sàn không giữ vị thế nào", ErrCloseRefused)

	// ErrPositionDisagrees is the venue's own position contradicting what the
	// caller believes is open. NOT reconciled — see CloseRequest.QtyCoin.
	ErrPositionDisagrees = fmt.Errorf("%w: vị thế sàn báo khác với cỡ được yêu cầu đóng", ErrCloseRefused)

	// ErrCloseIncomplete is a pair that is still hedged but was not fully
	// closed. The invariant holds — both legs hold the same amount — and the
	// position is still there, so the caller must act rather than move on.
	ErrCloseIncomplete = errors.New("execution: ĐÓNG CHƯA XONG — cặp vẫn phòng hộ ở cỡ nhỏ hơn, phải đóng nốt")

	// ErrCloseLeavesDust is a remainder that no correctly-rounded order can
	// reach. Reported rather than subtracted.
	ErrCloseLeavesDust = errors.New("execution: phần dư không nằm trên lưới của sàn nên không lệnh nào đóng được")
)

// CloseClientOrderID derives the closing order's id, distinct from both the
// opening leg's and the unwind's, and derived the same way — so a restarted
// process, or a diagnostic holding nothing but the intent id, can ask the venue
// about the CLOSE as well as the open (step 5.3).
func CloseClientOrderID(intentID string, leg LegName) string {
	return LegClientOrderID(intentID+"|close", leg)
}

func closeClientOrderID(intentID string, leg LegName) string {
	return CloseClientOrderID(intentID, leg)
}

// Close closes both legs of one position and reports what it made.
func (o *Trader) Close(ctx context.Context, req CloseRequest) (CloseResult, error) {
	intent := req.Intent
	res := CloseResult{IntentID: intent.ID, Outcome: OutcomeBothOpen}
	startedAt := o.cfg.Now()
	o.record(ctx, Event{IntentID: intent.ID, Kind: EventCloseStarted, QtyCoin: req.QtyCoin})

	if intent.ID == "" || intent.Symbol == "" {
		return res, fmt.Errorf("%w: ý định không có id hoặc symbol", ErrCloseRefused)
	}

	// Rule 7: what is open is what the VENUE says is open.
	pos, err := o.perp.GetPosition(ctx, broker.MarketFuturesUSDM, intent.Symbol)
	if err != nil {
		return res, fmt.Errorf("%w: không đọc được vị thế perp từ sàn: %s", ErrCloseRefused, err.Error())
	}
	res.VenuePositionQtyCoin = pos.QtyCoin
	if pos.QtyCoin > 0 {
		// We opened a SHORT. A long here is somebody else's position, or ours
		// flipped by something nobody saw; either way, closing it would trade
		// in the wrong direction.
		return res, fmt.Errorf("%w: sàn báo vị thế LONG %v coin, còn chiến lược này chỉ mở SHORT", ErrPositionDisagrees, pos.QtyCoin)
	}
	venueQtyCoin := math.Abs(pos.QtyCoin)

	qtyCoin := req.QtyCoin
	if qtyCoin <= 0 {
		qtyCoin = venueQtyCoin
	}
	res.RequestedQtyCoin = qtyCoin
	if qtyCoin <= 0 {
		return res, ErrNothingToClose
	}
	tolerance := math.Max(intent.SpotInstrument.StepSizeCoin, intent.PerpInstrument.StepSizeCoin)
	if venueQtyCoin < qtyCoin-tolerance-gridEpsilon {
		return res, fmt.Errorf("%w: sàn giữ %v coin, yêu cầu đóng %v coin", ErrPositionDisagrees, venueQtyCoin, qtyCoin)
	}

	if qty, ok := o.readSpotBaseQtyCoin(ctx, intent); ok {
		res.SpotBaseBalanceBeforeQtyCoin, res.SpotBaseBalanceRead = qty, true
	}

	// PREFLIGHT. Both closing orders must be placeable before either is sent:
	// a refusal here leaves the position exactly as it was, which is the only
	// completely safe failure a close has.
	spotRounded, err := broker.RoundOrder(broker.RoundRequest{
		Rules: intent.SpotInstrument, Side: broker.SideSell, Type: broker.OrderTypeMarket,
		QtyCoin: qtyCoin, PriceQuote: intent.SpotPriceQuote,
	})
	if err != nil {
		return res, fmt.Errorf("%w: chân spot: %s", ErrCloseRefused, err.Error())
	}
	perpRounded, err := broker.RoundOrder(broker.RoundRequest{
		Rules: intent.PerpInstrument, Side: broker.SideBuy, Type: broker.OrderTypeMarket,
		QtyCoin: qtyCoin, PriceQuote: intent.PerpPriceQuote,
	})
	if err != nil {
		return res, fmt.Errorf("%w: chân perp: %s", ErrCloseRefused, err.Error())
	}
	if dust := qtyCoin - math.Min(spotRounded.QtyCoin, perpRounded.QtyCoin); dust > gridEpsilon*tolerance {
		// Not fatal — the closeable part is still closed below — but it is
		// stated here, before sending, so nobody discovers it afterwards.
		res.ReasonVI = fmt.Sprintf("%s: %.10g coin", ErrCloseLeavesDust.Error(), dust)
	}

	safe, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.cfg.UnwindTimeout)
	defer cancel()

	res.Spot = LegResult{Leg: LegSpot, Market: broker.MarketSpot, Symbol: intent.Symbol,
		ClientOrderID: closeClientOrderID(intent.ID, LegSpot)}
	res.Perp = LegResult{Leg: LegPerp, Market: broker.MarketFuturesUSDM, Symbol: intent.Symbol,
		ClientOrderID: closeClientOrderID(intent.ID, LegPerp)}

	// PERP FIRST — the leg whose failure would leave the liquidatable side
	// naked goes before the leg whose failure would not.
	perpClosed, perpErr := o.closeLegWithID(safe, intent, LegPerp, o.perp, broker.MarketFuturesUSDM,
		broker.SideBuy, perpRounded.QtyCoin, intent.PerpPriceQuote, res.Perp.ClientOrderID, true)
	res.Perp.UnwoundQtyCoin = perpClosed
	if perpErr != nil && perpClosed <= 0 {
		// Nothing moved on either leg.
		return res, fmt.Errorf("%w: chân perp không đóng được và chân spot chưa gửi: %s", ErrCloseRefused, perpErr.Error())
	}

	// The spot leg closes BY WHAT THE PERP LEG REALLY CLOSED. Closing the
	// requested quantity when the perp only managed part of it is exactly how
	// a close unbalances a hedged pair.
	spotTargetQtyCoin := perpClosed
	if spotTargetQtyCoin > spotRounded.QtyCoin {
		spotTargetQtyCoin = spotRounded.QtyCoin
	}
	spotRounded, err = broker.RoundOrder(broker.RoundRequest{
		Rules: intent.SpotInstrument, Side: broker.SideSell, Type: broker.OrderTypeMarket,
		QtyCoin: spotTargetQtyCoin, PriceQuote: intent.SpotPriceQuote,
	})
	if err != nil {
		res.ReasonVI = joinVI([]string{res.ReasonVI, fmt.Sprintf("chân spot không đóng được %v coin: %s", spotTargetQtyCoin, err.Error())})
		return o.finishClose(safe, req, &res, startedAt, ErrCloseIncomplete)
	}
	spotClosed, spotErr := o.closeLegWithID(safe, intent, LegSpot, o.spot, broker.MarketSpot,
		broker.SideSell, spotRounded.QtyCoin, intent.SpotPriceQuote, res.Spot.ClientOrderID, false)
	res.Spot.UnwoundQtyCoin = spotClosed
	if spotErr != nil {
		res.ReasonVI = joinVI([]string{res.ReasonVI, "chân spot: " + spotErr.Error()})
	}

	res.ClosedQtyCoin = math.Min(spotClosed, perpClosed)
	res.RemainingQtyCoin = math.Max(0, qtyCoin-res.ClosedQtyCoin)

	var outErr error
	switch {
	case math.Abs(spotClosed-perpClosed) > tolerance+gridEpsilon:
		// The one state this must never reach quietly.
		outErr = fmt.Errorf("%w: chân spot đóng %.10g coin, chân perp đóng %.10g coin — lệch quá một bước %.10g",
			ErrUnwindIncomplete, spotClosed, perpClosed, tolerance)
	case res.RemainingQtyCoin > tolerance+gridEpsilon:
		outErr = fmt.Errorf("%w: đã đóng %.10g/%.10g coin, còn %.10g coin mỗi chân",
			ErrCloseIncomplete, res.ClosedQtyCoin, qtyCoin, res.RemainingQtyCoin)
	}
	return o.finishClose(safe, req, &res, startedAt, outErr)
}

// finishClose confirms against the venue, prices what the position made, and
// labels the outcome.
func (o *Trader) finishClose(ctx context.Context, req CloseRequest, res *CloseResult, startedAt time.Time, outErr error) (CloseResult, error) {
	intent := req.Intent
	res.Duration = o.cfg.Now().Sub(startedAt)

	// The perp side of the flat proof, from the venue.
	if pos, err := o.perp.GetPosition(ctx, broker.MarketFuturesUSDM, intent.Symbol); err == nil {
		res.VenuePositionQtyCoin = pos.QtyCoin
		if !pos.Flat() && res.RemainingQtyCoin <= gridEpsilon {
			outErr = errors.Join(outErr, fmt.Errorf("%w: sàn vẫn báo vị thế perp %v coin sau khi đóng", ErrUnwindIncomplete, pos.QtyCoin))
		}
	} else if !errors.Is(err, broker.ErrNotSupported) {
		res.ReasonVI = joinVI([]string{res.ReasonVI, "không đọc lại được vị thế perp: " + err.Error()})
	}

	// The spot side, with both pieces of evidence — the same two Open uses.
	after, read := o.readSpotBaseQtyCoin(ctx, intent)
	step := intent.SpotInstrument.StepSizeCoin
	ordersAgree := math.Abs(res.Spot.UnwoundQtyCoin-res.ClosedQtyCoin) <= step+gridEpsilon
	if read && res.SpotBaseBalanceRead {
		res.SpotBaseBalanceAfterQtyCoin = after
		delta := res.SpotBaseBalanceBeforeQtyCoin - after
		balanceAgrees := math.Abs(delta-res.ClosedQtyCoin) <= step+gridEpsilon
		res.SpotFlatEvidenceVI = fmt.Sprintf(
			"chân spot: SỐ DƯ SÀN %.10g → %.10g (giảm %.10g coin) — %s; SỔ CỦA TA đóng %.10g coin — %s",
			res.SpotBaseBalanceBeforeQtyCoin, after, delta, agreementVI(balanceAgrees),
			res.Spot.UnwoundQtyCoin, agreementVI(ordersAgree))
		if balanceAgrees != ordersAgree {
			outErr = errors.Join(outErr, fmt.Errorf("%w: %s", ErrFlatEvidenceConflict, res.SpotFlatEvidenceVI))
		}
	} else {
		res.SpotFlatEvidenceVI = fmt.Sprintf(
			"chân spot: SỐ DƯ SÀN không đọc được; SỔ CỦA TA đóng %.10g coin — %s", res.Spot.UnwoundQtyCoin, agreementVI(ordersAgree))
	}

	o.priceClose(ctx, req, res)

	if res.RemainingQtyCoin <= gridEpsilon && outErr == nil {
		res.Outcome = OutcomeBothFlat
	}
	o.record(ctx, Event{IntentID: intent.ID, Kind: EventCloseDone, Outcome: res.Outcome,
		FilledQtyCoin: res.ClosedQtyCoin, Err: outErr,
		DetailVI: fmt.Sprintf("đóng %.10g coin trong %s; funding %.6f, phí %.6f, trượt %.6f → RealizedQuote %.6f",
			res.ClosedQtyCoin, res.Duration, res.FundingReceivedQuote, res.CommissionQuote, res.SlippageQuote, res.RealizedQuote)})
	if outErr != nil {
		res.ReasonVI = joinVI([]string{res.ReasonVI, outErr.Error()})
	}
	return *res, outErr
}

// closeLegWithID sends one closing MARKET order under a caller-chosen id and
// returns what the VENUE says it filled.
func (o *Trader) closeLegWithID(ctx context.Context, intent Intent, leg LegName, b broker.Broker,
	market broker.Market, side broker.Side, qtyCoin, priceQuote float64, clientOrderID string, reduceOnly bool) (float64, error) {

	req := broker.PlaceOrderRequest{
		Market: market, Symbol: intent.Symbol, Side: side, Type: broker.OrderTypeMarket,
		ClientOrderID: clientOrderID, QtyCoin: qtyCoin, ReduceOnly: reduceOnly,
	}
	o.record(ctx, Event{IntentID: intent.ID, Kind: EventCloseLeg, Leg: leg,
		ClientOrderID: clientOrderID, QtyCoin: qtyCoin})

	order, err := b.PlaceOrder(ctx, req)
	if err != nil {
		// The same ambiguity contract as the open: ask the venue by the id we
		// chose before sending, never guess.
		q := broker.OrderQuery{Market: market, Symbol: intent.Symbol, ClientOrderID: clientOrderID}
		found, qErr := o.resolveByID(ctx, b, q, o.cfg.Now().Add(o.cfg.UnwindTimeout))
		if qErr != nil {
			return 0, fmt.Errorf("lệnh đóng %s hỏng và không xác nhận được: %s", leg, err.Error())
		}
		order = found
	}
	if order.FilledQtyCoin <= 0 {
		return 0, fmt.Errorf("lệnh đóng %s không khớp được gì (trạng thái %s)", leg, order.Status)
	}
	return order.FilledQtyCoin, nil
}
