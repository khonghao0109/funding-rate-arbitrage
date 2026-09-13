package brokertest_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/broker/brokertest"
)

func TestFake_SatisfiesTheBrokerContract(t *testing.T) {
	brokertest.RunBrokerContract(t, func(t *testing.T) broker.Broker { return brokertest.New() })
}

// The case the package exists for: PlaceOrder fails, and the caller cannot tell
// from the error whether the venue has the order. Both worlds are built here,
// and the ONLY thing that distinguishes them is a lookup by the id the caller
// chose before sending.
func TestFake_AnAmbiguousTimeoutIsResolvedByTheClientOrderID(t *testing.T) {
	ctx := context.Background()
	const id = "recover-1"
	req := broker.PlaceOrderRequest{
		Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT",
		Side: broker.SideBuy, Type: broker.OrderTypeLimitGTC,
		ClientOrderID: id, QtyCoin: 0.01, PriceQuote: 10_000,
	}

	cases := []struct {
		name      string
		behaviour brokertest.Behaviour
		arrived   bool
	}{
		{"the venue accepted it and the answer was lost", brokertest.Behaviour{PlaceTimesOutAfterAccepting: true}, true},
		{"the request never reached the venue", brokertest.Behaviour{PlaceTimesOutBeforeAccepting: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := brokertest.New()
			f.SetBehaviour(tc.behaviour)

			_, err := f.PlaceOrder(ctx, req)
			if !errors.Is(err, brokertest.ErrTimeout) {
				t.Fatalf("PlaceOrder err = %v, want a timeout", err)
			}
			// A timeout must NOT read as "no such order": that is precisely
			// the conflation that produces a double fill.
			if errors.Is(err, broker.ErrOrderNotFound) {
				t.Fatal("the timeout satisfies errors.Is(err, ErrOrderNotFound); a caller branching on that would resend an order the venue may hold")
			}

			_, err = f.GetOrder(ctx, broker.OrderQuery{
				Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", ClientOrderID: id,
			})
			switch {
			case tc.arrived && err != nil:
				t.Fatalf("the venue holds the order but GetOrder said %v — the caller would resend and double the position", err)
			case !tc.arrived && !errors.Is(err, broker.ErrOrderNotFound):
				t.Fatalf("the venue never got the order but GetOrder said %v — the caller would wait for a leg that does not exist", err)
			}
		})
	}
}

// A partial fill, which is the state step 4.4 must resolve and cannot produce
// on demand against a real venue.
func TestFake_PartialFillKeepsAQuantityWeightedAverageAndIsGross(t *testing.T) {
	ctx := context.Background()
	f := brokertest.New()
	const id = "partial-1"
	req := broker.PlaceOrderRequest{
		Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT",
		Side: broker.SideBuy, Type: broker.OrderTypeLimitGTC,
		ClientOrderID: id, QtyCoin: 1.0, PriceQuote: 10_000,
	}
	if _, err := f.PlaceOrder(ctx, req); err != nil {
		t.Fatal(err)
	}

	// Two fills at different prices: 0.25 at 10,000 and 0.25 at 10,200.
	if err := f.Fill(id, 0.25, 10_000); err != nil {
		t.Fatal(err)
	}
	if err := f.Fill(id, 0.25, 10_200); err != nil {
		t.Fatal(err)
	}

	got, err := f.GetOrder(ctx, broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", ClientOrderID: id})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != broker.OrderStatusPartiallyFilled {
		t.Errorf("status = %q, want PARTIALLY_FILLED", got.Status)
	}
	if math.Abs(got.FilledQtyCoin-0.5) > 1e-9 {
		t.Errorf("FilledQtyCoin = %v, want 0.5", got.FilledQtyCoin)
	}
	if math.Abs(got.RemainingQtyCoin()-0.5) > 1e-9 {
		t.Errorf("RemainingQtyCoin = %v, want 0.5", got.RemainingQtyCoin())
	}
	// Quantity-weighted, not the last price and not the mean of the two.
	if want := 10_100.0; math.Abs(got.AvgFillPriceQuote-want) > 1e-9 {
		t.Errorf("AvgFillPriceQuote = %v, want %v", got.AvgFillPriceQuote, want)
	}
	// GROSS: 0.5 x 10,100, with no commission taken off anywhere.
	if want := 5_050.0; math.Abs(got.FilledNotionalQuote()-want) > 1e-9 {
		t.Errorf("FilledNotionalQuote = %v, want %v gross", got.FilledNotionalQuote(), want)
	}

	// Filling the rest completes it.
	if err := f.Fill(id, 0.5, 10_100); err != nil {
		t.Fatal(err)
	}
	got, _ = f.GetOrder(ctx, broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", ClientOrderID: id})
	if got.Status != broker.OrderStatusFilled {
		t.Errorf("status = %q, want FILLED", got.Status)
	}
	if !got.Status.Done() {
		t.Error("FILLED must report Done")
	}
	// Overfilling is a test bug and is refused rather than silently absorbed.
	if err := f.Fill(id, 0.1, 10_100); err == nil {
		t.Error("filling past the order quantity must be refused")
	}
}

// The race a two-leg unwind has to survive: the cancel arrives after the fill.
func TestFake_ACancelThatRacesAFillDoesNotReportSuccess(t *testing.T) {
	ctx := context.Background()
	f := brokertest.New()
	const id = "race-1"
	if _, err := f.PlaceOrder(ctx, broker.PlaceOrderRequest{
		Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT",
		Side: broker.SideSell, Type: broker.OrderTypeLimitGTC,
		ClientOrderID: id, QtyCoin: 0.01, PriceQuote: 90_000,
	}); err != nil {
		t.Fatal(err)
	}
	f.SetBehaviour(brokertest.Behaviour{CancelRacesAFill: true})

	q := broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", ClientOrderID: id}
	order, err := f.CancelOrder(ctx, q)
	if err == nil {
		t.Fatal("cancelling an order that has already filled reported success; the caller now believes it has no position")
	}
	if order.Status != broker.OrderStatusFilled {
		t.Errorf("the returned order says %q; the caller needs to see it FILLED to know it must now unwind", order.Status)
	}
}

// Position and balance come back as the venue's own state (rule 7), and the
// unrealized figure is gross.
func TestFake_PositionAndBalanceReadBackWhatTheVenueHolds(t *testing.T) {
	ctx := context.Background()
	f := brokertest.New()

	flat, err := f.GetPosition(ctx, broker.MarketFuturesUSDM, "BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if !flat.Flat() {
		t.Errorf("an unknown symbol must read flat, got %v", flat.QtyCoin)
	}

	f.SetPosition(broker.Position{
		Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT",
		QtyCoin: -0.5, EntryPriceQuote: 60_000, MarkPriceQuote: 59_000,
		UnrealizedPnLQuote: 500,
	})
	p, err := f.GetPosition(ctx, broker.MarketFuturesUSDM, "BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	// Signed, so a short cannot be read as a long by ignoring a side field.
	if p.QtyCoin >= 0 {
		t.Errorf("QtyCoin = %v, want negative for a short", p.QtyCoin)
	}
	if p.Flat() {
		t.Error("a position of -0.5 reports Flat")
	}

	f.SetBalance(broker.MarketSpot,
		broker.Balance{Market: broker.MarketSpot, Asset: "USDT", FreeQtyCoin: 1000, LockedQtyCoin: 250},
		broker.Balance{Market: broker.MarketSpot, Asset: "BTC"},
	)
	balances, err := f.GetBalance(ctx, broker.MarketSpot)
	if err != nil {
		t.Fatal(err)
	}
	if len(balances) != 2 {
		t.Fatalf("got %d balances, want 2", len(balances))
	}
	// Locked counts: a balance committed to a resting order is still balance.
	if want := 1250.0; balances[0].TotalQtyCoin() != want {
		t.Errorf("TotalQtyCoin = %v, want %v (free + locked)", balances[0].TotalQtyCoin(), want)
	}
	if futures, err := f.GetBalance(ctx, broker.MarketFuturesUSDM); err != nil || len(futures) != 0 {
		t.Errorf("an unset market must be empty, not an error: %v, %v", futures, err)
	}
}

// A duplicate ClientOrderID must be refused: the id is the caller's handle, and
// two orders sharing one makes the recovery lookup ambiguous again.
func TestFake_ADuplicateClientOrderIDIsRefused(t *testing.T) {
	ctx := context.Background()
	f := brokertest.New()
	req := broker.PlaceOrderRequest{
		Market: broker.MarketSpot, Symbol: "BTCUSDT",
		Side: broker.SideBuy, Type: broker.OrderTypeLimitGTC,
		ClientOrderID: "dupe-1", QtyCoin: 0.01, PriceQuote: 10_000,
	}
	if _, err := f.PlaceOrder(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := f.PlaceOrder(ctx, req); !errors.Is(err, broker.ErrInvalidOrder) {
		t.Fatalf("err = %v, want ErrInvalidOrder for a reused id", err)
	}
}
