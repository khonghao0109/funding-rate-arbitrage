package autotrade

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// HedgeStatus is the portal's hedge verdict for one symbol, read from the venue.
type HedgeStatus string

const (
	HedgeBothOpen         HedgeStatus = "both_open"
	HedgeBothFlat         HedgeStatus = "both_flat"
	HedgeUnhedged         HedgeStatus = "unhedged"
	HedgeEvidenceConflict HedgeStatus = "evidence_conflict"
	HedgeUnknown          HedgeStatus = "unknown"
)

// Holding is what the venue says one symbol holds, as the portal reads it
// (rule 7: from the venue's orders and position, never from the cache).
type Holding struct {
	Status   HedgeStatus
	ReasonVI string

	// HeldIntents is how many tracked intents still hold something by their
	// own orders. IntentID is that intent when there is exactly one.
	HeldIntents int
	IntentID    string
	// FromAutotrade is the intent id having been minted by this bot.
	FromAutotrade bool

	// From the intent's cache file, for adopting a pair after a restart: when it
	// opened and the two mids its entry was decided on.
	OpenedAtMs      int64
	SpotRefMidQuote float64
	PerpRefMidQuote float64
	QtyCoin         float64
}

// OpenOrder is what the bot asks the portal to open.
type OpenOrder struct {
	Symbol        string
	NotionalQuote float64
	// SignalEntryCostPct is the entry's slippage as the scan priced it, in
	// PERCENT. execution re-prices the book immediately before placing and
	// refuses when it has widened past its tolerance — the gap between a
	// decision and an order is exactly what that check is for.
	SignalEntryCostPct float64
}

// OpenResult is the portal's open, reduced to what the machine branches on.
type OpenResult struct {
	IntentID string
	// Busy: another write held the portal's lock, and nothing was sent.
	Busy bool
	// Refused: nothing reached the venue.
	Refused bool
	// Hedged: both legs open within the coarser step.
	Hedged bool
	// Alarm: the invariant may not hold — an unwind that could not complete or
	// two flat proofs that disagree.
	Alarm bool

	OpenedAtMs       int64
	QtyCoin          float64
	ResidualQtyCoin  float64
	SpotRefMidQuote  float64
	PerpRefMidQuote  float64
	UnhedgedWindowMs int64
	UnwindDurationMs int64
	ErrorVI          string
}

// CloseResult is the portal's close, reduced the same way.
type CloseResult struct {
	IntentID string `json:"intent_id"`
	Busy     bool   `json:"busy"`
	// Refused: nothing was sent and both legs are as they were.
	Refused bool `json:"refused"`
	// SentUnconfirmed: a closing order reached the venue and filled nothing
	// the portal could confirm.
	SentUnconfirmed bool `json:"sent_unconfirmed"`
	Flat            bool `json:"flat"`
	Alarm           bool `json:"alarm"`

	ClosedQtyCoin        float64 `json:"closed_qty_coin"`
	RemainingQtyCoin     float64 `json:"remaining_qty_coin"`
	FundingReceivedQuote float64 `json:"funding_received_quote"`
	SettlementsCounted   int     `json:"settlements_counted"`
	// RealizedQuote is execution's figure: funding minus commission minus
	// slippage, NOT net — the pair's price drift is outside it.
	RealizedQuote float64 `json:"realized_quote"`
	ErrorVI       string  `json:"error_vi"`
}

// Market reads both testnet markets for one symbol, with every settlement
// listed at or after settledSinceMs.
type Market interface {
	Snapshot(ctx context.Context, symbol string, settledSinceMs int64) (Snapshot, error)
}

// Trader is the portal's order path. The engine holds no broker: everything it
// sends goes through here, under the portal's write lock, through the same
// execution machine a button press runs.
type Trader interface {
	Holding(ctx context.Context, symbol string) (Holding, error)
	Open(ctx context.Context, order OpenOrder) OpenResult
	Close(ctx context.Context, symbol, intentID string) CloseResult
}

// Options builds an Engine.
type Options struct {
	Market Market
	Trader Trader

	// DefaultSymbol is the symbol a kill or a stop acts on before any run has
	// named one.
	DefaultSymbol string
	// MaxNotionalQuote is the portal's ceiling per leg; a start above it is
	// refused.
	MaxNotionalQuote float64
	// PerpMarginFrac is the portal's collateral decision, for the on-capital
	// figure beside every APR.
	PerpMarginFrac float64
	// ActionTimeout bounds one open or close.
	ActionTimeout time.Duration

	Now  func() time.Time
	Logf func(format string, args ...any)
}

// Engine is the auto-trader. One goroutine runs it (Run); Start, Stop, Kill and
// Status are safe from any other.
type Engine struct {
	market        Market
	trader        Trader
	now           func() time.Time
	logf          func(format string, args ...any)
	defaultSymbol string
	maxNotional   float64
	marginFrac    float64
	actionTimeout time.Duration

	wake chan struct{}

	// opMu is held by whatever is touching the venue for a decision: one Step,
	// one Stop, one Kill. Two of them never trade at the same time, and a kill
	// that arrives during an open waits for that open to RETURN — execution's
	// invariant is a promise about calls that return — before it closes.
	opMu sync.Mutex

	// mu guards everything below. It is never held across a venue call
	// (CONVENTIONS §9).
	mu            sync.Mutex
	state         State
	stateSince    time.Time
	cfg           Config
	signal        *SignalView
	pos           *PositionView
	cooldownUntil time.Time
	readFailures  int
	tradeFailures int
	haltReasonVI  string
	log           ringLog
	lastScanAt    time.Time
	nextScanAt    time.Time
	lastEntryKey  string
	// busy names the operator action running now ("stop", "kill"). A kill
	// takes it over from a stop; a stop clears only its own.
	busy string
	// killing makes a Step already in flight record what it did (a position it
	// just opened, above all) and change no state: the kill decides the state.
	killing bool
	// stopping is a stop waiting for the Step in flight: that Step sends no
	// order it had not already sent when the operator pressed DỪNG.
	stopping bool
	// stopSeq names the stop that owns stopping and busy=="stop"; only that
	// stop clears them.
	stopSeq int
	// haltSeq counts halts. A stop may acknowledge only the halt the operator
	// saw when pressing it: one raised while the stop waited — an alarm from
	// the open in flight, an unhedged read — is refused, never cleared unseen.
	haltSeq int
	// killSeq counts kills. A stop remembers it when pressed; a kill that ran
	// while the stop waited supersedes the stop, which must not then return a
	// halted bot to disabled — that acknowledgement is the operator's to give
	// after seeing why it halted.
	killSeq    int
	cancelScan context.CancelFunc
	// historyBlindSince is when the funding history became unreadable while
	// holding; zero while it reads.
	historyBlindSince time.Time
}

// New builds a disabled engine.
func New(o Options) (*Engine, error) {
	if o.Market == nil || o.Trader == nil {
		return nil, errors.New("autotrade: a Market and a Trader are required")
	}
	if o.DefaultSymbol == "" || !(o.MaxNotionalQuote > 0) || o.ActionTimeout <= 0 || o.PerpMarginFrac < 0 {
		return nil, fmt.Errorf("autotrade: incomplete options (symbol %q, max notional %v, action timeout %s, margin %v)",
			o.DefaultSymbol, o.MaxNotionalQuote, o.ActionTimeout, o.PerpMarginFrac)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	e := &Engine{
		market: o.Market, trader: o.Trader, now: o.Now, logf: o.Logf,
		defaultSymbol: o.DefaultSymbol, maxNotional: o.MaxNotionalQuote, marginFrac: o.PerpMarginFrac,
		actionTimeout: o.ActionTimeout, wake: make(chan struct{}, 1),
		state: StateDisabled, cfg: DefaultConfig(o.DefaultSymbol),
	}
	e.stateSince = e.now()
	return e, nil
}

// maxHistoryBlind is how long the bot holds a pair while the settled-funding
// history cannot be read before it halts. A time, not a count of scans: exits
// act on settlements hours apart, so a few minutes of a rate-limited or flaky
// history endpoint cost nothing, while a history that stays unreadable must not
// hold a pair through settlements nobody can see.
const maxHistoryBlind = 30 * time.Minute

// scanTimeout bounds one scan's reads.
const scanTimeout = 30 * time.Second

// idleDelay is how long Run sleeps while nothing is enabled; Start wakes it.
const idleDelay = time.Hour

// ErrBusy is an operator action refused because another one is running.
var ErrBusy = errors.New("autotrade: một thao tác khác của bot đang chạy")

// ErrNotStartable is Start refused in the current state.
var ErrNotStartable = errors.New("autotrade: không bật được ở trạng thái hiện tại")

// Run drives the machine until ctx ends. An open or close in flight when ctx
// ends is NOT cancelled — it runs to its own deadline, because a half-finished
// open is what the invariant forbids — and Run returns after it. A held position
// is kept; the next Start adopts it.
func (e *Engine) Run(ctx context.Context) {
	for {
		e.Step(ctx)
		delay := e.scheduleNext()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			e.mu.Lock()
			if e.state != StateDisabled && e.state != StateEmergencyHalted {
				what := "không giữ vị thế nào"
				if e.pos != nil {
					what = "vị thế " + e.pos.IntentID + " GIỮ NGUYÊN trên sàn — lần BẬT sau sẽ tiếp nhận"
				}
				e.addLogLocked("STOP", "portal tắt — bot dừng, "+what)
				e.setStateLocked(StateDisabled, true)
			}
			e.mu.Unlock()
			return
		case <-e.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (e *Engine) scheduleNext() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	delay := e.cfg.ScanInterval
	switch e.state {
	case StateDisabled, StateEmergencyHalted:
		delay = idleDelay
	case StateCooldown:
		if left := e.cooldownUntil.Sub(now); left < delay {
			delay = max(left, 0)
		}
	}
	e.nextScanAt = now.Add(delay)
	if delay == idleDelay {
		e.nextScanAt = time.Time{}
	}
	return delay
}

func (e *Engine) poke() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Step runs one scan now: what Run does on its timer.
func (e *Engine) Step(ctx context.Context) {
	e.opMu.Lock()
	defer e.opMu.Unlock()

	e.mu.Lock()
	if e.killing || e.stopping || ctx.Err() != nil {
		e.mu.Unlock()
		return
	}
	switch e.state {
	case StateDisabled, StateEmergencyHalted:
		e.mu.Unlock()
		return
	case StateCooldown:
		if e.now().Before(e.cooldownUntil) {
			e.mu.Unlock()
			return
		}
		e.setStateLocked(StateIdleScanning, false)
	}
	cfg := e.cfg
	var pos *PositionView
	if e.pos != nil {
		p := *e.pos
		pos = &p
	}
	if pos == nil {
		e.setStateLocked(StateEvaluating, false)
	}
	scanCtx, cancel := context.WithTimeout(ctx, scanTimeout)
	e.cancelScan = cancel
	e.lastScanAt = e.now()
	e.mu.Unlock()

	defer func() {
		cancel()
		e.mu.Lock()
		e.cancelScan = nil
		e.mu.Unlock()
	}()
	if pos == nil {
		e.scanFlat(ctx, scanCtx, cfg)
		return
	}
	e.scanHolding(ctx, scanCtx, cfg, *pos)
}

// interrupted reports a scan that ended because an operator stopped or killed
// the bot, or the portal is shutting down — not a venue failure.
func interrupted(ctx, scanCtx context.Context) bool {
	return ctx.Err() != nil || errors.Is(scanCtx.Err(), context.Canceled)
}

// interruptedLocked reports an operator stop or kill, or the portal shutting
// down, which a decision already made must not outlive. Caller holds mu.
func (e *Engine) interruptedLocked(ctx context.Context) bool {
	return e.killing || e.stopping || ctx.Err() != nil
}

func (e *Engine) scanFlat(ctx, scanCtx context.Context, cfg Config) {
	holding, err := e.trader.Holding(scanCtx, cfg.Symbol)
	if err != nil {
		if !interrupted(ctx, scanCtx) {
			e.readFailed("không đọc được vị thế từ sàn: " + err.Error())
		}
		return
	}
	switch holding.Status {
	case HedgeUnhedged, HedgeEvidenceConflict:
		e.halt(fmt.Sprintf("sàn báo %s trên %s: %s — bot KHÔNG tự làm phẳng; xử lý bằng LÀM PHẲNG rồi TẮT bot để xác nhận", holding.Status, cfg.Symbol, holding.ReasonVI))
		return
	case HedgeUnknown:
		if !interrupted(ctx, scanCtx) {
			e.readFailed("không xác định được vị thế: " + holding.ReasonVI)
		}
		return
	case HedgeBothOpen:
		if holding.FromAutotrade && holding.HeldIntents == 1 && holding.IntentID != "" {
			e.adopt(ctx, holding)
			return
		}
	}

	now := e.now()
	snap, err := e.market.Snapshot(scanCtx, cfg.Symbol, now.Add(-trailingWindow).UnixMilli())
	if err != nil {
		if !interrupted(ctx, scanCtx) {
			e.readFailed("không đọc được thị trường: " + err.Error())
		}
		return
	}
	held := ""
	if holding.Status == HedgeBothOpen {
		held = fmt.Sprintf("%d ý định không phải của bot (%s)", holding.HeldIntents, holding.ReasonVI)
	}
	sig := assessEntry(entryInput{Cfg: cfg, Snap: snap, Now: now, MarginFrac: e.marginFrac,
		Flat: holding.Status == HedgeBothFlat, HeldVI: held})

	e.mu.Lock()
	if e.interruptedLocked(ctx) || e.state != StateEvaluating {
		// A stop, a kill or the portal's shutdown landed while the books were
		// read: the decision is dropped, not acted on.
		e.mu.Unlock()
		return
	}
	e.signal = &sig
	e.readFailures = 0
	if key := entryKey(sig); key != e.lastEntryKey {
		e.lastEntryKey = key
		e.addLogLocked("SCAN", scanLine(sig))
	}
	if !sig.EntryEligible || sig.EntryCostPct == nil {
		// An eligible signal always carries its priced entry cost; one without
		// it would reach execution's widening check as NaN, which compares
		// false and would let any book through.
		e.setStateLocked(StateIdleScanning, false)
		e.mu.Unlock()
		return
	}
	e.setStateLocked(StateOpening, false)
	e.addLogLocked("OPEN", fmt.Sprintf("đủ điều kiện — gửi lệnh mở %.2f quote %s qua portal", cfg.NotionalQuote, cfg.Symbol))
	e.mu.Unlock()

	// Once sent, an open is never cancelled — not by a stop, a kill or the
	// portal's shutdown. A cancelled PlaceOrder is not a venue refusal, so its
	// order may be on the wire and still invisible to the one lookup that
	// resolves it; letting the open RETURN, bounded by its own leg timeouts, is
	// what keeps "both open or both flat" a fact the kill can then act on.
	tradeCtx, cancelTrade := context.WithTimeout(context.WithoutCancel(ctx), e.actionTimeout)

	res := e.trader.Open(tradeCtx, OpenOrder{Symbol: cfg.Symbol, NotionalQuote: cfg.NotionalQuote, SignalEntryCostPct: *sig.EntryCostPct})
	cancelTrade()
	e.afterOpen(cfg, res)
}

// entryKey changes when the verdict or the set of failing checks does, so the
// console logs a scan when it says something new rather than every five seconds.
func entryKey(sig SignalView) string {
	var failed []string
	for _, c := range sig.EntryChecks {
		if !c.Passed {
			failed = append(failed, c.NameVI)
		}
	}
	return fmt.Sprintf("%v|%s", sig.EntryEligible, strings.Join(failed, "|"))
}

func scanLine(sig SignalView) string {
	parts := []string{sig.Symbol}
	if sig.ForecastRatePerIntervalBps != nil {
		parts = append(parts, fmt.Sprintf("funding %+.2f bps", *sig.ForecastRatePerIntervalBps))
	}
	if sig.NetAPRPct != nil {
		parts = append(parts, fmt.Sprintf("Net APR %.2f%%", *sig.NetAPRPct))
	}
	if sig.BasisBps != nil {
		parts = append(parts, fmt.Sprintf("basis %+.1f bps", *sig.BasisBps))
	}
	return strings.Join(parts, ", ") + " → " + sig.VerdictVI
}

func (e *Engine) afterOpen(cfg Config, res OpenResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case res.Busy:
		e.addLogLocked("OPEN", "portal đang bận thao tác ghi khác — chưa gửi gì, thử lại lượt sau: "+res.ErrorVI)
		e.setStateLocked(StateIdleScanning, false)
	case res.Alarm:
		e.haltLocked("MỞ BÁO ĐỘNG — bất biến có thể không giữ: " + res.ErrorVI + " (ý định " + res.IntentID + ")")
	case res.Hedged:
		basis, _ := basisBps(res.SpotRefMidQuote, res.PerpRefMidQuote)
		e.pos = &PositionView{IntentID: res.IntentID, OpenedAtMs: res.OpenedAtMs, QtyCoin: res.QtyCoin, EntryBasisBps: basis}
		e.historyBlindSince = time.Time{}
		e.tradeFailures = 0
		e.addLogLocked("OPEN", fmt.Sprintf("đã mở %s: %.8f coin mỗi chân, lệch %.8f, cửa sổ trần %d ms — HEDGED", res.IntentID, res.QtyCoin, res.ResidualQtyCoin, res.UnhedgedWindowMs))
		e.setStateLocked(StateInPosition, false)
	case res.Refused:
		e.tradeFailures++
		e.addLogLocked("OPEN", "từ chối trước khi gửi lệnh (không có gì trên sàn): "+res.ErrorVI)
		e.cooldownLocked(cfg)
	default:
		e.tradeFailures++
		e.addLogLocked("OPEN", fmt.Sprintf("không mở được — đã gỡ về PHẲNG (gỡ %d ms): %s", res.UnwindDurationMs, res.ErrorVI))
		e.cooldownLocked(cfg)
	}
	if e.tradeFailures >= cfg.MaxConsecutiveFailures && e.state != StateEmergencyHalted {
		e.haltLocked(fmt.Sprintf("%d lần mở/đóng hỏng liên tiếp — ngắt bảo vệ", e.tradeFailures))
	}
}

func (e *Engine) adopt(ctx context.Context, h Holding) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.interruptedLocked(ctx) || e.state != StateEvaluating {
		return
	}
	if h.OpenedAtMs <= 0 {
		// Without the open's instant every listed settlement would count as
		// "after the open" and a MaxHoldEpochs exit would fire at once.
		e.haltLocked(fmt.Sprintf("vị thế của bot %s đang mở nhưng không đọc được thời điểm mở từ file ý định — không tiếp nhận; đóng tay hoặc sửa cache rồi TẮT để xác nhận", h.IntentID))
		return
	}
	basis, ok := basisBps(h.SpotRefMidQuote, h.PerpRefMidQuote)
	pos := &PositionView{IntentID: h.IntentID, OpenedAtMs: h.OpenedAtMs, QtyCoin: h.QtyCoin, Adopted: true}
	if ok {
		pos.EntryBasisBps = basis
	}
	e.pos = pos
	e.readFailures = 0
	e.historyBlindSince = time.Time{}
	e.addLogLocked("ADOPT", fmt.Sprintf("tiếp nhận vị thế của bot %s đang mở trên sàn (%s)", h.IntentID, h.ReasonVI))
	e.setStateLocked(StateInPosition, false)
}

func (e *Engine) scanHolding(ctx, scanCtx context.Context, cfg Config, pos PositionView) {
	holding, err := e.trader.Holding(scanCtx, cfg.Symbol)
	if err != nil {
		if !interrupted(ctx, scanCtx) {
			e.readFailed("không đọc được vị thế từ sàn: " + err.Error())
		}
		return
	}
	switch holding.Status {
	case HedgeUnhedged, HedgeEvidenceConflict:
		e.halt(fmt.Sprintf("đang giữ %s thì sàn báo %s: %s — bot KHÔNG tự làm phẳng", pos.IntentID, holding.Status, holding.ReasonVI))
		return
	case HedgeUnknown:
		if !interrupted(ctx, scanCtx) {
			e.readFailed("không xác định được vị thế: " + holding.ReasonVI)
		}
		return
	case HedgeBothFlat:
		e.mu.Lock()
		if !e.killing && e.state == StateInPosition {
			e.pos = nil
			e.addLogLocked("CLOSE", "vị thế "+pos.IntentID+" đã PHẲNG trên sàn mà bot không đóng (đóng tay?) — hồi phục rồi quét lại")
			e.cooldownLocked(cfg)
		}
		e.mu.Unlock()
		return
	}
	if holding.HeldIntents != 1 || holding.IntentID != pos.IntentID {
		e.halt(fmt.Sprintf("bot giữ %s nhưng sàn cho thấy %d ý định đang giữ (%q) — không đóng thứ bot không nhận ra", pos.IntentID, holding.HeldIntents, holding.IntentID))
		return
	}

	now := e.now()
	since := min(now.Add(-trailingWindow).UnixMilli(), pos.OpenedAtMs)
	snap, err := e.market.Snapshot(scanCtx, cfg.Symbol, since)
	if err != nil {
		if !interrupted(ctx, scanCtx) {
			e.readFailed("không đọc được thị trường: " + err.Error())
		}
		return
	}
	sig := gauge(cfg, snap, now, e.marginFrac)
	ex := assessExit(cfg, snap, pos)
	sig.ExitChecks, sig.ExitDue = ex.Checks, ex.Due
	sig.EntryBasisBps, sig.BasisWidenBps = ptr(pos.EntryBasisBps), ex.BasisWidenBps
	if ex.Due {
		sig.VerdictVI = "ĐIỀU KIỆN THOÁT: " + strings.Join(ex.ReasonsVI, " · ")
	} else {
		sig.VerdictVI = "GIỮ VỊ THẾ"
	}

	e.mu.Lock()
	if e.interruptedLocked(ctx) || e.state != StateInPosition {
		e.mu.Unlock()
		return
	}
	e.signal = &sig
	e.readFailures = 0
	if snap.SettledErrVI == "" {
		e.historyBlindSince = time.Time{}
		if e.pos != nil {
			e.pos.SettlementsSinceOpen = ex.SettlementsSinceOpen
		}
	} else if !ex.Due {
		// While holding, an unreadable history blinds the funding and epoch
		// exits. Tolerated for maxHistoryBlind — settlements are hours apart —
		// then the bot halts rather than hold through what it cannot see.
		if e.historyBlindSince.IsZero() {
			e.historyBlindSince = now
			e.addLogLocked("ERROR", "lịch sử funding không đọc được khi đang giữ — lối thoát funding bị mù: "+snap.SettledErrVI)
		}
		if blind := now.Sub(e.historyBlindSince); blind >= maxHistoryBlind {
			e.haltLocked(fmt.Sprintf("lịch sử funding không đọc được suốt %s khi đang giữ %s — ngắt bảo vệ, vị thế GIỮ NGUYÊN: %s", blind.Round(time.Second), pos.IntentID, snap.SettledErrVI))
		}
		e.mu.Unlock()
		return
	}
	if !ex.Due {
		e.mu.Unlock()
		return
	}
	e.setStateLocked(StateClosing, false)
	e.addLogLocked("EXIT", sig.VerdictVI+" — gửi lệnh đóng "+pos.IntentID)
	e.mu.Unlock()

	// A close is never cancelled either: the kill wants it closed too, and a
	// cancelled close is a close that stopped half-way.
	tradeCtx, cancelTrade := context.WithTimeout(context.WithoutCancel(ctx), e.actionTimeout)
	res := e.trader.Close(tradeCtx, cfg.Symbol, pos.IntentID)
	cancelTrade()
	e.afterClose(cfg, pos, res)
}

func (e *Engine) afterClose(cfg Config, pos PositionView, res CloseResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case res.Busy:
		e.addLogLocked("CLOSE", "portal đang bận thao tác ghi khác — chưa gửi gì, đóng lại lượt sau")
		e.setStateLocked(StateInPosition, false)
	case res.Flat:
		e.pos = nil
		e.tradeFailures = 0
		e.addLogLocked("CLOSE", fmt.Sprintf("đã đóng %s PHẲNG cả hai chân: %.8f coin · funding sàn trả %+.8f qua %d mốc · RealizedQuote %+.8f (không phải lãi ròng)",
			pos.IntentID, res.ClosedQtyCoin, res.FundingReceivedQuote, res.SettlementsCounted, res.RealizedQuote))
		e.cooldownLocked(cfg)
	case res.Alarm:
		e.haltLocked("ĐÓNG BÁO ĐỘNG — hai chân có thể lệch: " + res.ErrorVI)
	case res.SentUnconfirmed:
		e.haltLocked("lệnh đóng ĐÃ GỬI nhưng không xác nhận được khớp: " + res.ErrorVI + " — đọc lại vị thế, không gửi lại")
	case res.Refused:
		e.tradeFailures++
		e.addLogLocked("CLOSE", "đóng bị từ chối trước khi gửi (hai chân nguyên vẹn): "+res.ErrorVI)
		e.setStateLocked(StateInPosition, false)
		if e.tradeFailures >= cfg.MaxConsecutiveFailures {
			e.haltLocked(fmt.Sprintf("%d lần đóng bị từ chối liên tiếp — ngắt bảo vệ, vị thế vẫn mở", e.tradeFailures))
		}
	default:
		e.haltLocked(fmt.Sprintf("đóng chưa phẳng: đã đóng %.8f, còn %.8f coin mỗi chân — %s", res.ClosedQtyCoin, res.RemainingQtyCoin, res.ErrorVI))
	}
}

func (e *Engine) readFailed(whyVI string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.killing {
		return
	}
	e.readFailures++
	e.addLogLocked("ERROR", fmt.Sprintf("lần đọc hỏng %d/%d: %s", e.readFailures, e.cfg.MaxConsecutiveFailures, whyVI))
	if e.readFailures >= e.cfg.MaxConsecutiveFailures {
		e.haltLocked(fmt.Sprintf("%d lần đọc sàn hỏng liên tiếp — ngắt bảo vệ: %s", e.readFailures, whyVI))
		return
	}
	switch e.state {
	case StateEvaluating:
		e.setStateLocked(StateIdleScanning, false)
	}
}

func (e *Engine) halt(whyVI string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.killing {
		return
	}
	e.haltLocked(whyVI)
}

func (e *Engine) haltLocked(whyVI string) {
	e.haltSeq++
	e.haltReasonVI = whyVI
	e.addLogLocked("HALT", whyVI)
	e.setStateLocked(StateEmergencyHalted, true)
}

func (e *Engine) cooldownLocked(cfg Config) {
	e.cooldownUntil = e.now().Add(cfg.Cooldown)
	e.setStateLocked(StateCooldown, false)
}

// setStateLocked moves the machine. A Step in flight while a kill runs changes
// nothing (force is the kill's own transition).
func (e *Engine) setStateLocked(to State, force bool) {
	if e.killing && !force {
		return
	}
	if e.state != to {
		e.state, e.stateSince = to, e.now()
	}
}

func (e *Engine) addLogLocked(kind, messageVI string) {
	e.log.add(LogEntry{AtMs: e.now().UnixMilli(), Kind: kind, MessageVI: messageVI})
	e.logf("execportal: autotrade [%s] %s", kind, messageVI)
}

// Start begins a run. Only a disabled bot starts: a halted one must be stopped
// first — the operator's acknowledgement that the reason was looked at.
func (e *Engine) Start(cfg Config) (StatusView, error) {
	if err := cfg.Validate(e.maxNotional); err != nil {
		return e.Status(), err
	}
	e.mu.Lock()
	switch {
	case e.busy != "" || e.killing || e.stopping:
		busy := e.busy
		e.mu.Unlock()
		return e.Status(), fmt.Errorf("%w (%s)", ErrBusy, busy)
	case e.state == StateEmergencyHalted:
		e.mu.Unlock()
		return e.Status(), fmt.Errorf("%w: bot đang DỪNG BẢO VỆ (%s) — xử lý nguyên nhân rồi bấm TẮT để xác nhận trước khi bật lại", ErrNotStartable, e.haltReasonVI)
	case e.state != StateDisabled:
		e.mu.Unlock()
		return e.Status(), fmt.Errorf("%w: bot đang chạy (%s)", ErrNotStartable, stateVI[e.state])
	}
	e.cfg = cfg
	e.readFailures, e.tradeFailures = 0, 0
	e.haltReasonVI, e.lastEntryKey = "", ""
	e.historyBlindSince = time.Time{}
	e.signal, e.pos = nil, nil
	hold := "giữ khi funding còn dương"
	if cfg.MaxHoldEpochs > 0 {
		hold = fmt.Sprintf("tối đa %d mốc settle", cfg.MaxHoldEpochs)
	}
	e.addLogLocked("START", fmt.Sprintf("BẬT trên %s: %.2f quote mỗi chân, Net APR ≥ %.2f%%, %s, quét mỗi %s", cfg.Symbol, cfg.NotionalQuote, cfg.MinNetAPRPct, hold, cfg.ScanInterval))
	e.setStateLocked(StateIdleScanning, true)
	e.mu.Unlock()
	e.poke()
	return e.Status(), nil
}

// OperatorClose is what a stop-and-close or a kill did to the position.
type OperatorClose struct {
	// Attempted is a close having been sent to the portal.
	Attempted bool        `json:"attempted"`
	Result    CloseResult `json:"result"`
	// Flat is the symbol ending flat — closed now, or nothing was held.
	Flat bool `json:"flat"`
	// NotTheBots is a position held on the symbol that the bot did not open:
	// left alone, and not a failure of the stop.
	NotTheBots bool   `json:"not_the_bots"`
	DetailVI   string `json:"detail_vi"`
}

// StopOutcome is what a stop did: the position it left open, or the close it
// ran.
type StopOutcome struct {
	// KeptIntentID is the bot's position left open on the venue by a stop
	// that keeps it, read AFTER the Step in flight returned — so an open that
	// was already on its way when DỪNG was pressed is named here too.
	KeptIntentID string         `json:"kept_intent_id"`
	Close        *OperatorClose `json:"close"`
}

// Stop ends the run. closeNow also closes the bot's pair; without it the
// position is kept as it is. From EMERGENCY_HALTED it is the acknowledgement
// that returns the bot to disabled. The status returned is read after the stop
// has fully finished.
func (e *Engine) Stop(ctx context.Context, closeNow bool) (StatusView, StopOutcome, error) {
	e.mu.Lock()
	if e.busy != "" || e.killing {
		busy := e.busy
		e.mu.Unlock()
		return e.Status(), StopOutcome{}, fmt.Errorf("%w (%s)", ErrBusy, busy)
	}
	e.stopSeq++
	mine := stopTicket{stopSeq: e.stopSeq, killSeq: e.killSeq, haltSeq: e.haltSeq}
	e.busy, e.stopping = "stop", true
	if e.cancelScan != nil {
		e.cancelScan() // a read is not worth waiting for; an open in flight is
	}
	e.mu.Unlock()

	out, err := e.stop(ctx, closeNow, mine)

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopSeq == mine.stopSeq {
		e.stopping = false
		if e.busy == "stop" {
			e.busy = ""
		}
	}
	return e.statusLocked(), out, err
}

// stopTicket is what a stop saw when it was pressed.
type stopTicket struct {
	stopSeq, killSeq, haltSeq int
}

// ErrHaltedWhileStopping is a stop refused because the bot halted while the stop
// waited for the scan or trade in flight.
var ErrHaltedWhileStopping = errors.New("autotrade: bot vừa DỪNG BẢO VỆ trong lúc lệnh dừng chờ — lệnh dừng KHÔNG xác nhận thứ bạn chưa đọc")

func (e *Engine) stop(ctx context.Context, closeNow bool, t stopTicket) (StopOutcome, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()

	e.mu.Lock()
	if e.killing || e.killSeq != t.killSeq {
		// A kill took over while this stop waited; the kill decides.
		e.mu.Unlock()
		return StopOutcome{}, fmt.Errorf("%w (kill đã chạy trong lúc lệnh dừng chờ — lệnh dừng bị bỏ)", ErrBusy)
	}
	if e.haltSeq != t.haltSeq {
		why := e.haltReasonVI
		e.mu.Unlock()
		return StopOutcome{}, fmt.Errorf("%w: %s", ErrHaltedWhileStopping, why)
	}
	symbol := e.cfg.Symbol
	if !closeNow {
		defer e.mu.Unlock()
		out := StopOutcome{}
		kept := "không giữ vị thế nào"
		if e.pos != nil {
			out.KeptIntentID = e.pos.IntentID
			kept = "vị thế " + e.pos.IntentID + " GIỮ NGUYÊN trên sàn"
		}
		if e.state == StateEmergencyHalted {
			e.addLogLocked("STOP", "người vận hành xác nhận DỪNG BẢO VỆ ("+e.haltReasonVI+") — về TẮT")
		} else {
			e.addLogLocked("STOP", "dừng tự động — "+kept)
		}
		e.pos, e.haltReasonVI = nil, ""
		e.setStateLocked(StateDisabled, true)
		return out, nil
	}
	e.mu.Unlock()

	oc := e.closeForOperator(ctx, symbol, "stop")
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.killing {
		return StopOutcome{Close: &oc}, nil
	}
	if oc.Flat || oc.NotTheBots {
		e.pos, e.haltReasonVI = nil, ""
		e.addLogLocked("STOP", "dừng tự động và đóng: "+oc.DetailVI)
		e.setStateLocked(StateDisabled, true)
	} else {
		e.haltLocked("DỪNG & ĐÓNG không về phẳng: " + oc.DetailVI)
	}
	return StopOutcome{Close: &oc}, nil
}

// Kill stops the bot at once and closes its pair, leaving the bot
// EMERGENCY_HALTED whatever the close did. A scan in flight is abandoned; an
// open or close already sent is waited for — never cancelled — so the close
// below acts on a pair that is both open or both flat, read from the venue.
func (e *Engine) Kill(ctx context.Context) (StatusView, *OperatorClose, error) {
	e.mu.Lock()
	if e.killing {
		e.mu.Unlock()
		return e.Status(), nil, fmt.Errorf("%w (kill)", ErrBusy)
	}
	e.killing = true
	e.killSeq++
	e.busy = "kill" // taken over from a stop, if one is waiting
	symbol := e.cfg.Symbol
	e.addLogLocked("KILL", "KILL SWITCH — dừng ngay, đóng hai chân của bot")
	if e.cancelScan != nil {
		e.cancelScan()
	}
	e.mu.Unlock()

	e.opMu.Lock()
	defer e.opMu.Unlock()
	oc := e.closeForOperator(ctx, symbol, "kill")

	e.mu.Lock()
	defer e.mu.Unlock()
	e.killing, e.busy = false, ""
	if oc.Flat {
		e.pos = nil
	}
	e.haltLocked("KILL SWITCH bởi người vận hành — " + oc.DetailVI)
	return e.statusLocked(), &oc, nil
}

// busyRetryEvery is how often an operator close retries a portal whose write
// lock is held by a manual action.
const busyRetryEvery = 500 * time.Millisecond

// closeForOperator closes the bot's pair on symbol for a stop or a kill,
// deciding from the venue: nothing held is already flat; ONE tracked intent
// both-open and minted by the bot is closed; anything else is refused, because
// squaring an unbalanced, unexplained or somebody else's position is a
// person's call.
func (e *Engine) closeForOperator(ctx context.Context, symbol, why string) OperatorClose {
	if symbol == "" {
		symbol = e.defaultSymbol
	}

	actionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.actionTimeout)
	defer cancel()
	holding, err := e.trader.Holding(actionCtx, symbol)
	if err != nil {
		return OperatorClose{DetailVI: "không đọc được vị thế " + symbol + " từ sàn — KHÔNG đóng, kiểm tay: " + err.Error()}
	}
	switch {
	case holding.Status == HedgeBothFlat:
		return OperatorClose{Flat: true, DetailVI: symbol + " không giữ gì trên sàn — không có gì để đóng"}
	case holding.Status != HedgeBothOpen:
		return OperatorClose{DetailVI: fmt.Sprintf("%s đang %s (%s) — bot không tự đóng hay làm phẳng; dùng LÀM PHẲNG", symbol, holding.Status, holding.ReasonVI)}
	case holding.HeldIntents != 1 || holding.IntentID == "":
		return OperatorClose{DetailVI: fmt.Sprintf("%s có %d ý định đang giữ — không biết đóng cái nào; đóng tay từng ý định", symbol, holding.HeldIntents)}
	case !holding.FromAutotrade:
		// The bot manages its own position only. A pair a person opened has
		// its own ĐÓNG VỊ THẾ button, and a kill switch that also closed it
		// would be the bot trading a position it cannot name as its own.
		return OperatorClose{NotTheBots: true, DetailVI: fmt.Sprintf("vị thế đang mở trên %s là %s, không phải của bot — KHÔNG đóng; đóng bằng ĐÓNG VỊ THẾ nếu cần", symbol, holding.IntentID)}
	}

	for {
		res := e.trader.Close(actionCtx, symbol, holding.IntentID)
		if !res.Busy {
			oc := OperatorClose{Attempted: true, Result: res, Flat: res.Flat}
			switch {
			case res.Flat:
				oc.DetailVI = fmt.Sprintf("đã đóng %s PHẲNG: %.8f coin · RealizedQuote %+.8f (không phải lãi ròng)", holding.IntentID, res.ClosedQtyCoin, res.RealizedQuote)
			case res.Refused:
				oc.DetailVI = "đóng " + holding.IntentID + " bị từ chối trước khi gửi — hai chân vẫn mở: " + res.ErrorVI
			default:
				oc.DetailVI = fmt.Sprintf("đóng %s chưa phẳng (gửi chưa xác nhận %v, báo động %v): %s", holding.IntentID, res.SentUnconfirmed, res.Alarm, res.ErrorVI)
			}
			e.mu.Lock()
			e.addLogLocked(strings.ToUpper(why), oc.DetailVI)
			e.mu.Unlock()
			return oc
		}
		select {
		case <-actionCtx.Done():
			return OperatorClose{Attempted: false, DetailVI: "portal bận thao tác ghi khác tới hết hạn — CHƯA gửi lệnh đóng " + holding.IntentID}
		case <-time.After(busyRetryEvery):
		}
	}
}

// Status is a copy of everything the page shows.
func (e *Engine) Status() StatusView {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.statusLocked()
}

func (e *Engine) statusLocked() StatusView {
	v := StatusView{
		State: e.state, StateVI: stateVI[e.state], StateSinceMs: e.stateSince.UnixMilli(),
		Enabled: e.state != StateDisabled && e.state != StateEmergencyHalted,
		Config:  e.cfg.view(), NowMs: e.now().UnixMilli(),
		ReadFailures: e.readFailures, TradeFailures: e.tradeFailures,
		HaltReasonVI: e.haltReasonVI, Busy: e.busy,
		Log: e.log.newestFirst(), NoticeVI: noticeVI,
	}
	if !e.lastScanAt.IsZero() {
		v.LastScanAtMs = e.lastScanAt.UnixMilli()
	}
	if !e.nextScanAt.IsZero() && v.Enabled {
		v.NextScanAtMs = e.nextScanAt.UnixMilli()
	}
	if e.state == StateCooldown {
		v.CooldownUntilMs = e.cooldownUntil.UnixMilli()
	}
	if e.signal != nil {
		s := *e.signal
		v.Signal = &s
	}
	if e.pos != nil {
		p := *e.pos
		v.Position = &p
	}
	return v
}
