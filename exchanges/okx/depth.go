package okx

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"fmt"
)

// OKX order book depth (step 2.7b).
//
//	GET /api/v5/market/books?instId=BTC-USDT-SWAP&sz=100
//	{"code":"0","msg":"","data":[{"asks":[["81123","39.72","0","17"],...],
//	  "bids":[["81122.9","12.05","0","9"],...],"ts":"1788500546408"}]}
//
// Measured 2026-09-04: sz=100 returns 100 levels a side, bids descending and
// asks ascending.
//
// ⚠️ A level is FOUR elements, not two: price, size, a deprecated field that is
// always "0", and the number of orders at that level. Reading it as a pair and
// taking entry[1] happens to work; taking the last element does not.
//
// ⚠️ Size is in CONTRACTS. BTC-USDT-SWAP has ctVal 0.01, so the raw number is a
// hundred times the coin quantity. internal/depth multiplies by the registry's
// ContractSizeCoin — nothing here does.
// https://www.okx.com/docs-v5/en/#order-book-trading-market-data-get-order-book
const okxDepthMaxLevels = 400

type okxDepthResponse struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []struct {
		Asks [][]string `json:"asks"`
		Bids [][]string `json:"bids"`
		TS   string     `json:"ts"`
	} `json:"data"`
}

// FetchDepth reads one swap book.
func FetchDepth(ctx context.Context, source string, symbol exchanges.Symbol, levels int) (exchanges.DepthBook, error) {
	// A hard ceiling: sz above 400 is refused, and even 400 only spans 0.077%
	// of mid on BTC-USDT-SWAP (measured 2026-09-04), which does not reach the
	// tight window. OKX depth figures are therefore lower bounds by nature and
	// the summary says so.
	levels = min(levels, okxDepthMaxLevels)
	url := fmt.Sprintf("https://www.okx.com/api/v5/market/books?instId=%s&sz=%d", symbol.Venue, levels)
	var resp okxDepthResponse
	if err := exchanges.FetchJSON(ctx, url, &resp); err != nil {
		return exchanges.DepthBook{}, err
	}
	return parseOKXDepth(resp, source, symbol)
}

func parseOKXDepth(resp okxDepthResponse, source string, symbol exchanges.Symbol) (exchanges.DepthBook, error) {
	// OKX wraps its errors in HTTP 200; the shared recognizer owns which code
	// means "not listed" (docs/CLAUDE.md "Not listed" trap); everything else
	// non-zero stays a loud failure.
	if okxCodeMeansNotListed(resp.Code) {
		return exchanges.DepthBook{}, fmt.Errorf("okx depth %s: %w", symbol.Venue, exchanges.ErrNotListed)
	}
	if resp.Code != "0" {
		return exchanges.DepthBook{}, fmt.Errorf("okx depth %s: code %s %s", symbol.Venue, resp.Code, resp.Msg)
	}
	if len(resp.Data) == 0 {
		return exchanges.DepthBook{}, fmt.Errorf("okx depth %s: code 0 with no book in data", symbol.Venue)
	}

	entry := resp.Data[0]
	// OKX books are quoted in contracts of ctVal×ctMult coin.
	book := exchanges.DepthBook{Symbol: symbol.Standard, Source: source, IsContractBook: true}
	if entry.TS != "" {
		// A stamp that does not parse is diagnostic data, not a reason to lose
		// the book: VenueTimeMs stays 0, which is its documented "not supplied".
		if ms, err := exchanges.ParseFloatField(source, symbol.Venue, "ts", entry.TS); err == nil {
			book.VenueTimeMs = int64(ms)
		}
	}
	var err error
	if book.Bids, err = exchanges.ParseDepthSide(source, symbol.Venue, entry.Bids); err != nil {
		return exchanges.DepthBook{}, err
	}
	if book.Asks, err = exchanges.ParseDepthSide(source, symbol.Venue, entry.Asks); err != nil {
		return exchanges.DepthBook{}, err
	}
	return exchanges.FinishDepthBook(book)
}
