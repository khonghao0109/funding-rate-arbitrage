package bybit

import (
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// Bybit quotes HOURS on the WS ticker while the SAME venue quotes MINUTES on
// REST instruments-info (trap ③) — the builder takes the WS hours.
func TestFundingUnitConversion(t *testing.T) {
	recvAt := time.Date(2026, 9, 3, 8, 36, 0, 0, time.UTC)
	got, err := normalizeBybitFunding(bybitFundingInput{
		FundingReading: exchanges.FundingReading{Symbol: "BTCUSDT", Source: "bybit_futures", RecvAt: recvAt, RateFrac: 0.0001},
		IntervalHours:  8, NextFundingAtMs: 1788451200000,
	})
	exchangestest.CheckNormalizedFunding(t, "bybit hours", got, err, recvAt, exchanges.FundingData{
		Symbol: "BTCUSDT", Source: "bybit_futures",
		Model: exchanges.FundingDiscrete, RawRate: 0.0001, RawRateField: "fundingRate",
		RatePerIntervalFrac: 0.0001, IntervalSec: 28800,
		RatePer8hFrac: 0.0001, APRFrac: 0.0001 * 1095,
		NextFundingAtMs: 1788451200000,
	})
}

func TestFundingBuilder_RejectsNonPositiveIntervals(t *testing.T) {
	if _, err := normalizeBybitFunding(bybitFundingInput{
		FundingReading: exchanges.FundingReading{Symbol: "X", Source: "s", RecvAt: time.Now(), RateFrac: 0.0001},
		IntervalHours:  0, NextFundingAtMs: 1,
	}); err == nil {
		t.Error("zero interval: want error, got nil")
	}
}
