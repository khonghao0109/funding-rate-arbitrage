package exchanges

import (
	"context"
	"fmt"
	"sort"
)

// Historical PRICE candles, added 2026-09-07 for the phase-3 basis exit.
//
// # Why this is not price_snapshots
//
// price_snapshots holds the top of book as the scanner saw it live: a mid, a
// best bid, a best ask, and the quantities resting on them. A candle holds none
// of that. It has an open, a high, a low and a close for a window, and no book
// at all. Writing a close into a column named best_bid_quote would put a
// different measurement under a name that promises a specific one, which is the
// failure CONVENTIONS §1 exists to prevent. So candles live in their own table,
// with their own names, and a reader can always tell which one they are holding.
//
// # Why it can be backfilled at all
//
// Depth cannot: the book a venue had an hour ago is gone. Candles can, because
// every venue keeps them — and that difference is the whole reason the basis
// exit is testable in hindsight while the slippage model is not. What a backtest
// gets from these is the perp-over-spot difference at a past instant, which is
// exactly what strategy.EvaluateExit's basis condition needs and has never had.
//
// # What a close is and is not
//
// A close is the last trade of the window, not a quote. It is not a mid, it has
// no spread, and two venues' closes for the same hour are two different trades
// at two different instants inside that hour. For a BASIS comparison that is
// enough — the question is whether perp and spot have come apart by percent, and
// an intra-hour timing difference moves that by far less than the limits the
// rule tests. It would not be enough to price a fill, and nothing here does.

const (
	// PriceCandleIntervalSec is the one cadence this project stores. Funding
	// settles hourly at the fastest (Hyperliquid, Kraken), so an hourly candle
	// gives every settlement on every venue a price at or before it, and a
	// finer grid would multiply the corpus for no decision it could change.
	PriceCandleIntervalSec = 3600

	// MaxPriceHistoryPages bounds a paginating loop. Same reasoning as
	// MaxFundingHistoryPages: a runaway guard, not a budget. A year of hourly
	// candles is 8,760 rows, and the smallest page any venue here serves is
	// OKX's 300, so 40 pages covers it with room for a venue that shrinks its
	// page size.
	MaxPriceHistoryPages = 400
)

// PriceCandle is one interval of one market's traded price, normalized.
//
// Like FundingHistoryEntry it carries NO RecvAt: a candle from last March has
// no meaningful receive time and inventing one would add a fourth stamping site
// (CLAUDE.md rule 13).
type PriceCandle struct {
	Symbol string // normalized: BTCUSDT
	Source string // wire id: binance_futures, ...

	// OpenTimeMs is the START of the interval, as the venue stamps it, stored
	// VERBATIM — the same rule the settled funding stamps follow. It is the
	// primary key together with source and symbol, so a re-fetch of the same
	// window updates rows rather than duplicating them.
	OpenTimeMs  int64
	IntervalSec int64

	OpenPriceQuote  float64
	HighPriceQuote  float64
	LowPriceQuote   float64
	ClosePriceQuote float64

	// BaseVolumeCoin is the traded volume in the BASE asset. 0 means NOT KNOWN
	// — the same convention as the top-of-book quantities — because Gate
	// publishes its futures volume in contracts and this package does not have
	// the registry needed to convert it.
	BaseVolumeCoin float64
}

// PriceCandleRow is one candle as a venue's own parser produces it, before the
// cross-venue tidying FinishPriceHistory does.
type PriceCandleRow struct {
	OpenTimeMs      int64
	IntervalSec     int64
	OpenPriceQuote  float64
	HighPriceQuote  float64
	LowPriceQuote   float64
	ClosePriceQuote float64
	BaseVolumeCoin  float64
}

// PriceWindow is the span a price backfill asks for.
type PriceWindow struct {
	StartMs int64 // inclusive
	EndMs   int64 // exclusive
}

// Contains reports whether a candle's open stamp falls inside the window.
func (w PriceWindow) Contains(stampMs int64) bool {
	return stampMs >= w.StartMs && stampMs < w.EndMs
}

// PriceHistoryFetchFunc reads the candles one source published for one symbol
// inside a window, OLDEST FIRST.
//
// It paginates internally for the same reason the funding fetchers do: every
// venue does it differently — measured 2026-09-07, page sizes are 1500
// (Binance futures), 1000 (Binance spot, Bybit), 300 (OKX), 2000 (Gate,
// Kraken) and ~5000 (Hyperliquid), three of them descending and three of them
// stamping in seconds — and pushing that into one caller would put six cursor
// idioms in one loop.
//
// It returns what EXISTS. A venue's retention limit is not an error: measured
// the same day, Hyperliquid answers an EMPTY ARRAY beyond about 208 days of
// hourly candles, exactly as OKX does beyond its funding retention. Read the
// coverage; never assume the span asked for is the span returned.
type PriceHistoryFetchFunc func(ctx context.Context, source string, symbol Symbol, window PriceWindow) ([]PriceCandle, error)

// FinishPriceHistory turns one venue's rows into candles: inside the window,
// sorted oldest first, and deduplicated on the open stamp.
//
// Dedup keeps the LAST row seen for a stamp. Pages overlap at their boundaries
// on several of these venues, and the later copy is the one fetched most
// recently — for a closed interval the two are identical, and for the interval
// still forming the newer one is the better measurement.
func FinishPriceHistory(source string, symbol Symbol, rows []PriceCandleRow) ([]PriceCandle, error) {
	if source == "" || symbol.Standard == "" {
		return nil, fmt.Errorf("exchanges: price history needs a source and a symbol, got %q/%q", source, symbol.Standard)
	}
	byStamp := make(map[int64]PriceCandleRow, len(rows))
	order := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.OpenTimeMs <= 0 || row.IntervalSec <= 0 {
			continue
		}
		if _, seen := byStamp[row.OpenTimeMs]; !seen {
			order = append(order, row.OpenTimeMs)
		}
		byStamp[row.OpenTimeMs] = row
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	out := make([]PriceCandle, 0, len(order))
	for _, stamp := range order {
		row := byStamp[stamp]
		// A candle with no close prices nothing. Refusing it here keeps the
		// zero out of the corpus, where a later reader would have to guess
		// whether 0 meant "free" or "not known" — the same distinction the
		// quantity columns carry a comment about.
		if row.ClosePriceQuote <= 0 {
			continue
		}
		out = append(out, PriceCandle{
			Symbol: symbol.Standard, Source: source,
			OpenTimeMs: row.OpenTimeMs, IntervalSec: row.IntervalSec,
			OpenPriceQuote: row.OpenPriceQuote, HighPriceQuote: row.HighPriceQuote,
			LowPriceQuote: row.LowPriceQuote, ClosePriceQuote: row.ClosePriceQuote,
			BaseVolumeCoin: row.BaseVolumeCoin,
		})
	}
	return out, nil
}
