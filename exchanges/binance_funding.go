package exchanges

import (
	"fmt"
	"time"
)

// normalizeBinanceFunding normalizes one Binance USDⓈ-M reading.
//
// The WS markPrice stream carries the rate in "r" and the next settlement in
// "T" (ms); the interval comes from fundingInfo in HOURS, defaulting to 8 for
// symbols the endpoint omits — it documents itself as returning only adjusted
// symbols, even though on 2026-09-03 it happened to cover every trading
// perpetual, at 4h (443), 8h (331) and 1h (3).
// https://developers.binance.com/docs/derivatives/usds-margined-futures/websocket-market-streams/Mark-Price-Stream
// https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Get-Funding-Rate-Info
func normalizeBinanceFunding(symbol, source string, recvAt time.Time, venueTimeMs int64,
	rateFrac float64, fundingIntervalHours, nextFundingAtMs int64) (FundingData, error) {
	if fundingIntervalHours <= 0 {
		return FundingData{}, fmt.Errorf("binance funding %s: non-positive interval %dh", symbol, fundingIntervalHours)
	}
	return deriveFundingRates(FundingData{
		Symbol: symbol, Source: source, RecvAt: recvAt, VenueTimeMs: venueTimeMs,
		Model:               FundingDiscrete,
		RawRate:             rateFrac,
		RawRateField:        "r",
		RatePerIntervalFrac: rateFrac,
		IntervalSec:         fundingIntervalHours * secPerHour,
		NextFundingAtMs:     nextFundingAtMs,
	})
}
