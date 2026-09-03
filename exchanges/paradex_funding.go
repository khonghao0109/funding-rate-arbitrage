package exchanges

import (
	"fmt"
	"time"
)

// normalizeParadexFunding normalizes one Paradex funding-data reading.
//
// Funding V2 accrues continuously through a funding index — there is no
// settlement instant, so NextFundingAtMs stays 0 and counting-settlements
// logic must branch on Model. The rate itself is quoted for a self-declared
// window (funding_period_hours, 8 as measured 2026-09-03), so IntervalSec
// here means "the quoted window", not a settlement cadence.
// https://docs.paradex.trade/api-reference/prod/funding/list-funding-data
func normalizeParadexFunding(symbol, source string, recvAt time.Time, venueTimeMs int64,
	rateFrac float64, fundingPeriodHours int64) (FundingData, error) {
	if fundingPeriodHours <= 0 {
		return FundingData{}, fmt.Errorf("paradex funding %s: non-positive quote window %dh", symbol, fundingPeriodHours)
	}
	return deriveFundingRates(FundingData{
		Symbol: symbol, Source: source, RecvAt: recvAt, VenueTimeMs: venueTimeMs,
		Model:               FundingContinuous,
		RawRate:             rateFrac,
		RawRateField:        "funding_rate",
		RatePerIntervalFrac: rateFrac,
		IntervalSec:         fundingPeriodHours * secPerHour,
		NextFundingAtMs:     0,
	})
}
