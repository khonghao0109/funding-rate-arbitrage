package kraken

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"errors"
	"fmt"
	"strconv"
)

// Kraken Futures hourly candles (step 3.3b).
//
//	GET https://futures.kraken.com/api/charts/v1/trade/PF_XBTUSD/1h?from=&to=
//	{"candles":[{"time":1757001600000,"open":"109533","high":"109710",
//	             "low":"109367","close":"109455","volume":"145.6571"}],
//	 "more_candles":true}
//
// The `trade` series, not `mark` or `index`: the basis this feeds is a
// comparison of what actually traded on the perp against what actually traded
// on the spot leg, and a mark price is the venue's own construction.
//
// Measured 2026-09-07: oldest first, `from`/`to` in SECONDS while `time` comes
// back in MILLISECONDS — the two units live in one request/response pair — and
// a hard cap of 2000 candles per response whatever span is asked for (2000h,
// 4000h and 9000h all returned exactly 2000).
//
// `volume` is in contracts, but Kraken's PF_ contracts are 1 base unit each
// (settled at step 2.3, CLAUDE.md trap table), so the number is numerically the
// base coin and is carried through as such.
// https://docs.kraken.com/api/docs/futures-api/charts/get-ohlc
const (
	krakenPriceHistoryLimit = 2000
	msPerSecondKraken       = 1000
)

type krakenChartResponse struct {
	Candles []struct {
		Time   int64  `json:"time"`
		Open   string `json:"open"`
		High   string `json:"high"`
		Low    string `json:"low"`
		Close  string `json:"close"`
		Volume string `json:"volume"`
	} `json:"candles"`
	MoreCandles bool `json:"more_candles"`
}

// FetchPriceHistory walks the window forward, one page at a time.
func FetchPriceHistory(ctx context.Context, source string, symbol exchanges.Symbol, window exchanges.PriceWindow) ([]exchanges.PriceCandle, error) {
	var rows []exchanges.PriceCandleRow
	cursorSec := window.StartMs / msPerSecondKraken
	endSec := window.EndMs / msPerSecondKraken

	for page := 0; page < exchanges.MaxPriceHistoryPages; page++ {
		if cursorSec >= endSec {
			break
		}
		url := fmt.Sprintf("https://futures.kraken.com/api/charts/v1/trade/%s/1h?from=%d&to=%d",
			symbol.Venue, cursorSec, endSec)
		var resp krakenChartResponse
		if err := exchanges.FetchFundingHistoryPage(ctx, exchanges.FundingHistoryPageDelay, func() error {
			resp = krakenChartResponse{}
			return exchanges.FetchJSON(ctx, url, &resp)
		}); err != nil {
			if errors.Is(err, exchanges.ErrNotListed) {
				break
			}
			return nil, err
		}
		if len(resp.Candles) == 0 {
			break
		}
		parsed, err := parseKrakenCandles(resp, source, window)
		if err != nil {
			return nil, err
		}
		rows = append(rows, parsed...)
		if len(resp.Candles) < krakenPriceHistoryLimit {
			break
		}
		// The stamps come back in ms and the cursor is in seconds — the one
		// place this asymmetry has to be handled, and getting it wrong walks
		// the cursor a thousand times too far in one step.
		next := resp.Candles[len(resp.Candles)-1].Time/msPerSecondKraken + 1
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

func parseKrakenCandles(resp krakenChartResponse, source string, window exchanges.PriceWindow) ([]exchanges.PriceCandleRow, error) {
	out := make([]exchanges.PriceCandleRow, 0, len(resp.Candles))
	for _, candle := range resp.Candles {
		if !window.Contains(candle.Time) {
			continue
		}
		values := make([]float64, 5)
		for i, field := range []struct{ name, text string }{
			{"open", candle.Open}, {"high", candle.High}, {"low", candle.Low},
			{"close", candle.Close}, {"volume", candle.Volume},
		} {
			value, err := strconv.ParseFloat(field.text, 64)
			if err != nil {
				return nil, fmt.Errorf("%s charts: %s of the candle at %d is %q, which does not parse",
					source, field.name, candle.Time, field.text)
			}
			values[i] = value
		}
		out = append(out, exchanges.PriceCandleRow{
			OpenTimeMs: candle.Time, IntervalSec: exchanges.PriceCandleIntervalSec,
			OpenPriceQuote: values[0], HighPriceQuote: values[1],
			LowPriceQuote: values[2], ClosePriceQuote: values[3],
			BaseVolumeCoin: values[4],
		})
	}
	return out, nil
}
