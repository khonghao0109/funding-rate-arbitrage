package exchanges

import (
	"fmt"
	"time"
)

// normalizeGateFunding normalizes one Gate futures ticker/contract reading.
//
// Gate is the one venue that already quotes funding_interval in SECONDS
// (28800), and funding_next_apply in epoch SECONDS — the only conversion is
// seconds→ms for the timestamp. Trap ③, verified live 2026-09-03.
// https://www.gate.com/docs/developers/apiv4/en/#get-a-single-contract
func normalizeGateFunding(symbol, source string, recvAt time.Time, venueTimeMs int64,
	rateFrac float64, fundingIntervalSec, fundingNextApplySec int64) (FundingData, error) {
	if fundingIntervalSec <= 0 {
		return FundingData{}, fmt.Errorf("gate funding %s: non-positive interval %ds", symbol, fundingIntervalSec)
	}
	return deriveFundingRates(FundingData{
		Symbol: symbol, Source: source, RecvAt: recvAt, VenueTimeMs: venueTimeMs,
		Model:               FundingDiscrete,
		RawRate:             rateFrac,
		RawRateField:        "funding_rate",
		RatePerIntervalFrac: rateFrac,
		IntervalSec:         fundingIntervalSec,
		NextFundingAtMs:     fundingNextApplySec * msPerSecond,
	})
}
