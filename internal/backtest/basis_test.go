package backtest

import (
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/strategy"
)

// hourlyCandles builds a closing series at the top of each hour from epoch.
func hourlyCandles(source string, closes ...float64) []exchanges.PriceCandle {
	out := make([]exchanges.PriceCandle, 0, len(closes))
	for i, close := range closes {
		out = append(out, exchanges.PriceCandle{
			Symbol: "BTCUSDT", Source: source,
			OpenTimeMs: epoch + int64(i)*3600*msPerSec, IntervalSec: 3600,
			OpenPriceQuote: close, HighPriceQuote: close, LowPriceQuote: close,
			ClosePriceQuote: close,
		})
	}
	return out
}

// THE rule of a backtest: never decide with a price from the future. The candle
// CONTAINING a settlement closes up to an hour after it, so the price used must
// be the newest candle that had already finished.
func TestPriceSeries_NeverUsesACandleThatHadNotClosedYet(t *testing.T) {
	series := newPriceSeries(hourlyCandles("binance_spot", 100, 200, 300))

	// The first candle covers [epoch, epoch+1h) and closes at epoch+1h.
	if _, ok := series.closeAt(epoch); ok {
		t.Error("at the very first open no candle has closed; a price here is a price from the future")
	}
	if _, ok := series.closeAt(epoch + 3599*msPerSec); ok {
		t.Error("one second before the first candle closes it is still unfinished")
	}
	got, ok := series.closeAt(epoch + 3600*msPerSec)
	if !ok || got != 100 {
		t.Errorf("at the instant the first candle closes: got %v (%v), want 100", got, ok)
	}
	// Halfway through the SECOND candle the answer is still the first one's
	// close — one hour stale, and never the 200 that has not happened yet.
	got, ok = series.closeAt(epoch + 5400*msPerSec)
	if !ok || got != 100 {
		t.Errorf("mid-second-candle: got %v (%v), want the first candle's 100", got, ok)
	}
}

// A gap is a hole in the data, not a price. Carrying the last close across an
// outage would turn missing information into a confident number.
func TestPriceSeries_RefusesAPriceAcrossALongGap(t *testing.T) {
	series := newPriceSeries([]exchanges.PriceCandle{{
		Symbol: "BTCUSDT", Source: "binance_spot",
		OpenTimeMs: epoch, IntervalSec: 3600, ClosePriceQuote: 100,
	}})

	// One missing candle is tolerated: real venues drop one now and then.
	if _, ok := series.closeAt(epoch + 3*3600*msPerSec); !ok {
		t.Error("two intervals after the close is inside the allowance")
	}
	// Three intervals later is an outage.
	if got, ok := series.closeAt(epoch + 4*3600*msPerSec); ok {
		t.Errorf("a price %v was produced across a gap of three intervals", got)
	}
}

func TestPriceSeries_EmptyAndUnsortedInput(t *testing.T) {
	if _, ok := newPriceSeries(nil).closeAt(epoch); ok {
		t.Error("an empty series produced a price")
	}
	// The store and the fetchers deliver oldest-first, but the lookup sorts
	// anyway: a silently wrong answer here would look like a market that moved.
	candles := hourlyCandles("binance_spot", 100, 200, 300)
	candles[0], candles[2] = candles[2], candles[0]
	got, ok := newPriceSeries(candles).closeAt(epoch + 2*3600*msPerSec)
	if !ok || got != 200 {
		t.Errorf("got %v (%v), want 200 from the shuffled input", got, ok)
	}
}

// The payoff: with candles for both legs the basis condition is judged, and a
// basis blown through its limit closes the position — the one exit rule that
// watches delta-neutrality breaking, untestable in hindsight until step 3.3b.
func TestRun_BasisExitFiresOnStoredCandles(t *testing.T) {
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2, 2, 2, 2)
	series := seriesOf(entries)

	// Hourly closes covering the whole funding window: spot flat at 100, perp
	// flat at 100 until it dislocates to 105 (a 5% basis against a 1% limit).
	hours := int((entries[len(entries)-1].SettledAtMs-entries[0].SettledAtMs)/(3600*msPerSec)) + 4
	spot := make([]float64, hours)
	perp := make([]float64, hours)
	for i := range spot {
		spot[i] = 100
		perp[i] = 100
		if i >= hours/2 {
			perp[i] = 105
		}
	}
	series.SpotCandles = hourlyCandles("binance_spot", spot...)
	series.PerpCandles = hourlyCandles("binance_futures", perp...)
	// The candles must start where the funding series does, not at epoch+0.
	shift := entries[0].SettledAtMs - epoch
	for i := range series.SpotCandles {
		series.SpotCandles[i].OpenTimeMs += shift
		series.PerpCandles[i].OpenTimeMs += shift
	}

	got := Run(series, fullWindow(entries), testParams())
	if !got.OK {
		t.Fatalf("Run: %s", got.ReasonVI)
	}
	if got.BasisEvaluable == 0 {
		t.Fatal("no settlement had prices on both legs; the fixture is wrong, not the engine")
	}
	if len(got.Trades) == 0 {
		t.Fatal("no trade at all")
	}
	if !strings.Contains(got.Trades[0].ExitReasonVI, "basis") {
		t.Errorf("a 5%% basis against a 1%% limit did not close the position; exit was: %s",
			got.Trades[0].ExitReasonVI)
	}
	if !containsAny(got.AssumptionsVI, "ĐƯỢC đánh giá") {
		t.Errorf("a run WITH candles must not claim the basis was unevaluable:\n%s",
			strings.Join(got.AssumptionsVI, "\n"))
	}
}

// Without candles the engine must behave exactly as it did before they
// existed — and SAY so, or a reader cannot tell "never fired" from "never
// tested".
func TestRun_NoCandlesLeavesTheBasisUnevaluableAndSaysSo(t *testing.T) {
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2)
	got := Run(seriesOf(entries), fullWindow(entries), testParams())
	if !got.OK {
		t.Fatalf("Run: %s", got.ReasonVI)
	}
	if got.BasisEvaluable != 0 {
		t.Errorf("BasisEvaluable = %d with no candles at all", got.BasisEvaluable)
	}
	if got.BasisNotEvaluable == 0 {
		t.Error("no exit evaluation reported the basis as unevaluable")
	}
	if !containsAny(got.AssumptionsVI, "KHÔNG đánh giá được") {
		t.Errorf("a run without candles must say the basis was untested:\n%s",
			strings.Join(got.AssumptionsVI, "\n"))
	}
}

// A position opened at a settlement with no price has no measured entry basis,
// and a 0 there is not a measurement — it would read as a drift of the whole
// current basis the moment prices came back.
func TestRun_APositionOpenedWithoutPricesIsCounted(t *testing.T) {
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2, 2)
	series := seriesOf(entries)

	// Candles that only begin AFTER the entry decision: the first three
	// settlements build persistence and entry fires on the third.
	start := entries[3].SettledAtMs
	closes := hourlyCandles("binance_spot", 100, 100, 100, 100, 100, 100, 100, 100, 100)
	perp := hourlyCandles("binance_futures", 100, 100, 100, 100, 100, 100, 100, 100, 100)
	for i := range closes {
		closes[i].OpenTimeMs = start + int64(i)*3600*msPerSec
		perp[i].OpenTimeMs = start + int64(i)*3600*msPerSec
	}
	series.SpotCandles, series.PerpCandles = closes, perp

	got := Run(series, fullWindow(entries), testParams())
	if !got.OK {
		t.Fatalf("Run: %s", got.ReasonVI)
	}
	if got.EnteredWithoutBasis == 0 {
		t.Error("a position opened before any candle existed was not counted as entered blind")
	}
}

// The engine must never grow its own copy of the rule. basisPct exists only to
// stamp the entry basis, and it has to agree with what strategy computes from
// the same two prices — a drift here is a position whose measured drift is
// wrong from the first settlement.
func TestBasisPct_AgreesWithTheStrategyPackage(t *testing.T) {
	const spot, perp = 81050.605, 81455.86
	want := basisPct(spot, perp)

	// strategy reports the basis in the check's own words; the position is
	// opened AT this basis, so a correct stamp makes the movement exactly zero.
	c := strategy.Candidate{
		Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot",
		SpotPriceQuote: spot, PerpPriceQuote: perp,
	}
	pos := strategy.Position{Symbol: "BTCUSDT", EntryBasisPct: want}
	p := strategy.Params{MaxBasisPct: 1.0, MaxBasisWidenPct: 0.0001, ExitPersistencePeriods: 1}

	for _, check := range strategy.EvaluateExit(time.UnixMilli(epoch), pos, c, p).Checks {
		if check.Name != "basis_widened" {
			continue
		}
		if check.Passed {
			t.Errorf("stamping the entry basis with basisPct still produced a drift exit: %s", check.DetailVI)
		}
		return
	}
	t.Fatal("no basis_widened check was reported")
}
