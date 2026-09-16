package autotrade

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
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

	// ResidualQtyCoin is the spot leg plus the venue's signed perp position —
	// zero for a hedged or flat symbol — and ToleranceQtyCoin the coarser step
	// it is judged against.
	ResidualQtyCoin  float64
	ToleranceQtyCoin float64

	// HeldIntents is how many tracked intents still hold something by their
	// own orders. IntentID is that intent when there is exactly one.
	HeldIntents int
	IntentID    string
	// FromAutotrade is the intent id having been minted by this bot.
	FromAutotrade bool

	// From the intent's cache file, for adopting a pair after a restart: when it
	// opened, the notional it asked for, the two mids its entry was decided on
	// and the two legs' average entry fills.
	OpenedAtMs       int64
	NotionalQuote    float64
	SpotRefMidQuote  float64
	PerpRefMidQuote  float64
	SpotAvgFillQuote float64
	PerpAvgFillQuote float64
	QtyCoin          float64
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
	SpotAvgFillQuote float64
	PerpAvgFillQuote float64
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
// listed at or after settledSinceMs. It is called for several symbols at once.
type Market interface {
	Snapshot(ctx context.Context, symbol string, settledSinceMs int64) (Snapshot, error)
}

// Trader is the portal's order path. The engine holds no broker: everything it
// sends goes through here, under the portal's write lock, through the same
// execution machine a button press runs. Holding is called for several symbols
// at once; Open and Close never are.
type Trader interface {
	Holding(ctx context.Context, symbol string) (Holding, error)
	Open(ctx context.Context, order OpenOrder) OpenResult
	Close(ctx context.Context, symbol, intentID, reasonVI string) CloseResult
	// Account is the two wallets' quote equity, for the periodic rebalance
	// (capital.go). It is read at most once per RebalanceIntervalHours, never
	// on the scan's own cadence, and an error only leaves the size where it is.
	Account(ctx context.Context) (Account, error)
}

// Options builds an Engine.
type Options struct {
	Market Market
	Trader Trader

	// Symbols is the portal's allow-list, in the order the page lists it. A run
	// enters a subset; kill and stop-and-close read every one of them.
	Symbols []string
	// MaxNotionalQuote is the portal's ceiling per leg; a start above it is
	// refused.
	MaxNotionalQuote float64
	// PerpMarginFrac is the portal's collateral decision: the capital a pair
	// ties up is its notional × (1 + PerpMarginFrac).
	PerpMarginFrac float64
	// ActionTimeout bounds one open or close.
	ActionTimeout time.Duration

	Now  func() time.Time
	Logf func(format string, args ...any)
}

// pair is one symbol's machine. Every field is guarded by Engine.mu.
type pair struct {
	symbol string
	cfg    Config

	// inRun is the symbol being one the run may ENTER; paused stops entries on
	// it. A pair that may not enter still has its held position managed.
	inRun  bool
	paused bool

	state      State
	stateSince time.Time

	signal        *SignalView
	pos           *PositionView
	cooldownUntil time.Time
	readFailures  int
	tradeFailures int
	haltReasonVI  string
	// haltSeq is Engine.haltSeq at this pair's halt: the number an
	// acknowledgement must quote.
	haltSeq int

	lastEntryKey string
	lastSkipKey  string
	lastWatchKey string
	// historyBlindSince is when the funding history became unreadable while
	// holding; zero while it reads.
	historyBlindSince time.Time
	lastScanAt        time.Time

	rank   int
	skipVI string

	holding       Holding
	holdingReadAt time.Time
	// provenFlat is the pair's newest venue reading proving both legs flat,
	// with no order sent since. Anything else — no reading yet, a read that
	// failed or could not decide, legs the bot does not manage — takes a place
	// in the portfolio's counts while the bot runs: the limits count what the
	// VENUE may hold, not what the engine believes it holds.
	provenFlat bool
	// lastGoodFlat is the newest reading that DID decide having been flat,
	// with no order sent since: a run of failed reads on such a pair halts it
	// without taking a place.
	lastGoodFlat bool
	// haltMayHold is a halted pair possibly holding legs; it keeps its place
	// until acknowledged.
	haltMayHold bool
}

// entering reports whether the pair may open a position.
func (p *pair) entering() bool { return p.inRun && !p.paused }

// Engine is the auto-trader. One goroutine runs it (Run); Start, Stop, Kill,
// ClosePair, PairControl and Status are safe from any other.
type Engine struct {
	market        Market
	trader        Trader
	now           func() time.Time
	logf          func(format string, args ...any)
	symbols       []string
	maxNotional   float64
	marginFrac    float64
	actionTimeout time.Duration

	wake chan struct{}

	// opMu is held by whatever is touching the venue for a decision: one Step,
	// one Stop, one Kill, one ClosePair. Two of them never trade at the same
	// time, and a kill that arrives during an open waits for that open to
	// RETURN — execution's invariant is a promise about calls that return —
	// before it closes.
	opMu sync.Mutex

	// mu guards everything below and every pair. It is never held across a
	// venue call (CONVENTIONS §9).
	mu           sync.Mutex
	state        State
	stateSince   time.Time
	pcfg         PortfolioConfig
	pairs        map[string]*pair
	haltReasonVI string
	log          ringLog
	lastScanAt   time.Time
	nextScanAt   time.Time
	// busy names the operator action running now ("stop", "kill",
	// "close_pair"); busySeq names its owner, so only the action that set it
	// clears it. A kill takes it over from anything.
	busy    string
	busySeq int
	// killing makes a Step already in flight record what it did (a position it
	// just opened, above all) and change no state: the kill decides the state.
	killing bool
	// stopping is a stop waiting for the Step in flight: that Step sends no
	// order it had not already sent when the operator pressed DỪNG.
	stopping bool
	// stopSeq names the stop that owns stopping.
	stopSeq int
	// haltSeq counts halts, of any pair or of the bot. A stop may acknowledge
	// only the halts the operator saw when pressing it, and a pair's halt only
	// by the number the operator saw.
	haltSeq int
	// haltGlobalSeq is haltSeq at the bot's own halt.
	haltGlobalSeq int
	// killSeq counts kills. A stop or a close-pair remembers it when pressed; a
	// kill that ran while it waited supersedes it.
	killSeq    int
	cancelScan context.CancelFunc
	// lastOverKey de-duplicates the over-limit warning, and lastRebalanceKey
	// the rebalance's own, which is retried on every scan until it succeeds.
	lastOverKey      string
	lastRebalanceKey string
}

// New builds a disabled engine.
func New(o Options) (*Engine, error) {
	if o.Market == nil || o.Trader == nil {
		return nil, errors.New("autotrade: a Market and a Trader are required")
	}
	if len(o.Symbols) == 0 || !(o.MaxNotionalQuote > 0) || o.ActionTimeout <= 0 || o.PerpMarginFrac < 0 {
		return nil, fmt.Errorf("autotrade: incomplete options (symbols %v, max notional %v, action timeout %s, margin %v)",
			o.Symbols, o.MaxNotionalQuote, o.ActionTimeout, o.PerpMarginFrac)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	e := &Engine{
		market: o.Market, trader: o.Trader, now: o.Now, logf: o.Logf,
		maxNotional: o.MaxNotionalQuote, marginFrac: o.PerpMarginFrac,
		actionTimeout: o.ActionTimeout, wake: make(chan struct{}, 1),
		state: StateDisabled, pairs: map[string]*pair{},
	}
	e.stateSince = e.now()
	for _, s := range o.Symbols {
		if strings.TrimSpace(s) == "" || e.pairs[s] != nil {
			return nil, fmt.Errorf("autotrade: symbol %q is empty or listed twice", s)
		}
		e.symbols = append(e.symbols, s)
		e.pairs[s] = &pair{symbol: s, cfg: DefaultConfig(s), state: StateDisabled, stateSince: e.stateSince}
	}
	e.pcfg = DefaultPortfolioConfig(e.symbols)
	return e, nil
}

// maxHistoryBlind is how long the bot holds a pair while the settled-funding
// history cannot be read before it halts that pair. A time, not a count of
// scans: exits act on settlements hours apart, so a few minutes of a
// rate-limited or flaky history endpoint cost nothing, while a history that
// stays unreadable must not hold a pair through settlements nobody can see.
const maxHistoryBlind = 30 * time.Minute

// readTimeout bounds ONE pair's reads, counted from when the pair's turn comes
// — not from the start of the scan, so a long queue of pairs does not starve
// the last ones into failures that count toward a halt.
const readTimeout = 30 * time.Second

// idleDelay is how long Run sleeps while nothing is enabled; Start wakes it.
const idleDelay = time.Hour

// maxParallelReads is how many pairs are read at once. Each read is a handful
// of venue calls; reads never place anything, and the orders that follow them
// are sent one at a time.
const maxParallelReads = 4

// ErrBusy is an operator action refused because another one is running.
var ErrBusy = errors.New("autotrade: một thao tác khác của bot đang chạy")

// ErrNotStartable is an action refused in the current state.
var ErrNotStartable = errors.New("autotrade: không thực hiện được ở trạng thái hiện tại")

// ErrUnknownSymbol names a symbol the portal does not trade.
var ErrUnknownSymbol = errors.New("autotrade: symbol không nằm trong danh sách của portal")

// ErrStaleAcknowledgement is an acknowledgement quoting a halt that is not the
// pair's current one.
var ErrStaleAcknowledgement = errors.New("autotrade: DỪNG BẢO VỆ của cặp đã đổi kể từ lúc bạn đọc — lệnh xác nhận KHÔNG xác nhận thứ bạn chưa đọc")

// Run drives the machine until ctx ends. An open or close in flight when ctx
// ends is NOT cancelled — it runs to its own deadline, because a half-finished
// open is what the invariant forbids — and Run returns after it. Held positions
// are kept; the next Start adopts them.
func (e *Engine) Run(ctx context.Context) {
	for {
		e.Step(ctx)
		delay := e.scheduleNext()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			e.mu.Lock()
			if e.state == StateRunning {
				var kept []string
				for _, s := range e.symbols {
					p := e.pairs[s]
					if p.pos != nil {
						kept = append(kept, s+" "+p.pos.IntentID)
					}
					if p.state != StateEmergencyHalted {
						e.setPairStateLocked(p, StateDisabled, true)
					}
				}
				what := "không giữ vị thế nào"
				if len(kept) > 0 {
					what = "vị thế " + strings.Join(kept, ", ") + " GIỮ NGUYÊN trên sàn — lần BẬT sau sẽ tiếp nhận"
				}
				e.logLocked("STOP", "", "portal tắt — bot dừng, "+what)
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
	if e.state != StateRunning {
		e.nextScanAt = time.Time{}
		return idleDelay
	}
	delay := e.pcfg.ScanInterval
	e.nextScanAt = e.now().Add(delay)
	return delay
}

func (e *Engine) poke() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// scanJob is one pair's part of a scan, decided under the lock before any read.
type scanJob struct {
	symbol string
	cfg    Config
	// pos is the held position, copied: its exits are judged.
	pos *PositionView
	// entry is a flat pair that may open.
	entry bool
	// watch is a pair that may not open now: its holding is read — never its
	// market — for the counts, and for an adoption.
	watch bool
	// halted is a watch job planned for a halted pair: its reading counts, and
	// nothing else — not even once an acknowledgement lands during the read.
	halted bool
}

// reading is what the venue said to one job.
type reading struct {
	holding Holding
	holdErr error
	snap    Snapshot
	snapErr error
	// snapAt is when the snapshot was read, on the engine's own clock.
	snapAt time.Time
}

type exitPlan struct {
	symbol   string
	cfg      Config
	pos      PositionView
	reasonVI string
}

type entryPlan struct {
	symbol string
	cfg    Config
	sig    SignalView
	readAt time.Time
}

// Step runs one scan now: what Run does on its timer.
//
// It has three phases. Every pair's holding and market are READ, several at a
// time, with no lock held. Every reading is then JUDGED, one pair at a time,
// under the lock — halts, adoptions, the gauge. Finally the decisions are
// TRADED one at a time through the portal: every due exit first (an exit
// reduces risk and frees a place), then the eligible entries in order of Net
// APR, each re-checked against the portfolio's limits, the age of its reading
// and the next settlement at the moment it would be sent. A stop, a kill or the
// portal's shutdown between two trades drops every trade not yet sent.
func (e *Engine) Step(ctx context.Context) {
	e.opMu.Lock()
	defer e.opMu.Unlock()

	// The size is re-read from the account BEFORE the jobs are built, so a scan
	// that rebalances judges and opens at the new size rather than one scan
	// behind it. The venue read itself happens with no lock held (CONVENTIONS
	// §9), which is why this is two lock sections and not one.
	e.rebalance(ctx)

	e.mu.Lock()
	if e.killing || e.stopping || ctx.Err() != nil || e.state != StateRunning {
		e.mu.Unlock()
		return
	}
	now := e.now()
	var jobs []scanJob
	for _, s := range e.symbols {
		p := e.pairs[s]
		switch p.state {
		case StateOpening, StateClosing:
			continue
		case StateEmergencyHalted:
			// Read, never acted on: a halt waits for a person, but its place in
			// the counts follows the venue — a pair halted flat that a person
			// then opens takes a place, one read flat again gives it back, and a
			// halted pair still naming the bot's position is counted at what the
			// venue shows once that differs from the position.
			jobs = append(jobs, scanJob{symbol: s, cfg: p.cfg, watch: true, halted: true})
			continue
		case StateCooldown:
			if !now.Before(p.cooldownUntil) {
				e.restPairLocked(p)
			}
		}
		job := scanJob{symbol: s, cfg: p.cfg}
		switch {
		case p.pos != nil:
			held := *p.pos
			job.pos = &held
		case p.state != StateCooldown && p.entering():
			job.entry = true
			e.setPairStateLocked(p, StateEvaluating, false)
		default:
			if p.state == StateEvaluating {
				e.restPairLocked(p)
			}
			// A pair that may not enter now — outside the run, paused, cooling
			// down — still has its holding read on every scan: the limits count
			// what the venue holds as of this scan, and a person may open on
			// any symbol between two of them.
			job.watch = true
		}
		jobs = append(jobs, job)
	}
	e.lastScanAt = now
	if len(jobs) == 0 {
		e.mu.Unlock()
		return
	}
	scanCtx, cancel := context.WithCancel(ctx)
	e.cancelScan = cancel
	e.mu.Unlock()

	defer func() {
		cancel()
		e.mu.Lock()
		e.cancelScan = nil
		e.mu.Unlock()
	}()

	readings := e.readAll(scanCtx, jobs, now)

	var exits []exitPlan
	var entries []entryPlan
	for i, job := range jobs {
		switch {
		case job.pos != nil:
			if x, ok := e.judgeHolding(ctx, scanCtx, job, readings[i], now); ok {
				exits = append(exits, x)
			}
		default:
			if en, ok := e.judgeFlat(ctx, scanCtx, job, readings[i], now); ok {
				entries = append(entries, en)
			}
		}
	}

	e.warnOverLimit()

	for _, x := range exits {
		if !e.sendExit(ctx, x) {
			e.dropEntries(entries)
			return
		}
	}
	e.openRanked(ctx, entries)
}

// rebalance re-sizes the run's default notional from the account's own equity
// when one is due. It is the whole of the periodic rebalance, in three parts:
// decide under the lock, READ with no lock held, apply under the lock again.
//
// Nothing it does can reach a position already open. It writes exactly one
// field — PortfolioConfig.DefaultPairConfig.NotionalQuote — and each pair picks
// that up when its next job is built. A pair that is HOLDING keeps the size it
// opened at until it exits on its own terms.
func (e *Engine) rebalance(ctx context.Context) {
	e.mu.Lock()
	now := e.now()
	if e.killing || e.stopping || ctx.Err() != nil || e.state != StateRunning || !e.pcfg.rebalanceDue(now.UnixMilli()) {
		e.mu.Unlock()
		return
	}
	pcfg := e.pcfg
	// The bot's OWN spot legs, marked to each pair's newest mid. Spot equity
	// that is in coin rather than in quote; a position whose mid is unknown
	// contributes nothing, which sizes DOWN and never up.
	openSpot := 0.0
	for _, p := range e.pairs {
		if p.pos == nil || !(p.pos.QtyCoin > 0) || p.signal == nil || !(p.signal.SpotMidQuote > 0) {
			continue
		}
		openSpot += p.pos.QtyCoin * p.signal.SpotMidQuote
	}
	e.mu.Unlock()

	readCtx, cancel := context.WithTimeout(ctx, readTimeout)
	acct, err := e.trader.Account(readCtx)
	cancel()

	e.mu.Lock()
	defer e.mu.Unlock()
	// The run may have been stopped, killed or restarted with another
	// portfolio while the balances were read; a size from the old run's
	// parameters must not land on the new one.
	if e.interruptedLocked(ctx) || e.state != StateRunning || !e.pcfg.rebalanceDue(e.now().UnixMilli()) ||
		e.pcfg.MaxConcurrentPositions != pcfg.MaxConcurrentPositions || e.pcfg.TotalCapitalCapQuote != pcfg.TotalCapitalCapQuote {
		return
	}
	if err != nil {
		// The clock is NOT moved: a failed read leaves the rebalance owed, so
		// the next scan tries again rather than waiting another week. That also
		// means a venue that stays unreadable would log on every scan, so the
		// line is keyed by its reason and repeats only when the reason changes.
		e.logRebalanceOnceLocked("read|"+err.Error(), "không đọc được số dư hai ví — giữ nguyên quy mô "+
			fmt.Sprintf("%.2f quote mỗi chân: ", e.pcfg.DefaultPairConfig.NotionalQuote)+err.Error())
		return
	}
	plan := planNotional(notionalPlanInput{
		Account: acct, OpenSpotValueQuote: openSpot,
		Slots: e.pcfg.MaxConcurrentPositions, MarginFrac: e.marginFrac, BufferPct: e.pcfg.MarginBufferPct,
		CapQuote: e.pcfg.TotalCapitalCapQuote, MaxNotionalQuote: e.maxNotional,
	})
	if !plan.OK {
		e.logRebalanceOnceLocked("plan|"+plan.ReasonVI, plan.logLineVI(e.pcfg.MaxConcurrentPositions)+
			fmt.Sprintf(" Giữ nguyên %.2f quote mỗi chân.", e.pcfg.DefaultPairConfig.NotionalQuote))
		return
	}
	// A size the whole run would be refused for is not applied: Config.Validate
	// is what every start is held to, and a rebalance may not put the run in a
	// state a person could not have started it in.
	sized := e.pcfg.DefaultPairConfig
	sized.NotionalQuote = plan.NotionalQuote
	if sized.Symbol == "" && len(e.pcfg.Symbols) > 0 {
		sized.Symbol = e.pcfg.Symbols[0]
	}
	if err := sized.Validate(e.maxNotional); err != nil {
		e.logRebalanceOnceLocked("invalid|"+err.Error(), fmt.Sprintf("quy mô tính ra %.2f quote bị từ chối, giữ nguyên %.2f: %v",
			plan.NotionalQuote, e.pcfg.DefaultPairConfig.NotionalQuote, err))
		return
	}
	was := e.pcfg.DefaultPairConfig.NotionalQuote
	e.pcfg.DefaultPairConfig.NotionalQuote = plan.NotionalQuote
	e.pcfg.LastRebalancedAtMs = e.now().UnixMilli()
	// Every pair NOT carrying its own Config follows the new default from its
	// next job; one that does is left alone, and said so.
	var kept []string
	for _, sym := range e.pcfg.Symbols {
		p := e.pairs[sym]
		if p == nil {
			continue
		}
		if _, own := e.pcfg.PairOverrides[sym]; own {
			kept = append(kept, sym)
			continue
		}
		p.cfg.NotionalQuote = plan.NotionalQuote
	}
	e.lastRebalanceKey = ""
	line := fmt.Sprintf("%.2f → %.2f quote mỗi chân · ", was, plan.NotionalQuote) + plan.logLineVI(e.pcfg.MaxConcurrentPositions)
	if len(kept) > 0 {
		line += " Giữ cấu hình riêng (không đổi quy mô): " + strings.Join(kept, ", ") + "."
	}
	e.logLocked("REBALANCE", "", line)
}

// logRebalanceOnceLocked writes a REBALANCE line only when its reason differs
// from the last one. A rebalance that cannot be done stays OWED, so it is tried
// on every scan; without this, an unreadable wallet would fill the console.
func (e *Engine) logRebalanceOnceLocked(key, messageVI string) {
	if key == e.lastRebalanceKey {
		return
	}
	e.lastRebalanceKey = key
	e.logLocked("REBALANCE", "", messageVI)
}

// readAll reads every job, at most maxParallelReads at a time.
func (e *Engine) readAll(scanCtx context.Context, jobs []scanJob, now time.Time) []reading {
	out := make([]reading, len(jobs))
	sem := make(chan struct{}, maxParallelReads)
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			readCtx, cancel := context.WithTimeout(scanCtx, readTimeout)
			defer cancel()
			out[i] = e.read(readCtx, job, now)
		}()
	}
	wg.Wait()
	return out
}

// adoptable is a holding that is exactly one of the bot's own intents, hedged.
func adoptable(h Holding) bool {
	return h.Status == HedgeBothOpen && h.FromAutotrade && h.HeldIntents == 1 && h.IntentID != ""
}

// read asks the venue what one job needs, and nothing it does not: a holding
// that already decides the pair — a halt, an adoption, a flat pair that was
// held — reads no market.
func (e *Engine) read(scanCtx context.Context, job scanJob, now time.Time) reading {
	var r reading
	r.holding, r.holdErr = e.trader.Holding(scanCtx, job.symbol)
	if r.holdErr != nil || job.watch {
		return r
	}
	switch r.holding.Status {
	case HedgeUnhedged, HedgeEvidenceConflict, HedgeUnknown:
		return r
	case HedgeBothFlat:
		if job.pos != nil {
			return r
		}
	case HedgeBothOpen:
		if job.pos == nil && adoptable(r.holding) {
			return r
		}
		if job.pos != nil && (r.holding.HeldIntents != 1 || r.holding.IntentID != job.pos.IntentID) {
			return r
		}
	}
	since := now.Add(-trailingWindow).UnixMilli()
	if job.pos != nil {
		since = min(since, job.pos.OpenedAtMs)
	}
	r.snap, r.snapErr = e.market.Snapshot(scanCtx, job.symbol, since)
	r.snapAt = e.now()
	return r
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

// recordHoldingLocked keeps the venue's verdict for the pair's row and for the
// portfolio's counts.
func (e *Engine) recordHoldingLocked(p *pair, h Holding) {
	p.holding, p.holdingReadAt = h, e.now()
	p.provenFlat = h.Status == HedgeBothFlat
	switch h.Status {
	case HedgeBothFlat:
		p.lastGoodFlat = true
	case HedgeUnknown:
		// undecided: the last decided reading still stands
	default:
		p.lastGoodFlat = false
	}
}

// recordReadErrorLocked keeps a failed holding read as what it is: a reading
// that proves nothing.
func (e *Engine) recordReadErrorLocked(p *pair, err error) {
	e.recordHoldingLocked(p, Holding{Status: HedgeUnknown, ReasonVI: "không đọc được vị thế từ sàn: " + err.Error()})
}

// jobValidLocked reports whether the pair is still in the state the job was
// planned for. A reading taken before an acknowledgement, a pause or a close
// must not halt, count against or adopt into the pair as it is now.
func (e *Engine) jobValidLocked(p *pair, job scanJob) bool {
	if e.state != StateRunning || job.halted {
		// A halted pair's reading was planned to be counted, never acted on: an
		// acknowledgement during the read makes the pair look idle, but what it
		// read was read for a halt.
		return false
	}
	switch {
	case job.pos != nil:
		return p.state == StateInPosition && p.pos != nil && p.pos.IntentID == job.pos.IntentID
	case job.entry:
		return p.state == StateEvaluating
	default:
		return p.pos == nil && (p.state == StateDisabled || p.state == StateIdleScanning || p.state == StateCooldown)
	}
}

// judgeFlat judges a pair that holds nothing the bot manages. It returns an
// entry when every entry check passed.
func (e *Engine) judgeFlat(ctx, scanCtx context.Context, job scanJob, r reading, now time.Time) (entryPlan, bool) {
	p := e.pairs[job.symbol]
	if interrupted(ctx, scanCtx) {
		return entryPlan{}, false
	}
	e.mu.Lock()
	// The reading is venue evidence whatever happened to the pair while it was
	// taken, so it is kept for the counts even when the job no longer applies
	// (a pause, an acknowledgement); only the decisions below need the job.
	if r.holdErr != nil {
		e.recordReadErrorLocked(p, r.holdErr)
	} else {
		e.recordHoldingLocked(p, r.holding)
	}
	valid := e.jobValidLocked(p, job)
	e.mu.Unlock()
	if !valid {
		return entryPlan{}, false
	}

	h := r.holding
	if job.watch {
		// A pair that may not enter now: only one of the bot's own positions
		// matters here, to be adopted (never while cooling down — the next
		// entry scan does that). Anything else is logged once and never acted
		// on — it may be a person's position half-way through a manual open —
		// but it takes its place in the counts until a reading proves the pair
		// flat.
		if r.holdErr == nil && adoptable(h) && p.state != StateCooldown {
			e.adopt(ctx, p, h, job)
			return entryPlan{}, false
		}
		e.mu.Lock()
		if r.holdErr != nil || h.Status != HedgeBothFlat {
			why := h.ReasonVI
			if r.holdErr != nil {
				why = r.holdErr.Error()
			}
			// Keyed by the kind of state, not its words: a message that carries a
			// changing number (a weight tally) would log again on every scan.
			if key := orStatus(h.Status, r.holdErr); key != p.lastWatchKey {
				p.lastWatchKey = key
				e.logLocked("WARN", p.symbol, fmt.Sprintf("cặp không vào lệnh lúc này, sàn chưa chứng minh phẳng (%s: %s) — bot không quản lý, không làm phẳng; cặp giữ một chỗ tới khi đọc được phẳng", orStatus(h.Status, r.holdErr), why))
			}
		} else {
			p.lastWatchKey = ""
		}
		e.mu.Unlock()
		return entryPlan{}, false
	}
	switch {
	case r.holdErr != nil:
		e.pairReadFailed(p, job, "không đọc được vị thế từ sàn: "+r.holdErr.Error())
		return entryPlan{}, false
	case adoptable(h):
		e.adopt(ctx, p, h, job)
		return entryPlan{}, false
	}
	switch h.Status {
	case HedgeUnhedged, HedgeEvidenceConflict:
		e.pairHalt(p, job, fmt.Sprintf("sàn báo %s trên %s: %s — bot KHÔNG tự làm phẳng; xử lý bằng LÀM PHẲNG rồi XÁC NHẬN cặp", h.Status, p.symbol, h.ReasonVI))
		return entryPlan{}, false
	case HedgeUnknown:
		e.pairReadFailed(p, job, "không xác định được vị thế: "+h.ReasonVI)
		return entryPlan{}, false
	}
	if r.snapErr != nil {
		if !interrupted(ctx, scanCtx) {
			e.pairReadFailed(p, job, "không đọc được thị trường: "+r.snapErr.Error())
		}
		return entryPlan{}, false
	}
	held := ""
	if h.Status == HedgeBothOpen {
		held = fmt.Sprintf("%d ý định không phải của bot (%s)", h.HeldIntents, h.ReasonVI)
	}
	sig := assessEntry(entryInput{Cfg: job.cfg, Snap: r.snap, Now: now, MarginFrac: e.marginFrac,
		Flat: h.Status == HedgeBothFlat, HeldVI: held})

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.interruptedLocked(ctx) || !e.jobValidLocked(p, job) {
		// A stop, a kill, a pause or the portal's shutdown landed while the
		// books were read: the decision is dropped, not acted on.
		return entryPlan{}, false
	}
	p.signal = &sig
	p.readFailures = 0
	p.lastScanAt = now
	if key := entryKey(sig); key != p.lastEntryKey {
		p.lastEntryKey = key
		e.logLocked("SCAN", p.symbol, scanLine(sig))
	}
	if !sig.EntryEligible || sig.EntryCostPct == nil {
		// An eligible signal always carries its priced entry cost; one without
		// it would reach execution's widening check as NaN, which compares
		// false and would let any book through.
		p.rank, p.skipVI, p.lastSkipKey = 0, "", ""
		e.setPairStateLocked(p, StateIdleScanning, false)
		return entryPlan{}, false
	}
	return entryPlan{symbol: p.symbol, cfg: job.cfg, sig: sig, readAt: r.snapAt}, true
}

func orStatus(s HedgeStatus, err error) string {
	if err != nil {
		return "đọc hỏng"
	}
	return string(s)
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
	var parts []string
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

// judgeHolding judges a pair the bot holds. It returns an exit when one is due.
func (e *Engine) judgeHolding(ctx, scanCtx context.Context, job scanJob, r reading, now time.Time) (exitPlan, bool) {
	p := e.pairs[job.symbol]
	pos := *job.pos
	if interrupted(ctx, scanCtx) {
		return exitPlan{}, false
	}
	e.mu.Lock()
	if r.holdErr != nil {
		e.recordReadErrorLocked(p, r.holdErr)
	} else {
		e.recordHoldingLocked(p, r.holding)
	}
	valid := e.jobValidLocked(p, job)
	e.mu.Unlock()
	if !valid {
		return exitPlan{}, false
	}
	if r.holdErr != nil {
		e.pairReadFailed(p, job, "không đọc được vị thế từ sàn: "+r.holdErr.Error())
		return exitPlan{}, false
	}
	h := r.holding
	switch h.Status {
	case HedgeUnhedged, HedgeEvidenceConflict:
		e.pairHalt(p, job, fmt.Sprintf("đang giữ %s thì sàn báo %s: %s — bot KHÔNG tự làm phẳng", pos.IntentID, h.Status, h.ReasonVI))
		return exitPlan{}, false
	case HedgeUnknown:
		e.pairReadFailed(p, job, "không xác định được vị thế: "+h.ReasonVI)
		return exitPlan{}, false
	case HedgeBothFlat:
		e.mu.Lock()
		if !e.killing && p.state == StateInPosition {
			p.pos = nil
			e.logLocked("CLOSE", p.symbol, "vị thế "+pos.IntentID+" đã PHẲNG trên sàn mà bot không đóng (đóng tay?) — hồi phục rồi quét lại")
			e.cooldownPairLocked(p)
		}
		e.mu.Unlock()
		return exitPlan{}, false
	}
	if h.HeldIntents != 1 || h.IntentID != pos.IntentID {
		e.pairHalt(p, job, fmt.Sprintf("bot giữ %s nhưng sàn cho thấy %d ý định đang giữ (%q) — không đóng thứ bot không nhận ra", pos.IntentID, h.HeldIntents, h.IntentID))
		return exitPlan{}, false
	}
	if r.snapErr != nil {
		if !interrupted(ctx, scanCtx) {
			e.pairReadFailed(p, job, "không đọc được thị trường: "+r.snapErr.Error())
		}
		return exitPlan{}, false
	}
	// The gauge and the exits price THE POSITION, not the run's current slot
	// size: after a rebalance the two differ, and a held pair's round trip and
	// APR must describe the legs that are really on the venue.
	holdCfg := job.cfg
	if pos.NotionalQuote > 0 {
		holdCfg.NotionalQuote = pos.NotionalQuote
	}
	sig := gauge(holdCfg, r.snap, now, e.marginFrac)
	ex := assessExit(holdCfg, r.snap, pos, now)
	sig.ExitChecks, sig.ExitDue = ex.Checks, ex.Due
	sig.EntryBasisBps, sig.BasisWidenBps = ptr(pos.EntryBasisBps), ex.BasisWidenBps
	sig.fillHolding(holdCfg, ex.Result)
	if ex.Due {
		sig.VerdictVI = "ĐIỀU KIỆN THOÁT: " + strings.Join(ex.ReasonsVI, " · ")
	} else {
		sig.VerdictVI = "GIỮ VỊ THẾ"
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.interruptedLocked(ctx) || !e.jobValidLocked(p, job) {
		return exitPlan{}, false
	}
	p.signal = &sig
	p.readFailures = 0
	p.rank, p.skipVI = 0, ""
	p.lastScanAt = now
	if r.snap.SettledErrVI == "" {
		p.historyBlindSince = time.Time{}
		p.pos.SettlementsSinceOpen = ex.SettlementsSinceOpen
	} else if !ex.Due {
		// While holding, an unreadable history blinds the funding and epoch
		// exits. Tolerated for maxHistoryBlind — settlements are hours apart —
		// then the pair halts rather than hold through what it cannot see.
		if p.historyBlindSince.IsZero() {
			p.historyBlindSince = now
			e.logLocked("ERROR", p.symbol, "lịch sử funding không đọc được khi đang giữ — lối thoát funding bị mù: "+r.snap.SettledErrVI)
		}
		if blind := now.Sub(p.historyBlindSince); blind >= maxHistoryBlind {
			e.haltPairLocked(p, fmt.Sprintf("lịch sử funding không đọc được suốt %s khi đang giữ %s — ngắt bảo vệ, vị thế GIỮ NGUYÊN: %s", blind.Round(time.Second), pos.IntentID, r.snap.SettledErrVI))
		}
		return exitPlan{}, false
	}
	if !ex.Due {
		return exitPlan{}, false
	}
	reason := strings.Join(ex.ReasonsVI, "; ")
	if reason == "" && p.signal != nil {
		reason = p.signal.VerdictVI
	}
	if reason == "" {
		reason = "Điều kiện thoát đạt ngưỡng"
	}
	return exitPlan{symbol: p.symbol, cfg: job.cfg, pos: pos, reasonVI: reason}, true
}

// warnOverLimit logs, once per change, a portfolio already over its pair limit
// after every pair of the scan was judged — adopted positions, pairs not proven
// flat, halted pairs. Nothing new opens while it lasts; nothing is closed for it.
func (e *Engine) warnOverLimit() {
	e.mu.Lock()
	defer e.mu.Unlock()
	slots, _, _, _, _ := e.deployedLocked()
	key := ""
	if slots > e.pcfg.MaxConcurrentPositions {
		key = e.slotHoldersLocked()
	}
	if key != e.lastOverKey {
		e.lastOverKey = key
		if key != "" {
			e.logLocked("WARN", "", fmt.Sprintf("đang dùng %d chỗ, vượt tối đa %d — không mở cặp mới tới khi bớt: %s", slots, e.pcfg.MaxConcurrentPositions, key))
		}
	}
}

// sendExit sends one due exit. It returns false when nothing more may be sent
// this scan: it was interrupted, or the portal was busy with a manual write.
func (e *Engine) sendExit(ctx context.Context, x exitPlan) bool {
	p := e.pairs[x.symbol]
	e.mu.Lock()
	if e.interruptedLocked(ctx) {
		e.mu.Unlock()
		return false
	}
	if p.state != StateInPosition || p.pos == nil || p.pos.IntentID != x.pos.IntentID {
		e.mu.Unlock()
		return true
	}
	verdict := "ĐIỀU KIỆN THOÁT"
	if x.reasonVI != "" {
		verdict = x.reasonVI
	} else if p.signal != nil {
		verdict = p.signal.VerdictVI
	}
	e.setPairStateLocked(p, StateClosing, false)
	e.logLocked("EXIT", p.symbol, verdict+" — gửi lệnh đóng "+x.pos.IntentID)
	e.mu.Unlock()

	// A close is never cancelled: the kill wants it closed too, and a cancelled
	// close is a close that stopped half-way.
	tradeCtx, cancelTrade := context.WithTimeout(context.WithoutCancel(ctx), e.actionTimeout)
	res := e.trader.Close(tradeCtx, x.symbol, x.pos.IntentID, verdict)
	cancelTrade()
	e.afterClose(p, x.cfg, x.pos, res)
	// A busy portal means a manual write is running right now: every reading
	// of this scan may be out of date, so its remaining trades wait for the
	// next scan — the exit included.
	return !res.Busy
}

// dropEntries returns eligible pairs whose entry will not be sent to scanning.
func (e *Engine) dropEntries(entries []entryPlan) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, en := range entries {
		if p := e.pairs[en.symbol]; p.state == StateEvaluating {
			// never tried: no rank among what was sent, and no skip of its own
			p.rank, p.skipVI = 0, ""
			e.setPairStateLocked(p, StateIdleScanning, false)
		}
	}
}

// Skip categories, for the console's de-duplication.
const (
	skipCapacity = "capacity"
	skipUnsized  = "unsized"
	skipCapital  = "capital"
	skipStale    = "stale"
	skipSettle   = "settle"
)

// openRanked sends the eligible entries in order of Net APR, highest first,
// while the portfolio's limits allow.
func (e *Engine) openRanked(ctx context.Context, entries []entryPlan) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := deref(entries[i].sig.NetAPRPct), deref(entries[j].sig.NetAPRPct)
		if a != b {
			return a > b
		}
		return entries[i].symbol < entries[j].symbol
	})
	e.mu.Lock()
	for i, en := range entries {
		e.pairs[en.symbol].rank = i + 1
	}
	e.mu.Unlock()

	for i, en := range entries {
		p := e.pairs[en.symbol]
		e.mu.Lock()
		if e.interruptedLocked(ctx) {
			e.mu.Unlock()
			e.dropEntries(entries[i:])
			return
		}
		if p.state != StateEvaluating || p.pos != nil || !p.entering() {
			if p.state == StateEvaluating {
				e.setPairStateLocked(p, StateIdleScanning, false)
			}
			e.mu.Unlock()
			continue
		}
		cpn := e.capitalPerNotional()
		slots, _, _, _, committed := e.deployedLocked()
		need := en.cfg.NotionalQuote * cpn
		now := e.now()
		kind, skip := "", ""
		unsized := e.unsizedLocked()
		switch {
		case slots >= e.pcfg.MaxConcurrentPositions:
			kind, skip = skipCapacity, fmt.Sprintf("đã dùng %d/%d chỗ (số cặp tối đa đồng thời): %s", slots, e.pcfg.MaxConcurrentPositions, e.slotHoldersLocked())
		case unsized != "":
			kind, skip = skipUnsized, fmt.Sprintf("không biết vốn của cặp đang chiếm chỗ: %s — không mở thêm tới khi sàn nêu được cỡ hoặc chứng minh phẳng", unsized)
		case committed+need > e.pcfg.TotalCapitalCapQuote+1e-9:
			kind, skip = skipCapital, fmt.Sprintf("vốn đã cam kết %.2f + %.2f của cặp này vượt hạn mức %.2f quote", committed, need, e.pcfg.TotalCapitalCapQuote)
		case now.Sub(en.readAt) > maxBookAge:
			kind, skip = skipStale, fmt.Sprintf("tín hiệu đọc từ %s trước — quá %s, không gửi lệnh trên sổ cũ; quét lại lượt sau", now.Sub(en.readAt).Round(time.Second), maxBookAge)
		case en.sig.NextFundingTimeMs > 0 && time.UnixMilli(en.sig.NextFundingTimeMs).Sub(now) <= en.cfg.MinTimeToSettle:
			kind, skip = skipSettle, fmt.Sprintf("tới lúc gửi chỉ còn %s tới mốc settle, cần > %s", time.UnixMilli(en.sig.NextFundingTimeMs).Sub(now).Round(time.Second), en.cfg.MinTimeToSettle)
		}
		if skip != "" {
			p.skipVI = skip
			if p.lastSkipKey != kind {
				p.lastSkipKey = kind
				e.logLocked("SKIP", p.symbol, fmt.Sprintf("đủ điều kiện (hạng %d, Net APR %.2f%%) nhưng BỎ QUA: %s", p.rank, deref(en.sig.NetAPRPct), skip))
			}
			e.setPairStateLocked(p, StateIdleScanning, false)
			e.mu.Unlock()
			continue
		}
		e.setPairStateLocked(p, StateOpening, false)
		p.provenFlat, p.lastGoodFlat = false, false
		e.logLocked("OPEN", p.symbol, fmt.Sprintf("đủ điều kiện (hạng %d, Net APR %.2f%%) — gửi lệnh mở %.2f quote qua portal", p.rank, deref(en.sig.NetAPRPct), en.cfg.NotionalQuote))
		e.mu.Unlock()

		// Once sent, an open is never cancelled — not by a stop, a kill or the
		// portal's shutdown. A cancelled PlaceOrder is not a venue refusal, so
		// its order may be on the wire and still invisible to the one lookup
		// that resolves it; letting the open RETURN, bounded by its own leg
		// timeouts, is what keeps "both open or both flat" a fact the kill can
		// then act on.
		tradeCtx, cancelTrade := context.WithTimeout(context.WithoutCancel(ctx), e.actionTimeout)
		res := e.trader.Open(tradeCtx, OpenOrder{Symbol: en.symbol, NotionalQuote: en.cfg.NotionalQuote, SignalEntryCostPct: *en.sig.EntryCostPct})
		cancelTrade()
		e.afterOpen(p, en.cfg, res)
		if res.Busy {
			// A manual write is running right now, on some symbol: every
			// reading of this scan may already be out of date.
			e.dropEntries(entries[i+1:])
			return
		}
	}
}

func (e *Engine) capitalPerNotional() float64 { return 1 + e.marginFrac }

// deployedLocked counts the portfolio: slots used, the pairs held, their
// notional and capital, and the capital committed.
func (e *Engine) deployedLocked() (slots, open int, notional, capital, committed float64) {
	for _, s := range e.symbols {
		p := e.pairs[s]
		pl := e.placeLocked(p)
		if !pl.uses {
			continue
		}
		slots++
		committed += pl.capitalQuote
		if p.pos != nil {
			open++
			notional += p.pos.NotionalQuote
			capital += p.pos.CapitalQuote
		}
	}
	return slots, open, notional, capital, committed
}

// slotHoldersLocked names the pairs taking a place and why, for a skip message
// that says what stands in the way.
func (e *Engine) slotHoldersLocked() string {
	var out []string
	for _, s := range e.symbols {
		if pl := e.placeLocked(e.pairs[s]); pl.uses {
			out = append(out, s+" "+pl.whyVI)
		}
	}
	return strings.Join(out, "; ")
}

// unsizedLocked names the pairs taking a place whose capital nothing states,
// each with why. Counting one at the configured notional would be a guess, and
// a kept position of 500 counted at 65 lets the cap be crossed: entries wait
// instead.
func (e *Engine) unsizedLocked() string {
	var out []string
	for _, s := range e.symbols {
		p := e.pairs[s]
		pl := e.placeLocked(p)
		if !pl.uses || pl.sized {
			continue
		}
		if p.holdingReadAt.IsZero() {
			out = append(out, s+" (chưa đọc được vị thế)")
			continue
		}
		out = append(out, fmt.Sprintf("%s (%s: %s)", s, p.holding.Status, p.holding.ReasonVI))
	}
	return strings.Join(out, ", ")
}

// place is one pair's share of the portfolio's limits.
type place struct {
	uses bool
	// capitalQuote is what the pair is counted at against the capital cap.
	capitalQuote float64
	// sized is whether anything states that capital. When nothing does, the
	// configured size stands in for the count and every entry waits.
	sized bool
	whyVI string
}

// placeLocked says whether a pair takes a place in the portfolio, the capital
// it is counted at, and why. A place is taken by a held pair; by a pair trading
// now; by a halted pair that may hold legs; and — while the bot runs — by any
// pair whose newest venue reading does not prove it flat. The limits bound what
// the venue may hold, so what could not be read counts as held — and at a size
// only a reading can state.
func (e *Engine) placeLocked(p *pair) place {
	configured := p.cfg.NotionalQuote * e.capitalPerNotional()
	halted := ""
	if p.state == StateEmergencyHalted {
		halted = "DỪNG BẢO VỆ, "
	}
	switch {
	case p.pos != nil && e.readingContradictsLocked(p):
		// The newest reading does not show the one position the bot names — a
		// person closed it and opened another, added an intent beside it, or
		// the venue could not be read. The position is a belief; only a
		// reading states what is held.
		capital, sized := e.sizeLocked(p, p.pos.CapitalQuote)
		return place{uses: true, capitalQuote: capital, sized: sized,
			whyVI: fmt.Sprintf("%sbot ghi đang giữ %s nhưng lần đọc sàn không cho thấy đúng vị thế đó (%s: %s)", halted, p.pos.IntentID, p.holding.Status, p.holding.ReasonVI)}
	case p.pos != nil:
		return place{uses: true, capitalQuote: p.pos.CapitalQuote, sized: true, whyVI: halted + "đang giữ " + p.pos.IntentID}
	case p.state == StateOpening || p.state == StateClosing:
		return place{uses: true, capitalQuote: configured, sized: true, whyVI: "đang có lệnh trên đường tới sàn"}
	case p.state == StateEmergencyHalted:
		if !p.haltMayHold && p.provenFlat {
			return place{}
		}
		capital, sized := e.sizeLocked(p, configured)
		why := "DỪNG BẢO VỆ và có thể còn giữ hai chân — giữ một chỗ tới khi được xác nhận"
		if !p.haltMayHold {
			why = fmt.Sprintf("DỪNG BẢO VỆ, sàn chưa chứng minh phẳng (%s: %s)", p.holding.Status, p.holding.ReasonVI)
		}
		return place{uses: true, capitalQuote: capital, sized: sized, whyVI: why}
	case e.state == StateRunning && !p.provenFlat:
		capital, sized := e.sizeLocked(p, configured)
		why := fmt.Sprintf("sàn chưa chứng minh cặp phẳng (%s: %s)", p.holding.Status, p.holding.ReasonVI)
		switch {
		case p.holdingReadAt.IsZero():
			why = "chưa đọc được vị thế trên sàn"
		case p.holding.Status == HedgeBothFlat:
			// Flat at its last reading, and an order was decided since that
			// the venue never received (the portal was busy).
			why = "đã quyết một lệnh sau lần đọc phẳng gần nhất — chờ lần đọc sau"
		}
		return place{uses: true, capitalQuote: capital, sized: sized, whyVI: why}
	}
	return place{}
}

// readingContradictsLocked reports a held pair whose newest reading does not
// show exactly its one position: legs under another intent, several or none,
// a conflict, or a reading that decided nothing.
func (e *Engine) readingContradictsLocked(p *pair) bool {
	if p.holdingReadAt.IsZero() {
		return false
	}
	switch p.holding.Status {
	case HedgeBothFlat:
		// less than the position, never more
		return false
	case HedgeBothOpen, HedgeUnhedged:
		return p.holding.HeldIntents != 1 || p.holding.IntentID != p.pos.IntentID
	}
	return true
}

// sizeLocked is the capital a pair's newest reading states it holds, and
// whether it states one at all. Only two readings do: legs held under exactly
// one intent whose notional is known — counted at that notional, whoever's
// it is — and both legs flat. A pair still taking a place after a flat reading
// has sent an order since (an open that alarmed, one found busy), and every
// order the bot sends is the configured notional: standIn is the ceiling of
// what it can have put on the venue since that reading, NOT a formality — an
// open that alarms is held to it for the rest of its scan. Everything else states nothing and
// is counted at standIn, unsized, which stops every entry: no reading, a
// failed or undecided one, a conflict (perp no intent explains), legs under
// several intents or one with no notional. Neither a halt nor the bot's own
// record is a size: a pair halted beside a person's 500-notional position is
// counted at 500 once read, never at the 65 the bot would open or once did.
func (e *Engine) sizeLocked(p *pair, standIn float64) (float64, bool) {
	if p.holdingReadAt.IsZero() {
		return standIn, false
	}
	switch h := p.holding; h.Status {
	case HedgeBothFlat:
		return standIn, true
	case HedgeBothOpen, HedgeUnhedged:
		if h.HeldIntents == 1 && h.NotionalQuote > 0 {
			return h.NotionalQuote * e.capitalPerNotional(), true
		}
	}
	return standIn, false
}

func (e *Engine) afterOpen(p *pair, cfg Config, res OpenResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case res.Busy:
		e.logLocked("OPEN", p.symbol, "portal đang bận thao tác ghi khác — chưa gửi gì, thử lại lượt sau: "+res.ErrorVI)
		e.setPairStateLocked(p, StateIdleScanning, false)
	case res.Alarm:
		e.haltPairLocked(p, "MỞ BÁO ĐỘNG — bất biến có thể không giữ: "+res.ErrorVI+" (ý định "+res.IntentID+")")
	case res.Hedged:
		basis, _ := basisBps(res.SpotRefMidQuote, res.PerpRefMidQuote)
		p.pos = &PositionView{Symbol: p.symbol, IntentID: res.IntentID, OpenedAtMs: res.OpenedAtMs, QtyCoin: res.QtyCoin,
			NotionalQuote: cfg.NotionalQuote, CapitalQuote: cfg.NotionalQuote * e.capitalPerNotional(), EntryBasisBps: basis,
			SpotEntryAvgQuote: res.SpotAvgFillQuote, PerpEntryAvgQuote: res.PerpAvgFillQuote}
		p.historyBlindSince = time.Time{}
		p.tradeFailures = 0
		p.lastSkipKey = ""
		// execution read both legs back from the venue before it called the
		// pair hedged: that is the pair's newest verdict until the next scan.
		e.recordHoldingLocked(p, Holding{Status: HedgeBothOpen, ReasonVI: "theo lệnh mở vừa xong (execution đọc lại hai chân từ sàn)",
			ResidualQtyCoin: res.ResidualQtyCoin, ToleranceQtyCoin: p.holding.ToleranceQtyCoin, HeldIntents: 1, IntentID: res.IntentID, FromAutotrade: true})
		e.logLocked("OPEN", p.symbol, fmt.Sprintf("đã mở %s: %.8f coin mỗi chân, lệch %.8f, cửa sổ trần %d ms — HEDGED", res.IntentID, res.QtyCoin, res.ResidualQtyCoin, res.UnhedgedWindowMs))
		e.setPairStateLocked(p, StateInPosition, false)
	case res.Refused:
		p.tradeFailures++
		// Nothing reached the venue — but the portal refuses an open exactly
		// when it finds the symbol NOT flat (a person's open between the scan's
		// read and this send, an order still working), so the reading the entry
		// was decided on proves nothing any more. The next scan reads it again.
		e.logLocked("OPEN", p.symbol, "từ chối trước khi gửi lệnh (không có gì trên sàn): "+res.ErrorVI)
		// Until the pair is read again its size is unknown: counted at the
		// configured notional it would let the scan's next entry past a
		// position the portal has just seen. Not a reading, so it keeps the
		// last reading's time.
		p.holding = Holding{Status: HedgeUnknown, ReasonVI: "portal từ chối lệnh mở — chờ lần đọc sàn sau: " + res.ErrorVI,
			ToleranceQtyCoin: p.holding.ToleranceQtyCoin}
		p.provenFlat = false
		e.cooldownPairLocked(p)
	default:
		p.tradeFailures++
		// execution proved both legs flat after the unwind, or it would have
		// raised an alarm.
		e.recordHoldingLocked(p, Holding{Status: HedgeBothFlat, ReasonVI: "theo lần mở vừa gỡ về phẳng (execution chứng minh từ sàn)", ToleranceQtyCoin: p.holding.ToleranceQtyCoin})
		e.logLocked("OPEN", p.symbol, fmt.Sprintf("không mở được — đã gỡ về PHẲNG (gỡ %d ms): %s", res.UnwindDurationMs, res.ErrorVI))
		e.cooldownPairLocked(p)
	}
	if p.tradeFailures >= cfg.MaxConsecutiveFailures && p.state != StateEmergencyHalted {
		e.haltPairLocked(p, fmt.Sprintf("%d lần mở/đóng hỏng liên tiếp — ngắt bảo vệ cặp", p.tradeFailures))
	}
}

// adopt takes over one of the bot's own positions found open on the venue. A
// pair that may not enter adopts too: its position is managed to its exit and
// never re-entered.
func (e *Engine) adopt(ctx context.Context, p *pair, h Holding, job scanJob) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.interruptedLocked(ctx) || !e.jobValidLocked(p, job) || p.pos != nil {
		return
	}
	if h.OpenedAtMs <= 0 || !(h.NotionalQuote > 0) {
		// Without the open's instant every listed settlement would count as
		// "after the open" and a MaxHoldEpochs exit would fire at once; without
		// its notional the capital limit would count a guess.
		e.haltPairLocked(p, fmt.Sprintf("vị thế của bot %s đang mở nhưng không đọc được thời điểm mở hoặc notional từ file ý định — không tiếp nhận; đóng tay hoặc sửa cache rồi XÁC NHẬN cặp", h.IntentID))
		return
	}
	notional := h.NotionalQuote
	pos := &PositionView{Symbol: p.symbol, IntentID: h.IntentID, OpenedAtMs: h.OpenedAtMs, QtyCoin: h.QtyCoin, Adopted: true,
		NotionalQuote: notional, CapitalQuote: notional * e.capitalPerNotional(),
		SpotEntryAvgQuote: h.SpotAvgFillQuote, PerpEntryAvgQuote: h.PerpAvgFillQuote}
	if basis, ok := basisBps(h.SpotRefMidQuote, h.PerpRefMidQuote); ok {
		pos.EntryBasisBps = basis
	}
	p.pos = pos
	p.readFailures = 0
	p.historyBlindSince = time.Time{}
	note := ""
	if !p.entering() {
		note = " — cặp không được vào lệnh mới: chỉ quản lý tới lúc thoát"
	}
	e.logLocked("ADOPT", p.symbol, fmt.Sprintf("tiếp nhận vị thế của bot %s đang mở trên sàn (%s)%s", h.IntentID, h.ReasonVI, note))
	e.setPairStateLocked(p, StateInPosition, false)
}

func (e *Engine) afterClose(p *pair, cfg Config, pos PositionView, res CloseResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case res.Busy:
		e.logLocked("CLOSE", p.symbol, "portal đang bận thao tác ghi khác — chưa gửi gì, đóng lại lượt sau")
		e.setPairStateLocked(p, StateInPosition, false)
	case res.Flat:
		p.pos = nil
		p.tradeFailures = 0
		e.recordHoldingLocked(p, Holding{Status: HedgeBothFlat, ReasonVI: "theo lệnh đóng vừa xong (execution chứng minh hai chân phẳng từ sàn)",
			ToleranceQtyCoin: p.holding.ToleranceQtyCoin})
		e.logLocked("CLOSE", p.symbol, fmt.Sprintf("đã đóng %s PHẲNG cả hai chân: %.8f coin · funding sàn trả %+.8f qua %d mốc · RealizedQuote %+.8f (không phải lãi ròng)",
			pos.IntentID, res.ClosedQtyCoin, res.FundingReceivedQuote, res.SettlementsCounted, res.RealizedQuote))
		e.cooldownPairLocked(p)
	case res.Alarm:
		e.haltPairLocked(p, "ĐÓNG BÁO ĐỘNG — hai chân có thể lệch: "+res.ErrorVI)
	case res.SentUnconfirmed:
		e.haltPairLocked(p, "lệnh đóng ĐÃ GỬI nhưng không xác nhận được khớp: "+res.ErrorVI+" — đọc lại vị thế, không gửi lại")
	case res.Refused:
		p.tradeFailures++
		e.logLocked("CLOSE", p.symbol, "đóng bị từ chối trước khi gửi (hai chân nguyên vẹn): "+res.ErrorVI)
		e.setPairStateLocked(p, StateInPosition, false)
		if p.tradeFailures >= cfg.MaxConsecutiveFailures {
			e.haltPairLocked(p, fmt.Sprintf("%d lần đóng bị từ chối liên tiếp — ngắt bảo vệ cặp, vị thế vẫn mở", p.tradeFailures))
		}
	default:
		e.haltPairLocked(p, fmt.Sprintf("đóng chưa phẳng: đã đóng %.8f, còn %.8f coin mỗi chân — %s", res.ClosedQtyCoin, res.RemainingQtyCoin, res.ErrorVI))
	}
}

func (e *Engine) pairReadFailed(p *pair, job scanJob, whyVI string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.killing || !e.jobValidLocked(p, job) {
		return
	}
	p.readFailures++
	e.logLocked("ERROR", p.symbol, fmt.Sprintf("lần đọc hỏng %d/%d: %s", p.readFailures, p.cfg.MaxConsecutiveFailures, whyVI))
	if p.readFailures >= p.cfg.MaxConsecutiveFailures {
		// A pair whose last decided reading was flat, with nothing sent since,
		// cannot have come to hold legs of the bot's: it halts without the
		// may-hold mark, so the first reading that proves it flat gives its
		// place back with no acknowledgement. Until then it is an unread pair
		// like any other — a place of unknown size, which holds every entry.
		mayHold := p.pos != nil || !p.lastGoodFlat
		e.haltPairMayHoldLocked(p, fmt.Sprintf("%d lần đọc sàn hỏng liên tiếp — ngắt bảo vệ cặp: %s", p.readFailures, whyVI), mayHold)
		return
	}
	if p.state == StateEvaluating {
		e.setPairStateLocked(p, StateIdleScanning, false)
	}
}

func (e *Engine) pairHalt(p *pair, job scanJob, whyVI string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.killing || !e.jobValidLocked(p, job) {
		return
	}
	e.haltPairLocked(p, whyVI)
}

// haltPairLocked halts ONE pair, which may hold legs. Its position, if any,
// stays named and keeps its place in the portfolio's counts; every other pair
// runs on.
func (e *Engine) haltPairLocked(p *pair, whyVI string) {
	e.haltPairMayHoldLocked(p, whyVI, true)
}

func (e *Engine) haltPairMayHoldLocked(p *pair, whyVI string, mayHold bool) {
	e.haltSeq++
	p.haltSeq = e.haltSeq
	p.haltMayHold = mayHold
	p.haltReasonVI = whyVI
	p.skipVI = ""
	e.logLocked("HALT", p.symbol, whyVI)
	e.setPairStateLocked(p, StateEmergencyHalted, true)
}

// haltLocked halts the whole bot.
func (e *Engine) haltLocked(whyVI string) {
	e.haltSeq++
	e.haltGlobalSeq = e.haltSeq
	e.haltReasonVI = whyVI
	e.logLocked("HALT", "", whyVI)
	e.setStateLocked(StateEmergencyHalted, true)
}

// restPairLocked is where a pair holding nothing goes: scanning when the run may
// scan it, disabled otherwise.
func (e *Engine) restPairLocked(p *pair) {
	to := StateIdleScanning
	if e.state != StateRunning || !p.inRun {
		to = StateDisabled
	}
	e.setPairStateLocked(p, to, false)
}

func (e *Engine) cooldownPairLocked(p *pair) {
	if !p.entering() {
		e.restPairLocked(p)
		return
	}
	p.cooldownUntil = e.now().Add(p.cfg.Cooldown)
	e.setPairStateLocked(p, StateCooldown, false)
}

// setPairStateLocked moves one pair. A Step in flight while a kill runs changes
// nothing (force is the kill's, and a halt's, own transition).
func (e *Engine) setPairStateLocked(p *pair, to State, force bool) {
	if e.killing && !force {
		return
	}
	if to != StateEvaluating && to != StateIdleScanning {
		p.skipVI = ""
	}
	if p.state != to {
		p.state, p.stateSince = to, e.now()
	}
}

// setStateLocked moves the portfolio.
func (e *Engine) setStateLocked(to State, force bool) {
	if e.killing && !force {
		return
	}
	if e.state != to {
		e.state, e.stateSince = to, e.now()
	}
}

func (e *Engine) logLocked(kind, symbol, messageVI string) {
	e.log.add(LogEntry{AtMs: e.now().UnixMilli(), Kind: kind, Symbol: symbol, MessageVI: messageVI})
	if symbol != "" {
		e.logf("execportal: autotrade [%s %s] %s", kind, symbol, messageVI)
		return
	}
	e.logf("execportal: autotrade [%s] %s", kind, messageVI)
}

// Start begins a run. Only a disabled bot starts: a halted one must be stopped
// first — the operator's acknowledgement that the reason was looked at.
func (e *Engine) Start(pc PortfolioConfig) (StatusView, error) {
	if err := pc.Validate(e.symbols, e.maxNotional, e.capitalPerNotional()); err != nil {
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
	for _, s := range e.symbols {
		if p := e.pairs[s]; p.state == StateEmergencyHalted {
			// A start re-creates every pair; it must not clear a halt nobody
			// acknowledged.
			e.mu.Unlock()
			return e.Status(), fmt.Errorf("%w: %s đang DỪNG BẢO VỆ (%s) — bấm TẮT để xác nhận trước khi bật lại", ErrNotStartable, s, p.haltReasonVI)
		}
	}
	pc.Symbols = append([]string(nil), pc.Symbols...)
	overrides := map[string]Config{}
	for s, c := range pc.PairOverrides {
		c.Symbol = s
		overrides[s] = c
	}
	pc.PairOverrides = overrides
	e.pcfg = pc
	inRun := map[string]bool{}
	for _, s := range pc.Symbols {
		inRun[s] = true
	}
	now := e.now()
	for _, s := range e.symbols {
		p := e.pairs[s]
		*p = pair{symbol: s, cfg: pc.PairConfig(s), inRun: inRun[s], state: StateDisabled, stateSince: now}
		if p.inRun {
			p.state = StateIdleScanning
		}
	}
	e.haltReasonVI = ""
	e.setStateLocked(StateRunning, true)
	hold := "giữ khi funding còn dương"
	if d := pc.DefaultPairConfig; d.MaxHoldEpochs > 0 {
		hold = fmt.Sprintf("tối đa %d mốc settle", d.MaxHoldEpochs)
	}
	overridden := ""
	if len(pc.PairOverrides) > 0 {
		var names []string
		for s := range pc.PairOverrides {
			names = append(names, s)
		}
		sort.Strings(names)
		overridden = " · cấu hình riêng: " + strings.Join(names, ", ")
	}
	e.logLocked("START", "", fmt.Sprintf("BẬT trên %s: tối đa %d cặp đồng thời, hạn mức vốn %.2f quote, mặc định %.2f quote mỗi chân, Net APR ≥ %.2f%%, %s, quét mỗi %s%s",
		strings.Join(pc.Symbols, ", "), pc.MaxConcurrentPositions, pc.TotalCapitalCapQuote, pc.DefaultPairConfig.NotionalQuote, pc.DefaultPairConfig.MinNetAPRPct, hold, pc.ScanInterval, overridden))
	e.mu.Unlock()
	e.poke()
	return e.Status(), nil
}

// OperatorClose is what a stop-and-close, a kill or a close-pair did to one
// symbol.
type OperatorClose struct {
	Symbol string `json:"symbol"`
	// Attempted is a close having been sent to the portal.
	Attempted bool        `json:"attempted"`
	Result    CloseResult `json:"result"`
	// Flat is the symbol ending flat — closed now, or nothing was held.
	Flat bool `json:"flat"`
	// NotTheBots is a position held on the symbol that the bot did not open:
	// left alone, and not a failure of the stop.
	NotTheBots bool   `json:"not_the_bots"`
	DetailVI   string `json:"detail_vi"`

	// holding is the venue reading the close was decided on; read says there
	// was one.
	holding Holding
	read    bool
}

// StopOutcome is what a stop did: the positions it left open, or the closes it
// ran.
type StopOutcome struct {
	// KeptIntentIDs are the bot's positions left open on the venue by a stop
	// that keeps them, read AFTER the Step in flight returned — so an open that
	// was already on its way when DỪNG was pressed is named here too.
	KeptIntentIDs []string        `json:"kept_intent_ids"`
	Closes        []OperatorClose `json:"closes"`
}

// Stop ends the run. closeNow also closes every one of the bot's pairs; without
// it the positions are kept as they are, and stay named in the status. From
// EMERGENCY_HALTED — of the bot or of any pair — it is the acknowledgement that
// returns the bot to disabled. seenHaltSeq is the halt count the page showed
// when the operator pressed; a stop is refused when a halt has been raised
// since, because it would acknowledge what nobody read. A negative seenHaltSeq
// checks nothing (a caller with no page). The status returned is read after the
// stop has fully finished.
func (e *Engine) Stop(ctx context.Context, closeNow bool, seenHaltSeq int) (StatusView, StopOutcome, error) {
	e.mu.Lock()
	if e.busy != "" || e.killing {
		busy := e.busy
		e.mu.Unlock()
		return e.Status(), StopOutcome{}, fmt.Errorf("%w (%s)", ErrBusy, busy)
	}
	if seenHaltSeq >= 0 && seenHaltSeq != e.haltSeq {
		why := e.newestHaltLocked()
		seq := e.haltSeq
		e.mu.Unlock()
		return e.Status(), StopOutcome{}, fmt.Errorf("%w (trang hiển thị #%d, hiện là #%d): %s", ErrHaltedWhileStopping, seenHaltSeq, seq, why)
	}
	e.stopSeq++
	e.busySeq++
	mine := stopTicket{stopSeq: e.stopSeq, killSeq: e.killSeq, haltSeq: e.haltSeq, busySeq: e.busySeq}
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
	}
	if e.busySeq == mine.busySeq {
		e.busy = ""
	}
	return e.statusLocked(), out, err
}

// stopTicket is what a stop saw when it was pressed.
type stopTicket struct {
	stopSeq, killSeq, haltSeq, busySeq int
}

// ErrHaltedWhileStopping is a stop refused because the bot or a pair halted
// while the stop waited for the scan or trade in flight.
var ErrHaltedWhileStopping = errors.New("autotrade: bot hoặc một cặp vừa DỪNG BẢO VỆ trong lúc lệnh dừng chờ — lệnh dừng KHÔNG xác nhận thứ bạn chưa đọc")

// newestHaltLocked is the reason of the most recent halt.
func (e *Engine) newestHaltLocked() string {
	why, seq := e.haltReasonVI, e.haltGlobalSeq
	for _, s := range e.symbols {
		if p := e.pairs[s]; p.state == StateEmergencyHalted && p.haltSeq > seq {
			why, seq = s+": "+p.haltReasonVI, p.haltSeq
		}
	}
	return why
}

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
		why := e.newestHaltLocked()
		e.mu.Unlock()
		return StopOutcome{}, fmt.Errorf("%w: %s", ErrHaltedWhileStopping, why)
	}
	if !closeNow {
		defer e.mu.Unlock()
		out := StopOutcome{}
		var halted []string
		for _, s := range e.symbols {
			p := e.pairs[s]
			if p.pos != nil {
				out.KeptIntentIDs = append(out.KeptIntentIDs, p.pos.IntentID)
			}
			if p.state == StateEmergencyHalted {
				halted = append(halted, s+" ("+p.haltReasonVI+")")
			}
		}
		kept := "không giữ vị thế nào"
		if len(out.KeptIntentIDs) > 0 {
			kept = "vị thế " + strings.Join(out.KeptIntentIDs, ", ") + " GIỮ NGUYÊN trên sàn"
		}
		switch {
		case e.state == StateEmergencyHalted:
			e.logLocked("STOP", "", "người vận hành xác nhận DỪNG BẢO VỆ ("+e.haltReasonVI+") — về TẮT; "+kept)
		case len(halted) > 0:
			e.logLocked("STOP", "", "dừng tự động và xác nhận DỪNG BẢO VỆ của "+strings.Join(halted, "; ")+" — "+kept)
		default:
			e.logLocked("STOP", "", "dừng tự động — "+kept)
		}
		e.resetPairsLocked(true)
		e.haltReasonVI = ""
		e.setStateLocked(StateDisabled, true)
		return out, nil
	}
	e.mu.Unlock()

	closes := e.closeAllForOperator(ctx, "stop")
	e.mu.Lock()
	defer e.mu.Unlock()
	out := StopOutcome{Closes: closes}
	if e.killing {
		return out, nil
	}
	var failed []string
	for _, oc := range closes {
		p := e.pairs[oc.Symbol]
		switch {
		case oc.Flat, oc.NotTheBots:
			p.pos = nil
			if oc.read {
				e.recordHoldingLocked(p, oc.holding)
			}
			if oc.Flat {
				e.recordHoldingLocked(p, Holding{Status: HedgeBothFlat, ReasonVI: "theo DỪNG & ĐÓNG (đọc từ sàn)", ToleranceQtyCoin: p.holding.ToleranceQtyCoin})
			}
		default:
			failed = append(failed, oc.Symbol+": "+oc.DetailVI)
			e.haltPairLocked(p, "DỪNG & ĐÓNG không về phẳng: "+oc.DetailVI)
		}
	}
	if len(failed) == 0 {
		e.resetPairsLocked(false)
		e.haltReasonVI = ""
		e.logLocked("STOP", "", "dừng tự động và đóng mọi cặp của bot — phẳng")
		e.setStateLocked(StateDisabled, true)
		return out, nil
	}
	e.retirePairsLocked()
	e.haltLocked("DỪNG & ĐÓNG không về phẳng: " + strings.Join(failed, " · "))
	return out, nil
}

// retirePairsLocked takes every pair out of the run for a bot that is about to
// halt: a halted pair keeps its halt and its position, the rest are disabled.
func (e *Engine) retirePairsLocked() {
	for _, s := range e.symbols {
		p := e.pairs[s]
		p.inRun, p.paused, p.skipVI, p.rank = false, false, "", 0
		if p.state != StateEmergencyHalted {
			e.setPairStateLocked(p, StateDisabled, true)
		}
	}
}

// resetPairsLocked returns every pair to disabled with no halt. keepPositions
// leaves the positions the bot held named — the venue still holds them, and a
// status that dropped them would show a portfolio emptier than the account; the
// next Start re-reads and adopts them either way.
func (e *Engine) resetPairsLocked(keepPositions bool) {
	for _, s := range e.symbols {
		p := e.pairs[s]
		if !keepPositions {
			p.pos = nil
		}
		p.haltReasonVI, p.inRun, p.paused, p.skipVI, p.rank = "", false, false, "", 0
		p.readFailures, p.tradeFailures = 0, 0
		p.historyBlindSince = time.Time{}
		e.setPairStateLocked(p, StateDisabled, true)
	}
}

// Kill stops the bot at once and closes every one of its pairs, leaving the bot
// EMERGENCY_HALTED whatever the closes did. A scan in flight is abandoned; an
// open or close already sent is waited for — never cancelled — so each close
// below acts on a pair that is both open or both flat, read from the venue.
func (e *Engine) Kill(ctx context.Context) (StatusView, []OperatorClose, error) {
	e.mu.Lock()
	if e.killing {
		e.mu.Unlock()
		return e.Status(), nil, fmt.Errorf("%w (kill)", ErrBusy)
	}
	e.killing = true
	e.killSeq++
	e.busySeq++
	e.busy = "kill" // taken over from a stop or a close-pair, if one is waiting
	e.logLocked("KILL", "", "KILL SWITCH — dừng ngay, đóng hai chân của MỌI cặp của bot")
	if e.cancelScan != nil {
		e.cancelScan()
	}
	e.mu.Unlock()

	e.opMu.Lock()
	defer e.opMu.Unlock()
	closes := e.closeAllForOperator(ctx, "kill")

	e.mu.Lock()
	defer e.mu.Unlock()
	e.killing, e.busy = false, ""
	flat, left := 0, []string{}
	for _, oc := range closes {
		p := e.pairs[oc.Symbol]
		switch {
		case oc.Flat || oc.NotTheBots:
			flat++
			if oc.NotTheBots {
				// The venue names a person's intent: a position view the bot
				// kept is no longer the bot's.
				p.pos = nil
			}
			if oc.Flat {
				p.pos = nil
				e.recordHoldingLocked(p, Holding{Status: HedgeBothFlat, ReasonVI: "theo KILL (đọc từ sàn)", ToleranceQtyCoin: p.holding.ToleranceQtyCoin})
				if p.state == StateEmergencyHalted {
					// Flat now, read from the venue: the pair's own halt no
					// longer describes it. The bot's halt below still needs
					// the operator's acknowledgement, and names this.
					e.logLocked("KILL", p.symbol, "cặp đã phẳng — DỪNG BẢO VỆ trước đó của cặp ("+p.haltReasonVI+") gộp vào DỪNG BẢO VỆ của bot")
					p.haltReasonVI = ""
					e.setPairStateLocked(p, StateDisabled, true)
				}
			}
		default:
			left = append(left, oc.Symbol+": "+oc.DetailVI)
			why := "KILL không về phẳng: " + oc.DetailVI
			if p.haltReasonVI != "" {
				why += " (trước đó: " + p.haltReasonVI + ")"
			}
			e.haltPairLocked(p, why)
		}
	}
	e.retirePairsLocked()
	summary := fmt.Sprintf("%d/%d symbol phẳng hoặc không phải của bot", flat, len(closes))
	if len(left) > 0 {
		summary += " · CHƯA PHẲNG — " + strings.Join(left, " · ")
	}
	e.haltLocked("KILL SWITCH bởi người vận hành — " + summary)
	return e.statusLocked(), closes, nil
}

// ClosePair closes the bot's position on ONE symbol and pauses that pair's
// entries, leaving every other pair running. A close that does not end flat
// halts the pair. A pair that was halted stays halted: closing is not
// acknowledging.
func (e *Engine) ClosePair(ctx context.Context, symbol string) (StatusView, OperatorClose, error) {
	e.mu.Lock()
	p, ok := e.pairs[symbol]
	if !ok {
		e.mu.Unlock()
		return e.Status(), OperatorClose{}, fmt.Errorf("%w: %q", ErrUnknownSymbol, symbol)
	}
	if e.busy != "" || e.killing || e.stopping {
		busy := e.busy
		e.mu.Unlock()
		return e.Status(), OperatorClose{}, fmt.Errorf("%w (%s)", ErrBusy, busy)
	}
	e.busySeq++
	t := closeTicket{busySeq: e.busySeq, killSeq: e.killSeq}
	e.busy = "close_pair"
	// Paused BEFORE waiting for the scan in flight: that scan checks it at the
	// moment it would send, so it opens nothing more on this symbol.
	p.paused = true
	e.logLocked("CLOSE_PAIR", symbol, "người vận hành đóng cặp — tạm dừng vào lệnh mới trên cặp, chờ lượt quét đang chạy xong")
	e.mu.Unlock()

	oc, err := e.closePair(ctx, p, t)

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.busySeq == t.busySeq {
		e.busy = ""
	}
	return e.statusLocked(), oc, err
}

// closeTicket is what a close-pair saw when it was pressed.
type closeTicket struct {
	busySeq, killSeq int
}

func (e *Engine) closePair(ctx context.Context, p *pair, t closeTicket) (OperatorClose, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.Lock()
	if e.killing || e.killSeq != t.killSeq {
		e.mu.Unlock()
		return OperatorClose{}, fmt.Errorf("%w (kill đã chạy trong lúc chờ — lệnh đóng cặp bị bỏ)", ErrBusy)
	}
	e.mu.Unlock()

	oc := e.closeForOperator(ctx, p.symbol, "close_pair")

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.killing {
		return oc, nil
	}
	if oc.read {
		e.recordHoldingLocked(p, oc.holding)
	}
	// A cooling-down pair keeps its cooldown: closing, like pausing, is not a
	// way around it.
	rest := func() {
		if p.state != StateEmergencyHalted && !(p.state == StateCooldown && e.now().Before(p.cooldownUntil)) {
			e.restPairLocked(p)
		}
	}
	switch {
	case oc.Flat:
		p.pos = nil
		p.tradeFailures = 0
		e.recordHoldingLocked(p, Holding{Status: HedgeBothFlat, ReasonVI: "theo lệnh đóng cặp vừa xong (đọc từ sàn)", ToleranceQtyCoin: p.holding.ToleranceQtyCoin})
		rest()
	case oc.NotTheBots:
		p.pos = nil
		rest()
	default:
		e.haltPairLocked(p, "ĐÓNG CẶP không về phẳng: "+oc.DetailVI)
	}
	return oc, nil
}

// PairAction is an operator's switch on one pair.
type PairAction string

const (
	// PairPause stops new entries on the pair; a held position is still
	// managed to its exit. It sends nothing.
	PairPause PairAction = "pause"
	// PairResume lets the bot open the pair again — the one pair action that
	// leads to orders, so the page confirms it first.
	PairResume PairAction = "resume"
	// PairAcknowledge clears the pair's halt, quoting the halt the operator
	// read, and leaves the pair PAUSED: acknowledging never opens anything by
	// itself. A position still on the venue is adopted at the next scan.
	PairAcknowledge PairAction = "ack"
)

// PairControl pauses, resumes or acknowledges one pair. haltSeq is the pair's
// halt number as the operator read it; only an acknowledgement uses it.
func (e *Engine) PairControl(symbol string, action PairAction, haltSeq int) (StatusView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.pairs[symbol]
	if !ok {
		return e.statusLocked(), fmt.Errorf("%w: %q", ErrUnknownSymbol, symbol)
	}
	if e.busy != "" || e.killing || e.stopping {
		return e.statusLocked(), fmt.Errorf("%w (%s)", ErrBusy, e.busy)
	}
	switch action {
	case PairPause:
		if e.state != StateRunning {
			return e.statusLocked(), fmt.Errorf("%w: bot không chạy", ErrNotStartable)
		}
		if p.paused || !p.inRun {
			return e.statusLocked(), nil
		}
		p.paused = true
		if p.state == StateEvaluating {
			// A cooling-down pair keeps its cooldown: pausing and resuming
			// must not be a way around it.
			e.restPairLocked(p)
		}
		e.logLocked("PAUSE", symbol, "tạm dừng vào lệnh mới trên cặp — vị thế đang giữ (nếu có) vẫn được quản lý thoát")
	case PairResume:
		if e.state != StateRunning {
			return e.statusLocked(), fmt.Errorf("%w: bot không chạy — BẬT bot với cặp này thay vì tiếp tục", ErrNotStartable)
		}
		if p.state == StateEmergencyHalted {
			return e.statusLocked(), fmt.Errorf("%w: cặp đang DỪNG BẢO VỆ (%s) — xác nhận trước", ErrNotStartable, p.haltReasonVI)
		}
		if !p.inRun {
			cfg := e.pcfg.PairConfig(symbol)
			if err := cfg.Validate(e.maxNotional); err != nil {
				return e.statusLocked(), err
			}
			if need := cfg.NotionalQuote * e.capitalPerNotional(); need > e.pcfg.TotalCapitalCapQuote {
				return e.statusLocked(), fmt.Errorf("%w: một vị thế %s buộc %.2f quote vốn, vượt hạn mức %.2f", errConfig, symbol, need, e.pcfg.TotalCapitalCapQuote)
			}
			p.cfg = cfg
		}
		wasIn := p.inRun
		p.inRun, p.paused = true, false
		p.lastEntryKey, p.lastSkipKey = "", ""
		if p.state == StateDisabled {
			e.setPairStateLocked(p, StateIdleScanning, false)
		}
		if !wasIn {
			e.pcfg.Symbols = append(e.pcfg.Symbols, symbol)
		}
		e.logLocked("RESUME", symbol, "cho phép vào lệnh lại trên cặp")
		defer e.poke()
	case PairAcknowledge:
		if p.state != StateEmergencyHalted {
			return e.statusLocked(), fmt.Errorf("%w: cặp %s không DỪNG BẢO VỆ", ErrNotStartable, symbol)
		}
		if haltSeq != p.haltSeq {
			return e.statusLocked(), fmt.Errorf("%w (bạn đọc #%d, hiện là #%d: %s)", ErrStaleAcknowledgement, haltSeq, p.haltSeq, p.haltReasonVI)
		}
		e.logLocked("ACK", symbol, "người vận hành xác nhận DỪNG BẢO VỆ #"+fmt.Sprint(p.haltSeq)+" ("+p.haltReasonVI+") — cặp TẠM DỪNG; lượt quét sau đọc lại cặp từ sàn và tiếp nhận vị thế của bot nếu còn")
		p.haltReasonVI, p.pos = "", nil
		p.readFailures, p.tradeFailures = 0, 0
		p.historyBlindSince = time.Time{}
		p.lastEntryKey, p.lastSkipKey = "", ""
		if p.inRun {
			p.paused = true
		}
		e.restPairLocked(p)
		if e.state == StateRunning {
			defer e.poke()
		}
	default:
		return e.statusLocked(), fmt.Errorf("%w: hành động %q không phải pause, resume hay ack", errConfig, action)
	}
	return e.statusLocked(), nil
}

// closeAllForOperator closes the bot's pair on every symbol the portal trades,
// one at a time, each decided from the venue.
func (e *Engine) closeAllForOperator(ctx context.Context, why string) []OperatorClose {
	out := make([]OperatorClose, 0, len(e.symbols))
	for _, s := range e.symbols {
		out = append(out, e.closeForOperator(ctx, s, why))
	}
	return out
}

// busyRetryEvery is how often an operator close retries a portal whose write
// lock is held by a manual action.
const busyRetryEvery = 500 * time.Millisecond

// closeForOperator closes the bot's pair on symbol for a stop, a kill or a
// close-pair, deciding from the venue: nothing held is already flat; ONE
// tracked intent both-open and minted by the bot is closed; anything else is
// refused, because squaring an unbalanced, unexplained or somebody else's
// position is a person's call.
func (e *Engine) closeForOperator(ctx context.Context, symbol, why string) OperatorClose {
	actionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.actionTimeout)
	defer cancel()
	holding, err := e.trader.Holding(actionCtx, symbol)
	if err != nil {
		return OperatorClose{Symbol: symbol, DetailVI: "không đọc được vị thế " + symbol + " từ sàn — KHÔNG đóng, kiểm tay: " + err.Error()}
	}
	decided := func(oc OperatorClose) OperatorClose {
		oc.holding, oc.read = holding, true
		return oc
	}
	switch {
	case holding.Status == HedgeBothFlat:
		return decided(OperatorClose{Symbol: symbol, Flat: true, DetailVI: symbol + " không giữ gì trên sàn — không có gì để đóng"})
	case holding.Status != HedgeBothOpen:
		return decided(OperatorClose{Symbol: symbol, DetailVI: fmt.Sprintf("%s đang %s (%s) — bot không tự đóng hay làm phẳng; dùng LÀM PHẲNG", symbol, holding.Status, holding.ReasonVI)})
	case holding.HeldIntents != 1 || holding.IntentID == "":
		return decided(OperatorClose{Symbol: symbol, DetailVI: fmt.Sprintf("%s có %d ý định đang giữ — không biết đóng cái nào; đóng tay từng ý định", symbol, holding.HeldIntents)})
	case !holding.FromAutotrade:
		// The bot manages its own positions only. A pair a person opened has
		// its own ĐÓNG VỊ THẾ button, and a kill switch that also closed it
		// would be the bot trading a position it cannot name as its own.
		return decided(OperatorClose{Symbol: symbol, NotTheBots: true, DetailVI: fmt.Sprintf("vị thế đang mở trên %s là %s, không phải của bot — KHÔNG đóng; đóng bằng ĐÓNG VỊ THẾ nếu cần", symbol, holding.IntentID)})
	}

	reasonVI := "Đóng thủ công"
	switch why {
	case "stop":
		reasonVI = "Dừng bot: Stop & Close"
	case "kill":
		reasonVI = "Kill bot khẩn cấp"
	case "close_pair":
		reasonVI = "Người vận hành đóng qua nút Đóng vị thế"
	}

	for {
		res := e.trader.Close(actionCtx, symbol, holding.IntentID, reasonVI)
		if !res.Busy {
			oc := OperatorClose{Symbol: symbol, Attempted: true, Result: res, Flat: res.Flat}
			switch {
			case res.Flat:
				oc.DetailVI = fmt.Sprintf("đã đóng %s PHẲNG: %.8f coin · RealizedQuote %+.8f (không phải lãi ròng)", holding.IntentID, res.ClosedQtyCoin, res.RealizedQuote)
			case res.Refused:
				oc.DetailVI = "đóng " + holding.IntentID + " bị từ chối trước khi gửi — hai chân vẫn mở: " + res.ErrorVI
			default:
				oc.DetailVI = fmt.Sprintf("đóng %s chưa phẳng (gửi chưa xác nhận %v, báo động %v): %s", holding.IntentID, res.SentUnconfirmed, res.Alarm, res.ErrorVI)
			}
			e.mu.Lock()
			e.logLocked(strings.ToUpper(why), symbol, oc.DetailVI)
			e.mu.Unlock()
			return oc
		}
		select {
		case <-actionCtx.Done():
			return OperatorClose{Symbol: symbol, DetailVI: "portal bận thao tác ghi khác tới hết hạn — CHƯA gửi lệnh đóng " + holding.IntentID}
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

// markToMid fills a held position's drift figures from the pair's newest mids.
func markToMid(pos *PositionView, sig *SignalView) {
	pos.DriftLabelVI = driftLabelVI
	if sig == nil || !(pos.QtyCoin > 0) || !(pos.SpotEntryAvgQuote > 0) || !(pos.PerpEntryAvgQuote > 0) ||
		!(sig.SpotMidQuote > 0) || !(sig.PerpMidQuote > 0) {
		return
	}
	spot := (sig.SpotMidQuote - pos.SpotEntryAvgQuote) * pos.QtyCoin
	perp := (pos.PerpEntryAvgQuote - sig.PerpMidQuote) * pos.QtyCoin
	pos.SpotLegDriftQuote, pos.PerpLegDriftQuote, pos.PairDriftQuote = ptr(spot), ptr(perp), ptr(spot+perp)
	pos.MarkedAtMs = sig.EvaluatedAtMs
}

func (e *Engine) radarOfLocked(p *pair) Radar {
	switch {
	case p.state == StateEmergencyHalted:
		return RadarHalted
	case p.state == StateOpening:
		return RadarOpening
	case p.pos != nil || p.state == StateInPosition || p.state == StateClosing:
		return RadarHolding
	case p.state == StateCooldown:
		return RadarCooldown
	case e.state == StateRunning && !p.provenFlat && !p.holdingReadAt.IsZero():
		return RadarUnproven
	case !p.inRun:
		return RadarOff
	case p.paused:
		return RadarPaused
	case p.skipVI != "":
		return RadarSkipped
	case p.signal != nil && p.signal.EntryEligible:
		return RadarEligible
	}
	return RadarScanning
}

func (e *Engine) statusLocked() StatusView {
	cpn := e.capitalPerNotional()
	v := StatusView{
		State: e.state, StateVI: stateVI[e.state], StateSinceMs: e.stateSince.UnixMilli(),
		Enabled: e.state == StateRunning, Portfolio: e.pcfg.view(), NowMs: e.now().UnixMilli(),
		CapitalPerNotional: cpn, HaltReasonVI: e.haltReasonVI, HaltSeq: e.haltSeq, Busy: e.busy,
		Log: e.log.newestFirst(), NoticeVI: noticeVI, Pairs: []PairView{}, Positions: []PositionView{},
		UnrealizedDriftLabelVI: driftLabelVI,
	}
	if !e.lastScanAt.IsZero() {
		v.LastScanAtMs = e.lastScanAt.UnixMilli()
	}
	if !e.nextScanAt.IsZero() && v.Enabled {
		v.NextScanAtMs = e.nextScanAt.UnixMilli()
	}
	v.SlotsUsed, v.OpenPositions, v.NotionalDeployedQuote, v.CapitalDeployedQuote, v.CapitalCommittedQuote = e.deployedLocked()
	for _, s := range e.symbols {
		if pl := e.placeLocked(e.pairs[s]); pl.uses && !pl.sized {
			v.UnsizedPairs = append(v.UnsizedPairs, s)
		}
	}
	for _, s := range e.symbols {
		p := e.pairs[s]
		_, overridden := e.pcfg.PairOverrides[s]
		pv := PairView{
			Symbol: s, State: p.state, StateVI: stateVI[p.state], StateSinceMs: p.stateSince.UnixMilli(),
			InRun: p.inRun, Paused: p.paused, Overridden: overridden && p.inRun, Config: p.cfg.view(),
			Radar: e.radarOfLocked(p), Rank: p.rank, SkipVI: p.skipVI,
			HedgeStatus: p.holding.Status, HedgeReasonVI: p.holding.ReasonVI,
			ResidualQtyCoin: p.holding.ResidualQtyCoin, ToleranceQtyCoin: p.holding.ToleranceQtyCoin,
			ReadFailures: p.readFailures, TradeFailures: p.tradeFailures,
			HaltReasonVI: p.haltReasonVI, HaltSeq: p.haltSeq, CapitalPerNotional: cpn,
		}
		pv.RadarVI = radarVI[pv.Radar]
		pl := e.placeLocked(p)
		pv.SlotUsed, pv.SlotReasonVI = pl.uses, pl.whyVI
		if pv.Radar != RadarEligible && pv.Radar != RadarSkipped && pv.Radar != RadarOpening {
			// A rank is the pair's place among the eligible pairs of its scan;
			// once the pair is held or no longer eligible it describes nothing.
			pv.Rank = 0
		}
		if !p.holdingReadAt.IsZero() {
			pv.HoldingReadAtMs = p.holdingReadAt.UnixMilli()
		}
		if !p.lastScanAt.IsZero() {
			pv.LastScanAtMs = p.lastScanAt.UnixMilli()
		}
		if p.state == StateCooldown {
			pv.CooldownUntilMs = p.cooldownUntil.UnixMilli()
		}
		if p.state == StateEmergencyHalted {
			v.HaltedPairs++
		}
		if p.signal != nil {
			sig := *p.signal
			pv.Signal = &sig
		}
		if p.pos != nil {
			pos := *p.pos
			markToMid(&pos, p.signal)
			if pos.PairDriftQuote != nil && !math.IsNaN(*pos.PairDriftQuote) {
				v.UnrealizedDriftQuote += *pos.PairDriftQuote
				v.UnrealizedDriftPriced++
			}
			pv.Position = &pos
			v.Positions = append(v.Positions, pos)
		}
		v.Pairs = append(v.Pairs, pv)
	}
	return v
}
