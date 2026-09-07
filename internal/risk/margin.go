package risk

import "fmt"

// The margin model for the SHORT PERP leg of a delta-neutral funding position
// (step 3.3c).
//
// # Why a delta-neutral position can still be liquidated
//
// The position is flat in the coin: long spot, short perp, equal notional. A
// price rise loses on the perp and gains on the spot by the same amount, so the
// COMBINED position does not care. But the two legs sit in different accounts,
// usually at different venues, and margin is not fungible between them. The
// perp account can run out of collateral while the spot account holds an
// unrealized gain it cannot post. That is the structural risk of cross-venue
// delta-neutral, and no funding figure anywhere in this project deducts it.
//
// # What is modelled and what is not
//
// Modelled: isolated margin on the perp leg, one maintenance bracket, and the
// price at which the venue would close it.
//
// NOT modelled, and each would move the liquidation price the same way — later:
// funding COLLECTED into the perp account, cross-margin against other
// positions, and any collateral added after entry. Leaving them out makes this
// model fire EARLY rather than late, which is the safe direction for a
// backtest: it pays round trips the real position might not have paid, so a
// result that survives it is not surviving on optimism. On a quote-bridged
// venue the funding is credited in a different asset from the spot capital
// anyway, so crediting it here would be its own assumption.
//
// Also not modelled: the venue's own liquidation MECHANICS. A real liquidation
// is partial, sequential and charges a fee; this says only when one starts.

// Bracket is one maintenance-margin tier as a venue publishes it.
//
// TierCeilingQuote is the top of the tier this rate applies to, because
// maintenance is a STEP function of size — sizing up moves a position into a
// stricter bracket with no other change (doc.go). A rate quoted without the
// size it applies to is not a rate.
//
// Verified separates "the venue publishes this and it was read" from "nobody
// looked it up", exactly as config.Fee.Verified does, and for the same reason:
// an unverified 0 would read as "no maintenance requirement", which is the most
// dangerous possible default.
type Bracket struct {
	Source string

	MaintenanceMarginFrac float64
	TierCeilingQuote      float64
	MaxLeverage           float64

	Verified bool
	NoteVI   string
}

// State is a perp leg's margin position at one price.
type State struct {
	// LiquidationPriceQuote is where the venue would begin closing the short.
	// 0 with OK false when the inputs do not describe a position.
	LiquidationPriceQuote float64

	// BufferPct is how far the price has to RISE from here to reach it, in
	// percent of the current price. It is the number a rule should watch:
	// the distance to liquidation, not the leverage that was chosen.
	BufferPct float64

	// EquityQuote is margin posted plus the short's unrealized PnL;
	// MaintenanceQuote is what the venue requires against the position at this
	// price. They are REPORTED figures — the liquidation verdict is decided on
	// the price, see Evaluate.
	EquityQuote      float64
	MaintenanceQuote float64
	Liquidated       bool

	OK       bool
	ReasonVI string
}

// Position is the short perp leg as this model needs it.
type Position struct {
	// NotionalQuote is the size AT ENTRY, and EntryPriceQuote the price it was
	// opened at. The two together fix the quantity; everything else is derived
	// from the current price.
	NotionalQuote   float64
	EntryPriceQuote float64

	// MarginFrac is the collateral posted on the perp leg as a fraction of the
	// entry notional — the reciprocal of leverage. 0.10 is 10x. It is a
	// DECISION, not a venue fact: the venue only sets the maximum.
	MarginFrac float64
}

// Evaluate prices the short perp leg at a current price.
//
// The arithmetic, for a SHORT of notional N opened at P0 with margin M = N·f,
// at price P and maintenance rate m:
//
//	unrealized PnL = N·(1 − P/P0)          (a short loses as P rises)
//	equity         = M + PnL
//	maintenance    = m·N·P/P0              (required against the CURRENT value)
//	liquidated when equity ≤ maintenance
//	              ⇔ P/P0 ≥ (1 + f)/(1 + m)
//
// so the liquidation price is P0·(1 + f)/(1 + m) and does not depend on P.
func Evaluate(pos Position, bracket Bracket, priceQuote float64) State {
	switch {
	case !positiveFinite(pos.NotionalQuote):
		return State{ReasonVI: fmt.Sprintf("Notional %v không phải số dương hữu hạn.", pos.NotionalQuote)}
	case !positiveFinite(pos.EntryPriceQuote):
		return State{ReasonVI: fmt.Sprintf("Giá vào %v không phải số dương hữu hạn.", pos.EntryPriceQuote)}
	case !positiveFinite(priceQuote):
		return State{ReasonVI: fmt.Sprintf("Giá hiện tại %v không phải số dương hữu hạn.", priceQuote)}
	case !positiveFinite(pos.MarginFrac):
		return State{ReasonVI: "Chưa khai báo tỷ lệ ký quỹ cho chân perp — không suy ra được giá thanh lý."}
	case pos.MarginFrac > 1:
		return State{ReasonVI: fmt.Sprintf("Tỷ lệ ký quỹ %g > 1: ký quỹ không thể lớn hơn notional.", pos.MarginFrac)}
	case bracket.MaintenanceMarginFrac < 0 || bracket.MaintenanceMarginFrac >= 1:
		return State{ReasonVI: fmt.Sprintf("Tỷ lệ ký quỹ duy trì %g nằm ngoài [0,1).", bracket.MaintenanceMarginFrac)}
	case !bracket.Verified:
		// The same refusal internal/strategy makes for an unverified fee
		// schedule. A maintenance rate nobody looked up is not zero, and a
		// zero here would put the liquidation price at (1+f)·P0 — further away
		// than the venue would ever allow.
		return State{ReasonVI: fmt.Sprintf(
			"Biểu ký quỹ duy trì của %s chưa xác minh — từ chối suy ra giá thanh lý thay vì coi như 0.",
			bracket.Source)}
	}

	ratio := pos.EntryPriceQuote / priceQuote // >1 while the short is winning
	out := State{
		LiquidationPriceQuote: pos.EntryPriceQuote * (1 + pos.MarginFrac) / (1 + bracket.MaintenanceMarginFrac),
		EquityQuote:           pos.NotionalQuote * (pos.MarginFrac + 1 - 1/ratio),
		MaintenanceQuote:      bracket.MaintenanceMarginFrac * pos.NotionalQuote / ratio,
		OK:                    true,
	}
	// Liquidated is decided against the PRICE, not by comparing equity with
	// maintenance. The two comparisons are the same inequality algebraically,
	// but at the boundary the equity form is two nearly equal numbers minus
	// each other and the last bits decide: measured here, equity and
	// maintenance both printed 180.903020 at the liquidation price and the
	// comparison still said "not liquidated". Deciding on the price uses one
	// definition, computed once, and it cannot disagree with BufferPct.
	// EquityQuote and MaintenanceQuote remain as REPORTED figures.
	out.Liquidated = priceQuote >= out.LiquidationPriceQuote
	out.BufferPct = (out.LiquidationPriceQuote - priceQuote) / priceQuote * 100
	if bracket.TierCeilingQuote > 0 && pos.NotionalQuote > bracket.TierCeilingQuote {
		// A real number computed against the wrong bracket, which is worse than
		// no number: it is optimistic by exactly the amount the stricter tier
		// would have taken.
		out.OK = false
		out.ReasonVI = fmt.Sprintf(
			"Notional %.0f vượt trần bậc %.0f của %s: bậc cao hơn có tỷ lệ duy trì khắt khe hơn, "+
				"nên con số này sẽ LẠC QUAN — từ chối công bố.",
			pos.NotionalQuote, bracket.TierCeilingQuote, bracket.Source)
	}
	return out
}

func positiveFinite(v float64) bool {
	return v > 0 && v-v == 0
}
