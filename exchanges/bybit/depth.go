package bybit

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"fmt"
)

// Bybit order book depth (step 2.7b).
//
//	GET /v5/market/orderbook?category=linear&symbol=BTCUSDT&limit=200
//	{"retCode":0,"retMsg":"OK","result":{"s":"BTCUSDT",
//	  "b":[["81124.30","15.424"],...],"a":[["81125.10","3.201"],...],
//	  "ts":1788496607935,"u":123,"seq":456,"cts":1788496607900}}
//
// Measured 2026-09-04: linear and spot both answered limit=200 in full, with
// `b` descending and `a` ascending, sizes in COIN. The field names are one
// letter, unlike every other venue here — `b`/`a`, not bids/asks.
//
// Unlike the ticker stream this is a plain snapshot: there is no delta form to
// merge, so the trap that governs bybit_ticker.go does not apply.
// https://bybit-exchange.github.io/docs/v5/market/orderbook
//
// The documented maxima are 500 for linear and 200 for spot. Measured
// 2026-09-04 the endpoint actually answers 1000 and 500 — but an undocumented
// capability is one a venue can withdraw without notice, so the DOCUMENTED
// limit is what this asks for. At those limits BTCUSDT spans 0.101% (linear)
// and 0.177% (spot) of mid, against 0.024% and 0.084% at 100 levels.
const (
	bybitLinearDepthMaxLevels = 500
	bybitSpotDepthMaxLevels   = 200
)

type bybitDepthResponse struct {
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
	Result  struct {
		Symbol      string     `json:"s"`
		Bids        [][]string `json:"b"`
		Asks        [][]string `json:"a"`
		TimestampMs int64      `json:"ts"`
	} `json:"result"`
}

// FetchFuturesDepth reads the linear (USDT perpetual) book.
func FetchFuturesDepth(ctx context.Context, source string, symbol exchanges.Symbol, levels int) (exchanges.DepthBook, error) {
	return fetchBybitDepth(ctx, "linear", source, symbol, min(levels, bybitLinearDepthMaxLevels))
}

// FetchSpotDepth reads the spot book.
func FetchSpotDepth(ctx context.Context, source string, symbol exchanges.Symbol, levels int) (exchanges.DepthBook, error) {
	return fetchBybitDepth(ctx, "spot", source, symbol, min(levels, bybitSpotDepthMaxLevels))
}

func fetchBybitDepth(ctx context.Context, category, source string, symbol exchanges.Symbol, levels int) (exchanges.DepthBook, error) {
	url := fmt.Sprintf("https://api.bybit.com/v5/market/orderbook?category=%s&symbol=%s&limit=%d",
		category, symbol.Venue, levels)
	var resp bybitDepthResponse
	if err := exchanges.FetchJSON(ctx, url, &resp); err != nil {
		return exchanges.DepthBook{}, err
	}
	return parseBybitDepth(resp, source, symbol)
}

func parseBybitDepth(resp bybitDepthResponse, source string, symbol exchanges.Symbol) (exchanges.DepthBook, error) {
	// Bybit wraps its errors in HTTP 200, so the body's code is the only
	// signal. "Not listed" is absence rather than failure, through the same
	// predicate the instrument and history fetchers use — but here absence
	// still cannot produce a book, so it surfaces as an error the caller
	// records against this one series instead of a retry loop.
	if bybitSaysSymbolNotListed(resp.RetCode, resp.RetMsg) {
		return exchanges.DepthBook{}, fmt.Errorf("bybit depth %s: %w", symbol.Venue, exchanges.ErrNotListed)
	}
	if resp.RetCode != 0 {
		return exchanges.DepthBook{}, fmt.Errorf("bybit depth %s: retCode %d %s", symbol.Venue, resp.RetCode, resp.RetMsg)
	}

	book := exchanges.DepthBook{Symbol: symbol.Standard, Source: source, VenueTimeMs: resp.Result.TimestampMs}
	var err error
	if book.Bids, err = exchanges.ParseDepthSide(source, symbol.Venue, resp.Result.Bids); err != nil {
		return exchanges.DepthBook{}, err
	}
	if book.Asks, err = exchanges.ParseDepthSide(source, symbol.Venue, resp.Result.Asks); err != nil {
		return exchanges.DepthBook{}, err
	}
	return exchanges.FinishDepthBook(book)
}
