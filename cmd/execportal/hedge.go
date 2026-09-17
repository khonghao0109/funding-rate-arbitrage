package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
	bybitbroker "futures-arbitrage-scanner/internal/broker/bybit"
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
//
// A venue that keeps a spot BUY's fee in the BASE coin (Bybit, PLAN 4.5j) makes
// "buys minus sells" the ORDERS, not the wallet: execution buys the spot leg
// grossed up, Q ÷ (1 − fee), so the buy order alone reads spot-long by the fee
// beside a perp of Q — 20 DOGE past a 1 DOGE tolerance on a 20,000 DOGE pair,
// which the page would call UNHEDGED and LÀM PHẲNG would "square" by selling
// coin the wallet never received. On such a venue every filled spot buy is
// counted NET of the base-coin commission its own fills state, read from the
// venue (broker.TradeReader); a fill whose fee cannot be read makes the leg
// unknown, never a guess.

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
	kind string // mở | đóng | gỡ | cân | thu
	id   string
}

// intentOrderIDs lists all five derived ids for one leg. The reconcile id is
// the one kind that matters beyond the sum: planSquare refuses to reuse it.
// "thu" is execution's reduceToMatch cut, which has its own id since review
// 4.5j n2 — leaving it out would read a pair Open shrank as unbalanced.
func intentOrderIDs(intentID string, leg execution.LegName) []orderRef {
	return []orderRef{
		{"mở", execution.LegClientOrderID(intentID, leg)},
		{"đóng", execution.CloseClientOrderID(intentID, leg)},
		{"gỡ", execution.UnwindClientOrderID(intentID, leg)},
		{"cân", execution.ReconcileClientOrderID(intentID, leg)},
		{"thu", execution.ReduceClientOrderID(intentID, leg)},
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
	// baseFees is the base-coin commission of a FINISHED spot buy, keyed like
	// orders; the same argument applies — a finished order's fills are fixed.
	baseFees map[string]float64
}

func newDoneOrders() *doneOrders {
	return &doneOrders{orders: map[string]broker.Order{}, baseFees: map[string]float64{}}
}

// forget drops everything remembered. Every write calls it before and after:
// a finished order cannot change, but an id this portal refuses to reuse could
// still be reused by another tool, and a write is the moment that matters.
func (d *doneOrders) forget() {
	d.ordersMu.Lock()
	d.orders = map[string]broker.Order{}
	d.baseFees = map[string]float64{}
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

// baseCommissionQtyCoin is the commission a spot buy's fills state in asset —
// the base coin — summed over every fill the VENUE lists for the order.
//
// Refused rather than guessed: a venue that cannot list fills, a fill charging
// commission in an asset it does not name, and a list whose quantities do not
// add up to the order's own fill (a window that cut some of them) all leave the
// wallet's share unknown.
func (d *doneOrders) baseCommissionQtyCoin(ctx context.Context, b broker.Broker, q broker.OrderQuery, order broker.Order, asset string) (float64, error) {
	d.ordersMu.Lock()
	fee, ok := d.baseFees[d.key(q)]
	d.ordersMu.Unlock()
	if ok {
		return fee, nil
	}
	reader, ok := b.(broker.TradeReader)
	if !ok {
		return 0, errors.New("sàn spot không cho đọc từng lần khớp, nên phí thu bằng coin là ẩn số")
	}
	trades, err := reader.OrderTrades(ctx, q)
	if err != nil {
		return 0, err
	}
	listedQtyCoin := 0.0
	for _, t := range trades {
		listedQtyCoin += t.QtyCoin
		switch {
		case t.CommissionQtyInAsset == 0:
		case t.CommissionAsset == asset:
			fee += t.CommissionQtyInAsset
		case t.CommissionAsset == "":
			return 0, fmt.Errorf("lần khớp %s thu phí %v mà sàn không nêu bằng đồng nào", t.TradeID, t.CommissionQtyInAsset)
		}
	}
	if math.Abs(listedQtyCoin-order.FilledQtyCoin) > gridEpsilon*math.Max(1, order.FilledQtyCoin) {
		return 0, fmt.Errorf("sàn liệt kê các lần khớp cộng lại %.10g coin, lệnh báo khớp %.10g — thiếu lần khớp thì thiếu phí", listedQtyCoin, order.FilledQtyCoin)
	}
	if order.Status.Done() {
		d.ordersMu.Lock()
		d.baseFees[d.key(q)] = fee
		d.ordersMu.Unlock()
	}
	return fee, nil
}

// hedgeFees is how spot BUY fills turn into what the wallet received.
//
// StatedOpen holds, per intent id, the base-coin fee the opening buy's fills
// stated when execution opened it (intentState.SpotBuyBaseFeeQtyCoin). It is
// used whatever the venue, so an intent file is the one source for both — a
// grossed-up buy is never counted gross because a profile flag disagreed with
// the fee execution was given. BaseAsset, set on a venue that keeps the fee in
// the base coin, reads the fills from the venue for a buy with no stated fee.
// The zero value is Binance testnet's path: every buy counted as filled.
type hedgeFees struct {
	BaseAsset  string
	StatedOpen map[string]float64

	// NotVisibleIsAbsent marks the intents whose derived ids may be read as
	// ABSENT when the venue lists them nowhere (bybit.ErrOrderNotVisible). On a
	// Unified account /v5/order/history keeps every order WITH fills for 730
	// days and an order without fills for 24 hours ("Get Order History", read
	// 2026-09-17), so once creation's asynchronous delay has passed, an id
	// neither list shows filled nothing: never sent, or cancelled empty. Most
	// derived ids of a held intent (close, unwind, reconcile, reduce) are of
	// that kind, and reading them as unknown left every Bybit position
	// unreadable and uncloseable from the page (review 4.5j part 2, N1). An
	// intent a write touched within notVisibleGrace — or while a write is
	// still running — is not marked: its order may exist and not show yet.
	// Execution keeps the adapter's reading (ambiguous); this rule is the
	// portal's only.
	//
	// It NEVER covers an intent's OPENING orders ("mở"). Those filled, so an
	// unlisted open is an anomaly, not an absence — and the adapter queries
	// /v5/order/history with no time window, which the page says answers the
	// last 7 days by default. Whether an orderLinkId lookup reaches past that
	// is unmeasured, so a pair held longer reads UNKNOWN rather than flat: read
	// as absent, a spot open alone gone from the lists would have planned a
	// perp buy-back that strips the hedge off a real spot long (review 4.5j
	// part 4, R1).
	NotVisibleIsAbsent map[string]bool

	// MustSee lists client order ids that are never read as absent: an order
	// the current write itself just sent (review part 4, R2).
	MustSee map[string]bool
}

// notVisibleGrace is how long after a write an order the venue does not list
// still counts as possibly in flight. Creation is asynchronous and history "may
// delay", but a write does not return before execution has read its own orders
// back (OrderSettleTimeout, 5 s), so what the grace covers is an order that
// write could not confirm — which it reports as an alarm anyway. Thirty seconds
// is six times that read-back budget; during it the pair reads UNKNOWN and the
// page refuses to close it, which is the price of never reading an order in
// flight as absent.
const notVisibleGrace = 30 * time.Second

// readLegNet sums one leg's derived orders.
func readLegNet(ctx context.Context, b broker.Broker, memo *doneOrders, market broker.Market, symbol, intentID string, leg execution.LegName, fees hedgeFees) legNet {
	var out legNet
	for _, ref := range intentOrderIDs(intentID, leg) {
		q := broker.OrderQuery{Market: market, Symbol: symbol, ClientOrderID: ref.id}
		order, err := memo.read(ctx, b, q)
		switch {
		case errors.Is(err, broker.ErrOrderNotFound):
			continue
		case errors.Is(err, bybitbroker.ErrOrderNotVisible) && fees.NotVisibleIsAbsent[intentID] &&
			ref.kind != "mở" && !fees.MustSee[ref.id]:
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
		feeVI := ""
		if order.Side == broker.SideSell {
			signed = -signed
		} else if market == broker.MarketSpot && order.FilledQtyCoin > 0 {
			if fee, ok := fees.StatedOpen[intentID]; ok && ref.kind == "mở" {
				signed -= fee
				feeVI = fmt.Sprintf(" − phí coin gốc %.8f (lần khớp khai lúc mở, lưu trong file ý định) = ví nhận %.8f", fee, signed)
			} else if fees.BaseAsset != "" {
				fee, err := memo.baseCommissionQtyCoin(ctx, b, q, order, fees.BaseAsset)
				if err != nil {
					out.UnreadableVI = append(out.UnreadableVI, fmt.Sprintf("phí %s thu bằng coin của lệnh %s %s (%s)", fees.BaseAsset, ref.kind, leg, err.Error()))
					continue
				}
				signed -= fee
				feeVI = fmt.Sprintf(" − phí %.8f %s = ví nhận %.8f", fee, fees.BaseAsset, signed)
			}
		}
		out.QtyCoin += signed
		if order.FilledQtyCoin > 0 {
			out.SeenVI = append(out.SeenVI, fmt.Sprintf("%s %s %.8f%s", ref.kind, order.Side, order.FilledQtyCoin, feeVI))
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
//
// fees is readLegNet's; callers inside the portal go through
// portal.intentHedges, which fills it from the intent files and the profile.
func readIntentHedges(ctx context.Context, spot, perp broker.Broker, memo *doneOrders, symbol string, intentIDs []string, fees hedgeFees) []intentHedge {
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
				Spot:     readLegNet(ctx, spot, memo, broker.MarketSpot, symbol, id, execution.LegSpot, fees),
				Perp:     readLegNet(ctx, perp, memo, broker.MarketFuturesUSDM, symbol, id, execution.LegPerp, hedgeFees{NotVisibleIsAbsent: fees.NotVisibleIsAbsent, MustSee: fees.MustSee}),
			}
		}()
	}
	wg.Wait()
	return out
}

// intentHedges reads intents the way this portal's venue requires. The fee each
// opening buy's fills stated comes from its intent file. Only a buy with none
// stored, on a venue that keeps the fee in the base coin, reads the fills from
// the venue — with the base asset from the venue's own instrument rules, never
// from the symbol string; rules that cannot be read leave such an intent's spot
// leg unknown.
//
// beforeSending is true only for a read made INSIDE a write, holding the write
// lock, before that write has sent anything: the running write is then known
// to have no order in flight, and only earlier writes and the intent files date
// the intents. Every other read treats a running write as possibly mid-send.
// A write that has sent orders and read them back may pass beforeSending with
// those orders' ids as sentThisWrite: they are then required to be seen, and
// the intent's other ids are judged as before the write.
func (p *portal) intentHedges(ctx context.Context, symbol string, intentIDs []string, beforeSending bool, sentThisWrite ...string) []intentHedge {
	fees := hedgeFees{StatedOpen: map[string]float64{}, NotVisibleIsAbsent: map[string]bool{}, MustSee: map[string]bool{}}
	for _, id := range sentThisWrite {
		fees.MustSee[id] = true
	}
	p.busyMu.Lock()
	writing, lastWriteEndMs := p.busyAction != "" && !beforeSending, p.lastWriteEndMs
	p.busyMu.Unlock()
	nowMs := p.now().UnixMilli()
	var unstated []string
	for _, id := range intentIDs {
		if st, err := loadState(p.stateDir, id); err == nil && st.SpotBuyBaseFeeStated {
			fees.StatedOpen[id] = st.SpotBuyBaseFeeQtyCoin
		} else {
			unstated = append(unstated, id)
		}
		// The intent file is rewritten after every write that touched the
		// intent, so its modification time bounds when its last order was sent
		// — across a restart too, where lastWriteEndMs starts at zero.
		touchedMs := lastWriteEndMs
		if path, err := statePath(p.stateDir, id); err == nil {
			if fi, err := os.Stat(path); err == nil && fi.ModTime().UnixMilli() > touchedMs {
				touchedMs = fi.ModTime().UnixMilli()
			} else if err != nil {
				touchedMs = nowMs // no file to date the intent by: not settled
			}
		}
		if !writing && nowMs-touchedMs >= notVisibleGrace.Milliseconds() {
			fees.NotVisibleIsAbsent[id] = true
		}
	}
	if p.markets.profile.SpotBuyFeeInBaseCoin && len(unstated) > 0 {
		rules, err := p.rulesFor(ctx, symbol)
		if err == nil && rules.Spot.BaseAsset == "" {
			err = errors.New("sàn không khai báo coin gốc")
		}
		if err != nil {
			out := make([]intentHedge, len(intentIDs))
			for i, id := range intentIDs {
				out[i] = intentHedge{IntentID: id, Spot: legNet{UnreadableVI: []string{
					"không đọc được luật spot để biết coin gốc thu phí (" + err.Error() + ") — chân spot là ẩn số"}}}
			}
			return out
		}
		fees.BaseAsset = rules.Spot.BaseAsset
	}
	return readIntentHedges(ctx, p.markets.spot, p.markets.perp, p.memo, symbol, intentIDs, fees)
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
