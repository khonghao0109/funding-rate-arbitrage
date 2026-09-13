package execution

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/broker/brokertest"
)

// Step 4.5's tests. Every one of them asserts against what the fake VENUES hold
// rather than against what CloseResult says, for the same reason step 4.4a's do:
// a machine that has lost a leg reports success with complete confidence.

// closeHarness opens a position and hands back everything needed to close it.
type closeHarness struct {
	*harness
	open Result
	req  CloseRequest
}

func newCloseHarness(t *testing.T, tune func(*Config)) *closeHarness {
	t.Helper()
	h := newHarness(t, tune)
	res, err := h.opener.Open(context.Background(), h.intent)
	if err != nil {
		t.Fatalf("the position could not be opened, so there is nothing to close: %v%s", err, h.rec.Dump())
	}
	if res.Outcome != OutcomeBothOpen {
		t.Fatalf("open ended %q, want both_open", res.Outcome)
	}
	return &closeHarness{
		harness: h,
		open:    res,
		req: CloseRequest{
			Intent:                 h.intent,
			OpenedAtMs:             time.Now().UnixMilli() - time.Hour.Milliseconds(),
			EntrySpotAvgPriceQuote: res.Spot.AvgFillPriceQuote,
			EntryPerpAvgPriceQuote: res.Perp.AvgFillPriceQuote,
			EntrySpotRefMidQuote:   h.intent.SpotBook.MidPriceQuote,
			EntryPerpRefMidQuote:   h.intent.PerpBook.MidPriceQuote,
		},
	}
}

func TestClose_BothLegsClose_AndTheVenuesEndFlat(t *testing.T) {
	h := newCloseHarness(t, nil)
	res, err := h.opener.Close(context.Background(), h.req)
	if err != nil {
		t.Fatalf("Close: %v%s", err, h.rec.Dump())
	}
	if res.Outcome != OutcomeBothFlat {
		t.Fatalf("outcome = %q, want both_flat%s", res.Outcome, h.rec.Dump())
	}
	// The invariant, read from the venues: every buy and sell nets to nothing.
	if got := netQtyCoin(h.spot, broker.MarketSpot); math.Abs(got) > 1e-12 {
		t.Errorf("the spot venue still nets %v coin after a complete close", got)
	}
	if got := netQtyCoin(h.perp, broker.MarketFuturesUSDM); math.Abs(got) > 1e-12 {
		t.Errorf("the perp venue still nets %v coin after a complete close", got)
	}
	if res.ClosedQtyCoin != h.open.Perp.FilledQtyCoin {
		t.Errorf("closed %v, want the %v that was open", res.ClosedQtyCoin, h.open.Perp.FilledQtyCoin)
	}
	if res.SpotFlatEvidenceVI == "" {
		t.Error("no flat evidence was reported for the spot leg")
	}
}

// The perp leg closes FIRST, and the order is the mirror of Open's: the leg
// whose failure would leave the liquidatable side naked goes before the leg
// whose failure would not.
func TestClose_ClosesThePerpLegBeforeTheSpotLeg(t *testing.T) {
	h := newCloseHarness(t, nil)
	if _, err := h.opener.Close(context.Background(), h.req); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var order []LegName
	for _, e := range h.rec.Events() {
		if e.Kind == EventCloseLeg {
			order = append(order, e.Leg)
		}
	}
	if len(order) != 2 || order[0] != LegPerp || order[1] != LegSpot {
		t.Errorf("closing order was %v, want [perp spot] — a naked spot long cannot be liquidated and a naked perp short can", order)
	}
}

// Everything refusable is refused BEFORE anything is sent, so a refusal leaves
// the position exactly as it was. That is the only wholly safe failure a close
// has, which is why the checks are all in front.
func TestClose_RefusalsHappenBeforeAnythingIsSent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*closeHarness)
		wantErr error
	}{
		{
			name: "the venue holds nothing",
			mutate: func(c *closeHarness) {
				c.perp.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: c.intent.Symbol})
			},
			wantErr: ErrNothingToClose,
		},
		{
			name: "the venue holds less than we were told to close",
			mutate: func(c *closeHarness) {
				c.perp.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: c.intent.Symbol, QtyCoin: -0.01})
				c.req.QtyCoin = c.open.Perp.FilledQtyCoin
			},
			wantErr: ErrPositionDisagrees,
		},
		{
			name: "the venue holds the OPPOSITE side",
			mutate: func(c *closeHarness) {
				c.perp.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: c.intent.Symbol, QtyCoin: +0.3333})
			},
			wantErr: ErrPositionDisagrees,
		},
		{
			name: "the size is under a venue minimum",
			mutate: func(c *closeHarness) {
				c.perp.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: c.intent.Symbol, QtyCoin: -0.0001})
			},
			wantErr: ErrCloseRefused,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCloseHarness(t, nil)
			before := len(h.spot.Orders()) + len(h.perp.Orders())
			tc.mutate(h)

			res, err := h.opener.Close(context.Background(), h.req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v%s", err, tc.wantErr, h.rec.Dump())
			}
			if !errors.Is(err, ErrCloseRefused) {
				t.Errorf("err = %v, want it to also be a before-sending refusal", err)
			}
			if after := len(h.spot.Orders()) + len(h.perp.Orders()); after != before {
				t.Errorf("%d order(s) were sent by a refused close", after-before)
			}
			if res.Outcome != OutcomeBothOpen {
				t.Errorf("outcome = %q, want both_open — a refused close leaves the position where it was", res.Outcome)
			}
		})
	}
}

// A perp close that only half-fills must shrink the PAIR, not unbalance it.
// Closing the requested quantity on spot when the perp managed half of it is
// exactly how a close leaves a naked leg.
func TestClose_APartialPerpCloseIsMatchedByTheSpotLeg(t *testing.T) {
	h := newCloseHarness(t, nil)
	h.perp.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1, MarketFillFraction: 0.5})

	res, err := h.opener.Close(context.Background(), h.req)
	if !errors.Is(err, ErrCloseIncomplete) {
		t.Fatalf("err = %v, want ErrCloseIncomplete%s", err, h.rec.Dump())
	}
	if math.Abs(res.Spot.UnwoundQtyCoin-res.Perp.UnwoundQtyCoin) > h.intent.PerpInstrument.StepSizeCoin+1e-12 {
		t.Fatalf("spot closed %v and perp closed %v — a partial close unbalanced the pair, which is the one thing it must never do",
			res.Spot.UnwoundQtyCoin, res.Perp.UnwoundQtyCoin)
	}
	if res.Outcome != OutcomeBothOpen {
		t.Errorf("outcome = %q, want both_open — the pair is smaller, not gone", res.Outcome)
	}
	if res.RemainingQtyCoin <= 0 {
		t.Errorf("remaining = %v, want what is still open on both legs", res.RemainingQtyCoin)
	}
	// And the venues agree: what is left is hedged.
	spotNet := netQtyCoin(h.spot, broker.MarketSpot)
	perpNet := netQtyCoin(h.perp, broker.MarketFuturesUSDM)
	if math.Abs(spotNet+perpNet) > h.intent.PerpInstrument.StepSizeCoin+1e-12 {
		t.Errorf("INVARIANT VIOLATED at the venues: spot net %v, perp net %v", spotNet, perpNet)
	}
}

// ------------------------------------------------------------ the figures

func TestClose_RealizedIsExactlyFundingMinusCommissionMinusSlippage(t *testing.T) {
	h := newCloseHarness(t, nil)
	// Three settlements the VENUE says it paid — rows, not a rate times a
	// holding time (CLAUDE.md rule 6).
	openedAtMs := h.req.OpenedAtMs
	h.perp.SetFundingIncome(
		broker.FundingIncome{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", IncomeQuote: 1.25, Asset: "USDT", SettledAtMs: openedAtMs + 1000},
		broker.FundingIncome{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", IncomeQuote: 0.75, Asset: "USDT", SettledAtMs: openedAtMs + 2000},
		// Negative: funding turned and the short PAID. Summed with its sign,
		// because taking the absolute value would turn a cost into a profit.
		broker.FundingIncome{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", IncomeQuote: -0.40, Asset: "USDT", SettledAtMs: openedAtMs + 3000},
		// Outside the window: somebody else's position.
		broker.FundingIncome{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", IncomeQuote: 99, Asset: "USDT", SettledAtMs: openedAtMs - 5000},
	)
	h.spot.SetCommission(0.0001, "USDT")
	h.perp.SetCommission(0.0004, "USDT")

	res, err := h.opener.Close(context.Background(), h.req)
	if err != nil {
		t.Fatalf("Close: %v%s", err, h.rec.Dump())
	}

	if res.SettlementsCounted != 3 {
		t.Errorf("counted %d settlements, want the 3 inside the holding window", res.SettlementsCounted)
	}
	if want := 1.60; math.Abs(res.FundingReceivedQuote-want) > 1e-9 {
		t.Errorf("funding = %v, want %v (1.25 + 0.75 - 0.40, signs as the venue gives them)", res.FundingReceivedQuote, want)
	}
	if res.CommissionQuote <= 0 {
		t.Errorf("commission = %v, want the venue's own charge on four fills: %s", res.CommissionQuote, res.CommissionSourceVI)
	}
	if want := res.FundingReceivedQuote - res.CommissionQuote - res.SlippageQuote; math.Abs(res.RealizedQuote-want) > 1e-9 {
		t.Errorf("RealizedQuote = %v, want %v — it is exactly the three components and nothing else", res.RealizedQuote, want)
	}
	for _, s := range []string{res.FundingSourceVI, res.CommissionSourceVI} {
		if !strings.Contains(s, "đọc từ sàn") {
			t.Errorf("a figure does not say it came from the venue: %q", s)
		}
	}
}

// A figure that could not be read must not be indistinguishable from a figure
// that was read and came out zero.
func TestClose_AnUnreadFigureSaysSoRatherThanReportingZero(t *testing.T) {
	h := newCloseHarness(t, nil)
	// A broker with no funding capability at all: the interface assertion
	// fails, and the result has to say why rather than report 0.
	plain := onlyBroker{h.perp}
	tr, err := NewOpener(h.spot, plain, h.cfg, h.rec)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := tr.Close(context.Background(), h.req)

	if res.FundingReceivedQuote != 0 {
		t.Fatalf("funding = %v from a venue that cannot report it", res.FundingReceivedQuote)
	}
	if !strings.Contains(res.FundingSourceVI, "CHƯA ĐỌC ĐƯỢC") {
		t.Errorf("a zero that means 'not read' is not labelled: %q", res.FundingSourceVI)
	}
	if !strings.Contains(res.CommissionSourceVI, "CHƯA ĐỌC ĐƯỢC") {
		t.Errorf("commission from an unreadable venue is not labelled: %q", res.CommissionSourceVI)
	}
}

// onlyBroker hides every optional capability, leaving the plain interface. It
// is what a venue that cannot answer looks like from here.
type onlyBroker struct{ broker.Broker }

// Commission taken in an asset that is not the quote asset is REPORTED,
// unconverted, and never summed into the quote figure: converting it needs a
// price at a moment, and picking one silently is rule 5's wrong-but-not-
// obviously-wrong number.
func TestClose_CommissionInAnotherAssetIsReportedAndNotConverted(t *testing.T) {
	h := newCloseHarness(t, nil)
	h.spot.SetCommission(0.001, "BNB")
	h.perp.SetCommission(0.0004, "USDT")
	res, err := h.opener.Close(context.Background(), h.req)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.Contains(res.CommissionOtherVI, "BNB") {
		t.Errorf("the BNB commission is not reported: %q", res.CommissionOtherVI)
	}
	if strings.Contains(res.CommissionOtherVI, "USDT") {
		t.Errorf("a quote-asset commission was filed under 'other': %q", res.CommissionOtherVI)
	}
	// And it is NOT in the quote total.
	perpOnly := res.CommissionQuote
	h2 := newCloseHarness(t, nil)
	h2.spot.SetCommission(0, "")
	h2.perp.SetCommission(0.0004, "USDT")
	res2, err := h2.opener.Close(context.Background(), h2.req)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if math.Abs(perpOnly-res2.CommissionQuote) > 1e-9 {
		t.Errorf("CommissionQuote moved from %v to %v when the OTHER-asset fee was removed — it was being summed in",
			perpOnly, res2.CommissionQuote)
	}
}

// CLAUDE.md rule 2: only internal/strategy may say "net". A result that is
// funding minus two costs and nothing else must not wear that word, because a
// reader would take it for a figure with the book and all four fills deducted.
func TestCloseResult_NeverSaysNet(t *testing.T) {
	bad := regexp.MustCompile(`(?i)net(apr|profit|pnl|income|quote)?$|^net`)
	tp := reflect.TypeOf(CloseResult{})
	for i := 0; i < tp.NumField(); i++ {
		if name := tp.Field(i).Name; bad.MatchString(name) {
			t.Errorf("CloseResult.%s uses the word 'net'; only internal/strategy may (rule 2)", name)
		}
	}
	// And the drift the figure does NOT include has to be reported beside it,
	// or a reader will assume it was deducted.
	if _, ok := tp.FieldByName("PairPriceDriftQuote"); !ok {
		t.Error("the basis drift between entry and exit is not reported, so nothing tells a reader it is excluded")
	}
}

// A close under a cancelled context still has to reach the invariant: the
// caller giving up is exactly the caller who must not be left holding one leg.
func TestClose_ContextCancelledMidway_StillReachesTheInvariant(t *testing.T) {
	h := newCloseHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := h.opener.Close(ctx, h.req)
	_ = err
	spotNet := netQtyCoin(h.spot, broker.MarketSpot)
	perpNet := netQtyCoin(h.perp, broker.MarketFuturesUSDM)
	residual := math.Abs(spotNet + perpNet)
	if residual > h.intent.PerpInstrument.StepSizeCoin+1e-12 {
		t.Fatalf("INVARIANT VIOLATED at the venues after a cancelled close: spot net %v, perp net %v (%s)%s",
			spotNet, perpNet, res.ReasonVI, h.rec.Dump())
	}
}

var _ = fmt.Sprintf

// A MARKET order's ANSWER is not always its RESULT, and believing it cost a
// naked leg on the real venue.
//
// Binance USDⓈ-M replies to POST /fapi/v1/order with an acknowledgement —
// status NEW, executedQty 0 — and reports the fill only on a read-back; spot,
// on the same exchange, answers with the fills attached. Measured on testnet
// 2026-09-13 during the 4.5 acceptance: the closing perp order came back NEW,
// the machine read it as "nothing filled", reported both legs intact, and the
// venue had already flattened the perp side — leaving 0.0008 BTC of spot long
// with no hedge.
//
// This is the same class of mistake as believing our own cancel, and it is
// fixed the same way: ask the venue.
func TestClose_AMarketAcknowledgementIsNotAFill(t *testing.T) {
	h := newCloseHarness(t, nil)
	h.perp.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1, MarketAckIsNotAFill: true})
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1, MarketAckIsNotAFill: true})

	res, err := h.opener.Close(context.Background(), h.req)
	if err != nil {
		t.Fatalf("Close: %v%s", err, h.rec.Dump())
	}
	if res.Outcome != OutcomeBothFlat {
		t.Fatalf("outcome = %q, want both_flat — the orders DID fill, the venue simply said so a moment later%s",
			res.Outcome, h.rec.Dump())
	}
	if res.ClosedQtyCoin <= 0 {
		t.Fatalf("closed %v: an acknowledgement was read as a result", res.ClosedQtyCoin)
	}
	// The venues, which is where the naked leg would show.
	spotNet := netQtyCoin(h.spot, broker.MarketSpot)
	perpNet := netQtyCoin(h.perp, broker.MarketFuturesUSDM)
	if math.Abs(spotNet) > 1e-12 || math.Abs(perpNet) > 1e-12 {
		t.Fatalf("INVARIANT VIOLATED at the venues: spot net %v, perp net %v", spotNet, perpNet)
	}
}

// The same trap on the UNWIND path, which is the one that runs when a leg has
// already been left naked and is therefore the one that must not repeat it.
func TestOpen_UnwindDoesNotBelieveAMarketAcknowledgement(t *testing.T) {
	h := newHarness(t, nil)
	// Leg 1 half-fills so the run unwinds; the unwind's MARKET order is then
	// acknowledged rather than reported filled.
	h.spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.5, MarketAckIsNotAFill: true})

	res, err := h.opener.Open(context.Background(), h.intent)
	if err == nil {
		t.Fatal("a leg that fell short must report an error")
	}
	h.assertInvariant(t, res)
	if res.Outcome != OutcomeBothFlat {
		t.Fatalf("outcome = %q, want both_flat%s", res.Outcome, h.rec.Dump())
	}
	if res.Spot.UnwoundQtyCoin <= 0 {
		t.Errorf("the unwind reported closing nothing, though the venue filled it%s", h.rec.Dump())
	}
}
