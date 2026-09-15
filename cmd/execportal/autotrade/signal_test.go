package autotrade

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/strategy"
)

// The decision as pure functions: cadence, hold plan, every entry check, every
// exit check. No venue, no clock beyond the one passed in.

func TestMeasureIntervalSec(t *testing.T) {
	now := time.Now()
	hourly := func(n int, gap time.Duration) []SettledRate {
		out := make([]SettledRate, n)
		for i := range out {
			out[i] = SettledRate{SettledAtMs: now.Add(time.Duration(i) * gap).UnixMilli()}
		}
		return out
	}
	if got, why := measureIntervalSec(settledEvery8h(21, 0.0001, now)); got != 28_800 || why != "" {
		t.Errorf("8h with a millisecond of jitter = %d %q", got, why)
	}
	if got, _ := measureIntervalSec(hourly(30, time.Hour)); got != 3_600 {
		t.Errorf("1h = %d", got)
	}
	// One missed settlement in 21 is a double gap, not a new cadence.
	missed := settledEvery8h(22, 0.0001, now)
	missed = append(missed[:10], missed[11:]...)
	if got, why := measureIntervalSec(missed); got != 28_800 {
		t.Errorf("8h with one missed settlement = %d %q", got, why)
	}
	// A symbol moved from 8h to 4h inside the window: refused, not averaged.
	mixed := append(hourly(8, 8*time.Hour), hourly(8, 4*time.Hour)...)
	for i := 8; i < len(mixed); i++ {
		mixed[i].SettledAtMs += mixed[7].SettledAtMs - now.UnixMilli() + (4 * time.Hour).Milliseconds()
	}
	if got, why := measureIntervalSec(mixed); got != 0 || why == "" {
		t.Errorf("a mixed cadence = %d %q", got, why)
	}
	if got, why := measureIntervalSec(hourly(2, time.Hour)); got != 0 || why == "" {
		t.Errorf("two stamps = %d %q", got, why)
	}
	// Special settlements are off the cadence and do not break it.
	withSpecial := settledEvery8h(10, 0.0001, now)
	withSpecial = append(withSpecial, SettledRate{SettledAtMs: withSpecial[4].SettledAtMs + 1000, Special: true})
	if got, _ := measureIntervalSec(withSpecial); got != 28_800 {
		t.Errorf("with a special settlement = %d", got)
	}
}

// A plan of N settlements must be priced as exactly N by strategy's own floor
// count — for every hold the form accepts, at every cadence Binance runs.
func TestHoldPlan_CountsExactlyTheSettlementsPlanned(t *testing.T) {
	cost := strategy.RoundTrip{OK: true, TotalPct: 0.1}
	for _, intervalSec := range []int64{3_600, 14_400, 28_800} {
		for n := 1; n <= maxHoldEpochsCap; n++ {
			cfg := Config{MaxHoldEpochs: n}
			days, want := holdPlan(cfg, intervalSec)
			res := strategy.NetAPR(strategy.NetAPRInput{Model: exchanges.FundingDiscrete, RatePerIntervalFrac: 0.0001,
				IntervalSec: intervalSec, HoldingDays: days, Cost: cost})
			if !res.OK || res.SettlementsInHold != want {
				t.Fatalf("interval %d, %d epochs: strategy counts %d (ok %v %s)", intervalSec, n, res.SettlementsInHold, res.OK, res.ReasonVI)
			}
		}
	}
	if days, n := holdPlan(Config{ProjectionHoldDays: 30}, 28_800); days != 30 || n != 0 {
		t.Errorf("hold while funding pays = %v days, %d", days, n)
	}
}

func goodInput(now time.Time) entryInput {
	return entryInput{Cfg: DefaultConfig(testSymbol), Snap: goodMarket(now).snap, Now: now, MarginFrac: 0.5, Flat: true}
}

func check(t *testing.T, checks []CheckView, prefix string) CheckView {
	t.Helper()
	for _, c := range checks {
		if strings.HasPrefix(c.NameVI, prefix) {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", prefix, checks)
	return CheckView{}
}

func TestAssessEntry_AGoodReadingPassesEveryCheck(t *testing.T) {
	now := time.Now()
	sig := assessEntry(goodInput(now))
	if !sig.EntryEligible || sig.VerdictVI != "ĐỦ ĐIỀU KIỆN VÀO" || len(sig.EntryChecks) != 7 {
		t.Fatalf("verdict %q, %d checks: %+v", sig.VerdictVI, len(sig.EntryChecks), sig.EntryChecks)
	}
	// 90 settlements of 1 bps over 30 days, a round trip of 8 bps of fees plus
	// the four fills' slippage: strategy's figure, carried as it is.
	if sig.IntervalSec != 28_800 || sig.SettlementsInHold != 90 || sig.HoldingDays != 30 || sig.TrailingSettlements != 21 {
		t.Errorf("interval %d, settlements %d, days %v, trailing %d", sig.IntervalSec, sig.SettlementsInHold, sig.HoldingDays, sig.TrailingSettlements)
	}
	if sig.FeesPct == nil || math.Abs(*sig.FeesPct-0.08) > 1e-12 {
		t.Errorf("fees = %v, want (0 + 4) × 2 / 100", sig.FeesPct)
	}
	want := (90*0.0001 - *sig.RoundTripCostPct/100) * 365 / 30 * 100
	if sig.NetAPRPct == nil || math.Abs(*sig.NetAPRPct-want) > 1e-9 {
		t.Errorf("net APR = %v, want %v", sig.NetAPRPct, want)
	}
	if math.Abs(*sig.NetAPROnCapitalPct-*sig.NetAPRPct/1.5) > 1e-9 || sig.CapitalPerNotional != 1.5 {
		t.Errorf("on capital = %v at %v× notional", *sig.NetAPROnCapitalPct, sig.CapitalPerNotional)
	}
	if sig.ForecastRatePer8hBps == nil || math.Abs(*sig.ForecastRatePer8hBps-1) > 1e-9 {
		t.Errorf("per 8h = %v", sig.ForecastRatePer8hBps)
	}
	if sig.EntryCostPct == nil || *sig.EntryCostPct <= 0 || len(sig.AppliedVI) == 0 || len(sig.ExcludedVI) == 0 {
		t.Errorf("entry cost %v, applied %d, excluded %d", sig.EntryCostPct, len(sig.AppliedVI), len(sig.ExcludedVI))
	}
}

// Every check runs: a reading that fails four of them names all four.
func TestAssessEntry_NamesEveryFailingCheck(t *testing.T) {
	now := time.Now()
	in := goodInput(now)
	in.Flat, in.HeldVI = false, "1 ý định không phải của bot"
	in.Snap.ForecastRatePerIntervalFrac = -0.00002
	// One minute before the next settlement, with the history consistent with
	// that stamp (the last settled one exactly 8h earlier).
	in.Snap.NextFundingTimeMs = now.Add(time.Minute).UnixMilli()
	shift := in.Snap.NextFundingTimeMs - (8 * time.Hour).Milliseconds() - in.Snap.Settled[len(in.Snap.Settled)-1].SettledAtMs
	in.Snap.Settled = append([]SettledRate(nil), in.Snap.Settled...)
	for i := range in.Snap.Settled {
		in.Snap.Settled[i].SettledAtMs += shift
	}
	in.Snap.SpotBook.BidDepthWithinWideQuote = 100
	sig := assessEntry(in)
	if sig.EntryEligible {
		t.Fatal("eligible")
	}
	for _, name := range []string{"Không có vị thế nào mở", "Funding đang hình thành", "Độ sâu", "Còn đủ xa mốc settle"} {
		if c := check(t, sig.EntryChecks, name); c.Passed {
			t.Errorf("%s passed: %+v", name, c)
		}
		if !strings.Contains(sig.VerdictVI, name) {
			t.Errorf("verdict %q does not name %s", sig.VerdictVI, name)
		}
	}
	if c := check(t, sig.EntryChecks, "Net APR"); !c.Passed {
		t.Errorf("the APR check should still have run and passed: %+v", c)
	}
}

// Anything the price depends on that could not be read refuses the figure and
// the entry — never a zero fee, a guessed cadence or a thin mean.
func TestAssessEntry_AnUnreadInputIsNeverPricedAsZero(t *testing.T) {
	now := time.Now()
	for name, mutate := range map[string]func(in *entryInput){
		"fees unread":         func(in *entryInput) { in.Snap.FeesErrVI = "401" },
		"history unread":      func(in *entryInput) { in.Snap.SettledErrVI = "timeout" },
		"two settlements":     func(in *entryInput) { in.Snap.Settled = in.Snap.Settled[len(in.Snap.Settled)-2:] },
		"a stale book":        func(in *entryInput) { in.Snap.PerpBook.SampledAtMs = now.Add(-5 * time.Minute).UnixMilli() },
		"no next settlement":  func(in *entryInput) { in.Snap.NextFundingTimeMs = 0 },
		"last settled ≤ 0":    func(in *entryInput) { in.Snap.Settled[len(in.Snap.Settled)-1].RatePerIntervalFrac = 0 },
		"below the floor":     func(in *entryInput) { in.Cfg.MinNetAPRPct = 50 },
		"one settlement hold": func(in *entryInput) { in.Cfg.MaxHoldEpochs = 1 },
	} {
		in := goodInput(now)
		in.Snap.Settled = append([]SettledRate(nil), in.Snap.Settled...)
		mutate(&in)
		sig := assessEntry(in)
		if sig.EntryEligible {
			t.Errorf("%s: eligible (%+v)", name, sig.EntryChecks)
		}
	}
	in := goodInput(now)
	in.Snap.FeesErrVI = "401"
	if sig := assessEntry(in); sig.NetAPRPct != nil || !strings.Contains(sig.NetAPRReasonVI, "miễn phí") {
		t.Errorf("unread fees priced: %v %q", sig.NetAPRPct, sig.NetAPRReasonVI)
	}
	// A book that stops short of the window and holds less than asked is
	// UNKNOWN, not thin.
	in = goodInput(now)
	in.Snap.PerpBook.AskSpanPct, in.Snap.PerpBook.AskDepthWithinWideQuote = 0.2, 50
	if c := check(t, assessEntry(in).EntryChecks, "Độ sâu"); c.Passed || c.Evaluated || !strings.Contains(c.DetailVI, "≥50") {
		t.Errorf("a truncated thin side = %+v", c)
	}
}

// The clock breaker and the cadence cross-check.
func TestAssessEntry_TheClockAndTheNextStampMustAgree(t *testing.T) {
	now := time.Now()
	in := goodInput(now)
	in.Snap.PerpClockSkewMs = ptrInt(1_200)
	if c := check(t, assessEntry(in).EntryChecks, "Lệch đồng hồ"); c.Passed || !c.Evaluated {
		t.Errorf("a 1.2 s skew = %+v", c)
	}
	in = goodInput(now)
	in.Snap.SpotClockSkewMs = nil
	if c := check(t, assessEntry(in).EntryChecks, "Lệch đồng hồ"); c.Passed || c.Evaluated {
		t.Errorf("an unmeasured clock = %+v", c)
	}
	// The symbol moved to 4h on the last settlement: history still says 8h.
	in = goodInput(now)
	in.Snap.NextFundingTimeMs = in.Snap.Settled[len(in.Snap.Settled)-1].SettledAtMs + (4 * time.Hour).Milliseconds()
	if sig := assessEntry(in); sig.EntryEligible || sig.NetAPRPct != nil || !strings.Contains(sig.NetAPRReasonVI, "chu kỳ") {
		t.Errorf("a next stamp off the measured cadence priced: %v %q", sig.NetAPRPct, sig.NetAPRReasonVI)
	}
}

// Specials are settlements crossed but not the regular rate.
func TestTrailingMean_LeavesSpecialsOut(t *testing.T) {
	now := time.Now()
	rows := settledEvery8h(5, 0.0001, now)
	rows = append(rows, SettledRate{SettledAtMs: now.UnixMilli(), RatePerIntervalFrac: 0.05, Special: true})
	if mean, n := trailingMean(rows, 0); n != 5 || math.Abs(mean-0.0001) > 1e-15 {
		t.Errorf("mean %v over %d", mean, n)
	}
}

// One side that definitely fails is a definite failure, whatever another side
// could not show.
func TestDepthCheck_ADefiniteFailureIsEvaluated(t *testing.T) {
	now := time.Now()
	in := goodInput(now)
	in.Snap.PerpBook.AskSpanPct, in.Snap.PerpBook.AskDepthWithinWideQuote = 0.2, 50 // unknown
	in.Snap.SpotBook.BidDepthWithinWideQuote = 10                                   // definite
	if c := check(t, assessEntry(in).EntryChecks, "Độ sâu"); c.Passed || !c.Evaluated {
		t.Errorf("a definite and an unknown shortfall = %+v", c)
	}
}

func TestAssessExit(t *testing.T) {
	now := time.Now()
	pos := PositionView{IntentID: "a", OpenedAtMs: now.Add(-time.Hour).UnixMilli(), EntryBasisBps: 1}
	base := goodMarket(now).snap
	cfg := DefaultConfig(testSymbol)

	ex := assessExit(cfg, base, pos)
	if ex.Due || ex.SettlementsSinceOpen != 0 || len(ex.Checks) != 3 {
		t.Fatalf("a fresh hold = %+v", ex)
	}

	paid := base
	paid.Settled = append(append([]SettledRate(nil), base.Settled...), SettledRate{SettledAtMs: now.UnixMilli(), RatePerIntervalFrac: 0.0001})
	if ex := assessExit(cfg, paid, pos); ex.Due || ex.SettlementsSinceOpen != 1 {
		t.Errorf("a paid settlement = %+v", ex)
	}
	charged := base
	charged.Settled = append(append([]SettledRate(nil), base.Settled...), SettledRate{SettledAtMs: now.UnixMilli(), RatePerIntervalFrac: -0.00001})
	if ex := assessExit(cfg, charged, pos); !ex.Due || !strings.Contains(strings.Join(ex.ReasonsVI, " "), "≤ 0") {
		t.Errorf("a charged settlement = %+v", ex)
	}
	// A negative rate from BEFORE the open was somebody else's settlement.
	before := base
	before.Settled = append([]SettledRate(nil), base.Settled...)
	before.Settled[len(before.Settled)-1].RatePerIntervalFrac = -0.001
	if ex := assessExit(cfg, before, pos); ex.Due {
		t.Errorf("a pre-open negative rate closed the pair: %+v", ex)
	}

	epochs := cfg
	epochs.MaxHoldEpochs = 2
	if ex := assessExit(epochs, paid, pos); ex.Due {
		t.Errorf("1 of 2 epochs closed: %+v", ex)
	}
	epochs.MaxHoldEpochs = 1
	if ex := assessExit(epochs, paid, pos); !ex.Due {
		t.Errorf("1 of 1 epochs held: %+v", ex)
	}

	wide := base
	wide.PerpBook.MidPriceQuote = testMid * 1.0032 // +32 bps against an entry basis of +1
	if ex := assessExit(cfg, wide, pos); !ex.Due || ex.BasisWidenBps == nil || math.Abs(*ex.BasisWidenBps-31) > 1e-6 {
		t.Errorf("a widened basis = %+v (%v)", ex, ex.BasisWidenBps)
	}

	// Unreadable inputs never close a pair.
	blind := wide
	blind.SettledErrVI = "timeout"
	blind.PerpBook.MidPriceQuote = 0
	if ex := assessExit(epochs, blind, pos); ex.Due {
		t.Errorf("missing data closed the pair: %+v", ex)
	}
	for _, c := range assessExit(epochs, blind, pos).Checks {
		if c.Evaluated || !c.Passed {
			t.Errorf("an unreadable exit check = %+v", c)
		}
	}
}

func TestRingLog_KeepsTheNewestTwentyNewestFirst(t *testing.T) {
	var r ringLog
	for i := 0; i < 27; i++ {
		r.add(LogEntry{AtMs: int64(i), MessageVI: fmt.Sprint(i)})
	}
	got := r.newestFirst()
	if len(got) != logCapacity || got[0].AtMs != 26 || got[logCapacity-1].AtMs != 7 {
		t.Errorf("ring = %d entries, %d … %d", len(got), got[0].AtMs, got[len(got)-1].AtMs)
	}
	var small ringLog
	small.add(LogEntry{AtMs: 1})
	small.add(LogEntry{AtMs: 2})
	if got := small.newestFirst(); len(got) != 2 || got[0].AtMs != 2 {
		t.Errorf("small ring = %+v", got)
	}
}
