package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
)

// The order book of the TESTNET this client sends to.
//
// # Why this is not exchanges/binance's depth fetcher
//
// The same reason rules.go is not its instrument fetcher: that one reads public
// MAINNET data for the scanner, and this reads a different venue instance whose
// prices are its own. Testnet BTCUSDT does not track mainnet BTCUSDT — it is a
// separate matching engine with separate participants — so sizing an order
// against a mainnet book and sending it to testnet prices the trade off the
// wrong market entirely.
//
// The book is a REST SNAPSHOT, which is CLAUDE.md rule 10's rule and not an
// oversight: incremental depth is justified only while an order is being
// placed, and whether this snapshot is good enough AT THIS SIZE is a
// measurement PLAN 4.4b takes rather than an assumption either way.

// depthLimit is fixed at 100 so the endpoint's weight is the exact documented
// figure rather than a range. Both venues charge 5 there.
const depthLimit = 100

// FetchDepthBook reads one symbol's book from this market, in the repo's
// normalized shape.
//
// IsContractBook is false: Binance denominates both of these books in COIN
// (DATA-REQUIREMENTS §11, and CLAUDE.md's contracts row lists the three venues
// that do not — Binance is not among them). Declaring it rather than inferring
// it is the fetcher's job, because internal/depth refuses to publish a
// contract-denominated book it cannot convert.
func (c *Client) FetchDepthBook(ctx context.Context, symbol string) (exchanges.DepthBook, error) {
	ep := broker.FuturesDepth
	source := "binance_futures"
	if c.market == broker.MarketSpot {
		ep, source = broker.SpotDepth, "binance_spot"
	}
	var raw struct {
		LastUpdateID int64           `json:"lastUpdateId"`
		EventTimeMs  int64           `json:"E"`
		TxTimeMs     int64           `json:"T"`
		Bids         [][]json.Number `json:"bids"`
		Asks         [][]json.Number `json:"asks"`
	}
	if err := c.http.GetPublic(ctx, ep, []broker.Param{
		{Key: "symbol", Value: symbol},
		{Key: "limit", Value: strconv.Itoa(depthLimit)},
	}, &raw); err != nil {
		return exchanges.DepthBook{}, classify(err)
	}

	book := exchanges.DepthBook{
		Symbol: symbol, Source: source,
		VenueTimeMs:    raw.TxTimeMs,
		Bids:           levels(raw.Bids),
		Asks:           levels(raw.Asks),
		IsContractBook: false,
	}
	finished, err := exchanges.FinishDepthBook(book)
	if err != nil {
		return exchanges.DepthBook{}, fmt.Errorf("binance %s: %w", source, err)
	}
	return finished, nil
}

// levels turns the venue's [price, quantity] string pairs into levels.
//
// A pair that is not two elements is skipped rather than guessed at: a
// three-element level would mean the venue changed its shape, and inventing a
// meaning for the third element is what rule 5 forbids.
func levels(raw [][]json.Number) []exchanges.DepthLevel {
	out := make([]exchanges.DepthLevel, 0, len(raw))
	for _, lv := range raw {
		if len(lv) < 2 {
			continue
		}
		price, err1 := strconv.ParseFloat(lv[0].String(), 64)
		qty, err2 := strconv.ParseFloat(lv[1].String(), 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, exchanges.DepthLevel{PriceQuote: price, QtyNative: qty})
	}
	return out
}
