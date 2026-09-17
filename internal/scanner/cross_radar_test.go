package scanner

import (
	"math"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/depth"
)

// The radar's arithmetic, pinned with numbers worked out by hand. The fee
// schedules come from the shipped config.yaml (TestMain): binance_futures taker
// 5.0 bps and bybit_futures 5.5 bps, both verified, so four fills cost
// 2×5.0 + 2×5.5 = 21 bps.

var radarNow = time.UnixMilli(1_789_620_000_000)

func radarCfg() config.CrossRadar {
	return config.CrossRadar{Enabled: true, SourceA: "binance_futures", SourceB: "bybit_futures",
		LeverageXPerLeg: 2, PlannedHoldDays: 7, GoodMinAfterCostAPRCapitalPct: 20, GoodMaxBreakevenDays: 3,
		MaxTouchSpreadBps: 5, NormalBelowGrossAPRPct: 10,
		EventThresholdsGrossAPRPct: []float64{15, 25}, EventEndBelowSec: 60, SampleEverySec: 5}
}

// fundingAt builds a live discrete reading at a per-8h rate in bps.
func fundingAt(symbol, source string, per8hBps float64, intervalSec int64) exchanges.FundingData {
	return exchanges.FundingData{Symbol: symbol, Source: source, Model: exchanges.FundingDiscrete,
		RatePer8hFrac: per8hBps / 10000, RatePerIntervalFrac: per8hBps / 10000 * float64(intervalSec) / 28800,
		IntervalSec: intervalSec, IsEstimated: true, RecvAt: radarNow.Add(-time.Second),
		NextFundingAtMs: radarNow.Add(time.Hour).UnixMilli()}
}

func book(bid, ask float64) PricePoint {
	return PricePoint{Price: (bid + ask) / 2, BestBid: bid, BestAsk: ask, RecvAt: radarNow.Add(-time.Second)}
}

func radarInputs(symbol string, aBps, bBps float64, aBook, bBook PricePoint) crossInputs {
	return crossInputs{symbols: []string{symbol},
		funding: map[string]map[string]exchanges.FundingData{symbol: {
			"binance_futures": fundingAt(symbol, "binance_futures", aBps, 28800),
			"bybit_futures":   fundingAt(symbol, "bybit_futures", bBps, 28800)}},
		prices: map[string]map[string]PricePoint{symbol: {"binance_futures": aBook, "bybit_futures": bBook}},
		depth:  map[string]map[string]depth.Summary{}}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCrossRadar_ArithmeticByHand(t *testing.T) {
	// Binance +1.25 bps/8h, Bybit −6.50: spread +7.75 ⇒ short Binance, long Bybit.
	// Gross = 7.75 × 1095 / 100 = 84.8625 %/yr.
	// Touch: Binance 100.00/100.01 ⇒ 0.01/100.005 × 1e4 = 0.99995 bps;
	//        Bybit 99.99/100.01 ⇒ 0.02/100 × 1e4 = 2 bps. Touch round trip 2.99995.
	// Cost 23.99995 bps over 7 days ⇒ 0.2399995 × 365/7 = 12.51426 %/yr.
	// After cost on notional 72.34824; on capital at K=2 the same (K/2 = 1).
	in := radarInputs("SUIUSDT", 1.25, -6.50, book(100.00, 100.01), book(99.99, 100.01))
	snap := buildCrossRadar(radarCfg(), in, radarNow)
	if len(snap.Pairs) != 1 {
		t.Fatalf("%d pairs", len(snap.Pairs))
	}
	p := snap.Pairs[0]
	if p.Direction != "LONG_BYBIT_SHORT_BINANCE" || p.ShortSource != "binance_futures" || p.LongSource != "bybit_futures" {
		t.Errorf("direction %q short %s long %s", p.Direction, p.ShortSource, p.LongSource)
	}
	if !near(p.SpreadPer8hBps, 7.75) || !near(p.GrossAPRPct, 84.8625) {
		t.Errorf("spread %v gross %v", p.SpreadPer8hBps, p.GrossAPRPct)
	}
	if p.FeesRoundTripBps == nil || !near(*p.FeesRoundTripBps, 21) {
		t.Errorf("fees %v, want 21 bps from the verified schedules", p.FeesRoundTripBps)
	}
	touch := 0.01/100.005*1e4 + 2
	cost := 21 + touch
	notional := 84.8625 - cost/100*365/7
	if p.TouchRoundTripBps == nil || !near(*p.TouchRoundTripBps, touch) ||
		p.AfterCostAPRNotionalPct == nil || math.Abs(*p.AfterCostAPRNotionalPct-notional) > 1e-9 ||
		math.Abs(*p.AfterCostAPRCapitalPct-notional) > 1e-9 {
		t.Errorf("touch %v notional %v capital %v, want %v / %v", p.TouchRoundTripBps, p.AfterCostAPRNotionalPct, p.AfterCostAPRCapitalPct, touch, notional)
	}
	basis := (100.005 - 100.0) / 100.0025 * 1e4
	if p.CrossBasisBps == nil || !near(*p.CrossBasisBps, basis) {
		t.Errorf("basis %v want %v", p.CrossBasisBps, basis)
	}
	if p.Status != CrossStatusGood || !p.Feasible {
		t.Errorf("status %s feasible %v", p.Status, p.Feasible)
	}
	if snap.CapitalPerNotional != 1 {
		t.Errorf("capital per notional at K=2 is 2/K = 1, got %v", snap.CapitalPerNotional)
	}
}

// Capital is 2N/K, so the capital figure is K/2 × the notional one — at K=3
// one and a half times, never three times.
func TestCrossRadar_CapitalIsKOverTwoNeverK(t *testing.T) {
	cfg := radarCfg()
	cfg.LeverageXPerLeg = 3
	p := buildCrossRadar(cfg, radarInputs("X", 5, -5, book(100, 100.01), book(100, 100.01)), radarNow).Pairs[0]
	if !near(*p.AfterCostAPRCapitalPct, *p.AfterCostAPRNotionalPct*1.5) {
		t.Errorf("capital %v notional %v: want ×1.5", *p.AfterCostAPRCapitalPct, *p.AfterCostAPRNotionalPct)
	}
}

func TestCrossRadar_Statuses(t *testing.T) {
	cases := []struct {
		name         string
		a, b         float64
		aBook, bBook PricePoint
		want         string
	}{
		{"wide spread, tight books", 5, -5, book(100, 100.01), book(100, 100.01), CrossStatusGood},
		{"wide spread, one wide book", 5, -5, book(100, 100.01), book(100, 100.10), CrossStatusWaitLiquidity},
		{"gross 10.95%, under good", 0.5, -0.5, book(100, 100.01), book(100, 100.01), CrossStatusWatch},
		{"gross under 10%", 0.4, 0, book(100, 100.01), book(100, 100.01), CrossStatusNormal},
	}
	for _, tc := range cases {
		in := radarInputs("X", tc.a, tc.b, tc.aBook, tc.bBook)
		if got := buildCrossRadar(radarCfg(), in, radarNow).Pairs[0].Status; got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

// A stale or missing reading is never ranked or called feasible, and a book that
// is not live costs nothing only by being refused, never by being zero.
func TestCrossRadar_StaleOrMissingIsUnavailable(t *testing.T) {
	in := radarInputs("X", 5, -5, book(100, 100.01), book(100, 100.01))
	stale := in.funding["X"]["bybit_futures"]
	stale.RecvAt = radarNow.Add(-24 * time.Hour)
	in.funding["X"]["bybit_futures"] = stale
	p := buildCrossRadar(radarCfg(), in, radarNow).Pairs[0]
	if p.Status != CrossStatusUnavailable || p.Feasible || p.AfterCostAPRCapitalPct != nil {
		t.Errorf("stale funding: %+v", p)
	}

	in = radarInputs("X", 5, -5, book(100, 100.01), book(100, 100.01))
	oldBook := in.prices["X"]["binance_futures"]
	oldBook.RecvAt = radarNow.Add(-time.Hour)
	in.prices["X"]["binance_futures"] = oldBook
	p = buildCrossRadar(radarCfg(), in, radarNow).Pairs[0]
	if p.A.TouchSpreadBps != nil || p.AfterCostAPRCapitalPct != nil || p.Status != CrossStatusUnavailable {
		t.Errorf("stale book must leave the cost unknown: %+v", p)
	}

	in = radarInputs("X", 5, -5, book(100, 100.01), book(100, 100.01))
	delete(in.funding["X"], "bybit_futures")
	p = buildCrossRadar(radarCfg(), in, radarNow).Pairs[0]
	if p.Status != CrossStatusUnavailable || p.Direction != "" || p.GrossAPRPct != 0 || p.B.FundingStatus != "missing" {
		t.Errorf("missing venue: %+v", p)
	}
}

func TestCrossRadar_UnverifiedFeeGivesNoAfterCostFigure(t *testing.T) {
	markFeeUnverified(t, "bybit_futures")
	p := buildCrossRadar(radarCfg(), radarInputs("X", 5, -5, book(100, 100.01), book(100, 100.01)), radarNow).Pairs[0]
	if p.FeesRoundTripBps != nil || p.AfterCostAPRCapitalPct != nil || p.Status == CrossStatusGood {
		t.Errorf("an unverified fee is not a free one: %+v", p)
	}
}

// Rates on different cadences are compared per 8h, and the note says so.
func TestCrossRadar_MixedCadenceIsNormalizedAndNoted(t *testing.T) {
	in := radarInputs("HYPEUSDT", 2, 1, book(100, 100.01), book(100, 100.01))
	in.funding["HYPEUSDT"]["binance_futures"] = fundingAt("HYPEUSDT", "binance_futures", 2, 14400)
	p := buildCrossRadar(radarCfg(), in, radarNow).Pairs[0]
	if !near(p.SpreadPer8hBps, 1) || !strings.Contains(strings.Join(p.NotesVI, " "), "chu kỳ funding khác nhau") {
		t.Errorf("spread %v notes %v", p.SpreadPer8hBps, p.NotesVI)
	}
}

func TestCrossRadar_OrderAndDisabled(t *testing.T) {
	in := crossInputs{symbols: []string{"LOW", "HIGH", "GONE"},
		funding: map[string]map[string]exchanges.FundingData{
			"LOW":  {"binance_futures": fundingAt("LOW", "binance_futures", 1, 28800), "bybit_futures": fundingAt("LOW", "bybit_futures", 0, 28800)},
			"HIGH": {"binance_futures": fundingAt("HIGH", "binance_futures", 9, 28800), "bybit_futures": fundingAt("HIGH", "bybit_futures", 0, 28800)},
		},
		prices: map[string]map[string]PricePoint{
			"LOW":  {"binance_futures": book(100, 100.01), "bybit_futures": book(100, 100.01)},
			"HIGH": {"binance_futures": book(100, 100.01), "bybit_futures": book(100, 100.01)},
		}}
	snap := buildCrossRadar(radarCfg(), in, radarNow)
	if len(snap.Pairs) != 2 || snap.Pairs[0].Symbol != "HIGH" {
		t.Errorf("want HIGH first and GONE (no funding anywhere) absent: %+v", snap.Pairs)
	}
	cfg := radarCfg()
	cfg.Enabled = false
	if off := buildCrossRadar(cfg, in, radarNow); off.Enabled || len(off.Pairs) != 0 {
		t.Errorf("disabled radar published %d pairs", len(off.Pairs))
	}
}

// Nothing on the radar may be called net (CLAUDE.md rule 2).
func TestCrossRadar_NoFieldSaysNet(t *testing.T) {
	raw, err := jsonMarshalForTest(buildCrossRadar(radarCfg(), radarInputs("X", 5, -5, book(100, 100.01), book(100, 100.01)), radarNow))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "net_") || strings.Contains(raw, "profit") {
		t.Errorf("the radar's JSON names a net or profit field: %s", raw)
	}
}

// The seven-day APR can read high on a spread that needs far longer than a wide
// spread has ever lasted to pay its costs; such a pair is watched, not green.
// 3 bps/8h against 21 + 0.01×… bps: breakeven ≈ 21.x / 9 ≈ 2.3 days → good;
// 2.2 bps/8h: after cost at 7 days = 24.09 − ~11 ≈ 13% (< 20, watch anyway),
// so the gate is exercised with a 4-day hold assumption instead.
func TestCrossRadar_BreakevenGate(t *testing.T) {
	cfg := radarCfg()
	cfg.PlannedHoldDays = 30 // cost ≈ 2.6%/yr, so after-cost APR clears 20 easily
	in := radarInputs("X", 2.5, 0, book(100, 100.001), book(100, 100.001))
	p := buildCrossRadar(cfg, in, radarNow).Pairs[0]
	// 2.5 bps/8h = 7.5 bps/day; cost ≈ 21.002 bps ⇒ breakeven ≈ 2.8 days ≤ 3.
	if p.BreakevenHoldDays == nil || math.Abs(*p.BreakevenHoldDays-(21+0.001/100.0005*1e4*2)/7.5) > 1e-9 || p.Status != CrossStatusGood {
		t.Fatalf("breakeven %v status %s", p.BreakevenHoldDays, p.Status)
	}
	in = radarInputs("X", 2.2, 0, book(100, 100.001), book(100, 100.001))
	p = buildCrossRadar(cfg, in, radarNow).Pairs[0]
	// 2.2 bps/8h = 6.6 bps/day ⇒ ≈ 3.2 days > 3, while after-cost APR ≈ 21.5%.
	if *p.AfterCostAPRCapitalPct < 20 || p.Status != CrossStatusWatch || !strings.Contains(strings.Join(p.NotesVI, " "), "trả hết chi phí vòng") {
		t.Fatalf("after-cost %v breakeven %v status %s notes %v", *p.AfterCostAPRCapitalPct, *p.BreakevenHoldDays, p.Status, p.NotesVI)
	}
}

func TestCrossRadar_OnChangeVenueIsNoted(t *testing.T) {
	p := buildCrossRadar(radarCfg(), radarInputs("X", 1, 0, book(100, 100.01), book(100, 100.01)), radarNow).Pairs[0]
	if p.B.FundingPublishMode != "on_change" || !strings.Contains(strings.Join(p.NotesVI, " "), "chỉ phát funding khi rate đổi") {
		t.Errorf("bybit_futures publishes on change per config.yaml: %+v %v", p.B, p.NotesVI)
	}
}
