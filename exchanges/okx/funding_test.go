package okx

import (
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// fundingTime IS the next settlement (trap ①) and the interval is derived from
// the two stamps because the venue publishes none.
func TestFundingUnitConversion(t *testing.T) {
	recvAt := time.Date(2026, 9, 3, 8, 36, 0, 0, time.UTC)
	got, err := normalizeOKXFunding(okxFundingInput{
		FundingReading: exchanges.FundingReading{Symbol: "BTCUSDT", Source: "okx_futures", RecvAt: recvAt, VenueTimeMs: 1788424561000, RateFrac: 0.0000475156961351},
		FundingAtMs:    1788451200000, NextFundingAtMs: 1788480000000,
	})
	exchangestest.CheckNormalizedFunding(t, "okx derived interval", got, err, recvAt, exchanges.FundingData{
		Symbol: "BTCUSDT", Source: "okx_futures", VenueTimeMs: 1788424561000,
		Model: exchanges.FundingDiscrete, RawRate: 0.0000475156961351, RawRateField: "fundingRate",
		RatePerIntervalFrac: 0.0000475156961351, IntervalSec: 28800,
		RatePer8hFrac: 0.0000475156961351, APRFrac: 0.0000475156961351 * 1095,
		// fundingTime, never nextFundingTime — mapping the latter is off by
		// one full period.
		NextFundingAtMs: 1788451200000,
	})
}

// A non-positive derived interval cannot be normalized: same stamps, reversed
// stamps, and sub-second spacing that truncates to zero must all refuse.
func TestFundingBuilder_RejectsNonPositiveIntervals(t *testing.T) {
	at := time.Now()
	for _, c := range []struct {
		name                 string
		fundingMs, nextendMs int64
	}{
		{"next not after funding time", 5, 5},
		{"next before funding time", 5, 4},
		{"sub-second spacing truncates to zero", 5, 900},
	} {
		if _, err := normalizeOKXFunding(okxFundingInput{
			FundingReading: exchanges.FundingReading{Symbol: "X", Source: "s", RecvAt: at, RateFrac: 0.0001},
			FundingAtMs:    c.fundingMs, NextFundingAtMs: c.nextendMs,
		}); err == nil {
			t.Errorf("%s: want error, got nil", c.name)
		}
	}
}
