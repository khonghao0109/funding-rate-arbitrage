package scanner

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/fees"
)

// The cross-venue funding radar (PLAN 4.5i, direction 1). READ-ONLY: it reads
// the funding and top-of-book the scanner already holds and places nothing.
//
// # What every figure is, and is not (CLAUDE.md rule 2)
//
// The rates are the venues' FORMING rates — Binance premiumIndex, Bybit ticker —
// not settlements. A forming rate is revised until its stamp, and the step-3.2
// lesson is that a decision made on one is made on a number the venue has not
// committed to. So the radar ranks and logs; it does not decide.
//
// AfterCostAPR* deducts two things and names both: the taker commission of the
// four fills (open and close on both venues, from config.yaml's verified
// schedules) and the touch spread of both books (crossing half of each book's
// spread on each of two fills per venue). It deducts NOTHING else — not depth
// beyond the touch, not basis drift between entry and exit, not the funding the
// spread may stop paying, not liquidation risk on two separately margined legs,
// not the cost of moving margin between venues. It is not called "net": that
// word belongs to internal/strategy.
//
// # The capital denominator
//
// Both legs are perps on different venues, each margined at N/K, so capital is
// 2N/K and a return on capital is K/2 times the return on notional. At K = 1 the
// capital figure is HALF the notional one.

// crossRadarCfg is installed by Configure.
var crossRadarCfg config.CrossRadar

// Cross-radar status values.
const (
	CrossStatusGood          = "good"           // 🟢 CƠ HỘI TỐT
	CrossStatusWaitLiquidity = "wait_liquidity" // 🟡 CHỜ THANH KHOẢN
	CrossStatusWatch         = "watch"          // spread ≥ normal threshold, below "good"
	CrossStatusNormal        = "normal"         // ⚪ BÌNH THƯỜNG
	CrossStatusUnavailable   = "unavailable"    // a reading is missing or stale
)

// settlementsPer8hYear converts a per-8h rate into a per-year one: 365 days of
// three 8-hour periods. This is an ANNUALIZATION of a normalized rate for
// comparison, not a count of settlements a position would cross (rule 6) —
// each venue pays on its own cadence, stated per leg.
const settlementsPer8hYear = 365 * 3

// CrossLeg is one venue's side of a pair.
type CrossLeg struct {
	Source string `json:"source"`
	Venue  string `json:"venue"`

	RatePer8hBps  float64 `json:"rate_per_8h_bps"`
	IntervalSec   int64   `json:"interval_sec"`
	IsEstimated   bool    `json:"is_estimated"`
	NextFundingMs int64   `json:"next_funding_at_ms"`
	FundingStatus string  `json:"funding_status"` // live | stale | unknown | missing
	FundingAgeMs  int64   `json:"funding_age_ms"`
	// FundingPublishMode is config.yaml's periodic | on_change. For on_change
	// (Bybit) a small age proves nothing and a large one is normal: "live"
	// there means only "not proven dead" (CLAUDE.md, Funding cadence trap).
	FundingPublishMode string `json:"funding_publish_mode"`

	BestBidQuote   float64 `json:"best_bid_quote"`
	BestAskQuote   float64 `json:"best_ask_quote"`
	BestBidQtyCoin float64 `json:"best_bid_qty_coin"`
	BestAskQtyCoin float64 `json:"best_ask_qty_coin"`
	MidQuote       float64 `json:"mid_quote"`
	PriceStatus    string  `json:"price_status"` // live | stale | unknown | missing
	// TouchSpreadBps is (ask − bid) / mid; null when the book is not live.
	TouchSpreadBps *float64 `json:"touch_spread_bps"`

	// Depth from the periodic REST sweep (rule 10: never streamed), the thinner
	// side within 0.1% of mid. 0 with DepthSampledAtMs 0 means no sweep yet.
	DepthWithin0_1PctMinQuote float64 `json:"depth_within_0_1pct_min_quote"`
	DepthSampledAtMs          int64   `json:"depth_sampled_at_ms"`
}

// CrossPair is one symbol compared across the two venues.
type CrossPair struct {
	Symbol string `json:"symbol"`
	// Direction names the trade that COLLECTS the spread, e.g.
	// LONG_BYBIT_SHORT_BINANCE; "" when the spread is exactly 0 or unknown.
	Direction   string `json:"direction"`
	ShortSource string `json:"short_source"`
	LongSource  string `json:"long_source"`

	A CrossLeg `json:"a"`
	B CrossLeg `json:"b"`

	// SpreadPer8hBps is A − B, signed. GrossAPRPct is its absolute value
	// annualized on notional, with nothing deducted.
	SpreadPer8hBps float64 `json:"spread_per_8h_bps"`
	GrossAPRPct    float64 `json:"gross_apr_pct"`

	// Costs of one round trip in bps of notional. FeesRoundTripBps is null when
	// either fee schedule is unverified — an unknown fee is not a free one.
	FeesRoundTripBps  *float64 `json:"fees_round_trip_bps"`
	TouchRoundTripBps *float64 `json:"touch_round_trip_bps"`
	CostRoundTripBps  *float64 `json:"cost_round_trip_bps"`

	// BreakevenHoldDays is how many days the CURRENT spread, if it held, takes
	// to repay CostRoundTripBps: cost ÷ (|spread per 8h| × 3). null when the
	// cost is unknown or the spread is 0.
	BreakevenHoldDays *float64 `json:"breakeven_hold_days"`

	// The cost spread over PlannedHoldDays, deducted from the gross APR. null
	// whenever any cost is unknown.
	AfterCostAPRNotionalPct *float64 `json:"after_cost_apr_notional_pct"`
	AfterCostAPRCapitalPct  *float64 `json:"after_cost_apr_capital_pct"`

	// CrossBasisBps is (mid A − mid B) / average mid; null unless both books
	// are live. A wide basis can move against a new position.
	CrossBasisBps *float64 `json:"cross_basis_bps"`

	Status   string   `json:"status"`
	Feasible bool     `json:"feasible"`
	NotesVI  []string `json:"notes_vi"`
}

// CrossRadarSnapshot is the whole radar at one instant.
type CrossRadarSnapshot struct {
	Enabled         bool     `json:"enabled"`
	UpdatedAtMs     int64    `json:"updated_at_ms"`
	SourceA         string   `json:"source_a"`
	SourceB         string   `json:"source_b"`
	RateModel       string   `json:"rate_model"` // forming_gross
	CostsAppliedVI  []string `json:"costs_applied_vi"`
	CostsExcludedVI []string `json:"costs_excluded_vi"`

	LeverageXPerLeg               float64 `json:"leverage_x_per_leg"`
	CapitalPerNotional            float64 `json:"capital_per_notional"`
	PlannedHoldDays               float64 `json:"planned_hold_days"`
	GoodMinAfterCostAPRCapitalPct float64 `json:"good_min_after_cost_apr_capital_pct"`
	GoodMaxBreakevenDays          float64 `json:"good_max_breakeven_days"`
	// EventThresholdsGrossAPRPct are the episode thresholds, ascending; the page
	// filters on the lowest and alerts on the highest, so no cut-off is typed
	// twice.
	EventThresholdsGrossAPRPct []float64 `json:"event_thresholds_gross_apr_pct"`
	MaxTouchSpreadBps          float64   `json:"max_touch_spread_bps"`
	NormalBelowGrossAPRPct     float64   `json:"normal_below_gross_apr_pct"`

	Pairs []CrossPair `json:"pairs"`
}

// crossInputs is what the radar reads, gathered under the scanner's locks and
// then computed without holding any.
type crossInputs struct {
	symbols []string
	funding map[string]map[string]exchanges.FundingData
	prices  map[string]map[string]PricePoint
	depth   map[string]map[string]depth.Summary
}

// CrossRadar builds the radar from what the scanner holds right now.
func (s *Scanner) CrossRadar() CrossRadarSnapshot {
	cfg := crossRadarCfg
	in := crossInputs{symbols: s.symbols,
		funding: map[string]map[string]exchanges.FundingData{},
		prices:  map[string]map[string]PricePoint{},
		depth:   map[string]map[string]depth.Summary{}}
	if cfg.Enabled {
		wanted := []string{cfg.SourceA, cfg.SourceB}
		s.fundingMutex.RLock()
		for sym, bySource := range s.funding {
			for _, src := range wanted {
				if d, ok := bySource[src]; ok {
					if in.funding[sym] == nil {
						in.funding[sym] = map[string]exchanges.FundingData{}
					}
					in.funding[sym][src] = d
				}
			}
		}
		s.fundingMutex.RUnlock()
		s.pricesMutex.RLock()
		for sym, bySource := range s.prices {
			for _, src := range wanted {
				if p, ok := bySource[src]; ok {
					if in.prices[sym] == nil {
						in.prices[sym] = map[string]PricePoint{}
					}
					in.prices[sym][src] = p
				}
			}
		}
		s.pricesMutex.RUnlock()
		s.depthMutex.RLock()
		for sym, bySource := range s.depthSummaries {
			for _, src := range wanted {
				if d, ok := bySource[src]; ok {
					if in.depth[sym] == nil {
						in.depth[sym] = map[string]depth.Summary{}
					}
					in.depth[sym][src] = d
				}
			}
		}
		s.depthMutex.RUnlock()
	}
	return buildCrossRadar(cfg, in, s.now())
}

func buildCrossRadar(cfg config.CrossRadar, in crossInputs, now time.Time) CrossRadarSnapshot {
	out := CrossRadarSnapshot{
		Enabled: cfg.Enabled, UpdatedAtMs: now.UnixMilli(),
		SourceA: cfg.SourceA, SourceB: cfg.SourceB, RateModel: "forming_gross",
		CostsAppliedVI: []string{
			"phí taker 4 lệnh (mở + đóng ở cả hai sàn, biểu phí đã xác minh trong config.yaml)",
			"spread chạm của cả hai sổ lệnh (nửa spread mỗi lệnh × 2 lệnh mỗi sàn)",
		},
		CostsExcludedVI: []string{
			"trượt giá sâu hơn mức chạm (độ sâu chỉ đo mỗi lượt quét REST)",
			"trôi basis giữa hai sàn từ lúc vào tới lúc ra",
			"funding đang hình thành có thể đổi trước mốc settle — lịch sử: chênh ≥ 15% giữ trung vị 1 ngày",
			"rủi ro thanh lý của hai chân ký quỹ riêng ở hai sàn",
			"phí và thời gian chuyển ký quỹ giữa hai sàn",
		},
		LeverageXPerLeg:               cfg.LeverageXPerLeg,
		PlannedHoldDays:               cfg.PlannedHoldDays,
		GoodMinAfterCostAPRCapitalPct: cfg.GoodMinAfterCostAPRCapitalPct,
		GoodMaxBreakevenDays:          cfg.GoodMaxBreakevenDays,
		EventThresholdsGrossAPRPct:    append([]float64{}, cfg.EventThresholdsGrossAPRPct...),
		MaxTouchSpreadBps:             cfg.MaxTouchSpreadBps,
		NormalBelowGrossAPRPct:        cfg.NormalBelowGrossAPRPct,
		Pairs:                         []CrossPair{},
	}
	if !cfg.Enabled {
		return out
	}
	if cfg.LeverageXPerLeg > 0 {
		out.CapitalPerNotional = 2 / cfg.LeverageXPerLeg
	}
	for _, symbol := range in.symbols {
		fa, okA := in.funding[symbol][cfg.SourceA]
		fb, okB := in.funding[symbol][cfg.SourceB]
		if !okA && !okB {
			// Neither venue funds this symbol here: not a pair on this radar.
			continue
		}
		out.Pairs = append(out.Pairs, buildCrossPair(cfg, symbol,
			crossLeg(cfg.SourceA, fa, okA, in.prices[symbol], in.depth[symbol], now),
			crossLeg(cfg.SourceB, fb, okB, in.prices[symbol], in.depth[symbol], now)))
	}
	sort.SliceStable(out.Pairs, func(i, j int) bool { return crossLess(out.Pairs[i], out.Pairs[j]) })
	return out
}

func crossLeg(source string, f exchanges.FundingData, haveFunding bool, prices map[string]PricePoint, books map[string]depth.Summary, now time.Time) CrossLeg {
	leg := CrossLeg{Source: source, FundingStatus: "missing", PriceStatus: "missing", FundingAgeMs: -1}
	if meta, ok := sourceMetaFor(source); ok {
		leg.Venue = meta.Venue
		leg.FundingPublishMode = meta.FundingPublishMode
	}
	if haveFunding {
		leg.RatePer8hBps = f.RatePer8hFrac * bpsPerUnit
		leg.IntervalSec = f.IntervalSec
		leg.IsEstimated = f.IsEstimated
		leg.NextFundingMs = f.NextFundingAtMs
		leg.FundingStatus, _ = fundingStatus(f, fundingStaleAfter(source), now)
		leg.FundingAgeMs = ageMs(f.RecvAt, now)
	}
	if p, ok := prices[source]; ok {
		leg.PriceStatus = priceStatus(p, staleAfter(source), now)
		leg.BestBidQuote, leg.BestAskQuote = p.BestBid, p.BestAsk
		leg.BestBidQtyCoin, leg.BestAskQtyCoin = p.BestBidQtyCoin, p.BestAskQtyCoin
		if leg.PriceStatus == statusLive && p.BestBid > 0 && p.BestAsk >= p.BestBid {
			leg.MidQuote = (p.BestBid + p.BestAsk) / 2
			spread := (p.BestAsk - p.BestBid) / leg.MidQuote * bpsPerUnit
			leg.TouchSpreadBps = &spread
		}
	}
	if d, ok := books[source]; ok {
		leg.DepthWithin0_1PctMinQuote = math.Min(d.BidDepthWithinTightQuote, d.AskDepthWithinTightQuote)
		leg.DepthSampledAtMs = d.SampledAtMs
	}
	return leg
}

func buildCrossPair(cfg config.CrossRadar, symbol string, a, b CrossLeg) CrossPair {
	p := CrossPair{Symbol: symbol, A: a, B: b, NotesVI: []string{}}
	fundingLive := a.FundingStatus == statusLive && b.FundingStatus == statusLive
	if a.FundingStatus != "missing" && b.FundingStatus != "missing" {
		p.SpreadPer8hBps = a.RatePer8hBps - b.RatePer8hBps
		p.GrossAPRPct = math.Abs(p.SpreadPer8hBps) * settlementsPer8hYear / bpsPerUnit * pctPerUnit
		switch {
		case p.SpreadPer8hBps > 0:
			p.ShortSource, p.LongSource = a.Source, b.Source
		case p.SpreadPer8hBps < 0:
			p.ShortSource, p.LongSource = b.Source, a.Source
		}
		p.Direction = CrossDirection(p.ShortSource, p.LongSource)
	}

	if feePct, ok := fees.RoundTripTakerPct(scheduleFor(a.Source), scheduleFor(b.Source)); ok {
		v := feePct * pctPerUnit // pct → bps
		p.FeesRoundTripBps = &v
	}
	if a.TouchSpreadBps != nil && b.TouchSpreadBps != nil {
		// Each venue: open and close each cross half the spread → one full spread.
		v := *a.TouchSpreadBps + *b.TouchSpreadBps
		p.TouchRoundTripBps = &v
		avgMid := (a.MidQuote + b.MidQuote) / 2
		basis := (a.MidQuote - b.MidQuote) / avgMid * bpsPerUnit
		p.CrossBasisBps = &basis
	}
	if p.FeesRoundTripBps != nil && p.TouchRoundTripBps != nil && fundingLive && cfg.PlannedHoldDays > 0 && cfg.LeverageXPerLeg > 0 {
		cost := *p.FeesRoundTripBps + *p.TouchRoundTripBps
		p.CostRoundTripBps = &cost
		costAPRPct := cost / bpsPerUnit * pctPerUnit * 365 / cfg.PlannedHoldDays
		notional := p.GrossAPRPct - costAPRPct
		capital := notional * cfg.LeverageXPerLeg / 2
		p.AfterCostAPRNotionalPct, p.AfterCostAPRCapitalPct = &notional, &capital
		if perDayBps := math.Abs(p.SpreadPer8hBps) * 3; perDayBps > 0 {
			days := cost / perDayBps
			p.BreakevenHoldDays = &days
		}
	}

	switch {
	case !fundingLive:
		p.Status = CrossStatusUnavailable
		p.NotesVI = append(p.NotesVI, "thiếu hoặc cũ funding ở ít nhất một sàn — không xếp hạng")
	case p.GrossAPRPct < cfg.NormalBelowGrossAPRPct:
		p.Status = CrossStatusNormal
	case p.AfterCostAPRCapitalPct == nil:
		p.Status = CrossStatusUnavailable
		p.NotesVI = append(p.NotesVI, "chênh đáng xem nhưng chi phí không tính được (sổ lệnh không live hoặc biểu phí chưa xác minh)")
	case *p.AfterCostAPRCapitalPct >= cfg.GoodMinAfterCostAPRCapitalPct &&
		(p.BreakevenHoldDays == nil || *p.BreakevenHoldDays > cfg.GoodMaxBreakevenDays):
		p.Status = CrossStatusWatch
		p.NotesVI = append(p.NotesVI, "APR sau chi phí cao theo giả định giữ "+strconv.FormatFloat(cfg.PlannedHoldDays, 'g', -1, 64)+
			" ngày, nhưng chênh hiện tại cần hơn "+strconv.FormatFloat(cfg.GoodMaxBreakevenDays, 'g', -1, 64)+
			" ngày mới trả hết chi phí vòng — lịch sử: chênh rộng hiếm khi giữ lâu vậy")
	case *p.AfterCostAPRCapitalPct >= cfg.GoodMinAfterCostAPRCapitalPct &&
		*a.TouchSpreadBps <= cfg.MaxTouchSpreadBps && *b.TouchSpreadBps <= cfg.MaxTouchSpreadBps:
		p.Status = CrossStatusGood
	case *p.AfterCostAPRCapitalPct >= cfg.GoodMinAfterCostAPRCapitalPct:
		p.Status = CrossStatusWaitLiquidity
		p.NotesVI = append(p.NotesVI, "APR sau chi phí cao nhưng spread chạm của một sàn vượt ngưỡng")
	default:
		p.Status = CrossStatusWatch
	}
	p.Feasible = p.Status == CrossStatusGood

	if a.IntervalSec > 0 && b.IntervalSec > 0 && a.IntervalSec != b.IntervalSec {
		p.NotesVI = append(p.NotesVI, "chu kỳ funding khác nhau ("+intervalLabel(a.IntervalSec)+" / "+intervalLabel(b.IntervalSec)+
			") — đã quy về 8h để so; tiền về theo mốc riêng từng sàn")
	}
	for _, leg := range []CrossLeg{a, b} {
		if leg.FundingPublishMode == "on_change" {
			p.NotesVI = append(p.NotesVI, leg.Venue+" chỉ phát funding khi rate đổi: tuổi của số đọc không chứng minh nó còn đúng")
		}
	}
	if a.DepthSampledAtMs == 0 || b.DepthSampledAtMs == 0 {
		p.NotesVI = append(p.NotesVI, "chưa có lượt quét độ sâu cho một sàn")
	}
	return p
}

// crossLess orders the radar: rankable pairs by after-cost APR on capital,
// highest first; then pairs with only a gross figure, by gross; unavailable last.
func crossLess(x, y CrossPair) bool {
	rank := func(p CrossPair) int {
		switch {
		case p.Status == CrossStatusUnavailable:
			return 2
		case p.AfterCostAPRCapitalPct == nil:
			return 1
		}
		return 0
	}
	rx, ry := rank(x), rank(y)
	if rx != ry {
		return rx < ry
	}
	if rx == 0 && *x.AfterCostAPRCapitalPct != *y.AfterCostAPRCapitalPct {
		return *x.AfterCostAPRCapitalPct > *y.AfterCostAPRCapitalPct
	}
	if x.GrossAPRPct != y.GrossAPRPct {
		return x.GrossAPRPct > y.GrossAPRPct
	}
	return x.Symbol < y.Symbol
}

// CrossDirection names the trade that collects a spread, e.g.
// LONG_BYBIT_SHORT_BINANCE; "" when there is no short side.
func CrossDirection(shortSource, longSource string) string {
	if shortSource == "" || longSource == "" {
		return ""
	}
	return "LONG_" + venueUpper(longSource) + "_SHORT_" + venueUpper(shortSource)
}

// CrossRadarThresholds are the configured episode thresholds, ascending.
func (s *Scanner) CrossRadarThresholds() []float64 {
	return append([]float64(nil), crossRadarCfg.EventThresholdsGrossAPRPct...)
}

func venueUpper(source string) string {
	if meta, ok := sourceMetaFor(source); ok && meta.Venue != "" {
		return strings.ToUpper(meta.Venue)
	}
	return strings.ToUpper(source)
}

func intervalLabel(sec int64) string {
	return (time.Duration(sec) * time.Second).String()
}
