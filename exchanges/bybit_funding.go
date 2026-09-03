package exchanges

import (
	"fmt"
	"time"
)

// normalizeBybitFunding normalizes one Bybit v5 linear ticker reading.
//
// The interval arrives in MINUTES from instruments-info ("Funding interval
// (minute)" — 480 for 8h); the same venue's ticker quotes it in HOURS
// (fundingIntervalHour), which is exactly why the unit is converted here and
// nowhere else. Trap ③, verified live 2026-09-03.
// https://bybit-exchange.github.io/docs/v5/market/instrument
// https://bybit-exchange.github.io/docs/v5/market/tickers
//
// ⚠️ The v5 ticker is snapshot+delta: an absent field means UNCHANGED, not
// zero. The step-2.5 connector must merge into cached state and call this
// builder with the merged values — never with a delta's partial view.
func normalizeBybitFunding(symbol, source string, recvAt time.Time, venueTimeMs int64,
	rateFrac float64, fundingIntervalMin, nextFundingAtMs int64) (FundingData, error) {
	if fundingIntervalMin <= 0 {
		return FundingData{}, fmt.Errorf("bybit funding %s: non-positive interval %dmin", symbol, fundingIntervalMin)
	}
	return deriveFundingRates(FundingData{
		Symbol: symbol, Source: source, RecvAt: recvAt, VenueTimeMs: venueTimeMs,
		Model:               FundingDiscrete,
		RawRate:             rateFrac,
		RawRateField:        "fundingRate",
		RatePerIntervalFrac: rateFrac,
		IntervalSec:         fundingIntervalMin * 60,
		NextFundingAtMs:     nextFundingAtMs,
	})
}
