package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"futures-arbitrage-scanner/internal/paper"
	"futures-arbitrage-scanner/internal/store"
	"futures-arbitrage-scanner/internal/strategy"
)

// Build turns the journal and the corpus into a priced ledger, from scratch,
// every time. Rebuilding rather than updating is deliberate: the ledger is
// then a pure function of the database and can be reconstructed for any
// window that has already gone by (PLAN 4.3 acceptance 2), and a restart of
// this process loses nothing because it keeps nothing.

// Inputs is what one build reads.
type Inputs struct {
	DB           *store.Store
	FromMs       int64
	ToMs         int64 // 0 = NowMs
	NowMs        int64
	CapitalQuote float64
	MarkEvery    time.Duration
	FromNoteVI   string
}

// Report is the ledger as served: the account, every position, every event,
// the equity curve, and — in words — what is real and what is fake.
type Report struct {
	Mode      string `json:"mode"` // always "paper"
	BuiltAtMs int64  `json:"built_at_ms"`

	WindowFromMs   int64  `json:"window_from_ms"`
	WindowToMs     int64  `json:"window_to_ms"`
	WindowFromNote string `json:"window_from_note_vi"`

	CapitalQuote     float64 `json:"capital_quote"`
	CashQuote        float64 `json:"cash_quote"`
	EquityQuote      float64 `json:"equity_quote"`
	OpenPnLQuote     float64 `json:"open_pnl_quote"` // open positions: basis − entry fees + funding ALREADY credited
	RealizedQuote    float64 `json:"realized_quote"`
	FundingQuote     float64 `json:"funding_quote"`
	FundingSpecial   int     `json:"funding_special"` // Binance dividend rows credited here, dropped by the rule
	FeesPaidQuote    float64 `json:"fees_paid_quote"`
	MaxDrawdownQuote float64 `json:"max_drawdown_quote"`

	JournalRows int `json:"journal_rows"`
	Enters      int `json:"journal_enters"`
	Exits       int `json:"journal_exits"`
	Holds       int `json:"journal_holds"`
	Skips       int `json:"journal_skips"`
	Refusals    int `json:"refusals"`
	Anomalies   int `json:"anomalies"`

	Open   []*paper.Position   `json:"open"`
	Closed []*paper.Position   `json:"closed"`
	Events []paper.Event       `json:"events"`
	Equity []paper.EquityPoint `json:"equity"`

	RealVI        []string `json:"real_vi"`
	FakeVI        []string `json:"fake_vi"`
	AssumptionsVI []string `json:"assumptions_vi"`
	LabelVI       string   `json:"label_vi"`
}

// SummaryLine is the one-line log form.
func (r Report) SummaryLine() string {
	return fmt.Sprintf("PAPER · %d hàng nhật ký (%d enter, %d exit) · %d vị thế mở, %d đã đóng, %d từ chối · equity %.2f / vốn %.0f · funding %.2f · phí %.2f",
		r.JournalRows, r.Enters, r.Exits, len(r.Open), len(r.Closed), r.Refusals, r.EquityQuote, r.CapitalQuote, r.FundingQuote, r.FeesPaidQuote)
}

// journalParams is the part of params_json a paper fill needs.
type journalParams struct {
	NotionalQuote  float64 `json:"notional_quote"`
	MaxBookAgeMin  float64 `json:"max_book_age_min"`
	PerpMarginFrac float64 `json:"perp_margin_frac"`
	Fees           *struct {
		Spot *feeJSON `json:"spot"`
		Perp *feeJSON `json:"perp"`
	} `json:"fees"`
}

type feeJSON struct {
	Source   string  `json:"source"`
	TakerBps float64 `json:"taker_bps"`
	Verified bool    `json:"verified"`
}

func feeState(f *feeJSON) paper.FeeState {
	if f == nil {
		return paper.FeeState{}
	}
	return paper.FeeState{Source: f.Source, TakerBps: f.TakerBps, Verified: f.Verified, Known: true}
}

// timeline event kinds, in the order they apply at one instant: a settlement
// is collected before a close at the same stamp, a close before an open (a
// pair re-entered at the instant it left), and marks after everything.
const (
	evFunding = iota
	evClose
	evOpen
	evMark
)

type timelineEvent struct {
	atMs int64
	kind int
	row  store.SignalRecord
	fund store.FundingRow
}

// markMaxAge bounds how old a price sample may be and still mark a position
// or price a settlement's mark: the sampler runs every 30s, so 15 minutes of
// silence is a dead feed, and a dead feed's last price is not the price now.
const markMaxAge = 15 * time.Minute

// Build reads the window and prices it.
func Build(ctx context.Context, in Inputs) (Report, error) {
	toMs := in.ToMs
	if toMs == 0 {
		toMs = in.NowMs
	}
	if toMs <= in.FromMs {
		return Report{}, fmt.Errorf("window %s → %s is empty", stampMs(in.FromMs), stampMs(toMs))
	}
	// The tally counts in SQL and the rows read are the enter/exit ones
	// without their checks_json: a rebuild every five minutes on the machine
	// running the live scanner must not page a fortnight of reasoning.
	tally, err := in.DB.SignalActionTally(ctx, in.FromMs, toMs)
	if err != nil {
		return Report{}, err
	}
	rows, err := in.DB.SignalPositionRows(ctx, in.FromMs, toMs)
	if err != nil {
		return Report{}, err
	}
	quoteOf, err := in.DB.InstrumentQuoteAssets(ctx)
	if err != nil {
		return Report{}, err
	}

	report := Report{
		Mode: "paper", BuiltAtMs: in.NowMs, WindowFromMs: in.FromMs, WindowToMs: toMs, WindowFromNote: in.FromNoteVI,
		CapitalQuote: in.CapitalQuote,
		Enters:       tally[string(strategy.ActionEnter)], Exits: tally[string(strategy.ActionExit)],
		Holds: tally[string(strategy.ActionHold)], Skips: tally[string(strategy.ActionSkip)],
		RealVI:        realVI(),
		FakeVI:        fakeVI(),
		AssumptionsVI: assumptionsVI(in),
		LabelVI:       "PAPER: giả định khớp đủ hai chân ở sổ đo được, chưa có thanh lý / khớp một phần / độ trễ / sự cố API.",
	}

	var events []timelineEvent
	symbols := map[string]bool{}
	perps := map[string]bool{}
	firstDecisionMs := int64(0)
	for _, n := range tally {
		report.JournalRows += n
	}
	for _, r := range rows {
		switch r.Action {
		case string(strategy.ActionEnter):
			events = append(events, timelineEvent{atMs: r.EvaluatedAtMs, kind: evOpen, row: r})
		case string(strategy.ActionExit):
			events = append(events, timelineEvent{atMs: r.EvaluatedAtMs, kind: evClose, row: r})
		default:
			continue
		}
		symbols[r.Symbol] = true
		perps[r.Symbol+"|"+r.PerpSource] = true
		if firstDecisionMs == 0 || r.EvaluatedAtMs < firstDecisionMs {
			firstDecisionMs = r.EvaluatedAtMs
		}
	}
	// Settlements of every series that had a position decision in the window.
	// Whether a given settlement is collected is the ledger's call — it knows
	// which position was open at that instant.
	for symbol := range symbols {
		frows, err := in.DB.FundingHistory(ctx, symbol, in.FromMs, toMs)
		if err != nil {
			return Report{}, err
		}
		for _, fr := range frows {
			if !perps[fr.Symbol+"|"+fr.Source] {
				continue
			}
			events = append(events, timelineEvent{atMs: fr.SettledAtMs, kind: evFunding, fund: fr})
		}
	}
	// Marks on a grid from the first decision to the window's end, so an
	// empty journal costs nothing and a held position is valued hourly; the
	// closing mark at the window's end is unconditional, because the
	// report's equity is read off the last point and a cash move with no
	// mark after it would leave equity at the starting capital.
	if firstDecisionMs > 0 {
		if in.MarkEvery > 0 {
			step := in.MarkEvery.Milliseconds()
			for at := firstDecisionMs; at < toMs; at += step {
				events = append(events, timelineEvent{atMs: at, kind: evMark})
			}
		}
		events = append(events, timelineEvent{atMs: toMs, kind: evMark})
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].atMs != events[j].atMs {
			return events[i].atMs < events[j].atMs
		}
		return events[i].kind < events[j].kind
	})

	ledger := paper.New(in.CapitalQuote)
	price := func(atMs int64) paper.PriceFn {
		return func(source, symbol string) (float64, bool) {
			sample, ok, err := in.DB.PriceSampleAt(ctx, source, symbol, atMs)
			if err != nil || !ok || sample.MidPriceQuote <= 0 {
				return 0, false
			}
			if atMs-sample.SampledAtMs > markMaxAge.Milliseconds() {
				return 0, false
			}
			return sample.MidPriceQuote, true
		}
	}
	for _, ev := range events {
		switch ev.kind {
		case evOpen:
			applyOpen(ctx, in.DB, ledger, ev.row, quoteOf)
		case evClose:
			applyClose(ctx, in.DB, ledger, ev.row)
		case evFunding:
			applyFunding(ledger, ev.fund, price(ev.fund.SettledAtMs))
		case evMark:
			ledger.Mark(ev.atMs, price(ev.atMs))
		}
	}

	report.CashQuote = ledger.CashQuote
	report.FeesPaidQuote = ledger.FeesPaidQuote
	report.Refusals = ledger.Refusals
	report.Open = ledger.OpenPositions() // already in open-time order
	report.Closed = ledger.Closed
	report.Events = ledger.Events
	report.Equity = ledger.Equity
	for _, e := range ledger.Events {
		if e.Kind == paper.EventAnomaly {
			report.Anomalies++
		}
	}
	for _, p := range report.Open {
		if p.Credits == nil {
			p.Credits = []paper.FundingCredit{}
		}
		report.OpenPnLQuote += p.OpenPnLQuote
		report.FundingQuote += p.FundingQuote
		report.FundingSpecial += p.FundingSpecial
	}
	for _, p := range report.Closed {
		if p.Credits == nil {
			p.Credits = []paper.FundingCredit{}
		}
		report.RealizedQuote += p.RealizedPnLQuote
		report.FundingQuote += p.FundingQuote
		report.FundingSpecial += p.FundingSpecial
	}
	report.EquityQuote = in.CapitalQuote
	if n := len(ledger.Equity); n > 0 {
		report.EquityQuote = ledger.Equity[n-1].EquityQuote
		for _, pt := range ledger.Equity {
			if pt.DrawdownFromPeakQuote > report.MaxDrawdownQuote {
				report.MaxDrawdownQuote = pt.DrawdownFromPeakQuote
			}
		}
	}
	return report, nil
}

func applyOpen(ctx context.Context, db *store.Store, ledger *paper.Ledger, r store.SignalRecord, quoteOf map[string]string) {
	var jp journalParams
	if err := json.Unmarshal([]byte(r.ParamsJSON), &jp); err != nil {
		// An unreadable params_json leaves every field zero; the ledger then
		// refuses on the first thing it checks (the notional), and the event
		// carries the row's identity — the reason reads "notional_quote 0",
		// which is what an unreadable row IS to a pricer.
		jp = journalParams{}
	}
	req := paper.OpenRequest{
		Symbol: r.Symbol, PerpSource: r.PerpSource, SpotSource: r.SpotSource,
		SpotQuoteAsset: quoteOf[r.SpotSource+"|"+r.Symbol], PerpQuoteAsset: quoteOf[r.PerpSource+"|"+r.Symbol],
		AtMs: r.EvaluatedAtMs, NotionalQuote: jp.NotionalQuote,
		MaxBookAge:          time.Duration(jp.MaxBookAgeMin * float64(time.Minute)),
		PerpMarginFrac:      jp.PerpMarginFrac,
		ProjectedNetAPRFrac: r.NetAPRFrac, ProjectedNetAPROK: r.NetAPROK, JournalCostTotalPct: r.CostTotalPct,
	}
	if jp.Fees != nil {
		req.SpotFee, req.PerpFee = feeState(jp.Fees.Spot), feeState(jp.Fees.Perp)
	}
	if r.SpotSource != "" {
		req.SpotBook, _, _ = db.DepthSnapshotAt(ctx, r.SpotSource, r.Symbol, r.EvaluatedAtMs)
	}
	req.PerpBook, _, _ = db.DepthSnapshotAt(ctx, r.PerpSource, r.Symbol, r.EvaluatedAtMs)
	_ = ledger.Open(req) // refusals and anomalies are on the ledger's event log
}

func applyClose(ctx context.Context, db *store.Store, ledger *paper.Ledger, r store.SignalRecord) {
	var jp journalParams
	_ = json.Unmarshal([]byte(r.ParamsJSON), &jp)
	req := paper.CloseRequest{
		Symbol: r.Symbol, PerpSource: r.PerpSource, AtMs: r.EvaluatedAtMs,
		MaxBookAge: time.Duration(jp.MaxBookAgeMin * float64(time.Minute)),
	}
	if jp.Fees != nil {
		req.SpotFee, req.PerpFee = feeState(jp.Fees.Spot), feeState(jp.Fees.Perp)
	}
	// The spot leg is the POSITION's, not the exit row's: a row with no spot
	// source (registry not refreshed at that tick) still has to sell the
	// spot the position holds.
	spotSource := r.SpotSource
	if p, ok := ledger.Position(r.Symbol, r.PerpSource); ok {
		spotSource = p.SpotSource
	}
	if spotSource != "" {
		req.SpotBook, _, _ = db.DepthSnapshotAt(ctx, spotSource, r.Symbol, r.EvaluatedAtMs)
	}
	req.PerpBook, _, _ = db.DepthSnapshotAt(ctx, r.PerpSource, r.Symbol, r.EvaluatedAtMs)
	_ = ledger.Close(req)
}

// applyFunding resolves the mark a settlement was charged on and hands the
// settlement to the ledger. Binance publishes the mark beside the historical
// rate; every other venue does not, and the closest sampled mid at or before
// the stamp stands in for it — labelled, because it is a stand-in.
func applyFunding(ledger *paper.Ledger, fr store.FundingRow, price paper.PriceFn) {
	s := paper.FundingSettlement{
		SettledAtMs: fr.SettledAtMs, Model: fr.Model, RatePerIntervalFrac: fr.RatePerIntervalFrac,
		IntervalSec: fr.IntervalSec, RateType: fr.RateType,
	}
	switch {
	case fr.MarkPriceQuote > 0:
		s.MarkPriceQuote, s.MarkSourceVI = fr.MarkPriceQuote, "mark sàn công bố"
	default:
		if mid, ok := price(fr.Source, fr.Symbol); ok {
			s.MarkPriceQuote, s.MarkSourceVI = mid, "mid lấy mẫu ≤15 phút trước mốc (sàn không công bố mark)"
		} else if p, held := ledger.Position(fr.Symbol, fr.Source); held && p.PerpMarkQuote > 0 {
			s.MarkPriceQuote, s.MarkSourceVI = p.PerpMarkQuote, "mark cuối của sổ giấy (KHÔNG có mẫu giá trong 15 phút trước mốc)"
		}
	}
	ledger.CreditFunding(fr.Symbol, fr.Source, s)
}

func oldestJournalRow(ctx context.Context, db *store.Store) (int64, error) {
	var oldest *int64
	if err := db.DB().QueryRowContext(ctx, `SELECT MIN(evaluated_at_ms) FROM signal_journal`).Scan(&oldest); err != nil {
		return 0, fmt.Errorf("oldest journal row: %w", err)
	}
	if oldest == nil {
		return 0, fmt.Errorf("the signal journal is empty — nothing to price")
	}
	return *oldest, nil
}

func realVI() []string {
	return []string{
		"Giá, sổ lệnh, funding rate, quy tắc hợp đồng: đọc từ kho dữ liệu thật của scanner.",
		"Đường quyết định: production EvaluateEntry / EvaluateExit, đúng như tiến trình 3.5 đã ghi vào signal_journal — sổ này KHÔNG quyết định gì thêm.",
	}
}

func fakeVI() []string {
	return []string{
		"Khớp lệnh: ước từ sổ ĐO ĐƯỢC qua strategy.EstimateFill (mua ở ask, bán ở bid), không có lệnh nào gửi đi.",
		"Vị thế và số dư: sổ ảo trong bộ nhớ, dựng lại từ đầu mỗi lần từ nhật ký — không có tài khoản nào ở sàn.",
		"Ghi có funding: mark × size × rate tại mốc settle; mark của sàn không công bố được thay bằng mid lấy mẫu và ghi rõ.",
	}
}

func assumptionsVI(in Inputs) []string {
	return []string{
		fmt.Sprintf("Vốn ảo khởi điểm %.0f quote; chân perp ký quỹ trọn notional khi perp_margin_frac = 0 (vốn = N·(1+f), trần 2×).", in.CapitalQuote),
		"Sổ lệnh dùng để khớp là snapshot GẦN NHẤT TRƯỚC hoặc ĐÚNG mốc quyết định (không bao giờ sau), trong hạn max_book_age_min của chính hàng đó; lệnh vượt độ sâu 0,5% bị TỪ CHỐI như production — lúc thoát, kích thước lệnh là qty × mid HIỆN TẠI, không phải notional lúc vào.",
		"sampled_at_ms của độ sâu là mốc BẮT ĐẦU lượt quét (~117 lượt tải, nghỉ 150 ms, kéo dài 30–90 s), nên luật 'không sau quyết định' lọc ở độ phân giải LƯỢT QUÉT chứ không phải từng lượt tải; cột fetched_at_ms là việc sau cổng 3.5.",
		"Mốc Binance rateType=Special (cổ tức) ĐƯỢC ghi có ở đây vì tài khoản thật nhận nó; luật vào/ra và backtest LOẠI nó, nên funding của sổ giấy có thể cao hơn backtest đúng bằng các mốc đó — đếm ở funding_special.",
		"Phí taker lấy từ params_json.fees của chính hàng nhật ký, không đọc config.yaml lúc chạy; hàng không ghi phí (lần chạy 1) bị từ chối.",
		"Funding là sự kiện rời rạc: chỉ mốc settle sau lúc mở và tới lúc đóng; không pro-rate; chuỗi continuous (Paradex) không ghi có.",
		fmt.Sprintf("Đánh dấu theo mid mỗi %s; mẫu giá quá 15 phút coi như không có và điểm equity ghi 'stale'.", in.MarkEvery),
		"Cặp KHÁC QUOTE (perp USD / spot USDT) cộng hai đồng như một và được gắn cờ quote_bridged — rủi ro USDT/USD không được trừ ở đâu.",
		"CHƯA có: khớp một phần, vị trí hàng đợi, độ trễ, lệnh bị từ chối thật, thanh lý, margin call, sự cố API (Bước 4.4/4.6 đo những thứ này).",
	}
}
