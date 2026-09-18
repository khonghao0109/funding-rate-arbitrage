package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/execution"
)

// The three endpoints that send orders: open, close, reconcile.
//
// Each is cmd/execcheck's action of the same name, driven by a button instead
// of a flag, and each keeps execcheck's two properties. Open and close are also
// what the testnet auto-trader drives (autotrade.go, PLAN Q18) — the same
// functions under the same write lock, never a second order path; reconcile
// stays a person's alone. And the venue is the truth: what is closed or squared
// is decided from the venue's record of the intent's own orders, never from the
// cache.
//
// Every write holds writeMu for its whole duration and runs on a context
// DETACHED from the request. A browser tab closed in the middle of an open must
// not cancel the open: execution's invariant survives a cancelled context, but
// a cancelled open is still an open that stopped early, and the operator who
// closed the tab did not ask for that.

// legView is one leg's result on the wire, GROSS.
type legView struct {
	Leg               string  `json:"leg"`
	Market            string  `json:"market"`
	ClientOrderID     string  `json:"client_order_id"`
	VenueOrderID      string  `json:"venue_order_id"`
	Status            string  `json:"status"`
	FilledQtyCoin     float64 `json:"filled_qty_coin"`
	AvgFillPriceQuote float64 `json:"avg_fill_price_quote"`
	UnwoundQtyCoin    float64 `json:"unwound_qty_coin"`
}

func toLegView(l execution.LegResult) legView {
	return legView{
		Leg: string(l.Leg), Market: string(l.Market), ClientOrderID: l.ClientOrderID, VenueOrderID: l.VenueOrderID,
		Status: string(l.Status), FilledQtyCoin: l.FilledQtyCoin, AvgFillPriceQuote: l.AvgFillPriceQuote,
		UnwoundQtyCoin: l.UnwoundQtyCoin,
	}
}

func eventLines(rec *execution.MemoryRecorder) []string {
	events := rec.Events()
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.At.Format("15:04:05.000")+" "+ev.String())
	}
	return out
}

// busy answers a write that arrived while another was running.
func (p *portal) busy(w http.ResponseWriter, heldBy string, sinceMs int64) {
	// The lock is taken a moment before its name is written, so a refusal can
	// land in between and see no name. It is still a refusal.
	what := "một thao tác ghi khác đang chạy"
	if heldBy != "" {
		what = fmt.Sprintf("đang có thao tác ghi khác (%s, bắt đầu %s)", heldBy, time.UnixMilli(sinceMs).Format("15:04:05"))
	}
	writeError(w, http.StatusConflict, "busy", what+" — chờ xong rồi thử lại; lệnh này KHÔNG được gửi")
}

func (p *portal) actionContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), p.exec.ActionTimeout)
}

// --------------------------------------------------------------------- open

type openRequest struct {
	Symbol        string  `json:"symbol"`
	NotionalQuote float64 `json:"notional_quote"`
	LegOrder      string  `json:"leg_order"`

	// SignalEntryCostPct is the auto-trader's priced entry (PLAN Q18), never
	// read from a request body. nil is a button press, whose "signal" is the
	// book the open reads itself.
	SignalEntryCostPct *float64 `json:"-"`
}

type openView struct {
	IntentID      string  `json:"intent_id"`
	Symbol        string  `json:"symbol"`
	NotionalQuote float64 `json:"notional_quote"`
	LegOrder      string  `json:"leg_order"`

	// Outcome is the invariant: both_open or both_flat. Hedged is both_open.
	// RefusedBeforePlacing means nothing reached the venue. Alarm means the
	// invariant may NOT hold — an unwind that could not be completed or two
	// flat proofs that disagree — and a human must look before anything else.
	Outcome              string `json:"outcome"`
	Hedged               bool   `json:"hedged"`
	RefusedBeforePlacing bool   `json:"refused_before_placing"`
	Alarm                bool   `json:"alarm"`
	ReducedToMatch       bool   `json:"reduced_to_match"`

	TargetQtyCoin    float64 `json:"target_qty_coin"`
	ResidualQtyCoin  float64 `json:"residual_qty_coin"`
	ToleranceQtyCoin float64 `json:"tolerance_qty_coin"`

	Spot legView `json:"spot"`
	Perp legView `json:"perp"`

	SpotBestAskQuote     float64  `json:"spot_best_ask_quote"`
	PerpBestBidQuote     float64  `json:"perp_best_bid_quote"`
	SpotEntrySlippageBps *float64 `json:"spot_entry_slippage_bps"`
	PerpEntrySlippageBps *float64 `json:"perp_entry_slippage_bps"`

	UnhedgedWindowMs int64 `json:"unhedged_window_ms"`
	UnwindDurationMs int64 `json:"unwind_duration_ms"`
	BookAgeMs        int64 `json:"book_age_ms"`
	ElapsedMs        int64 `json:"elapsed_ms"`
	OpenedAtMs       int64 `json:"opened_at_ms"`

	// The two books' mids the open was sized on — the reference its slippage
	// and, for the auto-trader, its entry basis are measured against.
	SpotRefMidQuote float64 `json:"spot_ref_mid_quote"`
	PerpRefMidQuote float64 `json:"perp_ref_mid_quote"`

	MaxSlippageBps    float64 `json:"max_slippage_bps"`
	LegTimeoutMs      int64   `json:"leg_timeout_ms"`
	PerpMarginFrac    float64 `json:"perp_margin_frac"`
	BracketVI         string  `json:"bracket_vi"`
	NextFundingTimeMs int64   `json:"next_funding_time_ms"`

	SpotFlatEvidenceVI string   `json:"spot_flat_evidence_vi"`
	ReasonVI           string   `json:"reason_vi"`
	ErrorVI            string   `json:"error_vi"`
	EventsVI           []string `json:"events_vi"`
	CacheFile          string   `json:"cache_file"`
	CacheErrorVI       string   `json:"cache_error_vi"`
}

func (p *portal) validateOpen(req *openRequest) error {
	symbol, err := p.allowedSymbol(req.Symbol)
	if err != nil {
		return err
	}
	req.Symbol = symbol
	if math.IsNaN(req.NotionalQuote) || math.IsInf(req.NotionalQuote, 0) || req.NotionalQuote <= 0 {
		return fmt.Errorf("notional_quote phải là số dương, nhận %v", req.NotionalQuote)
	}
	if req.NotionalQuote > maxNotionalQuote {
		return fmt.Errorf("notional_quote %.2f vượt trần của portal %.0f quote mỗi chân", req.NotionalQuote, maxNotionalQuote)
	}
	switch execution.LegOrder(req.LegOrder) {
	case "":
		req.LegOrder = string(execution.LegOrderSequentialSpotFirst)
	case execution.LegOrderSequentialSpotFirst, execution.LegOrderParallel:
	default:
		return fmt.Errorf("leg_order %s không phải %q hay %q", quoteForMessage(req.LegOrder),
			execution.LegOrderSequentialSpotFirst, execution.LegOrderParallel)
	}
	return nil
}

func (p *portal) handleOpen(w http.ResponseWriter, r *http.Request) {
	var req openRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := p.validateOpen(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error()+" — lệnh KHÔNG được gửi")
		return
	}
	if err := p.markets.both(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "no_credentials", "thiếu credential, không mở được: "+err.Error())
		return
	}
	release, heldBy, since, ok := p.acquire("open")
	if !ok {
		p.busy(w, heldBy, since)
		return
	}
	defer release()
	defer p.afterOrderWrite()
	p.memo.forget()

	ctx, cancel := p.actionContext(r)
	defer cancel()
	view, status := p.open(ctx, req)
	log.Printf("execportal: OPEN %s %s %.2f quote → %s hedged=%v alarm=%v %s",
		view.IntentID, view.Symbol, view.NotionalQuote, view.Outcome, view.Hedged, view.Alarm, view.ErrorVI)
	writeJSON(w, status, view)
}

// open is execcheck's runOpen: fresh rules and books from both testnets, the
// maintenance bracket read with the key, the intent handed to execution, the
// cache written whatever happened.
func (p *portal) open(ctx context.Context, req openRequest) (openView, int) {
	return p.openAs(ctx, req, intentPrefixPortal)
}

// openAs is open under the id prefix of the tool that asked: a button press or
// the auto-trader. It is the ONE place either reaches execution.Open.
func (p *portal) openAs(ctx context.Context, req openRequest, intentPrefix string) (openView, int) {
	v := openView{
		Symbol: req.Symbol, NotionalQuote: req.NotionalQuote, LegOrder: req.LegOrder,
		MaxSlippageBps: p.exec.MaxSlippageBps, LegTimeoutMs: p.exec.LegTimeout.Milliseconds(), PerpMarginFrac: p.exec.MarginFrac,
		Outcome: string(execution.OutcomeBothFlat), RefusedBeforePlacing: true,
	}
	spotMkt, err := readMarket(ctx, p.markets.spot, req.Symbol)
	if err != nil {
		v.ErrorVI = "chân spot: " + err.Error() + " — chưa gửi lệnh nào"
		return v, http.StatusBadGateway
	}
	perpMkt, err := readMarket(ctx, p.markets.perp, req.Symbol)
	if err != nil {
		v.ErrorVI = "chân perp: " + err.Error() + " — chưa gửi lệnh nào"
		return v, http.StatusBadGateway
	}
	if err := sameAsset(spotMkt.Rules, perpMkt.Rules); err != nil {
		v.ErrorVI = err.Error() + " — chưa gửi lệnh nào"
		return v, http.StatusUnprocessableEntity
	}
	v.ToleranceQtyCoin = math.Max(spotMkt.Rules.StepSizeCoin, perpMkt.Rules.StepSizeCoin)
	if whyVI := p.openBlockedVI(ctx, req.Symbol, v.ToleranceQtyCoin); whyVI != "" {
		v.ErrorVI = whyVI + " — chưa gửi lệnh nào"
		return v, http.StatusConflict
	}
	bracket, err := perpBracket(ctx, p.markets.perp, p.markets.profile, req.Symbol, req.NotionalQuote)
	if err != nil {
		v.ErrorVI = "không đọc được biểu ký quỹ duy trì: " + err.Error() + " — chưa gửi lệnh nào"
		return v, http.StatusBadGateway
	}
	v.BracketVI = bracket.NoteVI

	// The spot buy's fee, which a venue may keep IN THE BASE COIN — Bybit always
	// does on a taker buy (PLAN 4.5j). execution buys the spot leg grossed up by
	// it and judges the leg on the fills and the wallet; an unread fee is an
	// unknown size, so nothing is sent. The rate is this account's own (Binance
	// spot testnet reads 0, which leaves execution's accepted path untouched).
	fees, err := p.commissionsFor(ctx, req.Symbol)
	if err != nil {
		v.ErrorVI = "không đọc được phí của tài khoản (phí mua spot có thể bị thu bằng coin): " + err.Error() + " — chưa gửi lệnh nào"
		return v, http.StatusBadGateway
	}

	intentID, err := p.mintIntentIDWith(intentPrefix, req.Symbol)
	if err != nil {
		v.ErrorVI = err.Error() + " — chưa gửi lệnh nào"
		return v, http.StatusInternalServerError
	}
	v.IntentID = intentID
	intent := execution.Intent{
		ID: intentID, Symbol: req.Symbol,
		SpotInstrument: spotMkt.Rules.Instrument, PerpInstrument: perpMkt.Rules.Instrument,
		SpotBook: spotMkt.Book, PerpBook: perpMkt.Book,
		SpotPriceQuote: spotMkt.PriceQuote, PerpPriceQuote: perpMkt.PriceQuote,
		NotionalQuote: req.NotionalQuote,
		// For a button press the "signal" is a person, so the cost the decision
		// was made at IS the cost the book prices now — execcheck's reasoning,
		// and no widening check applies. The auto-trader's signal priced an
		// entry on the scan's books a moment ago, and execution refuses when
		// this book has widened past that by more than its default tolerance.
		SignalEntryCostPct:   0,
		PerpMarginFrac:       p.exec.MarginFrac,
		PerpBracket:          bracket,
		SpotBuyFeeInBaseFrac: fees.Spot.TakerBuyFrac,
	}
	cfg := execution.DefaultConfig()
	cfg.MaxEntryCostWidenBps = math.Inf(1)
	if req.SignalEntryCostPct != nil {
		intent.SignalEntryCostPct = *req.SignalEntryCostPct
		cfg.MaxEntryCostWidenBps = execution.DefaultConfig().MaxEntryCostWidenBps
	}
	cfg.MaxSlippageBps = p.exec.MaxSlippageBps
	cfg.LegTimeout = p.exec.LegTimeout
	cfg.LegOrder = execution.LegOrder(req.LegOrder)

	rec := execution.NewMemoryRecorder()
	tr, err := execution.NewOpener(p.markets.spot, p.markets.perp, cfg, rec)
	if err != nil {
		v.ErrorVI = err.Error() + " — chưa gửi lệnh nào"
		return v, http.StatusInternalServerError
	}

	// The exclusive symbol lock (PLAN 4.5k step 4). It is taken LAST, after every
	// cheaper refusal, and before the first order: a lock held while a book read
	// fails is a symbol Engine 2 could not have used and did not need to lose.
	// When Engine 2 is not wired there is no lock and settle is a no-op.
	settleLock, lockWhyVI := p.acquireEngine1(ctx, req.Symbol, intentID, "Động cơ 1: spot long + perp short trên "+p.markets.profile.LabelVI)
	if lockWhyVI != "" {
		v.ErrorVI = lockWhyVI + " — chưa gửi lệnh nào"
		return v, http.StatusConflict
	}

	startedAt := time.Now()
	res, openErr := tr.Open(ctx, intent)
	v.ElapsedMs = time.Since(startedAt).Milliseconds()

	v.Outcome, v.Hedged, v.ReducedToMatch = string(res.Outcome), res.Hedged(), res.ReducedToMatch
	v.RefusedBeforePlacing = errors.Is(openErr, execution.ErrRefusedBeforePlacing)
	v.Alarm = errors.Is(openErr, execution.ErrUnwindIncomplete) || errors.Is(openErr, execution.ErrFlatEvidenceConflict)
	// Keep the lock whenever the venues may hold something: a hedged pair, or a
	// loud result where an order that has not been proven finished stands beside
	// whatever the positions read. Only a run that is BOTH flat and quiet gives
	// the symbol back, and even then the coordinator re-reads the venues first.
	settleLock(ctx, v.Hedged || v.Alarm)
	v.TargetQtyCoin, v.ResidualQtyCoin = res.TargetQtyCoin, res.ResidualQtyCoin
	v.Spot, v.Perp = toLegView(res.Spot), toLegView(res.Perp)
	v.SpotBestAskQuote, v.PerpBestBidQuote = spotMkt.Book.BestAskQuote, perpMkt.Book.BestBidQuote
	v.OpenedAtMs, v.SpotRefMidQuote, v.PerpRefMidQuote = startedAt.UnixMilli(), spotMkt.Book.MidPriceQuote, perpMkt.Book.MidPriceQuote
	v.SpotEntrySlippageBps = bpsPtr(slippageBps(res.Spot.AvgFillPriceQuote, spotMkt.Book.BestAskQuote, true))
	v.PerpEntrySlippageBps = bpsPtr(slippageBps(res.Perp.AvgFillPriceQuote, perpMkt.Book.BestBidQuote, false))
	v.UnhedgedWindowMs, v.UnwindDurationMs, v.BookAgeMs = res.UnhedgedWindow.Milliseconds(), res.UnwindDuration.Milliseconds(), res.BookAgeMs
	v.SpotFlatEvidenceVI, v.ReasonVI = res.SpotFlatEvidenceVI, res.ReasonVI
	if openErr != nil {
		v.ErrorVI = openErr.Error()
	}
	v.EventsVI = eventLines(rec)

	st := intentState{
		IntentID: intentID, Symbol: req.Symbol, OpenedAtMs: startedAt.UnixMilli(),
		LegOrder: req.LegOrder, NotionalQuote: req.NotionalQuote, TargetQtyCoin: res.TargetQtyCoin,
		SpotClientOrderID: res.Spot.ClientOrderID, PerpClientOrderID: res.Perp.ClientOrderID,
		SpotFilledQtyCoin: res.Spot.FilledQtyCoin, PerpFilledQtyCoin: res.Perp.FilledQtyCoin,
		SpotAvgPriceQuote: res.Spot.AvgFillPriceQuote, PerpAvgPriceQuote: res.Perp.AvgFillPriceQuote,
		SpotRefMidQuote: spotMkt.Book.MidPriceQuote, PerpRefMidQuote: perpMkt.Book.MidPriceQuote,
		SpotBestAskQuote: spotMkt.Book.BestAskQuote, PerpBestBidQuote: perpMkt.Book.BestBidQuote,
		BookSampledAtMs: spotMkt.Book.SampledAtMs, UnhedgedWindowMs: res.UnhedgedWindow.Milliseconds(),
		ReducedToMatch: res.ReducedToMatch, Outcome: string(res.Outcome),
		SpotBuyBaseFeeQtyCoin: res.SpotBuyBaseFeeQtyCoin, SpotBuyBaseFeeStated: res.SpotBuyBaseFeeStated,
	}
	if v.Alarm {
		// Written into the cache so the intent stays TRACKED: the page reads
		// its orders from the venue on every refresh until a person clears it.
		st.NoteVI = openErr.Error()
	}
	if mp, err := p.markets.perp.MarkPrice(ctx, broker.MarketFuturesUSDM, req.Symbol); err == nil {
		st.NextFundingTimeMs = mp.NextFundingTimeMs
		v.NextFundingTimeMs = mp.NextFundingTimeMs
	}
	if path, err := statePath(p.stateDir, intentID); err == nil {
		v.CacheFile = path
	}
	if err := saveState(p.stateDir, st); err != nil {
		v.CacheErrorVI = "KHÔNG ghi được file ý định: " + err.Error() +
			" — vị thế (nếu có) vẫn hỏi được từ sàn bằng intent_id, vì mọi ClientOrderID đều suy ra từ nó"
	}
	return v, http.StatusOK
}

// openBlockedVI says why a position may NOT be opened on this symbol now, or ""
// when nothing is held.
//
// ONE position per symbol, and the reason is execution's, not taste.
// execution.Close proves a close complete by reading the venue's perp position
// back as FLAT, and the venue has one position per symbol for the whole
// account. With two intents open, closing the first leaves the second's perp
// leg on the account, and Close reports that as an incomplete unwind — a false
// alarm that would then sit in the intent's note forever. So the portal refuses
// the second open instead: the venue's perp position must be zero and every
// tracked intent must hold nothing by its own orders, all read from the venue.
func (p *portal) openBlockedVI(ctx context.Context, symbol string, toleranceQtyCoin float64) string {
	pos, err := p.markets.perp.GetPosition(ctx, broker.MarketFuturesUSDM, symbol)
	if err != nil {
		return "không đọc được vị thế perp từ sàn để kiểm symbol còn trống: " + err.Error()
	}
	if math.Abs(pos.QtyCoin) > gridEpsilon {
		return fmt.Sprintf("sàn đang giữ vị thế perp %+.8f coin trên %s — portal chỉ giữ MỘT vị thế mỗi symbol; đóng hoặc làm phẳng nó trước",
			pos.QtyCoin, symbol)
	}
	states, unreadable, err := listStates(p.stateDir, symbol)
	if err != nil || len(unreadable) > 0 {
		return fmt.Sprintf("không đọc được mọi file ý định (%v %v) — không biết symbol còn trống hay không", err, unreadable)
	}
	var ids []string
	for _, s := range states {
		if s.tracked() {
			ids = append(ids, s.IntentID)
		}
	}
	for _, h := range p.intentHedges(ctx, symbol, ids, true) {
		switch {
		case len(h.unreadable()) > 0 || len(h.working()) > 0:
			return fmt.Sprintf("ý định %s có lệnh không đọc được hoặc còn chạy trên sàn — không biết symbol còn trống hay không", h.IntentID)
		case math.Abs(h.Spot.QtyCoin) > toleranceQtyCoin+gridEpsilon || math.Abs(h.Perp.QtyCoin) > toleranceQtyCoin+gridEpsilon:
			return fmt.Sprintf("ý định %s còn giữ spot %+.8f / perp %+.8f coin theo lệnh của chính nó — portal chỉ giữ MỘT vị thế mỗi symbol; đóng hoặc làm phẳng nó trước",
				h.IntentID, h.Spot.QtyCoin, h.Perp.QtyCoin)
		}
	}
	return ""
}

// spotBaseBalanceQtyCoin is the spot wallet's FREE balance of one asset, read
// from the venue. Locked coin sits under a resting order and cannot be sold, so
// counting it would pass a check the spot close then fails — after the perp leg
// has already been bought back.
func spotBaseBalanceQtyCoin(ctx context.Context, spot venue, asset string) (float64, error) {
	balances, err := spot.GetBalance(ctx, broker.MarketSpot)
	if err != nil {
		return 0, err
	}
	for _, b := range balances {
		if b.Asset == asset {
			return b.FreeQtyCoin, nil
		}
	}
	return 0, fmt.Errorf("sàn không liệt kê %s trong ví spot", asset)
}

// mintIntentID returns an id no cache file already uses. The derived
// ClientOrderIDs make a duplicate intent id a duplicate ORDER id, which the
// venue refuses only while the first order is still open.
func (p *portal) mintIntentID(symbol string) (string, error) {
	return p.mintIntentIDWith(intentPrefixPortal, symbol)
}

func (p *portal) mintIntentIDWith(prefix, symbol string) (string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		id := newIntentIDWith(prefix, symbol, p.now().Add(time.Duration(attempt)*time.Millisecond))
		if _, err := loadState(p.stateDir, id); err != nil {
			return id, nil
		}
	}
	return "", errors.New("không sinh được intent id chưa dùng")
}

// -------------------------------------------------------------------- close

type closeRequest struct {
	Symbol   string `json:"symbol"`
	IntentID string `json:"intent_id"`
	ReasonVI string `json:"reason_vi,omitempty"`
}

type closeView struct {
	IntentID string `json:"intent_id"`
	Symbol   string `json:"symbol"`

	// Outcome both_flat is the whole position closed. Refused means nothing
	// was sent and both legs are exactly as they were. Alarm is the invariant
	// possibly broken.
	Outcome string `json:"outcome"`
	Flat    bool   `json:"flat"`
	Refused bool   `json:"refused"`
	// Deferred is a close the pre-flight spread guard held back (closeGuard):
	// nothing was sent, Refused is set too, and the bot retries next scan.
	Deferred bool `json:"deferred"`
	Alarm    bool `json:"alarm"`
	// SentUnconfirmed is a closing order that DID reach the venue and filled
	// nothing the portal could confirm. execution calls that a refusal
	// (ErrCloseRefused) because no quantity moved; it is not "both legs
	// untouched" — the order exists, may still fill, and its id is spent.
	SentUnconfirmed bool `json:"sent_unconfirmed"`

	// IntentQtyCoin is what this intent's own orders hold at the venue, the
	// size asked to close. VenuePerpQtyCoin is the venue's signed perp
	// position after the close.
	IntentQtyCoin    float64 `json:"intent_qty_coin"`
	RequestedQtyCoin float64 `json:"requested_qty_coin"`
	ClosedQtyCoin    float64 `json:"closed_qty_coin"`
	RemainingQtyCoin float64 `json:"remaining_qty_coin"`
	VenuePerpQtyCoin float64 `json:"venue_perp_qty_coin"`

	Spot legView `json:"spot"`
	Perp legView `json:"perp"`

	DurationMs int64 `json:"duration_ms"`
	ElapsedMs  int64 `json:"elapsed_ms"`

	FundingReceivedQuote float64 `json:"funding_received_quote"`
	SettlementsCounted   int     `json:"settlements_counted"`
	CommissionQuote      float64 `json:"commission_quote"`
	SlippageQuote        float64 `json:"slippage_quote"`
	RealizedQuote        float64 `json:"realized_quote"`
	PairPriceDriftQuote  float64 `json:"pair_price_drift_quote"`
	RealizedLabelVI      string  `json:"realized_label_vi"`

	FundingSourceVI    string   `json:"funding_source_vi"`
	CommissionSourceVI string   `json:"commission_source_vi"`
	CommissionOtherVI  string   `json:"commission_other_vi"`
	PriceDriftPricedVI string   `json:"price_drift_priced_vi"`
	SpotFlatEvidenceVI string   `json:"spot_flat_evidence_vi"`
	ReasonVI           string   `json:"reason_vi"`
	ErrorVI            string   `json:"error_vi"`
	EventsVI           []string `json:"events_vi"`
	CacheErrorVI       string   `json:"cache_error_vi"`
}

const realizedLabelVI = "RealizedQuote = funding sàn đã trả − phí sàn thu bằng quote − trượt giá so với mid lúc quyết định. " +
	"KHÔNG phải lãi ròng: trôi giá cặp (basis giữa vào và ra), phí thu bằng tài sản khác quote và chi phí vốn nằm NGOÀI con số này."

func (p *portal) handleClose(w http.ResponseWriter, r *http.Request) {
	var req closeRequest
	if !decodeBody(w, r, &req) {
		return
	}
	symbol, err := p.allowedSymbol(req.Symbol)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_symbol", err.Error())
		return
	}
	if err := validIntentID(req.IntentID); err != nil {
		writeError(w, http.StatusBadRequest, "bad_intent_id", err.Error())
		return
	}
	if err := p.markets.both(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "no_credentials", "thiếu credential, không đóng được: "+err.Error())
		return
	}
	release, heldBy, since, ok := p.acquire("close")
	if !ok {
		p.busy(w, heldBy, since)
		return
	}
	defer release()
	defer p.afterOrderWrite()
	p.memo.forget()

	st, err := loadState(p.stateDir, req.IntentID)
	if err != nil {
		writeError(w, http.StatusNotFound, "intent_not_found",
			"không đọc được file ý định "+req.IntentID+": "+err.Error()+" (đóng cần giá khớp và mid lúc VÀO mà chỉ file này giữ)")
		return
	}
	if st.Symbol != symbol {
		writeError(w, http.StatusUnprocessableEntity, "symbol_mismatch",
			fmt.Sprintf("ý định %s là %s, yêu cầu nói %s — không đóng", st.IntentID, st.Symbol, symbol))
		return
	}

	ctx, cancel := p.actionContext(r)
	defer cancel()
	reasonVI := req.ReasonVI
	if reasonVI == "" {
		reasonVI = "Đóng thủ công bởi người vận hành"
	}
	// An operator's close is never held back for a spread: a person pressed it.
	view, status := p.close(ctx, st, reasonVI, closeGuard{})
	log.Printf("execportal: CLOSE %s %s → %s flat=%v refused=%v alarm=%v closed=%.8f %s",
		view.IntentID, view.Symbol, view.Outcome, view.Flat, view.Refused, view.Alarm, view.ClosedQtyCoin, view.ErrorVI)
	writeJSON(w, status, view)
}

// closeGuard is what a close checks on the books it has just read, before any
// order is sent.
type closeGuard struct {
	// MaxSpreadBps > 0 defers the close when either book's touch is wider than
	// this, in basis points of that book's mid, or cannot be measured. Only the
	// bot's take-profit sets it (autotrade.CloseOrder.MaxSpreadBps); a risk exit,
	// a kill, a stop-and-close and a person's close all send zero, because a
	// wide book is a reason to wait for a GAIN and never while a hedge breaks.
	MaxSpreadBps float64
}

// deferVI is the reason the guard holds the close back, or "" to send.
//
// An unmeasurable touch DEFERS here, unlike the scan-time brake in autotrade,
// which lets it through. The scan's brake judges a reading, and jamming it
// would silence the take-profit on a missing auxiliary figure; this guard is
// the last look before two MARKET orders cross the book, and sending them into
// a touch nobody could read is sending them blind.
func (g closeGuard) deferVI(spot, perp depth.Summary) string {
	if !(g.MaxSpreadBps > 0) {
		return ""
	}
	spotBps, spotOK := touchSpreadBps(spot)
	perpBps, perpOK := touchSpreadBps(perp)
	if !spotOK || !perpOK {
		return fmt.Sprintf("không đo được spread tức thời của sổ lệnh (spot %s, perp %s) — hoãn chốt lời để tránh trượt giá mù; chưa gửi lệnh nào",
			spreadWordVI(spotBps, spotOK), spreadWordVI(perpBps, perpOK))
	}
	if spotBps > g.MaxSpreadBps || perpBps > g.MaxSpreadBps {
		return fmt.Sprintf("Spread sổ lệnh tức thời bị giãn (Spot %.1f bps, Perp %.1f bps > trần %.1f bps) — hoãn đóng để bảo vệ lợi nhuận; chưa gửi lệnh nào",
			spotBps, perpBps, g.MaxSpreadBps)
	}
	return ""
}

// touchSpreadBps is (best ask − best bid) ÷ mid in basis points, recomputed
// from the touch prices rather than read from Summary.SpreadPct, so a summary
// whose derived field was never filled cannot read as a perfectly tight book.
func touchSpreadBps(b depth.Summary) (float64, bool) {
	if !(b.MidPriceQuote > 0) || !(b.BestBidQuote > 0) || !(b.BestAskQuote > 0) || b.BestAskQuote < b.BestBidQuote {
		return 0, false
	}
	return (b.BestAskQuote - b.BestBidQuote) / b.MidPriceQuote * 10_000, true
}

func spreadWordVI(bps float64, ok bool) string {
	if !ok {
		return "không đo được"
	}
	return fmt.Sprintf("%.1f bps", bps)
}

// close is execcheck's runClose with ONE deliberate difference: the size.
//
// execcheck passes QtyCoin 0, "ask the venue", and the venue answers with the
// ACCOUNT's whole perp position. With one intent open that is the intent; with
// two it is both, and closing one intent would flatten the other's perp leg
// under the first one's ids — the account still hedged, both intents' records
// now wrong. So the portal sizes the close from the VENUE's record of THIS
// intent's own orders, which execution then checks against the venue position
// rather than trusts (CloseRequest.QtyCoin). Still rule 7; narrower.
func (p *portal) close(ctx context.Context, st intentState, reasonVI string, guard closeGuard) (closeView, int) {
	v := closeView{IntentID: st.IntentID, Symbol: st.Symbol, Outcome: string(execution.OutcomeBothOpen),
		Refused: true, RealizedLabelVI: realizedLabelVI}

	spotMkt, err := readMarket(ctx, p.markets.spot, st.Symbol)
	if err != nil {
		v.ErrorVI = "chân spot: " + err.Error() + " — chưa gửi lệnh nào"
		return v, http.StatusBadGateway
	}
	perpMkt, err := readMarket(ctx, p.markets.perp, st.Symbol)
	if err != nil {
		v.ErrorVI = "chân perp: " + err.Error() + " — chưa gửi lệnh nào"
		return v, http.StatusBadGateway
	}
	// PRE-FLIGHT SPREAD GUARD (audit R6). These two books were read a moment
	// ago, for this close, and the MARKET orders below cross exactly these
	// touches. A take-profit is the one close that may wait for a tighter book.
	if whyVI := guard.deferVI(spotMkt.Book, perpMkt.Book); whyVI != "" {
		v.Deferred = true
		v.ErrorVI = whyVI
		return v, http.StatusConflict
	}
	tolerance := math.Max(spotMkt.Rules.StepSizeCoin, perpMkt.Rules.StepSizeCoin)

	h := p.intentHedges(ctx, st.Symbol, []string{st.IntentID}, true)[0]
	spotQtyCoin, perpShortQtyCoin := h.Spot.QtyCoin, -h.Perp.QtyCoin
	switch {
	case len(h.unreadable()) > 0:
		v.ErrorVI = "không đọc được lệnh của ý định từ sàn (" + strings.Join(h.unreadable(), "; ") + ") — không đóng trên con số chưa đầy đủ, chưa gửi lệnh nào"
		return v, http.StatusBadGateway
	case len(h.working()) > 0:
		v.ErrorVI = "lệnh của ý định còn đang chạy trên sàn (" + strings.Join(h.working(), "; ") + ") — chưa gửi lệnh nào"
		return v, http.StatusConflict
	case math.Abs(perpShortQtyCoin) <= gridEpsilon && math.Abs(spotQtyCoin) <= tolerance+gridEpsilon:
		v.ErrorVI = fmt.Sprintf("ý định %s không còn giữ gì trên sàn theo lệnh của chính nó (spot %+.8f, perp %+.8f) — không có gì để đóng",
			st.IntentID, h.Spot.QtyCoin, h.Perp.QtyCoin)
		return v, http.StatusConflict
	case perpShortQtyCoin <= gridEpsilon:
		// Perp flat, spot still holding: the naked-spot shape of 2026-09-13.
		// Reconcile squares it under its own unused id, so that is where the
		// operator is sent — checked before the spent close id would say
		// "by hand".
		v.ErrorVI = fmt.Sprintf("chân perp của ý định đã phẳng mà chân spot còn %+.8f coin — chạy LÀM PHẲNG (reconcile) để bán phần spot trần; chưa gửi lệnh nào",
			h.Spot.QtyCoin)
		return v, http.StatusConflict
	case h.Spot.CloseOrderExists || h.Perp.CloseOrderExists:
		// The close ids are derived and fixed per intent, so a second close
		// would send an order under an id the venue already knows, and the
		// read-back could answer about the FIRST one.
		v.ErrorVI = fmt.Sprintf("lệnh ĐÓNG của ý định này đã tồn tại trên sàn (spot %v, perp %v) — gửi lại cùng id sẽ mơ hồ; phần còn lại (spot %+.8f, perp %+.8f) phải xử lý tay, chưa gửi lệnh nào",
			h.Spot.CloseOrderExists, h.Perp.CloseOrderExists, h.Spot.QtyCoin, h.Perp.QtyCoin)
		return v, http.StatusConflict
	case math.Abs(spotQtyCoin-perpShortQtyCoin) > tolerance+gridEpsilon:
		v.ErrorVI = fmt.Sprintf("cặp của ý định đang LỆCH theo lệnh của chính nó trên sàn: spot %+.8f, perp %+.8f coin — chạy LÀM PHẲNG (reconcile) trước; chưa gửi lệnh nào",
			h.Spot.QtyCoin, h.Perp.QtyCoin)
		return v, http.StatusConflict
	}

	// The close is sized at the PERP SHORT, not at the smaller leg. The two
	// legs may differ by less than the coarser step and still be a hedge (spot
	// 0.00079 beside perp 0.0008), and execution floors the perp order onto
	// its own grid: sized at 0.00079 it would buy back 0.0007 and leave a
	// whole perp step on the account — a position no later close can reach
	// (its ids are spent) and no reconcile will square (it is inside the
	// tolerance). Sized at the perp, the perp ends flat and spot sells up to
	// the COARSER step more than this intent bought (0.0001 BTC on BTCUSDT),
	// which the wallet must hold free.
	v.IntentQtyCoin = perpShortQtyCoin
	if spotQtyCoin < perpShortQtyCoin-gridEpsilon {
		balanceQtyCoin, err := spotBaseBalanceQtyCoin(ctx, p.markets.spot, spotMkt.Rules.BaseAsset)
		if err != nil || balanceQtyCoin < perpShortQtyCoin-gridEpsilon {
			v.ErrorVI = fmt.Sprintf("chân spot của ý định giữ %.8f coin, chân perp %.8f; đóng trọn perp cần bán %.8f %s trên spot nhưng ví spot có %.8f tự do (%v) — không đóng để khỏi để lại một bước perp không lệnh nào gỡ được; chưa gửi lệnh nào",
				spotQtyCoin, perpShortQtyCoin, perpShortQtyCoin, spotMkt.Rules.BaseAsset, balanceQtyCoin, err)
			return v, http.StatusConflict
		}
	}

	// The venue's perp position must be THIS intent's, or execution's flat
	// proof reads someone else's perp as a close that failed.
	pos, err := p.markets.perp.GetPosition(ctx, broker.MarketFuturesUSDM, st.Symbol)
	if err != nil {
		v.ErrorVI = "không đọc được vị thế perp từ sàn: " + err.Error() + " — chưa gửi lệnh nào"
		return v, http.StatusBadGateway
	}
	if math.Abs(pos.QtyCoin-h.Perp.QtyCoin) > tolerance+gridEpsilon {
		v.ErrorVI = fmt.Sprintf("SÀN báo vị thế perp %+.8f coin, lệnh của ý định này giải thích %+.8f coin — có vị thế khác trên symbol, không đóng; người vận hành quyết, chưa gửi lệnh nào",
			pos.QtyCoin, h.Perp.QtyCoin)
		return v, http.StatusConflict
	}

	rec := execution.NewMemoryRecorder()
	tr, err := execution.NewOpener(p.markets.spot, p.markets.perp, execution.DefaultConfig(), rec)
	if err != nil {
		v.ErrorVI = err.Error()
		return v, http.StatusInternalServerError
	}
	startedAt := time.Now()
	res, closeErr := tr.Close(ctx, execution.CloseRequest{
		Intent: execution.Intent{
			ID: st.IntentID, Symbol: st.Symbol,
			SpotInstrument: spotMkt.Rules.Instrument, PerpInstrument: perpMkt.Rules.Instrument,
			SpotBook: spotMkt.Book, PerpBook: perpMkt.Book,
			SpotPriceQuote: spotMkt.PriceQuote, PerpPriceQuote: perpMkt.PriceQuote,
		},
		QtyCoin:                v.IntentQtyCoin,
		OpenedAtMs:             st.OpenedAtMs,
		EntrySpotAvgPriceQuote: st.SpotAvgPriceQuote,
		EntryPerpAvgPriceQuote: st.PerpAvgPriceQuote,
		EntrySpotRefMidQuote:   st.SpotRefMidQuote,
		EntryPerpRefMidQuote:   st.PerpRefMidQuote,
		EntrySpotFilledQtyCoin: st.SpotFilledQtyCoin,
	})
	v.ElapsedMs = time.Since(startedAt).Milliseconds()
	v.EventsVI = eventLines(rec)

	v.Outcome = string(res.Outcome)
	v.Flat = res.Outcome == execution.OutcomeBothFlat && closeErr == nil
	sent := rec.Has(execution.EventCloseLeg)
	v.Refused = errors.Is(closeErr, execution.ErrCloseRefused) && !sent
	v.SentUnconfirmed = errors.Is(closeErr, execution.ErrCloseRefused) && sent
	v.Alarm = errors.Is(closeErr, execution.ErrUnwindIncomplete) || errors.Is(closeErr, execution.ErrFlatEvidenceConflict)
	v.RequestedQtyCoin, v.ClosedQtyCoin, v.RemainingQtyCoin = res.RequestedQtyCoin, res.ClosedQtyCoin, res.RemainingQtyCoin
	v.VenuePerpQtyCoin = res.VenuePositionQtyCoin
	v.Spot, v.Perp = toLegView(res.Spot), toLegView(res.Perp)
	v.DurationMs = res.Duration.Milliseconds()
	v.FundingReceivedQuote, v.SettlementsCounted = res.FundingReceivedQuote, res.SettlementsCounted
	v.CommissionQuote, v.SlippageQuote, v.RealizedQuote = res.CommissionQuote, res.SlippageQuote, res.RealizedQuote
	v.PairPriceDriftQuote = res.PairPriceDriftQuote
	v.FundingSourceVI, v.CommissionSourceVI, v.CommissionOtherVI = res.FundingSourceVI, res.CommissionSourceVI, res.CommissionOtherVI
	v.PriceDriftPricedVI, v.SpotFlatEvidenceVI, v.ReasonVI = res.PriceDriftPricedVI, res.SpotFlatEvidenceVI, res.ReasonVI
	if closeErr != nil {
		v.ErrorVI = closeErr.Error()
	}

	if v.Refused {
		// Nothing reached the venue, so the cache is left exactly as it was.
		// execcheck marks such an intent closed; the portal does not, because a
		// file that says "closed" over a position the venue still holds is the
		// kind of cache rule 7 warns about.
		return v, http.StatusOK
	}
	if v.SentUnconfirmed {
		// Not closed, and not untouched either. The note keeps the intent
		// tracked, so every refresh reads its orders — including that one —
		// back from the venue until a person has looked.
		st.NoteVI = "lệnh đóng đã GỬI nhưng không khớp/không xác nhận được: " + closeErr.Error()
		if err := saveState(p.stateDir, st); err != nil {
			v.CacheErrorVI = "KHÔNG ghi được file ý định: " + err.Error()
		}
		return v, http.StatusOK
	}
	st.ClosedAtMs = time.Now().UnixMilli()
	st.ClosedQtyCoin = res.ClosedQtyCoin
	st.RealizedQuote = res.RealizedQuote
	st.FundingQuote = res.FundingReceivedQuote
	st.CommissionQuote = res.CommissionQuote
	st.SlippageQuote = res.SlippageQuote
	st.SettlementsCounted = res.SettlementsCounted
	st.PairPriceDriftQuote = res.PairPriceDriftQuote
	switch {
	case closeErr != nil:
		st.NoteVI = closeErr.Error()
	case !v.Flat:
		// No error and not flat: execution closed what its grids allowed and
		// the pair still holds something on both legs. Noted, so the intent
		// stays tracked and the page keeps reading it from the venue.
		st.NoteVI = fmt.Sprintf("đóng %.8f coin nhưng CHƯA phẳng: còn %.8f coin mỗi chân theo execution, perp sàn báo %+.8f — kiểm lại",
			res.ClosedQtyCoin, res.RemainingQtyCoin, res.VenuePositionQtyCoin)
	case v.Flat:
		// A complete close proved both legs flat against the venue, so an
		// older note — a reconcile, a failed first attempt — no longer
		// describes this intent, and leaving it would keep the intent polled
		// forever. It is logged before it is dropped.
		if st.NoteVI != "" {
			log.Printf("execportal: %s closed flat; clearing the earlier note: %s", st.IntentID, st.NoteVI)
		}
		st.NoteVI = ""
		if reasonVI != "" {
			st.CloseReasonVI = reasonVI
		}
		// Both legs proved flat by execution's own machine. Hand the symbol
		// back — the coordinator reads every venue again before it writes idle,
		// so this never rests on what this process believes (Q22).
		if whyVI := p.releaseEngine1(ctx, st.Symbol, st.IntentID); whyVI != "" {
			v.ErrorVI = strings.TrimSpace(v.ErrorVI + " " + whyVI)
			st.NoteVI = whyVI
		}
	}
	if err := saveState(p.stateDir, st); err != nil {
		v.CacheErrorVI = "KHÔNG ghi được file ý định: " + err.Error()
	}
	return v, http.StatusOK
}

// ---------------------------------------------------------------- reconcile

type reconcileRequest struct {
	Symbol string `json:"symbol"`
	// Apply sends the squaring orders. Without it the answer is the plan only,
	// execcheck's -reconcile without -apply.
	Apply bool `json:"apply"`
	// PlanDigest is the plan_digest of the dry run the operator confirmed.
	// Required with Apply: the apply re-reads the venue, and if what it would
	// send is not what the dialog showed, it sends nothing.
	PlanDigest string `json:"plan_digest"`
}

// planDigest fingerprints exactly what an apply would send and the venue
// position it was judged against. Two plans with the same digest send the same
// orders; anything that moved between the dry run and the apply — a fill,
// another tool's order, a changed perp position — changes it.
func planDigest(v reconcileView) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|perp=%.10f|conflict=%t", v.Symbol, v.VenuePerpQtyCoin, v.ConflictVI != "")
	for _, pl := range v.Plans {
		fmt.Fprintf(h, "|%s:%s:%s:%s:%s:%.10f:%t", pl.IntentID, pl.Action, pl.Market, pl.Side, pl.ClientOrderID, pl.QtyCoin, pl.ReduceOnly)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

type squareResult struct {
	IntentID          string  `json:"intent_id"`
	ClientOrderID     string  `json:"client_order_id"`
	VenueOrderID      string  `json:"venue_order_id"`
	Status            string  `json:"status"`
	FilledQtyCoin     float64 `json:"filled_qty_coin"`
	AvgFillPriceQuote float64 `json:"avg_fill_price_quote"`
	ResidualAfterCoin float64 `json:"residual_after_coin"`
	Balanced          bool    `json:"balanced"`
	ErrorVI           string  `json:"error_vi"`
}

type reconcileView struct {
	Symbol   string `json:"symbol"`
	Apply    bool   `json:"apply"`
	ReadAtMs int64  `json:"read_at_ms"`

	VenuePerpQtyCoin   float64 `json:"venue_perp_qty_coin"`
	IntentsPerpQtyCoin float64 `json:"intents_perp_qty_coin"`
	ToleranceQtyCoin   float64 `json:"tolerance_qty_coin"`
	ConflictVI         string  `json:"conflict_vi"`

	IntentsScanned int            `json:"intents_scanned"`
	PlanDigest     string         `json:"plan_digest"`
	Plans          []squarePlan   `json:"plans"`
	Results        []squareResult `json:"results"`
	ToSend         int            `json:"to_send"`
	Refused        int            `json:"refused"`
	Balanced       int            `json:"balanced"`

	CacheUnreadableVI []string `json:"cache_unreadable_vi"`
	NoteVI            string   `json:"note_vi"`
	ErrorVI           string   `json:"error_vi"`
}

func (p *portal) handleReconcile(w http.ResponseWriter, r *http.Request) {
	var req reconcileRequest
	if !decodeBody(w, r, &req) {
		return
	}
	symbol, err := p.allowedSymbol(req.Symbol)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_symbol", err.Error())
		return
	}
	req.Symbol = symbol
	if err := p.markets.both(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "no_credentials", "thiếu credential, không cân được: "+err.Error())
		return
	}
	// A dry run takes the lock too: a plan read while an open is half-way
	// through describes a pair that is about to change.
	release, heldBy, since, ok := p.acquire("reconcile")
	if !ok {
		p.busy(w, heldBy, since)
		return
	}
	defer release()
	p.memo.forget()
	if req.Apply {
		defer p.afterOrderWrite()
	}

	ctx, cancel := p.actionContext(r)
	defer cancel()
	view, status := p.reconcile(ctx, req)
	if req.Apply {
		log.Printf("execportal: RECONCILE %s apply → sent %d, refused %d, balanced %d %s %s",
			view.Symbol, len(view.Results), view.Refused, view.Balanced, view.ConflictVI, view.ErrorVI)
	}
	writeJSON(w, status, view)
}

// reconcile scans EVERY cached intent for the symbol — not only the tracked
// ones — reads each one's orders from the venue, and squares the ones that are
// unbalanced by their own record.
//
// It refuses to send anything when the venue's perp position is not what the
// intents' own orders add up to. That disagreement means a position exists
// that no intent names, and a squaring order sized from the intents would be
// trading against it blind; both numbers are returned and a person decides.
func (p *portal) reconcile(ctx context.Context, req reconcileRequest) (v reconcileView, status int) {
	v = reconcileView{Symbol: req.Symbol, Apply: req.Apply,
		NoteVI: "Chỉ làm phẳng phần LỆCH của từng ý định (chân trần), theo lệnh của chính ý định đó đọc từ sàn. " +
			"Không đóng phần đã phòng hộ — muốn đóng cả cặp dùng ĐÓNG VỊ THẾ."}
	defer func() { v.ReadAtMs = p.now().UnixMilli() }()

	spotMkt, err := readMarket(ctx, p.markets.spot, req.Symbol)
	if err != nil {
		v.ErrorVI = "chân spot: " + err.Error()
		return v, http.StatusBadGateway
	}
	perpMkt, err := readMarket(ctx, p.markets.perp, req.Symbol)
	if err != nil {
		v.ErrorVI = "chân perp: " + err.Error()
		return v, http.StatusBadGateway
	}
	v.ToleranceQtyCoin = math.Max(spotMkt.Rules.StepSizeCoin, perpMkt.Rules.StepSizeCoin)

	pos, err := p.markets.perp.GetPosition(ctx, broker.MarketFuturesUSDM, req.Symbol)
	if err != nil {
		v.ErrorVI = "không đọc được vị thế perp từ sàn: " + err.Error()
		return v, http.StatusBadGateway
	}
	v.VenuePerpQtyCoin = pos.QtyCoin

	states, unreadableFiles, err := listStates(p.stateDir, req.Symbol)
	if err != nil {
		v.ErrorVI = "không đọc được thư mục ý định: " + err.Error()
		return v, http.StatusInternalServerError
	}
	v.CacheUnreadableVI = unreadableFiles
	ids := make([]string, 0, len(states))
	for _, s := range states {
		ids = append(ids, s.IntentID)
	}
	v.IntentsScanned = len(ids)
	hedges := p.intentHedges(ctx, req.Symbol, ids, true)

	incomplete := len(unreadableFiles) > 0
	for _, h := range hedges {
		v.IntentsPerpQtyCoin += h.Perp.QtyCoin
		if len(h.unreadable()) > 0 || len(h.working()) > 0 {
			incomplete = true
		}
		plan := planSquare(h, spotMkt.Rules.Instrument, perpMkt.Rules.Instrument, spotMkt.PriceQuote, perpMkt.PriceQuote)
		switch plan.Action {
		case "send":
			v.ToSend++
		case "refuse":
			v.Refused++
		default:
			v.Balanced++
			continue // a balanced intent is not news; the counts say how many
		}
		v.Plans = append(v.Plans, plan)
	}
	if math.Abs(v.VenuePerpQtyCoin-v.IntentsPerpQtyCoin) > v.ToleranceQtyCoin+gridEpsilon {
		v.ConflictVI = fmt.Sprintf("SÀN báo vị thế perp %+.8f coin, lệnh của %d ý định giải thích %+.8f coin — có vị thế không ý định nào nhận; KHÔNG tự cân, người vận hành quyết",
			v.VenuePerpQtyCoin, len(ids), v.IntentsPerpQtyCoin)
	}

	v.PlanDigest = planDigest(v)
	if !req.Apply {
		return v, http.StatusOK
	}
	switch {
	case req.PlanDigest == "" || req.PlanDigest != v.PlanDigest:
		v.ErrorVI = "không gửi lệnh nào: kế hoạch trên sàn lúc này KHÁC kế hoạch đã xác nhận (plan_digest " +
			quoteForMessage(req.PlanDigest) + " → " + v.PlanDigest + ") — xem lại rồi xác nhận lần nữa"
		return v, http.StatusConflict
	case v.ConflictVI != "":
		v.ErrorVI = "không gửi lệnh nào: " + v.ConflictVI
		return v, http.StatusConflict
	case incomplete:
		v.ErrorVI = "không gửi lệnh nào: có ý định hoặc lệnh không đọc được / còn đang chạy, nên tổng trên chưa đầy đủ"
		return v, http.StatusConflict
	case v.ToSend == 0:
		return v, http.StatusOK
	}

	for _, plan := range v.Plans {
		if plan.Action != "send" {
			continue
		}
		v.Results = append(v.Results, p.sendSquare(ctx, req.Symbol, plan, spotMkt, perpMkt))
	}
	return v, http.StatusOK
}

// sendSquare places one squaring order, reads it back until the venue says it
// is finished — an acknowledgement is not a fill, the very mistake reconcile
// exists to clean up after — and re-reads the intent's balance. Whatever
// happens, the intent's note records it, so the intent stays tracked.
func (p *portal) sendSquare(ctx context.Context, symbol string, plan squarePlan, spotMkt, perpMkt market) squareResult {
	out := squareResult{IntentID: plan.IntentID, ClientOrderID: plan.ClientOrderID}
	var b broker.Broker = p.markets.spot
	if plan.Market == broker.MarketFuturesUSDM {
		b = p.markets.perp
	}
	q := broker.OrderQuery{Market: plan.Market, Symbol: symbol, ClientOrderID: plan.ClientOrderID}
	order, err := b.PlaceOrder(ctx, broker.PlaceOrderRequest{
		Market: plan.Market, Symbol: symbol, Side: plan.Side, Type: broker.OrderTypeMarket,
		ClientOrderID: plan.ClientOrderID, QtyCoin: plan.QtyCoin, ReduceOnly: plan.ReduceOnly,
	})
	if err != nil {
		// Refused or ambiguous: ask the venue by the id chosen before sending,
		// and keep asking while the answer means nothing (broker.Broker's
		// third branch). Not found means it never arrived.
		found, qErr := resolveOrder(ctx, b, q, 10*time.Second)
		switch {
		case errors.Is(qErr, broker.ErrOrderNotFound):
			// Not conclusive right after an ambiguous error — the venue may not
			// have indexed it yet — so it is worded as what was seen.
			out.ErrorVI = "lệnh cân hỏng và CHƯA THẤY trên sàn khi hỏi lại theo id: " + err.Error() + " — đọc lại vị thế trước khi cân lần nữa"
			p.noteSquare(plan, "chưa thấy trên sàn: "+err.Error(), &out)
			return out
		case qErr != nil:
			out.ErrorVI = "lệnh cân hỏng: " + err.Error() + " — và KHÔNG xác nhận được lệnh có tới sàn hay không: " + qErr.Error()
			p.noteSquare(plan, "KHÔNG xác nhận được: "+qErr.Error(), &out)
			return out
		}
		order = found
	}
	order = settle(ctx, b, q, order, 10*time.Second)
	out.VenueOrderID, out.Status = order.VenueOrderID, string(order.Status)
	out.FilledQtyCoin, out.AvgFillPriceQuote = order.FilledQtyCoin, order.AvgFillPriceQuote

	// Inside the write, after THIS intent's one squaring order was sent and read
	// back: that order must be seen, every other id of the intent predates the
	// write (review 4.5j part 4, R2).
	h := p.intentHedges(ctx, symbol, []string{plan.IntentID}, true, plan.ClientOrderID)[0]
	out.ResidualAfterCoin = h.residualCoin()
	tolerance := math.Max(spotMkt.Rules.StepSizeCoin, perpMkt.Rules.StepSizeCoin)
	out.Balanced = len(h.unreadable()) == 0 && len(h.working()) == 0 && math.Abs(out.ResidualAfterCoin) <= tolerance+gridEpsilon
	if !out.Balanced {
		out.ErrorVI = strings.TrimPrefix(out.ErrorVI+fmt.Sprintf(" · VẪN LỆCH %+.8f coin (hoặc chưa đọc đủ) sau khi cân — dừng và xử lý tay", out.ResidualAfterCoin), " · ")
	}
	p.noteSquare(plan, fmt.Sprintf("%s, lệch còn %+.8f", order.Status, out.ResidualAfterCoin), &out)
	return out
}

func (p *portal) noteSquare(plan squarePlan, whatVI string, out *squareResult) {
	st, err := loadState(p.stateDir, plan.IntentID)
	if err != nil {
		out.ErrorVI = strings.TrimPrefix(out.ErrorVI+" · không đọc được file ý định để ghi chú: "+err.Error(), " · ")
		return
	}
	st.NoteVI = fmt.Sprintf("đã cân lại %s qua execportal: gửi %s %.8f coin trên %s (id %s) — %s",
		time.Now().Format(time.RFC3339), plan.Side, plan.QtyCoin, plan.Market, plan.ClientOrderID, whatVI)
	if err := saveState(p.stateDir, st); err != nil {
		out.ErrorVI = strings.TrimPrefix(out.ErrorVI+" · không ghi được file ý định: "+err.Error(), " · ")
	}
}

// resolveOrder asks the venue about one order until the answer means
// something — found, or positively not found — or `within` has passed.
func resolveOrder(ctx context.Context, b broker.Broker, q broker.OrderQuery, within time.Duration) (broker.Order, error) {
	deadline := time.Now().Add(within)
	for {
		order, err := b.GetOrder(ctx, q)
		if err == nil || errors.Is(err, broker.ErrOrderNotFound) || !time.Now().Before(deadline) {
			return order, err
		}
		select {
		case <-ctx.Done():
			return order, err
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// settle reads an order back until the venue says it is finished or `within`
// has passed, and returns the newest answer it got.
func settle(ctx context.Context, b broker.Broker, q broker.OrderQuery, order broker.Order, within time.Duration) broker.Order {
	deadline := time.Now().Add(within)
	for !order.Status.Done() && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return order
		case <-time.After(200 * time.Millisecond):
		}
		if latest, err := b.GetOrder(ctx, q); err == nil {
			order = latest
		}
	}
	return order
}
