package backtest

import (
	"sort"

	"futures-arbitrage-scanner/exchanges"
)

// Historical basis for the replay (step 3.3b).
//
// Until price_history existed the engine passed NO prices, so
// strategy.EvaluateExit's basis condition reported "not evaluable" at every
// settlement of every run — the one exit rule that watches delta-neutrality
// breaking was untested for the whole of step 3.3. This file is what feeds it.

// maxBasisCandleGapIntervals bounds how stale a candle may be and still price a
// settlement.
//
// One interval of slack is the normal case: the newest CLOSED hourly candle at
// a settlement is always between zero and one hour old. Two allows a single
// missing candle, which every venue produces occasionally. Beyond that the
// venue had an outage, and a price from before an outage is not the price at
// this instant — the basis then reports NOT EVALUABLE, exactly as it did when
// there were no candles at all. Filling the gap with the last known price would
// turn a hole in the data into a confident number, which is the failure the
// NotEvaluated flag exists to prevent.
const maxBasisCandleGapIntervals = 2

// priceSeries answers "what had this market last traded at, as of instant T"
// from a candle series sorted oldest first.
type priceSeries struct {
	candles []exchanges.PriceCandle
	// closeMs[i] is when candles[i] finished, precomputed so the lookup can
	// binary-search on the value it actually compares.
	closeMs []int64
}

func newPriceSeries(candles []exchanges.PriceCandle) priceSeries {
	sorted := make([]exchanges.PriceCandle, len(candles))
	copy(sorted, candles)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].OpenTimeMs < sorted[j].OpenTimeMs })

	closeMs := make([]int64, len(sorted))
	for i, candle := range sorted {
		closeMs[i] = candle.OpenTimeMs + candle.IntervalSec*msPerSecond
	}
	return priceSeries{candles: sorted, closeMs: closeMs}
}

// closeAt returns the close of the newest candle that had FULLY CLOSED at or
// before atMs, and whether one was close enough in time to be usable.
//
// Fully closed, not "the candle containing this instant". The containing
// candle's close is stamped at the END of its interval, which is up to an hour
// AFTER the settlement being decided — using it would let the replay decide
// with a price from the future, which is the one error a backtest must never
// make. The cost is that the price is up to one interval old, and that is
// stated in the assumptions rather than hidden.
func (p priceSeries) closeAt(atMs int64) (float64, bool) {
	if len(p.candles) == 0 {
		return 0, false
	}
	// First index whose close is strictly after atMs; the one before it is the
	// newest that had already finished.
	i := sort.Search(len(p.closeMs), func(i int) bool { return p.closeMs[i] > atMs })
	if i == 0 {
		return 0, false // every candle we hold closes after this instant
	}
	candle := p.candles[i-1]
	maxAgeMs := candle.IntervalSec * msPerSecond * maxBasisCandleGapIntervals
	if atMs-p.closeMs[i-1] > maxAgeMs {
		return 0, false // a gap, not a price
	}
	if candle.ClosePriceQuote <= 0 {
		return 0, false
	}
	return candle.ClosePriceQuote, true
}

// basisPct is the perp-over-spot difference in percent, and whether both legs
// had a usable price.
//
// It is the SAME arithmetic strategy.exitBasisWidened performs on the two
// prices it is handed. It exists here only to stamp the entry basis onto the
// position; the exit rule is never reimplemented.
func basisPct(spotQuote, perpQuote float64) float64 {
	return (perpQuote - spotQuote) / spotQuote * 100
}
