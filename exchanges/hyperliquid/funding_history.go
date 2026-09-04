package hyperliquid

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
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

// hyperliquidHistoryPageDelay paces this fetcher to the budget Hyperliquid
// publishes, which is far tighter for THIS endpoint than the shared default
// assumes.
//
// The documented cost: REST requests share "an aggregated weight limit of 1200
// per minute" per IP; a documented info request is weight 20; and fundingHistory
// is on the list carrying "an additional rate limit weight per 20 items returned
// in the response". A full 500-row page is therefore 20 + 25 = 45, so the budget
// is 1200/45 ≈ 26 pages a minute — one every 2.3 seconds. 2.5s is that with
// margin.
//
// The shared 200ms default is eleven times over it, and this is not theoretical:
// on 2026-09-04 a 12-month backfill of four pairs got BTC and ETH in full and
// then HTTP 429 on XRP and SOL, which left both series at the 7 days the
// scanner's bootstrap had collected. A backfill has no deadline; a corpus with
// two holes in it is a phase-3 problem.
// https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/rate-limits-and-user-limits
const hyperliquidHistoryPageDelay = 2500 * time.Millisecond

type hyperliquidFundingHistoryRow struct {
	Coin        string `json:"coin"`
	FundingRate string `json:"fundingRate"`
	Premium     string `json:"premium"`
	Time        int64  `json:"time"`
}

// FetchFundingHistory walks the window forward.
func FetchFundingHistory(ctx context.Context, source string, symbol exchanges.Symbol, window exchanges.FundingWindow) ([]exchanges.FundingHistoryEntry, error) {
	var rows []exchanges.FundingHistoryRow
	cursorMs := window.StartMs

	for page := 0; page < exchanges.MaxFundingHistoryPages; page++ {
		payload := fmt.Sprintf(`{"type":"fundingHistory","coin":%q,"startTime":%d,"endTime":%d}`,
			symbol.Venue, cursorMs, window.EndMs)
		var raw []hyperliquidFundingHistoryRow
		if err := exchanges.FetchFundingHistoryPage(ctx, hyperliquidHistoryPageDelay, func() error {
			raw = nil
			return exchanges.PostJSON(ctx, "https://api.hyperliquid.xyz/info", payload, &raw)
		}); err != nil {
			// A market this venue does not list is ABSENT, not an error:
			// step 2.4 found live that one unsupported pair (XLMUSDT on
			// Paradex) otherwise blanks a whole source. Here it would fill
			// the log with an hourly failure for a pair that will never
			// exist.
			if errors.Is(err, exchanges.ErrNotListed) {
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
		if err := exchanges.FundingHistoryPause(ctx, hyperliquidHistoryPageDelay); err != nil {
			return nil, err
		}
	}
	return exchanges.FinishFundingHistory(source, symbol, exchanges.FundingDiscrete, rows)
}

func parseHyperliquidFundingHistory(raw []hyperliquidFundingHistoryRow, symbol exchanges.Symbol, window exchanges.FundingWindow) ([]exchanges.FundingHistoryRow, error) {
	out := make([]exchanges.FundingHistoryRow, 0, len(raw))
	for _, entry := range raw {
		if entry.Coin != symbol.Venue || !window.Contains(entry.Time) {
			continue
		}
		rateFrac, err := strconv.ParseFloat(entry.FundingRate, 64)
		if err != nil {
			return nil, fmt.Errorf("hyperliquid funding history %s: fundingRate %q does not parse",
				entry.Coin, entry.FundingRate)
		}
		out = append(out, exchanges.FundingHistoryRow{
			SettledAtMs:  entry.Time,
			RateFrac:     rateFrac,
			RawRate:      rateFrac,
			RawRateField: "fundingRate",
		})
	}
	return out, nil
}
