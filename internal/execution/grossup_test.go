package execution

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
)

// PLAN 4.5j, second half: a venue that keeps a spot BUY's fee in the base coin
// (Bybit, 10 bps measured on testnet 2026-09-17) is traded by buying the spot
// leg GROSSED UP — Q ÷ (1 − fee), rounded up onto the spot grid — and by
// judging the hedge on what the WALLET received, read from the venue.
//
// Every assertion here reads the fake VENUES: the spot wallet's balance and
// the perp position. A machine that has confused its orders with its wallet
// reports both_open with complete confidence, which is exactly the defect the
// second review of 4.5j found.

// walletQtyCoin is the spot venue's base balance, free plus locked.
func walletQtyCoin(t *testing.T, f *brokertest.Fake, asset string) float64 {
	t.Helper()
	balances, err := f.GetBalance(context.Background(), broker.MarketSpot)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range balances {
		if b.Asset == asset {
			return b.TotalQtyCoin()
		}
	}
	return 0
}

func perpPositionQtyCoin(t *testing.T, f *brokertest.Fake, symbol string) float64 {
	t.Helper()
	pos, err := f.GetPosition(context.Background(), broker.MarketFuturesUSDM, symbol)
	if err != nil {
		t.Fatal(err)
	}
	return pos.QtyCoin
}

// assertWalletHedged reads the two venues and fails unless the wallet and the
// perp position are one hedged pair — or both flat, where "flat" on spot allows
// the less-than-one-step sliver no correctly-rounded sell can reach.
func assertWalletHedged(t *testing.T, h *harness, res Result, wantOutcome Outcome) {
	t.Helper()
	asset := h.intent.SpotInstrument.BaseAsset
	wallet := walletQtyCoin(t, h.spot, asset)
	perp := perpPositionQtyCoin(t, h.perp, h.intent.Symbol)
	tol := math.Max(h.intent.SpotInstrument.StepSizeCoin, h.intent.PerpInstrument.StepSizeCoin)
	bothOpen := wallet > 0 && perp < 0 && math.Abs(wallet+perp) <= tol+1e-9
	bothFlat := perp == 0 && wallet < h.intent.SpotInstrument.StepSizeCoin-1e-12
	switch {
	case !bothOpen && !bothFlat:
		t.Fatalf("INVARIANT VIOLATED at the venues: wallet %.10g %s, perp %.10g, residual %.10g > %.10g\n  result %q (%s)%s",
			wallet, asset, perp, math.Abs(wallet+perp), tol, res.Outcome, res.ReasonVI, h.rec.Dump())
	case bothOpen && res.Outcome != OutcomeBothOpen, bothFlat && res.Outcome != OutcomeBothFlat:
		t.Fatalf("the venues say open=%v flat=%v but the result says %q%s", bothOpen, bothFlat, res.Outcome, h.rec.Dump())
	case res.Outcome != wantOutcome:
		t.Fatalf("outcome %q, want %q (%s)%s", res.Outcome, wantOutcome, res.ReasonVI, h.rec.Dump())
	}
}

// The size the first half of 4.5j REFUSED — 0.3333 BTC, whose 10 bps fee gap
// (0.000333) exceeds the 0.0001 perp step — now opens, in both placement
// orders, with the spot order grossed up and the wallet holding the perp.
func TestOpen_GrossesUpTheSpotBuySoTheWalletHoldsThePerp(t *testing.T) {
	for _, order := range []LegOrder{LegOrderSequentialSpotFirst, LegOrderParallel} {
		t.Run(string(order), func(t *testing.T) {
			h := newHarness(t, func(c *Config) { c.LegOrder = order })
			const fee = 0.001
			h.intent.SpotBuyFeeInBaseFrac = fee
			h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1, SpotBuyFeeInBaseFrac: fee})

			res, err := h.opener.Open(context.Background(), h.intent)
			if err != nil {
				t.Fatalf("Open: %v%s", err, h.rec.Dump())
			}
			assertWalletHedged(t, h, res, OutcomeBothOpen)
			if res.ReducedToMatch {
				t.Errorf("a grossed-up open needed no cut, yet it reduced%s", h.rec.Dump())
			}
			wantSpot := broker.CeilToStep(0.3333/(1-fee), h.intent.SpotInstrument.StepSizeCoin)
			if res.Spot.FilledQtyCoin != wantSpot || res.Perp.FilledQtyCoin != 0.3333 {
				t.Errorf("spot order %v (want %v), perp %v (want 0.3333)", res.Spot.FilledQtyCoin, wantSpot, res.Perp.FilledQtyCoin)
			}
			wallet := walletQtyCoin(t, h.spot, "BTC")
			if wallet < 0.3333-1e-12 || wallet-0.3333 >= h.intent.SpotInstrument.StepSizeCoin {
				t.Errorf("wallet holds %v beside a 0.3333 perp; want at least the perp and less than one spot step more", wallet)
			}
			if math.Abs(res.SpotHeldQtyCoin-wallet) > 1e-9 || !strings.Contains(res.SpotHeldSourceVI, "SỐ DƯ SÀN") {
				t.Errorf("SpotHeldQtyCoin %v vs wallet %v, source %q", res.SpotHeldQtyCoin, wallet, res.SpotHeldSourceVI)
			}
			if res.TargetQtyCoin != 0.3333 {
				t.Errorf("TargetQtyCoin %v: the target is the hedge, not the grossed-up order", res.TargetQtyCoin)
			}
		})
	}
}

// DOGE is the pair the refusal would have halted the bot on: a whole-coin perp
// grid makes the tolerance 1 DOGE, so any slot over 1,000 DOGE had a fee gap
// past it. Bybit DOGEUSDT's grids, spot basePrecision 0.1 / linear qtyStep 1,
// with a 5-quote minimum on both.
func TestOpen_ACoarseStepPairOpensInsteadOfBeingRefused(t *testing.T) {
	h := newHarness(t, nil)
	const fee, mid = 0.001, 0.15
	spot := exchanges.Instrument{Symbol: "DOGEUSDT", Source: "bybit_spot", MarketType: "spot",
		Status: exchanges.StatusTrading, TickSizeQuote: 0.00001, StepSizeCoin: 0.1, MinQtyCoin: 0.1,
		MaxQtyCoin: 5_000_000, MinNotionalQuote: 5, ContractSizeCoin: 1, BaseAsset: "DOGE", QuoteAsset: "USDT"}
	perp := exchanges.Instrument{Symbol: "DOGEUSDT", Source: "bybit_linear", MarketType: "perp",
		Status: exchanges.StatusTrading, TickSizeQuote: 0.00001, StepSizeCoin: 1, MinQtyCoin: 1,
		MaxQtyCoin: 5_000_000, MinNotionalQuote: 5, ContractSizeCoin: 1, BaseAsset: "DOGE", QuoteAsset: "USDT"}
	nowMs := time.Now().UnixMilli()
	h.intent.Symbol, h.intent.SpotInstrument, h.intent.PerpInstrument = "DOGEUSDT", spot, perp
	h.intent.SpotBook, h.intent.PerpBook = deepBook("bybit_spot", mid, nowMs), deepBook("bybit_linear", mid, nowMs)
	h.intent.SpotPriceQuote, h.intent.PerpPriceQuote = mid, mid
	h.intent.NotionalQuote = 3_000 // 20,000 DOGE: the old gap 20 DOGE against a 1 DOGE tolerance
	h.intent.SpotBuyFeeInBaseFrac = fee
	h.spot.SetBaseAsset("DOGE")
	h.perp.SetBaseAsset("DOGE")
	h.spot.SetStepSizeCoin(spot.StepSizeCoin)
	h.perp.SetStepSizeCoin(perp.StepSizeCoin)
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1, SpotBuyFeeInBaseFrac: fee})

	res, err := h.opener.Open(context.Background(), h.intent)
	if err != nil {
		t.Fatalf("Open refused or failed a slot the gross-up exists to allow: %v%s", err, h.rec.Dump())
	}
	assertWalletHedged(t, h, res, OutcomeBothOpen)
	if res.Perp.FilledQtyCoin != 20_000 || math.Abs(res.Spot.FilledQtyCoin-20_020.1) > 1e-9 {
		t.Errorf("perp %v (want 20000), spot order %v (want 20020.1 = 20000 ÷ 0.999 rounded UP to 0.1)",
			res.Perp.FilledQtyCoin, res.Spot.FilledQtyCoin)
	}
}

// Fee surcharge: the account publishes 10 bps and the venue keeps 30. The
// orders alone look perfect; only the wallet shows the coin missing, past the
// 0.0001 tolerance. The pair must never be reported hedged on the orders' word:
// it is cut down to the WALLET — the perp bought back under the REDUCE id —
// when the cut is placeable, and unwound flat when it is not.
func TestOpen_AFeeChargedAboveThePublishedRateIsCaughtByTheWallet(t *testing.T) {
	t.Run("cut to the wallet", func(t *testing.T) {
		h := newHarness(t, nil)
		// 0.4777 BTC: the wallet gets 0.47674546, so the cut is 0.0009 perp
		// (54 quote, over the 50 minimum) and the residual after it is worth
		// 3.27 quote, under both minimums.
		h.intent.NotionalQuote = 28_662
		h.intent.SpotBuyFeeInBaseFrac = 0.001
		h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1, SpotBuyFeeInBaseFrac: 0.003})

		res, err := h.opener.Open(context.Background(), h.intent)
		if err != nil {
			t.Fatalf("Open: %v%s", err, h.rec.Dump())
		}
		assertWalletHedged(t, h, res, OutcomeBothOpen)
		if !res.ReducedToMatch {
			t.Errorf("a wallet short by nine times the tolerance was not cut to match%s", h.rec.Dump())
		}
		if pos := perpPositionQtyCoin(t, h.perp, "BTCUSDT"); math.Abs(pos+0.4768) > 1e-9 {
			t.Errorf("perp position %v, want -0.4768", pos)
		}
		cuts := 0
		for _, o := range h.perp.Orders() {
			if o.Side == broker.SideBuy {
				cuts++
				if o.ClientOrderID != ReduceClientOrderID(h.intent.ID, LegPerp) {
					t.Errorf("the cut went out as %q, want the REDUCE id %q, not the unwind's",
						o.ClientOrderID, ReduceClientOrderID(h.intent.ID, LegPerp))
				}
			}
		}
		if cuts != 1 {
			t.Errorf("%d perp buys, want exactly one cut", cuts)
		}
	})
	t.Run("cut not placeable, unwound", func(t *testing.T) {
		h := newHarness(t, nil)
		// 0.3333 BTC: the 0.0006 cut is 36 quote, under the perp's 50 minimum.
		h.intent.SpotBuyFeeInBaseFrac = 0.001
		h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1, SpotBuyFeeInBaseFrac: 0.003, RefuseSpotSellBeyondBalance: true})

		res, err := h.opener.Open(context.Background(), h.intent)
		if err == nil {
			t.Fatalf("Open reported success on a wallet short of the perp%s", h.rec.Dump())
		}
		assertWalletHedged(t, h, res, OutcomeBothFlat)
		if !strings.Contains(res.ReasonVI, "ví spot chỉ nhận") {
			t.Errorf("the reason does not name the wallet: %q", res.ReasonVI)
		}
	})
}

// A parallel open where spot fills half and perp fills whole — review M1's
// first shape. The perp is cut to what the wallet holds.
func TestOpen_ParallelPartialSpotFillWithABaseCoinFeeShrinksThePerpToTheWallet(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.LegOrder = LegOrderParallel })
	h.intent.SpotBuyFeeInBaseFrac = 0.001
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.5, SpotBuyFeeInBaseFrac: 0.001})

	res, err := h.opener.Open(context.Background(), h.intent)
	if err != nil {
		t.Fatalf("Open: %v%s", err, h.rec.Dump())
	}
	assertWalletHedged(t, h, res, OutcomeBothOpen)
	if !res.ReducedToMatch || res.Perp.UnwoundQtyCoin <= 0 {
		t.Errorf("reduced %v, perp cut %v%s", res.ReducedToMatch, res.Perp.UnwoundQtyCoin, h.rec.Dump())
	}
}

// With no wallet to compare against, the published fee is the estimate, and
// the result says only one piece of evidence exists.
func TestOpen_AnUnreadableWalletFallsBackToThePublishedFeeAndSaysSo(t *testing.T) {
	h := newHarness(t, nil)
	h.intent.SpotBuyFeeInBaseFrac = 0.001
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1, SpotBuyFeeInBaseFrac: 0.001})
	h.spot.SetBalanceError(fmt.Errorf("wallet unreadable"))

	res, err := h.opener.Open(context.Background(), h.intent)
	if err != nil || res.Outcome != OutcomeBothOpen {
		t.Fatalf("Open: %v %q%s", err, res.Outcome, h.rec.Dump())
	}
	if want := res.Spot.FilledQtyCoin * 0.999; res.SpotHeldQtyCoin != want {
		t.Errorf("SpotHeldQtyCoin %v, want the estimate %v", res.SpotHeldQtyCoin, want)
	}
	if !strings.Contains(res.SpotHeldSourceVI, "chỉ có một bằng chứng") {
		t.Errorf("the single-evidence case is not stated: %q", res.SpotHeldSourceVI)
	}
}

// laggingWallet answers the spot balance a few reads late, the way a venue's
// wallet can trail the fill that moved it.
type laggingWallet struct {
	*brokertest.Fake
	mu        sync.Mutex
	reads     int
	lagReads  int
	firstSeen []broker.Balance
}

func (w *laggingWallet) GetBalance(ctx context.Context, m broker.Market) ([]broker.Balance, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reads++
	if w.reads == 1 {
		b, err := w.Fake.GetBalance(ctx, m)
		w.firstSeen = b
		return b, err
	}
	if w.reads <= 1+w.lagReads {
		return w.firstSeen, nil
	}
	return w.Fake.GetBalance(ctx, m)
}

// A balance that trails the fill is read again, not taken as a wallet that
// received nothing — or every open on a slow wallet would unwind a good pair.
func TestOpen_AWalletThatTrailsTheFillIsReadAgainBeforeJudging(t *testing.T) {
	h := newHarness(t, nil)
	h.intent.SpotBuyFeeInBaseFrac = 0.001
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1, SpotBuyFeeInBaseFrac: 0.001})
	lag := &laggingWallet{Fake: h.spot, lagReads: 3}
	o, err := NewOpener(lag, h.perp, h.cfg, h.rec)
	if err != nil {
		t.Fatal(err)
	}

	res, err := o.Open(context.Background(), h.intent)
	if err != nil {
		t.Fatalf("Open: %v%s", err, h.rec.Dump())
	}
	assertWalletHedged(t, h, res, OutcomeBothOpen)
	if res.ReducedToMatch {
		t.Errorf("a late balance was taken for a short wallet and the pair was cut%s", h.rec.Dump())
	}
	if lag.reads < 5 {
		t.Errorf("the wallet was read %d times; the three stale answers were not waited out", lag.reads)
	}
}

// The property test, on the WALLET: random fills, timeouts, refusals and races
// on both legs, a fee at the published rate or 40% above it, both placement
// orders. Every run must end hedged on the venues or flat on them.
func TestOpen_InvariantHoldsOnTheWalletUnderABaseCoinFee(t *testing.T) {
	const runs = 132
	knobs := []brokertest.Behaviour{
		{FillFractionOnPlace: 1},
		{FillFractionOnPlace: 0},
		{FillFractionOnPlace: 0.5},
		{FillFractionOnPlace: 0.9},
		{FillFractionOnPlace: 1, PlaceTimesOutAfterAccepting: true, TimeoutTimes: 1},
		{FillFractionOnPlace: 1, PlaceTimesOutBeforeAccepting: true, TimeoutTimes: 1},
		{FillFractionOnPlace: 0.5, CancelRacesAFill: true},
		{RejectWith: fmt.Errorf("%w: rejected", broker.ErrBelowMinNotional)},
		{FillFractionOnPlace: 1, PlaceTimesOutAfterAccepting: true},
		{FillFractionOnPlace: 1, RefuseSpotSellBeyondBalance: true},
		{FillFractionOnPlace: 0.7, RefuseSpotSellBeyondBalance: true},
	}
	orders := []LegOrder{LegOrderSequentialSpotFirst, LegOrderParallel}
	fees := []float64{0.001, 0.0014}
	for i := 0; i < runs; i++ {
		spotKnob := knobs[i%len(knobs)]
		perpKnob := knobs[(i/len(knobs)+i*5)%len(knobs)]
		perpKnob.RefuseSpotSellBeyondBalance = false
		order, venueFee := orders[i%2], fees[(i/2)%2]
		spotKnob.SpotBuyFeeInBaseFrac = venueFee

		h := newHarness(t, func(c *Config) {
			c.LegOrder = order
			c.LegTimeout = 25 * time.Millisecond
			c.PollEvery = 3 * time.Millisecond
			c.UnwindTimeout = time.Second
		})
		h.intent.SpotBuyFeeInBaseFrac = 0.001
		h.spot.SetBehaviour(spotKnob)
		h.perp.SetBehaviour(perpKnob)

		res, err := h.opener.Open(context.Background(), h.intent)
		wallet := walletQtyCoin(t, h.spot, "BTC")
		perp := perpPositionQtyCoin(t, h.perp, "BTCUSDT")
		tol := math.Max(h.intent.SpotInstrument.StepSizeCoin, h.intent.PerpInstrument.StepSizeCoin)
		bothOpen := wallet > 0 && perp < 0 && math.Abs(wallet+perp) <= tol+1e-9
		bothFlat := perp == 0 && wallet < h.intent.SpotInstrument.StepSizeCoin-1e-12
		if !bothOpen && !bothFlat {
			t.Fatalf("run %d (%s, venue fee %v) VIOLATED the invariant: wallet %.10g, perp %.10g\n  spot %+v\n  perp %+v\n  result %q err %v%s",
				i, order, venueFee, wallet, perp, spotKnob, perpKnob, res.Outcome, err, h.rec.Dump())
		}
		if bothOpen && res.Outcome != OutcomeBothOpen {
			t.Fatalf("run %d: venues hedged, result %q (%v)%s", i, res.Outcome, err, h.rec.Dump())
		}
		if bothOpen && err != nil {
			t.Fatalf("run %d: venues hedged but Open returned %v%s", i, err, h.rec.Dump())
		}
	}
}

// A spot fill that reaches the HEDGE size but not the grossed-up ORDER is a
// short leg 1: the wallet holds Q × (1 − fee), less than the perp would be. The
// sequential path must stop there — no perp sent — and unwind, exactly as for
// any other leg 1 that fell short.
func TestOpen_ASpotFillShortOfTheGrossedUpOrderIsAShortLegOne(t *testing.T) {
	h := newHarness(t, nil)
	h.intent.SpotBuyFeeInBaseFrac = 0.001
	// 0.33364 × 0.999 floors to exactly 0.3333 on the spot grid: the hedge
	// size, one fee short of the order.
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.999, SpotBuyFeeInBaseFrac: 0.001})

	res, err := h.opener.Open(context.Background(), h.intent)
	if err == nil {
		t.Fatalf("Open succeeded on a spot leg one fee short%s", h.rec.Dump())
	}
	assertWalletHedged(t, h, res, OutcomeBothFlat)
	if n := len(h.perp.Orders()); n != 0 {
		t.Errorf("%d perp orders were sent after a spot leg that did not reach its grossed-up target%s", n, h.rec.Dump())
	}
}

// creditingWallet reports coin arriving from somewhere else — a transfer, a
// deposit — on every balance read after the first.
type creditingWallet struct {
	*brokertest.Fake
	mu           sync.Mutex
	reads        int
	extraQtyCoin float64
}

func (w *creditingWallet) GetBalance(ctx context.Context, m broker.Market) ([]broker.Balance, error) {
	w.mu.Lock()
	w.reads++
	first := w.reads == 1
	w.mu.Unlock()
	balances, err := w.Fake.GetBalance(ctx, m)
	if err != nil || first {
		return balances, err
	}
	out := append([]broker.Balance(nil), balances...)
	for i := range out {
		if out[i].Asset == "BTC" {
			out[i].FreeQtyCoin += w.extraQtyCoin
		}
	}
	return out, nil
}

// Coin the order did not buy does not hedge the perp it sold, and a wallet
// that gained more than the fills explain is a contradiction, not a leg: Open
// stops with ErrSpotEvidenceConflict and sends nothing more — no cut sells the
// extra coin, no unwind — while both legs stay as the venues hold them.
func TestOpen_CoinCreditedFromElsewhereIsAConflictNotTheSpotLeg(t *testing.T) {
	h := newHarness(t, nil)
	h.intent.SpotBuyFeeInBaseFrac = 0.001
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1, SpotBuyFeeInBaseFrac: 0.001})
	wallet := &creditingWallet{Fake: h.spot, extraQtyCoin: 0.05}
	o, err := NewOpener(wallet, h.perp, h.cfg, h.rec)
	if err != nil {
		t.Fatal(err)
	}

	res, err := o.Open(context.Background(), h.intent)
	if !errors.Is(err, ErrSpotEvidenceConflict) || !errors.Is(err, ErrFlatEvidenceConflict) {
		t.Fatalf("err %v, want ErrSpotEvidenceConflict (an alarm)%s", err, h.rec.Dump())
	}
	if res.Outcome != OutcomeBothOpen || res.ReducedToMatch {
		t.Errorf("outcome %q reduced %v — both legs are on the venue and nothing was cut", res.Outcome, res.ReducedToMatch)
	}
	if n := len(h.spot.Orders()) + len(h.perp.Orders()); n != 2 {
		t.Errorf("%d orders — only the two opening legs may exist%s", n, h.rec.Dump())
	}
	if !strings.Contains(res.SpotHeldSourceVI, "LỆCH") {
		t.Errorf("both numbers are not stated: %q", res.SpotHeldSourceVI)
	}
}

// Review part 2, M2: a wallet still behind when the settle deadline passes is
// not a wallet to cut the perp to. Nothing more is sent.
func TestOpen_AWalletStillBehindAtTheDeadlineIsAConflictNotACut(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.OrderSettleTimeout = 60 * time.Millisecond })
	h.intent.SpotBuyFeeInBaseFrac = 0.001
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1, SpotBuyFeeInBaseFrac: 0.001})
	lag := &laggingWallet{Fake: h.spot, lagReads: 1_000_000}
	o, err := NewOpener(lag, h.perp, h.cfg, h.rec)
	if err != nil {
		t.Fatal(err)
	}

	res, err := o.Open(context.Background(), h.intent)
	if !errors.Is(err, ErrSpotEvidenceConflict) {
		t.Fatalf("err %v, want ErrSpotEvidenceConflict%s", err, h.rec.Dump())
	}
	if n := len(h.spot.Orders()) + len(h.perp.Orders()); n != 2 || res.ReducedToMatch {
		t.Errorf("%d orders, reduced %v — a lagging balance cut or unwound the pair%s", n, res.ReducedToMatch, h.rec.Dump())
	}
	if pos := perpPositionQtyCoin(t, h.perp, "BTCUSDT"); pos != -0.3333 {
		t.Errorf("perp %v, want the untouched -0.3333", pos)
	}
}

// Review part 2, M1: the intent carries the published TAKER rate, but a part
// that rested fills as maker and a maker fee can be zero. The fills say so, the
// wallet agrees, and the pair is judged on that — the extra coin is cut back
// under the REDUCE id rather than reported hedged beside a shorter perp.
func TestOpen_AFeeLowerThanPublishedIsReadFromTheFills(t *testing.T) {
	h := newHarness(t, nil)
	h.intent.SpotBuyFeeInBaseFrac = 0.001
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1}) // maker: no fee kept at all

	res, err := h.opener.Open(context.Background(), h.intent)
	if err != nil {
		t.Fatalf("Open: %v%s", err, h.rec.Dump())
	}
	assertWalletHedged(t, h, res, OutcomeBothOpen)
	if !res.SpotBuyBaseFeeStated || res.SpotBuyBaseFeeQtyCoin != 0 {
		t.Errorf("fee stated %v = %v, want the fills' 0", res.SpotBuyBaseFeeStated, res.SpotBuyBaseFeeQtyCoin)
	}
	if !res.ReducedToMatch {
		t.Errorf("0.00034 BTC of extra spot was not cut back%s", h.rec.Dump())
	}
	for _, ord := range h.spot.Orders() {
		if ord.Side == broker.SideSell && ord.ClientOrderID != ReduceClientOrderID(h.intent.ID, LegSpot) {
			t.Errorf("spot sell under %q, want the reduce id", ord.ClientOrderID)
		}
	}
}

// noFills hides the fill list: a spot venue that cannot state its commission.
type noFills struct{ broker.Broker }

// Without fills the published rate is the only credit, and a venue that kept
// three times as much is a contradiction the wallet shows — an alarm, not a
// guess in either direction.
func TestOpen_WithoutFillsASurchargeIsAConflict(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.OrderSettleTimeout = 60 * time.Millisecond })
	h.intent.SpotBuyFeeInBaseFrac = 0.001
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1, SpotBuyFeeInBaseFrac: 0.003})
	o, err := NewOpener(noFills{h.spot}, h.perp, h.cfg, h.rec)
	if err != nil {
		t.Fatal(err)
	}
	res, err := o.Open(context.Background(), h.intent)
	if !errors.Is(err, ErrSpotEvidenceConflict) || res.SpotBuyBaseFeeStated {
		t.Fatalf("err %v stated %v%s", err, res.SpotBuyBaseFeeStated, h.rec.Dump())
	}
	if !strings.Contains(res.SpotHeldSourceVI, "phí công bố") {
		t.Errorf("the fallback is not named: %q", res.SpotHeldSourceVI)
	}
}

// halfFirstSpotSell fills only half of the first spot MARKET sell, the way a
// thin book answers a cut.
type halfFirstSpotSell struct {
	*brokertest.Fake
	mu   sync.Mutex
	done bool
}

func (b *halfFirstSpotSell) PlaceOrder(ctx context.Context, req broker.PlaceOrderRequest) (broker.Order, error) {
	b.mu.Lock()
	if !b.done && req.Side == broker.SideSell && req.Type == broker.OrderTypeMarket {
		b.done = true
		req.QtyCoin = broker.CeilToStep(req.QtyCoin/2, 0.00001)
	}
	b.mu.Unlock()
	return b.Fake.PlaceOrder(ctx, req)
}

// A cut that only half fills leaves a pair that is still not hedged, so it
// unwinds — and the unwind's fee is a share of the ORIGINAL buy: the coin the
// cut already sold left the wallet whole. With the wallet unreadable there is
// no balance to cap the sell, so the arithmetic alone must be right, or the
// venue refuses a sell the wallet cannot fund and the long stays naked.
func TestOpen_TheUnwindAfterAPartialCutChargesTheFeeOnTheOriginalBuy(t *testing.T) {
	h := newHarness(t, nil)
	h.intent.SpotBuyFeeInBaseFrac = 0.001
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1, SpotBuyFeeInBaseFrac: 0.001, RefuseSpotSellBeyondBalance: true})
	h.perp.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.6})
	h.spot.SetBalanceError(fmt.Errorf("wallet unreadable"))
	spot := &halfFirstSpotSell{Fake: h.spot}
	o, err := NewOpener(spot, h.perp, h.cfg, h.rec)
	if err != nil {
		t.Fatal(err)
	}

	res, err := o.Open(context.Background(), h.intent)
	h.spot.SetBalanceError(nil)
	if err == nil {
		t.Fatalf("a half-filled cut was reported as a hedge%s", h.rec.Dump())
	}
	if res.Spot.UnwoundQtyCoin <= 0 {
		t.Fatalf("the premise is gone: no spot cut and unwind happened%s", h.rec.Dump())
	}
	assertWalletHedged(t, h, res, OutcomeBothFlat)
}

// Inside the pair's tolerance the two readings are not a conflict, and the leg
// is the SMALLER one: a wallet a few satoshi above what the fills explain does
// not make the spot leg longer than the order bought.
func TestOpen_ASmallExtraInTheWalletKeepsTheFillsFigure(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.OrderSettleTimeout = 60 * time.Millisecond })
	h.intent.SpotBuyFeeInBaseFrac = 0.001
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1, SpotBuyFeeInBaseFrac: 0.001})
	wallet := &creditingWallet{Fake: h.spot, extraQtyCoin: 0.00005} // 5 spot steps, half the tolerance
	o, err := NewOpener(wallet, h.perp, h.cfg, h.rec)
	if err != nil {
		t.Fatal(err)
	}
	res, err := o.Open(context.Background(), h.intent)
	if err != nil || res.Outcome != OutcomeBothOpen {
		t.Fatalf("Open: %v %q%s", err, res.Outcome, h.rec.Dump())
	}
	if want := res.Spot.FilledQtyCoin - res.SpotBuyBaseFeeQtyCoin; res.SpotHeldQtyCoin != want {
		t.Errorf("held %v, want the fills' %v — the extra coin was counted as the leg", res.SpotHeldQtyCoin, want)
	}
}

// Review part 2, M-a: an unwind whose fills state NO base-coin fee (a maker
// fill) sells the whole fill, and its flat proof must expect exactly that — not
// the published rate's smaller figure, which read a clean unwind as a conflict.
func TestOpen_AnUnwindAfterAMakerFillIsFlatWithoutAFalseConflict(t *testing.T) {
	h := newHarness(t, nil)
	h.intent.SpotBuyFeeInBaseFrac = 0.001
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1, RefuseSpotSellBeyondBalance: true}) // maker: nothing kept
	h.perp.SetBehaviour(brokertest.Behaviour{RejectWith: fmt.Errorf("%w: perp refused", broker.ErrInvalidOrder)})

	res, err := h.opener.Open(context.Background(), h.intent)
	if err == nil || errors.Is(err, ErrFlatEvidenceConflict) || errors.Is(err, ErrUnwindIncomplete) {
		t.Fatalf("want a clean unwind, got %v%s", err, h.rec.Dump())
	}
	assertWalletHedged(t, h, res, OutcomeBothFlat)
}
