package okx

import "futures-arbitrage-scanner/exchanges"

import "fmt"

// okxFundingInput is one OKX funding-rate reading.
//
// Trap ①, verified live 2026-09-03 and again 2026-09-04: OKX's "fundingTime"
// is the UPCOMING settlement and "nextFundingTime" the one AFTER it — mapping
// nextFundingTime like Binance's "T" is off by one full period. The interval
// is DERIVED from the two timestamps because OKX publishes no interval field;
// never pass a constant instead of the real stamps.
//
// nextFundingRate arrives EMPTY under method=current_period (measured both
// days): the connector leaves HasFollowingRate false rather than parsing ""
// as 0.
// https://www.okx.com/docs-v5/en/#public-data-websocket-funding-rate-channel
type okxFundingInput struct {
	exchanges.FundingReading
	FundingAtMs     int64 // "fundingTime" — the UPCOMING settlement
	NextFundingAtMs int64 // "nextFundingTime" — the one after it
}

func normalizeOKXFunding(in okxFundingInput) (exchanges.FundingData, error) {
	if in.NextFundingAtMs <= in.FundingAtMs {
		return exchanges.FundingData{}, fmt.Errorf("okx funding %s: nextFundingTime %d not after fundingTime %d",
			in.Symbol, in.NextFundingAtMs, in.FundingAtMs)
	}
	return exchanges.DeriveFundingRates(exchanges.FundingData{
		Symbol: in.Symbol, Source: in.Source, RecvAt: in.RecvAt, VenueTimeMs: in.VenueTimeMs,
		Model:               exchanges.FundingDiscrete,
		RawRate:             in.RateFrac,
		RawRateField:        "fundingRate",
		RatePerIntervalFrac: in.RateFrac,
		IntervalSec:         (in.NextFundingAtMs - in.FundingAtMs) / exchanges.MsPerSecond,
		NextFundingAtMs:     in.FundingAtMs, // fundingTime, NOT nextFundingTime
	})
}
