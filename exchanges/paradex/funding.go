package paradex

import "futures-arbitrage-scanner/exchanges"

import "fmt"

// paradexFundingInput is one Paradex funding_data reading.
//
// Funding V2 accrues continuously through a funding index — there is no
// settlement instant, so NextFundingAtMs stays 0 and settlement-counting
// logic must branch on Model. The rate is quoted for a self-declared window
// (funding_period_hours: 8, measured 2026-09-04 on the funding_data channel,
// where funding_rate 0.00007172918481 sits beside funding_rate_8h
// 0.00007172 — confirming the rate IS the 8h-window figure).
// https://docs.paradex.trade/ws/channels/funding-data
type paradexFundingInput struct {
	exchanges.FundingReading
	PeriodHours int64
}

func normalizeParadexFunding(in paradexFundingInput) (exchanges.FundingData, error) {
	if in.PeriodHours <= 0 {
		return exchanges.FundingData{}, fmt.Errorf("paradex funding %s: non-positive quote window %dh", in.Symbol, in.PeriodHours)
	}
	return exchanges.DeriveFundingRates(exchanges.FundingData{
		Symbol: in.Symbol, Source: in.Source, RecvAt: in.RecvAt, VenueTimeMs: in.VenueTimeMs,
		Model:               exchanges.FundingContinuous,
		RawRate:             in.RateFrac,
		RawRateField:        "funding_rate",
		RatePerIntervalFrac: in.RateFrac,
		IntervalSec:         in.PeriodHours * exchanges.SecPerHour,
		NextFundingAtMs:     0,
	})
}
