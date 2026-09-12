package main

// The step-3.5 comparison, by machine.
//
// PLAN.md 3.5 ③ says how the live journal and a replay are compared: same
// window, same parameters (step 2), then decision by decision — for every
// settlement the replay decided on, the journal row that judged the SAME
// settlement — counting (a) the same action, (b) a different action explained
// by ONE of three inputs the two sides cannot share (the live book against
// the fixed one, the basis exit only the live side can evaluate, a settlement
// the hourly top-up had not delivered yet), and (c) the rest, which is the
// two implementations having drifted — the one thing the gate exists to
// catch. Step 5's thresholds are (c) = 0 and (a) ≥ 95% on the enter/exit
// decisions; step 1's is an unbroken window, which the no-row bucket tests.
//
// "The row that judged the same settlement" is not the first row after it.
// The live path receives a settlement through the hourly top-up, 20–60
// minutes after its stamp (measured on run 1: recorded_at_ms 19–40 min
// late), while the journal ticks every 10 minutes — so the first rows after
// a settlement still judged the PREVIOUS one, and pairing on them made every
// enter a mismatch by construction (found by the review of 2026-09-10). A
// row is paired only once it carries the settlement: since 2026-09-10 the
// row says so itself (params_json.inputs.newest_settled_at_ms); an older
// row is placed by the store's recorded_at_ms of that settlement.
//
// Nothing here decides anything: the replay is backtest.RunTraced through the
// production rules, the journal is read as written, and this file only lines
// the two up and counts.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"futures-arbitrage-scanner/internal/backtest"
	"futures-arbitrage-scanner/internal/fees"
	"futures-arbitrage-scanner/internal/store"
	"futures-arbitrage-scanner/internal/strategy"
)

type journalBucket string

const (
	bucketMatch       journalBucket = "a_match"
	bucketBook        journalBucket = "b_book"
	bucketBasis       journalBucket = "b_basis"
	bucketHistory     journalBucket = "b_history"
	bucketState       journalBucket = "state"
	bucketNoRow       journalBucket = "no_row"
	bucketLiveGap     journalBucket = "live_gap"
	bucketUnexplained journalBucket = "c_unexplained"
)

// journalLagMs bounds how long after a settlement its journal row may come:
// the top-up delivers within the hour and the journal ticks every 10 minutes,
// so two hours of silence is a gap in the run (a process down or a sleeping
// machine), not a late row.
const journalLagMs = int64(2 * time.Hour / time.Millisecond)

// pairing is one replay decision beside the journal row that answered it —
// or, for LiveOnly, a live enter/exit no replay decision answered, beside
// the replay decision in force at that moment.
type pairing struct {
	AtMs     int64
	Backtest backtest.DecisionAt
	Live     *store.SignalRecord
	LiveOnly bool
	Bucket   journalBucket
	WhyVI    string
}

// Which checks each of the three explainable inputs can move. history_depth
// is never diffed: the replay loads 200 days before the window and the live
// path 30 days, so the count differs by construction and says nothing.
// funding_negative is cost-dependent through the C gate and the M floor.
var (
	bookChecks    = map[string]bool{"liquidity": true, "net_apr": true, "net_apr_floor": true, "trailing_mean": true, "funding_negative": true}
	basisChecks   = map[string]bool{"basis_widened": true}
	historyChecks = map[string]bool{"rate_threshold": true, "persistence": true, "trailing_mean": true, "net_apr": true,
		"funding_negative": true, "net_apr_floor": true}
)

// rowHasSettlement says whether a journal row had received the settlement
// stamped `settledAtMs` when it was written: by the row's own record of
// what it decided on, or — for rows written before that record existed —
// by when the store recorded the settlement (0 = unknown, so the row's time
// alone decides).
func rowHasSettlement(row store.SignalRecord, settledAtMs, recordedAtMs int64) bool {
	if row.EvaluatedAtMs < settledAtMs {
		return false
	}
	if newest, ok := liveInputs(row); ok {
		return newest >= settledAtMs
	}
	return row.EvaluatedAtMs >= recordedAtMs
}

// pairDecisions gives every replay decision the first journal row that had
// its settlement, within the lag cap; rows are consumed in order, so a row
// answers at most one decision. Live enter/exit rows no decision consumed
// are returned as LiveOnly pairings against the replay decision in force at
// their time (the newest decision at or before the row), so a basis exit
// taken three hours after a settlement is classified rather than lost.
func pairDecisions(decisions []backtest.DecisionAt, rows []store.SignalRecord, recordedAt map[int64]int64, maxLagMs int64) []pairing {
	sorted := append([]store.SignalRecord(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].EvaluatedAtMs < sorted[j].EvaluatedAtMs })
	out := make([]pairing, 0, len(decisions))
	consumed := make([]bool, len(sorted))
	j := 0
	for _, d := range decisions {
		for j < len(sorted) && sorted[j].EvaluatedAtMs < d.AtMs {
			j++
		}
		p := pairing{AtMs: d.AtMs, Backtest: d, Bucket: bucketNoRow,
			WhyVI: "không có hàng nhật ký nào mang mốc này trong 2 giờ sau nó (tiến trình không chạy, máy ngủ, hay top-up không tới)"}
		for k := j; k < len(sorted) && sorted[k].EvaluatedAtMs < d.AtMs+maxLagMs; k++ {
			if consumed[k] || !rowHasSettlement(sorted[k], d.AtMs, recordedAt[d.AtMs]) {
				continue
			}
			row := sorted[k]
			consumed[k] = true
			p.Live = &row
			p.Bucket, p.WhyVI = classify(d, row)
			break
		}
		out = append(out, p)
	}
	// Live actions the pairing did not consume.
	di := 0
	for k, row := range sorted {
		if consumed[k] || (row.Action != string(strategy.ActionEnter) && row.Action != string(strategy.ActionExit)) {
			continue
		}
		for di+1 < len(decisions) && decisions[di+1].AtMs <= row.EvaluatedAtMs {
			di++
		}
		r := row
		p := pairing{AtMs: row.EvaluatedAtMs, Live: &r, LiveOnly: true}
		if len(decisions) == 0 || decisions[di].AtMs > row.EvaluatedAtMs {
			p.Bucket, p.WhyVI = bucketState, fmt.Sprintf("bên sống %s lúc %s, TRƯỚC mốc settle đầu tiên của cửa sổ — vị thế mang từ trước cửa sổ (sổ giấy seed từ nhật ký cũ) hoặc quyết định trên lịch sử trước cửa sổ; replay bắt đầu trống",
				row.Action, stampMs(row.EvaluatedAtMs))
		} else {
			p.Backtest = decisions[di]
			p.Bucket, p.WhyVI = classify(decisions[di], row)
			p.WhyVI = "hành động sống ngoài mốc settle: " + p.WhyVI
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].AtMs < out[b].AtMs })
	return out
}

// classify says whether one replay decision and its journal row agree, and if
// not, which ONE of the three inputs explains every differing check — or that
// none does on its own.
func classify(bt backtest.DecisionAt, live store.SignalRecord) (journalBucket, string) {
	liveHolding := live.Action == string(strategy.ActionHold) || live.Action == string(strategy.ActionExit)
	if liveHolding != bt.Holding {
		return bucketState, fmt.Sprintf("hai bên không cùng trạng thái vị thế (backtest %s, sống %s) — hệ quả của một lệch trước đó hoặc của vị thế mang vào cửa sổ, không phải lệch mới",
			stateVI(bt.Holding), stateVI(liveHolding))
	}
	if string(bt.Decision.Action) == live.Action {
		return bucketMatch, "cùng hành động"
	}

	btChecks := map[string]bool{}
	for _, c := range bt.Decision.Checks {
		btChecks[c.Name] = c.Passed && !c.NotEvaluated
	}
	lvChecks, err := liveChecks(live)
	if err != nil {
		return bucketUnexplained, "không đọc được checks_json của hàng sống: " + err.Error()
	}
	// Only conditions BOTH sides evaluated are compared: a check one binary
	// did not have yet (margin_known, trailing_mean on the first run) is
	// named, never counted — the side lacking it could not have failed it,
	// and if the other side did, the actions differ with no common check
	// differing, which lands in (c) below as it should.
	var diffs, oneSided []string
	for name := range btChecks {
		if name == "history_depth" {
			continue
		}
		if lv, seen := lvChecks[name]; !seen {
			oneSided = append(oneSided, name)
		} else if btChecks[name] != lv {
			diffs = append(diffs, name)
		}
	}
	for name := range lvChecks {
		if _, seen := btChecks[name]; !seen && name != "history_depth" {
			oneSided = append(oneSided, name)
		}
	}
	sort.Strings(diffs)
	sort.Strings(oneSided)
	oneSidedVI := ""
	if len(oneSided) > 0 {
		oneSidedVI = fmt.Sprintf(" (điều kiện chỉ một bên có: %s)", strings.Join(oneSided, ", "))
	}
	if len(diffs) == 0 {
		return bucketUnexplained, fmt.Sprintf("cùng mọi điều kiện chung mà khác hành động (backtest %s, sống %s) — luật đã trôi%s", bt.Decision.Action, live.Action, oneSidedVI)
	}

	// Each input present is tried ALONE against every differing check; the
	// first that covers them all is the explanation. Two inputs each
	// covering a part is no explanation by step 4's wording.
	type cause struct {
		bucket journalBucket
		why    string
		set    map[string]bool
	}
	var causes []cause
	newest, haveInputs := liveInputs(live)
	historyVI := "lịch sử không rõ (hàng sống không ghi inputs)"
	if haveInputs {
		if newest != bt.Decision.NewestSettledAtMs {
			causes = append(causes, cause{bucketHistory, fmt.Sprintf("lịch sử (bên sống có mốc mới nhất %s, backtest %s)", stampMs(newest), stampMs(bt.Decision.NewestSettledAtMs)), historyChecks})
			historyVI = "lịch sử khác"
		} else {
			historyVI = "cùng lịch sử"
		}
	}
	liveCost := liveCostVI(live)
	costVI := "cùng chi phí"
	if math.Abs(live.CostTotalPct-bt.Decision.Cost.TotalPct) > 1e-9 || !live.NetAPROK {
		causes = append(causes, cause{bucketBook, fmt.Sprintf("sổ lệnh (chi phí sống %s ≠ cố định %.4f%%)", liveCost, bt.Decision.Cost.TotalPct), bookChecks})
		costVI = "chi phí khác"
	}
	causes = append(causes, cause{bucketBasis, "basis (chỉ bên sống xét được basis_widened)", basisChecks})

	var partial []string
	for _, c := range causes {
		covered, any := true, false
		for _, d := range diffs {
			if c.set[d] {
				any = true
			} else {
				covered = false
			}
		}
		if any && covered {
			return c.bucket, c.why + oneSidedVI
		}
		if any {
			partial = append(partial, c.why)
		}
	}
	if len(partial) > 0 {
		return bucketUnexplained, fmt.Sprintf("không đầu vào nào giải thích được TRỌN các điều kiện khác nhau (%s); từng phần: %s%s",
			strings.Join(diffs, ", "), strings.Join(partial, " + "), oneSidedVI)
	}
	return bucketUnexplained, fmt.Sprintf("điều kiện khác nhau (%s) mà %s, %s, không phải basis — luật đã trôi%s",
		strings.Join(diffs, ", "), historyVI, costVI, oneSidedVI)
}

func stateVI(holding bool) string {
	if holding {
		return "đang giữ"
	}
	return "trống"
}

// liveCostVI renders a live row's round trip: 0 beside net_apr_ok=false is
// "no number", never a price of zero (rule 2).
func liveCostVI(live store.SignalRecord) string {
	if !live.NetAPROK && live.CostTotalPct == 0 {
		return "không định giá được"
	}
	return fmt.Sprintf("%.4f%%", live.CostTotalPct)
}

func liveChecks(rec store.SignalRecord) (map[string]bool, error) {
	var cs []struct {
		Name   string `json:"name"`
		Passed bool   `json:"passed"`
	}
	if err := json.Unmarshal([]byte(rec.ChecksJSON), &cs); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(cs))
	for _, c := range cs {
		out[c.Name] = c.Passed
	}
	return out, nil
}

// liveInputs reads what the live row decided on (journalled since
// 2026-09-10); absent on older rows.
func liveInputs(rec store.SignalRecord) (newestMs int64, ok bool) {
	var p struct {
		Inputs *struct {
			NewestSettledAtMs float64 `json:"newest_settled_at_ms"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal([]byte(rec.ParamsJSON), &p); err != nil || p.Inputs == nil {
		return 0, false
	}
	return int64(p.Inputs.NewestSettledAtMs), true
}

type liveFee struct {
	Source   string  `json:"source"`
	TakerBps float64 `json:"taker_bps"`
	Verified bool    `json:"verified"`
}

// absentMeans is what a key ABSENT from a row's params_json meant when the
// row was written: the rule before the key existed (PLAN 3.5 ⑤ — compare
// the effective values of common keys, never the key count). Every key not
// listed here was absent ≡ 0.
var absentMeans = map[string]float64{"exit_negative_periods": 1}

// paramsMismatch is step 2: every row's params_json must render the block
// the replay ran (strategy.Params.Map, effective values), the row must be
// hedged on the spot leg the replay is hedged on, and the fee state it
// carries must be the schedules the replay priced with. A key the row lacks
// is compared at what its absence meant, so a row written by an older
// binary matches a block that leaves the newer keys at their old rule and
// mismatches one that moved them. max_book_age_min is skipped by name — the
// replay prices one fixed book and has no age. Rows without a fees key (the
// first run, before 2026-09-10) are judged on parameters alone. A row with
// NO spot leg (the registry had not refreshed at that tick) is not a
// parameter mismatch: it is reported as a live gap by the pairing.
func paramsMismatch(rows []store.SignalRecord, p strategy.Params, spotSource string, spot, perp fees.Schedule) []string {
	want := p.Map()
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, r := range rows {
		where := fmt.Sprintf("hàng %s %s/%s", stampMs(r.EvaluatedAtMs), r.Symbol, r.PerpSource)
		var got map[string]any
		if err := json.Unmarshal([]byte(r.ParamsJSON), &got); err != nil {
			out = append(out, fmt.Sprintf("%s: params_json không đọc được: %v", where, err))
			continue
		}
		for _, k := range keys {
			if k == "max_book_age_min" {
				continue
			}
			w, _ := numOf(want[k])
			g, ok := numOf(got[k])
			if !ok {
				if _, present := got[k]; present {
					out = append(out, fmt.Sprintf("%s: %s = %v không phải số", where, k, got[k]))
					continue
				}
				if meant := absentMeans[k]; math.Abs(w-meant) > 1e-9 {
					out = append(out, fmt.Sprintf("%s: không có khoá %s (vắng ≡ %v), khối = %v", where, k, meant, want[k]))
				}
				continue
			}
			if math.Abs(w-g) > 1e-9 {
				out = append(out, fmt.Sprintf("%s: %s = %v, khối = %v", where, k, got[k], want[k]))
			}
		}
		if r.SpotSource != "" && spotSource != "" && r.SpotSource != spotSource {
			out = append(out, fmt.Sprintf("%s: chân spot sống %s, replay %s — hai vị thế khác nhau", where, r.SpotSource, spotSource))
		}
		if raw, has := got["fees"]; has && raw != nil {
			var f struct {
				Spot *liveFee `json:"spot"`
				Perp *liveFee `json:"perp"`
			}
			b, _ := json.Marshal(raw)
			if err := json.Unmarshal(b, &f); err != nil {
				out = append(out, fmt.Sprintf("%s: fees không đọc được: %v", where, err))
				continue
			}
			out = append(out, feeMismatch(where, "perp", f.Perp, perp)...)
			if r.SpotSource != "" && spot.Source != "" {
				out = append(out, feeMismatch(where, "spot", f.Spot, spot)...)
			}
		}
	}
	return out
}

func feeMismatch(where, leg string, got *liveFee, want fees.Schedule) []string {
	if got == nil {
		return []string{fmt.Sprintf("%s: fees.%s null, replay có biểu phí %s", where, leg, want.Source)}
	}
	var out []string
	if got.Source != "" && got.Source != want.Source {
		out = append(out, fmt.Sprintf("%s: fees.%s.source = %s, replay = %s", where, leg, got.Source, want.Source))
	}
	if math.Abs(got.TakerBps-want.TakerFeeBps) > 1e-9 {
		out = append(out, fmt.Sprintf("%s: fees.%s.taker_bps = %g, replay = %g", where, leg, got.TakerBps, want.TakerFeeBps))
	}
	if got.Verified != want.Verified {
		out = append(out, fmt.Sprintf("%s: fees.%s.verified = %v, replay = %v", where, leg, got.Verified, want.Verified))
	}
	return out
}

func numOf(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func stampMs(ms int64) string {
	if ms == 0 {
		return "—"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04")
}

// compareStats is one series' (or the total's) tally.
type compareStats struct {
	decisions, enterExit, enterExitMatch int
	buckets                              map[journalBucket]int
	liveEnterNetAPR, btEnterNetAPR       float64
	costGapAPR                           float64
	// pairedEnters is how many settlements BOTH sides entered at, which is the
	// set the three numbers above are summed over. The two counts beside it are
	// the entries only one side made: they are deliberately NOT in the sums —
	// see add — and are printed so nobody reads a matched total as a complete
	// one.
	pairedEnters, liveOnlyEnters, btOnlyEnters int
	// unpricedEnters is paired entries where one side could not price the APR
	// (net_apr_ok = 0). Dropping them silently would quietly shrink the check.
	unpricedEnters int
}

func newCompareStats() *compareStats { return &compareStats{buckets: map[journalBucket]int{}} }

func (s *compareStats) add(p pairing, holdingDays float64) {
	s.decisions++
	s.buckets[p.Bucket]++
	btAction, liveAction := "", ""
	if !p.LiveOnly {
		btAction = string(p.Backtest.Decision.Action)
	}
	if p.Live != nil {
		liveAction = p.Live.Action
	}
	isEnterExit := func(a string) bool { return a == string(strategy.ActionEnter) || a == string(strategy.ActionExit) }
	if isEnterExit(btAction) || isEnterExit(liveAction) {
		s.enterExit++
		if p.Bucket == bucketMatch {
			s.enterExitMatch++
		}
	}
	// The money check (step 5) compares what the two sides PROJECTED "cùng đơn
	// vị, cùng thời điểm" — same unit, SAME INSTANT. So it is summed only over
	// settlements both sides entered at.
	//
	// It used to sum each side's entries independently while bounding the
	// difference by a band computed only where both entered. Whenever the two
	// sides entered at different settlements — which is the interesting case,
	// and was every case in the 2026-09-12 rehearsal — the sums covered
	// different sets and the band could not bound them even in principle:
	// measured Σ difference 1.2365 against a band of ±0.0001. A bound that
	// cannot bound its quantity is worse than no bound, because it is printed
	// beside a verdict.
	//
	// Entries only one side made are counted instead, and printed. They are
	// already visible to the verdict through (a), (b) and (c); what they must
	// not do is enter an arithmetic that pretends to be like-for-like.
	btEnter := !p.LiveOnly && !p.Backtest.Holding && btAction == string(strategy.ActionEnter)
	liveEnter := p.Live != nil && liveAction == string(strategy.ActionEnter)
	switch {
	case btEnter && liveEnter:
		if !p.Backtest.Decision.NetAPR.OK || !p.Live.NetAPROK {
			// One side has no number at all. net_apr_ok = 0 is "not priced",
			// never a zero to add in.
			s.unpricedEnters++
			break
		}
		s.pairedEnters++
		s.btEnterNetAPR += p.Backtest.Decision.NetAPR.NetAPRFrac
		s.liveEnterNetAPR += p.Live.NetAPRFrac
		if holdingDays > 0 {
			// A cost difference enters the net APR as Δcost / hold × 365.
			s.costGapAPR += math.Abs(p.Live.CostTotalPct-p.Backtest.Decision.Cost.TotalPct) / 100 * 365 / holdingDays
		}
	case liveEnter:
		s.liveOnlyEnters++
	case btEnter:
		s.btOnlyEnters++
	}
}

func (s *compareStats) merge(o *compareStats) {
	s.decisions += o.decisions
	s.enterExit += o.enterExit
	s.enterExitMatch += o.enterExitMatch
	for k, v := range o.buckets {
		s.buckets[k] += v
	}
	s.liveEnterNetAPR += o.liveEnterNetAPR
	s.btEnterNetAPR += o.btEnterNetAPR
	s.costGapAPR += o.costGapAPR
	s.pairedEnters += o.pairedEnters
	s.liveOnlyEnters += o.liveOnlyEnters
	s.btOnlyEnters += o.btOnlyEnters
	s.unpricedEnters += o.unpricedEnters
}

// onlySideNote names the entries left OUT of the money sums. They are not an
// error and they are not silently dropped: (a), (b) and (c) already account for
// them as decisions. What they must not do is enter a sum that claims to be
// like-for-like, so they are reported beside it instead.
func onlySideNote(s *compareStats) string {
	if s.liveOnlyEnters == 0 && s.btOnlyEnters == 0 && s.unpricedEnters == 0 {
		return ""
	}
	return fmt.Sprintf(" · ngoài phép so: %d enter chỉ bên sống, %d chỉ bên replay, %d không định giá được",
		s.liveOnlyEnters, s.btOnlyEnters, s.unpricedEnters)
}

func (s *compareStats) line(name string) string {
	share := "—"
	if s.enterExit > 0 {
		share = fmt.Sprintf("%.1f%%", 100*float64(s.enterExitMatch)/float64(s.enterExit))
	}
	return fmt.Sprintf("%-34s %5d cặp · (a) %4d · (b) sổ %3d basis %3d lịch sử %3d · (c) %3d · trạng thái %3d · thiếu hàng %3d · thiếu chân %3d · enter/exit %d, khớp %s",
		name, s.decisions, s.buckets[bucketMatch], s.buckets[bucketBook], s.buckets[bucketBasis], s.buckets[bucketHistory],
		s.buckets[bucketUnexplained], s.buckets[bucketState], s.buckets[bucketNoRow], s.buckets[bucketLiveGap], s.enterExit, share)
}

// recordedAtOf is when the store recorded each of a series' settlements —
// the moment the live path could first have seen it.
func recordedAtOf(ctx context.Context, db *store.Store, s backtest.Series, window backtest.Window) (map[int64]int64, error) {
	rows, err := db.FundingHistory(ctx, s.Symbol, window.FromMs, window.ToMs)
	if err != nil {
		return nil, err
	}
	out := map[int64]int64{}
	for _, r := range rows {
		if r.Source == s.PerpSource {
			out[r.SettledAtMs] = r.RecordedAtMs
		}
	}
	return out, nil
}

// runCompare runs the whole protocol over every hedgeable series and prints
// the verdict. Exit code: 0 passed, 1 failed or undecidable, 2 parameters
// mismatched (step 2 says stop — the two runs are not comparable).
func runCompare(ctx context.Context, db *store.Store, series []backtest.Series, window backtest.Window, params strategy.Params, csvPath string) int {
	// Rows a little past the window's end too: the last settlement's row
	// may land after -to, and reading up to the edge alone would call it
	// a gap.
	rows, err := db.SignalDecisions(ctx, window.FromMs, window.ToMs+journalLagMs)
	if err != nil {
		fmt.Printf("không đọc được nhật ký: %v\n", err)
		return 1
	}
	byKey := map[string][]store.SignalRecord{}
	for _, r := range rows {
		k := r.Symbol + "|" + r.PerpSource
		byKey[k] = append(byKey[k], r)
	}
	fmt.Printf("SO SÁNH NHẬT KÝ ↔ REPLAY · %s → %s · %d hàng nhật ký · %d chuỗi replay được · giao thức PLAN 3.5 ③\n\n",
		stampMs(window.FromMs), stampMs(window.ToMs), len(rows), len(series))

	total := newCompareStats()
	var mismatches []string
	var allPairs []pairing
	var pairSeries []string
	var flagged []pairing
	for _, s := range series {
		name := fmt.Sprintf("%s/%s ← %s", s.Symbol, s.PerpSource, s.SpotSource)
		srows := byKey[s.Symbol+"|"+s.PerpSource]
		if len(srows) == 0 {
			fmt.Printf("%-34s không có hàng nhật ký nào trong cửa sổ\n", name)
			continue
		}
		// Rows without a hedge leg are a live input gap, not this series'
		// decisions: set aside and counted, never compared.
		var hedged []store.SignalRecord
		gaps := 0
		for _, r := range srows {
			if r.SpotSource == "" && s.SpotSource != "" {
				gaps++
				continue
			}
			hedged = append(hedged, r)
		}
		if mm := paramsMismatch(hedged, params, s.SpotSource, s.SpotFee, s.PerpFee); len(mm) > 0 {
			mismatches = append(mismatches, mm...)
			fmt.Printf("%-34s BƯỚC 2 LỆCH: %d hàng có params_json/fees/chân spot khác khối replay (vd. %s)\n", name, len(mm), mm[0])
			continue
		}
		result, decisions := backtest.RunTraced(s, window, params)
		if !result.OK {
			fmt.Printf("%-34s replay từ chối: %s\n", name, result.ReasonVI)
			continue
		}
		recorded, err := recordedAtOf(ctx, db, s, window)
		if err != nil {
			fmt.Printf("%-34s không đọc được recorded_at_ms: %v\n", name, err)
			return 1
		}
		pairs := pairDecisions(decisions, hedged, recorded, journalLagMs)
		st := newCompareStats()
		for _, p := range pairs {
			st.add(p, params.HoldingDays)
			if p.Bucket == bucketUnexplained || p.Bucket == bucketState || p.Bucket == bucketNoRow {
				flagged = append(flagged, p)
			}
		}
		st.buckets[bucketLiveGap] += gaps
		fmt.Println(st.line(name))
		fmt.Printf("%-34s   tiền trên %d mốc CẢ HAI cùng enter: Σ net_apr sống %.4f · Σ APR ròng replay %.4f · biên chênh chi phí ±%.4f%s\n",
			"", st.pairedEnters, st.liveEnterNetAPR, st.btEnterNetAPR, st.costGapAPR, onlySideNote(st))
		total.merge(st)
		allPairs = append(allPairs, pairs...)
		for range pairs {
			pairSeries = append(pairSeries, name)
		}
	}
	fmt.Println()
	fmt.Println(total.line("TỔNG"))
	shown := 0
	for _, p := range flagged {
		if p.Bucket == bucketNoRow && shown >= 20 {
			continue
		}
		if shown == 40 {
			fmt.Printf("  … và %d dòng nữa (xem -csv)\n", len(flagged)-40)
			break
		}
		shown++
		live, sym := "—", ""
		if p.Live != nil {
			live, sym = p.Live.Action, p.Live.Symbol+"/"+p.Live.PerpSource
		} else {
			sym = p.Backtest.Decision.Symbol + "/" + p.Backtest.Decision.PerpSource
		}
		bt := "—"
		if !p.LiveOnly {
			bt = string(p.Backtest.Decision.Action)
		}
		fmt.Printf("  [%s] %s %s: backtest %s, sống %s — %s\n", p.Bucket, stampMs(p.AtMs), sym, bt, live, p.WhyVI)
	}
	fmt.Println()
	writePairsCSV(csvPath, allPairs, pairSeries)
	switch {
	case len(mismatches) > 0:
		fmt.Printf("DỪNG (bước 2): %d hàng có tham số/biểu phí/chân spot khác khối replay — hai lượt chạy không so được. Chạy replay với đúng khối nhật ký đã nạp.\n", len(mismatches))
		return 2
	case total.decisions == 0:
		fmt.Println("KHÔNG CÓ GÌ ĐỂ SO: không mốc settle nào trong cửa sổ có hàng nhật ký.")
		return 1
	}
	share := 0.0
	if total.enterExit > 0 {
		share = float64(total.enterExitMatch) / float64(total.enterExit)
	}
	unexplained := total.buckets[bucketUnexplained]
	noRow := total.buckets[bucketNoRow]
	moneyGap := math.Abs(total.liveEnterNetAPR - total.btEnterNetAPR)
	fmt.Printf("Bước 1: %d mốc không có hàng nhật ký (cần 0 — cửa sổ phải liền) · thiếu chân hedge %d\n", noRow, total.buckets[bucketLiveGap])
	fmt.Printf("Bước 5: (c) = %d (cần 0) · (a) trên mốc enter/exit = %d/%d = %.1f%% (cần ≥ 95%%) · trạng thái lệch %d\n",
		unexplained, total.enterExitMatch, total.enterExit, 100*share, total.buckets[bucketState])
	fmt.Printf("Bước 5 (tiền): trên %d mốc CẢ HAI cùng enter, Σ APR ròng sống−replay = %.4f so với biên chi phí ±%.4f%s\n",
		total.pairedEnters, total.liveEnterNetAPR-total.btEnterNetAPR, total.costGapAPR, onlySideNote(total))
	switch {
	case noRow > 0:
		fmt.Println("CỬA SỔ ĐỨT (bước 1): có mốc settle không được nhật ký ghi — tính lại cửa sổ từ lần lên cuối, không cộng dồn hai mảnh. Chưa phán quyết.")
		return 1
	case total.enterExit == 0:
		fmt.Printf("CHƯA PHÁN QUYẾT ĐƯỢC: không có mốc enter/exit nào để đo (a); (c) = %d.\n", unexplained)
		return 1
	case unexplained == 0 && share >= 0.95 && moneyGap <= total.costGapAPR+1e-9:
		fmt.Println("ĐẠT theo giao thức 3.5 ③ bước 5 trên cửa sổ này.")
		return 0
	case unexplained == 0 && share >= 0.95:
		fmt.Println("KHÔNG ĐẠT: quyết định khớp nhưng tổng APR ròng lúc vào lệch quá biên chênh chi phí — là (c) theo bước 5.")
		return 1
	default:
		fmt.Println("KHÔNG ĐẠT → quay lại Bước 3.2, không sang GĐ 4.")
		return 1
	}
}

func writePairsCSV(path string, pairs []pairing, names []string) {
	if path == "" {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Printf("không ghi được %s: %v\n", path, err)
		return
	}
	defer f.Close()
	w := csv.NewWriter(f)
	write := func(rec []string) {
		if err := w.Write(rec); err != nil {
			fmt.Printf("không ghi được %s: %v\n", path, err)
		}
	}
	write([]string{"series", "at_ms", "live_only", "backtest_holding", "backtest_action", "backtest_cost_pct", "backtest_newest_settled_at_ms",
		"live_evaluated_at_ms", "live_action", "live_cost_pct", "live_net_apr_ok", "bucket", "why_vi"})
	for i, p := range pairs {
		liveAt, liveAction, liveCost, liveOK := "", "", "", ""
		if p.Live != nil {
			liveAt, liveAction, liveOK = fmt.Sprint(p.Live.EvaluatedAtMs), p.Live.Action, fmt.Sprint(p.Live.NetAPROK)
			if p.Live.NetAPROK || p.Live.CostTotalPct != 0 {
				liveCost = fmt.Sprintf("%.6f", p.Live.CostTotalPct)
			}
		}
		btHolding, btAction, btCost, btNewest := "", "", "", ""
		if !p.LiveOnly {
			btHolding, btAction = fmt.Sprint(p.Backtest.Holding), string(p.Backtest.Decision.Action)
			btCost, btNewest = fmt.Sprintf("%.6f", p.Backtest.Decision.Cost.TotalPct), fmt.Sprint(p.Backtest.Decision.NewestSettledAtMs)
		}
		write([]string{names[i], fmt.Sprint(p.AtMs), fmt.Sprint(p.LiveOnly), btHolding, btAction, btCost, btNewest,
			liveAt, liveAction, liveCost, liveOK, string(p.Bucket), p.WhyVI})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		fmt.Printf("không ghi được %s: %v\n", path, err)
		return
	}
	fmt.Printf("Đã ghi %d cặp quyết định vào %s\n", len(pairs), path)
}
