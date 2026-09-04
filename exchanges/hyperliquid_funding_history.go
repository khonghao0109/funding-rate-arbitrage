package exchanges

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

// Hyperliquid settled funding rates (step 2.6).
//
//	POST /info  {"type":"fundingHistory","coin":"BTC","startTime":...,"endTime":...}
//	[{"coin":"BTC","fundingRate":"0.0000125","premium":"-0.0003591749",
//	  "time":1788480000030}]
//
// Measured 2026-09-04: oldest first, 500 rows per response, and a startTime a
// year back still answers. Unlike predictedFundings this endpoint is already
// scoped to Hyperliquid's own book, so there is no competitor row to filter out.
//
// The stamps carry tens of milliseconds of jitter — 1788480000030 for an hourly
// settlement, 1788483600062 for the next — and are stored exactly as sent. That
// is what roundedGapSec exists for: truncating those two gaps gives 3600 and
// 3599, and a cadence measured in two values is not a cadence.
//
// This venue settles HOURLY. Nothing here asserts that; the spacing of the rows
// says it, which is the point of measuring the cadence rather than declaring it.
// https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/info-endpoint/perpetuals
const hyperliquidFundingHistoryLimit = 500

type hyperliquidFundingHistoryRow struct {
	Coin        string `json:"coin"`
	FundingRate string `json:"fundingRate"`
	Premium     string `json:"premium"`
	Time        int64  `json:"time"`
}

// FetchHyperliquidFundingHistory walks the window forward.
func FetchHyperliquidFundingHistory(ctx context.Context, source string, symbol Symbol, window FundingWindow) ([]FundingHistoryEntry, error) {
	var rows []fundingHistoryRow
	cursorMs := window.StartMs

	for page := 0; page < maxFundingHistoryPages; page++ {
		payload := fmt.Sprintf(`{"type":"fundingHistory","coin":%q,"startTime":%d,"endTime":%d}`,
			symbol.Venue, cursorMs, window.EndMs)
		var raw []hyperliquidFundingHistoryRow
		if err := fetchFundingHistoryPage(ctx, func() error {
			raw = nil
			return postInstrumentJSON(ctx, "https://api.hyperliquid.xyz/info", payload, &raw)
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
		parsed, err := parseHyperliquidFundingHistory(raw, symbol, window)
		if err != nil {
			return nil, err
		}
		rows = append(rows, parsed...)
		if len(raw) < hyperliquidFundingHistoryLimit {
			break
		}
		next := raw[len(raw)-1].Time + 1
		if next <= cursorMs {
			break
		}
		cursorMs = next
		if err := fundingHistoryPause(ctx); err != nil {
			return nil, err
		}
	}
	return finishFundingHistory(source, symbol, FundingDiscrete, rows)
}

func parseHyperliquidFundingHistory(raw []hyperliquidFundingHistoryRow, symbol Symbol, window FundingWindow) ([]fundingHistoryRow, error) {
	out := make([]fundingHistoryRow, 0, len(raw))
	for _, entry := range raw {
		if entry.Coin != symbol.Venue || !window.contains(entry.Time) {
			continue
		}
		rateFrac, err := strconv.ParseFloat(entry.FundingRate, 64)
		if err != nil {
			return nil, fmt.Errorf("hyperliquid funding history %s: fundingRate %q does not parse",
				entry.Coin, entry.FundingRate)
		}
		out = append(out, fundingHistoryRow{
			SettledAtMs:  entry.Time,
			RateFrac:     rateFrac,
			RawRate:      rateFrac,
			RawRateField: "fundingRate",
		})
	}
	return out, nil
}
