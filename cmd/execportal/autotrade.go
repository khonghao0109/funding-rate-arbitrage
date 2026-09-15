package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"futures-arbitrage-scanner/cmd/execportal/autotrade"
	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
)

// The testnet auto-trader's side of the portal (PLAN §7.1 Q18).
//
// package autotrade decides; this file is everything it is allowed to touch,
// and it is deliberately thin:
//
//   - portalMarket reads the two testnet markets — books, mark price and
//     forming rate, the settled rates, this account's fees — through the same
//     venue clients the page reads with, under the same half-of-the-budget
//     read wall.
//   - portalTrader reads the hedge through readPositions (the venue's orders
//     and position, exactly what the banner shows) and opens and closes
//     through openAs and close — the functions a button press runs, under the
//     same write lock, so a manual write and a bot write can never overlap and
//     neither can reach the venue by a road the other does not take.
//     guard_test.go holds this file to that: no order is placed here, and no
//     execution machine is built here.
//   - The four endpoints: status, and start / stop / kill behind the same
//     header, origin and JSON walls as every other write.

// The action header values of the bot's writes. A write is refused unless it
// carries its own name, which only the page's confirmed path sends.
const (
	autotradeStartAction = "autotrade-start"
	// autotradeStopAction keeps the position and sends no order, so the page
	// sends it without a dialog; a stop that CLOSES sends orders and carries
	// its own name, which only a confirmed path may send.
	autotradeStopAction      = "autotrade-stop"
	autotradeStopCloseAction = "autotrade-stop-close"
	autotradeKillAction      = "autotrade-kill"

	// The portal's write lock names what holds it; these appear on the page's
	// busy line when a button press finds the bot mid-trade.
	autotradeOpenLock  = "autotrade-open"
	autotradeCloseLock = "autotrade-close"
)

// Caching of the two slow-moving reads a scan makes every few seconds.
const (
	// commissionTTL: a fee schedule does not change between scans, and each
	// read costs weight 20 on each market.
	commissionTTL = 10 * time.Minute
	// fundingRatesTTL: a new settlement is seen at most this late, which the
	// exits can afford — they act on what settled, and the next settlement is
	// hours away. The endpoint's own limit is 500 per five minutes.
	fundingRatesTTL = 2 * time.Minute
	// maxFundingRatePages bounds the paging of a long hold's history.
	maxFundingRatePages = 10
)

type commissionPair struct {
	Spot binancebroker.CommissionRates
	Perp binancebroker.CommissionRates
}

// newAutotrade builds the portal's engine. It does not run until main starts
// Run, and it trades nothing until someone presses BẬT (or -autotrade).
func newAutotrade(p *portal) *autotrade.Engine {
	symbol := "BTCUSDT"
	if len(p.symbols) > 0 {
		symbol = p.symbols[0]
	}
	eng, err := autotrade.New(autotrade.Options{
		Market: portalMarket{p}, Trader: portalTrader{p},
		DefaultSymbol: symbol, MaxNotionalQuote: maxNotionalQuote,
		PerpMarginFrac: p.exec.MarginFrac, ActionTimeout: max(p.exec.ActionTimeout, time.Minute),
		Now: p.now, Logf: log.Printf,
	})
	if err != nil {
		// Every option is the portal's own; a refusal is a programming error.
		panic(err)
	}
	return eng
}

// ------------------------------------------------------------------ market

type portalMarket struct{ p *portal }

func (m portalMarket) Snapshot(ctx context.Context, symbol string, settledSinceMs int64) (autotrade.Snapshot, error) {
	p := m.p
	snap := autotrade.Snapshot{Symbol: symbol}
	if err := p.markets.both(); err != nil {
		return snap, err
	}
	if err := p.markets.readBudgetError(); err != nil {
		return snap, err
	}
	var err error
	if snap.SpotBook, err = readBook(ctx, p.markets.spot, symbol); err != nil {
		return snap, fmt.Errorf("sổ spot: %w", err)
	}
	if snap.PerpBook, err = readBook(ctx, p.markets.perp, symbol); err != nil {
		return snap, fmt.Errorf("sổ perp: %w", err)
	}
	mp, err := p.markets.perp.MarkPrice(ctx, broker.MarketFuturesUSDM, symbol)
	if err != nil {
		return snap, fmt.Errorf("premiumIndex: %w", err)
	}
	snap.ForecastRatePerIntervalFrac, snap.NextFundingTimeMs = mp.LastFundingRateFrac, mp.NextFundingTimeMs
	snap.ReadAtMs = p.now().UnixMilli()

	if rows, err := p.settledRates(ctx, symbol, settledSinceMs, mp.NextFundingTimeMs); err != nil {
		snap.SettledErrVI = err.Error()
	} else {
		for _, r := range rows {
			if r.SettledAtMs < settledSinceMs {
				continue
			}
			snap.Settled = append(snap.Settled, autotrade.SettledRate{
				SettledAtMs: r.SettledAtMs, RatePerIntervalFrac: r.RatePerIntervalFrac, Special: strings.EqualFold(r.RateType, "Special"),
			})
		}
	}

	fees, _, err := p.commissions.get(symbol, commissionTTL, func() (commissionPair, error) {
		spot, err := p.markets.spot.CommissionRates(ctx, symbol)
		if err != nil {
			return commissionPair{}, fmt.Errorf("spot: %w", err)
		}
		perp, err := p.markets.perp.CommissionRates(ctx, symbol)
		if err != nil {
			return commissionPair{}, fmt.Errorf("futures: %w", err)
		}
		return commissionPair{Spot: spot, Perp: perp}, nil
	})
	if err != nil {
		snap.FeesErrVI = err.Error()
	} else {
		snap.SpotTakerFeeBps, snap.PerpTakerFeeBps = fees.Spot.TakerBps(), fees.Perp.TakerBps()
		snap.FeeSourceVI = fees.Spot.SourceVI + " · " + fees.Perp.SourceVI
	}
	// The offset every signed request is corrected by, as the broker last
	// measured it — read, never re-measured here: a sync taken while an order
	// is being signed is how a -1021 happens (api.go).
	for _, pair := range []struct {
		c   venue
		dst **int64
	}{{p.markets.spot, &snap.SpotClockSkewMs}, {p.markets.perp, &snap.PerpClockSkewMs}} {
		if h := pair.c.HTTP(); !h.ClockMeasuredAt().IsZero() {
			skew := h.ClockSkewMs()
			*pair.dst = &skew
		}
	}
	return snap, nil
}

// settledRates reads every settlement from sinceMs to now, oldest first.
//
// It pages until the venue answers with nothing more, rather than stopping at
// a short page: the venue returns the OLDEST rows first when a window holds
// more than it sends, and the testnet's real page ceiling has not been
// measured, so a short page is not proof the newest settlements — the ones the
// exits read — were included.
//
// The cache is keyed by the hour the window starts in AND by the venue's next
// settlement stamp, so a settlement that has just happened moves the key and is
// read at the next scan instead of after the TTL. The caller filters to its
// exact start.
func (p *portal) settledRates(ctx context.Context, symbol string, sinceMs, nextFundingTimeMs int64) ([]binancebroker.FundingRate, error) {
	hourMs := time.Hour.Milliseconds()
	startMs := sinceMs / hourMs * hourMs
	key := fmt.Sprintf("%s|%d|%d", symbol, startMs, nextFundingTimeMs)
	// The key moves every hour and every settlement; the cache keeps only the
	// newest one rather than growing for the life of the process.
	p.fundingKeyMu.Lock()
	if key != p.fundingKey {
		p.fundingRates.invalidate()
		p.fundingKey = key
	}
	p.fundingKeyMu.Unlock()
	rows, _, err := p.fundingRates.get(key, fundingRatesTTL, func() ([]binancebroker.FundingRate, error) {
		var all []binancebroker.FundingRate
		from := startMs
		endMs := p.now().UnixMilli()
		seen := map[int64]bool{}
		for page := 0; page < maxFundingRatePages; page++ {
			batch, err := p.markets.perp.FundingRateHistory(ctx, symbol, from, endMs)
			if err != nil {
				return nil, err
			}
			// Only rows inside the window asked for, each stamp once: a venue
			// that ignored startTime and answered the same page again would
			// otherwise double every settlement — and with it the count a
			// MaxHoldEpochs exit closes on.
			var last int64
			added := 0
			for _, r := range batch {
				if r.SettledAtMs < from || r.SettledAtMs > endMs || seen[r.SettledAtMs] {
					continue
				}
				seen[r.SettledAtMs] = true
				all = append(all, r)
				added++
				last = max(last, r.SettledAtMs)
			}
			switch {
			case len(batch) > 0 && added == 0:
				// A page of rows none of which is new: the venue answered
				// outside the window asked for, and the newest settlements —
				// the ones the exits read — may be missing.
				return nil, errors.New("sàn trả lại các dòng funding ngoài cửa sổ đã hỏi — không dùng một chuỗi có thể thiếu mốc mới nhất")
			case added == 0 || last >= endMs:
				return all, nil
			}
			from = last + 1
		}
		return nil, fmt.Errorf("hơn %d trang lịch sử funding — không dùng một chuỗi đọc chưa hết", maxFundingRatePages)
	})
	return rows, err
}

// ------------------------------------------------------------------ trader

type portalTrader struct{ p *portal }

// Holding is the banner's own reading: readPositions, from the venue.
func (t portalTrader) Holding(ctx context.Context, symbol string) (autotrade.Holding, error) {
	p := t.p
	v := p.readPositions(ctx, symbol)
	h := autotrade.Holding{Status: autotrade.HedgeStatus(v.Status), ReasonVI: strings.TrimPrefix(v.ReasonVI+" · "+v.ErrorVI, " · ")}
	h.ReasonVI = strings.TrimSuffix(h.ReasonVI, " · ")
	tol := v.ToleranceQtyCoin + gridEpsilon
	var held []intentHedgeView
	for _, in := range v.Intents {
		if math.Abs(in.Spot.QtyCoin) > tol || math.Abs(in.Perp.QtyCoin) > tol {
			held = append(held, in)
		}
	}
	h.HeldIntents = len(held)
	if len(held) == 1 {
		h.IntentID = held[0].IntentID
		h.FromAutotrade = originOf(h.IntentID) == "autotrade"
		h.QtyCoin = -held[0].Perp.QtyCoin
		// The cache is read only for what the venue cannot say: when the
		// intent opened and the mids its entry was decided on.
		if st, err := loadState(p.stateDir, h.IntentID); err == nil {
			h.OpenedAtMs, h.SpotRefMidQuote, h.PerpRefMidQuote = st.OpenedAtMs, st.SpotRefMidQuote, st.PerpRefMidQuote
		}
	}
	return h, nil
}

func (t portalTrader) Open(ctx context.Context, order autotrade.OpenOrder) autotrade.OpenResult {
	p := t.p
	cost := order.SignalEntryCostPct
	req := openRequest{Symbol: order.Symbol, NotionalQuote: order.NotionalQuote, SignalEntryCostPct: &cost}
	if err := p.validateOpen(&req); err != nil {
		return autotrade.OpenResult{Refused: true, ErrorVI: err.Error()}
	}
	if err := p.markets.both(); err != nil {
		return autotrade.OpenResult{Refused: true, ErrorVI: "thiếu credential: " + err.Error()}
	}
	release, heldBy, _, ok := p.acquire(autotradeOpenLock)
	if !ok {
		return autotrade.OpenResult{Busy: true, ErrorVI: "khoá ghi đang do " + orUnknown(heldBy) + " giữ"}
	}
	defer release()
	defer p.invalidateVenueReads()
	p.memo.forget()

	v, _ := p.openAs(ctx, req, intentPrefixAutotrade)
	log.Printf("execportal: AUTOTRADE OPEN %s %s %.2f quote → %s hedged=%v alarm=%v %s",
		v.IntentID, v.Symbol, v.NotionalQuote, v.Outcome, v.Hedged, v.Alarm, v.ErrorVI)
	errorVI := v.ErrorVI
	if v.CacheErrorVI != "" {
		errorVI = strings.TrimPrefix(errorVI+" · "+v.CacheErrorVI, " · ")
	}
	return autotrade.OpenResult{
		IntentID: v.IntentID, Refused: v.RefusedBeforePlacing, Hedged: v.Hedged, Alarm: v.Alarm,
		OpenedAtMs: v.OpenedAtMs, QtyCoin: v.Perp.FilledQtyCoin, ResidualQtyCoin: v.ResidualQtyCoin,
		SpotRefMidQuote: v.SpotRefMidQuote, PerpRefMidQuote: v.PerpRefMidQuote,
		UnhedgedWindowMs: v.UnhedgedWindowMs, UnwindDurationMs: v.UnwindDurationMs, ErrorVI: errorVI,
	}
}

func (t portalTrader) Close(ctx context.Context, symbol, intentID string) autotrade.CloseResult {
	p := t.p
	out := autotrade.CloseResult{IntentID: intentID, Refused: true}
	symbol, err := p.allowedSymbol(symbol)
	if err != nil {
		out.ErrorVI = err.Error()
		return out
	}
	if err := validIntentID(intentID); err != nil {
		out.ErrorVI = err.Error()
		return out
	}
	if err := p.markets.both(); err != nil {
		out.ErrorVI = "thiếu credential: " + err.Error()
		return out
	}
	release, heldBy, _, ok := p.acquire(autotradeCloseLock)
	if !ok {
		return autotrade.CloseResult{IntentID: intentID, Busy: true, ErrorVI: "khoá ghi đang do " + orUnknown(heldBy) + " giữ"}
	}
	defer release()
	defer p.invalidateVenueReads()
	p.memo.forget()

	st, err := loadState(p.stateDir, intentID)
	if err != nil {
		out.ErrorVI = "không đọc được file ý định " + intentID + ": " + err.Error()
		return out
	}
	if st.Symbol != symbol {
		out.ErrorVI = fmt.Sprintf("ý định %s là %s, không phải %s", intentID, st.Symbol, symbol)
		return out
	}
	v, _ := p.close(ctx, st)
	log.Printf("execportal: AUTOTRADE CLOSE %s %s → %s flat=%v refused=%v alarm=%v closed=%.8f %s",
		v.IntentID, v.Symbol, v.Outcome, v.Flat, v.Refused, v.Alarm, v.ClosedQtyCoin, v.ErrorVI)
	errorVI := v.ErrorVI
	if v.CacheErrorVI != "" {
		errorVI = strings.TrimPrefix(errorVI+" · "+v.CacheErrorVI, " · ")
	}
	return autotrade.CloseResult{
		IntentID: intentID, Refused: v.Refused, SentUnconfirmed: v.SentUnconfirmed, Flat: v.Flat, Alarm: v.Alarm,
		ClosedQtyCoin: v.ClosedQtyCoin, RemainingQtyCoin: v.RemainingQtyCoin,
		FundingReceivedQuote: v.FundingReceivedQuote, SettlementsCounted: v.SettlementsCounted,
		RealizedQuote: v.RealizedQuote, ErrorVI: errorVI,
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "một thao tác khác"
	}
	return s
}

// --------------------------------------------------------------- endpoints

// autotradeActionView answers every bot write: the status after it, what a
// stop-and-close or a kill did to the position, and the position a stop kept.
type autotradeActionView struct {
	Status       autotrade.StatusView     `json:"status"`
	Close        *autotrade.OperatorClose `json:"close"`
	KeptIntentID string                   `json:"kept_intent_id"`
}

func (p *portal) handleAutotradeStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.autotrade.Status())
}

// autotradeStartRequest is the page's form. An absent field takes the shipped
// default; a present one is validated, never clamped.
type autotradeStartRequest struct {
	Symbol        string   `json:"symbol"`
	NotionalQuote *float64 `json:"notional_quote"`
	MinNetAPRPct  *float64 `json:"min_net_apr_pct"`
	MaxHoldEpochs *int     `json:"max_hold_epochs"`
}

func (p *portal) handleAutotradeStart(w http.ResponseWriter, r *http.Request) {
	var req autotradeStartRequest
	if !decodeBody(w, r, &req) {
		return
	}
	symbol, err := p.allowedSymbol(req.Symbol)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_symbol", err.Error()+" — bot KHÔNG bật")
		return
	}
	if err := p.markets.both(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "no_credentials", "thiếu credential, bot không bật: "+err.Error())
		return
	}
	cfg := autotrade.DefaultConfig(symbol)
	if req.NotionalQuote != nil {
		cfg.NotionalQuote = *req.NotionalQuote
	}
	if req.MinNetAPRPct != nil {
		cfg.MinNetAPRPct = *req.MinNetAPRPct
	}
	if req.MaxHoldEpochs != nil {
		cfg.MaxHoldEpochs = *req.MaxHoldEpochs
	}
	st, err := p.autotrade.Start(cfg)
	if err != nil {
		code, status := "autotrade_refused", http.StatusBadRequest
		if errors.Is(err, autotrade.ErrBusy) || errors.Is(err, autotrade.ErrNotStartable) {
			code, status = "autotrade_conflict", http.StatusConflict
		}
		writeError(w, status, code, err.Error()+" — bot KHÔNG bật")
		return
	}
	writeJSON(w, http.StatusOK, autotradeActionView{Status: st})
}

type autotradeStopRequest struct {
	CloseNow bool `json:"close_now"`
}

func (p *portal) handleAutotradeStop(w http.ResponseWriter, r *http.Request) {
	var req autotradeStopRequest
	if !decodeBody(w, r, &req) {
		return
	}
	// The route accepts both names; the body must agree with the one sent, so
	// the no-dialog name can never carry a close.
	if want := map[bool]string{false: autotradeStopAction, true: autotradeStopCloseAction}[req.CloseNow]; r.Header.Get(actionHeader) != want {
		writeError(w, http.StatusForbidden, "action_header_mismatch",
			"close_now="+fmt.Sprint(req.CloseNow)+" cần header "+actionHeader+": "+want+" — dừng-và-đóng gửi lệnh và chỉ đi sau hộp xác nhận")
		return
	}
	// Detached from the request: a tab closed half-way through a close must not
	// cancel the close (actions.go).
	st, out, err := p.autotrade.Stop(context.WithoutCancel(r.Context()), req.CloseNow)
	if err != nil {
		writeError(w, http.StatusConflict, "autotrade_conflict", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, autotradeActionView{Status: st, Close: out.Close, KeptIntentID: out.KeptIntentID})
}

type autotradeKillRequest struct{}

func (p *portal) handleAutotradeKill(w http.ResponseWriter, r *http.Request) {
	var req autotradeKillRequest
	if !decodeBody(w, r, &req) {
		return
	}
	st, oc, err := p.autotrade.Kill(context.WithoutCancel(r.Context()))
	if err != nil {
		writeError(w, http.StatusConflict, "autotrade_conflict", err.Error())
		return
	}
	log.Printf("execportal: AUTOTRADE KILL → %s flat=%v %s", st.State, oc != nil && oc.Flat, st.HaltReasonVI)
	writeJSON(w, http.StatusOK, autotradeActionView{Status: st, Close: oc})
}
