package exchanges

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

// Binance settled funding rates (step 2.6).
//
//	GET /fapi/v1/fundingRate?symbol=&startTime=&endTime=&limit=1000
//	[{"symbol":"BTCUSDT","fundingTime":1788422400000,"fundingRate":"0.00005866",
//	  "markPrice":"77622.37865217","rateType":"Regular"}]
//
// Measured 2026-09-04: ascending, 1000 rows per request, and a startTime a full
// year back still answers with data. rateType is present HERE and nowhere else
// in this codebase — it is the field PLAN.md 3.3 requires a backtest to filter
// "Special" on, and this is the only endpoint that publishes it.
// https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Get-Funding-Rate-History
const binanceFundingHistoryLimit = 1000

type binanceFundingHistoryRow struct {
	Symbol      string `json:"symbol"`
	FundingTime int64  `json:"fundingTime"`
	FundingRate string `json:"fundingRate"`
	MarkPrice   string `json:"markPrice"`
	RateType    string `json:"rateType"`
}

// FetchBinanceFundingHistory walks the window forward, one page at a time.
func FetchBinanceFundingHistory(ctx context.Context, source string, symbol Symbol, window FundingWindow) ([]FundingHistoryEntry, error) {
	var rows []fundingHistoryRow
	cursorMs := window.StartMs

	for page := 0; page < maxFundingHistoryPages; page++ {
		url := fmt.Sprintf("https://fapi.binance.com/fapi/v1/fundingRate?symbol=%s&startTime=%d&endTime=%d&limit=%d",
			symbol.Venue, cursorMs, window.EndMs, binanceFundingHistoryLimit)
		var raw []binanceFundingHistoryRow
		if err := fetchFundingHistoryPage(ctx, fundingHistoryPageDelay, func() error {
			raw = nil
			return fetchInstrumentJSON(ctx, url, &raw)
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
		parsed, err := parseBinanceFundingHistory(raw, symbol, window)
		if err != nil {
			return nil, err
		}
		rows = append(rows, parsed...)
		if len(raw) < binanceFundingHistoryLimit {
			break // the venue had nothing more to give inside the window
		}
		// Advance past the last row RECEIVED, not the last row kept: a page
		// entirely outside the window would otherwise leave the cursor where it
		// was and the loop would re-request it until the page budget ran out.
		next := raw[len(raw)-1].FundingTime + 1
		if next <= cursorMs {
			break
		}
		cursorMs = next
		if err := fundingHistoryPause(ctx, fundingHistoryPageDelay); err != nil {
			return nil, err
		}
	}
	return finishFundingHistory(source, symbol, FundingDiscrete, rows)
}

func parseBinanceFundingHistory(raw []binanceFundingHistoryRow, symbol Symbol, window FundingWindow) ([]fundingHistoryRow, error) {
	out := make([]fundingHistoryRow, 0, len(raw))
	for _, entry := range raw {
		if entry.Symbol != symbol.Venue || !window.contains(entry.FundingTime) {
			continue
		}
		rateFrac, err := strconv.ParseFloat(entry.FundingRate, 64)
		if err != nil {
			return nil, fmt.Errorf("binance funding history %s: fundingRate %q does not parse", entry.Symbol, entry.FundingRate)
		}
		markPrice, _ := strconv.ParseFloat(entry.MarkPrice, 64)
		out = append(out, fundingHistoryRow{
			SettledAtMs:    entry.FundingTime,
			RateFrac:       rateFrac,
			RawRate:        rateFrac,
			RawRateField:   "fundingRate",
			RateType:       entry.RateType,
			MarkPriceQuote: markPrice,
		})
	}
	return out, nil
}
