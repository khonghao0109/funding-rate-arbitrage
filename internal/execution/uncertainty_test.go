package execution

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/broker/brokertest"
	"futures-arbitrage-scanner/internal/depth"
)

// The five things the 4.4a report listed as NOT settled, each closed by a test
// that fails if the old behaviour comes back.
//
// They are in one file on purpose: an uncertainty that is closed without a test
// naming it is an uncertainty that reopens the next time somebody refactors,
// and the report that listed them is the only record that they were ever open.

// ---------------------------------------------------------------- (a) the cap

// The marketable limit comes from the BOOK'S OWN best price and a stated
// tolerance — never from the fill estimate's reach.
//
// EstimateFill reconstructs a curve from two aggregate points and CLAUDE.md
// says it is deliberately PESSIMISTIC. Pessimism is the safe direction for a
// COST estimate (you plan for a worse fill than you get) and the UNSAFE
// direction for a PRICE CAP: an estimate that thinks the fill reaches 0.4% into
// the book writes a cap 0.4% away from the touch, and the order may then
// execute at a price no one authorised. The cap must not be derivable from the
// number whose errors point that way.
func TestPlanEntry_TheCapComesFromTheBestPriceAndNotFromTheEstimatesReach(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	cfg := DefaultConfig()
	cfg.Now = func() time.Time { return time.UnixMilli(nowMs) }

	// Two books with the SAME best prices and very different depth, so
	// EstimateFill reaches far further into the thinner one.
	shallow := testIntent(nowMs)
	shallow.SpotBook.BidDepthWithinTightQuote = 30_000
	shallow.SpotBook.AskDepthWithinTightQuote = 30_000
	shallow.SpotBook.BidDepthWithinWideQuote = 120_000
	shallow.SpotBook.AskDepthWithinWideQuote = 120_000
	shallow.PerpBook = shallow.SpotBook
	shallow.PerpBook.Source = "binance_futures"

	deep := testIntent(nowMs)

	planShallow, err := planEntry(shallow, cfg)
	if err != nil {
		t.Fatalf("shallow book: %v", err)
	}
	planDeep, err := planEntry(deep, cfg)
	if err != nil {
		t.Fatalf("deep book: %v", err)
	}

	if planShallow.SpotFill.ReachedOffsetPct <= planDeep.SpotFill.ReachedOffsetPct {
		t.Fatalf("the test's own premise failed: the thinner book must reach further (%v vs %v)",
			planShallow.SpotFill.ReachedOffsetPct, planDeep.SpotFill.ReachedOffsetPct)
	}
	if planShallow.SpotOrder.PriceQuote != planDeep.SpotOrder.PriceQuote {
		t.Errorf("the buy cap moved with the estimate's reach: %v on the thin book, %v on the deep one — the cap must come from the best price alone",
			planShallow.SpotOrder.PriceQuote, planDeep.SpotOrder.PriceQuote)
	}
	if planShallow.PerpOrder.PriceQuote != planDeep.PerpOrder.PriceQuote {
		t.Errorf("the sell floor moved with the estimate's reach: %v vs %v",
			planShallow.PerpOrder.PriceQuote, planDeep.PerpOrder.PriceQuote)
	}

	// And it is exactly the stated distance from the touch, rounded the
	// passive way: a buy cap DOWN onto the tick, a sell floor UP.
	wantCap := deep.SpotBook.BestAskQuote * (1 + cfg.MaxSlippageBps/10_000)
	if got := planDeep.SpotOrder.PriceQuote; got > wantCap+1e-9 || wantCap-got > deep.SpotInstrument.TickSizeQuote {
		t.Errorf("buy cap %v, want the first tick at or below %v (best ask %v + %v bps)",
			got, wantCap, deep.SpotBook.BestAskQuote, cfg.MaxSlippageBps)
	}
	wantFloor := deep.PerpBook.BestBidQuote * (1 - cfg.MaxSlippageBps/10_000)
	if got := planDeep.PerpOrder.PriceQuote; got < wantFloor-1e-9 || got-wantFloor > deep.PerpInstrument.TickSizeQuote {
		t.Errorf("sell floor %v, want the first tick at or above %v (best bid %v - %v bps)",
			got, wantFloor, deep.PerpBook.BestBidQuote, cfg.MaxSlippageBps)
	}
}

// A tighter tolerance must produce a tighter cap, in both directions. This is
// the property that makes MaxSlippageBps a control rather than a decoration.
func TestPlanEntry_TheCapTightensWithTheParameter(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	intent := testIntent(nowMs)

	price := func(bps float64) (buyCap, sellFloor float64) {
		t.Helper()
		cfg := DefaultConfig()
		cfg.MaxSlippageBps = bps
		cfg.Now = func() time.Time { return time.UnixMilli(nowMs) }
		p, err := planEntry(intent, cfg)
		if err != nil {
			t.Fatalf("%v bps: %v", bps, err)
		}
		return p.SpotOrder.PriceQuote, p.PerpOrder.PriceQuote
	}

	wideCap, wideFloor := price(50)
	tightCap, tightFloor := price(2)
	if !(tightCap < wideCap) {
		t.Errorf("buy cap at 2 bps is %v and at 50 bps is %v — tightening the parameter did not tighten the cap", tightCap, wideCap)
	}
	if !(tightFloor > wideFloor) {
		t.Errorf("sell floor at 2 bps is %v and at 50 bps is %v — tightening the parameter did not raise the floor", tightFloor, wideFloor)
	}
}

// A book with no best price on the side being taken cannot price a cap, and
// inventing one from the mid is precisely rule 5's "plausible wrong number".
func TestPlanEntry_RefusesWhenTheBookPublishesNoBestPriceOnTheSideBeingTaken(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	cfg := DefaultConfig()
	cfg.Now = func() time.Time { return time.UnixMilli(nowMs) }

	for _, tc := range []struct {
		name string
		mut  func(*Intent)
	}{
		{"the spot book publishes no ask", func(i *Intent) { i.SpotBook.BestAskQuote = 0 }},
		{"the perp book publishes no bid", func(i *Intent) { i.PerpBook.BestBidQuote = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			intent := testIntent(nowMs)
			tc.mut(&intent)
			_, err := planEntry(intent, cfg)
			if !errors.Is(err, ErrNoBestPrice) {
				t.Fatalf("err = %v, want ErrNoBestPrice", err)
			}
			if !errors.Is(err, ErrRefusedBeforePlacing) {
				t.Errorf("err = %v, want it to also be a before-placing refusal", err)
			}
		})
	}
}

// ------------------------------------------------------- (b) reduce to match

// When leg 2 stops short, the pair is SHRUNK to what leg 2 really holds rather
// than thrown away — provided the smaller size is still a legal position on
// both venues and the book can absorb the reduction.
//
// The 4.4a report called unwinding-to-flat here the design it was least sure
// of, for a plain reason: a 60% fill is a good position, and closing it pays a
// round trip to destroy it.
func TestOpen_Leg2StopsShort_TheLargerLegIsReducedToMatchRatherThanUnwound(t *testing.T) {
	h := newHarness(t, nil)
	h.perp.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.6})

	res, err := h.opener.Open(context.Background(), h.intent)
	if err != nil {
		t.Fatalf("Open: %v%s", err, h.rec.Dump())
	}
	h.assertInvariant(t, res)
	if res.Outcome != OutcomeBothOpen {
		t.Fatalf("outcome = %q, want both_open — a 60%% fill that clears both minimums is a position worth keeping%s",
			res.Outcome, h.rec.Dump())
	}
	if !res.ReducedToMatch {
		t.Errorf("the result does not say the pair was reduced%s", h.rec.Dump())
	}
	// The kept size is the SMALLER leg's, and the larger leg was sold down to
	// it — not the other way round, which would have bought more perp and
	// traded more than the intent asked for.
	if res.Perp.FilledQtyCoin >= res.TargetQtyCoin {
		t.Fatalf("the perp leg holds %v, which is not short of the %v target — the premise of this test is gone",
			res.Perp.FilledQtyCoin, res.TargetQtyCoin)
	}
	if math.Abs(res.Spot.FilledQtyCoin-res.Perp.FilledQtyCoin) > h.intent.PerpInstrument.StepSizeCoin+1e-12 {
		t.Errorf("legs ended at %v and %v, further apart than one step", res.Spot.FilledQtyCoin, res.Perp.FilledQtyCoin)
	}
	if res.Spot.UnwoundQtyCoin <= 0 {
		t.Errorf("nothing was sold back on the spot leg, so it cannot have been reduced%s", h.rec.Dump())
	}
}

// The other branch, and the reason the first one needs a guard: when the book
// cannot absorb the reduction at an acceptable price, the pair unwinds exactly
// as it did in 4.4a rather than being cut at any price.
//
// The asymmetry with the unwind below it is on purpose and is written into
// reduceToMatch: reducing is OPTIONAL, so it may refuse on price; unwinding is
// MANDATORY, so it may not.
func TestOpen_TheBookCannotAbsorbTheReduction_UnwindsFlatInstead(t *testing.T) {
	h := newHarness(t, nil)
	// The ASK side stays deep, so the entry still prices; the BID side — which
	// is where a reduction of the spot leg would have to sell — is almost
	// empty.
	h.intent.SpotBook.BidDepthWithinTightQuote = 50
	h.intent.SpotBook.BidDepthWithinWideQuote = 200
	h.perp.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.6})

	res, err := h.opener.Open(context.Background(), h.intent)
	if err == nil {
		t.Fatal("a pair that could not be kept must report why")
	}
	h.assertInvariant(t, res)
	if res.Outcome != OutcomeBothFlat {
		t.Fatalf("outcome = %q, want both_flat%s", res.Outcome, h.rec.Dump())
	}
	if res.ReducedToMatch {
		t.Error("the pair claims to have been reduced through a book that could not absorb the cut")
	}
}

// A leg that fills BELOW the venue's own minimum notional can be neither kept
// nor closed, and this test exists to pin that the machine says so loudly
// rather than quietly holding it.
//
// It is a real limitation of the venue, not of this package. Binance USDⓈ-M
// publishes MIN_NOTIONAL as "the minimum notional value allowed for an order on
// a symbol. An order's notional value is the price * quantity", with NO
// exemption for reduceOnly or closing orders
// (https://developers.binance.com/docs/derivatives/usds-margined-futures/common-definition,
// read 2026-09-13). So a $18 perp position on a venue whose minimum is $50 is
// one no order can close. On BTCUSDT that means every perp fill under $50 is
// stuck by construction, whatever this code does — the defence is a size that
// cannot partially fill into that range, not a cleverer unwind.
//
// What the package owes here is honesty: ErrUnwindIncomplete, the quantity, and
// the venue's own refusal, so an operator knows there is something to clean up
// by hand.
func TestOpen_AFillUnderTheVenueMinimumIsStuckAndSaysSo(t *testing.T) {
	h := newHarness(t, nil)
	// ~0.0003 BTC at $60k is $18, under the perp venue's $50 minimum.
	h.perp.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.001})

	res, err := h.opener.Open(context.Background(), h.intent)
	if !errors.Is(err, ErrUnwindIncomplete) {
		t.Fatalf("err = %v, want ErrUnwindIncomplete — a position no order can close must be loud%s", err, h.rec.Dump())
	}
	if res.ReducedToMatch {
		t.Error("the pair claims to have been reduced to a size neither venue would trade")
	}
	// The message has to carry the number and the venue's own words, or the
	// operator cannot act on it.
	for _, want := range []string{"0.0003", "minNotional"} {
		if !strings.Contains(res.ReasonVI, want) {
			t.Errorf("the reason does not mention %q: %s", want, res.ReasonVI)
		}
	}
	// And the venue really is left holding it — the test asserts the truth
	// rather than the comfortable answer.
	if got := netQtyCoin(h.perp, broker.MarketFuturesUSDM); got == 0 {
		t.Error("the fake venue holds nothing, so this test is no longer about a stuck position")
	}
}

// ------------------------------------------------------------ (c) leg order

// The default does not move in this session: spot first, because a naked spot
// long cannot be liquidated and a naked perp short can.
func TestDefaultConfig_PlacesSpotFirstAndSaysSoByName(t *testing.T) {
	if got := DefaultConfig().LegOrder; got != LegOrderSequentialSpotFirst {
		t.Errorf("default LegOrder = %q, want %q", got, LegOrderSequentialSpotFirst)
	}
}

// Parallel is a parameter, and it reaches the same invariant. What it buys is
// a shorter unhedged window; what it costs is that BOTH legs can now be live
// when one of them fails, which is why it is not the default and why 4.4b
// measures both before anyone moves it.
func TestOpen_ParallelPlacement_ReachesTheSameInvariant(t *testing.T) {
	for _, tc := range []struct {
		name        string
		perp        brokertest.Behaviour
		wantOutcome Outcome
	}{
		{"both legs fill", brokertest.Behaviour{FillFractionOnPlace: 1}, OutcomeBothOpen},
		{"the perp leg is refused outright", brokertest.Behaviour{
			RejectWith: fmt.Errorf("%w: notional 12 < 50", broker.ErrBelowMinNotional)}, OutcomeBothFlat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(c *Config) { c.LegOrder = LegOrderParallel })
			h.perp.SetBehaviour(tc.perp)

			res, _ := h.opener.Open(context.Background(), h.intent)
			h.assertInvariant(t, res)
			if res.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %q, want %q%s", res.Outcome, tc.wantOutcome, h.rec.Dump())
			}
			if n := len(h.spot.Orders()); n == 0 {
				t.Errorf("the spot venue holds no order at all%s", h.rec.Dump())
			}
		})
	}
}

// The unhedged window is REPORTED, whichever order the legs were sent in. It is
// the number 4.4b measures on a real venue, and a figure nobody records is a
// figure nobody can compare.
func TestOpen_ReportsTheUnhedgedWindow(t *testing.T) {
	for _, order := range []LegOrder{LegOrderSequentialSpotFirst, LegOrderParallel} {
		t.Run(string(order), func(t *testing.T) {
			h := newHarness(t, func(c *Config) { c.LegOrder = order })
			res, err := h.opener.Open(context.Background(), h.intent)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if res.Spot.FilledAtMs == 0 || res.Perp.FilledAtMs == 0 {
				t.Fatalf("a leg has no fill instant: spot %d, perp %d", res.Spot.FilledAtMs, res.Perp.FilledAtMs)
			}
			if res.UnhedgedWindow < 0 {
				t.Errorf("unhedged window %v is negative", res.UnhedgedWindow)
			}
		})
	}
}

// --------------------------------------------------- (d) the spot flat proof

// balanceScript is a broker whose balance answers are written by the test, so
// the two pieces of evidence can be made to disagree on purpose. Everything
// else is the fake.
type balanceScript struct {
	*brokertest.Fake
	answers [][]broker.Balance
	calls   int
	place   func(req broker.PlaceOrderRequest, through func() (broker.Order, error)) (broker.Order, error)
}

func (b *balanceScript) GetBalance(ctx context.Context, m broker.Market) ([]broker.Balance, error) {
	if m != broker.MarketSpot || len(b.answers) == 0 {
		return b.Fake.GetBalance(ctx, m)
	}
	i := b.calls
	if i >= len(b.answers) {
		i = len(b.answers) - 1
	}
	b.calls++
	return b.answers[i], nil
}

func (b *balanceScript) PlaceOrder(ctx context.Context, req broker.PlaceOrderRequest) (broker.Order, error) {
	through := func() (broker.Order, error) { return b.Fake.PlaceOrder(ctx, req) }
	if b.place != nil {
		return b.place(req, through)
	}
	return through()
}

func baseBalance(qtyCoin float64) []broker.Balance {
	return []broker.Balance{{Market: broker.MarketSpot, Asset: "BTC", FreeQtyCoin: qtyCoin}}
}

// openWithSpot runs one Open against a scripted spot broker.
func openWithSpot(t *testing.T, spot broker.Broker, fake *brokertest.Fake, tune func(*brokertest.Fake)) (Result, error, *MemoryRecorder) {
	t.Helper()
	nowMs := time.Now().UnixMilli()
	perp := brokertest.New()
	perp.SetStepSizeCoin(perpRules().StepSizeCoin)
	perp.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1})
	fake.SetStepSizeCoin(spotRules().StepSizeCoin)
	if tune != nil {
		tune(fake)
	}
	cfg := DefaultConfig()
	cfg.LegTimeout = 300 * time.Millisecond
	cfg.UnwindTimeout = 2 * time.Second
	cfg.PollEvery = 10 * time.Millisecond
	rec := NewMemoryRecorder()
	o, err := NewOpener(spot, perp, cfg, rec)
	if err != nil {
		t.Fatal(err)
	}
	res, err := o.Open(context.Background(), testIntent(nowMs))
	return res, err, rec
}

// The agreeing case: the closing order filled what the opening order did, AND
// the venue's own base balance came back to where it started.
func TestOpen_SpotFlatEvidence_BothPiecesAgree(t *testing.T) {
	fake := brokertest.New()
	spot := &balanceScript{Fake: fake, answers: [][]broker.Balance{baseBalance(2), baseBalance(2)}}
	// Leg 1 fills only halfway, so the run unwinds and the proof is exercised.
	res, err, rec := openWithSpot(t, spot, fake, func(f *brokertest.Fake) {
		f.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.5})
	})
	if err == nil {
		t.Fatal("a leg that fell short must report an error")
	}
	if errors.Is(err, ErrFlatEvidenceConflict) {
		t.Fatalf("the two pieces of evidence agree and were reported as conflicting: %v%s", err, rec.Dump())
	}
	if res.Outcome != OutcomeBothFlat {
		t.Fatalf("outcome = %q, want both_flat%s", res.Outcome, rec.Dump())
	}
	if res.SpotFlatEvidenceVI == "" {
		t.Error("the flat proof was not reported at all")
	}
	if res.SpotBaseBalanceBeforeQtyCoin != 2 || res.SpotBaseBalanceAfterQtyCoin != 2 {
		t.Errorf("balances reported as %v -> %v, want the venue's 2 -> 2",
			res.SpotBaseBalanceBeforeQtyCoin, res.SpotBaseBalanceAfterQtyCoin)
	}
}

// Conflict one: our order arithmetic balances, and the VENUE says the holding
// moved anyway. Something filled that we did not see, or something else is
// trading this account. It is named and printed, never reconciled away.
func TestOpen_SpotFlatEvidence_TheBalanceMovedWhileTheOrdersBalanced(t *testing.T) {
	fake := brokertest.New()
	spot := &balanceScript{Fake: fake, answers: [][]broker.Balance{baseBalance(2), baseBalance(2.5)}}
	res, err, rec := openWithSpot(t, spot, fake, func(f *brokertest.Fake) {
		f.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.5})
	})
	if !errors.Is(err, ErrFlatEvidenceConflict) {
		t.Fatalf("err = %v, want ErrFlatEvidenceConflict%s", err, rec.Dump())
	}
	if res.SpotFlatEvidenceVI == "" {
		t.Error("a conflict was reported with no explanation of which two things disagreed")
	}
	if res.SpotBaseBalanceAfterQtyCoin != 2.5 {
		t.Errorf("the venue's own after-balance %v was not carried into the result", res.SpotBaseBalanceAfterQtyCoin)
	}
}

// Conflict two, the other way round: the venue's holding is where it started,
// and our own arithmetic says we did not close what we opened. Both readings
// cannot be true, and choosing one silently is how a wrong belief survives.
func TestOpen_SpotFlatEvidence_TheOrdersDisagreeWhileTheBalanceIsUnchanged(t *testing.T) {
	fake := brokertest.New()
	spot := &balanceScript{
		Fake:    fake,
		answers: [][]broker.Balance{baseBalance(2), baseBalance(2)},
		place: func(req broker.PlaceOrderRequest, through func() (broker.Order, error)) (broker.Order, error) {
			o, err := through()
			if err != nil || req.Type != broker.OrderTypeMarket {
				return o, err
			}
			// The closing order reports back less than it really did. The
			// balance, read from the venue, does not agree — and that
			// disagreement is the whole signal.
			o.FilledQtyCoin *= 0.5
			return o, nil
		},
	}
	res, err, rec := openWithSpot(t, spot, fake, func(f *brokertest.Fake) {
		f.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.5})
	})
	if !errors.Is(err, ErrFlatEvidenceConflict) {
		t.Fatalf("err = %v, want ErrFlatEvidenceConflict%s", err, rec.Dump())
	}
	_ = res
}

// ------------------------------------------------------------- (e) timeouts

// The leg timeout stays at ten seconds and stays a parameter. It is pinned here
// because it is a GUESS — nothing has measured how long a testnet leg really
// takes — and a guess that nobody can see is a guess that becomes a fact.
func TestDefaultConfig_LegTimeoutIsTheUnmeasuredTenSeconds(t *testing.T) {
	d := DefaultConfig()
	if d.LegTimeout != DefaultLegTimeout {
		t.Errorf("LegTimeout = %v, want DefaultLegTimeout", d.LegTimeout)
	}
	if DefaultLegTimeout != 10*time.Second {
		t.Errorf("DefaultLegTimeout = %v, want 10s until 4.4b measures a better one", DefaultLegTimeout)
	}
	if d.MaxSlippageBps <= 0 {
		t.Errorf("MaxSlippageBps = %v, want a positive default distance from the touch", d.MaxSlippageBps)
	}
}

// A negative tolerance would invert the cap — a buy priced BELOW the touch,
// which never fills and looks like a venue problem.
func TestNewOpener_RefusesANegativeSlippageTolerance(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSlippageBps = -1
	if _, err := NewOpener(brokertest.New(), brokertest.New(), cfg, nil); err == nil {
		t.Fatal("NewOpener accepted a negative MaxSlippageBps")
	}
}

// A guard so the fixtures above keep meaning what they say.
var _ = depth.Summary{}
