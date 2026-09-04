package hyperliquid

import "futures-arbitrage-scanner/exchanges"

import "fmt"

// hyperliquidFundingInput is one Hyperliquid activeAssetCtx reading.
//
// The docs compute an 8h-window rate but pay "every hour at one eighth of the
// computed rate", and the API's "funding" field is that HOURLY, already-÷8
// value — used as-is with the interval the venue SELF-DECLARES in
// predictedFundings (fundingIntervalHours: 1 for HlPerp, measured 2026-09-04).
// Annualizing this venue as 8h is wrong by 8×.
//
// The WS ctx carries neither the interval nor a settlement stamp, so both come
// from that REST endpoint, which publishes nextFundingTime alongside the
// interval. A stamp already in the past is dropped by the connector rather
// than published — a countdown to a moment that has gone is worse than none.
// https://hyperliquid.gitbook.io/hyperliquid-docs/trading/funding
// https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/info-endpoint/perpetuals
type hyperliquidFundingInput struct {
	exchanges.FundingReading
	IntervalHours   int64
	NextFundingAtMs int64
}

func normalizeHyperliquidFunding(in hyperliquidFundingInput) (exchanges.FundingData, error) {
	if in.IntervalHours <= 0 {
		return exchanges.FundingData{}, fmt.Errorf("hyperliquid funding %s: non-positive interval %dh", in.Symbol, in.IntervalHours)
	}
	return exchanges.DeriveFundingRates(exchanges.FundingData{
		Symbol: in.Symbol, Source: in.Source, RecvAt: in.RecvAt, VenueTimeMs: in.VenueTimeMs,
		Model:               exchanges.FundingDiscrete,
		RawRate:             in.RateFrac,
		RawRateField:        "funding",
		RatePerIntervalFrac: in.RateFrac,
		IntervalSec:         in.IntervalHours * exchanges.SecPerHour,
		NextFundingAtMs:     in.NextFundingAtMs,
	})
}
