package exchanges

import (
	"fmt"
	"time"
)

// normalizeOKXFunding normalizes one OKX funding-rate reading.
//
// Trap ①, verified live 2026-09-03: OKX's "fundingTime" is the UPCOMING
// settlement and "nextFundingTime" the one AFTER it — mapping nextFundingTime
// like Binance's "T" is off by one full period. The interval is derived from
// the two timestamps because OKX publishes no interval field; never pass a
// constant instead of the real timestamps.
//
// nextFundingRate arrives EMPTY under method=current_period (measured
// 2026-09-03): the connector must leave HasFollowingRate false rather than
// parse "" as 0.
// https://www.okx.com/docs-v5/en/#public-data-websocket-funding-rate-channel
func normalizeOKXFunding(symbol, source string, recvAt time.Time, venueTimeMs int64,
	rateFrac float64, fundingAtMs, nextFundingAtMs int64) (FundingData, error) {
	if nextFundingAtMs <= fundingAtMs {
		return FundingData{}, fmt.Errorf("okx funding %s: nextFundingTime %d not after fundingTime %d", symbol, nextFundingAtMs, fundingAtMs)
	}
	return deriveFundingRates(FundingData{
		Symbol: symbol, Source: source, RecvAt: recvAt, VenueTimeMs: venueTimeMs,
		Model:               FundingDiscrete,
		RawRate:             rateFrac,
		RawRateField:        "fundingRate",
		RatePerIntervalFrac: rateFrac,
		IntervalSec:         (nextFundingAtMs - fundingAtMs) / msPerSecond,
		NextFundingAtMs:     fundingAtMs, // fundingTime, NOT nextFundingTime
	})
}
