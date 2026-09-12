package main

import (
	"encoding/json"
	"strings"
	"testing"

	"futures-arbitrage-scanner/internal/backtest"
	"futures-arbitrage-scanner/internal/fees"
	"futures-arbitrage-scanner/internal/store"
	"futures-arbitrage-scanner/internal/strategy"
)

// --- the step-3.5 comparison, by machine (PLAN 3.5 ③ steps 1, 2, 4, 5) ---

const hour = int64(3600 * 1000)
const minute = int64(60 * 1000)

func checks(passed map[string]bool, details map[string]string) []strategy.Check {
	var out []strategy.Check
	for _, name := range []string{"hedge_leg", "history_depth", "rate_threshold", "persistence", "trailing_mean", "liquidity", "net_apr", "margin_known",
		"hedge_gone", "funding_negative", "net_apr_floor", "basis_widened", "margin_thin"} {
		if p, ok := passed[name]; ok {
			out = append(out, strategy.Check{Name: name, Passed: p, DetailVI: details[name]})
		}
	}
	return out
}

// liveRow builds a journal row; newestMs 0 leaves out the inputs (a row of
// the first run), and a positive cost sets net_apr_ok.
func liveRow(at int64, action strategy.Action, cs []strategy.Check, costPct float64, newestMs int64, params map[string]any) store.SignalRecord {
	type checkJSON struct {
		Name     string `json:"name"`
		Passed   bool   `json:"passed"`
		DetailVI string `json:"detail_vi"`
	}
	var cj []checkJSON
	for _, c := range cs {
		cj = append(cj, checkJSON{c.Name, c.Passed, c.DetailVI})
	}
	cb, _ := json.Marshal(cj)
	if params == nil {
		params = map[string]any{}
	}
	if newestMs != 0 {
		params["inputs"] = map[string]any{"newest_settled_at_ms": newestMs, "settled_rows": 100}
	}
	pb, _ := json.Marshal(params)
	return store.SignalRecord{EvaluatedAtMs: at, Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot",
		Action: string(action), CostTotalPct: costPct, NetAPROK: costPct > 0, ChecksJSON: string(cb), ParamsJSON: string(pb)}
}

func btAt(at int64, holding bool, action strategy.Action, cs []strategy.Check, costPct float64, newestMs int64) backtest.DecisionAt {
	return backtest.DecisionAt{AtMs: at, Holding: holding, Decision: strategy.Decision{
		Symbol: "BTCUSDT", PerpSource: "binance_futures", Action: action, Checks: cs,
		Cost: strategy.RoundTrip{OK: true, TotalPct: costPct}, NewestSettledAtMs: newestMs}}
}

var entryOK = map[string]bool{"hedge_leg": true, "history_depth": true, "rate_threshold": true, "persistence": true, "trailing_mean": true, "liquidity": true, "net_apr": true, "margin_known": true}
var exitHold = map[string]bool{"hedge_gone": false, "funding_negative": false, "net_apr_floor": false, "basis_widened": false, "margin_thin": false}

func with(m map[string]bool, k string, v bool) map[string]bool {
	out := map[string]bool{}
	for kk, vv := range m {
		out[kk] = vv
	}
	out[k] = v
	return out
}

func TestClassify_SameActionIsAMatch(t *testing.T) {
	bt := btAt(1000*hour, false, strategy.ActionEnter, checks(entryOK, nil), 0.30, 1000*hour)
	live := liveRow(1000*hour+33*minute, strategy.ActionEnter, checks(entryOK, nil), 0.31, 1000*hour, nil)
	if b, _ := classify(bt, live); b != bucketMatch {
		t.Errorf("same action must be (a), got %s", b)
	}
}

// (b) book: the live book priced the round trip differently, so net_apr
// (and liquidity) verdicts differ — the ONE input that differs is the book.
func TestClassify_NetAPRDifferenceWithADifferentCostIsTheBook(t *testing.T) {
	bt := btAt(1000*hour, false, strategy.ActionSkip, checks(with(entryOK, "net_apr", false), nil), 0.35, 1000*hour)
	live := liveRow(1000*hour+33*minute, strategy.ActionEnter, checks(entryOK, nil), 0.28, 1000*hour, nil)
	if b, why := classify(bt, live); b != bucketBook {
		t.Errorf("want (b) book, got %s: %s", b, why)
	}
	// The same verdicts with the SAME cost cannot be the book's doing.
	live = liveRow(1000*hour+33*minute, strategy.ActionEnter, checks(entryOK, nil), 0.35, 1000*hour, nil)
	if b, why := classify(bt, live); b != bucketUnexplained || !strings.Contains(why, "cùng chi phí") {
		t.Errorf("net_apr differs at the same cost → (c), got %s: %s", b, why)
	}
}

// (b) basis: only the live side can evaluate the basis exit; the replay
// reports it not evaluated (passed=false in the journal's shape).
func TestClassify_BasisExitOnlyLiveCanSeeIsTheBasis(t *testing.T) {
	bt := btAt(1000*hour, true, strategy.ActionHold, checks(exitHold, nil), 0.30, 1000*hour)
	live := liveRow(1000*hour+33*minute, strategy.ActionExit, checks(with(exitHold, "basis_widened", true), nil), 0.30, 1000*hour, nil)
	if b, why := classify(bt, live); b != bucketBasis {
		t.Errorf("want (b) basis, got %s: %s", b, why)
	}
}

// (b) history: the live side's newest stamp is older, and every differing
// check is one the newest print moves.
func TestClassify_OlderNewestStampOnTheLiveSideIsHistory(t *testing.T) {
	bt := btAt(1000*hour, false, strategy.ActionEnter, checks(entryOK, nil), 0.30, 1000*hour)
	live := liveRow(1000*hour+5*minute, strategy.ActionSkip, checks(with(with(entryOK, "persistence", false), "rate_threshold", false), nil), 0.30, 992*hour, nil)
	if b, why := classify(bt, live); b != bucketHistory {
		t.Errorf("want (b) history, got %s: %s", b, why)
	}
	// Same newest stamp, persistence differs anyway: the rules drifted.
	live = liveRow(1000*hour+33*minute, strategy.ActionSkip, checks(with(entryOK, "persistence", false), nil), 0.30, 1000*hour, nil)
	if b, why := classify(bt, live); b != bucketUnexplained || !strings.Contains(why, "cùng lịch sử") {
		t.Errorf("same history, different persistence verdict → (c), got %s: %s", b, why)
	}
	// A row without inputs cannot claim history either way — and says so.
	live = liveRow(1000*hour+33*minute, strategy.ActionSkip, checks(with(entryOK, "persistence", false), nil), 0.30, 0, nil)
	if b, why := classify(bt, live); b != bucketUnexplained || !strings.Contains(why, "không rõ") {
		t.Errorf("history unknown must be named, not asserted: %s: %s", b, why)
	}
}

// One input that covers EVERY differing check explains the pair even when a
// second input also differs (the live book differs by a hair at every tick):
// history covering net_apr + persistence wins over a book cause that would
// cover only net_apr. Two inputs each covering a part is (c).
func TestClassify_OneCoveringCauseWinsAPartialOneIsUnexplained(t *testing.T) {
	bt := btAt(1000*hour, false, strategy.ActionEnter, checks(entryOK, nil), 0.3013, 1000*hour)
	live := liveRow(1000*hour+5*minute, strategy.ActionSkip, checks(with(with(entryOK, "persistence", false), "net_apr", false), nil), 0.3012, 992*hour, nil)
	if b, why := classify(bt, live); b != bucketHistory {
		t.Errorf("history covers both diffs; the hair of cost is not a second cause: %s: %s", b, why)
	}
	bt = btAt(1000*hour, true, strategy.ActionHold, checks(exitHold, nil), 0.30, 1000*hour)
	live = liveRow(1000*hour+33*minute, strategy.ActionExit, checks(with(with(exitHold, "basis_widened", true), "net_apr_floor", true), nil), 0.25, 1000*hour, nil)
	b, why := classify(bt, live)
	if b != bucketUnexplained || !strings.Contains(why, "basis") || !strings.Contains(why, "sổ") {
		t.Errorf("want (c) naming both partial causes, got %s: %s", b, why)
	}
}

// funding_negative depends on the priced cost (the C gate, the M floor), so
// a cost difference that flips only that check is the book's.
func TestClassify_FundingNegativeIsCostDependent(t *testing.T) {
	bt := btAt(1000*hour, true, strategy.ActionHold, checks(exitHold, nil), 0.3013, 1000*hour)
	live := liveRow(1000*hour+33*minute, strategy.ActionExit, checks(with(exitHold, "funding_negative", true), nil), 0.2950, 1000*hour, nil)
	if b, why := classify(bt, live); b != bucketBook {
		t.Errorf("want (b) book, got %s: %s", b, why)
	}
}

// One side flat, the other holding: a consequence of an earlier mismatch,
// reported as state drift rather than as a fresh (c).
func TestClassify_DifferentPositionStateIsStateDrift(t *testing.T) {
	bt := btAt(1000*hour, false, strategy.ActionSkip, checks(entryOK, nil), 0.30, 1000*hour)
	live := liveRow(1000*hour+33*minute, strategy.ActionHold, checks(exitHold, nil), 0.30, 1000*hour, nil)
	if b, _ := classify(bt, live); b != bucketState {
		t.Errorf("want state drift, got %s", b)
	}
}

// A condition only one binary had (the first run predates margin_known and
// trailing_mean) is named, never counted.
func TestClassify_AConditionOnlyOneSideHasIsNamedNotCounted(t *testing.T) {
	older := map[string]bool{"hedge_leg": true, "history_depth": true, "rate_threshold": true, "persistence": true, "liquidity": true, "net_apr": true}
	bt := btAt(1000*hour, false, strategy.ActionSkip, checks(with(entryOK, "net_apr", false), nil), 0.35, 1000*hour)
	live := liveRow(1000*hour+33*minute, strategy.ActionEnter, checks(older, nil), 0.28, 0, nil)
	b, why := classify(bt, live)
	if b != bucketBook || !strings.Contains(why, "margin_known") {
		t.Errorf("want (b) book naming the one-sided checks, got %s: %s", b, why)
	}
	bt = btAt(1000*hour, false, strategy.ActionSkip, checks(with(entryOK, "margin_known", false), nil), 0.30, 1000*hour)
	live = liveRow(1000*hour+33*minute, strategy.ActionEnter, checks(older, nil), 0.30, 0, nil)
	if b, why := classify(bt, live); b != bucketUnexplained || !strings.Contains(why, "margin_known") {
		t.Errorf("a check only the replay failed → (c), named: %s: %s", b, why)
	}
}

// A refused live cost is "no number", never a price of zero.
func TestClassify_ARefusedLiveCostIsNotZero(t *testing.T) {
	bt := btAt(1000*hour, false, strategy.ActionEnter, checks(entryOK, nil), 0.30, 1000*hour)
	live := liveRow(1000*hour+33*minute, strategy.ActionSkip, checks(with(with(entryOK, "liquidity", false), "net_apr", false), nil), 0, 1000*hour, nil)
	if b, why := classify(bt, live); b != bucketBook || strings.Contains(why, "0.0000%") || !strings.Contains(why, "không định giá được") {
		t.Errorf("want (b) book with the cost named as refused: %s: %s", b, why)
	}
}

// Pairing: the first row that HAD the settlement — by its own inputs, or by
// the store's recorded_at for older rows — never a row before delivery; a
// row answers one decision; none within the lag → no_row.
func TestPairDecisions_TakesTheFirstRowThatHadTheSettlement(t *testing.T) {
	d0 := btAt(1000*hour, false, strategy.ActionEnter, checks(entryOK, nil), 0.3, 1000*hour)
	d1 := btAt(1008*hour, false, strategy.ActionSkip, checks(entryOK, nil), 0.3, 1008*hour)
	d2 := btAt(1016*hour, false, strategy.ActionSkip, checks(entryOK, nil), 0.3, 1016*hour)
	rows := []store.SignalRecord{
		liveRow(1000*hour-1*minute, strategy.ActionSkip, checks(entryOK, nil), 0.3, 992*hour, nil),    // before d0
		liveRow(1000*hour+3*minute, strategy.ActionSkip, checks(entryOK, nil), 0.3, 992*hour, nil),    // after d0, still the previous print
		liveRow(1000*hour+33*minute, strategy.ActionEnter, checks(entryOK, nil), 0.3, 1000*hour, nil), // the row that had it
		liveRow(1000*hour+43*minute, strategy.ActionHold, checks(exitHold, nil), 0.3, 1000*hour, nil),
		liveRow(1008*hour+3*minute, strategy.ActionSkip, checks(entryOK, nil), 0.3, 0, nil),  // first run's shape: no inputs, before delivery
		liveRow(1008*hour+25*minute, strategy.ActionSkip, checks(entryOK, nil), 0.3, 0, nil), // at delivery
	}
	recorded := map[int64]int64{1000 * hour: 1000*hour + 30*minute, 1008 * hour: 1008*hour + 25*minute}
	pairs := pairDecisions([]backtest.DecisionAt{d0, d1, d2}, rows, recorded, 2*hour)
	byAt := map[int64]pairing{}
	for _, p := range pairs {
		if !p.LiveOnly {
			byAt[p.AtMs] = p
		}
	}
	if p := byAt[1000*hour]; p.Live == nil || p.Live.EvaluatedAtMs != 1000*hour+33*minute || p.Bucket != bucketMatch {
		t.Errorf("d0 must pair with the 08:33 row that carried the print: %+v", p)
	}
	if p := byAt[1008*hour]; p.Live == nil || p.Live.EvaluatedAtMs != 1008*hour+25*minute {
		t.Errorf("an old row is placed by recorded_at: %+v", p.Live)
	}
	if p := byAt[1016*hour]; p.Live != nil || p.Bucket != bucketNoRow {
		t.Errorf("d2 has no row within the lag: %+v", p)
	}
	for _, p := range pairs {
		if p.LiveOnly {
			t.Errorf("no live-only action expected here: %+v", p)
		}
	}
}

// A live enter/exit no decision consumed — a basis exit three hours into a
// period, an enter before the first settlement — is classified against the
// decision in force, or reported as carried-in when there is none yet.
func TestPairDecisions_ClassifiesLiveActionsBetweenSettlements(t *testing.T) {
	d0 := btAt(1000*hour, true, strategy.ActionHold, checks(exitHold, nil), 0.3, 1000*hour)
	rows := []store.SignalRecord{
		liveRow(1000*hour-2*hour, strategy.ActionEnter, checks(entryOK, nil), 0.3, 992*hour, nil), // before any decision
		liveRow(1000*hour+33*minute, strategy.ActionHold, checks(exitHold, nil), 0.3, 1000*hour, nil),
		liveRow(1000*hour+3*hour, strategy.ActionExit, checks(with(exitHold, "basis_widened", true), nil), 0.3, 1000*hour, nil),
	}
	pairs := pairDecisions([]backtest.DecisionAt{d0}, rows, nil, 2*hour)
	var liveOnly []pairing
	for _, p := range pairs {
		if p.LiveOnly {
			liveOnly = append(liveOnly, p)
		}
	}
	if len(liveOnly) != 2 {
		t.Fatalf("want the carried-in enter and the basis exit, got %d: %+v", len(liveOnly), liveOnly)
	}
	if liveOnly[0].Bucket != bucketState || !strings.Contains(liveOnly[0].WhyVI, "TRƯỚC mốc settle đầu tiên") {
		t.Errorf("an action before the first decision is carried-in state: %+v", liveOnly[0])
	}
	if liveOnly[1].Bucket != bucketBasis {
		t.Errorf("a basis exit between settlements is (b) basis: %+v", liveOnly[1])
	}
}

// Step 2: every row's params_json must render the block the replay ran —
// effective values, config key names — the spot leg must be the replay's,
// and the fee state of both legs must be the schedules the replay priced
// with. max_book_age_min is skipped by name.
func TestParamsMismatch_NamesTheKeyAndTheRow(t *testing.T) {
	p := strategy.Params{MinRatePer8hBps: 0.3, PersistencePeriods: 6, MinNetAPRFrac: 0.02, NotionalQuote: 50000, HoldingDays: 90,
		ExitPersistencePeriods: 48, ExitNegativeMinBps: 2, ExitNegativePeriods: 2, ExitNegativeCumCostFrac: 1, MinHoldRecoveredCostFrac: 1,
		MaxBasisPct: 2, MaxBasisWidenPct: 2}
	spot := fees.Schedule{Source: "binance_spot", TakerFeeBps: 10, Verified: true}
	perp := fees.Schedule{Source: "binance_futures", TakerFeeBps: 5, Verified: true}
	good := p.Map()
	good["max_book_age_min"] = 120.0
	good["fees"] = map[string]any{"spot": map[string]any{"source": "binance_spot", "taker_bps": 10.0, "verified": true},
		"perp": map[string]any{"source": "binance_futures", "taker_bps": 5.0, "verified": true}}
	ok := liveRow(1000*hour, strategy.ActionSkip, nil, 0.3, 1000*hour, good)
	if got := paramsMismatch([]store.SignalRecord{ok}, p, "binance_spot", spot, perp); len(got) != 0 {
		t.Fatalf("a matching row must not be reported: %v", got)
	}
	bad := p.Map()
	bad["min_rate_per_8h_bps"] = 0.5
	bad["fees"] = map[string]any{"spot": good["fees"].(map[string]any)["spot"],
		"perp": map[string]any{"source": "binance_futures", "taker_bps": 5.0, "verified": false}}
	rows := []store.SignalRecord{ok, liveRow(1001*hour, strategy.ActionSkip, nil, 0.3, 1001*hour, bad)}
	got := paramsMismatch(rows, p, "binance_spot", spot, perp)
	if len(got) != 2 || !strings.Contains(got[0], "min_rate_per_8h_bps") || !strings.Contains(got[1], "verified") {
		t.Errorf("want the key and the fee flag named, got %v", got)
	}
	// A first-run row without fees is accepted on parameters alone.
	if got := paramsMismatch([]store.SignalRecord{liveRow(1002*hour, strategy.ActionSkip, nil, 0.3, 0, p.Map())}, p, "binance_spot", spot, perp); len(got) != 0 {
		t.Errorf("a row without a fees key must not be a mismatch: %v", got)
	}
	// A different spot leg is a different position.
	other := liveRow(1003*hour, strategy.ActionSkip, nil, 0.3, 1003*hour, p.Map())
	other.SpotSource = "bybit_spot"
	if got := paramsMismatch([]store.SignalRecord{other}, p, "binance_spot", spot, perp); len(got) != 1 || !strings.Contains(got[0], "chân spot") {
		t.Errorf("a different spot leg must be named: %v", got)
	}
}

// PLAN 3.5 ⑤: keys are compared at their EFFECTIVE values, never counted. A
// row written by the older binary (ten keys) matches a block that leaves the
// newer keys at what their absence meant and mismatches one that moved them.
func TestParamsMismatch_AnAbsentKeyMeansTheOldRule(t *testing.T) {
	old := strategy.Params{MinRatePer8hBps: 0.5, PersistencePeriods: 3, MinNetAPRFrac: 0.02, NotionalQuote: 50000, HoldingDays: 30,
		ExitNetAPRFrac: 0.005, ExitPersistencePeriods: 3, MaxBasisPct: 1.0, MaxBasisWidenPct: 0.5}
	tenKeys := map[string]any{"min_rate_per_8h_bps": 0.5, "persistence_periods": 3, "min_net_apr_frac": 0.02, "notional_quote": 50000,
		"holding_days": 30, "max_book_age_min": 120, "exit_net_apr_frac": 0.005, "exit_persistence_periods": 3,
		"max_basis_pct": 1.0, "max_basis_widen_pct": 0.5}
	row := liveRow(1000*hour, strategy.ActionSkip, nil, 0.3, 0, tenKeys)
	spot, perp := fees.Schedule{Source: "binance_spot", TakerFeeBps: 10, Verified: true}, fees.Schedule{Source: "binance_futures", TakerFeeBps: 5, Verified: true}
	if got := paramsMismatch([]store.SignalRecord{row}, old, "binance_spot", spot, perp); len(got) != 0 {
		t.Errorf("the old block at the old rule must match a ten-key row: %v", got)
	}
	moved := old
	moved.MinHoldRecoveredCostFrac = 1.0
	moved.ExitNegativePeriods = 2
	got := paramsMismatch([]store.SignalRecord{row}, moved, "binance_spot", spot, perp)
	if len(got) != 2 || !strings.Contains(got[0], "exit_negative_periods") || !strings.Contains(got[1], "min_hold_recovered_cost_frac") {
		t.Errorf("a moved key the row lacks is a mismatch at what its absence meant: %v", got)
	}
}

// --- the money check of step 5, which must be like-for-like ---

// enterPair builds a pairing in which each side may or may not enter, with its
// own projected net APR and round-trip cost.
func enterPair(liveAction, btAction strategy.Action, liveAPR, btAPR, liveCost, btCost float64, liveOK, btOK bool) pairing {
	live := liveRow(hour, liveAction, checks(entryOK, nil), liveCost, 0, nil)
	live.NetAPRFrac, live.NetAPROK = liveAPR, liveOK
	bt := btAt(hour, false, btAction, checks(entryOK, nil), btCost, hour)
	bt.Decision.NetAPR = strategy.NetAPRResult{OK: btOK, NetAPRFrac: btAPR}
	return pairing{AtMs: hour, Live: &live, Backtest: bt, Bucket: bucketMatch}
}

// Step 5 says the two sums are compared "cùng đơn vị, cùng thời điểm". Summing
// each side's own entries independently while bounding the difference by a band
// measured only where both entered gives a bound that cannot bound its own
// quantity: in the 2026-09-13 rehearsal the difference was 1.2365 against a
// band of ±0.0001, because the two sides entered at almost entirely different
// settlements. Only settlements BOTH sides entered at may go into the sums.
func TestMoneyCheck_SumsOnlyTheSettlementsBothSidesEnteredAt(t *testing.T) {
	s := newCompareStats()
	// Both entered: 0.10 live against 0.08 replay, costs 0.30% and 0.28%.
	s.add(enterPair(strategy.ActionEnter, strategy.ActionEnter, 0.10, 0.08, 0.30, 0.28, true, true), 30)
	// Live entered alone, with a large projection that must NOT reach the sum.
	s.add(enterPair(strategy.ActionEnter, strategy.ActionSkip, 5.00, 0, 0.30, 0.30, true, true), 30)
	// Replay entered alone, likewise.
	s.add(enterPair(strategy.ActionSkip, strategy.ActionEnter, 0, 7.00, 0.30, 0.30, true, true), 30)

	if s.pairedEnters != 1 {
		t.Fatalf("pairedEnters = %d, want 1", s.pairedEnters)
	}
	if s.liveOnlyEnters != 1 || s.btOnlyEnters != 1 {
		t.Errorf("one-sided entries counted %d live / %d replay, want 1 / 1", s.liveOnlyEnters, s.btOnlyEnters)
	}
	if s.liveEnterNetAPR != 0.10 {
		t.Errorf("Σ live = %.4f, want 0.10 — the live-only entry of 5.00 leaked into a like-for-like sum", s.liveEnterNetAPR)
	}
	if s.btEnterNetAPR != 0.08 {
		t.Errorf("Σ replay = %.4f, want 0.08 — the replay-only entry of 7.00 leaked in", s.btEnterNetAPR)
	}
	// The band must now cover the very difference it is placed beside:
	// |0.30-0.28|/100 * 365/30 = 0.002433, against |0.10-0.08| = 0.02.
	gap := s.liveEnterNetAPR - s.btEnterNetAPR
	if s.costGapAPR <= 0 {
		t.Fatal("the cost band is zero on a pair whose costs differ")
	}
	if want := 0.02 / 100 * 365 / 30; !nearly(s.costGapAPR, want) {
		t.Errorf("band = %.6f, want %.6f (Δcost / hold × 365)", s.costGapAPR, want)
	}
	t.Logf("gap %.6f against band %.6f — both measured on the same one settlement", gap, s.costGapAPR)
}

// net_apr_ok = 0 is "not priced", never a zero to add in. A paired entry where
// either side has no number is counted out loud rather than folded into the sum
// as if it were worth nothing.
func TestMoneyCheck_AnUnpricedSideIsCountedNotTreatedAsZero(t *testing.T) {
	s := newCompareStats()
	s.add(enterPair(strategy.ActionEnter, strategy.ActionEnter, 0.10, 0, 0.30, 0.30, true, false), 30)
	s.add(enterPair(strategy.ActionEnter, strategy.ActionEnter, 0, 0.09, 0.30, 0.30, false, true), 30)

	if s.unpricedEnters != 2 {
		t.Errorf("unpricedEnters = %d, want 2", s.unpricedEnters)
	}
	if s.pairedEnters != 0 {
		t.Errorf("pairedEnters = %d, want 0", s.pairedEnters)
	}
	if s.liveEnterNetAPR != 0 || s.btEnterNetAPR != 0 {
		t.Errorf("an unpriced entry reached the sums: live %.4f, replay %.4f", s.liveEnterNetAPR, s.btEnterNetAPR)
	}
	if note := onlySideNote(s); !strings.Contains(note, "không định giá được") {
		t.Errorf("the report does not say two entries were unpriced: %q", note)
	}
}

func nearly(got, want float64) bool {
	d := got - want
	return d < 1e-9 && d > -1e-9
}
