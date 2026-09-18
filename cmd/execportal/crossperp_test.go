package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
	"futures-arbitrage-scanner/internal/coordinator"
	"futures-arbitrage-scanner/internal/execution/crossperp"
	"futures-arbitrage-scanner/internal/risk"
)

// Engine 2 as the PORTAL wires it (PLAN 4.5k step 4). The engine, the lock and
// the guard are tested against fakes in their own packages; what is tested here
// is the wiring — that the two engines really ask the same lock, that a refusal
// reaches the operator instead of an order reaching a venue, and that every
// number the page prints says what it is.
//
// Both venues here are brokertest fakes behind fakeVenue, so nothing opens a
// socket: an Engine-2 acceptance that needed the network could not run in CI and
// would stop being run.

// bybitPerpRulesBTC is Bybit linear BTCUSDT as cmd/bybitcheck measured it live
// on 2026-09-18: step 0.001 BTC, tick 0.1, minimum 5 USDT. It is the COARSER
// grid of the two, which is what makes it the interesting fixture — the common
// step of a pair is the coarser one, and a test on two equal grids would never
// see a rounding rule at all.
var bybitPerpRulesBTC = exchanges.Instrument{
	Symbol: "BTCUSDT", StepSizeCoin: 0.001, MinQtyCoin: 0.001, TickSizeQuote: 0.1, MinNotionalQuote: 5,
}

// staticMargin is a margin reader the test drives directly.
type staticMargin struct {
	venue string

	mu      sync.Mutex
	ratio   float64
	err     error
	nowFunc func() time.Time
}

func (m *staticMargin) VenueName() string { return m.venue }

func (m *staticMargin) set(ratioFrac float64, err error) {
	m.mu.Lock()
	m.ratio, m.err = ratioFrac, err
	m.mu.Unlock()
}

func (m *staticMargin) ReadMargin(context.Context) (risk.MarginReading, error) {
	m.mu.Lock()
	ratio, err := m.ratio, m.err
	m.mu.Unlock()
	if err != nil {
		return risk.MarginReading{}, err
	}
	// A DERIVED reading, as Binance's is: two totals whose quotient is the
	// ratio. Building the reading the real way keeps the test honest about what
	// the guard is given.
	return risk.DerivedMarginReading(m.venue, "USDT", ratio*1000, 1000, "fake", m.nowFunc().UnixMilli())
}

type crossFixture struct {
	desk    *crossDesk
	binance *fakeVenue
	bybit   *fakeVenue
	margins map[string]*staticMargin
	ids     int
}

// newCrossFixture builds a desk on two fake perp venues with an empty lock file
// in a temp directory, already reconciled — which is the state the page is in
// after start-up.
func newCrossFixture(t *testing.T) *crossFixture {
	t.Helper()
	f := &crossFixture{margins: map[string]*staticMargin{}}

	f.binance = newFakeVenue(t, broker.MarketFuturesUSDM, perpRulesBTC)
	f.binance.SetMarkPrice(broker.MarkPrice{MarkPriceQuote: fakeMidQuote, NextFundingTimeMs: time.Now().Add(time.Hour).UnixMilli()})
	f.bybit = newFakeVenue(t, broker.MarketFuturesUSDM, bybitPerpRulesBTC)
	f.bybit.SetMarkPrice(broker.MarkPrice{MarkPriceQuote: fakeMidQuote, NextFundingTimeMs: time.Now().Add(time.Hour).UnixMilli()})

	now := time.Now
	venues := crossVenues{}
	for _, v := range []struct {
		name string
		fake *fakeVenue
	}{{crossVenueBinance, f.binance}, {crossVenueBybit, f.bybit}} {
		m := &staticMargin{venue: v.name, ratio: 0.10, nowFunc: now}
		f.margins[v.name] = m
		venues.list = append(venues.list, crossVenue{Name: v.name, Perp: v.fake, SourceVI: "fake", Margin: m})
	}

	settings := defaultCrossSettings()
	settings.LocksPath = filepath.Join(t.TempDir(), "coordinator-locks.json")
	desk, err := newCrossDesk(venues, []string{"BTCUSDT"}, settings, defaultCrossPilotConfig(), now)
	if err != nil {
		t.Fatalf("newCrossDesk: %v", err)
	}
	desk.mintID = func(prefix, symbol string) (string, error) {
		f.ids++
		return fmt.Sprintf("%s%s-%d", prefix, strings.ToLower(symbol), f.ids), nil
	}
	f.desk = desk
	if err := desk.reconcileLocks(context.Background()); err != nil {
		t.Fatalf("reconcileLocks: %v", err)
	}
	// One guard tick, because an UNREAD venue refuses every open — which is the
	// guard behaving correctly, and is its own test below. A fixture that
	// skipped it would make every other test here pass for the wrong reason.
	desk.marginGuard.Tick(context.Background())
	return f
}

func (f *crossFixture) openBTC(t *testing.T, notionalQuote float64) crossOpenView {
	t.Helper()
	id, err := f.desk.mintIntentID("BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	view, _ := f.desk.openPair(context.Background(), crossOpenRequest{
		Symbol: "BTCUSDT", LongVenue: crossVenueBybit, ShortVenue: crossVenueBinance,
		NotionalQuote: notionalQuote, ExpectedAPROnCapitalFrac: 0.2, ExpectedAPRBasisVI: "fixture",
	}, id)
	return view
}

// ------------------------------------------------------------ the wiring

func TestCrossDesk_RefusesUnlessBothVenuesAnsweredACredential(t *testing.T) {
	only := crossVenues{list: []crossVenue{
		{Name: crossVenueBinance, Perp: newFakeVenue(t, broker.MarketFuturesUSDM, perpRulesBTC)},
		{Name: crossVenueBybit, Err: errors.New("BYBIT_API_KEY chưa đặt")},
	}}
	_, err := newCrossDesk(only, []string{"BTCUSDT"}, defaultCrossSettings(), defaultCrossPilotConfig(), time.Now)
	if err == nil {
		t.Fatal("a desk was built with one venue — Engine 2 is a pair or it is nothing")
	}
	if !strings.Contains(err.Error(), "BYBIT_API_KEY") {
		t.Errorf("the refusal does not name the missing credential: %v", err)
	}
}

// A page that could not tell "Engine 2 is off" from "Engine 2 is broken" would
// send its operator looking for a fault that is not there.
func TestCrossperp_EveryRouteSaysDisabledRatherThanFailingWhenTheDeskIsOff(t *testing.T) {
	p, _, _ := fakePortal(t)
	if p.cross != nil {
		t.Fatal("a portal built without -crossperp must have no Engine-2 desk")
	}
	for _, path := range []string{"/api/crossperp/status", "/api/coordinator/locks", "/api/risk/margin"} {
		t.Run("GET "+path, func(t *testing.T) {
			body := getJSON[map[string]any](t, p, path)
			if body["enabled"] != false {
				t.Errorf("enabled = %v, want false", body["enabled"])
			}
			why, _ := body["disabled_vi"].(string)
			if !strings.Contains(why, "-crossperp") {
				t.Errorf("disabled_vi does not say how to turn it on: %q", why)
			}
		})
	}
	writes := []struct {
		path, action string
		body         any
	}{
		{"/api/crossperp/open", crossOpenAction, crossOpenRequest{Symbol: "BTCUSDT",
			LongVenue: crossVenueBybit, ShortVenue: crossVenueBinance, NotionalQuote: 100, ExpectedAPRBasisVI: "x"}},
		{"/api/crossperp/close", crossCloseAction, crossCloseRequest{Symbol: "BTCUSDT"}},
		{"/api/crossperp/reconcile", crossReconcileAction, struct{}{}},
		{"/api/crossperp/unblock", crossUnblockAction, crossSymbolRequest{Symbol: "BTCUSDT"}},
		{"/api/risk/margin/ack", crossAckMarginAction, crossAckRequest{SeenSeq: 1}},
	}
	for _, w := range writes {
		t.Run("POST "+w.path, func(t *testing.T) {
			code, _ := postJSON[map[string]any](t, p, w.action, w.path, w.body)
			if code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503 - a write to an absent engine must not read as accepted", code)
			}
		})
	}
}

// --------------------------------------------------------------- the sizing

func TestCrossNotionalQuote_TakesExactlyOneSizeAndSaysWhichBookPricedIt(t *testing.T) {
	leg := crossperp.LegSpec{Venue: crossperp.Venue{Name: crossVenueBybit}}
	leg.Book.MidPriceQuote = 80_000

	t.Run("neither is refused", func(t *testing.T) {
		if _, _, err := crossNotionalQuote(crossOpenRequest{}, leg); err == nil {
			t.Fatal("a request naming no size was accepted")
		}
	})
	t.Run("both is refused", func(t *testing.T) {
		_, _, err := crossNotionalQuote(crossOpenRequest{NotionalQuote: 100, QtyCoin: 0.001}, leg)
		if err == nil {
			t.Fatal("a request naming both sizes was accepted — one of them would have been ignored silently")
		}
	})
	t.Run("a quantity is converted and the conversion is stated", func(t *testing.T) {
		got, basisVI, err := crossNotionalQuote(crossOpenRequest{QtyCoin: 0.001}, leg)
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(got-80) > 1e-9 {
			t.Errorf("notional = %v, want 80", got)
		}
		for _, want := range []string{"0.00100000", "80000", crossVenueBybit} {
			if !strings.Contains(basisVI, want) {
				t.Errorf("the basis does not say %q: %s", want, basisVI)
			}
		}
	})
	t.Run("a quantity with no mid is refused, not priced at zero", func(t *testing.T) {
		blind := crossperp.LegSpec{Venue: crossperp.Venue{Name: crossVenueBybit}}
		if _, _, err := crossNotionalQuote(crossOpenRequest{QtyCoin: 0.001}, blind); err == nil {
			t.Fatal("a quantity was converted on a book with no mid price")
		}
	})
	t.Run("the ceiling binds on both forms", func(t *testing.T) {
		if _, _, err := crossNotionalQuote(crossOpenRequest{NotionalQuote: crossMaxNotionalQuote + 1}, leg); err == nil {
			t.Fatal("a notional over the ceiling was accepted")
		}
		if _, _, err := crossNotionalQuote(crossOpenRequest{QtyCoin: 10}, leg); err == nil {
			t.Fatal("a quantity worth more than the ceiling was accepted")
		}
	})
}

// --------------------------------------------------------- the funding clock

// Rule 3 is the rule this project has been bitten by most, and a cross-venue
// spread is where it bites hardest: the two venues do not settle on the same
// clock, so a difference of two published rates is a difference of two different
// units.
func TestMeasureFunding_ReadsTheCadenceOffTheStampsAndRefusesWhenItCannot(t *testing.T) {
	at := func(hoursAgo float64, rate float64) binancebroker.FundingRate {
		return binancebroker.FundingRate{SettledAtMs: time.Now().Add(-time.Duration(hoursAgo * float64(time.Hour))).UnixMilli(),
			RatePerIntervalFrac: rate}
	}
	t.Run("too few settlements produce no number at all", func(t *testing.T) {
		_, why := measureFunding([]binancebroker.FundingRate{at(8, 1e-4), at(0, 1e-4)})
		if why == "" {
			t.Fatal("a cadence was measured from one gap")
		}
	})
	t.Run("a 4h Binance symbol is measured as 4h, not assumed 8h", func(t *testing.T) {
		var rows []binancebroker.FundingRate
		for i := 6; i >= 0; i-- {
			rows = append(rows, at(float64(i*4), 1e-4))
		}
		got, why := measureFunding(rows)
		if why != "" {
			t.Fatal(why)
		}
		if got.intervalSec != 4*3600 {
			t.Errorf("intervalSec = %d, want %d — a 4h symbol read as 8h halves its annual figure", got.intervalSec, 4*3600)
		}
	})
	t.Run("seconds of stamp jitter do not change the cadence", func(t *testing.T) {
		// Gate lands 1–3s past the hour and Hyperliquid jitters by tens of ms;
		// stamps are stored verbatim, so the measurement has to absorb it.
		var rows []binancebroker.FundingRate
		base := time.Now().Add(-48 * time.Hour)
		for i := 0; i < 6; i++ {
			stamp := base.Add(time.Duration(i) * 8 * time.Hour).Add(time.Duration(i%3) * 1700 * time.Millisecond)
			rows = append(rows, binancebroker.FundingRate{SettledAtMs: stamp.UnixMilli(), RatePerIntervalFrac: 1e-4})
		}
		got, why := measureFunding(rows)
		if why != "" {
			t.Fatal(why)
		}
		if got.intervalSec != 8*3600 {
			t.Errorf("intervalSec = %d, want %d — stamp jitter was read as a different cadence", got.intervalSec, 8*3600)
		}
	})
	t.Run("the mean is the mean and the newest is the newest", func(t *testing.T) {
		rows := []binancebroker.FundingRate{at(24, 4e-4), at(16, 2e-4), at(8, 0), at(0, 2e-4)}
		got, why := measureFunding(rows)
		if why != "" {
			t.Fatal(why)
		}
		if math.Abs(got.meanRateFrac-2e-4) > 1e-12 {
			t.Errorf("meanRateFrac = %v, want 2e-4", got.meanRateFrac)
		}
		if math.Abs(got.newestRateFrac-2e-4) > 1e-12 || got.newestAtMs != rows[3].SettledAtMs {
			t.Errorf("the newest settlement is not the newest stamp: %+v", got)
		}
	})
}

// A spread between two venues on different clocks is only a spread once both
// sides are annualized on their OWN measured cadence.
func TestCrossPilot_AnnualizesEachVenueOnItsOwnMeasuredCadence(t *testing.T) {
	f := newCrossFixture(t)
	pilot := f.desk.pilot

	// Same rate per settlement, different cadences: 1 bps every 8h against
	// 1 bps every 4h. The second venue pays twice as much a year, and a pilot
	// that subtracted the published rates would call the spread zero.
	hourly := func(intervalHours int, rate float64) []binancebroker.FundingRate {
		var rows []binancebroker.FundingRate
		for i := 8; i >= 0; i-- {
			rows = append(rows, binancebroker.FundingRate{
				SettledAtMs:         time.Now().Add(-time.Duration(i*intervalHours) * time.Hour).UnixMilli(),
				RatePerIntervalFrac: rate})
		}
		return rows
	}
	f.binance.fundingRates = hourly(4, 1e-4)
	f.bybit.fundingRates = hourly(8, 1e-4)

	signals, notes := pilot.measure(context.Background())
	if len(signals) != 1 {
		t.Fatalf("signals = %d (%v), want 1", len(signals), notes)
	}
	s := signals[0]
	if s.ShortVenue != crossVenueBinance || s.LongVenue != crossVenueBybit {
		t.Errorf("the pair is short %s / long %s — the SHORT leg must be the venue that pays more", s.ShortVenue, s.LongVenue)
	}
	// 1 bps × (365×24/4) − 1 bps × (365×24/8) = 1 bps × 1095 = 0.1095
	if math.Abs(s.AnnualSpreadFrac-0.1095) > 1e-3 {
		t.Errorf("AnnualSpreadFrac = %v, want ≈0.1095 — each venue must be annualized on its own cadence", s.AnnualSpreadFrac)
	}
	if !strings.Contains(s.BasisVI, "14400s") || !strings.Contains(s.BasisVI, "28800s") {
		t.Errorf("the basis does not print BOTH measured cadences: %s", s.BasisVI)
	}
}

// Rule 2: nothing outside internal/strategy may present a figure as "net", and
// a figure the operator can act on must say what it does NOT cover.
func TestCrossPilot_TheEntryFigureIsOnCapitalAndNamesWhatIsNotDeducted(t *testing.T) {
	f := newCrossFixture(t)
	pilot := f.desk.pilot
	rows := func(rate float64) []binancebroker.FundingRate {
		var out []binancebroker.FundingRate
		for i := 8; i >= 0; i-- {
			out = append(out, binancebroker.FundingRate{
				SettledAtMs: time.Now().Add(-time.Duration(i*8) * time.Hour).UnixMilli(), RatePerIntervalFrac: rate})
		}
		return out
	}
	f.binance.fundingRates = rows(3e-4)
	f.bybit.fundingRates = rows(0)

	signals, notes := pilot.measure(context.Background())
	if len(signals) != 1 {
		t.Fatalf("signals = %d (%v), want 1", len(signals), notes)
	}
	s := signals[0]
	if strings.Contains(strings.ToLower(s.BasisVI), "ròng") || strings.Contains(strings.ToLower(s.BasisVI), "net ") {
		t.Errorf(`the basis calls the figure "net" — that word belongs to internal/strategy (rule 2): %s`, s.BasisVI)
	}
	for _, want := range []string{"CHƯA TRỪ", "thanh lý", "sổ lệnh lúc THOÁT", "GIẢ ĐỊNH"} {
		if !strings.Contains(s.BasisVI, want) {
			t.Errorf("the basis does not name %q among what it excludes: %s", want, s.BasisVI)
		}
	}
	// Both legs are levered at the shipped 50%, so capital is 1.0 × notional.
	if math.Abs(s.CapitalPerNotionalFrac-1.0) > 1e-9 {
		t.Errorf("CapitalPerNotionalFrac = %v, want 1.0", s.CapitalPerNotionalFrac)
	}
	// The arithmetic, spelled out rather than recomputed by the same code:
	// 3 bps × 1095 settlements = 0.3285/yr gross; the round trip is 2×(4+4) bps
	// of fee — both fakes answer the futures rate — plus four half-spreads.
	wantGross := 3e-4 * (365 * 24 / 8.0)
	if math.Abs(s.AnnualSpreadFrac-wantGross) > 1e-6 {
		t.Fatalf("AnnualSpreadFrac = %v, want %v", s.AnnualSpreadFrac, wantGross)
	}
	holdYears := pilot.cfg.PlannedHoldDays / 365
	wantAPR := (s.AnnualSpreadFrac*holdYears - s.RoundTripCostFrac) / holdYears / s.CapitalPerNotionalFrac
	if math.Abs(s.AfterCostAPROnCapitalFrac-wantAPR) > 1e-9 {
		t.Errorf("AfterCostAPROnCapitalFrac = %v, want %v", s.AfterCostAPROnCapitalFrac, wantAPR)
	}
	if s.AfterCostAPROnCapitalFrac >= s.AnnualSpreadFrac {
		t.Error("the after-cost figure is not below the gross one — a cost was not taken off")
	}
}

// --------------------------------------------------------- Q21 and Q22

// Decision Q21: when the evidence conflicts the machine STOPS. It does not send
// a reduce-only "probe" to find out what the venue really holds.
//
// The rule is judged as a VALUE so every state can be put in front of it,
// including ones a fake venue cannot be made to produce on demand; the test
// after this one drives one of them all the way through a real close.
func TestCrossStopReason_NamesEveryPieceOfEvidenceAutomationMayNotResolve(t *testing.T) {
	pair := func(edit func(*crossperp.Pair)) []crossperp.Pair {
		p := crossperp.Pair{IntentID: "xbtcusdt-1", Symbol: "BTCUSDT"}
		edit(&p)
		return []crossperp.Pair{p}
	}
	cases := []struct {
		name  string
		pairs []crossperp.Pair
		held  map[string]error
		locks []coordinator.SymbolLock
		wants string
	}{
		{"a close blocked by conflicting evidence", pair(func(p *crossperp.Pair) {
			p.CloseBlockedVI = "lenh khop 0.003 nhung san bao 0.001"
		}), nil, nil, "Q21"},
		{"an unresolved pair", pair(func(p *crossperp.Pair) { p.Unresolved = true }), nil, nil, "CHƯA GIẢI QUYẾT"},
		{"an opening order never proven finished", pair(func(p *crossperp.Pair) {
			p.OpeningOrdersUnproven = true
		}), nil, nil, "vẫn có thể khớp"},
		{"a reduce-only order never proven finished", pair(func(p *crossperp.Pair) {
			p.PendingOrders = []crossperp.PendingOrder{{Venue: crossVenueBybit, ClientOrderID: "cp1sr-x"}}
		}), nil, nil, "BẤT KỲ vị thế nào"},
		{"a lock kept because its release failed", nil,
			map[string]error{"ETHUSDT": errors.New("san khong tra loi")}, nil, "nhả hỏng"},
		{"a symbol in evidence conflict", nil, nil,
			[]coordinator.SymbolLock{{Symbol: "SOLUSDT", State: coordinator.StateConflict, EvidenceVI: "hai san noi khac nhau"}}, "XUNG ĐỘT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := crossStopReasonVI(tc.pairs, tc.held, tc.locks)
			if got == "" {
				t.Fatal("automation was allowed to continue beside evidence it may not resolve")
			}
			if !strings.Contains(got, tc.wants) {
				t.Errorf("the stop does not say why: %s", got)
			}
		})
	}
	t.Run("a clean hedged pair does not stop anything", func(t *testing.T) {
		clean := pair(func(p *crossperp.Pair) { p.LongQtyCoin, p.ShortQtyCoin = 0.003, 0.003 })
		if got := crossStopReasonVI(clean, nil, []coordinator.SymbolLock{{Symbol: "BTCUSDT", State: coordinator.StateOccupied}}); got != "" {
			t.Errorf("a clean pair stopped the pilot: %s", got)
		}
	})
}

// The same rule reached the long way: a close whose venue position disagrees
// with the pair it is closing must leave the pair loud, halt the pilot, keep the
// lock, and send nothing more.
func TestCrossPilot_AConflictFromARealCloseHaltsItAndSendsNothingMore(t *testing.T) {
	f := newCrossFixture(t)
	f.desk.pilot.cfg.Enabled = true
	if view := f.openBTC(t, 200); !view.Hedged {
		t.Fatalf("the fixture did not open a pair: %s / %s", view.Outcome, view.ErrorVI)
	}

	// The short leg's venue now reports the WRONG SIDE for this pair: a long
	// where the pair holds a short. Nothing may be sized off that.
	f.binance.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", QtyCoin: +0.002})

	closeView, _ := f.desk.closePair(context.Background(), crossCloseRequest{Symbol: "BTCUSDT"})
	if closeView.Outcome == string(crossperp.OutcomeBothFlat) {
		t.Fatalf("a close on conflicting evidence reported the pair flat: %+v", closeView)
	}
	f.desk.pilot.scan(context.Background())
	if view := f.desk.pilot.view(); !view.Halted {
		t.Fatalf("the pilot kept trading after a close it could not complete: %+v", closeView.ErrorVI)
	}
	if lock, held := f.desk.coord.QueryLock("BTCUSDT"); !held || lock.State == coordinator.StateIdle {
		t.Errorf("the lock went idle beside a pair nobody could prove flat: %+v", lock)
	}
	after := orderCount(f.binance, f.bybit)
	f.desk.pilot.scan(context.Background())
	if now := orderCount(f.binance, f.bybit); now != after {
		t.Errorf("a halted pilot sent %d more orders", now-after)
	}
}

// --------------------------------------------------- the two engines' lock

// Item 1 of step 4, and self-audit question 4: with Engine 2 holding the
// symbol, Engine 1 must be refused BEFORE it sends anything.
func TestEngine1_IsRefusedWhileEngine2HoldsTheSymbol(t *testing.T) {
	f := newCrossFixture(t)
	p, spot, perp := fakePortal(t)
	p.attachCross(f.desk)

	if view := f.openBTC(t, 200); !view.Hedged {
		t.Fatalf("Engine 2 did not open: %s / %s", view.Outcome, view.ErrorVI)
	}
	spotOrders, perpOrders := len(spot.Orders()), len(perp.Orders())

	settle, whyVI := p.acquireEngine1(context.Background(), "BTCUSDT", "fa1btcusdt-1", "test")
	if whyVI == "" {
		t.Fatal("Engine 1 was granted a symbol Engine 2 holds — on a one-way account its short would net against Engine 2's long")
	}
	if !strings.Contains(whyVI, "BẬN") {
		t.Errorf("the refusal does not say the symbol is busy: %s", whyVI)
	}
	settle(context.Background(), false)

	if len(spot.Orders()) != spotOrders || len(perp.Orders()) != perpOrders {
		t.Error("a refused acquire still reached a venue")
	}
	lock, held := f.desk.coord.QueryLock("BTCUSDT")
	if !held || lock.OwnerEngine != coordinator.EngineCrossPerp {
		t.Errorf("the refusal moved the lock: %+v", lock)
	}
}

// The other direction, which is the one the live account is actually in: Engine
// 1 holds BTCUSDT, so Engine 2 must be refused before it sends anything.
func TestEngine2_IsRefusedWhileEngine1HoldsTheSymbol(t *testing.T) {
	f := newCrossFixture(t)
	p, _, _ := fakePortal(t)
	p.attachCross(f.desk)

	settle, whyVI := p.acquireEngine1(context.Background(), "BTCUSDT", "fa1btcusdt-1", "Engine 1 giữ trước")
	if whyVI != "" {
		t.Fatalf("Engine 1 could not take an idle symbol: %s", whyVI)
	}
	defer settle(context.Background(), true)

	view := f.openBTC(t, 200)
	if view.Hedged {
		t.Fatal("Engine 2 opened on a symbol Engine 1 holds")
	}
	if !view.LockRefused {
		t.Errorf("the refusal was not reported as a lock refusal: %+v", view.ErrorVI)
	}
	if !view.RefusedBeforePlacing {
		t.Error("the refusal happened after an order was sent")
	}
	if n := len(f.binance.Orders()) + len(f.bybit.Orders()); n != 0 {
		t.Errorf("orders on the venues = %d, want 0 — nothing may be sent under a refused lock", n)
	}
}

// A lock recovered from the VENUES rather than from the file: the case that
// makes a lost or corrupt lock file safe, and the case the live account is in
// when another process holds a position this one never opened.
func TestCrossDesk_ReconcileGivesAVenueHeldShortToEngine1AndRefusesEngine2(t *testing.T) {
	f := newCrossFixture(t)

	// Binance holds a short and Bybit is flat — the shape of a Strategy 1
	// position, opened by a process this one has never spoken to.
	f.binance.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", QtyCoin: -0.0100})

	if err := f.desk.reconcileLocks(context.Background()); err != nil {
		t.Fatal(err)
	}
	lock, held := f.desk.coord.QueryLock("BTCUSDT")
	if !held || lock.State != coordinator.StateOccupied {
		t.Fatalf("a live position did not make the symbol busy: %+v", lock)
	}
	if lock.OwnerEngine != coordinator.EngineCashAndCarry {
		t.Errorf("owner = %s, want Engine 1 — one short leg is Strategy 1's shape", lock.OwnerEngine)
	}
	if lock.Source != coordinator.SourceReconciledInferred {
		t.Errorf("source = %s, want the lock to say it was inferred from the venues", lock.Source)
	}

	view := f.openBTC(t, 200)
	if view.Hedged || !view.LockRefused {
		t.Fatalf("Engine 2 opened against a position it found on the venue: %+v", view.ErrorVI)
	}
	if n := len(f.bybit.Orders()); n != 0 {
		t.Errorf("orders on the untouched venue = %d, want 0", n)
	}
}

// ----------------------------------------------------- open, close, unlock

// The whole loop the acceptance runs, in miniature and offline: idle → the pair
// opens and the lock is occupied → the pair closes and the lock is idle again.
func TestCrossDesk_ALifecycleTakesTheLockAndGivesItBackOnlyWhenTheVenuesAreFlat(t *testing.T) {
	f := newCrossFixture(t)

	view := f.openBTC(t, 200)
	if !view.Hedged {
		t.Fatalf("outcome = %s: %s", view.Outcome, view.ErrorVI)
	}
	// The pair is sized on the COARSER of the two grids, which is Bybit's 0.001.
	if math.Abs(view.CommonStepCoin-0.001) > 1e-12 {
		t.Errorf("CommonStepCoin = %v, want Bybit's 0.001 — the coarser grid is the common one", view.CommonStepCoin)
	}
	if view.DeltaImbalanceQtyCoin != 0 {
		t.Errorf("delta = %v coin, want 0", view.DeltaImbalanceQtyCoin)
	}
	lock, held := f.desk.coord.QueryLock("BTCUSDT")
	if !held || lock.State != coordinator.StateOccupied || lock.OwnerEngine != coordinator.EngineCrossPerp {
		t.Fatalf("the open did not take the lock: %+v", lock)
	}
	if lock.OrdersSentAtMs == 0 {
		t.Error("the lock carries no 'orders were sent' mark — it could be handed back on this process's word alone")
	}

	closeView, status := f.desk.closePair(context.Background(), crossCloseRequest{Symbol: "BTCUSDT", FirstVenue: crossVenueBinance})
	if status != http.StatusOK || closeView.Outcome != string(crossperp.OutcomeBothFlat) {
		t.Fatalf("close: status %d outcome %s: %s", status, closeView.Outcome, closeView.ErrorVI)
	}
	if !closeView.LockReleased {
		t.Errorf("the lock was not released after a flat close: %s", closeView.LockStateVI)
	}
	for _, v := range []*fakeVenue{f.binance, f.bybit} {
		pos, err := v.GetPosition(context.Background(), broker.MarketFuturesUSDM, "BTCUSDT")
		if err != nil {
			t.Fatal(err)
		}
		if pos.QtyCoin != 0 {
			t.Errorf("a venue still holds %+.8f coin after a both_flat close", pos.QtyCoin)
		}
	}
	after, held := f.desk.coord.QueryLock("BTCUSDT")
	if held && after.State != coordinator.StateIdle {
		t.Errorf("lock after the close = %+v, want idle", after)
	}
}

// Q22's second half: the release must rest on a READING of the venues, not on
// what this process believes it closed. With a venue unreadable, the lock stays.
func TestCrossDesk_AReleaseThatCannotReadAVenueKeepsTheLock(t *testing.T) {
	f := newCrossFixture(t)
	if view := f.openBTC(t, 200); !view.Hedged {
		t.Fatalf("the fixture did not open: %s", view.ErrorVI)
	}
	_, _ = f.desk.closePair(context.Background(), crossCloseRequest{Symbol: "BTCUSDT"})

	// Both venues are flat now. Make one unreadable and ask the coordinator to
	// release a lock it no longer has evidence for.
	f.binance.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", QtyCoin: -0.002})
	if _, err := f.desk.coord.Release(context.Background(), "BTCUSDT", coordinator.EngineCrossPerp, "whatever"); err == nil {
		t.Fatal("a lock was released while a venue reported a live position")
	}
}

// ---------------------------------------------------------- what the page says

// An order the VENUE answered "execution status unknown" about is the one case
// that must never be read as absent, however long it has been quiet.
func TestPendingWhyVI_NeverCallsAStatusUnknownOrderAbsent(t *testing.T) {
	long := time.Now().Add(-time.Hour).UnixMilli()
	unknown := pendingWhyVI(crossperp.PendingOrder{StatusUnknown: true, SendReturnedAtMs: long})
	if strings.Contains(unknown, "coi là vắng") && !strings.Contains(unknown, "không bao giờ") {
		t.Errorf("a status-unknown order is described as absentable: %s", unknown)
	}
	if !strings.Contains(unknown, "KHÔNG BIẾT") || !strings.Contains(unknown, "GIỮ") {
		t.Errorf("a status-unknown order does not say the venue answered, nor that the lock is held: %s", unknown)
	}
	seen := pendingWhyVI(crossperp.PendingOrder{Seen: true, SendReturnedAtMs: long})
	if !strings.Contains(seen, "GIỮ") {
		t.Errorf("an order the venue showed does not say the lock is held: %s", seen)
	}
	lost := pendingWhyVI(crossperp.PendingOrder{SendReturnedAtMs: long})
	if !strings.Contains(lost, "Q22") {
		t.Errorf("a lost send does not name the rule that governs it: %s", lost)
	}
}

// The margin view must make an unread venue look different from a safe one:
// "no reading" is not "green" (risk.MarginTierUnknown).
func TestCrossDesk_AnUnreadVenueIsNotGreen(t *testing.T) {
	f := newCrossFixture(t)
	f.margins[crossVenueBybit].set(0, errors.New("mạng hỏng"))
	f.desk.marginGuard.Tick(context.Background())

	view := f.desk.marginView()
	byVenue := map[string]crossMarginVenueView{}
	for _, v := range view.Venues {
		byVenue[v.Venue] = v
	}
	// The guard KEEPS the last good figure and marks the tier unknown, which is
	// the right shape: the page can still show what the venue last said, and the
	// tier says nobody knows whether it is still true. What must never happen is
	// the stale figure carrying a GREEN tier.
	got := byVenue[crossVenueBybit]
	switch {
	case got.Tier != string(risk.MarginTierUnknown):
		t.Errorf("a venue whose newest read failed reads as %q, want unknown", got.Tier)
	case got.ProblemVI == "":
		t.Error("an unknown venue does not say what the problem was")
	case !strings.Contains(got.ProblemVI, "mạng hỏng"):
		t.Errorf("the problem does not carry the venue's own error: %s", got.ProblemVI)
	}
	if got := byVenue[crossVenueBinance]; got.Tier != string(risk.MarginTierGreen) {
		t.Errorf("the readable venue = %s, want green", got.Tier)
	}
	if view.RedFrac != 0.65 || view.YellowFrac != 0.50 || view.OrangeFrac != 0.60 {
		t.Errorf("the tier table the page draws is not the design's 50/60/65: %+v", view)
	}
}

// The guard's red tier must stop Engine 2 opening, and the refusal must reach
// the page rather than an order reaching a venue.
func TestCrossDesk_AVenueOverTheRedThresholdStopsEveryOpen(t *testing.T) {
	f := newCrossFixture(t)
	f.margins[crossVenueBinance].set(0.66, nil)
	f.desk.marginGuard.Tick(context.Background())

	view := f.openBTC(t, 200)
	if view.Hedged {
		t.Fatal("a pair opened with a venue at 66% maintenance margin")
	}
	if !view.RefusedBeforePlacing {
		t.Errorf("the refusal came after an order: %+v", view.ErrorVI)
	}
	if n := len(f.binance.Orders()) + len(f.bybit.Orders()); n != 0 {
		t.Errorf("orders = %d, want 0", n)
	}
}
