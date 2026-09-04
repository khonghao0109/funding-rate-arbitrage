package exchanges

import (
	"context"
	"fmt"
)

// Binance order book depth (step 2.7b).
//
//	GET /fapi/v1/depth?symbol=BTCUSDT&limit=100      (USDⓈ-M futures)
//	GET /api/v3/depth?symbol=BTCUSDT&limit=100       (spot)
//	{"lastUpdateId":11471005431016,"E":1788496607935,"T":1788496607933,
//	 "bids":[["81124.30","15.424"],...],"asks":[["81125.10","3.201"],...]}
//
// Measured 2026-09-04: both return exactly the requested 100 levels a side,
// bids descending and asks ascending, sizes in COIN. The futures response
// carries `E` (event time) and `T` (transaction time); the spot one carries
// neither, only lastUpdateId — so VenueTimeMs is 0 there, which is what the
// field's documented default is for.
// https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Order-Book
// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/market-data-endpoints
//
// binanceDepthMaxLevels is what this fetcher will ask for. Both endpoints
// accept more (spot goes to 5000), but the request weight climbs with the limit
// and 1000 already reaches far enough: measured 2026-09-04 on BTCUSDT, 100
// levels span 0.017% of mid while 1000 span 0.161% (futures) and 0.235%
// (spot) — past the tight window this project measures depth over. 100 was the
// first value tried at step 2.7b and produced figures that were floors on
// almost every venue.
const binanceDepthMaxLevels = 1000

type binanceDepthResponse struct {
	LastUpdateID int64 `json:"lastUpdateId"`
	// EventTimeMs is futures-only; spot leaves it absent and therefore 0.
	EventTimeMs int64      `json:"E"`
	Bids        [][]string `json:"bids"`
	Asks        [][]string `json:"asks"`
}

// FetchBinanceFuturesDepth reads the USDⓈ-M futures book.
func FetchBinanceFuturesDepth(ctx context.Context, source string, symbol Symbol, levels int) (DepthBook, error) {
	return fetchBinanceDepth(ctx, "https://fapi.binance.com/fapi/v1/depth", source, symbol, levels)
}

// FetchBinanceSpotDepth reads the spot book.
func FetchBinanceSpotDepth(ctx context.Context, source string, symbol Symbol, levels int) (DepthBook, error) {
	return fetchBinanceDepth(ctx, "https://api.binance.com/api/v3/depth", source, symbol, levels)
}

func fetchBinanceDepth(ctx context.Context, endpoint, source string, symbol Symbol, levels int) (DepthBook, error) {
	levels = min(levels, binanceDepthMaxLevels)
	url := fmt.Sprintf("%s?symbol=%s&limit=%d", endpoint, symbol.Venue, levels)
	var resp binanceDepthResponse
	if err := fetchInstrumentJSON(ctx, url, &resp); err != nil {
		return DepthBook{}, err
	}
	return parseBinanceDepth(resp, source, symbol)
}

func parseBinanceDepth(resp binanceDepthResponse, source string, symbol Symbol) (DepthBook, error) {
	book := DepthBook{Symbol: symbol.Standard, Source: source, VenueTimeMs: resp.EventTimeMs}
	var err error
	if book.Bids, err = parseDepthSide(source, symbol.Venue, resp.Bids); err != nil {
		return DepthBook{}, err
	}
	if book.Asks, err = parseDepthSide(source, symbol.Venue, resp.Asks); err != nil {
		return DepthBook{}, err
	}
	return finishDepthBook(book)
}
