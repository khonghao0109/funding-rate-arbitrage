package exchanges

import "fmt"

// gateFundingInput is one Gate futures.tickers reading.
//
// Gate is the one venue that already quotes funding_interval in SECONDS
// (28800) and funding_next_apply in epoch SECONDS — both confirmed in the WS
// ticker payload, measured 2026-09-04. The only conversion is seconds→ms for
// the timestamp.
// https://www.gate.com/docs/developers/futures/ws/en/#tickers-api
type gateFundingInput struct {
	fundingReading
	IntervalSec         int64
	FundingNextApplySec int64
}

func normalizeGateFunding(in gateFundingInput) (FundingData, error) {
	if in.IntervalSec <= 0 {
		return FundingData{}, fmt.Errorf("gate funding %s: non-positive interval %ds", in.Symbol, in.IntervalSec)
	}
	return deriveFundingRates(FundingData{
		Symbol: in.Symbol, Source: in.Source, RecvAt: in.RecvAt, VenueTimeMs: in.VenueTimeMs,
		Model:               FundingDiscrete,
		RawRate:             in.RateFrac,
		RawRateField:        "funding_rate",
		RatePerIntervalFrac: in.RateFrac,
		IntervalSec:         in.IntervalSec,
		NextFundingAtMs:     in.FundingNextApplySec * msPerSecond,
	})
}
