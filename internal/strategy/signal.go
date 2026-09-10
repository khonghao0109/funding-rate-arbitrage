package strategy

import (
	"fmt"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/fees"
	"futures-arbitrage-scanner/internal/risk"
)

// Entry and exit signals (step 3.2).
//
// Two functions, EvaluateEntry and EvaluateExit, and the whole point of the
// package is that these are the ONLY implementations: the live path and
// internal/backtest both call them, because docs/PLAN.md step 3.5 gates the
// project on the two agreeing, and a gate between two implementations cannot
// tell "the strategy is wrong" from "the implementations drifted" (Q8, §7.1).
//
// # Which rate decides
//
// The decision is made on the venue's SETTLED history, never on the rate for a
// period still running. Two independent reasons, and they point the same way:
//
//   - Only settled rates exist on both sides of the 3.5 gate. A backtest
//     standing at an instant in the past has the settlements up to it and
//     nothing else. If entry depended on a live forming rate, the backtest
//     could not reproduce the decision even in principle.
//   - IsEstimated does not mean the same thing at every venue, so a rule
//     phrased in terms of it would mean seven different things. Measured
//     (CLAUDE.md trap table, DATA-REQUIREMENTS §3.3⑥): Gate's flag is true
//     because the rate genuinely drifts mid-period; Kraken's is false because
//     its WS field is the rate that ALREADY SETTLED for the hour just
//     finished — it does not forecast the next stamp at all, and its forming
//     estimate lives in a different field this project does not read. Filtering
//     venues on that flag, in either direction, is the naive reading the trap
//     table warns about: it would drop Gate for being honest about drift, or
//     credit Kraken with a forecast it never made.
//
// So a live reading is carried for the operator's log and never enters the
// arithmetic. Nothing is lost: what makes a funding position worth opening is
// that the regime has PERSISTED, and persistence is a statement about rates
// that have already been paid.
//
// # Comparison unit
//
// Every rate comparison is on RatePer8hFrac. Venues settle hourly, 4-hourly and
// 8-hourly, and the same per-interval number is three different regimes
// depending on which (CLAUDE.md rules 3 and 4).

// Action is what the signal says to do.
type Action string

const (
	ActionEnter Action = "enter"
	ActionSkip  Action = "skip"
	ActionHold  Action = "hold"
	ActionExit  Action = "exit"
)

// Check is one condition, its verdict, and the numbers behind it.
//
// Name is a stable machine identifier so an alert can be throttled per failing
// condition; DetailVI is the human sentence. Both are required: the acceptance
// criterion for this step is that a signal logs its full reasoning, and a
// verdict with no numbers cannot be argued with after the fact.
//
// Polarity differs by direction and is stated on each function: for an ENTRY,
// Passed means the condition is satisfied and entry may proceed. For an EXIT,
// Passed means the condition FIRED — the thing it watches for has happened.
type Check struct {
	Name     string
	Passed   bool
	DetailVI string

	// NotEvaluated marks a check that could not be judged at all — a dependency
	// failed (no hedge leg, so nothing to price) or an input was absent (no
	// prices, so no basis). Distinct from Passed=false: "not tested" and
	// "tested and failed" must never read alike, and a consumer counting how
	// often a condition went untested (internal/backtest) reads THIS flag, not
	// the wording of DetailVI.
	NotEvaluated bool
}

// Candidate is one perp, its hedge leg, and everything needed to judge them.
//
// Deliberately built from normalized values only: settled history as the store
// and the venues' REST both deliver it, fee schedules from config, books from
// internal/depth. No raw venue payload reaches this package (doc.go).
type Candidate struct {
	Symbol     string
	PerpSource string

	// SpotSource is the spot market this perp can actually be hedged against,
	// or "" when the mapping found none — in which case HedgeNoteVI says why in
	// the venues' own terms. A USD-quoted perp refused against USDT-only spot
	// markets is CORRECT behaviour (step 2.4), and the signal's job is to not
	// emit a lead for a position nobody can open.
	SpotSource  string
	HedgeNoteVI string

	// Settled is the venue's already-paid rates for this pair, OLDEST FIRST.
	Settled []exchanges.FundingHistoryEntry

	// LatestVI describes the venue's current live reading, if the caller has
	// one. It is LOGGED and never computed with — see the header.
	LatestVI string

	SpotFee fees.Schedule
	PerpFee fees.Schedule

	SpotBook depth.Summary
	PerpBook depth.Summary

	SpotPriceQuote float64
	PerpPriceQuote float64

	// PerpMargin is the maintenance bracket the perp venue publishes for a
	// position this size. An unverified one refuses to produce a liquidation
	// price rather than defaulting to zero — the same rule the fee schedules
	// follow, and for the same reason.
	PerpMargin risk.Bracket
}

// Position is an open funding position, as much of it as the exit rule needs.
//
// CLAUDE.md rule 7 — a real position is read from the venue, never from local
// state. This struct is what the CALLER read; nothing here caches it.
type Position struct {
	Symbol     string
	PerpSource string
	SpotSource string

	OpenedAtMs    int64
	NotionalQuote float64

	// PerpEntryPriceQuote is what the perp leg was SHORTED at, which together
	// with the notional fixes the quantity and hence the liquidation price. 0
	// means it was not recorded, and the margin condition then reports that
	// rather than inventing a price to divide by.
	PerpEntryPriceQuote float64

	// EntryBasisPct is the perp-over-spot difference when the position was
	// opened. The exit rule watches how far it has MOVED, not only its level:
	// a position opened at a 0.4% basis and now at 0.6% has drifted less than
	// one opened at 0.0% and now at 0.5%.
	EntryBasisPct float64
}

// Decision is one evaluation: what to do, and every condition behind it.
type Decision struct {
	At         time.Time
	Symbol     string
	PerpSource string
	SpotSource string

	Action Action
	Checks []Check

	// NetAPR is the figure the decision was made on. Its OK flag may be false —
	// that is itself one of the reasons an entry is refused.
	NetAPR NetAPRResult

	// Cost is what the round trip was priced at, kept so an alert can say what
	// has been deducted without recomputing it.
	Cost RoundTrip
}

// LogLines renders the decision for a log or an alert, verdict first.
//
// PLAN.md step 3.2's acceptance criterion is exactly this: every signal carries
// its full reasoning. A one-line "entered BTCUSDT" is not debuggable a week
// later, when the question is which condition was marginal.
func (d Decision) LogLines() []string {
	lines := []string{fmt.Sprintf("[%s] %s %s/%s ← %s",
		d.At.UTC().Format(time.RFC3339), d.Action, d.Symbol, d.PerpSource, d.SpotSourceVI())}
	for _, check := range d.Checks {
		mark := "✗"
		if check.Passed {
			mark = "✓"
		}
		lines = append(lines, fmt.Sprintf("  %s %-18s %s", mark, check.Name, check.DetailVI))
	}
	if d.NetAPR.OK {
		lines = append(lines, fmt.Sprintf("  · APR thô %.2f%% → RÒNG %.2f%% (chi phí vòng %.4f%%, giữ %.0f ngày)",
			d.NetAPR.GrossAPRFrac*pctPerUnit, d.NetAPR.NetAPRFrac*pctPerUnit,
			d.Cost.TotalPct, d.NetAPR.HoldingDays))
		if d.NetAPR.DepthIsLowerBound {
			lines = append(lines, "  ⚠ "+d.NetAPR.NoteVI)
		}
	}
	if d.Cost.OK {
		for _, applied := range d.Cost.AppliedVI {
			lines = append(lines, "  − đã trừ: "+applied)
		}
		for _, excluded := range d.Cost.ExcludedVI {
			lines = append(lines, "  ! CHƯA trừ: "+excluded)
		}
	}
	return lines
}

// SpotSourceVI names the hedge leg, or says there is none.
func (d Decision) SpotSourceVI() string {
	if d.SpotSource == "" {
		return "KHÔNG CÓ CHÂN HEDGE"
	}
	return d.SpotSource
}

// Failed lists the conditions that did not hold, for a caller that wants the
// short form.
func (d Decision) Failed() []Check {
	var out []Check
	for _, check := range d.Checks {
		if !check.Passed {
			out = append(out, check)
		}
	}
	return out
}

// Params is the strategy's tuning. Every field carries its unit (CONVENTIONS
// §1); the sweep in internal/backtest varies exactly these.
type Params struct {
	// --- entry ---
	// MinRatePer8hBps is the floor the SETTLED rate must clear, quoted per 8h
	// so venues on different cadences are comparable.
	MinRatePer8hBps float64
	// PersistencePeriods is how many consecutive recent settlements must clear
	// that floor. One good period is noise; the strategy is paid for a regime.
	PersistencePeriods int
	// MinNetAPRFrac is the floor on the NET figure — after commission and
	// slippage, amortized over HoldingDays.
	MinNetAPRFrac float64

	NotionalQuote float64
	HoldingDays   float64
	MaxBookAge    time.Duration

	// --- series selection (added 2026-09-09) ---
	// MinTrailingMeanBps is the floor the series' MEAN settled rate over the
	// last TrailingMeanDays must clear, per 8h. 0 turns the condition off,
	// which is every run before the field existed (pinned by test).
	//
	// This is the CAPITAL-ALLOCATION rule: which series gets a position at
	// all. The persistence check asks whether the last few settlements were
	// good; this one asks whether the series has PAID over a horizon of the
	// order of the hold, because the break-even horizon measured on
	// 2026-09-09 is 21–52 days on the majors and infinite on the six series
	// whose mean funding sits below the round trip — and no exit rule saves a
	// trade opened there. Measured the same day on 13 pairs: pointing the
	// shipped set at the BTC+ETH series alone doubled its return on capital
	// at half the drawdown, and nothing in this package decided that.
	//
	// DAYS, not a count of settlements (CLAUDE.md rule 3): 270 settlements is
	// 90 days at an 8h cadence and 11 at an hourly one, so one swept count
	// would be a different horizon on every venue. The window is measured on
	// the stamps, and a history that does not reach back to its start
	// refuses rather than averaging what it has.
	MinTrailingMeanBps float64
	TrailingMeanDays   float64
	// TrailingMeanMinCostFrac (added 2026-09-10) is the same selection against
	// the series' OWN cost-crossing instead of one number for every series:
	// the funding the trailing mean would pay over HoldingDays, counted in
	// settlements at the venue's cadence exactly as NetAPR counts them, must
	// reach this fraction of the priced round trip. 1.0 is break-even — "do
	// not open a series whose trailing funding has not been paying for its
	// own round trip". 0 is off, which is every run before the field existed
	// (pinned by test), and a fraction with no TrailingMeanDays is off too.
	//
	// Why a fraction of the cost and not a level: the three-year study
	// (docs/reports/regime-3y-2026-09-09.html, regularity 2) found the level
	// that pays a round trip is 0.33 bps/8h at BTC/ETH's 0.30% trip and 0.8
	// at NEAR's 0.74%, and that it moved from ≈0.25 to ≈0.8 between years —
	// a universal floor is wrong on every series but the one it was tuned
	// on. Both floors may be set; the mean has to clear each.
	TrailingMeanMinCostFrac float64

	// --- exit ---
	// ExitNetAPRFrac is lower than MinNetAPRFrac on purpose: entering costs a
	// round trip, so the bar to STAY in is below the bar to get in, or the
	// position churns across the entry threshold paying commission each way.
	ExitNetAPRFrac float64
	// ExitPersistencePeriods is how many CONSECUTIVE recent settlements must
	// come in under ExitNetAPRFrac before the position closes for decay. 0 and
	// 1 both mean "close on the first one".
	//
	// It exists because the level hysteresis above is not enough on its own.
	// Entry is decided on a persistence window; if exit were decided on a
	// single print, the exit rule would be strictly more sensitive than the
	// entry rule and the gap between the two thresholds would buy nothing.
	// Measured on the real corpus — binance BTCUSDT, August 2026, per 8h:
	// 0.79 → 0.51 → 0.23 → 0.20 → 0.83 → 1.00 bps. A single-print rule closes
	// at 0.20 and misses the recovery two settlements later, paying a round
	// trip in each direction to do it.
	//
	// This does NOT slow the sign-flip exit: funding turning negative is money
	// leaving on every settlement (risk R1) and fires on the newest reading —
	// unless the ExitNegative* gates below are set.
	ExitPersistencePeriods int

	// --- sign-flip exit gates (added 2026-09-07) ---
	// The zero values reproduce the rule exactly as step 3.2 wrote it: close on
	// the first settled negative rate, whatever its size. Measured on the
	// 12-month binance corpus that day, that rule closed 78% of every trade in
	// the parameter sweep, while the negative episode it fled cost a median
	// 0.3 bps (90th percentile 2.7, worst 9.1) against a 30 bps round trip —
	// no episode in a year cost more than the exit did (PLAN 3.3). The gates
	// let a sweep ask how deep, how long or how expensive a negative regime
	// has to be before leaving beats staying. They combine with AND: every
	// configured gate has to agree before the position closes.
	//
	// ExitNegativeMinBps: the newest settled rate must be at or below
	// −ExitNegativeMinBps bps/8h. 0 = any negative print.
	ExitNegativeMinBps float64
	// ExitNegativePeriods: the last N settled rates must all be negative. 0
	// and 1 both mean the newest alone.
	ExitNegativePeriods int
	// ExitNegativeCumCostFrac: the funding PAID over the current negative run
	// (consecutive negative settlements ending at the newest, as a fraction of
	// notional) must reach this fraction of the round-trip cost. 0 = no such
	// gate. When the round trip cannot be priced the gate counts as met — the
	// same fail-safe direction the decay exit takes.
	//
	// JURISDICTION. Negative prints belong to this exit and its gates ONLY.
	// The decay exit (ExitNetAPRFrac / ExitPersistencePeriods) judges the
	// last ExitPersistencePeriods NON-negative settlements; it never counts a
	// negative one. Before 2026-09-07 it did, and since every negative print
	// is under any non-negative floor, ExitPersistencePeriods was a hard
	// ceiling on all three gates: the position closed at the P-th negative
	// print whatever the gates said, labelled "decay" — found by the review
	// of the gate sweep, where half the grid was degenerate for that reason.
	// A continuous-model series (Paradex) has samples, not settlements, so the
	// N and C gates are not evaluable there and the 3.2 rule applies.
	ExitNegativeCumCostFrac float64
	// --- minimum hold (added 2026-09-07) ---
	// MinHoldRecoveredCostFrac blocks the two YIELD exits — the sign flip and
	// the decay — until the funding THIS position has collected reaches that
	// fraction of its round-trip cost. 0 is off, and off is the rule exactly
	// as it stood before this field existed (pinned by test).
	//
	// It is a fraction of the round trip rather than a count of settlements on
	// purpose. A count means eight different things across this project's
	// venues: 98 settlements is 33 days at Binance's 8h cadence and 4 days at
	// Hyperliquid's hourly one, so one swept value would be one rule on paper
	// and 24 rules in the results (CLAUDE.md rules 3 and 4). A fraction of the
	// cost is the same statement everywhere: do not pay to leave until you
	// have earned back what leaving costs.
	//
	// Measured on the 12-month corpus, which is what the floor is FOR: the
	// median trade in the wide sweep held 0.67 days (sign flip) and 2.17 days
	// (decay), collected 13 and 27 settlements, and lost 0.30% and 0.27% — the
	// exits fire long before funding covers the round trip, and the trades
	// that made money were the ones the rules never closed.
	//
	// JURISDICTION, and it is the whole design. The floor covers exits about
	// YIELD and never exits about RISK. A vanished hedge leg, a basis blown
	// through its limit, and losing the ability to price the position at all
	// are not judgements about whether holding pays — they are statements that
	// the position is no longer the position that was opened — so they close
	// it whatever the floor says. A floor that could pin a naked short open
	// would be a worse bug than the churn it was written to stop.
	//
	// It fails SAFE: with no priced round trip there is no denominator, and
	// the floor steps aside rather than blocking every exit on a venue whose
	// book cannot be read. Same direction as ExitNegativeCumCostFrac.
	MinHoldRecoveredCostFrac float64

	// MaxBasisPct is the absolute perp-over-spot difference beyond which the
	// position is closed regardless of funding; MaxBasisWidenPct is how far it
	// may move from where it was opened.
	MaxBasisPct      float64
	MaxBasisWidenPct float64

	// --- margin on the perp leg (added 2026-09-07) ---
	// PerpMarginFrac is the collateral posted on the SHORT PERP leg as a
	// fraction of its notional — the reciprocal of leverage, so 0.10 is 10x.
	// 0 turns the margin condition off entirely, which is what every run
	// before this field existed did.
	//
	// It is a DECISION and not a venue fact. The venue sets only the maximum
	// leverage; how much collateral to post against a delta-neutral short is
	// the operator's, and it is the single biggest lever on whether this
	// strategy survives a rally: at 10x a short liquidates about 9.4% up, at
	// 3x about 32%, at 1x about 99%.
	PerpMarginFrac float64
	// MinLiquidationBufferPct closes the position when the price is within
	// this many percent of the perp leg's liquidation price. 0 means "only
	// when actually liquidated", which is too late to be a rule.
	//
	// This is a RISK exit, not a yield one: MinHoldRecoveredCostFrac never
	// blocks it. A position that is about to be force-closed by the venue is
	// not a position whose funding economics are still the question.
	MinLiquidationBufferPct float64
}

// EffectiveExitNegativePeriods is the gate as the rule reads it: 0 and 1 both
// mean the newest print alone. Journals and CSVs write THIS, so an absent
// key, a zero and a one are the same rule in every artifact.
func (p Params) EffectiveExitNegativePeriods() int {
	if p.ExitNegativePeriods < 1 {
		return 1
	}
	return p.ExitNegativePeriods
}

// EvaluateEntry decides whether to open a position, and reports every condition.
//
// `at` is passed in and never read from a clock: the backtest evaluates
// historical instants (doc.go).
//
// All checks run even after one fails. Short-circuiting would name the first
// problem and hide the rest, so an operator would fix one thing, re-run, and
// find the next — while the log had the answer all along.
//
// Check polarity here: Passed means the condition is SATISFIED.
func EvaluateEntry(at time.Time, c Candidate, p Params) Decision {
	d := Decision{At: at, Symbol: c.Symbol, PerpSource: c.PerpSource, SpotSource: c.SpotSource, Action: ActionSkip}

	usable, droppedSpecial := UsableSettled(c.Settled)
	hedge := checkHedgeLeg(c)

	// The cost is only priced when there IS a hedge leg. Without one there is
	// no spot book and no spot fee schedule, and pricing against those zero
	// values made the liquidity and net-APR checks report a FEE problem for an
	// unnamed venue — three failures describing one cause, and the loudest of
	// them pointing at the wrong thing. Seen on real data: every USD-quoted
	// perp logged "biểu phí của (nguồn không tên) chưa xác minh" when its
	// actual and only problem was having no USDT spot market to hedge against.
	if hedge.Passed {
		d.Cost = RoundTripCost(RoundTripInput{
			NotionalQuote: p.NotionalQuote,
			SpotFee:       c.SpotFee, PerpFee: c.PerpFee,
			SpotBook: c.SpotBook, PerpBook: c.PerpBook,
			At: at, MaxBookAge: p.MaxBookAge,
		})
	}
	newest, haveNewest := newestOf(usable)
	if haveNewest && hedge.Passed {
		d.NetAPR = NetAPR(NetAPRInput{
			Source: c.PerpSource, Symbol: c.Symbol,
			Model:               newest.Model,
			RatePerIntervalFrac: newest.RatePerIntervalFrac,
			IntervalSec:         newest.IntervalSec,
			HoldingDays:         p.HoldingDays,
			Cost:                d.Cost,
		})
	}

	d.Checks = []Check{
		hedge,
		checkHistoryDepth(usable, droppedSpecial, p),
		checkRateThreshold(newest, haveNewest, p),
		checkPersistence(usable, p),
		checkTrailingMean(at, usable, c, d.Cost, hedge.Passed, p),
		checkLiquidity(d.Cost, p, hedge.Passed),
		checkNetAPR(d.NetAPR, p, hedge.Passed),
		checkMarginKnown(c, p),
	}

	for _, check := range d.Checks {
		if !check.Passed {
			return d
		}
	}
	d.Action = ActionEnter
	return d
}

// EvaluateExit decides whether to close an open position.
//
// Check polarity here is INVERTED against EvaluateEntry: Passed means the
// condition FIRED — the thing it watches for has happened — and any one of them
// firing closes the position. Exits are disjunctive because each condition is
// independently sufficient: funding that has turned costs money every
// settlement, and a hedge leg that has vanished means the position is not
// delta-neutral any more whatever funding does.
func EvaluateExit(at time.Time, pos Position, c Candidate, p Params) Decision {
	d := Decision{At: at, Symbol: pos.Symbol, PerpSource: pos.PerpSource, SpotSource: c.SpotSource, Action: ActionHold}

	usable, _ := UsableSettled(c.Settled)
	newest, haveNewest := newestOf(usable)

	hedgeGone := exitHedgeGone(c)
	if !hedgeGone.Passed {
		d.Cost = RoundTripCost(RoundTripInput{
			NotionalQuote: pos.NotionalQuote,
			SpotFee:       c.SpotFee, PerpFee: c.PerpFee,
			SpotBook: c.SpotBook, PerpBook: c.PerpBook,
			At: at, MaxBookAge: p.MaxBookAge,
		})
	}
	if haveNewest && !hedgeGone.Passed {
		d.NetAPR = NetAPR(NetAPRInput{
			Source: pos.PerpSource, Symbol: pos.Symbol,
			Model:               newest.Model,
			RatePerIntervalFrac: newest.RatePerIntervalFrac,
			IntervalSec:         newest.IntervalSec,
			HoldingDays:         p.HoldingDays,
			Cost:                d.Cost,
		})
	}

	floor := minHoldFloor(pos, usable, d.Cost, p)
	d.Checks = []Check{
		hedgeGone,
		exitFundingNegative(usable, d.Cost, p, floor),
		exitNetAPRFloor(pos, usable, d.Cost, d.NetAPR, p, hedgeGone.Passed, floor),
		exitBasisWidened(pos, c, p),
		// A RISK condition, evaluated after the others and blocked by nothing:
		// the floor above governs exits about YIELD only.
		exitMarginThin(pos, c, p),
	}

	for _, check := range d.Checks {
		if check.Passed {
			d.Action = ActionExit
			return d
		}
	}
	return d
}

// --- entry conditions ---

func checkHedgeLeg(c Candidate) Check {
	if c.SpotSource != "" {
		return Check{Name: "hedge_leg", Passed: true, DetailVI: fmt.Sprintf("Chân hedge: spot %s.", c.SpotSource)}
	}
	note := c.HedgeNoteVI
	if note == "" {
		note = "bảng ghép spot↔perp không có chân nào cho perp này."
	}
	return Check{Name: "hedge_leg", Passed: false, DetailVI: fmt.Sprintf("KHÔNG mở được vị thế: perp %s không có chân spot để hedge — %s", c.PerpSource, note)}
}

func checkHistoryDepth(usable []exchanges.FundingHistoryEntry, droppedSpecial int, p Params) Check {
	detail := fmt.Sprintf("Có %d mốc settle dùng được, cần %d để xét độ bền.", len(usable), p.PersistencePeriods)
	if droppedSpecial > 0 {
		detail += fmt.Sprintf(" (Đã loại %d mốc rateType=Special — cổ tức, không phải chế độ funding.)", droppedSpecial)
	}
	return Check{Name: "history_depth", Passed: p.PersistencePeriods > 0 && len(usable) >= p.PersistencePeriods, DetailVI: detail}
}

func checkRateThreshold(newest exchanges.FundingHistoryEntry, have bool, p Params) Check {
	if !have {
		return Check{Name: "rate_threshold", Passed: false, DetailVI: "Không có mốc settle nào để so ngưỡng."}
	}
	bps := newest.RatePer8hFrac * bpsPerUnit
	return Check{Name: "rate_threshold", Passed: bps >= p.MinRatePer8hBps, DetailVI: fmt.Sprintf(
		"Mốc settle mới nhất %.4f bps/8h so với ngưỡng %.4f bps/8h (chu kỳ thật %ds).",
		bps, p.MinRatePer8hBps, newest.IntervalSec)}
}

// checkPersistence requires the LAST PersistencePeriods settlements to have all
// cleared the threshold.
//
// Consecutive and recent, not an average: an average lets one enormous
// settlement carry a series that has otherwise gone quiet, which is the regime
// this strategy is least able to hold through.
func checkPersistence(usable []exchanges.FundingHistoryEntry, p Params) Check {
	if p.PersistencePeriods <= 0 {
		return Check{Name: "persistence", Passed: false, DetailVI: "PersistencePeriods không dương — tham số sai."}
	}
	if len(usable) < p.PersistencePeriods {
		return Check{Name: "persistence", Passed: false, DetailVI: fmt.Sprintf(
			"Chỉ có %d mốc, không đủ %d để kết luận độ bền.", len(usable), p.PersistencePeriods)}
	}
	window := usable[len(usable)-p.PersistencePeriods:]
	var worstBps = window[0].RatePer8hFrac * bpsPerUnit
	held := 0
	for _, entry := range window {
		bps := entry.RatePer8hFrac * bpsPerUnit
		if bps < worstBps {
			worstBps = bps
		}
		if bps >= p.MinRatePer8hBps {
			held++
		}
	}
	return Check{Name: "persistence", Passed: held == len(window), DetailVI: fmt.Sprintf(
		"%d/%d mốc gần nhất trên ngưỡng %.4f bps/8h; mốc thấp nhất trong cửa sổ %.4f bps/8h.",
		held, len(window), p.MinRatePer8hBps, worstBps)}
}

// checkTrailingMean is the series-selection condition: the mean settled rate
// over the last TrailingMeanDays, per 8h, must clear every floor configured —
// MinTrailingMeanBps, one number for every series, and/or the series' OWN
// cost-crossing (TrailingMeanMinCostFrac, 2026-09-10): the level at which
// funding at that mean, held for HoldingDays at the venue's cadence, pays the
// configured fraction of the priced round trip.
//
// The window is cut on the venue's stamps, never on a count, and it must be
// COVERED: a history that starts inside the window would average a shorter
// horizon than the one configured and call it the same number. One interval
// of slack at the start, because the first settlement inside a window that
// begins at 00:00 lands at the cadence boundary after it, not on it.
//
// The mean is of the per-8h rate across settlements, which is the figure the
// pair screen ranks on (tools/report/pairscreen.py), so "0.94 bps/8h on the
// screen" and "0.94 bps/8h here" are the same statement about the same series.
//
// The crossing is read off NetAPR rather than restated. NetAPR with a UNIT
// per-interval rate returns, as its gross hold return, the settlements the
// hold crosses (or the continuous model's time ratio) and, beside it, the
// cost as a fraction of notional; the per-interval rate that pays `frac`
// round trips is frac × cost ÷ that, and per 8h it is that × 8h ÷ interval.
// One arithmetic serves the entry's net APR and this floor, which is the
// point: at frac = 1 the floor is exactly "NetAPR of the trailing mean ≥ 0",
// and both sides of the 3.5 gate take it from the same function. Per 8h the
// crossing does not depend on the cadence — a 0.30% trip over 30 days is 90
// settlements at 8h or 720 at 1h, the same money — which is the reason the
// comparison unit is per 8h at all.
//
// It depends on a priced round trip, so without one — no hedge leg, or a
// schedule that refuses to price — it reports NOT EVALUATED rather than a
// second cause beside the hedge/liquidity check that already names the real
// one; the absolute floor alone never needed the cost and still does not.
func checkTrailingMean(at time.Time, usable []exchanges.FundingHistoryEntry, c Candidate, cost RoundTrip, hedged bool, p Params) Check {
	absoluteOn := p.MinTrailingMeanBps > 0 && p.TrailingMeanDays > 0
	crossingOn := p.TrailingMeanMinCostFrac > 0 && p.TrailingMeanDays > 0
	if !absoluteOn && !crossingOn {
		return Check{Name: "trailing_mean", Passed: true,
			DetailVI: "Không xét funding trung bình của chuỗi (min_trailing_mean_bps = 0, trailing_mean_min_cost_frac = 0) — mọi chuỗi đủ điều kiện khác đều được vào."}
	}
	if len(usable) == 0 {
		return Check{Name: "trailing_mean", Passed: false, DetailVI: "Không có mốc settle nào để tính funding trung bình."}
	}
	startMs := at.Add(-time.Duration(p.TrailingMeanDays * float64(24*time.Hour))).UnixMilli()
	newest := usable[len(usable)-1]
	if usable[0].SettledAtMs > startMs+newest.IntervalSec*1000 {
		return Check{Name: "trailing_mean", Passed: false, DetailVI: fmt.Sprintf(
			"Lịch sử chỉ bắt đầu từ %s, chưa phủ %g ngày để tính funding trung bình — không lấy trung bình của một cửa sổ ngắn hơn thay thế.",
			time.UnixMilli(usable[0].SettledAtMs).UTC().Format("2006-01-02"), p.TrailingMeanDays)}
	}
	var sum float64
	n := 0
	for i := len(usable) - 1; i >= 0 && usable[i].SettledAtMs >= startMs; i-- {
		sum += usable[i].RatePer8hFrac
		n++
	}
	if n == 0 {
		return Check{Name: "trailing_mean", Passed: false, DetailVI: fmt.Sprintf(
			"Không có mốc settle nào trong %g ngày gần nhất.", p.TrailingMeanDays)}
	}
	meanBps := sum / float64(n) * bpsPerUnit

	floorPer8hBps, floors := 0.0, ""
	if absoluteOn {
		floorPer8hBps = p.MinTrailingMeanBps
		floors += fmt.Sprintf("; ngưỡng chọn chuỗi %.4f bps/8h", p.MinTrailingMeanBps)
	}
	if crossingOn {
		if !hedged {
			return Check{Name: "trailing_mean", NotEvaluated: true, DetailVI: notEvaluatedVI}
		}
		if !cost.OK {
			return Check{Name: "trailing_mean", NotEvaluated: true,
				DetailVI: "Chưa đánh giá điểm cắt chi phí — không định giá được vòng vào/ra, xem điều kiện liquidity."}
		}
		// A UNIT per-interval rate: its gross hold return is the settlement
		// count (or the continuous model's time ratio), with the same
		// identity, hold and cadence checks the real rate gets.
		unitRate := NetAPR(NetAPRInput{
			Source: c.PerpSource, Symbol: c.Symbol,
			Model:               newest.Model,
			RatePerIntervalFrac: 1,
			IntervalSec:         newest.IntervalSec,
			HoldingDays:         p.HoldingDays,
			Cost:                cost,
		})
		if !unitRate.OK {
			return Check{Name: "trailing_mean", Passed: false, DetailVI: "Không tính được điểm cắt chi phí của chuỗi: " + unitRate.ReasonVI}
		}
		if unitRate.GrossReturnHoldFrac <= 0 {
			return Check{Name: "trailing_mean", Passed: false, DetailVI: fmt.Sprintf(
				"Giữ %g ngày không qua nổi một mốc settle ở nhịp %ds — không có điểm cắt chi phí để so.",
				p.HoldingDays, newest.IntervalSec)}
		}
		crossingPerIntervalFrac := p.TrailingMeanMinCostFrac * unitRate.CostHoldFrac / unitRate.GrossReturnHoldFrac
		// Per 8h through the ONE per-8h derivation the readings and the
		// stored history share (GrossAPRFrac delegates to it for the same
		// reason): a second copy of "× 8h ÷ interval" here would drift.
		crossing, err := exchanges.DeriveFundingRates(exchanges.FundingData{
			Source: c.PerpSource, Symbol: c.Symbol,
			RatePerIntervalFrac: crossingPerIntervalFrac, IntervalSec: newest.IntervalSec,
		})
		if err != nil {
			return Check{Name: "trailing_mean", Passed: false, DetailVI: "Không quy điểm cắt chi phí về 8h được: " + err.Error()}
		}
		crossingPer8hBps := crossing.RatePer8hFrac * bpsPerUnit
		if crossingPer8hBps > floorPer8hBps {
			floorPer8hBps = crossingPer8hBps
		}
		over := fmt.Sprintf("%d mốc", unitRate.SettlementsInHold)
		if newest.Model == exchanges.FundingContinuous {
			over = "mẫu liên tục, quy theo thời gian"
		}
		floors += fmt.Sprintf("; điểm cắt chi phí của chuỗi này %.4f bps/8h (= %.2f × vòng %.4f%% đã định giá chia cho %s của %g ngày giữ ở nhịp %ds)",
			crossingPer8hBps, p.TrailingMeanMinCostFrac, cost.TotalPct, over, p.HoldingDays, newest.IntervalSec)
	}
	passed := meanBps >= floorPer8hBps
	verdict := "đạt"
	if !passed {
		verdict = "KHÔNG đạt"
	}
	return Check{Name: "trailing_mean", Passed: passed, DetailVI: fmt.Sprintf(
		"Funding trung bình %.4f bps/8h qua %d mốc trong %g ngày%s — %s.",
		meanBps, n, p.TrailingMeanDays, floors, verdict)}
}

// checkMarginKnown refuses to OPEN a leveraged short whose liquidation price
// cannot be stated.
//
// It is an ENTRY condition and that placement is the point. The exit side
// (exitMarginThin) also leaves when the liquidation price becomes unknowable,
// which is right for a position already open — but if that were the only rule,
// a venue with no verified bracket would be entered and closed at the very next
// settlement, forever. Measured before this check existed: 12 months, 24
// series, margin model on — 46 trades per series and −322% summed, against 1.7
// trades and +27.6% with the model off. Nothing about the strategy had changed;
// the four Binance and four Hyperliquid series were simply churning, because
// Binance publishes its brackets only behind an API key and Hyperliquid's rate
// is derived per coin rather than published.
//
// Refusing at the door costs those series their trades and says why, which is
// the honest outcome: an unverified maintenance rate is not a zero one, and a
// leveraged short whose distance to a forced close nobody can state is not a
// position this project will open.
func checkMarginKnown(c Candidate, p Params) Check {
	if p.PerpMarginFrac <= 0 {
		return Check{Name: "margin_known", Passed: true,
			DetailVI: "Không dùng đòn bẩy trên chân perp (perp_margin_frac = 0) — không cần biểu ký quỹ."}
	}
	// A notional and an entry price the venue would accept; the price is not
	// known at this point, so the bracket is probed with the current perp
	// price, which is what the position would open at.
	state := risk.Evaluate(risk.Position{
		NotionalQuote:   p.NotionalQuote,
		EntryPriceQuote: c.PerpPriceQuote,
		MarginFrac:      p.PerpMarginFrac,
	}, c.PerpMargin, c.PerpPriceQuote)
	if !state.OK {
		return Check{Name: "margin_known", Passed: false, DetailVI: fmt.Sprintf(
			"KHÔNG mở vị thế đòn bẩy %.1fx trên %s: %s", 1/p.PerpMarginFrac, c.PerpSource, state.ReasonVI)}
	}
	return Check{Name: "margin_known", Passed: true, DetailVI: fmt.Sprintf(
		"Ký quỹ %.1f%% notional trên %s: giá thanh lý %.2f, cách %.2f%% (duy trì %.4f%%, bậc tới %.0f).",
		p.PerpMarginFrac*pctPerUnit, c.PerpSource, state.LiquidationPriceQuote, state.BufferPct,
		c.PerpMargin.MaintenanceMarginFrac*pctPerUnit, c.PerpMargin.TierCeilingQuote)}
}

// notEvaluatedVI is what a check reports when a condition it depends on failed.
//
// A dependent check must not manufacture a second, unrelated-looking failure:
// three lines blaming three things when one thing is wrong is how an operator
// fixes the wrong one.
const notEvaluatedVI = "Chưa đánh giá — không có chân hedge nên không có vị thế để định giá."

func checkLiquidity(cost RoundTrip, p Params, hedged bool) Check {
	if !hedged {
		return Check{Name: "liquidity", DetailVI: notEvaluatedVI, NotEvaluated: true}
	}
	if !cost.OK {
		return Check{Name: "liquidity", Passed: false, DetailVI: fmt.Sprintf(
			"Không định giá được vòng vào/ra ở vốn %.0f: %s", p.NotionalQuote, cost.ReasonVI)}
	}
	detail := fmt.Sprintf("Cả 4 lượt khớp nằm trong sổ đo được ở vốn %.0f; slippage %.4f%%, phí %.4f%%.",
		p.NotionalQuote, cost.SlippagePct, cost.FeesPct)
	if cost.DepthIsLowerBound {
		detail += " ⚠ Có lượt ăn quá mức sàn công bố → chi phí là cận TRÊN."
	}
	return Check{Name: "liquidity", Passed: true, DetailVI: detail}
}

func checkNetAPR(net NetAPRResult, p Params, hedged bool) Check {
	if !hedged {
		return Check{Name: "net_apr", DetailVI: notEvaluatedVI, NotEvaluated: true}
	}
	if !net.OK {
		return Check{Name: "net_apr", Passed: false, DetailVI: "Không có APR ròng: " + net.ReasonVI}
	}
	return Check{Name: "net_apr", Passed: net.NetAPRFrac >= p.MinNetAPRFrac, DetailVI: fmt.Sprintf(
		"APR RÒNG %.2f%% so với sàn tối thiểu %.2f%% (thô %.2f%%, đã trừ phí và slippage).",
		net.NetAPRFrac*pctPerUnit, p.MinNetAPRFrac*pctPerUnit, net.GrossAPRFrac*pctPerUnit)}
}

// --- exit conditions ---

// holdFloor is the minimum-hold gate's verdict for one evaluation: whether a
// yield exit is blocked, and the sentence that says so.
//
// Computed ONCE per evaluation in EvaluateExit and handed to both yield
// checks, so the two can never disagree about what this position has
// collected.
type holdFloor struct {
	blocks bool
	// noteVI is appended to a blocked exit's detail. Empty when the floor is
	// off or was cleared, so an unblocked check reads exactly as before.
	noteVI string
}

// minHoldFloor measures what this position has earned back against what
// leaving it costs.
//
// Only settlements the position was open FOR count — strictly after
// OpenedAtMs, which is the same arithmetic internal/backtest's equity curve
// uses, so the gate and the curve can never disagree about the same trade
// (CLAUDE.md rule 6: the payment is collected because the position existed at
// the stamp, not because time passed).
func minHoldFloor(pos Position, usable []exchanges.FundingHistoryEntry, cost RoundTrip, p Params) holdFloor {
	if p.MinHoldRecoveredCostFrac <= 0 {
		return holdFloor{}
	}
	if !cost.OK {
		// No denominator. Step aside rather than pin the position open for a
		// reason unrelated to whether holding is wise.
		return holdFloor{}
	}
	newest, have := newestOf(usable)
	if have && newest.Model == exchanges.FundingContinuous {
		// Samples of a funding index, not payments: there is nothing this
		// position can be said to have COLLECTED. The gate is not evaluable,
		// so it does not block.
		return holdFloor{}
	}

	collectedFrac, settlements := 0.0, 0
	for _, entry := range usable {
		if entry.SettledAtMs <= pos.OpenedAtMs {
			continue
		}
		collectedFrac += entry.RatePerIntervalFrac
		settlements++
	}
	needFrac := p.MinHoldRecoveredCostFrac * cost.TotalPct / pctPerUnit
	if collectedFrac >= needFrac {
		return holdFloor{}
	}
	return holdFloor{blocks: true, noteVI: fmt.Sprintf(
		" GIỮ vì cổng giữ tối thiểu: vị thế mới hoàn %.4f%% notional qua %d mốc, cần %.4f%% "+
			"(= %.2f × chi phí vòng %.4f%%) — rời bây giờ là trả nốt phí ra cho phần chi phí vào chưa hoàn.",
		collectedFrac*pctPerUnit, settlements, needFrac*pctPerUnit,
		p.MinHoldRecoveredCostFrac, cost.TotalPct)}
}

func exitHedgeGone(c Candidate) Check {
	if c.SpotSource != "" {
		return Check{Name: "hedge_gone", Passed: false, DetailVI: fmt.Sprintf("Chân hedge còn nguyên: spot %s.", c.SpotSource)}
	}
	note := c.HedgeNoteVI
	if note == "" {
		note = "không rõ lý do."
	}
	return Check{Name: "hedge_gone", Passed: true, DetailVI: "THOÁT: chân spot không còn ghép được nên vị thế hết delta-neutral — " + note}
}

// exitFundingNegative closes a position once funding has turned against it:
// as step 3.2 wrote it, on the first settled negative print; with the
// ExitNegative* gates set, only once the negative regime is deep, long or
// expensive enough (all configured gates, AND) that leaving beats paying it.
//
// The run it measures is the CURRENT one — consecutive negative settlements
// ending at the newest — because the question is "how much is this episode
// costing", not "how much has funding ever cost".
func exitFundingNegative(usable []exchanges.FundingHistoryEntry, cost RoundTrip, p Params, floor holdFloor) Check {
	newest, have := newestOf(usable)
	if !have {
		return Check{Name: "funding_negative", Passed: false, DetailVI: "Chưa có mốc settle mới để xét dấu."}
	}
	bps := newest.RatePer8hFrac * bpsPerUnit
	if bps >= 0 {
		return Check{Name: "funding_negative", Passed: false, DetailVI: fmt.Sprintf("Funding còn dương: %.4f bps/8h.", bps)}
	}

	run, paidFrac := 0, 0.0
	for i := len(usable) - 1; i >= 0 && usable[i].RatePer8hFrac < 0; i-- {
		run++
		paidFrac += -usable[i].RatePerIntervalFrac
	}
	needPeriods := p.EffectiveExitNegativePeriods()
	if newest.Model == exchanges.FundingContinuous && (needPeriods > 1 || p.ExitNegativeCumCostFrac > 0) {
		// Samples of a funding index, not settlements: nothing to count and
		// nothing paid per row. The gates cannot be evaluated, so the 3.2 rule
		// applies — and says so.
		return Check{Name: "funding_negative", Passed: true, DetailVI: fmt.Sprintf(
			"THOÁT: funding đã đảo dấu, mốc mới nhất %.4f bps/8h — vị thế đang TRẢ chứ không thu. "+
				"Chuỗi %s là MẪU liên tục, không có mốc settle để đếm đợt hay cộng chi phí: cổng N/C không xét được, áp dụng luật 3.2.",
			bps, newest.Source)}
		// The continuous model reaches here only when the floor could not be
		// evaluated either, so there is nothing to consult.
	}
	deepEnough := bps <= -p.ExitNegativeMinBps
	longEnough := run >= needPeriods
	expensiveEnough, costGate := true, ""
	if p.ExitNegativeCumCostFrac > 0 {
		if !cost.OK {
			costGate = " Không định giá được vòng vào/ra nên cổng chi phí coi như đạt — thoát cho an toàn."
		} else {
			needFrac := p.ExitNegativeCumCostFrac * cost.TotalPct / 100
			expensiveEnough = paidFrac >= needFrac
			costGate = fmt.Sprintf(" Đợt âm đã trả %.4f%% notional so với cổng %.4f%% (= %.2f × vòng %.4f%%).",
				paidFrac*100, needFrac*100, p.ExitNegativeCumCostFrac, cost.TotalPct)
		}
	}
	gates := fmt.Sprintf("Mốc mới nhất %.4f bps/8h (cổng ≤ −%.4f), %d mốc âm liên tiếp (cổng ≥ %d).%s",
		bps, p.ExitNegativeMinBps, run, needPeriods, costGate)
	if deepEnough && longEnough && expensiveEnough {
		if floor.blocks {
			return Check{Name: "funding_negative", Passed: false, DetailVI: fmt.Sprintf(
				"Funding đã đảo dấu và qua hết cổng thoát (mốc mới nhất %.4f bps/8h). %s%s",
				bps, gates, floor.noteVI)}
		}
		return Check{Name: "funding_negative", Passed: true, DetailVI: fmt.Sprintf(
			"THOÁT: funding đã đảo dấu, mốc mới nhất %.4f bps/8h — vị thế đang TRẢ chứ không thu. %s", bps, gates)}
	}
	return Check{Name: "funding_negative", Passed: false, DetailVI: "Funding âm nhưng chưa qua cổng thoát: " + gates}
}

// exitNetAPRFloor closes a position whose net APR has stayed under the floor
// for ExitPersistencePeriods consecutive settlements.
//
// Consecutive, not the newest alone — see ExitPersistencePeriods for the
// measurement that forced it. Each period in the window is annualized on its
// OWN rate through the same NetAPR the entry uses, so decay is judged by the
// same arithmetic that admitted the position.
func exitNetAPRFloor(pos Position, usable []exchanges.FundingHistoryEntry, cost RoundTrip,
	net NetAPRResult, p Params, hedgeAlreadyGone bool, floor holdFloor) Check {

	if hedgeAlreadyGone {
		// The hedge check has already fired and closes the position. Firing
		// again here would report two independent causes for one event.
		return Check{Name: "net_apr_floor", DetailVI: notEvaluatedVI, NotEvaluated: true}
	}
	if !net.OK {
		return Check{Name: "net_apr_floor", Passed: true, DetailVI: "THOÁT: không còn tính được APR ròng — " + net.ReasonVI}
	}

	window := p.ExitPersistencePeriods
	if window < 1 {
		window = 1
	}

	// The window is the last `window` NON-negative settlements (see the
	// JURISDICTION note on Params): a negative print is the sign-flip exit's
	// to judge, through its gates. Under the 3.2 gates the newest negative
	// print fires that exit first, and a pre-entry negative never completes
	// an all-under window because the entry print itself cleared
	// MinNetAPRFrac — so this changes nothing for the live 3.3 set, and the
	// 24-set grid replays identically (pinned in cmd/backtest).
	recent := make([]exchanges.FundingHistoryEntry, 0, window)
	skippedNegative := 0
	for i := len(usable) - 1; i >= 0 && len(recent) < window; i-- {
		if usable[i].RatePer8hFrac < 0 {
			skippedNegative++
			continue
		}
		recent = append(recent, usable[i])
	}
	negNote := ""
	if skippedNegative > 0 {
		negNote = fmt.Sprintf(" (bỏ qua %d mốc âm — thuộc lối thoát đảo dấu và cổng của nó)", skippedNegative)
	}
	// priced counts the settlements this window could actually value, and it is
	// what `under` is compared against — NOT len(recent).
	//
	// usableSettled has already dropped the unusable shapes, so a period that
	// still cannot be priced here is rare: it takes a rate that is finite but
	// large enough to overflow once annualized. Rare is not never, and counting
	// it in the denominator while it can never reach the numerator made the
	// decay exit UNABLE TO FIRE — one unpriceable row in the window and the
	// position rides out a dead funding regime paying commission it never earns
	// back. The min/max are seeded off the same counter for the same reason: an
	// unpriceable FIRST period used to leave the reported floor at 0.00%, a rate
	// no venue published.
	priced, under := 0, 0
	var worstFrac, bestFrac float64
	for _, entry := range recent {
		periodNet := NetAPR(NetAPRInput{
			Source: pos.PerpSource, Symbol: pos.Symbol,
			Model:               entry.Model,
			RatePerIntervalFrac: entry.RatePerIntervalFrac,
			IntervalSec:         entry.IntervalSec,
			HoldingDays:         p.HoldingDays,
			Cost:                cost,
		})
		if !periodNet.OK {
			continue
		}
		if priced == 0 || periodNet.NetAPRFrac < worstFrac {
			worstFrac = periodNet.NetAPRFrac
		}
		if priced == 0 || periodNet.NetAPRFrac > bestFrac {
			bestFrac = periodNet.NetAPRFrac
		}
		priced++
		if periodNet.NetAPRFrac < p.ExitNetAPRFrac {
			under++
		}
	}

	if priced == 0 {
		// No evidence either way. The newest-reading check above already exits
		// when the CURRENT figure cannot be computed, so staying silent here
		// avoids reporting a second cause for that same one.
		return Check{Name: "net_apr_floor", Passed: false, DetailVI: fmt.Sprintf(
			"Không định giá được mốc nào trong %d mốc không âm gần nhất — không kết luận suy giảm từ cửa sổ này.%s",
			len(recent), negNote)}
	}
	if under == priced {
		skipped := negNote
		if priced < len(recent) {
			skipped = fmt.Sprintf(" (%d/%d mốc không định giá được, đã loại khỏi phép đếm)",
				len(recent)-priced, len(recent))
		}
		if floor.blocks {
			return Check{Name: "net_apr_floor", Passed: false, DetailVI: fmt.Sprintf(
				"Cả %d mốc settle định giá được gần nhất đều cho APR ròng dưới ngưỡng giữ %.2f%% "+
					"(thấp nhất %.2f%%, cao nhất %.2f%%)%s.%s",
				priced, p.ExitNetAPRFrac*pctPerUnit, worstFrac*pctPerUnit, bestFrac*pctPerUnit,
				skipped, floor.noteVI)}
		}
		return Check{Name: "net_apr_floor", Passed: true, DetailVI: fmt.Sprintf(
			"THOÁT: cả %d mốc settle định giá được gần nhất đều cho APR ròng dưới ngưỡng giữ %.2f%% "+
				"(thấp nhất %.2f%%, cao nhất %.2f%%)%s — chế độ funding đã tàn, không phải một mốc lỗi nhịp.",
			priced, p.ExitNetAPRFrac*pctPerUnit, worstFrac*pctPerUnit, bestFrac*pctPerUnit, skipped)}
	}
	return Check{Name: "net_apr_floor", Passed: false, DetailVI: fmt.Sprintf(
		"%d/%d mốc định giá được gần nhất dưới ngưỡng giữ %.2f%% (mới nhất %.2f%%) — chưa đủ bền để đóng vị thế.",
		under, priced, p.ExitNetAPRFrac*pctPerUnit, net.NetAPRFrac*pctPerUnit)}
}

// exitBasisWidened watches the perp-over-spot difference on two axes: how wide
// it is, and how far it has moved since the position was opened.
//
// Both matter, and neither implies the other. A wide basis is capital tied up
// in a convergence that has not happened; a basis that has MOVED against the
// entry is an unrealized loss on the pair that funding has to earn back before
// the position is worth anything.
func exitBasisWidened(pos Position, c Candidate, p Params) Check {
	if !isPositiveFinite(c.SpotPriceQuote) || !isPositiveFinite(c.PerpPriceQuote) {
		return Check{Name: "basis_widened", DetailVI: "Chưa đo được basis: thiếu giá một trong hai chân.", NotEvaluated: true}
	}
	basisPct := (c.PerpPriceQuote - c.SpotPriceQuote) / c.SpotPriceQuote * pctPerUnit
	movedPct := basisPct - pos.EntryBasisPct

	if abs(basisPct) > p.MaxBasisPct {
		return Check{Name: "basis_widened", Passed: true, DetailVI: fmt.Sprintf(
			"THOÁT: basis %.4f%% vượt trần %.4f%%.", basisPct, p.MaxBasisPct)}
	}
	if abs(movedPct) > p.MaxBasisWidenPct {
		return Check{Name: "basis_widened", Passed: true, DetailVI: fmt.Sprintf(
			"THOÁT: basis đã dịch %.4f điểm %% so với lúc vào (%.4f%% → %.4f%%), quá hạn %.4f.",
			movedPct, pos.EntryBasisPct, basisPct, p.MaxBasisWidenPct)}
	}
	return Check{Name: "basis_widened", Passed: false, DetailVI: fmt.Sprintf(
		"Basis %.4f%% (vào lệnh %.4f%%), trong cả trần %.4f%% lẫn biên dịch %.4f.",
		basisPct, pos.EntryBasisPct, p.MaxBasisPct, p.MaxBasisWidenPct)}
}

// exitMarginThin closes a position whose SHORT PERP leg is close to being
// force-closed by the venue.
//
// A delta-neutral position is flat in the coin and its perp leg is not: the
// spot gain sits in a different account, usually at a different venue, and
// margin is not fungible between them. A rally can therefore liquidate the
// perp while the combined position is exactly where it started — the one way
// this strategy loses far more than the funding it was collecting.
//
// It is deliberately expressed as a distance to the LIQUIDATION PRICE and not
// as a leverage limit: the liquidation price is what the venue acts on, it
// moves with the maintenance bracket the position's SIZE falls into, and the
// leverage a user picked is only one of its inputs (internal/risk doc.go).
func exitMarginThin(pos Position, c Candidate, p Params) Check {
	if p.PerpMarginFrac <= 0 {
		return Check{Name: "margin_thin", NotEvaluated: true,
			DetailVI: "Chưa bật mô hình ký quỹ (perp_margin_frac = 0) — không xét khoảng cách thanh lý."}
	}
	if !isPositiveFinite(pos.PerpEntryPriceQuote) || !isPositiveFinite(c.PerpPriceQuote) {
		return Check{Name: "margin_thin", NotEvaluated: true,
			DetailVI: "Chưa đo được khoảng cách thanh lý: thiếu giá perp lúc vào hoặc lúc này."}
	}
	state := risk.Evaluate(risk.Position{
		NotionalQuote:   pos.NotionalQuote,
		EntryPriceQuote: pos.PerpEntryPriceQuote,
		MarginFrac:      p.PerpMarginFrac,
	}, c.PerpMargin, c.PerpPriceQuote)
	if !state.OK {
		// Not knowing how far liquidation is, is itself a reason to leave: the
		// alternative is holding a leveraged short whose distance to a forced
		// close nobody can state. Same direction as the net-APR check, which
		// exits when the position can no longer be valued.
		return Check{Name: "margin_thin", Passed: true,
			DetailVI: "THOÁT: không suy ra được giá thanh lý của chân perp — " + state.ReasonVI}
	}
	if state.Liquidated {
		return Check{Name: "margin_thin", Passed: true, DetailVI: fmt.Sprintf(
			"THOÁT: chân perp ĐÃ chạm giá thanh lý %.2f (giá hiện tại %.2f, ký quỹ %.1f%% notional, "+
				"duy trì %.4f%%). Đây là mất vốn, không phải một lần thoát bình thường.",
			state.LiquidationPriceQuote, c.PerpPriceQuote,
			p.PerpMarginFrac*pctPerUnit, c.PerpMargin.MaintenanceMarginFrac*pctPerUnit)}
	}
	if state.BufferPct <= p.MinLiquidationBufferPct {
		return Check{Name: "margin_thin", Passed: true, DetailVI: fmt.Sprintf(
			"THOÁT: chân perp chỉ còn %.2f%% tới giá thanh lý %.2f, dưới biên an toàn %.2f%%.",
			state.BufferPct, state.LiquidationPriceQuote, p.MinLiquidationBufferPct)}
	}
	return Check{Name: "margin_thin", Passed: false, DetailVI: fmt.Sprintf(
		"Chân perp còn %.2f%% tới giá thanh lý %.2f (biên tối thiểu %.2f%%); vốn %.2f so với mức duy trì %.2f.",
		state.BufferPct, state.LiquidationPriceQuote, p.MinLiquidationBufferPct,
		state.EquityQuote, state.MaintenanceQuote)}
}

// --- shared ---

// usableSettled drops what must never carry a decision, and counts what it
// dropped so the log can say so.
//
// Binance labels dividend-driven rates "Special" and PLAN.md 3.3 requires them
// filtered; doing it here rather than in the backtest means the live path
// cannot forget to. A row with a non-positive interval is refused for the same
// reason exchanges.DeriveFundingRates refuses one: everything downstream
// divides by it.
// UsableSettled drops the settlements no rule may be built on, and reports how
// many were dropped for being dividend-driven.
//
// Exported because internal/backtest must apply the SAME filter the signal
// layer applies. What counts as a usable settlement is a strategy rule, not a
// reader's convenience: a backtest that accrued a Binance "Special" rate the
// signal never saw would diverge from production for a reason step 3.5 could
// not diagnose (doc.go, PLAN Q8).
// The returned slice is READ-ONLY: when nothing needs dropping it IS the
// caller's slice, not a copy. That fast path is not a micro-optimization —
// internal/backtest calls this once per settlement on a growing prefix of the
// history, so copying made one replay O(n²) in the number of settlements.
// Venues that settle hourly publish ~8,760 rows a year (Hyperliquid, Kraken),
// where the copies came to gigabytes per replay and a parameter sweep over
// them became hours. Every caller only reads the result; a caller that ever
// needs to mutate it must copy first.
func UsableSettled(entries []exchanges.FundingHistoryEntry) (usable []exchanges.FundingHistoryEntry, droppedSpecial int) {
	drop := func(e exchanges.FundingHistoryEntry) bool {
		return e.RateType == "Special" || e.IntervalSec <= 0 ||
			!isFinite(e.RatePer8hFrac) || !isFinite(e.RatePerIntervalFrac)
	}
	if len(entries) == 0 {
		// nil, not an empty slice: identical to what the copying version
		// returned, so no caller can tell the two implementations apart.
		return nil, 0
	}
	first := -1
	for i := range entries {
		if drop(entries[i]) {
			first = i
			break
		}
	}
	if first < 0 {
		return entries, 0
	}
	usable = make([]exchanges.FundingHistoryEntry, first, len(entries))
	copy(usable, entries[:first])
	for _, entry := range entries[first:] {
		if entry.RateType == "Special" {
			droppedSpecial++
			continue
		}
		if entry.IntervalSec <= 0 || !isFinite(entry.RatePer8hFrac) || !isFinite(entry.RatePerIntervalFrac) {
			continue
		}
		usable = append(usable, entry)
	}
	if len(usable) == 0 {
		return nil, droppedSpecial
	}
	return usable, droppedSpecial
}

// newestOf returns the last entry, which is the newest: every producer of this
// slice — store.FundingHistory and the venue fetchers — orders oldest first.
func newestOf(entries []exchanges.FundingHistoryEntry) (exchanges.FundingHistoryEntry, bool) {
	if len(entries) == 0 {
		return exchanges.FundingHistoryEntry{}, false
	}
	return entries[len(entries)-1], true
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
