package strategy

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"futures-arbitrage-scanner/internal/depth"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/fees"
)

const (
	secPer8h  = 8 * 3600
	secPer1h  = 3600
	secPer4h  = 4 * 3600
	tolerance = 1e-12
)

func TestGrossAPRFrac_CountsTheVenuesRealCadence(t *testing.T) {
	// Hand-computed: a year holds 31,536,000 seconds.
	cases := []struct {
		name                string
		ratePerIntervalFrac float64
		intervalSec         int64
		wantAPRFrac         float64
	}{
		// 0.01% every 8h → 1095 settlements → 10.95%.
		{"binance 8h", 0.0001, secPer8h, 0.0001 * 1095},
		// 0.01% every 4h → 2190 settlements. Binance moved the majority of its
		// symbols to 4h; reading them as 8h halves the figure.
		{"binance 4h", 0.0001, secPer4h, 0.0001 * 2190},
		// 0.00125% every hour is the SAME per-8h rate as the first case, and
		// must annualize to the same number. Reading Hyperliquid's hourly rate
		// as 8-hourly is the 8× error in CLAUDE.md's trap table.
		{"hyperliquid 1h", 0.0001 / 8, secPer1h, 0.0001 * 1095},
		{"negative rate", -0.0002, secPer8h, -0.0002 * 1095},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GrossAPRFrac(tc.ratePerIntervalFrac, tc.intervalSec)
			if err != nil {
				t.Fatalf("GrossAPRFrac: %v", err)
			}
			if math.Abs(got-tc.wantAPRFrac) > tolerance {
				t.Errorf("APR = %.12f, want %.12f", got, tc.wantAPRFrac)
			}
		})
	}
}

// The live path and the backtest must not be able to disagree about what "APR"
// means, so this pins that they run the SAME arithmetic rather than two copies
// that happen to agree today.
func TestGrossAPRFrac_IsTheSameArithmeticTheConnectorsUse(t *testing.T) {
	for _, intervalSec := range []int64{secPer1h, secPer4h, secPer8h, 28800, 3600 * 2} {
		derived, err := exchanges.DeriveFundingRates(exchanges.FundingData{
			Source: "test", Symbol: "BTCUSDT",
			RatePerIntervalFrac: 0.00013, IntervalSec: intervalSec,
		})
		if err != nil {
			t.Fatalf("DeriveFundingRates: %v", err)
		}
		got, err := GrossAPRFrac(0.00013, intervalSec)
		if err != nil {
			t.Fatalf("GrossAPRFrac: %v", err)
		}
		if got != derived.APRFrac {
			t.Errorf("interval %ds: strategy %v vs exchanges %v — the two paths have drifted",
				intervalSec, got, derived.APRFrac)
		}
	}
}

func TestGrossAPRFrac_RefusesANonPositiveInterval(t *testing.T) {
	for _, intervalSec := range []int64{0, -1, -28800} {
		if _, err := GrossAPRFrac(0.0001, intervalSec); err == nil {
			t.Errorf("interval %d must be refused, not divided by", intervalSec)
		}
	}
}

// fixedCost is a consistent, verified round trip whose total is stated flat, so
// the APR tests measure the APR arithmetic and not the book model.
//
// Built as a literal rather than by mutating a real RoundTripCost result: doing
// the latter left AppliedVI describing measured slippage that the mutation had
// just removed, and AppliedVI is the mechanism enforcing CLAUDE.md rule 2. A
// fixture whose labels contradict its own numbers is the wrong thing to
// normalize in the package that exists to keep the two together.
func fixedCost(perpSource, symbol string, totalPct float64) RoundTrip {
	return RoundTrip{
		Symbol:        symbol,
		SpotSource:    "binance_spot",
		PerpSource:    perpSource,
		NotionalQuote: 10_000,
		FeesPct:       totalPct,
		SlippagePct:   0,
		TotalPct:      totalPct,
		OK:            true,
		AppliedVI:     []string{fmt.Sprintf("Chi phí vào/ra cố định của fixture: %.4f%%.", totalPct)},
		ExcludedVI:    []string{"fixture — không mô hình chi phí nào khác"},
	}
}

func TestNetAPR_AmortizesTheRoundTripOverTheHold(t *testing.T) {
	// 0.01% every 8h, held 30 days, costing 0.24% to get in and out.
	//   settlements  = 30 × 86400 / 28800          = 90
	//   gross return = 0.0001 × 90                 = 0.0090
	//   net return   = 0.0090 − 0.0024             = 0.0066
	//   net APR      = 0.0066 × 365 / 30           = 0.0803
	got := NetAPR(NetAPRInput{
		Source: "binance_futures", Symbol: "BTCUSDT",
		Model:               exchanges.FundingDiscrete,
		RatePerIntervalFrac: 0.0001,
		IntervalSec:         secPer8h,
		HoldingDays:         30,
		Cost:                fixedCost("binance_futures", "BTCUSDT", 0.24),
	})
	if !got.OK {
		t.Fatalf("NetAPR: %s", got.ReasonVI)
	}
	if got.SettlementsInHold != 90 {
		t.Errorf("settlements = %d, want 90", got.SettlementsInHold)
	}
	if math.Abs(got.GrossReturnHoldFrac-0.0090) > tolerance {
		t.Errorf("gross return = %.12f, want 0.0090", got.GrossReturnHoldFrac)
	}
	if math.Abs(got.NetReturnHoldFrac-0.0066) > tolerance {
		t.Errorf("net return = %.12f, want 0.0066", got.NetReturnHoldFrac)
	}
	if want := 0.0066 * 365 / 30; math.Abs(got.NetAPRFrac-want) > tolerance {
		t.Errorf("net APR = %.12f, want %.12f", got.NetAPRFrac, want)
	}
	// The gross figure must stay visible beside it, and must be the bigger one.
	if math.Abs(got.GrossAPRFrac-0.1095) > tolerance {
		t.Errorf("gross APR = %.12f, want 0.1095", got.GrossAPRFrac)
	}
	if got.NetAPRFrac >= got.GrossAPRFrac {
		t.Error("a cost was charged; net must be below gross")
	}
}

// CLAUDE.md rule 6: a position earns nothing unless it is open at a settlement.
func TestNetAPR_PaysNothingForAHoldThatCrossesNoSettlement(t *testing.T) {
	got := NetAPR(NetAPRInput{
		Source: "binance_futures", Symbol: "BTCUSDT",
		Model:               exchanges.FundingDiscrete,
		RatePerIntervalFrac: 0.0001,
		IntervalSec:         secPer8h,
		HoldingDays:         (8*3600 - 60) / 86400.0, // 7h59m
		Cost:                fixedCost("binance_futures", "BTCUSDT", 0.24),
	})
	if !got.OK {
		t.Fatalf("NetAPR: %s", got.ReasonVI)
	}
	if got.SettlementsInHold != 0 {
		t.Errorf("settlements = %d, want 0 — 7h59m of an 8h period pays nothing", got.SettlementsInHold)
	}
	if got.GrossReturnHoldFrac != 0 {
		t.Errorf("gross return = %v, want exactly 0", got.GrossReturnHoldFrac)
	}
	if got.NetAPRFrac >= 0 {
		t.Errorf("net APR = %v: a hold that collects nothing and pays a round trip must be negative", got.NetAPRFrac)
	}
}

// Two venues quoting the same per-8h rate must reach the same hold return, or
// the cadence has been applied twice.
func TestNetAPR_IsCadenceIndependentAtEqualPer8hRates(t *testing.T) {
	eightHourly := NetAPR(NetAPRInput{
		Source: "binance_futures", Symbol: "BTCUSDT", Model: exchanges.FundingDiscrete,
		RatePerIntervalFrac: 0.0001, IntervalSec: secPer8h,
		HoldingDays: 30, Cost: fixedCost("binance_futures", "BTCUSDT", 0.24),
	})
	hourly := NetAPR(NetAPRInput{
		Source: "hyperliquid_futures", Symbol: "BTCUSDT", Model: exchanges.FundingDiscrete,
		RatePerIntervalFrac: 0.0001 / 8, IntervalSec: secPer1h,
		HoldingDays: 30, Cost: fixedCost("hyperliquid_futures", "BTCUSDT", 0.24),
	})
	if hourly.SettlementsInHold != 720 {
		t.Errorf("hourly settlements = %d, want 720", hourly.SettlementsInHold)
	}
	if math.Abs(eightHourly.NetAPRFrac-hourly.NetAPRFrac) > tolerance {
		t.Errorf("8h %.12f vs 1h %.12f — the same per-8h rate must annualize the same",
			eightHourly.NetAPRFrac, hourly.NetAPRFrac)
	}
}

// Paradex accrues continuously and settles nothing. Counting its samples as
// settlements is the 8,760-payments-a-year error; NOT accruing at all would be
// the opposite mistake.
func TestNetAPR_ContinuousAccruesAcrossAPartialWindow(t *testing.T) {
	got := NetAPR(NetAPRInput{
		Source: "paradex_futures", Symbol: "BTCUSDT",
		Model:               exchanges.FundingContinuous,
		RatePerIntervalFrac: 0.0001, // per the venue's 8h QUOTE window
		IntervalSec:         secPer8h,
		HoldingDays:         4.0 / 24.0, // half a quote window
		Cost:                fixedCost("paradex_futures", "BTCUSDT", 0),
	})
	if !got.OK {
		t.Fatalf("NetAPR: %s", got.ReasonVI)
	}
	if got.SettlementsInHold != 0 {
		t.Errorf("settlements = %d: a continuous venue settles nothing at all", got.SettlementsInHold)
	}
	if want := 0.0001 * 0.5; math.Abs(got.GrossReturnHoldFrac-want) > tolerance {
		t.Errorf("gross return = %.12f, want %.12f — half a window accrues half the rate",
			got.GrossReturnHoldFrac, want)
	}

	// The same inputs read as discrete would pay nothing, which is exactly why
	// the branch exists.
	asDiscrete := NetAPR(NetAPRInput{
		Source: "paradex_futures", Symbol: "BTCUSDT", Model: exchanges.FundingDiscrete,
		RatePerIntervalFrac: 0.0001, IntervalSec: secPer8h,
		HoldingDays: 4.0 / 24.0, Cost: fixedCost("paradex_futures", "BTCUSDT", 0),
	})
	if asDiscrete.GrossReturnHoldFrac != 0 {
		t.Error("the discrete branch must pay nothing for a partial period")
	}
}

func TestNetAPR_RefusesWhenTheCostCouldNotBeComputed(t *testing.T) {
	refused := RoundTripCost(RoundTripInput{
		NotionalQuote: 10_000,
		SpotFee:       fees.Schedule{Source: "unverified"},
		PerpFee:       verifiedFee("perp", 5),
		SpotBook:      deepBook("spot"),
		PerpBook:      deepBook("perp"),
	})
	got := NetAPR(NetAPRInput{
		Source: "perp", Symbol: "BTCUSDT", Model: exchanges.FundingDiscrete,
		RatePerIntervalFrac: 0.0001, IntervalSec: secPer8h, HoldingDays: 30, Cost: refused,
	})
	if got.OK {
		t.Fatal("no cost means no net figure — CLAUDE.md rule 2")
	}
	if got.NetAPRFrac != 0 {
		t.Errorf("a refused net APR must carry no number, got %v", got.NetAPRFrac)
	}
	if got.ReasonVI == "" {
		t.Error("a refusal must say why")
	}
}

func TestNetAPR_RefusesUnusableInputs(t *testing.T) {
	base := NetAPRInput{
		Source: "binance_futures", Symbol: "BTCUSDT", Model: exchanges.FundingDiscrete,
		RatePerIntervalFrac: 0.0001, IntervalSec: secPer8h, HoldingDays: 30,
	}
	base.Cost = fixedCost("binance_futures", "BTCUSDT", 0.2)

	noInterval := base
	noInterval.IntervalSec = 0
	if NetAPR(noInterval).OK {
		t.Error("a zero interval must be refused, not divided by")
	}

	noHold := base
	noHold.HoldingDays = 0
	if NetAPR(noHold).OK {
		t.Error("a zero holding period must be refused, not divided by")
	}
}

// The acceptance criterion of step 3.1: a thin book must visibly cost APR.
func TestNetAPR_ThinBookCostsRealAPRAgainstIgnoringSlippage(t *testing.T) {
	const notionalQuote = 20_000

	// The real rows of depth_snapshots, both sides kept apart. Feeding the bid
	// figures to both sides — which an earlier version of this test did — makes
	// the fixture a different world from the acceptance run and produced two
	// numbers that were then quoted as one measurement.
	withSlippage := RoundTripCost(RoundTripInput{
		NotionalQuote: notionalQuote,
		SpotFee:       verifiedFee("binance_spot", 10),
		// config.yaml's schedule, not a rounded-off zero: Paradex retail is 0%
		// but the config deliberately carries the PRO tier (4.5 bps taker) so
		// the after-fee figure is a lower bound on what is kept.
		PerpFee:  verifiedFee("paradex_futures", 4.5),
		SpotBook: storedBinanceSpotBTC(),
		PerpBook: storedParadexBTC(),
	})
	if !withSlippage.OK {
		t.Fatalf("20k must fit inside a 21.5k book: %s", withSlippage.ReasonVI)
	}

	feesOnly := withSlippage
	feesOnly.SlippagePct = 0
	feesOnly.TotalPct = feesOnly.FeesPct

	in := NetAPRInput{
		Source: "paradex_futures", Symbol: "BTCUSDT", Model: exchanges.FundingContinuous,
		RatePerIntervalFrac: 0.0001, IntervalSec: secPer8h, HoldingDays: 30,
	}
	in.Cost = withSlippage
	net := NetAPR(in)
	in.Cost = feesOnly
	ignoringSlippage := NetAPR(in)

	if !net.OK || !ignoringSlippage.OK {
		t.Fatalf("both figures must compute: %s / %s", net.ReasonVI, ignoringSlippage.ReasonVI)
	}
	if net.NetAPRFrac >= ignoringSlippage.NetAPRFrac {
		t.Fatal("slippage must reduce the net APR")
	}
	gap := ignoringSlippage.NetAPRFrac - net.NetAPRFrac
	if gap < 0.01 {
		t.Errorf("ignoring slippage on a %v-deep book flatters the APR by only %.4f — "+
			"the thin-book case must be visible in whole percentage points", notionalQuote, gap)
	}
	t.Logf("paradex 20k: net APR %.2f%% with slippage, %.2f%% ignoring it (gap %.2f points); "+
		"round trip %.4f%% of which slippage %.4f%%",
		net.NetAPRFrac*100, ignoringSlippage.NetAPRFrac*100, gap*100,
		withSlippage.TotalPct, withSlippage.SlippagePct)
}

// A realistic BTC setup must land in the band CLAUDE.md states for this
// strategy. A number far outside it is a bug in this file, not an edge.
func TestNetAPR_RealisticSetupLandsInThePlausibleBand(t *testing.T) {
	cost := RoundTripCost(RoundTripInput{
		NotionalQuote: 50_000,
		SpotFee:       verifiedFee("binance_spot", 10),
		PerpFee:       verifiedFee("binance_futures", 5),
		SpotBook:      storedBinanceSpotBTC(),
		PerpBook:      storedBinanceFuturesBTC(),
	})
	if !cost.OK {
		t.Fatalf("cost: %s", cost.ReasonVI)
	}
	got := NetAPR(NetAPRInput{
		Source: "binance_futures", Symbol: "BTCUSDT", Model: exchanges.FundingDiscrete,
		RatePerIntervalFrac: 0.0001, IntervalSec: secPer8h, HoldingDays: 30, Cost: cost,
	})
	if !got.OK {
		t.Fatalf("NetAPR: %s", got.ReasonVI)
	}
	if got.NetAPRFrac <= 0 || got.NetAPRFrac > 0.15 {
		t.Errorf("net APR = %.2f%%: outside the 5-15%% band this strategy is expected to produce; "+
			"a number far above it is a counting bug, not an edge", got.NetAPRFrac*100)
	}
	t.Logf("binance BTC 50k, 0.01%%/8h, 30 days: gross %.2f%% → net %.2f%% (round trip %.4f%%)",
		got.GrossAPRFrac*100, got.NetAPRFrac*100, cost.TotalPct)
}

func TestNetAPR_RefusesNonFiniteInput(t *testing.T) {
	base := NetAPRInput{
		Source: "binance_futures", Symbol: "BTCUSDT", Model: exchanges.FundingDiscrete,
		RatePerIntervalFrac: 0.0001, IntervalSec: secPer8h, HoldingDays: 30,
	}
	base.Cost = fixedCost("binance_futures", "BTCUSDT", 0.2)

	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		badRate := base
		badRate.RatePerIntervalFrac = bad
		if got := NetAPR(badRate); got.OK {
			t.Errorf("rate %v must be refused, got net APR %v", bad, got.NetAPRFrac)
		}
		badHold := base
		badHold.HoldingDays = bad
		if got := NetAPR(badHold); got.OK {
			t.Errorf("holding days %v must be refused, got net APR %v", bad, got.NetAPRFrac)
		}
	}

	// A holding period longer than the strategy models is a caller bug, and
	// float64→int64 conversion of an out-of-range settlement count is not
	// defined to produce anything sensible.
	absurd := base
	absurd.HoldingDays = 1e18
	if got := NetAPR(absurd); got.OK {
		t.Errorf("a 1e18-day hold must be refused, got %d settlements", got.SettlementsInHold)
	}
}

func TestRoundTripCost_RefusesNonFiniteNotional(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1)} {
		got := RoundTripCost(RoundTripInput{
			NotionalQuote: bad,
			SpotFee:       verifiedFee("spot", 10),
			PerpFee:       verifiedFee("perp", 5),
			SpotBook:      deepBook("spot"),
			PerpBook:      deepBook("perp"),
		})
		if got.OK {
			t.Errorf("notional %v must be refused, got total %v%%", bad, got.TotalPct)
		}
	}
}

// A cost priced on one position, applied to another's funding rate, produces a
// number that looks entirely reasonable and is about nothing.
func TestNetAPR_RefusesACostPricedForADifferentPosition(t *testing.T) {
	base := NetAPRInput{
		Source: "binance_futures", Symbol: "BTCUSDT", Model: exchanges.FundingDiscrete,
		RatePerIntervalFrac: 0.0001, IntervalSec: secPer8h, HoldingDays: 30,
	}

	wrongVenue := base
	wrongVenue.Cost = fixedCost("bybit_futures", "BTCUSDT", 0.24)
	if got := NetAPR(wrongVenue); got.OK {
		t.Error("a bybit cost must not annualize a binance rate")
	}

	wrongPair := base
	wrongPair.Cost = fixedCost("binance_futures", "ETHUSDT", 0.24)
	if got := NetAPR(wrongPair); got.OK {
		t.Error("an ETH cost must not annualize a BTC rate")
	}

	matching := base
	matching.Cost = fixedCost("binance_futures", "BTCUSDT", 0.24)
	if got := NetAPR(matching); !got.OK {
		t.Fatalf("a matching identity must compute: %s", got.ReasonVI)
	}
}

// Rule 2 is enforced by the labels travelling with the number, so the
// denominator the percentage is measured against has to be one of them.
func TestRoundTripCost_StatesItsDenominator(t *testing.T) {
	got := RoundTripCost(RoundTripInput{
		NotionalQuote: 10_000,
		SpotFee:       verifiedFee("binance_spot", 10),
		PerpFee:       verifiedFee("binance_futures", 5),
		SpotBook:      deepBook("binance_spot"),
		PerpBook:      deepBook("binance_futures"),
	})
	if !got.OK {
		t.Fatalf("cost: %s", got.ReasonVI)
	}
	var stated bool
	for _, line := range got.AppliedVI {
		if strings.Contains(line, "MẪU SỐ") {
			stated = true
		}
	}
	if !stated {
		t.Error("AppliedVI must say what the percentages are a percentage OF — " +
			"funding is earned on one leg's notional, while a delta-neutral position ties up more capital than that")
	}
}

// The three books below are `depth_snapshots` rows for BTCUSDT as they were
// measured on 2026-09-04, transcribed verbatim — both sides, because the sides
// differ and the exit leg trades against the other one.
//
//	sqlite3 data/scanner.db "SELECT source, spread_pct,
//	  bid_depth_within_0_1pct_quote, bid_depth_within_0_5pct_quote, bid_span_pct,
//	  ask_depth_within_0_1pct_quote, ask_depth_within_0_5pct_quote, ask_span_pct
//	  FROM depth_snapshots WHERE symbol='BTCUSDT'"

func storedBinanceSpotBTC() depth.Summary {
	return depth.Summary{
		Source: "binance_spot", Symbol: "BTCUSDT",
		MidPriceQuote: 81050.605, BestBidQuote: 81050.6, BestAskQuote: 81050.61,
		SpreadPct:                1.23379708205279e-05,
		BidDepthWithinTightQuote: 8_236_224.1310699, BidDepthWithinWideQuote: 12_476_103.4675101,
		AskDepthWithinTightQuote: 7_837_598.4077681, AskDepthWithinWideQuote: 12_607_195.3525245,
		BidSpanPct: 0.310010023984416, AskSpanPct: 0.250750750102342,
	}
}

func storedBinanceFuturesBTC() depth.Summary {
	return depth.Summary{
		Source: "binance_futures", Symbol: "BTCUSDT",
		MidPriceQuote: 81043.75, BestBidQuote: 81043.7, BestAskQuote: 81043.8,
		SpreadPct:                0.000123394704303514,
		BidDepthWithinTightQuote: 18_605_496.4404, BidDepthWithinWideQuote: 36_748_527.3936,
		AskDepthWithinTightQuote: 17_405_099.6739, AskDepthWithinWideQuote: 35_492_824.8514,
		BidSpanPct: 0.172, AskSpanPct: 0.166,
	}
}

func storedParadexBTC() depth.Summary {
	return depth.Summary{
		Source: "paradex_futures", Symbol: "BTCUSDT",
		MidPriceQuote: 81052.5, BestBidQuote: 81038.9, BestAskQuote: 81066.1,
		SpreadPct:                0.0335584960365339,
		BidDepthWithinTightQuote: 16_046.603327, BidDepthWithinWideQuote: 21_543.218886,
		AskDepthWithinTightQuote: 14_135.248613, AskDepthWithinWideQuote: 27_807.826268,
		BidSpanPct: 14.7096018013016, AskSpanPct: 279.466025107184,
	}
}
