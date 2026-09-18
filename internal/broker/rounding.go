package broker

import (
	"errors"
	"fmt"
	"math"
	"strconv"

	"futures-arbitrage-scanner/exchanges"
)

// Order rounding against the venue's own trading rules.
//
// The rules come from exchanges.Instrument, which internal/instruments has
// filled from each venue's public instrument endpoint since step 2.3. There is
// no second copy of stepSize/tickSize/minNotional anywhere in this package and
// there must not be: a rule parsed twice is a rule that disagrees with itself.
//
// # Read the rules from the venue you are about to send to
//
// The registry can hold rules for several sources, and a market's rules are not
// the same across markets or across environments. Measured 2026-09-13 on
// Binance testnet, BTCUSDT alone:
//
//	futures  tick 0.10   step 0.0001   MIN_NOTIONAL 50
//	spot     tick 0.01   step 0.00001  minNotional   5
//
// So "BTCUSDT's tick size" is not a fact; "BTCUSDT's tick size on this market
// of this venue right now" is. And because step 4.2 sends to TESTNET, the rules
// must be the TESTNET's, read at run time — a snapshot of mainnet rules sitting
// in the corpus is a different venue's answer. Where they agree it is luck, not
// design, and it is not the sort of luck to build on.
//
// # Which way things round, and why it is never "nearest"
//
// Quantity always goes DOWN onto the step grid. Rounding up spends money the
// caller did not agree to spend, and on a delta-neutral pair it breaks the
// equal-notional property the whole strategy rests on.
//
// Price rounds the way the CALLER says, and there is no default. The passive
// direction — a buy down, a sell up — keeps a limit order on its own side of
// the book; rounding a buy up by one tick can cross the spread and turn the
// maker fill the fee model assumed into a taker fill. "Nearest" is exactly the
// harmless-looking default that does that, so an unstated direction is an
// error rather than a guess.
//
// # Refusals, never repairs
//
// A quantity that lands under the venue's minimum notional is REFUSED by name.
// It is not raised to reach the minimum: raising it trades more than was asked
// for, and on a hedged pair it unbalances the legs while looking like success.

// PriceRounding is which way a price may move to reach the tick grid.
type PriceRounding string

const (
	// PriceRoundPassive keeps the order on its own side of the book: a BUY
	// rounds down, a SELL rounds up.
	PriceRoundPassive PriceRounding = "passive"
	// PriceRoundDown and PriceRoundUp are the explicit overrides.
	PriceRoundDown PriceRounding = "down"
	PriceRoundUp   PriceRounding = "up"
)

// The refusals, each its own sentinel so a caller can tell them apart: "too
// small to be worth sending" and "this market is not trading" call for
// different actions.
var (
	ErrRulesUnknown     = errors.New("broker: the venue publishes no rule to round by")
	ErrNotTrading       = errors.New("broker: the venue is not trading this market")
	ErrBelowMinQty      = errors.New("broker: quantity is below the venue's minimum")
	ErrAboveMaxQty      = errors.New("broker: quantity is above the venue's maximum")
	ErrBelowMinNotional = errors.New("broker: notional is below the venue's minimum")
)

// RoundRequest is one order to fit onto a venue's grids.
type RoundRequest struct {
	// Rules is the instrument as the VENUE published it. See the package note
	// above: these must come from the market being sent to.
	Rules exchanges.Instrument

	Side Side
	Type OrderType

	// QtyCoin is the intended size in base coin. It only ever comes back the
	// same or smaller.
	QtyCoin float64

	// PriceQuote is the limit price for a LIMIT_GTC order. For a MARKET order
	// it is a REFERENCE price, used only to check the minimum notional, and
	// nothing is returned for it.
	PriceQuote float64

	// Price is the rounding direction. Required for LIMIT_GTC; there is no
	// default, deliberately. Ignored for MARKET, which has no price to round.
	Price PriceRounding

	// ReduceOnly says this order can only SHRINK a position, which on USDⓈ-M
	// futures exempts it from the minimum notional.
	//
	// That exemption is the venue's, stated in the text of its own error:
	// "-4164 MIN_NOTIONAL: Order's notional must be no smaller than 5.0
	// (unless you choose reduce only)"
	// (https://developers.binance.com/docs/derivatives/usds-margined-futures/error-code,
	// read 2026-09-13). It matters more than it looks: without it a perp leg
	// that filled below the minimum could be neither kept nor closed, and this
	// package would have reported a position nothing could get out of.
	//
	// Set it only for a FUTURES order. PlaceOrderRequest.Validate refuses
	// ReduceOnly on spot, so the two cannot disagree, and spot publishes no
	// such exemption — its NOTIONAL filter applies to a sell as much as to a
	// buy.
	ReduceOnly bool
}

// RoundedOrder is what may actually be sent.
type RoundedOrder struct {
	QtyCoin    float64
	PriceQuote float64

	// NotionalQuote is QtyCoin x price, GROSS — no fee, no slippage. For a
	// MARKET order it is against the reference price that was supplied, so it
	// is an estimate and the fill will differ.
	NotionalQuote float64

	// QtyCoinText and PriceQuoteText are the exact strings to put in the
	// request. They exist because a float64 formatted with %v prints
	// 0.30000000000000004 often enough to matter, and a venue that parses that
	// against a 0.0001 step rejects the order for a precision error the caller
	// cannot see in its own logs. PriceQuoteText is empty for a MARKET order.
	QtyCoinText    string
	PriceQuoteText string
}

// gridEpsilon absorbs float64 representation error when testing whether a value
// already sits on a venue's grid.
//
// It is a tolerance on the STEP COUNT, not on the value, so it does not scale
// with size — the same convention and the same magnitude as
// internal/instruments' gridTolerance, on purpose. Without it, 0.3/0.0001 is
// 2999.9999999999995 and a plain Floor turns a perfectly valid 0.3 into 0.2999.
const gridEpsilon = 1e-6

// RoundOrder fits a quantity and a price onto one venue's grids, or refuses.
func RoundOrder(req RoundRequest) (RoundedOrder, error) {
	r := req.Rules
	if r.Status != "" && r.Status != exchanges.StatusTrading {
		return RoundedOrder{}, fmt.Errorf("%w: %s on %s reports status %q", ErrNotTrading, r.Symbol, r.Source, r.Status)
	}
	if req.QtyCoin <= 0 {
		return RoundedOrder{}, fmt.Errorf("%w: QtyCoin is %v", ErrInvalidOrder, req.QtyCoin)
	}
	if r.StepSizeCoin <= 0 {
		// 0 means "not stated" (exchanges.Instrument's own contract), never
		// "any quantity is fine".
		return RoundedOrder{}, fmt.Errorf("%w: %s on %s publishes no step size, so no quantity can be placed on its grid",
			ErrRulesUnknown, r.Symbol, r.Source)
	}

	// Quantity: DOWN, always.
	qtySteps := math.Floor(req.QtyCoin/r.StepSizeCoin + gridEpsilon)
	qtyDecimals := decimalsOf(r.StepSizeCoin)
	qtyCoin := quantize(qtySteps*r.StepSizeCoin, qtyDecimals)

	if qtyCoin <= 0 {
		return RoundedOrder{}, fmt.Errorf("%w: %v rounds down to nothing on %s's %v step for %s",
			ErrBelowMinQty, req.QtyCoin, r.Source, r.StepSizeCoin, r.Symbol)
	}
	if r.MinQtyCoin > 0 && qtyCoin < r.MinQtyCoin-gridEpsilon*r.StepSizeCoin {
		return RoundedOrder{}, fmt.Errorf("%w: %v (rounded from %v) is under %s's minQty %v for %s",
			ErrBelowMinQty, qtyCoin, req.QtyCoin, r.Source, r.MinQtyCoin, r.Symbol)
	}
	// Refused, not clamped: a clamp silently places a different order from the
	// one that was asked for, and the caller that wanted this size needs to
	// decide whether to split it.
	if r.MaxQtyCoin > 0 && qtyCoin > r.MaxQtyCoin+gridEpsilon*r.StepSizeCoin {
		return RoundedOrder{}, fmt.Errorf("%w: %v is over %s's maxQty %v for %s — split the order rather than clamping it",
			ErrAboveMaxQty, qtyCoin, r.Source, r.MaxQtyCoin, r.Symbol)
	}

	out := RoundedOrder{QtyCoin: qtyCoin, QtyCoinText: formatDecimals(qtyCoin, qtyDecimals)}

	// Price, for the notional check and — on a LIMIT — for the order itself.
	priceForNotional := req.PriceQuote
	if req.Type == OrderTypeLimitGTC {
		if r.TickSizeQuote <= 0 {
			// Hyperliquid is the real case: price granularity by RULE (at most
			// five significant figures) rather than by constant. This package
			// does not implement that rule, and guessing one is what rule 5
			// forbids.
			return RoundedOrder{}, fmt.Errorf("%w: %s on %s publishes no tick size; its price rule is not a constant and this package does not implement it",
				ErrRulesUnknown, r.Symbol, r.Source)
		}
		if req.PriceQuote <= 0 {
			return RoundedOrder{}, fmt.Errorf("%w: a %s order needs a price, got %v", ErrInvalidOrder, req.Type, req.PriceQuote)
		}
		direction, err := resolveDirection(req.Price, req.Side)
		if err != nil {
			return RoundedOrder{}, err
		}
		ticks := req.PriceQuote / r.TickSizeQuote
		if direction == PriceRoundDown {
			ticks = math.Floor(ticks + gridEpsilon)
		} else {
			ticks = math.Ceil(ticks - gridEpsilon)
		}
		priceDecimals := decimalsOf(r.TickSizeQuote)
		price := quantize(ticks*r.TickSizeQuote, priceDecimals)
		if price <= 0 {
			return RoundedOrder{}, fmt.Errorf("%w: %v rounds to %v on %s's %v tick", ErrInvalidOrder, req.PriceQuote, price, r.Source, r.TickSizeQuote)
		}
		out.PriceQuote = price
		out.PriceQuoteText = formatDecimals(price, priceDecimals)
		priceForNotional = price
	}

	if priceForNotional > 0 {
		out.NotionalQuote = out.QtyCoin * priceForNotional
		// 0 means the venue publishes none — nothing to check, never "zero is
		// acceptable" (the same reading internal/instruments takes).
		if r.MinNotionalQuote > 0 && out.NotionalQuote < r.MinNotionalQuote && !req.ReduceOnly {
			return out, fmt.Errorf(
				"%w: %s on %s would trade %.8g x %.8g = %.8g, under its minNotional %v — raise the SIZE deliberately or do not send it; this function will not top it up",
				ErrBelowMinNotional, r.Symbol, r.Source, out.QtyCoin, priceForNotional, out.NotionalQuote, r.MinNotionalQuote)
		}
	}
	return out, nil
}

// CeilToStep rounds v UP onto a grid of step, with the same step-count
// tolerance RoundOrder floors with, and snaps the result to the grid's decimals.
//
// It is the one deliberate exception to "quantity always goes DOWN", and it is
// not for an ordinary order. A venue that keeps a spot BUY's fee in the base
// coin (Bybit, always) credits the wallet Q × (1 − fee) for an order of Q, so
// a spot leg meant to HOLD Q must buy Q ÷ (1 − fee) — and rounding that down
// would leave the wallet short of the hedge by up to one step plus the fee,
// which is the unbalanced pair the rounding rules exist to prevent. Rounded
// up, the wallet holds Q plus less than one step; the result is still passed
// through RoundOrder, which refuses it against minQty, maxQty and minNotional
// like any other size.
//
// A v that is not a positive finite number, or a step that is not, returns 0:
// "no quantity", which RoundOrder refuses by name rather than guessing a grid.
func CeilToStep(v, step float64) float64 {
	if !(v > 0) || math.IsInf(v, 0) || !(step > 0) || math.IsInf(step, 0) {
		return 0
	}
	return quantize(math.Ceil(v/step-gridEpsilon)*step, decimalsOf(step))
}

// FloorToStep rounds v DOWN onto a grid of step — RoundOrder's own quantity
// rule, with the same step-count tolerance — and snaps the result to the grid's
// decimals, so its shortest decimal form carries no float dust:
// strconv.FormatFloat(FloorToStep(0.1+0.2, 0.1), 'f', -1, 64) is "0.3", never
// "0.30000000000000004".
//
// It exists for a caller that must size ONE quantity on the coarser of two
// venues' grids before either venue's own rules are applied — the cross-venue
// perp–perp engine (internal/execution/crossperp), whose two legs sit on two
// different venues. It checks no minimum and no maximum: the result still goes
// through RoundOrder on each venue, which refuses it by name.
//
// A v that is not a positive finite number, or a step that is not, returns 0:
// "no quantity", which RoundOrder refuses rather than guessing a grid.
func FloorToStep(v, step float64) float64 {
	if !(v > 0) || math.IsInf(v, 0) || !(step > 0) || math.IsInf(step, 0) {
		return 0
	}
	return quantize(math.Floor(v/step+gridEpsilon)*step, decimalsOf(step))
}

// resolveDirection turns the caller's stated intent into a concrete direction.
func resolveDirection(p PriceRounding, side Side) (PriceRounding, error) {
	switch p {
	case PriceRoundDown, PriceRoundUp:
		return p, nil
	case PriceRoundPassive:
		// Passive is "stay on your own side of the book".
		if side == SideBuy {
			return PriceRoundDown, nil
		}
		return PriceRoundUp, nil
	case "":
		return "", fmt.Errorf("%w: no price rounding direction was stated; there is no implicit 'nearest', because rounding a buy up by one tick crosses the spread", ErrInvalidOrder)
	default:
		return "", fmt.Errorf("%w: price rounding %q is not one of %q, %q, %q", ErrInvalidOrder, p, PriceRoundPassive, PriceRoundDown, PriceRoundUp)
	}
}

// decimalsOf returns how many decimal places a grid of this size needs.
//
// It is derived from the step rather than taken from the venue's own
// `quantityPrecision`/`pricePrecision` because the step is what the value must
// actually satisfy: a price on a 0.10 tick needs one decimal, whatever the
// venue says its price precision is. Anything the step implies beyond 12 places
// is float noise, not a rule.
func decimalsOf(step float64) int {
	for d := 0; d <= 12; d++ {
		scaled := step * math.Pow(10, float64(d))
		if math.Abs(scaled-math.Round(scaled)) < 1e-9 {
			return d
		}
	}
	return 12
}

// quantize snaps a computed value to the grid's decimals, removing the float
// dust that multiplication leaves behind (3000 x 0.001 is 3.0000000000000004).
func quantize(v float64, decimals int) float64 {
	scale := math.Pow(10, float64(decimals))
	return math.Round(v*scale) / scale
}

// formatDecimals renders the exact string to send. Fixed notation, never %v:
// %v switches to exponent form for small numbers, and "1e-05" is not a
// quantity any venue accepts.
func formatDecimals(v float64, decimals int) string {
	return strconv.FormatFloat(v, 'f', decimals, 64)
}
