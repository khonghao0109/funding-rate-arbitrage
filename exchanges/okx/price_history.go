package okx

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"errors"
	"fmt"
	"strconv"
)

// OKX hourly candles (step 3.3b).
//
//	GET /api/v5/market/history-candles?instId=BTC-USDT-SWAP&bar=1H&after=&limit=300
//	{"code":"0","data":[["1788768000000","79357.9","79508","79350.6","79385.1",
//	                     "148262.59","1482.6259","117753437.49","1"]]}
//
// Positional, all strings: 0 ts (ms, the START of the bar), 1 open, 2 high,
// 3 low, 4 close, 5 vol, 6 volCcy, 7 volCcyQuote, 8 confirm.
//
// Three things measured 2026-09-07 against the live endpoint.
//
// history-candles, NOT candles. The plain endpoint answered an EMPTY data
// array at 300 days back while history-candles returned rows at 400 — a venue
// retention boundary that looks exactly like "this market has no history".
//
// The data comes back NEWEST FIRST, so the cursor is the LAST element.
//
// `after` is an EXCLUSIVE UPPER bound on ts — it means "candles older than
// this", which is the opposite of what the word suggests and the opposite of
// Binance's startTime. Walking a window therefore means starting at its END and
// stepping backwards, and the loop below is the only one in this package that
// does. Getting this backwards returns the newest 300 candles on every page
// and never terminates against the budget.
//
// vol is in CONTRACTS for a swap and volCcy in the base coin, but which is
// which depends on the instrument's own ctVal — the registry knows that and
// this package does not, so BaseVolumeCoin stays 0, which by this project's
// convention means NOT KNOWN and never "no volume".
// https://www.okx.com/docs-v5/en/#order-book-trading-market-data-get-candlesticks-history
const okxPriceHistoryLimit = 300

type okxCandleResponse struct {
	Code string     `json:"code"`
	Msg  string     `json:"msg"`
	Data [][]string `json:"data"`
}

// FetchPriceHistory walks the window BACKWARDS, one page at a time.
func FetchPriceHistory(ctx context.Context, source string, symbol exchanges.Symbol, window exchanges.PriceWindow) ([]exchanges.PriceCandle, error) {
	var rows []exchanges.PriceCandleRow
	cursorMs := window.EndMs // exclusive upper bound; the first page is the newest

	for page := 0; page < exchanges.MaxPriceHistoryPages; page++ {
		url := fmt.Sprintf(
			"https://www.okx.com/api/v5/market/history-candles?instId=%s&bar=1H&after=%d&limit=%d",
			symbol.Venue, cursorMs, okxPriceHistoryLimit)
		var resp okxCandleResponse
		if err := exchanges.FetchFundingHistoryPage(ctx, exchanges.FundingHistoryPageDelay, func() error {
			resp = okxCandleResponse{}
			return exchanges.FetchJSON(ctx, url, &resp)
		}); err != nil {
			if errors.Is(err, exchanges.ErrNotListed) {
				break
			}
			return nil, err
		}
		if resp.Code != "0" {
			// 51001 is "instrument does not exist", which reads as absent
			// (CLAUDE.md trap table). Every other code stays a loud error.
			if okxCodeMeansNotListed(resp.Code) {
				break
			}
			return nil, fmt.Errorf("okx history-candles %s: code %s %q", symbol.Venue, resp.Code, resp.Msg)
		}
		if len(resp.Data) == 0 {
			break // past this venue's retention, which is not an error
		}
		parsed, err := parseOKXCandles(resp.Data, source, window)
		if err != nil {
			return nil, err
		}
		rows = append(rows, parsed...)

		// NEWEST FIRST, so the OLDEST candle of this page is the last element,
		// and the next page is everything older than it.
		oldest, err := strconv.ParseInt(resp.Data[len(resp.Data)-1][0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("okx history-candles %s: ts %q does not parse", symbol.Venue, resp.Data[len(resp.Data)-1][0])
		}
		if oldest <= window.StartMs || oldest >= cursorMs {
			break // reached the start of the window, or the venue stopped moving
		}
		cursorMs = oldest
		if err := exchanges.FundingHistoryPause(ctx, exchanges.FundingHistoryPageDelay); err != nil {
			return nil, err
		}
	}
	return exchanges.FinishPriceHistory(source, symbol, rows)
}

func parseOKXCandles(data [][]string, source string, window exchanges.PriceWindow) ([]exchanges.PriceCandleRow, error) {
	out := make([]exchanges.PriceCandleRow, 0, len(data))
	for _, candle := range data {
		if len(candle) < 5 {
			return nil, fmt.Errorf("%s history-candles: a candle has %d fields, need at least 5", source, len(candle))
		}
		openTimeMs, err := strconv.ParseInt(candle[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s history-candles: ts %q does not parse", source, candle[0])
		}
		if !window.Contains(openTimeMs) {
			continue
		}
		values := make([]float64, 4)
		for i := 1; i <= 4; i++ {
			value, err := strconv.ParseFloat(candle[i], 64)
			if err != nil {
				return nil, fmt.Errorf("%s history-candles: field %d of the candle at %d is %q, which does not parse",
					source, i, openTimeMs, candle[i])
			}
			values[i-1] = value
		}
		out = append(out, exchanges.PriceCandleRow{
			OpenTimeMs: openTimeMs, IntervalSec: exchanges.PriceCandleIntervalSec,
			OpenPriceQuote: values[0], HighPriceQuote: values[1],
			LowPriceQuote: values[2], ClosePriceQuote: values[3],
			// BaseVolumeCoin stays 0 — see the header.
		})
	}
	return out, nil
}
