package exchanges

import "time"

// normalizeKrakenFunding normalizes one Kraken Futures WS ticker reading.
//
// relative_funding_rate is a PER-1-HOUR rate applied in full at each hourly
// settlement — used as-is, ×8 only for the 8h comparison, never ÷8. The
// original survey had this backwards; correction and evidence in
// docs/DATA-REQUIREMENTS.md §3.2②. Contract spec: "the amount of funding an
// account will receive by maintaining a 1 contract unit short position for
// 1 hour".
// https://support.kraken.com/articles/4844359082772-linear-multi-collateral-derivatives-contract-specifications
//
// next_funding_rate_time is an ABSOLUTE epoch-ms timestamp. This is the
// survey's second Kraken correction: the old note said "milliseconds
// remaining", quoting the WS doc's field description — but the doc's own
// sample value and the live feed are absolute (probed twice, independently,
// on 2026-09-03: next_funding_rate_time=1788426000000 = 09:00:00Z, the next
// round hour, ~24min ahead of the probe; DATA-REQUIREMENTS §3.3⑥). Adding
// "now" to it would stamp settlements around year 2083, and phase-3
// settlement counting would conclude Kraken never settles.
// https://docs.kraken.com/api/docs/futures-api/websocket/ticker/
//
// The hourly cadence is pinned here as a venue semantic — no Kraken funding
// message carries an interval field, so there is nothing to read it from.
// That sits close to CLAUDE.md rule 3 by design: the rule forbids assuming
// one interval across venues, and this constant is this venue's documented,
// measured cadence (spacing of historicalfundingrates entries, 2026-09-03),
// re-measured by the step-2.6 history job on every backfill. A cadence change
// between backfills would go unseen — the 5× coherence check cannot catch a
// 2× shift — which is accepted and recorded in the PLAN.
func normalizeKrakenFunding(symbol, source string, recvAt time.Time, venueTimeMs int64,
	relativeRatePerHourFrac float64, nextFundingAtMs int64) (FundingData, error) {
	return deriveFundingRates(FundingData{
		Symbol: symbol, Source: source, RecvAt: recvAt, VenueTimeMs: venueTimeMs,
		Model:               FundingDiscrete,
		RawRate:             relativeRatePerHourFrac,
		RawRateField:        "relative_funding_rate",
		RatePerIntervalFrac: relativeRatePerHourFrac,
		IntervalSec:         secPerHour,
		NextFundingAtMs:     nextFundingAtMs,
	})
}
