package autotrade

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/depth"
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
	cfg := DefaultConfig(testSymbol)
	// The shipped DefaultNotionalQuote is a SEED, not a size a run trades: with
	// auto-rebalance on, the first scan sizes it from the account before any
	// entry is judged. At goodSnapshot's rules it is under the venue's own size
	// floor, so a fixture standing for "a reading every check passes" carries
	// the size the rig's runs actually use.
	cfg.NotionalQuote = testNotional
	return entryInput{Cfg: cfg, Snap: *goodSnapshot(now, 0.0001), Now: now, MarginFrac: 0.5, Flat: true}
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
	if !sig.EntryEligible || sig.VerdictVI != "ĐỦ ĐIỀU KIỆN VÀO" || len(sig.EntryChecks) != 9 {
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

// heldPosition is a pair opened on goodSnapshot's own reading: the two legs
// filled at the two mids, so its drift against an unchanged market is zero and
// only funding, the entry commission and the priced exit move its result.
func heldPosition(now time.Time) PositionView {
	const qty = 0.0008
	return PositionView{IntentID: "a", OpenedAtMs: now.Add(-time.Hour).UnixMilli(), EntryBasisBps: 1,
		QtyCoin: qty, NotionalQuote: qty * testMid, CapitalQuote: qty * testMid * 1.5,
		SpotEntryAvgQuote: testMid, PerpEntryAvgQuote: testPerpMid}
}

func TestAssessExit(t *testing.T) {
	now := time.Now()
	pos := heldPosition(now)
	base := *goodSnapshot(now, 0.0001)
	cfg := DefaultConfig(testSymbol)

	ex := assessExit(cfg, base, pos, now)
	if ex.Due || ex.SettlementsSinceOpen != 0 || len(ex.Checks) != 4 {
		t.Fatalf("a fresh hold = %+v", ex)
	}

	paid := base
	paid.Settled = append(append([]SettledRate(nil), base.Settled...), SettledRate{SettledAtMs: now.UnixMilli(), RatePerIntervalFrac: 0.0001})
	if ex := assessExit(cfg, paid, pos, now); ex.Due || ex.SettlementsSinceOpen != 1 {
		t.Errorf("a paid settlement = %+v", ex)
	}
	charged := base
	charged.Settled = append(append([]SettledRate(nil), base.Settled...), SettledRate{SettledAtMs: now.UnixMilli(), RatePerIntervalFrac: -0.00001})
	if ex := assessExit(cfg, charged, pos, now); ex.Due {
		t.Errorf("a charged settlement inside min hold floor exited: %+v", ex)
	}
	legacy := cfg
	legacy.MinHoldEpochs = 0
	if ex := assessExit(legacy, charged, pos, now); !ex.Due || !strings.Contains(strings.Join(ex.ReasonsVI, " "), "≤ 0") {
		t.Errorf("a charged settlement with no min hold = %+v", ex)
	}
	// A negative rate from BEFORE the open was somebody else's settlement.
	before := base
	before.Settled = append([]SettledRate(nil), base.Settled...)
	before.Settled[len(before.Settled)-1].RatePerIntervalFrac = -0.001
	if ex := assessExit(cfg, before, pos, now); ex.Due {
		t.Errorf("a pre-open negative rate closed the pair: %+v", ex)
	}

	epochs := cfg
	epochs.MaxHoldEpochs = 2
	if ex := assessExit(epochs, paid, pos, now); ex.Due {
		t.Errorf("1 of 2 epochs closed: %+v", ex)
	}
	epochs.MaxHoldEpochs = 1
	if ex := assessExit(epochs, paid, pos, now); !ex.Due {
		t.Errorf("1 of 1 epochs held: %+v", ex)
	}

	wide := base
	wide.PerpBook.MidPriceQuote = testMid * 1.0102 // +102 bps against an entry basis of +1 = +101 bps > 100
	if ex := assessExit(cfg, wide, pos, now); !ex.Due || ex.BasisWidenBps == nil || math.Abs(*ex.BasisWidenBps-101) > 1e-6 {
		t.Errorf("a widened basis = %+v (%v)", ex, ex.BasisWidenBps)
	}

	// Unreadable inputs never close a pair.
	blind := wide
	blind.SettledErrVI = "timeout"
	blind.PerpBook.MidPriceQuote = 0
	if ex := assessExit(epochs, blind, pos, now); ex.Due {
		t.Errorf("missing data closed the pair: %+v", ex)
	}
	for _, c := range assessExit(epochs, blind, pos, now).Checks {
		if c.Evaluated || !c.Passed {
			t.Errorf("an unreadable exit check = %+v", c)
		}
	}
}

func TestRingLog_KeepsTheNewestNewestFirst(t *testing.T) {
	var r ringLog
	for i := 0; i < logCapacity+7; i++ {
		r.add(LogEntry{AtMs: int64(i), MessageVI: fmt.Sprint(i)})
	}
	got := r.newestFirst()
	if len(got) != logCapacity || got[0].AtMs != int64(logCapacity+6) || got[logCapacity-1].AtMs != 7 {
		t.Errorf("ring = %d entries, %d … %d", len(got), got[0].AtMs, got[len(got)-1].AtMs)
	}
	var small ringLog
	small.add(LogEntry{AtMs: 1})
	small.add(LogEntry{AtMs: 2})
	if got := small.newestFirst(); len(got) != 2 || got[0].AtMs != 2 {
		t.Errorf("small ring = %+v", got)
	}
}

// Every check carries the stable key the page reads, and no two share one: the
// radar's four badges must not depend on the wording of a Vietnamese name.
func TestChecks_EveryConditionHasItsOwnKey(t *testing.T) {
	now := time.Now()
	entry := assessEntry(goodInput(now))
	want := []CheckKey{CheckFlat, CheckFormingPositive, CheckLastSettledPositive, CheckEntryBasis, CheckSizeFits, CheckNetAPR, CheckDepth, CheckClock, CheckTimeToSettle}
	if len(entry.EntryChecks) != len(want) {
		t.Fatalf("%d entry checks, want %d", len(entry.EntryChecks), len(want))
	}
	for i, c := range entry.EntryChecks {
		if c.Key != want[i] {
			t.Errorf("entry check %d (%s) key = %q, want %q", i, c.NameVI, c.Key, want[i])
		}
	}
	ex := assessExit(DefaultConfig(testSymbol), *goodSnapshot(now, 0.0001), heldPosition(now), now)
	wantExit := []CheckKey{CheckExitFunding, CheckExitEpochs, CheckExitBasis, CheckExitTakeProfit}
	if len(ex.Checks) != len(wantExit) {
		t.Fatalf("%d exit checks", len(ex.Checks))
	}
	for i, c := range ex.Checks {
		if c.Key != wantExit[i] {
			t.Errorf("exit check %d (%s) key = %q, want %q", i, c.NameVI, c.Key, wantExit[i])
		}
	}
}

// ------------------------- the convergence-and-amortization set (4.5f)
//
// The four pillars of docs/AUTOTRADE-CONVERGENCE-HOLDING-STRATEGY.md, each
// tested where it is decided: assessEntry for the basis floor, assessExit for
// the amortization floor, the hysteresis and the take-profit.

// Trụ cột 1: a pair whose perp is not trading above its spot is refused, and
// the refusal is the basis check alone — every other condition still passes, so
// the console names the real reason.
func TestAssessEntry_RefusesAnEntryBelowTheBasisFloor(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		nameVI     string
		perpMid    float64
		floorBps   float64
		wantPassed bool
	}{
		{"perp cao hơn spot đúng ngưỡng", testMid * 1.0005, 5, true},
		{"perp cao hơn spot dưới ngưỡng", testMid * 1.0004, 5, false},
		{"basis phẳng", testMid, 5, false},
		{"basis lõm", testMid * 0.9962, 5, false}, // -38 bps, the NEAR trap the doc names
		{"basis lõm nhưng ngưỡng tắt", testMid * 0.9962, -100, true},
	} {
		in := goodInput(now)
		in.Cfg.MinEntryBasisBps = tc.floorBps
		in.Snap.PerpBook = book("binance_futures", tc.perpMid, now)
		sig := assessEntry(in)
		c := check(t, sig.EntryChecks, "Basis lúc vào")
		if !c.Evaluated || c.Passed != tc.wantPassed {
			t.Errorf("%s: basis check = %+v, want passed=%v", tc.nameVI, c, tc.wantPassed)
		}
		if sig.EntryEligible != tc.wantPassed {
			t.Errorf("%s: eligible = %v (verdict %q)", tc.nameVI, sig.EntryEligible, sig.VerdictVI)
		}
	}
	// No mids: the floor is not evaluated, and an entry check that could not be
	// read never passes.
	in := goodInput(now)
	in.Snap.PerpBook.MidPriceQuote = 0
	if c := check(t, assessEntry(in).EntryChecks, "Basis lúc vào"); c.Evaluated || c.Passed {
		t.Errorf("an unmeasurable basis = %+v", c)
	}
}

// Trụ cột 2: inside the amortization floor the funding exit is forbidden
// whatever a settlement did — and the take-profit and the basis stop are not.
func TestAssessExit_TheAmortizationFloorHoldsThroughNegativeSettlements(t *testing.T) {
	now := time.Now()
	pos := heldPosition(now)
	cfg := DefaultConfig(testSymbol) // MinHoldEpochs 6
	base := *goodSnapshot(now, 0.0001)

	// Five settlements after the open, every one of them a deep charge: still
	// inside the floor, so nothing closes.
	charged := base
	for i := 0; i < 5; i++ {
		charged.Settled = append(charged.Settled, SettledRate{
			SettledAtMs: now.Add(time.Duration(i) * time.Minute).UnixMilli(), RatePerIntervalFrac: -0.001, // -10 bps
		})
	}
	ex := assessExit(cfg, charged, pos, now)
	if ex.Due || ex.SettlementsSinceOpen != 5 {
		t.Fatalf("the floor let a pair go at 5 of 6 settlements: %+v", ex)
	}
	if c := check(t, ex.Checks, "Mốc settle sau khi vào"); !c.Evaluated || !c.Passed || !strings.Contains(c.DetailVI, "sàn giữ tối thiểu 6 mốc") {
		t.Errorf("funding check inside the floor = %+v", c)
	}
	// The sixth settlement reaches the floor, and the run of charges is past
	// the hysteresis, so now it closes.
	charged.Settled = append(charged.Settled, SettledRate{SettledAtMs: now.Add(6 * time.Minute).UnixMilli(), RatePerIntervalFrac: -0.001})
	if ex := assessExit(cfg, charged, pos, now); !ex.Due {
		t.Errorf("the 6th settlement did not release the funding exit: %+v", ex)
	}

	// Inside the floor a BASIS break still closes the pair: the floor buys time
	// for funding to amortize a cost, it is not a promise to hold through a
	// broken hedge.
	broken := base
	broken.Settled = append(broken.Settled, SettledRate{SettledAtMs: now.UnixMilli(), RatePerIntervalFrac: 0.0001})
	broken.PerpBook = book("binance_futures", testMid*1.0102, now) // +102 bps against an entry basis of +1
	if ex := assessExit(cfg, broken, pos, now); !ex.Due || !strings.Contains(strings.Join(ex.ReasonsVI, " "), "Cắt lỗ basis nổ") {
		t.Errorf("a basis break inside the floor did not close: %+v", ex)
	}
}

// Trụ cột 4: past the floor, one charge is not a reason to pay a round trip.
func TestAssessExit_NegativeFundingNeedsARunPastTheFloor(t *testing.T) {
	now := time.Now()
	pos := heldPosition(now)
	cfg := DefaultConfig(testSymbol)
	cfg.MinHoldEpochs = 1 // past the floor from the first settlement on

	after := func(rates ...float64) Snapshot {
		s := *goodSnapshot(now, 0.0001)
		for i, r := range rates {
			s.Settled = append(s.Settled, SettledRate{SettledAtMs: now.Add(time.Duration(i) * time.Minute).UnixMilli(), RatePerIntervalFrac: r})
		}
		return s
	}
	const deep, shallow, paid = -0.0003, -0.00001, 0.0001 // -3 bps, -0.1 bps, +1 bps

	for _, tc := range []struct {
		nameVI  string
		rates   []float64
		wantDue bool
	}{
		{"một mốc âm sâu", []float64{deep}, false},
		{"hai mốc âm sâu liên tiếp", []float64{deep, deep}, true},
		{"hai mốc âm sâu bị cắt quãng", []float64{deep, paid, deep}, false},
		{"ba mốc âm li ti", []float64{shallow, shallow, shallow}, false},
		{"âm li ti rồi hai mốc sâu", []float64{shallow, deep, deep}, true},
	} {
		ex := assessExit(cfg, after(tc.rates...), pos, now)
		if ex.Due != tc.wantDue {
			t.Errorf("%s: due = %v, want %v · %v", tc.nameVI, ex.Due, tc.wantDue, ex.ReasonsVI)
		}
	}

	// The two thresholds are parameters, not constants: one print at the
	// configured rate closes a pair configured to accept one.
	prompt := cfg
	prompt.ExitNegativeConsecutiveEpochs = 1
	if ex := assessExit(prompt, after(deep), pos, now); !ex.Due {
		t.Errorf("a one-epoch gate held through a charge: %+v", ex)
	}
	// And a deeper threshold ignores the same run.
	patient := cfg
	patient.ExitNegativeFundingRateBps = -50
	if ex := assessExit(patient, after(deep, deep), pos, now); ex.Due {
		t.Errorf("a -50 bps gate closed on -3 bps: %+v", ex)
	}
	// MinHoldEpochs 0 is the step-3.2 rule exactly: ANY settlement at or below
	// zero, no run required. Pinned so a run configured the old way is not
	// quietly moved by this change.
	legacy := cfg
	legacy.MinHoldEpochs = 0
	if ex := assessExit(legacy, after(shallow), pos, now); !ex.Due || !strings.Contains(strings.Join(ex.ReasonsVI, " "), "≤ 0") {
		t.Errorf("the legacy single-print rule = %+v", ex)
	}
}

// Trụ cột 3: a basis that converges pays the round trip, and the pair leaves
// before its funding ever could.
func TestAssessExit_TakesProfitWhenTheBasisConverges(t *testing.T) {
	now := time.Now()
	pos := heldPosition(now)
	cfg := DefaultConfig(testSymbol)
	base := *goodSnapshot(now, 0.0001)

	// Unchanged market: the pair is under water by its own entry commission and
	// the exit it has not paid yet, and holds.
	if ex := assessExit(cfg, base, pos, now); ex.Due || !ex.Result.OK || ex.Result.ReturnOnCapitalPct >= 0 {
		t.Fatalf("a pair that has earned nothing = %+v", ex)
	}

	// The perp falls 2.8% towards (and past) the spot: the SHORT leg gains it.
	// The fixture tracks the shipped target, which is now +1.50% on capital —
	// measured on this fixture the move converts at roughly 0.71 points of
	// capital per percent of perp, less a fixed ~0.06 for the round trip.
	converged := base
	converged.PerpBook = book("binance_futures", testMid*0.972, now)
	ex := assessExit(cfg, converged, pos, now)
	if !ex.Due {
		t.Fatalf("a converged basis did not take profit: %+v", ex)
	}
	r := ex.Result
	if !r.OK || r.ReturnOnCapitalPct < cfg.TargetTakeProfitNetPct {
		t.Fatalf("running result = %+v", r)
	}
	// The reason is the sentence the intent file keeps and the history table
	// prints, to the wording.
	want := fmt.Sprintf("Chốt lời hội tụ Basis: Net PnL %+.2f%% trên vốn ≥ ngưỡng %+.2f%%", r.ReturnOnCapitalPct, cfg.TargetTakeProfitNetPct)
	if got := strings.Join(ex.ReasonsVI, "; "); got != want {
		t.Errorf("close reason = %q, want %q", got, want)
	}
	// It fires INSIDE the amortization floor — that is the point of holding a
	// convergence trade rather than a funding trade.
	if ex.SettlementsSinceOpen >= cfg.MinHoldEpochs {
		t.Errorf("the fixture left the floor: %d settlements", ex.SettlementsSinceOpen)
	}

	// 0 disables it: the same reading holds.
	off := cfg
	off.TargetTakeProfitNetPct = 0
	if ex := assessExit(off, converged, pos, now); ex.Due {
		t.Errorf("a disabled take-profit still closed: %+v", ex)
	}
	// A book old enough to describe a market that has moved prices nothing: the
	// take-profit is the only exit that closes a position for a GAIN, and a
	// phantom drift would pay a real round trip for it.
	stale := converged
	stale.PerpBook.SampledAtMs = now.Add(-2 * maxBookAge).UnixMilli()
	if ex := assessExit(cfg, stale, pos, now); ex.Due || ex.Result.OK || !strings.Contains(ex.Result.ReasonVI, "không định giá chốt lời trên giá cũ") {
		t.Errorf("a stale book took profit: %+v", ex.Result)
	}
	// A book stamped in the FUTURE is the same fault seen from the other side —
	// a clock that disagrees is not a book that is fresh.
	ahead := converged
	ahead.SpotBook.SampledAtMs = now.Add(2 * maxBookAge).UnixMilli()
	if ex := assessExit(cfg, ahead, pos, now); ex.Due || ex.Result.OK {
		t.Errorf("a book from the future took profit: %+v", ex.Result)
	}
	// The BASIS STOP keeps working on that same reading: a stop that goes quiet
	// when a book ages is worse than one acting on a stale price.
	brokenStale := stale
	brokenStale.PerpBook = book("binance_futures", testMid*1.0102, now)
	brokenStale.PerpBook.SampledAtMs = now.Add(-2 * maxBookAge).UnixMilli()
	if ex := assessExit(cfg, brokenStale, pos, now); !ex.Due || !strings.Contains(strings.Join(ex.ReasonsVI, " "), "Cắt lỗ basis nổ") {
		t.Errorf("the basis stop went quiet on a stale book: %+v", ex)
	}

	// A reading that cannot be priced never closes anything.
	blind := converged
	blind.FeesErrVI = "timeout"
	if ex := assessExit(cfg, blind, pos, now); ex.Due || ex.Result.OK {
		t.Errorf("an unpriced reading closed the pair: %+v", ex)
	}
	if c := check(t, assessExit(cfg, blind, pos, now).Checks, "Chốt lời"); c.Evaluated || !c.Passed {
		t.Errorf("an unpriced take-profit check = %+v", c)
	}
}

// The arithmetic behind the take-profit, term by term. The trap it pins is the
// double count: the drift is measured from the FILL price, so the entry's
// slippage is already inside it and must not be subtracted again.
func TestPriceHolding_CountsEachTermOnceAndEntrySlippageOnlyThroughTheDrift(t *testing.T) {
	now := time.Now()
	const qty = 0.001
	snap := *goodSnapshot(now, 0.0001)
	snap.SpotTakerFeeBps, snap.PerpTakerFeeBps = 10, 4
	snap.SpotBook = book("binance_spot", 100_000, now)
	snap.PerpBook = book("binance_futures", 100_000, now)

	// Both legs filled 50 quote WORSE than the mids they were decided on: the
	// spot bought high, the perp sold low. That is 100 quote of entry slippage
	// on a 1-coin position, and it shows up once — as drift.
	pos := PositionView{IntentID: "a", OpenedAtMs: now.Add(-time.Hour).UnixMilli(), QtyCoin: qty,
		NotionalQuote: qty * 100_000, CapitalQuote: qty * 100_000 * 1.5,
		SpotEntryAvgQuote: 100_050, PerpEntryAvgQuote: 99_950}

	settled := []SettledRate{
		{SettledAtMs: now.Add(-30 * time.Minute).UnixMilli(), RatePerIntervalFrac: 0.0002},
		{SettledAtMs: now.Add(-10 * time.Minute).UnixMilli(), RatePerIntervalFrac: 0.0001},
	}
	got := priceHolding(snap, pos, settled, now)
	if !got.OK {
		t.Fatalf("refused: %s", got.ReasonVI)
	}
	// Funding: the two rates on the PERP leg's notional at entry.
	wantFunding := 0.0003 * qty * 99_950
	// Drift: (spot mid − spot fill) + (perp fill − perp mid), times the qty.
	wantDrift := (100_000-100_050)*qty + (99_950-100_000)*qty
	// Entry commission: each leg's own bps on its own entry notional.
	wantEntryFee := qty*100_050*10/10_000 + qty*99_950*4/10_000
	for _, tc := range []struct {
		nameVI    string
		got, want float64
	}{
		{"funding", got.FundingQuote, wantFunding},
		{"trôi giá", got.DriftQuote, wantDrift},
		{"phí vào", got.EntryFeeQuote, wantEntryFee},
	} {
		if math.Abs(tc.got-tc.want) > 1e-9 {
			t.Errorf("%s = %v, want %v", tc.nameVI, tc.got, tc.want)
		}
	}
	// The exit is priced at the legs' CURRENT value on the sides it would take,
	// and it is a cost — never a credit.
	if got.ExitCostQuote <= 0 {
		t.Errorf("exit cost = %v, want a positive charge", got.ExitCostQuote)
	}
	wantCash := wantFunding + wantDrift - wantEntryFee - got.ExitCostQuote
	if math.Abs(got.CashResultQuote-wantCash) > 1e-9 {
		t.Errorf("cash result = %v, want %v — the four terms and nothing else", got.CashResultQuote, wantCash)
	}
	if math.Abs(got.ReturnOnCapitalPct-wantCash/pos.CapitalQuote*100) > 1e-9 || got.CapitalQuote != pos.CapitalQuote {
		t.Errorf("on capital = %v%% of %v", got.ReturnOnCapitalPct, got.CapitalQuote)
	}
	// Only settlements handed in count: rule 6 is counted crossings, and the
	// caller has already filtered to those after the open.
	if none := priceHolding(snap, pos, nil, now); none.FundingQuote != 0 {
		t.Errorf("funding with no settlement = %v", none.FundingQuote)
	}

	// Every missing input refuses rather than pricing it as zero.
	for _, tc := range []struct {
		nameVI string
		break_ func(*Snapshot, *PositionView)
	}{
		{"phí chưa đọc", func(s *Snapshot, _ *PositionView) { s.FeesErrVI = "timeout" }},
		{"lịch sử funding chưa đọc", func(s *Snapshot, _ *PositionView) { s.SettledErrVI = "timeout" }},
		{"không có khối lượng", func(_ *Snapshot, p *PositionView) { p.QtyCoin = 0 }},
		{"không có giá khớp vào", func(_ *Snapshot, p *PositionView) { p.PerpEntryAvgQuote = 0 }},
		{"không có giá giữa", func(s *Snapshot, _ *PositionView) { s.SpotBook.MidPriceQuote = 0 }},
		{"không biết vốn", func(_ *Snapshot, p *PositionView) { p.CapitalQuote = 0 }},
		{"sổ lệnh không hấp thụ nổi", func(s *Snapshot, _ *PositionView) {
			s.SpotBook.BidDepthWithinTightQuote, s.SpotBook.BidDepthWithinWideQuote = 0, 0
			s.SpotBook.BidLevels, s.SpotBook.BidSpanPct = 0, 0
		}},
	} {
		brokenSnap, brokenPos := snap, pos
		tc.break_(&brokenSnap, &brokenPos)
		r := priceHolding(brokenSnap, brokenPos, settled, now)
		if r.OK || r.ReasonVI == "" {
			t.Errorf("%s: priced anyway = %+v", tc.nameVI, r)
		}
	}
}

// A configuration no run may start with. Each of the four new knobs is refused
// for the reason a person would get it wrong.
func TestConfig_RefusesTheWaysTheNewThresholdsGoWrong(t *testing.T) {
	for _, tc := range []struct {
		nameVI   string
		mutate   func(*Config)
		fragment string
	}{
		{"ngưỡng basis vô hạn", func(c *Config) { c.MinEntryBasisBps = math.Inf(1) }, "min_entry_basis_bps"},
		{"ngưỡng basis vô lý", func(c *Config) { c.MinEntryBasisBps = 20_000 }, "min_entry_basis_bps"},
		{"sàn giữ âm", func(c *Config) { c.MinHoldEpochs = -1 }, "min_hold_epochs"},
		{"sàn giữ ≥ trần giữ", func(c *Config) { c.MaxHoldEpochs, c.MinHoldEpochs = 6, 6 }, "lối thoát funding không bao giờ chạy được"},
		{"chốt lời âm", func(c *Config) { c.TargetTakeProfitNetPct = -1 }, "target_take_profit_net_pct"},
		{"chốt lời không bao giờ tới", func(c *Config) { c.TargetTakeProfitNetPct = 500 }, "target_take_profit_net_pct"},
		{"ngưỡng thoát âm lại dương", func(c *Config) { c.ExitNegativeFundingRateBps = 2 }, "exit_negative_funding_rate_bps"},
		{"số mốc âm liên tiếp bằng 0", func(c *Config) { c.ExitNegativeConsecutiveEpochs = 0 }, "exit_negative_consecutive_epochs"},
		{"trần spread âm", func(c *Config) { c.MaxExitSpreadBps = -1 }, "max_exit_spread_bps"},
		{"trần spread vô hạn", func(c *Config) { c.MaxExitSpreadBps = math.Inf(1) }, "max_exit_spread_bps"},
		{"trần spread vô lý", func(c *Config) { c.MaxExitSpreadBps = 20_000 }, "max_exit_spread_bps"},
	} {
		c := DefaultConfig(testSymbol)
		tc.mutate(&c)
		err := c.Validate(50_000)
		if err == nil {
			t.Errorf("%s: accepted", tc.nameVI)
			continue
		}
		if !strings.Contains(err.Error(), tc.fragment) {
			t.Errorf("%s: %v does not name %q", tc.nameVI, err, tc.fragment)
		}
	}
	// A floor BELOW the ceiling is fine, and so is the shipped set.
	ok := DefaultConfig(testSymbol)
	ok.MaxHoldEpochs, ok.MinHoldEpochs = 12, 6
	if err := ok.Validate(50_000); err != nil {
		t.Errorf("a floor under a ceiling was refused: %v", err)
	}
	if err := DefaultConfig(testSymbol).Validate(50_000); err != nil {
		t.Errorf("the shipped set was refused: %v", err)
	}
}

// ------------------------- buffered-slot sizing and rebalancing (4.5g)

// The arithmetic of one slot, term by term, against the brief's own formula:
// hold the buffer back, split what is left N ways, divide by the capital a
// quote of notional ties up.
func TestPlanNotional_SizesASlotFromEquity(t *testing.T) {
	in := notionalPlanInput{
		// Split in exactly the ratio the position needs — spot 1, futures 0.5 —
		// so the brief's one-pool formula and the two wallets agree, and the
		// test is about the formula rather than about which wallet binds.
		Account: Account{QuoteAsset: "USDT", SpotQuoteTotal: 10_000, FuturesQuoteTotal: 5_000},
		Slots:   6, MarginFrac: 0.5, BufferPct: 0.30,
		CapQuote: 1_000_000, MaxNotionalQuote: 50_000,
	}
	got := planNotional(in)
	if !got.OK {
		t.Fatalf("refused: %s", got.ReasonVI)
	}
	// 15,000 × 0.70 = 10,500 tradable; ÷ 6 = 1,750 a slot; ÷ 1.5 = 1,166.67.
	for _, tc := range []struct {
		nameVI    string
		got, want float64
	}{
		{"tổng vốn", got.TotalEquityQuote, 15_000},
		{"đệm", got.BufferQuote, 4_500},
		{"vốn được phân bổ", got.TradableQuote, 10_500},
		{"vốn mỗi chỗ", got.CapitalPerSlotQuote, 1_750},
		{"notional", got.NotionalQuote, 10_500.0 / 6 / 1.5},
		{"vốn trên mỗi notional", got.CapitalPerNotional, 1.5},
	} {
		if math.Abs(tc.got-tc.want) > 1e-9 {
			t.Errorf("%s = %v, want %v", tc.nameVI, tc.got, tc.want)
		}
	}
	// The whole plan fits both wallets: 6 × 1,166.67 = 7,000 of spot against
	// 10,000, and 6 × 0.5 × 1,166.67 = 3,500 of margin against 5,000 × 0.70.
	if !strings.Contains(got.BoundByVI, "phần vốn mỗi chỗ") {
		t.Errorf("bound by %q, want the equity share", got.BoundByVI)
	}

	// The bot's own open positions are spot equity held in coin. Leaving them
	// out would shrink every plan as positions open and grow it as they close.
	deployed := in
	deployed.Account.SpotQuoteTotal, deployed.OpenSpotValueQuote = 3_000, 7_000
	if same := planNotional(deployed); !same.OK || math.Abs(same.NotionalQuote-got.NotionalQuote) > 1e-9 {
		t.Errorf("a fully deployed account sized %v, want the same %v — the plan ratchets", same.NotionalQuote, got.NotionalQuote)
	}
}

// The two wallets are separate registrations and nothing can move funds
// between them, so whichever runs out first sets the size — and says so.
func TestPlanNotional_TheTighterWalletBindsAndIsNamed(t *testing.T) {
	base := notionalPlanInput{Slots: 4, MarginFrac: 0.5, BufferPct: 0.30, CapQuote: 1_000_000, MaxNotionalQuote: 50_000}
	for _, tc := range []struct {
		nameVI       string
		spot, perp   float64
		wantNotional float64
		wantBound    string
	}{
		// 10,000 spot / 4 = 2,500 a leg, against an equity share of
		// (10,000 + 500) × 0.7 / 4 / 1.5 = 1,225 — the share still binds.
		{"hai ví cân đối", 10_000, 5_000, 15_000 * 0.7 / 4 / 1.5, "phần vốn mỗi chỗ"},
		// Almost nothing on the futures side: its margin, not the share, binds.
		{"ví futures cạn", 10_000, 300, 300 * 0.7 / (4 * 0.5), "ví futures"},
		// Almost nothing on the spot side: the spot leg is the whole notional.
		{"ví spot cạn", 400, 9_000, 400.0 / 4, "ví spot"},
	} {
		in := base
		in.Account = Account{QuoteAsset: "USDT", SpotQuoteTotal: tc.spot, FuturesQuoteTotal: tc.perp}
		got := planNotional(in)
		if !got.OK {
			t.Errorf("%s: refused: %s", tc.nameVI, got.ReasonVI)
			continue
		}
		if math.Abs(got.NotionalQuote-tc.wantNotional) > 1e-9 || !strings.Contains(got.BoundByVI, tc.wantBound) {
			t.Errorf("%s: notional %v (want %v), bound by %q (want %q)", tc.nameVI, got.NotionalQuote, tc.wantNotional, got.BoundByVI, tc.wantBound)
		}
		// Whatever bound it, the plan must fit BOTH wallets. This is the whole
		// point: a size the futures wallet cannot margin is an open that fails.
		if slots := float64(in.Slots); got.NotionalQuote*slots > got.SpotPoolQuote+1e-9 ||
			got.NotionalQuote*slots*in.MarginFrac > got.FuturesPoolQuote*(1-in.BufferPct)+1e-9 {
			t.Errorf("%s: %d × %.2f does not fit spot %.2f / futures %.2f", tc.nameVI, in.Slots, got.NotionalQuote, got.SpotPoolQuote, got.FuturesPoolQuote)
		}
	}
}

// The two hard ceilings still hold however much equity there is.
func TestPlanNotional_NeverExceedsTheCapitalCapOrThePortalCeiling(t *testing.T) {
	rich := notionalPlanInput{
		Account: Account{QuoteAsset: "USDT", SpotQuoteTotal: 10_000_000, FuturesQuoteTotal: 10_000_000},
		Slots:   4, MarginFrac: 0.5, BufferPct: 0.30, CapQuote: 1_200, MaxNotionalQuote: 50_000,
	}
	got := planNotional(rich)
	// 1,200 of capital over 4 slots at 1.5× is 200 a leg, and not one quote more.
	if !got.OK || math.Abs(got.NotionalQuote-200) > 1e-9 || !strings.Contains(got.BoundByVI, "hạn mức vốn") {
		t.Fatalf("capital cap = %+v", got)
	}
	if total := got.NotionalQuote * float64(rich.Slots) * got.CapitalPerNotional; total > rich.CapQuote+1e-9 {
		t.Errorf("%d slots tie up %.2f, over the cap %.2f", rich.Slots, total, rich.CapQuote)
	}
	// With the cap lifted, the portal's own per-leg ceiling is the last word.
	rich.CapQuote = 1e12
	rich.MaxNotionalQuote = 5_000
	if got := planNotional(rich); !got.OK || got.NotionalQuote != 5_000 || !strings.Contains(got.BoundByVI, "trần notional") {
		t.Errorf("portal ceiling = %+v", got)
	}
}

// An input that cannot be trusted produces NO size, so the caller leaves the
// one it has rather than trading on a number nobody can defend.
func TestPlanNotional_RefusesRatherThanSizingOnNonsense(t *testing.T) {
	ok := notionalPlanInput{
		Account: Account{QuoteAsset: "USDT", SpotQuoteTotal: 10_000, FuturesQuoteTotal: 5_000},
		Slots:   4, MarginFrac: 0.5, BufferPct: 0.30, CapQuote: 100_000, MaxNotionalQuote: 50_000,
	}
	if got := planNotional(ok); !got.OK {
		t.Fatalf("the good input was refused: %s", got.ReasonVI)
	}
	for _, tc := range []struct {
		nameVI   string
		mutate   func(*notionalPlanInput)
		fragment string
	}{
		{"không có chỗ nào", func(in *notionalPlanInput) { in.Slots = 0 }, "số chỗ"},
		{"ký quỹ bằng 0", func(in *notionalPlanInput) { in.MarginFrac = 0 }, "tỷ lệ ký quỹ"},
		{"đệm quá thấp", func(in *notionalPlanInput) { in.BufferPct = 0.05 }, "đệm ký quỹ"},
		{"đệm quá cao", func(in *notionalPlanInput) { in.BufferPct = 0.9 }, "đệm ký quỹ"},
		{"đệm NaN", func(in *notionalPlanInput) { in.BufferPct = math.NaN() }, "đệm ký quỹ"},
		{"số dư âm", func(in *notionalPlanInput) { in.Account.SpotQuoteTotal = -1 }, "số dư đọc được"},
		{"số dư vô hạn", func(in *notionalPlanInput) { in.Account.FuturesQuoteTotal = math.Inf(1) }, "số dư đọc được"},
		{"vị thế đang giữ âm", func(in *notionalPlanInput) { in.OpenSpotValueQuote = -1 }, "chân spot đang giữ"},
		{"không có hạn mức vốn", func(in *notionalPlanInput) { in.CapQuote = 0 }, "hạn mức vốn"},
		{"tài khoản rỗng", func(in *notionalPlanInput) { in.Account = Account{} }, "vốn không đủ"},
	} {
		in := ok
		tc.mutate(&in)
		got := planNotional(in)
		if got.OK || got.NotionalQuote != 0 {
			t.Errorf("%s: sized anyway = %+v", tc.nameVI, got)
			continue
		}
		if !strings.Contains(got.ReasonVI, tc.fragment) {
			t.Errorf("%s: %q does not name %q", tc.nameVI, got.ReasonVI, tc.fragment)
		}
	}
}

// The venue's own floor on one leg, and the guard that keeps a slot from being
// mostly rounding error.
func TestSizeFloorQuote_ReadsTheVenuesRulesAndTheQuantizationGuard(t *testing.T) {
	// BTCUSDT as this testnet publishes it: futures steps 0.0001 and wants 50
	// quote, at 77,000 a coin. The step is 7.70, so the 5% tolerance asks 154 —
	// three times the venue's own minimum, and it is the binding one.
	floor, why, ok := sizeFloorQuote(0.0001, 0.0001, 50, 77_000)
	if !ok || math.Abs(floor-154) > 1e-9 || !strings.Contains(why, "bước nhảy") {
		t.Errorf("BTC floor = %v (%s, ok %v), want 154 from the step", floor, why, ok)
	}
	// A cheap coin with a coarse step: the venue's own minimum notional wins.
	if floor, why, ok := sizeFloorQuote(1, 1, 20, 0.15); !ok || math.Abs(floor-20) > 1e-9 || !strings.Contains(why, "notional tối thiểu") {
		t.Errorf("cheap coin floor = %v (%s, ok %v), want the venue's 20", floor, why, ok)
	}
	// A minimum quantity worth more than either: it wins.
	if floor, why, ok := sizeFloorQuote(0.001, 10, 5, 100); !ok || math.Abs(floor-1_000) > 1e-9 || !strings.Contains(why, "lượng tối thiểu") {
		t.Errorf("min-qty floor = %v (%s, ok %v), want 1000", floor, why, ok)
	}
	// An unread rule is NOT "no limit": the floor is unknown, and the entry
	// check that reads it refuses rather than opening an order the venue bins.
	for _, tc := range []struct {
		nameVI                           string
		step, minQty, minNotional, price float64
	}{
		{"chưa có giá", 0.0001, 0.0001, 50, 0},
		{"chưa đọc bước nhảy", 0, 0.0001, 50, 77_000},
		{"chưa đọc lượng tối thiểu", 0.0001, 0, 50, 77_000},
		{"chưa đọc notional tối thiểu", 0.0001, 0.0001, 0, 77_000},
	} {
		if floor, why, ok := sizeFloorQuote(tc.step, tc.minQty, tc.minNotional, tc.price); ok || floor != 0 || why == "" {
			t.Errorf("%s: floor %v (%s, ok %v) — an unread rule must not read as no limit", tc.nameVI, floor, why, ok)
		}
	}
}

// A size the venue would refuse, or one mostly lost to its grid, never opens.
// Both cost real money: the first is an order the portal refuses before
// placing, which the engine counts as a failed trade and halts the pair after
// five; the second opens and deploys far less of its slot than the allocation
// says (capital.go).
func TestAssessEntry_RefusesASizeTheVenueGridWouldEat(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		nameVI     string
		notional   float64
		wantPassed bool
	}{
		{"đúng bằng sàn quy mô", 154, true},
		{"trên sàn quy mô", 200, true},
		{"quy mô cũ 65 quote", 65, false},
		{"dưới notional tối thiểu của sàn", 40, false},
	} {
		in := goodInput(now)
		in.Cfg.NotionalQuote = tc.notional
		c := check(t, assessEntry(in).EntryChecks, "Quy mô đủ lớn")
		if !c.Evaluated || c.Passed != tc.wantPassed {
			t.Errorf("%s: size check = %+v, want passed=%v", tc.nameVI, c, tc.wantPassed)
		}
	}
	// The gauge says what the size really becomes on the grid, whether or not
	// the check passes: 200 quote at 77,000 and a 0.0001 step is 0.0025 coin.
	sig := assessEntry(goodInput(now))
	if sig.PlannedQtyCoin == nil || math.Abs(*sig.PlannedQtyCoin-0.0025) > 1e-12 ||
		sig.SizeErrorPct == nil || *sig.SizeErrorPct < 0 || *sig.SizeErrorPct > 5 {
		t.Errorf("planned qty %v, size error %v%%", sig.PlannedQtyCoin, sig.SizeErrorPct)
	}
	if sig.SizeFloorQuote == nil || math.Abs(*sig.SizeFloorQuote-154) > 1e-9 {
		t.Errorf("size floor on the gauge = %v", sig.SizeFloorQuote)
	}
	// Rules that could not be read are not evaluated, and an entry check that
	// could not be evaluated never passes.
	blind := goodInput(now)
	blind.Snap.RulesErrVI = "timeout"
	if c := check(t, assessEntry(blind).EntryChecks, "Quy mô đủ lớn"); c.Evaluated || c.Passed {
		t.Errorf("unread rules = %+v", c)
	}
	if assessEntry(blind).EntryEligible {
		t.Error("a pair with unread venue rules was eligible")
	}
}

// The schedule, as a value: when a size read is owed and when it is not.
func TestRebalanceDue_FollowsTheClockAndTheSwitch(t *testing.T) {
	const hourMs = int64(3_600_000)
	base := DefaultPortfolioConfig([]string{testSymbol})
	base.RebalanceIntervalHours = 168

	// Never sized: due at once, so a bot switched on does not trade a week at
	// the seed before it first looks at the account.
	if !base.rebalanceDue(1_000) {
		t.Error("a run that never sized is not due")
	}
	sized := base
	sized.LastRebalancedAtMs = 1_000_000
	for _, tc := range []struct {
		nameVI  string
		atMs    int64
		wantDue bool
	}{
		{"ngay sau khi cân bằng", sized.LastRebalancedAtMs + 1, false},
		{"một giờ trước hạn", sized.LastRebalancedAtMs + 167*hourMs, false},
		{"đúng hạn", sized.LastRebalancedAtMs + 168*hourMs, true},
		{"quá hạn", sized.LastRebalancedAtMs + 400*hourMs, true},
	} {
		if got := sized.rebalanceDue(tc.atMs); got != tc.wantDue {
			t.Errorf("%s: due = %v, want %v", tc.nameVI, got, tc.wantDue)
		}
	}
	if want := sized.LastRebalancedAtMs + 168*hourMs; sized.nextRebalanceAtMs() != want {
		t.Errorf("next = %d, want %d", sized.nextRebalanceAtMs(), want)
	}
	// The switch off means never, however long it has been.
	off := sized
	off.AutoRebalance = false
	if off.rebalanceDue(sized.LastRebalancedAtMs+10_000*hourMs) || off.nextRebalanceAtMs() != 0 {
		t.Errorf("a switched-off run is due at %d", off.nextRebalanceAtMs())
	}
	offNever := off
	offNever.LastRebalancedAtMs = 0
	if offNever.rebalanceDue(1) {
		t.Error("a switched-off run that never sized is due")
	}
}

// A run no start may begin with, for each of the two new values.
func TestPortfolio_RefusesTheWaysTheRebalanceValuesGoWrong(t *testing.T) {
	for _, tc := range []struct {
		nameVI   string
		mutate   func(*PortfolioConfig)
		fragment string
	}{
		{"đệm quá thấp", func(pc *PortfolioConfig) { pc.MarginBufferPct = 0.05 }, "margin_buffer_pct"},
		{"đệm quá cao", func(pc *PortfolioConfig) { pc.MarginBufferPct = 0.6 }, "margin_buffer_pct"},
		{"đệm NaN", func(pc *PortfolioConfig) { pc.MarginBufferPct = math.NaN() }, "margin_buffer_pct"},
		{"chu kỳ quá ngắn", func(pc *PortfolioConfig) { pc.RebalanceIntervalHours = 0.5 }, "rebalance_interval_hours"},
		{"chu kỳ vô hạn", func(pc *PortfolioConfig) { pc.RebalanceIntervalHours = math.Inf(1) }, "rebalance_interval_hours"},
		{"mốc cân bằng âm", func(pc *PortfolioConfig) { pc.LastRebalancedAtMs = -1 }, "last_rebalanced_at_ms"},
		// Validated even with the switch off: a run started off with a nonsense
		// buffer would size wrongly the moment somebody turns it on.
		{"đệm sai khi đã tắt", func(pc *PortfolioConfig) { pc.AutoRebalance, pc.MarginBufferPct = false, 0 }, "margin_buffer_pct"},
	} {
		pc := DefaultPortfolioConfig([]string{testSymbol})
		tc.mutate(&pc)
		err := pc.Validate([]string{testSymbol}, 50_000, 1.5)
		if err == nil {
			t.Errorf("%s: accepted", tc.nameVI)
			continue
		}
		if !strings.Contains(err.Error(), tc.fragment) {
			t.Errorf("%s: %v does not name %q", tc.nameVI, err, tc.fragment)
		}
	}
	if err := DefaultPortfolioConfig([]string{testSymbol}).Validate([]string{testSymbol}, 50_000, 1.5); err != nil {
		t.Errorf("the shipped run was refused: %v", err)
	}
}

// widenTouch returns the same book with the same MID and a touch of exactly
// wantBps. The mid is held so that the pair's running result does not move:
// what is being tested is the brake, not a different profit.
func widenTouch(b depth.Summary, wantBps float64) depth.Summary {
	half := wantBps / 2 / bpsPerUnit
	b.BestBidQuote = b.MidPriceQuote * (1 - half)
	b.BestAskQuote = b.MidPriceQuote * (1 + half)
	b.SpreadPct = wantBps / 100
	return b
}

// Trụ cột 3's brake (4.5i): a reading that has cleared the take-profit target
// is HELD BACK while either book's touch is wider than MaxExitSpreadBps, and
// leaves on the first scan the book is tight again. The position is untouched
// in between — this defers an order, it does not cancel a decision.
func TestAssessExit_DefersTakeProfitOnWideSpread(t *testing.T) {
	now := time.Now()
	pos := heldPosition(now)
	cfg := DefaultConfig(testSymbol)

	// A convergence well past the +1.50% target.
	converged := *goodSnapshot(now, 0.0001)
	converged.PerpBook = book("binance_futures", testMid*0.972, now)
	if ex := assessExit(cfg, converged, pos, now); !ex.Due || ex.Result.ReturnOnCapitalPct < cfg.TargetTakeProfitNetPct {
		t.Fatalf("the fixture does not reach the target: %+v", ex.Result)
	}

	// Each leg on its own: either wide book defers, so one maker stepping away
	// is enough. 15 bps against a 10 bps ceiling.
	for _, leg := range []string{"spot", "perp"} {
		t.Run(leg+" giãn", func(t *testing.T) {
			wide := converged
			if leg == "spot" {
				wide.SpotBook = widenTouch(wide.SpotBook, 15)
			} else {
				wide.PerpBook = widenTouch(wide.PerpBook, 15)
			}
			ex := assessExit(cfg, wide, pos, now)
			if ex.Due {
				t.Fatalf("a wide %s book still sent the close: %v", leg, ex.ReasonsVI)
			}
			c := check(t, ex.Checks, "Chốt lời")
			if !strings.Contains(c.DetailVI, "HOÃN CHỐT: Spread bị giãn") {
				t.Errorf("the deferral does not say why: %q", c.DetailVI)
			}
			// The result itself is untouched: it is the ORDER that waits.
			if !ex.Result.OK || ex.Result.ReturnOnCapitalPct < cfg.TargetTakeProfitNetPct {
				t.Errorf("the deferral changed the running result: %+v", ex.Result)
			}
		})
	}

	// The book comes back: 1 bps, and the close goes on the very next reading.
	tight := converged
	tight.SpotBook = widenTouch(tight.SpotBook, 1)
	tight.PerpBook = widenTouch(tight.PerpBook, 1)
	ex := assessExit(cfg, tight, pos, now)
	if !ex.Due {
		t.Fatalf("a tight book did not release the take-profit: %+v", ex)
	}
	if got := strings.Join(ex.ReasonsVI, "; "); !strings.HasPrefix(got, "Chốt lời hội tụ Basis: Net PnL ") {
		t.Errorf("close reason = %q", got)
	}

	// And with the brake off, the SAME 15 bps touch closes: this is a knob, not
	// a wall, and 0 is the pre-4.5i behaviour exactly.
	//
	// 15 rather than something larger, because the running result already
	// prices part of a wide touch: strategy.EstimateFill charges the exit at
	// the book it is given, so widening both legs to 40 bps raises the priced
	// exit from 0.03 to 0.27 quote and drops this fixture from +1.22% to
	// +0.92% — under the target on its own merits, with the brake never
	// consulted. The brake is a SECOND line of defence over that pricing, not
	// the only one, and a test that confused the two would pass either way.
	off := cfg
	off.MaxExitSpreadBps = 0
	if ex := assessExit(off, widenBoth(converged, 15), pos, now); !ex.Due {
		t.Errorf("MaxExitSpreadBps 0 should disable the brake: %+v", ex.Checks)
	}
}

func widenBoth(s Snapshot, bps float64) Snapshot {
	s.SpotBook = widenTouch(s.SpotBook, bps)
	s.PerpBook = widenTouch(s.PerpBook, bps)
	return s
}

// The boundary that matters most: the brake is the TAKE-PROFIT's alone. A
// hedge that is breaking leaves into whatever book exists, because deferring a
// risk exit is how a small loss becomes a large one.
func TestAssessExit_WideSpreadDoesNotBlockRiskExits(t *testing.T) {
	now := time.Now()
	cfg := DefaultConfig(testSymbol)

	t.Run("cắt lỗ basis nổ", func(t *testing.T) {
		pos := heldPosition(now)
		blown := widenBoth(*goodSnapshot(now, 0.0001), 30)
		// The perp runs far ABOVE the spot: the basis widens past 100 bps.
		blown.PerpBook = widenTouch(book("binance_futures", testMid*1.05, now), 30)
		ex := assessExit(cfg, blown, pos, now)
		if !ex.Due {
			t.Fatalf("a 30 bps touch deferred the basis stop: %+v", ex.Checks)
		}
		if !strings.Contains(strings.Join(ex.ReasonsVI, "; "), "Cắt lỗ basis nổ") {
			t.Errorf("reasons = %v", ex.ReasonsVI)
		}
	})

	t.Run("thoát funding âm trễ", func(t *testing.T) {
		pos := heldPosition(now)
		snap := widenBoth(*goodSnapshot(now, 0.0001), 30)
		// Past the amortization floor, on a run of settlements below the floor.
		snap.Settled = settledEvery8h(21, 0.0001, now)
		for i := len(snap.Settled) - DefaultMinHoldEpochs; i < len(snap.Settled); i++ {
			snap.Settled[i].SettledAtMs = now.Add(time.Duration(i-len(snap.Settled)+1) * time.Minute).UnixMilli()
			snap.Settled[i].RatePerIntervalFrac = -0.0005
		}
		ex := assessExit(cfg, snap, pos2(pos, now), now)
		if !ex.Due {
			t.Fatalf("a 30 bps touch deferred the funding exit: %+v", ex.Checks)
		}
	})
}

// pos2 is heldPosition opened far enough back that every settlement in the
// fixture counts as "after the open".
func pos2(p PositionView, now time.Time) PositionView {
	p.OpenedAtMs = now.Add(-30 * 24 * time.Hour).UnixMilli()
	return p
}

// A Unified Trading Account (Bybit, PLAN 4.5j) is ONE wallet. The plan must
// size from it once: reading both markets and adding them doubles the equity,
// and bounding a "futures wallet" of 0 sizes every slot to nothing.
func TestPlanNotional_UnifiedWalletIsOnePool(t *testing.T) {
	in := notionalPlanInput{
		Account: Account{QuoteAsset: "USDT", SpotQuoteTotal: 15_000, UnifiedWallet: true},
		Slots:   6, MarginFrac: 0.5, BufferPct: 0.30, CapQuote: 1_000_000, MaxNotionalQuote: 50_000,
	}
	got := planNotional(in)
	if !got.OK {
		t.Fatalf("refused: %s", got.ReasonVI)
	}
	// 15,000 × 0.70 ÷ 6 ÷ 1.5 — the same slot two balanced Binance wallets
	// holding the same 15,000 IN TOTAL would get, never the 30,000 a double
	// count would see.
	if want := 15_000 * 0.7 / 6 / 1.5; math.Abs(got.NotionalQuote-want) > 1e-9 || got.TotalEquityQuote != 15_000 {
		t.Errorf("notional %v (want %v), equity %v (want 15000)", got.NotionalQuote, want, got.TotalEquityQuote)
	}
	if strings.Contains(got.BoundByVI, "ví futures") || strings.Contains(got.BoundByVI, "ví spot") {
		t.Errorf("a per-wallet bound was applied to one wallet: %q", got.BoundByVI)
	}
	// The whole plan fits the ONE wallet: spot legs plus margin.
	if used := got.NotionalQuote * float64(in.Slots) * got.CapitalPerNotional; used > 15_000*(1-in.BufferPct)+1e-9 {
		t.Errorf("%d slots tie up %.2f of a 15000 wallet with a 30%% buffer", in.Slots, used)
	}
	if line := got.logLineVI(in.Slots); !strings.Contains(line, "MỘT ví hợp nhất") {
		t.Errorf("console line does not say one wallet: %s", line)
	}
	// Coin the bot's positions hold is still equity, exactly as on two wallets.
	deployed := in
	deployed.Account.SpotQuoteTotal, deployed.OpenSpotValueQuote = 8_000, 7_000
	if same := planNotional(deployed); !same.OK || math.Abs(same.NotionalQuote-got.NotionalQuote) > 1e-9 {
		t.Errorf("deployed %v, want %v", same.NotionalQuote, got.NotionalQuote)
	}
	// A futures figure BESIDE a unified wallet is a double count by the reader.
	double := in
	double.Account.FuturesQuoteTotal = 15_000
	if got := planNotional(double); got.OK || !strings.Contains(got.ReasonVI, "hai lần") {
		t.Errorf("a unified wallet read twice was sized: %+v", got)
	}
}
