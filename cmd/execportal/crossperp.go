// Engine 2's desk: the portal's wiring for the cross-venue perp–perp engine
// (internal/execution/crossperp), its exclusive symbol lock
// (internal/coordinator) and the dual margin guard (internal/risk) — PLAN 4.5k,
// step 4 (decision Q20).
//
// # What this file is, and what it is not
//
// It is WIRING. Every rule about what may be sent lives in the three packages
// above and is tested there against fakes; this file reads the two venues, hands
// the engine an intent, and turns what comes back into JSON. It decides nothing
// about a trade — the signal is crosssignal.go's, and it is OFF unless a person
// passed -crossperp-pilot.
//
// # Why it dials its own venues
//
// The rest of the portal is ONE venue: Strategy 1 is spot and perp on a single
// exchange, so `markets` holds one profile with two markets of it. Engine 2 is
// the opposite shape — one market (USDⓈ-M / linear perp) on TWO exchanges — so
// it gets its own pair of clients rather than bending `markets` into a shape
// Engine 1 would then have to check for. Both clients still come from the same
// dial functions, so they inherit the same walls: broker.NewClient refuses any
// host outside the documented testnet list, refuses every redirect and every
// host but its own, and this command takes no flag that could move either.
//
// # Why the coordinator lives here and not beside Engine 1
//
// The lock's whole job is to stop the two engines netting each other on a
// one-way account, and it can only do that if it can READ both engines' venues:
// ReconcileActivePositions grants nothing until every venue has answered. A
// coordinator that knew only Binance would read an Engine-2 pair as an Engine-1
// short and hand the symbol to the wrong engine. So the coordinator exists only
// when BOTH venues are dialled — that is, only under -crossperp — and Engine 1
// consults it only then (engine1Lock). Running Engine 1 WITHOUT -crossperp
// leaves it unlocked, exactly as it was before this step; that is a deployment
// precondition, written down rather than papered over.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
	bybitbroker "futures-arbitrage-scanner/internal/broker/bybit"
	"futures-arbitrage-scanner/internal/coordinator"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/execution"
	"futures-arbitrage-scanner/internal/execution/crossperp"
	"futures-arbitrage-scanner/internal/risk"
)

// The two venue names. They are the ONE spelling shared by the coordinator's
// lock table, the margin guard's tiers, risk.OpenRequest.Venues and the page —
// risk.MarginReader's contract requires exactly that.
const (
	crossVenueBinance = "binance_futures"
	crossVenueBybit   = "bybit_linear"
)

// crossIntentPrefix marks an Engine-2 intent id, as "a" marks the Binance
// auto-trader's. It keeps the two engines' ids apart in a log read by eye; the
// ORDER ids are already disjoint by construction (crossperp's "cp1" prefix).
const crossIntentPrefix = "x"

// crossMaxNotionalQuote is the portal's ceiling on ONE leg of an Engine-2 pair.
// Engine 2 is levered on BOTH legs, where Strategy 1 pays for its spot leg in
// full, so the ceiling is lower than maxNotionalQuote and is its own constant
// rather than a shared one.
const crossMaxNotionalQuote = 5_000.0

// crossSettings are the desk's parameters, units in the names (rule 4).
type crossSettings struct {
	MaxSlippageBps float64
	LegTimeout     time.Duration
	ActionTimeout  time.Duration
	LocksPath      string

	// PerpMarginFracByVenue is the collateral posted on ONE leg as a fraction of
	// its notional. A DECISION, not a venue fact, and the guard is told about it
	// only through what the venues then report.
	PerpMarginFrac float64
}

func defaultCrossSettings() crossSettings {
	return crossSettings{
		MaxSlippageBps: execution.DefaultMaxSlippageBps,
		LegTimeout:     execution.DefaultLegTimeout,
		ActionTimeout:  3 * time.Minute,
		LocksPath:      coordinator.DefaultLocksPath,
		PerpMarginFrac: 0.50,
	}
}

// crossPerpVenue is everything Engine 2 asks of ONE venue, and nothing more.
//
// It is deliberately NARROWER than perpVenue. Engine 2 never needs the raw HTTP
// client, the market name or the maintenance bracket, and an interface that
// demanded them could only be satisfied by a real client — which would make the
// desk untestable without a network, and an untestable desk is where the
// mistakes that cost a leg live. Both real venues satisfy it (the assertions
// below), and so does a fake.
type crossPerpVenue interface {
	broker.Broker
	broker.MarkPriceReader
	FetchInstrument(ctx context.Context, symbol string) (binancebroker.MarketRules, error)
	FetchDepthBook(ctx context.Context, symbol string) (exchanges.DepthBook, error)
	CommissionRates(ctx context.Context, symbol string) (binancebroker.CommissionRates, error)
	FundingRateHistory(ctx context.Context, symbol string, startMs, endMs int64) ([]binancebroker.FundingRate, error)
}

var (
	_ crossPerpVenue = (*binancebroker.Client)(nil)
	_ crossPerpVenue = bybitVenue{}
)

// crossVenue is one perpetual venue as the desk holds it: the name everything
// else knows it by, the client, and the margin reader built on the same client.
type crossVenue struct {
	Name     string
	Perp     crossPerpVenue
	SourceVI string
	Err      error
	Margin   risk.MarginReader
}

// crossVenues is the pair, in a fixed order so the page and the log never
// reorder under the reader.
type crossVenues struct {
	list []crossVenue
}

func (v crossVenues) get(name string) (crossVenue, bool) {
	for _, c := range v.list {
		if c.Name == name {
			return c, true
		}
	}
	return crossVenue{}, false
}

// ready reports whether BOTH venues answered a credential. Engine 2 is a pair
// or it is nothing: one leg is not a smaller version of the strategy.
func (v crossVenues) ready() error {
	var errs []error
	for _, c := range v.list {
		if c.Perp == nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.Name, c.Err))
		}
	}
	if len(v.list) < 2 {
		errs = append(errs, errors.New("Động cơ 2 cần ĐÚNG hai sàn"))
	}
	return errors.Join(errs...)
}

// dialCrossVenues builds both perp clients. Like dialMarketsFor it does not
// stop at the first missing credential: the page must come up and SAY what is
// missing rather than the process dying.
func dialCrossVenues() crossVenues {
	var out crossVenues

	bn := crossVenue{Name: crossVenueBinance}
	if c, source, err := dialOne(broker.MarketFuturesUSDM, futuresEnv); err != nil {
		bn.Err = err
	} else {
		bn.Perp, bn.SourceVI = c, source
		bn.Margin = crossperp.BinanceMarginReader{Venue: crossVenueBinance, Account: c, Now: time.Now}
	}
	out.list = append(out.list, bn)

	by := crossVenue{Name: crossVenueBybit}
	if c, source, err := dialBybit(); err != nil {
		by.Err = err
	} else {
		by.Perp, by.SourceVI = bybitVenue{c}, source
		by.Margin = crossperp.BybitMarginReader{Venue: crossVenueBybit, Account: c, Now: time.Now}
	}
	out.list = append(out.list, by)

	return out
}

var (
	_ crossperp.BinanceAccountMargin = (*binancebroker.Client)(nil)
	_ crossperp.BybitAccount         = (*bybitbroker.Client)(nil)
)

// lateCloser breaks the construction cycle: the executor needs the margin
// guard's refusal, the guard needs the engine to close for it, and the engine
// needs the executor. It answers an ERROR rather than an empty list until the
// engine is set — risk.EmergencyCloser's contract says an error is not "no
// pairs", and a guard that read "no pairs" during start-up would clear a red
// latch it never acted on.
type lateCloser struct {
	mu    sync.RWMutex
	inner risk.EmergencyCloser
}

var errCloserNotReady = errors.New("crossperp: Động cơ 2 chưa gắn xong — chưa đọc được danh sách cặp đang mở")

func (c *lateCloser) set(inner risk.EmergencyCloser) {
	c.mu.Lock()
	c.inner = inner
	c.mu.Unlock()
}

func (c *lateCloser) get() risk.EmergencyCloser {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.inner
}

func (c *lateCloser) OpenPairs(ctx context.Context) ([]risk.ExposedPair, error) {
	inner := c.get()
	if inner == nil {
		return nil, errCloserNotReady
	}
	return inner.OpenPairs(ctx)
}

func (c *lateCloser) CloseForMargin(ctx context.Context, pairID, stressedVenue string) error {
	inner := c.get()
	if inner == nil {
		return errCloserNotReady
	}
	return inner.CloseForMargin(ctx, pairID, stressedVenue)
}

// crossDesk is Engine 2 as the portal operates it.
type crossDesk struct {
	venues   crossVenues
	symbols  []string
	settings crossSettings
	now      func() time.Time

	coord       *coordinator.Coordinator
	executor    *crossperp.Executor
	engine2     *crossperp.Engine
	marginGuard *risk.MarginGuard
	events      *crossEvents

	// pilot is the funding-spread signal and the exit loop (crosssignal.go). It
	// is never nil once the desk exists — an advisory pilot still measures and
	// still shows — but it sends nothing unless its own Enabled is set.
	pilot *crossPilot

	// mintID is the portal's intent-id source, shared so a pilot id and a button
	// id can never collide. Set by newPortal.
	mintID func(prefix, symbol string) (string, error)

	// loadVI is what the lock file said when the process started: it is the one
	// fact about the locks that a later read cannot recover.
	loadVI string

	mu             sync.Mutex
	reconciledAtMs int64
	reconcileVI    string
	reconcileErrVI string
	lastOpen       *crossOpenView
	lastClose      *crossCloseView
}

// newCrossDesk wires the coordinator, the executor, the engine and the guard.
// It reads no venue and sends nothing; reconcile does the first reading.
func newCrossDesk(venues crossVenues, symbols []string, settings crossSettings, pilotCfg crossPilotConfig, now func() time.Time) (*crossDesk, error) {
	if now == nil {
		now = time.Now
	}
	if err := venues.ready(); err != nil {
		return nil, fmt.Errorf("Động cơ 2 cần credential của CẢ HAI sàn: %w", err)
	}
	if len(symbols) == 0 {
		return nil, errors.New("Động cơ 2 cần ít nhất một symbol")
	}

	coordVenues := make([]coordinator.Venue, 0, len(venues.list))
	readers := make([]risk.MarginReader, 0, len(venues.list))
	for _, v := range venues.list {
		coordVenues = append(coordVenues, coordinator.Venue{Name: v.Name, Reader: crossVenueReader{v.Perp}})
		readers = append(readers, v.Margin)
	}

	coord, load, err := coordinator.New(coordinator.Config{
		Path: settings.LocksPath, Venues: coordVenues, Symbols: symbols, Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("không dựng được bộ khóa cặp: %w", err)
	}

	events := newCrossEvents()
	closer := &lateCloser{}

	guardCfg := risk.DefaultMarginGuardConfig()
	guardCfg.Now = now
	guardCfg.OnEvent = events.onMargin
	guard, err := risk.NewMarginGuard(guardCfg, readers, closer)
	if err != nil {
		return nil, fmt.Errorf("không dựng được van ký quỹ kép: %w", err)
	}

	execCfg := crossperp.DefaultConfig()
	execCfg.MaxSlippageBps = settings.MaxSlippageBps
	execCfg.LegTimeout = settings.LegTimeout
	execCfg.Now = now
	executor, err := crossperp.NewExecutor(execCfg, coord, guard, events)
	if err != nil {
		return nil, fmt.Errorf("không dựng được cỗ máy chéo sàn: %w", err)
	}

	engine, err := crossperp.NewEngine(executor, coord)
	if err != nil {
		return nil, fmt.Errorf("không dựng được Động cơ 2: %w", err)
	}
	closer.set(engine)

	d := &crossDesk{
		venues: venues, symbols: symbols, settings: settings, now: now,
		coord: coord, executor: executor, engine2: engine, marginGuard: guard,
		events: events, loadVI: loadReportVI(load),
	}
	d.pilot = newCrossPilot(d, pilotCfg)
	return d, nil
}

func loadReportVI(r coordinator.LoadReport) string {
	switch {
	case r.CorruptMovedTo != "":
		return fmt.Sprintf("file khóa HỎNG, đã dời sang %s và bắt đầu rỗng: %s",
			r.CorruptMovedTo, strings.Join(r.ProblemsVI, "; "))
	case r.Found:
		return fmt.Sprintf("đọc %d khóa từ file (CACHE — sàn mới là bằng chứng)", r.Locks)
	default:
		return "không có file khóa — bắt đầu rỗng"
	}
}

// crossVenueReader adapts a perp client to what the coordinator reads: a
// position and the resting orders, both from the VENUE (rule 7). It pins the
// market: an Engine-2 leg is a PERP on both venues, and a coordinator handed a
// spot reader would prove a perp symbol flat by reading the wrong market.
type crossVenueReader struct{ perp crossPerpVenue }

func (r crossVenueReader) GetPosition(ctx context.Context, _ broker.Market, symbol string) (broker.Position, error) {
	return r.perp.GetPosition(ctx, broker.MarketFuturesUSDM, symbol)
}

func (r crossVenueReader) OpenOrders(ctx context.Context, _ broker.Market, symbol string) ([]broker.Order, error) {
	return r.perp.OpenOrders(ctx, broker.MarketFuturesUSDM, symbol)
}

// ---------------------------------------------------------------- reconcile

// reconcileLocks reads every venue's position and resting orders for every
// symbol
// and rebuilds the lock table from them. It is NOT portal.reconcile, which is
// Engine 1's squaring and sends orders: this one only reads. Until it has run
// once the coordinator
// grants nothing, so this is what makes the desk usable — and re-running it is
// how the operator recovers after a network outage.
func (d *crossDesk) reconcileLocks(ctx context.Context) error {
	report, err := d.coord.ReconcileActivePositions(ctx)
	d.mu.Lock()
	d.reconciledAtMs = d.now().UnixMilli()
	if err != nil {
		d.reconcileErrVI = err.Error()
		d.reconcileVI = ""
	} else {
		d.reconcileErrVI = ""
		d.reconcileVI = fmt.Sprintf("%d cặp ĐANG BẬN, %d XUNG ĐỘT, %d CHƯA XÁC MINH trên %d symbol",
			len(report.Occupied), len(report.Conflicts), len(report.Unverified), len(report.Symbols))
	}
	d.mu.Unlock()
	if err != nil {
		return err
	}
	for _, l := range d.coord.ListLocks() {
		log.Printf("execportal/crossperp: khóa %s = %s chủ %s (%s)", l.Symbol, l.State, l.OwnerEngine, l.Details)
	}
	for _, symbol := range report.Unverified {
		log.Printf("execportal/crossperp: %s CHƯA XÁC MINH — không cấp khóa cho tới khi đọc được cả hai sàn", symbol)
	}
	return nil
}

// adoptPairs rebuilds Engine 2's record of any pair the reconciled lock table
// says it owns, so a restart can still CLOSE what a previous process opened. It
// sends nothing: Adopt reads the venues.
func (d *crossDesk) adoptPairs(ctx context.Context) {
	for _, lock := range d.coord.ListLocks() {
		if lock.State != coordinator.StateOccupied || lock.OwnerEngine != coordinator.EngineCrossPerp {
			continue
		}
		long, short, err := d.legSpecsFor(ctx, lock.Symbol, lock.Venues)
		if err != nil {
			log.Printf("execportal/crossperp: %s thuộc Động cơ 2 nhưng KHÔNG nhận lại được (%v) — cặp này chỉ đóng được bằng tay", lock.Symbol, err)
			continue
		}
		intentID := lock.IntentID
		if intentID == "" {
			intentID = crossIntentPrefix + lock.Symbol + "-adopted"
		}
		pair, err := d.engine2.Adopt(ctx, intentID, long, short)
		if err != nil {
			log.Printf("execportal/crossperp: %s — Adopt bị từ chối: %v", lock.Symbol, err)
			continue
		}
		log.Printf("execportal/crossperp: nhận lại cặp %s (%s) long %.8f / short %.8f coin, chưa giải quyết=%v",
			pair.Symbol, pair.IntentID, pair.LongQtyCoin, pair.ShortQtyCoin, pair.Unresolved)
	}
}

// legSpecsFor builds the two leg specs of a symbol from the lock's own venues,
// long first. A lock naming venues this desk does not hold is refused rather
// than guessed at.
func (d *crossDesk) legSpecsFor(ctx context.Context, symbol string, venues []string) (long, short crossperp.LegSpec, err error) {
	names := venues
	if len(names) != 2 {
		// A lock inferred from the venues names both of them; one that does not
		// is not an Engine-2 shape this desk can rebuild.
		return long, short, fmt.Errorf("khóa không nêu đúng hai sàn: %v", venues)
	}
	first, ok := d.venues.get(names[0])
	if !ok {
		return long, short, fmt.Errorf("sàn %q không thuộc bàn Động cơ 2", names[0])
	}
	second, ok := d.venues.get(names[1])
	if !ok {
		return long, short, fmt.Errorf("sàn %q không thuộc bàn Động cơ 2", names[1])
	}
	if long, err = d.legSpec(ctx, first, symbol); err != nil {
		return long, short, err
	}
	short, err = d.legSpec(ctx, second, symbol)
	return long, short, err
}

// legSpec reads ONE leg's rules and book from the venue that leg trades on.
func (d *crossDesk) legSpec(ctx context.Context, v crossVenue, symbol string) (crossperp.LegSpec, error) {
	rules, err := v.Perp.FetchInstrument(ctx, symbol)
	if err != nil {
		return crossperp.LegSpec{}, fmt.Errorf("%s: không đọc được luật giao dịch: %w", v.Name, err)
	}
	book, err := crossReadBook(ctx, v.Perp, symbol)
	if err != nil {
		return crossperp.LegSpec{}, fmt.Errorf("%s: không đọc được sổ lệnh: %w", v.Name, err)
	}
	return crossperp.LegSpec{
		Venue: crossperp.Venue{Name: v.Name, Broker: v.Perp},
		Rules: rules.Instrument,
		Book:  book,
	}, nil
}

// crossReadBook is readBook over the narrow interface: the same summarizing, the
// same rule-13 stamp taken HERE at the read rather than from the venue's clock,
// and the same refusal of a book that does not summarize.
//
// Both perp markets here are denominated in COIN — Binance USDⓈ-M and Bybit
// linear both quote quantities in the base asset — so the contract multiplier is
// 1 and no other is offered. A venue that denominated in contracts would need
// the registry, and handing it 1 would misstate every size (CLAUDE.md's
// contracts trap).
func crossReadBook(ctx context.Context, v crossPerpVenue, symbol string) (depth.Summary, error) {
	book, err := v.FetchDepthBook(ctx, symbol)
	if err != nil {
		return depth.Summary{}, fmt.Errorf("book: %w", err)
	}
	summary := depth.Summarize(book, time.Now().UnixMilli(), func(string, string) (float64, bool) { return 1, true })
	if !summary.OK() {
		return summary, fmt.Errorf("book: %s", summary.ErrVI)
	}
	return summary, nil
}

// ------------------------------------------------------------------- events

// crossEvents keeps the last few execution and margin events for the page and
// puts every one in the process log. It is the executor's Recorder, so it is
// written from the goroutines that drive the legs: everything is under its own
// mutex.
type crossEvents struct {
	mu     sync.Mutex
	lines  []crossEventLine
	margin []crossEventLine
}

type crossEventLine struct {
	AtMs      int64  `json:"at_ms"`
	IntentID  string `json:"intent_id,omitempty"`
	Kind      string `json:"kind"`
	MessageVI string `json:"message_vi"`
}

const crossEventsKept = 200

func newCrossEvents() *crossEvents { return &crossEvents{} }

// Record implements execution.Recorder.
func (e *crossEvents) Record(_ context.Context, ev execution.Event) error {
	line := crossEventLine{AtMs: ev.At.UnixMilli(), IntentID: ev.IntentID, Kind: string(ev.Kind),
		MessageVI: crossEventText(ev)}
	e.mu.Lock()
	e.lines = append(e.lines, line)
	if n := len(e.lines); n > crossEventsKept {
		e.lines = append([]crossEventLine(nil), e.lines[n-crossEventsKept:]...)
	}
	e.mu.Unlock()
	return nil
}

func crossEventText(ev execution.Event) string {
	var b strings.Builder
	if ev.Leg != "" {
		fmt.Fprintf(&b, "[%s] ", ev.Leg)
	}
	b.WriteString(string(ev.Kind))
	if ev.ClientOrderID != "" {
		fmt.Fprintf(&b, " id=%s", ev.ClientOrderID)
	}
	if ev.QtyCoin != 0 {
		fmt.Fprintf(&b, " %.8f coin", ev.QtyCoin)
	}
	if ev.DetailVI != "" {
		b.WriteString(" — " + ev.DetailVI)
	}
	if ev.Err != nil {
		b.WriteString(" — LỖI: " + ev.Err.Error())
	}
	return b.String()
}

func (e *crossEvents) onMargin(ev risk.MarginEvent) {
	msg := ev.MessageVI
	if ev.Err != nil {
		msg += " — LỖI: " + ev.Err.Error()
	}
	line := crossEventLine{AtMs: ev.AtMs, IntentID: ev.PairID, Kind: string(ev.Kind), MessageVI: msg}
	log.Printf("execportal/crossperp: VAN KÝ QUỸ %s %s %s", ev.Kind, ev.Venue, msg)
	e.mu.Lock()
	e.margin = append(e.margin, line)
	if n := len(e.margin); n > crossEventsKept {
		e.margin = append([]crossEventLine(nil), e.margin[n-crossEventsKept:]...)
	}
	e.mu.Unlock()
}

func (e *crossEvents) recent() []crossEventLine {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]crossEventLine(nil), e.lines...)
}

func (e *crossEvents) recentMargin() []crossEventLine {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]crossEventLine(nil), e.margin...)
}
