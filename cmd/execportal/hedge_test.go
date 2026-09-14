package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/broker/brokertest"
	"futures-arbitrage-scanner/internal/execution"
)

// The hedge is computed from the VENUE's record of each intent's own orders.
// These tests drive that record through brokertest's in-memory venue.

const testIntent = "pbtcusdt-20260914-101500-123"

// The BTCUSDT grids measured on the two testnets on 2026-09-13 (broker/rounding.go).
var (
	spotRulesBTC = exchanges.Instrument{Symbol: "BTCUSDT", StepSizeCoin: 0.00001, MinQtyCoin: 0.00001, TickSizeQuote: 0.01, MinNotionalQuote: 5}
	perpRulesBTC = exchanges.Instrument{Symbol: "BTCUSDT", StepSizeCoin: 0.0001, MinQtyCoin: 0.0001, TickSizeQuote: 0.1, MinNotionalQuote: 50}
)

func place(t *testing.T, b broker.Broker, market broker.Market, side broker.Side, id string, qtyCoin float64) {
	t.Helper()
	if _, err := b.PlaceOrder(context.Background(), broker.PlaceOrderRequest{
		Market: market, Symbol: "BTCUSDT", Side: side, Type: broker.OrderTypeMarket, ClientOrderID: id, QtyCoin: qtyCoin,
	}); err != nil {
		t.Fatal(err)
	}
}

func openPair(t *testing.T, spot, perp broker.Broker, intentID string, qtyCoin float64) {
	t.Helper()
	place(t, spot, broker.MarketSpot, broker.SideBuy, execution.LegClientOrderID(intentID, execution.LegSpot), qtyCoin)
	place(t, perp, broker.MarketFuturesUSDM, broker.SideSell, execution.LegClientOrderID(intentID, execution.LegPerp), qtyCoin)
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestReadLegNet_SumsTheIntentsOwnOrdersFromTheVenue(t *testing.T) {
	ctx := context.Background()
	spot, perp := brokertest.New(), brokertest.New()
	openPair(t, spot, perp, testIntent, 0.0008)
	// An order of ANOTHER intent on the same venue must not count.
	openPair(t, spot, perp, "xbtcusdt-20260913-075852", 0.0259)

	h := readIntentHedges(ctx, spot, perp, newDoneOrders(), "BTCUSDT", []string{testIntent})[0]
	if !near(h.Spot.QtyCoin, 0.0008) || !near(h.Perp.QtyCoin, -0.0008) {
		t.Fatalf("after open: spot %v perp %v, want +0.0008 / -0.0008", h.Spot.QtyCoin, h.Perp.QtyCoin)
	}

	// The close: perp bought back, spot sold, both under the CLOSE ids.
	place(t, perp, broker.MarketFuturesUSDM, broker.SideBuy, execution.CloseClientOrderID(testIntent, execution.LegPerp), 0.0008)
	place(t, spot, broker.MarketSpot, broker.SideSell, execution.CloseClientOrderID(testIntent, execution.LegSpot), 0.0008)
	h = readIntentHedges(ctx, spot, perp, newDoneOrders(), "BTCUSDT", []string{testIntent})[0]
	if !near(h.Spot.QtyCoin, 0) || !near(h.Perp.QtyCoin, 0) || !near(h.residualCoin(), 0) {
		t.Errorf("after close: spot %v perp %v", h.Spot.QtyCoin, h.Perp.QtyCoin)
	}
	if len(h.Spot.SeenVI) != 2 || len(h.Perp.SeenVI) != 2 {
		t.Errorf("seen lists = %v / %v, want the open and the close on each leg", h.Spot.SeenVI, h.Perp.SeenVI)
	}
	if h.Spot.ReconcileOrderExists || h.Perp.ReconcileOrderExists {
		t.Error("no reconcile order was placed, yet one is reported")
	}
}

// The 2026-09-13 failure, replayed: the perp leg closed, the spot leg did not.
// The pair must read as unbalanced by the intent's own record.
func TestReadLegNet_ANakedSpotLegIsVisible(t *testing.T) {
	ctx := context.Background()
	spot, perp := brokertest.New(), brokertest.New()
	openPair(t, spot, perp, testIntent, 0.0008)
	place(t, perp, broker.MarketFuturesUSDM, broker.SideBuy, execution.CloseClientOrderID(testIntent, execution.LegPerp), 0.0008)

	h := readIntentHedges(ctx, spot, perp, newDoneOrders(), "BTCUSDT", []string{testIntent})[0]
	if !near(h.residualCoin(), 0.0008) {
		t.Fatalf("residual %v, want +0.0008 of naked spot", h.residualCoin())
	}
	plan := planSquare(h, spotRulesBTC, perpRulesBTC, 77_000, 77_000)
	if plan.Action != "send" || plan.Market != broker.MarketSpot || plan.Side != broker.SideSell || !near(plan.QtyCoin, 0.0008) {
		t.Errorf("plan = %+v, want SELL 0.0008 on spot", plan)
	}
	if plan.ClientOrderID != execution.ReconcileClientOrderID(testIntent, execution.LegSpot) {
		t.Errorf("plan id %q is not the derived reconcile id execcheck would look for", plan.ClientOrderID)
	}
	if plan.ReduceOnly {
		t.Error("a spot order was marked reduce-only, which the venue refuses")
	}
}

// flakyVenue answers GetOrder with a non-NotFound error for one id — an order
// whose size is unknown, which must never be read as zero.
type flakyVenue struct {
	broker.Broker
	failID string
	reads  atomic.Int64
}

func (f *flakyVenue) GetOrder(ctx context.Context, q broker.OrderQuery) (broker.Order, error) {
	f.reads.Add(1)
	if q.ClientOrderID == f.failID {
		return broker.Order{}, errors.New("HTTP 502 from a gateway")
	}
	return f.Broker.GetOrder(ctx, q)
}

func TestReadLegNet_AnUnreadableOrderIsUnknownNotZero(t *testing.T) {
	ctx := context.Background()
	spotFake, perp := brokertest.New(), brokertest.New()
	openPair(t, spotFake, perp, testIntent, 0.0008)
	spot := &flakyVenue{Broker: spotFake, failID: execution.CloseClientOrderID(testIntent, execution.LegSpot)}

	h := readIntentHedges(ctx, spot, perp, newDoneOrders(), "BTCUSDT", []string{testIntent})[0]
	if len(h.Spot.UnreadableVI) != 1 {
		t.Fatalf("unreadable = %v, want the one close order", h.Spot.UnreadableVI)
	}
	status, _ := classifyHedge(hedgeEvidence{SpotLegQtyCoin: h.Spot.QtyCoin, IntentsPerpQtyCoin: h.Perp.QtyCoin,
		VenuePerpQtyCoin: h.Perp.QtyCoin, ToleranceQtyCoin: 0.0001, PerpPositionRead: true, UnreadableVI: h.unreadable()})
	if status != statusUnknown {
		t.Errorf("status with an unreadable order = %s, want unknown", status)
	}
	if plan := planSquare(h, spotRulesBTC, perpRulesBTC, 77_000, 77_000); plan.Action != "refuse" {
		t.Errorf("plan with an unreadable order = %+v, want refuse", plan)
	}
}

func TestReadLegNet_AWorkingOrderIsUnknownAndNotSquared(t *testing.T) {
	ctx := context.Background()
	spot, perp := brokertest.New(), brokertest.New()
	// A resting LIMIT from a crashed open: accepted, nothing filled, still NEW.
	if _, err := spot.PlaceOrder(ctx, broker.PlaceOrderRequest{Market: broker.MarketSpot, Symbol: "BTCUSDT", Side: broker.SideBuy,
		Type: broker.OrderTypeLimitGTC, ClientOrderID: execution.LegClientOrderID(testIntent, execution.LegSpot), QtyCoin: 0.0008, PriceQuote: 70_000}); err != nil {
		t.Fatal(err)
	}
	h := readIntentHedges(ctx, spot, perp, newDoneOrders(), "BTCUSDT", []string{testIntent})[0]
	if len(h.working()) != 1 {
		t.Fatalf("working = %v", h.working())
	}
	if plan := planSquare(h, spotRulesBTC, perpRulesBTC, 77_000, 77_000); plan.Action != "refuse" || !strings.Contains(plan.ReasonVI, "đang chạy") {
		t.Errorf("plan beside a working order = %+v, want refuse", plan)
	}
}

// Finished orders cannot change at the venue, so they are read once; an id the
// venue does not know is asked again every time.
func TestDoneOrders_ReadsAFinishedOrderOnce(t *testing.T) {
	ctx := context.Background()
	spotFake, perpFake := brokertest.New(), brokertest.New()
	openPair(t, spotFake, perpFake, testIntent, 0.0008)
	spot := &flakyVenue{Broker: spotFake}
	perp := &flakyVenue{Broker: perpFake}
	memo := newDoneOrders()

	readIntentHedges(ctx, spot, perp, memo, "BTCUSDT", []string{testIntent})
	first := spot.reads.Load() + perp.reads.Load()
	readIntentHedges(ctx, spot, perp, memo, "BTCUSDT", []string{testIntent})
	second := spot.reads.Load() + perp.reads.Load() - first
	if first != 8 {
		t.Errorf("first scan made %d lookups, want 8 (four ids per leg)", first)
	}
	if second != 6 {
		t.Errorf("second scan made %d lookups, want 6 — the two FILLED opens remembered, the six unknown ids asked again", second)
	}
}

func TestClassifyHedge(t *testing.T) {
	const tol = 0.0001
	base := hedgeEvidence{ToleranceQtyCoin: tol, PerpPositionRead: true}
	with := func(f func(*hedgeEvidence)) hedgeEvidence { e := base; f(&e); return e }
	cases := []struct {
		name string
		ev   hedgeEvidence
		want hedgeStatus
	}{
		{"nothing anywhere", base, statusBothFlat},
		{"a clean pair", with(func(e *hedgeEvidence) {
			e.SpotLegQtyCoin, e.IntentsPerpQtyCoin, e.VenuePerpQtyCoin = 0.0008, -0.0008, -0.0008
		}), statusBothOpen},
		{"pair within one step", with(func(e *hedgeEvidence) {
			e.SpotLegQtyCoin, e.IntentsPerpQtyCoin, e.VenuePerpQtyCoin = 0.00085, -0.0008, -0.0008
		}), statusBothOpen},
		{"dust under a step on both", with(func(e *hedgeEvidence) {
			e.SpotLegQtyCoin, e.IntentsPerpQtyCoin, e.VenuePerpQtyCoin = 0.00002, -0.00005, -0.00005
		}), statusBothFlat},
		{"naked spot", with(func(e *hedgeEvidence) { e.SpotLegQtyCoin = 0.0008 }), statusUnhedged},
		{"naked perp short", with(func(e *hedgeEvidence) { e.IntentsPerpQtyCoin, e.VenuePerpQtyCoin = -0.0008, -0.0008 }), statusUnhedged},
		{"spot long and perp LONG", with(func(e *hedgeEvidence) {
			e.SpotLegQtyCoin, e.IntentsPerpQtyCoin, e.VenuePerpQtyCoin = 0.0008, 0.0008, 0.0008
		}), statusUnhedged},
		{"pair off by two steps", with(func(e *hedgeEvidence) {
			e.SpotLegQtyCoin, e.IntentsPerpQtyCoin, e.VenuePerpQtyCoin = 0.0010, -0.0008, -0.0008
		}), statusUnhedged},
		{"venue perp no intent explains", with(func(e *hedgeEvidence) {
			e.SpotLegQtyCoin, e.IntentsPerpQtyCoin, e.VenuePerpQtyCoin = 0.0008, -0.0008, -0.0016
		}), statusEvidenceConflict},
		{"intents think a perp is open, venue flat", with(func(e *hedgeEvidence) { e.SpotLegQtyCoin, e.IntentsPerpQtyCoin = 0.0008, -0.0008 }), statusEvidenceConflict},
		{"perp position unread", with(func(e *hedgeEvidence) { e.PerpPositionRead = false }), statusUnknown},
		{"no rules", with(func(e *hedgeEvidence) { e.ToleranceQtyCoin = 0 }), statusUnknown},
		{"an unreadable order beats a clean-looking pair", with(func(e *hedgeEvidence) {
			e.SpotLegQtyCoin, e.IntentsPerpQtyCoin, e.VenuePerpQtyCoin = 0.0008, -0.0008, -0.0008
			e.UnreadableVI = []string{"x"}
		}), statusUnknown},
		{"a working order beats flat", with(func(e *hedgeEvidence) { e.WorkingVI = []string{"x"} }), statusUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := classifyHedge(tc.ev)
			if got != tc.want {
				t.Errorf("classifyHedge = %s (%s), want %s", got, why, tc.want)
			}
			if why == "" {
				t.Error("no reason given")
			}
			if statusVI[got] == "" {
				t.Errorf("status %s has no operator label", got)
			}
		})
	}
	if statusVI[statusBothOpen] != "DELTA-NEUTRAL (HEDGED)" || statusVI[statusUnhedged] != "UNHEDGED DELTA RISK" {
		t.Error("the two labels the operator's acceptance names have drifted")
	}
}

func TestPlanSquare(t *testing.T) {
	mk := func(spotQty, perpQty float64) intentHedge {
		return intentHedge{IntentID: testIntent, Spot: legNet{QtyCoin: spotQty}, Perp: legNet{QtyCoin: perpQty}}
	}
	t.Run("balanced does nothing", func(t *testing.T) {
		if p := planSquare(mk(0.0008, -0.0008), spotRulesBTC, perpRulesBTC, 77_000, 77_000); p.Action != "none" {
			t.Errorf("%+v", p)
		}
	})
	t.Run("too much perp short buys perp back reduce-only, under the min notional", func(t *testing.T) {
		p := planSquare(mk(0, -0.0008), spotRulesBTC, perpRulesBTC, 77_000, 77_000)
		// 0.0008 x 77,000 = 61.6 clears 50 anyway; the case below is the one
		// only reduce-only can send.
		if p.Action != "send" || p.Market != broker.MarketFuturesUSDM || p.Side != broker.SideBuy || !p.ReduceOnly || !near(p.QtyCoin, 0.0008) {
			t.Errorf("%+v", p)
		}
		p = planSquare(mk(0, -0.0003), spotRulesBTC, perpRulesBTC, 77_000, 77_000)
		if p.Action != "send" || !near(p.QtyCoin, 0.0003) {
			t.Errorf("a 23-quote perp remainder must be closeable reduce-only (-4164's own exemption): %+v", p)
		}
		if p.ClientOrderID != execution.ReconcileClientOrderID(testIntent, execution.LegPerp) {
			t.Errorf("id %q", p.ClientOrderID)
		}
	})
	t.Run("a remainder inside the coarser step is balanced", func(t *testing.T) {
		if p := planSquare(mk(0.00003, 0), spotRulesBTC, perpRulesBTC, 77_000, 77_000); p.Action != "none" {
			t.Errorf("%+v", p)
		}
	})
	t.Run("a spot remainder under the spot minimum is refused, not topped up", func(t *testing.T) {
		// Past the 0.0001 tolerance, and 0.0002 x 20,000 = 4 quote, under
		// spot's minimum of 5. Spot publishes no reduce-only exemption.
		p := planSquare(mk(0.0002, 0), spotRulesBTC, perpRulesBTC, 20_000, 20_000)
		if p.Action != "refuse" || p.QtyCoin != 0 || !strings.Contains(p.ReasonVI, "xử lý tay") {
			t.Errorf("%+v", p)
		}
	})
	t.Run("an off-grid residual rounds DOWN", func(t *testing.T) {
		p := planSquare(mk(0.000876, 0), spotRulesBTC, perpRulesBTC, 77_000, 77_000)
		if p.Action != "send" || !near(p.QtyCoin, 0.00087) {
			t.Errorf("%+v", p)
		}
	})
	t.Run("a leg whose squaring id already exists is not squared again", func(t *testing.T) {
		h := mk(0.0008, 0)
		h.Spot.ReconcileOrderExists = true
		p := planSquare(h, spotRulesBTC, perpRulesBTC, 77_000, 77_000)
		if p.Action != "refuse" || !strings.Contains(p.ReasonVI, "đã có lệnh cân") {
			t.Errorf("%+v", p)
		}
		// The OTHER leg's reconcile id does not block this one.
		h = mk(0.0008, 0)
		h.Perp.ReconcileOrderExists = true
		if p := planSquare(h, spotRulesBTC, perpRulesBTC, 77_000, 77_000); p.Action != "send" {
			t.Errorf("%+v", p)
		}
	})
	t.Run("no rules, no tolerance, no order", func(t *testing.T) {
		if p := planSquare(mk(0.0008, 0), exchanges.Instrument{}, exchanges.Instrument{}, 77_000, 77_000); p.Action != "refuse" {
			t.Errorf("%+v", p)
		}
	})
}

// A squaring order placed through the fake lands where the plan said and
// brings the intent's own record back to zero.
func TestPlanSquare_ThePlannedOrderBalancesTheIntent(t *testing.T) {
	ctx := context.Background()
	spot, perp := brokertest.New(), brokertest.New()
	openPair(t, spot, perp, testIntent, 0.0008)
	place(t, spot, broker.MarketSpot, broker.SideSell, execution.CloseClientOrderID(testIntent, execution.LegSpot), 0.0008)

	h := readIntentHedges(ctx, spot, perp, newDoneOrders(), "BTCUSDT", []string{testIntent})[0]
	plan := planSquare(h, spotRulesBTC, perpRulesBTC, 77_000, 77_000)
	if plan.Action != "send" || plan.Market != broker.MarketFuturesUSDM {
		t.Fatalf("plan = %+v", plan)
	}
	if _, err := perp.PlaceOrder(ctx, broker.PlaceOrderRequest{Market: plan.Market, Symbol: "BTCUSDT", Side: plan.Side,
		Type: broker.OrderTypeMarket, ClientOrderID: plan.ClientOrderID, QtyCoin: plan.QtyCoin, ReduceOnly: plan.ReduceOnly}); err != nil {
		t.Fatal(err)
	}
	h = readIntentHedges(ctx, spot, perp, newDoneOrders(), "BTCUSDT", []string{testIntent})[0]
	if !near(h.residualCoin(), 0) || !h.Perp.ReconcileOrderExists {
		t.Errorf("after squaring: residual %v, reconcile seen %v", h.residualCoin(), h.Perp.ReconcileOrderExists)
	}
	if again := planSquare(h, spotRulesBTC, perpRulesBTC, 77_000, 77_000); again.Action != "none" {
		t.Errorf("a second reconcile would do %+v, want nothing", again)
	}
}
