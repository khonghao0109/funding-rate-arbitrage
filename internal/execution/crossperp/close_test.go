package crossperp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/broker/brokertest"
	"futures-arbitrage-scanner/internal/execution"
)

func (h *harness) hold(longQtyCoin, shortQtyCoin float64) {
	h.binance.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", QtyCoin: longQtyCoin})
	h.bybit.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", QtyCoin: shortQtyCoin})
}

func (h *harness) closeRequest() CloseRequest {
	intent := h.intent()
	return CloseRequest{IntentID: testIntentID, Symbol: "BTCUSDT", Long: intent.Long, Short: intent.Short, ReasonVI: "test"}
}

// Both legs close ONE AFTER THE OTHER (review 4.5k, M2), each with one
// reduce-only MARKET order sized from its venue's own position — the long by
// SELLING on its venue, the short by BUYING on its own (the design's text has the
// two sides the other way round).
func TestClose_BothLegsOneAfterTheOther_BothFlat(t *testing.T) {
	h := newHarness(t)
	h.hold(0.333, -0.333)
	res, err := h.exec.Close(context.Background(), h.closeRequest())
	if err != nil || res.Outcome != OutcomeBothFlat {
		t.Fatalf("%s, %v\n%s", res.Outcome, err, h.rec.Dump())
	}
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v", long, short)
	}
	lc := h.binance.sentWith(testIntentID, PurposeClose, LegLong)
	sc := h.bybit.sentWith(testIntentID, PurposeClose, LegShort)
	if len(lc) != 1 || lc[0].Side != broker.SideSell || !lc[0].ReduceOnly || lc[0].Type != broker.OrderTypeMarket || lc[0].QtyCoin != 0.333 {
		t.Errorf("long close %+v", lc)
	}
	if len(sc) != 1 || sc[0].Side != broker.SideBuy || !sc[0].ReduceOnly || sc[0].Type != broker.OrderTypeMarket || sc[0].QtyCoin != 0.333 {
		t.Errorf("short close %+v", sc)
	}
	if res.LongBeforeQtyCoin != 0.333 || res.ShortBeforeQtyCoin != -0.333 || !res.Long.VenuePositionRead || res.Long.VenuePositionQtyCoin != 0 {
		t.Errorf("result %+v", res)
	}
	if res.FirstLeg != LegLong {
		t.Errorf("first leg %s, want long on a tie", res.FirstLeg)
	}
}

// A closing MARKET order that fills half is followed by a second round, sized
// again from the venue, under a new id.
func TestClose_APartialFillTakesASecondRound(t *testing.T) {
	h := newHarness(t)
	h.hold(0.333, -0.333)
	h.binance.SetBehaviour(brokertest.Behaviour{MarketFillFraction: 0.5})
	h.binance.set(func(s *scripted) {
		s.placeAfter = func(broker.PlaceOrderRequest) error {
			s.Fake.SetBehaviour(brokertest.Behaviour{}) // later rounds fill whole
			return nil
		}
	})
	res, err := h.exec.Close(context.Background(), h.closeRequest())
	if err != nil || res.Outcome != OutcomeBothFlat || res.Rounds != 2 {
		t.Fatalf("%s rounds=%d, %v\n%s", res.Outcome, res.Rounds, err, h.rec.Dump())
	}
	rounds := h.binance.sentWith(testIntentID, PurposeClose, LegLong)
	if len(rounds) != 2 || rounds[0].QtyCoin != 0.333 || rounds[1].QtyCoin != 0.1665 || rounds[0].ClientOrderID == rounds[1].ClientOrderID {
		t.Fatalf("close rounds %+v — want 0.333 then the venue's remaining 0.1665 under a new id", rounds)
	}
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v", long, short)
	}
}

func TestClose_RefusesBeforeSending(t *testing.T) {
	t.Run("already flat sends nothing and is not an error", func(t *testing.T) {
		h := newHarness(t)
		res, err := h.exec.Close(context.Background(), h.closeRequest())
		if err != nil || res.Outcome != OutcomeBothFlat || !res.AlreadyFlat || len(h.binance.sent())+len(h.bybit.sent()) != 0 {
			t.Fatalf("%+v, %v", res, err)
		}
	})
	t.Run("a wrong-way position", func(t *testing.T) {
		h := newHarness(t)
		h.hold(-0.1, 0.1)
		res, err := h.exec.Close(context.Background(), h.closeRequest())
		if !errors.Is(err, ErrPositionDisagrees) || len(h.binance.sent())+len(h.bybit.sent()) != 0 {
			t.Fatalf("%v, sends %v %v", err, h.binance.sent(), h.bybit.sent())
		}
		if res.Outcome != OutcomeUnresolved {
			t.Errorf("outcome %q — the venues were read and are no pair of this intent", res.Outcome)
		}
	})
	t.Run("a venue that cannot be read", func(t *testing.T) {
		h := newHarness(t)
		h.hold(0.333, -0.333)
		h.bybit.set(func(s *scripted) {
			s.position = func(broker.Position) (broker.Position, error) { return broker.Position{}, errTransport }
		})
		res, err := h.exec.Close(context.Background(), h.closeRequest())
		if !errors.Is(err, ErrCloseRefused) || len(h.binance.sent())+len(h.bybit.sent()) != 0 {
			t.Fatalf("%v", err)
		}
		if res.Outcome != "" {
			t.Errorf("outcome %q for a close refused before it read the venues — want empty, not a reading (review 4.5k round 3, minor 6)", res.Outcome)
		}
	})
	t.Run("without the lock", func(t *testing.T) {
		h := newHarness(t)
		h.hold(0.333, -0.333)
		h.build(holdsFor{"BTCUSDT", "someone-else"}, allowAll{})
		if _, err := h.exec.Close(context.Background(), h.closeRequest()); !errors.Is(err, ErrCloseRefused) || len(h.binance.sent())+len(h.bybit.sent()) != 0 {
			t.Fatalf("%v", err)
		}
	})
}

// One venue refuses every round. Closed SECOND, it leaves one leg — named, with
// the venue's own number, loud, and never re-hedged. Closed FIRST, it leaves the
// pair exactly as it was: nothing is sent on the other venue, and the result is a
// hedge still to close, not a naked leg (review 4.5k, M2).
func TestClose_AVenueThatRefusesEveryRound(t *testing.T) {
	t.Run("closed second: one leg, loud, never re-hedged", func(t *testing.T) {
		h := newHarness(t)
		h.hold(0.333, -0.333)
		h.bybit.SetBehaviour(brokertest.Behaviour{RejectWith: errDefiniteRefusal})
		res, err := h.exec.Close(context.Background(), h.closeRequest())
		if res.Outcome != OutcomeUnresolved || !errors.Is(err, execution.ErrUnwindIncomplete) {
			t.Fatalf("%s, %v\n%s", res.Outcome, err, h.rec.Dump())
		}
		if long, short := h.positions(); long != 0 || short != -0.333 {
			t.Fatalf("venues hold %v / %v", long, short)
		}
		for _, r := range h.binance.sent() {
			if !r.ReduceOnly || r.Side != broker.SideSell {
				t.Errorf("binance received %+v — a close never opens anything to re-hedge", r)
			}
		}
		if n := len(h.bybit.sentWith(testIntentID, PurposeClose, LegShort)); n != h.cfg.MaxCloseRounds {
			t.Errorf("bybit close attempts %d, want MaxCloseRounds %d", n, h.cfg.MaxCloseRounds)
		}
	})
	t.Run("closed first: the pair stays whole", func(t *testing.T) {
		h := newHarness(t)
		h.hold(0.333, -0.333)
		h.bybit.SetBehaviour(brokertest.Behaviour{RejectWith: errDefiniteRefusal})
		req := h.closeRequest()
		req.FirstVenue = venueBybit
		res, err := h.exec.Close(context.Background(), req)
		if res.Outcome != OutcomeBothOpen || !errors.Is(err, ErrCloseIncomplete) || isLoud(err) {
			t.Fatalf("%s, %v\n%s", res.Outcome, err, h.rec.Dump())
		}
		if long, short := h.positions(); long != 0.333 || short != -0.333 {
			t.Fatalf("venues hold %v / %v — the pair must be untouched", long, short)
		}
		if n := len(h.binance.sent()); n != 0 {
			t.Errorf("binance received %d orders while the first leg had closed nothing", n)
		}
	})
}

// Review 4.5k, B1: a second Close of the same intent sends NEW orders. The first
// close's reduce-only IOC orders were accepted and expired unfilled on a thin
// book; a venue refuses a reused client id (Bybit: orderLinkId "always unique"),
// so a close that reused them could never send anything again.
func TestClose_ASecondCloseSendsNewOrders(t *testing.T) {
	h := newHarness(t)
	h.hold(0.333, -0.333)
	h.bybit.SetBehaviour(brokertest.Behaviour{MarketFillFraction: 0.0001}) // floors to 0 on a 0.001 grid: EXPIRED, recorded
	req := h.closeRequest()
	req.FirstVenue = venueBybit
	first, err := h.exec.Close(context.Background(), req)
	if first.Outcome != OutcomeBothOpen || !errors.Is(err, ErrCloseIncomplete) {
		t.Fatalf("first close: %s, %v", first.Outcome, err)
	}
	firstIDs := map[string]bool{}
	for _, o := range h.bybit.Orders() {
		firstIDs[o.ClientOrderID] = true
	}

	h.bybit.SetBehaviour(brokertest.Behaviour{}) // the book is healthy again
	second, err := h.exec.Close(context.Background(), req)
	if err != nil || second.Outcome != OutcomeBothFlat {
		t.Fatalf("second close: %s, %v\n%s", second.Outcome, err, h.rec.Dump())
	}
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v", long, short)
	}
	if first.AttemptNonce == second.AttemptNonce {
		t.Fatal("two closes share an attempt nonce")
	}
	for _, r := range h.bybit.sentWith(testIntentID, PurposeClose, LegShort)[len(firstIDs):] {
		if firstIDs[r.ClientOrderID] {
			t.Errorf("the second close reused %s", r.ClientOrderID)
		}
	}
}

// flattenPair touches the second leg only when the first leg's POSITION was read;
// a first leg with an order that may still execute is taken as it reads, because
// that order can only shrink it (review 4.5k, N5).
func TestClose_TheSecondLegWaitsUntilTheFirstLegsSizeIsKnown(t *testing.T) {
	t.Run("the first venue cannot be read", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.UnwindTimeout = 200 * time.Millisecond
		h.build(holdsFor{"BTCUSDT", testIntentID}, allowAll{})
		h.hold(0.333, -0.333)
		var reads atomic.Int64
		h.bybit.set(func(s *scripted) {
			s.position = func(p broker.Position) (broker.Position, error) {
				if reads.Add(1) > 1 { // Close's own first read succeeds; the drive's fail
					return broker.Position{}, errTransport
				}
				return p, nil
			}
		})
		req := h.closeRequest()
		req.FirstVenue = venueBybit
		res, err := h.exec.Close(context.Background(), req)
		if !errors.Is(err, execution.ErrUnwindIncomplete) {
			t.Fatalf("%s, %v", res.Outcome, err)
		}
		if n := len(h.binance.sent()); n != 0 {
			t.Fatalf("binance received %d orders while the first leg's size was unknown", n)
		}
		if long, short := h.positions(); long != 0.333 || short != -0.333 {
			t.Fatalf("venues hold %v / %v", long, short)
		}
	})
	// The first leg closes half, then sends an order whose answer is lost, then
	// meets an IP ban. That order may still execute — but it is reduce-only, so
	// the first leg can only shrink further. The second leg is cut to what the
	// first leg READS (0.167), never below it: leaving the second leg whole would
	// leave the whole difference naked, while this leaves at most the lost order's
	// own size. And two equal legs beside that order are not reported as a hedge.
	t.Run("the first leg has an order that may still execute", func(t *testing.T) {
		h := newHarness(t)
		h.hold(0.333, -0.333)
		h.bybit.SetBehaviour(brokertest.Behaviour{MarketFillFraction: 0.5})
		var mu sync.Mutex
		sends, lostID := 0, ""
		h.bybit.set(func(s *scripted) {
			s.placeBefore = func(req broker.PlaceOrderRequest) error {
				mu.Lock()
				defer mu.Unlock()
				sends++
				switch sends {
				case 1:
					return nil // fills half
				case 2:
					lostID = req.ClientOrderID
					return errTransport
				}
				return fmt.Errorf("%w: test", broker.ErrIPCoolingDown)
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
		req := h.closeRequest()
		req.FirstVenue = venueBybit
		res, err := h.exec.Close(context.Background(), req)
		if res.Outcome != OutcomeUnresolved || !errors.Is(err, ErrLegAmbiguous) || errors.Is(err, ErrCloseIncomplete) {
			t.Fatalf("%s, %v — two equal legs beside an order that may still execute are not a hedge\n%s", res.Outcome, err, h.rec.Dump())
		}
		// 0.333 × 0.5 floors onto Bybit's 0.001 grid: 0.166 closed, 0.167 left.
		long, short := h.positions()
		if short != -0.167 || long != 0.167 {
			t.Fatalf("venues hold %v / %v — want the long cut to the short's reading, +0.167 / −0.167", long, short)
		}
		for _, r := range h.binance.sent() {
			if !r.ReduceOnly || r.Side != broker.SideSell {
				t.Errorf("binance received %+v", r)
			}
		}
	})
}

// Review 4.5k, N1: every reduce-only order of the FIRST leg is lost on the wire
// and stays invisible, so both legs still read the same size. That is not "still
// hedged, close again later": any of those orders may execute after Close returns
// and leave the second leg naked. Loud, and never ErrCloseIncomplete.
func TestClose_EqualLegsBesideUnresolvedReductionsAreLoud(t *testing.T) {
	h := newHarness(t)
	h.hold(0.333, -0.333)
	var mu sync.Mutex
	lost := map[string]bool{}
	h.binance.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			mu.Lock()
			defer mu.Unlock()
			lost[req.ClientOrderID] = true
			return errTransport
		}
		s.getOrder = func(q broker.OrderQuery) error {
			mu.Lock()
			defer mu.Unlock()
			if lost[q.ClientOrderID] {
				return errNotVisible
			}
			return nil
		}
	})
	res, err := h.exec.Close(context.Background(), h.closeRequest()) // a tie closes the long (binance) first
	if res.Outcome != OutcomeUnresolved || !errors.Is(err, ErrLegAmbiguous) || errors.Is(err, ErrCloseIncomplete) {
		t.Fatalf("%s, %v — want unresolved and loud\n%s", res.Outcome, err, h.rec.Dump())
	}
	if long, short := h.positions(); long != 0.333 || short != -0.333 {
		t.Fatalf("venues hold %v / %v", long, short)
	}
	if n := len(h.bybit.sent()); n != 0 {
		t.Errorf("bybit received %d orders — the long never moved, so there was nothing to match", n)
	}
}

// Review 4.5k, N3: a pair whose opening orders are unproven. Close cancels them
// by their derived ids and reads them back BEFORE it reads what to close; flat
// venues are a closed pair only when every order that could open something is
// proven finished and nothing rests on the symbol.
func TestClose_OpeningOrdersAreSettledFirstAndProvenAtTheEnd(t *testing.T) {
	openingOn := func(t *testing.T, s *scripted, leg execution.LegName, side broker.Side, fill float64) {
		t.Helper()
		s.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: fill})
		if _, err := s.Fake.PlaceOrder(context.Background(), broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT",
			Side: side, Type: broker.OrderTypeLimitGTC, ClientOrderID: ClientOrderID(testIntentID, PurposeOpen, leg, "", 0),
			QtyCoin: 0.333, PriceQuote: testMid}); err != nil {
			t.Fatal(err)
		}
		s.SetBehaviour(brokertest.Behaviour{})
	}
	t.Run("a resting opening order is cancelled before the close and the pair ends flat", func(t *testing.T) {
		h := newHarness(t)
		openingOn(t, h.binance, LegLong, broker.SideBuy, 1)   // filled: +0.333
		openingOn(t, h.bybit, LegShort, broker.SideSell, 0.5) // half filled, the rest RESTING
		h.bybit.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", QtyCoin: -0.333})
		req := h.closeRequest()
		req.OpeningOrdersUnproven = true
		res, err := h.exec.Close(context.Background(), req)
		if err != nil || res.Outcome != OutcomeBothFlat || !res.OpeningOrdersProven || res.RestingOrders != 0 {
			t.Fatalf("%+v, %v\n%s", res, err, h.rec.Dump())
		}
		o, _ := h.bybit.Fake.GetOrder(context.Background(), broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT",
			ClientOrderID: ClientOrderID(testIntentID, PurposeOpen, LegShort, "", 0)})
		if o.Status != broker.OrderStatusCanceled {
			t.Errorf("the resting opening order is %s, want CANCELED", o.Status)
		}
		if long, short := h.positions(); long != 0 || short != 0 {
			t.Errorf("venues hold %v / %v", long, short)
		}
	})
	// The cancel must come BEFORE the reductions: a resting opening BUY on the
	// first leg's venue that fills after that leg was taken to zero — here, as the
	// second leg sends its first reduction — reopens a leg the close has already
	// sized the other one against.
	t.Run("a resting opening order is cancelled before the first reduction, not after", func(t *testing.T) {
		h := newHarness(t)
		openingOn(t, h.binance, LegLong, broker.SideBuy, 0.5) // half filled, the rest RESTING
		h.hold(0.333, -0.333)
		openLong := ClientOrderID(testIntentID, PurposeOpen, LegLong, "", 0)
		h.bybit.set(func(s *scripted) {
			s.placeBefore = func(req broker.PlaceOrderRequest) error {
				if !req.ReduceOnly {
					return nil
				}
				o, err := h.binance.Fake.GetOrder(context.Background(), broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", ClientOrderID: openLong})
				if err == nil && !o.Status.Done() {
					if ferr := h.binance.Fake.Fill(openLong, o.RemainingQtyCoin(), testMid); ferr != nil {
						t.Errorf("fill: %v", ferr)
					}
				}
				return nil
			}
		})
		// Bybit's opening order must be findable too, or the verdict stays loud.
		openingOn(t, h.bybit, LegShort, broker.SideSell, 1)
		h.bybit.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", QtyCoin: -0.333})
		req := h.closeRequest()
		req.OpeningOrdersUnproven = true
		res, err := h.exec.Close(context.Background(), req)
		if err != nil || res.Outcome != OutcomeBothFlat {
			t.Fatalf("%s, %v\n%s", res.Outcome, err, h.rec.Dump())
		}
		if long, short := h.positions(); long != 0 || short != 0 {
			t.Fatalf("venues hold %v / %v", long, short)
		}
		// A close that closes a late opening fill after the fact (round 3, M5)
		// still ends flat; what cancelling first buys is that there was none.
		o, _ := h.binance.Fake.GetOrder(context.Background(), broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", ClientOrderID: openLong})
		if o.FilledQtyCoin != 0.1665 || o.Status != broker.OrderStatusCanceled {
			t.Errorf("the opening BUY ended %s after %v filled — it kept working into the close", o.Status, o.FilledQtyCoin)
		}
	})
	t.Run("an opening order no venue answer can prove is loud however flat the venues read", func(t *testing.T) {
		h := newHarness(t)
		h.bybit.set(func(s *scripted) {
			s.getOrder = func(broker.OrderQuery) error { return errNotVisible }
			s.cancel = func(broker.OrderQuery) error { return errNotVisible }
		})
		req := h.closeRequest()
		req.OpeningOrdersUnproven = true
		res, err := h.exec.Close(context.Background(), req)
		if res.Outcome != OutcomeUnresolved || !errors.Is(err, ErrLegAmbiguous) || res.OpeningOrdersProven || !res.AlreadyFlat {
			t.Fatalf("%+v, %v", res, err)
		}
		if n := len(h.binance.sent()) + len(h.bybit.sent()); n != 0 {
			t.Errorf("%d orders sent on flat venues", n)
		}
	})
	t.Run("an order resting on the symbol keeps flat venues from being a closed pair", func(t *testing.T) {
		h := newHarness(t)
		h.bybit.SetBehaviour(brokertest.Behaviour{}) // rests
		if _, err := h.bybit.Fake.PlaceOrder(context.Background(), broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT",
			Side: broker.SideSell, Type: broker.OrderTypeLimitGTC, ClientOrderID: "someone-else", QtyCoin: 0.01, PriceQuote: 70_000}); err != nil {
			t.Fatal(err)
		}
		res, err := h.exec.Close(context.Background(), h.closeRequest())
		if res.Outcome != OutcomeUnresolved || !errors.Is(err, ErrLegAmbiguous) || res.RestingOrders != 1 {
			t.Fatalf("%+v, %v", res, err)
		}
	})
}

// Review 4.5k, m-j: closing one leg after the other leaves the pair unhedged
// from the first leg's first reducing fill to the second leg's last. It is
// measured, not assumed away: a second venue 30 ms slower shows it.
func TestClose_TheUnhedgedWindowOfTheSequentialCloseIsMeasured(t *testing.T) {
	h := newHarness(t)
	h.hold(0.333, -0.333)
	h.bybit.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			time.Sleep(30 * time.Millisecond)
			return nil
		}
	})
	res, err := h.exec.Close(context.Background(), h.closeRequest()) // long (binance) first
	if err != nil || res.Outcome != OutcomeBothFlat {
		t.Fatalf("%s, %v", res.Outcome, err)
	}
	// The window is stamped in whole milliseconds, the duration in nanoseconds.
	if res.UnhedgedWindow < 30*time.Millisecond || res.UnhedgedWindow > res.Duration+time.Millisecond {
		t.Errorf("unhedged window %s (close took %s), want at least the second venue's 30 ms", res.UnhedgedWindow, res.Duration)
	}
}

// A close of a pair whose opening orders are unproven does not wait for them to
// be read back before it starts reducing: at red margin that read-back can take
// the whole OrderSettleTimeout. The cancels go out first, the first reduce-only
// order right after them, and the read-back decides the verdict at the end.
func TestClose_UnprovenOpeningOrdersDoNotDelayTheFirstReduction(t *testing.T) {
	h := newHarness(t)
	h.cfg.OrderSettleTimeout = 400 * time.Millisecond
	h.build(holdsFor{"BTCUSDT", testIntentID}, allowAll{})
	h.hold(0.333, -0.333)
	var mu sync.Mutex
	var firstReduceAt time.Time
	for _, v := range []*scripted{h.binance, h.bybit} {
		v.set(func(s *scripted) {
			s.getOrder = func(q broker.OrderQuery) error {
				if isOrderOf(q.ClientOrderID, PurposeOpen, LegLong) || isOrderOf(q.ClientOrderID, PurposeOpen, LegShort) {
					return errNotVisible
				}
				return nil
			}
			s.placeBefore = func(req broker.PlaceOrderRequest) error {
				mu.Lock()
				defer mu.Unlock()
				if req.ReduceOnly && firstReduceAt.IsZero() {
					firstReduceAt = time.Now()
				}
				return nil
			}
		})
	}
	req := h.closeRequest()
	req.OpeningOrdersUnproven = true
	started := time.Now()
	res, err := h.exec.Close(context.Background(), req)
	mu.Lock()
	wait := firstReduceAt.Sub(started)
	mu.Unlock()
	if firstReduceAt.IsZero() || wait >= 200*time.Millisecond {
		t.Fatalf("the first reduction went out %s after the close started — it waited for the %s read-back", wait, h.cfg.OrderSettleTimeout)
	}
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v", long, short)
	}
	if res.Outcome != OutcomeUnresolved || !errors.Is(err, ErrLegAmbiguous) || res.OpeningOrdersProven {
		t.Errorf("%s, %v — flat, but the opening orders were never proven: loud", res.Outcome, err)
	}
}

// Review 4.5k round 3, M5: the pair reads flat, its opening orders are unproven,
// and the lost opening SHORT is processed and fills during the close. The
// read-back proves it Filled — and the close closes what it opened instead of
// reading it and leaving it naked.
func TestClose_ALateOpeningFillFoundByTheReadBackIsClosed(t *testing.T) {
	h := newHarness(t)
	openLong := ClientOrderID(testIntentID, PurposeOpen, LegLong, "", 0)
	openShort := ClientOrderID(testIntentID, PurposeOpen, LegShort, "", 0)
	if _, err := h.binance.Fake.PlaceOrder(context.Background(), broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT",
		Side: broker.SideBuy, Type: broker.OrderTypeLimitGTC, ClientOrderID: openLong, QtyCoin: 0.333, PriceQuote: testMid}); err != nil {
		t.Fatal(err)
	}
	h.hold(0, 0) // the long's fill was unwound during the open
	lateShort := broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", Side: broker.SideSell,
		Type: broker.OrderTypeLimitGTC, ClientOrderID: openShort, QtyCoin: 0.333, PriceQuote: testMid}
	var once sync.Once
	h.bybit.set(func(s *scripted) {
		s.getOrder = func(q broker.OrderQuery) error {
			if q.ClientOrderID == openShort {
				once.Do(func() {
					if _, err := s.Fake.PlaceOrder(context.Background(), lateShort); err != nil {
						t.Errorf("late: %v", err)
					}
				})
			}
			return nil
		}
	})
	req := h.closeRequest()
	req.OpeningOrdersUnproven = true
	res, err := h.exec.Close(context.Background(), req)
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v after the close: %s, %v", long, short, res.Outcome, err)
	}
	if err != nil || res.Outcome != OutcomeBothFlat || !res.OpeningOrdersProven {
		t.Errorf("%+v, %v — want both_flat with the opening orders proven", res, err)
	}
}

// Review 4.5k round 3, minor 1: Bybit's client confirms a cancel for up to three
// seconds. Cancels of the opening orders go out first but the close does not
// wait for their answers before it reduces.
func TestClose_ASlowCancelOfAnOpeningOrderDoesNotDelayTheFirstReduction(t *testing.T) {
	h := newHarness(t)
	h.hold(0.333, -0.333)
	var mu sync.Mutex
	var firstReduceAt time.Time
	h.bybit.set(func(s *scripted) {
		s.cancel = func(q broker.OrderQuery) error {
			if isOrderOf(q.ClientOrderID, PurposeOpen, LegShort) {
				time.Sleep(600 * time.Millisecond) // a confirming cancel loop
				return errors.New("test: the cancel is not confirmed")
			}
			return nil
		}
	})
	for _, v := range []*scripted{h.binance, h.bybit} {
		v.set(func(s *scripted) {
			s.placeBefore = func(req broker.PlaceOrderRequest) error {
				mu.Lock()
				defer mu.Unlock()
				if req.ReduceOnly && firstReduceAt.IsZero() {
					firstReduceAt = time.Now()
				}
				return nil
			}
		})
	}
	req := h.closeRequest()
	req.OpeningOrdersUnproven = true
	started := time.Now()
	res, err := h.exec.Close(context.Background(), req)
	elapsed := time.Since(started)
	mu.Lock()
	wait := firstReduceAt.Sub(started)
	mu.Unlock()
	if firstReduceAt.IsZero() || wait >= 200*time.Millisecond {
		t.Fatalf("the first reduction went out %s after the close started — it waited for a slow cancel", wait)
	}
	if elapsed < 600*time.Millisecond {
		t.Errorf("Close returned after %s, before the cancel it sent had answered", elapsed)
	}
	if long, short := h.positions(); long != 0 || short != 0 {
		t.Errorf("venues hold %v / %v: %s, %v", long, short, res.Outcome, err)
	}
}

// Review 4.5k round 3, minor 2: the close's "still hedged" verdict uses the
// executor's and Adopt's one criterion. Long 0.333 against short 0.332 on $60k
// BTC leaves $60 — one Bybit step, but tradeable on Binance's $50 minimum: not a
// hedge, so not the quiet ErrCloseIncomplete.
func TestClose_ARemainderWorthAMinimumNotionalIsNotCalledAHedge(t *testing.T) {
	h := newHarness(t)
	h.hold(0.333, -0.332)
	h.binance.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			if req.ReduceOnly {
				return errDefiniteRefusal
			}
			return nil
		}
	})
	res, err := h.exec.Close(context.Background(), h.closeRequest())
	if res.Outcome != OutcomeUnresolved || !isLoud(err) || errors.Is(err, ErrCloseIncomplete) {
		t.Fatalf("%s, %v — want unresolved and loud", res.Outcome, err)
	}
}
