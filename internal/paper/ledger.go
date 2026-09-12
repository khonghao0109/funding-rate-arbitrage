package paper

import (
	"fmt"
	"math"
	"sort"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/strategy"
)

const (
	pctPerUnit = 100
	bpsPerUnit = 10000
	msPerDay   = 24 * 60 * 60 * 1000
)

// FeeState is one leg's taker schedule AS THE JOURNAL ROW RECORDED IT
// (params_json.fees). Known is false when the row carried no fee state at
// all — the first run's rows — and such a row cannot be priced here: "not
// recorded" is not "free", the same rule fees.Schedule.Verified follows.
type FeeState struct {
	Source   string  `json:"source"`
	TakerBps float64 `json:"taker_bps"`
	Verified bool    `json:"verified"`
	Known    bool    `json:"known"`
}

// Leg is one fill of one side on one book.
type Leg struct {
	Source string        `json:"source"`
	Side   strategy.Side `json:"side"`

	// BookSampledAtMs is when the book this fill was priced on was measured
	// — at or before the decision, by construction, and carried so a reader
	// can see how far before.
	BookSampledAtMs int64   `json:"book_sampled_at_ms"`
	MidPriceQuote   float64 `json:"mid_price_quote"`
	// SlippagePct is the fill's average distance from the MID in percent,
	// half the spread included (strategy.FillEstimate.SlippagePct).
	SlippagePct       float64 `json:"slippage_pct"`
	FillPriceQuote    float64 `json:"fill_price_quote"`
	QtyCoin           float64 `json:"qty_coin"`
	NotionalQuote     float64 `json:"notional_quote"`
	TakerFeeBps       float64 `json:"taker_fee_bps"`
	FeeQuote          float64 `json:"fee_quote"`
	DepthIsLowerBound bool    `json:"depth_is_lower_bound"`
	NoteVI            string  `json:"note_vi"`
}

// FundingCredit is one settlement the position collected.
type FundingCredit struct {
	SettledAtMs         int64   `json:"settled_at_ms"`
	RatePerIntervalFrac float64 `json:"rate_per_interval_frac"`
	IntervalSec         int64   `json:"interval_sec"`
	MarkPriceQuote      float64 `json:"mark_price_quote"`
	MarkSourceVI        string  `json:"mark_source_vi"`
	AmountQuote         float64 `json:"amount_quote"`
	RateType            string  `json:"rate_type"`
}

// FundingSettlement is one settled rate as the caller resolved it: the
// venue's row plus a mark price, because the amount is mark × size × rate
// (DATA-REQUIREMENTS §11.2 trap 4) and only Binance publishes the mark beside
// the historical rate. MarkSourceVI says where the caller got it.
type FundingSettlement struct {
	SettledAtMs         int64
	Model               exchanges.FundingModel
	RatePerIntervalFrac float64
	IntervalSec         int64
	RateType            string
	MarkPriceQuote      float64
	MarkSourceVI        string
}

// Position is one paper position: two legs, opened together, closed together.
type Position struct {
	Symbol     string `json:"symbol"`
	PerpSource string `json:"perp_source"`
	SpotSource string `json:"spot_source"`

	// QuoteBridged marks a pair whose two legs quote different assets (a USD
	// perp against a USDT spot). Every quote figure on such a position adds
	// the two as one currency, which nothing here deducts — the flag is what
	// stops the sum from reading as a measurement (CLAUDE.md, quote bridging).
	SpotQuoteAsset string `json:"spot_quote_asset"`
	PerpQuoteAsset string `json:"perp_quote_asset"`
	QuoteBridged   bool   `json:"quote_bridged"`

	OpenedAtMs int64 `json:"opened_at_ms"`
	ClosedAtMs int64 `json:"closed_at_ms"` // 0 while open

	QtyCoin       float64 `json:"qty_coin"`
	NotionalQuote float64 `json:"notional_quote"` // as requested by the decision
	// PerpMarginFrac is the collateral posted on the short leg as a fraction
	// of its notional; 0 means the whole notional (an unlevered short), which
	// is what every journal so far ran with. MarginQuote is what was posted.
	PerpMarginFrac float64 `json:"perp_margin_frac"`
	MarginQuote    float64 `json:"margin_quote"`

	EntrySpot Leg `json:"entry_spot"`
	EntryPerp Leg `json:"entry_perp"`
	ExitSpot  Leg `json:"exit_spot"` // zero while open
	ExitPerp  Leg `json:"exit_perp"`

	// What the decision PROJECTED when it opened the position, carried so it
	// can stand beside what the paper position actually made (PLAN 4.3
	// acceptance 2). OK false means the row had no figure, not a rate of 0.
	ProjectedNetAPRFrac float64 `json:"projected_net_apr_frac"`
	ProjectedNetAPROK   bool    `json:"projected_net_apr_ok"`
	// JournalCostTotalPct is the round trip the LIVE path priced on its live
	// book; PaperEntryCostPct/PaperRoundTripCostPct are what this ledger
	// priced on the stored one. The two should agree closely — the live book
	// was the stored book's source — and the gap is a consistency check.
	JournalCostTotalPct   float64 `json:"journal_cost_total_pct"`
	PaperEntryCostPct     float64 `json:"paper_entry_cost_pct"`
	PaperRoundTripCostPct float64 `json:"paper_round_trip_cost_pct"` // 0 while open

	FundingQuote             float64         `json:"funding_quote"`
	FundingSettlements       int             `json:"funding_settlements"`
	FundingSpecial           int             `json:"funding_special"`
	FundingContinuousSkipped int             `json:"funding_continuous_skipped"`
	FundingUnpriced          int             `json:"funding_unpriced"`
	Credits                  []FundingCredit `json:"credits"`

	// The newest mark. Basis P&L is the pair marked to mid: coin-flat, so it
	// moves only when perp and spot move apart — the unrealized loss PLAN 4.3
	// wants visible before funding has covered it.
	MarkedAtMs    int64   `json:"marked_at_ms"`
	SpotMarkQuote float64 `json:"spot_mark_quote"`
	PerpMarkQuote float64 `json:"perp_mark_quote"`
	MarkIsStale   bool    `json:"mark_is_stale"`
	BasisPnLQuote float64 `json:"basis_pnl_quote"`

	// RealizedPnLQuote is set at close: proceeds − cost on both legs, all four
	// fees, plus funding. OpenPnLQuote is the same identity at the mark while
	// open — basis P&L minus the entry fees PLUS the funding already credited
	// to cash — so it INCLUDES money that has already been paid; exit fees
	// and exit slippage are not yet charged. Named "open", not "unrealized",
	// because part of it is realized.
	RealizedPnLQuote float64 `json:"realized_pnl_quote"`
	OpenPnLQuote     float64 `json:"open_pnl_quote"`

	// QuoteAssetsKnown is false when the instrument snapshot could not name
	// BOTH legs' quote assets; QuoteBridged is then also false, and a reader
	// must not take that for "same quote" — 0 means not known.
	QuoteAssetsKnown bool `json:"quote_assets_known"`

	ExitRefusedAtMs int64  `json:"exit_refused_at_ms"`
	ExitRefusedVI   string `json:"exit_refused_vi"`
}

// Open reports whether the position is still held.
func (p *Position) Open() bool { return p.ClosedAtMs == 0 }

// HeldDays is the holding time so far, or to the close.
func (p *Position) HeldDays(nowMs int64) float64 {
	end := nowMs
	if !p.Open() {
		end = p.ClosedAtMs
	}
	if end <= p.OpenedAtMs {
		return 0
	}
	return float64(end-p.OpenedAtMs) / msPerDay
}

// EventKind names what an Event records.
type EventKind string

const (
	EventOpen        EventKind = "open"
	EventClose       EventKind = "close"
	EventFunding     EventKind = "funding"
	EventRefuseOpen  EventKind = "refuse_open"
	EventRefuseClose EventKind = "refuse_close"
	EventAnomaly     EventKind = "anomaly"
)

// Event is one thing the ledger did or refused to do, in time order.
type Event struct {
	AtMs        int64     `json:"at_ms"`
	Kind        EventKind `json:"kind"`
	Symbol      string    `json:"symbol"`
	PerpSource  string    `json:"perp_source"`
	AmountQuote float64   `json:"amount_quote"`
	DetailVI    string    `json:"detail_vi"`
}

// EquityPoint is the account at one instant.
type EquityPoint struct {
	AtMs                  int64   `json:"at_ms"`
	CashQuote             float64 `json:"cash_quote"`
	EquityQuote           float64 `json:"equity_quote"`
	OpenPnLQuote          float64 `json:"open_pnl_quote"`
	OpenPositions         int     `json:"open_positions"`
	StaleMarks            int     `json:"stale_marks"`
	DrawdownFromPeakQuote float64 `json:"drawdown_from_peak_quote"`
}

// OpenRequest is everything one opening needs, all of it from the journal
// row that decided it and the store the row was written beside.
type OpenRequest struct {
	Symbol     string
	PerpSource string
	SpotSource string

	SpotQuoteAsset string
	PerpQuoteAsset string

	AtMs           int64 // the row's evaluated_at_ms
	NotionalQuote  float64
	MaxBookAge     time.Duration // the row's max_book_age_min; 0 = no budget
	PerpMarginFrac float64

	SpotFee FeeState
	PerpFee FeeState

	SpotBook depth.Summary
	PerpBook depth.Summary

	ProjectedNetAPRFrac float64
	ProjectedNetAPROK   bool
	JournalCostTotalPct float64
}

// CloseRequest is everything one closing needs.
type CloseRequest struct {
	Symbol     string
	PerpSource string
	AtMs       int64
	MaxBookAge time.Duration

	SpotFee FeeState
	PerpFee FeeState

	SpotBook depth.Summary
	PerpBook depth.Summary
}

// Ledger is the virtual account. It is NOT serialized as a whole — the open
// set is a map and the process's Report is what goes on the wire — so it
// carries no JSON tags on purpose: a json.Marshal of it would silently omit
// the open positions.
type Ledger struct {
	CapitalQuote float64
	CashQuote    float64

	open   map[string]*Position
	Closed []*Position

	Events []Event
	Equity []EquityPoint

	// Counters a summary needs without walking the lists.
	Refusals      int
	FeesPaidQuote float64
	peak          float64
}

// New starts an account holding capitalQuote in cash and nothing else.
func New(capitalQuote float64) *Ledger {
	// Empty slices rather than nil: the report is served as JSON and a
	// reader must see [] for "none", never null for "unknown".
	return &Ledger{CapitalQuote: capitalQuote, CashQuote: capitalQuote, open: map[string]*Position{},
		Closed: []*Position{}, Events: []Event{}, Equity: []EquityPoint{}, peak: capitalQuote}
}

func key(symbol, perp string) string { return symbol + "|" + perp }

// Position returns an open position, if any.
func (l *Ledger) Position(symbol, perp string) (*Position, bool) {
	p, ok := l.open[key(symbol, perp)]
	return p, ok
}

// OpenPositions lists open positions in a fixed order: by open time, then
// symbol and perp source.
func (l *Ledger) OpenPositions() []*Position {
	out := make([]*Position, 0, len(l.open))
	for _, p := range l.open {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OpenedAtMs != out[j].OpenedAtMs {
			return out[i].OpenedAtMs < out[j].OpenedAtMs
		}
		return out[i].Symbol+"|"+out[i].PerpSource < out[j].Symbol+"|"+out[j].PerpSource
	})
	return out
}

func (l *Ledger) note(at int64, kind EventKind, symbol, perp string, amount float64, detail string) {
	l.Events = append(l.Events, Event{AtMs: at, Kind: kind, Symbol: symbol, PerpSource: perp, AmountQuote: amount, DetailVI: detail})
}

// Open fills both legs of a decision or refuses the whole thing.
func (l *Ledger) Open(req OpenRequest) error {
	k := key(req.Symbol, req.PerpSource)
	if _, held := l.open[k]; held {
		// The journal says enter while this ledger already holds the pair:
		// a restart that reseeded differently, or a row the pairing missed.
		// Recorded, never doubled.
		msg := "nhật ký ghi enter trong khi sổ giấy đang giữ vị thế này — bỏ qua, không mở vị thế thứ hai"
		l.note(req.AtMs, EventAnomaly, req.Symbol, req.PerpSource, 0, msg)
		return fmt.Errorf("paper: %s/%s: %s", req.Symbol, req.PerpSource, msg)
	}
	if req.SpotSource == "" {
		return l.refuseOpen(req, "hàng nhật ký không có chân spot — không có vị thế để mở")
	}
	if !isPositiveFinite(req.NotionalQuote) {
		return l.refuseOpen(req, fmt.Sprintf("notional_quote %v không phải số dương hữu hạn", req.NotionalQuote))
	}
	if reason := feeRefusal(req.SpotFee, req.PerpFee); reason != "" {
		return l.refuseOpen(req, reason)
	}
	spot, reason := fill(req.SpotBook, strategy.SideBuy, req.NotionalQuote, req.AtMs, req.MaxBookAge, req.SpotFee)
	if reason != "" {
		return l.refuseOpen(req, "chân MUA spot: "+reason)
	}
	// Same COIN quantity on the perp, priced on the perp's own book: the
	// notional there differs from the spot leg's by the basis, which is the
	// point of holding both.
	perp, reason := fillQty(req.PerpBook, strategy.SideSell, spot.QtyCoin, req.AtMs, req.MaxBookAge, req.PerpFee)
	if reason != "" {
		return l.refuseOpen(req, "chân BÁN perp: "+reason)
	}

	marginFrac := req.PerpMarginFrac
	if marginFrac <= 0 {
		marginFrac = 1 // an unlevered short posts its whole notional
	}
	margin := marginFrac * perp.NotionalQuote
	need := spot.NotionalQuote + spot.FeeQuote + margin + perp.FeeQuote
	if l.CashQuote < need {
		return l.refuseOpen(req, fmt.Sprintf("vốn ảo không đủ: cần %.2f (spot %.2f + phí %.2f + ký quỹ perp %.2f + phí %.2f), còn %.2f",
			need, spot.NotionalQuote, spot.FeeQuote, margin, perp.FeeQuote, l.CashQuote))
	}

	p := &Position{
		Symbol: req.Symbol, PerpSource: req.PerpSource, SpotSource: req.SpotSource,
		SpotQuoteAsset: req.SpotQuoteAsset, PerpQuoteAsset: req.PerpQuoteAsset,
		QuoteAssetsKnown: req.SpotQuoteAsset != "" && req.PerpQuoteAsset != "",
		QuoteBridged:     req.SpotQuoteAsset != "" && req.PerpQuoteAsset != "" && req.SpotQuoteAsset != req.PerpQuoteAsset,
		OpenedAtMs:       req.AtMs,
		QtyCoin:          spot.QtyCoin, NotionalQuote: req.NotionalQuote,
		PerpMarginFrac: req.PerpMarginFrac, MarginQuote: margin,
		EntrySpot: spot, EntryPerp: perp,
		ProjectedNetAPRFrac: req.ProjectedNetAPRFrac, ProjectedNetAPROK: req.ProjectedNetAPROK,
		JournalCostTotalPct: req.JournalCostTotalPct,
		PaperEntryCostPct:   spot.SlippagePct + perp.SlippagePct + (spot.TakerFeeBps+perp.TakerFeeBps)/bpsPerUnit*pctPerUnit,
		MarkedAtMs:          req.AtMs, SpotMarkQuote: spot.MidPriceQuote, PerpMarkQuote: perp.MidPriceQuote,
	}
	l.CashQuote -= need
	l.FeesPaidQuote += spot.FeeQuote + perp.FeeQuote
	l.open[k] = p
	p.mark(req.AtMs, spot.MidPriceQuote, perp.MidPriceQuote, false)
	l.note(req.AtMs, EventOpen, req.Symbol, req.PerpSource, -(spot.FeeQuote + perp.FeeQuote), fmt.Sprintf(
		"MỞ (PAPER): mua %.6f %s spot ở %.4f (%s, slippage %.4f%%, phí %.2f) · bán perp ở %.4f (%s, slippage %.4f%%, phí %.2f) · ký quỹ %.2f",
		p.QtyCoin, req.Symbol, spot.FillPriceQuote, req.SpotSource, spot.SlippagePct, spot.FeeQuote,
		perp.FillPriceQuote, req.PerpSource, perp.SlippagePct, perp.FeeQuote, margin))
	return nil
}

func (l *Ledger) refuseOpen(req OpenRequest, reason string) error {
	l.Refusals++
	l.note(req.AtMs, EventRefuseOpen, req.Symbol, req.PerpSource, 0, "TỪ CHỐI mở (PAPER): "+reason)
	return fmt.Errorf("paper: open %s/%s refused: %s", req.Symbol, req.PerpSource, reason)
}

// Close fills both exit legs or leaves the position open and says why.
func (l *Ledger) Close(req CloseRequest) error {
	k := key(req.Symbol, req.PerpSource)
	p, held := l.open[k]
	if !held {
		msg := "nhật ký ghi exit trong khi sổ giấy không giữ vị thế này — bỏ qua"
		l.note(req.AtMs, EventAnomaly, req.Symbol, req.PerpSource, 0, msg)
		return fmt.Errorf("paper: %s/%s: %s", req.Symbol, req.PerpSource, msg)
	}
	if reason := feeRefusal(req.SpotFee, req.PerpFee); reason != "" {
		return l.refuseClose(p, req.AtMs, reason)
	}
	// Exit sides: SELL the spot (bid side, the leg that hurts) and BUY the
	// perp back (ask side) — the same four-sided round trip strategy prices.
	// Sized at the coin quantity's CURRENT value on each book, not at the
	// notional the position was opened with: after a rally the same coins
	// are a bigger order, and the depth gate has to see the order that would
	// actually be sent (found by the review of 2026-09-11).
	spot, reason := fillQty(req.SpotBook, strategy.SideSell, p.QtyCoin, req.AtMs, req.MaxBookAge, req.SpotFee)
	if reason != "" {
		return l.refuseClose(p, req.AtMs, "chân BÁN spot: "+reason)
	}
	perp, reason := fillQty(req.PerpBook, strategy.SideBuy, p.QtyCoin, req.AtMs, req.MaxBookAge, req.PerpFee)
	if reason != "" {
		return l.refuseClose(p, req.AtMs, "chân MUA lại perp: "+reason)
	}

	spotProceeds := spot.NotionalQuote - spot.FeeQuote
	perpPnL := p.QtyCoin*(p.EntryPerp.FillPriceQuote-perp.FillPriceQuote) - perp.FeeQuote
	l.CashQuote += spotProceeds + p.MarginQuote + perpPnL
	l.FeesPaidQuote += spot.FeeQuote + perp.FeeQuote
	l.noteIfOverdrawn(req.AtMs, req.Symbol, req.PerpSource)

	p.ClosedAtMs = req.AtMs
	p.ExitSpot, p.ExitPerp = spot, perp
	p.ExitRefusedAtMs, p.ExitRefusedVI = 0, ""
	p.PaperRoundTripCostPct = p.PaperEntryCostPct + spot.SlippagePct + perp.SlippagePct + (spot.TakerFeeBps+perp.TakerFeeBps)/bpsPerUnit*pctPerUnit
	p.RealizedPnLQuote = (spotProceeds - p.EntrySpot.NotionalQuote - p.EntrySpot.FeeQuote) +
		(p.QtyCoin*(p.EntryPerp.FillPriceQuote-perp.FillPriceQuote) - p.EntryPerp.FeeQuote - perp.FeeQuote) +
		p.FundingQuote
	p.mark(req.AtMs, spot.MidPriceQuote, perp.MidPriceQuote, false)
	p.OpenPnLQuote = 0
	delete(l.open, k)
	l.Closed = append(l.Closed, p)
	l.note(req.AtMs, EventClose, req.Symbol, req.PerpSource, p.RealizedPnLQuote, fmt.Sprintf(
		"ĐÓNG (PAPER): bán %.6f %s spot ở %.4f (slippage %.4f%%, phí %.2f) · mua lại perp ở %.4f (slippage %.4f%%, phí %.2f) · P&L thực hiện %.2f (funding %.2f, %d mốc)",
		p.QtyCoin, req.Symbol, spot.FillPriceQuote, spot.SlippagePct, spot.FeeQuote,
		perp.FillPriceQuote, perp.SlippagePct, perp.FeeQuote, p.RealizedPnLQuote, p.FundingQuote, p.FundingSettlements))
	return nil
}

func (l *Ledger) refuseClose(p *Position, at int64, reason string) error {
	l.Refusals++
	p.ExitRefusedAtMs, p.ExitRefusedVI = at, reason
	l.note(at, EventRefuseClose, p.Symbol, p.PerpSource, 0,
		"TỪ CHỐI đóng (PAPER) — vị thế VẪN MỞ và được đánh dấu theo mid, không khớp ở giá tưởng tượng: "+reason)
	return fmt.Errorf("paper: close %s/%s refused: %s", p.Symbol, p.PerpSource, reason)
}

// CreditFunding applies one settlement to the position that held the pair at
// that instant, and reports whether anything was credited.
//
// Strictly after the open — the position opened on the decision at
// settlement i earns from i+1 — and at or before the close, which is the
// same arithmetic internal/backtest's equity curve and
// strategy.minHoldFloor use, so three readers of one trade cannot disagree.
func (l *Ledger) CreditFunding(symbol, perp string, s FundingSettlement) bool {
	p, held := l.open[key(symbol, perp)]
	if !held || s.SettledAtMs <= p.OpenedAtMs {
		return false
	}
	if s.Model == exchanges.FundingContinuous {
		p.FundingContinuousSkipped++
		return false
	}
	if !isPositiveFinite(s.MarkPriceQuote) || !isFinite(s.RatePerIntervalFrac) {
		p.FundingUnpriced++
		l.note(s.SettledAtMs, EventAnomaly, symbol, perp, 0, fmt.Sprintf(
			"mốc settle %s không định giá được (mark %v, rate %v) — KHÔNG ghi có, không phải 0",
			time.UnixMilli(s.SettledAtMs).UTC().Format(time.RFC3339), s.MarkPriceQuote, s.RatePerIntervalFrac))
		return false
	}
	// Positive funding is paid by longs to shorts; the paper position is
	// short the perp, so a positive rate is money IN.
	amount := s.MarkPriceQuote * p.QtyCoin * s.RatePerIntervalFrac
	p.FundingQuote += amount
	p.FundingSettlements++
	if s.RateType == "Special" {
		p.FundingSpecial++
	}
	p.Credits = append(p.Credits, FundingCredit{
		SettledAtMs: s.SettledAtMs, RatePerIntervalFrac: s.RatePerIntervalFrac, IntervalSec: s.IntervalSec,
		MarkPriceQuote: s.MarkPriceQuote, MarkSourceVI: s.MarkSourceVI, AmountQuote: amount, RateType: s.RateType,
	})
	l.CashQuote += amount
	l.note(s.SettledAtMs, EventFunding, symbol, perp, amount, fmt.Sprintf(
		"FUNDING (PAPER): %.6g × %.6f coin × mark %.4f (%s) = %.4f", s.RatePerIntervalFrac, p.QtyCoin, s.MarkPriceQuote, s.MarkSourceVI, amount))
	l.noteIfOverdrawn(s.SettledAtMs, symbol, perp)
	return true
}

// noteIfOverdrawn records the account going below zero cash: a real account
// would be on margin call here, and a paper one must at least say so.
func (l *Ledger) noteIfOverdrawn(atMs int64, symbol, perp string) {
	if l.CashQuote < 0 {
		l.note(atMs, EventAnomaly, symbol, perp, l.CashQuote, fmt.Sprintf(
			"tiền mặt ảo ÂM (%.2f): tài khoản thật đã bị gọi ký quỹ — sổ giấy không mô hình việc đó", l.CashQuote))
	}
}

// PriceFn answers the mid of one market at the mark instant, or false when
// there is no usable price.
type PriceFn func(source, symbol string) (midQuote float64, ok bool)

// Mark values every open position at one instant and records the account.
// A leg with no price keeps its last mark and the point says so — a missing
// sample must not read as an unchanged price without the flag.
func (l *Ledger) Mark(atMs int64, price PriceFn) EquityPoint {
	pt := EquityPoint{AtMs: atMs, CashQuote: l.CashQuote, OpenPositions: len(l.open)}
	equity := l.CashQuote
	// Sorted, so the equity figure is the same sum in the same order on
	// every rebuild — a map walk would move the last ulp between runs.
	for _, p := range l.OpenPositions() {
		spot, okS := price(p.SpotSource, p.Symbol)
		perp, okP := price(p.PerpSource, p.Symbol)
		stale := !okS || !okP
		if !okS {
			spot = p.SpotMarkQuote
		}
		if !okP {
			perp = p.PerpMarkQuote
		}
		p.mark(atMs, spot, perp, stale)
		if stale {
			pt.StaleMarks++
		}
		// Spot at its mark, the margin posted, the short's unrealized move.
		equity += p.QtyCoin*p.SpotMarkQuote + p.MarginQuote + p.QtyCoin*(p.EntryPerp.FillPriceQuote-p.PerpMarkQuote)
		pt.OpenPnLQuote += p.OpenPnLQuote
	}
	pt.EquityQuote = equity
	if equity > l.peak {
		l.peak = equity
	}
	pt.DrawdownFromPeakQuote = l.peak - equity
	l.Equity = append(l.Equity, pt)
	return pt
}

// mark stores the pair's value at one instant.
func (p *Position) mark(atMs int64, spotMid, perpMid float64, stale bool) {
	p.MarkedAtMs, p.SpotMarkQuote, p.PerpMarkQuote, p.MarkIsStale = atMs, spotMid, perpMid, stale
	// Coin-flat: spot gain and perp loss cancel when both move together, so
	// what is left is the basis moving against the entry.
	p.BasisPnLQuote = p.QtyCoin*(spotMid-p.EntrySpot.FillPriceQuote) + p.QtyCoin*(p.EntryPerp.FillPriceQuote-perpMid)
	if p.Open() {
		// Entry fees already paid; exit fees and slippage are NOT charged
		// here — the position is still open and the exit book is unknown.
		p.OpenPnLQuote = p.BasisPnLQuote - p.EntrySpot.FeeQuote - p.EntryPerp.FeeQuote + p.FundingQuote
	}
}

// fill prices a fill of a given NOTIONAL and derives the coin quantity.
func fill(book depth.Summary, side strategy.Side, notionalQuote float64, atMs int64, maxAge time.Duration, fee FeeState) (Leg, string) {
	if reason := bookRefusal(book, atMs, maxAge); reason != "" {
		return Leg{}, reason
	}
	leg, reason := estimate(book, side, notionalQuote, fee)
	if reason != "" {
		return Leg{}, reason
	}
	leg.QtyCoin = notionalQuote / leg.FillPriceQuote
	leg.NotionalQuote = notionalQuote
	leg.FeeQuote = leg.NotionalQuote * leg.TakerFeeBps / bpsPerUnit
	return leg, ""
}

// fillQty prices a fill of a given COIN quantity at that quantity's CURRENT
// value on the book (qty × mid): that is the order the venue would receive,
// and the size the depth gate must be asked about. The leg's notional is
// then qty × the fill price.
func fillQty(book depth.Summary, side strategy.Side, qtyCoin float64, atMs int64, maxAge time.Duration, fee FeeState) (Leg, string) {
	if reason := bookRefusal(book, atMs, maxAge); reason != "" {
		return Leg{}, reason
	}
	if !isPositiveFinite(qtyCoin) {
		return Leg{}, fmt.Sprintf("khối lượng %v coin không phải số dương hữu hạn", qtyCoin)
	}
	leg, reason := estimate(book, side, qtyCoin*book.MidPriceQuote, fee)
	if reason != "" {
		return Leg{}, reason
	}
	leg.QtyCoin = qtyCoin
	leg.NotionalQuote = qtyCoin * leg.FillPriceQuote
	leg.FeeQuote = leg.NotionalQuote * leg.TakerFeeBps / bpsPerUnit
	return leg, ""
}

// bookRefusal is why a book may not price a decision at atMs, or "". These
// are the same gates strategy.bookAgeRefusal applies before pricing a round
// trip — a book with no stamp, or older than the budget — plus the one rule
// PLAN 4.3 spells out for the ledger alone: never a book from AFTER the
// decision, not even by a millisecond. Kept as a second copy on purpose:
// the strategy gate guards a decision, this one guards an execution, and the
// two are pinned to the same boundaries by test rather than shared.
func bookRefusal(book depth.Summary, atMs int64, maxAge time.Duration) string {
	if !book.OK() {
		reason := "không có sổ lệnh đo được"
		if book.ErrVI != "" {
			reason += ": " + book.ErrVI
		}
		return reason
	}
	if book.SampledAtMs <= 0 {
		return "sổ lệnh không mang mốc lấy mẫu nên không biết nó có trước quyết định hay không"
	}
	if book.SampledAtMs > atMs {
		return fmt.Sprintf("sổ lệnh lấy mẫu lúc %s, SAU quyết định lúc %s — giá của tương lai không được dùng",
			time.UnixMilli(book.SampledAtMs).UTC().Format(time.RFC3339), time.UnixMilli(atMs).UTC().Format(time.RFC3339))
	}
	if maxAge > 0 {
		if age := time.Duration(atMs-book.SampledAtMs) * time.Millisecond; age > maxAge {
			return fmt.Sprintf("sổ lệnh đã %s tuổi, quá hạn %s của chính quyết định đó", age.Round(time.Second), maxAge)
		}
	}
	return ""
}

// estimate is the one place a fill price is derived from a book: the
// production estimate for an order of sizeQuote, on a book bookRefusal has
// already accepted.
func estimate(book depth.Summary, side strategy.Side, sizeQuote float64, fee FeeState) (Leg, string) {
	est := strategy.EstimateFill(book, side, sizeQuote)
	if !est.Fillable {
		return Leg{}, est.ReasonVI
	}
	// Buying lifts the ask: the average price is ABOVE the mid by the
	// slippage; selling hits the bid: below it.
	price := book.MidPriceQuote * (1 + est.SlippagePct/pctPerUnit)
	if side == strategy.SideSell {
		price = book.MidPriceQuote * (1 - est.SlippagePct/pctPerUnit)
	}
	if !isPositiveFinite(price) {
		return Leg{}, fmt.Sprintf("giá khớp suy ra %v không phải số dương hữu hạn", price)
	}
	return Leg{
		Source: book.Source, Side: side, BookSampledAtMs: book.SampledAtMs, MidPriceQuote: book.MidPriceQuote,
		SlippagePct: est.SlippagePct, FillPriceQuote: price, TakerFeeBps: fee.TakerBps,
		DepthIsLowerBound: est.DepthIsLowerBound, NoteVI: est.NoteVI,
	}, ""
}

// feeRefusal is why a pair of fee states cannot price anything, or "". The
// Verified rule is fees.RoundTripTakerPct's, restated here for the journal's
// recorded state rather than a live schedule — the same boundary, pinned by
// test, not shared code (see bookRefusal).
func feeRefusal(spot, perp FeeState) string {
	for _, f := range []struct {
		leg string
		fee FeeState
	}{{"spot", spot}, {"perp", perp}} {
		if !f.fee.Known {
			return fmt.Sprintf("hàng nhật ký không ghi biểu phí chân %s (hàng của lần chạy 1) — 'không ghi' không phải 'miễn phí'", f.leg)
		}
		if !f.fee.Verified {
			return fmt.Sprintf("biểu phí của %s (chân %s) chưa xác minh lúc hàng được ghi — không định giá", f.fee.Source, f.leg)
		}
		if !isFinite(f.fee.TakerBps) || f.fee.TakerBps < 0 {
			return fmt.Sprintf("phí taker %v bps của %s không hợp lệ", f.fee.TakerBps, f.fee.Source)
		}
	}
	return ""
}

func isFinite(v float64) bool         { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func isPositiveFinite(v float64) bool { return isFinite(v) && v > 0 }
