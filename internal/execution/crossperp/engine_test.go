package crossperp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/broker/binance"
	"futures-arbitrage-scanner/internal/broker/brokertest"
	"futures-arbitrage-scanner/internal/coordinator"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/execution"
	"futures-arbitrage-scanner/internal/risk"
)

type engineHarness struct {
	*harness
	path  string
	coord *coordinator.Coordinator
	eng   *Engine
}

// newEngineHarness wires Engine 2 to a REAL coordinator reading the fake venues,
// reconciled once, as a process would start.
func newEngineHarness(t *testing.T) *engineHarness {
	t.Helper()
	h := newHarness(t)
	eh := &engineHarness{harness: h, path: filepath.Join(t.TempDir(), "coordinator-locks.json")}
	eh.restart(t)
	return eh
}

// restart builds a new coordinator and engine over the same lock file and
// venues — what a process restart sees.
func (eh *engineHarness) restart(t *testing.T) {
	t.Helper()
	coord, _, err := coordinator.New(coordinator.Config{
		Path:          eh.path,
		Venues:        []coordinator.Venue{{Name: venueBinance, Reader: eh.binance}, {Name: venueBybit, Reader: eh.bybit}},
		Symbols:       []string{"BTCUSDT"},
		ContestWindow: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord.ReconcileActivePositions(context.Background()); err != nil {
		t.Fatal(err)
	}
	eh.coord = coord
	eh.build(coord, allowAll{})
	eng, err := NewEngine(eh.exec, coord)
	if err != nil {
		t.Fatal(err)
	}
	eh.eng = eng
}

func (eh *engineHarness) open(t *testing.T) (Result, error) {
	t.Helper()
	return eh.eng.Open(context.Background(), OpenRequest{Intent: eh.intent(),
		PriorityAPROnCapitalFrac: 0.08, PriorityAPRBasisVI: "test: chưa có hàm strategy nào định giá Động cơ 2"})
}

func engine1Request() coordinator.AcquireRequest {
	return coordinator.AcquireRequest{Symbol: "BTCUSDT", Engine: coordinator.EngineCashAndCarry, IntentID: "cc-btc",
		Venues: []string{venueBinance}, PriorityAPROnCapitalFrac: 9, PriorityAPRBasisVI: "test"}
}

func TestEngine_OpenHoldsTheLockAgainstEngine1AndCloseReleasesIt(t *testing.T) {
	eh := newEngineHarness(t)
	if res, err := eh.open(t); err != nil || res.Outcome != OutcomeBothOpen {
		t.Fatalf("open: %s %v\n%s", res.Outcome, err, eh.rec.Dump())
	}
	if !eh.coord.Holds("BTCUSDT", coordinator.EngineCrossPerp, testIntentID) {
		t.Fatal("an open pair does not hold its lock")
	}
	if _, err := eh.coord.TryAcquire(context.Background(), engine1Request()); !errors.Is(err, coordinator.ErrOccupied) {
		t.Fatalf("Engine 1 on a symbol Engine 2 holds: %v, want ErrOccupied", err)
	}
	if res, err := eh.eng.Close(context.Background(), "BTCUSDT", "", "test"); err != nil || res.Outcome != OutcomeBothFlat {
		t.Fatalf("close: %s %v", res.Outcome, err)
	}
	if _, held := eh.coord.QueryLock("BTCUSDT"); held {
		t.Fatal("a flat pair still holds its lock")
	}
	if dec, err := eh.coord.TryAcquire(context.Background(), engine1Request()); err != nil || !dec.Granted {
		t.Fatalf("Engine 1 after Engine 2 closed: %+v, %v", dec, err)
	}
}

func TestEngine_AnUnwoundOpenReleasesTheLockOnTheVenuesWord(t *testing.T) {
	eh := newEngineHarness(t)
	eh.bybit.answerAfterOtherOpens(eh.binance, errDefiniteRefusal)
	res, err := eh.open(t)
	if res.Outcome != OutcomeUnwoundFlat || !errors.Is(err, ErrOpenUnwound) {
		t.Fatalf("%s, %v", res.Outcome, err)
	}
	if _, held := eh.coord.QueryLock("BTCUSDT"); held {
		t.Fatal("an unwound, venue-flat open kept its lock")
	}
}

// A result nobody can name keeps the symbol locked: nothing opens beside it. The
// open's short never reached Bybit as far as any answer can tell, so the pair is
// recorded — flat, unresolved, its opening orders unproven — where the margin
// guard sees it (review 4.5k, N3). When the lost order arrives late and fills,
// the guard sees THAT, read from the venue; its close takes the short to flat but
// is not called done while the order stays unprovable; and only a person's word
// that the opening orders are finished lets the next close release the lock.
func TestEngine_AnAmbiguousOpenKeepsTheLockAndStaysVisible(t *testing.T) {
	eh := newEngineHarness(t)
	var mu sync.Mutex
	var lost *broker.PlaceOrderRequest
	eh.bybit.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			if req.ReduceOnly {
				return nil
			}
			eh.binance.awaitOpeningSent()
			mu.Lock()
			r := req
			lost = &r
			mu.Unlock()
			return errTransport
		}
		s.getOrder = func(q broker.OrderQuery) error {
			if isOrderOf(q.ClientOrderID, PurposeOpen, LegShort) {
				return errNotVisible
			}
			return nil
		}
		s.cancel = func(q broker.OrderQuery) error {
			if isOrderOf(q.ClientOrderID, PurposeOpen, LegShort) {
				return errNotVisible
			}
			return nil
		}
	})
	if _, err := eh.open(t); !errors.Is(err, ErrLegAmbiguous) {
		t.Fatalf("%v", err)
	}
	if !eh.coord.Holds("BTCUSDT", coordinator.EngineCrossPerp, testIntentID) {
		t.Fatal("a loud open released its lock")
	}
	if _, err := eh.coord.TryAcquire(context.Background(), engine1Request()); !errors.Is(err, coordinator.ErrOccupied) {
		t.Fatalf("Engine 1 beside an ambiguous Engine-2 order: %v", err)
	}
	pairs := eh.eng.Pairs()
	if len(pairs) != 1 || !pairs[0].Unresolved || !pairs[0].OpeningOrdersUnproven || len(eh.eng.Held()) != 0 {
		t.Fatalf("pairs %+v, held %v — want the flat open recorded as an unresolved pair", pairs, eh.eng.Held())
	}

	// The backend processes the lost short now.
	mu.Lock()
	req := *lost
	mu.Unlock()
	if _, err := eh.bybit.Fake.PlaceOrder(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	exposed, _ := eh.eng.OpenPairs(context.Background())
	if len(exposed) != 1 || exposed[0].ExposureQuoteByVenue[venueBybit] <= 0 {
		t.Fatalf("OpenPairs %+v — the late short must be visible, read from the venue", exposed)
	}
	if err := eh.eng.CloseForMargin(context.Background(), testIntentID, venueBybit); !errors.Is(err, ErrLegAmbiguous) {
		t.Fatalf("emergency close beside an unprovable opening order: %v — want an error, not 'closed'", err)
	}
	if long, short := eh.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v after the emergency close", long, short)
	}
	if !eh.coord.Holds("BTCUSDT", coordinator.EngineCrossPerp, testIntentID) || len(eh.eng.Pairs()) != 1 {
		t.Fatal("the lock or the pair went away beside an order nobody can prove finished")
	}

	if err := eh.eng.ConfirmOrdersFinished("BTCUSDT", "another-intent"); !errors.Is(err, ErrUnknownPair) {
		t.Errorf("vouching for another intent: %v", err)
	}
	if err := eh.eng.ConfirmOrdersFinished("BTCUSDT", testIntentID); err != nil {
		t.Fatal(err)
	}
	if res, err := eh.eng.Close(context.Background(), "BTCUSDT", "", "operator"); err != nil || res.Outcome != OutcomeBothFlat {
		t.Fatalf("close after the operator's word: %s, %v", res.Outcome, err)
	}
	if _, held := eh.coord.QueryLock("BTCUSDT"); held {
		t.Error("the lock survived a proven close")
	}
}

// ratioFromVenue is a margin reader whose ratio follows the fake venue: red
// while Binance holds the long leg, green once it is flat.
type ratioFromVenue struct {
	name      string
	venue     *scripted
	red, calm float64
}

func (r ratioFromVenue) VenueName() string { return r.name }

func (r ratioFromVenue) ReadMargin(ctx context.Context) (risk.MarginReading, error) {
	p, err := r.venue.Fake.GetPosition(ctx, broker.MarketFuturesUSDM, "BTCUSDT")
	if err != nil {
		return risk.MarginReading{}, err
	}
	ratio := r.calm
	if p.QtyCoin != 0 {
		ratio = r.red
	}
	return risk.DerivedMarginReading(r.name, "USDT", ratio*1000, 1000, "test", time.Now().UnixMilli())
}

// The design's acceptance, end to end: Binance maintenance margin at 66% with
// an Engine-2 pair open. One tick closes the pair on both venues, the lock is
// released on the venues' word, and every open stays refused until an operator
// acknowledges the latch.
func TestEngine_MarginGuardAt66PercentClosesThePairAndReleasesTheLock(t *testing.T) {
	eh := newEngineHarness(t)
	if res, err := eh.open(t); err != nil || res.Outcome != OutcomeBothOpen {
		t.Fatalf("open: %s %v", res.Outcome, err)
	}
	guard, err := risk.NewMarginGuard(risk.DefaultMarginGuardConfig(), []risk.MarginReader{
		ratioFromVenue{name: venueBinance, venue: eh.binance, red: 0.66, calm: 0.30},
		ratioFromVenue{name: venueBybit, venue: eh.bybit, red: 0.20, calm: 0.10},
	}, eh.eng)
	if err != nil {
		t.Fatal(err)
	}
	report := guard.Tick(context.Background())
	if len(report.ClosedPairIDs) != 1 || report.ClosedPairIDs[0] != testIntentID || !report.Emergency {
		t.Fatalf("report %+v", report)
	}
	if long, short := eh.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v after the emergency close", long, short)
	}
	if _, held := eh.coord.QueryLock("BTCUSDT"); held {
		t.Error("the closed pair's lock was not released")
	}
	if n := len(eh.binance.sentWith(testIntentID, PurposeClose, LegLong)) + len(eh.bybit.sentWith(testIntentID, PurposeClose, LegShort)); n != 2 {
		t.Errorf("%d emergency close orders, want one per leg", n)
	}
	for _, req := range []risk.OpenRequest{
		{Engine: string(coordinator.EngineCrossPerp), CrossVenuePerp: true, Venues: []string{venueBinance, venueBybit}},
		{Engine: string(coordinator.EngineCashAndCarry), Venues: []string{venueBybit}},
	} {
		if err := guard.AllowOpen(req); !errors.Is(err, risk.ErrOpenBlockedByMargin) {
			t.Errorf("%+v after the red latch: %v", req, err)
		}
	}
	// The executor asks the same guard: an Engine-2 open is refused before a
	// lock is even asked of it.
	eh.build(eh.coord, guard)
	eng, _ := NewEngine(eh.exec, eh.coord)
	if _, err := eng.Open(context.Background(), OpenRequest{Intent: eh.intent(), PriorityAPROnCapitalFrac: 1, PriorityAPRBasisVI: "test"}); !errors.Is(err, ErrMarginGate) {
		t.Errorf("an open through the latched guard: %v, want ErrMarginGate", err)
	}
}

// An emergency close on a symbol with an open still in flight waits for it,
// instead of sending a second set of orders beside it.
func TestEngine_AnEmergencyCloseWaitsForTheOperationInFlight(t *testing.T) {
	eh := newEngineHarness(t)
	if _, err := eh.open(t); err != nil {
		t.Fatal(err)
	}
	if err := eh.eng.begin(context.Background(), "BTCUSDT"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- eh.eng.CloseForMargin(context.Background(), testIntentID, venueBinance) }()
	time.Sleep(30 * time.Millisecond)
	if n := len(eh.binance.sentWith(testIntentID, PurposeClose, LegLong)); n != 0 {
		t.Fatalf("the emergency close sent %d orders while another operation held the symbol", n)
	}
	eh.eng.end("BTCUSDT")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the emergency close never ran after the operation ended")
	}
	if long, short := eh.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v", long, short)
	}
}

// A restart: the lock comes back from the file, the engine adopts the pair from
// the VENUES, and can close it and release the lock.
func TestEngine_AdoptsAPairAfterARestartAndClosesIt(t *testing.T) {
	eh := newEngineHarness(t)
	if _, err := eh.open(t); err != nil {
		t.Fatal(err)
	}
	eh.restart(t)
	if len(eh.eng.Pairs()) != 0 {
		t.Fatal("a restarted engine remembers pairs it never read")
	}
	intent := eh.intent()
	if _, err := eh.eng.Adopt(context.Background(), "someone-else", intent.Long, intent.Short); !errors.Is(err, coordinator.ErrNotOwner) {
		t.Errorf("adopting under another intent id: %v", err)
	}
	p, err := eh.eng.Adopt(context.Background(), testIntentID, intent.Long, intent.Short)
	if err != nil || p.LongQtyCoin != 0.333 || p.ShortQtyCoin != 0.333 || !p.Adopted {
		t.Fatalf("adopt: %+v, %v", p, err)
	}
	pairs, _ := eh.eng.OpenPairs(context.Background())
	if len(pairs) != 1 || pairs[0].ExposureQuoteByVenue[venueBinance] <= 0 {
		t.Fatalf("OpenPairs %+v", pairs)
	}
	if res, err := eh.eng.Close(context.Background(), "BTCUSDT", "", "test"); err != nil || res.Outcome != OutcomeBothFlat {
		t.Fatalf("close after adopt: %s, %v", res.Outcome, err)
	}
	if _, held := eh.coord.QueryLock("BTCUSDT"); held {
		t.Fatal("the adopted pair's lock survived its close")
	}
}

// Two opens of different intents on one symbol from two goroutines: one lock,
// one pair — under -race.
func TestEngine_ConcurrentOpensOnOneSymbolOpenOnePair(t *testing.T) {
	eh := newEngineHarness(t)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			intent := eh.intent()
			intent.ID = []string{testIntentID, "cp-intent-0002"}[i]
			_, errs[i] = eh.eng.Open(context.Background(), OpenRequest{Intent: intent, PriorityAPROnCapitalFrac: float64(i), PriorityAPRBasisVI: "test"})
		}(i)
	}
	wg.Wait()
	granted := 0
	for _, err := range errs {
		if err == nil {
			granted++
		} else if !errors.Is(err, coordinator.ErrLostContest) && !errors.Is(err, coordinator.ErrOccupied) && !errors.Is(err, ErrLockNotHeld) {
			t.Errorf("unexpected: %v", err)
		}
	}
	if granted != 1 || len(eh.eng.Pairs()) != 1 {
		t.Fatalf("%d opens succeeded, %d pairs — want exactly one", granted, len(eh.eng.Pairs()))
	}
	if long, short := eh.positions(); long != 0.333 || short != -0.333 {
		t.Fatalf("venues hold %v / %v — two engines' orders netted", long, short)
	}
}

// Review 4.5k, M6: an open refused before its first order gives its lock back —
// so a symbol another engine holds does not end up locked for both.
func TestEngine_AnOpenRefusedBeforeSendingGivesTheLockBack(t *testing.T) {
	eh := newEngineHarness(t)
	eh.binance.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", QtyCoin: -0.05}) // Engine 1, bypassing the lock
	if _, err := eh.open(t); !errors.Is(err, ErrVenueNotFlat) {
		t.Fatalf("open: %v", err)
	}
	if _, held := eh.coord.QueryLock("BTCUSDT"); held {
		t.Fatal("a refused open kept its lock")
	}
	eh.binance.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", QtyCoin: 0}) // Engine 1 closed
	if _, err := eh.coord.ReconcileActivePositions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dec, err := eh.coord.TryAcquire(context.Background(), engine1Request()); err != nil || !dec.Granted {
		t.Fatalf("Engine 1 on the flat symbol: %+v, %v", dec, err)
	}
}

// Review 4.5k, M4: legs an open could not resolve are recorded, so the margin
// guard sees them; legs whose EVIDENCE conflicts are recorded but blocked from an
// automatic close.
func TestEngine_UnresolvedLegsAreVisibleToTheGuard(t *testing.T) {
	t.Run("a failed unwind", func(t *testing.T) {
		eh := newEngineHarness(t)
		eh.bybit.answerAfterOtherOpens(eh.binance, errDefiniteRefusal)
		eh.binance.set(func(s *scripted) {
			s.placeBefore = func(req broker.PlaceOrderRequest) error {
				if req.ReduceOnly {
					return fmt.Errorf("%w: test", broker.ErrIPCoolingDown)
				}
				return nil
			}
		})
		res, err := eh.open(t)
		if res.Outcome != OutcomeUnresolved || !errors.Is(err, execution.ErrUnwindIncomplete) {
			t.Fatalf("%s, %v", res.Outcome, err)
		}
		pairs, _ := eh.eng.OpenPairs(context.Background())
		if len(pairs) != 1 || pairs[0].ExposureQuoteByVenue[venueBinance] <= 0 || pairs[0].ExposureQuoteByVenue[venueBybit] != 0 || pairs[0].CloseBlockedVI != "" {
			t.Fatalf("OpenPairs %+v — want the naked long, closable", pairs)
		}
		if !eh.coord.Holds("BTCUSDT", coordinator.EngineCrossPerp, testIntentID) {
			t.Error("the lock was released beside a naked leg")
		}
		// The cool-down passes; the guard's close takes the naked leg to zero.
		eh.binance.set(func(s *scripted) { s.placeBefore = nil })
		if err := eh.eng.CloseForMargin(context.Background(), testIntentID, venueBinance); err != nil {
			t.Fatalf("emergency close of the naked leg: %v", err)
		}
		if long, short := eh.positions(); long != 0 || short != 0 {
			t.Fatalf("venues hold %v / %v", long, short)
		}
	})
	t.Run("conflicting evidence", func(t *testing.T) {
		eh := newEngineHarness(t)
		var reads atomic.Int64
		eh.bybit.set(func(s *scripted) {
			s.position = func(p broker.Position) (broker.Position, error) {
				if reads.Add(1) > 1 { // the pre-trade read is true; after the fill the venue answers 0
					p.QtyCoin = 0
				}
				return p, nil
			}
		})
		if _, err := eh.open(t); !errors.Is(err, ErrFillEvidenceConflict) {
			t.Fatalf("%v", err)
		}
		pairs, _ := eh.eng.OpenPairs(context.Background())
		if len(pairs) != 1 || pairs[0].CloseBlockedVI == "" {
			t.Fatalf("OpenPairs %+v — want the pair listed and blocked", pairs)
		}
		if _, err := eh.eng.Close(context.Background(), "BTCUSDT", "", "test"); !errors.Is(err, ErrCloseBlocked) {
			t.Errorf("close of a blocked pair: %v", err)
		}
	})
}

// Review 4.5k, M7 and N4: Adopt refuses venues that are not the lock's own, and
// RECORDS a pair whose legs are not a hedge — as unresolved, at both legs' real
// sizes, where the guard sees and can close it — instead of refusing it into
// invisibility.
func TestEngine_AdoptRefusesForeignVenuesAndRecordsAnUnbalancedPairAsUnresolved(t *testing.T) {
	eh := newEngineHarness(t)
	if _, err := eh.open(t); err != nil {
		t.Fatal(err)
	}
	eh.restart(t)
	intent := eh.intent()
	foreign := intent.Short
	foreign.Venue.Name = "okx_futures"
	if _, err := eh.eng.Adopt(context.Background(), testIntentID, intent.Long, foreign); !errors.Is(err, ErrAdoptRefused) {
		t.Errorf("adopting on a venue the lock never named: %v", err)
	}
	if _, err := eh.eng.Adopt(context.Background(), testIntentID, intent.Short, intent.Long); !errors.Is(err, ErrAdoptRefused) {
		t.Errorf("adopting with the legs swapped (the long venue holds a long): %v", err)
	}
	// Three Bybit steps apart: not a hedge on either venue's grid.
	eh.bybit.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", QtyCoin: -0.330})
	p, err := eh.eng.Adopt(context.Background(), testIntentID, intent.Long, intent.Short)
	if err != nil || !p.Unresolved || p.LongQtyCoin != 0.333 || p.ShortQtyCoin != 0.330 {
		t.Fatalf("adopting 0.333 / −0.330: %+v, %v", p, err)
	}
	exposed, _ := eh.eng.OpenPairs(context.Background())
	if len(exposed) != 1 || exposed[0].ExposureQuoteByVenue[venueBinance] <= exposed[0].ExposureQuoteByVenue[venueBybit] {
		t.Fatalf("OpenPairs %+v — want the larger long visible", exposed)
	}
	if _, err := eh.eng.Adopt(context.Background(), testIntentID, intent.Long, intent.Short); !errors.Is(err, ErrPairBusy) {
		t.Errorf("adopting the same symbol twice: %v", err)
	}
	if err := eh.eng.CloseForMargin(context.Background(), testIntentID, venueBinance); err != nil {
		t.Fatalf("closing the unbalanced adopted pair: %v\n%s", err, eh.rec.Dump())
	}
	if long, short := eh.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v", long, short)
	}
}

// Review 4.5k, N4: a pair the executor keeps as hedged — 0.05 against 0.049, one
// step of Bybit's grid and $60 under both minimum notionals, 2% apart — is still
// a pair after a restart: not a conflict nobody can close.
func TestEngine_ASmallHedgeIsStillAPairAfterARestart(t *testing.T) {
	eh := newEngineHarness(t)
	eh.bybit.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.98}) // 0.05 × 0.98 floors to 0.049
	intent := eh.intent()
	intent.NotionalQuote = 3_000 // Q = 0.05 BTC
	intent.Long.Rules.MinNotionalQuote, intent.Short.Rules.MinNotionalQuote = 100, 100
	res, err := eh.eng.Open(context.Background(), OpenRequest{Intent: intent, PriorityAPROnCapitalFrac: 0.1, PriorityAPRBasisVI: "test"})
	if err != nil || res.Outcome != OutcomeBothOpen || res.ReducedToMatch {
		t.Fatalf("open: %s reduced=%v %v", res.Outcome, res.ReducedToMatch, err)
	}
	if long, short := eh.positions(); long != 0.05 || short != -0.049 {
		t.Fatalf("venues hold %v / %v", long, short)
	}
	eh.restart(t)
	lock, held := eh.coord.QueryLock("BTCUSDT")
	if !held || lock.State != coordinator.StateOccupied || lock.OwnerEngine != coordinator.EngineCrossPerp {
		t.Fatalf("after restart: %+v — want Engine 2's lock, not a conflict", lock)
	}
	p, err := eh.eng.Adopt(context.Background(), testIntentID, intent.Long, intent.Short)
	if err != nil || p.Unresolved || p.OpeningOrdersUnproven {
		t.Fatalf("adopt: %+v, %v — want a clean pair", p, err)
	}
	if res, err := eh.eng.Close(context.Background(), "BTCUSDT", "", "operator"); err != nil || res.Outcome != OutcomeBothFlat {
		t.Fatalf("close: %s, %v", res.Outcome, err)
	}
	if _, held := eh.coord.QueryLock("BTCUSDT"); held {
		t.Error("the lock survived the close")
	}
}

// Review 4.5k, N7: after a restart, a lock from the file over FLAT venues — a
// close whose release failed before the crash, or a crash between the grant and
// the first order — has a way out through Engine 2: Adopt records it, Close
// proves it flat, quiet and its orders finished, and releases it.
func TestEngine_AFlatLockFromTheFileHasAWayOut(t *testing.T) {
	t.Run("orders were sent under it", func(t *testing.T) {
		eh := newEngineHarness(t)
		if _, err := eh.open(t); err != nil {
			t.Fatal(err)
		}
		eh.binance.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", QtyCoin: 0})
		eh.bybit.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", QtyCoin: 0})
		eh.restart(t)
		intent := eh.intent()
		p, err := eh.eng.Adopt(context.Background(), testIntentID, intent.Long, intent.Short)
		if err != nil || !p.OpeningOrdersUnproven {
			t.Fatalf("adopt: %+v, %v — orders were sent under this lock, so their state must be proven", p, err)
		}
		res, err := eh.eng.Close(context.Background(), "BTCUSDT", "", "operator")
		if err != nil || res.Outcome != OutcomeBothFlat || !res.AlreadyFlat || !res.OpeningOrdersProven {
			t.Fatalf("close: %+v, %v", res, err)
		}
		if dec, err := eh.coord.TryAcquire(context.Background(), engine1Request()); err != nil || !dec.Granted {
			t.Fatalf("Engine 1 afterwards: %+v, %v", dec, err)
		}
	})
	t.Run("nothing was ever sent under it", func(t *testing.T) {
		eh := newEngineHarness(t)
		dec, err := eh.coord.TryAcquire(context.Background(), coordinator.AcquireRequest{Symbol: "BTCUSDT", Engine: coordinator.EngineCrossPerp,
			IntentID: testIntentID, Venues: []string{venueBinance, venueBybit}, PriorityAPROnCapitalFrac: 0.1, PriorityAPRBasisVI: "test"})
		if err != nil || !dec.Granted {
			t.Fatal(err)
		}
		eh.restart(t) // the process died before sending
		intent := eh.intent()
		p, err := eh.eng.Adopt(context.Background(), testIntentID, intent.Long, intent.Short)
		if err != nil || p.Unresolved || p.OpeningOrdersUnproven {
			t.Fatalf("adopt: %+v, %v", p, err)
		}
		if res, err := eh.eng.Close(context.Background(), "BTCUSDT", "", "operator"); err != nil || res.Outcome != OutcomeBothFlat {
			t.Fatalf("close: %s, %v", res.Outcome, err)
		}
		if n := len(eh.binance.sent()) + len(eh.bybit.sent()); n != 0 {
			t.Errorf("%d orders sent to release a flat lock", n)
		}
		if _, held := eh.coord.QueryLock("BTCUSDT"); held {
			t.Error("the lock survived")
		}
	})
}

// Review 4.5k, m-g: Withdraw does not take the caller's word. Once the executor
// marked orders as sent — before its first order — the lock of a live pair cannot
// be withdrawn.
func TestEngine_ALockUnderWhichOrdersWereSentCannotBeWithdrawn(t *testing.T) {
	eh := newEngineHarness(t)
	if _, err := eh.open(t); err != nil {
		t.Fatal(err)
	}
	if err := eh.coord.Withdraw("BTCUSDT", coordinator.EngineCrossPerp, testIntentID); !errors.Is(err, coordinator.ErrNotWithdrawable) {
		t.Fatalf("withdrawing beside an open pair: %v", err)
	}
	if !eh.coord.Holds("BTCUSDT", coordinator.EngineCrossPerp, testIntentID) {
		t.Fatal("the lock went away")
	}
}

// Review 4.5k, N3 (the reviewer's V11): an open whose unwind failed, with its
// short opening order still RESTING and unreadable by id. The guard's close
// flattens the positions, but a resting order on the symbol is not a closed pair:
// CloseForMargin says so, and the pair stays where the guard sees the order's fill.
func TestEngine_AnEmergencyCloseBesideARestingOpeningOrderIsNotReportedClosed(t *testing.T) {
	eh := newEngineHarness(t)
	openShort := ClientOrderID(testIntentID, PurposeOpen, LegShort, "", 0)
	eh.bybit.SetBehaviour(brokertest.Behaviour{}) // the short rests
	eh.bybit.set(func(s *scripted) {
		s.placeAfter = func(req broker.PlaceOrderRequest) error {
			if req.ClientOrderID == openShort {
				return errTransport // recorded and resting, answer lost
			}
			return nil
		}
		s.getOrder = func(q broker.OrderQuery) error {
			if q.ClientOrderID == openShort {
				return errNotVisible
			}
			return nil
		}
		s.cancel = func(q broker.OrderQuery) error {
			if q.ClientOrderID == openShort {
				return errNotVisible
			}
			return nil
		}
	})
	eh.binance.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			if req.ReduceOnly {
				return errDefiniteRefusal // the unwind is refused: the long stays
			}
			return nil
		}
	})
	res, err := eh.open(t)
	if !isLoud(err) || len(eh.eng.Pairs()) != 1 {
		t.Fatalf("open: %s, %v, pairs %v", res.Outcome, err, eh.eng.Pairs())
	}
	eh.binance.set(func(s *scripted) { s.placeBefore = nil })
	if err := eh.eng.CloseForMargin(context.Background(), testIntentID, venueBinance); err == nil {
		t.Fatal("CloseForMargin said closed while the opening short still rests")
	}
	if long, short := eh.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v", long, short)
	}
	if err := eh.bybit.Fake.Fill(openShort, 0.333, testMid); err != nil {
		t.Fatal(err)
	}
	exposed, _ := eh.eng.OpenPairs(context.Background())
	if len(exposed) != 1 || exposed[0].ExposureQuoteByVenue[venueBybit] <= 0 {
		t.Fatalf("OpenPairs %+v — the resting order's fill must be visible to the guard", exposed)
	}
	if !eh.coord.Holds("BTCUSDT", coordinator.EngineCrossPerp, testIntentID) {
		t.Error("the lock was released")
	}
}

// Review 4.5k, m-f: a close whose final read fails leaves the pair UNRESOLVED,
// and a venue on the wrong side blocks the automatic close.
func TestEngine_ACloseThatCannotReadOrReadsTheWrongSideSaysSo(t *testing.T) {
	t.Run("unreadable after the close", func(t *testing.T) {
		eh := newEngineHarness(t)
		if _, err := eh.open(t); err != nil {
			t.Fatal(err)
		}
		var closing atomic.Bool
		eh.bybit.set(func(s *scripted) {
			s.placeAfter = func(req broker.PlaceOrderRequest) error {
				if req.ReduceOnly {
					closing.Store(true)
				}
				return nil
			}
			s.position = func(p broker.Position) (broker.Position, error) {
				if closing.Load() {
					return broker.Position{}, errTransport
				}
				return p, nil
			}
		})
		res, err := eh.eng.Close(context.Background(), "BTCUSDT", "", "test")
		if res.Outcome != OutcomeUnresolved || err == nil {
			t.Fatalf("%s, %v", res.Outcome, err)
		}
		if p := eh.eng.Pairs(); len(p) != 1 || !p[0].Unresolved {
			t.Fatalf("pairs %+v — want the pair marked unresolved", p)
		}
	})
	t.Run("a venue on the wrong side", func(t *testing.T) {
		eh := newEngineHarness(t)
		if _, err := eh.open(t); err != nil {
			t.Fatal(err)
		}
		eh.bybit.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", QtyCoin: 0.2})
		if _, err := eh.eng.Close(context.Background(), "BTCUSDT", "", "test"); !errors.Is(err, ErrPositionDisagrees) {
			t.Fatalf("%v", err)
		}
		p := eh.eng.Pairs()
		if len(p) != 1 || p[0].CloseBlockedVI == "" || !p[0].Unresolved {
			t.Fatalf("pairs %+v — want the pair blocked from an automatic close", p)
		}
		if err := eh.eng.CloseForMargin(context.Background(), testIntentID, venueBinance); !errors.Is(err, ErrCloseBlocked) {
			t.Errorf("guard close of a blocked pair: %v", err)
		}
	})
}

// Review 4.5k, m-h: a retry works without the engine's mutex, so it may only
// clear or update a hold that is still its own intent's.
func TestEngine_AHoldIsTouchedOnlyByItsOwnIntent(t *testing.T) {
	eh := newEngineHarness(t)
	eh.eng.hold("BTCUSDT", "newer-intent", errors.New("test"))
	eh.eng.clearHold("BTCUSDT", "older-intent")
	eh.eng.updateHold("BTCUSDT", "older-intent", errors.New("stale"))
	held := eh.eng.Held()
	if err, ok := held["BTCUSDT"]; !ok || err.Error() != "test" {
		t.Fatalf("held %v — an older intent's retry touched a newer hold", held)
	}
	eh.eng.clearHold("BTCUSDT", "newer-intent")
	if len(eh.eng.Held()) != 0 {
		t.Error("the owner could not clear its hold")
	}
}

// withQuiet rebuilds the harness's executor and engine with a quiet period of its
// own, before anything is opened.
func (eh *engineHarness) withQuiet(t *testing.T, quiet time.Duration) {
	t.Helper()
	eh.cfg.AmbiguousSendQuiet = quiet
	eh.build(eh.coord, allowAll{})
	eng, err := NewEngine(eh.exec, eh.coord)
	if err != nil {
		t.Fatal(err)
	}
	eh.eng = eng
}

// Review 4.5k round 3, M1: a reduce-only order whose answer was lost reduces
// WHATEVER the venue holds when it executes — Engine 1's short, once Engine 2's
// lock is gone. The close that sent it keeps the lock, flat as the venues are,
// until the quiet period after that send has passed and the venue still shows no
// such order; Engine 1 is refused the symbol for all of it.
func TestEngine_ALostReductionKeepsTheLockThroughItsQuietPeriod(t *testing.T) {
	eh := newEngineHarness(t)
	const quiet = 300 * time.Millisecond
	eh.withQuiet(t, quiet)
	if res, err := eh.open(t); err != nil || res.Outcome != OutcomeBothOpen {
		t.Fatalf("open: %s %v", res.Outcome, err)
	}
	var mu sync.Mutex
	lostID := ""
	var lostAt time.Time
	eh.bybit.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			mu.Lock()
			defer mu.Unlock()
			if req.ReduceOnly && lostID == "" {
				lostID, lostAt = req.ClientOrderID, time.Now()
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
	refusedDuringQuiet := make(chan error, 1)
	go func() {
		for {
			mu.Lock()
			at := lostAt
			mu.Unlock()
			if !at.IsZero() {
				time.Sleep(quiet / 2)
				_, err := eh.coord.TryAcquire(context.Background(), engine1Request())
				refusedDuringQuiet <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	res, err := eh.eng.Close(context.Background(), "BTCUSDT", venueBybit, "test")
	mu.Lock()
	sinceLost := time.Since(lostAt)
	mu.Unlock()
	if err != nil || res.Outcome != OutcomeBothFlat {
		t.Fatalf("close: %s, %v", res.Outcome, err)
	}
	if sinceLost < quiet {
		t.Errorf("the close released the lock %s after the lost reduction's send — before its %s quiet period", sinceLost, quiet)
	}
	if err := <-refusedDuringQuiet; !errors.Is(err, coordinator.ErrOccupied) {
		t.Errorf("Engine 1 during the quiet period: %v — want ErrOccupied", err)
	}
	if _, held := eh.coord.QueryLock("BTCUSDT"); held {
		t.Error("the lock survived a proven close")
	}
}

// Review 4.5k round 3, M1: a reduction the VENUE answered "execution status
// unknown" (-1007) is never taken as absent. The pair stays — flat, unresolved,
// its pending order named — and so does the lock, until a person says the order
// is finished.
func TestEngine_AReductionAnsweredStatusUnknownKeepsThePairAndItsLock(t *testing.T) {
	eh := newEngineHarness(t)
	if res, err := eh.open(t); err != nil || res.Outcome != OutcomeBothOpen {
		t.Fatalf("open: %s %v", res.Outcome, err)
	}
	statusUnknown := fmt.Errorf("%w; %w", &binance.VenueError{Code: -1007, StatusCode: 400}, &broker.HTTPError{StatusCode: 400})
	var mu sync.Mutex
	lostID := ""
	eh.bybit.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			mu.Lock()
			defer mu.Unlock()
			if req.ReduceOnly && lostID == "" {
				lostID = req.ClientOrderID
				return statusUnknown
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
	res, err := eh.eng.Close(context.Background(), "BTCUSDT", venueBybit, "test")
	if long, short := eh.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v", long, short)
	}
	if res.Outcome != OutcomeUnresolved || !errors.Is(err, ErrLegAmbiguous) || len(res.PendingOrders) != 1 || !res.PendingOrders[0].StatusUnknown {
		t.Fatalf("close: %+v, %v — want loud, the -1007 reduction pending", res, err)
	}
	pairs := eh.eng.Pairs()
	if len(pairs) != 1 || len(pairs[0].PendingOrders) != 1 || !pairs[0].Unresolved {
		t.Fatalf("pairs %+v — want the flat pair kept with its pending order", pairs)
	}
	if _, err := eh.coord.TryAcquire(context.Background(), engine1Request()); !errors.Is(err, coordinator.ErrOccupied) {
		t.Fatalf("Engine 1 beside a status-unknown reduction: %v", err)
	}
	// Another close reads it back again; the venue still does not know it.
	if _, err := eh.eng.Close(context.Background(), "BTCUSDT", "", "again"); !errors.Is(err, ErrLegAmbiguous) {
		t.Fatalf("second close: %v", err)
	}
	if err := eh.eng.ConfirmOrdersFinished("BTCUSDT", testIntentID); err != nil {
		t.Fatal(err)
	}
	if res, err := eh.eng.Close(context.Background(), "BTCUSDT", "", "operator"); err != nil || res.Outcome != OutcomeBothFlat {
		t.Fatalf("close after the operator's word: %s, %v", res.Outcome, err)
	}
	if _, held := eh.coord.QueryLock("BTCUSDT"); held {
		t.Error("the lock survived")
	}
}

// Self-review, round 4: a close REFUSED before its verdict — here because a venue
// reads the wrong way — must not make the engine forget the reduce-only orders an
// earlier close left pending. They are what keeps the lock.
func TestEngine_ACloseRefusedBeforeItsVerdictKeepsThePendingOrders(t *testing.T) {
	eh := newEngineHarness(t)
	if res, err := eh.open(t); err != nil || res.Outcome != OutcomeBothOpen {
		t.Fatalf("open: %s %v", res.Outcome, err)
	}
	statusUnknown := fmt.Errorf("%w; %w", &binance.VenueError{Code: -1007, StatusCode: 400}, &broker.HTTPError{StatusCode: 400})
	var mu sync.Mutex
	lostID := ""
	eh.bybit.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			mu.Lock()
			defer mu.Unlock()
			if req.ReduceOnly && lostID == "" {
				lostID = req.ClientOrderID
				return statusUnknown
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
	if _, err := eh.eng.Close(context.Background(), "BTCUSDT", venueBybit, "test"); !errors.Is(err, ErrLegAmbiguous) {
		t.Fatalf("first close: %v", err)
	}
	if p := eh.eng.Pairs(); len(p) != 1 || len(p[0].PendingOrders) != 1 {
		t.Fatalf("pairs %+v — want the pending reduction recorded", p)
	}
	// Something else now holds the long venue the wrong way round.
	eh.binance.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", QtyCoin: -0.1})
	if _, err := eh.eng.Close(context.Background(), "BTCUSDT", "", "again"); !errors.Is(err, ErrPositionDisagrees) {
		t.Fatalf("second close: %v", err)
	}
	p := eh.eng.Pairs()
	if len(p) != 1 || len(p[0].PendingOrders) != 1 || p[0].CloseBlockedVI == "" {
		t.Fatalf("pairs %+v — want the pending order kept and the pair blocked", p)
	}
	if !eh.coord.Holds("BTCUSDT", coordinator.EngineCrossPerp, testIntentID) {
		t.Error("the lock was released")
	}
}

// Review 4.5k round 3, M1 on the OPEN path: a cut whose answer said "execution
// status unknown", then an unwind to flat. Flat is not proven quiet: the open is
// loud, the pair is recorded with the cut pending, and the lock stays.
func TestEngine_AnOpenUnwoundBesideAStatusUnknownCutKeepsTheLock(t *testing.T) {
	eh := newEngineHarness(t)
	eh.binance.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 0.4})
	statusUnknown := fmt.Errorf("%w; %w", &binance.VenueError{Code: -1007, StatusCode: 400}, &broker.HTTPError{StatusCode: 400})
	var mu sync.Mutex
	lostID := ""
	eh.bybit.set(func(s *scripted) {
		s.placeBefore = func(req broker.PlaceOrderRequest) error {
			mu.Lock()
			defer mu.Unlock()
			if isOrderOf(req.ClientOrderID, PurposeReduce, LegShort) && lostID == "" {
				lostID = req.ClientOrderID
				return statusUnknown
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
	res, err := eh.open(t)
	if long, short := eh.positions(); long != 0 || short != 0 {
		t.Fatalf("venues hold %v / %v", long, short)
	}
	if res.Outcome != OutcomeUnwoundFlat || !errors.Is(err, ErrLegAmbiguous) || len(res.PendingOrders) != 1 {
		t.Fatalf("open: %s, %v, pending %v — want unwound_flat, loud, the cut pending", res.Outcome, err, res.PendingOrders)
	}
	if !eh.coord.Holds("BTCUSDT", coordinator.EngineCrossPerp, testIntentID) {
		t.Fatal("the lock was released beside a status-unknown cut")
	}
	if p := eh.eng.Pairs(); len(p) != 1 || len(p[0].PendingOrders) != 1 {
		t.Fatalf("pairs %+v", p)
	}
}

// Review 4.5k round 3, minor 3: legs with no book cannot value the pair's
// exposure — the guard would rank it at zero — nor show a residual to be below a
// minimum notional. Adopt refuses them.
func TestEngine_AdoptRefusesLegsWithNoReferencePrice(t *testing.T) {
	eh := newEngineHarness(t)
	if _, err := eh.open(t); err != nil {
		t.Fatal(err)
	}
	eh.restart(t)
	intent := eh.intent()
	long, short := intent.Long, intent.Short
	long.Book = depth.Summary{}
	if _, err := eh.eng.Adopt(context.Background(), testIntentID, long, short); !errors.Is(err, ErrAdoptRefused) {
		t.Fatalf("adopt with no book on the long: %v", err)
	}
	if len(eh.eng.Pairs()) != 0 {
		t.Error("a pair was recorded")
	}
	// Neither leg has a price: nothing can show the residual is below a minimum
	// notional (with one book, that leg's own minimum would catch this residual).
	short.Book = depth.Summary{}
	if hedgedWithin(long, short, 0.001, 0.333, 0.332) {
		t.Error("hedgedWithin called a residual a hedge with no price to value it")
	}
}

// Review 4.5k round 3, minor 4: a lock granted in THIS process has a live owner
// — between its grant and its first order, say. Adopting it would give the same
// intent a record its owner does not know; the coordinator refuses.
func TestEngine_AdoptRefusesALockItsOwnerIsOperating(t *testing.T) {
	eh := newEngineHarness(t)
	if _, err := eh.coord.TryAcquire(context.Background(), coordinator.AcquireRequest{Symbol: "BTCUSDT", Engine: coordinator.EngineCrossPerp,
		IntentID: testIntentID, Venues: []string{venueBinance, venueBybit}, PriorityAPROnCapitalFrac: 0.1, PriorityAPRBasisVI: "test"}); err != nil {
		t.Fatal(err)
	}
	intent := eh.intent()
	if _, err := eh.eng.Adopt(context.Background(), testIntentID, intent.Long, intent.Short); !errors.Is(err, coordinator.ErrOwnerLive) {
		t.Fatalf("adopting a lock granted in this process: %v", err)
	}
	if len(eh.eng.Pairs()) != 0 {
		t.Error("a pair was recorded")
	}
}

// Review 4.5k round 3, minor 8: an open refused before sending whose Withdraw
// failed (the lock file would not write) is retried as a Withdraw. A Release
// needs flat venues, and the position that caused the refusal is still there.
func TestEngine_AFailedWithdrawIsRetriedAsAWithdraw(t *testing.T) {
	eh := newEngineHarness(t)
	dir := filepath.Dir(eh.path)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	var once sync.Once
	eh.binance.set(func(s *scripted) {
		s.position = func(p broker.Position) (broker.Position, error) {
			// The pre-trade read: Engine 1's short is there, and from now on the
			// lock file cannot be written.
			once.Do(func() {
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Errorf("chmod: %v", err)
				}
			})
			p.QtyCoin = -0.05
			return p, nil
		}
	})
	if _, err := eh.open(t); !errors.Is(err, ErrVenueNotFlat) || !errors.Is(err, coordinator.ErrPersist) {
		t.Fatalf("open: %v — want refused before sending, with a failed withdraw", err)
	}
	if len(eh.eng.Held()) != 1 {
		t.Fatalf("held %v", eh.eng.Held())
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if held := eh.eng.RetryReleases(context.Background()); len(held) != 0 {
		t.Fatalf("still held after the file became writable: %v — retried as a Release beside Engine 1's short", held)
	}
	if _, locked := eh.coord.QueryLock("BTCUSDT"); locked {
		t.Error("the lock is still there")
	}
}
