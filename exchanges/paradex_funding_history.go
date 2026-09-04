package exchanges

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

// Paradex funding index samples (step 2.6).
//
//	GET /v1/funding/data?market=&end_at=<ms>&page_size=1
//	{"next":"...","results":[{"market":"BTC-USD-PERP",
//	  "funding_index":"23784.07241073547335","funding_premium":"6.55509500088924211",
//	  "funding_rate":"0.00008100357318","funding_rate_8h":"0.000081",
//	  "funding_period_hours":8,"created_at":1788492012060,
//	  "impact_premium_rate":"-0.0004035339113141"}]}
//
// ⚠️ THIS VENUE HAS NO SETTLEMENTS TO BACKFILL. Funding V2 accrues continuously
// through a funding index, so "history" here is a sample of that index taken
// every FIVE SECONDS — measured 2026-09-04: 5,000 results per page spanning
// 6.94 hours, which is ~3.1 million rows per market for six months.
//
// Because the index is CUMULATIVE, an hourly sample loses nothing that matters:
// the accrual between any two sampled instants is exactly the difference of
// their indices. So this fetcher walks hour boundaries and takes the newest
// sample at or before each one, which turns 3.1 million rows into 4,380 and one
// tiny request per hour. `end_at` is what makes that possible — verified live:
// end_at=1788400000000 answered with created_at 1788399999353, newest first.
//
// Every entry it returns is FundingContinuous, and SettledAtMs is the sample
// instant, not a payment. Phase-3 code must branch on Model: counting these as
// settlements would credit 8,760 payments a year on a venue that makes none.
// https://docs.paradex.trade/api-reference/prod/funding/get-funding-data
const (
	// paradexFundingSampleEverySec is the sampling grid. One hour is the finest
	// cadence any venue in this scanner settles at, so a continuous venue
	// sampled at the same rate stays comparable with the discrete ones.
	paradexFundingSampleEverySec = secPerHour

	// paradexFundingHistoryPageSize is 1 on purpose: only the newest sample at
	// or before each boundary is kept, and asking for more would download
	// thousands of five-second rows to discard them.
	paradexFundingHistoryPageSize = 1
)

type paradexFundingHistoryResponse struct {
	Results []struct {
		Market string `json:"market"`
		// FundingRate is the rate for one funding PERIOD, and the venue states
		// that period in the same row — the only venue in this package that
		// publishes an interval alongside a historical rate.
		FundingRate        string `json:"funding_rate"`
		FundingRate8h      string `json:"funding_rate_8h"`
		FundingPeriodHours int64  `json:"funding_period_hours"`
		FundingIndex       string `json:"funding_index"`
		CreatedAt          int64  `json:"created_at"`
	} `json:"results"`
}

// FetchParadexFundingHistory samples the funding index once an hour across the
// window, newest boundary first.
//
// Its reach is bounded by maxFundingHistoryPages hours (~83 days) because one
// boundary costs one request. That is a deliberate trade against the ~3.1
// million five-second rows a native-resolution fetch would move; a caller that
// needs more asks for a longer window in more than one run.
func FetchParadexFundingHistory(ctx context.Context, source string, symbol Symbol, window FundingWindow) ([]FundingHistoryEntry, error) {
	gridMs := int64(paradexFundingSampleEverySec) * msPerSecond
	boundaryMs := (window.EndMs / gridMs) * gridMs

	var rows []fundingHistoryRow
	for page := 0; page < maxFundingHistoryPages && boundaryMs > window.StartMs; page++ {
		url := fmt.Sprintf("https://api.prod.paradex.trade/v1/funding/data?market=%s&end_at=%d&page_size=%d",
			symbol.Venue, boundaryMs, paradexFundingHistoryPageSize)
		var resp paradexFundingHistoryResponse
		if err := fetchFundingHistoryPage(ctx, fundingHistoryPageDelay, func() error {
			resp = paradexFundingHistoryResponse{}
			return fetchInstrumentJSON(ctx, url, &resp)
		}); err != nil {
			// A market this venue does not list is ABSENT, not an error:
			// step 2.4 found live that one unsupported pair (XLMUSDT on
			// Paradex) otherwise blanks a whole source. Here it would fill
			// the log with an hourly failure for a pair that will never
			// exist.
			if errors.Is(err, errInstrumentNotListed) {
				break
			}
			return nil, err
		}
		parsed, err := parseParadexFundingHistory(resp, symbol, window)
		if err != nil {
			return nil, err
		}
		rows = append(rows, parsed...)
		boundaryMs -= gridMs
		if boundaryMs <= window.StartMs {
			break
		}
		if err := fundingHistoryPause(ctx, fundingHistoryPageDelay); err != nil {
			return nil, err
		}
	}
	return finishFundingHistory(source, symbol, FundingContinuous, rows)
}

func parseParadexFundingHistory(resp paradexFundingHistoryResponse, symbol Symbol, window FundingWindow) ([]fundingHistoryRow, error) {
	out := make([]fundingHistoryRow, 0, len(resp.Results))
	for _, entry := range resp.Results {
		if entry.Market != symbol.Venue || !window.contains(entry.CreatedAt) {
			continue
		}
		rateFrac, err := strconv.ParseFloat(entry.FundingRate, 64)
		if err != nil {
			return nil, fmt.Errorf("paradex funding history %s: funding_rate %q does not parse",
				entry.Market, entry.FundingRate)
		}
		if entry.FundingPeriodHours <= 0 {
			// The venue declares the period per row; without it there is
			// nothing to normalize against, and the 5-second sample spacing
			// would be measured as the interval instead — a 5,760× error.
			return nil, fmt.Errorf("paradex funding history %s: funding_period_hours is %d",
				entry.Market, entry.FundingPeriodHours)
		}
		out = append(out, fundingHistoryRow{
			SettledAtMs:  entry.CreatedAt,
			RateFrac:     rateFrac,
			RawRate:      rateFrac,
			RawRateField: "funding_rate",
			IntervalSec:  entry.FundingPeriodHours * secPerHour,
		})
	}
	return out, nil
}
