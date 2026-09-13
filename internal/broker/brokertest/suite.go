package brokertest

import (
	"context"
	"errors"
	"testing"

	"futures-arbitrage-scanner/internal/broker"
)

// RunBrokerContract exercises the behaviours EVERY broker.Broker must have,
// whatever it talks to.
//
// It is written once, here, rather than in each implementation's own tests, for
// the reason internal/backtest already applies to strategy: a rule asserted in
// one place and re-described in another drifts.
//
// Only the fake runs it as a unit test, and that limit is worth stating rather
// than glossing: this suite places and cancels orders, so running it against
// the Binance implementation would mean a live venue and a credential, which
// `go test` may not have. What holds that implementation to the same contract
// is cmd/brokercheck's acceptance run, which performs this same sequence —
// place, look up by the caller's id, cancel, re-read, confirm it has left the
// open orders, and confirm a second cancel reports not-found — against the real
// testnet, plus the golden tests that replay what the venue actually answered.
//
// newBroker must return a broker with no orders on it. The suite only ever
// places orders it invents, and cleans nothing up: give it a fresh one.
func RunBrokerContract(t *testing.T, newBroker func(t *testing.T) broker.Broker) {
	t.Helper()
	ctx := context.Background()
	const sym = "BTCUSDT"

	// A request that rests rather than fills, so the order is still there to
	// query and cancel.
	resting := func(id string) broker.PlaceOrderRequest {
		return broker.PlaceOrderRequest{
			Market: broker.MarketFuturesUSDM, Symbol: sym,
			Side: broker.SideBuy, Type: broker.OrderTypeLimitGTC,
			ClientOrderID: id, QtyCoin: 0.01, PriceQuote: 10_000,
		}
	}

	t.Run("a request without a ClientOrderID is refused before it is sent", func(t *testing.T) {
		b := newBroker(t)
		req := resting("")
		_, err := b.PlaceOrder(ctx, req)
		if !errors.Is(err, broker.ErrInvalidOrder) {
			t.Fatalf("err = %v, want ErrInvalidOrder: without an id the caller cannot resolve a timeout", err)
		}
	})

	t.Run("a malformed request is refused by the same rules everywhere", func(t *testing.T) {
		b := newBroker(t)
		bad := map[string]func(broker.PlaceOrderRequest) broker.PlaceOrderRequest{
			"no quantity":             func(r broker.PlaceOrderRequest) broker.PlaceOrderRequest { r.QtyCoin = 0; return r },
			"negative quantity":       func(r broker.PlaceOrderRequest) broker.PlaceOrderRequest { r.QtyCoin = -1; return r },
			"limit without a price":   func(r broker.PlaceOrderRequest) broker.PlaceOrderRequest { r.PriceQuote = 0; return r },
			"market carrying a price": func(r broker.PlaceOrderRequest) broker.PlaceOrderRequest { r.Type = broker.OrderTypeMarket; return r },
			"no symbol":               func(r broker.PlaceOrderRequest) broker.PlaceOrderRequest { r.Symbol = ""; return r },
			"unknown side":            func(r broker.PlaceOrderRequest) broker.PlaceOrderRequest { r.Side = "LONG"; return r },
			"unknown market":          func(r broker.PlaceOrderRequest) broker.PlaceOrderRequest { r.Market = "futures_coinm"; return r },
			"reduce-only on spot": func(r broker.PlaceOrderRequest) broker.PlaceOrderRequest {
				r.Market, r.ReduceOnly = broker.MarketSpot, true
				return r
			},
		}
		for name, mutate := range bad {
			if _, err := b.PlaceOrder(ctx, mutate(resting("contract-bad-"+name))); !errors.Is(err, broker.ErrInvalidOrder) {
				t.Errorf("%s: err = %v, want ErrInvalidOrder", name, err)
			}
		}
	})

	t.Run("a placed order is retrievable by the id the CALLER chose", func(t *testing.T) {
		b := newBroker(t)
		const id = "contract-lookup-1"
		placed, err := b.PlaceOrder(ctx, resting(id))
		if err != nil {
			t.Fatalf("PlaceOrder: %v", err)
		}
		if placed.ClientOrderID != id {
			t.Errorf("the venue echoed ClientOrderID %q, want %q — an id the caller did not choose is no handle at all", placed.ClientOrderID, id)
		}
		if placed.VenueOrderID == "" {
			t.Error("no VenueOrderID came back")
		}

		byClient, err := b.GetOrder(ctx, broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: sym, ClientOrderID: id})
		if err != nil {
			t.Fatalf("GetOrder by ClientOrderID: %v", err)
		}
		if byClient.VenueOrderID != placed.VenueOrderID {
			t.Errorf("lookup by client id found a different order: %q vs %q", byClient.VenueOrderID, placed.VenueOrderID)
		}
		byVenue, err := b.GetOrder(ctx, broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: sym, VenueOrderID: placed.VenueOrderID})
		if err != nil {
			t.Fatalf("GetOrder by VenueOrderID: %v", err)
		}
		if byVenue.ClientOrderID != id {
			t.Errorf("lookup by venue id found a different order: %q", byVenue.ClientOrderID)
		}
	})

	t.Run("an unknown order is ErrOrderNotFound, which is what resolves a timeout", func(t *testing.T) {
		b := newBroker(t)
		q := broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: sym, ClientOrderID: "contract-never-sent"}
		if _, err := b.GetOrder(ctx, q); !errors.Is(err, broker.ErrOrderNotFound) {
			t.Fatalf("GetOrder err = %v, want ErrOrderNotFound — any other error leaves the caller unable to say whether resending is safe", err)
		}
		if _, err := b.CancelOrder(ctx, q); !errors.Is(err, broker.ErrOrderNotFound) {
			t.Fatalf("CancelOrder err = %v, want ErrOrderNotFound", err)
		}
	})

	t.Run("a query identifying nothing is refused", func(t *testing.T) {
		b := newBroker(t)
		if _, err := b.GetOrder(ctx, broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: sym}); !errors.Is(err, broker.ErrInvalidOrder) {
			t.Errorf("a query with neither id: err = %v, want ErrInvalidOrder", err)
		}
	})

	t.Run("cancel makes the order CANCELED and removes it from OpenOrders", func(t *testing.T) {
		b := newBroker(t)
		const id = "contract-cancel-1"
		if _, err := b.PlaceOrder(ctx, resting(id)); err != nil {
			t.Fatalf("PlaceOrder: %v", err)
		}
		open, err := b.OpenOrders(ctx, broker.MarketFuturesUSDM, sym)
		if err != nil {
			t.Fatalf("OpenOrders: %v", err)
		}
		if !containsClientID(open, id) {
			t.Fatalf("the resting order is not in OpenOrders: %+v", open)
		}

		q := broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: sym, ClientOrderID: id}
		cancelled, err := b.CancelOrder(ctx, q)
		if err != nil {
			t.Fatalf("CancelOrder: %v", err)
		}
		if cancelled.Status != broker.OrderStatusCanceled {
			t.Errorf("status after cancel = %q, want CANCELED", cancelled.Status)
		}
		after, err := b.GetOrder(ctx, q)
		if err != nil {
			t.Fatalf("GetOrder after cancel: %v", err)
		}
		if after.Status != broker.OrderStatusCanceled {
			t.Errorf("re-reading after cancel = %q, want CANCELED", after.Status)
		}
		open, err = b.OpenOrders(ctx, broker.MarketFuturesUSDM, sym)
		if err != nil {
			t.Fatalf("OpenOrders: %v", err)
		}
		if containsClientID(open, id) {
			t.Errorf("a cancelled order is still listed as open: %+v", open)
		}
	})

	t.Run("spot has no position and says so rather than reporting flat", func(t *testing.T) {
		b := newBroker(t)
		if _, err := b.GetPosition(ctx, broker.MarketSpot, sym); !errors.Is(err, broker.ErrNotSupported) {
			t.Fatalf("GetPosition on spot: err = %v, want ErrNotSupported — a zero Position reads as 'flat', which is a different claim", err)
		}
	})

	t.Run("nothing filled means no average price to mistake for one", func(t *testing.T) {
		b := newBroker(t)
		const id = "contract-unfilled-1"
		placed, err := b.PlaceOrder(ctx, resting(id))
		if err != nil {
			t.Fatalf("PlaceOrder: %v", err)
		}
		if placed.FilledQtyCoin != 0 {
			t.Errorf("FilledQtyCoin = %v on a resting order", placed.FilledQtyCoin)
		}
		if placed.RemainingQtyCoin() != placed.QtyCoin {
			t.Errorf("RemainingQtyCoin = %v, want the whole %v", placed.RemainingQtyCoin(), placed.QtyCoin)
		}
	})
}

func containsClientID(orders []broker.Order, id string) bool {
	for _, o := range orders {
		if o.ClientOrderID == id {
			return true
		}
	}
	return false
}
