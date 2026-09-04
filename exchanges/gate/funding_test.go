package gate

import (
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// Gate's interval is already SECONDS and funding_next_apply is epoch seconds
// (trap ③: three venues, three units for the same concept).
func TestFundingUnitConversion(t *testing.T) {
	recvAt := time.Date(2026, 9, 3, 8, 36, 0, 0, time.UTC)
	got, err := normalizeGateFunding(gateFundingInput{
		FundingReading: exchanges.FundingReading{Symbol: "BTCUSDT", Source: "gate_futures", RecvAt: recvAt, RateFrac: 0.000038},
		IntervalSec:    28800, FundingNextApplySec: 1788451200,
	})
	exchangestest.CheckNormalizedFunding(t, "gate seconds", got, err, recvAt, exchanges.FundingData{
		Symbol: "BTCUSDT", Source: "gate_futures",
		Model: exchanges.FundingDiscrete, RawRate: 0.000038, RawRateField: "funding_rate",
		RatePerIntervalFrac: 0.000038, IntervalSec: 28800,
		RatePer8hFrac: 0.000038, APRFrac: 0.000038 * 1095,
		NextFundingAtMs: 1788451200000,
	})
}

func TestFundingBuilder_RejectsNonPositiveIntervals(t *testing.T) {
	if _, err := normalizeGateFunding(gateFundingInput{
		FundingReading: exchanges.FundingReading{Symbol: "X", Source: "s", RecvAt: time.Now(), RateFrac: 0.0001},
		IntervalSec:    0, FundingNextApplySec: 1,
	}); err == nil {
		t.Error("zero interval: want error, got nil")
	}
}
