package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"futures-arbitrage-scanner/internal/broker"
)

const (
	venueBinance = "binance_futures"
	venueBybit   = "bybit_linear"
)

// fakeVenue answers positions and resting orders from maps a test writes.
type fakeVenue struct {
	mu        sync.Mutex
	positions map[string]float64
	resting   map[string]int
	posErr    map[string]error
	ordersErr error
	reads     atomic.Int64
}

func newFakeVenue() *fakeVenue {
	return &fakeVenue{positions: map[string]float64{}, resting: map[string]int{}, posErr: map[string]error{}}
}

func (f *fakeVenue) GetPosition(ctx context.Context, market broker.Market, symbol string) (broker.Position, error) {
	f.reads.Add(1)
	if market != broker.MarketFuturesUSDM {
		return broker.Position{}, broker.ErrNotSupported
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.posErr[symbol]; err != nil {
		return broker.Position{}, err
	}
	return broker.Position{Market: market, Symbol: symbol, QtyCoin: f.positions[symbol]}, nil
}

func (f *fakeVenue) OpenOrders(ctx context.Context, market broker.Market, symbol string) ([]broker.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ordersErr != nil {
		return nil, f.ordersErr
	}
	var out []broker.Order
	for s, n := range f.resting {
		if symbol != "" && s != symbol {
			continue
		}
		for i := 0; i < n; i++ {
			out = append(out, broker.Order{Market: market, Symbol: s, Status: broker.OrderStatusNew, ClientOrderID: fmt.Sprintf("%s-%d", s, i)})
		}
	}
	return out, nil
}

func (f *fakeVenue) set(symbol string, qtyCoin float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.positions[symbol] = qtyCoin
}

func (f *fakeVenue) setErr(symbol string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posErr[symbol] = err
}

func (f *fakeVenue) setResting(symbol string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resting[symbol] = n
}

type fixture struct {
	path    string
	binance *fakeVenue
	bybit   *fakeVenue
	symbols []string
	window  time.Duration
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{
		path:    filepath.Join(t.TempDir(), ".paper", "coordinator-locks.json"),
		binance: newFakeVenue(), bybit: newFakeVenue(),
		symbols: []string{"BTCUSDT", "ETHUSDT", "SOLUSDT", "DOGEUSDT", "XRPUSDT"},
		window:  40 * time.Millisecond,
	}
}

func (fx *fixture) open(t *testing.T) (*Coordinator, LoadReport) {
	t.Helper()
	c, report, err := New(Config{
		Path:          fx.path,
		Venues:        []Venue{{Name: venueBinance, Reader: fx.binance}, {Name: venueBybit, Reader: fx.bybit}},
		Symbols:       fx.symbols,
		ContestWindow: fx.window,
		ReadTimeout:   time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, report
}

func (fx *fixture) reconciled(t *testing.T) *Coordinator {
	t.Helper()
	c, _ := fx.open(t)
	if _, err := c.ReconcileActivePositions(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return c
}

func engine2(symbol, intent string, aprOnCapital float64) AcquireRequest {
	return AcquireRequest{Symbol: symbol, Engine: EngineCrossPerp, IntentID: intent,
		Venues:                   []string{venueBinance, venueBybit},
		PriorityAPROnCapitalFrac: aprOnCapital, PriorityAPRBasisVI: "test: sau phí taker 4 lệnh và trượt giá sổ"}
}

func engine1(symbol, intent string, aprOnCapital float64) AcquireRequest {
	return AcquireRequest{Symbol: symbol, Engine: EngineCashAndCarry, IntentID: intent,
		Venues:                   []string{venueBinance},
		PriorityAPROnCapitalFrac: aprOnCapital, PriorityAPRBasisVI: "test: strategy.NetAPR trên vốn"}
}

func readFile(t *testing.T, path string) lockFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	var f lockFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("lock file does not decode: %v\n%s", err, raw)
	}
	return f
}

func TestTryAcquire_RefusesEverythingUntilTheVenuesHaveBeenRead(t *testing.T) {
	fx := newFixture(t)
	c, _ := fx.open(t)
	if _, err := c.TryAcquire(context.Background(), engine2("BTCUSDT", "i1", 0.1)); !errors.Is(err, ErrNotReconciled) {
		t.Fatalf("before any reconcile: %v, want ErrNotReconciled", err)
	}
	if _, err := c.ReconcileActivePositions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TryAcquire(context.Background(), engine2("ADAUSDT", "i1", 0.1)); !errors.Is(err, ErrNotReconciled) {
		t.Errorf("a symbol outside the reconciled universe: %v, want ErrNotReconciled", err)
	}
	if d, err := c.TryAcquire(context.Background(), engine2("BTCUSDT", "i1", 0.1)); err != nil || !d.Granted {
		t.Fatalf("after reconcile: %+v, %v", d, err)
	}
}

// Question 1 of the self-audit, as a test: many requests for ONE idle symbol
// released at the same instant from separate goroutines. Exactly one wins, it is
// the one with the highest figure on capital, every other is refused by name,
// and the file on disk holds exactly the winner.
func TestTryAcquire_ConcurrentRequestsGrantExactlyOneAndItIsTheHighestAPR(t *testing.T) {
	for round := 0; round < 10; round++ {
		fx := newFixture(t)
		fx.window = time.Hour // decided below, once every request has arrived
		c := fx.reconciled(t)

		const n = 40
		aprs := rand.New(rand.NewSource(int64(round))).Perm(n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		type answer struct {
			req AcquireRequest
			dec Decision
			err error
		}
		answers := make([]answer, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				req := engine2("BTCUSDT", fmt.Sprintf("e2-%d", i), float64(aprs[i])/100)
				if i%2 == 0 {
					req = engine1("BTCUSDT", fmt.Sprintf("e1-%d", i), float64(aprs[i])/100)
				}
				<-start
				dec, err := c.TryAcquire(context.Background(), req)
				answers[i] = answer{req, dec, err}
			}(i)
		}
		close(start)
		resolveWhenBids(t, c, "BTCUSDT", n)
		wg.Wait()

		var winners []answer
		for _, a := range answers {
			switch {
			case a.dec.Granted && a.err == nil:
				winners = append(winners, a)
			case a.dec.Granted || a.err == nil:
				t.Fatalf("round %d: Granted=%v with err=%v — the two must agree", round, a.dec.Granted, a.err)
			case !errors.Is(a.err, ErrLostContest):
				t.Fatalf("round %d: a loser got %v, want ErrLostContest", round, a.err)
			}
		}
		if len(winners) != 1 {
			t.Fatalf("round %d: %d winners, want exactly 1", round, len(winners))
		}
		if got := winners[0].req.PriorityAPROnCapitalFrac; got != float64(n-1)/100 {
			t.Fatalf("round %d: the winner bid %v, want the highest %v", round, got, float64(n-1)/100)
		}
		f := readFile(t, fx.path)
		if len(f.Locks) != 1 || f.Locks[0].IntentID != winners[0].req.IntentID || f.Locks[0].State != StateOccupied {
			t.Fatalf("round %d: file holds %+v, want only the winner %q", round, f.Locks, winners[0].req.IntentID)
		}
	}
}

// Engine 1 and Engine 2 in "the same microsecond": whichever ARRIVES first, the
// higher figure wins; on a tie, the earlier arrival wins.
func TestTryAcquire_HigherAPRWinsWhicheverEngineArrivesFirst(t *testing.T) {
	type order struct {
		name           string
		first, second  AcquireRequest
		wantWinnerFrom EngineID
	}
	for _, o := range []order{
		{"engine 1 first, engine 2 higher", engine1("ETHUSDT", "a", 0.04), engine2("ETHUSDT", "b", 0.09), EngineCrossPerp},
		{"engine 2 first, engine 1 higher", engine2("ETHUSDT", "b", 0.03), engine1("ETHUSDT", "a", 0.05), EngineCashAndCarry},
		{"a tie goes to the earlier arrival", engine1("ETHUSDT", "a", 0.05), engine2("ETHUSDT", "b", 0.05), EngineCashAndCarry},
	} {
		t.Run(o.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.window = time.Hour // decided below, once both requests have arrived
			c := fx.reconciled(t)
			var wg sync.WaitGroup
			decs := make([]Decision, 2)
			errs := make([]error, 2)
			wg.Add(2)
			go func() { defer wg.Done(); decs[0], errs[0] = c.TryAcquire(context.Background(), o.first) }()
			// Arrive clearly second, but inside the first one's window.
			waitForBids(t, c, "ETHUSDT", 1)
			go func() { defer wg.Done(); decs[1], errs[1] = c.TryAcquire(context.Background(), o.second) }()
			resolveWhenBids(t, c, "ETHUSDT", 2)
			wg.Wait()

			lock, held := c.QueryLock("ETHUSDT")
			if !held || lock.OwnerEngine != o.wantWinnerFrom {
				t.Fatalf("holder %+v, want %s", lock, o.wantWinnerFrom)
			}
			if decs[0].Granted == decs[1].Granted {
				t.Fatalf("grants %v/%v — exactly one must win", decs[0].Granted, decs[1].Granted)
			}
			if decs[0].Contenders != 2 || decs[1].Contenders != 2 {
				t.Errorf("contenders %d/%d, want both requests compared", decs[0].Contenders, decs[1].Contenders)
			}
		})
	}
}

// resolveWhenBids decides the symbol's contest the moment it holds n bids — what
// the window's timer does, without betting the test on the scheduler delivering
// every goroutine inside a real window. Deciding twice is a no-op.
func resolveWhenBids(t *testing.T, c *Coordinator, symbol string, n int) {
	t.Helper()
	waitForBids(t, c, symbol, n)
	c.mu.Lock()
	ct := c.contests[symbol]
	c.mu.Unlock()
	c.resolve(symbol, ct)
}

// waitForBids blocks until the symbol's open contest holds n bids.
func waitForBids(t *testing.T, c *Coordinator, symbol string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		ct := c.contests[symbol]
		got := 0
		if ct != nil {
			got = len(ct.bids)
		}
		c.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("contest for %s never held %d bids", symbol, n)
}

// A request after the grant is refused at the gate however high its figure: the
// winner may already have orders on the wire.
func TestTryAcquire_AnOccupiedSymbolIsRefusedAtTheGateWithNoPreemption(t *testing.T) {
	fx := newFixture(t)
	c := fx.reconciled(t)
	if _, err := c.TryAcquire(context.Background(), engine1("BTCUSDT", "e1", 0.02)); err != nil {
		t.Fatal(err)
	}
	dec, err := c.TryAcquire(context.Background(), engine2("BTCUSDT", "e2", 9.99))
	if !errors.Is(err, ErrOccupied) || dec.Granted {
		t.Fatalf("a 999%% bid for an occupied symbol: %+v, %v — want ErrOccupied", dec, err)
	}
	if dec.Lock.OwnerEngine != EngineCashAndCarry || !strings.Contains(err.Error(), "engine_1") {
		t.Errorf("the refusal must name the holder: %+v / %v", dec.Lock, err)
	}
	// The same intent asking again is refused too: a lock is taken back after a
	// restart with Adopt, never re-granted to an intent that may already have
	// orders under its ids (review 4.5k, M5).
	if dec, err := c.TryAcquire(context.Background(), engine1("BTCUSDT", "e1", 0.02)); !errors.Is(err, ErrOccupied) || dec.Granted {
		t.Errorf("the holder asking again: %+v, %v — want ErrOccupied", dec, err)
	}
	if dec, err := c.TryAcquire(context.Background(), engine1("BTCUSDT", "e1-other-intent", 0.5)); !errors.Is(err, ErrOccupied) || dec.Granted {
		t.Errorf("the same engine with ANOTHER intent: %v — one intent per symbol", err)
	}
	if !c.Holds("BTCUSDT", EngineCashAndCarry, "e1") || c.Holds("BTCUSDT", EngineCrossPerp, "e1") || c.Holds("BTCUSDT", EngineCashAndCarry, "") {
		t.Error("Holds answers wrongly")
	}
}

func TestTryAcquire_RefusesARequestThatDoesNotDescribeAPosition(t *testing.T) {
	fx := newFixture(t)
	c := fx.reconciled(t)
	bad := map[string]AcquireRequest{}
	r := engine2("BTCUSDT", "i", 0.1)
	r.Venues = []string{venueBinance}
	bad["engine 2 on one venue"] = r
	r = engine1("BTCUSDT", "i", 0.1)
	r.Venues = []string{venueBinance, venueBybit}
	bad["engine 1 on two venues"] = r
	r = engine2("BTCUSDT", "i", 0.1)
	r.Venues = []string{venueBinance, "okx_futures"}
	bad["an unconfigured venue"] = r
	r = engine2("BTCUSDT", "i", 0.1)
	r.Venues = []string{venueBybit, venueBybit}
	bad["one venue twice"] = r
	r = engine2("BTCUSDT", "", 0.1)
	bad["no intent id"] = r
	r = engine2("BTCUSDT", "i", 0.1)
	r.PriorityAPRBasisVI = " "
	bad["a figure with no words for what it deducted"] = r
	r = engine2("BTCUSDT", "i", 0.1)
	r.Engine = "engine_3"
	bad["an unknown engine"] = r
	for name, req := range bad {
		if _, err := c.TryAcquire(context.Background(), req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v, want ErrInvalidRequest", name, err)
		}
	}
}

// A caller that stops waiting never leaves a lock behind — not while the
// contest is open, and not after it won.
func TestTryAcquire_ACancelledCallerNeverHoldsALock(t *testing.T) {
	fx := newFixture(t)
	fx.window = 60 * time.Millisecond
	c := fx.reconciled(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if dec, err := c.TryAcquire(ctx, engine2("SOLUSDT", "gone", 0.3)); !errors.Is(err, context.DeadlineExceeded) || dec.Granted {
		t.Fatalf("%+v, %v — want the caller's deadline", dec, err)
	}
	time.Sleep(fx.window * 2)
	if l, held := c.QueryLock("SOLUSDT"); held {
		t.Fatalf("a withdrawn request left %+v", l)
	}
	if d, err := c.TryAcquire(context.Background(), engine1("SOLUSDT", "next", 0.01)); err != nil || !d.Granted {
		t.Fatalf("the symbol after a withdrawal: %+v, %v", d, err)
	}

	// Granted in the instant the caller gave up: returned, never held.
	fx2 := newFixture(t)
	c2 := fx2.reconciled(t)
	ct := &contest{done: make(chan struct{}), resolved: true}
	b := &bid{req: engine2("XRPUSDT", "late", 0.2), dec: Decision{Granted: true}}
	c2.mu.Lock()
	c2.versions++
	c2.locks["XRPUSDT"] = &entry{version: c2.versions, liveOwner: true, lock: SymbolLock{Symbol: "XRPUSDT", State: StateOccupied, OwnerEngine: EngineCrossPerp, IntentID: "late"}}
	c2.mu.Unlock()
	if _, err := c2.withdrawBid("XRPUSDT", ct, b, context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("withdraw: %v", err)
	}
	if l, held := c2.QueryLock("XRPUSDT"); held {
		t.Fatalf("a grant the caller never received is still held: %+v", l)
	}
}

// Release is proven on the venues: every venue's position exactly zero and no
// order resting — never the caller's word.
func TestRelease_OnlyTheOwnerAndOnlyWhenEveryVenueIsFlatAndQuiet(t *testing.T) {
	fx := newFixture(t)
	c := fx.reconciled(t)
	if _, err := c.TryAcquire(context.Background(), engine2("ETHUSDT", "pair-1", 0.1)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := c.Release(ctx, "ETHUSDT", EngineCashAndCarry, "pair-1"); !errors.Is(err, ErrNotOwner) {
		t.Errorf("the other engine releasing: %v, want ErrNotOwner", err)
	}
	if _, err := c.Release(ctx, "ETHUSDT", EngineCrossPerp, "pair-2"); !errors.Is(err, ErrNotOwner) {
		t.Errorf("another intent releasing: %v, want ErrNotOwner", err)
	}

	fx.binance.set("ETHUSDT", 0.5)
	fx.bybit.set("ETHUSDT", -0.5)
	if rep, err := c.Release(ctx, "ETHUSDT", EngineCrossPerp, "pair-1"); !errors.Is(err, ErrVenueNotFlat) || rep.Released {
		t.Fatalf("both legs open: %+v, %v — want ErrVenueNotFlat and the lock kept", rep, err)
	}
	fx.binance.set("ETHUSDT", 0)
	if _, err := c.Release(ctx, "ETHUSDT", EngineCrossPerp, "pair-1"); !errors.Is(err, ErrVenueNotFlat) {
		t.Fatalf("one leg still short on Bybit: %v — a lock released now would let the other engine net it", err)
	}
	fx.bybit.set("ETHUSDT", 0)
	fx.bybit.setResting("ETHUSDT", 1)
	if _, err := c.Release(ctx, "ETHUSDT", EngineCrossPerp, "pair-1"); !errors.Is(err, ErrVenueNotFlat) {
		t.Fatalf("flat with an order still resting: %v — want the lock kept", err)
	}
	fx.bybit.setResting("ETHUSDT", 0)
	fx.bybit.setErr("ETHUSDT", errors.New("HTTP 503"))
	if _, err := c.Release(ctx, "ETHUSDT", EngineCrossPerp, "pair-1"); !errors.Is(err, ErrVenueUnreadable) {
		t.Fatalf("an unreadable venue: %v — want the lock kept", err)
	}
	if _, held := c.QueryLock("ETHUSDT"); !held {
		t.Fatal("a refused release dropped the lock")
	}
	fx.bybit.setErr("ETHUSDT", nil)
	rep, err := c.Release(ctx, "ETHUSDT", EngineCrossPerp, "pair-1")
	if err != nil || !rep.Released {
		t.Fatalf("flat and quiet everywhere: %+v, %v", rep, err)
	}
	if len(readFile(t, fx.path).Locks) != 0 {
		t.Error("the file still holds the released lock")
	}
	if _, err := c.Release(ctx, "ETHUSDT", EngineCrossPerp, "pair-1"); !errors.Is(err, ErrNotLocked) {
		t.Errorf("a second release: %v, want ErrNotLocked", err)
	}
}

// A process restarts: the locks come back from the JSON file, nothing is granted
// until the venues are read, and a lock whose venues agree keeps its owner.
func TestRestart_LocksComeBackFromTheFileAndAreCheckedAgainstTheVenues(t *testing.T) {
	fx := newFixture(t)
	c := fx.reconciled(t)
	if _, err := c.TryAcquire(context.Background(), engine2("ETHUSDT", "pair-eth", 0.12)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TryAcquire(context.Background(), engine1("BTCUSDT", "cc-btc", 0.05)); err != nil {
		t.Fatal(err)
	}
	fx.binance.set("ETHUSDT", 1.2)
	fx.bybit.set("ETHUSDT", -1.2)
	fx.binance.set("BTCUSDT", -0.01)

	restarted, load := fx.open(t)
	if !load.Found || load.Locks != 2 {
		t.Fatalf("load report %+v, want the file's 2 locks", load)
	}
	if _, err := restarted.TryAcquire(context.Background(), engine1("SOLUSDT", "x", 0.01)); !errors.Is(err, ErrNotReconciled) {
		t.Fatalf("before reconciling: %v", err)
	}
	report, err := restarted.ReconcileActivePositions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"ETHUSDT", "BTCUSDT"} {
		l, held := restarted.QueryLock(s)
		if !held || l.State != StateOccupied {
			t.Fatalf("%s after restart: %+v (report %+v)", s, l, report)
		}
	}
	if l, _ := restarted.QueryLock("ETHUSDT"); l.OwnerEngine != EngineCrossPerp || l.IntentID != "pair-eth" || l.Source != SourceAcquired {
		t.Errorf("ETH lock lost its owner across the restart: %+v", l)
	}
	if !strings.Contains(mustLock(t, restarted, "ETHUSDT").EvidenceVI, "+1.2") {
		t.Errorf("the reconcile did not record the venues' evidence: %q", mustLock(t, restarted, "ETHUSDT").EvidenceVI)
	}

	// The restarted engine closes, the venues go flat, and the owner releases.
	fx.binance.set("ETHUSDT", 0)
	fx.bybit.set("ETHUSDT", 0)
	if _, err := restarted.Release(context.Background(), "ETHUSDT", EngineCrossPerp, "pair-eth"); err != nil {
		t.Fatalf("release after restart: %v", err)
	}
}

func mustLock(t *testing.T, c *Coordinator, symbol string) SymbolLock {
	t.Helper()
	l, held := c.QueryLock(symbol)
	if !held {
		t.Fatalf("%s is not locked", symbol)
	}
	return l
}

// A corrupt file is moved aside — it is evidence — and the coordinator starts
// empty, grants nothing, and re-derives occupancy from the venues.
func TestLoad_ACorruptFileIsMovedAsideAndTheVenuesDecide(t *testing.T) {
	fx := newFixture(t)
	if err := os.MkdirAll(filepath.Dir(fx.path), 0o755); err != nil {
		t.Fatal(err)
	}
	garbage := []byte(`{"version":1,"locks":[{"symbol":"BTCUSDT","state":"occup`)
	if err := os.WriteFile(fx.path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	fx.binance.set("BTCUSDT", -0.02)

	c, load := fx.open(t)
	if load.CorruptMovedTo == "" {
		t.Fatalf("load report %+v, want the corrupt file moved aside", load)
	}
	if kept, err := os.ReadFile(load.CorruptMovedTo); err != nil || string(kept) != string(garbage) {
		t.Fatalf("the corrupt file was not preserved byte for byte: %v", err)
	}
	if _, err := c.TryAcquire(context.Background(), engine2("BTCUSDT", "e2", 1)); !errors.Is(err, ErrNotReconciled) {
		t.Fatalf("before reconcile: %v", err)
	}
	if _, err := c.ReconcileActivePositions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l := mustLock(t, c, "BTCUSDT"); l.OwnerEngine != EngineCashAndCarry || l.Source != SourceReconciledInferred {
		t.Errorf("BTC after a corrupt file: %+v, want inferred Engine 1", l)
	}
}

// Question 4 of the self-audit, as a test: Engine 1 holds a short BTC perp on
// Binance, the lock file is LOST, and Engine 2 asks for BTC. The venues remember
// what the file forgot.
func TestReconcile_ALostLockFileNeverLetsEngine2OpenAgainstEngine1(t *testing.T) {
	fx := newFixture(t)
	c := fx.reconciled(t)
	if _, err := c.TryAcquire(context.Background(), engine1("BTCUSDT", "cc-btc", 0.05)); err != nil {
		t.Fatal(err)
	}
	fx.binance.set("BTCUSDT", -0.05) // Engine 1's perp leg

	if err := os.Remove(fx.path); err != nil {
		t.Fatal(err)
	}
	restarted := fx.reconciled(t)
	dec, err := restarted.TryAcquire(context.Background(), engine2("BTCUSDT", "e2-btc", 5))
	if !errors.Is(err, ErrOccupied) || dec.Granted {
		t.Fatalf("Engine 2 after the file was lost: %+v, %v — want ErrOccupied", dec, err)
	}
	if dec.Lock.OwnerEngine != EngineCashAndCarry {
		t.Errorf("attributed to %s, want Engine 1 (a short-only shape)", dec.Lock.OwnerEngine)
	}
}

func TestReconcile_AttributesEveryShapeOrCallsItAConflict(t *testing.T) {
	fx := newFixture(t)
	fx.binance.set("BTCUSDT", -0.01)                   // Engine 1 on Binance
	fx.binance.set("ETHUSDT", 0.4)                     // Engine 2: long Binance
	fx.bybit.set("ETHUSDT", -0.4)                      //           short Bybit
	fx.bybit.set("SOLUSDT", 5)                         // a naked long: nobody's shape
	fx.bybit.setErr("XRPUSDT", errors.New("HTTP 403")) // unreadable
	fx.binance.setResting("DOGEUSDT", 2)               // flat, but somebody is working orders
	c := fx.reconciled(t)

	if l := mustLock(t, c, "BTCUSDT"); l.State != StateOccupied || l.OwnerEngine != EngineCashAndCarry {
		t.Errorf("BTC short only: %+v", l)
	}
	if l := mustLock(t, c, "ETHUSDT"); l.State != StateOccupied || l.OwnerEngine != EngineCrossPerp || l.IntentID != "" {
		t.Errorf("ETH long/short: %+v", l)
	}
	if l := mustLock(t, c, "SOLUSDT"); l.State != StateConflict {
		t.Errorf("SOL naked long: %+v, want a conflict", l)
	}
	for _, s := range []string{"XRPUSDT", "DOGEUSDT"} {
		if _, err := c.TryAcquire(context.Background(), engine1(s, "x", 0.1)); !errors.Is(err, ErrNotReconciled) {
			t.Errorf("%s: %v, want ErrNotReconciled", s, err)
		}
	}
	if _, err := c.TryAcquire(context.Background(), engine2("SOLUSDT", "x", 0.1)); !errors.Is(err, ErrConflict) {
		t.Errorf("SOL: %v, want ErrConflict", err)
	}

	// Once the unreadable venue answers and the orders are gone, the symbols
	// are known again; the inferred Engine-1 lock releases itself when its
	// position is gone.
	fx.bybit.setErr("XRPUSDT", nil)
	fx.binance.setResting("DOGEUSDT", 0)
	fx.binance.set("BTCUSDT", 0)
	if _, err := c.ReconcileActivePositions(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"XRPUSDT", "DOGEUSDT", "BTCUSDT"} {
		if d, err := c.TryAcquire(context.Background(), engine1(s, "later-"+s, 0.1)); err != nil || !d.Granted {
			t.Errorf("%s after the evidence cleared: %+v, %v", s, d, err)
		}
	}
}

// The file says Engine 2 holds ETH across Binance and Bybit; the venues show one
// short leg. That is the shape a crash between two legs leaves: a conflict, with
// both stories in it, that nobody but an operator clears.
func TestReconcile_AFileOwnerTheVenuesContradictBecomesAConflict(t *testing.T) {
	fx := newFixture(t)
	c := fx.reconciled(t)
	if _, err := c.TryAcquire(context.Background(), engine2("ETHUSDT", "pair-eth", 0.2)); err != nil {
		t.Fatal(err)
	}
	fx.bybit.set("ETHUSDT", -0.7)

	restarted := fx.reconciled(t)
	l := mustLock(t, restarted, "ETHUSDT")
	if l.State != StateConflict || !strings.Contains(l.Details, "engine_2_cross_perp") || !strings.Contains(l.EvidenceVI, "-0.7") {
		t.Fatalf("ETH: %+v — want a conflict naming the file's owner and the venues' evidence", l)
	}
	if _, err := restarted.Release(context.Background(), "ETHUSDT", EngineCrossPerp, "pair-eth"); !errors.Is(err, ErrConflict) {
		t.Errorf("the recorded owner releasing a conflict: %v, want ErrConflict", err)
	}
	if _, err := restarted.ClearConflict(context.Background(), "ETHUSDT", l.EvidenceAtMs); !errors.Is(err, ErrVenueNotFlat) {
		t.Errorf("clearing while a leg is still open: %v, want ErrVenueNotFlat", err)
	}
	if _, err := restarted.ClearConflict(context.Background(), "ETHUSDT", l.EvidenceAtMs-1); !errors.Is(err, ErrLockChanged) {
		t.Errorf("clearing with an older look at the evidence: %v, want ErrLockChanged", err)
	}
	fx.bybit.set("ETHUSDT", 0)
	if rep, err := restarted.ClearConflict(context.Background(), "ETHUSDT", l.EvidenceAtMs); err != nil || !rep.Released {
		t.Fatalf("operator clearing a flat conflict: %+v, %v", rep, err)
	}
}

// An engine alive in this process passes through shapes that are not its final
// one. Reconcile reports that and changes nothing.
func TestReconcile_ALiveOwnerMidOperationIsReportedNotFlipped(t *testing.T) {
	fx := newFixture(t)
	c := fx.reconciled(t)
	if _, err := c.TryAcquire(context.Background(), engine2("ETHUSDT", "pair-eth", 0.2)); err != nil {
		t.Fatal(err)
	}
	fx.binance.set("ETHUSDT", 0.3) // the long leg filled, the short is still working
	report, err := c.ReconcileActivePositions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	l := mustLock(t, c, "ETHUSDT")
	if l.State != StateOccupied || l.IntentID != "pair-eth" {
		t.Fatalf("a live owner mid-open was flipped: %+v", l)
	}
	found := false
	for _, row := range report.Symbols {
		found = found || row.Symbol == "ETHUSDT" && strings.Contains(row.ActionVI, "KHÔNG KHỚP")
	}
	if !found {
		t.Errorf("the report does not say the shape disagreed: %+v", report.Symbols)
	}
}

func TestAdopt_BindsAnInferredLockToTheRecoveredIntent(t *testing.T) {
	fx := newFixture(t)
	fx.binance.set("ETHUSDT", 0.4)
	fx.bybit.set("ETHUSDT", -0.4)
	c := fx.reconciled(t)
	if _, err := c.Adopt("ETHUSDT", EngineCashAndCarry, "x"); !errors.Is(err, ErrNotOwner) {
		t.Errorf("the wrong engine adopting: %v", err)
	}
	l, err := c.Adopt("ETHUSDT", EngineCrossPerp, "recovered-eth")
	if err != nil || l.IntentID != "recovered-eth" {
		t.Fatalf("adopt: %+v, %v", l, err)
	}
	if !c.Holds("ETHUSDT", EngineCrossPerp, "recovered-eth") {
		t.Error("the adopted intent does not hold the lock")
	}
	if f := readFile(t, fx.path); len(f.Locks) != 1 || f.Locks[0].IntentID != "recovered-eth" {
		t.Errorf("the adoption was not written: %+v", f.Locks)
	}
	if _, err := c.Adopt("ETHUSDT", EngineCrossPerp, "someone-else"); !errors.Is(err, ErrNotOwner) {
		t.Errorf("a second adoption under another intent: %v", err)
	}
}

func TestNew_RefusesAConfigurationThatCannotVerifyALock(t *testing.T) {
	v := newFakeVenue()
	cases := map[string]Config{
		"no venue":      {Symbols: []string{"BTCUSDT"}},
		"no reader":     {Venues: []Venue{{Name: venueBinance}}, Symbols: []string{"BTCUSDT"}},
		"nameless":      {Venues: []Venue{{Reader: v}}, Symbols: []string{"BTCUSDT"}},
		"twice":         {Venues: []Venue{{Name: venueBinance, Reader: v}, {Name: venueBinance, Reader: v}}, Symbols: []string{"BTCUSDT"}},
		"no universe":   {Venues: []Venue{{Name: venueBinance, Reader: v}}},
		"blank symbols": {Venues: []Venue{{Name: venueBinance, Reader: v}}, Symbols: []string{""}},
	}
	for name, cfg := range cases {
		cfg.Path = filepath.Join(t.TempDir(), "locks.json")
		if _, _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Review 4.5k, M5: a lock that came back from the file is not re-granted to its
// own intent — before the venues are read or after.
func TestTryAcquire_AFileLockIsNeverReGrantedToItsOwnIntent(t *testing.T) {
	fx := newFixture(t)
	if err := os.MkdirAll(filepath.Dir(fx.path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"written_at_ms":1,"locks":[{"symbol":"ETHUSDT","state":"occupied","owner_engine":"engine_2_cross_perp","intent_id":"pair-eth","venues":["binance_futures","bybit_linear"],"source":"acquired"}]}`
	if err := os.WriteFile(fx.path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := fx.open(t)
	if dec, err := c.TryAcquire(context.Background(), engine2("ETHUSDT", "pair-eth", 0.1)); dec.Granted || err == nil {
		t.Fatalf("before any reconcile: %+v, %v — want refused", dec, err)
	}
	fx.binance.set("ETHUSDT", 0.4)
	fx.bybit.set("ETHUSDT", -0.4)
	if _, err := c.ReconcileActivePositions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dec, err := c.TryAcquire(context.Background(), engine2("ETHUSDT", "pair-eth", 0.1)); dec.Granted || !errors.Is(err, ErrOccupied) {
		t.Fatalf("after reconcile: %+v, %v — want ErrOccupied; the way back is Adopt", dec, err)
	}
	if _, err := c.Adopt("ETHUSDT", EngineCrossPerp, "pair-eth"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
}

// Review 4.5k, M6: a lock under which nothing was sent is given back without a
// venue proof — and only such a lock.
func TestWithdraw_GivesBackOnlyAFreshGrantUnderWhichNothingWasSent(t *testing.T) {
	fx := newFixture(t)
	c := fx.reconciled(t)
	if _, err := c.TryAcquire(context.Background(), engine2("BTCUSDT", "e2", 0.1)); err != nil {
		t.Fatal(err)
	}
	fx.binance.set("BTCUSDT", -0.05) // Engine 1 opened meanwhile, bypassing the lock
	if _, err := c.Release(context.Background(), "BTCUSDT", EngineCrossPerp, "e2"); !errors.Is(err, ErrVenueNotFlat) {
		t.Fatalf("release: %v", err)
	}
	if err := c.Withdraw("BTCUSDT", EngineCashAndCarry, "e2"); !errors.Is(err, ErrNotOwner) {
		t.Errorf("another engine withdrawing: %v", err)
	}
	if err := c.Withdraw("BTCUSDT", EngineCrossPerp, "e2"); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if _, held := c.QueryLock("BTCUSDT"); held {
		t.Fatal("a withdrawn lock is still held")
	}
	if len(readFile(t, fx.path).Locks) != 0 {
		t.Error("the withdrawal was not written")
	}
	// The next reconcile attributes what the venues hold.
	if _, err := c.ReconcileActivePositions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l := mustLock(t, c, "BTCUSDT"); l.OwnerEngine != EngineCashAndCarry {
		t.Errorf("after withdraw + reconcile: %+v", l)
	}

	// A lock from the file cannot be withdrawn — its intent may have sent.
	fx2 := newFixture(t)
	c2 := fx2.reconciled(t)
	if _, err := c2.TryAcquire(context.Background(), engine2("ETHUSDT", "pair-eth", 0.1)); err != nil {
		t.Fatal(err)
	}
	restarted := fx2.reconciled(t)
	if err := restarted.Withdraw("ETHUSDT", EngineCrossPerp, "pair-eth"); !errors.Is(err, ErrNotWithdrawable) {
		t.Errorf("withdrawing a lock from the file: %v", err)
	}
}

// Review 4.5k, M7 then N4: a long on one venue and a short on another is Engine
// 2's shape whatever the sizes — Engine 1 holds no long perp — so it is Engine
// 2's lock, with the imbalance written into it. Whether it is a HEDGE depends on
// the two venues' grids, which Engine 2 judges when it adopts; a conflict here
// made a pair the executor keeps unclosable after a restart.
func TestReconcile_AnUnbalancedCrossShapeIsEngine2sAndSaysSo(t *testing.T) {
	fx := newFixture(t)
	fx.binance.set("BTCUSDT", 1.0)
	fx.bybit.set("BTCUSDT", -0.1)
	fx.binance.set("ETHUSDT", 0.049) // one step of a 0.001 grid below 0.05: 2% apart
	fx.bybit.set("ETHUSDT", -0.05)
	fx.binance.set("SOLUSDT", 5)
	fx.binance.set("DOGEUSDT", 1_000) // two longs: nobody's shape
	fx.bybit.set("DOGEUSDT", 1_000)
	c := fx.reconciled(t)
	for _, sym := range []string{"BTCUSDT", "ETHUSDT"} {
		l := mustLock(t, c, sym)
		if l.State != StateOccupied || l.OwnerEngine != EngineCrossPerp || !strings.Contains(l.Details, "LỆCH") {
			t.Errorf("%s: %+v — want Engine 2's lock that names the imbalance", sym, l)
		}
	}
	if l := mustLock(t, c, "DOGEUSDT"); l.State != StateConflict {
		t.Errorf("two longs: %+v, want a conflict", l)
	}
	// A file lock of Engine 2 whose legs have drifted apart stays Engine 2's.
	fx2 := newFixture(t)
	c2 := fx2.reconciled(t)
	if _, err := c2.TryAcquire(context.Background(), engine2("ETHUSDT", "pair-eth", 0.1)); err != nil {
		t.Fatal(err)
	}
	fx2.binance.set("ETHUSDT", 0.333)
	fx2.bybit.set("ETHUSDT", -0.2)
	restarted := fx2.reconciled(t)
	if l := mustLock(t, restarted, "ETHUSDT"); l.State != StateOccupied || l.IntentID != "pair-eth" {
		t.Errorf("a drifted Engine-2 pair from the file: %+v", l)
	}
}

// Review 4.5k, m-g: the mark that orders were sent is written before it is
// reported, survives a restart, and ends every Withdraw of that lock.
func TestMarkOrdersSent_IsDurableAndEndsWithdraw(t *testing.T) {
	fx := newFixture(t)
	c := fx.reconciled(t)
	if _, err := c.TryAcquire(context.Background(), engine2("BTCUSDT", "e2", 0.1)); err != nil {
		t.Fatal(err)
	}
	if err := c.MarkOrdersSent("BTCUSDT", EngineCrossPerp, "someone-else"); !errors.Is(err, ErrNotOwner) {
		t.Errorf("marking another intent's lock: %v", err)
	}
	if err := c.MarkOrdersSent("ETHUSDT", EngineCrossPerp, "e2"); !errors.Is(err, ErrNotLocked) {
		t.Errorf("marking an idle symbol: %v", err)
	}
	if err := c.MarkOrdersSent("BTCUSDT", EngineCrossPerp, "e2"); err != nil {
		t.Fatal(err)
	}
	f := readFile(t, fx.path)
	if len(f.Locks) != 1 || f.Locks[0].OrdersSentAtMs == 0 {
		t.Fatalf("the mark is not on disk: %+v", f.Locks)
	}
	stamp := f.Locks[0].OrdersSentAtMs
	if err := c.MarkOrdersSent("BTCUSDT", EngineCrossPerp, "e2"); err != nil || readFile(t, fx.path).Locks[0].OrdersSentAtMs != stamp {
		t.Errorf("a second mark moved the first: %v", err)
	}
	if err := c.Withdraw("BTCUSDT", EngineCrossPerp, "e2"); !errors.Is(err, ErrNotWithdrawable) {
		t.Fatalf("withdrawing a marked lock: %v", err)
	}
	restarted := fx.reconciled(t)
	if l := mustLock(t, restarted, "BTCUSDT"); l.OrdersSentAtMs != stamp {
		t.Errorf("after restart: %+v", l)
	}
	// Adopted in a new process: still not withdrawable, marked or not.
	if _, err := restarted.Adopt("BTCUSDT", EngineCrossPerp, "e2"); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Withdraw("BTCUSDT", EngineCrossPerp, "e2"); !errors.Is(err, ErrNotWithdrawable) {
		t.Errorf("withdrawing an adopted lock: %v", err)
	}

	// A lock from the file with NO mark — the process died between the grant and
	// its first order — is not withdrawable after it is adopted either: this
	// process does not know that lock's history, only Release reads it.
	fx3 := newFixture(t)
	c3 := fx3.reconciled(t)
	if _, err := c3.TryAcquire(context.Background(), engine2("ETHUSDT", "pair-eth", 0.1)); err != nil {
		t.Fatal(err)
	}
	restarted3 := fx3.reconciled(t)
	if _, err := restarted3.Adopt("ETHUSDT", EngineCrossPerp, "pair-eth"); err != nil {
		t.Fatal(err)
	}
	if err := restarted3.Withdraw("ETHUSDT", EngineCrossPerp, "pair-eth"); !errors.Is(err, ErrNotWithdrawable) {
		t.Errorf("withdrawing an adopted, unmarked lock from the file: %v", err)
	}
}

// Review 4.5k round 3, minor 4: Adopt is for a lock recovered from the file or
// inferred from the venues — never for one its owner was granted in this process.
func TestAdopt_RefusesALockGrantedInThisProcess(t *testing.T) {
	fx := newFixture(t)
	c := fx.reconciled(t)
	if _, err := c.TryAcquire(context.Background(), engine2("ETHUSDT", "pair-eth", 0.1)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Adopt("ETHUSDT", EngineCrossPerp, "pair-eth"); !errors.Is(err, ErrOwnerLive) {
		t.Fatalf("adopting a lock granted here: %v", err)
	}
	restarted := fx.reconciled(t)
	if _, err := restarted.Adopt("ETHUSDT", EngineCrossPerp, "pair-eth"); err != nil {
		t.Fatalf("adopting the same lock after a restart: %v", err)
	}
}
