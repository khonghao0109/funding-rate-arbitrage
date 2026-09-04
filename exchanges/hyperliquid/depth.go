package hyperliquid

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"fmt"
)

// Hyperliquid order book depth (step 2.7b).
//
//	POST /info  {"type":"l2Book","coin":"BTC"}
//	{"coin":"BTC","time":1788500549476,
//	 "levels":[[{"px":"81117.0","sz":"3.32858","n":13},...],
//	           [{"px":"81118.0","sz":"5.34178","n":24},...]]}
//
// ⚠️ `levels` is a two-element array, NOT an object: levels[0] is the BID side
// (descending) and levels[1] is the ASK side (ascending). There is no key
// naming either of them, so the only thing separating bids from asks is the
// position — and getting it backwards produces a crossed book, which
// FinishDepthBook refuses rather than publishes.
//
// ⚠️ IT RETURNS 20 LEVELS A SIDE AND NOTHING WIDENS THAT. Measured 2026-09-04
// on BTC: twenty levels spanned 81117 down to 81097, which is 0.025% — narrower
// than the 0.1% window this project measures depth over. Any depth figure from
// this venue is therefore a LOWER BOUND, and internal/depth reports the span so
// the dashboard can say so instead of showing Hyperliquid as thin.
//
// Sizes are in COIN. l2Book is one of the weight-2 info requests, the cheapest
// tier, so an hourly poll costs nothing against the 1200/minute budget.
// https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/info-endpoint
type hyperliquidDepthResponse struct {
	Coin   string `json:"coin"`
	TimeMs int64  `json:"time"`
	Levels [][]struct {
		Px string `json:"px"`
		Sz string `json:"sz"`
		N  int    `json:"n"`
	} `json:"levels"`
}

// hyperliquidDepthSides is the number of sides the response must carry.
const hyperliquidDepthSides = 2

// FetchDepth reads one book. levels is ignored: the endpoint has no
// depth parameter and answers with 20 a side.
func FetchDepth(ctx context.Context, source string, symbol exchanges.Symbol, _ int) (exchanges.DepthBook, error) {
	payload := fmt.Sprintf(`{"type":"l2Book","coin":%q}`, symbol.Venue)
	var resp hyperliquidDepthResponse
	if err := exchanges.PostJSON(ctx, "https://api.hyperliquid.xyz/info", payload, &resp); err != nil {
		return exchanges.DepthBook{}, err
	}
	return parseHyperliquidDepth(resp, source, symbol)
}

func parseHyperliquidDepth(resp hyperliquidDepthResponse, source string, symbol exchanges.Symbol) (exchanges.DepthBook, error) {
	// A market this venue does not list answers with an empty levels array
	// rather than an error, and an empty book must not read as "no liquidity".
	if len(resp.Levels) != hyperliquidDepthSides {
		return exchanges.DepthBook{}, fmt.Errorf("hyperliquid depth %s: %d sides in levels, want %d",
			symbol.Venue, len(resp.Levels), hyperliquidDepthSides)
	}

	book := exchanges.DepthBook{Symbol: symbol.Standard, Source: source, VenueTimeMs: resp.TimeMs}
	for _, side := range []struct {
		into  *[]exchanges.DepthLevel
		index int
	}{{&book.Bids, 0}, {&book.Asks, 1}} {
		for _, level := range resp.Levels[side.index] {
			parsed, err := exchanges.ParseDepthLevel(source, symbol.Venue, level.Px, level.Sz)
			if err != nil {
				return exchanges.DepthBook{}, err
			}
			*side.into = append(*side.into, parsed)
		}
	}
	return exchanges.FinishDepthBook(book)
}
