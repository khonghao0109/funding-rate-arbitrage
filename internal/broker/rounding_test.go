package broker

import (
	"errors"
	"math"
	"strings"
	"testing"

	"futures-arbitrage-scanner/exchanges"
)

// The trading rules below are REAL, read from each TESTNET's own
// /fapi/v1/exchangeInfo and /api/v3/exchangeInfo on 2026-09-13.
//
// They are here as a fixture, and the fixture is the point of the warning: the
// running code must read the rules from the venue it is about to send to, not
// from this file and not from a mainnet snapshot in the corpus. The two sets
// really do differ — on 2026-09-13 futures BTCUSDT testnet wanted a tick of
// 0.10 and a MIN_NOTIONAL of 50 while spot BTCUSDT testnet wanted 0.01 and 5 —
// so a rule borrowed from the wrong market is a rejected order at best and a
// silently different size at worst.
var (
	futBTCUSDT = exchanges.Instrument{
		Symbol: "BTCUSDT", Source: "binance_futures", MarketType: "perp",
		Status:        exchanges.StatusTrading,
		TickSizeQuote: 0.10,                                         // PRICE_FILTER tickSize
		StepSizeCoin:  0.0001, MinQtyCoin: 0.0001, MaxQtyCoin: 1000, // LOT_SIZE
		MinNotionalQuote: 50, // MIN_NOTIONAL notional
		ContractSizeCoin: 1,
	}
	futETHUSDT = exchanges.Instrument{
		Symbol: "ETHUSDT", Source: "binance_futures", MarketType: "perp",
		Status:        exchanges.StatusTrading,
		TickSizeQuote: 0.01,
		StepSizeCoin:  0.001, MinQtyCoin: 0.001, MaxQtyCoin: 10000,
		MinNotionalQuote: 20,
		ContractSizeCoin: 1,
	}
	spotBTCUSDT = exchanges.Instrument{
		Symbol: "BTCUSDT", Source: "binance_spot", MarketType: "spot",
		Status:        exchanges.StatusTrading,
		TickSizeQuote: 0.01,
		StepSizeCoin:  0.00001, MinQtyCoin: 0.00001, MaxQtyCoin: 9000,
		MinNotionalQuote: 5, // NOTIONAL minNotional
		ContractSizeCoin: 1,
	}
	spotETHUSDT = exchanges.Instrument{
		Symbol: "ETHUSDT", Source: "binance_spot", MarketType: "spot",
		Status:        exchanges.StatusTrading,
		TickSizeQuote: 0.01,
		StepSizeCoin:  0.0001, MinQtyCoin: 0.0001, MaxQtyCoin: 9000,
		MinNotionalQuote: 5,
		ContractSizeCoin: 1,
	}
)

func TestRoundOrder_AgainstTheRealTestnetRules(t *testing.T) {
	cases := []struct {
		name     string
		rules    exchanges.Instrument
		side     Side
		rounding PriceRounding
		qtyIn    float64
		priceIn  float64

		wantQty   float64
		wantPrice float64
		wantQtyTx string
		wantPxTx  string
	}{
		{
			// Quantity always goes DOWN: rounding up spends money the caller
			// did not agree to spend, and on the hedge leg it breaks the
			// equal-notional property the whole strategy rests on.
			name: "futures BTC: qty floors onto the 0.0001 grid", rules: futBTCUSDT,
			side: SideBuy, rounding: PriceRoundPassive,
			qtyIn: 0.00129, priceIn: 60_000.07,
			wantQty: 0.0012, wantPrice: 60_000.00, wantQtyTx: "0.0012", wantPxTx: "60000.0",
		},
		{
			// A BUY rounds its price DOWN — towards the passive side. Rounding
			// it up crosses the spread and turns a maker order into a taker.
			name: "futures BTC: a buy price floors onto the 0.10 tick", rules: futBTCUSDT,
			side: SideBuy, rounding: PriceRoundPassive,
			qtyIn: 0.01, priceIn: 60_000.19,
			wantQty: 0.01, wantPrice: 60_000.10, wantQtyTx: "0.0100", wantPxTx: "60000.1",
		},
		{
			// A SELL rounds UP, for the same reason mirrored.
			name: "futures BTC: a sell price ceils onto the 0.10 tick", rules: futBTCUSDT,
			side: SideSell, rounding: PriceRoundPassive,
			qtyIn: 0.01, priceIn: 60_000.11,
			wantQty: 0.01, wantPrice: 60_000.20, wantQtyTx: "0.0100", wantPxTx: "60000.2",
		},
		{
			// The caller may override the passive default, but only by saying
			// so: there is no implicit "nearest".
			name: "futures BTC: an explicit down on a sell is honoured", rules: futBTCUSDT,
			side: SideSell, rounding: PriceRoundDown,
			qtyIn: 0.01, priceIn: 60_000.19,
			wantQty: 0.01, wantPrice: 60_000.10, wantQtyTx: "0.0100", wantPxTx: "60000.1",
		},
		{
			name: "futures ETH: 0.001 step and 0.01 tick", rules: futETHUSDT,
			side: SideBuy, rounding: PriceRoundPassive,
			qtyIn: 0.0129, priceIn: 3_000.567,
			wantQty: 0.012, wantPrice: 3_000.56, wantQtyTx: "0.012", wantPxTx: "3000.56",
		},
		{
			// Spot BTC's step is 0.00001 — two decimals finer than futures.
			name: "spot BTC: the finer 0.00001 step", rules: spotBTCUSDT,
			side: SideBuy, rounding: PriceRoundPassive,
			qtyIn: 0.000123456, priceIn: 60_000.999,
			wantQty: 0.00012, wantPrice: 60_000.99, wantQtyTx: "0.00012", wantPxTx: "60000.99",
		},
		{
			name: "spot ETH: 0.0001 step", rules: spotETHUSDT,
			side: SideSell, rounding: PriceRoundPassive,
			qtyIn: 0.01239, priceIn: 3_000.001,
			wantQty: 0.0123, wantPrice: 3_000.01, wantQtyTx: "0.0123", wantPxTx: "3000.01",
		},
		{
			// A value already ON the grid must survive untouched. This is the
			// float trap: 0.3/0.0001 is 2999.9999999999995 in float64, and a
			// plain Floor turns a perfectly good 0.3 into 0.2999.
			name: "futures BTC: a quantity already on the grid is not shaved", rules: futBTCUSDT,
			side: SideBuy, rounding: PriceRoundPassive,
			qtyIn: 0.3, priceIn: 60_000.0,
			wantQty: 0.3, wantPrice: 60_000.0, wantQtyTx: "0.3000", wantPxTx: "60000.0",
		},
		{
			name: "futures ETH: 0.7 on a 0.001 grid survives", rules: futETHUSDT,
			side: SideBuy, rounding: PriceRoundPassive,
			qtyIn: 0.7, priceIn: 3_000.0,
			wantQty: 0.7, wantPrice: 3_000.0, wantQtyTx: "0.700", wantPxTx: "3000.00",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RoundOrder(RoundRequest{
				Rules: tc.rules, Side: tc.side, Type: OrderTypeLimitGTC,
				QtyCoin: tc.qtyIn, PriceQuote: tc.priceIn, Price: tc.rounding,
			})
			if err != nil {
				t.Fatalf("RoundOrder: %v", err)
			}
			if math.Abs(got.QtyCoin-tc.wantQty) > 1e-12 {
				t.Errorf("QtyCoin = %v, want %v", got.QtyCoin, tc.wantQty)
			}
			if math.Abs(got.PriceQuote-tc.wantPrice) > 1e-9 {
				t.Errorf("PriceQuote = %v, want %v", got.PriceQuote, tc.wantPrice)
			}
			if got.QtyCoinText != tc.wantQtyTx {
				t.Errorf("QtyCoinText = %q, want %q", got.QtyCoinText, tc.wantQtyTx)
			}
			if got.PriceQuoteText != tc.wantPxTx {
				t.Errorf("PriceQuoteText = %q, want %q", got.PriceQuoteText, tc.wantPxTx)
			}
			// The rounded values must really sit on the venue's grids.
			if steps := got.QtyCoin / tc.rules.StepSizeCoin; math.Abs(steps-math.Round(steps)) > 1e-6 {
				t.Errorf("QtyCoin %v is not a whole multiple of the %v step", got.QtyCoin, tc.rules.StepSizeCoin)
			}
			if ticks := got.PriceQuote / tc.rules.TickSizeQuote; math.Abs(ticks-math.Round(ticks)) > 1e-6 {
				t.Errorf("PriceQuote %v is not a whole multiple of the %v tick", got.PriceQuote, tc.rules.TickSizeQuote)
			}
		})
	}
}

// The refusal that must never become a quantity increase. Raising the size to
// reach a minimum notional trades more than the caller asked for, and on a
// hedged pair it silently unbalances the two legs.
func TestRoundOrder_BelowMinNotionalIsRefusedByNameAndNeverTopUp(t *testing.T) {
	cases := []struct {
		name  string
		rules exchanges.Instrument
		qty   float64
		price float64
	}{
		// 0.0008 x 60,000 = 48, under futures BTC's MIN_NOTIONAL of 50.
		{"futures BTC just under 50", futBTCUSDT, 0.0008, 60_000},
		// 0.006 x 3,000 = 18, under futures ETH's 20.
		{"futures ETH just under 20", futETHUSDT, 0.006, 3_000},
		// 0.00007 x 60,000 = 4.2, under spot BTC's 5.
		{"spot BTC just under 5", spotBTCUSDT, 0.00007, 60_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RoundOrder(RoundRequest{
				Rules: tc.rules, Side: SideBuy, Type: OrderTypeLimitGTC,
				QtyCoin: tc.qty, PriceQuote: tc.price, Price: PriceRoundPassive,
			})
			if !errors.Is(err, ErrBelowMinNotional) {
				t.Fatalf("err = %v, want ErrBelowMinNotional", err)
			}
			// The refusal must carry both numbers, or the operator cannot tell
			// how much short it was.
			for _, want := range []string{"minNotional", tc.rules.Symbol} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q: %v", want, err)
				}
			}
			if got.QtyCoin > tc.qty {
				t.Errorf("the quantity was RAISED from %v to %v to reach the minimum", tc.qty, got.QtyCoin)
			}
		})
	}
}

// Rounding down can take a quantity under the venue's own minimum, and that is
// a refusal too — not a silent zero-size order.
func TestRoundOrder_RoundingBelowMinQtyIsRefused(t *testing.T) {
	_, err := RoundOrder(RoundRequest{
		Rules: futBTCUSDT, Side: SideBuy, Type: OrderTypeLimitGTC,
		QtyCoin: 0.00005, PriceQuote: 60_000, Price: PriceRoundPassive,
	})
	if !errors.Is(err, ErrBelowMinQty) {
		t.Fatalf("err = %v, want ErrBelowMinQty — 0.00005 floors to 0 on a 0.0001 step", err)
	}
}

func TestRoundOrder_AboveMaxQtyIsRefusedRatherThanClamped(t *testing.T) {
	_, err := RoundOrder(RoundRequest{
		Rules: futBTCUSDT, Side: SideBuy, Type: OrderTypeLimitGTC,
		QtyCoin: 1500, PriceQuote: 60_000, Price: PriceRoundPassive,
	})
	if !errors.Is(err, ErrAboveMaxQty) {
		t.Fatalf("err = %v, want ErrAboveMaxQty; clamping would place a different order than the caller asked for", err)
	}
}

// No implicit "nearest": a caller that does not say which way must be refused,
// because the default that looks harmless is the one that crosses the spread.
func TestRoundOrder_RefusesAPriceWithNoStatedDirection(t *testing.T) {
	_, err := RoundOrder(RoundRequest{
		Rules: futBTCUSDT, Side: SideBuy, Type: OrderTypeLimitGTC,
		QtyCoin: 0.01, PriceQuote: 60_000.07,
	})
	if !errors.Is(err, ErrInvalidOrder) {
		t.Fatalf("err = %v, want ErrInvalidOrder for an unstated rounding direction", err)
	}
}

// Rules the venue does not publish cannot be applied, and guessing is rule 5's
// exact prohibition.
func TestRoundOrder_RefusesRulesItCannotApply(t *testing.T) {
	noStep := futBTCUSDT
	noStep.StepSizeCoin = 0
	if _, err := RoundOrder(RoundRequest{Rules: noStep, Side: SideBuy, Type: OrderTypeLimitGTC,
		QtyCoin: 1, PriceQuote: 60_000, Price: PriceRoundPassive}); !errors.Is(err, ErrRulesUnknown) {
		t.Errorf("a zero step size must be refused, got %v", err)
	}

	noTick := futBTCUSDT
	noTick.TickSizeQuote = 0
	if _, err := RoundOrder(RoundRequest{Rules: noTick, Side: SideBuy, Type: OrderTypeLimitGTC,
		QtyCoin: 1, PriceQuote: 60_000, Price: PriceRoundPassive}); !errors.Is(err, ErrRulesUnknown) {
		t.Errorf("a zero tick size must be refused for a LIMIT order, got %v", err)
	}

	halted := futBTCUSDT
	halted.Status = "halt"
	if _, err := RoundOrder(RoundRequest{Rules: halted, Side: SideBuy, Type: OrderTypeLimitGTC,
		QtyCoin: 1, PriceQuote: 60_000, Price: PriceRoundPassive}); !errors.Is(err, ErrNotTrading) {
		t.Errorf("a market the venue is not trading must be refused, got %v", err)
	}
}

// A MARKET order has no price of its own, but minNotional still applies — so
// the caller supplies a reference price and gets no price back.
func TestRoundOrder_MarketUsesAReferencePriceAndReturnsNone(t *testing.T) {
	got, err := RoundOrder(RoundRequest{
		Rules: futBTCUSDT, Side: SideBuy, Type: OrderTypeMarket,
		QtyCoin: 0.00129, PriceQuote: 60_000,
	})
	if err != nil {
		t.Fatalf("RoundOrder: %v", err)
	}
	if got.PriceQuote != 0 || got.PriceQuoteText != "" {
		t.Errorf("a market order came back with a price: %v %q", got.PriceQuote, got.PriceQuoteText)
	}
	if math.Abs(got.QtyCoin-0.0012) > 1e-12 {
		t.Errorf("QtyCoin = %v, want 0.0012", got.QtyCoin)
	}
	// The reference price still has to clear minNotional.
	if _, err := RoundOrder(RoundRequest{
		Rules: futBTCUSDT, Side: SideBuy, Type: OrderTypeMarket,
		QtyCoin: 0.0008, PriceQuote: 60_000,
	}); !errors.Is(err, ErrBelowMinNotional) {
		t.Errorf("a market order under minNotional must be refused too, got %v", err)
	}
}

// The rounded request must be one PlaceOrderRequest.Validate accepts: the two
// are used together and a rounding that produces an invalid order is a trap.
func TestRoundOrder_ProducesARequestTheInterfaceAccepts(t *testing.T) {
	got, err := RoundOrder(RoundRequest{
		Rules: futBTCUSDT, Side: SideBuy, Type: OrderTypeLimitGTC,
		QtyCoin: 0.00129, PriceQuote: 60_000.07, Price: PriceRoundPassive,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := PlaceOrderRequest{
		Market: MarketFuturesUSDM, Symbol: "BTCUSDT", Side: SideBuy,
		Type: OrderTypeLimitGTC, ClientOrderID: "round-1",
		QtyCoin: got.QtyCoin, PriceQuote: got.PriceQuote,
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("the rounded order is not a valid request: %v", err)
	}
}

// A reduceOnly order is exempt from the venue's minimum notional, and the
// exemption is the VENUE's own, stated in the text of its error:
//
//	-4164 MIN_NOTIONAL: "Order's notional must be no smaller than 5.0
//	(unless you choose reduce only)"
//	https://developers.binance.com/docs/derivatives/usds-margined-futures/error-code
//
// It matters more than it reads. Without it, a perp leg that filled below the
// minimum could be neither kept as a position nor closed by any order, and
// internal/execution would have had to report a position nothing could exit.
func TestRoundOrder_AReduceOnlyOrderIsExemptFromTheMinimumNotional(t *testing.T) {
	rules := futBTCUSDT
	req := RoundRequest{
		Rules: rules, Side: SideBuy, Type: OrderTypeMarket,
		QtyCoin: 0.0003, PriceQuote: 60_000, // $18 against a $50 minimum
	}

	if _, err := RoundOrder(req); !errors.Is(err, ErrBelowMinNotional) {
		t.Fatalf("an ordinary order of $18 against a $50 minimum returned %v, want ErrBelowMinNotional", err)
	}

	req.ReduceOnly = true
	out, err := RoundOrder(req)
	if err != nil {
		t.Fatalf("a reduceOnly close of the same size was refused: %v", err)
	}
	if out.QtyCoin != 0.0003 {
		t.Errorf("qty = %v, want the 0.0003 that is actually held", out.QtyCoin)
	}

	// The exemption is for the MINIMUM only. Everything else still applies:
	// a quantity under the venue's minQty is not a quantity the venue trades,
	// reduceOnly or not.
	req.QtyCoin = rules.MinQtyCoin / 2
	if _, err := RoundOrder(req); err == nil {
		t.Error("reduceOnly was read as a licence to ignore minQty and the step grid too")
	}
}
