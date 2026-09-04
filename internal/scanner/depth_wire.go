package scanner

import (
	"time"

	"futures-arbitrage-scanner/internal/depth"
)

// The depth half of the contract (step 2.7b). docs/WS-CONTRACT.md §11 is
// authoritative.
//
// Depth is a separate message type rather than fields inside `prices` because
// the two move at completely different speeds: prices go out five times a
// second, a book is sampled once an hour. Folding depth into the price snapshot
// would resend a large, unchanged payload eighteen thousand times per refresh.

// depthStaleFactor is how many refresh periods a snapshot may age before the
// dashboard stops calling it current.
//
// Depth is polled on OUR schedule, so unlike a venue feed there is no question
// of what the normal gap is — it is the refresh period. Two and a half of them
// is late enough to mean a sweep was missed rather than merely slow.
const depthStaleFactor = 2.5

// depthRefreshEverySec is the configured sweep period, used to judge staleness
// and published in meta so the dashboard can explain the age it renders.
// Configure sets it; the fallback only applies when depth collection is off.
var depthRefreshEverySec int64 = 3600

// wireDepthPoint is one venue's book for one pair, reduced to what a person
// ranking opportunities needs to see.
//
// Depth figures are in the market's QUOTE asset, never summed across quotes:
// three venues here quote USD and the rest USDT, and adding them would be the
// currency mix §5.2 refuses for basis.
type wireDepthPoint struct {
	SampledAtMs int64  `json:"sampled_at_ms"`
	AgeMs       int64  `json:"age_ms"`
	Status      string `json:"status"`
	VenueTimeMs int64  `json:"venue_time_ms"`

	MidPriceQuote  float64 `json:"mid_price_quote"`
	BestBidQuote   float64 `json:"best_bid_quote"`
	BestAskQuote   float64 `json:"best_ask_quote"`
	BestBidQtyCoin float64 `json:"best_bid_qty_coin"`
	BestAskQtyCoin float64 `json:"best_ask_qty_coin"`
	SpreadPct      float64 `json:"spread_pct"`

	// Notional resting within each window of the mid. The bid side of a SPOT
	// market is the one that decides the exit: a funding position is unwound by
	// selling the spot leg, and that happens when funding turns — which
	// correlates with the moment books thin out (PLAN §7.4).
	BidDepthWithinTightQuote float64 `json:"bid_depth_within_0_1pct_quote"`
	AskDepthWithinTightQuote float64 `json:"ask_depth_within_0_1pct_quote"`
	BidDepthWithinWideQuote  float64 `json:"bid_depth_within_0_5pct_quote"`
	AskDepthWithinWideQuote  float64 `json:"ask_depth_within_0_5pct_quote"`

	BidLevels int `json:"bid_levels"`
	AskLevels int `json:"ask_levels"`

	// How far from the mid the farthest returned level sits. Below a window,
	// that window's figure is a FLOOR, not a measurement — Hyperliquid returns
	// twenty levels spanning 0.025% on BTC. The two booleans say it outright so
	// the dashboard does not have to re-derive the comparison.
	BidSpanPct   float64 `json:"bid_span_pct"`
	AskSpanPct   float64 `json:"ask_span_pct"`
	CoversTight  bool    `json:"covers_0_1pct"`
	CoversWide   bool    `json:"covers_0_5pct"`
	IsContractIn bool    `json:"is_contract_book"`

	// ErrorVI is set when the book could not be read or converted. The row is
	// still published: a venue that vanishes from the table reads as a venue
	// with no liquidity rather than one that could not be reached.
	ErrorVI string `json:"error_vi"`
}

type wireDepth struct {
	Type         string                               `json:"type"`
	V            int                                  `json:"v"`
	ServerTimeMs int64                                `json:"server_time_ms"`
	Depth        map[string]map[string]wireDepthPoint `json:"depth"`
}

// wireDepthMeta describes the sampling to the dashboard, so the windows and the
// cadence are not hardcoded in JavaScript.
type wireDepthMeta struct {
	RefreshEverySec int64     `json:"refresh_every_sec"`
	WindowsPct      []float64 `json:"windows_pct"`
	NoteVI          string    `json:"note_vi"`
}

func newWireDepthMeta() wireDepthMeta {
	return wireDepthMeta{
		RefreshEverySec: depthRefreshEverySec,
		WindowsPct:      []float64{depth.WindowTightPct, depth.WindowWidePct},
		NoteVI: "Độ sâu lấy qua REST theo chu kỳ, không phải stream: vị thế funding giữ hàng ngày " +
			"nên không cần biết sổ lệnh đổi từng mili-giây. Số tính bằng ĐỒNG QUOTE của chính " +
			"sàn đó và không cộng chéo quote. Dấu ≥ nghĩa là sàn không trả đủ mức xa tới cửa sổ " +
			"đó — con số là cận dưới, không phải sổ mỏng.",
	}
}

func newWireDepth(summaries []depth.Summary, now time.Time) wireDepth {
	out := wireDepth{
		Type:         "depth",
		V:            wireVersion,
		ServerTimeMs: now.UnixMilli(),
		Depth:        make(map[string]map[string]wireDepthPoint),
	}
	for _, summary := range summaries {
		if out.Depth[summary.Symbol] == nil {
			out.Depth[summary.Symbol] = make(map[string]wireDepthPoint)
		}
		out.Depth[summary.Symbol][summary.Source] = newWireDepthPoint(summary, now)
	}
	return out
}

func newWireDepthPoint(summary depth.Summary, now time.Time) wireDepthPoint {
	point := wireDepthPoint{
		SampledAtMs: summary.SampledAtMs,
		AgeMs:       -1,
		Status:      statusUnknown,
		VenueTimeMs: summary.VenueTimeMs,

		MidPriceQuote:  summary.MidPriceQuote,
		BestBidQuote:   summary.BestBidQuote,
		BestAskQuote:   summary.BestAskQuote,
		BestBidQtyCoin: summary.BestBidQtyCoin,
		BestAskQtyCoin: summary.BestAskQtyCoin,
		SpreadPct:      summary.SpreadPct,

		BidDepthWithinTightQuote: summary.BidDepthWithinTightQuote,
		AskDepthWithinTightQuote: summary.AskDepthWithinTightQuote,
		BidDepthWithinWideQuote:  summary.BidDepthWithinWideQuote,
		AskDepthWithinWideQuote:  summary.AskDepthWithinWideQuote,

		BidLevels:    summary.BidLevels,
		AskLevels:    summary.AskLevels,
		BidSpanPct:   summary.BidSpanPct,
		AskSpanPct:   summary.AskSpanPct,
		CoversTight:  summary.CoversTight(),
		CoversWide:   summary.CoversWide(),
		IsContractIn: summary.IsContractBook,
		ErrorVI:      summary.ErrVI,
	}

	if summary.SampledAtMs > 0 {
		point.AgeMs = now.Sub(time.UnixMilli(summary.SampledAtMs)).Milliseconds()
		point.Status = depthStatus(point.AgeMs, summary)
	}
	return point
}

// depthStatus decides whether a snapshot still describes the book.
//
// A summary that failed to read is `unknown` rather than `stale`, however
// recent the attempt: stale means "this measurement is old", and there is no
// measurement here to be old.
func depthStatus(ageMs int64, summary depth.Summary) string {
	if !summary.OK() {
		return statusUnknown
	}
	if float64(ageMs) > float64(depthRefreshEverySec)*depthStaleFactor*1000 {
		return statusStale
	}
	return statusLive
}
