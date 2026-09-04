package kraken

import "futures-arbitrage-scanner/exchanges"

// krakenFundingInput is one Kraken Futures WS ticker reading.
//
// RateFrac must be relative_funding_rate, a PER-1-HOUR rate applied in full
// at each hourly settlement — used as-is, ×8 only for the 8h comparison,
// never ÷8. The original survey had this backwards; correction and evidence
// in docs/DATA-REQUIREMENTS.md §3.2②. The venue's own funding_rate field is
// an absolute price amount (measured 2026-09-04: -1.198 next to a relative
// -0.0000148) and must never reach this builder.
// https://support.kraken.com/articles/4844359082772-linear-multi-collateral-derivatives-contract-specifications
//
// NextFundingAtMs is next_funding_rate_time, an ABSOLUTE epoch-ms timestamp.
// This is the survey's second Kraken correction: the old note said
// "milliseconds remaining", quoting the WS doc's prose — but the doc's own
// sample and the live feed are absolute (probed three times independently,
// most recently 2026-09-04: 1788490800000 = the next round hour).
// https://docs.kraken.com/api/docs/futures-api/websocket/ticker/
//
// The hourly cadence is pinned as a venue semantic — no Kraken funding
// message carries an interval field, so there is nothing to read it from.
// That sits close to CLAUDE.md rule 3 by design: the rule forbids assuming
// one interval ACROSS venues, and this constant is this venue's documented,
// measured cadence, re-measured by the step-2.6 history job on every backfill.
type krakenFundingInput struct {
	exchanges.FundingReading
	NextFundingAtMs int64
}

func normalizeKrakenFunding(in krakenFundingInput) (exchanges.FundingData, error) {
	return exchanges.DeriveFundingRates(exchanges.FundingData{
		Symbol: in.Symbol, Source: in.Source, RecvAt: in.RecvAt, VenueTimeMs: in.VenueTimeMs,
		Model:               exchanges.FundingDiscrete,
		RawRate:             in.RateFrac,
		RawRateField:        "relative_funding_rate",
		RatePerIntervalFrac: in.RateFrac,
		IntervalSec:         exchanges.SecPerHour,
		NextFundingAtMs:     in.NextFundingAtMs,
	})
}
