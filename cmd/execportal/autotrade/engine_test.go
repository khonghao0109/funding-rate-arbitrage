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
// in-memory venues. The Trader below is a test double of the portal's order
// path, but everything under it is production code: execution.Open places two
// legs and unwinds one when the other fails, execution.Close proves both flat.
// Every assertion about a position reads the FAKE VENUES, never the engine's
// own belief — a machine that has lost a leg reports itself hedged with
// complete confidence (the 4.4a lesson).

const (
	testSymbol = "BTCUSDT"
	testMid    = 77_000.0
	perpStep   = 0.0001
	spotStep   = 0.00001
)

func spotRules() exchanges.Instrument {
	return exchanges.Instrument{Symbol: testSymbol, Source: "binance_spot", MarketType: "spot", Status: exchanges.StatusTrading,
		TickSizeQuote: 0.01, StepSizeCoin: spotStep, MinQtyCoin: spotStep, MaxQtyCoin: 9000, MinNotionalQuote: 5,
		ContractSizeCoin: 1, BaseAsset: "BTC", QuoteAsset: "USDT"}
}

func perpRules() exchanges.Instrument {
	return exchanges.Instrument{Symbol: testSymbol, Source: "binance_futures", MarketType: "perp", Status: exchanges.StatusTrading,
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
	openedAtMs       int64
	spotMid, perpMid float64
	spotAvg, perpAvg float64
	fromAutotrade    bool
}

// venueTrader is the portal's order path in miniature: execution over two fakes,
// hedge status read back from the venues by each intent's derived order ids.
type venueTrader struct {
	t          *testing.T
	spot, perp *brokertest.Fake

	mu          sync.Mutex
	intents     []*intentRecord
	seq         int
	opens       int
	closes      int
	busy        bool
	holdErr     error
	refuseOpen  bool
	refuseClose bool
	alarmOpen   bool          // the open returns an ALARM after placing
	openGate    chan struct{} // when set, Open waits on it (or on ctx) before placing
	closeGate   chan struct{} // when set, Close waits on it before closing
}

func newVenueTrader(t *testing.T) *venueTrader {
	spot, perp := brokertest.New(), brokertest.New()
	spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1})
	perp.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1})
	spot.SetStepSizeCoin(spotStep)
	perp.SetStepSizeCoin(perpStep)
	spot.SetBaseAsset("BTC")
	spot.SetBalance(broker.MarketSpot, broker.Balance{Market: broker.MarketSpot, Asset: "BTC", FreeQtyCoin: 1},
		broker.Balance{Market: broker.MarketSpot, Asset: "USDT", FreeQtyCoin: 10_000})
	perp.SetMarkPrice(broker.MarkPrice{MarkPriceQuote: testMid})
	return &venueTrader{t: t, spot: spot, perp: perp}
}

// closeCount is how many closes were asked for, read under the lock — the kill
// that asks runs on another goroutine.
func (v *venueTrader) closeCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.closes
}

// legNet is one leg's signed filled quantity over every id an intent derives.
func legNet(ctx context.Context, b broker.Broker, market broker.Market, id string, leg execution.LegName) (float64, error) {
	net := 0.0
	for _, cid := range []string{execution.LegClientOrderID(id, leg), execution.CloseClientOrderID(id, leg),
		execution.UnwindClientOrderID(id, leg), execution.ReconcileClientOrderID(id, leg)} {
		o, err := b.GetOrder(ctx, broker.OrderQuery{Market: market, Symbol: testSymbol, ClientOrderID: cid})
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

// venueLegs is what the two venues hold for the strategy: the spot leg summed
// from every intent's own orders, and the venue's perp position.
func (v *venueTrader) venueLegs(t *testing.T) (spotQty, perpQty float64) {
	t.Helper()
	ctx := context.Background()
	v.mu.Lock()
	intents := append([]*intentRecord(nil), v.intents...)
	v.mu.Unlock()
	for _, in := range intents {
		s, err := legNet(ctx, v.spot, broker.MarketSpot, in.id, execution.LegSpot)
		if err != nil {
			t.Fatal(err)
		}
		spotQty += s
	}
	pos, err := v.perp.GetPosition(ctx, broker.MarketFuturesUSDM, testSymbol)
	if err != nil {
		t.Fatal(err)
	}
	return spotQty, pos.QtyCoin
}

func (v *venueTrader) Holding(ctx context.Context, symbol string) (Holding, error) {
	v.mu.Lock()
	if err := v.holdErr; err != nil {
		v.mu.Unlock()
		return Holding{}, err
	}
	intents := append([]*intentRecord(nil), v.intents...)
	v.mu.Unlock()

	pos, err := v.perp.GetPosition(ctx, broker.MarketFuturesUSDM, symbol)
	if err != nil {
		return Holding{}, err
	}
	h := Holding{}
	var spotSum, perpSum float64
	var held *intentRecord
	for _, in := range intents {
		s, err := legNet(ctx, v.spot, broker.MarketSpot, in.id, execution.LegSpot)
		if err != nil {
			return Holding{}, err
		}
		p, err := legNet(ctx, v.perp, broker.MarketFuturesUSDM, in.id, execution.LegPerp)
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
		h.IntentID, h.FromAutotrade = held.id, held.fromAutotrade
		h.OpenedAtMs, h.SpotRefMidQuote, h.PerpRefMidQuote, h.QtyCoin = held.openedAtMs, held.spotMid, held.perpMid, -pos.QtyCoin
	}
	return h, nil
}

func (v *venueTrader) intent(id string, at time.Time) execution.Intent {
	return execution.Intent{
		ID: id, Symbol: testSymbol, SpotInstrument: spotRules(), PerpInstrument: perpRules(),
		SpotBook: book("binance_spot", testMid, at), PerpBook: book("binance_futures", testMid, at),
		SpotPriceQuote: testMid, PerpPriceQuote: testMid, PerpMarginFrac: 0.5,
		PerpBracket: risk.Bracket{Source: "binance_futures", MaintenanceMarginFrac: 0.004, TierCeilingQuote: 50_000, MaxLeverage: 125, Verified: true},
	}
}

func (v *venueTrader) Open(ctx context.Context, order OpenOrder) OpenResult {
	v.mu.Lock()
	if v.busy {
		v.mu.Unlock()
		return OpenResult{Busy: true, ErrorVI: "bận"}
	}
	gate, refuse, alarm := v.openGate, v.refuseOpen, v.alarmOpen
	v.opens++
	v.seq++
	id := fmt.Sprintf("abtcusdt-test-%03d", v.seq)
	v.mu.Unlock()
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
	now := time.Now()
	in := v.intent(id, now)
	in.NotionalQuote = order.NotionalQuote
	in.SignalEntryCostPct = order.SignalEntryCostPct
	cfg := execution.DefaultConfig()
	cfg.LegTimeout = 2 * time.Second
	tr, err := execution.NewOpener(v.spot, v.perp, cfg, execution.NewMemoryRecorder())
	if err != nil {
		v.t.Fatal(err)
	}
	res, openErr := tr.Open(ctx, in)
	rec := &intentRecord{id: id, openedAtMs: now.UnixMilli(), spotMid: testMid, perpMid: testMid,
		spotAvg: res.Spot.AvgFillPriceQuote, perpAvg: res.Perp.AvgFillPriceQuote, fromAutotrade: true}
	v.mu.Lock()
	v.intents = append(v.intents, rec)
	v.mu.Unlock()
	out := OpenResult{IntentID: id, Hedged: res.Hedged(), OpenedAtMs: rec.openedAtMs, QtyCoin: res.Perp.FilledQtyCoin,
		ResidualQtyCoin: res.ResidualQtyCoin, SpotRefMidQuote: testMid, PerpRefMidQuote: testMid,
		UnhedgedWindowMs: res.UnhedgedWindow.Milliseconds(), UnwindDurationMs: res.UnwindDuration.Milliseconds(),
		Refused: errors.Is(openErr, execution.ErrRefusedBeforePlacing),
		Alarm:   errors.Is(openErr, execution.ErrUnwindIncomplete) || errors.Is(openErr, execution.ErrFlatEvidenceConflict)}
	if openErr != nil {
		out.ErrorVI = openErr.Error()
	}
	return out
}

func (v *venueTrader) Close(ctx context.Context, symbol, intentID string) CloseResult {
	v.mu.Lock()
	if v.busy {
		v.mu.Unlock()
		return CloseResult{Busy: true}
	}
	v.closes++
	gate, refuse := v.closeGate, v.refuseClose
	var rec *intentRecord
	for _, in := range v.intents {
		if in.id == intentID {
			rec = in
		}
	}
	v.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if refuse {
		return CloseResult{IntentID: intentID, Refused: true, ErrorVI: "từ chối trước khi gửi (test)"}
	}
	if rec == nil {
		return CloseResult{Refused: true, ErrorVI: "không có ý định " + intentID}
	}
	pos, err := v.perp.GetPosition(ctx, broker.MarketFuturesUSDM, symbol)
	if err != nil {
		return CloseResult{Refused: true, ErrorVI: err.Error()}
	}
	tr, err := execution.NewOpener(v.spot, v.perp, execution.DefaultConfig(), execution.NewMemoryRecorder())
	if err != nil {
		v.t.Fatal(err)
	}
	res, closeErr := tr.Close(ctx, execution.CloseRequest{
		Intent: v.intent(intentID, time.Now()), QtyCoin: -pos.QtyCoin, OpenedAtMs: rec.openedAtMs,
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

// scriptMarket answers a scan with the snapshot the test last set.
type scriptMarket struct {
	mu    sync.Mutex
	snap  Snapshot
	err   error
	calls int
	since []int64
	// afterRead, when set, runs once the reading is taken and before the
	// engine sees it — the window between a scan and its decision.
	afterRead func()
}

func (m *scriptMarket) set(fn func(s *Snapshot)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.snap)
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
	if m.err != nil {
		return Snapshot{}, m.err
	}
	now := time.Now()
	s := m.snap
	s.Symbol, s.ReadAtMs = symbol, now.UnixMilli()
	s.SpotBook.SampledAtMs, s.PerpBook.SampledAtMs = now.UnixMilli(), now.UnixMilli()
	s.Settled = nil
	if s.SettledErrVI != "" {
		return s, nil // an unreadable history carries no rows, as portalMarket's does
	}
	for _, r := range m.snap.Settled {
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

// goodMarket is a reading every entry check passes: 1 bps per 8h settled for a
// week, the testnet's fees, deep books, the next settlement two hours away.
func goodMarket(now time.Time) *scriptMarket {
	return &scriptMarket{snap: Snapshot{
		SpotBook: book("binance_spot", testMid, now), PerpBook: book("binance_futures", testMid, now),
		// The next stamp is one 8h interval after the last settled one, as the
		// venue publishes it — the cadence cross-check refuses anything else.
		ForecastRatePerIntervalFrac: 0.0001, NextFundingTimeMs: now.Add(7 * time.Hour).UnixMilli(),
		Settled:         settledEvery8h(21, 0.0001, now),
		SpotTakerFeeBps: 0, PerpTakerFeeBps: 4, FeeSourceVI: "test",
		SpotClockSkewMs: ptrInt(220), PerpClockSkewMs: ptrInt(-140),
	}}
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
	eng, err := New(Options{Market: m, Trader: tr, DefaultSymbol: testSymbol, MaxNotionalQuote: 50_000,
		PerpMarginFrac: 0.5, ActionTimeout: 30 * time.Second, Logf: t.Logf, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, eng: eng, trader: tr, market: m}
}

func testConfig() Config {
	c := DefaultConfig(testSymbol)
	c.Cooldown = time.Hour // a test steps the machine by hand; cooldown is asserted, not waited out
	return c
}

func (r *rig) start(cfg Config) {
	r.t.Helper()
	if _, err := r.eng.Start(cfg); err != nil {
		r.t.Fatalf("Start: %v", err)
	}
}

func (r *rig) step() StatusView {
	r.eng.Step(context.Background())
	return r.eng.Status()
}

func (r *rig) wantState(want State) StatusView {
	r.t.Helper()
	st := r.eng.Status()
	if st.State != want {
		r.t.Fatalf("state = %s (%s), want %s · halt %q · log %v", st.State, st.StateVI, want, st.HaltReasonVI, logLines(st))
	}
	return st
}

func (r *rig) wantVenueHedged() float64 {
	r.t.Helper()
	spotQty, perpQty := r.trader.venueLegs(r.t)
	if !(spotQty > 0) || !(perpQty < 0) || math.Abs(spotQty+perpQty) > perpStep+1e-9 {
		r.t.Fatalf("venues hold spot %v perp %v — not a hedged pair", spotQty, perpQty)
	}
	return spotQty
}

func (r *rig) wantVenueFlat() {
	r.t.Helper()
	spotQty, perpQty := r.trader.venueLegs(r.t)
	if math.Abs(spotQty) > 1e-12 || math.Abs(perpQty) > 1e-12 {
		r.t.Fatalf("venues hold spot %v perp %v — not flat", spotQty, perpQty)
	}
}

// settleAfterTheOpen lists one settlement stamped strictly after the open —
// a stamp in the open's own millisecond is not "after" it.
func settleAfterTheOpen(r *rig, rate float64) {
	time.Sleep(2 * time.Millisecond)
	r.market.set(func(s *Snapshot) {
		s.Settled = append(s.Settled, SettledRate{SettledAtMs: time.Now().UnixMilli(), RatePerIntervalFrac: rate})
	})
}

func logLines(st StatusView) []string {
	var out []string
	for _, e := range st.Log {
		out = append(out, "["+e.Kind+"] "+e.MessageVI)
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

// -------------------------------------------------------------------- tests

// The acceptance path of Q18, on fakes: Disabled → Start → a good signal → both
// legs open and hedged at the venue → a settlement pays negative → both legs
// flat at the venue → cooldown.
func TestEngine_OpensHedgedOnASignalAndClosesFlatOnAnExit(t *testing.T) {
	r := newRig(t)
	r.wantState(StateDisabled)
	if n := r.step(); n.State != StateDisabled || r.market.calls != 0 {
		t.Fatalf("a disabled bot scanned: %s, %d reads", n.State, r.market.calls)
	}

	r.start(testConfig())
	r.wantState(StateIdleScanning)
	st := r.step()
	r.wantState(StateInPosition)
	qty := r.wantVenueHedged()
	if st.Position == nil || !strings.HasPrefix(st.Position.IntentID, "abtcusdt-") || math.Abs(st.Position.QtyCoin-qty) > 1e-12 {
		t.Fatalf("position = %+v, venue spot %v", st.Position, qty)
	}
	if st.Signal == nil || !st.Signal.EntryEligible || st.Signal.NetAPRPct == nil || *st.Signal.NetAPRPct < 5 {
		t.Fatalf("signal = %+v", st.Signal)
	}
	if !hasLog(st, "OPEN", "HEDGED") {
		t.Errorf("no OPEN line: %v", logLines(st))
	}

	// Holding while nothing says leave: no order.
	opens, closes := r.trader.opens, r.trader.closes
	st = r.step()
	r.wantState(StateInPosition)
	if r.trader.opens != opens || r.trader.closes != closes {
		t.Errorf("a quiet hold sent orders: opens %d→%d closes %d→%d", opens, r.trader.opens, closes, r.trader.closes)
	}
	if st.Signal == nil || st.Signal.ExitDue || len(st.Signal.ExitChecks) != 3 {
		t.Errorf("hold signal = %+v", st.Signal)
	}

	// A settlement after the open pays negative.
	settleAfterTheOpen(r, -0.00005)
	st = r.step()
	r.wantState(StateCooldown)
	r.wantVenueFlat()
	if st.Position != nil || !hasLog(st, "EXIT", "≤ 0") || !hasLog(st, "CLOSE", "PHẲNG") {
		t.Errorf("after exit: position %+v · log %v", st.Position, logLines(st))
	}
	if st.CooldownUntilMs <= st.NowMs {
		t.Errorf("cooldown until %d, now %d", st.CooldownUntilMs, st.NowMs)
	}
	// Cooling down: nothing is read, nothing is sent.
	calls := r.market.calls
	r.step()
	if r.market.calls != calls || r.trader.opens != opens {
		t.Error("a cooling-down bot scanned or traded")
	}
}

// Leg 2 refused at the venue: execution unwinds leg 1, the bot ends flat at the
// venue and cools down — never holding a naked leg.
func TestEngine_ALeg2RefusalUnwindsToFlatAndCoolsDown(t *testing.T) {
	r := newRig(t)
	r.trader.perp.SetBehaviour(brokertest.Behaviour{RejectWith: &broker.HTTPError{StatusCode: 400, URL: "https://demo-fapi.binance.com/fapi/v1/order", Body: `{"code":-2019}`}})
	r.start(testConfig())
	st := r.step()
	r.wantState(StateCooldown)
	r.wantVenueFlat()
	if len(r.trader.spot.Orders()) == 0 {
		t.Fatal("leg 1 was never placed, so this did not test an unwind")
	}
	if st.Position != nil || st.TradeFailures != 1 || !hasLog(st, "OPEN", "PHẲNG") {
		t.Errorf("after a failed open: position %+v, trade failures %d, log %v", st.Position, st.TradeFailures, logLines(st))
	}
}

// Three failed opens in a row halt the bot; a successful scan between them does
// not reset that count.
func TestEngine_ConsecutiveFailedOpensHalt(t *testing.T) {
	r := newRig(t)
	r.trader.perp.SetBehaviour(brokertest.Behaviour{RejectWith: &broker.HTTPError{StatusCode: 400, URL: "https://demo-fapi.binance.com/fapi/v1/order"}})
	cfg := testConfig()
	cfg.Cooldown = 0
	r.start(cfg)
	for i := 0; i < 3; i++ {
		r.step()
	}
	st := r.wantState(StateEmergencyHalted)
	r.wantVenueFlat()
	if st.TradeFailures != 3 || !strings.Contains(st.HaltReasonVI, "liên tiếp") {
		t.Errorf("halt = %q after %d failures", st.HaltReasonVI, st.TradeFailures)
	}
	opens := r.trader.opens
	r.step()
	if r.trader.opens != opens {
		t.Error("a halted bot opened again")
	}
}

// KILL while holding: both legs flat at the venue at once, and the bot halted.
func TestEngine_KillWhileHoldingClosesBothLegsAndHalts(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	r.wantState(StateInPosition)
	r.wantVenueHedged()

	st, oc, err := r.eng.Kill(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r.wantState(StateEmergencyHalted)
	r.wantVenueFlat()
	if oc == nil || !oc.Attempted || !oc.Flat || st.Position != nil || !strings.Contains(st.HaltReasonVI, "KILL SWITCH") {
		t.Fatalf("kill = %+v · status position %+v halt %q", oc, st.Position, st.HaltReasonVI)
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
	if _, _, err := r.eng.Stop(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	r.wantState(StateDisabled)
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
	waitFor(t, func() bool { return r.eng.Status().State == StateOpening })

	killed := make(chan *OperatorClose)
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
	oc := <-killed
	r.wantState(StateEmergencyHalted)
	r.wantVenueFlat()
	if oc == nil || !oc.Attempted || !oc.Flat {
		t.Errorf("kill after the open returned = %+v", oc)
	}
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

// A decision already made when the operator presses DỪNG, KILL or the portal
// shuts down is dropped: the books were read, the bot would have opened, and
// nothing is sent (review of Q18: a stop let a decided open through).
func TestEngine_AnInterruptionBetweenTheReadAndTheDecisionSendsNothing(t *testing.T) {
	for name, interrupt := range map[string]func(r *rig, cancel context.CancelFunc){
		"stop": func(r *rig, _ context.CancelFunc) {
			go func() { _, _, _ = r.eng.Stop(context.Background(), false) }()
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
			if r.trader.opens != 0 || len(r.trader.spot.Orders())+len(r.trader.perp.Orders()) != 0 {
				t.Errorf("%s between the read and the decision: %d opens, %d orders", name, r.trader.opens, len(r.trader.spot.Orders()))
			}
			if st := r.eng.Status(); st.State == StateInPosition || st.State == StateOpening {
				t.Errorf("state after %s = %s", name, st.State)
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
		go func() { _, _, _ = r.eng.Stop(context.Background(), false) }()
		waitFor(t, func() bool { return r.eng.Status().Busy == "stop" })
	}
	r.eng.Step(context.Background())
	waitFor(t, func() bool { return r.eng.Status().Busy == "" })
	if r.trader.closeCount() != 0 {
		t.Errorf("a stop that keeps the position let a due exit close it (%d closes)", r.trader.closeCount())
	}
	r.wantState(StateDisabled)
	r.wantVenueHedged()
}

// A stop waiting for an open that returns an ALARM must not acknowledge the halt
// that alarm raises: the stop is refused, the halt and its reason stay.
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
	waitFor(t, func() bool { return r.eng.Status().State == StateOpening })
	stopErr := make(chan error, 1)
	go func() {
		_, _, err := r.eng.Stop(context.Background(), false)
		stopErr <- err
	}()
	waitFor(t, func() bool { return r.eng.Status().Busy == "stop" })
	close(gate)
	<-stepped
	if err := <-stopErr; !errors.Is(err, ErrHaltedWhileStopping) || !strings.Contains(err.Error(), "BÁO ĐỘNG") {
		t.Errorf("a stop through an alarm = %v", err)
	}
	st := r.wantState(StateEmergencyHalted)
	if !strings.Contains(st.HaltReasonVI, "BÁO ĐỘNG") || st.Busy != "" {
		t.Errorf("halt %q busy %q", st.HaltReasonVI, st.Busy)
	}
	// Acknowledged by a stop pressed AFTER the halt was on screen.
	if _, _, err := r.eng.Stop(context.Background(), false); err != nil {
		t.Fatal(err)
	}
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
	waitFor(t, func() bool { return r.eng.Status().State == StateOpening })
	killed := make(chan StatusView, 1)
	go func() {
		st, _, _ := r.eng.Kill(context.Background())
		killed <- st
	}()
	waitFor(t, func() bool { return r.eng.Status().Busy == "kill" })
	close(gate)
	<-stepped
	st := <-killed
	if st.State != StateEmergencyHalted || st.Position == nil || !strings.HasPrefix(st.Position.IntentID, "abtcusdt-") {
		t.Fatalf("kill with a refused close after an open in flight = %s position %+v", st.State, st.Position)
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
	st, out, err := r.eng.Stop(context.Background(), true)
	if err != nil || out.Close == nil || !out.Close.NotTheBots || st.State != StateDisabled || r.trader.closeCount() != 0 {
		t.Fatalf("stop and close beside a manual pair = %+v, %v, %s, %d closes", out.Close, err, st.State, r.trader.closeCount())
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
	cfg.Cooldown = 0
	r.start(cfg)
	r.step()
	r.wantState(StateInPosition)
	// Blind, then a basis exit closes the pair while still blind.
	r.market.set(func(s *Snapshot) { s.SettledErrVI = "timeout" })
	r.step()
	r.market.set(func(s *Snapshot) { s.PerpBook = book("binance_futures", testMid*1.004, time.Now()) })
	r.step()
	r.wantVenueFlat()
	// The history reads again, and the very next scan opens a new pair — no
	// readable HOLDING scan in between that would reset the timer by itself.
	r.market.set(func(s *Snapshot) {
		s.SettledErrVI = ""
		s.PerpBook = book("binance_futures", testMid, time.Now())
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
	r.wantState(StateCooldown)
	if st.TradeFailures != 1 || !hasLog(st, "OPEN", "từ chối") {
		t.Errorf("refused open: failures %d, log %v", st.TradeFailures, logLines(st))
	}
}

// A refused close keeps the pair — counted, held, retried — and is never read as
// flat; three in a row halt with the position still open.
func TestEngine_ARefusedCloseKeepsThePairAndHaltsAfterThree(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	r.trader.refuseClose = true
	settleAfterTheOpen(r, -0.00005)
	for i := 1; i <= 2; i++ {
		st := r.step()
		r.wantState(StateInPosition)
		if st.TradeFailures != i || st.Position == nil {
			t.Fatalf("after refused close %d: failures %d, position %+v", i, st.TradeFailures, st.Position)
		}
	}
	r.step()
	r.wantState(StateEmergencyHalted)
	r.wantVenueHedged()
}

// Two intents holding the symbol while the bot holds one: the bot cannot name
// what it would close, so it halts and closes nothing.
func TestEngine_ASecondHeldIntentHaltsTheBot(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	r.wantState(StateInPosition)
	if res := r.trader.Open(context.Background(), OpenOrder{Symbol: testSymbol, NotionalQuote: 65, SignalEntryCostPct: 1}); !res.Hedged {
		t.Fatalf("second open = %+v", res)
	}
	closes := r.trader.closes
	st := r.step()
	r.wantState(StateEmergencyHalted)
	if r.trader.closes != closes || !strings.Contains(st.HaltReasonVI, "2 ý định") {
		t.Errorf("closes %d→%d, halt %q", closes, r.trader.closes, st.HaltReasonVI)
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
	if _, _, err := r.eng.Stop(context.Background(), false); !errors.Is(err, ErrBusy) {
		t.Errorf("stop during a kill = %v", err)
	}
	if st := r.eng.Status(); st.Busy != "kill" {
		t.Errorf("busy during the kill = %q", st.Busy)
	}
	close(gate)
	<-done
	r.wantState(StateEmergencyHalted)
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
	waitFor(t, func() bool { return r.eng.Status().State == StateOpening })

	stopErr := make(chan error, 1)
	go func() {
		_, _, err := r.eng.Stop(context.Background(), false)
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
	st := r.wantState(StateEmergencyHalted)
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
	r.wantState(StateEmergencyHalted)
}

// Adopting a bot position whose open instant cannot be read would count every
// listed settlement as "after the open"; the bot halts instead.
func TestEngine_AnAdoptionWithoutAnOpenInstantHalts(t *testing.T) {
	r := newRig(t)
	if res := r.trader.Open(context.Background(), OpenOrder{Symbol: testSymbol, NotionalQuote: 65, SignalEntryCostPct: 1}); !res.Hedged {
		t.Fatalf("seed = %+v", res)
	}
	r.trader.intents[0].openedAtMs = 0
	cfg := testConfig()
	cfg.MaxHoldEpochs = 1
	r.start(cfg)
	st := r.step()
	r.wantState(StateEmergencyHalted)
	if r.trader.closes != 0 || !strings.Contains(st.HaltReasonVI, "thời điểm mở") {
		t.Errorf("closes %d, halt %q", r.trader.closes, st.HaltReasonVI)
	}
}

// While holding, a funding history that stays unreadable is tolerated for a
// time budget — settlements are hours apart — then halts with the pair kept.
// The settlement count shown is not reset to 0 by the outage.
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
	if st := r.eng.Status(); st.Position == nil || st.Position.SettlementsSinceOpen != 1 {
		t.Fatalf("settlements since open = %+v", st.Position)
	}
	r.market.set(func(s *Snapshot) { s.SettledErrVI = "timeout" })
	for i := 0; i < 5; i++ {
		r.step()
	}
	st := r.wantState(StateInPosition)
	if st.Position.SettlementsSinceOpen != 1 || st.ReadFailures != 0 {
		t.Errorf("during a short outage: settlements %d, read failures %d", st.Position.SettlementsSinceOpen, st.ReadFailures)
	}
	advance(maxHistoryBlind + time.Minute)
	r.step()
	st = r.wantState(StateEmergencyHalted)
	r.wantVenueHedged()
	if !strings.Contains(st.HaltReasonVI, "lịch sử funding") {
		t.Errorf("halt = %q", st.HaltReasonVI)
	}
}

// A naked leg on the venue: the bot halts and sends NOTHING — squaring is a
// person's call.
func TestEngine_AnUnhedgedVenueHaltsWithoutTrading(t *testing.T) {
	r := newRig(t)
	r.start(testConfig())
	r.step()
	id := r.eng.Status().Position.IntentID
	// The spot leg is sold behind the bot's back.
	if _, err := r.trader.spot.PlaceOrder(context.Background(), broker.PlaceOrderRequest{Market: broker.MarketSpot, Symbol: testSymbol,
		Side: broker.SideSell, Type: broker.OrderTypeMarket, ClientOrderID: execution.CloseClientOrderID(id, execution.LegSpot), QtyCoin: 0.0008}); err != nil {
		t.Fatal(err)
	}
	orders := len(r.trader.spot.Orders()) + len(r.trader.perp.Orders())
	st := r.step()
	r.wantState(StateEmergencyHalted)
	if got := len(r.trader.spot.Orders()) + len(r.trader.perp.Orders()); got != orders {
		t.Errorf("the bot sent %d orders into an unhedged pair", got-orders)
	}
	if !strings.Contains(st.HaltReasonVI, "KHÔNG tự làm phẳng") {
		t.Errorf("halt = %q", st.HaltReasonVI)
	}
	// And a kill does not square it either.
	if _, oc, _ := r.eng.Kill(context.Background()); oc == nil || oc.Attempted || oc.Flat {
		t.Errorf("kill on an unhedged pair = %+v", oc)
	}
	if got := len(r.trader.spot.Orders()) + len(r.trader.perp.Orders()); got != orders {
		t.Error("the kill traded an unhedged pair")
	}
}

// Reads that keep failing halt the bot after the configured count.
func TestEngine_ConsecutiveReadFailuresHalt(t *testing.T) {
	r := newRig(t)
	r.market.err = errors.New("venue down")
	r.start(testConfig())
	r.step()
	st := r.step()
	if st.State == StateEmergencyHalted || st.ReadFailures != 2 {
		t.Fatalf("after 2 failures: %s, %d", st.State, st.ReadFailures)
	}
	st = r.step()
	r.wantState(StateEmergencyHalted)
	if r.trader.opens != 0 || !strings.Contains(st.HaltReasonVI, "đọc sàn hỏng") {
		t.Errorf("opens %d, halt %q", r.trader.opens, st.HaltReasonVI)
	}
}

// A signal that does not qualify opens nothing, and says why.
func TestEngine_ANegativeFundingOpensNothing(t *testing.T) {
	r := newRig(t)
	r.market.set(func(s *Snapshot) { s.ForecastRatePerIntervalFrac = -0.0001 })
	r.start(testConfig())
	st := r.step()
	r.wantState(StateIdleScanning)
	if r.trader.opens != 0 || st.Signal == nil || st.Signal.EntryEligible || !strings.Contains(st.Signal.VerdictVI, "Funding đang hình thành") {
		t.Errorf("opens %d signal %+v", r.trader.opens, st.Signal)
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
	cfg.MaxHoldEpochs = 1
	cfg.MinNetAPRPct = -10_000 * 0.09 // one settlement cannot pay a round trip; the operator lowered the floor to test the exit
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
	r.market.set(func(s *Snapshot) { s.PerpBook = book("binance_futures", testMid*1.004, time.Now()) }) // +40 bps
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
	id := r.eng.Status().Position.IntentID
	st0, out, err := r.eng.Stop(context.Background(), false)
	if err != nil || out.Close != nil || out.KeptIntentID != id || st0.Position != nil || st0.Busy != "" {
		t.Fatalf("stop = %+v, %v · status position %+v busy %q", out, err, st0.Position, st0.Busy)
	}
	r.wantState(StateDisabled)
	r.wantVenueHedged()

	r.start(testConfig())
	st := r.step()
	r.wantState(StateInPosition)
	if st.Position == nil || st.Position.IntentID != id || !st.Position.Adopted || r.trader.opens != 1 {
		t.Fatalf("after restart: position %+v, opens %d", st.Position, r.trader.opens)
	}
	// STOP with close_now flattens it and leaves the bot disabled.
	if _, out, err := r.eng.Stop(context.Background(), true); err != nil || out.Close == nil || !out.Close.Flat {
		t.Fatalf("stop and close = %+v, %v", out, err)
	}
	r.wantState(StateDisabled)
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
	st := r.step()
	r.wantState(StateIdleScanning)
	if r.trader.opens != opens || st.Position != nil || st.Signal == nil || !strings.Contains(st.Signal.VerdictVI, "Không có vị thế nào mở") {
		t.Errorf("opens %d→%d, position %+v, verdict %q", opens, r.trader.opens, st.Position, st.Signal.VerdictVI)
	}
	// And the kill switch does not close it either.
	if _, oc, _ := r.eng.Kill(context.Background()); oc == nil || oc.Attempted || oc.Flat || !strings.Contains(oc.DetailVI, "không phải của bot") {
		t.Errorf("kill beside a manual position = %+v", oc)
	}
	r.wantVenueHedged()
}

// The portal busy with a manual write: nothing is sent, and the bot tries again.
func TestEngine_ABusyPortalIsRetriedNotCounted(t *testing.T) {
	r := newRig(t)
	r.trader.busy = true
	r.start(testConfig())
	st := r.step()
	r.wantState(StateIdleScanning)
	if st.TradeFailures != 0 || len(r.trader.spot.Orders()) != 0 {
		t.Fatalf("busy: failures %d, orders %d", st.TradeFailures, len(r.trader.spot.Orders()))
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
	cfg := testConfig()
	cfg.ScanInterval = time.Second
	r.start(cfg)
	deadline := time.Now().Add(10 * time.Second)
	for r.eng.Status().State != StateInPosition {
		if time.Now().After(deadline) {
			t.Fatalf("Run never opened: %s %v", r.eng.Status().State, logLines(r.eng.Status()))
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
	st := r.wantState(StateDisabled)
	r.wantVenueHedged()
	if !hasLog(st, "STOP", "GIỮ NGUYÊN") {
		t.Errorf("log %v", logLines(st))
	}
}

// A start is refused when its configuration is not one a run may use.
func TestEngine_StartRefusesABadConfiguration(t *testing.T) {
	r := newRig(t)
	for name, mutate := range map[string]func(c *Config){
		"no symbol":            func(c *Config) { c.Symbol = "" },
		"zero notional":        func(c *Config) { c.NotionalQuote = 0 },
		"notional over cap":    func(c *Config) { c.NotionalQuote = 60_000 },
		"NaN floor":            func(c *Config) { c.MinNetAPRPct = math.NaN() },
		"negative epochs":      func(c *Config) { c.MaxHoldEpochs = -1 },
		"sub-second scan":      func(c *Config) { c.ScanInterval = 100 * time.Millisecond },
		"no projection":        func(c *Config) { c.ProjectionHoldDays = 0 },
		"depth multiple under": func(c *Config) { c.DepthMultiple = 0.5 },
	} {
		c := testConfig()
		mutate(&c)
		if _, err := r.eng.Start(c); err == nil {
			t.Errorf("%s: started", name)
			_, _, _ = r.eng.Stop(context.Background(), false)
		}
	}
	r.wantState(StateDisabled)
}
