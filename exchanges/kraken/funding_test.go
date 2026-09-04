package kraken

import (
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// Every number in this case is from the live WS frame probed 2026-09-03T08:36Z:
// relative_funding_rate matched the settled 08:00 hour of
// /v4/historicalfundingrates, and next_funding_rate_time was the ABSOLUTE stamp
// of the next round hour — not a remaining-ms countdown (DATA-REQUIREMENTS
// §3.3⑥). The per-1h rate is used AS-IS and ×8 for the 8h figure (§3.2②).
func TestFundingUnitConversion(t *testing.T) {
	recvAt := time.Date(2026, 9, 3, 8, 36, 0, 0, time.UTC)
	got, err := normalizeKrakenFunding(krakenFundingInput{
		FundingReading:  exchanges.FundingReading{Symbol: "BTCUSDT", Source: "kraken_futures", RecvAt: recvAt, VenueTimeMs: 1788424576257, RateFrac: 1.625386637931e-05},
		NextFundingAtMs: 1788426000000,
	})
	exchangestest.CheckNormalizedFunding(t, "kraken per-1h as-is", got, err, recvAt, exchanges.FundingData{
		Symbol: "BTCUSDT", Source: "kraken_futures", VenueTimeMs: 1788424576257,
		Model: exchanges.FundingDiscrete, RawRate: 1.625386637931e-05, RawRateField: "relative_funding_rate",
		RatePerIntervalFrac: 1.625386637931e-05, IntervalSec: 3600,
		RatePer8hFrac: 1.625386637931e-05 * 8, APRFrac: 1.625386637931e-05 * 8760,
		NextFundingAtMs: 1788426000000,
	})
}
