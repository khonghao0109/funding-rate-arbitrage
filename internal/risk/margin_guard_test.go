package risk

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The venue names the tests gate on — the same names internal/execution/crossperp
// and internal/coordinator use.
const (
	venueBinance = "binance_futures"
	venueBybit   = "bybit_linear"
)

// testClock is a clock several goroutines can read while a test moves it.
type testClock struct{ ms atomic.Int64 }

func newTestClock() *testClock {
	c := &testClock{}
	c.ms.Store(1_789_000_000_000)
	return c
}
func (c *testClock) Now() time.Time          { return time.UnixMilli(c.ms.Load()) }
func (c *testClock) advance(d time.Duration) { c.ms.Add(d.Milliseconds()) }

// fakeMarginReader is a venue whose ratio a test sets. It derives its reading
// the way the Binance reader does, so the path under test is the real one.
type fakeMarginReader struct {
	name  string
	clock *testClock

	mu        sync.Mutex
	ratio     float64
	published float64 // non-zero: answer as a published reading
	err       error
	calls     int
}

func (f *fakeMarginReader) VenueName() string { return f.name }

func (f *fakeMarginReader) ReadMargin(ctx context.Context) (MarginReading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return MarginReading{}, f.err
	}
	if f.published != 0 {
		return PublishedMarginReading(f.name, "USD", f.published, true, f.ratio*10_000, 10_000, "fake: accountMMRate", f.clock.Now().UnixMilli())
	}
	return DerivedMarginReading(f.name, "USDT", f.ratio*10_000, 10_000, "fake: maint ÷ balance", f.clock.Now().UnixMilli())
}

func (f *fakeMarginReader) set(ratio float64, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ratio, f.err, f.published = ratio, err, 0
}

// setConflict makes the venue answer a PUBLISHED ratio its own totals
// contradict — the reading and its error together, as PublishedMarginReading
// returns them.
func (f *fakeMarginReader) setConflict(publishedFrac, derivedFrac float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ratio, f.err, f.published = derivedFrac, nil, publishedFrac
}

// fakeCloser holds Engine 2's pairs and records which were closed, in order.
type fakeCloser struct {
	mu       sync.Mutex
	pairs    []ExposedPair
	closed   []string
	stressed []string
	failFor  map[string]error
	onClose  func(pairID string)
	listErr  error
}

func (f *fakeCloser) OpenPairs(ctx context.Context) ([]ExposedPair, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]ExposedPair(nil), f.pairs...), nil
}

func (f *fakeCloser) CloseForMargin(ctx context.Context, pairID, stressedVenue string) error {
	f.mu.Lock()
	f.stressed = append(f.stressed, stressedVenue)
	if err := f.failFor[pairID]; err != nil {
		f.mu.Unlock()
		return err
	}
	f.closed = append(f.closed, pairID)
	kept := f.pairs[:0]
	for _, p := range f.pairs {
		if p.PairID != pairID {
			kept = append(kept, p)
		}
	}
	f.pairs = kept
	hook := f.onClose
	f.mu.Unlock()
	if hook != nil {
		hook(pairID)
	}
	return nil
}

func (f *fakeCloser) closedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.closed...)
}

func pair(id string, binanceQuote, bybitQuote float64) ExposedPair {
	return ExposedPair{PairID: id, Symbol: id + "USDT",
		ExposureQuoteByVenue: map[string]float64{venueBinance: binanceQuote, venueBybit: bybitQuote},
		ExposureBasisVI:      "test"}
}

type guardFixture struct {
	clock   *testClock
	binance *fakeMarginReader
	bybit   *fakeMarginReader
	closer  *fakeCloser
	guard   *MarginGuard
	events  *eventLog
}

type eventLog struct {
	mu  sync.Mutex
	evs []MarginEvent
}

func (l *eventLog) add(ev MarginEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.evs = append(l.evs, ev)
}

func (l *eventLog) kinds() []MarginEventKind {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]MarginEventKind, 0, len(l.evs))
	for _, e := range l.evs {
		out = append(out, e.Kind)
	}
	return out
}

func (l *eventLog) has(kind MarginEventKind) bool {
	for _, k := range l.kinds() {
		if k == kind {
			return true
		}
	}
	return false
}

func newGuardFixture(t *testing.T, binanceRatio, bybitRatio float64) *guardFixture {
	t.Helper()
	clock := newTestClock()
	fx := &guardFixture{
		clock:   clock,
		binance: &fakeMarginReader{name: venueBinance, clock: clock, ratio: binanceRatio},
		bybit:   &fakeMarginReader{name: venueBybit, clock: clock, ratio: bybitRatio},
		closer:  &fakeCloser{},
		events:  &eventLog{},
	}
	cfg := DefaultMarginGuardConfig()
	cfg.Now = clock.Now
	cfg.OnEvent = fx.events.add
	g, err := NewMarginGuard(cfg, []MarginReader{fx.binance, fx.bybit}, fx.closer)
	if err != nil {
		t.Fatal(err)
	}
	fx.guard = g
	return fx
}

// Every combination of engine and venues an open can ask about.
func everyOpen() []OpenRequest {
	return []OpenRequest{
		{Engine: "engine_2_cross_perp", CrossVenuePerp: true, Venues: []string{venueBinance, venueBybit}},
		{Engine: "engine_2_cross_perp", CrossVenuePerp: true, Venues: []string{venueBybit, venueBinance}},
		{Engine: "engine_1_cash_and_carry", Venues: []string{venueBinance}},
		{Engine: "engine_1_cash_and_carry", Venues: []string{venueBybit}},
	}
}

func TestMarginThresholds_ARatioAtABoundaryIsInTheHigherTier(t *testing.T) {
	th := DefaultMarginThresholds()
	for _, c := range []struct {
		ratio float64
		want  MarginTier
	}{
		{0, MarginTierGreen}, {0.4999, MarginTierGreen},
		{0.50, MarginTierYellow}, {0.5999, MarginTierYellow},
		{0.60, MarginTierOrange}, {0.6499, MarginTierOrange},
		{0.65, MarginTierRed}, {0.66, MarginTierRed}, {0.67, MarginTierRed}, {1.2, MarginTierRed},
		{math.Inf(1), MarginTierRed},
		{math.NaN(), MarginTierUnknown}, {-0.01, MarginTierUnknown},
	} {
		if got := th.TierOf(c.ratio); got != c.want {
			t.Errorf("TierOf(%v) = %s, want %s", c.ratio, got, c.want)
		}
	}
}

func TestDerivedMarginReading_EdgesAreNamedNotGuessed(t *testing.T) {
	r, err := DerivedMarginReading(venueBinance, "USDT", 660, 1000, "totalMaintMargin ÷ totalMarginBalance", 1)
	if err != nil || math.Abs(r.MaintenanceMarginRatioFrac-0.66) > 1e-12 {
		t.Fatalf("660 ÷ 1000 = %v, %v; want 0.66", r.MaintenanceMarginRatioFrac, err)
	}
	if r, err := DerivedMarginReading(venueBinance, "USDT", 0, 0, "", 1); err != nil || r.MaintenanceMarginRatioFrac != 0 {
		t.Errorf("no maintenance on an empty account = %v, %v; want 0 — nothing can be liquidated", r.MaintenanceMarginRatioFrac, err)
	}
	if r, err := DerivedMarginReading(venueBinance, "USDT", 5, 0, "", 1); err != nil || !math.IsInf(r.MaintenanceMarginRatioFrac, 1) {
		t.Errorf("maintenance against a zero balance = %v, %v; want +Inf (at or past liquidation), never 0", r.MaintenanceMarginRatioFrac, err)
	}
	if r, err := DerivedMarginReading(venueBinance, "USDT", 5, -3, "", 1); err != nil || DefaultMarginThresholds().TierOf(r.MaintenanceMarginRatioFrac) != MarginTierRed {
		t.Errorf("maintenance against a NEGATIVE balance must read red, got %v, %v", r.MaintenanceMarginRatioFrac, err)
	}
	for _, c := range []struct{ maint, bal float64 }{{-1, 100}, {math.NaN(), 100}, {1, math.NaN()}, {math.Inf(1), 100}, {1, math.Inf(1)}} {
		if _, err := DerivedMarginReading(venueBinance, "USDT", c.maint, c.bal, "", 1); !errors.Is(err, ErrMarginReadingInvalid) {
			t.Errorf("maint %v, balance %v: err %v, want ErrMarginReadingInvalid", c.maint, c.bal, err)
		}
	}
}

// Bybit's accountMMRate unit is not verified live, so the published figure is
// checked against the two totals published beside it. A percent-for-fraction
// slip is a named conflict, never a number the tiers act on.
func TestPublishedMarginReading_IsCrossCheckedAgainstItsOwnTotals(t *testing.T) {
	if r, err := PublishedMarginReading(venueBybit, "USD", 0.6712, true, 6712, 10_000, "accountMMRate", 1); err != nil || r.MaintenanceMarginRatioFrac != 0.6712 {
		t.Fatalf("agreeing evidence: %v, %v; want the published 0.6712", r.MaintenanceMarginRatioFrac, err)
	}
	if _, err := PublishedMarginReading(venueBybit, "USD", 67.12, true, 6712, 10_000, "accountMMRate", 1); !errors.Is(err, ErrMarginEvidenceConflict) {
		t.Errorf("published 67.12 (a percent) beside totals giving 0.6712: err %v, want ErrMarginEvidenceConflict", err)
	}
	if _, err := PublishedMarginReading(venueBybit, "USD", 0.006712, true, 6712, 10_000, "accountMMRate", 1); !errors.Is(err, ErrMarginEvidenceConflict) {
		t.Errorf("published 0.006712 beside totals giving 0.6712: err %v, want ErrMarginEvidenceConflict", err)
	}
	if _, err := PublishedMarginReading(venueBybit, "USD", 0, false, 0, 0, "accountMMRate \"\"", 1); !errors.Is(err, ErrMarginNotPublished) {
		t.Errorf("a blank ratio: err %v, want ErrMarginNotPublished — blank is not zero", err)
	}
	if _, err := PublishedMarginReading(venueBybit, "USD", 0.1, true, 50, 0, "accountMMRate", 1); !errors.Is(err, ErrMarginEvidenceConflict) {
		t.Errorf("published 10%% while the totals say past liquidation: err %v, want ErrMarginEvidenceConflict", err)
	}
}

func TestNewMarginGuard_RefusesWhatCannotGuard(t *testing.T) {
	clock := newTestClock()
	good := &fakeMarginReader{name: venueBinance, clock: clock}
	if _, err := NewMarginGuard(DefaultMarginGuardConfig(), nil, nil); err == nil {
		t.Error("no readers accepted")
	}
	if _, err := NewMarginGuard(DefaultMarginGuardConfig(), []MarginReader{good, &fakeMarginReader{name: venueBinance, clock: clock}}, nil); err == nil {
		t.Error("two readers for one venue accepted")
	}
	if _, err := NewMarginGuard(DefaultMarginGuardConfig(), []MarginReader{&fakeMarginReader{name: " ", clock: clock}}, nil); err == nil {
		t.Error("a reader with no venue name accepted")
	}
	for _, th := range []MarginThresholds{
		{YellowFrac: 0.6, OrangeFrac: 0.5, RedFrac: 0.65, ReleaseFrac: 0.5},
		{YellowFrac: 0.5, OrangeFrac: 0.6, RedFrac: 1.0, ReleaseFrac: 0.5},
		{YellowFrac: 0.5, OrangeFrac: 0.6, RedFrac: 0.65, ReleaseFrac: 0.7},
	} {
		cfg := DefaultMarginGuardConfig()
		cfg.Thresholds = th
		if _, err := NewMarginGuard(cfg, []MarginReader{good}, nil); err == nil {
			t.Errorf("thresholds %+v accepted", th)
		}
	}
}

func TestMarginGuard_NoReadingYetRefusesEveryOpen(t *testing.T) {
	fx := newGuardFixture(t, 0.10, 0.10)
	for _, req := range everyOpen() {
		if err := fx.guard.AllowOpen(req); !errors.Is(err, ErrOpenBlockedByMargin) {
			t.Errorf("%+v before any tick: %v, want a refusal — a margin nobody read is not safe", req, err)
		}
	}
	fx.guard.Tick(context.Background())
	for _, req := range everyOpen() {
		if err := fx.guard.AllowOpen(req); err != nil {
			t.Errorf("%+v at 10%%/10%%: %v, want allowed", req, err)
		}
	}
	if err := fx.guard.AllowOpen(OpenRequest{Engine: "x", Venues: []string{"okx_futures"}}); !errors.Is(err, ErrOpenBlockedByMargin) {
		t.Errorf("a venue the guard does not read: %v, want a refusal", err)
	}
}

// Yellow (≥ 50%) stops Engine 2 on THAT venue and nothing else.
func TestMarginGuard_YellowStopsOnlyEngine2OnTheVenueItNames(t *testing.T) {
	fx := newGuardFixture(t, 0.20, 0.55)
	fx.guard.Tick(context.Background())

	if err := fx.guard.AllowOpen(OpenRequest{Engine: "engine_2_cross_perp", CrossVenuePerp: true, Venues: []string{venueBinance, venueBybit}}); !errors.Is(err, ErrOpenBlockedByMargin) || !strings.Contains(err.Error(), venueBybit) {
		t.Errorf("Engine 2 across a yellow Bybit: %v, want a refusal naming %s", err, venueBybit)
	}
	if err := fx.guard.AllowOpen(OpenRequest{Engine: "engine_1_cash_and_carry", Venues: []string{venueBybit}}); err != nil {
		t.Errorf("Engine 1 on the yellow venue: %v — yellow is Engine 2's limit only", err)
	}
	if err := fx.guard.AllowOpen(OpenRequest{Engine: "engine_1_cash_and_carry", Venues: []string{venueBinance}}); err != nil {
		t.Errorf("Engine 1 on the green venue: %v", err)
	}
	if got := fx.closer.closedIDs(); len(got) != 0 {
		t.Errorf("yellow closed %v; only red closes", got)
	}
}

// Orange (≥ 60%) on ONE venue stops every engine on EVERY venue.
func TestMarginGuard_OrangeOnOneVenueStopsEveryOpenEverywhere(t *testing.T) {
	fx := newGuardFixture(t, 0.61, 0.05)
	fx.guard.Tick(context.Background())
	for _, req := range everyOpen() {
		if err := fx.guard.AllowOpen(req); !errors.Is(err, ErrOpenBlockedByMargin) {
			t.Errorf("%+v with Binance orange: %v, want a system-wide refusal", req, err)
		}
	}
	if got := fx.closer.closedIDs(); len(got) != 0 {
		t.Errorf("orange closed %v; only red closes", got)
	}

	// Staleness never lowers a block: the orange reading goes stale and the
	// next read fails, and the system stays closed to new positions.
	fx.binance.set(0, errors.New("venue down"))
	fx.clock.advance(time.Minute)
	fx.guard.Tick(context.Background())
	if err := fx.guard.AllowOpen(OpenRequest{Engine: "engine_1_cash_and_carry", Venues: []string{venueBybit}}); !errors.Is(err, ErrOpenBlockedByMargin) {
		t.Errorf("after the orange venue went dark: %v, want still refused", err)
	}
}

// The design's acceptance case: Binance MMR jumps to 66%. Every open is refused,
// Engine 2's pairs close largest-Binance-exposure first, the guard re-reads after
// each close and stops once every venue is under 50%, and the latch holds opens
// shut until the operator acknowledges the sequence number they saw.
func TestMarginGuard_BinanceAt66PercentClosesPairsAndRefusesEveryOpen(t *testing.T) {
	fx := newGuardFixture(t, 0.66, 0.30)
	fx.closer.pairs = []ExposedPair{
		pair("P1", 30_000, 30_000),
		pair("P2", 50_000, 5_000),
		pair("P3", 10_000, 90_000),
	}
	fx.closer.onClose = func(pairID string) {
		switch pairID {
		case "P2":
			fx.binance.set(0.58, nil) // still above release
		case "P1":
			fx.binance.set(0.45, nil) // below release
		}
	}

	report := fx.guard.Tick(context.Background())

	if got, want := strings.Join(fx.closer.closedIDs(), ","), "P2,P1"; got != want {
		t.Fatalf("closed %q, want %q — largest Binance exposure first, and stop once below 50%%", got, want)
	}
	fx.closer.mu.Lock()
	stressed := strings.Join(fx.closer.stressed, ",")
	fx.closer.mu.Unlock()
	if stressed != venueBinance+","+venueBinance {
		t.Errorf("stressed venue passed %q — the leg on the red venue closes first", stressed)
	}
	if !report.Emergency || report.EmergencySeq != 1 {
		t.Fatalf("report %+v, want the red latch #1 set", report)
	}
	for _, kind := range []MarginEventKind{MarginEventEmergencyStarted, MarginEventPairClosing, MarginEventPairClosed, MarginEventBelowRelease} {
		if !fx.events.has(kind) {
			t.Errorf("no %s event; got %v", kind, fx.events.kinds())
		}
	}

	// 100% of opens refused while latched, although every venue is now green.
	for _, req := range everyOpen() {
		if err := fx.guard.AllowOpen(req); !errors.Is(err, ErrOpenBlockedByMargin) {
			t.Errorf("%+v while the red latch is set: %v, want refused", req, err)
		}
	}

	// A later tick below release closes nothing more.
	fx.guard.Tick(context.Background())
	if got := fx.closer.closedIDs(); len(got) != 2 {
		t.Errorf("a tick below release closed more: %v", got)
	}

	// The acknowledgement must name the latch the operator saw.
	if err := fx.guard.Acknowledge(7); err == nil {
		t.Error("acknowledging #7 cleared latch #1")
	}
	if err := fx.guard.Acknowledge(1); err != nil {
		t.Fatalf("acknowledging #1 below release: %v", err)
	}
	for _, req := range everyOpen() {
		if err := fx.guard.AllowOpen(req); err != nil {
			t.Errorf("%+v after acknowledgement at 45%%/30%%: %v, want allowed", req, err)
		}
	}
	if err := fx.guard.Acknowledge(1); !errors.Is(err, ErrNothingToAcknowledge) {
		t.Errorf("a second acknowledgement: %v, want ErrNothingToAcknowledge", err)
	}
}

// The same on Bybit at 67%, read the way the Bybit reader reads it — a
// PUBLISHED ratio — and ranked by exposure on Bybit, the stressed venue.
func TestMarginGuard_BybitAt67PercentRanksByBybitExposure(t *testing.T) {
	fx := newGuardFixture(t, 0.20, 0.67)
	fx.closer.pairs = []ExposedPair{pair("P1", 90_000, 10_000), pair("P2", 5_000, 40_000)}
	fx.closer.onClose = func(string) { fx.bybit.set(0.40, nil) }

	fx.guard.Tick(context.Background())
	if got := fx.closer.closedIDs(); len(got) != 1 || got[0] != "P2" {
		t.Fatalf("closed %v, want [P2] — the pair with the most exposure on the red venue, then stop below 50%%", got)
	}
	if err := fx.guard.Acknowledge(0); err == nil {
		t.Error("acknowledging a latch number nobody was shown succeeded")
	}
	// Red again before anyone acknowledged: the remaining pair closes, nothing
	// lowers the ratio this time, and the acknowledgement is refused with the
	// numbers rather than reopening a red account.
	fx.closer.mu.Lock()
	fx.closer.onClose = nil
	fx.closer.mu.Unlock()
	fx.bybit.set(0.66, nil)
	fx.guard.Tick(context.Background())
	if got := fx.closer.closedIDs(); len(got) != 2 || got[1] != "P1" {
		t.Errorf("closed %v, want [P2 P1]", got)
	}
	if err := fx.guard.Acknowledge(1); err == nil || !strings.Contains(err.Error(), venueBybit) {
		t.Errorf("acknowledging while Bybit reads 66%%: %v, want a refusal naming the venue", err)
	}
}

// Unknown never closes: a failed read, a stale reading and conflicting evidence
// all refuse opens on that venue, and none of them pays for a round trip.
func TestMarginGuard_UnknownRefusesOpensThereAndNeverCloses(t *testing.T) {
	fx := newGuardFixture(t, 0.10, 0.10)
	fx.closer.pairs = []ExposedPair{pair("P1", 1, 1)}
	fx.guard.Tick(context.Background())

	cases := []struct {
		name  string
		setup func()
	}{
		{"read failed", func() { fx.bybit.set(0, errors.New("HTTP 503")) }},
		{"evidence conflict", func() {
			fx.bybit.set(0, ErrMarginEvidenceConflict)
		}},
		{"isolated margin", func() { fx.bybit.set(0, ErrMarginNotApplicable) }},
	}
	for _, c := range cases {
		c.setup()
		fx.guard.Tick(context.Background())
		if err := fx.guard.AllowOpen(OpenRequest{Engine: "engine_1_cash_and_carry", Venues: []string{venueBybit}}); !errors.Is(err, ErrOpenBlockedByMargin) {
			t.Errorf("%s: an open on Bybit was allowed (%v)", c.name, err)
		}
		if err := fx.guard.AllowOpen(OpenRequest{Engine: "engine_1_cash_and_carry", Venues: []string{venueBinance}}); err != nil {
			t.Errorf("%s: an open on the readable Binance was refused: %v", c.name, err)
		}
	}

	// Stale: the reader keeps answering but the clock runs past StaleAfter
	// between the read and the gate.
	fx.bybit.set(0.10, nil)
	fx.guard.Tick(context.Background())
	fx.clock.advance(DefaultMarginGuardConfig().StaleAfter + time.Second)
	if err := fx.guard.AllowOpen(OpenRequest{Engine: "engine_1_cash_and_carry", Venues: []string{venueBybit}}); !errors.Is(err, ErrOpenBlockedByMargin) {
		t.Errorf("stale reading: %v, want refused", err)
	}

	if got := fx.closer.closedIDs(); len(got) != 0 {
		t.Errorf("an unknown margin closed %v — nothing may be closed on evidence that cannot be trusted", got)
	}
	if !fx.events.has(MarginEventReadFailed) {
		t.Errorf("no read_failed alarm; events %v", fx.events.kinds())
	}
}

func TestMarginGuard_AFailedCloseMovesToTheNextPair(t *testing.T) {
	fx := newGuardFixture(t, 0.70, 0.10)
	fx.closer.pairs = []ExposedPair{pair("BIG", 80_000, 1), pair("SMALL", 20_000, 1)}
	fx.closer.failFor = map[string]error{"BIG": errors.New("venue refused the reduce-only order")}
	fx.closer.onClose = func(string) { fx.binance.set(0.30, nil) }

	report := fx.guard.Tick(context.Background())
	if len(report.FailedPairIDs) != 1 || report.FailedPairIDs[0] != "BIG" {
		t.Errorf("failed %v, want [BIG]", report.FailedPairIDs)
	}
	if got := fx.closer.closedIDs(); len(got) != 1 || got[0] != "SMALL" {
		t.Errorf("closed %v, want [SMALL] after BIG failed", got)
	}
	if !fx.events.has(MarginEventPairCloseFailed) {
		t.Errorf("no pair_close_failed alarm: %v", fx.events.kinds())
	}
}

// Red with no Engine-2 pair left (Engine 1's margin holds it up) stops and says
// so, instead of spinning or touching Engine 1.
func TestMarginGuard_RedWithNothingOfEngine2LeftSaysSoAndStops(t *testing.T) {
	fx := newGuardFixture(t, 0.80, 0.10)
	fx.guard.Tick(context.Background())
	fx.guard.Tick(context.Background())
	if !fx.events.has(MarginEventNothingToClose) {
		t.Errorf("no nothing_to_close alarm: %v", fx.events.kinds())
	}
	if err := fx.guard.AllowOpen(OpenRequest{Engine: "engine_1_cash_and_carry", Venues: []string{venueBybit}}); !errors.Is(err, ErrOpenBlockedByMargin) {
		t.Errorf("red with nothing to close still let an open through: %v", err)
	}
}

// Run ticking in the background while engines ask the gate and a page reads the
// snapshot — under -race.
func TestMarginGuard_RunAndGateAreSafeForConcurrentUse(t *testing.T) {
	fx := newGuardFixture(t, 0.20, 0.20)
	cfg := DefaultMarginGuardConfig()
	cfg.Interval = time.Millisecond
	cfg.Now = time.Now
	g, err := NewMarginGuard(cfg, []MarginReader{fx.binance, fx.bybit}, fx.closer)
	if err != nil {
		t.Fatal(err)
	}
	fx.binance.clock = &testClock{}
	fx.binance.clock.ms.Store(time.Now().UnixMilli())
	fx.bybit.clock = fx.binance.clock

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = g.Run(ctx) }()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for ctx.Err() == nil {
				_ = g.AllowOpen(everyOpen()[i])
				_ = g.Snapshot()
				fx.bybit.set(0.2+float64(i%3)*0.2, nil)
				fx.binance.clock.ms.Store(time.Now().UnixMilli())
			}
		}(i)
	}
	wg.Wait()
}

// Review 4.5k, M3: once red has latched, a venue that becomes UNKNOWN is not a
// reason to keep closing. The first close brings Binance to 30%; Bybit's read
// then fails — nothing more is closed, and it says why.
func TestMarginGuard_ALatchedEmergencyDoesNotCloseOnUnknown(t *testing.T) {
	fx := newGuardFixture(t, 0.66, 0.30)
	fx.closer.pairs = []ExposedPair{pair("P1", 30_000, 30_000), pair("P2", 50_000, 5_000), pair("P3", 10_000, 90_000)}
	fx.closer.onClose = func(pairID string) {
		if pairID == "P2" {
			fx.binance.set(0.30, nil)
			fx.bybit.set(0, errors.New("HTTP 503 bybit"))
		}
	}
	fx.guard.Tick(context.Background())
	if got := fx.closer.closedIDs(); len(got) != 1 || got[0] != "P2" {
		t.Fatalf("closed %v, want only [P2] — Bybit unknown and Binance at 30%% is not a reason to close", got)
	}
	if !fx.events.has(MarginEventStoppedOnUnknown) {
		t.Errorf("no stopped_on_unknown alarm: %v", fx.events.kinds())
	}
	// Later ticks with Bybit still dark close nothing either; the latch holds.
	fx.guard.Tick(context.Background())
	fx.guard.Tick(context.Background())
	if got := fx.closer.closedIDs(); len(got) != 1 {
		t.Fatalf("later ticks closed %v", got)
	}
	if err := fx.guard.AllowOpen(OpenRequest{Engine: "engine_1_cash_and_carry", Venues: []string{venueBinance}}); !errors.Is(err, ErrOpenBlockedByMargin) {
		t.Errorf("the latch let an open through: %v", err)
	}
	// And the alarm is not repeated every tick (review 4.5k, m8).
	n := 0
	for _, k := range fx.events.kinds() {
		if k == MarginEventStoppedOnUnknown {
			n++
		}
	}
	if n != 1 {
		t.Errorf("stopped_on_unknown raised %d times over three ticks, want once per latch", n)
	}
}

// Review 4.5k, M8: two pieces of evidence that BOTH say red still start the
// emergency; one that only the HIGHER figure calls orange still blocks every
// open system-wide — and neither is averaged.
func TestMarginGuard_ConflictingEvidenceTakesTheSafeSideOfEachDecision(t *testing.T) {
	fx := newGuardFixture(t, 0.10, 0.10)
	fx.closer.pairs = []ExposedPair{pair("P1", 1_000, 50_000)}
	fx.closer.onClose = func(string) { fx.bybit.set(0.20, nil) }

	fx.bybit.setConflict(0.70, 0.66) // both red, 4 points apart
	report := fx.guard.Tick(context.Background())
	if !report.Emergency || len(fx.closer.closedIDs()) != 1 {
		t.Fatalf("both figures red: emergency=%v closed=%v — want the latch and the close", report.Emergency, fx.closer.closedIDs())
	}

	fx2 := newGuardFixture(t, 0.10, 0.10)
	fx2.closer.pairs = []ExposedPair{pair("P1", 1_000, 50_000)}
	fx2.guard.Tick(context.Background())
	fx2.bybit.setConflict(0.90, 0.30) // only the higher figure is alarming
	report = fx2.guard.Tick(context.Background())
	if report.Emergency || len(fx2.closer.closedIDs()) != 0 {
		t.Fatalf("a conflict whose lower figure is 30%%: emergency=%v closed=%v — no close on a contradicted number", report.Emergency, fx2.closer.closedIDs())
	}
	for _, req := range everyOpen() {
		if err := fx2.guard.AllowOpen(req); !errors.Is(err, ErrOpenBlockedByMargin) {
			t.Errorf("%+v beside a 90%% conflicting reading: %v — the higher figure blocks system-wide", req, err)
		}
	}
	// Staleness never lowers that block: Bybit stops answering altogether.
	fx2.bybit.set(0, errors.New("HTTP 503"))
	fx2.guard.Tick(context.Background())
	if err := fx2.guard.AllowOpen(OpenRequest{Engine: "engine_1_cash_and_carry", Venues: []string{venueBinance}}); !errors.Is(err, ErrOpenBlockedByMargin) {
		t.Errorf("the conflict's block fell away when the venue went dark: %v", err)
	}
	// A clean reading below orange lifts it.
	fx2.bybit.set(0.20, nil)
	fx2.guard.Tick(context.Background())
	if err := fx2.guard.AllowOpen(OpenRequest{Engine: "engine_1_cash_and_carry", Venues: []string{venueBinance}}); err != nil {
		t.Errorf("after a clean 20%% reading: %v", err)
	}
}

// A pair whose own evidence conflicts is raised, never closed automatically.
func TestMarginGuard_ACloseBlockedPairIsRaisedNotClosed(t *testing.T) {
	fx := newGuardFixture(t, 0.70, 0.10)
	blocked := pair("B", 90_000, 1)
	blocked.CloseBlockedVI = "lệnh và vị thế sàn nói khác nhau"
	fx.closer.pairs = []ExposedPair{blocked, pair("OK", 10_000, 1)}
	fx.closer.onClose = func(string) { fx.binance.set(0.30, nil) }
	fx.guard.Tick(context.Background())
	if got := fx.closer.closedIDs(); len(got) != 1 || got[0] != "OK" {
		t.Fatalf("closed %v, want [OK] — B is blocked", got)
	}
	if !fx.events.has(MarginEventCloseBlocked) {
		t.Errorf("no close_blocked alarm: %v", fx.events.kinds())
	}
}

// Review 4.5k, N6: the system-wide block stands on the higher figure of the
// conflicting answers of the last StaleAfter. One 62% print followed by answers
// that conflict at 13%/10% blocks for StaleAfter and no longer — at the guard's
// 5 s cadence — while a venue flapping back to a high contradiction inside that
// window keeps it up, and silence after a high print keeps it up for good.
func TestMarginGuard_ANewerConflictingAnswerReplacesTheBlockItsPredecessorSet(t *testing.T) {
	fx := newGuardFixture(t, 0.10, 0.10)
	tick := func() {
		fx.clock.advance(5 * time.Second)
		fx.binance.set(0.10, nil) // keeps Binance's own reading fresh
		fx.guard.Tick(context.Background())
	}
	engine1OnBinance := OpenRequest{Engine: "engine_1_cash_and_carry", Venues: []string{venueBinance}}
	fx.bybit.setConflict(0.62, 0.58)
	tick()
	if err := fx.guard.AllowOpen(engine1OnBinance); !errors.Is(err, ErrOpenBlockedByMargin) {
		t.Fatalf("beside a 62%% conflicting print: %v", err)
	}
	fx.bybit.setConflict(0.13, 0.10)
	tick() // 5 s after the high print
	if err := fx.guard.AllowOpen(engine1OnBinance); !errors.Is(err, ErrOpenBlockedByMargin) {
		t.Errorf("5 s after a 62%% print: %v — a flap inside StaleAfter must not open the system", err)
	}
	for i := 0; i < 3; i++ {
		tick() // 10, 15 and 20 s after the high print
	}
	if err := fx.guard.AllowOpen(engine1OnBinance); err != nil {
		t.Errorf("20 s of fresh conflicting answers at 13%%/10%%: %v — the 62%% print no longer describes the venue", err)
	}
	if err := fx.guard.AllowOpen(OpenRequest{Engine: "engine_1_cash_and_carry", Venues: []string{venueBybit}}); !errors.Is(err, ErrOpenBlockedByMargin) {
		t.Errorf("an open ON the conflicting venue: %v — its own opens stay refused", err)
	}
	fx.bybit.setConflict(0.63, 0.59)
	tick()
	fx.bybit.set(0, errors.New("HTTP 503"))
	for i := 0; i < 10; i++ {
		tick()
	}
	if err := fx.guard.AllowOpen(engine1OnBinance); !errors.Is(err, ErrOpenBlockedByMargin) {
		t.Errorf("a high conflict then 50 s of silence: %v — staleness never lowers a block", err)
	}
}

// Review 4.5k, N6: a red latch can be acknowledged while a venue keeps answering
// in conflict — once BOTH of its figures are below the release threshold.
func TestMarginGuard_ARedLatchCanBeAcknowledgedUnderAPersistentLowConflict(t *testing.T) {
	fx := newGuardFixture(t, 0.70, 0.10)
	fx.closer.onClose = func(string) {}
	fx.guard.Tick(context.Background()) // Binance red: latch #1
	fx.binance.set(0.10, nil)
	fx.bybit.setConflict(0.55, 0.10) // the higher figure is above release
	fx.guard.Tick(context.Background())
	if err := fx.guard.Acknowledge(1); err == nil || !strings.Contains(err.Error(), "mâu thuẫn") {
		t.Errorf("acknowledging beside a conflict whose higher figure is 55%%: %v — want refused, naming the conflict", err)
	}
	fx.bybit.setConflict(0.13, 0.10)
	fx.guard.Tick(context.Background())
	if err := fx.guard.Acknowledge(1); err != nil {
		t.Fatalf("acknowledging beside a conflict at 13%%/10%%: %v", err)
	}
}

// Review 4.5k, m-a: when the emergency stops on evidence that is missing or
// contradicts itself, the message names what each venue said.
func TestMarginGuard_TheStopMessageNamesEachVenuesEvidence(t *testing.T) {
	fx := newGuardFixture(t, 0.70, 0.10)
	fx.closer.pairs = []ExposedPair{pair("P1", 50_000, 5_000), pair("P2", 40_000, 4_000)}
	fx.closer.onClose = func(string) {
		fx.binance.set(0.30, nil)
		fx.bybit.setConflict(0.60, 0.40) // the lower figure is under release
	}
	fx.guard.Tick(context.Background())
	var msg string
	for _, ev := range fx.guard.Snapshot().RecentEvents {
		if ev.Kind == MarginEventStoppedOnUnknown {
			msg = ev.MessageVI
		}
	}
	if !strings.Contains(msg, venueBybit+" MÂU THUẪN") || !strings.Contains(msg, "60.00%") || !strings.Contains(msg, venueBinance+" 30.00%") {
		t.Fatalf("stop message %q — want each venue's evidence", msg)
	}
	if got := fx.closer.closedIDs(); len(got) != 1 {
		t.Errorf("closed %v, want one close before the stop", got)
	}
}

// Review 4.5k, m-b: which venue to relieve first is ranked on a conflict's HIGHER
// figure. Bybit publishes 95% against 55% derived; Binance reads a clean 66%.
func TestMarginGuard_TheStressedVenueIsRankedOnAConflictsHigherFigure(t *testing.T) {
	fx := newGuardFixture(t, 0.66, 0.10)
	fx.bybit.setConflict(0.95, 0.55)
	fx.closer.pairs = []ExposedPair{pair("BINANCE_HEAVY", 90_000, 10_000), pair("BYBIT_HEAVY", 10_000, 90_000)}
	fx.closer.onClose = func(string) {}
	fx.guard.Tick(context.Background())
	fx.closer.mu.Lock()
	stressed := append([]string(nil), fx.closer.stressed...)
	fx.closer.mu.Unlock()
	if got := fx.closer.closedIDs(); len(got) == 0 || got[0] != "BYBIT_HEAVY" || stressed[0] != venueBybit {
		t.Fatalf("closed %v with stressed %v — want BYBIT_HEAVY first, Bybit's leg first", got, stressed)
	}
}

// Review 4.5k, m-c: with three venues, the most stressed venue overall may hold no
// leg of the pair being closed. The close is told the most stressed venue AMONG
// ITS OWN LEGS, never a venue a close would refuse.
func TestMarginGuard_TheStressedVenueIsOneOfThePairsOwnLegs(t *testing.T) {
	clock := newTestClock()
	okx := &fakeMarginReader{name: "okx_futures", clock: clock, ratio: 0.90}
	binance := &fakeMarginReader{name: venueBinance, clock: clock, ratio: 0.66}
	bybit := &fakeMarginReader{name: venueBybit, clock: clock, ratio: 0.20}
	closer := &fakeCloser{pairs: []ExposedPair{pair("P", 40_000, 40_000)}}
	cfg := DefaultMarginGuardConfig()
	cfg.Now = clock.Now
	g, err := NewMarginGuard(cfg, []MarginReader{okx, binance, bybit}, closer)
	if err != nil {
		t.Fatal(err)
	}
	g.Tick(context.Background())
	closer.mu.Lock()
	defer closer.mu.Unlock()
	if len(closer.stressed) == 0 || closer.stressed[0] != venueBinance {
		t.Fatalf("stressed %v — want binance_futures, the most stressed of the pair's own two venues", closer.stressed)
	}
}

// Review 4.5k, m-d: the de-duplication keys of a finished emergency do not
// accumulate across emergencies.
func TestMarginGuard_OnceOnlyKeysDoNotOutliveTheirEmergency(t *testing.T) {
	fx := newGuardFixture(t, 0.70, 0.10)
	for i := 0; i < 5; i++ {
		blocked := pair(fmt.Sprintf("B%d", i), 90_000, 1)
		blocked.CloseBlockedVI = "test"
		fx.closer.pairs = []ExposedPair{blocked}
		fx.binance.set(0.70, nil)
		fx.guard.Tick(context.Background())
		fx.binance.set(0.10, nil)
		fx.guard.Tick(context.Background())
		if err := fx.guard.Acknowledge(uint64(i + 1)); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	fx.guard.mu.Lock()
	defer fx.guard.mu.Unlock()
	if n := len(fx.guard.reported); n > 3 {
		t.Fatalf("%d once-only keys after five emergencies: %v", n, fx.guard.reported)
	}
}

// Review 4.5k round 3, M6: every orange answer of the window counts, not only
// its peak. Conflicting answers at 62%, then 61% three times, then 13%, five
// seconds apart: at the 13% tick the last StaleAfter still holds a 61% print.
func TestMarginGuard_EveryOrangeConflictOfTheWindowCounts(t *testing.T) {
	fx := newGuardFixture(t, 0.10, 0.10)
	tick := func() {
		fx.clock.advance(5 * time.Second)
		fx.binance.set(0.10, nil)
		fx.guard.Tick(context.Background())
	}
	engine1OnBinance := OpenRequest{Engine: "engine_1_cash_and_carry", Venues: []string{venueBinance}}
	fx.bybit.setConflict(0.62, 0.58)
	tick()
	for i := 0; i < 3; i++ {
		fx.bybit.setConflict(0.61, 0.57)
		tick()
	}
	fx.bybit.setConflict(0.13, 0.10)
	tick()
	if err := fx.guard.AllowOpen(engine1OnBinance); !errors.Is(err, ErrOpenBlockedByMargin) || !strings.Contains(err.Error(), "61.00%") {
		t.Fatalf("one low print 5 s after an orange one: %v — want blocked on the 61%%", err)
	}
	for i := 0; i < 3; i++ {
		tick() // 13% answers until the 61% prints are older than StaleAfter
	}
	if err := fx.guard.AllowOpen(engine1OnBinance); err != nil {
		t.Errorf("after 20 s of 13%%/10%% answers: %v", err)
	}
}
