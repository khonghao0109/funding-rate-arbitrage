package execution

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/broker/brokertest"
)

// Every test here asserts the INVARIANT, and asserts it against what the fake
// VENUES hold — never against what Result says happened.
//
// That distinction is the whole point. A state machine that has lost track of a
// leg will happily report OutcomeBothFlat while the venue still holds the
// position, and a test that believed the Result would agree with it. So
// assertInvariant sums the fills the fakes actually recorded, the way rule 7
// says a real position is established.

type harness struct {
	spot   *brokertest.Fake
	perp   *brokertest.Fake
	rec    *MemoryRecorder
	opener *Opener
	intent Intent
	cfg    Config
}

func newHarness(t *testing.T, tune func(*Config)) *harness {
	t.Helper()
	nowMs := time.Now().UnixMilli()
	cfg := DefaultConfig()
	cfg.LegTimeout = 300 * time.Millisecond
	cfg.UnwindTimeout = 2 * time.Second
	cfg.PollEvery = 10 * time.Millisecond
	if tune != nil {
		tune(&cfg)
	}
	h := &harness{
		spot:   brokertest.New(),
		perp:   brokertest.New(),
		rec:    NewMemoryRecorder(),
		intent: testIntent(nowMs),
		cfg:    cfg,
	}
	// The venues keep their own position and base balance and move them as
	// orders fill, so every assertion about "what the venue holds" is about
	// something the venue did rather than something the test wrote down.
	h.spot.SetBaseAsset(h.intent.SpotInstrument.BaseAsset)
	h.perp.SetBaseAsset(h.intent.PerpInstrument.BaseAsset)

	// Fills land on each venue's own quantity grid, as a real venue's do.
	// Without this the fake reports fills BETWEEN two grid points, which no
	// venue does and which no correctly-rounded closing order could ever
	// close.
	h.spot.SetStepSizeCoin(h.intent.SpotInstrument.StepSizeCoin)
	h.perp.SetStepSizeCoin(h.intent.PerpInstrument.StepSizeCoin)

	// Both legs fill on placement unless a test says otherwise.
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1})
	h.perp.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1})

	o, err := NewOpener(h.spot, h.perp, cfg, h.rec)
	if err != nil {
		t.Fatal(err)
	}
	h.opener = o
	return h
}

// netQtyCoin is what a venue actually holds because of this run: buys minus
// sells, over every order the fake recorded. An unwound leg nets to zero.
func netQtyCoin(f *brokertest.Fake, market broker.Market) float64 {
	sum := 0.0
	for _, o := range f.Orders() {
		if o.Market != market {
			continue
		}
		if o.Side == broker.SideSell {
			sum -= o.FilledQtyCoin
			continue
		}
		sum += o.FilledQtyCoin
	}
	return sum
}

// assertInvariant is the only assertion that matters, and it reads the VENUES.
func (h *harness) assertInvariant(t *testing.T, res Result) {
	t.Helper()
	spotNet := netQtyCoin(h.spot, broker.MarketSpot)
	perpNet := netQtyCoin(h.perp, broker.MarketFuturesUSDM)
	tol := math.Max(h.intent.SpotInstrument.StepSizeCoin, h.intent.PerpInstrument.StepSizeCoin)

	// perpNet is negative for a short; the hedge is spot + perp ~ 0.
	residual := math.Abs(spotNet + perpNet)
	bothFlat := spotNet == 0 && perpNet == 0
	bothOpen := spotNet > 0 && perpNet < 0 && residual <= tol+1e-12

	if !bothFlat && !bothOpen {
		t.Fatalf("INVARIANT VIOLATED at the venues: spot net %.10g, perp net %.10g, residual %.10g > tolerance %.10g\n"+
			"  result said %q (%s)\n  events:%s",
			spotNet, perpNet, residual, tol, res.Outcome, res.ReasonVI, h.rec.Dump())
	}
	// And the Result must not disagree with the venues about which of the two
	// states it is — a truthful machine that lies in its report is still a
	// machine nobody can act on.
	switch {
	case bothOpen && res.Outcome != OutcomeBothOpen:
		t.Errorf("the venues are hedged but the result says %q%s", res.Outcome, h.rec.Dump())
	case bothFlat && res.Outcome != OutcomeBothFlat:
		t.Errorf("the venues are flat but the result says %q%s", res.Outcome, h.rec.Dump())
	}
}

// ---------------------------------------------------------------- the cases

func TestOpen_BothLegsFillInFull(t *testing.T) {
	h := newHarness(t, nil)
	res, err := h.opener.Open(context.Background(), h.intent)
	if err != nil {
		t.Fatalf("Open: %v%s", err, h.rec.Dump())
	}
	h.assertInvariant(t, res)
	if !res.Hedged() {
		t.Fatalf("outcome = %q, want both_open", res.Outcome)
	}
	if res.ResidualQtyCoin != 0 {
		t.Errorf("residual = %v, want 0 on commensurable grids", res.ResidualQtyCoin)
	}
	if res.Spot.FilledQtyCoin != res.TargetQtyCoin || res.Perp.FilledQtyCoin != res.TargetQtyCoin {
		t.Errorf("legs filled %v and %v, target %v", res.Spot.FilledQtyCoin, res.Perp.FilledQtyCoin, res.TargetQtyCoin)
	}
	// Each leg carries the id THIS process derived, which is what a restarted
	// process would look for.
	if res.Spot.ClientOrderID != LegClientOrderID(h.intent.ID, LegSpot) {
		t.Errorf("spot leg id = %q, not the derived one", res.Spot.ClientOrderID)
	}
	if res.UnwindDuration != 0 {
		t.Errorf("an unwind ran on a clean open: %v", res.UnwindDuration)
	}
}

func TestOpen_Leg2RejectedOutright_Leg1IsUnwound(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"under the venue minimum notional", fmt.Errorf("%w: notional 12 < 50", broker.ErrBelowMinNotional)},
		{"precision the venue will not accept", fmt.Errorf("%w: -1111 precision", broker.ErrInvalidOrder)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.perp.SetBehaviour(brokertest.Behaviour{RejectWith: tc.err})

			startedAt := time.Now()
			res, err := h.opener.Open(context.Background(), h.intent)
			elapsed := time.Since(startedAt)

			if err == nil {
				t.Fatal("a rejected second leg must be an error")
			}
			h.assertInvariant(t, res)
			if res.Outcome != OutcomeBothFlat {
				t.Fatalf("outcome = %q, want both_flat", res.Outcome)
			}
			// PLAN 4.4's acceptance, in seconds: leg 2 fails, leg 1 closes.
			if elapsed > 5*time.Second {
				t.Errorf("the unwind took %v; the acceptance says a few seconds", elapsed)
			}
			t.Logf("unwind measured at %v (wall clock), reported %v", elapsed, res.UnwindDuration)

			// A definite rejection must NOT have been resent: the venue
			// answered, and asking it again wastes the deadline.
			if h.rec.Has(EventLegResent) {
				t.Error("a definite rejection was resent")
			}
			if !h.rec.Has(EventUnwindStarted) || !h.rec.Has(EventUnwindLeg) {
				t.Errorf("no unwind was recorded%s", h.rec.Dump())
			}
		})
	}
}

func TestOpen_Leg2TimesOutBeforeReachingTheVenue_ResentOnceThenFills(t *testing.T) {
	h := newHarness(t, nil)
	h.perp.SetBehaviour(brokertest.Behaviour{
		FillFractionOnPlace: 1, PlaceTimesOutBeforeAccepting: true, TimeoutTimes: 1,
	})

	res, err := h.opener.Open(context.Background(), h.intent)
	if err != nil {
		t.Fatalf("Open: %v%s", err, h.rec.Dump())
	}
	h.assertInvariant(t, res)
	if res.Outcome != OutcomeBothOpen {
		t.Fatalf("outcome = %q, want both_open after a safe resend", res.Outcome)
	}
	if !h.rec.Has(EventLegAmbiguous) {
		t.Error("the timeout was not recorded as ambiguous")
	}
	if !h.rec.Has(EventLegResent) {
		t.Errorf("no resend was recorded, although the venue positively did not have the order%s", h.rec.Dump())
	}
	// Exactly ONE perp order exists. A resend down the wrong branch is how a
	// position ends up doubled and unhedged.
	if n := len(h.perp.Orders()); n != 1 {
		t.Errorf("the venue holds %d perp orders, want 1", n)
	}
}

func TestOpen_Leg2TimesOutAfterTheVenueAccepted_FoundByGetOrderAndNotSentTwice(t *testing.T) {
	h := newHarness(t, nil)
	h.perp.SetBehaviour(brokertest.Behaviour{
		FillFractionOnPlace: 1, PlaceTimesOutAfterAccepting: true, TimeoutTimes: 1,
	})

	res, err := h.opener.Open(context.Background(), h.intent)
	if err != nil {
		t.Fatalf("Open: %v%s", err, h.rec.Dump())
	}
	h.assertInvariant(t, res)
	if res.Outcome != OutcomeBothOpen {
		t.Fatalf("outcome = %q, want both_open — the order was there all along", res.Outcome)
	}
	// THE point of this case: the order was already at the venue, so it must
	// NOT have been sent again. Two orders here is a doubled, unhedged
	// position.
	if n := len(h.perp.Orders()); n != 1 {
		t.Fatalf("the venue holds %d perp orders, want exactly 1 — the ambiguous send was repeated", n)
	}
	if h.rec.Has(EventLegResent) {
		t.Error("the order was resent although the venue already had it")
	}
	if !h.rec.Has(EventLegAmbiguous) {
		t.Error("the ambiguity was not recorded")
	}
}

func TestOpen_Leg1FillsPartiallyThenExpires_CancelledAndTheFilledPartUnwound(t *testing.T) {
	h := newHarness(t, nil)
	// Half fills and the rest rests; the leg timeout then expires.
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.5})

	res, err := h.opener.Open(context.Background(), h.intent)
	if err == nil {
		t.Fatal("a leg that never reached its target must be an error")
	}
	h.assertInvariant(t, res)
	if res.Outcome != OutcomeBothFlat {
		t.Fatalf("outcome = %q, want both_flat%s", res.Outcome, h.rec.Dump())
	}
	// The unwind must be sized to what FILLED, not to the intended notional.
	if res.Spot.UnwoundQtyCoin <= 0 {
		t.Fatalf("nothing was unwound, though half the leg filled%s", h.rec.Dump())
	}
	if want := res.TargetQtyCoin * 0.5; math.Abs(res.Spot.UnwoundQtyCoin-want) > 1e-9 {
		t.Errorf("unwound %v, want the %v that actually filled — sizing an unwind from the intended notional is how a partial fill becomes an opposite position",
			res.Spot.UnwoundQtyCoin, want)
	}
	// The perp leg was never placed at all.
	if n := len(h.perp.Orders()); n != 0 {
		t.Errorf("the perp venue holds %d orders, want 0 — leg 2 must not be placed after leg 1 fell short", n)
	}
	if !h.rec.Has(EventLegCancelling) || !h.rec.Has(EventLegReadBack) {
		t.Errorf("the cancel and its read-back were not both recorded%s", h.rec.Dump())
	}
}

func TestOpen_CancelRacesAFill_TheVenueIsRereadAndTheOutcomeStillHolds(t *testing.T) {
	h := newHarness(t, nil)
	// The spot leg rests, so it reaches its deadline and is cancelled — and
	// the cancel finds it already filled, which is the race.
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0, CancelRacesAFill: true})

	res, err := h.opener.Open(context.Background(), h.intent)
	h.assertInvariant(t, res)

	// Trusting our own cancel would have concluded "nothing filled" and left
	// the venue holding a full spot position with no hedge. Reading it back
	// discovers the fill, so the machine goes on to hedge it.
	if res.Outcome != OutcomeBothOpen {
		t.Fatalf("outcome = %q, want both_open: the cancel lost the race, the position is real, and it must be hedged rather than abandoned (err %v)%s",
			res.Outcome, err, h.rec.Dump())
	}
	if !h.rec.Has(EventLegReadBack) {
		t.Fatalf("no read-back was recorded%s", h.rec.Dump())
	}
}

func TestOpen_AThinnedBookPlacesNothingAtAll(t *testing.T) {
	h := newHarness(t, nil)
	h.intent.SpotBook = thinBook("binance_spot", 60_000, h.intent.SpotBook.SampledAtMs)

	res, err := h.opener.Open(context.Background(), h.intent)
	if !errors.Is(err, ErrRefusedBeforePlacing) {
		t.Fatalf("err = %v, want a before-placing refusal", err)
	}
	h.assertInvariant(t, res)
	if n := len(h.spot.Orders()) + len(h.perp.Orders()); n != 0 {
		t.Fatalf("%d orders were placed against a book that could not price the size", n)
	}
	if res.Outcome != OutcomeBothFlat {
		t.Errorf("outcome = %q, want both_flat", res.Outcome)
	}
}

func TestOpen_ContextCancelledMidway_StillReachesTheInvariant(t *testing.T) {
	h := newHarness(t, nil)
	// Leg 1 fills; leg 2 rests, so the run is still working when the context
	// dies underneath it.
	h.perp.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()

	res, err := h.opener.Open(ctx, h.intent)
	if err == nil {
		t.Fatal("a cancelled open must report an error")
	}
	// The unwind runs on a context DERIVED FROM but not cancelled with the
	// caller's: a caller who gave up is exactly the caller who must not be
	// left holding one leg.
	h.assertInvariant(t, res)
	if res.Outcome != OutcomeBothFlat {
		t.Fatalf("outcome = %q, want both_flat after cancellation%s", res.Outcome, h.rec.Dump())
	}
}

// ---------------------------------------------------------------- property

// The property test: drive the fakes through randomly chosen behaviours and
// assert the invariant after every single run.
//
// It is not looking for a specific bug. It is looking for the combination
// nobody wrote a case for — and the cases above are all combinations somebody
// did.
func TestOpen_InvariantHoldsUnderRandomBehaviour(t *testing.T) {
	const runs = 240
	knobs := []brokertest.Behaviour{
		{FillFractionOnPlace: 1},
		{FillFractionOnPlace: 0},
		{FillFractionOnPlace: 0.5},
		{FillFractionOnPlace: 0.9},
		{FillFractionOnPlace: 1, PlaceTimesOutAfterAccepting: true, TimeoutTimes: 1},
		{FillFractionOnPlace: 1, PlaceTimesOutBeforeAccepting: true, TimeoutTimes: 1},
		{FillFractionOnPlace: 0, CancelRacesAFill: true},
		{FillFractionOnPlace: 0.5, CancelRacesAFill: true},
		{RejectWith: fmt.Errorf("%w: rejected", broker.ErrBelowMinNotional)},
		{FillFractionOnPlace: 1, PlaceTimesOutAfterAccepting: true},
		{FillFractionOnPlace: 1, PlaceTimesOutBeforeAccepting: true},
	}

	violations := 0
	for i := 0; i < runs; i++ {
		spotKnob := knobs[i%len(knobs)]
		perpKnob := knobs[(i/len(knobs)+i*7)%len(knobs)]

		h := newHarness(t, func(c *Config) {
			// Short deadlines: this test is about which STATE each run lands
			// in, not about how long a venue is given, and 240 runs at the
			// default deadline is a minute of waiting for timers.
			c.LegTimeout = 25 * time.Millisecond
			c.PollEvery = 3 * time.Millisecond
			c.UnwindTimeout = time.Second
		})
		h.spot.SetBehaviour(spotKnob)
		h.perp.SetBehaviour(perpKnob)

		res, _ := h.opener.Open(context.Background(), h.intent)

		spotNet := netQtyCoin(h.spot, broker.MarketSpot)
		perpNet := netQtyCoin(h.perp, broker.MarketFuturesUSDM)
		tol := math.Max(h.intent.SpotInstrument.StepSizeCoin, h.intent.PerpInstrument.StepSizeCoin)
		residual := math.Abs(spotNet + perpNet)
		bothFlat := spotNet == 0 && perpNet == 0
		bothOpen := spotNet > 0 && perpNet < 0 && residual <= tol+1e-12

		if !bothFlat && !bothOpen {
			violations++
			t.Errorf("run %d VIOLATED the invariant: spot net %.10g, perp net %.10g (residual %.10g)\n"+
				"  spot knob %+v\n  perp knob %+v\n  result %q (%s)%s",
				i, spotNet, perpNet, residual, spotKnob, perpKnob, res.Outcome, res.ReasonVI, h.rec.Dump())
			if violations > 3 {
				t.Fatal("stopping after four violations")
			}
		}
	}
	t.Logf("%d randomised runs, %d invariant violations", runs, violations)
}

// A 5xx is NOT a refusal, and the difference is the whole ambiguity contract.
//
// A 4xx comes from the matching engine: the order does not exist and never
// will, so resending is pointless and asking wastes the deadline. A 502 or 503
// typically comes from a gateway in FRONT of the venue, which may have
// forwarded the order perfectly well before failing to relay the reply. Treating
// it as a refusal skips the GetOrder resolution, so leg 1 gets unwound while
// leg 2 may be live — the exact failure this package exists to prevent, reached
// through the code meant to prevent it.
func TestOpen_AGatewayErrorIsAmbiguousWhileAClientErrorIsDefinite(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		wantAsk    bool
	}{
		{"400 from the matching engine is definite", 400, false},
		{"422 is definite", 422, false},
		{"500 leaves the order's fate unknown", 500, true},
		{"502 from a gateway leaves it unknown", 502, true},
		{"503 leaves it unknown", 503, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.perp.SetBehaviour(brokertest.Behaviour{
				RejectWith: &broker.HTTPError{StatusCode: tc.statusCode, URL: "https://demo-fapi.binance.com/fapi/v1/order", Body: "{}"},
			})

			res, err := h.opener.Open(context.Background(), h.intent)
			if err == nil {
				t.Fatal("a rejected leg must be an error")
			}
			// Whatever the classification, the invariant holds.
			h.assertInvariant(t, res)

			asked := h.rec.Has(EventLegAmbiguous)
			if asked != tc.wantAsk {
				verb := "did not ask"
				if asked {
					verb = "asked"
				}
				t.Errorf("on HTTP %d the machine %s the venue whether the order exists; want ask=%v%s",
					tc.statusCode, verb, tc.wantAsk, h.rec.Dump())
			}
		})
	}
}
