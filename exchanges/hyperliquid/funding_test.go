package hyperliquid

import (
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// The venue's `funding` field is the per-1h rate ALREADY ÷8 by the venue
// (DATA-REQUIREMENTS §3.3⑤) — used as-is with IntervalSec 3600, ×8 for the 8h
// window. Annualizing this venue as 8h is wrong by 8×.
func TestFundingUnitConversion(t *testing.T) {
	recvAt := time.Date(2026, 9, 3, 8, 36, 0, 0, time.UTC)
	got, err := normalizeHyperliquidFunding(hyperliquidFundingInput{
		FundingReading: exchanges.FundingReading{Symbol: "BTCUSDT", Source: "hyperliquid_futures", RecvAt: recvAt, RateFrac: 0.0000125},
		IntervalHours:  1, NextFundingAtMs: 1788422400000,
	})
	exchangestest.CheckNormalizedFunding(t, "hyperliquid hourly", got, err, recvAt, exchanges.FundingData{
		Symbol: "BTCUSDT", Source: "hyperliquid_futures",
		Model: exchanges.FundingDiscrete, RawRate: 0.0000125, RawRateField: "funding",
		RatePerIntervalFrac: 0.0000125, IntervalSec: 3600,
		RatePer8hFrac: 0.0001, APRFrac: 0.0000125 * 8760,
		NextFundingAtMs: 1788422400000,
	})
}

func TestFundingBuilder_RejectsNonPositiveIntervals(t *testing.T) {
	if _, err := normalizeHyperliquidFunding(hyperliquidFundingInput{
		FundingReading: exchanges.FundingReading{Symbol: "X", Source: "s", RecvAt: time.Now(), RateFrac: 0.0001},
		IntervalHours:  0, NextFundingAtMs: 1,
	}); err == nil {
		t.Error("zero interval: want error, got nil")
	}
}
