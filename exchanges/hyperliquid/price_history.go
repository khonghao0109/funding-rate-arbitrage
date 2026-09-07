package hyperliquid

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"errors"
	"fmt"
	"strconv"
)

// Hyperliquid hourly candles (step 3.3b).
//
//	POST /info  {"type":"candleSnapshot","req":{"coin":"BTC","interval":"1h",
//	                                            "startTime":...,"endTime":...}}
//	[{"t":1786176000000,"T":1786179599999,"s":"BTC","i":"1h","o":"64926.0",
//	  "c":"64961.0","h":"64983.0","l":"64919.0","v":"109.42002","n":6492}]
//
// Oldest first. `t` is the open stamp in ms, `T` the close stamp, `v` the base
// volume, `n` the trade count.
//
// THE REACH IS ABOUT 208 DAYS, and the venue says so by answering an EMPTY
// ARRAY rather than an error. Measured 2026-09-07 by walking backwards: 30, 120,
// 200 and 210 days back all returned candles; 250 and 365 returned []. A single
// unbounded request answered 5003 rows starting 2026-02-10 whatever startTime
// was asked for, which is the same 5000-candle ceiling seen from the other side.
// This is the price-history twin of the funding-history depth trap: a 12-month
// backfill of this venue produces about seven months, silently, and anything
// comparing it against Binance's full year is comparing two different spans.
// Read the coverage.
//
// Paced at hyperliquidHistoryPageDelay, the same budget the funding fetcher
// computes from Hyperliquid's published weights — the two share one IP limit,
// and a price backfill running at the shared 200ms default would spend the
// funding backfill's allowance.
// https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/info-endpoint
const hyperliquidPriceHistoryLimit = 5000

// hyperliquidCandle declares BOTH members of the t/T pair even though only the
// open stamp is used.
//
// This payload carries "t" (open, ms) and "T" (close, ms) and Go's
// encoding/json prefers an exact tag match but FALLS BACK TO A CASE-INSENSITIVE
// one. Declaring only OpenTimeMs `json:"t"` therefore let "T" overwrite it, and
// every candle came out stamped 1786096799999 instead of 1786096800000 — one
// millisecond before the hour, so every join against another venue's candles
// found nothing and the basis for this venue was silently empty. Measured here
// on 2026-09-07; it is the same defect the trap table records for Binance's
// aggTrade m/M, hit a second time in a second package.
//
// Leaving the unused member out is not "ignore it", it is "let it overwrite the
// other one".
type hyperliquidCandle struct {
	OpenTimeMs  int64 `json:"t"`
	CloseTimeMs int64 `json:"T"`

	S string `json:"s"`
	O string `json:"o"`
	H string `json:"h"`
	L string `json:"l"`
	C string `json:"c"`
	V string `json:"v"`
}

// FetchPriceHistory walks the window forward.
func FetchPriceHistory(ctx context.Context, source string, symbol exchanges.Symbol, window exchanges.PriceWindow) ([]exchanges.PriceCandle, error) {
	var rows []exchanges.PriceCandleRow
	cursorMs := window.StartMs

	for page := 0; page < exchanges.MaxPriceHistoryPages; page++ {
		payload := fmt.Sprintf(`{"type":"candleSnapshot","req":{"coin":%q,"interval":"1h","startTime":%d,"endTime":%d}}`,
			symbol.Venue, cursorMs, window.EndMs)
		var raw []hyperliquidCandle
		if err := exchanges.FetchFundingHistoryPage(ctx, hyperliquidHistoryPageDelay, func() error {
			raw = nil
			return exchanges.PostJSON(ctx, "https://api.hyperliquid.xyz/info", payload, &raw)
		}); err != nil {
			if errors.Is(err, exchanges.ErrNotListed) {
				break
			}
			return nil, err
		}
		if len(raw) == 0 {
			// Either past the ~208-day reach or past the end of the window.
			// Both are "nothing more exists", not a failure.
			break
		}
		parsed, err := parseHyperliquidCandles(raw, symbol, source, window)
		if err != nil {
			return nil, err
		}
		rows = append(rows, parsed...)
		if len(raw) < hyperliquidPriceHistoryLimit {
			break
		}
		next := raw[len(raw)-1].OpenTimeMs + 1
		if next <= cursorMs {
			break
		}
		cursorMs = next
		if err := exchanges.FundingHistoryPause(ctx, hyperliquidHistoryPageDelay); err != nil {
			return nil, err
		}
	}
	return exchanges.FinishPriceHistory(source, symbol, rows)
}

func parseHyperliquidCandles(raw []hyperliquidCandle, symbol exchanges.Symbol, source string,
	window exchanges.PriceWindow) ([]exchanges.PriceCandleRow, error) {

	out := make([]exchanges.PriceCandleRow, 0, len(raw))
	for _, candle := range raw {
		// The coin is checked for the same reason the funding parser checks it:
		// this endpoint is asked for one coin, and a row for another one would
		// mean the request and the response have come apart.
		if candle.S != symbol.Venue || !window.Contains(candle.OpenTimeMs) {
			continue
		}
		values := make([]float64, 5)
		for i, field := range []struct{ name, text string }{
			{"o", candle.O}, {"h", candle.H}, {"l", candle.L}, {"c", candle.C}, {"v", candle.V},
		} {
			value, err := strconv.ParseFloat(field.text, 64)
			if err != nil {
				return nil, fmt.Errorf("%s candleSnapshot: %s of the candle at %d is %q, which does not parse",
					source, field.name, candle.OpenTimeMs, field.text)
			}
			values[i] = value
		}
		out = append(out, exchanges.PriceCandleRow{
			OpenTimeMs: candle.OpenTimeMs, IntervalSec: exchanges.PriceCandleIntervalSec,
			OpenPriceQuote: values[0], HighPriceQuote: values[1],
			LowPriceQuote: values[2], ClosePriceQuote: values[3],
			BaseVolumeCoin: values[4],
		})
	}
	return out, nil
}
