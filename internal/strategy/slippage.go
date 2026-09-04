package strategy

import (
	"fmt"
	"math"

	"futures-arbitrage-scanner/internal/depth"
)

// Slippage estimation from the measured order book (step 3.1).
//
// This is the first place in the roadmap that turns depth into a COST. Step
// 2.7b answered "how much is resting near the price"; this answers "what would
// a fill of THIS size actually pay", which is what docs/WS-CONTRACT.md §11.2
// says the depth numbers are not yet.
//
// What there is to work with, and why the model looks the way it does: the
// store and the wire both keep depth as AGGREGATES, never as levels — two
// cumulative figures per side (within 0.1% and within 0.5% of mid), the best
// price, the spread, and how far the returned book reached. There is no level
// list to walk, in the corpus or in memory. So the model reconstructs the
// cumulative depth curve from the points that exist and integrates along it:
//
//	(0 quote, half the spread)  ← nothing fills closer to mid than the best price
//	(depth within 0.1%, 0.1%)
//	(depth within 0.5%, 0.5%)
//
// Linear between the points, which assumes liquidity is spread evenly inside a
// window. Real books are denser near the touch, so this OVERSTATES the cost of
// a fill that would have cleared at the best level — the safe direction for a
// profit estimate, and stated here rather than discovered later.
//
// Two things it deliberately refuses to do:
//
//   - Extrapolate past the measured book. A fill larger than the depth within
//     0.5% is not priced at all; it is refused with a reason, which is the hard
//     size gate of docs/PLAN.md §7.4 item 1.
//   - Let a truncated response pass as a measurement. Measured 2026-09-04, 7 of
//     9 venues do not reach 0.1% at 100 levels and none reaches 0.5%
//     (docs/WS-CONTRACT.md §11.1), so an estimate whose fill runs past the last
//     level the venue actually published is labelled a lower bound on depth —
//     which makes it an UPPER bound on cost, and the label says so.

// Side names which side of a book a fill takes. A delta-neutral round trip
// touches all four combinations of side and leg, and getting one backwards
// prices the exit against the entry's book — the asymmetry docs/PLAN.md §7.4
// item 2 exists to warn about.
type Side string

const (
	// SideBuy lifts the ASK side: buying the spot leg at entry, buying the perp
	// leg back at exit.
	SideBuy Side = "buy"
	// SideSell hits the BID side: selling the perp leg at entry, and — the leg
	// that hurts — selling the spot leg at exit.
	SideSell Side = "sell"
)

// FillEstimate is what one fill of one size on one side of one book costs.
type FillEstimate struct {
	Source string
	Symbol string
	Side   Side

	NotionalQuote float64

	// SlippagePct is the average execution price's distance from the MID, in
	// percent, so it INCLUDES crossing half the spread. Measured against mid
	// rather than against the touch on purpose: funding is charged on the mark
	// price, which tracks the mid, so the two figures share a reference and can
	// be subtracted from each other.
	SlippagePct float64

	// ReachedOffsetPct is how far from the mid the LAST unit of this fill lands.
	// It is what decides whether the estimate stayed inside the levels the
	// venue published.
	ReachedOffsetPct float64

	// Fillable is false when the book cannot absorb this size at all. The
	// estimate carries no number then — a refused fill must never contribute a
	// zero cost to a net figure.
	Fillable bool

	// DepthIsLowerBound marks an estimate whose fill ran past the farthest
	// level the venue returned. The published depth is then a floor and this
	// cost is a ceiling: the real fill is at least this good. Never the other
	// way round, which is why it is safe to act on and still must be labelled.
	DepthIsLowerBound bool

	ReasonVI string // why a fill was refused; empty when it was priced
	NoteVI   string // qualification on a priced fill; empty when it needs none
}

// curvePoint is one (cumulative notional, distance from mid) pair on the book's
// cumulative depth curve.
type curvePoint struct {
	CumNotionalQuote float64
	OffsetPct        float64
}

// EstimateFill prices one fill against one measured book.
func EstimateFill(book depth.Summary, side Side, notionalQuote float64) FillEstimate {
	out := FillEstimate{
		Source:        book.Source,
		Symbol:        book.Symbol,
		Side:          side,
		NotionalQuote: notionalQuote,
	}

	if !isPositiveFinite(notionalQuote) {
		out.ReasonVI = fmt.Sprintf("Kích thước lệnh %v không phải số dương hữu hạn — không có gì để định giá.", notionalQuote)
		return out
	}
	if !book.OK() {
		out.ReasonVI = "Không có sổ lệnh đo được cho nguồn này"
		if book.ErrVI != "" {
			out.ReasonVI += ": " + book.ErrVI
		} else {
			out.ReasonVI += "."
		}
		return out
	}

	tightQuote, wideQuote, spanPct := sideDepth(book, side)
	// Every figure the curve is built from has to be a real number. A NaN
	// slips through every comparison below (all of them are false against it),
	// leaves the integration loop untouched, and emerges as SlippagePct = 0/NaN
	// with Fillable true and no reason — a refused fill wearing the shape of a
	// priced one, which is the single thing RoundTrip's contract forbids. An
	// infinity is worse: it survives into a settlement count whose conversion
	// to int64 is implementation-dependent, so two machines disagree.
	if !isFinite(book.SpreadPct) || !isFinite(tightQuote) || !isFinite(wideQuote) || !isFinite(spanPct) {
		out.ReasonVI = fmt.Sprintf(
			"Sổ lệnh của %s chứa số không hữu hạn (spread %v, sâu 0,1%% %v, sâu 0,5%% %v, vươn tới %v) — từ chối định giá.",
			book.Source, book.SpreadPct, tightQuote, wideQuote, spanPct)
		return out
	}
	points := buildCurve(book.SpreadPct, tightQuote, wideQuote)
	deepest := points[len(points)-1]

	if notionalQuote > deepest.CumNotionalQuote {
		// Two different refusals, and the difference is exactly what the data
		// can support — no more. Whether the VENUE has more liquidity further
		// out is NOT knowable from a Summary: it carries no record of how many
		// levels were asked for, so "returned fewer levels than requested"
		// cannot be distinguished from "returned its whole book". Claiming that
		// distinction would be an operator-facing lie in the direction that
		// matters (it would suggest a re-try might find more).
		if spanPct >= depth.WindowWidePct {
			out.ReasonVI = fmt.Sprintf(
				"Sổ đo được chỉ có %.0f (quote) trong cửa sổ %.1f%% quanh mid, không đủ cho lệnh %.0f. "+
					"Sổ trả về đã vươn tới %.3f%%, tức QUÁ cửa sổ — nên con số này là phép ĐO đầy đủ của cửa sổ đó.",
				deepest.CumNotionalQuote, depth.WindowWidePct, notionalQuote, spanPct)
		} else {
			out.ReasonVI = fmt.Sprintf(
				"Sổ trả về chỉ vươn tới %.3f%% khỏi mid, chưa phủ hết cửa sổ %.1f%%, và tổng %.0f (quote) trong đó "+
					"không đủ cho lệnh %.0f. Con số này là CẬN DƯỚI của thanh khoản trong cửa sổ — không phân biệt được "+
					"'sàn chỉ có ngần ấy' với 'sàn chỉ trả ngần ấy mức'.",
				spanPct, depth.WindowWidePct, deepest.CumNotionalQuote, notionalQuote)
		}
		return out
	}

	sumPctQuote, reachedPct := integrate(points, notionalQuote)
	out.Fillable = true
	out.SlippagePct = sumPctQuote / notionalQuote
	out.ReachedOffsetPct = reachedPct

	if reachedPct > spanPct {
		out.DepthIsLowerBound = true
		out.NoteVI = fmt.Sprintf(
			"Lệnh này ăn tới %.4f%% khỏi mid trong khi sàn chỉ công bố tới %.4f%%: độ sâu là CẬN DƯỚI "+
				"nên chi phí %.4f%% là cận TRÊN — fill thật không tệ hơn con số này.",
			reachedPct, spanPct, out.SlippagePct)
	}
	return out
}

// sideDepth picks the cumulative figures and the reach of the side being taken.
func sideDepth(book depth.Summary, side Side) (tightQuote, wideQuote, spanPct float64) {
	if side == SideBuy {
		return book.AskDepthWithinTightQuote, book.AskDepthWithinWideQuote, book.AskSpanPct
	}
	return book.BidDepthWithinTightQuote, book.BidDepthWithinWideQuote, book.BidSpanPct
}

// buildCurve assembles the cumulative depth curve, dropping any point that does
// not move BOTH axes forward.
//
// Both degenerate shapes are real and neither is an error: a venue whose spread
// is already wider than the tight window publishes no depth inside it, and a
// venue whose book stops short of 0.5% reports the same figure for both windows
// (Bybit's perp book spans 0.091%, so its two figures are identical). Keeping a
// flat or backward point would divide by a zero-width segment.
func buildCurve(spreadPct, tightQuote, wideQuote float64) []curvePoint {
	points := []curvePoint{{CumNotionalQuote: 0, OffsetPct: spreadPct / 2}}
	add := func(cumQuote, offsetPct float64) {
		last := points[len(points)-1]
		if cumQuote > last.CumNotionalQuote && offsetPct > last.OffsetPct {
			points = append(points, curvePoint{CumNotionalQuote: cumQuote, OffsetPct: offsetPct})
		}
	}
	add(tightQuote, depth.WindowTightPct)
	add(wideQuote, depth.WindowWidePct)
	return points
}

// integrate walks the curve to notionalQuote, returning the integral of the
// offset over the filled notional and the offset the last unit reaches.
//
// The integral of a linear segment is its trapezoid, so this is exact for the
// curve it is given — the modelling assumption is the curve's shape, not the
// arithmetic over it.
func integrate(points []curvePoint, notionalQuote float64) (sumPctQuote, reachedPct float64) {
	reachedPct = points[0].OffsetPct
	remaining := notionalQuote

	for i := 1; i < len(points) && remaining > 0; i++ {
		from, to := points[i-1], points[i]
		width := to.CumNotionalQuote - from.CumNotionalQuote
		slope := (to.OffsetPct - from.OffsetPct) / width

		taken := width
		if remaining < taken {
			taken = remaining
		}
		endPct := from.OffsetPct + slope*taken
		sumPctQuote += (from.OffsetPct + endPct) / 2 * taken
		reachedPct = endPct
		remaining -= taken
	}
	return sumPctQuote, reachedPct
}

// isFinite rejects the two float64 values that survive an ordinary range check.
//
// Every guard in this package used to be `x <= 0`, which is FALSE for NaN, so a
// NaN walked through each one and came out the far side labelled a real figure.
func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// isPositiveFinite is the check a size, a price or a duration has to pass
// before anything divides by it or sums it.
func isPositiveFinite(v float64) bool { return isFinite(v) && v > 0 }
