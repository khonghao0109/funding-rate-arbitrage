package strategy

import (
	"math"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/fees"
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
