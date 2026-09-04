package strategy

import (
	"fmt"

	"futures-arbitrage-scanner/exchanges"
)

// The net APR calculator (step 3.1).
//
// This is the FIRST place in the roadmap allowed to use the word "net", and it
// earns it only by deducting both halves of the entry/exit cost: taker
// commission on all four fills (step 1.3) and slippage measured against the
// real order book for the intended size (step 2.7b). Everything before it says
// "gross" or "after trading fees" — see CLAUDE.md rule 2 — and everything it
// still does NOT deduct is carried in ExcludedVI beside the number.
//
// Two arithmetic rules decide the shape of this file.
//
// CLAUDE.md rule 3 — never hardcode an interval. The cadence comes from the
// reading; a year is 31,536,000 seconds and the venue's own IntervalSec says
// how many settlements fit in it. Hyperliquid and Kraken settle hourly, Binance
// runs 8h, 4h and 1h per symbol, and reading any of them as a constant is an
// error of up to 8×.
//
// CLAUDE.md rule 6 — funding is a discrete event. The hold return counts whole
// settlement INTERVALS inside the holding period; it never multiplies an APR by
// a duration. 7h59m of an 8h period pays exactly zero, and that falls out of the
// arithmetic rather than being noted in a comment. Paradex is the documented
// exception: it accrues continuously through a funding index, so its branch
// prorates the quote window instead of counting anything.
//
// Precisely what that count is, since the difference is worth stating: it is
// floor(hold / interval), which equals the number of settlements CROSSED only
// when the position opens exactly on a settlement boundary, and is one less
// than it otherwise. Counting crossings exactly needs the entry instant
// relative to the venue's settlement grid, which a forward-looking estimate
// does not have — the position has not been opened yet. Floor is therefore the
// deliberate, conservative reading (on a 30-day hold at 8h it understates by at
// most one settlement, 1.1% of gross return), and both sides of the step-3.5
// gate take it from this same function, so it cannot become a source of
// disagreement between them.
//
// The final step — annualizing the HOLD return by 365/HoldingDays — runs the
// other way round from what rule 6 forbids: it takes a return that has already
// been earned settlement by settlement and expresses it per year. The forbidden
// direction is taking an annual rate and scaling it down by holding time, which
// silently pays for periods the position was never open across.

const (
	secPerDay   = 86400
	daysPerYear = 365.0

	// The two display units. Go and SQLite hold fractions; percent and basis
	// points exist only where a number is shown or compared against a
	// configured threshold (CONVENTIONS §1.5).
	pctPerUnit = 100
	bpsPerUnit = 10000

	// maxHoldingDays bounds the amortization window at ten years.
	//
	// Not a policy on how long a position may be held — it is a guard on the
	// float64→int64 conversion of the settlement count. Go leaves that
	// conversion IMPLEMENTATION-DEPENDENT when the value does not fit, so an
	// absurd input saturates to MaxInt64 on one architecture and goes negative
	// on another. The backtest and the live path must not be able to disagree
	// about the same input; that is the entire premise of step 3.5.
	maxHoldingDays = 10 * daysPerYear
)

// GrossAPRFrac annualizes one venue's per-interval rate at that venue's real
// cadence.
//
// It delegates to exchanges.DeriveFundingRates rather than repeating the
// formula. The live readings and the stored history are both annualized by that
// function, and a second copy here would be a third implementation of the same
// arithmetic in a codebase whose phase-3 gate compares the numbers those paths
// produce (internal/backtest/doc.go).
func GrossAPRFrac(ratePerIntervalFrac float64, intervalSec int64) (float64, error) {
	derived, err := exchanges.DeriveFundingRates(exchanges.FundingData{
		Source:              "strategy",
		Symbol:              "-",
		RatePerIntervalFrac: ratePerIntervalFrac,
		IntervalSec:         intervalSec,
	})
	if err != nil {
		return 0, fmt.Errorf("strategy: gross APR: %w", err)
	}
	return derived.APRFrac, nil
}

// NetAPRInput is one venue's funding reading plus the position it would be
// acted on with.
//
// It takes the normalized rate FIELDS rather than an exchanges.FundingData so
// the live path and the backtest can both build it: the live path holds
// FundingData, the backtest holds store rows of exchanges.FundingHistoryEntry,
// and forcing either to convert into the other's type is how the two sides of
// the step-3.5 gate start drifting.
type NetAPRInput struct {
	Source string
	Symbol string

	Model               exchanges.FundingModel
	RatePerIntervalFrac float64
	IntervalSec         int64

	// HoldingDays is how long the position is expected to stay open. It is what
	// the one-off round trip is amortized over, so it is an ASSUMPTION and is
	// reported back on the result rather than being buried here.
	HoldingDays float64

	Cost RoundTrip
}

// NetAPR is the annualized return after entry and exit costs, with every
// intermediate figure kept so the number can be argued with.
//
// OK false means NetAPRFrac carries nothing. A refused figure is never zero:
// zero is a rate that pays nothing, and "we could not price this" is a
// different statement that must not be able to rank alongside real numbers.
type NetAPRResult struct {
	Source string
	Symbol string

	HoldingDays float64

	// GrossAPRFrac is the venue's rate annualized with nothing deducted. Kept
	// beside the net figure on purpose: the difference between them is the
	// entire contribution of this step, and a dashboard showing only one of the
	// two cannot show that.
	GrossAPRFrac float64

	// SettlementsInHold is how many funding payments the position is open
	// across. 0 for the continuous model, which settles nothing.
	SettlementsInHold int64

	GrossReturnHoldFrac float64
	CostHoldFrac        float64
	NetReturnHoldFrac   float64
	NetAPRFrac          float64

	OK       bool
	ReasonVI string

	DepthIsLowerBound bool
	NoteVI            string
	AppliedVI         []string
	ExcludedVI        []string
}

// NetAPR computes the net annualized return, or refuses and says why.
func NetAPR(in NetAPRInput) NetAPRResult {
	out := NetAPRResult{
		Source:      in.Source,
		Symbol:      in.Symbol,
		HoldingDays: in.HoldingDays,
		ExcludedVI:  in.Cost.ExcludedVI,
	}
	if out.ExcludedVI == nil {
		out.ExcludedVI = excludedFromRoundTrip()
	}

	if !in.Cost.OK {
		out.ReasonVI = "Không tính được chi phí vào/ra nên không có con số RÒNG nào: " + in.Cost.ReasonVI
		return out
	}
	// The cost and the rate must describe the SAME position. A cost priced on
	// one venue's book, applied to another venue's funding rate, produces a
	// number that looks entirely reasonable and is about nothing.
	if in.Cost.PerpSource != "" && in.Source != "" && in.Cost.PerpSource != in.Source {
		out.ReasonVI = fmt.Sprintf("Chi phí tính cho perp %s nhưng rate là của %s — không ghép hai thứ khác vị thế.",
			in.Cost.PerpSource, in.Source)
		return out
	}
	if in.Cost.Symbol != "" && in.Symbol != "" && in.Cost.Symbol != in.Symbol {
		out.ReasonVI = fmt.Sprintf("Chi phí tính cho cặp %s nhưng rate là của %s.", in.Cost.Symbol, in.Symbol)
		return out
	}
	if !isFinite(in.RatePerIntervalFrac) {
		out.ReasonVI = fmt.Sprintf("Funding rate %v không hữu hạn — từ chối annualize.", in.RatePerIntervalFrac)
		return out
	}
	if !isPositiveFinite(in.HoldingDays) {
		out.ReasonVI = fmt.Sprintf("Thời gian giữ dự kiến %v ngày không phải số dương hữu hạn — "+
			"chi phí một lần không khấu hao được.", in.HoldingDays)
		return out
	}
	if in.HoldingDays > maxHoldingDays {
		out.ReasonVI = fmt.Sprintf("Thời gian giữ %v ngày vượt trần %v ngày mô hình này nhận.",
			in.HoldingDays, maxHoldingDays)
		return out
	}

	grossAPRFrac, err := GrossAPRFrac(in.RatePerIntervalFrac, in.IntervalSec)
	if err != nil {
		out.ReasonVI = fmt.Sprintf("Chu kỳ funding %ds không dùng được: %v", in.IntervalSec, err)
		return out
	}
	out.GrossAPRFrac = grossAPRFrac

	holdSec := in.HoldingDays * secPerDay
	switch in.Model {
	case exchanges.FundingContinuous:
		// No settlement exists; the rate describes a QUOTE WINDOW and accrual
		// is proportional to time inside it. Counting these as payments would
		// credit 8,760 settlements a year on a venue that makes none.
		out.GrossReturnHoldFrac = in.RatePerIntervalFrac * holdSec / float64(in.IntervalSec)
	default:
		// Discrete: count whole settlements crossed and nothing else.
		out.SettlementsInHold = int64(holdSec / float64(in.IntervalSec))
		out.GrossReturnHoldFrac = in.RatePerIntervalFrac * float64(out.SettlementsInHold)
	}

	out.CostHoldFrac = in.Cost.TotalPct / pctPerUnit
	out.NetReturnHoldFrac = out.GrossReturnHoldFrac - out.CostHoldFrac
	out.NetAPRFrac = out.NetReturnHoldFrac * daysPerYear / in.HoldingDays

	if !isFinite(out.NetAPRFrac) {
		// Reachable from a rate that is finite but extreme enough to overflow
		// once annualized. Refusing beats publishing an infinity, which
		// encoding/json cannot marshal at all — one poisoned field would take
		// down the whole wire message rather than just itself.
		out.ReasonVI = fmt.Sprintf("APR ròng tính ra %v — không hữu hạn, từ chối công bố.", out.NetAPRFrac)
		out.NetAPRFrac, out.NetReturnHoldFrac, out.GrossReturnHoldFrac = 0, 0, 0
		return out
	}

	out.OK = true
	out.DepthIsLowerBound = in.Cost.DepthIsLowerBound
	out.NoteVI = in.Cost.NoteVI
	out.AppliedVI = in.Cost.AppliedVI
	return out
}
