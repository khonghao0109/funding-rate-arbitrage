package exchanges

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

// Bybit settled funding rates (step 2.6).
//
//	GET /v5/market/funding/history?category=linear&symbol=&startTime=&endTime=&limit=200
//	{"retCode":0,"result":{"category":"linear","list":[
//	  {"symbol":"BTCUSDT","fundingRate":"0.00007352","fundingRateTimestamp":"1788480000000"}]}}
//
// Measured 2026-09-04: NEWEST FIRST, 200 rows per request, and a window a year
// back still answers. There is no cursor — the window itself is the cursor, so
// paging means moving endTime back to just before the oldest row received.
// https://bybit-exchange.github.io/docs/v5/market/history-fund-rate
const bybitFundingHistoryLimit = 200

type bybitFundingHistoryResponse struct {
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
	Result  struct {
		List []struct {
			Symbol string `json:"symbol"`
			// Both are strings here, unlike the ticker stream where the rate is
			// a string and the timestamp a number.
			FundingRate          string `json:"fundingRate"`
			FundingRateTimestamp string `json:"fundingRateTimestamp"`
		} `json:"list"`
	} `json:"result"`
}

// FetchBybitFundingHistory walks the window backwards from its end.
func FetchBybitFundingHistory(ctx context.Context, source string, symbol Symbol, window FundingWindow) ([]FundingHistoryEntry, error) {
	var rows []fundingHistoryRow
	endMs := window.EndMs

	for page := 0; page < maxFundingHistoryPages; page++ {
		url := fmt.Sprintf("https://api.bybit.com/v5/market/funding/history?category=linear&symbol=%s&startTime=%d&endTime=%d&limit=%d",
			symbol.Venue, window.StartMs, endMs, bybitFundingHistoryLimit)
		var resp bybitFundingHistoryResponse
		if err := fetchFundingHistoryPage(ctx, fundingHistoryPageDelay, func() error {
			resp = bybitFundingHistoryResponse{}
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
		parsed, oldestMs, err := parseBybitFundingHistory(resp, symbol, window)
		if err != nil {
			return nil, err
		}
		rows = append(rows, parsed...)
		if len(resp.Result.List) < bybitFundingHistoryLimit || oldestMs <= window.StartMs {
			break
		}
		if oldestMs-1 >= endMs {
			break // the venue is not moving; stop rather than re-request
		}
		endMs = oldestMs - 1
		if err := fundingHistoryPause(ctx, fundingHistoryPageDelay); err != nil {
			return nil, err
		}
	}
	return finishFundingHistory(source, symbol, FundingDiscrete, rows)
}

// parseBybitFundingHistory also reports the oldest stamp SEEN, which is what
// the next page is requested from — including rows the window rejected, so a
// page of entirely out-of-window rows still advances the cursor.
func parseBybitFundingHistory(resp bybitFundingHistoryResponse, symbol Symbol, window FundingWindow) ([]fundingHistoryRow, int64, error) {
	// A Bybit error arrives inside HTTP 200; treating the body as a success
	// would read an empty list as "no funding ever" (docs/CLAUDE.md trap row).
	// The one shape that really means "not listed here" is absent, not a
	// failure — the same rule the instrument fetcher applies, through the same
	// predicate so the prose match exists once.
	if bybitSaysSymbolNotListed(resp.RetCode, resp.RetMsg) {
		return nil, 0, nil
	}
	if resp.RetCode != 0 {
		return nil, 0, fmt.Errorf("bybit funding history %s: retCode %d %s", symbol.Venue, resp.RetCode, resp.RetMsg)
	}

	out := make([]fundingHistoryRow, 0, len(resp.Result.List))
	var oldestMs int64
	for _, entry := range resp.Result.List {
		stampMs, err := strconv.ParseInt(entry.FundingRateTimestamp, 10, 64)
		if err != nil {
			return nil, 0, fmt.Errorf("bybit funding history %s: fundingRateTimestamp %q does not parse",
				entry.Symbol, entry.FundingRateTimestamp)
		}
		if oldestMs == 0 || stampMs < oldestMs {
			oldestMs = stampMs
		}
		if entry.Symbol != symbol.Venue || !window.contains(stampMs) {
			continue
		}
		rateFrac, err := strconv.ParseFloat(entry.FundingRate, 64)
		if err != nil {
			return nil, 0, fmt.Errorf("bybit funding history %s: fundingRate %q does not parse",
				entry.Symbol, entry.FundingRate)
		}
		out = append(out, fundingHistoryRow{
			SettledAtMs:  stampMs,
			RateFrac:     rateFrac,
			RawRate:      rateFrac,
			RawRateField: "fundingRate",
		})
	}
	return out, oldestMs, nil
}
