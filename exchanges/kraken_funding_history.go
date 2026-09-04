package exchanges

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Kraken settled funding rates (step 2.6).
//
//	GET /derivatives/api/v4/historicalfundingrates?symbol=PF_XBTUSD
//	{"result":"success","serverTime":"2026-09-04T03:20:13.849Z","rates":[
//	  {"timestamp":"2025-09-03T08:00:00Z","fundingRate":1.810655889040739200,
//	   "relativeFundingRate":0.000016297334722222}]}
//
// This venue has no pagination and needs none: measured 2026-09-04, one request
// returned 8,772 rows covering 2025-09-03 to 2026-09-04 in 1,010,412 bytes —
// a full year, oldest first, in a single response. The window is applied after
// the fetch because there is nothing to ask the venue for.
//
// ⚠️ `fundingRate` is an absolute PRICE amount, not a rate: 1.8107 next to a
// relative 0.0000163, the ratio being the index price. `relativeFundingRate` is
// the rate, and it is per 1 HOUR, applied in full at each hourly settlement —
// see kraken_funding.go and docs/DATA-REQUIREMENTS.md §3.2②. Reading the wrong
// field here would put price-sized numbers into a rate column.
//
// The gap measurement over this response is what re-verifies the hourly cadence
// that kraken_funding.go pins as a constant (the standing obligation from step
// 2.3): 8,764 of the 8,771 gaps were 3600s, six were 7200s and one 10800s —
// hourly, with seven missed settlements in a year.
// https://docs.kraken.com/api/docs/futures-api/trading/historical-funding-rates
type krakenFundingHistoryResponse struct {
	Result string `json:"result"`
	Error  string `json:"error"`
	Rates  []struct {
		Timestamp string `json:"timestamp"`
		// Absolute price amount — present but never used as a rate.
		FundingRate float64 `json:"fundingRate"`
		// A POINTER so an absent field is distinguishable from a rate of
		// exactly zero, which is a real value on a quiet hour.
		RelativeFundingRate *float64 `json:"relativeFundingRate"`
	} `json:"rates"`
}

// FetchKrakenFundingHistory reads the whole published history in one request
// and keeps the rows inside the window.
func FetchKrakenFundingHistory(ctx context.Context, source string, symbol Symbol, window FundingWindow) ([]FundingHistoryEntry, error) {
	var resp krakenFundingHistoryResponse
	url := "https://futures.kraken.com/derivatives/api/v4/historicalfundingrates?symbol=" + symbol.Venue
	if err := fetchFundingHistoryPage(ctx, func() error {
		resp = krakenFundingHistoryResponse{}
		return fetchInstrumentJSON(ctx, url, &resp)
	}); err != nil {
		// A market Kraken does not list has no history, which is a fact about
		// the pair rather than a failure of the fetch (step 2.4).
		if errors.Is(err, errInstrumentNotListed) {
			return nil, nil
		}
		return nil, err
	}
	rows, err := parseKrakenFundingHistory(resp, symbol, window)
	if err != nil {
		return nil, err
	}
	return finishFundingHistory(source, symbol, FundingDiscrete, rows)
}

func parseKrakenFundingHistory(resp krakenFundingHistoryResponse, symbol Symbol, window FundingWindow) ([]fundingHistoryRow, error) {
	if resp.Result != "success" {
		return nil, fmt.Errorf("kraken funding history %s: result %q %s", symbol.Venue, resp.Result, resp.Error)
	}

	out := make([]fundingHistoryRow, 0, len(resp.Rates))
	for _, entry := range resp.Rates {
		stamp, err := time.Parse(time.RFC3339, entry.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("kraken funding history %s: timestamp %q does not parse", symbol.Venue, entry.Timestamp)
		}
		stampMs := stamp.UnixMilli()
		if !window.contains(stampMs) {
			continue
		}
		if entry.RelativeFundingRate == nil {
			// Refusing beats substituting fundingRate: that field is an
			// absolute price amount and would land in the rate column looking
			// like a 180,000% funding rate.
			return nil, fmt.Errorf("kraken funding history %s: row %s has no relativeFundingRate",
				symbol.Venue, entry.Timestamp)
		}
		out = append(out, fundingHistoryRow{
			SettledAtMs:  stampMs,
			RateFrac:     *entry.RelativeFundingRate,
			RawRate:      *entry.RelativeFundingRate,
			RawRateField: "relativeFundingRate",
		})
	}
	return out, nil
}
