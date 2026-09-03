package exchanges

import (
	"fmt"
	"time"
)

// normalizeHyperliquidFunding normalizes one Hyperliquid activeAssetCtx
// reading.
//
// The docs compute an 8h-window rate but pay "every hour at one eighth of the
// computed rate", and the API's "funding" field is that HOURLY, already-÷8
// value — so it is used as-is with the interval the venue self-declares in
// predictedFundings (fundingIntervalHours, measured 1 on 2026-09-03). The
// caller reads that endpoint rather than assuming; annualizing this venue as
// 8h is wrong by 8×.
// https://hyperliquid.gitbook.io/hyperliquid-docs/trading/funding
// https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/info-endpoint/perpetuals
func normalizeHyperliquidFunding(symbol, source string, recvAt time.Time, venueTimeMs int64,
	rateFrac float64, fundingIntervalHours, nextFundingAtMs int64) (FundingData, error) {
	if fundingIntervalHours <= 0 {
		return FundingData{}, fmt.Errorf("hyperliquid funding %s: non-positive interval %dh", symbol, fundingIntervalHours)
	}
	return deriveFundingRates(FundingData{
		Symbol: symbol, Source: source, RecvAt: recvAt, VenueTimeMs: venueTimeMs,
		Model:               FundingDiscrete,
		RawRate:             rateFrac,
		RawRateField:        "funding",
		RatePerIntervalFrac: rateFrac,
		IntervalSec:         fundingIntervalHours * secPerHour,
		NextFundingAtMs:     nextFundingAtMs,
	})
}
