package exchanges

import (
	"context"
	"fmt"
	"sort"
)

// Order book depth over REST (step 2.7b).
//
// Depth is fetched PERIODICALLY, not streamed. A funding position is held for
// days to weeks, so incremental `depth` — with its sequence numbers, gap
// detection and resync — is only justified while an order is actually being
// placed, which is phase 4.4. A snapshot every hour is what screening needs.
// See docs/PLAN.md §7.4.
//
// What every venue was measured to deliver, 2026-09-04 (probe output and the
// per-venue traps: docs/DATA-REQUIREMENTS.md §11):
//
//	Binance futures/spot  limit=100, bids DESC, asks ASC, sizes in COIN
//	Bybit linear/spot     limit=200, "b"/"a", sizes in COIN
//	OKX swap              sz=100, 4-element levels, sizes in CONTRACTS
//	Gate futures          limit=100, {"s":int,"p":string}, sizes in CONTRACTS
//	Kraken futures        NO limit parameter — the whole book, ~44 KB,
//	                      and its bids come back ASCENDING
//	Hyperliquid           levels[0]=bids levels[1]=asks, 20 per side, COIN
//	Paradex               depth=100, sizes in COIN, ask side often much shorter
//
// Quantities are NOT converted here. Three venues denominate their books in
// contracts, and the multiplier is a measured property of the instrument
// (exchanges.Instrument.ContractSizeCoin) held by internal/instruments — which
// this package must not import. internal/depth does the conversion, and refuses
// to publish a number when the registry cannot supply the multiplier: an
// unconverted Gate book reads as ten thousand times more liquidity than exists.

// DepthLevel is one price level exactly as the venue published it.
type DepthLevel struct {
	PriceQuote float64

	// QtyNative is the size in the venue's OWN order unit: COIN on Binance,
	// Bybit, Hyperliquid and Paradex, CONTRACTS on OKX, Gate and Kraken.
	//
	// It is deliberately not called QtyCoin. Naming it that here would be a
	// lie on three venues, and the lie is expensive: Gate's BTC contract is
	// 0.0001 BTC, so an unconverted book reports 10,000x the real size.
	// Multiply by the registry's ContractSizeCoin before this number is
	// compared with anything, summed, or shown.
	QtyNative float64
}

// DepthBook is one venue's order book snapshot for one pair.
//
// Bids are DESCENDING and asks ASCENDING — best price first on both sides —
// whatever order the venue used. Kraken is why that is stated rather than
// assumed: it returns bids ascending, so its first bid is a resting order at a
// price of 1, and reading it as the top of book would put a $1 bid at the front
// of every liquidity calculation.
type DepthBook struct {
	Symbol string // normalized: BTCUSDT
	Source string // wire id: binance_futures, ...

	Bids []DepthLevel
	Asks []DepthLevel

	// VenueTimeMs is the venue's own stamp for the snapshot, 0 when it
	// publishes none. Diagnostic only — the sample time that counts is stamped
	// by the caller, exactly as for price samples and funding history, because
	// venue clocks measure skew rather than age (CLAUDE.md rule 13).
	VenueTimeMs int64

	// IsContractBook says this venue denominates its book in CONTRACTS. It is
	// a static fact about the venue's API, stamped by the fetcher that read
	// the book — never inferred from a multiplier's value, because Kraken's
	// PF_ books are contract-denominated with a multiplier of exactly 1 and
	// the inference labeled them coin. It also scopes the registry dependency:
	// only a book that says true needs a multiplier at all, so a registry gap
	// cannot blank a coin book it was never needed for.
	IsContractBook bool
}

// DepthFetchFunc fetches one book. levels is the number of price levels asked
// for per side; a venue that caps lower returns what it has, and a venue with
// no limit parameter at all ignores it.
type DepthFetchFunc func(ctx context.Context, source string, symbol Symbol, levels int) (DepthBook, error)

// FinishDepthBook puts a parsed book into the canonical order and drops levels
// that cannot mean anything.
//
// A level at price 0 or size 0 is not liquidity, and a venue that pads its book
// with them would otherwise contribute rows to a depth sum that add nothing but
// a level count. Sorting is unconditional rather than trusting the venue's
// documented order: Kraken's ascending bids were found by probing, not by
// reading, and the next venue to change its mind will not announce it.
func FinishDepthBook(book DepthBook) (DepthBook, error) {
	book.Bids = usableDepthLevels(book.Bids)
	book.Asks = usableDepthLevels(book.Asks)

	sort.Slice(book.Bids, func(i, j int) bool { return book.Bids[i].PriceQuote > book.Bids[j].PriceQuote })
	sort.Slice(book.Asks, func(i, j int) bool { return book.Asks[i].PriceQuote < book.Asks[j].PriceQuote })

	// One empty side is not a usable book: every figure downstream is measured
	// from the mid price, and a mid needs both.
	if len(book.Bids) == 0 || len(book.Asks) == 0 {
		return DepthBook{}, fmt.Errorf("depth %s %s: %d bids and %d asks, need both sides",
			book.Source, book.Symbol, len(book.Bids), len(book.Asks))
	}
	// A crossed book means the snapshot is inconsistent - two halves read at
	// different instants, or a venue bug. Publishing it would produce a
	// negative spread and a mid price between two prices that never coexisted.
	if book.Bids[0].PriceQuote >= book.Asks[0].PriceQuote {
		return DepthBook{}, fmt.Errorf("depth %s %s: book is crossed, best bid %g >= best ask %g",
			book.Source, book.Symbol, book.Bids[0].PriceQuote, book.Asks[0].PriceQuote)
	}
	return book, nil
}

func usableDepthLevels(levels []DepthLevel) []DepthLevel {
	out := levels[:0]
	for _, level := range levels {
		if level.PriceQuote > 0 && level.QtyNative > 0 {
			out = append(out, level)
		}
	}
	return out
}

// ParseDepthLevel reads the [price, size] string pair five of the nine venues
// use, so the same two error messages are not written five times.
func ParseDepthLevel(source, symbol, price, size string) (DepthLevel, error) {
	priceQuote, err := ParseFloatField(source, symbol, "price", price)
	if err != nil {
		return DepthLevel{}, err
	}
	qty, err := ParseFloatField(source, symbol, "size", size)
	if err != nil {
		return DepthLevel{}, err
	}
	return DepthLevel{PriceQuote: priceQuote, QtyNative: qty}, nil
}

// ParseDepthSide reads the [["price","size"], ...] shape. Binance, Bybit and
// Paradex all publish it; the differences between those venues are in the
// envelope around it, not in the level.
func ParseDepthSide(source, venueSymbol string, raw [][]string) ([]DepthLevel, error) {
	out := make([]DepthLevel, 0, len(raw))
	for _, entry := range raw {
		if len(entry) < 2 {
			return nil, fmt.Errorf("depth %s %s: level %v has no price/size pair", source, venueSymbol, entry)
		}
		level, err := ParseDepthLevel(source, venueSymbol, entry[0], entry[1])
		if err != nil {
			return nil, err
		}
		out = append(out, level)
	}
	return out, nil
}
