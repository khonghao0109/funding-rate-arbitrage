package binance

import "futures-arbitrage-scanner/exchanges"

import "fmt"

// binanceFundingInput is one Binance USDⓈ-M reading in Binance's own units.
//
// The rate and the settlement stamp come from REST premiumIndex
// (lastFundingRate, nextFundingTime): the documented WS mark-price stream is
// NOT the source, because it delivers nothing to this environment — measured
// 2026-09-04, a socket that accepted SUBSCRIBE for btcusdt@markPrice@1s and
// btcusdt@bookTicker together carried 4,782 bookTicker frames and ZERO
// markPriceUpdate frames in 45 seconds. See docs/DATA-REQUIREMENTS.md §3.4.
//
// IntervalHours comes from fundingInfo, which documents itself as returning
// only symbols whose config differs from the default — so 8 is the default
// and this is the override, never the other way round.
// https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Mark-Price
// https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Get-Funding-Rate-Info
type binanceFundingInput struct {
	exchanges.FundingReading
	IntervalHours   int64
	NextFundingAtMs int64
}

func normalizeBinanceFunding(in binanceFundingInput) (exchanges.FundingData, error) {
	if in.IntervalHours <= 0 {
		return exchanges.FundingData{}, fmt.Errorf("binance funding %s: non-positive interval %dh", in.Symbol, in.IntervalHours)
	}
	return exchanges.DeriveFundingRates(exchanges.FundingData{
		Symbol: in.Symbol, Source: in.Source, RecvAt: in.RecvAt, VenueTimeMs: in.VenueTimeMs,
		Model:               exchanges.FundingDiscrete,
		RawRate:             in.RateFrac,
		RawRateField:        "lastFundingRate",
		RatePerIntervalFrac: in.RateFrac,
		IntervalSec:         in.IntervalHours * exchanges.SecPerHour,
		NextFundingAtMs:     in.NextFundingAtMs,
	})
}
