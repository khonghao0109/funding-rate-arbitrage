package strategy

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/fees"
	"futures-arbitrage-scanner/internal/risk"
)

var evalAt = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

// settled builds a series of already-paid rates at a fixed cadence, oldest
// first, each quoted per its own interval.
func settled(source string, intervalSec int64, ratesPer8hBps ...float64) []exchanges.FundingHistoryEntry {
	out := make([]exchanges.FundingHistoryEntry, 0, len(ratesPer8hBps))
	for i, bps := range ratesPer8hBps {
		per8h := bps / bpsPerUnit
		entry := exchanges.FundingHistoryEntry{
			Symbol: "BTCUSDT", Source: source, Model: exchanges.FundingDiscrete,
			SettledAtMs:         evalAt.Add(-time.Duration(len(ratesPer8hBps)-i) * time.Duration(intervalSec) * time.Second).UnixMilli(),
			RatePerIntervalFrac: per8h * float64(intervalSec) / 28800,
			IntervalSec:         intervalSec,
			RatePer8hFrac:       per8h,
			GapPrevSec:          intervalSec,
		}
		out = append(out, entry)
	}
	return out
}

func goodCandidate() Candidate {
	return Candidate{
		Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot",
		Settled:        settled("binance_futures", 28800, 1.0, 1.1, 1.2, 1.0, 1.1, 1.3),
		SpotFee:        verifiedFee("binance_spot", 10),
		PerpFee:        verifiedFee("binance_futures", 5),
		SpotBook:       storedBinanceSpotBTC(),
		PerpBook:       storedBinanceFuturesBTC(),
		SpotPriceQuote: 81050.605, PerpPriceQuote: 81043.75,
	}
}

func entryParams() Params {
	return Params{
		MinRatePer8hBps:    0.8,
		PersistencePeriods: 4,
		MinNetAPRFrac:      0.05,
		NotionalQuote:      50_000,
		HoldingDays:        30,
		ExitNetAPRFrac:     0.02,
		MaxBasisPct:        1.0,
		MaxBasisWidenPct:   0.5,
	}
}

func TestEvaluateEntry_EntersWhenEveryConditionHolds(t *testing.T) {
	got := EvaluateEntry(evalAt, goodCandidate(), entryParams())
	if got.Action != ActionEnter {
		t.Fatalf("action = %s, want enter. Reasons:\n%s", got.Action, strings.Join(got.LogLines(), "\n"))
	}
	if !got.NetAPR.OK {
		t.Fatal("an entry signal must carry the net APR it was decided on")
	}
	// PLAN 3.2 acceptance: the signal logs its FULL reasoning, not just the
	// verdict — every condition, whether it passed or failed.
	if len(got.Checks) < 5 {
		t.Errorf("got %d checks; entry has five conditions and all must be reported", len(got.Checks))
	}
	for _, check := range got.Checks {
		if check.DetailVI == "" {
			t.Errorf("check %q carries no detail — a signal without a logged reason cannot be debugged", check.Name)
		}
	}
}

// A perp with no spot leg cannot be opened at all. Three of the seven venues
// quote USD against USDT-only spot markets and are refused BY DESIGN (step
// 2.4); a signal for a pair that cannot be opened is a defect, not a lead.
func TestEvaluateEntry_RefusesAPerpWithNoHedgeLeg(t *testing.T) {
	c := goodCandidate()
	c.PerpSource = "kraken_futures"
	c.SpotSource = "" // USD-quoted perp, no USDT spot it may pair with
	c.HedgeNoteVI = "Perp quote USD không ghép được với spot USDT."

	got := EvaluateEntry(evalAt, c, entryParams())
	if got.Action == ActionEnter {
		t.Fatal("a perp with no hedge leg must never produce an entry signal")
	}
	if !hasFailedCheck(got, "hedge_leg") {
		t.Errorf("the missing hedge must be the named reason, got:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	// The venue's own explanation has to survive into the log.
	if !strings.Contains(strings.Join(got.LogLines(), " "), "USD") {
		t.Error("the hedge note from the mapping must be carried through")
	}
}

func TestEvaluateEntry_RequiresTheRateToClearTheThreshold(t *testing.T) {
	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 0.1, 0.2, 0.1, 0.3, 0.2, 0.1)

	got := EvaluateEntry(evalAt, c, entryParams())
	if got.Action == ActionEnter {
		t.Fatal("a rate below the threshold must not enter")
	}
	if !hasFailedCheck(got, "rate_threshold") {
		t.Errorf("expected rate_threshold to fail:\n%s", strings.Join(got.LogLines(), "\n"))
	}
}

func TestEvaluateEntry_RequiresPersistenceNotOneGoodPeriod(t *testing.T) {
	// One spike on top of a flat, uninteresting series.
	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 0.1, 0.1, 0.1, 0.1, 0.1, 5.0)

	got := EvaluateEntry(evalAt, c, entryParams())
	if got.Action == ActionEnter {
		t.Fatal("a single spike is not a durable funding regime")
	}
	if !hasFailedCheck(got, "persistence") {
		t.Errorf("expected persistence to fail:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	// The newest rate DID clear the threshold, so that check must have passed —
	// otherwise the log blames the wrong condition.
	if hasFailedCheck(got, "rate_threshold") {
		t.Error("the newest rate cleared the threshold; only persistence should fail")
	}
}

func TestEvaluateEntry_RefusesAHistoryTooShortToJudge(t *testing.T) {
	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 1.2, 1.3)

	got := EvaluateEntry(evalAt, c, entryParams())
	if got.Action == ActionEnter {
		t.Fatal("two settlements cannot demonstrate persistence over four")
	}
	if !hasFailedCheck(got, "history_depth") {
		t.Errorf("expected history_depth to fail:\n%s", strings.Join(got.LogLines(), "\n"))
	}
}

func TestEvaluateEntry_RefusesWhenTheBookCannotFillTheSize(t *testing.T) {
	c := goodCandidate()
	c.PerpSource = "paradex_futures"
	c.PerpBook = storedParadexBTC()
	c.PerpFee = verifiedFee("paradex_futures", 4.5)
	c.Settled = settled("paradex_futures", 28800, 1.0, 1.1, 1.2, 1.0, 1.1, 1.3)

	p := entryParams()
	p.NotionalQuote = 60_000 // its book holds 21,543 inside 0.5%

	got := EvaluateEntry(evalAt, c, p)
	if got.Action == ActionEnter {
		t.Fatal("a size the book cannot absorb must not produce an entry signal")
	}
	if !hasFailedCheck(got, "liquidity") {
		t.Errorf("expected liquidity to fail:\n%s", strings.Join(got.LogLines(), "\n"))
	}
}

func TestEvaluateEntry_RefusesWhenNetAPRIsBelowTheFloor(t *testing.T) {
	c := goodCandidate()
	p := entryParams()
	p.MinNetAPRFrac = 0.50 // 50% APR — nothing on a major pair reaches it

	got := EvaluateEntry(evalAt, c, p)
	if got.Action == ActionEnter {
		t.Fatal("a net APR below the floor must not enter")
	}
	if !hasFailedCheck(got, "net_apr") {
		t.Errorf("expected net_apr to fail:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	// Gross would have cleared the floor comfortably; the point of the step is
	// that the decision is made on the net figure.
	if got.NetAPR.GrossAPRFrac <= got.NetAPR.NetAPRFrac {
		t.Error("gross must exceed net once a cost is charged")
	}
}

// Every check runs even after one fails, or the log names the first problem
// instead of all of them and an operator fixes one thing at a time.
func TestEvaluateEntry_ReportsEveryFailureNotJustTheFirst(t *testing.T) {
	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1)
	p := entryParams()
	p.MinNetAPRFrac = 0.50

	got := EvaluateEntry(evalAt, c, p)
	failed := 0
	for _, check := range got.Checks {
		if !check.Passed {
			failed++
		}
	}
	if failed < 3 {
		t.Errorf("only %d checks failed; rate, persistence and net APR are all wrong here:\n%s",
			failed, strings.Join(got.LogLines(), "\n"))
	}
}

// Binance labels dividend-driven rates "Special". They are not the funding
// regime and must not be able to carry a persistence check.
func TestEvaluateEntry_IgnoresSpecialRates(t *testing.T) {
	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 0.1, 0.1, 0.1, 0.1)
	for i := range c.Settled {
		c.Settled[i].RateType = "Regular"
	}
	special := settled("binance_futures", 28800, 9.0, 9.0, 9.0, 9.0)
	for i := range special {
		special[i].RateType = "Special"
	}
	c.Settled = append(c.Settled, special...)

	got := EvaluateEntry(evalAt, c, entryParams())
	if got.Action == ActionEnter {
		t.Fatalf("four Special rates must not manufacture a funding regime:\n%s",
			strings.Join(got.LogLines(), "\n"))
	}
}

func TestEvaluateExit_ClosesOnFundingTurningNegative(t *testing.T) {
	pos := openPosition()
	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 1.0, 0.5, -0.2, -0.8)

	got := EvaluateExit(evalAt, pos, c, entryParams())
	if got.Action != ActionExit {
		t.Fatalf("a position paying funding instead of collecting it must close:\n%s",
			strings.Join(got.LogLines(), "\n"))
	}
	if !hasTriggeredCheck(got, "funding_negative") {
		t.Errorf("the sign flip must be the named reason:\n%s", strings.Join(got.LogLines(), "\n"))
	}
}

func TestEvaluateExit_ClosesWhenNetAPRFallsThroughTheFloor(t *testing.T) {
	pos := openPosition()
	c := goodCandidate()
	// Still positive, but far too small to cover the round trip.
	c.Settled = settled("binance_futures", 28800, 0.05, 0.04, 0.03, 0.02)

	got := EvaluateExit(evalAt, pos, c, entryParams())
	if got.Action != ActionExit {
		t.Fatalf("net APR under the exit floor must close:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	if !hasTriggeredCheck(got, "net_apr_floor") {
		t.Errorf("expected net_apr_floor:\n%s", strings.Join(got.LogLines(), "\n"))
	}
}

func TestEvaluateExit_ClosesOnAnAbnormallyWideBasis(t *testing.T) {
	pos := openPosition()
	c := goodCandidate()
	c.PerpPriceQuote = c.SpotPriceQuote * 1.02 // perp 2% over spot

	got := EvaluateExit(evalAt, pos, c, entryParams())
	if got.Action != ActionExit {
		t.Fatalf("a 2%% basis against a 1%% limit must close:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	if !hasTriggeredCheck(got, "basis_widened") {
		t.Errorf("expected basis_widened:\n%s", strings.Join(got.LogLines(), "\n"))
	}
}

func TestEvaluateExit_HoldsWhileTheThesisStands(t *testing.T) {
	got := EvaluateExit(evalAt, openPosition(), goodCandidate(), entryParams())
	if got.Action != ActionHold {
		t.Fatalf("nothing has gone wrong; the position holds:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	// A hold is a decision too and must be explainable.
	if len(got.LogLines()) == 0 {
		t.Error("a hold must log why it held")
	}
}

func TestEvaluateExit_ClosesWhenTheHedgeLegDisappears(t *testing.T) {
	pos := openPosition()
	c := goodCandidate()
	c.SpotSource = ""
	c.HedgeNoteVI = "Spot market bị huỷ niêm yết."

	got := EvaluateExit(evalAt, pos, c, entryParams())
	if got.Action != ActionExit {
		t.Fatalf("a position whose hedge leg vanished cannot be held delta-neutral:\n%s",
			strings.Join(got.LogLines(), "\n"))
	}
}

func openPosition() Position {
	return Position{
		Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot",
		OpenedAtMs:    evalAt.Add(-10 * 24 * time.Hour).UnixMilli(),
		NotionalQuote: 50_000,
		EntryBasisPct: 0.01,
	}
}

func hasFailedCheck(d Decision, name string) bool {
	for _, check := range d.Checks {
		if check.Name == name && !check.Passed {
			return true
		}
	}
	return false
}

// An exit check "passes" when its condition is TRUE — the thing it watches for
// has happened — which is the opposite polarity from an entry check.
func hasTriggeredCheck(d Decision, name string) bool {
	for _, check := range d.Checks {
		if check.Name == name && check.Passed {
			return true
		}
	}
	return false
}

// Three failures describing one cause is how an operator fixes the wrong one.
//
// Found on real data during the step-3.2 acceptance run: every USD-quoted perp
// logged "biểu phí của (nguồn không tên) chưa xác minh" from the liquidity and
// net-APR checks, because with no hedge leg they were pricing against a zero
// spot book and a zero fee schedule. Their actual and only problem was having
// no USDT spot market to hedge against.
func TestEvaluateEntry_DependentChecksDoNotInventASecondCause(t *testing.T) {
	c := goodCandidate()
	c.PerpSource = "hyperliquid_futures"
	c.SpotSource = ""
	c.SpotFee = fees.Schedule{} // there is no spot venue, so there is no schedule
	c.SpotBook = depth.Summary{}
	c.HedgeNoteVI = "no spot market shares quote USD for BTCUSDT (spot quotes: USDT)"

	got := EvaluateEntry(evalAt, c, entryParams())
	if got.Action == ActionEnter {
		t.Fatal("no hedge leg must not enter")
	}

	joined := strings.Join(got.LogLines(), "\n")
	if strings.Contains(joined, "nguồn không tên") {
		t.Errorf("a missing hedge leg must not be reported as an unnamed venue's fee problem:\n%s", joined)
	}
	if strings.Contains(joined, "biểu phí") {
		t.Errorf("the fee schedule is not the problem here; only the hedge leg is:\n%s", joined)
	}
	for _, name := range []string{"liquidity", "net_apr"} {
		var found bool
		for _, check := range got.Checks {
			if check.Name == name {
				found = true
				if !strings.Contains(check.DetailVI, "Chưa đánh giá") {
					t.Errorf("%s must report that it could not be evaluated, got: %s", name, check.DetailVI)
				}
			}
		}
		if !found {
			t.Errorf("%s must still be reported, as not-evaluated rather than absent", name)
		}
	}
}

// A vanished hedge closes the position on its own; the net-APR floor must not
// also fire and claim a second independent cause for one event.
func TestEvaluateExit_HedgeGoneIsReportedAsOneCauseNotTwo(t *testing.T) {
	c := goodCandidate()
	c.SpotSource = ""
	c.SpotFee = fees.Schedule{}
	c.SpotBook = depth.Summary{}
	c.HedgeNoteVI = "Spot market bị huỷ niêm yết."

	got := EvaluateExit(evalAt, openPosition(), c, entryParams())
	if got.Action != ActionExit {
		t.Fatal("a vanished hedge closes the position")
	}
	triggered := 0
	for _, check := range got.Checks {
		if check.Passed {
			triggered++
		}
	}
	if triggered != 1 {
		t.Errorf("%d conditions fired for one event:\n%s", triggered, strings.Join(got.LogLines(), "\n"))
	}
	if !hasTriggeredCheck(got, "hedge_gone") {
		t.Error("the one that fires must be hedge_gone")
	}
}

// The decay window's min/max are seeded on the FIRST entry by index, but the
// loop skips any settlement whose net APR could not be computed. When the
// skipped one is the first, the seed never happens and the reported floor stays
// at the zero value — a decay message quoting a rate nobody published.
//
// Reachable, not theoretical: usableSettled admits any FINITE rate, and a rate
// large enough to overflow once annualized makes NetAPR refuse that period
// while its neighbours price normally.
func TestEvaluateExit_DecayWindowReportsRealRatesWhenOneCannotBePriced(t *testing.T) {
	c := goodCandidate()
	// Three settlements, all below the hold floor so the decay exit fires, with
	// the OLDEST unpriceable.
	c.Settled = settled("binance_futures", 28800, 0.05, 0.06, 0.07)
	c.Settled[0].RatePerIntervalFrac = math.MaxFloat64
	c.Settled[0].RatePer8hFrac = math.MaxFloat64

	p := entryParams()
	p.ExitPersistencePeriods = 3

	got := EvaluateExit(evalAt, openPosition(), c, p)
	if !hasTriggeredCheck(got, "net_apr_floor") {
		t.Fatalf("three settlements under the hold floor must close the position:\n%s",
			strings.Join(got.LogLines(), "\n"))
	}

	var detail string
	for _, check := range got.Checks {
		if check.Name == "net_apr_floor" {
			detail = check.DetailVI
		}
	}
	// The seeding bug shows up as a floor of exactly 0.00%: no settlement here
	// annualizes to zero, so quoting it means the seed was never taken.
	if strings.Contains(detail, "thấp nhất 0.00%") {
		t.Errorf("decay message quotes a floor of 0.00%%, which no settlement produced — "+
			"the min/max seed was skipped along with the unpriceable period:\n  %s", detail)
	}
}

// A check that could not be judged says so with a FLAG, not with the wording
// of its sentence. internal/backtest counts these; matching a Vietnamese prefix
// would make that count vanish the day the sentence is reworded.
func TestChecks_FlagNotEvaluatedInsteadOfEncodingItInProse(t *testing.T) {
	// Exit: no prices at all → basis cannot be judged.
	c := goodCandidate()
	c.SpotPriceQuote, c.PerpPriceQuote = 0, 0
	exit := EvaluateExit(evalAt, openPosition(), c, entryParams())
	var basis Check
	for _, ch := range exit.Checks {
		if ch.Name == "basis_widened" {
			basis = ch
		}
	}
	if !basis.NotEvaluated || basis.Passed {
		t.Errorf("basis with no prices must be NotEvaluated and not fired: %+v", basis)
	}

	// Entry: no hedge leg → liquidity and net_apr are not evaluated, not failed-for-a-second-reason.
	c = goodCandidate()
	c.SpotSource, c.HedgeNoteVI = "", "no spot shares quote USD"
	entry := EvaluateEntry(evalAt, c, entryParams())
	for _, ch := range entry.Checks {
		if (ch.Name == "liquidity" || ch.Name == "net_apr") && !ch.NotEvaluated {
			t.Errorf("%s must be flagged NotEvaluated when there is no hedge leg: %+v", ch.Name, ch)
		}
		if ch.Name == "hedge_leg" && ch.NotEvaluated {
			t.Error("the hedge check itself WAS evaluated (and failed); it must not carry the flag")
		}
	}
}

// The zero-valued gates ARE the step-3.2 rule: any settled negative print
// closes the position, however small. The 3.5 gate runs on this and must not
// move when the gates are added.
func TestEvaluateExit_ZeroGatesReproduceTheFirstNegativePrintRule(t *testing.T) {
	pos := openPosition()
	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 1.0, 0.5, -0.0001)
	p := entryParams()
	p.ExitPersistencePeriods = 50 // keep the decay exit out of the way
	got := EvaluateExit(evalAt, pos, c, p)
	if got.Action != ActionExit || !hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("a −0.0001 bps print must close the position under zero gates:\n%s", strings.Join(got.LogLines(), "\n"))
	}
}

func TestEvaluateExit_MinBpsGateIgnoresAShallowNegativePrint(t *testing.T) {
	pos := openPosition()
	p := entryParams()
	p.ExitPersistencePeriods = 50
	p.ExitNegativeMinBps = 0.5

	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 1.0, 0.5, -0.3)
	if got := EvaluateExit(evalAt, pos, c, p); got.Action != ActionHold || hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("−0.3 bps is above the −0.5 gate and must be held through:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	c.Settled = settled("binance_futures", 28800, 1.0, 0.5, -0.6)
	if got := EvaluateExit(evalAt, pos, c, p); got.Action != ActionExit || !hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("−0.6 bps clears the −0.5 gate and must close:\n%s", strings.Join(got.LogLines(), "\n"))
	}
}

func TestEvaluateExit_PeriodsGateNeedsConsecutiveNegatives(t *testing.T) {
	pos := openPosition()
	p := entryParams()
	p.ExitPersistencePeriods = 50
	p.ExitNegativePeriods = 3

	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 1.0, -0.3, -0.3)
	if got := EvaluateExit(evalAt, pos, c, p); hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("two negatives are not three:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	c.Settled = settled("binance_futures", 28800, 1.0, -0.3, -0.3, -0.3)
	if got := EvaluateExit(evalAt, pos, c, p); !hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("three consecutive negatives must close:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	// A positive print inside the window resets the run: the rule counts the
	// CURRENT episode, not any three negatives in the last N.
	c.Settled = settled("binance_futures", 28800, -0.3, -0.3, 0.2, -0.3)
	if got := EvaluateExit(evalAt, pos, c, p); hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("a run broken by a positive print is a run of one:\n%s", strings.Join(got.LogLines(), "\n"))
	}
}

func TestEvaluateExit_CumulativeGateComparesWhatWasPaidToTheRoundTrip(t *testing.T) {
	pos := openPosition()
	p := entryParams()
	p.ExitPersistencePeriods = 100
	p.ExitNegativeCumCostFrac = 0.5

	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 1.0, -1.0)
	probe := EvaluateExit(evalAt, pos, c, p)
	if !probe.Cost.OK {
		t.Fatalf("fixture must price the round trip: %s", probe.Cost.ReasonVI)
	}
	// −1.0 bps/8h at an 8h cadence pays 0.0001 of notional per settlement.
	need := int(math.Ceil(0.5 * probe.Cost.TotalPct / 100 / 0.0001))
	if need < 2 || need > 60 {
		t.Fatalf("fixture cost %.4f%% gives an unhelpful run length %d", probe.Cost.TotalPct, need)
	}
	rates := func(n int) []float64 {
		out := []float64{1.0}
		for i := 0; i < n; i++ {
			out = append(out, -1.0)
		}
		return out
	}
	c.Settled = settled("binance_futures", 28800, rates(need-1)...)
	if got := EvaluateExit(evalAt, pos, c, p); hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("%d negatives have paid less than half a round trip and must be held:\n%s", need-1, strings.Join(got.LogLines(), "\n"))
	}
	c.Settled = settled("binance_futures", 28800, rates(need)...)
	if got := EvaluateExit(evalAt, pos, c, p); !hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("%d negatives reach half a round trip and must close:\n%s", need, strings.Join(got.LogLines(), "\n"))
	}
}

func TestEvaluateExit_CumulativeGateFailsSafeWhenTheRoundTripCannotBePriced(t *testing.T) {
	pos := openPosition()
	p := entryParams()
	p.ExitPersistencePeriods = 100
	p.ExitNegativeCumCostFrac = 0.5
	c := goodCandidate()
	c.SpotBook = depth.Summary{} // no book: unpriceable
	c.Settled = settled("binance_futures", 28800, 1.0, -0.1)

	got := EvaluateExit(evalAt, pos, c, p)
	if !hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("with no price for the round trip the cost gate must not hold the position:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	if !strings.Contains(strings.Join(got.LogLines(), "\n"), "coi như đạt") {
		t.Error("the check must say the gate was assumed, not measured")
	}
}

func TestEvaluateExit_GatesCombineWithAnd(t *testing.T) {
	pos := openPosition()
	p := entryParams()
	p.ExitPersistencePeriods = 50
	p.ExitNegativeMinBps = 0.5
	p.ExitNegativePeriods = 2

	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 1.0, -0.6, -0.3) // long enough, newest too shallow
	if got := EvaluateExit(evalAt, pos, c, p); hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("depth gate fails on −0.3, so no exit:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	c.Settled = settled("binance_futures", 28800, 1.0, -0.3, -0.6) // both gates met
	if got := EvaluateExit(evalAt, pos, c, p); !hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("both gates met, must close:\n%s", strings.Join(got.LogLines(), "\n"))
	}
}

// Jurisdiction: with a gate set, a run of shallow negatives is the gate's to
// judge — the decay exit must not close the position at the P-th negative
// print under the wrong label, which is what made the gates a no-op.
func TestEvaluateExit_DecayExitDoesNotCountNegativePrints(t *testing.T) {
	pos := openPosition()
	p := entryParams()
	p.ExitPersistencePeriods = 3
	p.ExitNegativeMinBps = 0.5

	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 1.0, -0.1, -0.1, -0.1)
	got := EvaluateExit(evalAt, pos, c, p)
	if got.Action != ActionHold {
		t.Fatalf("three −0.1 prints are above the −0.5 gate and must be held through, not closed as decay:\n%s",
			strings.Join(got.LogLines(), "\n"))
	}
	// Weak POSITIVE prints between negatives are still decay.
	c.Settled = settled("binance_futures", 28800, 1.0, 0.05, -0.1, 0.04, -0.1, 0.03)
	got = EvaluateExit(evalAt, pos, c, p)
	if got.Action != ActionExit || !hasTriggeredCheck(got, "net_apr_floor") {
		t.Fatalf("three weak positives under the floor are decay whatever sits between them:\n%s",
			strings.Join(got.LogLines(), "\n"))
	}
}

func TestEvaluateExit_GatesReadPer8hForDepthAndPerIntervalForCost(t *testing.T) {
	pos := openPosition()
	p := entryParams()
	p.ExitPersistencePeriods = 100

	// Depth gate compares the per-8h figure: −0.6 bps/8h at an hourly cadence
	// is −0.075 bps per settlement, and must still clear a −0.5 gate.
	p.ExitNegativeMinBps = 0.5
	c := goodCandidate()
	c.Settled = settled("binance_futures", 3600, 1.0, -0.6)
	if got := EvaluateExit(evalAt, pos, c, p); !hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("−0.6 bps/8h at 1h must clear a −0.5 bps/8h gate:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	c.Settled = settled("binance_futures", 28800, 1.0, -0.5) // exactly on the gate
	if got := EvaluateExit(evalAt, pos, c, p); !hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("exactly −0.5 is at the gate and must close:\n%s", strings.Join(got.LogLines(), "\n"))
	}

	// Cost gate sums what was PAID per settlement: at 1h, −1.0 bps/8h pays
	// 1/8 of what it pays at 8h, so the same run length pays 8× less.
	p.ExitNegativeMinBps = 0
	p.ExitNegativeCumCostFrac = 0.5
	probe := EvaluateExit(evalAt, pos, c, p)
	need8h := int(math.Ceil(0.5 * probe.Cost.TotalPct / 100 / 0.0001))
	run := func(interval int64, n int) []exchanges.FundingHistoryEntry {
		rates := []float64{1.0}
		for i := 0; i < n; i++ {
			rates = append(rates, -1.0)
		}
		return settled("binance_futures", interval, rates...)
	}
	c.Settled = run(3600, need8h+1)
	if got := EvaluateExit(evalAt, pos, c, p); hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("%d hourly prints of −1.0 bps/8h pay an eighth of the 8h run and must not reach the gate:\n%s",
			need8h+1, strings.Join(got.LogLines(), "\n"))
	}
	c.Settled = run(3600, 8*need8h+1)
	if got := EvaluateExit(evalAt, pos, c, p); !hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("%d hourly prints pay the 8h amount and must close:\n%s", 8*need8h+1, strings.Join(got.LogLines(), "\n"))
	}
}

func TestEvaluateExit_UnpricedRoundTripDoesNotOverrideTheOtherGates(t *testing.T) {
	pos := openPosition()
	p := entryParams()
	p.ExitPersistencePeriods = 100
	p.ExitNegativeMinBps = 0.5
	p.ExitNegativeCumCostFrac = 0.5
	c := goodCandidate()
	c.SpotBook = depth.Summary{} // unpriceable round trip
	c.Settled = settled("binance_futures", 28800, 1.0, -0.3)

	got := EvaluateExit(evalAt, pos, c, p) // Action is exit anyway: the decay exit cannot price the newest figure
	if hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("−0.3 fails the depth gate; an unpriced cost may only waive the COST gate:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	if !strings.Contains(strings.Join(got.LogLines(), "\n"), "chưa qua cổng thoát") {
		t.Error("the check must say the gates were not met")
	}
	p.ExitNegativeCumCostFrac = 0
	p.ExitNegativeMinBps = 0.5
	got = EvaluateExit(evalAt, pos, c, p)
	if strings.Contains(strings.Join(got.LogLines(), "\n"), "coi như đạt") {
		t.Error("with no cost gate configured nothing is 'assumed met'")
	}
	p.ExitNegativeMinBps, p.ExitNegativeCumCostFrac, p.ExitNegativePeriods = 0, 0.5, 3
	c.Settled = settled("binance_futures", 28800, 1.0, -0.3, -0.3)
	if got := EvaluateExit(evalAt, pos, c, p); hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("two negatives are not three, whatever the cost gate assumes:\n%s", strings.Join(got.LogLines(), "\n"))
	}
}

// 0 (zero value), 1 (config.yaml) and an absent key are one rule: the newest
// print alone, exactly as step 3.2 wrote it.
func TestEvaluateExit_PeriodsGateZeroAndOneAreTheSameRule(t *testing.T) {
	pos := openPosition()
	for _, rates := range [][]float64{{1.0, -0.1}, {1.0, -0.3, 0.2}, {-0.3, -0.3, -0.3}, {0.5}, {1.0, -0.0001}} {
		c := goodCandidate()
		c.Settled = settled("binance_futures", 28800, rates...)
		p0, p1 := entryParams(), entryParams()
		p0.ExitPersistencePeriods, p1.ExitPersistencePeriods = 100, 100
		p1.ExitNegativePeriods = 1
		fired0 := hasTriggeredCheck(EvaluateExit(evalAt, pos, c, p0), "funding_negative")
		fired1 := hasTriggeredCheck(EvaluateExit(evalAt, pos, c, p1), "funding_negative")
		want := rates[len(rates)-1] < 0
		if fired0 != want || fired1 != want {
			t.Errorf("rates %v: zero-value fired=%v, periods=1 fired=%v, want %v (newest < 0)", rates, fired0, fired1, want)
		}
	}
	if (Params{}).EffectiveExitNegativePeriods() != 1 || (Params{ExitNegativePeriods: 3}).EffectiveExitNegativePeriods() != 3 {
		t.Error("EffectiveExitNegativePeriods must map 0 to 1 and leave others alone")
	}
}

func TestEvaluateExit_NegativeRunSeesThroughASpecialPrint(t *testing.T) {
	pos := openPosition()
	p := entryParams()
	p.ExitPersistencePeriods = 100
	p.ExitNegativePeriods = 2

	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 1.0, -0.3, 5.0, -0.3)
	c.Settled[2].RateType = "Special" // a dividend print between two negatives
	if got := EvaluateExit(evalAt, pos, c, p); !hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("the run is two usable negatives; a Special print does not break it:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	// A Special NEGATIVE print is not paid funding of the regime either.
	p.ExitNegativePeriods, p.ExitNegativeCumCostFrac = 1, 0.5
	c.Settled = settled("binance_futures", 28800, 1.0, -0.3, -5.0, -0.3)
	c.Settled[2].RateType = "Special"
	got := EvaluateExit(evalAt, pos, c, p)
	// Two usable −0.3 bps/8h prints at an 8h cadence pay 2 × 0.003% = 0.0060%
	// of notional; the Special −5.0 would have added 0.05%.
	if !strings.Contains(strings.Join(got.LogLines(), "\n"), "Đợt âm đã trả 0.0060%") {
		t.Errorf("paid funding must sum the two usable −0.3 bps prints (0.0060%%), not the Special −5.0:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	// Newest raw print Special and positive: the newest USABLE print decides.
	p.ExitNegativeCumCostFrac = 0
	c.Settled = settled("binance_futures", 28800, 1.0, -0.3, -0.3, 5.0)
	c.Settled[3].RateType = "Special"
	if got := EvaluateExit(evalAt, pos, c, p); !hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("a Special newest print must not hide the negative regime:\n%s", strings.Join(got.LogLines(), "\n"))
	}
}

func TestEvaluateExit_ContinuousSeriesFallsBackToTheOldRuleAndSaysSo(t *testing.T) {
	pos := openPosition()
	p := entryParams()
	p.ExitPersistencePeriods = 100
	p.ExitNegativePeriods = 3
	c := goodCandidate()
	c.Settled = settled("paradex_futures", 3600, 1.0, -0.3)
	for i := range c.Settled {
		c.Settled[i].Model = exchanges.FundingContinuous
	}
	got := EvaluateExit(evalAt, pos, c, p)
	if !hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("samples cannot be counted, so the 3.2 rule applies and the newest negative closes:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	if !strings.Contains(strings.Join(got.LogLines(), "\n"), "MẪU liên tục") {
		t.Error("the check must say the gates were not evaluable on a continuous series")
	}
}

// UsableSettled hands back the caller's own slice when it drops nothing. The
// backtest calls it once per settlement on a growing prefix, so a copy made
// one replay quadratic in the settlement count — invisible at Binance's 1,095
// rows a year, hours of sweep time at Hyperliquid's 8,759.
func TestUsableSettled_CleanInputIsNotCopied(t *testing.T) {
	entries := []exchanges.FundingHistoryEntry{
		{Symbol: "BTCUSDT", Model: exchanges.FundingDiscrete, IntervalSec: 3600,
			RatePer8hFrac: 0.0001, RatePerIntervalFrac: 0.0000125, SettledAtMs: 1},
		{Symbol: "BTCUSDT", Model: exchanges.FundingDiscrete, IntervalSec: 3600,
			RatePer8hFrac: 0.0002, RatePerIntervalFrac: 0.000025, SettledAtMs: 2},
	}
	usable, dropped := UsableSettled(entries)
	if dropped != 0 || len(usable) != len(entries) {
		t.Fatalf("clean input: got %d entries, %d dropped", len(usable), dropped)
	}
	if &usable[0] != &entries[0] {
		t.Error("a clean input must be returned as-is, not copied")
	}
	if n := testing.AllocsPerRun(50, func() { UsableSettled(entries) }); n != 0 {
		t.Errorf("clean input allocated %v times per call; the backtest calls this once per settlement", n)
	}
}

// The slow path must still drop exactly what it always dropped, and must not
// alias the input once it has to build a new slice.
func TestUsableSettled_DirtyInputStillFiltersAndCopies(t *testing.T) {
	good := exchanges.FundingHistoryEntry{Symbol: "BTCUSDT", Model: exchanges.FundingDiscrete,
		IntervalSec: 3600, RatePer8hFrac: 0.0001, RatePerIntervalFrac: 0.0000125}
	special := good
	special.RateType = "Special"
	zeroInterval := good
	zeroInterval.IntervalSec = 0
	notFinite := good
	notFinite.RatePer8hFrac = math.Inf(1)

	first := good
	first.SettledAtMs = 1
	last := good
	last.SettledAtMs = 2
	entries := []exchanges.FundingHistoryEntry{first, special, zeroInterval, notFinite, last}

	usable, dropped := UsableSettled(entries)
	if dropped != 1 {
		t.Errorf("droppedSpecial = %d, want 1", dropped)
	}
	if len(usable) != 2 || usable[0].SettledAtMs != 1 || usable[1].SettledAtMs != 2 {
		t.Fatalf("want the two usable rows in order, got %+v", usable)
	}
	if &usable[0] == &entries[0] {
		t.Error("the filtered result must be its own slice")
	}
}

// referenceUsableSettled is UsableSettled as it was written before the
// no-copy fast path — kept here, and only here, so the optimization can be
// proven to change nothing but allocation. If the RULE ever changes, both
// implementations must change together and this test will say so.
func referenceUsableSettled(entries []exchanges.FundingHistoryEntry) (usable []exchanges.FundingHistoryEntry, droppedSpecial int) {
	for _, entry := range entries {
		if entry.RateType == "Special" {
			droppedSpecial++
			continue
		}
		if entry.IntervalSec <= 0 || !isFinite(entry.RatePer8hFrac) || !isFinite(entry.RatePerIntervalFrac) {
			continue
		}
		usable = append(usable, entry)
	}
	return usable, droppedSpecial
}

func TestUsableSettled_MatchesTheReferenceImplementation(t *testing.T) {
	// A deterministic pseudo-random walk over every shape the filter reacts
	// to, at the lengths the hourly venues really produce.
	seed := uint64(1)
	next := func(n int) int { seed = seed*6364136223846793005 + 1442695040888963407; return int(seed>>33) % n }
	for _, length := range []int{0, 1, 2, 17, 300, 8760} {
		entries := make([]exchanges.FundingHistoryEntry, 0, length)
		for i := 0; i < length; i++ {
			e := exchanges.FundingHistoryEntry{
				Symbol: "BTCUSDT", Model: exchanges.FundingDiscrete, SettledAtMs: int64(i),
				IntervalSec: 3600, RatePer8hFrac: 0.0001, RatePerIntervalFrac: 0.0000125,
			}
			switch next(12) {
			case 0:
				e.RateType = "Special"
			case 1:
				e.IntervalSec = 0
			case 2:
				e.RatePer8hFrac = math.Inf(1)
			case 3:
				e.RatePerIntervalFrac = math.NaN()
			}
			entries = append(entries, e)
		}
		input := make([]exchanges.FundingHistoryEntry, len(entries))
		copy(input, entries)
		gotUsable, gotDropped := UsableSettled(input)
		wantUsable, wantDropped := referenceUsableSettled(entries)
		if gotDropped != wantDropped {
			t.Errorf("len %d: droppedSpecial %d, want %d", length, gotDropped, wantDropped)
		}
		// DeepEqual, not an element walk: nil and an empty slice must not be
		// swapped either, or a caller distinguishing them would see the two
		// implementations differ.
		if !reflect.DeepEqual(gotUsable, wantUsable) {
			t.Errorf("len %d: filtered result differs from the reference (%d vs %d entries)",
				length, len(gotUsable), len(wantUsable))
		}
		// NaN is deliberately in the data and never equals itself, so the
		// mutation check compares NaN-aware rather than with DeepEqual.
		if !sameEntries(input, entries) {
			t.Errorf("len %d: the input slice was modified", length)
		}
	}
}

// sameEntries compares two settlement slices treating NaN as equal to NaN, so
// a mutation check can use inputs that deliberately contain NaN.
func sameEntries(a, b []exchanges.FundingHistoryEntry) bool {
	if len(a) != len(b) {
		return false
	}
	sameFloat := func(x, y float64) bool { return x == y || (math.IsNaN(x) && math.IsNaN(y)) }
	for i := range a {
		x, y := a[i], b[i]
		if !sameFloat(x.RatePer8hFrac, y.RatePer8hFrac) || !sameFloat(x.RatePerIntervalFrac, y.RatePerIntervalFrac) {
			return false
		}
		x.RatePer8hFrac, x.RatePerIntervalFrac = 0, 0
		y.RatePer8hFrac, y.RatePerIntervalFrac = 0, 0
		if x != y {
			return false
		}
	}
	return true
}

// --- minimum hold: the yield exits wait until the round trip is earned back ---

// The whole point of the gate. Funding has turned and cleared all three
// sign-flip gates, but the position has collected almost nothing back: leaving
// now realizes the entry cost AND pays the exit cost, to escape an episode
// that on this corpus costs a fraction of either.
func TestEvaluateExit_MinHoldHoldsAYieldExitUntilTheRoundTripIsEarnedBack(t *testing.T) {
	pos := openPosition()
	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 0.2, 0.1, -3.0, -4.0)
	p := entryParams()
	p.ExitNegativeMinBps, p.ExitNegativePeriods, p.ExitNegativeCumCostFrac = 0, 1, 0

	// Without the floor the sign flip closes the position.
	if got := EvaluateExit(evalAt, pos, c, p); got.Action != ActionExit {
		t.Fatalf("precondition: the ungated rule must exit here:\n%s", strings.Join(got.LogLines(), "\n"))
	}

	p.MinHoldRecoveredCostFrac = 1.0
	got := EvaluateExit(evalAt, pos, c, p)
	if got.Action != ActionHold {
		t.Fatalf("a position that has not earned back its round trip must not leave for a yield reason:\n%s",
			strings.Join(got.LogLines(), "\n"))
	}
	if !hasFailedCheck(got, "funding_negative") {
		t.Errorf("the blocked exit must still be reported as a check:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	// The reader must be able to tell "the condition did not happen" from
	// "the condition happened and the floor held the position anyway".
	detail := detailOf(got, "funding_negative")
	for _, want := range []string{"đảo dấu", "hoàn", "cổng giữ tối thiểu"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the blocked exit must say WHY it was blocked, missing %q: %s", want, detail)
		}
	}
}

// The decay exit is a yield exit too, and the floor covers it for the same
// reason: a decayed regime that has not yet repaid the entry cost is worse to
// leave than to hold.
func TestEvaluateExit_MinHoldAlsoHoldsTheDecayExit(t *testing.T) {
	pos := openPosition()
	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 0.05, 0.04, 0.03, 0.02)
	p := entryParams()

	if got := EvaluateExit(evalAt, pos, c, p); !hasTriggeredCheck(got, "net_apr_floor") {
		t.Fatalf("precondition: the ungated rule must decay-exit here:\n%s", strings.Join(got.LogLines(), "\n"))
	}

	p.MinHoldRecoveredCostFrac = 1.0
	got := EvaluateExit(evalAt, pos, c, p)
	if got.Action != ActionHold {
		t.Fatalf("the decay exit must respect the floor:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	if !strings.Contains(detailOf(got, "net_apr_floor"), "cổng giữ tối thiểu") {
		t.Errorf("the blocked decay exit must name the floor: %s", detailOf(got, "net_apr_floor"))
	}
}

// The floor is about YIELD, never about RISK. A hedge leg that has vanished
// means the position is no longer delta-neutral, and no amount of unrecovered
// cost is a reason to sit in a naked short.
func TestEvaluateExit_MinHoldNeverBlocksARiskExit(t *testing.T) {
	p := entryParams()
	p.MinHoldRecoveredCostFrac = 100 // a floor nothing could ever clear

	t.Run("hedge leg gone", func(t *testing.T) {
		c := goodCandidate()
		c.SpotSource, c.HedgeNoteVI = "", "Spot market bị huỷ niêm yết."
		got := EvaluateExit(evalAt, openPosition(), c, p)
		if got.Action != ActionExit || !hasTriggeredCheck(got, "hedge_gone") {
			t.Errorf("a vanished hedge leg must close whatever the floor says:\n%s",
				strings.Join(got.LogLines(), "\n"))
		}
	})

	t.Run("basis blown out", func(t *testing.T) {
		c := goodCandidate()
		c.PerpPriceQuote = c.SpotPriceQuote * 1.02
		got := EvaluateExit(evalAt, openPosition(), c, p)
		if got.Action != ActionExit || !hasTriggeredCheck(got, "basis_widened") {
			t.Errorf("a 2%% basis must close whatever the floor says:\n%s",
				strings.Join(got.LogLines(), "\n"))
		}
	})

	t.Run("net APR no longer computable", func(t *testing.T) {
		c := goodCandidate()
		c.PerpFee.Verified = false // no verified schedule -> no net figure at all
		got := EvaluateExit(evalAt, openPosition(), c, p)
		if got.Action != ActionExit {
			t.Errorf("losing the ability to price the position is not a yield judgement:\n%s",
				strings.Join(got.LogLines(), "\n"))
		}
	})
}

// Once the position HAS earned the round trip back, the floor is silent and
// the rule is the rule again.
func TestEvaluateExit_MinHoldStopsBlockingOnceTheCostIsRecovered(t *testing.T) {
	pos := openPosition()
	c := goodCandidate()
	// Six fat settlements at 40 bps/8h = 2.4% collected against a ~0.3% round
	// trip, then the sign flip.
	c.Settled = settled("binance_futures", 28800, 40, 40, 40, 40, 40, 40, -3.0)
	p := entryParams()
	p.ExitNegativeMinBps, p.ExitNegativePeriods, p.ExitNegativeCumCostFrac = 0, 1, 0
	p.MinHoldRecoveredCostFrac = 1.0

	got := EvaluateExit(evalAt, pos, c, p)
	if got.Action != ActionExit || !hasTriggeredCheck(got, "funding_negative") {
		t.Fatalf("the floor has been cleared; the sign flip must close the position:\n%s",
			strings.Join(got.LogLines(), "\n"))
	}
}

// Only settlements the position was actually open for count. Rule 6: a
// payment is collected because the position existed at the stamp.
func TestEvaluateExit_MinHoldCountsOnlyWhatThisPositionCollected(t *testing.T) {
	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 40, 40, 40, 40, 40, 40, -3.0)
	p := entryParams()
	p.ExitNegativeMinBps, p.ExitNegativePeriods, p.ExitNegativeCumCostFrac = 0, 1, 0
	p.MinHoldRecoveredCostFrac = 1.0

	// Opened AFTER the six fat settlements: they belong to whoever held then,
	// not to this position.
	pos := openPosition()
	pos.OpenedAtMs = c.Settled[len(c.Settled)-2].SettledAtMs

	got := EvaluateExit(evalAt, pos, c, p)
	if got.Action != ActionHold {
		t.Fatalf("funding paid before this position opened must not clear its floor:\n%s",
			strings.Join(got.LogLines(), "\n"))
	}
}

// Zero is off, and off must be the rule byte for byte — the 3.5 journal and
// every archived sweep were produced without this field.
func TestEvaluateExit_MinHoldZeroChangesNothing(t *testing.T) {
	cases := []struct {
		name   string
		rates  []float64
		mutate func(*Candidate)
	}{
		{"sign flip", []float64{1.0, 0.5, -0.2, -0.8}, nil},
		{"decay", []float64{0.05, 0.04, 0.03, 0.02}, nil},
		{"healthy", []float64{1.0, 1.1, 1.2, 1.0, 1.1, 1.3}, nil},
		{"basis", nil, func(c *Candidate) { c.PerpPriceQuote = c.SpotPriceQuote * 1.02 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := goodCandidate()
			if tc.rates != nil {
				c.Settled = settled("binance_futures", 28800, tc.rates...)
			}
			if tc.mutate != nil {
				tc.mutate(&c)
			}
			p := entryParams()
			want := EvaluateExit(evalAt, openPosition(), c, p)
			p.MinHoldRecoveredCostFrac = 0
			got := EvaluateExit(evalAt, openPosition(), c, p)
			if got.Action != want.Action {
				t.Fatalf("action %s != %s with the floor at zero", got.Action, want.Action)
			}
			for i := range want.Checks {
				if got.Checks[i] != want.Checks[i] {
					t.Errorf("check %q changed with the floor at zero:\n got  %+v\n want %+v",
						want.Checks[i].Name, got.Checks[i], want.Checks[i])
				}
			}
		})
	}
}

// An unpriceable round trip leaves the floor with no denominator. It must fail
// SAFE — the same direction ExitNegativeCumCostFrac takes — because a floor
// that blocks every exit on a venue whose book cannot be read would pin a
// position open for the one reason least related to whether holding is wise.
func TestEvaluateExit_MinHoldFailsSafeWhenTheRoundTripCannotBePriced(t *testing.T) {
	pos := openPosition()
	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 0.2, 0.1, -3.0, -4.0)
	c.SpotBook.BidDepthWithinTightQuote = 0 // no book -> no priced fill
	c.SpotBook.BidDepthWithinWideQuote = 0
	c.SpotBook.BestBidQuote = 0
	p := entryParams()
	p.ExitNegativeMinBps, p.ExitNegativePeriods, p.ExitNegativeCumCostFrac = 0, 1, 0
	p.MinHoldRecoveredCostFrac = 1.0

	got := EvaluateExit(evalAt, pos, c, p)
	if got.Action != ActionExit {
		t.Fatalf("with no round trip to recover, the floor must not hold the position:\n%s",
			strings.Join(got.LogLines(), "\n"))
	}
}

func detailOf(d Decision, name string) string {
	for _, check := range d.Checks {
		if check.Name == name {
			return check.DetailVI
		}
	}
	return ""
}

// --- margin on the perp leg ---

func verifiedBracket() risk.Bracket {
	return risk.Bracket{Source: "binance_futures", MaintenanceMarginFrac: 0.005, MaxLeverage: 125, Verified: true}
}

// The condition the whole model exists for: the position is flat in the coin
// and the PERP leg is not, because the spot gain sits in an account the perp
// venue cannot see.
func TestEvaluateExit_ClosesWhenTheShortNearsLiquidation(t *testing.T) {
	c := goodCandidate()
	c.PerpMargin = verifiedBracket()
	pos := openPosition()
	pos.PerpEntryPriceQuote = 81_000

	p := entryParams()
	p.PerpMarginFrac = 0.10                      // 10x: liquidation about 9.4% up
	p.MinLiquidationBufferPct = 2.0              // leave with 2% of room left
	p.MaxBasisPct, p.MaxBasisWidenPct = 100, 100 // keep the basis rule out of this test

	// Comfortably below: hold.
	c.PerpPriceQuote, c.SpotPriceQuote = 82_000, 82_000
	if got := EvaluateExit(evalAt, pos, c, p); got.Action != ActionHold {
		t.Fatalf("2%% up is not close to a 9.4%% liquidation:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	// Inside the buffer: leave.
	c.PerpPriceQuote, c.SpotPriceQuote = 87_500, 87_500
	got := EvaluateExit(evalAt, pos, c, p)
	if got.Action != ActionExit || !hasTriggeredCheck(got, "margin_thin") {
		t.Fatalf("a short 1%% from liquidation must close:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	if !strings.Contains(detailOf(got, "margin_thin"), "thanh lý") {
		t.Errorf("the exit must name the liquidation price: %s", detailOf(got, "margin_thin"))
	}
}

// It is a RISK exit, so the minimum-hold floor — which governs YIELD exits —
// must never hold a position through it. A floor that could pin a position
// into a liquidation would be worse than the churn it prevents.
func TestEvaluateExit_MinHoldNeverBlocksTheMarginExit(t *testing.T) {
	c := goodCandidate()
	c.PerpMargin = verifiedBracket()
	c.PerpPriceQuote, c.SpotPriceQuote = 88_000, 88_000
	pos := openPosition()
	pos.PerpEntryPriceQuote = 81_000

	p := entryParams()
	p.PerpMarginFrac, p.MinLiquidationBufferPct = 0.10, 2.0
	p.MaxBasisPct, p.MaxBasisWidenPct = 100, 100
	p.MinHoldRecoveredCostFrac = 100 // a floor nothing could ever clear

	got := EvaluateExit(evalAt, pos, c, p)
	if got.Action != ActionExit || !hasTriggeredCheck(got, "margin_thin") {
		t.Fatalf("the hold floor blocked a margin exit:\n%s", strings.Join(got.LogLines(), "\n"))
	}
}

// Off is off: with no margin fraction configured the condition reports "not
// evaluated" and every earlier run replays unchanged.
func TestEvaluateExit_MarginConditionIsOffByDefault(t *testing.T) {
	c := goodCandidate()
	c.PerpPriceQuote, c.SpotPriceQuote = 200_000, 200_000 // far past any liquidation
	pos := openPosition()
	pos.PerpEntryPriceQuote = 81_000

	p := entryParams()
	p.MaxBasisPct, p.MaxBasisWidenPct = 100, 100
	got := EvaluateExit(evalAt, pos, c, p)
	if hasTriggeredCheck(got, "margin_thin") {
		t.Errorf("the margin exit fired with no margin fraction configured:\n%s",
			strings.Join(got.LogLines(), "\n"))
	}
	for _, check := range got.Checks {
		if check.Name == "margin_thin" && !check.NotEvaluated {
			t.Error("an unconfigured margin condition must read as NOT EVALUATED, not as passed")
		}
	}
}

// An unverified maintenance schedule cannot state a liquidation price, and a
// leveraged short whose distance to a forced close nobody can state is not one
// to keep holding.
func TestEvaluateExit_UnverifiedMarginScheduleClosesRatherThanGuesses(t *testing.T) {
	c := goodCandidate()
	c.PerpMargin = risk.Bracket{Source: "binance_futures", MaintenanceMarginFrac: 0} // never looked up
	c.PerpPriceQuote, c.SpotPriceQuote = 81_000, 81_000
	pos := openPosition()
	pos.PerpEntryPriceQuote = 81_000

	p := entryParams()
	p.PerpMarginFrac, p.MinLiquidationBufferPct = 0.10, 2.0
	p.MaxBasisPct, p.MaxBasisWidenPct = 100, 100

	got := EvaluateExit(evalAt, pos, c, p)
	if got.Action != ActionExit || !hasTriggeredCheck(got, "margin_thin") {
		t.Fatalf("an unverified schedule must not be treated as a zero maintenance rate:\n%s",
			strings.Join(got.LogLines(), "\n"))
	}
}

// Without the perp entry price there is no quantity and no liquidation price.
// That is "not evaluated", never a confident number.
func TestEvaluateExit_MarginConditionNeedsTheEntryPrice(t *testing.T) {
	c := goodCandidate()
	c.PerpMargin = verifiedBracket()
	c.PerpPriceQuote, c.SpotPriceQuote = 81_000, 81_000

	p := entryParams()
	p.PerpMarginFrac, p.MinLiquidationBufferPct = 0.10, 2.0
	p.MaxBasisPct, p.MaxBasisWidenPct = 100, 100

	got := EvaluateExit(evalAt, openPosition(), c, p) // no PerpEntryPriceQuote
	for _, check := range got.Checks {
		if check.Name == "margin_thin" {
			if !check.NotEvaluated {
				t.Errorf("a missing entry price produced a verdict: %+v", check)
			}
			return
		}
	}
	t.Fatal("no margin_thin check was reported")
}

// A leveraged short whose liquidation price cannot be stated must be refused at
// the DOOR, not opened and closed again at the next settlement. Measured before
// this check existed: 46 trades per series and -322% summed over 24 series,
// against 1.7 trades and +27.6% with the margin model off — pure churn on the
// venues with no verified bracket.
func TestEvaluateEntry_RefusesLeverageOnAnUnverifiedMarginSchedule(t *testing.T) {
	c := goodCandidate()
	c.PerpPriceQuote = 81_000
	p := entryParams()
	p.PerpMarginFrac, p.MinLiquidationBufferPct = 0.10, 2.0

	// Unverified: refused, by name.
	c.PerpMargin = risk.Bracket{Source: "binance_futures"}
	got := EvaluateEntry(evalAt, c, p)
	if got.Action == ActionEnter {
		t.Fatalf("opened a 10x short with no verified maintenance rate:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	if !hasFailedCheck(got, "margin_known") {
		t.Errorf("the refusal must name the margin schedule:\n%s", strings.Join(got.LogLines(), "\n"))
	}

	// Verified: the same candidate enters.
	c.PerpMargin = verifiedBracket()
	if got := EvaluateEntry(evalAt, c, p); got.Action != ActionEnter {
		t.Fatalf("a verified bracket must not block entry:\n%s", strings.Join(got.LogLines(), "\n"))
	}
}

// A position above the venue's tier ceiling belongs to a stricter bracket, so
// its liquidation price computed from this one is optimistic. Refused too.
func TestEvaluateEntry_RefusesANotionalAboveTheTierCeiling(t *testing.T) {
	c := goodCandidate()
	c.PerpPriceQuote = 81_000
	c.PerpMargin = verifiedBracket()
	c.PerpMargin.TierCeilingQuote = 30_000

	p := entryParams()
	p.PerpMarginFrac, p.MinLiquidationBufferPct = 0.10, 2.0
	p.NotionalQuote = 50_000 // above the ceiling

	got := EvaluateEntry(evalAt, c, p)
	if got.Action == ActionEnter || !hasFailedCheck(got, "margin_known") {
		t.Fatalf("a position above the tier ceiling was opened against this tier's rate:\n%s",
			strings.Join(got.LogLines(), "\n"))
	}
}

// With no leverage configured the condition passes and says why, so an entry
// log never leaves a reader wondering whether the margin was checked.
func TestEvaluateEntry_MarginCheckIsSilentWhenLeverageIsOff(t *testing.T) {
	got := EvaluateEntry(evalAt, goodCandidate(), entryParams())
	if got.Action != ActionEnter {
		t.Fatalf("the unleveraged candidate stopped entering:\n%s", strings.Join(got.LogLines(), "\n"))
	}
	if hasFailedCheck(got, "margin_known") {
		t.Error("the margin check failed with no leverage configured")
	}
	if !strings.Contains(detailOf(got, "margin_known"), "perp_margin_frac") {
		t.Errorf("the check must say it was not needed: %q", detailOf(got, "margin_known"))
	}
}

// --- series selection: the trailing-mean condition (2026-09-09) ---

// Off is the rule exactly as it stood before the fields existed: the same
// decision, check for check, with the new one reporting itself as not applied.
func TestEvaluateEntry_TrailingMeanOffChangesNothing(t *testing.T) {
	p := entryParams()
	before := EvaluateEntry(evalAt, goodCandidate(), p)
	p.MinTrailingMeanBps, p.TrailingMeanDays = 0, 0
	after := EvaluateEntry(evalAt, goodCandidate(), p)
	if before.Action != after.Action || len(before.Checks) != len(after.Checks) {
		t.Fatalf("zero selection fields changed the decision: %v vs %v", before.Action, after.Action)
	}
	tm := findCheck(t, after, "trailing_mean")
	if !tm.Passed || !strings.Contains(tm.DetailVI, "Không xét") {
		t.Errorf("an off condition must pass and say it is off: %+v", tm)
	}
	// A floor with no horizon is off too — config refuses the block, but the
	// package must not divide by an empty window if handed one.
	p.MinTrailingMeanBps, p.TrailingMeanDays = 0.5, 0
	if d := EvaluateEntry(evalAt, goodCandidate(), p); d.Action != before.Action {
		t.Errorf("a floor with a zero horizon must be off, got %v", d.Action)
	}
}

// The mean is taken over the venue's stamps inside the window, per 8h, and
// compared to the floor. Ten 8h settlements averaging 1.1 bps clear a 1.0
// floor and miss a 1.2 one.
func TestEvaluateEntry_TrailingMeanComparesTheWindowMeanToTheFloor(t *testing.T) {
	c := goodCandidate()
	c.Settled = settled("binance_futures", 28800, 1.0, 1.2, 1.0, 1.2, 1.0, 1.2, 1.0, 1.2, 1.0, 1.2) // mean 1.1
	p := entryParams()
	p.PersistencePeriods = 2
	p.TrailingMeanDays = 10 * 8.0 / 24 // exactly the ten settlements
	p.MinTrailingMeanBps = 1.0
	d := EvaluateEntry(evalAt, c, p)
	tm := findCheck(t, d, "trailing_mean")
	if !tm.Passed || !strings.Contains(tm.DetailVI, "1.1000 bps/8h") || !strings.Contains(tm.DetailVI, "10 mốc") {
		t.Errorf("mean 1.1 over 10 settlements must clear a 1.0 floor: %+v", tm)
	}
	p.MinTrailingMeanBps = 1.2
	d = EvaluateEntry(evalAt, c, p)
	if tm := findCheck(t, d, "trailing_mean"); tm.Passed {
		t.Errorf("mean 1.1 must not clear a 1.2 floor: %+v", tm)
	}
	if d.Action != ActionSkip {
		t.Errorf("a failed selection must skip, got %v", d.Action)
	}
}

// A history that starts INSIDE the window is refused rather than averaged:
// the mean of a shorter horizon is a different number wearing the same name.
// One interval of slack at the start, because the first settlement inside a
// window that begins at a cadence boundary lands one interval after it.
func TestEvaluateEntry_TrailingMeanRefusesAHistoryShorterThanItsWindow(t *testing.T) {
	c := goodCandidate() // six 8h settlements = 2 days of history
	p := entryParams()
	p.MinTrailingMeanBps = 0.5
	p.TrailingMeanDays = 30
	d := EvaluateEntry(evalAt, c, p)
	tm := findCheck(t, d, "trailing_mean")
	if tm.Passed || !strings.Contains(tm.DetailVI, "chưa phủ 30 ngày") {
		t.Errorf("2 days of history must not produce a 30-day mean: %+v", tm)
	}
	// Exactly the history's span, allowing the one-interval slack, is covered.
	p.TrailingMeanDays = 2
	if tm := findCheck(t, EvaluateEntry(evalAt, c, p), "trailing_mean"); !tm.Passed {
		t.Errorf("a 2-day window over 2 days of 8h history is covered: %+v", tm)
	}
}

// The window is cut on stamps, not on a count: 90 days is 270 settlements at
// 8h and 2,160 at 1h, and the same floor means the same thing on both.
func TestEvaluateEntry_TrailingMeanWindowIsDaysNotSettlements(t *testing.T) {
	hourly := make([]float64, 48) // two days of hourly settlements at 0.9
	for i := range hourly {
		hourly[i] = 0.9
	}
	c := goodCandidate()
	c.PerpSource = "hyperliquid_futures"
	c.Settled = settled("hyperliquid_futures", 3600, hourly...)
	p := entryParams()
	p.MinRatePer8hBps, p.PersistencePeriods = 0.5, 3
	p.MinTrailingMeanBps, p.TrailingMeanDays = 0.8, 1
	tm := findCheck(t, EvaluateEntry(evalAt, c, p), "trailing_mean")
	if !tm.Passed || !strings.Contains(tm.DetailVI, "24 mốc") {
		t.Errorf("one day of hourly history is 24 settlements, not 3: %+v", tm)
	}
}

// findCheck returns the named check of a decision, failing the test when it
// is absent — a condition that silently stopped being evaluated would
// otherwise pass every assertion written about it.
func findCheck(t *testing.T, d Decision, name string) Check {
	t.Helper()
	for _, c := range d.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in:\n%s", name, strings.Join(d.LogLines(), "\n"))
	return Check{}
}
