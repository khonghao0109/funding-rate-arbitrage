package paradex

import (
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// Continuous accrual: no settlement timestamp exists, and IntervalSec carries
// the rate's QUOTE window (8h), not a cadence.
func TestFundingUnitConversion(t *testing.T) {
	recvAt := time.Date(2026, 9, 3, 8, 36, 0, 0, time.UTC)
	got, err := normalizeParadexFunding(paradexFundingInput{
		FundingReading: exchanges.FundingReading{Symbol: "BTCUSDT", Source: "paradex_futures", RecvAt: recvAt, VenueTimeMs: 1788421452060, RateFrac: 0.00009228931609},
		PeriodHours:    8,
	})
	exchangestest.CheckNormalizedFunding(t, "paradex continuous", got, err, recvAt, exchanges.FundingData{
		Symbol: "BTCUSDT", Source: "paradex_futures", VenueTimeMs: 1788421452060,
		Model: exchanges.FundingContinuous, RawRate: 0.00009228931609, RawRateField: "funding_rate",
		RatePerIntervalFrac: 0.00009228931609, IntervalSec: 28800,
		RatePer8hFrac: 0.00009228931609, APRFrac: 0.00009228931609 * 1095,
		NextFundingAtMs: 0,
	})
}

func TestFundingBuilder_RejectsNonPositiveIntervals(t *testing.T) {
	if _, err := normalizeParadexFunding(paradexFundingInput{
		FundingReading: exchanges.FundingReading{Symbol: "X", Source: "s", RecvAt: time.Now(), RateFrac: 0.0001},
		PeriodHours:    0,
	}); err == nil {
		t.Error("zero window: want error, got nil")
	}
}
