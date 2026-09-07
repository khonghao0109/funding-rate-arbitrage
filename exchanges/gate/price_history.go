package gate

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"errors"
	"fmt"
	"strconv"
)

// Gate hourly candles, USDT-M futures (step 3.3b).
//
//	GET /api/v4/futures/usdt/candlesticks?contract=BTC_USDT&interval=1h&from=&to=
//	[{"t":1757235600,"o":"111009.7","h":"111200","l":"110800.1","c":"111062.9",
//	  "v":24797993,"sum":"275316298.57"}]
//
// Named fields, not positional — the one venue here that does. Measured
// 2026-09-07: oldest first, and a hard page cap of 2000 rows however wide the
// from/to span is.
//
// The stamp `t` is in SECONDS, and so are `from` and `to`. Every other venue in
// this project stamps candles in milliseconds; passing a millisecond value here
// asks for a window fifty thousand years wide and gets one row back. The
// conversion happens in exactly two places below, both against the window.
//
// This 1h endpoint is NOT subject to the 180-day limit the funding-history one
// enforces: measured the same day, a `from` a full year back answered with
// data, while /futures/usdt/funding_rate refuses anything older than 180 days.
// Two endpoints of the same venue with different retention is exactly the shape
// the history-depth trap warns about, so read the coverage rather than assuming
// either number applies to the other.
//
// `v` is the traded volume in CONTRACTS (Gate's futures are contract
// denominated — CLAUDE.md trap table), and converting it needs the instrument
// registry, which this package does not have. BaseVolumeCoin therefore stays 0,
// which by this project's convention means NOT KNOWN, never "no volume".
// https://www.gate.com/docs/developers/apiv4/#get-futures-candlesticks
const (
	gatePriceHistoryLimit = 2000
	msPerSecondGate       = 1000
)

type gateCandle struct {
	T int64   `json:"t"`
	O string  `json:"o"`
	H string  `json:"h"`
	L string  `json:"l"`
	C string  `json:"c"`
	V float64 `json:"v"`
}

// FetchPriceHistory walks the window forward, one page at a time.
func FetchPriceHistory(ctx context.Context, source string, symbol exchanges.Symbol, window exchanges.PriceWindow) ([]exchanges.PriceCandle, error) {
	var rows []exchanges.PriceCandleRow
	cursorSec := window.StartMs / msPerSecondGate
	endSec := window.EndMs / msPerSecondGate

	for page := 0; page < exchanges.MaxPriceHistoryPages; page++ {
		if cursorSec >= endSec {
			break
		}
		// Ask by `from` + `limit` rather than from/to: the cap is on ROWS, so a
		// from/to span wider than 2000 candles is silently truncated to the
		// first 2000 anyway and the explicit limit makes the page boundary a
		// number this loop can reason about.
		url := fmt.Sprintf(
			"https://api.gateio.ws/api/v4/futures/usdt/candlesticks?contract=%s&interval=1h&from=%d&limit=%d",
			symbol.Venue, cursorSec, gatePriceHistoryLimit)
		var raw []gateCandle
		if err := exchanges.FetchFundingHistoryPage(ctx, exchanges.FundingHistoryPageDelay, func() error {
			raw = nil
			return exchanges.FetchJSON(ctx, url, &raw)
		}); err != nil {
			if errors.Is(err, exchanges.ErrNotListed) {
				break
			}
			return nil, err
		}
		if len(raw) == 0 {
			break
		}
		parsed, err := parseGateCandles(raw, source, window)
		if err != nil {
			return nil, err
		}
		rows = append(rows, parsed...)
		if len(raw) < gatePriceHistoryLimit {
			break
		}
		next := raw[len(raw)-1].T + 1
		if next <= cursorSec {
			break
		}
		cursorSec = next
		if err := exchanges.FundingHistoryPause(ctx, exchanges.FundingHistoryPageDelay); err != nil {
			return nil, err
		}
	}
	return exchanges.FinishPriceHistory(source, symbol, rows)
}

func parseGateCandles(raw []gateCandle, source string, window exchanges.PriceWindow) ([]exchanges.PriceCandleRow, error) {
	out := make([]exchanges.PriceCandleRow, 0, len(raw))
	for _, candle := range raw {
		openTimeMs := candle.T * msPerSecondGate
		if !window.Contains(openTimeMs) {
			continue
		}
		values := make([]float64, 4)
		for i, field := range []struct {
			name string
			text string
		}{{"o", candle.O}, {"h", candle.H}, {"l", candle.L}, {"c", candle.C}} {
			value, err := strconv.ParseFloat(field.text, 64)
			if err != nil {
				return nil, fmt.Errorf("%s candlesticks: %s of the candle at %d is %q, which does not parse",
					source, field.name, openTimeMs, field.text)
			}
			values[i] = value
		}
		out = append(out, exchanges.PriceCandleRow{
			OpenTimeMs: openTimeMs, IntervalSec: exchanges.PriceCandleIntervalSec,
			OpenPriceQuote: values[0], HighPriceQuote: values[1],
			LowPriceQuote: values[2], ClosePriceQuote: values[3],
			// BaseVolumeCoin stays 0 — v is in contracts; see the header.
		})
	}
	return out, nil
}
