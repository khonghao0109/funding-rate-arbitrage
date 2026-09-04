package exchanges

import (
	"context"
	"fmt"
	"strconv"
)

// Gate futures order book depth (step 2.7b).
//
//	GET /api/v4/futures/usdt/order_book?contract=BTC_USDT&limit=100
//	{"current":1788496621.385,"update":1788496621.383,
//	 "asks":[{"s":11552,"p":"81123.8"},...],"bids":[{"s":13614,"p":"81108.3"},...]}
//
// Measured 2026-09-04: limit=100 returns 100 levels a side, bids descending and
// asks ascending.
//
// ⚠️ The level is an OBJECT, not an array, and its two fields have different
// types: `s` is a JSON NUMBER (contracts, integral) while `p` is a STRING.
// Declaring `s` as a string makes the whole response fail to decode — the same
// shape of trap as Bybit's fundingIntervalHour.
//
// ⚠️ `s` is in CONTRACTS. BTC_USDT has quanto_multiplier 0.0001, so 13,614
// there is 1.3614 BTC — a factor of ten thousand. internal/depth converts.
//
// `current` is the response time in SECONDS with a fractional part, not
// milliseconds like every other venue here.
// https://www.gate.com/docs/developers/apiv4/#futures-order-book
const gateDepthSettle = "usdt"

// gateDepthMaxLevels is a HARD ceiling, found by probing 2026-09-04: 300 is
// answered in full and 400 comes back HTTP 400. Asking beyond it loses the
// whole book rather than returning a shorter one, which is why this clamps
// rather than passing the caller's number through. 300 levels span 0.175% of
// mid on BTC_USDT, against 0.077% at 100.
const gateDepthMaxLevels = 300

type gateDepthResponse struct {
	CurrentSec float64 `json:"current"`
	Asks       []struct {
		SizeContracts float64 `json:"s"`
		Price         string  `json:"p"`
	} `json:"asks"`
	Bids []struct {
		SizeContracts float64 `json:"s"`
		Price         string  `json:"p"`
	} `json:"bids"`
}

// FetchGateDepth reads one futures book.
func FetchGateDepth(ctx context.Context, source string, symbol Symbol, levels int) (DepthBook, error) {
	levels = min(levels, gateDepthMaxLevels)
	url := fmt.Sprintf("https://api.gateio.ws/api/v4/futures/%s/order_book?contract=%s&limit=%d",
		gateDepthSettle, symbol.Venue, levels)
	var resp gateDepthResponse
	if err := fetchInstrumentJSON(ctx, url, &resp); err != nil {
		return DepthBook{}, err
	}
	return parseGateDepth(resp, source, symbol)
}

func parseGateDepth(resp gateDepthResponse, source string, symbol Symbol) (DepthBook, error) {
	book := DepthBook{
		Symbol: symbol.Standard,
		Source: source,
		// Seconds with a fraction, to milliseconds. 0 stays 0.
		VenueTimeMs: int64(resp.CurrentSec * msPerSecond),
		// Gate books are quoted in whole contracts of quanto_multiplier coin.
		IsContractBook: true,
	}
	for _, level := range resp.Bids {
		parsed, err := parseGateDepthLevel(source, symbol.Venue, level.Price, level.SizeContracts)
		if err != nil {
			return DepthBook{}, err
		}
		book.Bids = append(book.Bids, parsed)
	}
	for _, level := range resp.Asks {
		parsed, err := parseGateDepthLevel(source, symbol.Venue, level.Price, level.SizeContracts)
		if err != nil {
			return DepthBook{}, err
		}
		book.Asks = append(book.Asks, parsed)
	}
	return finishDepthBook(book)
}

func parseGateDepthLevel(source, venueSymbol, price string, sizeContracts float64) (DepthLevel, error) {
	priceQuote, err := strconv.ParseFloat(price, 64)
	if err != nil {
		return DepthLevel{}, fmt.Errorf("gate depth %s: price %q does not parse", venueSymbol, price)
	}
	return DepthLevel{PriceQuote: priceQuote, QtyNative: sizeContracts}, nil
}
