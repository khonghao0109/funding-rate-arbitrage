package crossperp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/broker/binance"
	"futures-arbitrage-scanner/internal/broker/brokertest"
	"futures-arbitrage-scanner/internal/broker/bybit"
	"futures-arbitrage-scanner/internal/execution"
)

// Every assertion about what is held reads the FAKE VENUES (h.positions), never
// the Result alone: a machine that has lost a leg reports both_flat with
// complete confidence (execution's 4.4a lesson).

var errDefiniteRefusal = fmt.Errorf("%w: test: the venue refused the order (110007 ab not enough)", broker.ErrInvalidOrder)

// (a) Both legs fill: both open at the common size, one marketable limit each,
// priced from each venue's own touch, under derived ids.
func TestOpen_BothLegsFill_BothOpenAtTheCommonSize(t *testing.T) {
	h := newHarness(t)
	intent := h.intent()
	res, err := h.exec.Open(context.Background(), intent)
	if err != nil || res.Outcome != OutcomeBothOpen {
		t.Fatalf("Open: %s %v\n%s", res.Outcome, err, h.rec.Dump())
	}
	long, short := h.positions()
	if long != 0.333 || short != -0.333 {
		t.Fatalf("venues hold %v / %v, want +0.333 / −0.333", long, short)
	}
	if res.TargetQtyCoin != 0.333 || res.CommonStepCoin != 0.001 || res.DeltaImbalanceQtyCoin != 0 {
		t.Errorf("sizing %+v", res)
	}
	lo := h.binance.sentWith(testIntentID, PurposeOpen, LegLong)
	so := h.bybit.sentWith(testIntentID, PurposeOpen, LegShort)
	if len(lo) != 1 || len(so) != 1 || len(h.binance.sent()) != 1 || len(h.bybit.sent()) != 1 {
		t.Fatalf("orders sent: binance %v, bybit %v — want exactly one opening order per venue", h.binance.sent(), h.bybit.sent())
	}
	capQuote := intent.Long.Book.BestAskQuote * (1 + h.cfg.MaxSlippageBps/10_000)
	if o := lo[0]; o.Side != broker.SideBuy || o.Type != broker.OrderTypeLimitGTC || o.QtyCoin != 0.333 ||
		o.PriceQuote > capQuote+1e-9 || o.PriceQuote < intent.Long.Book.BestAskQuote || o.ReduceOnly {
		t.Errorf("long order %+v, want a BUY LIMIT capped at %v (best ask %v)", o, capQuote, intent.Long.Book.BestAskQuote)
	}
	floorQuote := intent.Short.Book.BestBidQuote * (1 - h.cfg.MaxSlippageBps/10_000)
	if o := so[0]; o.Side != broker.SideSell || o.Type != broker.OrderTypeLimitGTC || o.QtyCoin != 0.333 ||
		o.PriceQuote < floorQuote-1e-9 || o.PriceQuote > intent.Short.Book.BestBidQuote || o.ReduceOnly {
		t.Errorf("short order %+v, want a SELL LIMIT floored at %v (best bid %v)", o, floorQuote, intent.Short.Book.BestBidQuote)
	}
	if res.Long.VenuePositionQtyCoin != 0.333 || res.Short.VenuePositionQtyCoin != -0.333 || !res.Long.OrderConfirmed || !res.Short.OrderConfirmed {
		t.Errorf("result legs %+v / %+v", res.Long, res.Short)
	}
}

// (b) Leg 2 refused by the venue after leg 1 filled: leg 1 unwinds to flat with
// one reduce-only MARKET order, and nothing about it is loud.
func TestOpen_ShortLegRefused_LongUnwoundFlat(t *testing.T) {
	h := newHarness(t)
	h.bybit.answerAfterOtherOpens(h.binance, errDefiniteRefusal)
	res, err := h.exec.Open(context.Background(), h.intent())

	if res.Outcome != OutcomeUnwoundFlat || !errors.Is(err, ErrOpenUnwound) || errors.Is(err, execution.ErrUnwindIncomplete) {
		t.Fatalf("%s, %v — want unwound_flat, not loud\n%s", res.Outcome, err, h.rec.Dump())
	}
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v after the unwind", long, short)
	}
	unwinds := h.binance.sentWith(testIntentID, PurposeUnwind, LegLong)
	if len(unwinds) != 1 {
		t.Fatalf("unwind orders %+v, want exactly one", unwinds)
	}
	if u := unwinds[0]; u.Side != broker.SideSell || u.Type != broker.OrderTypeMarket || !u.ReduceOnly || u.QtyCoin != 0.333 ||
		u.ClientOrderID != ClientOrderID(testIntentID, PurposeUnwind, LegLong, res.AttemptNonce, 1) {
		t.Errorf("unwind %+v, want SELL MARKET reduce-only 0.333 under the round-1 unwind id", u)
	}
	if n := len(h.bybit.sent()); n != 1 {
		t.Errorf("bybit received %d sends, want 1 — a definite refusal is not resent", n)
	}
	// On an in-memory venue this is microseconds, and says nothing about a real
	// one; the design's < 500 ms is only asserted as an upper bound here.
	if res.UnwindDuration <= 0 || res.UnwindDuration > 500*time.Millisecond {
		t.Errorf("UnwindDuration %s", res.UnwindDuration)
	}
}

// The operator's rule, on a fake: leg 1 filled on Binance, and leg 2's send to
// Bybit died on the network and never reached it. The Binance long is taken back
// to flat at once — well inside 500 ms of the failed send — while Bybit is still
// being asked about the lost order for the whole quiet period; the order is never
// resent, and since "no such order" proves nothing (review 4.5k, N2) the flat
// result is LOUD and keeps the lock. The 500 ms bound is on this machine's own
// work — every venue call here is in memory — and says nothing about a real
// venue's round trips.
func TestOpen_ShortLegLostOnTheNetwork_LongFlatWithin500msAndResultLoud(t *testing.T) {
	h := newHarness(t)
	h.cfg.AmbiguousSendQuiet = 800 * time.Millisecond
	h.build(holdsFor{"BTCUSDT", testIntentID}, allowAll{})
	var mu sync.Mutex
	var failedAt, flatAt time.Time
	h.bybit.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			if req.ReduceOnly {
				return nil
			}
			h.binance.awaitOpeningSent()
			mu.Lock()
			failedAt = time.Now()
			mu.Unlock()
			return errTransport
		}
	})
	h.binance.set(func(s *scripted) {
		s.placeAfter = func(req broker.PlaceOrderRequest) error {
			if req.ReduceOnly {
				if p, _ := s.Fake.GetPosition(context.Background(), broker.MarketFuturesUSDM, "BTCUSDT"); p.QtyCoin == 0 {
					mu.Lock()
					if flatAt.IsZero() {
						flatAt = time.Now()
					}
					mu.Unlock()
				}
			}
			return nil
		}
	})
	started := time.Now()
	res, err := h.exec.Open(context.Background(), h.intent())
	elapsed := time.Since(started)

	if res.Outcome != OutcomeUnwoundFlat || !errors.Is(err, ErrLegAmbiguous) || !errors.Is(err, execution.ErrUnwindIncomplete) {
		t.Fatalf("%s, %v — want unwound_flat AND loud\n%s", res.Outcome, err, h.rec.Dump())
	}
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v", long, short)
	}
	if sends := h.bybit.sent(); len(sends) != 1 {
		t.Errorf("bybit sends %+v — an opening order of Engine 2 is never resent (review 4.5k, B3)", sends)
	}
	mu.Lock()
	window := flatAt.Sub(failedAt)
	mu.Unlock()
	if flatAt.IsZero() || window <= 0 || window >= 500*time.Millisecond {
		t.Errorf("the long was flat %s after the send failed (flat at %v), want within 500 ms", window, flatAt)
	}
	if elapsed < h.cfg.AmbiguousSendQuiet {
		t.Errorf("Open returned after %s, before the %s quiet period — the lost order was not asked about for long enough", elapsed, h.cfg.AmbiguousSendQuiet)
	}
	if res.Short.OrderConfirmed {
		t.Error("an order the venue never showed was reported as confirmed")
	}
}

// Review 4.5k, B3: the lookup answers "no such order" before the venue's backend
// has processed the send, and the order then appears and fills. It is found,
// never resent — and because a leg cut off from its partner is unwound at once,
// the long is already flat by then, so the late short is taken back to flat too.
// Every order is proven finished, so nothing about it is loud.
func TestOpen_AnOrderTheVenueProcessesLateIsFoundAndTakenBack(t *testing.T) {
	h := newHarness(t)
	h.cfg.AmbiguousSendQuiet = 150 * time.Millisecond
	h.build(holdsFor{"BTCUSDT", testIntentID}, allowAll{})
	late := make(chan struct{})
	h.bybit.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			if !isOrderOf(req.ClientOrderID, PurposeOpen, LegShort) {
				return nil
			}
			h.binance.awaitOpeningSent()
			go func() {
				defer close(late)
				time.Sleep(20 * time.Millisecond)
				if _, err := s.Fake.PlaceOrder(context.Background(), req); err != nil {
					t.Errorf("late original: %v", err)
				}
			}()
			return errTransport // the answer is lost; the backend processes it 20 ms later
		}
	})
	res, err := h.exec.Open(context.Background(), h.intent())
	<-late
	if res.Outcome != OutcomeUnwoundFlat || !errors.Is(err, ErrOpenUnwound) || isLoud(err) {
		t.Fatalf("%s, %v — want unwound_flat, not loud\n%s", res.Outcome, err, h.rec.Dump())
	}
	if n := len(h.bybit.sentWith(testIntentID, PurposeOpen, LegShort)); n != 1 {
		t.Errorf("%d opening sends on bybit, want the one original", n)
	}
	if n := len(h.bybit.sentWith(testIntentID, PurposeUnwind, LegShort)); n == 0 {
		t.Error("the late short was never taken back")
	}
	if !res.Short.OrderConfirmed || res.Short.FilledQtyCoin != 0.333 {
		t.Errorf("short leg %+v — want the late order found and confirmed", res.Short)
	}
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Errorf("venues hold %v / %v", long, short)
	}
}

// Review 4.5k, N2: the quiet period runs from when the send RETURNED. A send that
// takes longer than the quiet period itself — an HTTP client timeout is 20 s
// against a 6 s quiet — would otherwise have used it all up before the first
// lookup. Here the order reaches the venue 40 ms after the slow send returned,
// inside a 60 ms quiet counted from the return and long after one counted from the
// send: it must be found.
func TestOpen_TheQuietPeriodRunsFromWhenTheSendReturned(t *testing.T) {
	h := newHarness(t)
	h.cfg.AmbiguousSendQuiet = 60 * time.Millisecond
	h.cfg.OrderSettleTimeout = 5 * time.Millisecond
	h.build(holdsFor{"BTCUSDT", testIntentID}, allowAll{})
	late := make(chan struct{})
	h.bybit.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			if !isOrderOf(req.ClientOrderID, PurposeOpen, LegShort) {
				return nil
			}
			time.Sleep(80 * time.Millisecond) // the send itself outlasts the quiet period
			go func() {
				defer close(late)
				time.Sleep(40 * time.Millisecond)
				if _, err := s.Fake.PlaceOrder(context.Background(), req); err != nil {
					t.Errorf("late original: %v", err)
				}
			}()
			return errTransport
		}
	})
	res, err := h.exec.Open(context.Background(), h.intent())
	<-late
	if isLoud(err) || !res.Short.OrderConfirmed {
		t.Fatalf("%s, %v, short %+v — the order arrived inside the quiet period counted from the send's return and was not found\n%s",
			res.Outcome, err, res.Short, h.rec.Dump())
	}
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Errorf("venues hold %v / %v", long, short)
	}
}

// Review 4.5k, N2, the other half: an order that arrives AFTER Open has given up
// asking was never concluded absent — the result said so, loudly.
func TestOpen_AnOrderStillUnseenAfterTheQuietPeriodIsNeverCalledAbsent(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var lost *broker.PlaceOrderRequest
	h.bybit.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			if !isOrderOf(req.ClientOrderID, PurposeOpen, LegShort) {
				return nil
			}
			h.binance.awaitOpeningSent()
			mu.Lock()
			r := req
			lost = &r
			mu.Unlock()
			return errTransport
		}
	})
	res, err := h.exec.Open(context.Background(), h.intent())
	if !errors.Is(err, ErrLegAmbiguous) || res.Short.OrderConfirmed {
		t.Fatalf("%s, %v — an order the venue never showed must leave the result loud", res.Outcome, err)
	}
	// The backend processes it now, after Open returned — which is exactly why
	// the result was loud: the caller keeps the lock and a close reads the venue
	// (engine_test.go).
	mu.Lock()
	req := *lost
	mu.Unlock()
	if _, perr := h.bybit.Fake.PlaceOrder(context.Background(), req); perr != nil {
		t.Fatal(perr)
	}
	if _, short := h.positions(); short != -0.333 {
		t.Fatalf("short %v", short)
	}
}

// A place whose answer was lost AFTER the venue took and filled it is found by
// its id and never resent; the open is taken back to flat, and nothing about it
// is loud.
func TestOpen_ShortLegAnswerLostAfterAccepting_FoundNotResent(t *testing.T) {
	h := newHarness(t)
	h.bybit.set(func(s *scripted) {
		first := true
		s.placeAfter = func(req broker.PlaceOrderRequest) error {
			if first {
				first = false
				return errTransport
			}
			return nil
		}
	})
	res, err := h.exec.Open(context.Background(), h.intent())
	if res.Outcome != OutcomeUnwoundFlat || isLoud(err) {
		t.Fatalf("%s, %v\n%s", res.Outcome, err, h.rec.Dump())
	}
	if n := len(h.bybit.sentWith(testIntentID, PurposeOpen, LegShort)); n != 1 {
		t.Errorf("%d opening sends on bybit — a resend of a filled order doubles the short", n)
	}
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Errorf("venues hold %v / %v", long, short)
	}
}

// (c) Leg 2 fills 40% and the rest never does: the remainder is cancelled and
// read back, and leg 1 is CUT to match rather than the pair thrown away.
func TestOpen_ShortLegPartialFill_LongReducedToMatch(t *testing.T) {
	h := newHarness(t)
	h.bybit.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.4})
	res, err := h.exec.Open(context.Background(), h.intent())
	if err != nil || res.Outcome != OutcomeBothOpen || !res.ReducedToMatch {
		t.Fatalf("%s reduced=%v, %v\n%s", res.Outcome, res.ReducedToMatch, err, h.rec.Dump())
	}
	long, short := h.positions()
	// 0.333 × 0.4 = 0.1332, floored onto Bybit's 0.001 grid by the venue.
	if long != 0.133 || short != -0.133 {
		t.Fatalf("venues hold %v / %v, want +0.133 / −0.133", long, short)
	}
	cuts := h.binance.sentWith(testIntentID, PurposeReduce, LegLong)
	if len(cuts) != 1 || cuts[0].QtyCoin != 0.2 || !cuts[0].ReduceOnly || cuts[0].Type != broker.OrderTypeMarket || cuts[0].Side != broker.SideSell {
		t.Errorf("cuts %+v, want one reduce-only MARKET SELL of 0.2", cuts)
	}
	if len(h.binance.sentWith(testIntentID, PurposeUnwind, LegLong))+len(h.bybit.sentWith(testIntentID, PurposeUnwind, LegShort)) != 0 {
		t.Error("a pair that could be reduced was unwound")
	}
	if res.Short.Status != broker.OrderStatusCanceled || !res.Short.OrderConfirmed {
		t.Errorf("the partial leg %+v — want its remainder cancelled and confirmed by read-back", res.Short)
	}
}

// PLAN 4.5i correction 9, paid here: a cancel that is only ACKNOWLEDGED leaves the
// order live, and it fills more before it ends. The read-back must wait for the
// venue to say the order is finished — taking the first answer would size the
// pair on 0.133 while the venue ends at 0.2, and cut a leg that is really there.
func TestOpen_AnAsynchronousCancelIsReadBackUntilTheOrderIsFinished(t *testing.T) {
	h := newHarness(t)
	h.bybit.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.4})
	done := make(chan struct{})
	var once sync.Once
	h.bybit.set(func(s *scripted) {
		// The venue completes the FIRST acknowledged cancel; a repeated cancel
		// (confirmLeg re-sends one to an order it keeps reading back as working,
		// as Bybit's own client does) changes nothing further.
		s.cancelAck = func(q broker.OrderQuery) {
			once.Do(func() {
				go func() {
					defer close(done)
					time.Sleep(10 * time.Millisecond)
					if err := s.Fake.Fill(q.ClientOrderID, 0.067, testMid); err != nil {
						t.Errorf("late fill: %v", err)
					}
					if _, err := s.Fake.CancelOrder(context.Background(), q); err != nil {
						t.Errorf("late cancel: %v", err)
					}
				}()
			})
		}
	})
	res, err := h.exec.Open(context.Background(), h.intent())
	<-done
	if err != nil || res.Outcome != OutcomeBothOpen {
		t.Fatalf("%s, %v\n%s", res.Outcome, err, h.rec.Dump())
	}
	if res.Short.FilledQtyCoin != 0.2 || res.Short.Status != broker.OrderStatusCanceled {
		t.Fatalf("short leg %+v — want the FINAL read: CANCELED after 0.2", res.Short)
	}
	if long, short := h.positions(); long != 0.2 || short != -0.2 {
		t.Fatalf("venues hold %v / %v, want +0.2 / −0.2", long, short)
	}
	if n := len(h.bybit.sentWith(testIntentID, PurposeReduce, LegShort)); n != 0 {
		t.Errorf("the short leg was cut %d times — its late fill was real", n)
	}
}

// A partial fill too small to KEEP (closeable without any reduce-only exemption
// on both venues) is not kept: both legs unwind.
func TestOpen_PartialFillTooSmallToKeep_UnwindsBoth(t *testing.T) {
	h := newHarness(t)
	h.bybit.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.01})
	intent := h.intent()
	const lowMid = 20_000.0
	intent.Long.Book = deepBook(venueBinance, lowMid, time.Now().UnixMilli())
	intent.Short.Book = deepBook(venueBybit, lowMid, time.Now().UnixMilli())
	intent.NotionalQuote = 2_000 // Q = 0.1; 1% = 0.001 BTC = $20, under Binance's $50

	res, err := h.exec.Open(context.Background(), intent)
	if res.Outcome != OutcomeUnwoundFlat || !errors.Is(err, ErrOpenUnwound) || !strings.Contains(err.Error(), "không thu nhỏ được") {
		t.Fatalf("%s, %v\n%s", res.Outcome, err, h.rec.Dump())
	}
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v", long, short)
	}
	if len(h.bybit.sentWith(testIntentID, PurposeUnwind, LegShort)) != 1 {
		t.Error("the short sliver was not unwound — it is a position somebody could close, so it is real")
	}
}

// Question 4, at the executor: a venue already holding a position on the symbol
// (Engine 1's short, or anyone's) or working an order there refuses the open
// BEFORE anything is sent — whatever the lock file says.
func TestOpen_AVenueAlreadyHoldingTheSymbolIsRefusedBeforeSending(t *testing.T) {
	h := newHarness(t)
	h.binance.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", QtyCoin: -0.05})
	res, err := h.exec.Open(context.Background(), h.intent())
	if !errors.Is(err, ErrVenueNotFlat) || !errors.Is(err, ErrRefusedBeforePlacing) || res.Outcome != OutcomeBothFlat {
		t.Fatalf("%s, %v", res.Outcome, err)
	}
	if len(h.binance.sent())+len(h.bybit.sent()) != 0 {
		t.Fatalf("orders were sent beside Engine 1's short: %v %v", h.binance.sent(), h.bybit.sent())
	}

	h = newHarness(t)
	h.bybit.SetBehaviour(brokertest.Behaviour{}) // rests
	if _, err := h.bybit.Fake.PlaceOrder(context.Background(), broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT",
		Side: broker.SideSell, Type: broker.OrderTypeLimitGTC, ClientOrderID: "someone-else", QtyCoin: 0.01, PriceQuote: 70_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.exec.Open(context.Background(), h.intent()); !errors.Is(err, ErrVenueNotFlat) {
		t.Fatalf("a resting order on the symbol: %v, want ErrVenueNotFlat", err)
	}
	if len(h.binance.sent())+len(h.bybit.sent()) != 0 {
		t.Fatal("orders were sent beside a working order")
	}
}

func TestOpen_WithoutTheLockOrPastTheMarginGate_NothingIsSent(t *testing.T) {
	h := newHarness(t)
	h.build(holdsFor{"BTCUSDT", "another-intent"}, allowAll{})
	if _, err := h.exec.Open(context.Background(), h.intent()); !errors.Is(err, ErrLockNotHeld) {
		t.Errorf("lock held by another intent: %v", err)
	}
	h.build(holdsFor{"BTCUSDT", testIntentID}, refuseAll{errors.New("test: bybit_linear yellow")})
	if _, err := h.exec.Open(context.Background(), h.intent()); !errors.Is(err, ErrMarginGate) {
		t.Errorf("margin gate refusing: %v", err)
	}
	if len(h.binance.sent())+len(h.bybit.sent()) != 0 {
		t.Fatal("orders were sent past a refusal")
	}
	if _, err := NewExecutor(testConfig(), nil, allowAll{}, nil); err == nil {
		t.Error("an executor with no lock holder was built")
	}
	if _, err := NewExecutor(testConfig(), holdsFor{}, nil, nil); err == nil {
		t.Error("an executor with no margin gate was built")
	}
}

// Question 2: leg 1 filled on Binance, leg 2's send to Bybit died on the
// network and Bybit never shows the order either way. What is CERTAINLY held —
// the Binance long — is unwound at once; the Bybit order that may still exist
// keeps the result loud.
func TestOpen_ShortLegStaysAmbiguous_CertainLegUnwoundAndResultLoud(t *testing.T) {
	h := newHarness(t)
	h.bybit.answerAfterOtherOpens(h.binance, errTransport)
	h.bybit.set(func(s *scripted) {
		s.getOrder = func(broker.OrderQuery) error { return errNotVisible }
		s.cancel = func(broker.OrderQuery) error { return errNotVisible }
	})
	res, err := h.exec.Open(context.Background(), h.intent())
	if !errors.Is(err, ErrLegAmbiguous) || !errors.Is(err, execution.ErrUnwindIncomplete) {
		t.Fatalf("err %v — want the loud ErrLegAmbiguous\n%s", err, h.rec.Dump())
	}
	if res.Outcome != OutcomeUnwoundFlat {
		t.Errorf("outcome %s, want unwound_flat (the certain leg is gone)", res.Outcome)
	}
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v", long, short)
	}
	if len(h.bybit.sent()) != 1 {
		t.Errorf("bybit sends %v — a place still ambiguous after asking is NEVER resent", h.bybit.sent())
	}
}

// Question 2's second half: the unwind itself meets trouble.
func TestOpen_TheUnwindMeetsTrouble(t *testing.T) {
	t.Run("an IP cool-down stops it at once and is loud", func(t *testing.T) {
		h := newHarness(t)
		h.bybit.answerAfterOtherOpens(h.binance, errDefiniteRefusal)
		h.binance.set(func(s *scripted) {
			s.placeBefore = func(req broker.PlaceOrderRequest) error {
				if req.ReduceOnly {
					return fmt.Errorf("%w: test", broker.ErrIPCoolingDown)
				}
				return nil
			}
		})
		res, err := h.exec.Open(context.Background(), h.intent())
		if res.Outcome != OutcomeUnresolved || !errors.Is(err, execution.ErrUnwindIncomplete) {
			t.Fatalf("%s, %v\n%s", res.Outcome, err, h.rec.Dump())
		}
		if long, _ := h.positions(); long != 0.333 {
			t.Errorf("the naked long is %v; the result must name it, and it must still be there", long)
		}
		if n := len(h.binance.sentWith(testIntentID, PurposeUnwind, LegLong)); n != 1 {
			t.Errorf("%d unwind sends inside a cool-down, want 1 — retrying inside a ban lengthens it", n)
		}
		if !strings.Contains(err.Error(), "+0.333") {
			t.Errorf("the loud error does not print the venue's position: %v", err)
		}
	})

	t.Run("an unwind send that never arrived is followed by a new round", func(t *testing.T) {
		h := newHarness(t)
		h.bybit.answerAfterOtherOpens(h.binance, errDefiniteRefusal)
		h.binance.set(func(s *scripted) {
			first := true
			s.placeBefore = func(req broker.PlaceOrderRequest) error {
				if req.ReduceOnly && first {
					first = false
					return errTransport
				}
				return nil
			}
		})
		res, err := h.exec.Open(context.Background(), h.intent())
		if res.Outcome != OutcomeUnwoundFlat || errors.Is(err, execution.ErrUnwindIncomplete) {
			t.Fatalf("%s, %v\n%s", res.Outcome, err, h.rec.Dump())
		}
		sends := h.binance.sentWith(testIntentID, PurposeUnwind, LegLong)
		if len(sends) != 2 || sends[0].ClientOrderID == sends[1].ClientOrderID {
			t.Errorf("unwind sends %+v — want round 1 and round 2 under different ids", sends)
		}
		if long, short := h.positions(); long != 0 || short != 0 {
			t.Errorf("venues hold %v / %v", long, short)
		}
	})

	// The case that makes a resend dangerous everywhere else: the unwind was
	// taken and FILLED, its answer lost, and the lookup stays ambiguous. The
	// next round reads the position — flat — and sends nothing: reduce-only plus
	// a position read is what makes "try again" safe here.
	t.Run("an unwind that filled with its answer lost is not doubled", func(t *testing.T) {
		h := newHarness(t)
		h.bybit.answerAfterOtherOpens(h.binance, errDefiniteRefusal)
		var mu sync.Mutex
		lostID := ""
		h.binance.set(func(s *scripted) {
			s.placeAfter = func(req broker.PlaceOrderRequest) error {
				mu.Lock()
				defer mu.Unlock()
				if isOrderOf(req.ClientOrderID, PurposeUnwind, LegLong) && lostID == "" {
					lostID = req.ClientOrderID
					return errTransport
				}
				return nil
			}
			s.getOrder = func(q broker.OrderQuery) error {
				mu.Lock()
				defer mu.Unlock()
				if q.ClientOrderID == lostID {
					return errNotVisible
				}
				return nil
			}
		})
		res, err := h.exec.Open(context.Background(), h.intent())
		if res.Outcome != OutcomeUnwoundFlat || errors.Is(err, execution.ErrUnwindIncomplete) {
			t.Fatalf("%s, %v\n%s", res.Outcome, err, h.rec.Dump())
		}
		if n := len(h.binance.sentWith(testIntentID, PurposeUnwind, LegLong)); n != 1 {
			t.Errorf("%d unwind sends, want 1 — the position said flat", n)
		}
		if long, short := h.positions(); long != 0 || short != 0 {
			t.Errorf("venues hold %v / %v", long, short)
		}
	})
}

// Review 4.5k, B2: a reduction toward a size ABOVE zero sends an order it cannot
// prove finished. A second round would be a second reduction that could also
// execute, so the reduction stops and both legs unwind to zero — where the late
// execution of the lost order is refused as reduce-only on a flat position.
func TestOpen_AnUnresolvedReductionUnwindsInsteadOfReportingAHedge(t *testing.T) {
	h := newHarness(t)
	h.bybit.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.4})
	var mu sync.Mutex
	var lost *broker.PlaceOrderRequest
	h.binance.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			mu.Lock()
			defer mu.Unlock()
			if isOrderOf(req.ClientOrderID, PurposeReduce, LegLong) && lost == nil {
				r := req
				lost = &r
				return errTransport
			}
			return nil
		}
		s.getOrder = func(q broker.OrderQuery) error {
			mu.Lock()
			defer mu.Unlock()
			if lost != nil && q.ClientOrderID == lost.ClientOrderID {
				return errNotVisible
			}
			return nil
		}
	})
	res, err := h.exec.Open(context.Background(), h.intent())
	if res.Outcome == OutcomeBothOpen {
		t.Fatalf("both_open beside an unresolved reduction: %v\n%s", err, h.rec.Dump())
	}
	if res.Outcome != OutcomeUnwoundFlat || !errors.Is(err, ErrOpenUnwound) {
		t.Fatalf("%s, %v\n%s", res.Outcome, err, h.rec.Dump())
	}
	if n := len(h.binance.sentWith(testIntentID, PurposeReduce, LegLong)); n != 1 {
		t.Errorf("%d reduce sends, want exactly 1 — no second reduction beside an unresolved one", n)
	}
	// The venue now executes the lost reduction: reduce-only on a flat position.
	mu.Lock()
	lateReq := *lost
	mu.Unlock()
	h.binance.set(func(s *scripted) { s.placeBefore = nil })
	if _, err := h.binance.PlaceOrder(context.Background(), lateReq); err == nil {
		t.Error("a late reduce-only order on a flat position was accepted")
	}
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v after the late execution", long, short)
	}
}

// Review 4.5k, M1: every close-out phase has its own budget. Reductions that
// stay ambiguous until their deadline must not leave the mandatory unwind after
// them with no time: the pair still ends flat.
func TestOpen_TheUnwindHasItsOwnBudget(t *testing.T) {
	h := newHarness(t)
	h.cfg.UnwindTimeout = 300 * time.Millisecond
	h.cfg.OrderSettleTimeout = 150 * time.Millisecond
	h.build(holdsFor{"BTCUSDT", testIntentID}, allowAll{})
	h.bybit.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.4})
	h.binance.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			if isOrderOf(req.ClientOrderID, PurposeReduce, LegLong) {
				return errTransport
			}
			return nil
		}
		s.getOrder = func(q broker.OrderQuery) error {
			if isOrderOf(q.ClientOrderID, PurposeReduce, LegLong) {
				time.Sleep(20 * time.Millisecond) // slow and ambiguous
				return errNotVisible
			}
			return nil
		}
	})
	res, err := h.exec.Open(context.Background(), h.intent())
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v — the unwind ran out of a budget it shared: %s, %v\n%s", long, short, res.Outcome, err, h.rec.Dump())
	}
	if res.Outcome != OutcomeUnwoundFlat {
		t.Errorf("outcome %s, %v", res.Outcome, err)
	}
}

// Review 4.5k, m3: a leg the venue refuses outright stops the other leg from
// working its marketable limit for the rest of LegTimeout.
func TestOpen_ARefusedLegStopsTheOtherAtOnce(t *testing.T) {
	h := newHarness(t)
	h.cfg.LegTimeout = 3 * time.Second
	h.build(holdsFor{"BTCUSDT", testIntentID}, allowAll{})
	h.binance.SetBehaviour(brokertest.Behaviour{}) // the long rests
	h.bybit.answerAfterOtherOpens(h.binance, errDefiniteRefusal)
	started := time.Now()
	res, err := h.exec.Open(context.Background(), h.intent())
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Open took %s beside a refused leg — the resting long worked on for its whole %s", elapsed, h.cfg.LegTimeout)
	}
	if res.Outcome != OutcomeBothFlat || !errors.Is(err, ErrNotOpened) || res.Long.Status != broker.OrderStatusCanceled {
		t.Fatalf("%s, %v, long %+v", res.Outcome, err, res.Long)
	}
	if n := len(h.binance.sentWith(testIntentID, PurposeOpen, LegLong)); n != 1 {
		t.Errorf("%d long sends — the test must reach the case where the long was sent and is then stopped", n)
	}
}

// A lost send stops the other leg as a refusal does: a resting long is cancelled
// at once instead of working its whole LegTimeout beside a short nobody can see.
func TestOpen_ALostSendStopsTheOtherLegAtOnce(t *testing.T) {
	h := newHarness(t)
	h.cfg.LegTimeout = 3 * time.Second
	h.build(holdsFor{"BTCUSDT", testIntentID}, allowAll{})
	h.binance.SetBehaviour(brokertest.Behaviour{}) // the long rests
	h.bybit.answerAfterOtherOpens(h.binance, errTransport)
	started := time.Now()
	res, err := h.exec.Open(context.Background(), h.intent())
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Open took %s — the resting long worked on beside a lost short", elapsed)
	}
	if res.Long.Status != broker.OrderStatusCanceled || !errors.Is(err, ErrLegAmbiguous) {
		t.Fatalf("%s, %v, long %+v", res.Outcome, err, res.Long)
	}
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Errorf("venues hold %v / %v", long, short)
	}
}

// A position can trail the fill that moved it. The flatten that starts at once
// beside a lost send waits for the long's position to show its known fill, rather
// than read "0", send nothing, and leave the long for the second pass after the
// whole quiet period.
func TestOpen_ALostSendDoesNotMistakeALaggingPositionForFlat(t *testing.T) {
	h := newHarness(t)
	h.cfg.AmbiguousSendQuiet = 800 * time.Millisecond
	h.cfg.PositionSettleTimeout = 300 * time.Millisecond
	h.build(holdsFor{"BTCUSDT", testIntentID}, allowAll{})
	var filled atomic.Bool
	var lagReads atomic.Int64
	var mu sync.Mutex
	var failedAt, flatAt time.Time
	h.binance.set(func(s *scripted) {
		s.placeAfter = func(req broker.PlaceOrderRequest) error {
			if !req.ReduceOnly {
				filled.Store(true)
				return nil
			}
			if p, _ := s.Fake.GetPosition(context.Background(), broker.MarketFuturesUSDM, "BTCUSDT"); p.QtyCoin == 0 {
				mu.Lock()
				if flatAt.IsZero() {
					flatAt = time.Now()
				}
				mu.Unlock()
			}
			return nil
		}
		s.position = func(p broker.Position) (broker.Position, error) {
			if filled.Load() && lagReads.Add(1) <= 3 {
				p.QtyCoin = 0 // the fill has not reached the position yet
			}
			return p, nil
		}
	})
	h.bybit.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			if req.ReduceOnly {
				return nil
			}
			h.binance.awaitOpeningSent()
			mu.Lock()
			failedAt = time.Now()
			mu.Unlock()
			return errTransport
		}
	})
	res, err := h.exec.Open(context.Background(), h.intent())
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v: %s, %v\n%s", long, short, res.Outcome, err, h.rec.Dump())
	}
	mu.Lock()
	window := flatAt.Sub(failedAt)
	mu.Unlock()
	if flatAt.IsZero() || window >= 500*time.Millisecond {
		t.Errorf("the long was flat %s after the send failed — a lagging position was read as flat", window)
	}
}

// gatingRecorder lets a test hold one goroutine at a recorded event until
// another has reached its own.
type gatingRecorder struct {
	inner *execution.MemoryRecorder
	gate  func(ev execution.Event)
}

func (g gatingRecorder) Record(ctx context.Context, ev execution.Event) error {
	g.gate(ev)
	return g.inner.Record(ctx, ev)
}

// Review 4.5k, m-e: a leg refused before the other has been sent keeps the other
// from being sent at all — nothing to unwind, nothing unhedged. The long is held
// at its "placing" event until the short's refusal has been recorded, which
// happens after the short has already told the long to stop.
func TestOpen_ALegRefusedBeforeTheOtherIsSentKeepsItFromBeingSent(t *testing.T) {
	h := newHarness(t)
	h.bybit.SetBehaviour(brokertest.Behaviour{RejectWith: errDefiniteRefusal})
	refused := make(chan struct{})
	var once sync.Once
	rec := gatingRecorder{inner: h.rec, gate: func(ev execution.Event) {
		switch {
		case ev.Kind == execution.EventLegResolved && ev.Leg == LegShort:
			once.Do(func() { close(refused) })
		case ev.Kind == execution.EventLegPlacing && ev.Leg == LegLong:
			select {
			case <-refused:
			case <-time.After(2 * time.Second):
			}
		}
	}}
	x, err := NewExecutor(h.cfg, holdsFor{"BTCUSDT", testIntentID}, allowAll{}, rec)
	if err != nil {
		t.Fatal(err)
	}
	res, err := x.Open(context.Background(), h.intent())
	if res.Outcome != OutcomeBothFlat || !errors.Is(err, ErrNotOpened) {
		t.Fatalf("%s, %v", res.Outcome, err)
	}
	if n := len(h.binance.sent()); n != 0 {
		t.Fatalf("binance received %d orders after bybit had already refused", n)
	}
}

// Review 4.5k, m4: a hedge the orders report while the venues' positions cannot
// be read is said to be unverified.
func TestOpen_AHedgeTheVenuesCouldNotConfirmIsSaidToBeUnverified(t *testing.T) {
	h := newHarness(t)
	var reads atomic.Int64
	h.bybit.set(func(s *scripted) {
		s.position = func(p broker.Position) (broker.Position, error) {
			if reads.Add(1) > 1 { // the pre-trade read succeeds; later reads fail
				return broker.Position{}, errTransport
			}
			return p, nil
		}
	})
	res, err := h.exec.Open(context.Background(), h.intent())
	if res.Outcome != OutcomeBothOpen || !errors.Is(err, ErrPositionsUnverified) || isLoud(err) {
		t.Fatalf("%s, %v", res.Outcome, err)
	}
}

// Review 4.5k, m9: a clock that does not advance cannot keep a loop alive.
func TestOpen_AFrozenClockCannotHangTheMachine(t *testing.T) {
	h := newHarness(t)
	frozen := time.UnixMilli(1_789_000_000_000)
	h.cfg.Now = func() time.Time { return frozen }
	h.build(holdsFor{"BTCUSDT", testIntentID}, allowAll{})
	h.binance.SetBehaviour(brokertest.Behaviour{}) // both rest: every poll loop is reached
	h.bybit.SetBehaviour(brokertest.Behaviour{})
	intent := h.intent()
	intent.Long.Book.SampledAtMs, intent.Short.Book.SampledAtMs = frozen.UnixMilli(), frozen.UnixMilli()
	done := make(chan struct{})
	var res Result
	var err error
	go func() {
		defer close(done)
		res, err = h.exec.Open(context.Background(), intent)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Open did not return under a frozen clock")
	}
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v: %s, %v", long, short, res.Outcome, err)
	}
}

func TestPlanOpen_ABookStampedInTheFutureIsRefused(t *testing.T) {
	h := newHarness(t)
	intent := h.intent()
	intent.Short.Book.SampledAtMs = time.Now().Add(10 * time.Second).UnixMilli()
	if _, err := planOpen(intent, h.cfg); !errors.Is(err, ErrBookStale) {
		t.Fatalf("%v, want ErrBookStale", err)
	}
}

// The orders say one thing and a venue's position another, past the settle
// deadline: nothing more is sent, and it is loud.
func TestOpen_FillsAndPositionsDisagree_NothingMoreIsSent(t *testing.T) {
	h := newHarness(t)
	h.bybit.set(func(s *scripted) {
		s.position = func(p broker.Position) (broker.Position, error) {
			p.QtyCoin = 0 // the venue's position never shows the fill its order reports
			return p, nil
		}
	})
	res, err := h.exec.Open(context.Background(), h.intent())
	if res.Outcome != OutcomeUnresolved || !errors.Is(err, ErrFillEvidenceConflict) || !errors.Is(err, execution.ErrFlatEvidenceConflict) {
		t.Fatalf("%s, %v\n%s", res.Outcome, err, h.rec.Dump())
	}
	for _, s := range []*scripted{h.binance, h.bybit} {
		if n := len(s.sent()); n != 1 {
			t.Errorf("%d sends on a venue, want only the opening order — trading on one of two contradicting numbers is picking one", n)
		}
	}
	if !strings.Contains(err.Error(), "LỆNH") || !strings.Contains(err.Error(), "SÀN") {
		t.Errorf("the error must print BOTH pieces of evidence: %v", err)
	}
}

// The invariant, asserted on the fake VENUES over randomised fills, refusals,
// timeouts on either side of acceptance, and cancels that race fills. With no
// ambiguity injected, every run ends both open (hedged within the coarser step)
// or both flat — never loud, never one leg.
func TestOpen_Property_EveryRunEndsBothOpenOrBothFlat(t *testing.T) {
	rnd := rand.New(rand.NewSource(20260917))
	fills := []float64{0, 0.25, 0.4, 0.5, 0.75, 1}
	knobs := func() brokertest.Behaviour {
		b := brokertest.Behaviour{FillFractionOnPlace: fills[rnd.Intn(len(fills))], CancelRacesAFill: rnd.Intn(3) == 0}
		switch rnd.Intn(5) {
		case 1:
			b.RejectWith = errDefiniteRefusal
		case 2:
			b.PlaceTimesOutAfterAccepting, b.TimeoutTimes = true, 1
		case 3:
			b.PlaceTimesOutBeforeAccepting, b.TimeoutTimes = true, 1
		}
		return b
	}
	counts := map[Outcome]int{}
	for run := 0; run < 150; run++ {
		h := newHarness(t)
		h.cfg.LegTimeout = 8 * time.Millisecond
		h.build(holdsFor{"BTCUSDT", testIntentID}, allowAll{})
		longKnobs, shortKnobs := knobs(), knobs()
		h.binance.SetBehaviour(longKnobs)
		h.bybit.SetBehaviour(shortKnobs)

		res, err := h.exec.Open(context.Background(), h.intent())
		counts[res.Outcome]++
		long, short := h.positions()
		where := fmt.Sprintf("run %d (long %+v, short %+v): %s, %v — venues %v / %v\n%s", run, longKnobs, shortKnobs, res.Outcome, err, long, short, h.rec.Dump())
		if errors.Is(err, execution.ErrUnwindIncomplete) || errors.Is(err, execution.ErrFlatEvidenceConflict) {
			// The one loud result the knobs can produce: an opening send that never
			// reached the venue, which no answer can prove absent (review 4.5k, N2).
			// It must still leave both venues flat.
			neverArrived := longKnobs.PlaceTimesOutBeforeAccepting || shortKnobs.PlaceTimesOutBeforeAccepting
			if !errors.Is(err, ErrLegAmbiguous) || !neverArrived || long != 0 || short != 0 || res.Outcome == OutcomeUnresolved {
				t.Fatalf("a loud error the knobs do not explain: %s", where)
			}
			counts["loud_flat"]++
		}
		switch {
		case long == 0 && short == 0:
			if res.Outcome != OutcomeBothFlat && res.Outcome != OutcomeUnwoundFlat {
				t.Fatalf("flat venues reported as %s: %s", res.Outcome, where)
			}
		case long > 0 && short < 0:
			residual := math.Abs(long + short)
			if res.Outcome != OutcomeBothOpen || residual > 0.001+1e-9 || (residual > 0 && residual*testMid >= 5) {
				t.Fatalf("an open pair that is not a hedge: residual %v: %s", residual, where)
			}
		default:
			t.Fatalf("ONE LEG, or a wrong-way position: %s", where)
		}
	}
	t.Logf("outcomes over 150 runs: %v", counts)
	if counts[OutcomeBothOpen] == 0 || counts[OutcomeUnwoundFlat] == 0 {
		t.Errorf("the knob space never reached both outcomes: %v", counts)
	}
}

func TestClientOrderID_DeterministicDistinctAndLegalOnBothVenues(t *testing.T) {
	seen := map[string]string{}
	for _, intent := range []string{"a", "b", "cp-intent-0001", strings.Repeat("x", 200)} {
		for _, purpose := range []Purpose{PurposeOpen, PurposeReduce, PurposeUnwind, PurposeClose} {
			for _, leg := range []execution.LegName{LegLong, LegShort} {
				for round := 0; round <= 5; round++ {
					attempt := []string{"", "a1b2c3d4e5f6", "0f0e0d0c0b0a"}[round%3]
					id := ClientOrderID(intent, purpose, leg, attempt, round)
					if id != ClientOrderID(intent, purpose, leg, attempt, round) {
						t.Fatal("not deterministic")
					}
					key := fmt.Sprintf("%s|%s|%s|%s|%d", intent, purpose, leg, attempt, round)
					if prev, dup := seen[id]; dup {
						t.Fatalf("%s and %s collide on %s", prev, key, id)
					}
					seen[id] = key
					if len(id) > 36 {
						t.Fatalf("%s is %d characters", id, len(id))
					}
					for _, r := range id {
						if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z') {
							t.Fatalf("%s carries %q", id, r)
						}
					}
					if id == execution.LegClientOrderID(intent, execution.LegPerp) || id == execution.LegClientOrderID(intent, execution.LegSpot) {
						t.Fatalf("%s collides with Engine 1's id space", id)
					}
				}
			}
		}
	}
}

// definiteRejection must NOT read a timeout that reached the venue as a refusal.
func TestDefiniteRejection_A408AndMinus1007AreAmbiguous(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"validation", fmt.Errorf("%w: x", broker.ErrInvalidOrder), true},
		{"HTTP 400", &broker.HTTPError{StatusCode: 400}, true},
		{"HTTP 403 WAF", &broker.HTTPError{StatusCode: 403}, true},
		{"HTTP 408 backend timeout", &broker.HTTPError{StatusCode: 408}, false},
		{"HTTP 503", &broker.HTTPError{StatusCode: 503}, false},
		// Each Binance code rides WITH a 4xx HTTPError, so the carve-out is what
		// decides — without it the 4xx would read as a refusal (review 4.5k, m1).
		{"Binance -1007 on a 4xx", fmt.Errorf("%w; %w", &binance.VenueError{Code: -1007, StatusCode: 400}, &broker.HTTPError{StatusCode: 400}), false},
		{"Binance -1006 on a 4xx", fmt.Errorf("%w; %w", &binance.VenueError{Code: -1006, StatusCode: 400}, &broker.HTTPError{StatusCode: 400}), false},
		{"Binance -1000 on a 4xx", fmt.Errorf("%w; %w", &binance.VenueError{Code: -1000, StatusCode: 400}, &broker.HTTPError{StatusCode: 400}), false},
		{"Binance -4116 duplicate id on a 4xx", fmt.Errorf("%w: %w", binance.ErrDuplicateClientOrderID, &broker.HTTPError{StatusCode: 400}), false},
		{"Bybit 110072 duplicate id", fmt.Errorf("%w: %w", bybit.ErrDuplicateClientOrderID, broker.ErrInvalidOrder), false},
		{"transport", errTransport, false},
	}
	for _, c := range cases {
		if got := definiteRejection(c.err); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// Review 4.5k round 3, B1: the long's opening send loses its answer, the cancel
// is answered "unknown order", and the order then reaches the venue and RESTS.
// Read back as working, it is cancelled again — never watched until the asking
// ends and left behind.
func TestOpen_ALostOpeningOrderThatArrivesAndRestsIsCancelled(t *testing.T) {
	h := newHarness(t)
	h.cfg.AmbiguousSendQuiet = 150 * time.Millisecond
	h.build(holdsFor{"BTCUSDT", testIntentID}, allowAll{})
	openLong := ClientOrderID(testIntentID, PurposeOpen, LegLong, "", 0)
	late := make(chan struct{})
	h.binance.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			if req.ClientOrderID != openLong {
				return nil
			}
			h.bybit.awaitOpeningSent()
			go func() {
				defer close(late)
				time.Sleep(30 * time.Millisecond)
				s.Fake.SetBehaviour(brokertest.Behaviour{}) // it rests
				if _, err := s.Fake.PlaceOrder(context.Background(), req); err != nil {
					t.Errorf("late original: %v", err)
				}
			}()
			return errTransport
		}
	})
	res, err := h.exec.Open(context.Background(), h.intent())
	<-late
	o, gerr := h.binance.Fake.GetOrder(context.Background(), broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", ClientOrderID: openLong})
	if gerr != nil || o.Status != broker.OrderStatusCanceled {
		t.Fatalf("the late long is %s (%v) after Open returned %s, %v — want CANCELED", o.Status, gerr, res.Outcome, err)
	}
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v", long, short)
	}
	if isLoud(err) || !res.Long.OrderConfirmed {
		t.Errorf("%s, %v — the order was found and proven finished, nothing is left to be loud about", res.Outcome, err)
	}
}

// Review 4.5k round 3, M2: the short's opening send is lost, the order fills 60
// ms later while every lookup keeps failing, and the long is already flat. The
// unconfirmed leg is watched while it is asked about, so the late short is taken
// back at once — not when the 1.5 s of asking ends.
func TestOpen_ALostOrderThatFillsWhileBeingAskedAboutIsTakenBackAtOnce(t *testing.T) {
	h := newHarness(t)
	h.cfg.AmbiguousSendQuiet = 1500 * time.Millisecond
	h.cfg.PollEvery = 5 * time.Millisecond
	h.build(holdsFor{"BTCUSDT", testIntentID}, allowAll{})
	openShort := ClientOrderID(testIntentID, PurposeOpen, LegShort, "", 0)
	var mu sync.Mutex
	var filledAt, flatAt time.Time
	h.bybit.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			if req.ClientOrderID != openShort {
				return nil
			}
			h.binance.awaitOpeningSent()
			go func() {
				time.Sleep(60 * time.Millisecond)
				if _, err := s.Fake.PlaceOrder(context.Background(), req); err != nil {
					t.Errorf("late original: %v", err)
				}
				mu.Lock()
				filledAt = time.Now()
				mu.Unlock()
			}()
			return errTransport
		}
		s.getOrder = func(q broker.OrderQuery) error {
			if q.ClientOrderID == openShort {
				return errTransport
			}
			return nil
		}
		s.placeAfter = func(req broker.PlaceOrderRequest) error {
			if req.ReduceOnly {
				if p, _ := s.Fake.GetPosition(context.Background(), broker.MarketFuturesUSDM, "BTCUSDT"); p.QtyCoin == 0 {
					mu.Lock()
					if flatAt.IsZero() {
						flatAt = time.Now()
					}
					mu.Unlock()
				}
			}
			return nil
		}
	})
	res, err := h.exec.Open(context.Background(), h.intent())
	mu.Lock()
	window := flatAt.Sub(filledAt)
	mu.Unlock()
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v: %s, %v", long, short, res.Outcome, err)
	}
	if flatAt.IsZero() || window <= 0 || window >= 500*time.Millisecond {
		t.Errorf("the late short was naked %s beside a flat long — want under 500 ms", window)
	}
	if !errors.Is(err, ErrLegAmbiguous) {
		t.Errorf("%v — the order was never read back, so the result stays loud", err)
	}
}

// Review 4.5k round 3, M3: the long is refused; the short filled 40% and rests,
// and its venue cancels asynchronously, 450 ms later. The filled part is taken
// back at once, while the cancel is still being read back.
func TestOpen_ALegStoppedByARefusedPartnerIsFlattenedBeforeItsCancelIsReadBack(t *testing.T) {
	h := newHarness(t)
	h.cfg.PollEvery = 50 * time.Millisecond
	h.cfg.OrderSettleTimeout = 2 * time.Second
	h.cfg.LegTimeout = 3 * time.Second
	h.build(holdsFor{"BTCUSDT", testIntentID}, allowAll{})
	h.bybit.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.4})
	var mu sync.Mutex
	var refusedAt, flatAt time.Time
	h.binance.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			if req.ReduceOnly {
				return nil
			}
			h.bybit.awaitOpeningSent()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			refusedAt = time.Now()
			mu.Unlock()
			return errDefiniteRefusal
		}
	})
	var once sync.Once
	h.bybit.set(func(s *scripted) {
		s.cancelAck = func(q broker.OrderQuery) {
			once.Do(func() {
				go func() {
					time.Sleep(450 * time.Millisecond)
					_, _ = s.Fake.CancelOrder(context.Background(), q)
				}()
			})
		}
		s.placeAfter = func(req broker.PlaceOrderRequest) error {
			if req.ReduceOnly {
				if p, _ := s.Fake.GetPosition(context.Background(), broker.MarketFuturesUSDM, "BTCUSDT"); p.QtyCoin == 0 {
					mu.Lock()
					if flatAt.IsZero() {
						flatAt = time.Now()
					}
					mu.Unlock()
				}
			}
			return nil
		}
	})
	res, err := h.exec.Open(context.Background(), h.intent())
	mu.Lock()
	window := flatAt.Sub(refusedAt)
	mu.Unlock()
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v: %s, %v", long, short, res.Outcome, err)
	}
	if flatAt.IsZero() || window >= 300*time.Millisecond {
		t.Errorf("the short's filled 40%% was flat %s after the long was refused — it waited for its own cancel", window)
	}
	if res.Outcome != OutcomeUnwoundFlat || isLoud(err) || res.Short.Status != broker.OrderStatusCanceled {
		t.Errorf("%s, %v, short %+v — want unwound_flat, not loud, the rest CANCELED", res.Outcome, err, res.Short)
	}
}

// Review 4.5k round 3, M4: a still-working opening SELL fills 0.1 more right
// after the first reduce-only BUY of the flatten. The wait for "held − filled"
// can no longer be met; the flatten sees that the position MOVED and re-sizes at
// once instead of waiting out PositionSettleTimeout beside a naked 0.1.
func TestOpen_AnOpeningFillDuringTheFlattenDoesNotStallIt(t *testing.T) {
	h := newHarness(t)
	h.cfg.PositionSettleTimeout = 2 * time.Second
	h.cfg.OrderSettleTimeout = 100 * time.Millisecond
	h.cfg.AmbiguousSendQuiet = 100 * time.Millisecond
	h.cfg.PollEvery = 5 * time.Millisecond
	h.build(holdsFor{"BTCUSDT", testIntentID}, allowAll{})
	openShort := ClientOrderID(testIntentID, PurposeOpen, LegShort, "", 0)
	openLong := ClientOrderID(testIntentID, PurposeOpen, LegLong, "", 0)
	h.bybit.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.4})
	h.binance.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			if req.ClientOrderID == openLong {
				h.bybit.awaitOpeningSent()
				time.Sleep(5 * time.Millisecond)
				return errTransport // lost, never arrives
			}
			return nil
		}
	})
	var mu sync.Mutex
	var extraAt, flatAt time.Time
	var once sync.Once
	h.bybit.set(func(s *scripted) {
		s.cancelAck = func(q broker.OrderQuery) {} // acknowledged, never completes
		s.placeAfter = func(req broker.PlaceOrderRequest) error {
			if !req.ReduceOnly {
				return nil
			}
			once.Do(func() {
				if err := s.Fake.Fill(openShort, 0.1, testMid); err != nil {
					t.Errorf("extra fill: %v", err)
				}
				mu.Lock()
				extraAt = time.Now()
				mu.Unlock()
			})
			if p, _ := s.Fake.GetPosition(context.Background(), broker.MarketFuturesUSDM, "BTCUSDT"); p.QtyCoin == 0 {
				mu.Lock()
				if flatAt.IsZero() {
					flatAt = time.Now()
				}
				mu.Unlock()
			}
			return nil
		}
	})
	res, err := h.exec.Open(context.Background(), h.intent())
	mu.Lock()
	window := flatAt.Sub(extraAt)
	mu.Unlock()
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v: %s, %v", long, short, res.Outcome, err)
	}
	if flatAt.IsZero() || window >= 500*time.Millisecond {
		t.Errorf("the extra 0.1 was naked %s — the flatten waited for a position the concurrent fill made unreachable", window)
	}
}

// The last flatten after the asking, under ids of its own: the short's opening
// order was found RESTING, its cancels were only ever acknowledged, and a fill
// races the final cancel sent once the asking is over — after the watch on that
// leg has stopped. Only the last pass can take that fill back.
func TestOpen_TheLastPassTakesBackAFillThatRacedTheFinalCancel(t *testing.T) {
	h := newHarness(t)
	openShort := ClientOrderID(testIntentID, PurposeOpen, LegShort, "", 0)
	h.bybit.SetBehaviour(brokertest.Behaviour{}) // the short rests
	var askingOver atomic.Bool
	rec := gatingRecorder{inner: h.rec, gate: func(ev execution.Event) {
		if ev.Kind == execution.EventLegReadBack && ev.Leg == LegShort && ev.Err != nil {
			askingOver.Store(true) // confirmLeg gave up on the short
		}
	}}
	x, err := NewExecutor(h.cfg, holdsFor{"BTCUSDT", testIntentID}, allowAll{}, rec)
	if err != nil {
		t.Fatal(err)
	}
	h.bybit.set(func(s *scripted) {
		s.placeAfter = func(req broker.PlaceOrderRequest) error {
			if req.ClientOrderID == openShort {
				h.binance.awaitOpeningSent()
				return errTransport // recorded and resting, answer lost
			}
			return nil
		}
		s.cancelAck = func(q broker.OrderQuery) {
			if q.ClientOrderID != openShort || !askingOver.Load() {
				return // acknowledged, never completed
			}
			if o, gerr := s.Fake.GetOrder(context.Background(), q); gerr == nil && !o.Status.Done() {
				if ferr := s.Fake.Fill(openShort, o.RemainingQtyCoin(), testMid); ferr != nil {
					t.Errorf("racing fill: %v", ferr)
				}
			}
		}
	})
	res, openErr := x.Open(context.Background(), h.intent())
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v: %s, %v\n%s", long, short, res.Outcome, openErr, h.rec.Dump())
	}
	if !askingOver.Load() || len(h.bybit.sentWith(testIntentID, PurposeUnwind, LegShort)) == 0 {
		t.Errorf("the scenario was not reached: asking over %v, short unwinds %d", askingOver.Load(), len(h.bybit.sentWith(testIntentID, PurposeUnwind, LegShort)))
	}
}
