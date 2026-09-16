package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"futures-arbitrage-scanner/cmd/execportal/autotrade"
	"futures-arbitrage-scanner/internal/execution"
)

// What the auto-trader's pairs returned, as a person reads it — and what each
// figure leaves out (CLAUDE.md rule 2). Nothing here is called net or profit.
//
// Three sources, each labelled where it surfaces and never blended silently:
//
//   - A CLOSED pair is read from its intent file, which execution's close
//     wrote from what the VENUE said: the funding it paid, the commission it
//     charged in the quote asset, and the four prices the legs filled at. The
//     quote the pair returned is funding − commission + the legs' price drift
//     between the entry and exit FILLS. Slippage against the decision's mids is
//     already inside that drift — a fill worse than the mid is a smaller drift
//     — so it is shown beside the figure and never subtracted a second time
//     (execution's RealizedQuote does subtract it, and leaves the drift out;
//     adding the two would count it twice). Commission taken in another asset
//     and the cost of capital are outside it.
//   - An OPEN pair is the engine's newest reading: the legs marked to the mids
//     of the pair's last scan — gross of the exit's commission and slippage —
//     plus the funding rows the venue lists inside the pair's hold.
//   - The EQUITY SAMPLES are this process's own record of the total, once a
//     minute while the bot runs or holds, kept in memory. A portal restart
//     starts a new series; before the first sample only the closed part is
//     known, from each close's instant in the cache.

const (
	// pnlSampleEvery is the equity series' spacing.
	pnlSampleEvery = time.Minute
	// pnlSampleCap keeps a week of minutes.
	pnlSampleCap = 7 * 24 * 60
	// pnlMarkMaxAge is the oldest scan an open pair is marked from. A bot that
	// stopped scanning — disabled with its positions kept, or halted — leaves
	// mids that no longer describe the pair, and a figure built on them would
	// be a price nobody read.
	pnlMarkMaxAge = 2 * time.Minute
)

// pnlPoint is one point of the equity series.
type pnlPoint struct {
	AtMs        int64   `json:"at_ms"`
	ClosedQuote float64 `json:"closed_quote"`
	// OpenQuote is the open pairs' marked-to-mid drift plus their funding rows;
	// nil when no open part is known (a point from the cache, before this
	// process sampled anything). OpenComplete is every open pair priced.
	OpenQuote            *float64 `json:"open_quote"`
	OpenComplete         bool     `json:"open_complete"`
	TotalQuote           float64  `json:"total_quote"`
	CapitalDeployedQuote float64  `json:"capital_deployed_quote"`
	OpenPositions        int      `json:"open_positions"`
	// Source is "closed" (a close from the cache; the open part unknown),
	// "sample" (this process's minute sample) or "now" (this read).
	Source string `json:"source"`
}

// pnlTracker is the in-memory equity series.
type pnlTracker struct {
	mu      sync.Mutex
	samples []pnlPoint
	next    int
	lastAt  time.Time
}

func newPnLTracker() *pnlTracker { return &pnlTracker{} }

// record keeps one sample, at most one per pnlSampleEvery. It reports whether
// the sample was kept.
func (t *pnlTracker) record(pt pnlPoint, at time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	// A ticker fires a hair early as often as late; a few seconds of slack keep
	// a one-minute series from dropping every other minute.
	if !t.lastAt.IsZero() && at.Sub(t.lastAt) < pnlSampleEvery-5*time.Second {
		return false
	}
	pt.Source, pt.AtMs = "sample", at.UnixMilli()
	t.lastAt = at
	if len(t.samples) < pnlSampleCap {
		t.samples = append(t.samples, pt)
		return true
	}
	t.samples[t.next] = pt
	t.next = (t.next + 1) % pnlSampleCap
	return true
}

// list copies the samples out, oldest first.
func (t *pnlTracker) list() []pnlPoint {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]pnlPoint, 0, len(t.samples))
	if len(t.samples) < pnlSampleCap {
		return append(out, t.samples...)
	}
	out = append(out, t.samples[t.next:]...)
	return append(out, t.samples[:t.next]...)
}

// pnlTradeView is one of the bot's pairs, closed or open.
type pnlTradeView struct {
	IntentID   string `json:"intent_id"`
	Symbol     string `json:"symbol"`
	OpenedAtMs int64  `json:"opened_at_ms"`
	ClosedAtMs int64  `json:"closed_at_ms"`
	// Status is "closed", "open" (the bot holds it now), "unwound" (an open
	// whose second leg failed and was taken back to flat — a cost the cache does
	// not record), "alarm" (an open whose unwind could not be proven flat: the
	// account may hold a naked leg) or "untracked" (the cache records no close
	// and the bot does not name the position).
	Status string `json:"status"`

	NotionalQuote     float64 `json:"notional_quote"`
	CapitalQuote      float64 `json:"capital_quote"`
	QtyCoin           float64 `json:"qty_coin"`
	SpotEntryAvgQuote float64 `json:"spot_entry_avg_quote"`
	PerpEntryAvgQuote float64 `json:"perp_entry_avg_quote"`

	// A closed pair's figures, as execution read them from the venue.
	FundingReceivedQuote float64 `json:"funding_received_quote"`
	CommissionQuote      float64 `json:"commission_quote"`
	SlippageQuote        float64 `json:"slippage_quote"`
	PairPriceDriftQuote  float64 `json:"pair_price_drift_quote"`
	RealizedQuote        float64 `json:"realized_quote"`
	SettlementsCounted   int     `json:"settlements_counted"`
	// CashResultQuote is funding − commission + drift at the fills (closed),
	// or marked-to-mid drift + the venue's funding rows (open); nil when an
	// open pair cannot be marked. OnCapitalPct divides it by CapitalQuote —
	// the hold's return, NOT a yearly rate.
	CashResultQuote        *float64 `json:"cash_result_quote"`
	CashResultOnCapitalPct *float64 `json:"cash_result_on_capital_pct"`

	// An open pair's parts.
	PairDriftQuote    *float64 `json:"pair_drift_quote"`
	FundingRowsQuote  float64  `json:"funding_rows_quote"`
	FundingRows       int      `json:"funding_rows"`
	FundingIncomplete string   `json:"funding_incomplete_vi"`
	MarkedAtMs        int64    `json:"marked_at_ms"`
	CloseReasonVI     string   `json:"close_reason_vi,omitempty"`
	NoteVI            string   `json:"note_vi"`
}

// pnlBar is one settlement's funding across the bot's pairs.
type pnlBar struct {
	SettledAtMs int64          `json:"settled_at_ms"`
	IncomeQuote float64        `json:"income_quote"`
	Rows        []pnlBarDetail `json:"rows"`
}

type pnlBarDetail struct {
	Symbol      string  `json:"symbol"`
	IntentID    string  `json:"intent_id"`
	IncomeQuote float64 `json:"income_quote"`
}

// pnlView is the whole result page.
type pnlView struct {
	ReadAtMs int64 `json:"read_at_ms"`

	ClosedTrades int `json:"closed_trades"`
	// UnwoundOpens counts opens that were taken back to flat: their commission
	// and slippage were real, and are in no figure here.
	UnwoundOpens int `json:"unwound_opens"`
	// AlarmOpens counts opens whose unwind raised an alarm.
	AlarmOpens            int     `json:"alarm_opens"`
	ClosedCashResultQuote float64 `json:"closed_cash_result_quote"`
	ClosedFundingQuote    float64 `json:"closed_funding_quote"`
	ClosedCommissionQuote float64 `json:"closed_commission_quote"`
	ClosedSlippageQuote   float64 `json:"closed_slippage_quote"`
	ClosedDriftQuote      float64 `json:"closed_drift_quote"`
	ClosedRealizedQuote   float64 `json:"closed_realized_quote"`

	OpenPositions      int     `json:"open_positions"`
	OpenPriced         int     `json:"open_priced"`
	OpenResultQuote    float64 `json:"open_result_quote"`
	OpenDriftQuote     float64 `json:"open_drift_quote"`
	OpenFundingQuote   float64 `json:"open_funding_quote"`
	OpenComplete       bool    `json:"open_complete"`
	TotalQuote         float64 `json:"total_quote"`
	CapitalPerNotional float64 `json:"capital_per_notional"`

	CapitalDeployedQuote float64 `json:"capital_deployed_quote"`
	TotalCapitalCapQuote float64 `json:"total_capital_cap_quote"`
	// PeakCapitalQuote is the most capital the bot's pairs in this view tied up
	// AT ONCE, from each pair's open and close instants. ReturnOnPeakCapitalPct
	// is TotalQuote over it: the return on the capital the results actually
	// needed — independent of a cap that may have changed since. The open part
	// over the capital of the pairs it prices is OpenReturnOnDeployedPct.
	// Neither is a yearly rate.
	PeakCapitalQuote        float64  `json:"peak_capital_quote"`
	ReturnOnPeakCapitalPct  *float64 `json:"return_on_peak_capital_pct"`
	OpenReturnOnDeployedPct *float64 `json:"open_return_on_deployed_pct"`

	Trades []pnlTradeView `json:"trades"`
	Bars   []pnlBar       `json:"bars"`
	Points []pnlPoint     `json:"points"`

	// openPricedCapital is the capital of the open pairs OpenResultQuote
	// prices: the denominator its ratio needs.
	openPricedCapital float64

	ClosedLabelVI string   `json:"closed_label_vi"`
	OpenLabelVI   string   `json:"open_label_vi"`
	TotalLabelVI  string   `json:"total_label_vi"`
	BarsLabelVI   string   `json:"bars_label_vi"`
	PointsLabelVI string   `json:"points_label_vi"`
	ProblemsVI    []string `json:"problems_vi"`
}

const (
	pnlClosedLabelVI = "Đã chốt = funding sàn đã trả − phí sàn thu bằng quote + trôi giá hai chân theo GIÁ KHỚP vào/ra, đọc từ file ý định do lệnh đóng ghi. " +
		"Trượt giá so với mid đã NẰM TRONG trôi giá theo giá khớp, hiện bên cạnh để xem, không trừ lần hai. " +
		"CHƯA trừ: phí thu bằng tài sản khác quote, phí chuyển, chi phí vốn, và chi phí của những lần mở hỏng đã gỡ về phẳng (file ý định không ghi). " +
		"File ý định ghi 0 cho phí hay trôi giá mà lệnh đóng KHÔNG đọc được — không phân biệt được với 0 thật; hàng có số 0 như vậy được đánh dấu. Không phải lãi ròng."
	pnlOpenLabelVI = "Tạm tính = trôi giá hai chân theo GIÁ GIỮA của lượt quét gần nhất (không cũ hơn 2 phút) + các dòng funding sàn đã ghi có trong lúc giữ. " +
		"CHƯA trừ phí và trượt giá khi đóng; phí lúc vào chỉ biết khi đóng. Cặp bot không quét nữa (đã tắt, dừng bảo vệ) thì chưa định giá. Không phải lãi ròng."
	pnlTotalLabelVI = "Tổng = Đã chốt + Tạm tính, gồm mọi cặp của bot còn trong thư mục ý định. ROI = Tổng ÷ vốn cao nhất các cặp đó từng buộc CÙNG LÚC " +
		"(spot trọn notional + ký quỹ perp, tính từ thời điểm mở và đóng) — lợi suất trên vốn thật sự cần, không quy năm. Mọi con số là của tài khoản TESTNET."
	pnlBarsLabelVI = "Mỗi cột là MỘT mốc settle sàn thực trả (quy tắc 6), 7 ngày gần nhất, cộng các dòng /fapi/v1/income bằng tài sản quote " +
		"mà đúng một ý định của bot giữ qua. Dòng chung hai ý định hay bằng tài sản khác không được cộng."
	pnlPointsLabelVI = "Đường vốn: trước mẫu đầu tiên của tiến trình portal này chỉ có phần đã chốt (thời điểm đóng trong file ý định); " +
		"sau đó mỗi phút một mẫu Tổng khi bot chạy hoặc đang giữ, lưu trong bộ nhớ — portal khởi động lại thì chuỗi mẫu bắt đầu lại."
)

// buildPnL assembles the page from the cache, the engine's status and — with
// withFunding — the venue's funding rows of every symbol the bot holds or
// closed inside the funding window.
func (p *portal) buildPnL(ctx context.Context, st autotrade.StatusView, withFunding bool) pnlView {
	now := p.now()
	cpn := st.CapitalPerNotional
	if !(cpn > 0) {
		cpn = 1 + p.exec.MarginFrac
	}
	v := pnlView{
		ReadAtMs: now.UnixMilli(), CapitalPerNotional: cpn, TotalCapitalCapQuote: st.Portfolio.TotalCapitalCapQuote,
		CapitalDeployedQuote: st.CapitalDeployedQuote, OpenPositions: len(st.Positions),
		ClosedLabelVI: pnlClosedLabelVI, OpenLabelVI: pnlOpenLabelVI, TotalLabelVI: pnlTotalLabelVI,
		BarsLabelVI: pnlBarsLabelVI, PointsLabelVI: pnlPointsLabelVI,
		Trades: []pnlTradeView{}, Bars: []pnlBar{}, Points: []pnlPoint{}, ProblemsVI: []string{},
	}

	states, unreadable, err := listStates(p.stateDir, "")
	if err != nil {
		v.ProblemsVI = append(v.ProblemsVI, "không đọc được thư mục ý định: "+err.Error())
	}
	for _, u := range unreadable {
		v.ProblemsVI = append(v.ProblemsVI, "file ý định không đọc được (có thể thiếu một lệnh của bot): "+u)
	}

	held := map[string]autotrade.PositionView{}
	for _, pos := range st.Positions {
		held[pos.IntentID] = pos
	}

	// The venue's funding rows, for every symbol the bot holds or closed a pair
	// on inside the rows' window.
	funding := map[string]fundingView{}
	if withFunding {
		need := map[string]bool{}
		windowStart := now.Add(-fundingLookback).UnixMilli()
		for _, s := range states {
			if originOf(s.IntentID) == "autotrade" && s.Outcome == string(execution.OutcomeBothOpen) && (s.ClosedAtMs == 0 || s.ClosedAtMs >= windowStart) {
				need[s.Symbol] = true
			}
		}
		for _, pos := range st.Positions {
			need[pos.Symbol] = true
		}
		symbols := make([]string, 0, len(need))
		for s := range need {
			symbols = append(symbols, s)
		}
		sort.Strings(symbols)
		if err := p.markets.both(); err != nil {
			if len(symbols) > 0 {
				v.ProblemsVI = append(v.ProblemsVI, "thiếu credential — không đọc funding sàn: "+err.Error())
			}
		} else {
			for _, s := range symbols {
				if _, err := p.allowedSymbol(s); err != nil {
					v.ProblemsVI = append(v.ProblemsVI, "funding của "+s+" không đọc: symbol không còn trong danh sách portal")
					continue
				}
				f := p.fundingFor(ctx, s)
				if f.ErrorVI != "" {
					v.ProblemsVI = append(v.ProblemsVI, s+": "+f.ErrorVI)
				}
				funding[s] = f
			}
		}
	}

	type step struct {
		atMs int64
		cash float64
	}
	var steps []step
	firstOpenMs := int64(0)
	seen := map[string]bool{}
	for _, s := range states {
		if originOf(s.IntentID) != "autotrade" {
			continue
		}
		pos, isHeld := held[s.IntentID]
		// An open that did not end both_open, was never closed and is not held.
		// execution subtracts what an unwind closed from each leg's filled
		// quantity, so the cache shows zero fills for an unwind and for an alarm
		// alike; what tells them apart is the note an alarm leaves, and whether
		// any order was placed at all (its ids are empty only when the open was
		// refused before placing).
		notHeld := s.Outcome != string(execution.OutcomeBothOpen) && s.ClosedAtMs == 0 && !isHeld
		placed := s.SpotClientOrderID != "" || s.PerpClientOrderID != ""
		alarm := notHeld && s.NoteVI != ""
		unwound := notHeld && !alarm
		if unwound && !placed {
			// Refused before anything was sent: nothing reached the venue.
			continue
		}
		seen[s.IntentID] = true
		t := pnlTradeView{
			IntentID: s.IntentID, Symbol: s.Symbol, OpenedAtMs: s.OpenedAtMs, ClosedAtMs: s.ClosedAtMs,
			NotionalQuote: s.NotionalQuote, CapitalQuote: s.NotionalQuote * cpn, QtyCoin: s.PerpFilledQtyCoin,
			SpotEntryAvgQuote: s.SpotAvgPriceQuote, PerpEntryAvgQuote: s.PerpAvgPriceQuote,
			CloseReasonVI: s.CloseReasonVI, NoteVI: s.NoteVI,
		}
		if firstOpenMs == 0 || (s.OpenedAtMs > 0 && s.OpenedAtMs < firstOpenMs) {
			firstOpenMs = s.OpenedAtMs
		}
		switch {
		case alarm:
			// execution reports an alarm's outcome as both_flat too; the note it
			// left is the difference, and the pair may not be flat at all.
			t.Status = "alarm"
			t.NoteVI = joinNote(t.NoteVI, "BÁO ĐỘNG — lần mở này có thể để lại chân trần; đọc vị thế ở tab Thực thi thủ công, KHÔNG cộng vào số nào")
			v.AlarmOpens++
			v.Trades = append(v.Trades, t)
			continue
		case unwound:
			t.Status = "unwound"
			t.NoteVI = joinNote(t.NoteVI, "mở hỏng, đã gỡ về phẳng (hoặc sàn từ chối lệnh trước khi khớp) — phí và trượt nếu có không được file ý định ghi, KHÔNG cộng vào số nào")
			v.UnwoundOpens++
			v.Trades = append(v.Trades, t)
			continue
		case s.ClosedAtMs > 0:
			t.Status = "closed"
			if s.CommissionQuote == 0 {
				t.NoteVI = joinNote(t.NoteVI, "phí = 0: có thể lệnh đóng không đọc được phí (file ý định không phân biệt)")
			}
			if s.PairPriceDriftQuote == 0 {
				t.NoteVI = joinNote(t.NoteVI, "trôi giá = 0: có thể lệnh đóng không đọc được đủ bốn giá khớp")
			}
			t.FundingReceivedQuote, t.CommissionQuote, t.SlippageQuote = s.FundingQuote, s.CommissionQuote, s.SlippageQuote
			t.PairPriceDriftQuote, t.RealizedQuote, t.SettlementsCounted = s.PairPriceDriftQuote, s.RealizedQuote, s.SettlementsCounted
			if s.ClosedQtyCoin > 0 {
				t.QtyCoin = s.ClosedQtyCoin
			}
			cash := s.FundingQuote - s.CommissionQuote + s.PairPriceDriftQuote
			t.CashResultQuote = finitePtr(cash)
			if t.CapitalQuote > 0 {
				t.CashResultOnCapitalPct = finitePtr(cash / t.CapitalQuote * 100)
			}
			v.ClosedTrades++
			v.ClosedCashResultQuote += cash
			v.ClosedFundingQuote += s.FundingQuote
			v.ClosedCommissionQuote += s.CommissionQuote
			v.ClosedSlippageQuote += s.SlippageQuote
			v.ClosedDriftQuote += s.PairPriceDriftQuote
			v.ClosedRealizedQuote += s.RealizedQuote
			steps = append(steps, step{atMs: s.ClosedAtMs, cash: cash})
		case isHeld:
			t.Status = "open"
			p.markOpen(&t, pos, funding[s.Symbol], now, &v)
		default:
			t.Status = "untracked"
			t.NoteVI = joinNote(t.NoteVI, "file ý định chưa ghi lệnh đóng và bot không đang giữ cặp này — đọc vị thế từ sàn ở tab Thực thi thủ công")
		}
		v.Trades = append(v.Trades, t)
	}
	// A held pair whose intent file could not be read still counts.
	for _, pos := range st.Positions {
		if seen[pos.IntentID] {
			continue
		}
		t := pnlTradeView{IntentID: pos.IntentID, Symbol: pos.Symbol, OpenedAtMs: pos.OpenedAtMs, Status: "open",
			NotionalQuote: pos.NotionalQuote, CapitalQuote: pos.CapitalQuote, QtyCoin: pos.QtyCoin,
			SpotEntryAvgQuote: pos.SpotEntryAvgQuote, PerpEntryAvgQuote: pos.PerpEntryAvgQuote,
			NoteVI: "không có file ý định đọc được — hàng này chỉ từ trạng thái bot"}
		p.markOpen(&t, pos, funding[pos.Symbol], now, &v)
		v.Trades = append(v.Trades, t)
	}
	v.OpenComplete = v.OpenPriced == v.OpenPositions
	v.TotalQuote = v.ClosedCashResultQuote + v.OpenResultQuote
	v.PeakCapitalQuote = peakCapital(v.Trades, now.UnixMilli())
	if v.PeakCapitalQuote > 0 {
		v.ReturnOnPeakCapitalPct = finitePtr(v.TotalQuote / v.PeakCapitalQuote * 100)
	}
	if v.openPricedCapital > 0 {
		v.OpenReturnOnDeployedPct = finitePtr(v.OpenResultQuote / v.openPricedCapital * 100)
	}
	if v.AlarmOpens > 0 {
		v.ProblemsVI = append(v.ProblemsVI, fmt.Sprintf("%d lần mở BÁO ĐỘNG (gỡ không chứng minh được phẳng) — kiểm tra vị thế trên sàn", v.AlarmOpens))
	}
	if v.UnwoundOpens > 0 {
		v.ProblemsVI = append(v.ProblemsVI, fmt.Sprintf("%d lần mở hỏng đã gỡ về phẳng: chi phí của chúng không nằm trong số nào ở đây", v.UnwoundOpens))
	}

	v.Bars = fundingBars(funding)

	// The equity series: the cache's closes before this process's first
	// sample, the samples, and this read.
	sort.Slice(steps, func(i, j int) bool { return steps[i].atMs < steps[j].atMs })
	samples := p.pnl.list()
	firstSample := int64(math.MaxInt64)
	if len(samples) > 0 {
		firstSample = samples[0].AtMs
	}
	if firstOpenMs > 0 && firstOpenMs < firstSample {
		v.Points = append(v.Points, pnlPoint{AtMs: firstOpenMs, Source: "closed"})
	}
	cum := 0.0
	for _, s := range steps {
		cum += s.cash
		if s.atMs < firstSample {
			v.Points = append(v.Points, pnlPoint{AtMs: s.atMs, ClosedQuote: cum, TotalQuote: cum, Source: "closed"})
		}
	}
	v.Points = append(v.Points, samples...)
	// This read joins the series only when every open pair is priced: a total
	// missing a pair's figure would draw a drop that never happened.
	if v.OpenComplete {
		v.Points = append(v.Points, v.point(now))
	}
	return v
}

// peakCapital is the most capital the trades held at once: each pair ties up
// its capital from its open to its close (now, while open). An unwound open is
// left out; it held both legs for well under a second.
func peakCapital(trades []pnlTradeView, nowMs int64) float64 {
	type edge struct {
		atMs  int64
		delta float64
	}
	var edges []edge
	for _, t := range trades {
		if t.Status == "unwound" || t.Status == "alarm" || !(t.CapitalQuote > 0) || t.OpenedAtMs <= 0 {
			continue
		}
		end := t.ClosedAtMs
		if end <= 0 {
			end = nowMs
		}
		if end <= t.OpenedAtMs {
			// A pair opened and closed inside one millisecond still held its
			// capital; without this its close would sort before its own open.
			end = t.OpenedAtMs + 1
		}
		edges = append(edges, edge{t.OpenedAtMs, t.CapitalQuote}, edge{end, -t.CapitalQuote})
	}
	// At one instant a close is counted before an open: capital freed by one
	// pair is what the next one is opened with.
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].atMs != edges[j].atMs {
			return edges[i].atMs < edges[j].atMs
		}
		return edges[i].delta < edges[j].delta
	})
	cur, peak := 0.0, 0.0
	for _, e := range edges {
		cur += e.delta
		peak = max(peak, cur)
	}
	return peak
}

// markOpen fills an open pair's figures from the engine's mark and the venue's
// funding rows.
func (p *portal) markOpen(t *pnlTradeView, pos autotrade.PositionView, f fundingView, now time.Time, v *pnlView) {
	t.SettlementsCounted = pos.SettlementsSinceOpen
	t.PairDriftQuote, t.MarkedAtMs = pos.PairDriftQuote, pos.MarkedAtMs
	if pos.CapitalQuote > 0 {
		t.CapitalQuote = pos.CapitalQuote
	}
	if pos.QtyCoin > 0 {
		t.QtyCoin = pos.QtyCoin
	}
	found := false
	for _, in := range f.Intents {
		if in.IntentID != pos.IntentID {
			continue
		}
		found = true
		t.FundingRowsQuote, t.FundingRows = in.VenueRowsQuote, in.VenueRows
		var missing []string
		if in.OutsideWindow {
			missing = append(missing, "giữ lâu hơn 7 ngày đã đọc — các dòng cũ hơn không được cộng")
		}
		if in.SharedRows > 0 {
			missing = append(missing, fmt.Sprintf("%d dòng chung với ý định khác, không cộng", in.SharedRows))
		}
		if in.OtherAssetRows > 0 {
			missing = append(missing, fmt.Sprintf("%d dòng bằng tài sản khác quote, không cộng", in.OtherAssetRows))
		}
		t.FundingIncomplete = joinNote("", missing...)
	}
	if !found {
		switch {
		case f.ReadAtMs == 0:
			t.FundingIncomplete = "chưa đọc được funding sàn cho symbol này"
		case f.ErrorVI != "":
			t.FundingIncomplete = "funding sàn: " + f.ErrorVI
		}
	}
	v.OpenFundingQuote += t.FundingRowsQuote
	if pos.PairDriftQuote == nil {
		t.NoteVI = joinNote(t.NoteVI, "chưa định giá được theo mid (bot chưa quét cặp này, hoặc thiếu giá khớp vào)")
		return
	}
	if age := now.Sub(time.UnixMilli(pos.MarkedAtMs)); age > pnlMarkMaxAge {
		t.PairDriftQuote = nil
		t.NoteVI = joinNote(t.NoteVI, fmt.Sprintf("giá giữa cũ %s (bot không quét cặp này) — chưa định giá", age.Round(time.Second)))
		return
	}
	cash := *pos.PairDriftQuote + t.FundingRowsQuote
	t.CashResultQuote = finitePtr(cash)
	if t.CapitalQuote > 0 {
		t.CashResultOnCapitalPct = finitePtr(cash / t.CapitalQuote * 100)
	}
	v.OpenPriced++
	v.OpenDriftQuote += *pos.PairDriftQuote
	v.OpenResultQuote += cash
	v.openPricedCapital += t.CapitalQuote
}

// point is this read as a point of the equity series.
func (v pnlView) point(at time.Time) pnlPoint {
	open := v.OpenResultQuote
	pt := pnlPoint{AtMs: at.UnixMilli(), ClosedQuote: v.ClosedCashResultQuote, TotalQuote: v.TotalQuote,
		CapitalDeployedQuote: v.CapitalDeployedQuote, OpenPositions: v.OpenPositions, OpenComplete: v.OpenComplete, Source: "now"}
	if v.OpenPositions == 0 || v.OpenPriced > 0 {
		pt.OpenQuote = &open
	}
	return pt
}

// fundingBars sums, per settlement stamp, the rows in the quote asset that
// exactly one of the bot's intents held across.
func fundingBars(funding map[string]fundingView) []pnlBar {
	byStamp := map[int64]*pnlBar{}
	for symbol, f := range funding {
		if f.QuoteAsset == "" {
			continue
		}
		for _, row := range f.Rows {
			if len(row.IntentIDs) != 1 || originOf(row.IntentIDs[0]) != "autotrade" || row.Asset != f.QuoteAsset {
				continue
			}
			b := byStamp[row.SettledAtMs]
			if b == nil {
				b = &pnlBar{SettledAtMs: row.SettledAtMs}
				byStamp[row.SettledAtMs] = b
			}
			b.IncomeQuote += row.IncomeQtyInAsset
			b.Rows = append(b.Rows, pnlBarDetail{Symbol: symbol, IntentID: row.IntentIDs[0], IncomeQuote: row.IncomeQtyInAsset})
		}
	}
	out := make([]pnlBar, 0, len(byStamp))
	for _, b := range byStamp {
		sort.Slice(b.Rows, func(i, j int) bool { return b.Rows[i].Symbol < b.Rows[j].Symbol })
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SettledAtMs < out[j].SettledAtMs })
	return out
}

func finitePtr(x float64) *float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return nil
	}
	return &x
}

func joinNote(base string, more ...string) string {
	for _, m := range more {
		if m == "" {
			continue
		}
		if base == "" {
			base = m
			continue
		}
		base += " · " + m
	}
	return base
}

// samplePnL records one equity sample while the bot runs or holds a pair.
func (p *portal) samplePnL(ctx context.Context, st autotrade.StatusView) bool {
	if !st.Enabled && len(st.Positions) == 0 {
		return false
	}
	readCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	v := p.buildPnL(readCtx, st, true)
	if !v.OpenComplete {
		// A sample missing an open pair's figure would draw a drop that never
		// happened; the gap is the honest line.
		return false
	}
	return p.pnl.record(v.point(p.now()), p.now())
}
