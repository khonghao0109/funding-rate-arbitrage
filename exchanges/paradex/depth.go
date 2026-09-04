package paradex

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"fmt"
)

// Paradex order book depth (step 2.7b).
//
//	GET /v1/orderbook/BTC-USD-PERP?depth=100
//	{"market":"BTC-USD-PERP","seq_no":7654537507,"last_updated_at":1788496622765,
//	 "bids":[["81130.9","0.00048"],...],"asks":[["81308","0.03"],...]}
//
// Measured 2026-09-04: bids descending, asks ascending, sizes in COIN.
//
// ⚠️ THE TWO SIDES ARE NOT THE SAME LENGTH. depth=100 returned 100 bids and 43
// asks, and the best ask sat 0.22% above the best bid while Binance's spread on
// the same pair was a fraction of that. Neither is an error — the book really
// is that thin and that wide — and it is exactly the case liquidity ranking
// exists to surface: a headline funding rate on a market nobody can exit at the
// modelled price.
//
// A market this venue does not list answers HTTP 404, which doInstrumentRequest
// already turns into ErrNotListed (measured at step 2.4, where one
// unsupported pair blanked the whole source).
// https://docs.paradex.trade/api-reference/prod/market-data/get-orderbook
//
// paradexDepthMaxLevels is a HARD ceiling the venue states in its own words.
// Asking for 200 returns HTTP 400 with
// {"error":"INVALID_REQUEST_PARAMETER","message":"Depth: must be no greater
// than 100."} — so, like Gate, asking too deep loses the whole book instead of
// returning a shorter one. Found during the step-2.7b acceptance run, when
// raising the shared request from 100 to 1000 turned four healthy Paradex
// series into failures.
//
// It costs nothing here: this book is thin and wide enough that 100 levels
// already span 14.5% of mid on BTC (measured 2026-09-04), far past every window
// this project measures.
const paradexDepthMaxLevels = 100

type paradexDepthResponse struct {
	Market        string     `json:"market"`
	SeqNo         int64      `json:"seq_no"`
	LastUpdatedAt int64      `json:"last_updated_at"`
	Bids          [][]string `json:"bids"`
	Asks          [][]string `json:"asks"`
}

// FetchDepth reads one perpetual book.
func FetchDepth(ctx context.Context, source string, symbol exchanges.Symbol, levels int) (exchanges.DepthBook, error) {
	levels = min(levels, paradexDepthMaxLevels)
	url := fmt.Sprintf("https://api.prod.paradex.trade/v1/orderbook/%s?depth=%d", symbol.Venue, levels)
	var resp paradexDepthResponse
	if err := exchanges.FetchJSON(ctx, url, &resp); err != nil {
		return exchanges.DepthBook{}, err
	}
	return parseParadexDepth(resp, source, symbol)
}

func parseParadexDepth(resp paradexDepthResponse, source string, symbol exchanges.Symbol) (exchanges.DepthBook, error) {
	book := exchanges.DepthBook{Symbol: symbol.Standard, Source: source, VenueTimeMs: resp.LastUpdatedAt}
	var err error
	if book.Bids, err = exchanges.ParseDepthSide(source, symbol.Venue, resp.Bids); err != nil {
		return exchanges.DepthBook{}, err
	}
	if book.Asks, err = exchanges.ParseDepthSide(source, symbol.Venue, resp.Asks); err != nil {
		return exchanges.DepthBook{}, err
	}
	return exchanges.FinishDepthBook(book)
}
