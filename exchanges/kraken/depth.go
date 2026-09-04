package kraken

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"fmt"
)

// Kraken futures order book depth (step 2.7b).
//
//	GET /derivatives/api/v3/orderbook?symbol=PF_XBTUSD
//	{"result":"success","serverTime":"2026-09-04T05:42:30.601Z",
//	 "orderBook":{"bids":[[1,41],[5,2.12],...,[80969,0.0953]],
//	              "asks":[[80970,0.1699],...,[364324,0.0002]]}}
//
// Two things here are unlike every other venue, both measured 2026-09-04:
//
// ⚠️ THE BIDS COME BACK ASCENDING. bids[0] is a resting order at a price of
// ONE, and bids[len-1] is the best bid. Reading index 0 as the top of book —
// which works on all eight other venues — puts a $1 bid at the front of the
// liquidity calculation and makes the spread look like the whole price.
// FinishDepthBook sorts unconditionally, which is why this is survivable.
//
// ⚠️ THERE IS NO LIMIT PARAMETER. The endpoint returns the entire book: 1,856
// bids and 873 asks, about 44 KB, whatever `levels` the caller asked for. That
// is a fixed cost per fetch and the reason depth is polled hourly rather than
// per minute.
//
// Prices and sizes are JSON NUMBERS here, not strings, so this venue does not
// use ParseDepthSide.
//
// Sizes are in CONTRACTS, but step 2.3 measured PF_ contracts at 1 base unit
// each, so the registry's multiplier is 1 and the conversion is an identity —
// stated by the data rather than assumed by this file.
// https://docs.kraken.com/api/docs/futures-api/trading/get-orderbook
type krakenDepthResponse struct {
	Result    string `json:"result"`
	Error     string `json:"error"`
	OrderBook struct {
		// [price, size] pairs as numbers.
		Bids [][]float64 `json:"bids"`
		Asks [][]float64 `json:"asks"`
	} `json:"orderBook"`
}

// FetchDepth reads one futures book. levels is ignored: the venue has no
// limit parameter and always answers with everything.
func FetchDepth(ctx context.Context, source string, symbol exchanges.Symbol, _ int) (exchanges.DepthBook, error) {
	url := fmt.Sprintf("https://futures.kraken.com/derivatives/api/v3/orderbook?symbol=%s", symbol.Venue)
	var resp krakenDepthResponse
	if err := exchanges.FetchJSON(ctx, url, &resp); err != nil {
		return exchanges.DepthBook{}, err
	}
	return parseKrakenDepth(resp, source, symbol)
}

func parseKrakenDepth(resp krakenDepthResponse, source string, symbol exchanges.Symbol) (exchanges.DepthBook, error) {
	if resp.Result != "success" {
		return exchanges.DepthBook{}, fmt.Errorf("kraken depth %s: result %q %s", symbol.Venue, resp.Result, resp.Error)
	}

	// Kraken PF_ books are quoted in contracts — contractSize is 1 base unit
	// today, but that is the registry's measured fact, not this file's.
	book := exchanges.DepthBook{Symbol: symbol.Standard, Source: source, IsContractBook: true}
	var err error
	if book.Bids, err = parseKrakenDepthSide(symbol.Venue, resp.OrderBook.Bids); err != nil {
		return exchanges.DepthBook{}, err
	}
	if book.Asks, err = parseKrakenDepthSide(symbol.Venue, resp.OrderBook.Asks); err != nil {
		return exchanges.DepthBook{}, err
	}
	return exchanges.FinishDepthBook(book)
}

func parseKrakenDepthSide(venueSymbol string, raw [][]float64) ([]exchanges.DepthLevel, error) {
	out := make([]exchanges.DepthLevel, 0, len(raw))
	for _, entry := range raw {
		if len(entry) < 2 {
			return nil, fmt.Errorf("kraken depth %s: level %v has no price/size pair", venueSymbol, entry)
		}
		out = append(out, exchanges.DepthLevel{PriceQuote: entry[0], QtyNative: entry[1]})
	}
	return out, nil
}
