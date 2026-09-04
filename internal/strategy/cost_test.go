package strategy

import (
	"math"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/fees"
)

func verifiedFee(source string, takerBps float64) fees.Schedule {
	return fees.Schedule{Source: source, TakerFeeBps: takerBps, MakerFeeBps: takerBps / 2, Verified: true}
}

// deepBook is liquid enough that slippage is not what any of these tests are
// measuring.
func deepBook(source string) depth.Summary {
	return bookAt(source, 0.001, 5_000_000, 25_000_000, 1.0)
}

func TestRoundTripCost_ChargesFourFillsOnBothAxes(t *testing.T) {
	got := RoundTripCost(RoundTripInput{
		NotionalQuote: 10_000,
		SpotFee:       verifiedFee("binance_spot", 10),
		PerpFee:       verifiedFee("binance_futures", 5),
		SpotBook:      deepBook("binance_spot"),
		PerpBook:      deepBook("binance_futures"),
	})
	if !got.OK {
		t.Fatalf("two verified venues on two deep books must produce a cost: %s", got.ReasonVI)
	}

	// 10 bps + 5 bps, entry and exit = 30 bps = 0.30%.
	if math.Abs(got.FeesPct-0.30) > 1e-9 {
		t.Errorf("fees = %g%%, want 0.30%%", got.FeesPct)
	}

	// Four fills, each a real number, each on the side its leg actually takes.
	fills := got.Fills()
	if len(fills) != 4 {
		t.Fatalf("a round trip is four fills, got %d", len(fills))
	}
	sides := map[string]Side{
		got.EntrySpotBuy.Source + "/entry-spot":  SideBuy,
		got.EntryPerpSell.Source + "/entry-perp": SideSell,
		got.ExitSpotSell.Source + "/exit-spot":   SideSell,
		got.ExitPerpBuy.Source + "/exit-perp":    SideBuy,
	}
	if len(sides) != 4 {
		t.Fatal("the four fills must be distinguishable")
	}
	if got.EntrySpotBuy.Side != SideBuy || got.ExitSpotSell.Side != SideSell {
		t.Error("the spot leg buys at entry and sells at exit")
	}
	if got.EntryPerpSell.Side != SideSell || got.ExitPerpBuy.Side != SideBuy {
		t.Error("the perp leg sells at entry and buys back at exit")
	}

	var sum float64
	for _, fill := range fills {
		sum += fill.SlippagePct
	}
	if math.Abs(got.SlippagePct-sum) > 1e-12 {
		t.Errorf("slippage = %g%%, want the sum of the four fills %g%%", got.SlippagePct, sum)
	}
	if math.Abs(got.TotalPct-(got.FeesPct+got.SlippagePct)) > 1e-12 {
		t.Errorf("total %g%% is not fees %g%% + slippage %g%%", got.TotalPct, got.FeesPct, got.SlippagePct)
	}
}

func TestRoundTripCost_UnverifiedFeeYieldsNoNumberAtAll(t *testing.T) {
	got := RoundTripCost(RoundTripInput{
		NotionalQuote: 10_000,
		SpotFee:       fees.Schedule{Source: "kraken_spot"}, // never looked up
		PerpFee:       verifiedFee("kraken_futures", 5),
		SpotBook:      deepBook("kraken_spot"),
		PerpBook:      deepBook("kraken_futures"),
	})
	if got.OK {
		t.Fatal("an unverified fee schedule must not produce a cost")
	}
	if got.TotalPct != 0 || got.FeesPct != 0 {
		t.Errorf("a refused cost must carry no number, got fees %g total %g", got.FeesPct, got.TotalPct)
	}
	if got.ReasonVI == "" {
		t.Error("a refusal must say why")
	}
}

func TestRoundTripCost_UnfillableLegRefusesTheWholeTrip(t *testing.T) {
	thin := bookAt("paradex_futures", 0.0335584960365339, 16046.603327, 21543.218886, 14.7096018013016)

	got := RoundTripCost(RoundTripInput{
		NotionalQuote: 60_000, // the size PLAN §7.4 works its example on
		SpotFee:       verifiedFee("binance_spot", 10),
		PerpFee:       verifiedFee("paradex_futures", 0),
		SpotBook:      deepBook("binance_spot"),
		PerpBook:      thin,
	})
	if got.OK {
		t.Fatal("a leg that cannot be filled must refuse the whole round trip")
	}
	if got.TotalPct != 0 {
		t.Errorf("a refused round trip must carry no cost, got %g%%", got.TotalPct)
	}
	if got.ReasonVI == "" {
		t.Error("a refusal must name the leg that could not be filled")
	}
}

func TestRoundTripCost_PropagatesTheLowerBoundLabel(t *testing.T) {
	// Bybit's real perp book stops at 0.091%, so a fill that eats all of it
	// leaves the published levels.
	truncated := bookAt("bybit_futures", 0.000123448026844787, 26_282_425.47, 26_282_425.47, 0.0910429197927308)

	got := RoundTripCost(RoundTripInput{
		NotionalQuote: 26_000_000,
		SpotFee:       verifiedFee("binance_spot", 10),
		PerpFee:       verifiedFee("bybit_futures", 5.5),
		SpotBook:      bookAt("binance_spot", 0.001, 30_000_000, 60_000_000, 1.0),
		PerpBook:      truncated,
	})
	if !got.OK {
		t.Fatalf("26M against a 26.28M book is inside it: %s", got.ReasonVI)
	}
	if !got.DepthIsLowerBound {
		t.Error("a round trip containing a truncated-book fill must carry the label")
	}
	if got.NoteVI == "" {
		t.Error("the label must carry an explanation the dashboard can render")
	}
}

func TestRoundTripCost_NamesWhatWasDeductedAndWhatWasNot(t *testing.T) {
	got := RoundTripCost(RoundTripInput{
		NotionalQuote: 10_000,
		SpotFee:       verifiedFee("binance_spot", 10),
		PerpFee:       verifiedFee("binance_futures", 5),
		SpotBook:      deepBook("binance_spot"),
		PerpBook:      deepBook("binance_futures"),
	})
	if len(got.AppliedVI) == 0 {
		t.Error("a cost must state what it deducted")
	}
	if len(got.ExcludedVI) == 0 {
		t.Error("a cost must state what it did NOT deduct — that is CLAUDE.md rule 2")
	}
}

func TestRoundTripCost_ZeroFeeVenueIsStillANumber(t *testing.T) {
	// A verified zero is DATA — the flag is the only thing separating "this
	// venue charges nothing" from "nobody looked it up" (step 1.3).
	got := RoundTripCost(RoundTripInput{
		NotionalQuote: 10_000,
		SpotFee:       verifiedFee("binance_spot", 10),
		PerpFee:       fees.Schedule{Source: "paradex_futures", Verified: true},
		SpotBook:      deepBook("binance_spot"),
		PerpBook:      deepBook("paradex_futures"),
	})
	if !got.OK {
		t.Fatalf("a verified zero fee is a number: %s", got.ReasonVI)
	}
	if math.Abs(got.FeesPct-0.20) > 1e-9 {
		t.Errorf("fees = %g%%, want 0.20%% (10 bps spot, twice, and nothing on the perp)", got.FeesPct)
	}
}

// internal/strategy/doc.go: nothing here may read the clock — the evaluation
// instant is passed in. This is the first thing in the package that needs one,
// because a depth sweep runs at most hourly and a fill priced against a book
// from before a move is an estimate of the past.
func TestRoundTripCost_RefusesABookOlderThanTheCallersBudget(t *testing.T) {
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	fresh := deepBook("binance_spot")
	fresh.SampledAtMs = at.Add(-2 * time.Minute).UnixMilli()
	stale := deepBook("binance_futures")
	stale.SampledAtMs = at.Add(-3 * time.Hour).UnixMilli()

	in := RoundTripInput{
		NotionalQuote: 10_000,
		SpotFee:       verifiedFee("binance_spot", 10),
		PerpFee:       verifiedFee("binance_futures", 5),
		SpotBook:      fresh,
		PerpBook:      stale,
		At:            at,
		MaxBookAge:    time.Hour,
	}
	if got := RoundTripCost(in); got.OK {
		t.Fatal("a 3-hour-old book must not price a fill under a 1-hour budget")
	}

	// The same books with no budget stated are priced: the backtest has no
	// historical depth to age, so the check has to be opt-in.
	noBudget := in
	noBudget.MaxBookAge = 0
	if got := RoundTripCost(noBudget); !got.OK {
		t.Fatalf("without a stated budget the age is not checked: %s", got.ReasonVI)
	}

	// A budget without an evaluation instant cannot be applied, and silently
	// skipping the check the caller asked for is worse than refusing.
	noClock := in
	noClock.At = time.Time{}
	if got := RoundTripCost(noClock); got.OK {
		t.Error("a freshness budget with no evaluation time must refuse, not skip the check")
	}

	// A book carrying no sample stamp cannot be aged either.
	unstamped := in
	unstamped.PerpBook.SampledAtMs = 0
	if got := RoundTripCost(unstamped); got.OK {
		t.Error("a book with no sample stamp cannot be judged fresh")
	}

	both := in
	both.PerpBook = fresh
	both.PerpBook.Source = "binance_futures"
	if got := RoundTripCost(both); !got.OK {
		t.Fatalf("two fresh books must price: %s", got.ReasonVI)
	}
}

func TestRoundTripCost_RefusesTwoLegsOfDifferentPairs(t *testing.T) {
	spot := deepBook("binance_spot")
	perp := deepBook("binance_futures")
	perp.Symbol = "ETHUSDT"

	got := RoundTripCost(RoundTripInput{
		NotionalQuote: 10_000,
		SpotFee:       verifiedFee("binance_spot", 10),
		PerpFee:       verifiedFee("binance_futures", 5),
		SpotBook:      spot,
		PerpBook:      perp,
	})
	if got.OK {
		t.Fatal("a BTC spot leg against an ETH perp leg is not a hedge")
	}
	if got.ReasonVI == "" {
		t.Error("a refusal must say why")
	}
}

func TestRoundTripCost_CarriesTheIdentityItWasPricedFor(t *testing.T) {
	got := RoundTripCost(RoundTripInput{
		NotionalQuote: 10_000,
		SpotFee:       verifiedFee("binance_spot", 10),
		PerpFee:       verifiedFee("bybit_futures", 5),
		SpotBook:      deepBook("binance_spot"),
		PerpBook:      deepBook("bybit_futures"),
	})
	if !got.OK {
		t.Fatalf("cost: %s", got.ReasonVI)
	}
	if got.Symbol != "BTCUSDT" || got.SpotSource != "binance_spot" || got.PerpSource != "bybit_futures" {
		t.Errorf("identity = %s/%s/%s, want BTCUSDT/binance_spot/bybit_futures",
			got.Symbol, got.SpotSource, got.PerpSource)
	}
}

func TestRoundTripCost_NamesOnlyTheUnverifiedVenue(t *testing.T) {
	got := RoundTripCost(RoundTripInput{
		NotionalQuote: 10_000,
		SpotFee:       fees.Schedule{Source: "kraken_spot"},
		PerpFee:       verifiedFee("kraken_futures", 5),
		SpotBook:      deepBook("kraken_spot"),
		PerpBook:      deepBook("kraken_futures"),
	})
	if got.OK {
		t.Fatal("an unverified schedule must refuse")
	}
	if !strings.Contains(got.ReasonVI, "kraken_spot") {
		t.Errorf("the refusal must name the unverified venue: %s", got.ReasonVI)
	}
	if strings.Contains(got.ReasonVI, "kraken_futures") {
		t.Errorf("the refusal must NOT name the verified venue: %s", got.ReasonVI)
	}
}
