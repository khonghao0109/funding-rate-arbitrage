package binance

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Binance hourly candles, spot and futures (step 3.3b).
//
//	GET https://fapi.binance.com/fapi/v1/klines?symbol=&interval=1h&startTime=&endTime=&limit=1500
//	GET https://api.binance.com/api/v3/klines?symbol=&interval=1h&startTime=&endTime=&limit=1000
//	[[1757235600000,"111009.7","111200.0","110800.1","111062.9","24797.9",1757239199999,...]]
//
// One array per candle, positional: 0 open time (ms), 1 open, 2 high, 3 low,
// 4 close, 5 base volume, 6 close time. The two endpoints share that shape and
// differ in host, path and page size, which is why one parser serves both and
// only the request differs.
//
// Measured 2026-09-07: both ascending; futures answered 1500 rows and spot
// 1000 for a startTime a full year back. Every element after index 6 is
// ignored here on purpose — quote volume, trade count and the taker splits are
// real fields this project has no use for, and reading one by position is how
// a shifted index becomes a silently wrong price.
// https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Kline-Candlestick-Data
// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/market-data-endpoints
const (
	binanceFuturesPriceHistoryLimit = 1500
	binanceSpotPriceHistoryLimit    = 1000
)

// FetchFuturesPriceHistory reads USDT-M perpetual candles.
func FetchFuturesPriceHistory(ctx context.Context, source string, symbol exchanges.Symbol, window exchanges.PriceWindow) ([]exchanges.PriceCandle, error) {
	return fetchBinancePriceHistory(ctx, source, symbol, window,
		"https://fapi.binance.com/fapi/v1/klines", binanceFuturesPriceHistoryLimit)
}

// FetchSpotPriceHistory reads spot candles.
func FetchSpotPriceHistory(ctx context.Context, source string, symbol exchanges.Symbol, window exchanges.PriceWindow) ([]exchanges.PriceCandle, error) {
	return fetchBinancePriceHistory(ctx, source, symbol, window,
		"https://api.binance.com/api/v3/klines", binanceSpotPriceHistoryLimit)
}

func fetchBinancePriceHistory(ctx context.Context, source string, symbol exchanges.Symbol,
	window exchanges.PriceWindow, endpoint string, limit int) ([]exchanges.PriceCandle, error) {

	var rows []exchanges.PriceCandleRow
	cursorMs := window.StartMs

	for page := 0; page < exchanges.MaxPriceHistoryPages; page++ {
		url := fmt.Sprintf("%s?symbol=%s&interval=1h&startTime=%d&endTime=%d&limit=%d",
			endpoint, symbol.Venue, cursorMs, window.EndMs, limit)
		var raw [][]json.RawMessage
		if err := exchanges.FetchFundingHistoryPage(ctx, exchanges.FundingHistoryPageDelay, func() error {
			raw = nil
			return exchanges.FetchJSON(ctx, url, &raw)
		}); err != nil {
			// A market this venue does not list is ABSENT, not an error — the
			// same rule the funding fetchers follow.
			if errors.Is(err, exchanges.ErrNotListed) {
				break
			}
			return nil, err
		}
		parsed, err := parseBinanceKlines(raw, source, window)
		if err != nil {
			return nil, err
		}
		rows = append(rows, parsed...)
		if len(raw) < limit {
			break
		}
		// Advance past the last candle RECEIVED, not the last one kept: a page
		// wholly outside the window would otherwise leave the cursor where it
		// was and the loop would re-request it until the page budget ran out.
		lastOpen, err := binanceKlineOpenTime(raw[len(raw)-1])
		if err != nil {
			return nil, err
		}
		next := lastOpen + 1
		if next <= cursorMs {
			break
		}
		cursorMs = next
		if err := exchanges.FundingHistoryPause(ctx, exchanges.FundingHistoryPageDelay); err != nil {
			return nil, err
		}
	}
	return exchanges.FinishPriceHistory(source, symbol, rows)
}

func binanceKlineOpenTime(kline []json.RawMessage) (int64, error) {
	if len(kline) < 6 {
		return 0, fmt.Errorf("binance klines: a candle has %d fields, need at least 6", len(kline))
	}
	var openTimeMs int64
	if err := json.Unmarshal(kline[0], &openTimeMs); err != nil {
		return 0, fmt.Errorf("binance klines: open time %s does not parse: %w", kline[0], err)
	}
	return openTimeMs, nil
}

func parseBinanceKlines(raw [][]json.RawMessage, source string, window exchanges.PriceWindow) ([]exchanges.PriceCandleRow, error) {
	out := make([]exchanges.PriceCandleRow, 0, len(raw))
	for _, kline := range raw {
		openTimeMs, err := binanceKlineOpenTime(kline)
		if err != nil {
			return nil, err
		}
		if !window.Contains(openTimeMs) {
			continue
		}
		prices := make([]float64, 5)
		for i := 1; i <= 5; i++ {
			var text string
			if err := json.Unmarshal(kline[i], &text); err != nil {
				return nil, fmt.Errorf("%s klines: field %d of the candle at %d is not a string", source, i, openTimeMs)
			}
			value, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return nil, fmt.Errorf("%s klines: field %d of the candle at %d is %q, which does not parse",
					source, i, openTimeMs, text)
			}
			prices[i-1] = value
		}
		out = append(out, exchanges.PriceCandleRow{
			OpenTimeMs: openTimeMs, IntervalSec: exchanges.PriceCandleIntervalSec,
			OpenPriceQuote: prices[0], HighPriceQuote: prices[1],
			LowPriceQuote: prices[2], ClosePriceQuote: prices[3],
			// Index 5 is the BASE asset volume on both endpoints; index 7 is
			// the quote one and is not read.
			BaseVolumeCoin: prices[4],
		})
	}
	return out, nil
}
