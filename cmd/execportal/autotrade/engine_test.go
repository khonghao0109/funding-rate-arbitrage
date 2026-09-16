package autotrade

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/broker/brokertest"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/execution"
	"futures-arbitrage-scanner/internal/risk"
)

// The machine, end to end, against the REAL execution machine on brokertest's
// in-memory venues — one spot and one perp venue PER SYMBOL, so several pairs
// are held at once exactly as the testnet holds them. The Trader below is a
// test double of the portal's order path, but everything under it is
// production code: execution.Open places two legs and unwinds one when the
// other fails, execution.Close proves both flat. Every assertion about a
// position reads the FAKE VENUES, never the engine's own belief — a machine
// that has lost a leg reports itself hedged with complete confidence (the 4.4a
// lesson).

const (
	testSymbol = "BTCUSDT"
	testMid    = 77_000.0
	// testPerpMid is the perp trading ABOVE the spot by testEntryBasisBps: the
	// shipped Config.MinEntryBasisBps refuses an entry on a flat or inverted
	// basis, so a fixture that stands for "a reading every entry check passes"
	// has to carry one. Every place a normal reading is built uses it — the two
	// books, the fill prices the fake venue reports and the mids an intent
	// records — so an opened position's entry basis is this number too.
	testEntryBasisBps = 6.0
	testPerpMid       = testMid * (1 + testEntryBasisBps/10_000)
	// testNotional is one leg, and testCapital what the pair ties up at the
	// rig's 0.5 perp margin. It clears goodSnapshot's size floor of 154 quote —
	// the venue's step size at testMid against the 5% quantization tolerance —
	// which the old 65 does not.
	testNotional = 200.0
	testCapital  = testNotional * 1.5
	perpStep     = 0.0001
	spotStep     = 0.00001
)

// allSymbols is the portal's allow-list in these tests.
var allSymbols = []string{"BTCUSDT", "ETHUSDT", "SOLUSDT", "BNBUSDT"}

func spotRules(symbol string) exchanges.Instrument {
	return exchanges.Instrument{Symbol: symbol, Source: "binance_spot", MarketType: "spot", Status: exchanges.StatusTrading,
		TickSizeQuote: 0.01, StepSizeCoin: spotStep, MinQtyCoin: spotStep, MaxQtyCoin: 9000, MinNotionalQuote: 5,
		ContractSizeCoin: 1, BaseAsset: "BTC", QuoteAsset: "USDT"}
}

func perpRules(symbol string) exchanges.Instrument {
	return exchanges.Instrument{Symbol: symbol, Source: "binance_futures", MarketType: "perp", Status: exchanges.StatusTrading,
		TickSizeQuote: 0.10, StepSizeCoin: perpStep, MinQtyCoin: perpStep, MaxQtyCoin: 1000, MinNotionalQuote: 50,
		ContractSizeCoin: 1, BaseAsset: "BTC", QuoteAsset: "USDT"}
}

func book(source string, mid float64, at time.Time) depth.Summary {
	return depth.Summary{Source: source, Symbol: testSymbol, SampledAtMs: at.UnixMilli(),
		MidPriceQuote: mid, BestBidQuote: mid * 0.99995, BestAskQuote: mid * 1.00005, SpreadPct: 0.01,
		BidDepthWithinTightQuote: 400_000, AskDepthWithinTightQuote: 400_000,
		BidDepthWithinWideQuote: 3_000_000, AskDepthWithinWideQuote: 3_000_000,
		BidLevels: 100, AskLevels: 100, BidSpanPct: 0.8, AskSpanPct: 0.8}
}

// ------------------------------------------------------------------ venues

type intentRecord struct {
	id               string
	symbol           string
	openedAtMs       int64
	notional         float64
	spotMid, perpMid float64
	spotAvg, perpAvg float64
	fromAutotrade    bool
}

// symVenue is one symbol's two markets.
type symVenue struct {
	spot, perp *brokertest.Fake
}

func newSymVenue() *symVenue {
	spot, perp := brokertest.New(), brokertest.New()
	spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1})
	perp.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1})
	spot.SetStepSizeCoin(spotStep)
	perp.SetStepSizeCoin(perpStep)
	spot.SetBaseAsset("BTC")
	spot.SetBalance(broker.MarketSpot, broker.Balance{Market: broker.MarketSpot, Asset: "BTC", FreeQtyCoin: 1},
		broker.Balance{Market: broker.MarketSpot, Asset: "USDT", FreeQtyCoin: 10_000})
	perp.SetMarkPrice(broker.MarkPrice{MarkPriceQuote: testPerpMid})
	return &symVenue{spot: spot, perp: perp}
}

// venueTrader is the portal's order path in miniature: execution over two fakes
// per symbol, hedge status read back from the venues by each intent's derived
// order ids. Holding is safe for several symbols at once; the engine never
// calls Open or Close concurrently, and inFlight asserts it.
type venueTrader struct {
	t      *testing.T
	venues map[string]*symVenue
	// spot and perp are BTCUSDT's, for the tests about one pair.
	spot, perp *brokertest.Fake

	mu      sync.Mutex
	intents []*intentRecord
	seq     int
	opens   int
	closes  int
	opened  []string
	// closeReasons is every reason the engine handed Close, in order. In the
	// binary that string becomes the intent file's close_reason_vi, so a test
	// that reads it here is reading what the history table will show.
	closeReasons []string

	// The two wallets the rebalance sizes on, and how many times it asked.
	account      Account
	accountErr   error
	accountReads int
	inFlight     int
	busy         bool
	holdErr      error
	refuseOpen   bool
	refuseClose  bool
	alarmOpen    bool          // the open returns an ALARM after placing
	openGate     chan struct{} // when set, Open waits on it (or on ctx) before placing
	closeGate    chan struct{} // when set, Close waits on it before closing
	afterOpen    func(symbol string)
}

func newVenueTrader(t *testing.T) *venueTrader {
	v := &venueTrader{t: t, venues: map[string]*symVenue{},
		account: Account{QuoteAsset: "USDT", SpotQuoteTotal: 10_000, FuturesQuoteTotal: 5_000, ReadAtMs: time.Now().UnixMilli()}}
	for _, s := range allSymbols {
		v.venues[s] = newSymVenue()
	}
	v.spot, v.perp = v.venues[testSymbol].spot, v.venues[testSymbol].perp
	return v
}

// closeCount is how many closes were asked for, read under the lock — the kill
// that asks runs on another goroutine.
func (v *venueTrader) closeCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.closes
}

func (v *venueTrader) openCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.opens
}

func (v *venueTrader) orders() int {
	n := 0
	for _, sv := range v.venues {
		n += len(sv.spot.Orders()) + len(sv.perp.Orders())
	}
	return n
}

// legNet is one leg's signed filled quantity over every id an intent derives.
func legNet(ctx context.Context, b broker.Broker, market broker.Market, symbol, id string, leg execution.LegName) (float64, error) {
	net := 0.0
	for _, cid := range []string{execution.LegClientOrderID(id, leg), execution.CloseClientOrderID(id, leg),
		execution.UnwindClientOrderID(id, leg), execution.ReconcileClientOrderID(id, leg)} {
		o, err := b.GetOrder(ctx, broker.OrderQuery{Market: market, Symbol: symbol, ClientOrderID: cid})
		if errors.Is(err, broker.ErrOrderNotFound) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if o.Side == broker.SideSell {
			net -= o.FilledQtyCoin
		} else {
			net += o.FilledQtyCoin
		}
	}
	return net, nil
}

func (v *venueTrader) intentsOf(symbol string) []*intentRecord {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []*intentRecord
	for _, in := range v.intents {
		if in.symbol == symbol {
			out = append(out, in)
		}
	}
	return out
}

// venueLegs is what one symbol's two venues hold for the strategy: the spot leg
// summed from every intent's own orders, and the venue's perp position.
func (v *venueTrader) venueLegs(t *testing.T, symbol string) (spotQty, perpQty float64) {
	t.Helper()
	ctx := context.Background()
	sv := v.venues[symbol]
	for _, in := range v.intentsOf(symbol) {
		s, err := legNet(ctx, sv.spot, broker.MarketSpot, symbol, in.id, execution.LegSpot)
		if err != nil {
			t.Fatal(err)
		}
		spotQty += s
	}
	pos, err := sv.perp.GetPosition(ctx, broker.MarketFuturesUSDM, symbol)
	if err != nil {
		t.Fatal(err)
	}
	return spotQty, pos.QtyCoin
}

func (v *venueTrader) Holding(ctx context.Context, symbol string) (Holding, error) {
	v.mu.Lock()
	err := v.holdErr
	v.mu.Unlock()
	if err != nil {
		return Holding{}, err
	}
	sv := v.venues[symbol]
	pos, err := sv.perp.GetPosition(ctx, broker.MarketFuturesUSDM, symbol)
	if err != nil {
		return Holding{}, err
	}
	h := Holding{ToleranceQtyCoin: perpStep}
	var spotSum, perpSum float64
	var held *intentRecord
	for _, in := range v.intentsOf(symbol) {
		s, err := legNet(ctx, sv.spot, broker.MarketSpot, symbol, in.id, execution.LegSpot)
		if err != nil {
			return Holding{}, err
		}
		p, err := legNet(ctx, sv.perp, broker.MarketFuturesUSDM, symbol, in.id, execution.LegPerp)
		if err != nil {
			return Holding{}, err
		}
		spotSum += s
		perpSum += p
		if math.Abs(s) > perpStep+1e-9 || math.Abs(p) > perpStep+1e-9 {
			h.HeldIntents++
			held = in
		}
	}
	tol := perpStep + 1e-9
	h.ResidualQtyCoin = spotSum + pos.QtyCoin
	switch {
	case math.Abs(pos.QtyCoin-perpSum) > tol:
		h.Status, h.ReasonVI = HedgeEvidenceConflict, fmt.Sprintf("sàn %v, ý định %v", pos.QtyCoin, perpSum)
	case math.Abs(spotSum) <= tol && math.Abs(pos.QtyCoin) <= tol:
		h.Status, h.ReasonVI = HedgeBothFlat, "phẳng"
	case math.Abs(spotSum+pos.QtyCoin) <= tol && spotSum > tol && pos.QtyCoin < -tol:
		h.Status, h.ReasonVI = HedgeBothOpen, "hedged"
	default:
		h.Status, h.ReasonVI = HedgeUnhedged, fmt.Sprintf("spot %v perp %v", spotSum, pos.QtyCoin)
	}
	if h.HeldIntents == 1 {
		v.mu.Lock()
		h.IntentID, h.FromAutotrade = held.id, held.fromAutotrade
		h.OpenedAtMs, h.SpotRefMidQuote, h.PerpRefMidQuote, h.QtyCoin = held.openedAtMs, held.spotMid, held.perpMid, -pos.QtyCoin
		h.NotionalQuote, h.SpotAvgFillQuote, h.PerpAvgFillQuote = held.notional, held.spotAvg, held.perpAvg
		v.mu.Unlock()
	}
	return h, nil
}

func (v *venueTrader) intent(symbol, id string, at time.Time) execution.Intent {
	return execution.Intent{
		ID: id, Symbol: symbol, SpotInstrument: spotRules(symbol), PerpInstrument: perpRules(symbol),
		SpotBook: book("binance_spot", testMid, at), PerpBook: book("binance_futures", testPerpMid, at),
		SpotPriceQuote: testMid, PerpPriceQuote: testPerpMid, PerpMarginFrac: 0.5,
		PerpBracket: risk.Bracket{Source: "binance_futures", MaintenanceMarginFrac: 0.004, TierCeilingQuote: 50_000, MaxLeverage: 125, Verified: true},
	}
}

func (v *venueTrader) enter() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.inFlight++
	if v.inFlight > 1 {
		v.t.Error("two orders were in flight at once — the engine must send one at a time")
	}
}

func (v *venueTrader) leave() {
	v.mu.Lock()
	v.inFlight--
	v.mu.Unlock()
}

func (v *venueTrader) Open(ctx context.Context, order OpenOrder) OpenResult {
	v.mu.Lock()
	if v.busy {
		v.mu.Unlock()
		return OpenResult{Busy: true, ErrorVI: "bận"}
	}
	gate, refuse, alarm, hook := v.openGate, v.refuseOpen, v.alarmOpen, v.afterOpen
	v.opens++
	v.seq++
	v.opened = append(v.opened, order.Symbol)
	id := fmt.Sprintf("a%s-test-%03d", strings.ToLower(order.Symbol), v.seq)
	v.mu.Unlock()
	v.enter()
	defer v.leave()
	if hook != nil {
		defer hook(order.Symbol)
	}
	if refuse {
		return OpenResult{IntentID: id, Refused: true, ErrorVI: "từ chối trước khi gửi (test)"}
	}

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}
	if alarm {
		return OpenResult{IntentID: id, Alarm: true, ErrorVI: "UNWIND INCOMPLETE (test)"}
	}
	sv := v.venues[order.Symbol]
	now := time.Now()
	in := v.intent(order.Symbol, id, now)
	in.NotionalQuote = order.NotionalQuote
	in.SignalEntryCostPct = order.SignalEntryCostPct
	cfg := execution.DefaultConfig()
	cfg.LegTimeout = 2 * time.Second
	tr, err := execution.NewOpener(sv.spot, sv.perp, cfg, execution.NewMemoryRecorder())
	if err != nil {
		v.t.Fatal(err)
	}
	res, openErr := tr.Open(ctx, in)
	rec := &intentRecord{id: id, symbol: order.Symbol, openedAtMs: now.UnixMilli(), notional: order.NotionalQuote, spotMid: testMid, perpMid: testPerpMid,
		spotAvg: res.Spot.AvgFillPriceQuote, perpAvg: res.Perp.AvgFillPriceQuote, fromAutotrade: true}
	v.mu.Lock()
	v.intents = append(v.intents, rec)
	v.mu.Unlock()
	out := OpenResult{IntentID: id, Hedged: res.Hedged(), OpenedAtMs: rec.openedAtMs, QtyCoin: res.Perp.FilledQtyCoin,
		ResidualQtyCoin: res.ResidualQtyCoin, SpotRefMidQuote: testMid, PerpRefMidQuote: testPerpMid,
		SpotAvgFillQuote: res.Spot.AvgFillPriceQuote, PerpAvgFillQuote: res.Perp.AvgFillPriceQuote,
		UnhedgedWindowMs: res.UnhedgedWindow.Milliseconds(), UnwindDurationMs: res.UnwindDuration.Milliseconds(),
		Refused: errors.Is(openErr, execution.ErrRefusedBeforePlacing),
		Alarm:   errors.Is(openErr, execution.ErrUnwindIncomplete) || errors.Is(openErr, execution.ErrFlatEvidenceConflict)}
	if openErr != nil {
		out.ErrorVI = openErr.Error()
	}
	return out
}

// Account answers the two wallets. The shipped fixture is deliberately big
// enough that the sizing tests are about the ARITHMETIC and not about a wallet
// running dry; a test that wants a dry wallet sets it.
func (v *venueTrader) Account(context.Context) (Account, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.accountReads++
	return v.account, v.accountErr
}

func (v *venueTrader) setAccount(spotQuote, futuresQuote float64, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.account, v.accountErr = Account{QuoteAsset: "USDT", SpotQuoteTotal: spotQuote, FuturesQuoteTotal: futuresQuote, ReadAtMs: time.Now().UnixMilli()}, err
}

func (v *venueTrader) reads() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.accountReads
}

func (v *venueTrader) Close(ctx context.Context, symbol, intentID, reasonVI string) CloseResult {
	v.mu.Lock()
	if v.busy {
		v.mu.Unlock()
		return CloseResult{Busy: true}
	}
	v.closes++
	v.closeReasons = append(v.closeReasons, reasonVI)
	gate, refuse := v.closeGate, v.refuseClose
	var rec *intentRecord
	for _, in := range v.intents {
		if in.id == intentID {
			rec = in
		}
	}
	v.mu.Unlock()
	v.enter()
	defer v.leave()
	if gate != nil {
		<-gate
	}
	if refuse {
		return CloseResult{IntentID: intentID, Refused: true, ErrorVI: "từ chối trước khi gửi (test)"}
	}
	if rec == nil || rec.symbol != symbol {
		return CloseResult{Refused: true, ErrorVI: "không có ý định " + intentID + " trên " + symbol}
	}
	sv := v.venues[symbol]
	pos, err := sv.perp.GetPosition(ctx, broker.MarketFuturesUSDM, symbol)
	if err != nil {
		return CloseResult{Refused: true, ErrorVI: err.Error()}
	}
	tr, err := execution.NewOpener(sv.spot, sv.perp, execution.DefaultConfig(), execution.NewMemoryRecorder())
	if err != nil {
		v.t.Fatal(err)
	}
	res, closeErr := tr.Close(ctx, execution.CloseRequest{
		Intent: v.intent(symbol, intentID, time.Now()), QtyCoin: -pos.QtyCoin, OpenedAtMs: rec.openedAtMs,
		EntrySpotAvgPriceQuote: rec.spotAvg, EntryPerpAvgPriceQuote: rec.perpAvg,
		EntrySpotRefMidQuote: rec.spotMid, EntryPerpRefMidQuote: rec.perpMid,
	})
	out := CloseResult{IntentID: intentID, Flat: res.Outcome == execution.OutcomeBothFlat && closeErr == nil,
		ClosedQtyCoin: res.ClosedQtyCoin, RemainingQtyCoin: res.RemainingQtyCoin, RealizedQuote: res.RealizedQuote,
		Refused: errors.Is(closeErr, execution.ErrCloseRefused),
		Alarm:   errors.Is(closeErr, execution.ErrUnwindIncomplete) || errors.Is(closeErr, execution.ErrFlatEvidenceConflict)}
	if closeErr != nil {
		out.ErrorVI = closeErr.Error()
	}
	return out
}

// ------------------------------------------------------------------ market

// scriptMarket answers a scan with the snapshot the test last set for the
// symbol. Safe for several symbols at once.
type scriptMarket struct {
	mu    sync.Mutex
	snaps map[string]*Snapshot
	err   error
	calls int
	since []int64
	// symbols is every symbol whose market was read, in order.
	symbols []string
	// afterRead, when set, runs once the reading is taken and before the
	// engine sees it — the window between a scan and its decision.
	afterRead func()
}

func (m *scriptMarket) set(fn func(s *Snapshot)) { m.setFor(testSymbol, fn) }

func (m *scriptMarket) setFor(symbol string, fn func(s *Snapshot)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(m.snaps[symbol])
}

func (m *scriptMarket) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *scriptMarket) Snapshot(ctx context.Context, symbol string, settledSinceMs int64) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	m.mu.Lock()
	hook := m.afterRead
	m.afterRead = nil
	m.mu.Unlock()
	if hook != nil {
		defer hook()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.since = append(m.since, settledSinceMs)
	m.symbols = append(m.symbols, symbol)
	if m.err != nil {
		return Snapshot{}, m.err
	}
	now := time.Now()
	src := m.snaps[symbol]
	s := *src
	s.Symbol, s.ReadAtMs = symbol, now.UnixMilli()
	s.SpotBook.SampledAtMs, s.PerpBook.SampledAtMs = now.UnixMilli(), now.UnixMilli()
	s.Settled = nil
	if s.SettledErrVI != "" {
		return s, nil // an unreadable history carries no rows, as portalMarket's does
	}
	for _, r := range src.Settled {
		if r.SettledAtMs >= settledSinceMs {
			s.Settled = append(s.Settled, r)
		}
	}
	return s, nil
}

// settledEvery8h is n settlements ending one hour ago, each at rate.
func settledEvery8h(n int, rate float64, now time.Time) []SettledRate {
	const interval = 8 * time.Hour
	last := now.Add(-time.Hour)
	out := make([]SettledRate, n)
	for i := range out {
		at := last.Add(-time.Duration(n-1-i) * interval)
		// The testnet's own jitter: a millisecond past the hour on some stamps.
		out[i] = SettledRate{SettledAtMs: at.UnixMilli() + int64(i%2), RatePerIntervalFrac: rate}
	}
	return out
}

// goodSnapshot is a reading every entry check passes: rate per 8h settled for a
// week, the testnet's fees, deep books, the next settlement seven hours away.
func goodSnapshot(now time.Time, rate float64) *Snapshot {
	return &Snapshot{
		SpotBook: book("binance_spot", testMid, now), PerpBook: book("binance_futures", testPerpMid, now),
		// The next stamp is one 8h interval after the last settled one, as the
		// venue publishes it — the cadence cross-check refuses anything else.
		ForecastRatePerIntervalFrac: rate, NextFundingTimeMs: now.Add(7 * time.Hour).UnixMilli(),
		Settled:         settledEvery8h(21, rate, now),
		SpotTakerFeeBps: 0, PerpTakerFeeBps: 4, FeeSourceVI: "test",
		SpotClockSkewMs: ptrInt(220), PerpClockSkewMs: ptrInt(-140),
		// The stricter of the two markets' rules, as the testnet publishes them
		// for BTCUSDT: futures steps 0.0001 and wants 50 quote, spot steps
		// 0.00001 and wants 5. At testMid that puts the size floor at 154 quote
		// (7.70 a step ÷ the 5% quantization tolerance), which is why the test
		// runs are sized at testNotional and not at the old 65.
		StepSizeCoin: perpStep, MinQtyCoin: perpStep, MinNotionalQuote: 50,
	}
}

// goodMarket answers every symbol with 1 bps per 8h.
func goodMarket(now time.Time) *scriptMarket {
	m := &scriptMarket{snaps: map[string]*Snapshot{}}
	for _, s := range allSymbols {
		m.snaps[s] = goodSnapshot(now, 0.0001)
	}
	return m
}

func ptrInt(v int64) *int64 { return &v }

// ------------------------------------------------------------------ helpers

type rig struct {
	t      *testing.T
	eng    *Engine
	trader *venueTrader
	market *scriptMarket
}

func newRig(t *testing.T) *rig {
	return newRigAt(t, time.Now)
}

func newRigAt(t *testing.T, now func() time.Time) *rig {
	t.Helper()
	tr := newVenueTrader(t)
	m := goodMarket(time.Now())
	eng, err := New(Options{Market: m, Trader: tr, Symbols: allSymbols, MaxNotionalQuote: 50_000,
		PerpMarginFrac: 0.5, ActionTimeout: 30 * time.Second, Logf: t.Logf, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, eng: eng, trader: tr, market: m}
}

// testPortfolio runs the shipped parameters on the given symbols; a test steps
// the machine by hand, so the cooldown is asserted, never waited out.
func testPortfolio(symbols ...string) PortfolioConfig {
	pc := DefaultPortfolioConfig(symbols)
	pc.DefaultPairConfig.Cooldown = time.Hour
	pc.DefaultPairConfig.MinHoldEpochs = 0
	// A FIXED size, so every test about slots, capital and adoption asserts on
	// the notional it configured. The rebalance is on by default in the shipped
	// run and has its own tests, which switch it back on explicitly.
	pc.AutoRebalance = false
	pc.DefaultPairConfig.NotionalQuote = testNotional
	return pc
}

// testConfig is a run on BTCUSDT alone.
func testConfig() PortfolioConfig { return testPortfolio(testSymbol) }

func (r *rig) start(pc PortfolioConfig) {
	r.t.Helper()
	if _, err := r.eng.Start(pc); err != nil {
		r.t.Fatalf("Start: %v", err)
	}
}

func (r *rig) step() StatusView {
	r.eng.Step(context.Background())
	return r.eng.Status()
}

func pairOf(t *testing.T, st StatusView, symbol string) PairView {
	t.Helper()
	for _, p := range st.Pairs {
		if p.Symbol == symbol {
			return p
		}
	}
	t.Fatalf("no pair %s in the status", symbol)
	return PairView{}
}

// wantBot asserts the PORTFOLIO's state.
func (r *rig) wantBot(want State) StatusView {
	r.t.Helper()
	st := r.eng.Status()
	if st.State != want {
		r.t.Fatalf("bot state = %s (%s), want %s · halt %q · log %v", st.State, st.StateVI, want, st.HaltReasonVI, logLines(st))
	}
	return st
}

// wantPair asserts one pair's state.
func (r *rig) wantPair(symbol string, want State) PairView {
	r.t.Helper()
	st := r.eng.Status()
	p := pairOf(r.t, st, symbol)
	if p.State != want {
		r.t.Fatalf("%s state = %s (%s), want %s · pair halt %q · bot %s %q · log %v", symbol, p.State, p.StateVI, want, p.HaltReasonVI, st.State, st.HaltReasonVI, logLines(st))
	}
	return p
}

// wantState is wantPair on BTCUSDT.
func (r *rig) wantState(want State) PairView {
	r.t.Helper()
	return r.wantPair(testSymbol, want)
}

func (r *rig) wantHedgedOn(symbol string) float64 {
	r.t.Helper()
	spotQty, perpQty := r.trader.venueLegs(r.t, symbol)
	if !(spotQty > 0) || !(perpQty < 0) || math.Abs(spotQty+perpQty) > perpStep+1e-9 {
		r.t.Fatalf("%s venues hold spot %v perp %v — not a hedged pair", symbol, spotQty, perpQty)
	}
	return spotQty
}

func (r *rig) wantFlatOn(symbol string) {
	r.t.Helper()
	spotQty, perpQty := r.trader.venueLegs(r.t, symbol)
	if math.Abs(spotQty) > 1e-12 || math.Abs(perpQty) > 1e-12 {
		r.t.Fatalf("%s venues hold spot %v perp %v — not flat", symbol, spotQty, perpQty)
	}
}

func (r *rig) wantVenueHedged() float64 { r.t.Helper(); return r.wantHedgedOn(testSymbol) }
func (r *rig) wantVenueFlat()           { r.t.Helper(); r.wantFlatOn(testSymbol) }

// settleAfterTheOpen lists one settlement on BTCUSDT stamped strictly after the
// open — a stamp in the open's own millisecond is not "after" it.
func settleAfterTheOpen(r *rig, rate float64) { settleAfterTheOpenOn(r, testSymbol, rate) }

func settleAfterTheOpenOn(r *rig, symbol string, rate float64) {
	time.Sleep(2 * time.Millisecond)
	r.market.setFor(symbol, func(s *Snapshot) {
		s.Settled = append(s.Settled, SettledRate{SettledAtMs: time.Now().UnixMilli(), RatePerIntervalFrac: rate})
	})
}

func logLines(st StatusView) []string {
	var out []string
	for _, e := range st.Log {
		out = append(out, "["+e.Kind+" "+e.Symbol+"] "+e.MessageVI)
	}
	return out
}

func hasLog(st StatusView, kind, fragment string) bool {
	for _, e := range st.Log {
		if e.Kind == kind && strings.Contains(e.MessageVI, fragment) {
			return true
		}
	}
	return false
}

func hasLogOn(st StatusView, kind, symbol, fragment string) bool {
	for _, e := range st.Log {
		if e.Kind == kind && e.Symbol == symbol && strings.Contains(e.MessageVI, fragment) {
			return true
		}
	}
	return false
}

// waitFor polls a condition for up to five seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// -------------------------------------------------------------- one pair

// The acceptance path of Q18, on fakes: Disabled → Start → a good signal → both
// legs open and hedged at the venue → a settlement pays negative → both legs
// flat at the venue → cooldown.
func TestEngine_OpensHedgedOnASignalAndClosesFlatOnAnExit(t *testing.T) {
	r := newRig(t)
	r.wantBot(StateDisabled)
	if n := r.step(); n.State != StateDisabled || r.market.callCount() != 0 {
		t.Fatalf("a disabled bot scanned: %s, %d reads", n.State, r.market.callCount())
	}

	r.start(testConfig())
	r.wantBot(StateRunning)
	r.wantState(StateIdleScanning)
	st := r.step()
	p := r.wantState(StateInPosition)
	qty := r.wantVenueHedged()
	if p.Position == nil || !strings.HasPrefix(p.Position.IntentID, "abtcusdt-") || math.Abs(p.Position.QtyCoin-qty) > 1e-12 {
		t.Fatalf("position = %+v, venue spot %v", p.Position, qty)
	}
	if p.Signal == nil || !p.Signal.EntryEligible || p.Signal.NetAPRPct == nil || *p.Signal.NetAPRPct < 5 {
		t.Fatalf("signal = %+v", p.Signal)
	}
	if p.Position.NotionalQuote != testNotional || p.Position.CapitalQuote != testCapital || p.Position.SpotEntryAvgQuote <= 0 || p.Position.PerpEntryAvgQuote <= 0 {
		t.Errorf("position sizing = %+v", p.Position)
	}
	if st.OpenPositions != 1 || st.SlotsUsed != 1 || st.CapitalDeployedQuote != testCapital || len(st.Positions) != 1 {
		t.Errorf("portfolio counts: open %d slots %d capital %v positions %d", st.OpenPositions, st.SlotsUsed, st.CapitalDeployedQuote, len(st.Positions))
	}
	if !hasLogOn(st, "OPEN", testSymbol, "HEDGED") {
		t.Errorf("no OPEN line: %v", logLines(st))
	}

	// Holding while nothing says leave: no order, and the pair marked to mid.
	opens, closes := r.trader.opens, r.trader.closes
	st = r.step()
	p = r.wantState(StateInPosition)
	if r.trader.opens != opens || r.trader.closes != closes {
		t.Errorf("a quiet hold sent orders: opens %d→%d closes %d→%d", opens, r.trader.opens, closes, r.trader.closes)
	}
	if p.Signal == nil || p.Signal.ExitDue || len(p.Signal.ExitChecks) != 4 {
		t.Errorf("hold signal = %+v", p.Signal)
	}
	// The held pair's running result is priced on every scan, whether or not it
	// reaches the take-profit: a page that cannot say how far a position is
	// from its target cannot be read.
	if p.Signal.HoldingCashResultQuote == nil || p.Signal.HoldingReturnOnCapitalPct == nil ||
		p.Signal.TakeProfitTargetPct != DefaultTargetTakeProfitNetPct || p.Signal.HoldingResultLabelVI == "" {
		t.Errorf("running result = %+v (%s)", p.Signal.HoldingCashResultQuote, p.Signal.HoldingResultReasonVI)
	}
	if p.Position.PairDriftQuote == nil || p.Position.MarkedAtMs == 0 || st.UnrealizedDriftPriced != 1 {
		t.Errorf("mark to mid = %+v, priced %d", p.Position, st.UnrealizedDriftPriced)
	}

	// A settlement after the open pays negative.
	settleAfterTheOpen(r, -0.00005)
	st = r.step()
	p = r.wantState(StateCooldown)
	r.wantVenueFlat()
	if p.Position != nil || !hasLog(st, "EXIT", "≤ 0") || !hasLog(st, "CLOSE", "PHẲNG") {
		t.Errorf("after exit: position %+v · log %v", p.Position, logLines(st))
	}
	if p.CooldownUntilMs <= st.NowMs {
		t.Errorf("cooldown until %d, now %d", p.CooldownUntilMs, st.NowMs)
	}
	// Cooling down: nothing is read, nothing is sent.
	calls := r.market.callCount()
	r.step()
	if r.market.callCount() != calls || r.trader.opens != opens {
		t.Error("a cooling-down pair scanned or traded")
	}
}

// Leg 2 refused at the venue: execution unwinds leg 1, the pair ends flat at the
// venue and cools down — never holding a naked leg.
func TestEngine_ALeg2RefusalUnwindsToFlatAndCoolsDown(t *testing.T) {
	r := newRig(t)
	r.trader.perp.SetBehaviour(brokertest.Behaviour{RejectWith: &broker.HTTPError{StatusCode: 400, URL: "https://demo-fapi.binance.com/fapi/v1/order", Body: `{"code":-2019}`}})
	r.start(testConfig())
	st := r.step()
	p := r.wantState(StateCooldown)
	r.wantVenueFlat()
	if len(r.trader.spot.Orders()) == 0 {
		t.Fatal("leg 1 was never placed, so this did not test an unwind")
	}
	if p.Position != nil || p.TradeFailures != 1 || !hasLog(st, "OPEN", "PHẲNG") {
		t.Errorf("after a failed open: position %+v, trade failures %d, log %v", p.Position, p.TradeFailures, logLines(st))
	}
}

// Three failed opens in a row halt the PAIR; a successful scan between them does
// not reset that count, and the bot keeps running.
func TestEngine_ConsecutiveFailedOpensHaltThePair(t *testing.T) {
	r := newRig(t)
	r.trader.perp.SetBehaviour(brokertest.Behaviour{RejectWith: &broker.HTTPError{StatusCode: 400, URL: "https://demo-fapi.binance.com/fapi/v1/order"}})
	cfg := testConfig()
	cfg.DefaultPairConfig.Cooldown = 0
	r.start(cfg)
	for i := 0; i < 5; i++ {
		r.step()
	}
	p := r.wantState(StateEmergencyHalted)
	r.wantBot(StateRunning)
	r.wantVenueFlat()
	if p.TradeFailures != 5 || !strings.Contains(p.HaltReasonVI, "liên tiếp") || p.Radar != RadarHalted {
		t.Errorf("halt = %q after %d failures, radar %s", p.HaltReasonVI, p.TradeFailures, p.Radar)
	}
	opens := r.trader.opens
	r.step()
	if r.trader.opens != opens {
		t.Error("a halted pair opened again")
	}
}

// KILL while holding: both legs flat at the venue at once, and the bot halted.
func TestEngine_KillWhileHoldingClosesBothLegsAndHalts(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	r.wantState(StateInPosition)
	r.wantVenueHedged()

	st, closes, err := r.eng.Kill(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r.wantBot(StateEmergencyHalted)
	r.wantVenueFlat()
	var btc *OperatorClose
	for i := range closes {
		if closes[i].Symbol == testSymbol {
			btc = &closes[i]
		}
	}
	if len(closes) != len(allSymbols) || btc == nil || !btc.Attempted || !btc.Flat || len(st.Positions) != 0 || !strings.Contains(st.HaltReasonVI, "KILL SWITCH") {
		t.Fatalf("kill = %+v · status positions %+v halt %q", closes, st.Positions, st.HaltReasonVI)
	}
	// Halted means halted: no scan trades, and a start is refused until the
	// operator acknowledges with a stop.
	opens := r.trader.opens
	r.step()
	if r.trader.opens != opens {
		t.Error("a killed bot opened again")
	}
	if _, err := r.eng.Start(testConfig()); !errors.Is(err, ErrNotStartable) {
		t.Errorf("start while halted = %v", err)
	}
	if _, _, err := r.eng.Stop(context.Background(), false, -1); err != nil {
		t.Fatal(err)
	}
	r.wantBot(StateDisabled)
	r.start(testConfig())
	r.wantState(StateIdleScanning)
}

// A kill that lands while an open is in flight does NOT cancel it — a cancelled
// order may be on the wire and invisible to the lookup that would resolve it.
// It waits for the open to RETURN, then closes what the venue holds: both flat.
func TestEngine_KillDuringAnOpenWaitsForItThenClosesIt(t *testing.T) {
	r := newRig(t)
	gate := make(chan struct{})
	r.trader.openGate = gate
	r.start(testConfig())

	stepped := make(chan struct{})
	go func() {
		r.eng.Step(context.Background())
		close(stepped)
	}()
	waitFor(t, func() bool { return pairOf(t, r.eng.Status(), testSymbol).State == StateOpening })

	killed := make(chan []OperatorClose)
	go func() {
		_, oc, _ := r.eng.Kill(context.Background())
		killed <- oc
	}()
	waitFor(t, func() bool { return r.eng.Status().Busy == "kill" })
	select {
	case <-killed:
		t.Fatal("the kill returned while the open it must wait for was still running")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := r.eng.Start(testConfig()); err == nil {
		t.Error("a start was accepted while a kill was running")
	}

	close(gate) // the open completes: both legs are placed
	<-stepped
	closes := <-killed
	r.wantBot(StateEmergencyHalted)
	r.wantVenueFlat()
	if len(closes) == 0 || !closes[0].Attempted || !closes[0].Flat {
		t.Errorf("kill after the open returned = %+v", closes)
	}
}

// A decision already made when the operator presses DỪNG, KILL or the portal
// shuts down is dropped: the books were read, the bot would have opened, and
// nothing is sent (review of Q18: a stop let a decided open through).
func TestEngine_AnInterruptionBetweenTheReadAndTheDecisionSendsNothing(t *testing.T) {
	for name, interrupt := range map[string]func(r *rig, cancel context.CancelFunc){
		"stop": func(r *rig, _ context.CancelFunc) {
			go func() { _, _, _ = r.eng.Stop(context.Background(), false, -1) }()
			waitFor(r.t, func() bool { return r.eng.Status().Busy == "stop" })
		},
		"kill": func(r *rig, _ context.CancelFunc) {
			go func() { _, _, _ = r.eng.Kill(context.Background()) }()
			waitFor(r.t, func() bool { return r.eng.Status().Busy == "kill" })
		},
		"shutdown": func(_ *rig, cancel context.CancelFunc) { cancel() },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.start(testConfig())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.market.afterRead = func() { interrupt(r, cancel) }
			r.eng.Step(ctx)
			waitFor(t, func() bool { return r.eng.Status().Busy == "" })
			if r.trader.openCount() != 0 || r.trader.orders() != 0 {
				t.Errorf("%s between the read and the decision: %d opens, %d orders", name, r.trader.openCount(), r.trader.orders())
			}
			if p := pairOf(t, r.eng.Status(), testSymbol); p.State == StateInPosition || p.State == StateOpening {
				t.Errorf("state after %s = %s", name, p.State)
			}
		})
	}
}

// The same for a CLOSE: an exit is due, the books were read, and DỪNG (keep the
// position) lands before the decision — the pair is kept, nothing is sent.
func TestEngine_AStopBetweenTheReadAndADueExitKeepsThePair(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	r.wantState(StateInPosition)
	settleAfterTheOpen(r, -0.00005) // the exit is due on the next scan
	r.market.afterRead = func() {
		go func() { _, _, _ = r.eng.Stop(context.Background(), false, -1) }()
		waitFor(t, func() bool { return r.eng.Status().Busy == "stop" })
	}
	r.eng.Step(context.Background())
	waitFor(t, func() bool { return r.eng.Status().Busy == "" })
	if r.trader.closeCount() != 0 {
		t.Errorf("a stop that keeps the position let a due exit close it (%d closes)", r.trader.closeCount())
	}
	r.wantBot(StateDisabled)
	r.wantVenueHedged()
}

// A stop waiting for an open that returns an ALARM must not acknowledge the halt
// that alarm raises: the stop is refused, the pair's halt and its reason stay.
func TestEngine_AStopDoesNotAcknowledgeAHaltRaisedWhileItWaited(t *testing.T) {
	r := newRig(t)
	gate := make(chan struct{})
	r.trader.openGate, r.trader.alarmOpen = gate, true
	r.start(testConfig())
	stepped := make(chan struct{})
	go func() {
		r.eng.Step(context.Background())
		close(stepped)
	}()
	waitFor(t, func() bool { return pairOf(t, r.eng.Status(), testSymbol).State == StateOpening })
	stopErr := make(chan error, 1)
	go func() {
		_, _, err := r.eng.Stop(context.Background(), false, -1)
		stopErr <- err
	}()
	waitFor(t, func() bool { return r.eng.Status().Busy == "stop" })
	close(gate)
	<-stepped
	if err := <-stopErr; !errors.Is(err, ErrHaltedWhileStopping) || !strings.Contains(err.Error(), "BÁO ĐỘNG") {
		t.Errorf("a stop through an alarm = %v", err)
	}
	p := r.wantState(StateEmergencyHalted)
	if !strings.Contains(p.HaltReasonVI, "BÁO ĐỘNG") || r.eng.Status().Busy != "" {
		t.Errorf("halt %q busy %q", p.HaltReasonVI, r.eng.Status().Busy)
	}
	// Acknowledged by a stop pressed AFTER the halt was on screen.
	if _, _, err := r.eng.Stop(context.Background(), false, -1); err != nil {
		t.Fatal(err)
	}
	r.wantBot(StateDisabled)
	r.wantState(StateDisabled)
}

// A kill that lands while an open is in flight, whose close is then refused,
// leaves the bot halted and still naming the position that open created — the
// operator must see what is on the venue.
func TestEngine_AKillWhoseCloseFailsStillNamesThePosition(t *testing.T) {
	r := newRig(t)
	gate := make(chan struct{})
	r.trader.openGate, r.trader.refuseClose = gate, true
	r.start(testConfig())
	stepped := make(chan struct{})
	go func() {
		r.eng.Step(context.Background())
		close(stepped)
	}()
	waitFor(t, func() bool { return pairOf(t, r.eng.Status(), testSymbol).State == StateOpening })
	killed := make(chan StatusView, 1)
	go func() {
		st, _, _ := r.eng.Kill(context.Background())
		killed <- st
	}()
	waitFor(t, func() bool { return r.eng.Status().Busy == "kill" })
	close(gate)
	<-stepped
	st := <-killed
	p := pairOf(t, st, testSymbol)
	if st.State != StateEmergencyHalted || p.State != StateEmergencyHalted || p.Position == nil || !strings.HasPrefix(p.Position.IntentID, "abtcusdt-") {
		t.Fatalf("kill with a refused close after an open in flight = %s / %s position %+v", st.State, p.State, p.Position)
	}
	r.wantVenueHedged()
}

// STOP & CLOSE beside a position a person opened: nothing of the bot's to close,
// so the bot is disabled, not halted, and the manual pair is untouched.
func TestEngine_AStopAndCloseBesideAManualPairDisables(t *testing.T) {
	r := newRig(t)
	if res := r.trader.Open(context.Background(), OpenOrder{Symbol: testSymbol, NotionalQuote: 65, SignalEntryCostPct: 1}); !res.Hedged {
		t.Fatalf("seed = %+v", res)
	}
	r.trader.intents[0].fromAutotrade = false
	r.start(testConfig())
	r.step()
	st, out, err := r.eng.Stop(context.Background(), true, -1)
	var btc OperatorClose
	for _, oc := range out.Closes {
		if oc.Symbol == testSymbol {
			btc = oc
		}
	}
	if err != nil || !btc.NotTheBots || st.State != StateDisabled || r.trader.closeCount() != 0 {
		t.Fatalf("stop and close beside a manual pair = %+v, %v, %s, %d closes", out.Closes, err, st.State, r.trader.closeCount())
	}
	r.wantVenueHedged()
}

// The blind-history timer belongs to the position it was started under: a later
// position does not inherit its age and halt on its first blind scan.
func TestEngine_ANewPositionDoesNotInheritAnOldBlindTimer(t *testing.T) {
	var clockMu sync.Mutex
	offset := time.Duration(0)
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return time.Now().Add(offset)
	}
	r := newRigAt(t, now)
	cfg := testConfig()
	cfg.DefaultPairConfig.Cooldown = 0
	r.start(cfg)
	r.step()
	r.wantState(StateInPosition)
	// Blind, then a basis exit closes the pair while still blind.
	r.market.set(func(s *Snapshot) { s.SettledErrVI = "timeout" })
	r.step()
	r.market.set(func(s *Snapshot) { s.PerpBook = book("binance_futures", testMid*1.011, time.Now()) })
	r.step()
	r.wantVenueFlat()
	// The history reads again, and the very next scan opens a new pair — no
	// readable HOLDING scan in between that would reset the timer by itself.
	r.market.set(func(s *Snapshot) {
		s.SettledErrVI = ""
		s.PerpBook = book("binance_futures", testPerpMid, time.Now())
	})
	r.step()
	r.wantState(StateInPosition)
	// Past the budget as measured from the OLD blind start, the new pair's first
	// scan is blind: it starts a fresh budget instead of halting at once.
	clockMu.Lock()
	offset += maxHistoryBlind + time.Minute
	clockMu.Unlock()
	r.market.set(func(s *Snapshot) { s.SettledErrVI = "timeout" })
	r.step()
	r.wantState(StateInPosition)
	// Recovery, then a second outage: the budget restarts from the second one.
	r.market.set(func(s *Snapshot) { s.SettledErrVI = "" })
	r.step()
	clockMu.Lock()
	offset += maxHistoryBlind + time.Minute
	clockMu.Unlock()
	r.market.set(func(s *Snapshot) { s.SettledErrVI = "timeout" })
	r.step()
	r.wantState(StateInPosition)
}

// A refused open (nothing sent) counts toward the breaker and cools down.
func TestEngine_ARefusedOpenCountsAndCoolsDown(t *testing.T) {
	r := newRig(t)
	r.trader.refuseOpen = true
	r.start(testConfig())
	st := r.step()
	p := r.wantState(StateCooldown)
	if p.TradeFailures != 1 || !hasLog(st, "OPEN", "từ chối") {
		t.Errorf("refused open: failures %d, log %v", p.TradeFailures, logLines(st))
	}
}

// A refused close keeps the pair — counted, held, retried — and is never read as
// flat; three in a row halt the pair with the position still open.
func TestEngine_ARefusedCloseKeepsThePairAndHaltsAfterThree(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	r.trader.refuseClose = true
	settleAfterTheOpen(r, -0.00005)
	for i := 1; i <= 4; i++ {
		r.step()
		p := r.wantState(StateInPosition)
		if p.TradeFailures != i || p.Position == nil {
			t.Fatalf("after refused close %d: failures %d, position %+v", i, p.TradeFailures, p.Position)
		}
	}
	r.step()
	p := r.wantState(StateEmergencyHalted)
	r.wantVenueHedged()
	if p.Position == nil {
		t.Error("a halted pair with legs on the venue no longer names its position")
	}
}

// Two intents holding the symbol while the bot holds one: the bot cannot name
// what it would close, so the pair halts and nothing is closed.
func TestEngine_ASecondHeldIntentHaltsThePair(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	r.wantState(StateInPosition)
	if res := r.trader.Open(context.Background(), OpenOrder{Symbol: testSymbol, NotionalQuote: 65, SignalEntryCostPct: 1}); !res.Hedged {
		t.Fatalf("second open = %+v", res)
	}
	closes := r.trader.closes
	r.step()
	p := r.wantState(StateEmergencyHalted)
	if r.trader.closes != closes || !strings.Contains(p.HaltReasonVI, "2 ý định") {
		t.Errorf("closes %d→%d, halt %q", closes, r.trader.closes, p.HaltReasonVI)
	}
}

// A stop that arrives while a kill is closing is refused, and the kill's busy
// marker survives it.
func TestEngine_AStopDuringAKillIsRefusedAndTheKillDecides(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	gate := make(chan struct{})
	r.trader.closeGate = gate
	done := make(chan struct{})
	go func() {
		_, _, _ = r.eng.Kill(context.Background())
		close(done)
	}()
	waitFor(t, func() bool { return r.trader.closeCount() > 0 })
	if _, _, err := r.eng.Stop(context.Background(), false, -1); !errors.Is(err, ErrBusy) {
		t.Errorf("stop during a kill = %v", err)
	}
	if st := r.eng.Status(); st.Busy != "kill" {
		t.Errorf("busy during the kill = %q", st.Busy)
	}
	close(gate)
	<-done
	r.wantBot(StateEmergencyHalted)
	r.wantVenueFlat()
}

// A stop pressed FIRST, still waiting behind an open in flight when a kill is
// pressed: the kill supersedes it. The waiting stop neither clears the kill's
// busy marker nor, once the kill has halted the bot, turns that halt into
// "disabled" — which would acknowledge a halt nobody has read.
func TestEngine_AKillSupersedesAStopThatWasWaiting(t *testing.T) {
	r := newRig(t)
	openGate, closeGate := make(chan struct{}), make(chan struct{})
	r.trader.openGate, r.trader.closeGate = openGate, closeGate
	r.start(testConfig())
	stepped := make(chan struct{})
	go func() {
		r.eng.Step(context.Background())
		close(stepped)
	}()
	waitFor(t, func() bool { return pairOf(t, r.eng.Status(), testSymbol).State == StateOpening })

	stopErr := make(chan error, 1)
	go func() {
		_, _, err := r.eng.Stop(context.Background(), false, -1)
		stopErr <- err
	}()
	waitFor(t, func() bool { return r.eng.Status().Busy == "stop" })
	killed := make(chan struct{})
	go func() {
		_, _, _ = r.eng.Kill(context.Background())
		close(killed)
	}()
	waitFor(t, func() bool { return r.eng.Status().Busy == "kill" })

	close(openGate)
	<-stepped
	waitFor(t, func() bool { return r.trader.closeCount() > 0 })
	if st := r.eng.Status(); st.Busy != "kill" {
		t.Errorf("busy while the kill closes = %q", st.Busy)
	}
	close(closeGate)
	<-killed
	if err := <-stopErr; !errors.Is(err, ErrBusy) {
		t.Errorf("the superseded stop = %v", err)
	}
	st := r.wantBot(StateEmergencyHalted)
	r.wantVenueFlat()
	if !strings.Contains(st.HaltReasonVI, "KILL SWITCH") {
		t.Errorf("halt = %q", st.HaltReasonVI)
	}
}

// The other interleaving of the same race, which the scheduler decides and a
// test cannot order through the public calls: the kill won the lock, halted the
// bot and returned, and only then does the stop pressed before it run. It must
// see that a kill happened since it was pressed and change nothing.
func TestEngine_AStopPressedBeforeACompletedKillDoesNotAcknowledgeIt(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	r.eng.mu.Lock()
	whenPressed := stopTicket{stopSeq: r.eng.stopSeq + 1, killSeq: r.eng.killSeq, haltSeq: r.eng.haltSeq}
	r.eng.mu.Unlock()
	if _, _, err := r.eng.Kill(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.eng.stop(context.Background(), false, whenPressed); !errors.Is(err, ErrBusy) {
		t.Errorf("a stale stop after a kill = %v", err)
	}
	r.wantBot(StateEmergencyHalted)
}

// Adopting a bot position whose open instant cannot be read would count every
// listed settlement as "after the open"; the pair halts instead.
func TestEngine_AnAdoptionWithoutAnOpenInstantHaltsThePair(t *testing.T) {
	r := newRig(t)
	if res := r.trader.Open(context.Background(), OpenOrder{Symbol: testSymbol, NotionalQuote: 65, SignalEntryCostPct: 1}); !res.Hedged {
		t.Fatalf("seed = %+v", res)
	}
	r.trader.intents[0].openedAtMs = 0
	cfg := testConfig()
	cfg.DefaultPairConfig.MaxHoldEpochs = 1
	cfg.DefaultPairConfig.MinNetAPRPct = -900
	r.start(cfg)
	r.step()
	p := r.wantState(StateEmergencyHalted)
	if r.trader.closes != 0 || !strings.Contains(p.HaltReasonVI, "thời điểm mở") {
		t.Errorf("closes %d, halt %q", r.trader.closes, p.HaltReasonVI)
	}
}

// While holding, a funding history that stays unreadable is tolerated for a
// time budget — settlements are hours apart — then halts the pair with the
// position kept. The settlement count shown is not reset to 0 by the outage.
func TestEngine_AnUnreadableHistoryWhileHoldingHaltsAfterItsBudget(t *testing.T) {
	var clockMu sync.Mutex
	offset := time.Duration(0)
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return time.Now().Add(offset)
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		offset += d
		clockMu.Unlock()
	}
	r := newRigAt(t, now)
	r.start(testConfig())
	r.step()
	r.wantState(StateInPosition)
	settleAfterTheOpen(r, 0.0001)
	r.step()
	if p := pairOf(t, r.eng.Status(), testSymbol); p.Position == nil || p.Position.SettlementsSinceOpen != 1 {
		t.Fatalf("settlements since open = %+v", p.Position)
	}
	r.market.set(func(s *Snapshot) { s.SettledErrVI = "timeout" })
	for i := 0; i < 5; i++ {
		r.step()
	}
	p := r.wantState(StateInPosition)
	if p.Position.SettlementsSinceOpen != 1 || p.ReadFailures != 0 {
		t.Errorf("during a short outage: settlements %d, read failures %d", p.Position.SettlementsSinceOpen, p.ReadFailures)
	}
	advance(maxHistoryBlind + time.Minute)
	r.step()
	p = r.wantState(StateEmergencyHalted)
	r.wantVenueHedged()
	if !strings.Contains(p.HaltReasonVI, "lịch sử funding") {
		t.Errorf("halt = %q", p.HaltReasonVI)
	}
}

// A naked leg on the venue: the pair halts and the bot sends NOTHING — squaring
// is a person's call.
func TestEngine_AnUnhedgedVenueHaltsWithoutTrading(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	id := pairOf(t, r.eng.Status(), testSymbol).Position.IntentID
	// The spot leg is sold behind the bot's back.
	if _, err := r.trader.spot.PlaceOrder(context.Background(), broker.PlaceOrderRequest{Market: broker.MarketSpot, Symbol: testSymbol,
		Side: broker.SideSell, Type: broker.OrderTypeMarket, ClientOrderID: execution.CloseClientOrderID(id, execution.LegSpot), QtyCoin: 0.0008}); err != nil {
		t.Fatal(err)
	}
	orders := r.trader.orders()
	r.step()
	p := r.wantState(StateEmergencyHalted)
	if got := r.trader.orders(); got != orders {
		t.Errorf("the bot sent %d orders into an unhedged pair", got-orders)
	}
	if !strings.Contains(p.HaltReasonVI, "KHÔNG tự làm phẳng") {
		t.Errorf("halt = %q", p.HaltReasonVI)
	}
	// And a kill does not square it either.
	_, closes, _ := r.eng.Kill(context.Background())
	for _, oc := range closes {
		if oc.Symbol == testSymbol && (oc.Attempted || oc.Flat) {
			t.Errorf("kill on an unhedged pair = %+v", oc)
		}
	}
	if got := r.trader.orders(); got != orders {
		t.Error("the kill traded an unhedged pair")
	}
	if p := pairOf(t, r.eng.Status(), testSymbol); p.State != StateEmergencyHalted || !strings.Contains(p.HaltReasonVI, "trước đó") {
		t.Errorf("after the kill the pair = %s %q — the earlier halt reason must survive", p.State, p.HaltReasonVI)
	}
}

// Reads that keep failing halt the pair after the configured count.
func TestEngine_ConsecutiveReadFailuresHaltThePair(t *testing.T) {
	r := newRig(t)
	r.market.err = errors.New("venue down")
	r.start(testConfig())
	for i := 0; i < 4; i++ {
		r.step()
	}
	if p := pairOf(t, r.eng.Status(), testSymbol); p.State == StateEmergencyHalted || p.ReadFailures != 4 {
		t.Fatalf("after 4 failures: %s, %d", p.State, p.ReadFailures)
	}
	r.step()
	p := r.wantState(StateEmergencyHalted)
	if r.trader.opens != 0 || !strings.Contains(p.HaltReasonVI, "đọc sàn hỏng") {
		t.Errorf("opens %d, halt %q", r.trader.opens, p.HaltReasonVI)
	}
}

// A signal that does not qualify opens nothing, and says why.
func TestEngine_ANegativeFundingOpensNothing(t *testing.T) {
	r := newRig(t)
	r.market.set(func(s *Snapshot) { s.ForecastRatePerIntervalFrac = -0.0001 })
	r.start(testConfig())
	st := r.step()
	p := r.wantState(StateIdleScanning)
	if r.trader.opens != 0 || p.Signal == nil || p.Signal.EntryEligible || !strings.Contains(p.Signal.VerdictVI, "Funding đang hình thành") || p.Radar != RadarScanning {
		t.Errorf("opens %d signal %+v radar %s", r.trader.opens, p.Signal, p.Radar)
	}
	// The console logs a verdict when it changes, not every scan.
	lines := len(st.Log)
	st = r.step()
	if len(st.Log) != lines {
		t.Errorf("an unchanged verdict was logged again: %v", logLines(st))
	}
}

// MaxHoldEpochs: the pair is closed once the venue has listed that many
// settlements after the open — counted, not derived from a duration.
func TestEngine_ClosesAfterMaxHoldEpochsSettlements(t *testing.T) {
	r := newRig(t)
	cfg := testConfig()
	cfg.DefaultPairConfig.MaxHoldEpochs = 1
	cfg.DefaultPairConfig.MinNetAPRPct = -10_000 * 0.09 // one settlement cannot pay a round trip; the operator lowered the floor to test the exit
	r.start(cfg)
	r.step()
	r.wantState(StateInPosition)
	r.step()
	r.wantState(StateInPosition) // no settlement yet
	settleAfterTheOpen(r, 0.0001)
	st := r.step()
	r.wantState(StateCooldown)
	r.wantVenueFlat()
	if !hasLog(st, "EXIT", "đủ số mốc") {
		t.Errorf("log %v", logLines(st))
	}
}

// A basis that widened past the threshold since the open closes the pair.
func TestEngine_ClosesWhenTheBasisWidens(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	r.wantState(StateInPosition)
	r.market.set(func(s *Snapshot) { s.PerpBook = book("binance_futures", testMid*1.011, time.Now()) }) // +110 bps
	st := r.step()
	r.wantState(StateCooldown)
	r.wantVenueFlat()
	if !hasLog(st, "EXIT", "Basis giãn") {
		t.Errorf("log %v", logLines(st))
	}
}

// STOP keeps the position; the next START adopts it rather than opening a
// second one.
func TestEngine_StopKeepsThePositionAndStartAdoptsIt(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	id := pairOf(t, r.eng.Status(), testSymbol).Position.IntentID
	st0, out, err := r.eng.Stop(context.Background(), false, -1)
	// The kept position stays named: the venue still holds it.
	if err != nil || out.Closes != nil || len(out.KeptIntentIDs) != 1 || out.KeptIntentIDs[0] != id || len(st0.Positions) != 1 || st0.Positions[0].IntentID != id || st0.Busy != "" {
		t.Fatalf("stop = %+v, %v · status positions %+v busy %q", out, err, st0.Positions, st0.Busy)
	}
	r.wantBot(StateDisabled)
	r.wantVenueHedged()

	r.start(testConfig())
	r.step()
	p := r.wantState(StateInPosition)
	if p.Position == nil || p.Position.IntentID != id || !p.Position.Adopted || r.trader.opens != 1 || p.Position.SpotEntryAvgQuote <= 0 {
		t.Fatalf("after restart: position %+v, opens %d", p.Position, r.trader.opens)
	}
	// The adoption is judged before the other pairs of its scan; that must not
	// read as a portfolio over its limit (live run of 2026-09-15).
	if st := r.eng.Status(); hasLog(st, "WARN", "vượt tối đa") {
		t.Errorf("a false over-limit warning after one adoption: %v", logLines(st))
	}
	// STOP with close_now flattens it and leaves the bot disabled.
	if _, out, err := r.eng.Stop(context.Background(), true, -1); err != nil || len(out.Closes) != len(allSymbols) {
		t.Fatalf("stop and close = %+v, %v", out, err)
	}
	r.wantBot(StateDisabled)
	r.wantVenueFlat()
}

// A position the bot did not open blocks its entry and is never touched.
func TestEngine_AManualPositionBlocksEntryAndIsLeftAlone(t *testing.T) {
	r := newRig(t)
	res := r.trader.Open(context.Background(), OpenOrder{Symbol: testSymbol, NotionalQuote: 65, SignalEntryCostPct: 1})
	if !res.Hedged {
		t.Fatalf("seed open = %+v", res)
	}
	r.trader.intents[0].fromAutotrade = false
	opens := r.trader.opens
	r.start(testConfig())
	r.step()
	p := r.wantState(StateIdleScanning)
	if r.trader.opens != opens || p.Position != nil || p.Signal == nil || !strings.Contains(p.Signal.VerdictVI, "Không có vị thế nào mở") {
		t.Errorf("opens %d→%d, position %+v, verdict %+v", opens, r.trader.opens, p.Position, p.Signal)
	}
	// And the kill switch does not close it either.
	_, closes, _ := r.eng.Kill(context.Background())
	for _, oc := range closes {
		if oc.Symbol == testSymbol && (oc.Attempted || oc.Flat || !strings.Contains(oc.DetailVI, "không phải của bot")) {
			t.Errorf("kill beside a manual position = %+v", oc)
		}
	}
	r.wantVenueHedged()
}

// The portal busy with a manual write: nothing is sent, and the bot tries again.
func TestEngine_ABusyPortalIsRetriedNotCounted(t *testing.T) {
	r := newRig(t)
	r.trader.busy = true
	r.start(testConfig())
	r.step()
	p := r.wantState(StateIdleScanning)
	if p.TradeFailures != 0 || len(r.trader.spot.Orders()) != 0 {
		t.Fatalf("busy: failures %d, orders %d", p.TradeFailures, len(r.trader.spot.Orders()))
	}
	r.trader.mu.Lock()
	r.trader.busy = false
	r.trader.mu.Unlock()
	r.step()
	r.wantState(StateInPosition)
}

// Run drives the machine on its own timer and wakes on Start; when its context
// ends the held position is kept and the bot is disabled. Run under -race.
func TestEngine_RunTradesOnItsTimerAndStopsWithItsContext(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.eng.Run(ctx)
		close(done)
	}()
	cfg := testPortfolio(testSymbol, "ETHUSDT")
	cfg.ScanInterval = time.Second
	r.start(cfg)
	deadline := time.Now().Add(10 * time.Second)
	for r.eng.Status().OpenPositions != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("Run never opened both: %+v %v", r.eng.Status().Pairs, logLines(r.eng.Status()))
		}
		_ = r.eng.Status() // concurrent readers, for the race detector
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	st := r.wantBot(StateDisabled)
	r.wantVenueHedged()
	r.wantHedgedOn("ETHUSDT")
	if !hasLog(st, "STOP", "GIỮ NGUYÊN") || len(st.Positions) != 2 {
		t.Errorf("positions %d, log %v", len(st.Positions), logLines(st))
	}
}

// A start is refused when its configuration is not one a run may use.
func TestEngine_StartRefusesABadConfiguration(t *testing.T) {
	r := newRig(t)
	for name, mutate := range map[string]func(c *PortfolioConfig){
		"no symbol":            func(c *PortfolioConfig) { c.Symbols = nil },
		"symbol off the list":  func(c *PortfolioConfig) { c.Symbols = []string{"DOGEUSDT"} },
		"symbol twice":         func(c *PortfolioConfig) { c.Symbols = []string{testSymbol, testSymbol} },
		"zero concurrent":      func(c *PortfolioConfig) { c.MaxConcurrentPositions = 0 },
		"fifty-one concurrent": func(c *PortfolioConfig) { c.MaxConcurrentPositions = 51 },
		"no capital cap":       func(c *PortfolioConfig) { c.TotalCapitalCapQuote = 0 },
		"NaN capital cap":      func(c *PortfolioConfig) { c.TotalCapitalCapQuote = math.NaN() },
		"cap under one pair":   func(c *PortfolioConfig) { c.TotalCapitalCapQuote = 90 }, // $65 at 1.5× ties up $97.50
		"sub-second scan":      func(c *PortfolioConfig) { c.ScanInterval = 100 * time.Millisecond },
		"zero notional":        func(c *PortfolioConfig) { c.DefaultPairConfig.NotionalQuote = 0 },
		"notional over cap":    func(c *PortfolioConfig) { c.DefaultPairConfig.NotionalQuote = 60_000 },
		"NaN floor":            func(c *PortfolioConfig) { c.DefaultPairConfig.MinNetAPRPct = math.NaN() },
		"negative epochs":      func(c *PortfolioConfig) { c.DefaultPairConfig.MaxHoldEpochs = -1 },
		"no projection":        func(c *PortfolioConfig) { c.DefaultPairConfig.ProjectionHoldDays = 0 },
		"depth multiple under": func(c *PortfolioConfig) { c.DefaultPairConfig.DepthMultiple = 0.5 },
		"override not in run":  func(c *PortfolioConfig) { c.PairOverrides = map[string]Config{"ETHUSDT": DefaultConfig("ETHUSDT")} },
		"bad override": func(c *PortfolioConfig) {
			o := DefaultConfig(testSymbol)
			o.NotionalQuote = -1
			c.PairOverrides = map[string]Config{testSymbol: o}
		},
	} {
		c := testConfig()
		mutate(&c)
		if _, err := r.eng.Start(c); err == nil {
			t.Errorf("%s: started", name)
			_, _, _ = r.eng.Stop(context.Background(), false, -1)
		}
	}
	r.wantBot(StateDisabled)
}

// The audit of 2026-09-15: the shipped thresholds are exactly the ones the
// operator's specification names, for one pair and for the portfolio.
func TestDefaults_AreTheAuditedSafetyThresholds(t *testing.T) {
	c := DefaultConfig(testSymbol)
	// The convergence-and-amortization set of
	// docs/AUTOTRADE-CONVERGENCE-HOLDING-STRATEGY.md §3, testnet column: enter
	// only at a perp premium, hold six settlements before the funding exit may
	// run, take profit at +0.50% of CAPITAL, and leave on a run of two
	// settlements at or below -2.0 bps or a basis break of 100 bps.
	want := Config{Symbol: testSymbol, NotionalQuote: 65, MinNetAPRPct: 5, MinEntryBasisBps: 5,
		MaxHoldEpochs: 0, MinHoldEpochs: 6, TargetTakeProfitNetPct: 0.50,
		ExitNegativeFundingRateBps: -2.0, ExitNegativeConsecutiveEpochs: 2, Cooldown: 60 * time.Second,
		ProjectionHoldDays: 30, MaxBasisWidenBps: 100, MinTimeToSettle: 300 * time.Second, DepthMultiple: 2, MaxConsecutiveFailures: 5}
	if c != want {
		t.Errorf("DefaultConfig = %+v, want %+v", c, want)
	}
	pc := DefaultPortfolioConfig(allSymbols)
	if pc.MaxConcurrentPositions != len(allSymbols) || MaxConcurrentPositionsCap != 50 || pc.TotalCapitalCapQuote != 10000 || pc.ScanInterval != 10*time.Second || pc.DefaultPairConfig.NotionalQuote != 65 {
		t.Errorf("DefaultPortfolioConfig = %+v", pc)
	}
	// The buffered-slot sizing (4.5g): ON, 30% held back, re-read weekly, and
	// nothing sized yet — so the first scan of a run sizes at once and the 65
	// above is a seed rather than a size anything trades.
	if !pc.AutoRebalance || pc.MarginBufferPct != 0.30 || pc.RebalanceIntervalHours != 168 || pc.LastRebalancedAtMs != 0 {
		t.Errorf("rebalance defaults = auto %v buffer %v every %vh, last %d",
			pc.AutoRebalance, pc.MarginBufferPct, pc.RebalanceIntervalHours, pc.LastRebalancedAtMs)
	}
	if MinMarginBufferPct != 0.10 || MaxMarginBufferPct != 0.50 || MinRebalanceIntervalHours != 1 {
		t.Errorf("rebalance bounds = buffer [%v, %v], interval ≥ %vh", MinMarginBufferPct, MaxMarginBufferPct, MinRebalanceIntervalHours)
	}
	if err := pc.Validate(allSymbols, 50_000, 1.5); err != nil {
		t.Errorf("the shipped portfolio does not validate: %v", err)
	}
}

// ----------------------------------------------------------- the portfolio

// withRates sets each symbol's forming and settled rate per 8h.
func withRates(r *rig, rates map[string]float64) {
	for s, rate := range rates {
		rate := rate
		r.market.setFor(s, func(snap *Snapshot) {
			fresh := goodSnapshot(time.Now(), rate)
			*snap = *fresh
		})
	}
}

func openedSymbols(r *rig) []string {
	r.trader.mu.Lock()
	defer r.trader.mu.Unlock()
	return append([]string(nil), r.trader.opened...)
}

// Every eligible pair is ranked by Net APR, and the opens go out in that order,
// one at a time, while the portfolio has room. A pair that is eligible but does
// not fit is BỎ QUA with the reason, and a pair that is not eligible is
// neither ranked nor skipped.
func TestPortfolio_RanksEligiblePairsByNetAPRAndOpensInThatOrder(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"BTCUSDT": 0.0001, "ETHUSDT": 0.00015, "SOLUSDT": 0.0002, "BNBUSDT": -0.0001})
	pc := testPortfolio(allSymbols...)
	pc.MaxConcurrentPositions = 2
	r.start(pc)
	st := r.step()

	if got := openedSymbols(r); len(got) != 2 || got[0] != "SOLUSDT" || got[1] != "ETHUSDT" {
		t.Fatalf("opens went out as %v, want [SOLUSDT ETHUSDT] (highest Net APR first)", got)
	}
	r.wantHedgedOn("SOLUSDT")
	r.wantHedgedOn("ETHUSDT")
	r.wantFlatOn("BTCUSDT")
	r.wantFlatOn("BNBUSDT")

	sol, eth, btc, bnb := pairOf(t, st, "SOLUSDT"), pairOf(t, st, "ETHUSDT"), pairOf(t, st, "BTCUSDT"), pairOf(t, st, "BNBUSDT")
	// Held pairs show no rank; the rank they opened at is in the console.
	if !hasLogOn(st, "OPEN", "SOLUSDT", "hạng 1") || !hasLogOn(st, "OPEN", "ETHUSDT", "hạng 2") || btc.Rank != 3 || bnb.Rank != 0 || sol.Rank != 0 {
		t.Errorf("ranks: SOL logged 1 %v, ETH logged 2 %v, BTC %d, BNB %d, SOL shown %d", hasLogOn(st, "OPEN", "SOLUSDT", "hạng 1"), hasLogOn(st, "OPEN", "ETHUSDT", "hạng 2"), btc.Rank, bnb.Rank, sol.Rank)
	}
	if *sol.Signal.NetAPRPct <= *eth.Signal.NetAPRPct || *eth.Signal.NetAPRPct <= *btc.Signal.NetAPRPct {
		t.Errorf("Net APR SOL %v ETH %v BTC %v is not the order the rates imply", *sol.Signal.NetAPRPct, *eth.Signal.NetAPRPct, *btc.Signal.NetAPRPct)
	}
	if btc.Radar != RadarSkipped || !strings.Contains(btc.SkipVI, "2/2") || !hasLogOn(st, "SKIP", "BTCUSDT", "BỎ QUA") {
		t.Errorf("BTC radar %s skip %q log %v", btc.Radar, btc.SkipVI, logLines(st))
	}
	if bnb.Radar != RadarScanning || bnb.SkipVI != "" {
		t.Errorf("BNB (negative funding) radar %s skip %q", bnb.Radar, bnb.SkipVI)
	}
	if sol.Radar != RadarHolding || st.OpenPositions != 2 || st.SlotsUsed != 2 || st.CapitalDeployedQuote != 2*testCapital || st.NotionalDeployedQuote != 2*testNotional {
		t.Errorf("portfolio: SOL radar %s, open %d slots %d capital %v notional %v", sol.Radar, st.OpenPositions, st.SlotsUsed, st.CapitalDeployedQuote, st.NotionalDeployedQuote)
	}
	// A skipped pair is not logged again on every scan.
	lines := len(r.eng.Status().Log)
	r.step()
	if st := r.eng.Status(); len(st.Log) != lines {
		t.Errorf("an unchanged skip was logged again: %v", logLines(st)[:len(st.Log)-lines])
	}
}

// Three pairs held at the default limit: a FOURTH pair whose Net APR beats all
// three is still not opened — until one of the three closes, and then it opens
// in the very scan that closed it, because exits go out before entries.
func TestPortfolio_MaxConcurrentHoldsBackABetterFourthUntilOneCloses(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"BTCUSDT": 0.0001, "ETHUSDT": 0.0001, "SOLUSDT": 0.0001, "BNBUSDT": -0.0001})
	pc := testPortfolio(allSymbols...)
	pc.MaxConcurrentPositions = 3
	r.start(pc)
	r.step()
	if st := r.eng.Status(); st.OpenPositions != 3 {
		t.Fatalf("held %d pairs, want 3: %v", st.OpenPositions, logLines(st))
	}

	// BNB turns into the best opportunity on the page.
	withRates(r, map[string]float64{"BNBUSDT": 0.0005})
	st := r.step()
	bnb := pairOf(t, st, "BNBUSDT")
	if bnb.Signal == nil || !bnb.Signal.EntryEligible || bnb.Rank != 1 || bnb.Radar != RadarSkipped {
		t.Fatalf("BNB = radar %s rank %d signal %+v", bnb.Radar, bnb.Rank, bnb.Signal)
	}
	r.wantFlatOn("BNBUSDT")
	if r.trader.openCount() != 3 {
		t.Fatalf("%d opens, the fourth went through the limit", r.trader.openCount())
	}

	// ETH's settlement after the open pays negative: ETH exits, BNB enters, in
	// ONE scan.
	settleAfterTheOpenOn(r, "ETHUSDT", -0.00005)
	st = r.step()
	r.wantFlatOn("ETHUSDT")
	r.wantHedgedOn("BNBUSDT")
	if st.OpenPositions != 3 || pairOf(t, st, "ETHUSDT").State != StateCooldown {
		t.Errorf("after the swap: %d held, ETH %s · %v", st.OpenPositions, pairOf(t, st, "ETHUSDT").State, logLines(st))
	}
	var exitAt, openAt int
	for i, e := range st.Log {
		if e.Kind == "EXIT" && e.Symbol == "ETHUSDT" && exitAt == 0 {
			exitAt = i + 1
		}
		if e.Kind == "OPEN" && e.Symbol == "BNBUSDT" && strings.Contains(e.MessageVI, "gửi lệnh mở") && openAt == 0 {
			openAt = i + 1
		}
	}
	// Newest first: the open is logged AFTER the exit, so it sits above it.
	if exitAt == 0 || openAt == 0 || openAt > exitAt {
		t.Errorf("exit line %d, open line %d (newest first) — the exit must go out first · %v", exitAt, openAt, logLines(st))
	}
}

// The capital cap bounds the portfolio independently of the pair count.
func TestPortfolio_TheCapitalCapHoldsBackAnEntryThatWouldExceedIt(t *testing.T) {
	r := newRig(t)
	pc := testPortfolio(allSymbols...)
	pc.MaxConcurrentPositions = 5
	pc.TotalCapitalCapQuote = 2.5 * testCapital // two pairs tie up 2 × testCapital; a third would not fit
	r.start(pc)
	st := r.step()
	if st.OpenPositions != 2 || st.CapitalDeployedQuote != 2*testCapital {
		t.Fatalf("held %d pairs at %v capital", st.OpenPositions, st.CapitalDeployedQuote)
	}
	skipped := 0
	for _, p := range st.Pairs {
		if p.Radar == RadarSkipped {
			skipped++
			if !strings.Contains(p.SkipVI, "hạn mức") {
				t.Errorf("%s skip = %q", p.Symbol, p.SkipVI)
			}
		}
	}
	if skipped != 2 {
		t.Errorf("%d pairs skipped on capital, want 2", skipped)
	}
}

// A per-pair override is the whole Config of that pair: a larger notional that
// the capital cap cannot fit beside the others, and a floor no reading clears.
func TestPortfolio_APairOverrideReplacesTheDefault(t *testing.T) {
	r := newRig(t)
	pc := testPortfolio("BTCUSDT", "ETHUSDT")
	eth := DefaultConfig("ETHUSDT")
	eth.MinNetAPRPct = 500
	pc.PairOverrides = map[string]Config{"ETHUSDT": eth}
	r.start(pc)
	st := r.step()
	r.wantHedgedOn("BTCUSDT")
	r.wantFlatOn("ETHUSDT")
	p := pairOf(t, st, "ETHUSDT")
	if !p.Overridden || p.Config.MinNetAPRPct != 500 || p.Signal == nil || p.Signal.EntryEligible || st.Portfolio.PairOverrides["ETHUSDT"].MinNetAPRPct != 500 {
		t.Errorf("ETH override: overridden %v config %+v signal eligible %v", p.Overridden, p.Config, p.Signal != nil && p.Signal.EntryEligible)
	}
}

// DỪNG CẶP NÀY closes ONE pair: that pair goes flat at the venue and is paused,
// the other pairs stay hedged and keep being managed, and the bot keeps running
// — the closed pair is not re-entered even though it is still eligible.
func TestPortfolio_ClosingOnePairLeavesTheOthersRunning(t *testing.T) {
	r := newRig(t)
	r.start(testPortfolio("BTCUSDT", "ETHUSDT", "SOLUSDT"))
	r.step()
	for _, s := range []string{"BTCUSDT", "ETHUSDT", "SOLUSDT"} {
		r.wantHedgedOn(s)
	}
	st, oc, err := r.eng.ClosePair(context.Background(), "ETHUSDT")
	if err != nil || !oc.Attempted || !oc.Flat || oc.Symbol != "ETHUSDT" {
		t.Fatalf("close pair = %+v, %v", oc, err)
	}
	r.wantFlatOn("ETHUSDT")
	r.wantHedgedOn("BTCUSDT")
	r.wantHedgedOn("SOLUSDT")
	eth := pairOf(t, st, "ETHUSDT")
	if st.State != StateRunning || st.OpenPositions != 2 || !eth.Paused || eth.Radar != RadarPaused || eth.Position != nil || st.Busy != "" {
		t.Fatalf("after closing ETH: bot %s, held %d, ETH paused %v radar %s position %+v busy %q", st.State, st.OpenPositions, eth.Paused, eth.Radar, eth.Position, st.Busy)
	}

	// The next scans: still eligible, never re-entered; the others still held.
	opens := r.trader.openCount()
	r.step()
	r.step()
	if r.trader.openCount() != opens {
		t.Errorf("a paused pair was re-entered: %v", openedSymbols(r))
	}
	r.wantFlatOn("ETHUSDT")
	// And the others are still MANAGED: BTC's exit still fires.
	settleAfterTheOpenOn(r, "BTCUSDT", -0.00005)
	r.step()
	r.wantFlatOn("BTCUSDT")
	r.wantHedgedOn("SOLUSDT")

	// RESUME lets the bot open ETH again.
	if _, err := r.eng.PairControl("ETHUSDT", PairResume, 0); err != nil {
		t.Fatal(err)
	}
	r.step()
	r.wantHedgedOn("ETHUSDT")
}

// A pair the venue shows unhedged halts ALONE: the other pairs keep their
// positions, their exits and their entries.
func TestPortfolio_AnUnhedgedPairHaltsOnlyThatPair(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"BNBUSDT": -0.0001})
	r.start(testPortfolio(allSymbols...))
	r.step()
	id := pairOf(t, r.eng.Status(), "ETHUSDT").Position.IntentID
	eth := r.trader.venues["ETHUSDT"]
	if _, err := eth.spot.PlaceOrder(context.Background(), broker.PlaceOrderRequest{Market: broker.MarketSpot, Symbol: "ETHUSDT",
		Side: broker.SideSell, Type: broker.OrderTypeMarket, ClientOrderID: execution.CloseClientOrderID(id, execution.LegSpot), QtyCoin: 0.0008}); err != nil {
		t.Fatal(err)
	}
	settleAfterTheOpenOn(r, "SOLUSDT", -0.00005)
	withRates(r, map[string]float64{"BNBUSDT": 0.0001})
	st := r.step()

	if p := pairOf(t, st, "ETHUSDT"); p.State != StateEmergencyHalted || p.Position == nil || p.HedgeStatus != HedgeUnhedged {
		t.Fatalf("ETH = %s position %+v hedge %s", p.State, p.Position, p.HedgeStatus)
	}
	if st.State != StateRunning || st.HaltedPairs != 1 {
		t.Errorf("bot %s with %d halted pairs", st.State, st.HaltedPairs)
	}
	r.wantFlatOn("SOLUSDT")                                        // its exit still went out
	r.wantHedgedOn("BTCUSDT")                                      // still held
	if p := pairOf(t, st, "BNBUSDT"); p.State != StateInPosition { // and an entry still went out
		t.Errorf("BNB = %s (SOL freed a place, ETH keeps its own) · %v", p.State, logLines(st))
	}
	// The halted pair keeps its place: BTC + BNB held, ETH halted = 3 slots.
	if st.SlotsUsed != 3 || st.OpenPositions != 3 {
		t.Errorf("slots %d open %d — a halted pair with legs on the venue must keep its place", st.SlotsUsed, st.OpenPositions)
	}
}

// A pair's halt is acknowledged only with the halt number the operator read; the
// acknowledgement pauses the pair, and the position still on the venue is
// adopted by the next scan rather than forgotten.
func TestPortfolio_APairHaltIsAcknowledgedOnlyByTheNumberRead(t *testing.T) {
	r := newRig(t)
	r.start(testPortfolio("BTCUSDT", "ETHUSDT"))
	r.step()
	r.trader.mu.Lock()
	r.trader.holdErr = errors.New("venue down")
	r.trader.mu.Unlock()
	for i := 0; i < 5; i++ {
		r.step()
	}
	r.trader.mu.Lock()
	r.trader.holdErr = nil
	r.trader.mu.Unlock()
	st := r.eng.Status()
	btc := pairOf(t, st, "BTCUSDT")
	if btc.State != StateEmergencyHalted || btc.HaltSeq == 0 {
		t.Fatalf("BTC = %s seq %d", btc.State, btc.HaltSeq)
	}
	if _, err := r.eng.PairControl("BTCUSDT", PairResume, 0); !errors.Is(err, ErrNotStartable) {
		t.Errorf("resume of a halted pair = %v", err)
	}
	if _, err := r.eng.PairControl("BTCUSDT", PairAcknowledge, btc.HaltSeq-1); !errors.Is(err, ErrStaleAcknowledgement) {
		t.Errorf("ack with a stale number = %v", err)
	}
	st, err := r.eng.PairControl("BTCUSDT", PairAcknowledge, btc.HaltSeq)
	if err != nil {
		t.Fatal(err)
	}
	if p := pairOf(t, st, "BTCUSDT"); p.State != StateIdleScanning || !p.Paused || p.HaltReasonVI != "" || p.Position != nil {
		t.Fatalf("after ack: %+v", p)
	}
	// Both pairs are still on the venue: the next scan adopts BTC's, paused.
	opens := r.trader.openCount()
	r.step()
	p := pairOf(t, r.eng.Status(), "BTCUSDT")
	if p.State != StateInPosition || p.Position == nil || !p.Position.Adopted || r.trader.openCount() != opens {
		t.Fatalf("after the scan: %s position %+v opens %d→%d", p.State, p.Position, opens, r.trader.openCount())
	}
}

// A position of the bot's kept on a symbol the NEW run does not enter is never
// left unwatched: it is adopted, its exit is managed, and the pair is never
// re-entered after it closes.
func TestPortfolio_AKeptPositionOutsideTheRunIsAdoptedAndManagedToItsExit(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	r.wantVenueHedged()
	if _, _, err := r.eng.Stop(context.Background(), false, -1); err != nil {
		t.Fatal(err)
	}
	pc := testPortfolio("ETHUSDT")
	pc.MaxConcurrentPositions = 2
	r.start(pc)
	st := r.step()
	btc := pairOf(t, st, testSymbol)
	if btc.InRun || btc.State != StateInPosition || btc.Position == nil || !btc.Position.Adopted || !hasLogOn(st, "ADOPT", testSymbol, "chỉ quản lý") {
		t.Fatalf("BTC outside the run = in run %v state %s position %+v · %v", btc.InRun, btc.State, btc.Position, logLines(st))
	}
	r.wantHedgedOn("ETHUSDT")
	settleAfterTheOpen(r, -0.00005)
	st = r.step()
	r.wantVenueFlat()
	if p := pairOf(t, st, testSymbol); p.State != StateDisabled || p.Radar != RadarOff {
		t.Errorf("BTC after its exit = %s radar %s", p.State, p.Radar)
	}
	opens := r.trader.openCount()
	r.step()
	if r.trader.openCount() != opens {
		t.Error("a pair outside the run was entered")
	}
}

// KILL closes EVERY one of the bot's pairs, one at a time, and leaves a pair a
// person opened alone.
func TestPortfolio_KillClosesEveryBotPairAndLeavesAManualOneAlone(t *testing.T) {
	r := newRig(t)
	if res := r.trader.Open(context.Background(), OpenOrder{Symbol: "BNBUSDT", NotionalQuote: 65, SignalEntryCostPct: 1}); !res.Hedged {
		t.Fatalf("seed = %+v", res)
	}
	r.trader.intents[0].fromAutotrade = false
	// A person's pair takes a place too (the venue holds it); four places let
	// the bot hold its three.
	pc := testPortfolio("BTCUSDT", "ETHUSDT", "SOLUSDT")
	pc.MaxConcurrentPositions = 4
	r.start(pc)
	r.step()
	st, closes, err := r.eng.Kill(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"BTCUSDT", "ETHUSDT", "SOLUSDT"} {
		r.wantFlatOn(s)
	}
	r.wantHedgedOn("BNBUSDT")
	attempted := 0
	for _, oc := range closes {
		if oc.Attempted {
			attempted++
		}
		if oc.Symbol == "BNBUSDT" && !oc.NotTheBots {
			t.Errorf("BNB (a person's) = %+v", oc)
		}
	}
	if attempted != 3 || st.State != StateEmergencyHalted || st.OpenPositions != 0 || !strings.Contains(st.HaltReasonVI, "4/4") {
		t.Errorf("kill: %d closes attempted, bot %s, held %d, halt %q", attempted, st.State, st.OpenPositions, st.HaltReasonVI)
	}
}

// Opens go out one at a time, so a later entry's reading can go stale while an
// earlier open runs: it is re-checked at the moment it would be sent, and a
// reading older than the book age is not traded on.
func TestPortfolio_AnEntryWhoseReadingWentStaleDuringAnEarlierOpenIsNotSent(t *testing.T) {
	var clockMu sync.Mutex
	offset := time.Duration(0)
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return time.Now().Add(offset)
	}
	r := newRigAt(t, now)
	withRates(r, map[string]float64{"BTCUSDT": 0.0002, "ETHUSDT": 0.0001, "SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	r.trader.afterOpen = func(symbol string) {
		if symbol == "BTCUSDT" {
			clockMu.Lock()
			offset += 2 * maxBookAge
			clockMu.Unlock()
		}
	}
	r.start(testPortfolio(allSymbols...))
	st := r.step()
	r.wantHedgedOn("BTCUSDT")
	r.wantFlatOn("ETHUSDT")
	if p := pairOf(t, st, "ETHUSDT"); p.Radar != RadarSkipped || !strings.Contains(p.SkipVI, "sổ cũ") {
		t.Errorf("ETH = radar %s skip %q", p.Radar, p.SkipVI)
	}
	// Re-read on the next scan — the books stamped on the market's clock again,
	// which the test's jump had put two minutes behind — it opens.
	r.trader.afterOpen = nil
	clockMu.Lock()
	offset = 0
	clockMu.Unlock()
	r.step()
	r.wantHedgedOn("ETHUSDT")
}

// A stop pressed while the portfolio is half-way through its opens drops every
// open not yet sent.
func TestPortfolio_AStopBetweenTwoOpensDropsTheRest(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"BTCUSDT": 0.0003, "ETHUSDT": 0.0002, "SOLUSDT": 0.0001, "BNBUSDT": -0.0001})
	gate := make(chan struct{})
	r.trader.openGate = gate
	r.start(testPortfolio(allSymbols...))
	stepped := make(chan struct{})
	go func() {
		r.eng.Step(context.Background())
		close(stepped)
	}()
	waitFor(t, func() bool { return r.trader.openCount() == 1 })
	stopped := make(chan error, 1)
	go func() {
		_, _, err := r.eng.Stop(context.Background(), false, -1)
		stopped <- err
	}()
	waitFor(t, func() bool { return r.eng.Status().Busy == "stop" })
	close(gate)
	<-stepped
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if got := openedSymbols(r); len(got) != 1 || got[0] != "BTCUSDT" {
		t.Errorf("opens sent = %v, want only the one in flight", got)
	}
	r.wantHedgedOn("BTCUSDT")
	r.wantFlatOn("ETHUSDT")
	r.wantFlatOn("SOLUSDT")
}

// Pausing a pair that holds keeps managing its exit; pausing and resuming are
// refused while the bot is not running.
func TestPortfolio_APausedPairStillExits(t *testing.T) {
	r := newRig(t)
	if _, err := r.eng.PairControl(testSymbol, PairPause, 0); !errors.Is(err, ErrNotStartable) {
		t.Errorf("pause on a disabled bot = %v", err)
	}
	r.start(testConfig())
	r.step()
	if _, err := r.eng.PairControl(testSymbol, PairPause, 0); err != nil {
		t.Fatal(err)
	}
	settleAfterTheOpen(r, -0.00005)
	r.step()
	r.wantVenueFlat()
	p := r.wantState(StateIdleScanning)
	if !p.Paused || p.Radar != RadarPaused {
		t.Errorf("after the exit: paused %v radar %s", p.Paused, p.Radar)
	}
	opens := r.trader.openCount()
	r.step()
	if r.trader.openCount() != opens {
		t.Error("a paused pair opened")
	}
	if _, err := r.eng.PairControl("DOGEUSDT", PairPause, 0); !errors.Is(err, ErrUnknownSymbol) {
		t.Errorf("unknown symbol = %v", err)
	}
}

// A close-pair that finds an unhedged pair sends nothing and halts it.
func TestPortfolio_ClosePairOnAnUnhedgedPairSendsNothingAndHalts(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	id := pairOf(t, r.eng.Status(), testSymbol).Position.IntentID
	if _, err := r.trader.spot.PlaceOrder(context.Background(), broker.PlaceOrderRequest{Market: broker.MarketSpot, Symbol: testSymbol,
		Side: broker.SideSell, Type: broker.OrderTypeMarket, ClientOrderID: execution.CloseClientOrderID(id, execution.LegSpot), QtyCoin: 0.0004}); err != nil {
		t.Fatal(err)
	}
	orders := r.trader.orders()
	st, oc, err := r.eng.ClosePair(context.Background(), testSymbol)
	if err != nil || oc.Attempted || oc.Flat || r.trader.orders() != orders {
		t.Fatalf("close pair on an unhedged pair = %+v, %v, %d new orders", oc, err, r.trader.orders()-orders)
	}
	if p := pairOf(t, st, testSymbol); p.State != StateEmergencyHalted || st.State != StateRunning {
		t.Errorf("pair %s bot %s", p.State, st.State)
	}
}

// ---------------------------------------------- review of the multi-pair change

// flakyTrader fails the holding read of the symbols named in failFor.
type flakyTrader struct {
	*venueTrader
	fmu     sync.Mutex
	failFor map[string]error
	reads   map[string]int
	// hook, when set, sees every holding read after the venue answered and
	// before the engine does, and may change it.
	hook func(symbol string, h Holding, err error) (Holding, error)
}

func (f *flakyTrader) setHook(hook func(symbol string, h Holding, err error) (Holding, error)) {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	f.hook = hook
}

func (f *flakyTrader) setFail(symbol string, err error) {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	if err == nil {
		delete(f.failFor, symbol)
		return
	}
	f.failFor[symbol] = err
}

func (f *flakyTrader) readsOf(symbol string) int {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	return f.reads[symbol]
}

func (f *flakyTrader) Holding(ctx context.Context, symbol string) (Holding, error) {
	f.fmu.Lock()
	f.reads[symbol]++
	err, hook := f.failFor[symbol], f.hook
	f.fmu.Unlock()
	h := Holding{}
	if err == nil {
		h, err = f.venueTrader.Holding(ctx, symbol)
	}
	if hook != nil {
		return hook(symbol, h, err)
	}
	return h, err
}

func newFlakyRig(t *testing.T) (*rig, *flakyTrader) {
	t.Helper()
	tr := newVenueTrader(t)
	ft := &flakyTrader{venueTrader: tr, failFor: map[string]error{}, reads: map[string]int{}}
	m := goodMarket(time.Now())
	eng, err := New(Options{Market: m, Trader: ft, Symbols: allSymbols, MaxNotionalQuote: 50_000,
		PerpMarginFrac: 0.5, ActionTimeout: 30 * time.Second, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, eng: eng, trader: tr, market: m}, ft
}

// heldOnVenue lists the symbols whose venues hold any leg.
func heldOnVenue(t *testing.T, r *rig) []string {
	t.Helper()
	var out []string
	for _, s := range allSymbols {
		spotQty, perpQty := r.trader.venueLegs(t, s)
		if math.Abs(spotQty) > 1e-12 || math.Abs(perpQty) > 1e-12 {
			out = append(out, s)
		}
	}
	return out
}

// The limit counts what the VENUE may hold. Three pairs kept by a stop, a
// restart whose first scan cannot read one of them: that symbol still takes
// its place, and the fourth pair does not open (review, blocking finding B1).
func TestPortfolio_AHoldingThatCannotBeReadStillTakesItsPlace(t *testing.T) {
	r, ft := newFlakyRig(t)
	withRates(r, map[string]float64{"BNBUSDT": -0.0001})
	pc := testPortfolio(allSymbols...)
	pc.MaxConcurrentPositions = 3
	r.start(pc)
	r.step()
	if got := heldOnVenue(t, r); len(got) != 3 {
		t.Fatalf("seed held %v", got)
	}
	if _, _, err := r.eng.Stop(context.Background(), false, -1); err != nil {
		t.Fatal(err)
	}
	withRates(r, map[string]float64{"BNBUSDT": 0.0005})
	ft.setFail("BTCUSDT", errors.New("timeout"))
	r.start(pc)
	st := r.step()
	if got := heldOnVenue(t, r); len(got) != 3 {
		t.Fatalf("venue holds %v with MaxConcurrentPositions 3 — an unread holding was counted as flat", got)
	}
	btc, bnb := pairOf(t, st, "BTCUSDT"), pairOf(t, st, "BNBUSDT")
	if !btc.SlotUsed || btc.Radar != RadarUnproven || st.SlotsUsed != 3 || bnb.Radar != RadarSkipped {
		t.Errorf("BTC slot %v radar %s (%s) · slots %d · BNB radar %s %q", btc.SlotUsed, btc.Radar, btc.SlotReasonVI, st.SlotsUsed, bnb.Radar, bnb.SkipVI)
	}
	// Readable again: BTC is adopted, and the limit is still three.
	ft.setFail("BTCUSDT", nil)
	st = r.step()
	if got := heldOnVenue(t, r); len(got) != 3 || pairOf(t, st, "BTCUSDT").State != StateInPosition {
		t.Errorf("after the read recovered: venue %v, BTC %s", got, pairOf(t, st, "BTCUSDT").State)
	}
}

// Acknowledging a pair halted with unhedged legs does not free its place: the
// legs are still on the venue, and no other pair opens into it (review B1).
func TestPortfolio_AnAcknowledgedUnhedgedPairKeepsItsPlace(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"BTCUSDT": 0.0003, "ETHUSDT": 0.0002, "SOLUSDT": 0.0001, "BNBUSDT": -0.0001})
	pc := testPortfolio(allSymbols...)
	pc.MaxConcurrentPositions = 2
	r.start(pc)
	r.step()
	id := pairOf(t, r.eng.Status(), "BTCUSDT").Position.IntentID
	if _, err := r.trader.spot.PlaceOrder(context.Background(), broker.PlaceOrderRequest{Market: broker.MarketSpot, Symbol: "BTCUSDT",
		Side: broker.SideSell, Type: broker.OrderTypeMarket, ClientOrderID: execution.CloseClientOrderID(id, execution.LegSpot), QtyCoin: 0.0004}); err != nil {
		t.Fatal(err)
	}
	st := r.step()
	btc := pairOf(t, st, "BTCUSDT")
	if btc.State != StateEmergencyHalted {
		t.Fatalf("BTC = %s", btc.State)
	}
	if _, err := r.eng.PairControl("BTCUSDT", PairAcknowledge, btc.HaltSeq); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		st = r.step()
	}
	if got := heldOnVenue(t, r); len(got) != 2 {
		t.Fatalf("%d symbols hold legs with MaxConcurrentPositions 2: %v", len(got), got)
	}
	btc = pairOf(t, st, "BTCUSDT")
	if !btc.SlotUsed || btc.Radar != RadarUnproven || btc.HedgeStatus != HedgeUnhedged || !hasLogOn(st, "WARN", "BTCUSDT", "chưa chứng minh phẳng") {
		t.Errorf("BTC after ack: slot %v radar %s hedge %s · %v", btc.SlotUsed, btc.Radar, btc.HedgeStatus, logLines(st))
	}
}

// A person's position takes a place: the limits bound the account's pairs, and
// the bot cannot tell whose capital a venue position ties up is spare.
func TestPortfolio_APositionTheBotDoesNotManageTakesAPlace(t *testing.T) {
	r := newRig(t)
	if res := r.trader.Open(context.Background(), OpenOrder{Symbol: "BNBUSDT", NotionalQuote: 65, SignalEntryCostPct: 1}); !res.Hedged {
		t.Fatalf("seed = %+v", res)
	}
	r.trader.intents[0].fromAutotrade = false
	pc := testPortfolio("BTCUSDT", "ETHUSDT")
	pc.MaxConcurrentPositions = 2
	r.start(pc)
	st := r.step()
	if got := heldOnVenue(t, r); len(got) != 2 {
		t.Errorf("venue holds %v — BNB's person's pair plus one of the bot's", got)
	}
	if bnb := pairOf(t, st, "BNBUSDT"); !bnb.SlotUsed || bnb.Position != nil {
		t.Errorf("BNB = slot %v position %+v", bnb.SlotUsed, bnb.Position)
	}
}

// A close-pair that halts a pair while the bot is disabled, then a Start: the
// start is refused rather than clearing a halt nobody acknowledged (review M1).
func TestPortfolio_StartIsRefusedWhileAPairIsHalted(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	if _, _, err := r.eng.Stop(context.Background(), false, -1); err != nil {
		t.Fatal(err)
	}
	id := r.trader.intents[0].id
	if _, err := r.trader.spot.PlaceOrder(context.Background(), broker.PlaceOrderRequest{Market: broker.MarketSpot, Symbol: "BTCUSDT",
		Side: broker.SideSell, Type: broker.OrderTypeMarket, ClientOrderID: execution.CloseClientOrderID(id, execution.LegSpot), QtyCoin: 0.0004}); err != nil {
		t.Fatal(err)
	}
	st, _, err := r.eng.ClosePair(context.Background(), "BTCUSDT")
	if err != nil || pairOf(t, st, "BTCUSDT").State != StateEmergencyHalted {
		t.Fatalf("close pair on an unhedged pair of a disabled bot = %v, %s", err, pairOf(t, st, "BTCUSDT").State)
	}
	if _, err := r.eng.Start(testPortfolio("ETHUSDT")); !errors.Is(err, ErrNotStartable) || pairOf(t, r.eng.Status(), "BTCUSDT").State != StateEmergencyHalted {
		t.Errorf("start with a halted pair = %v, BTC %s", err, pairOf(t, r.eng.Status(), "BTCUSDT").State)
	}
	if _, _, err := r.eng.Stop(context.Background(), false, r.eng.Status().HaltSeq); err != nil {
		t.Fatal(err)
	}
	if _, err := r.eng.Start(testPortfolio("ETHUSDT")); err != nil {
		t.Errorf("start after the acknowledgement = %v", err)
	}
}

// A stop quoting a halt count older than the bot's is refused: it would
// acknowledge a halt raised after the page last showed the status.
func TestEngine_AStopQuotingAnOlderHaltCountIsRefused(t *testing.T) {
	r := newRig(t)
	r.trader.refuseOpen = true
	cfg := testConfig()
	cfg.DefaultPairConfig.Cooldown = 0
	r.start(cfg)
	seen := r.eng.Status().HaltSeq
	for i := 0; i < 5; i++ {
		r.step()
	}
	if pairOf(t, r.eng.Status(), testSymbol).State != StateEmergencyHalted {
		t.Fatal("the pair did not halt")
	}
	if _, _, err := r.eng.Stop(context.Background(), false, seen); !errors.Is(err, ErrHaltedWhileStopping) {
		t.Errorf("a stop quoting #%d after a new halt = %v", seen, err)
	}
	r.wantBot(StateRunning)
	if _, _, err := r.eng.Stop(context.Background(), false, r.eng.Status().HaltSeq); err != nil {
		t.Errorf("a stop quoting the current count = %v", err)
	}
}

// A pair halted by failed reads whose last decided reading was flat keeps no
// place; a pair halted by failed trades — whose unwinds may not have finished —
// does (review: a short outage must not block the portfolio, and an alarm must).
func TestPortfolio_AReadFailureHaltOnAFlatPairKeepsNoPlaceButATradeFailureHaltDoes(t *testing.T) {
	r, ft := newFlakyRig(t)
	withRates(r, map[string]float64{"ETHUSDT": -0.0001, "SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	pc := testPortfolio("BTCUSDT", "ETHUSDT")
	pc.MaxConcurrentPositions = 1
	pc.DefaultPairConfig.Cooldown = 0
	r.start(pc)
	r.step() // BTC opens, ETH is read flat
	ft.setFail("ETHUSDT", errors.New("timeout"))
	for i := 0; i < 5; i++ {
		r.step()
	}
	// While the reads fail, the halted pair proves nothing and keeps a place;
	// read flat again, it gives the place back without an acknowledgement.
	if eth := pairOf(t, r.eng.Status(), "ETHUSDT"); eth.State != StateEmergencyHalted || !eth.SlotUsed {
		t.Errorf("ETH halted, still unreadable = %s, slot %v", eth.State, eth.SlotUsed)
	}
	ft.setFail("ETHUSDT", nil)
	st := r.step()
	eth := pairOf(t, st, "ETHUSDT")
	if eth.State != StateEmergencyHalted || eth.SlotUsed {
		t.Errorf("ETH halted by reads after a flat reading, read flat again = %s, slot %v (%s)", eth.State, eth.SlotUsed, eth.SlotReasonVI)
	}
	if st.SlotsUsed != 1 {
		t.Errorf("slots %d, want BTC's one", st.SlotsUsed)
	}

	r2 := newRig(t)
	r2.trader.refuseOpen = true
	pc2 := testPortfolio("BTCUSDT", "ETHUSDT")
	pc2.DefaultPairConfig.Cooldown = 0
	withRates(r2, map[string]float64{"ETHUSDT": 0.00005})
	r2.start(pc2)
	for i := 0; i < 5; i++ {
		r2.step()
	}
	if p := pairOf(t, r2.eng.Status(), "BTCUSDT"); p.State != StateEmergencyHalted || !p.SlotUsed {
		t.Errorf("BTC halted by failed trades = %s, slot %v", p.State, p.SlotUsed)
	}
}

// Two pairs with the same Net APR open in symbol order: the ranking is the same
// on every scan.
func TestPortfolio_ATieInNetAPROpensInSymbolOrder(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"BTCUSDT": 0.0001, "ETHUSDT": 0.0001, "SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	pc := testPortfolio(allSymbols...)
	pc.MaxConcurrentPositions = 1
	r.start(pc)
	r.step()
	if got := openedSymbols(r); len(got) != 1 || got[0] != "BTCUSDT" {
		t.Errorf("opened %v, want [BTCUSDT]", got)
	}
}

// A close-pair pressed while an earlier open is on the wire stops that pair's
// own open, which the scan had already decided: the close-pair pauses the pair
// before it waits for the scan, and the scan checks the pause at the moment it
// would send.
func TestPortfolio_APauseDuringAnEarlierOpenStopsThePairsOwnOpen(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"BTCUSDT": 0.0003, "ETHUSDT": 0.0002, "SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	gate := make(chan struct{})
	r.trader.openGate = gate
	r.start(testPortfolio(allSymbols...))
	stepped := make(chan struct{})
	go func() {
		r.eng.Step(context.Background())
		close(stepped)
	}()
	waitFor(t, func() bool { return r.trader.openCount() == 1 })
	closed := make(chan OperatorClose, 1)
	go func() {
		_, oc, _ := r.eng.ClosePair(context.Background(), "ETHUSDT")
		closed <- oc
	}()
	waitFor(t, func() bool { return r.eng.Status().Busy == "close_pair" })
	close(gate)
	<-stepped
	oc := <-closed
	if got := openedSymbols(r); len(got) != 1 {
		t.Errorf("opens %v — the pair being closed had its decided open sent", got)
	}
	r.wantFlatOn("ETHUSDT")
	if !oc.Flat || oc.Attempted {
		t.Errorf("close-pair on a pair that never opened = %+v", oc)
	}
}

// An entry re-checks the next settlement at the moment it would be sent: an
// earlier open that took long enough to bring it inside the floor skips it.
func TestPortfolio_AnEntryTooCloseToSettlementBySendTimeIsNotSent(t *testing.T) {
	var clockMu sync.Mutex
	offset := time.Duration(0)
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return time.Now().Add(offset)
	}
	r := newRigAt(t, now)
	withRates(r, map[string]float64{"BTCUSDT": 0.0002, "SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	// ETH's next settlement is 5 min 30 s away: eligible at the read.
	r.market.setFor("ETHUSDT", func(s *Snapshot) {
		base := time.Now().Add(-7*time.Hour + 5*time.Minute + 30*time.Second)
		fresh := goodSnapshot(base, 0.0001)
		fresh.SpotBook, fresh.PerpBook = book("binance_spot", testMid, time.Now()), book("binance_futures", testPerpMid, time.Now())
		*s = *fresh
	})
	r.trader.afterOpen = func(symbol string) {
		if symbol == "BTCUSDT" {
			clockMu.Lock()
			offset += 45 * time.Second
			clockMu.Unlock()
		}
	}
	r.start(testPortfolio(allSymbols...))
	st := r.step()
	eth := pairOf(t, st, "ETHUSDT")
	if eth.Signal == nil || !eth.Signal.EntryEligible {
		t.Fatalf("ETH was not eligible at the read: %+v", eth.Signal)
	}
	r.wantHedgedOn("BTCUSDT")
	r.wantFlatOn("ETHUSDT")
	if eth.Radar != RadarSkipped || !strings.Contains(eth.SkipVI, "mốc settle") {
		t.Errorf("ETH = radar %s skip %q", eth.Radar, eth.SkipVI)
	}
}

// A close-pair that waited while a kill ran is dropped: the kill decides.
func TestPortfolio_AClosePairSupersededByAKillSendsNothing(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	r.eng.mu.Lock()
	whenPressed := closeTicket{busySeq: r.eng.busySeq + 1, killSeq: r.eng.killSeq}
	r.eng.mu.Unlock()
	if _, _, err := r.eng.Kill(context.Background()); err != nil {
		t.Fatal(err)
	}
	closes := r.trader.closeCount()
	if _, err := r.eng.closePair(context.Background(), r.eng.pairs[testSymbol], whenPressed); !errors.Is(err, ErrBusy) {
		t.Errorf("a close-pair pressed before a completed kill = %v", err)
	}
	if r.trader.closeCount() != closes {
		t.Error("the superseded close-pair sent a close")
	}
}

// Resuming a pair outside the run refuses one whose own position would exceed
// the capital cap — possible when every pair in the run has an override.
func TestPortfolio_ResumingAPairThatCannotFitTheCapIsRefused(t *testing.T) {
	r := newRig(t)
	pc := testPortfolio("BTCUSDT")
	pc.TotalCapitalCapQuote = 100
	btc := DefaultConfig("BTCUSDT")
	btc.NotionalQuote = 60
	pc.PairOverrides = map[string]Config{"BTCUSDT": btc}
	pc.DefaultPairConfig.NotionalQuote = 80 // 120 of capital: not one that fits
	r.start(pc)
	if _, err := r.eng.PairControl("ETHUSDT", PairResume, 0); err == nil {
		t.Error("resumed a pair whose single position exceeds the cap")
	}
	if p := pairOf(t, r.eng.Status(), "ETHUSDT"); p.InRun {
		t.Error("the refused resume put the pair in the run")
	}
}

// Two exits due in one scan; a stop pressed while the first close is on the
// wire keeps the second pair.
func TestPortfolio_AStopBetweenTwoExitsKeepsTheSecondPair(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	r.start(testPortfolio("BTCUSDT", "ETHUSDT"))
	r.step()
	settleAfterTheOpenOn(r, "BTCUSDT", -0.00005)
	settleAfterTheOpenOn(r, "ETHUSDT", -0.00005)
	gate := make(chan struct{})
	r.trader.closeGate = gate
	stepped := make(chan struct{})
	go func() {
		r.eng.Step(context.Background())
		close(stepped)
	}()
	waitFor(t, func() bool { return r.trader.closeCount() == 1 })
	stopped := make(chan error, 1)
	go func() {
		_, _, err := r.eng.Stop(context.Background(), false, -1)
		stopped <- err
	}()
	waitFor(t, func() bool { return r.eng.Status().Busy == "stop" })
	close(gate)
	<-stepped
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if n := r.trader.closeCount(); n != 1 {
		t.Errorf("%d closes — the second exit went out after the stop", n)
	}
	if got := heldOnVenue(t, r); len(got) != 1 {
		t.Errorf("venue holds %v, want the second pair kept", got)
	}
}

// After a kill: pairs that ended flat are disabled and out of the run; a pair
// the kill could not flatten is halted and still names its position.
func TestPortfolio_KillLeavesEveryPairInAStatedPlace(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	r.start(testPortfolio("BTCUSDT", "ETHUSDT"))
	r.step()
	// ETH's spot leg is sold behind the bot's back: unhedged, the kill must not
	// touch it.
	id := pairOf(t, r.eng.Status(), "ETHUSDT").Position.IntentID
	if _, err := r.trader.venues["ETHUSDT"].spot.PlaceOrder(context.Background(), broker.PlaceOrderRequest{Market: broker.MarketSpot, Symbol: "ETHUSDT",
		Side: broker.SideSell, Type: broker.OrderTypeMarket, ClientOrderID: execution.CloseClientOrderID(id, execution.LegSpot), QtyCoin: 0.0004}); err != nil {
		t.Fatal(err)
	}
	st, _, err := r.eng.Kill(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range st.Pairs {
		if p.InRun || p.Paused {
			t.Errorf("%s after the kill: in run %v paused %v", p.Symbol, p.InRun, p.Paused)
		}
		switch p.Symbol {
		case "ETHUSDT":
			if p.State != StateEmergencyHalted || p.Position == nil {
				t.Errorf("ETH = %s position %+v", p.State, p.Position)
			}
		default:
			if p.State != StateDisabled || p.Position != nil {
				t.Errorf("%s = %s position %+v", p.Symbol, p.State, p.Position)
			}
		}
	}
	r.wantFlatOn("BTCUSDT")
}

// An adopted pair keeps the notional it was opened with — the capital counts
// what the venue holds, not what the default would open today.
func TestPortfolio_AnAdoptedPairKeepsItsOwnNotional(t *testing.T) {
	r := newRig(t)
	if res := r.trader.Open(context.Background(), OpenOrder{Symbol: testSymbol, NotionalQuote: 80, SignalEntryCostPct: 1}); !res.Hedged {
		t.Fatalf("seed = %+v", res)
	}
	r.start(testConfig())
	r.step()
	p := pairOf(t, r.eng.Status(), testSymbol)
	if p.Position == nil || !p.Position.Adopted || p.Position.NotionalQuote != 80 || p.Position.CapitalQuote != 120 {
		t.Errorf("adopted position = %+v", p.Position)
	}
}

// Marking to mid: the spot leg gains when spot rises, the perp short gains when
// the perp falls.
func TestMarkToMid_TheTwoLegsSigns(t *testing.T) {
	pos := PositionView{QtyCoin: 2, SpotEntryAvgQuote: 100, PerpEntryAvgQuote: 101}
	markToMid(&pos, &SignalView{SpotMidQuote: 102, PerpMidQuote: 100, EvaluatedAtMs: 7})
	if pos.SpotLegDriftQuote == nil || *pos.SpotLegDriftQuote != 4 || *pos.PerpLegDriftQuote != 2 || *pos.PairDriftQuote != 6 || pos.MarkedAtMs != 7 {
		t.Errorf("marked = spot %v perp %v pair %v at %d", deref(pos.SpotLegDriftQuote), deref(pos.PerpLegDriftQuote), deref(pos.PairDriftQuote), pos.MarkedAtMs)
	}
	unmarked := PositionView{QtyCoin: 2, SpotEntryAvgQuote: 100}
	markToMid(&unmarked, &SignalView{SpotMidQuote: 102, PerpMidQuote: 100})
	if unmarked.PairDriftQuote != nil {
		t.Error("a position without a perp entry price was marked")
	}
}

// A pair that may not enter has its HOLDING read on every scan — the counts
// are as of this scan — and never its market.
func TestPortfolio_APairOutsideTheRunHasItsHoldingReadEveryScanAndNoMarket(t *testing.T) {
	r, ft := newFlakyRig(t)
	r.start(testConfig())
	r.step()
	r.step()
	r.step()
	if n := ft.readsOf("ETHUSDT"); n != 3 {
		t.Errorf("ETH (outside the run) holding read %d times in three scans, want 3", n)
	}
	r.market.mu.Lock()
	calls := r.market.calls
	r.market.mu.Unlock()
	// BTC's market once (it opened on the first scan), then its holding scans
	// read the market for the exits: three in all, none for the others.
	if calls != 3 {
		t.Errorf("market read %d times in three scans, want 3 (BTC's alone)", calls)
	}
}

// portalLikeTrader refuses one open the way portal.openAs does when it finds the
// symbol NOT flat: a person opened it between the scan's read and the bot's send.
type portalLikeTrader struct {
	*venueTrader
	manualOn string
	done     bool
}

func (p *portalLikeTrader) Open(ctx context.Context, o OpenOrder) OpenResult {
	if o.Symbol == p.manualOn && !p.done {
		p.done = true
		res := p.venueTrader.Open(ctx, OpenOrder{Symbol: o.Symbol, NotionalQuote: 65, SignalEntryCostPct: 1})
		if !res.Hedged {
			p.t.Fatalf("manual seed %+v", res)
		}
		p.venueTrader.mu.Lock()
		p.venueTrader.intents[len(p.venueTrader.intents)-1].fromAutotrade = false
		p.venueTrader.mu.Unlock()
		return OpenResult{Refused: true, ErrorVI: "sàn đang giữ vị thế perp — portal chỉ giữ MỘT vị thế mỗi symbol"}
	}
	return p.venueTrader.Open(ctx, o)
}

// A refused open proves nothing about the symbol — the portal refuses exactly
// when it finds it not flat — so the next-ranked pair does not open into the
// place (review round 2).
func TestPortfolio_ARefusedOpenDoesNotProveThePairFlat(t *testing.T) {
	tr := newVenueTrader(t)
	pt := &portalLikeTrader{venueTrader: tr, manualOn: "BTCUSDT"}
	m := goodMarket(time.Now())
	eng, err := New(Options{Market: m, Trader: pt, Symbols: allSymbols, MaxNotionalQuote: 50_000,
		PerpMarginFrac: 0.5, ActionTimeout: 30 * time.Second, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, eng: eng, trader: tr, market: m}
	withRates(r, map[string]float64{"SOLUSDT": 0.0003, "BTCUSDT": 0.0002, "ETHUSDT": 0.0001, "BNBUSDT": -0.0001})
	pc := testPortfolio(allSymbols...)
	pc.MaxConcurrentPositions = 2
	r.start(pc)
	st := r.step()
	if got := heldOnVenue(t, r); len(got) != 2 {
		t.Fatalf("venue holds %v with MaxConcurrentPositions 2", got)
	}
	if btc := pairOf(t, st, "BTCUSDT"); !btc.SlotUsed {
		t.Errorf("BTC after its refused open = slot %v (%s)", btc.SlotUsed, btc.SlotReasonVI)
	}
}

// A pair cooling down is read every scan too: a person's open on it during the
// cooldown takes the place, and the next pair waits (review round 2).
func TestPortfolio_APairCoolingDownIsStillRead(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"BTCUSDT": 0.0003, "ETHUSDT": 0.0002, "SOLUSDT": 0.0001, "BNBUSDT": -0.0001})
	pc := testPortfolio("BTCUSDT", "ETHUSDT", "SOLUSDT")
	pc.MaxConcurrentPositions = 1
	r.start(pc)
	r.step() // BTC opens
	settleAfterTheOpenOn(r, "BTCUSDT", -0.00005)
	r.step() // BTC exits into cooldown, ETH opens
	r.wantHedgedOn("ETHUSDT")
	if res := r.trader.Open(context.Background(), OpenOrder{Symbol: "BTCUSDT", NotionalQuote: 65, SignalEntryCostPct: 1}); !res.Hedged {
		t.Fatalf("manual seed %+v", res)
	}
	r.trader.mu.Lock()
	r.trader.intents[len(r.trader.intents)-1].fromAutotrade = false
	r.trader.mu.Unlock()
	settleAfterTheOpenOn(r, "ETHUSDT", -0.00005)
	st := r.step() // ETH exits; SOL must wait behind the person's BTC
	if got := heldOnVenue(t, r); len(got) != 1 {
		t.Errorf("venue holds %v with MaxConcurrentPositions 1", got)
	}
	if btc := pairOf(t, st, "BTCUSDT"); btc.State != StateCooldown || !btc.SlotUsed {
		t.Errorf("BTC = %s slot %v", btc.State, btc.SlotUsed)
	}
}

// A kept position that cannot be read for long enough to halt keeps its place:
// its last decided reading was never flat (review round 2).
func TestPortfolio_AReadFailureHaltOnANeverProvenPairKeepsItsPlace(t *testing.T) {
	r, ft := newFlakyRig(t)
	withRates(r, map[string]float64{"BNBUSDT": -0.0001})
	pc := testPortfolio(allSymbols...)
	pc.MaxConcurrentPositions = 3
	pc.DefaultPairConfig.Cooldown = 0
	r.start(pc)
	r.step()
	if _, _, err := r.eng.Stop(context.Background(), false, -1); err != nil {
		t.Fatal(err)
	}
	withRates(r, map[string]float64{"BNBUSDT": 0.0005})
	ft.setFail("BTCUSDT", errors.New("timeout"))
	r.start(pc)
	for i := 0; i < 5; i++ {
		r.step()
	}
	if got := heldOnVenue(t, r); len(got) > 3 {
		t.Errorf("venue holds %v with MaxConcurrentPositions 3", got)
	}
	if btc := pairOf(t, r.eng.Status(), "BTCUSDT"); btc.State != StateEmergencyHalted || !btc.SlotUsed {
		t.Errorf("BTC = %s slot %v", btc.State, btc.SlotUsed)
	}
}

// An open that unwound to flat is proven flat by execution: its place is free
// for the next pair in the same scan.
func TestPortfolio_AnUnwoundOpenFreesItsPlaceInTheSameScan(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"BTCUSDT": 0.0003, "ETHUSDT": 0.0002, "SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	r.trader.perp.SetBehaviour(brokertest.Behaviour{RejectWith: &broker.HTTPError{StatusCode: 400, URL: "https://demo-fapi.binance.com/fapi/v1/order", Body: `{"code":-2019}`}})
	pc := testPortfolio(allSymbols...)
	pc.MaxConcurrentPositions = 1
	r.start(pc)
	r.step()
	r.wantFlatOn("BTCUSDT")
	r.wantHedgedOn("ETHUSDT")
}

// A kill that finds a person's intent where the bot kept a position view drops
// the view: the venue says it is not the bot's (review round 2).
func TestPortfolio_AKillDropsAKeptViewThatIsAPersonsPosition(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	if _, _, err := r.eng.Stop(context.Background(), false, -1); err != nil {
		t.Fatal(err)
	}
	r.trader.mu.Lock()
	r.trader.intents[0].fromAutotrade = false
	r.trader.mu.Unlock()
	st, _, _ := r.eng.Kill(context.Background())
	if p := pairOf(t, st, testSymbol); p.Position != nil || len(st.Positions) != 0 {
		t.Errorf("after the kill: position %+v, %d positions", p.Position, len(st.Positions))
	}
	r.wantVenueHedged()
}

// Pausing a pair that is cooling down keeps the cooldown: pause and resume are
// not a way around it.
func TestPortfolio_PauseAndResumeDoNotSkipACooldown(t *testing.T) {
	r := newRig(t)
	r.trader.refuseOpen = true
	r.start(testConfig())
	r.step()
	r.wantState(StateCooldown)
	if _, err := r.eng.PairControl(testSymbol, PairPause, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.eng.PairControl(testSymbol, PairResume, 0); err != nil {
		t.Fatal(err)
	}
	r.trader.mu.Lock()
	r.trader.refuseOpen = false
	r.trader.mu.Unlock()
	r.step()
	r.wantState(StateCooldown)
	if r.trader.openCount() != 1 {
		t.Errorf("%d opens — a pause and a resume ended the cooldown", r.trader.openCount())
	}
}

// Closing a pair that is cooling down keeps the cooldown: close-pair and resume
// are not a way around it either (review round 3).
func TestPortfolio_AClosePairKeepsACooldown(t *testing.T) {
	r := newRig(t)
	r.trader.refuseOpen = true
	r.start(testConfig())
	r.step()
	r.wantState(StateCooldown)
	if _, _, err := r.eng.ClosePair(context.Background(), testSymbol); err != nil {
		t.Fatal(err)
	}
	if _, err := r.eng.PairControl(testSymbol, PairResume, 0); err != nil {
		t.Fatal(err)
	}
	r.trader.mu.Lock()
	r.trader.refuseOpen = false
	r.trader.mu.Unlock()
	r.step()
	if n := r.trader.openCount(); n != 1 {
		t.Errorf("%d opens — a close-pair and a resume ended the cooldown (state %s)", n, pairOf(t, r.eng.Status(), testSymbol).State)
	}
}

// A pair halted flat is still read: a person's position opened on it afterwards
// takes a place (review round 3).
func TestPortfolio_APersonsPositionOnAHaltedFlatPairTakesAPlace(t *testing.T) {
	r, ft := newFlakyRig(t)
	withRates(r, map[string]float64{"BTCUSDT": -0.0001, "ETHUSDT": -0.0001, "SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	pc := testPortfolio(allSymbols...)
	pc.MaxConcurrentPositions = 2
	r.start(pc)
	r.step()
	ft.setFail("BTCUSDT", errors.New("timeout"))
	for i := 0; i < 5; i++ {
		r.step()
	}
	if p := pairOf(t, r.eng.Status(), "BTCUSDT"); p.State != StateEmergencyHalted {
		t.Fatalf("BTC = %s", p.State)
	}
	ft.setFail("BTCUSDT", nil)
	if res := r.trader.Open(context.Background(), OpenOrder{Symbol: "BTCUSDT", NotionalQuote: 65, SignalEntryCostPct: 1}); !res.Hedged {
		t.Fatalf("manual seed %+v", res)
	}
	r.trader.mu.Lock()
	r.trader.intents[len(r.trader.intents)-1].fromAutotrade = false
	r.trader.mu.Unlock()
	withRates(r, map[string]float64{"ETHUSDT": 0.0003, "SOLUSDT": 0.0002, "BNBUSDT": 0.0001})
	st := r.step()
	if got := heldOnVenue(t, r); len(got) != 2 {
		t.Errorf("venue holds %v with MaxConcurrentPositions 2", got)
	}
	if btc := pairOf(t, st, "BTCUSDT"); btc.State != StateEmergencyHalted || !btc.SlotUsed {
		t.Errorf("BTC = %s slot %v", btc.State, btc.SlotUsed)
	}
}

// A kept position that cannot be read after a start names no notional; it is
// not counted at the new run's size — entries wait until it can be read, and
// the cap is not crossed on a guess (review round 3).
func TestPortfolio_AnUnreadableKeptPositionHoldsEntriesInsteadOfBeingGuessed(t *testing.T) {
	r, ft := newFlakyRig(t)
	big := testPortfolio("BTCUSDT")
	big.DefaultPairConfig.NotionalQuote = 500
	r.start(big)
	r.step()
	r.wantHedgedOn("BTCUSDT")
	if _, _, err := r.eng.Stop(context.Background(), false, -1); err != nil {
		t.Fatal(err)
	}
	withRates(r, map[string]float64{"BNBUSDT": -0.0001})
	ft.setFail("BTCUSDT", errors.New("timeout"))
	small := testPortfolio(allSymbols...)
	small.TotalCapitalCapQuote = 300
	r.start(small)
	st := r.step()
	if got := heldOnVenue(t, r); len(got) != 1 {
		t.Errorf("venue holds %v — an entry went out beside a position of unknown size", got)
	}
	if eth := pairOf(t, st, "ETHUSDT"); eth.Radar != RadarSkipped || !strings.Contains(eth.SkipVI, "không biết vốn") {
		t.Errorf("ETH = radar %s skip %q", eth.Radar, eth.SkipVI)
	}
	// Readable again: BTC is adopted at its own 500, which alone fills the cap.
	ft.setFail("BTCUSDT", nil)
	st = r.step()
	if got := heldOnVenue(t, r); len(got) != 1 || pairOf(t, st, "BTCUSDT").Position == nil || pairOf(t, st, "BTCUSDT").Position.CapitalQuote != 750 {
		t.Errorf("after the read recovered: venue %v, BTC %+v", got, pairOf(t, st, "BTCUSDT").Position)
	}
}

// Stop & close and close-pair both drop a kept view the venue says is a
// person's (review round 3).
func TestPortfolio_StopCloseAndClosePairDropAPersonsKeptView(t *testing.T) {
	for name, act := range map[string]func(r *rig) StatusView{
		"stop and close": func(r *rig) StatusView { st, _, _ := r.eng.Stop(context.Background(), true, -1); return st },
		"close pair":     func(r *rig) StatusView { st, _, _ := r.eng.ClosePair(context.Background(), testSymbol); return st },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.start(testConfig())
			r.step()
			if _, _, err := r.eng.Stop(context.Background(), false, -1); err != nil {
				t.Fatal(err)
			}
			r.trader.mu.Lock()
			r.trader.intents[0].fromAutotrade = false
			r.trader.mu.Unlock()
			st := act(r)
			if p := pairOf(t, st, testSymbol); p.Position != nil || len(st.Positions) != 0 {
				t.Errorf("after %s: position %+v", name, p.Position)
			}
			r.wantVenueHedged()
		})
	}
}

// ------------------------------------------------------------ review round 4

// A pair halted by failed reads beside a kept position of 500 is counted, once
// it reads again, at the 500 its reading names — not at the 65 the run would
// open — so the cap is not crossed beside it. A halt is not a size (review
// round 4, blocking: the probe that opened ETH under a 300 cap).
func TestPortfolio_AHaltedPairIsCountedAtTheSizeItsReadingNames(t *testing.T) {
	r, ft := newFlakyRig(t)
	big := testPortfolio("BTCUSDT")
	big.DefaultPairConfig.NotionalQuote = 500
	r.start(big)
	r.step()
	r.wantHedgedOn("BTCUSDT")
	if _, _, err := r.eng.Stop(context.Background(), false, -1); err != nil {
		t.Fatal(err)
	}
	withRates(r, map[string]float64{"BNBUSDT": -0.0001})
	ft.setFail("BTCUSDT", errors.New("timeout"))
	small := testPortfolio(allSymbols...)
	small.TotalCapitalCapQuote = 300
	r.start(small)
	for i := 0; i < 5; i++ {
		r.step()
	}
	if btc := pairOf(t, r.eng.Status(), "BTCUSDT"); btc.State != StateEmergencyHalted {
		t.Fatalf("BTC after five failed reads = %s", btc.State)
	}
	ft.setFail("BTCUSDT", nil)
	st := r.step()
	if got := heldOnVenue(t, r); len(got) != 1 {
		t.Errorf("venue holds %v — an entry went out beside a halted 500-notional position under a 300 cap", got)
	}
	btc, eth := pairOf(t, st, "BTCUSDT"), pairOf(t, st, "ETHUSDT")
	if btc.State != StateEmergencyHalted || !btc.SlotUsed || st.CapitalCommittedQuote != 750 {
		t.Errorf("BTC %s slot %v · committed %.2f, want 750", btc.State, btc.SlotUsed, st.CapitalCommittedQuote)
	}
	if eth.Radar != RadarSkipped || !strings.Contains(eth.SkipVI, "vượt hạn mức") {
		t.Errorf("ETH = radar %s skip %q", eth.Radar, eth.SkipVI)
	}
}

// A person's raw perp position makes an in-run pair evidence_conflict: the pair
// halts, and since its legs name no intent there is no size to count it at —
// every entry waits, with the reason, until the venue shows the pair flat. No
// acknowledgement is needed for that: the halt still waits for a person, the
// count follows the venue (review round 4).
func TestPortfolio_APersonsUnsizedConflictHoldsEveryEntryUntilItIsFlat(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"BNBUSDT": -0.0001})
	ctx := context.Background()
	raw := broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", Side: broker.SideSell,
		Type: broker.OrderTypeMarket, ClientOrderID: "manual-raw-short", QtyCoin: 0.0008}
	if _, err := r.trader.perp.PlaceOrder(ctx, raw); err != nil {
		t.Fatal(err)
	}
	r.start(testPortfolio(allSymbols...))
	st := r.step()
	btc, eth := pairOf(t, st, "BTCUSDT"), pairOf(t, st, "ETHUSDT")
	if btc.State != StateEmergencyHalted || btc.HedgeStatus != HedgeEvidenceConflict {
		t.Fatalf("BTC = %s %s", btc.State, btc.HedgeStatus)
	}
	if n := r.trader.openCount(); n != 0 {
		t.Errorf("%d opens beside a position of unknown size", n)
	}
	if len(st.UnsizedPairs) != 1 || st.UnsizedPairs[0] != "BTCUSDT" {
		t.Errorf("unsized pairs = %v", st.UnsizedPairs)
	}
	if eth.Radar != RadarSkipped || !strings.Contains(eth.SkipVI, "không biết vốn") || !strings.Contains(eth.SkipVI, "BTCUSDT (evidence_conflict") {
		t.Errorf("ETH = radar %s skip %q", eth.Radar, eth.SkipVI)
	}
	cover := raw
	cover.Side, cover.ClientOrderID = broker.SideBuy, "manual-raw-cover"
	if _, err := r.trader.perp.PlaceOrder(ctx, cover); err != nil {
		t.Fatal(err)
	}
	st = r.step()
	if n := r.trader.openCount(); n != 2 {
		t.Errorf("after the person squared BTC: %d opens, want ETH and SOL beside BTC's place", n)
	}
	if btc := pairOf(t, st, "BTCUSDT"); btc.State != StateEmergencyHalted || !btc.SlotUsed {
		t.Errorf("BTC = %s slot %v", btc.State, btc.SlotUsed)
	}
}

// An open the bot sent and could not prove hedged or flat is NOT counted at the
// size it sent while the pair cannot be read: its own record is not what the
// venue holds (a person may have opened beside it). Entries wait until a
// reading states a size, and then the size the reading states is counted
// (review round 6).
func TestPortfolio_AnAlarmedOpenIsSizedByItsReadingNotByWhatWasSent(t *testing.T) {
	r, ft := newFlakyRig(t)
	withRates(r, map[string]float64{"BTCUSDT": 0.0003, "ETHUSDT": -0.0001, "SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	r.trader.alarmOpen = true
	r.start(testPortfolio(allSymbols...))
	r.step()
	if btc := pairOf(t, r.eng.Status(), "BTCUSDT"); btc.State != StateEmergencyHalted || !strings.Contains(btc.HaltReasonVI, "MỞ BÁO ĐỘNG") {
		t.Fatalf("BTC = %s %q", btc.State, btc.HaltReasonVI)
	}
	r.trader.mu.Lock()
	r.trader.alarmOpen = false
	r.trader.mu.Unlock()
	ft.setFail("BTCUSDT", errors.New("timeout"))
	withRates(r, map[string]float64{"ETHUSDT": 0.0002})
	st := r.step()
	if eth := pairOf(t, st, "ETHUSDT"); eth.Position != nil || !strings.Contains(eth.SkipVI, "không biết vốn") {
		t.Errorf("ETH = position %+v skip %q — an unreadable alarm was counted at the size it sent", eth.Position, eth.SkipVI)
	}
	// Read again: one unhedged intent of 300 — its notional is counted.
	ft.setFail("BTCUSDT", nil)
	ft.setHook(func(symbol string, h Holding, err error) (Holding, error) {
		if symbol == "BTCUSDT" {
			return Holding{Status: HedgeUnhedged, ReasonVI: "test", HeldIntents: 1, IntentID: "abtcusdt-test-001", NotionalQuote: 300}, nil
		}
		return h, err
	})
	st = r.step()
	if pairOf(t, st, "ETHUSDT").Position == nil || st.CapitalCommittedQuote != 450+testCapital {
		t.Errorf("ETH %+v · committed %.2f, want BTC's read 450 + ETH's %.2f", pairOf(t, st, "ETHUSDT").Position, st.CapitalCommittedQuote, testCapital)
	}
}

// A halt of another kind after an acknowledged alarm does not inherit the
// alarm's size: a run of failed reads names none, and entries wait again.
func TestPortfolio_AHaltAfterAnAcknowledgedAlarmDoesNotInheritItsSize(t *testing.T) {
	r, ft := newFlakyRig(t)
	withRates(r, map[string]float64{"BTCUSDT": 0.0003, "ETHUSDT": -0.0001, "SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	r.trader.alarmOpen = true
	r.start(testPortfolio(allSymbols...))
	r.step()
	btc := pairOf(t, r.eng.Status(), "BTCUSDT")
	if btc.State != StateEmergencyHalted {
		t.Fatalf("BTC = %s", btc.State)
	}
	r.trader.mu.Lock()
	r.trader.alarmOpen = false
	r.trader.mu.Unlock()
	if _, err := r.eng.PairControl("BTCUSDT", PairAcknowledge, btc.HaltSeq); err != nil {
		t.Fatal(err)
	}
	if _, err := r.eng.PairControl("BTCUSDT", PairResume, 0); err != nil {
		t.Fatal(err)
	}
	ft.setFail("BTCUSDT", errors.New("timeout"))
	withRates(r, map[string]float64{"ETHUSDT": 0.0002})
	var st StatusView
	for i := 0; i < 5; i++ {
		st = r.step()
	}
	if btc := pairOf(t, st, "BTCUSDT"); btc.State != StateEmergencyHalted || !strings.Contains(btc.HaltReasonVI, "đọc sàn hỏng") {
		t.Fatalf("BTC = %s %q", btc.State, btc.HaltReasonVI)
	}
	if eth := pairOf(t, st, "ETHUSDT"); eth.Position != nil || !strings.Contains(eth.SkipVI, "không biết vốn") {
		t.Errorf("ETH = position %+v skip %q — the read-failure halt was counted at the old alarm's size", eth.Position, eth.SkipVI)
	}
}

// A reading planned for a halted pair is only counted: an acknowledgement that
// lands while it is being read does not let it adopt in that scan. The next
// scan reads the pair as it is after the acknowledgement (review round 4).
func TestPortfolio_AnAcknowledgementDuringAHaltedPairsReadDoesNotLetThatReadingAct(t *testing.T) {
	r, ft := newFlakyRig(t)
	r.start(testConfig())
	r.step()
	r.wantVenueHedged()
	if _, _, err := r.eng.Stop(context.Background(), false, -1); err != nil {
		t.Fatal(err)
	}
	ft.setFail(testSymbol, errors.New("timeout"))
	r.start(testConfig())
	for i := 0; i < 5; i++ {
		r.step()
	}
	if p := pairOf(t, r.eng.Status(), testSymbol); p.State != StateEmergencyHalted || p.Position != nil {
		t.Fatalf("BTC = %s position %+v", p.State, p.Position)
	}
	ft.setFail(testSymbol, nil)
	var once sync.Once
	ft.setHook(func(symbol string, h Holding, err error) (Holding, error) {
		if symbol != testSymbol {
			return h, err
		}
		once.Do(func() {
			seq := pairOf(t, r.eng.Status(), symbol).HaltSeq
			if _, ackErr := r.eng.PairControl(symbol, PairAcknowledge, seq); ackErr != nil {
				t.Errorf("ack during the read: %v", ackErr)
			}
		})
		return h, err
	})
	st := r.step()
	if p := pairOf(t, st, testSymbol); p.Position != nil || hasLogOn(st, "ADOPT", testSymbol, "") {
		t.Errorf("a reading taken for the halt adopted after the ack landed: %+v · %v", p.Position, logLines(st))
	}
	st = r.step()
	if p := pairOf(t, st, testSymbol); p.Position == nil || !p.Paused {
		t.Errorf("the scan after the ack = position %+v paused %v", p.Position, p.Paused)
	}
}

// A watched pair's warning is logged once per KIND of state, not once per
// wording: a reason carrying a changing tally would otherwise log every scan
// (review round 4: the de-duplication had no test).
func TestPortfolio_AWatchedPairsWarningIsLoggedOncePerKindOfState(t *testing.T) {
	r, ft := newFlakyRig(t)
	if res := r.trader.Open(context.Background(), OpenOrder{Symbol: "BNBUSDT", NotionalQuote: 65, SignalEntryCostPct: 1}); !res.Hedged {
		t.Fatalf("seed = %+v", res)
	}
	r.trader.intents[0].fromAutotrade = false
	var mu sync.Mutex
	tally := 0
	ft.setHook(func(symbol string, h Holding, err error) (Holding, error) {
		if symbol == "BNBUSDT" && err == nil {
			mu.Lock()
			tally++
			h.ReasonVI = fmt.Sprintf("%s · weight %d", h.ReasonVI, tally)
			mu.Unlock()
		}
		return h, err
	})
	r.start(testPortfolio("BTCUSDT"))
	warns := func(st StatusView) int {
		n := 0
		for _, e := range st.Log {
			if e.Kind == "WARN" && e.Symbol == "BNBUSDT" {
				n++
			}
		}
		return n
	}
	var st StatusView
	for i := 0; i < 3; i++ {
		st = r.step()
	}
	if n := warns(st); n != 1 {
		t.Errorf("%d warnings over three scans of the same state: %v", n, logLines(st))
	}
	ft.setFail("BNBUSDT", errors.New("timeout"))
	for i := 0; i < 2; i++ {
		st = r.step()
	}
	if n := warns(st); n != 2 {
		t.Errorf("%d warnings after the state changed kind once: %v", n, logLines(st))
	}
}

// ------------------------------------------------------------ review round 5

// The bot holds BTC at 65; between two scans a person closes it by hand and
// opens a 500-notional BTC of their own. The scan that halts BTC keeps the
// bot's position named — but counts the pair at the 750 the venue now shows,
// in that scan and every scan after, until an acknowledgement: a halted pair
// with a position is read like any other (review round 5, blocking).
func TestPortfolio_AHaltedPositionTheVenueNoLongerShowsIsCountedAtWhatItShows(t *testing.T) {
	r, ft := newFlakyRig(t)
	withRates(r, map[string]float64{"BTCUSDT": 0.0003, "ETHUSDT": -0.0001, "SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	pc := testPortfolio(allSymbols...)
	pc.TotalCapitalCapQuote = 300
	r.start(pc)
	r.step()
	r.wantHedgedOn("BTCUSDT")
	id := pairOf(t, r.eng.Status(), "BTCUSDT").Position.IntentID
	ctx := context.Background()
	if res := r.trader.Close(ctx, "BTCUSDT", id, "manual test close"); !res.Flat {
		t.Fatalf("manual close %+v", res)
	}
	if res := r.trader.Open(ctx, OpenOrder{Symbol: "BTCUSDT", NotionalQuote: 500, SignalEntryCostPct: 1}); !res.Hedged {
		t.Fatalf("manual open %+v", res)
	}
	r.trader.mu.Lock()
	r.trader.intents[len(r.trader.intents)-1].fromAutotrade = false
	r.trader.mu.Unlock()
	withRates(r, map[string]float64{"ETHUSDT": 0.0002})
	st := r.step()
	btc := pairOf(t, st, "BTCUSDT")
	if btc.State != StateEmergencyHalted || btc.Position == nil {
		t.Fatalf("BTC = %s position %+v", btc.State, btc.Position)
	}
	if pairOf(t, st, "ETHUSDT").Position != nil || st.CapitalCommittedQuote != 750 {
		t.Errorf("the halting scan: ETH position %+v, committed %.2f — want no entry beside a venue holding 750", pairOf(t, st, "ETHUSDT").Position, st.CapitalCommittedQuote)
	}
	before := ft.readsOf("BTCUSDT")
	withRates(r, map[string]float64{"SOLUSDT": 0.0002})
	st = r.step()
	if ft.readsOf("BTCUSDT") == before {
		t.Error("a halted pair with a position was not read")
	}
	if got := heldOnVenue(t, r); len(got) != 1 || st.CapitalCommittedQuote != 750 {
		t.Errorf("the scan after: venue holds %v, committed %.2f", got, st.CapitalCommittedQuote)
	}
}

// A conflict names perp that no intent explains, so the one intent's notional
// is not the pair's size: a person's 65 intent beside a raw 0.01 BTC short is
// unsized, the same as the raw short alone (review round 5).
func TestPortfolio_AConflictBesideOneIntentIsNotSizedAtThatIntent(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"BNBUSDT": -0.0001})
	ctx := context.Background()
	if res := r.trader.Open(ctx, OpenOrder{Symbol: "BTCUSDT", NotionalQuote: 65, SignalEntryCostPct: 1}); !res.Hedged {
		t.Fatalf("manual open %+v", res)
	}
	r.trader.intents[0].fromAutotrade = false
	if _, err := r.trader.perp.PlaceOrder(ctx, broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", Side: broker.SideSell,
		Type: broker.OrderTypeMarket, ClientOrderID: "manual-raw-short", QtyCoin: 0.01}); err != nil {
		t.Fatal(err)
	}
	pc := testPortfolio(allSymbols...)
	pc.TotalCapitalCapQuote = 300
	r.start(pc)
	st := r.step()
	if btc := pairOf(t, st, "BTCUSDT"); btc.HedgeStatus != HedgeEvidenceConflict {
		t.Fatalf("BTC hedge = %s", btc.HedgeStatus)
	}
	if n := r.trader.openCount(); n != 1 {
		t.Errorf("%d bot opens beside a conflict whose raw perp no intent explains", n-1)
	}
}

// An open refused before placing — the portal refuses exactly when it finds the
// symbol not flat — leaves the pair's size unknown until it is read again, so
// the next entry of the same scan waits (review round 5).
func TestPortfolio_ARefusedOpenHoldsTheScansNextEntry(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"BTCUSDT": 0.0003, "ETHUSDT": 0.0002, "SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	pc := testPortfolio(allSymbols...)
	pc.TotalCapitalCapQuote = 1.5 * testCapital // one pair fits, two do not
	r.trader.refuseOpen = true
	r.trader.afterOpen = func(symbol string) {
		if symbol != "BTCUSDT" {
			return
		}
		r.trader.mu.Lock()
		r.trader.refuseOpen = false
		r.trader.mu.Unlock()
		if _, err := r.trader.perp.PlaceOrder(context.Background(), broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT",
			Side: broker.SideSell, Type: broker.OrderTypeMarket, ClientOrderID: "manual-raw-short", QtyCoin: 0.01}); err != nil {
			t.Error(err)
		}
	}
	r.start(pc)
	st := r.step()
	eth := pairOf(t, st, "ETHUSDT")
	if eth.Position != nil || !strings.Contains(eth.SkipVI, "portal từ chối") {
		t.Errorf("ETH = position %+v skip %q — it went out in the scan whose BTC open was refused", eth.Position, eth.SkipVI)
	}
}

// The portal busy with a manual write: nothing was sent, but a person is
// writing right now, so the scan's remaining entries are dropped (review round 5).
func TestPortfolio_ABusyPortalDropsTheScansRemainingEntries(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"BTCUSDT": 0.0003, "ETHUSDT": 0.0002, "SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	r.trader.busy = true
	r.start(testPortfolio(allSymbols...))
	st := r.step()
	if got := openedSymbols(r); len(got) != 0 {
		t.Errorf("opens %v", got)
	}
	asked := 0
	for _, e := range st.Log {
		if e.Kind == "OPEN" && strings.Contains(e.MessageVI, "portal đang bận") {
			asked++
		}
	}
	if asked != 1 {
		t.Errorf("%d opens asked of a busy portal in one scan, want the first only: %v", asked, logLines(st))
	}
	if eth := pairOf(t, st, "ETHUSDT"); eth.State != StateIdleScanning {
		t.Errorf("ETH = %s", eth.State)
	}
}

// One of the bot's positions whose intent file names no notional is not
// adopted at the configured size: the pair halts, and it counts as unsized
// (review round 5).
func TestPortfolio_AnAdoptionWithoutANotionalHaltsInsteadOfGuessing(t *testing.T) {
	r := newRig(t)
	if res := r.trader.Open(context.Background(), OpenOrder{Symbol: testSymbol, NotionalQuote: 65, SignalEntryCostPct: 1}); !res.Hedged {
		t.Fatalf("seed %+v", res)
	}
	r.trader.mu.Lock()
	r.trader.intents[0].notional = 0
	r.trader.mu.Unlock()
	r.start(testConfig())
	st := r.step()
	p := pairOf(t, st, testSymbol)
	if p.State != StateEmergencyHalted || p.Position != nil || !strings.Contains(p.HaltReasonVI, "notional") {
		t.Errorf("BTC = %s position %+v halt %q", p.State, p.Position, p.HaltReasonVI)
	}
	if len(st.UnsizedPairs) != 1 {
		t.Errorf("unsized = %v", st.UnsizedPairs)
	}
}

// ------------------------------------------------------------ review round 6

// heldAt is the rig after the bot opened BTC alone under a 300 cap, the other
// pairs not eligible.
func heldAt(t *testing.T) (*rig, *flakyTrader) {
	t.Helper()
	r, ft := newFlakyRig(t)
	withRates(r, map[string]float64{"BTCUSDT": 0.0003, "ETHUSDT": -0.0001, "SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	pc := testPortfolio(allSymbols...)
	pc.TotalCapitalCapQuote = 3 * testCapital // room for the held BTC and the ETH each case lets in
	r.start(pc)
	r.step()
	r.wantHedgedOn("BTCUSDT")
	return r, ft
}

// A person opens a second intent of 500 beside the bot's held BTC (execcheck
// -open checks no flatness and shares the intent directory). The reading names
// two intents, which states no size: the pair is unsized and no entry opens
// (review round 6, blocking).
func TestPortfolio_ASecondIntentBesideAHeldPositionStopsEntries(t *testing.T) {
	r, _ := heldAt(t)
	if res := r.trader.Open(context.Background(), OpenOrder{Symbol: "BTCUSDT", NotionalQuote: 500, SignalEntryCostPct: 1}); !res.Hedged {
		t.Fatalf("manual open %+v", res)
	}
	r.trader.mu.Lock()
	r.trader.intents[len(r.trader.intents)-1].fromAutotrade = false
	r.trader.mu.Unlock()
	withRates(r, map[string]float64{"ETHUSDT": 0.0002, "SOLUSDT": 0.0002})
	var st StatusView
	for i := 0; i < 2; i++ {
		st = r.step()
	}
	if got := heldOnVenue(t, r); len(got) != 1 {
		t.Errorf("venue holds %v — an entry went out beside two BTC intents of unknown total size", got)
	}
	if len(st.UnsizedPairs) != 1 || st.UnsizedPairs[0] != "BTCUSDT" {
		t.Errorf("unsized = %v", st.UnsizedPairs)
	}
}

// A held pair whose reading fails, or decides nothing, is not counted at the
// bot's own position: between two scans a person may have replaced it. Entries
// wait for the scan it reads again (review round 6).
func TestPortfolio_AHeldPairThatCannotBeReadIsUnsized(t *testing.T) {
	for name, fail := range map[string]func(ft *flakyTrader){
		"read error": func(ft *flakyTrader) { ft.setFail("BTCUSDT", errors.New("timeout")) },
		"unknown": func(ft *flakyTrader) {
			ft.setHook(func(symbol string, h Holding, err error) (Holding, error) {
				if symbol == "BTCUSDT" {
					return Holding{Status: HedgeUnknown, ReasonVI: "1 lệnh vẫn đang chạy trên sàn", ToleranceQtyCoin: perpStep}, nil
				}
				return h, err
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, ft := heldAt(t)
			fail(ft)
			withRates(r, map[string]float64{"ETHUSDT": 0.0002})
			st := r.step()
			if eth := pairOf(t, st, "ETHUSDT"); eth.Position != nil || !strings.Contains(eth.SkipVI, "không biết vốn") {
				t.Errorf("ETH = position %+v skip %q", eth.Position, eth.SkipVI)
			}
			ft.setFail("BTCUSDT", nil)
			ft.setHook(nil)
			st = r.step()
			if pairOf(t, st, "ETHUSDT").Position == nil {
				t.Errorf("read again, ETH still waits: %q", pairOf(t, st, "ETHUSDT").SkipVI)
			}
		})
	}
}

// The person's replacement of the bot's BTC is itself unhedged — one intent,
// but not the bot's: still a contradiction, counted at that intent's 500
// (review round 6: the unhedged case had no test).
func TestPortfolio_AnUnhedgedReplacementOfAHeldPositionIsCountedAtItsReading(t *testing.T) {
	r, _ := heldAt(t)
	ctx := context.Background()
	id := pairOf(t, r.eng.Status(), "BTCUSDT").Position.IntentID
	if res := r.trader.Close(ctx, "BTCUSDT", id, "manual test close"); !res.Flat {
		t.Fatalf("manual close %+v", res)
	}
	res := r.trader.Open(ctx, OpenOrder{Symbol: "BTCUSDT", NotionalQuote: 500, SignalEntryCostPct: 1})
	if !res.Hedged {
		t.Fatalf("manual open %+v", res)
	}
	r.trader.mu.Lock()
	r.trader.intents[len(r.trader.intents)-1].fromAutotrade = false
	r.trader.mu.Unlock()
	if _, err := r.trader.spot.PlaceOrder(ctx, broker.PlaceOrderRequest{Market: broker.MarketSpot, Symbol: "BTCUSDT", Side: broker.SideSell,
		Type: broker.OrderTypeMarket, ClientOrderID: execution.CloseClientOrderID(res.IntentID, execution.LegSpot), QtyCoin: 0.001}); err != nil {
		t.Fatal(err)
	}
	withRates(r, map[string]float64{"ETHUSDT": 0.0002})
	st := r.step()
	if btc := pairOf(t, st, "BTCUSDT"); btc.HedgeStatus != HedgeUnhedged || btc.State != StateEmergencyHalted {
		t.Fatalf("BTC = %s %s", btc.State, btc.HedgeStatus)
	}
	if pairOf(t, st, "ETHUSDT").Position != nil || st.CapitalCommittedQuote != 750 {
		t.Errorf("ETH %+v · committed %.2f, want the replacement's 750", pairOf(t, st, "ETHUSDT").Position, st.CapitalCommittedQuote)
	}
}

// A halted pair holding a position has its holding read every scan — and only
// its holding: no market, no exit, even once its readings match the position
// again (review round 6).
func TestPortfolio_AHaltedPairWithAPositionIsReadButNeverJudged(t *testing.T) {
	r, ft := heldAt(t)
	ft.setFail("BTCUSDT", errors.New("timeout"))
	for i := 0; i < 5; i++ {
		r.step()
	}
	if btc := pairOf(t, r.eng.Status(), "BTCUSDT"); btc.State != StateEmergencyHalted || btc.Position == nil {
		t.Fatalf("BTC = %s position %+v", btc.State, btc.Position)
	}
	ft.setFail("BTCUSDT", nil)
	settleAfterTheOpenOn(r, "BTCUSDT", -0.0003) // an exit would be due
	r.market.mu.Lock()
	r.market.symbols = nil
	r.market.mu.Unlock()
	reads, closes := ft.readsOf("BTCUSDT"), r.trader.closeCount()
	r.step()
	st := r.step()
	r.market.mu.Lock()
	marketBTC := 0
	for _, s := range r.market.symbols {
		if s == "BTCUSDT" {
			marketBTC++
		}
	}
	r.market.mu.Unlock()
	if ft.readsOf("BTCUSDT")-reads != 2 || marketBTC != 0 || r.trader.closeCount() != closes {
		t.Errorf("halted BTC over two scans: %d holding reads (want 2), %d market reads (want 0), %d closes", ft.readsOf("BTCUSDT")-reads, marketBTC, r.trader.closeCount()-closes)
	}
	if btc := pairOf(t, st, "BTCUSDT"); btc.HedgeStatus != HedgeBothOpen || btc.State != StateEmergencyHalted {
		t.Errorf("BTC = %s %s", btc.State, btc.HedgeStatus)
	}
}

// A refused open is not a reading: the pair's size is unknown until read, but
// the time of its last reading does not move (review round 6).
func TestPortfolio_ARefusedOpenDoesNotStampAReading(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	r := newRigAt(t, func() time.Time { return clock() })
	r.trader.refuseOpen = true
	r.start(testConfig())
	r.trader.afterOpen = func(string) { now = now.Add(time.Minute) }
	st := r.step()
	p := pairOf(t, st, testSymbol)
	if p.HedgeStatus != HedgeUnknown || !strings.Contains(p.HedgeReasonVI, "portal từ chối") {
		t.Fatalf("BTC hedge = %s %q", p.HedgeStatus, p.HedgeReasonVI)
	}
	if want := now.Add(-time.Minute).UnixMilli(); p.HoldingReadAtMs != want {
		t.Errorf("reading time %d, want the entry's read at %d — the refusal stamped a reading", p.HoldingReadAtMs, want)
	}
}

// The portal busy on an exit: the scan's entries wait too, and a pair whose
// entry was dropped shows neither a rank nor an old skip (review round 6).
func TestPortfolio_ABusyExitDropsTheScansEntriesAndTheirRanks(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"BTCUSDT": 0.0003, "ETHUSDT": -0.0001, "SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	pc := testPortfolio(allSymbols...)
	pc.MaxConcurrentPositions = 1
	r.start(pc)
	r.step()
	r.wantHedgedOn("BTCUSDT")
	withRates(r, map[string]float64{"ETHUSDT": 0.0002})
	st := r.step()
	if eth := pairOf(t, st, "ETHUSDT"); eth.SkipVI == "" {
		t.Fatalf("ETH was not skipped for capacity: %+v", eth)
	}
	// BTC's funding turns negative (an exit is due) while the portal is busy.
	settleAfterTheOpenOn(r, "BTCUSDT", -0.0003)
	r.trader.mu.Lock()
	r.trader.busy = true
	r.trader.mu.Unlock()
	st = r.step()
	eth := pairOf(t, st, "ETHUSDT")
	if eth.Position != nil || eth.SkipVI != "" || eth.Rank != 0 || eth.State != StateIdleScanning {
		t.Errorf("ETH after a busy exit = position %+v skip %q rank %d state %s: %v", eth.Position, eth.SkipVI, eth.Rank, eth.State, logLines(st))
	}
	// The exit itself is retried: BTC is back in position, and closes once the
	// portal is free (review round 7).
	if btc := pairOf(t, st, "BTCUSDT"); btc.State != StateInPosition || btc.Position == nil {
		t.Fatalf("BTC after a busy exit = %s position %+v — a pair left closing is never retried", btc.State, btc.Position)
	}
	r.trader.mu.Lock()
	r.trader.busy = false
	r.trader.mu.Unlock()
	st = r.step()
	if btc := pairOf(t, st, "BTCUSDT"); btc.Position != nil {
		t.Errorf("BTC after the portal was free = %s position %+v", btc.State, btc.Position)
	}
	r.wantFlatOn("BTCUSDT")
}

// An open that alarms is held, for the rest of its own scan, to the size it
// sent: the pair's newest reading is the flat one taken before the open, and
// the next entry must not fit into the cap beside legs that may be on the venue
// (review round 7: the round-6 rule held it, no test did).
func TestPortfolio_AnAlarmedOpenHoldsItsScansNextEntryToTheCap(t *testing.T) {
	r := newRig(t)
	withRates(r, map[string]float64{"BTCUSDT": 0.0003, "ETHUSDT": 0.0002, "SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	r.trader.alarmOpen = true
	pc := testPortfolio(allSymbols...)
	pc.TotalCapitalCapQuote = 1.5 * testCapital // one pair fits, a second does not
	r.start(pc)
	st := r.step()
	if btc := pairOf(t, st, "BTCUSDT"); btc.State != StateEmergencyHalted {
		t.Fatalf("BTC = %s", btc.State)
	}
	if n := r.trader.openCount(); n != 1 {
		t.Errorf("%d opens in the alarm's scan, want BTC's alone", n)
	}
	if eth := pairOf(t, st, "ETHUSDT"); eth.Position != nil || !strings.Contains(eth.SkipVI, "vượt hạn mức") {
		t.Errorf("ETH = position %+v skip %q", eth.Position, eth.SkipVI)
	}
}

// Legs held under several intents state no size even if a trader reported a
// notional beside them: only one intent's notional bounds its legs (review
// round 7: the guard was pinned by neither trader, which both report 0).
func TestPortfolio_SeveralIntentsStateNoSizeWhateverNotionalIsReported(t *testing.T) {
	r, ft := newFlakyRig(t)
	withRates(r, map[string]float64{"BNBUSDT": -0.0001})
	ft.setHook(func(symbol string, h Holding, err error) (Holding, error) {
		if symbol == "BTCUSDT" {
			return Holding{Status: HedgeBothOpen, ReasonVI: "test", HeldIntents: 2, NotionalQuote: 65, ToleranceQtyCoin: perpStep}, nil
		}
		return h, err
	})
	r.start(testPortfolio(allSymbols...))
	st := r.step()
	if n := r.trader.openCount(); n != 0 {
		t.Errorf("%d opens beside two intents of unknown total size", n)
	}
	if len(st.UnsizedPairs) != 1 || st.UnsizedPairs[0] != "BTCUSDT" {
		t.Errorf("unsized = %v", st.UnsizedPairs)
	}
}

// ------------------------- the convergence-and-amortization set (4.5f)

// closeReasonsOf copies what the engine handed the Trader.
func closeReasonsOf(r *rig) []string {
	r.trader.mu.Lock()
	defer r.trader.mu.Unlock()
	return append([]string(nil), r.trader.closeReasons...)
}

// Trụ cột 1 through the whole loop: the bot does not open a pair whose perp is
// not trading above its spot, and it does open once the premium is back.
func TestEngine_DoesNotOpenOnAFlatBasisAndOpensWhenThePremiumReturns(t *testing.T) {
	r := newRig(t)
	pc := testConfig()
	pc.DefaultPairConfig.MinEntryBasisBps = DefaultMinEntryBasisBps
	r.start(pc)

	r.market.set(func(s *Snapshot) { s.PerpBook = book("binance_futures", testMid, time.Now()) }) // basis 0
	st := r.step()
	r.wantState(StateIdleScanning)
	r.wantVenueFlat()
	if r.trader.opens != 0 {
		t.Errorf("the bot opened on a flat basis: %d opens", r.trader.opens)
	}
	if !hasLogOn(st, "SCAN", testSymbol, "Basis lúc vào") {
		t.Errorf("the console does not name the basis floor: %v", logLines(st))
	}
	// An inverted basis is refused the same way — the dip the strategy doc
	// names, where the book recovers and the widening stop then fires.
	r.market.set(func(s *Snapshot) { s.PerpBook = book("binance_futures", testMid*0.9962, time.Now()) })
	r.step()
	r.wantState(StateIdleScanning)
	if r.trader.opens != 0 {
		t.Errorf("the bot opened on an inverted basis: %d opens", r.trader.opens)
	}
	// Back to the premium: the same pair, the same reading in every other
	// respect, opens.
	r.market.set(func(s *Snapshot) { s.PerpBook = book("binance_futures", testPerpMid, time.Now()) })
	r.step()
	r.wantState(StateInPosition)
	r.wantVenueHedged()
}

// Trụ cột 2 through the whole loop: a negative settlement inside the
// amortization floor sends no order.
func TestEngine_TheAmortizationFloorSendsNoOrderOnASingleCharge(t *testing.T) {
	r := newRig(t)
	pc := testConfig()
	pc.DefaultPairConfig.MinHoldEpochs = DefaultMinHoldEpochs
	r.start(pc)
	r.step()
	r.wantState(StateInPosition)

	closes := r.trader.closes
	settleAfterTheOpen(r, -0.00005) // -0.5 bps: the print the retired rule left on
	st := r.step()
	r.wantState(StateInPosition)
	r.wantVenueHedged()
	if r.trader.closes != closes {
		t.Fatalf("the bot paid a round trip inside the floor: closes %d→%d · %v", closes, r.trader.closes, logLines(st))
	}
	p := pairOf(t, st, testSymbol)
	if p.Signal == nil || p.Signal.ExitDue || p.Position == nil || p.Position.SettlementsSinceOpen != 1 {
		t.Errorf("held pair = %+v", p.Signal)
	}
}

// Trụ cột 3 through the whole loop: a converged basis closes the pair, and the
// sentence the intent file keeps is the one the take-profit wrote.
func TestEngine_TakesProfitOnConvergenceAndNamesTheReason(t *testing.T) {
	r := newRig(t)
	pc := testConfig()
	pc.DefaultPairConfig.MinHoldEpochs = DefaultMinHoldEpochs // the floor is no obstacle to a take-profit
	r.start(pc)
	r.step()
	r.wantState(StateInPosition)

	// The perp falls 1.5% towards and past the spot: the SHORT leg gains it,
	// which on a $65 pair at 1.5× capital is well past the +0.50% target. (A
	// full percent is not: the fake venue fills each leg about 0.1% off its
	// mid, so the entry's own slippage eats a third of the move.)
	r.market.set(func(s *Snapshot) { s.PerpBook = book("binance_futures", testMid*0.985, time.Now()) })
	st := r.step()
	r.wantState(StateCooldown)
	r.wantVenueFlat()

	reasons := closeReasonsOf(r)
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0], "Chốt lời hội tụ Basis: Net PnL ") || !strings.Contains(reasons[0], "≥ ngưỡng +0.50%") {
		t.Fatalf("the reason handed to Close = %q", reasons)
	}
	if !hasLogOn(st, "EXIT", testSymbol, "Chốt lời hội tụ Basis") {
		t.Errorf("the console does not name the take-profit: %v", logLines(st))
	}
	// A basis that moved a hundred bps the other way is NOT a widening: the
	// stop must not be what closed this.
	if strings.Contains(reasons[0], "Basis giãn") {
		t.Errorf("the widening stop fired on a convergence: %q", reasons[0])
	}
}

// ------------------------- buffered-slot sizing and rebalancing (4.5g)

// The first scan of a run sizes every slot from the account, so a bot switched
// on with the shipped seed does not trade a week at 65.
func TestEngine_TheFirstScanSizesTheSlotsFromTheAccount(t *testing.T) {
	r := newRig(t)
	r.trader.setAccount(10_000, 5_000, nil)
	pc := testPortfolio(testSymbol)
	pc.AutoRebalance = true
	pc.MaxConcurrentPositions = 6
	pc.TotalCapitalCapQuote = 100_000
	pc.DefaultPairConfig.NotionalQuote = DefaultNotionalQuote // the seed, not a size
	r.start(pc)

	st := r.step()
	// 15,000 × 0.70 = 10,500 tradable ÷ 6 slots ÷ 1.5 = 1,166.67 a leg.
	want := 10_500.0 / 6 / 1.5
	got := r.eng.Status().Portfolio.DefaultPairConfig.NotionalQuote
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("sized to %v, want %v · %v", got, want, logLines(st))
	}
	if !hasLog(st, "REBALANCE", "tổng vốn 15000.00 quote") || !hasLog(st, "REBALANCE", "đệm 30%") {
		t.Errorf("the console does not show the arithmetic: %v", logLines(st))
	}
	// The pair it opened used the NEW size, not the seed.
	p := r.wantState(StateInPosition)
	if p.Position == nil || math.Abs(p.Position.NotionalQuote-want) > 1e-9 {
		t.Errorf("opened at %+v, want the rebalanced size %v", p.Position, want)
	}
	if st := r.eng.Status(); st.Portfolio.LastRebalancedAtMs == 0 || st.Portfolio.NextRebalanceAtMs <= st.Portfolio.LastRebalancedAtMs {
		t.Errorf("schedule = last %d next %d", st.Portfolio.LastRebalancedAtMs, st.Portfolio.NextRebalanceAtMs)
	}
}

// The rule that costs money if it is ever broken: a rebalance moves the size of
// FUTURE opens and never touches a position already on the venue.
func TestEngine_ARebalanceNeverClosesOrResizesAnOpenPosition(t *testing.T) {
	var clockMu sync.Mutex
	at := time.Now()
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return at
	}
	r := newRigAt(t, now)
	r.trader.setAccount(10_000, 5_000, nil)
	pc := testPortfolio(allSymbols...)
	pc.AutoRebalance = true
	pc.MaxConcurrentPositions = 4
	pc.TotalCapitalCapQuote = 100_000
	// Only BTC pays, so exactly one pair is held across the rebalance.
	withRates(r, map[string]float64{"BTCUSDT": 0.0003, "ETHUSDT": -0.0001, "SOLUSDT": -0.0001, "BNBUSDT": -0.0001})
	r.start(pc)
	r.step()
	held := r.wantState(StateInPosition).Position
	if held == nil {
		t.Fatal("no position to carry across the rebalance")
	}
	openedAt, openedQty, openedNotional := held.OpenedAtMs, held.QtyCoin, held.NotionalQuote
	venueQty := perpQtyOf(t, r, testSymbol)
	closes, opens := r.trader.closes, r.trader.opens

	// A week passes and the account has doubled.
	clockMu.Lock()
	at = at.Add(8 * 24 * time.Hour)
	clockMu.Unlock()
	r.trader.setAccount(20_000, 10_000, nil)
	// The pair's own history has to reach the new instant, or the hold goes
	// blind on funding and halts for a reason that is not this test's.
	r.market.set(func(s *Snapshot) { *s = *goodSnapshot(now(), 0.0003) })
	st := r.step()

	if !hasLog(st, "REBALANCE", "Vị thế ĐANG MỞ giữ nguyên quy mô cũ") {
		t.Fatalf("no rebalance line: %v", logLines(st))
	}
	if r.trader.closes != closes {
		t.Errorf("the rebalance sent %d closes", r.trader.closes-closes)
	}
	// Read from the VENUE, not from the engine's belief: the legs are exactly
	// as they were.
	if got := perpQtyOf(t, r, testSymbol); math.Abs(got-venueQty) > 1e-12 {
		t.Errorf("venue perp %v, was %v — the rebalance moved an open leg", got, venueQty)
	}
	after := pairOf(t, st, testSymbol).Position
	if after == nil || after.OpenedAtMs != openedAt || after.QtyCoin != openedQty || after.NotionalQuote != openedNotional {
		t.Errorf("position after the rebalance = %+v, want the one it opened with (%v coin, %v quote)", after, openedQty, openedNotional)
	}
	// The run's DEFAULT did move, so the next pair opens at the new size.
	sized := r.eng.Status().Portfolio.DefaultPairConfig.NotionalQuote
	if want := 30_000 * 0.7 / 4 / 1.5; math.Abs(sized-want) > 1e-9 {
		t.Errorf("default sized to %v, want %v", sized, want)
	}
	if r.trader.opens != opens {
		// Only BTC pays here, so nothing new should have opened either.
		t.Errorf("the rebalance's scan opened %d pairs", r.trader.opens-opens)
	}
}

// The clock: a size stands for the whole interval and is re-read after it.
func TestEngine_RebalanceRunsOnItsScheduleAndNotOnEveryScan(t *testing.T) {
	var clockMu sync.Mutex
	at := time.Now()
	r := newRigAt(t, func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return at
	})
	r.trader.setAccount(10_000, 5_000, nil)
	pc := testPortfolio(testSymbol)
	pc.AutoRebalance = true
	pc.RebalanceIntervalHours = 168
	pc.TotalCapitalCapQuote = 100_000
	r.start(pc)

	r.step()
	if n := r.trader.reads(); n != 1 {
		t.Fatalf("%d account reads on the first scan, want 1", n)
	}
	first := r.eng.Status().Portfolio.DefaultPairConfig.NotionalQuote

	// Three more scans well inside the interval: the account is not read again
	// and the size does not move.
	r.trader.setAccount(50_000, 25_000, nil)
	for i := 0; i < 3; i++ {
		clockMu.Lock()
		at = at.Add(time.Hour)
		clockMu.Unlock()
		r.step()
	}
	if n := r.trader.reads(); n != 1 {
		t.Errorf("%d account reads inside the interval, want 1", n)
	}
	if got := r.eng.Status().Portfolio.DefaultPairConfig.NotionalQuote; got != first {
		t.Errorf("size moved inside the interval: %v → %v", first, got)
	}

	// Past the interval: read once, and once only.
	clockMu.Lock()
	at = at.Add(168 * time.Hour)
	clockMu.Unlock()
	r.step()
	if n := r.trader.reads(); n != 2 {
		t.Errorf("%d account reads after the interval, want 2", n)
	}
	if got := r.eng.Status().Portfolio.DefaultPairConfig.NotionalQuote; got <= first {
		t.Errorf("size after a five-fold account = %v, was %v", got, first)
	}
	r.step()
	if n := r.trader.reads(); n != 2 {
		t.Errorf("%d account reads on the scan after a rebalance, want 2", n)
	}
}

// A run with the switch off never reads the account and never re-sizes.
func TestEngine_RebalanceOffLeavesTheSizeTheOperatorTyped(t *testing.T) {
	r := newRig(t)
	r.trader.setAccount(1_000_000, 1_000_000, nil)
	pc := testPortfolio(testSymbol) // AutoRebalance false
	r.start(pc)
	r.step()
	if n := r.trader.reads(); n != 0 {
		t.Errorf("%d account reads with the rebalance off", n)
	}
	if got := r.eng.Status().Portfolio.DefaultPairConfig.NotionalQuote; got != testNotional {
		t.Errorf("size = %v, want the %v the operator typed", got, testNotional)
	}
}

// A balance the venue would not give up leaves the size alone — and stays OWED,
// so the next scan tries again rather than waiting out the whole interval.
func TestEngine_AFailedBalanceReadKeepsTheSizeAndRetriesNextScan(t *testing.T) {
	r := newRig(t)
	r.trader.setAccount(0, 0, errors.New("timeout"))
	pc := testPortfolio(testSymbol)
	pc.AutoRebalance = true
	pc.TotalCapitalCapQuote = 100_000
	r.start(pc)

	st := r.step()
	if got := r.eng.Status().Portfolio.DefaultPairConfig.NotionalQuote; got != testNotional {
		t.Errorf("size after a failed read = %v, want the %v it had", got, testNotional)
	}
	if !hasLog(st, "REBALANCE", "không đọc được số dư hai ví") {
		t.Errorf("the console does not name the failure: %v", logLines(st))
	}
	if r.eng.Status().Portfolio.LastRebalancedAtMs != 0 {
		t.Error("a failed read moved the schedule — the rebalance is owed until it succeeds")
	}
	// It is still owed, so the very next scan asks again.
	r.trader.setAccount(10_000, 5_000, nil)
	r.step()
	if n := r.trader.reads(); n != 2 {
		t.Errorf("%d account reads, want a retry on the next scan", n)
	}
	if got := r.eng.Status().Portfolio.DefaultPairConfig.NotionalQuote; got == testNotional {
		t.Errorf("size after the retry = %v — it did not take", got)
	}
}

// An account too small to fund a slot leaves the size where it is rather than
// setting one no order could use.
func TestEngine_AnEmptyAccountLeavesTheSizeAloneAndSaysSo(t *testing.T) {
	r := newRig(t)
	r.trader.setAccount(0, 0, nil)
	pc := testPortfolio(testSymbol)
	pc.AutoRebalance = true
	r.start(pc)

	st := r.step()
	if got := r.eng.Status().Portfolio.DefaultPairConfig.NotionalQuote; got != testNotional {
		t.Errorf("size on an empty account = %v", got)
	}
	if !hasLog(st, "REBALANCE", "vốn không đủ") {
		t.Errorf("console: %v", logLines(st))
	}
}

// A pair the operator sized by hand is not re-sized by an automatic rule, and
// the console says which ones were left alone.
func TestEngine_ARebalanceLeavesAPairsOwnConfigAlone(t *testing.T) {
	r := newRig(t)
	r.trader.setAccount(10_000, 5_000, nil)
	pc := testPortfolio(testSymbol, "ETHUSDT")
	pc.AutoRebalance = true
	pc.MaxConcurrentPositions = 4
	pc.TotalCapitalCapQuote = 100_000
	own := pc.DefaultPairConfig
	own.Symbol, own.NotionalQuote = "ETHUSDT", 321
	pc.PairOverrides = map[string]Config{"ETHUSDT": own}
	r.start(pc)

	st := r.step()
	if !hasLog(st, "REBALANCE", "Giữ cấu hình riêng (không đổi quy mô): ETHUSDT") {
		t.Errorf("console: %v", logLines(st))
	}
	if eth := pairOf(t, st, "ETHUSDT"); eth.Config.NotionalQuote != 321 {
		t.Errorf("ETH sized to %v, want the 321 the operator named", eth.Config.NotionalQuote)
	}
	if btc := pairOf(t, st, testSymbol); btc.Config.NotionalQuote == testNotional {
		t.Errorf("BTC was not re-sized: %v", btc.Config.NotionalQuote)
	}
}

// perpQtyOf reads one symbol's perp position from the FAKE VENUE, never from
// the engine (the 4.4a lesson).
func perpQtyOf(t *testing.T, r *rig, symbol string) float64 {
	t.Helper()
	pos, err := r.trader.venues[symbol].perp.GetPosition(context.Background(), broker.MarketFuturesUSDM, symbol)
	if err != nil {
		t.Fatal(err)
	}
	return pos.QtyCoin
}

// A wallet that stays unreadable is retried on every scan, so its line must not
// be written on every scan: the console keeps 60 entries and a repeating
// failure would push every other event out of it within ten minutes.
func TestEngine_ARepeatingRebalanceFailureIsLoggedOnce(t *testing.T) {
	r := newRig(t)
	r.trader.setAccount(0, 0, errors.New("timeout"))
	pc := testPortfolio(testSymbol)
	pc.AutoRebalance = true
	pc.TotalCapitalCapQuote = 100_000
	r.start(pc)

	for i := 0; i < 4; i++ {
		r.step()
	}
	st := r.eng.Status()
	lines := 0
	for _, e := range st.Log {
		if e.Kind == "REBALANCE" {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("%d REBALANCE lines for one repeating failure: %v", lines, logLines(st))
	}
	if r.trader.reads() != 4 {
		t.Errorf("%d account reads — a failure must stay owed and be retried", r.trader.reads())
	}
	// A different failure is a different line, and a success clears the key so
	// the next failure of the first kind is reported again.
	r.trader.setAccount(0, 0, errors.New("banned"))
	r.step()
	r.trader.setAccount(10_000, 5_000, nil)
	r.step()
	r.trader.setAccount(0, 0, errors.New("timeout"))
	// Past the interval, so a read is owed again.
	pcNow := r.eng.Status().Portfolio
	if pcNow.LastRebalancedAtMs == 0 {
		t.Fatal("the successful rebalance did not take")
	}
	r.eng.mu.Lock()
	r.eng.pcfg.LastRebalancedAtMs = 1
	r.eng.mu.Unlock()
	r.step()
	lines = 0
	for _, e := range r.eng.Status().Log {
		if e.Kind == "REBALANCE" {
			lines++
		}
	}
	if lines != 4 {
		t.Errorf("%d REBALANCE lines, want timeout · banned · the success · timeout again: %v", lines, logLines(r.eng.Status()))
	}
}

// After a rebalance the run's slot size and a held position's size differ. The
// held pair's gauge must describe the legs on the venue, not the new slot.
func TestEngine_AHeldPairIsPricedAtItsOwnSizeAfterARebalance(t *testing.T) {
	var clockMu sync.Mutex
	at := time.Now()
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return at
	}
	r := newRigAt(t, now)
	r.trader.setAccount(10_000, 5_000, nil)
	pc := testPortfolio(testSymbol)
	pc.AutoRebalance = true
	pc.MaxConcurrentPositions = 4
	pc.TotalCapitalCapQuote = 100_000
	r.start(pc)
	r.step()
	held := r.wantState(StateInPosition).Position
	if held == nil {
		t.Fatal("nothing opened")
	}

	clockMu.Lock()
	at = at.Add(8 * 24 * time.Hour)
	clockMu.Unlock()
	r.trader.setAccount(40_000, 20_000, nil) // four times the equity
	r.market.set(func(s *Snapshot) { *s = *goodSnapshot(now(), 0.0003) })
	st := r.step()

	slot := st.Portfolio.DefaultPairConfig.NotionalQuote
	p := pairOf(t, st, testSymbol)
	if p.Position == nil || math.Abs(p.Position.NotionalQuote-held.NotionalQuote) > 1e-9 {
		t.Fatalf("the position was re-sized: %+v", p.Position)
	}
	if slot <= held.NotionalQuote {
		t.Fatalf("the slot did not grow: %v against the held %v", slot, held.NotionalQuote)
	}
	// The gauge's required depth is DepthMultiple × notional, so it is the
	// cheapest place to see which notional priced the reading.
	if p.Signal == nil || math.Abs(p.Signal.RequiredDepthQuote-p.Config.DepthMultiple*held.NotionalQuote) > 1e-9 {
		t.Errorf("held pair priced at %v of depth, want %v × its own %v",
			p.Signal.RequiredDepthQuote, p.Config.DepthMultiple, held.NotionalQuote)
	}
}
