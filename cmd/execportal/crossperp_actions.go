// Engine 2's two order paths and the four endpoints of PLAN 4.5k step 4.
//
// openPair and closePair are the ONLY functions in this command that reach
// crossperp.Engine, and guard_test.go pins their callers exactly as it pins
// Engine 1's openAs and close. Everything else here reads.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"futures-arbitrage-scanner/internal/coordinator"
	"futures-arbitrage-scanner/internal/execution"
	"futures-arbitrage-scanner/internal/execution/crossperp"
	"futures-arbitrage-scanner/internal/risk"
)

// The action names the page must send in X-Execportal-Action.
const (
	crossOpenAction      = "crossperp-open"
	crossCloseAction     = "crossperp-close"
	crossReconcileAction = "crossperp-reconcile"
	crossUnblockAction   = "crossperp-unblock"
	crossAckMarginAction = "crossperp-ack-margin"
	crossPilotAction     = "crossperp-pilot"
)

// ------------------------------------------------------------------ requests

type crossOpenRequest struct {
	Symbol     string `json:"symbol"`
	LongVenue  string `json:"long_venue"`
	ShortVenue string `json:"short_venue"`

	// Exactly one of these sizes the pair. QtyCoin is converted to quote on the
	// LONG leg's book mid before anything else looks at it, so the number the
	// engine sizes on is always a notional and always says which book it came
	// from (rule 4: the unit is in the name, and the conversion is stated).
	NotionalQuote float64 `json:"notional_quote"`
	QtyCoin       float64 `json:"qty_coin"`

	// ExpectedAPROnCapitalFrac is what this intent competes for the symbol with
	// when both engines want it in the same window. The coordinator NEVER
	// computes it — "net" belongs to internal/strategy (rule 2) — so the caller
	// must also say what the figure has taken off, and an empty basis is refused.
	ExpectedAPROnCapitalFrac float64 `json:"expected_apr_on_capital_frac"`
	ExpectedAPRBasisVI       string  `json:"expected_apr_basis_vi"`
}

type crossCloseRequest struct {
	Symbol string `json:"symbol"`

	// FirstVenue closes first. Empty lets the executor pick the larger leg; the
	// margin guard names the stressed venue instead.
	FirstVenue string `json:"first_venue"`
	ReasonVI   string `json:"reason_vi"`
}

type crossSymbolRequest struct {
	Symbol   string `json:"symbol"`
	IntentID string `json:"intent_id"`
}

type crossAckRequest struct {
	SeenSeq uint64 `json:"seen_seq"`
}

// -------------------------------------------------------------------- views

type crossLegView struct {
	Leg   string `json:"leg"`
	Venue string `json:"venue"`
	Side  string `json:"side"`

	ClientOrderID  string `json:"client_order_id,omitempty"`
	VenueOrderID   string `json:"venue_order_id,omitempty"`
	Status         string `json:"status,omitempty"`
	OrderConfirmed bool   `json:"order_confirmed"`

	FilledQtyCoin     float64 `json:"filled_qty_coin"`
	AvgFillPriceQuote float64 `json:"avg_fill_price_quote"`
	LimitPriceQuote   float64 `json:"limit_price_quote"`
	ClosedQtyCoin     float64 `json:"closed_qty_coin"`

	VenuePositionQtyCoin float64 `json:"venue_position_qty_coin"`
	VenuePositionRead    bool    `json:"venue_position_read"`
	FilledAtMs           int64   `json:"filled_at_ms,omitempty"`
}

func toCrossLegView(l crossperp.LegResult) crossLegView {
	return crossLegView{
		Leg: string(l.Leg), Venue: l.Venue, Side: string(l.Side),
		ClientOrderID: l.ClientOrderID, VenueOrderID: l.VenueOrderID, Status: string(l.Status),
		OrderConfirmed: l.OrderConfirmed,
		FilledQtyCoin:  l.FilledQtyCoin, AvgFillPriceQuote: l.AvgFillPriceQuote,
		LimitPriceQuote: l.LimitPriceQuote, ClosedQtyCoin: l.ClosedQtyCoin,
		VenuePositionQtyCoin: l.VenuePositionQtyCoin, VenuePositionRead: l.VenuePositionRead,
		FilledAtMs: l.FilledAtMs,
	}
}

type crossPendingView struct {
	Venue         string `json:"venue"`
	Leg           string `json:"leg"`
	ClientOrderID string `json:"client_order_id"`
	WhyVI         string `json:"why_vi"`
}

type crossOpenView struct {
	IntentID   string `json:"intent_id"`
	Symbol     string `json:"symbol"`
	LongVenue  string `json:"long_venue"`
	ShortVenue string `json:"short_venue"`

	NotionalQuote float64 `json:"notional_quote"`
	SizeBasisVI   string  `json:"size_basis_vi"`

	Outcome              string `json:"outcome"`
	Hedged               bool   `json:"hedged"`
	RefusedBeforePlacing bool   `json:"refused_before_placing"`
	LockRefused          bool   `json:"lock_refused"`

	// Alarm is the one flag the page turns red on: an order that may still
	// execute stands beside whatever the positions read.
	Alarm bool `json:"alarm"`

	TargetQtyCoin         float64 `json:"target_qty_coin"`
	CommonStepCoin        float64 `json:"common_step_coin"`
	ToleranceQtyCoin      float64 `json:"tolerance_qty_coin"`
	DeltaImbalanceQtyCoin float64 `json:"delta_imbalance_qty_coin"`
	ReducedToMatch        bool    `json:"reduced_to_match"`

	RefMidQuote  float64 `json:"ref_mid_quote"`
	BookAgeMs    int64   `json:"book_age_ms"`
	EntryCostPct float64 `json:"entry_cost_pct"`
	WidenBps     float64 `json:"widen_bps"`

	UnhedgedWindowMs int64 `json:"unhedged_window_ms"`
	UnwindDurationMs int64 `json:"unwind_duration_ms"`
	ElapsedMs        int64 `json:"elapsed_ms"`
	OpenedAtMs       int64 `json:"opened_at_ms"`

	Long          crossLegView       `json:"long"`
	Short         crossLegView       `json:"short"`
	PendingOrders []crossPendingView `json:"pending_orders,omitempty"`

	EvidenceVI string   `json:"evidence_vi,omitempty"`
	ReasonVI   string   `json:"reason_vi,omitempty"`
	ErrorVI    string   `json:"error_vi,omitempty"`
	EventsVI   []string `json:"events_vi,omitempty"`
}

type crossCloseView struct {
	IntentID   string `json:"intent_id"`
	Symbol     string `json:"symbol"`
	LongVenue  string `json:"long_venue"`
	ShortVenue string `json:"short_venue"`

	Outcome     string `json:"outcome"`
	AlreadyFlat bool   `json:"already_flat"`
	Refused     bool   `json:"refused"`
	Alarm       bool   `json:"alarm"`
	FirstLeg    string `json:"first_leg,omitempty"`
	FirstVenue  string `json:"first_venue,omitempty"`

	LongBeforeQtyCoin  float64 `json:"long_before_qty_coin"`
	ShortBeforeQtyCoin float64 `json:"short_before_qty_coin"`

	OpeningOrdersProven bool `json:"opening_orders_proven"`
	RestingOrders       int  `json:"resting_orders"`

	UnhedgedWindowMs int64 `json:"unhedged_window_ms"`
	Rounds           int   `json:"rounds"`
	ElapsedMs        int64 `json:"elapsed_ms"`

	// LockReleased is the fact the operator actually needs: the coordinator gave
	// the symbol back to idle, which happens only after the VENUES proved flat.
	LockReleased bool   `json:"lock_released"`
	LockStateVI  string `json:"lock_state_vi"`

	Long          crossLegView       `json:"long"`
	Short         crossLegView       `json:"short"`
	PendingOrders []crossPendingView `json:"pending_orders,omitempty"`

	ReasonVI string   `json:"reason_vi,omitempty"`
	ErrorVI  string   `json:"error_vi,omitempty"`
	EventsVI []string `json:"events_vi,omitempty"`
}

type crossPairView struct {
	IntentID     string  `json:"intent_id"`
	Symbol       string  `json:"symbol"`
	LongVenue    string  `json:"long_venue"`
	ShortVenue   string  `json:"short_venue"`
	LongQtyCoin  float64 `json:"long_qty_coin"`
	ShortQtyCoin float64 `json:"short_qty_coin"`

	LongAvgFillPriceQuote  float64 `json:"long_avg_fill_price_quote"`
	ShortAvgFillPriceQuote float64 `json:"short_avg_fill_price_quote"`
	OpenedAtMs             int64   `json:"opened_at_ms"`

	Adopted               bool               `json:"adopted"`
	Unresolved            bool               `json:"unresolved"`
	OpeningOrdersUnproven bool               `json:"opening_orders_unproven"`
	CloseBlockedVI        string             `json:"close_blocked_vi,omitempty"`
	PendingOrders         []crossPendingView `json:"pending_orders,omitempty"`
}

type crossVenueView struct {
	Name     string `json:"name"`
	Ready    bool   `json:"ready"`
	SourceVI string `json:"source_vi,omitempty"`
	ErrorVI  string `json:"error_vi,omitempty"`
}

type crossStatusView struct {
	Enabled bool `json:"enabled"`

	// DisabledVI says why Engine 2 is not on, so the page never has to guess
	// between "off" and "broken".
	DisabledVI string `json:"disabled_vi,omitempty"`

	Venues  []crossVenueView `json:"venues"`
	Symbols []string         `json:"symbols"`

	ReconciledAtMs int64  `json:"reconciled_at_ms"`
	ReconcileVI    string `json:"reconcile_vi,omitempty"`
	ReconcileErrVI string `json:"reconcile_err_vi,omitempty"`
	LockFileVI     string `json:"lock_file_vi,omitempty"`

	Pairs []crossPairView `json:"pairs"`

	// HeldLocks are symbols whose lock Engine 2 keeps although the venues read
	// flat: only the release itself failed.
	HeldLocks map[string]string `json:"held_locks,omitempty"`

	MaxNotionalQuote float64 `json:"max_notional_quote"`
	MaxSlippageBps   float64 `json:"max_slippage_bps"`

	Pilot *crossPilotView `json:"pilot,omitempty"`

	LastOpen  *crossOpenView   `json:"last_open,omitempty"`
	LastClose *crossCloseView  `json:"last_close,omitempty"`
	Events    []crossEventLine `json:"events,omitempty"`
	ReadAtMs  int64            `json:"read_at_ms"`
}

type crossLocksView struct {
	Enabled        bool                     `json:"enabled"`
	DisabledVI     string                   `json:"disabled_vi,omitempty"`
	ReconciledAtMs int64                    `json:"reconciled_at_ms"`
	Unverified     map[string]string        `json:"unverified,omitempty"`
	Locks          []coordinator.SymbolLock `json:"locks"`
	ReadAtMs       int64                    `json:"read_at_ms"`
}

type crossMarginVenueView struct {
	Venue      string  `json:"venue"`
	Tier       string  `json:"tier"`
	RatioFrac  float64 `json:"ratio_frac"`
	RatioPct   float64 `json:"ratio_pct"`
	HasReading bool    `json:"has_reading"`
	ReadAtMs   int64   `json:"read_at_ms"`
	AgeMs      int64   `json:"age_ms"`
	SourceVI   string  `json:"source_vi,omitempty"`
	ProblemVI  string  `json:"problem_vi,omitempty"`
}

type crossMarginView struct {
	Enabled    bool   `json:"enabled"`
	DisabledVI string `json:"disabled_vi,omitempty"`

	Venues       []crossMarginVenueView `json:"venues"`
	Emergency    bool                   `json:"emergency"`
	EmergencySeq uint64                 `json:"emergency_seq"`

	// The tier table, so the page never hardcodes it.
	YellowFrac  float64 `json:"yellow_frac"`
	OrangeFrac  float64 `json:"orange_frac"`
	RedFrac     float64 `json:"red_frac"`
	ReleaseFrac float64 `json:"release_frac"`

	Events   []crossEventLine `json:"events,omitempty"`
	AtMs     int64            `json:"at_ms"`
	ReadAtMs int64            `json:"read_at_ms"`
}

// manualOpenNoWidenTolerance is +Inf: "do not check the widening tolerance".
// It is a package variable because Intent takes a POINTER, and a pointer to a
// fresh local on every call would say the same thing more expensively.
var manualOpenNoWidenTolerance = math.Inf(1)

// --------------------------------------------------------------- order paths

// openPair is the ONE place in this command that opens an Engine-2 pair.
//
// It reads both venues' rules and books, hands the engine an intent, and turns
// what comes back into JSON. Every refusal — the lock, the margin guard, the
// books, the sizes — happens inside the engine and its executor, where it is
// tested; nothing here may decide that an order is safe to send.
func (d *crossDesk) openPair(ctx context.Context, req crossOpenRequest, intentID string) (crossOpenView, int) {
	v := crossOpenView{
		IntentID: intentID, Symbol: req.Symbol, LongVenue: req.LongVenue, ShortVenue: req.ShortVenue,
		Outcome: string(crossperp.OutcomeBothFlat), RefusedBeforePlacing: true,
	}
	longVenue, ok := d.venues.get(req.LongVenue)
	if !ok {
		v.ErrorVI = fmt.Sprintf("sàn long %s không thuộc bàn Động cơ 2 — chưa gửi lệnh nào", quoteForMessage(req.LongVenue))
		return v, http.StatusBadRequest
	}
	shortVenue, ok := d.venues.get(req.ShortVenue)
	if !ok {
		v.ErrorVI = fmt.Sprintf("sàn short %s không thuộc bàn Động cơ 2 — chưa gửi lệnh nào", quoteForMessage(req.ShortVenue))
		return v, http.StatusBadRequest
	}

	long, err := d.legSpec(ctx, longVenue, req.Symbol)
	if err != nil {
		v.ErrorVI = "chân long: " + err.Error() + " — chưa gửi lệnh nào"
		return v, http.StatusBadGateway
	}
	short, err := d.legSpec(ctx, shortVenue, req.Symbol)
	if err != nil {
		v.ErrorVI = "chân short: " + err.Error() + " — chưa gửi lệnh nào"
		return v, http.StatusBadGateway
	}

	notionalQuote, sizeBasisVI, err := crossNotionalQuote(req, long)
	if err != nil {
		v.ErrorVI = err.Error() + " — chưa gửi lệnh nào"
		return v, http.StatusUnprocessableEntity
	}
	v.NotionalQuote, v.SizeBasisVI = notionalQuote, sizeBasisVI

	startedAt := d.now()
	res, openErr := d.engine2.Open(ctx, crossperp.OpenRequest{
		Intent: crossperp.Intent{
			ID: intentID, Symbol: req.Symbol, Long: long, Short: short,
			NotionalQuote: notionalQuote,
			// A button press is its own signal: the cost the decision was made
			// at IS the cost these books price now, so no widening tolerance
			// applies — the same exemption Engine 1's openAs has had since 4.4b.
			// Without it every press is refused by however much the book itself
			// costs, which on the Bybit testnet's ADAUSDT is 12.1 bps against a
			// 5 bps tolerance. The cost is still MEASURED and reported
			// (EntryCostPct), and the PILOT keeps the shipped tolerance, because
			// a signal that priced an entry a minute ago has something real to
			// widen from.
			SignalEntryCostPct:           0,
			MaxEntryCostWidenBpsOverride: &manualOpenNoWidenTolerance,
		},
		PriorityAPROnCapitalFrac: req.ExpectedAPROnCapitalFrac,
		PriorityAPRBasisVI:       req.ExpectedAPRBasisVI,
		DetailsVI: fmt.Sprintf("Động cơ 2: long %s / short %s, %.2f quote mỗi chân",
			req.LongVenue, req.ShortVenue, notionalQuote),
	})
	v.ElapsedMs = time.Since(startedAt).Milliseconds()
	v.OpenedAtMs = startedAt.UnixMilli()

	d.fillOpenView(&v, res, openErr)
	d.mu.Lock()
	snapshot := v
	d.lastOpen = &snapshot
	d.mu.Unlock()

	status := http.StatusOK
	switch {
	case v.LockRefused:
		status = http.StatusConflict
	case openErr != nil && v.RefusedBeforePlacing:
		status = http.StatusUnprocessableEntity
	case v.Alarm:
		status = http.StatusConflict
	}
	return v, status
}

func (d *crossDesk) fillOpenView(v *crossOpenView, res crossperp.Result, openErr error) {
	v.Outcome, v.Hedged, v.ReducedToMatch = string(res.Outcome), res.Hedged(), res.ReducedToMatch
	v.LockRefused = errors.Is(openErr, coordinator.ErrOccupied) || errors.Is(openErr, coordinator.ErrConflict) ||
		errors.Is(openErr, coordinator.ErrLostContest) || errors.Is(openErr, coordinator.ErrNotReconciled)
	// A lock refusal happens BEFORE the executor is reached, so nothing was
	// sent — but the coordinator's sentinels are its own and do not wrap
	// crossperp.ErrRefusedBeforePlacing. Left to the wrap alone the page would
	// report refused_before_placing=false for the one refusal that is most
	// certainly before placing, which reads as "something may be on a venue".
	v.RefusedBeforePlacing = errors.Is(openErr, crossperp.ErrRefusedBeforePlacing) || v.LockRefused
	v.Alarm = errors.Is(openErr, execution.ErrUnwindIncomplete) || errors.Is(openErr, execution.ErrFlatEvidenceConflict)
	v.TargetQtyCoin, v.CommonStepCoin, v.ToleranceQtyCoin = res.TargetQtyCoin, res.CommonStepCoin, res.ToleranceQtyCoin
	v.DeltaImbalanceQtyCoin = res.DeltaImbalanceQtyCoin
	v.RefMidQuote, v.BookAgeMs, v.EntryCostPct, v.WidenBps = res.RefMidQuote, res.BookAgeMs, res.EntryCostPct, res.WidenBps
	v.UnhedgedWindowMs = res.UnhedgedWindow.Milliseconds()
	v.UnwindDurationMs = res.UnwindDuration.Milliseconds()
	v.Long, v.Short = toCrossLegView(res.Long), toCrossLegView(res.Short)
	v.PendingOrders = toPendingViews(res.PendingOrders)
	v.EvidenceVI, v.ReasonVI = res.EvidenceVI, res.ReasonVI
	if openErr != nil {
		v.ErrorVI = openErr.Error()
	}
	v.EventsVI = d.eventsFor(res.IntentID)
}

// closePair is the ONE place in this command that closes an Engine-2 pair by a
// person's press. The margin guard's emergency close does NOT come through here
// — it calls the engine directly, because an emergency must not wait behind a
// page's write lock (risk.EmergencyCloser).
func (d *crossDesk) closePair(ctx context.Context, req crossCloseRequest) (crossCloseView, int) {
	v := crossCloseView{Symbol: req.Symbol, FirstVenue: req.FirstVenue, Refused: true,
		Outcome: string(crossperp.OutcomeBothOpen), RestingOrders: -1}

	if req.FirstVenue != "" {
		if _, ok := d.venues.get(req.FirstVenue); !ok {
			v.ErrorVI = fmt.Sprintf("first_venue %s không thuộc bàn Động cơ 2 — chưa gửi lệnh nào", quoteForMessage(req.FirstVenue))
			return v, http.StatusBadRequest
		}
	}
	for _, p := range d.engine2.Pairs() {
		if p.Symbol == req.Symbol {
			v.IntentID, v.LongVenue, v.ShortVenue = p.IntentID, p.Long.Venue.Name, p.Short.Venue.Name
		}
	}

	reasonVI := strings.TrimSpace(req.ReasonVI)
	if reasonVI == "" {
		reasonVI = "người vận hành bấm ĐÓNG trên portal"
	}
	startedAt := d.now()
	res, closeErr := d.engine2.Close(ctx, req.Symbol, req.FirstVenue, reasonVI)
	v.ElapsedMs = time.Since(startedAt).Milliseconds()

	v.IntentID, v.Outcome, v.AlreadyFlat = res.IntentID, string(res.Outcome), res.AlreadyFlat
	v.Refused = errors.Is(closeErr, crossperp.ErrCloseRefused) || errors.Is(closeErr, crossperp.ErrUnknownPair) ||
		errors.Is(closeErr, crossperp.ErrCloseBlocked)
	v.Alarm = errors.Is(closeErr, execution.ErrUnwindIncomplete) || errors.Is(closeErr, execution.ErrFlatEvidenceConflict)
	v.FirstLeg = string(res.FirstLeg)
	v.LongBeforeQtyCoin, v.ShortBeforeQtyCoin = res.LongBeforeQtyCoin, res.ShortBeforeQtyCoin
	v.OpeningOrdersProven, v.RestingOrders = res.OpeningOrdersProven, res.RestingOrders
	v.UnhedgedWindowMs, v.Rounds = res.UnhedgedWindow.Milliseconds(), res.Rounds
	v.Long, v.Short = toCrossLegView(res.Long), toCrossLegView(res.Short)
	v.PendingOrders = toPendingViews(res.PendingOrders)
	v.ReasonVI = res.ReasonVI
	if closeErr != nil {
		v.ErrorVI = closeErr.Error()
	}
	v.EventsVI = d.eventsFor(res.IntentID)

	// Q22, and the fact the operator is actually asking about: the lock goes
	// back to idle only when coordinator.Release has PROVEN both venues flat and
	// quiet — it re-reads GetPosition and the resting orders on every venue. So
	// the page reports the lock's state as the coordinator holds it now, never
	// as this close believes it should be.
	lock, held := d.coord.QueryLock(req.Symbol)
	switch {
	case !held:
		v.LockReleased, v.LockStateVI = true, "khóa đã NHẢ — symbol về IDLE"
	default:
		v.LockReleased = lock.State == coordinator.StateIdle
		v.LockStateVI = fmt.Sprintf("khóa %s (chủ %s): %s", lock.State, lock.OwnerEngine, lock.EvidenceVI)
	}

	d.mu.Lock()
	snapshot := v
	d.lastClose = &snapshot
	d.mu.Unlock()

	switch {
	case v.Refused:
		return v, http.StatusConflict
	case v.Alarm:
		return v, http.StatusConflict
	case closeErr != nil:
		return v, http.StatusBadGateway
	}
	return v, http.StatusOK
}

func toPendingViews(pending []crossperp.PendingOrder) []crossPendingView {
	out := make([]crossPendingView, 0, len(pending))
	for _, o := range pending {
		out = append(out, crossPendingView{Venue: o.Venue, Leg: string(o.Leg),
			ClientOrderID: o.ClientOrderID, WhyVI: pendingWhyVI(o)})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// pendingWhyVI says, in the operator's language, WHY one reduce-only order is
// still not proven finished — which is the difference between "keep watching"
// and "the venue told us it does not know".
func pendingWhyVI(o crossperp.PendingOrder) string {
	sent := time.UnixMilli(o.SendReturnedAtMs).Format("15:04:05.000")
	switch {
	case o.StatusUnknown:
		return "sàn TRẢ LỜI rằng KHÔNG BIẾT lệnh có thực thi hay không (execution status unknown) — gửi xong lúc " + sent +
			"; không bao giờ coi là vắng, khóa được GIỮ"
	case o.Seen:
		return "sàn ĐÃ cho thấy lệnh này nhưng chưa chứng minh được là kết thúc — gửi xong lúc " + sent + "; khóa được GIỮ"
	default:
		return "mất câu trả lời khi gửi, sàn chưa từng cho thấy lệnh — gửi xong lúc " + sent +
			"; chỉ được coi là vắng SAU khoảng lặng, và chỉ khi vị thế cả hai sàn đọc về 0 (Q22)"
	}
}

func (d *crossDesk) eventsFor(intentID string) []string {
	if intentID == "" {
		return nil
	}
	var out []string
	for _, l := range d.events.recent() {
		if l.IntentID != intentID {
			continue
		}
		out = append(out, fmt.Sprintf("%s  %s", time.UnixMilli(l.AtMs).Format("15:04:05.000"), l.MessageVI))
	}
	return out
}

// crossNotionalQuote turns whichever size the caller gave into ONE leg's
// notional, and says which book priced it. A request naming both, or neither,
// is refused rather than one being preferred silently.
func crossNotionalQuote(req crossOpenRequest, long crossperp.LegSpec) (float64, string, error) {
	hasNotional := req.NotionalQuote != 0
	hasQty := req.QtyCoin != 0
	switch {
	case hasNotional == hasQty:
		return 0, "", errors.New("phải nêu ĐÚNG MỘT trong notional_quote và qty_coin")
	case hasNotional:
		if math.IsNaN(req.NotionalQuote) || math.IsInf(req.NotionalQuote, 0) || req.NotionalQuote <= 0 {
			return 0, "", fmt.Errorf("notional_quote phải là số dương, nhận %v", req.NotionalQuote)
		}
		if req.NotionalQuote > crossMaxNotionalQuote {
			return 0, "", fmt.Errorf("notional_quote %.2f vượt trần %.0f quote mỗi chân của Động cơ 2",
				req.NotionalQuote, crossMaxNotionalQuote)
		}
		return req.NotionalQuote, "người vận hành nhập notional trực tiếp", nil
	default:
		if math.IsNaN(req.QtyCoin) || math.IsInf(req.QtyCoin, 0) || req.QtyCoin <= 0 {
			return 0, "", fmt.Errorf("qty_coin phải là số dương, nhận %v", req.QtyCoin)
		}
		mid := long.Book.MidPriceQuote
		if mid <= 0 || math.IsNaN(mid) {
			return 0, "", fmt.Errorf("sổ lệnh %s không có giá giữa để quy đổi qty_coin ra notional", long.Venue.Name)
		}
		notional := req.QtyCoin * mid
		if notional > crossMaxNotionalQuote {
			return 0, "", fmt.Errorf("qty_coin %.8f × giá giữa %.2f = %.2f quote, vượt trần %.0f quote mỗi chân của Động cơ 2",
				req.QtyCoin, mid, notional, crossMaxNotionalQuote)
		}
		return notional, fmt.Sprintf("quy đổi từ qty_coin %.8f × giá giữa %.2f của sổ %s",
			req.QtyCoin, mid, long.Venue.Name), nil
	}
}

// ---------------------------------------------------------------- read views

func (d *crossDesk) statusView() crossStatusView {
	v := crossStatusView{Enabled: true, Symbols: d.symbols,
		MaxNotionalQuote: crossMaxNotionalQuote, MaxSlippageBps: d.settings.MaxSlippageBps,
		ReadAtMs: d.now().UnixMilli()}
	for _, ven := range d.venues.list {
		view := crossVenueView{Name: ven.Name, Ready: ven.Perp != nil, SourceVI: ven.SourceVI}
		if ven.Err != nil {
			view.ErrorVI = ven.Err.Error()
		}
		v.Venues = append(v.Venues, view)
	}
	for _, p := range d.engine2.Pairs() {
		v.Pairs = append(v.Pairs, crossPairView{
			IntentID: p.IntentID, Symbol: p.Symbol,
			LongVenue: p.Long.Venue.Name, ShortVenue: p.Short.Venue.Name,
			LongQtyCoin: p.LongQtyCoin, ShortQtyCoin: p.ShortQtyCoin,
			LongAvgFillPriceQuote: p.LongAvgFillPriceQuote, ShortAvgFillPriceQuote: p.ShortAvgFillPriceQuote,
			OpenedAtMs: p.OpenedAtMs, Adopted: p.Adopted, Unresolved: p.Unresolved,
			OpeningOrdersUnproven: p.OpeningOrdersUnproven, CloseBlockedVI: p.CloseBlockedVI,
			PendingOrders: toPendingViews(p.PendingOrders),
		})
	}
	sort.Slice(v.Pairs, func(i, j int) bool { return v.Pairs[i].Symbol < v.Pairs[j].Symbol })
	if held := d.engine2.Held(); len(held) > 0 {
		v.HeldLocks = map[string]string{}
		for symbol, err := range held {
			v.HeldLocks[symbol] = err.Error()
		}
	}
	d.mu.Lock()
	v.ReconciledAtMs, v.ReconcileVI, v.ReconcileErrVI = d.reconciledAtMs, d.reconcileVI, d.reconcileErrVI
	v.LastOpen, v.LastClose = d.lastOpen, d.lastClose
	d.mu.Unlock()
	v.LockFileVI = d.loadVI
	v.Events = d.events.recent()
	return v
}

func (d *crossDesk) locksView() crossLocksView {
	st := d.coord.Status()
	v := crossLocksView{Enabled: true, ReconciledAtMs: st.ReconciledAtMs,
		Locks: st.Locks, ReadAtMs: d.now().UnixMilli()}
	if len(st.Unverified) > 0 {
		v.Unverified = st.Unverified
	}
	if v.Locks == nil {
		v.Locks = []coordinator.SymbolLock{}
	}
	sort.Slice(v.Locks, func(i, j int) bool { return v.Locks[i].Symbol < v.Locks[j].Symbol })
	return v
}

func (d *crossDesk) marginView() crossMarginView {
	snap := d.marginGuard.Snapshot()
	t := risk.DefaultMarginThresholds()
	v := crossMarginView{Enabled: true, Emergency: snap.Emergency, EmergencySeq: snap.EmergencySeq,
		YellowFrac: t.YellowFrac, OrangeFrac: t.OrangeFrac, RedFrac: t.RedFrac, ReleaseFrac: t.ReleaseFrac,
		AtMs: snap.AtMs, ReadAtMs: d.now().UnixMilli()}
	for _, ven := range snap.Venues {
		v.Venues = append(v.Venues, crossMarginVenueView{
			Venue: ven.Venue, Tier: string(ven.Tier), RatioFrac: ven.RatioFrac, RatioPct: ven.RatioFrac * 100,
			HasReading: ven.HasReading, ReadAtMs: ven.ReadAtMs, AgeMs: ven.AgeMs,
			SourceVI: ven.SourceVI, ProblemVI: ven.ProblemVI,
		})
	}
	sort.Slice(v.Venues, func(i, j int) bool { return v.Venues[i].Venue < v.Venues[j].Venue })
	v.Events = d.events.recentMargin()
	return v
}

// ----------------------------------------------------------------- handlers

// crossOff answers every Engine-2 route when the desk is not wired, and says
// which of the two reasons it is.
func (p *portal) crossOff(w http.ResponseWriter, status int) bool {
	if p.cross != nil {
		return false
	}
	writeJSON(w, status, map[string]any{
		"enabled":     false,
		"disabled_vi": p.crossOffVI,
	})
	return true
}

func (p *portal) handleCrossStatus(w http.ResponseWriter, r *http.Request) {
	if p.crossOff(w, http.StatusOK) {
		return
	}
	v := p.cross.statusView()
	v.Pilot = p.cross.pilotView()
	writeJSON(w, http.StatusOK, v)
}

func (p *portal) handleCoordinatorLocks(w http.ResponseWriter, r *http.Request) {
	if p.crossOff(w, http.StatusOK) {
		return
	}
	writeJSON(w, http.StatusOK, p.cross.locksView())
}

func (p *portal) handleRiskMargin(w http.ResponseWriter, r *http.Request) {
	if p.crossOff(w, http.StatusOK) {
		return
	}
	writeJSON(w, http.StatusOK, p.cross.marginView())
}

func (p *portal) handleCrossOpen(w http.ResponseWriter, r *http.Request) {
	var req crossOpenRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if p.crossOff(w, http.StatusServiceUnavailable) {
		return
	}
	symbol, err := p.allowedSymbol(req.Symbol)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error()+" — lệnh KHÔNG được gửi")
		return
	}
	req.Symbol = symbol
	switch {
	case req.LongVenue == req.ShortVenue:
		writeError(w, http.StatusBadRequest, "bad_request",
			"long_venue và short_venue phải là HAI sàn khác nhau — đây là cặp chéo sàn; lệnh KHÔNG được gửi")
		return
	case strings.TrimSpace(req.ExpectedAPRBasisVI) == "":
		writeError(w, http.StatusBadRequest, "bad_request",
			"expected_apr_basis_vi bắt buộc: bộ điều phối KHÔNG tự tính 'ròng', nó chỉ so con số người gọi đưa và cần biết con số đó đã trừ những gì; lệnh KHÔNG được gửi")
		return
	}

	release, heldBy, since, ok := p.acquire(crossOpenAction)
	if !ok {
		p.busy(w, heldBy, since)
		return
	}
	defer release()
	defer p.afterOrderWrite()

	intentID, err := p.mintIntentIDWith(crossIntentPrefix, symbol)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "intent_id", err.Error()+" — chưa gửi lệnh nào")
		return
	}
	ctx, cancel := p.actionContext(r)
	defer cancel()
	view, status := p.cross.openPair(ctx, req, intentID)
	log.Printf("execportal/crossperp: MỞ %s %s long=%s short=%s %.2f quote → %s hedged=%v báo động=%v %s",
		view.IntentID, view.Symbol, view.LongVenue, view.ShortVenue, view.NotionalQuote,
		view.Outcome, view.Hedged, view.Alarm, view.ErrorVI)
	writeJSON(w, status, view)
}

func (p *portal) handleCrossClose(w http.ResponseWriter, r *http.Request) {
	var req crossCloseRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if p.crossOff(w, http.StatusServiceUnavailable) {
		return
	}
	symbol, err := p.allowedSymbol(req.Symbol)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error()+" — lệnh KHÔNG được gửi")
		return
	}
	req.Symbol = symbol

	release, heldBy, since, ok := p.acquire(crossCloseAction)
	if !ok {
		p.busy(w, heldBy, since)
		return
	}
	defer release()
	defer p.afterOrderWrite()

	ctx, cancel := p.actionContext(r)
	defer cancel()
	view, status := p.cross.closePair(ctx, req)
	log.Printf("execportal/crossperp: ĐÓNG %s %s → %s khóa nhả=%v báo động=%v %s",
		view.IntentID, view.Symbol, view.Outcome, view.LockReleased, view.Alarm, view.ErrorVI)
	writeJSON(w, status, view)
}

// handleCrossReconcile re-reads every venue and rebuilds the lock table. It
// sends no order, which is why it is the one Engine-2 write a person may run
// while wondering what is going on.
func (p *portal) handleCrossReconcile(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if !decodeBody(w, r, &req) {
		return
	}
	if p.crossOff(w, http.StatusServiceUnavailable) {
		return
	}
	ctx, cancel := p.actionContext(r)
	defer cancel()
	if err := p.cross.reconcileLocks(ctx); err != nil {
		writeError(w, http.StatusBadGateway, "reconcile_failed", "đối soát thất bại: "+err.Error())
		return
	}
	p.cross.adoptPairs(ctx)
	writeJSON(w, http.StatusOK, p.cross.locksView())
}

// handleCrossUnblock is a person saying, about ONE pair, either "I have looked
// at the conflicting evidence" (UnblockClose) or "I have proven those orders are
// finished" (ConfirmOrdersFinished). Both send nothing; both exist because Q21
// forbids the machine from resolving either on its own.
func (p *portal) handleCrossUnblock(w http.ResponseWriter, r *http.Request) {
	var req crossSymbolRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if p.crossOff(w, http.StatusServiceUnavailable) {
		return
	}
	symbol, err := p.allowedSymbol(req.Symbol)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.IntentID != "" {
		if err := p.cross.confirmOrders(symbol, req.IntentID); err != nil {
			writeError(w, http.StatusConflict, "confirm_refused", err.Error())
			return
		}
	} else if err := p.cross.unblockClose(symbol); err != nil {
		writeError(w, http.StatusConflict, "unblock_refused", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p.cross.statusView())
}

// handleCrossPilot is a person clearing the pilot's halt after looking at the
// evidence that raised it. It clears NOTHING about that evidence: the next scan
// re-reads the engine and halts again if it is still there, which is what makes
// this safe to expose (Q21 says a person decides, not that a person's click
// makes the conflict go away).
func (p *portal) handleCrossPilot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if p.crossOff(w, http.StatusServiceUnavailable) {
		return
	}
	if req.Action != "resume" {
		writeError(w, http.StatusBadRequest, "bad_request",
			`action phải là "resume" — phi công chỉ bật/tắt gửi lệnh bằng cờ -crossperp-pilot lúc khởi động`)
		return
	}
	p.cross.resumePilot()
	writeJSON(w, http.StatusOK, p.cross.statusView())
}

func (p *portal) handleCrossAckMargin(w http.ResponseWriter, r *http.Request) {
	var req crossAckRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if p.crossOff(w, http.StatusServiceUnavailable) {
		return
	}
	if err := p.cross.acknowledgeMargin(req.SeenSeq); err != nil {
		writeError(w, http.StatusConflict, "ack_refused", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p.cross.marginView())
}

// The three one-line wrappers exist so guard_test can pin who reaches the
// engine and the guard: a handler names the desk, and only the desk names them.
func (d *crossDesk) unblockClose(symbol string) error { return d.engine2.UnblockClose(symbol) }

func (d *crossDesk) confirmOrders(symbol, intentID string) error {
	return d.engine2.ConfirmOrdersFinished(symbol, intentID)
}

func (d *crossDesk) acknowledgeMargin(seenSeq uint64) error {
	return d.marginGuard.Acknowledge(seenSeq)
}

func (d *crossDesk) resumePilot() {
	if d.pilot != nil {
		d.pilot.Resume()
	}
}

// runMarginGuard is the 5-second dual read. It is started by main and ends with
// the process; the guard itself decides when to close, and calls the engine
// directly rather than through the page's write lock.
func (d *crossDesk) runMarginGuard(ctx context.Context) {
	if err := d.marginGuard.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("execportal/crossperp: van ký quỹ dừng: %v", err)
	}
}

// retryReleases gives back the locks Engine 2 kept only because the release
// itself failed. It reads the venues and sends nothing.
func (d *crossDesk) retryReleases(ctx context.Context) {
	for symbol, err := range d.engine2.RetryReleases(ctx) {
		if err != nil {
			log.Printf("execportal/crossperp: %s — nhả khóa lại vẫn hỏng: %v", symbol, err)
		} else {
			log.Printf("execportal/crossperp: %s — đã nhả được khóa còn treo", symbol)
		}
	}
}
