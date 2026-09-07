package bybit

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"errors"
	"fmt"
	"strconv"
)

// Bybit hourly candles, linear perpetual and spot (step 3.3b).
//
//	GET /v5/market/kline?category=linear|spot&symbol=&interval=60&start=&end=&limit=1000
//	{"retCode":0,"result":{"list":[["1788768000000","79353","79500","79349.1",
//	                               "79379.9","1388.495","110215417.6"]]}}
//
// Positional, all strings: 0 start (ms), 1 open, 2 high, 3 low, 4 close,
// 5 volume, 6 turnover.
//
// THREE traps, all measured 2026-09-07 against the live endpoint.
//
// The list comes back NEWEST FIRST. Every other venue here except OKX sends
// oldest first, and FinishPriceHistory sorts what survives.
//
// With BOTH start and end given, the page is anchored on `end`, not on `start`:
// asking for [a year ago, now] returns the newest 1000 candles ending at now,
// while asking for [a year ago, a year ago + 1000h] returns that window's 1000.
// So a window wider than one page has to be walked BACKWARDS — `end` moves down
// to just before the oldest candle of the page while `start` stays put. Walking
// `start` forward instead re-requests the same newest page every time: measured
// here on the first run, all four Bybit pairs came back with exactly 1000
// candles (42 days) of a 365-day request and reported it as a short reach,
// which is what a venue retention limit looks like. It was this loop.
//
// The interval is "60", meaning sixty MINUTES — not "1h" and not seconds.
// Bybit's minute intervals are bare numbers while its longer ones are letters
// (D, W, M), so the unit is implied by which of the two shapes was used.
// https://bybit-exchange.github.io/docs/v5/market/kline
const bybitPriceHistoryLimit = 1000

type bybitKlineResponse struct {
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
	Result  struct {
		Symbol   string     `json:"symbol"`
		Category string     `json:"category"`
		List     [][]string `json:"list"`
	} `json:"result"`
}

// FetchFuturesPriceHistory reads linear perpetual candles.
func FetchFuturesPriceHistory(ctx context.Context, source string, symbol exchanges.Symbol, window exchanges.PriceWindow) ([]exchanges.PriceCandle, error) {
	return fetchBybitPriceHistory(ctx, source, symbol, window, "linear")
}

// FetchSpotPriceHistory reads spot candles.
func FetchSpotPriceHistory(ctx context.Context, source string, symbol exchanges.Symbol, window exchanges.PriceWindow) ([]exchanges.PriceCandle, error) {
	return fetchBybitPriceHistory(ctx, source, symbol, window, "spot")
}

func fetchBybitPriceHistory(ctx context.Context, source string, symbol exchanges.Symbol,
	window exchanges.PriceWindow, category string) ([]exchanges.PriceCandle, error) {

	var rows []exchanges.PriceCandleRow
	cursorMs := window.EndMs // the page anchor, walked DOWN — see the header

	for page := 0; page < exchanges.MaxPriceHistoryPages; page++ {
		if cursorMs <= window.StartMs {
			break
		}
		url := fmt.Sprintf(
			"https://api.bybit.com/v5/market/kline?category=%s&symbol=%s&interval=60&start=%d&end=%d&limit=%d",
			category, symbol.Venue, window.StartMs, cursorMs, bybitPriceHistoryLimit)
		var resp bybitKlineResponse
		if err := exchanges.FetchFundingHistoryPage(ctx, exchanges.FundingHistoryPageDelay, func() error {
			resp = bybitKlineResponse{}
			return exchanges.FetchJSON(ctx, url, &resp)
		}); err != nil {
			if errors.Is(err, exchanges.ErrNotListed) {
				break
			}
			return nil, err
		}
		if resp.RetCode != 0 {
			// "symbol invalid" is this venue's way of saying not listed
			// (CLAUDE.md trap table), through the same detector the
			// instrument and funding fetchers use — 10001 is Bybit's GENERIC
			// parameter error, so the prose has to be read too. Every other
			// non-zero code stays a loud error.
			if bybitSaysSymbolNotListed(resp.RetCode, resp.RetMsg) {
				break
			}
			return nil, fmt.Errorf("bybit kline %s: retCode %d %q", symbol.Venue, resp.RetCode, resp.RetMsg)
		}
		parsed, err := parseBybitKlines(resp.Result.List, source, window)
		if err != nil {
			return nil, err
		}
		rows = append(rows, parsed...)
		if len(resp.Result.List) == 0 || len(resp.Result.List) < bybitPriceHistoryLimit {
			break
		}
		// NEWEST FIRST, so the OLDEST candle of this page is the last element,
		// and the next page ends just before it.
		last := resp.Result.List[len(resp.Result.List)-1]
		oldest, err := strconv.ParseInt(last[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bybit kline %s: start %q does not parse", symbol.Venue, last[0])
		}
		next := oldest - 1
		if next >= cursorMs {
			break // the venue stopped moving; do not spend the page budget on it
		}
		cursorMs = next
		if err := exchanges.FundingHistoryPause(ctx, exchanges.FundingHistoryPageDelay); err != nil {
			return nil, err
		}
	}
	return exchanges.FinishPriceHistory(source, symbol, rows)
}

func parseBybitKlines(list [][]string, source string, window exchanges.PriceWindow) ([]exchanges.PriceCandleRow, error) {
	out := make([]exchanges.PriceCandleRow, 0, len(list))
	for _, candle := range list {
		if len(candle) < 6 {
			return nil, fmt.Errorf("%s kline: a candle has %d fields, need at least 6", source, len(candle))
		}
		openTimeMs, err := strconv.ParseInt(candle[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s kline: start %q does not parse", source, candle[0])
		}
		if !window.Contains(openTimeMs) {
			continue
		}
		values := make([]float64, 5)
		for i := 1; i <= 5; i++ {
			value, err := strconv.ParseFloat(candle[i], 64)
			if err != nil {
				return nil, fmt.Errorf("%s kline: field %d of the candle at %d is %q, which does not parse",
					source, i, openTimeMs, candle[i])
			}
			values[i-1] = value
		}
		out = append(out, exchanges.PriceCandleRow{
			OpenTimeMs: openTimeMs, IntervalSec: exchanges.PriceCandleIntervalSec,
			OpenPriceQuote: values[0], HighPriceQuote: values[1],
			LowPriceQuote: values[2], ClosePriceQuote: values[3],
			// Index 5 is volume in the BASE asset; index 6 is turnover, in the
			// quote asset, and is not read.
			BaseVolumeCoin: values[4],
		})
	}
	return out, nil
}
