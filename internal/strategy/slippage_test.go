package strategy

import (
	"math"
	"testing"

	"futures-arbitrage-scanner/internal/depth"
)

// bookAt builds a summary whose cumulative depth curve is stated directly, so a
// test can be checked by hand without owning an order book.
func bookAt(source string, spreadPct, tight, wide, spanPct float64) depth.Summary {
	return depth.Summary{
		Source:                   source,
		Symbol:                   "BTCUSDT",
		MidPriceQuote:            100,
		BestBidQuote:             100 * (1 - spreadPct/200),
		BestAskQuote:             100 * (1 + spreadPct/200),
		SpreadPct:                spreadPct,
		BidDepthWithinTightQuote: tight,
		AskDepthWithinTightQuote: tight,
		BidDepthWithinWideQuote:  wide,
		AskDepthWithinWideQuote:  wide,
		BidSpanPct:               spanPct,
		AskSpanPct:               spanPct,
	}
}

func TestEstimateFill_HandCheckedAcrossBothSegments(t *testing.T) {
	// Curve: (0 quote, 0.01%) → (1000, 0.1%) → (5000, 0.5%).
	// Half the spread is 0.01%, so nothing fills closer to mid than that.
	book := bookAt("test", 0.02, 1000, 5000, 1.0)

	cases := []struct {
		name          string
		notionalQuote float64
		wantPct       float64
	}{
		// Whole first segment: the average of its two endpoints.
		{"exactly the tight window", 1000, (0.01 + 0.1) / 2},
		// Half of it: offset reaches 0.055%, average of 0.01 and 0.055.
		{"half the tight window", 500, (0.01 + 0.055) / 2},
		// Both segments: 55 + 400 quote·% over 3000 quote.
		{"into the wide window", 3000, (55.0 + 400.0) / 3000},
		// The whole measured book: 55 + (0.1+0.5)/2*4000 = 1255 over 5000.
		{"the whole book", 5000, 1255.0 / 5000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EstimateFill(book, SideBuy, tc.notionalQuote)
			if !got.Fillable {
				t.Fatalf("fill of %v must be inside the measured book: %s", tc.notionalQuote, got.ReasonVI)
			}
			if math.Abs(got.SlippagePct-tc.wantPct) > 1e-12 {
				t.Errorf("slippage = %.12f%%, want %.12f%%", got.SlippagePct, tc.wantPct)
			}
		})
	}
}

func TestEstimateFill_SmallFillStillPaysHalfTheSpread(t *testing.T) {
	book := bookAt("test", 0.02, 1000, 5000, 1.0)

	got := EstimateFill(book, SideBuy, 0.001)
	if !got.Fillable {
		t.Fatal("a dust fill is fillable")
	}
	// A taker never fills at the mid: the floor is half the spread.
	if got.SlippagePct < 0.01 {
		t.Errorf("slippage = %g%%, must not be below half the spread (0.01%%)", got.SlippagePct)
	}
}

func TestEstimateFill_RefusesBeyondTheMeasuredBook(t *testing.T) {
	// The venue reached past the wide window, so "not enough" is a real
	// liquidity verdict about the venue.
	deep := bookAt("deep", 0.02, 1000, 5000, 1.0)
	got := EstimateFill(deep, SideBuy, 5001)
	if got.Fillable {
		t.Fatal("a fill larger than the measured book must be refused")
	}
	if got.SlippagePct != 0 {
		t.Errorf("a refused fill must not carry a number: %g", got.SlippagePct)
	}
	if got.ReasonVI == "" {
		t.Error("a refusal must say why")
	}

	// The venue truncated its book before the wide window, so "not enough" is a
	// statement about the RESPONSE, not about the venue. The two must not read
	// the same.
	truncated := bookAt("truncated", 0.02, 1000, 1000, 0.05)
	short := EstimateFill(truncated, SideBuy, 1001)
	if short.Fillable {
		t.Fatal("a fill past a truncated book must be refused too")
	}
	if short.ReasonVI == got.ReasonVI {
		t.Error("a truncated book and a thin book must not give the same reason")
	}
}

func TestEstimateFill_FlagsAnEstimateThatLeavesThePublishedLevels(t *testing.T) {
	// Bybit's BTC perp book, measured 2026-09-04: 500 levels spanning 0.091%
	// on the bid, which does not even reach the 0.1% window.
	book := bookAt("bybit_futures", 0.000123448026844787, 26282425.47, 26282425.47, 0.0910429197927308)

	// A fill well inside the published levels is a measurement.
	small := EstimateFill(book, SideSell, 100_000)
	if !small.Fillable {
		t.Fatalf("100k against a 26M book is fillable: %s", small.ReasonVI)
	}
	if small.DepthIsLowerBound {
		t.Errorf("a fill reaching %.4f%% is inside the published 0.0910%%; it is a measurement, not a floor",
			small.ReachedOffsetPct)
	}

	// A fill that consumes the whole published book leaves them.
	whole := EstimateFill(book, SideSell, 26_282_425)
	if !whole.Fillable {
		t.Fatalf("the whole measured book is fillable by definition: %s", whole.ReasonVI)
	}
	if !whole.DepthIsLowerBound {
		t.Error("a fill reaching past the last published level must be labelled a lower bound")
	}
	if whole.NoteVI == "" {
		t.Error("the lower-bound label must carry an explanation")
	}
}

func TestEstimateFill_RefusesAnUnusableBook(t *testing.T) {
	broken := depth.Summary{Source: "x", Symbol: "BTCUSDT", ErrVI: "sàn không trả sổ"}
	if got := EstimateFill(broken, SideBuy, 1000); got.Fillable {
		t.Error("a book that could not be fetched cannot price a fill")
	}

	empty := bookAt("empty", 0.02, 0, 0, 0)
	if got := EstimateFill(empty, SideBuy, 1000); got.Fillable {
		t.Error("a book with no depth in either window cannot price a fill")
	}

	book := bookAt("test", 0.02, 1000, 5000, 1.0)
	if got := EstimateFill(book, SideBuy, 0); got.Fillable {
		t.Error("a zero-notional fill is a caller bug, not a free trade")
	}
}

// The step-2.7b acceptance measured Paradex's BTC book three orders of
// magnitude thinner than Bybit's while both quoted the same funding rate
// (PLAN.md step 2.7b). That is the case PLAN §7.4 says a screener without depth
// gets backwards, so it is pinned here with the real numbers.
func TestEstimateFill_ThinVenueCostsOrdersOfMagnitudeMore(t *testing.T) {
	bybit := bookAt("bybit_futures", 0.000123448026844787, 26282425.47, 26282425.47, 0.0910429197927308)
	paradex := bookAt("paradex_futures", 0.0335584960365339, 16046.603327, 21543.218886, 14.7096018013016)

	const notionalQuote = 20_000

	onBybit := EstimateFill(bybit, SideSell, notionalQuote)
	if !onBybit.Fillable {
		t.Fatalf("20k on a 26M book: %s", onBybit.ReasonVI)
	}
	onParadex := EstimateFill(paradex, SideSell, notionalQuote)
	if !onParadex.Fillable {
		t.Fatalf("20k on a 21.5k book must still be inside it: %s", onParadex.ReasonVI)
	}

	if onParadex.SlippagePct <= onBybit.SlippagePct*100 {
		t.Errorf("paradex %.4f%% vs bybit %.6f%%: the thin book must cost orders of magnitude more",
			onParadex.SlippagePct, onBybit.SlippagePct)
	}
}

// A NaN or an infinity must be refused AT THE BORDER, not carried.
//
// Every comparison against a NaN is false, so an unguarded NaN passes each
// threshold test in the signal layer by failing it silently, and it poisons any
// sum a backtest accumulates it into. The codebase refuses malformed numbers
// where they arrive (exchanges.DeriveFundingRates does the same for a
// non-positive interval); this is that rule applied to the size and the book.
func TestEstimateFill_RefusesNonFiniteInput(t *testing.T) {
	book := bookAt("test", 0.02, 1000, 5000, 1.0)

	for _, notionalQuote := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		got := EstimateFill(book, SideBuy, notionalQuote)
		if got.Fillable {
			t.Errorf("notional %v must be refused, not priced", notionalQuote)
		}
		if got.ReasonVI == "" {
			t.Errorf("notional %v: a refusal must say why", notionalQuote)
		}
	}

	for _, name := range []string{"spread", "tight", "wide", "span"} {
		broken := book
		switch name {
		case "spread":
			broken.SpreadPct = math.NaN()
		case "tight":
			broken.AskDepthWithinTightQuote = math.NaN()
		case "wide":
			broken.AskDepthWithinWideQuote = math.Inf(1)
		case "span":
			broken.AskSpanPct = math.NaN()
		}
		got := EstimateFill(broken, SideBuy, 500)
		if got.Fillable {
			t.Errorf("a book with a non-finite %s must be refused, not priced (got %v%%)", name, got.SlippagePct)
		}
	}
}

// asymmetricBook has a THIN bid and a FAT ask. Every other fixture in this file
// is symmetric, which made the whole buy→ask / sell→bid mapping untestable: a
// review inverted `sideDepth` and the entire suite still passed. That mapping is
// the reason cost.go exists at all — the leg that hurts is SELLING the spot at
// exit, and pricing it against the ask side would hide exactly that.
func asymmetricBook() depth.Summary {
	return depth.Summary{
		Source: "asym", Symbol: "BTCUSDT",
		MidPriceQuote: 100, BestBidQuote: 99.99, BestAskQuote: 100.01, SpreadPct: 0.02,
		BidDepthWithinTightQuote: 1_000, BidDepthWithinWideQuote: 2_000,
		AskDepthWithinTightQuote: 50_000, AskDepthWithinWideQuote: 250_000,
		BidSpanPct: 1.0, AskSpanPct: 1.0,
	}
}

func TestEstimateFill_BuyReadsTheAskSideAndSellReadsTheBid(t *testing.T) {
	book := asymmetricBook()

	// Selling walks the thin bid: 1000 is the whole 0.1% window, so the average
	// is the mean of its endpoints, (0.01 + 0.1)/2.
	sell := EstimateFill(book, SideSell, 1_000)
	if !sell.Fillable {
		t.Fatalf("1000 against a 2000 bid book: %s", sell.ReasonVI)
	}
	if want := (0.01 + 0.1) / 2; math.Abs(sell.SlippagePct-want) > 1e-12 {
		t.Errorf("sell = %.12f%%, want %.12f%% — a sell must price against the BID side", sell.SlippagePct, want)
	}

	// Buying walks the fat ask: 1000 of 50000 reaches only 0.0118%.
	buy := EstimateFill(book, SideBuy, 1_000)
	if !buy.Fillable {
		t.Fatalf("1000 against a 50000 ask book: %s", buy.ReasonVI)
	}
	if want := (0.01 + (0.01 + 0.09*1_000/50_000)) / 2; math.Abs(buy.SlippagePct-want) > 1e-12 {
		t.Errorf("buy = %.12f%%, want %.12f%% — a buy must price against the ASK side", buy.SlippagePct, want)
	}

	if !(sell.SlippagePct > buy.SlippagePct*4) {
		t.Errorf("sell %.6f%% vs buy %.6f%%: a book this lopsided must cost far more to sell into",
			sell.SlippagePct, buy.SlippagePct)
	}

	// And the fillability gate reads the right side too: 2001 exhausts the bid
	// while the ask absorbs it without noticing.
	if EstimateFill(book, SideSell, 2_001).Fillable {
		t.Error("2001 exceeds the 2000-deep bid side and must be refused")
	}
	if !EstimateFill(book, SideBuy, 2_001).Fillable {
		t.Error("2001 is well inside the 250000-deep ask side")
	}
}

func TestEstimateFill_ReachedOffsetIsTheLastUnitsDistance(t *testing.T) {
	book := bookAt("test", 0.02, 1000, 5000, 1.0)

	// Half way through the second segment: 0.1 + 0.4 × (2000/4000).
	got := EstimateFill(book, SideBuy, 3_000)
	if !got.Fillable {
		t.Fatalf("3000 is inside a 5000 book: %s", got.ReasonVI)
	}
	if want := 0.3; math.Abs(got.ReachedOffsetPct-want) > 1e-12 {
		t.Errorf("reached = %.12f%%, want %.12f%%", got.ReachedOffsetPct, want)
	}
	// The average must sit strictly between the entry offset and the reach, or
	// the integration is not an average of anything.
	if got.SlippagePct <= 0.01 || got.SlippagePct >= got.ReachedOffsetPct {
		t.Errorf("average %.6f%% must lie between the half-spread 0.01%% and the reach %.6f%%",
			got.SlippagePct, got.ReachedOffsetPct)
	}
}

// A spread wider than the 0.1% window leaves no depth inside it, so the curve
// has to drop that point rather than build a backward segment.
func TestEstimateFill_SpreadWiderThanTheTightWindow(t *testing.T) {
	book := depth.Summary{
		Source: "wide-spread", Symbol: "BTCUSDT",
		MidPriceQuote: 100, BestBidQuote: 99.85, BestAskQuote: 100.15, SpreadPct: 0.3,
		// Nothing rests within 0.1% of mid: the best ask is already 0.15% away.
		BidDepthWithinTightQuote: 0, AskDepthWithinTightQuote: 0,
		BidDepthWithinWideQuote: 50_000, AskDepthWithinWideQuote: 50_000,
		BidSpanPct: 1.0, AskSpanPct: 1.0,
	}

	got := EstimateFill(book, SideBuy, 25_000)
	if !got.Fillable {
		t.Fatalf("25000 of a 50000 book: %s", got.ReasonVI)
	}
	// Curve is (0, 0.15) → (50000, 0.5); half of it reaches 0.325, mean 0.2375.
	if want := 0.2375; math.Abs(got.SlippagePct-want) > 1e-12 {
		t.Errorf("slippage = %.12f%%, want %.12f%%", got.SlippagePct, want)
	}
	if want := 0.325; math.Abs(got.ReachedOffsetPct-want) > 1e-12 {
		t.Errorf("reached = %.12f%%, want %.12f%%", got.ReachedOffsetPct, want)
	}
	if got.SlippagePct <= 0.15 {
		t.Error("no fill may cost less than half the spread")
	}
}
