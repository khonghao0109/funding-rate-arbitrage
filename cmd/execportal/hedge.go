package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/execution"
)

// Is the account hedged? Answered from the VENUE, twice over.
//
// The spot market has no position, only a balance, and a testnet account is
// funded with coin before anything is traded — so "spot balance minus perp
// position" measures the faucet, not the hedge. What the spot leg holds
// BECAUSE OF THIS STRATEGY is read the way execcheck -status reads it: every
// order an intent could have produced, looked up at the venue by the
// ClientOrderID derived from the intent id, buys minus sells.
//
// The perp leg has a position, so it is read directly — and ALSO summed from
// the intents' own orders. The two answer the same question from different
// directions, and when they disagree the page says evidence_conflict with both
// numbers instead of picking one: a perp position no intent explains is either
// somebody else's or one of ours nobody is tracking, and a tool that squared it
// would be trading a position it cannot name.

// gridEpsilon absorbs float64 noise on quantities that ARE on the venue's grid,
// the same allowance internal/execution uses.
const gridEpsilon = 1e-9

// hedgeStatus is the answer, as a value.
type hedgeStatus string

const (
	statusBothOpen hedgeStatus = "both_open"
	statusBothFlat hedgeStatus = "both_flat"
	statusUnhedged hedgeStatus = "unhedged"
	// statusEvidenceConflict is the venue's perp position disagreeing with the
	// perp quantity the intents' own orders account for.
	statusEvidenceConflict hedgeStatus = "evidence_conflict"
	// statusUnknown is a read that failed or an order still working. It is
	// never folded into flat: an order nobody could read has an unknown size.
	statusUnknown hedgeStatus = "unknown"
)

var statusVI = map[hedgeStatus]string{
	statusBothOpen:         "DELTA-NEUTRAL (HEDGED)",
	statusBothFlat:         "PHẲNG CẢ HAI CHÂN",
	statusUnhedged:         "UNHEDGED DELTA RISK",
	statusEvidenceConflict: "HAI BẰNG CHỨNG PERP KHÔNG KHỚP",
	statusUnknown:          "CHƯA XÁC ĐỊNH ĐƯỢC",
}

// orderRef is one ClientOrderID an intent could have produced on one leg.
type orderRef struct {
	kind string // mở | đóng | gỡ | cân
	id   string
}

// intentOrderIDs lists all four derived ids for one leg. The reconcile id is
// the one kind that matters beyond the sum: planSquare refuses to reuse it.
func intentOrderIDs(intentID string, leg execution.LegName) []orderRef {
	return []orderRef{
		{"mở", execution.LegClientOrderID(intentID, leg)},
		{"đóng", execution.CloseClientOrderID(intentID, leg)},
		{"gỡ", execution.UnwindClientOrderID(intentID, leg)},
		{"cân", execution.ReconcileClientOrderID(intentID, leg)},
	}
}

// legNet is what one market holds because of one intent's own orders.
type legNet struct {
	QtyCoin      float64  `json:"qty_coin"`
	SeenVI       []string `json:"seen_vi"`
	WorkingVI    []string `json:"working_vi"`
	UnreadableVI []string `json:"unreadable_vi"`

	// ReconcileOrderExists and CloseOrderExists are the venue knowing this
	// leg's squaring or closing id. Either one already used is an id that
	// cannot be sent again: a lookup by it could answer about either order.
	ReconcileOrderExists bool `json:"reconcile_order_exists"`
	CloseOrderExists     bool `json:"close_order_exists"`
}

// doneOrders remembers orders the venue says are FINISHED. A filled, cancelled,
// rejected or expired order cannot change at the venue, so reading it again on
// every refresh spends request weight to learn nothing. An order that was not
// found, or is still working, is never remembered.
type doneOrders struct {
	ordersMu sync.Mutex
	orders   map[string]broker.Order
}

func newDoneOrders() *doneOrders { return &doneOrders{orders: map[string]broker.Order{}} }

// forget drops everything remembered. Every write calls it before and after:
// a finished order cannot change, but an id this portal refuses to reuse could
// still be reused by another tool, and a write is the moment that matters.
func (d *doneOrders) forget() {
	d.ordersMu.Lock()
	d.orders = map[string]broker.Order{}
	d.ordersMu.Unlock()
}

func (d *doneOrders) key(q broker.OrderQuery) string {
	return string(q.Market) + "|" + q.Symbol + "|" + q.ClientOrderID
}

func (d *doneOrders) read(ctx context.Context, b broker.Broker, q broker.OrderQuery) (broker.Order, error) {
	d.ordersMu.Lock()
	o, ok := d.orders[d.key(q)]
	d.ordersMu.Unlock()
	if ok {
		return o, nil
	}
	o, err := b.GetOrder(ctx, q)
	if err == nil && o.Status.Done() {
		d.ordersMu.Lock()
		d.orders[d.key(q)] = o
		d.ordersMu.Unlock()
	}
	return o, err
}

func readLegNet(ctx context.Context, b broker.Broker, memo *doneOrders, market broker.Market, symbol, intentID string, leg execution.LegName) legNet {
	var out legNet
	for _, ref := range intentOrderIDs(intentID, leg) {
		q := broker.OrderQuery{Market: market, Symbol: symbol, ClientOrderID: ref.id}
		order, err := memo.read(ctx, b, q)
		switch {
		case errors.Is(err, broker.ErrOrderNotFound):
			continue
		case err != nil:
			// NOT zero: an order nobody could read has an unknown quantity,
			// and reconciling on a guess is how a hedge becomes a position.
			out.UnreadableVI = append(out.UnreadableVI, fmt.Sprintf("%s %s (%s)", ref.kind, leg, err.Error()))
			continue
		}
		switch ref.kind {
		case "cân":
			out.ReconcileOrderExists = true
		case "đóng":
			out.CloseOrderExists = true
		}
		signed := order.FilledQtyCoin
		if order.Side == broker.SideSell {
			signed = -signed
		}
		out.QtyCoin += signed
		if order.FilledQtyCoin > 0 {
			out.SeenVI = append(out.SeenVI, fmt.Sprintf("%s %s %.8f", ref.kind, order.Side, order.FilledQtyCoin))
		}
		if !order.Status.Done() {
			out.WorkingVI = append(out.WorkingVI, fmt.Sprintf("%s %s còn %s trên sàn", ref.kind, leg, order.Status))
		}
	}
	return out
}

// intentHedge is one intent's two legs, as the venue records its orders.
type intentHedge struct {
	IntentID string `json:"intent_id"`
	Spot     legNet `json:"spot"`
	Perp     legNet `json:"perp"`
}

func (h intentHedge) residualCoin() float64 { return h.Spot.QtyCoin + h.Perp.QtyCoin }

func (h intentHedge) unreadable() []string {
	return append(append([]string(nil), h.Spot.UnreadableVI...), h.Perp.UnreadableVI...)
}

func (h intentHedge) working() []string {
	return append(append([]string(nil), h.Spot.WorkingVI...), h.Perp.WorkingVI...)
}

// readIntentHedges reads several intents at once, a few at a time. Each intent
// is eight lookups and a sequential scan of sixteen intents is half a minute of
// round trips; four at a time keeps it to seconds without leaning on the
// weight budget (spot 4, futures 1 per lookup).
func readIntentHedges(ctx context.Context, spot, perp broker.Broker, memo *doneOrders, symbol string, intentIDs []string) []intentHedge {
	out := make([]intentHedge, len(intentIDs))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, id := range intentIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = intentHedge{
				IntentID: id,
				Spot:     readLegNet(ctx, spot, memo, broker.MarketSpot, symbol, id, execution.LegSpot),
				Perp:     readLegNet(ctx, perp, memo, broker.MarketFuturesUSDM, symbol, id, execution.LegPerp),
			}
		}()
	}
	wg.Wait()
	return out
}

// hedgeEvidence is everything classifyHedge judges.
type hedgeEvidence struct {
	// SpotLegQtyCoin and IntentsPerpQtyCoin are sums over the intents' own
	// orders; VenuePerpQtyCoin is the venue's signed position.
	SpotLegQtyCoin     float64
	IntentsPerpQtyCoin float64
	VenuePerpQtyCoin   float64

	// ToleranceQtyCoin is the coarser of the two markets' step sizes. Zero or
	// less means the rules could not be read, and nothing is judged.
	ToleranceQtyCoin float64

	PerpPositionRead bool
	UnreadableVI     []string
	WorkingVI        []string
}

// classifyHedge turns the evidence into one status and says why.
func classifyHedge(ev hedgeEvidence) (hedgeStatus, string) {
	tol := ev.ToleranceQtyCoin + gridEpsilon
	switch {
	case !ev.PerpPositionRead:
		return statusUnknown, "không đọc được vị thế perp từ sàn"
	case ev.ToleranceQtyCoin <= 0:
		return statusUnknown, "không đọc được luật bước khối lượng của sàn, nên không có dung sai để so"
	case len(ev.UnreadableVI) > 0:
		return statusUnknown, fmt.Sprintf("%d lệnh không đọc được từ sàn — số lượng của chúng là ẩn số", len(ev.UnreadableVI))
	case len(ev.WorkingVI) > 0:
		return statusUnknown, fmt.Sprintf("%d lệnh vẫn đang chạy trên sàn — số khớp còn có thể đổi", len(ev.WorkingVI))
	case math.Abs(ev.VenuePerpQtyCoin-ev.IntentsPerpQtyCoin) > tol:
		return statusEvidenceConflict, fmt.Sprintf(
			"SÀN báo vị thế perp %+.8f coin, lệnh của các ý định đang theo dõi giải thích %+.8f coin — không tự hoà giải",
			ev.VenuePerpQtyCoin, ev.IntentsPerpQtyCoin)
	}
	residual := ev.SpotLegQtyCoin + ev.VenuePerpQtyCoin
	switch {
	case math.Abs(ev.SpotLegQtyCoin) <= tol && math.Abs(ev.VenuePerpQtyCoin) <= tol:
		return statusBothFlat, "spot và perp đều bằng 0 trong dung sai"
	case math.Abs(residual) <= tol && ev.SpotLegQtyCoin > tol && ev.VenuePerpQtyCoin < -tol:
		return statusBothOpen, fmt.Sprintf("spot LONG %.8f, perp SHORT %.8f, lệch %+.8f trong dung sai %.8f",
			ev.SpotLegQtyCoin, -ev.VenuePerpQtyCoin, residual, ev.ToleranceQtyCoin)
	}
	return statusUnhedged, fmt.Sprintf("spot %+.8f + perp %+.8f = lệch %+.8f coin, vượt dung sai %.8f",
		ev.SpotLegQtyCoin, ev.VenuePerpQtyCoin, residual, ev.ToleranceQtyCoin)
}

// squarePlan is what reconciling ONE intent would send, or why it would not.
type squarePlan struct {
	IntentID     string  `json:"intent_id"`
	SpotQtyCoin  float64 `json:"spot_qty_coin"`
	PerpQtyCoin  float64 `json:"perp_qty_coin"`
	ResidualCoin float64 `json:"residual_coin"`

	// Action is "none" (balanced), "send" or "refuse".
	Action        string        `json:"action"`
	Market        broker.Market `json:"market,omitempty"`
	Side          broker.Side   `json:"side,omitempty"`
	QtyCoin       float64       `json:"qty_coin,omitempty"`
	ReduceOnly    bool          `json:"reduce_only,omitempty"`
	ClientOrderID string        `json:"client_order_id,omitempty"`
	ReasonVI      string        `json:"reason_vi"`
}

// planSquare decides the squaring order for one intent, from the venue's
// record of that intent's orders alone. It is execcheck -reconcile's decision,
// with one refusal added: a leg whose squaring id the venue already knows is
// NOT squared again. That id is derived, so a second order would carry it too,
// and a lookup by it could then answer about either order.
func planSquare(h intentHedge, spotRules, perpRules exchanges.Instrument, spotPriceQuote, perpPriceQuote float64) squarePlan {
	p := squarePlan{IntentID: h.IntentID, SpotQtyCoin: h.Spot.QtyCoin, PerpQtyCoin: h.Perp.QtyCoin, ResidualCoin: h.residualCoin()}
	tolerance := math.Max(spotRules.StepSizeCoin, perpRules.StepSizeCoin)
	switch {
	case len(h.unreadable()) > 0:
		p.Action, p.ReasonVI = "refuse", "có lệnh không đọc được từ sàn — không cân trên một con số chưa đầy đủ"
		return p
	case len(h.working()) > 0:
		p.Action, p.ReasonVI = "refuse", "có lệnh còn đang chạy trên sàn — cân lúc này là đua với chính lệnh đó"
		return p
	case tolerance <= 0:
		p.Action, p.ReasonVI = "refuse", "không có luật bước khối lượng để tính dung sai"
		return p
	case math.Abs(p.ResidualCoin) <= tolerance+gridEpsilon:
		p.Action, p.ReasonVI = "none", fmt.Sprintf("đã cân trong dung sai %.8f coin", tolerance)
		return p
	}

	// The longer side is brought down: a positive residual is too much spot
	// LONG (sell spot), a negative one too much perp SHORT (buy perp, reduce-only).
	var (
		rules      = spotRules
		priceQuote = spotPriceQuote
		leg        = execution.LegSpot
		exists     = h.Spot.ReconcileOrderExists
	)
	p.Market, p.Side = broker.MarketSpot, broker.SideSell
	if p.ResidualCoin < 0 {
		p.Market, p.Side, p.ReduceOnly = broker.MarketFuturesUSDM, broker.SideBuy, true
		rules, priceQuote, leg, exists = perpRules, perpPriceQuote, execution.LegPerp, h.Perp.ReconcileOrderExists
	}
	p.ClientOrderID = execution.ReconcileClientOrderID(h.IntentID, leg)
	if exists {
		p.Action = "refuse"
		p.ReasonVI = fmt.Sprintf("sàn đã có lệnh cân %s của ý định này mà cặp vẫn lệch — gửi lệnh thứ hai cùng id sẽ mơ hồ, phải xử lý tay", p.ClientOrderID)
		return p
	}
	rounded, err := broker.RoundOrder(broker.RoundRequest{
		Rules: rules, Side: p.Side, Type: broker.OrderTypeMarket,
		QtyCoin: math.Abs(p.ResidualCoin), PriceQuote: priceQuote, ReduceOnly: p.ReduceOnly,
	})
	if err != nil {
		p.Action = "refuse"
		p.ReasonVI = fmt.Sprintf("không làm tròn được lệnh cân: %v (phần dư dưới mức tối thiểu của sàn không lệnh nào đóng được — xử lý tay)", err)
		return p
	}
	if rounded.QtyCoin <= 0 {
		p.Action, p.ReasonVI = "refuse", "lệnh cân làm tròn về 0 coin"
		return p
	}
	p.Action, p.QtyCoin = "send", rounded.QtyCoin
	p.ReasonVI = fmt.Sprintf("%s MARKET %.8f coin trên %s", p.Side, p.QtyCoin, p.Market)
	return p
}
