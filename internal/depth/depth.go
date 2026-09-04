// Package depth turns venue order books into the liquidity figures that rank an
// opportunity (step 2.7b).
//
// It answers one question: if this position had to be opened and closed, how
// much is actually resting near the price the model assumed? Without it a
// screener sorted by funding rate puts a 200% APR pair with a $2,000 book above
// a 15% pair with a $500,000 book — not missing information, but information
// pointing the wrong way (docs/PLAN.md §7.4).
//
// Layering: this package imports `exchanges` and nothing else under internal/.
// The contract→coin multiplier arrives as a function rather than a registry
// handle, so the venue-rules dependency stays in the entrypoint and both the
// wire layer and the store layer can use these types without inheriting it.
package depth

import (
	"context"
	"fmt"
	"sort"
	"time"

	"futures-arbitrage-scanner/exchanges"
)

// The windows depth is measured over, as a percentage away from the mid price.
//
// They are CONSTANTS, not configuration, because the store's column names carry
// them (`bid_depth_within_0_1pct_quote`). A configurable window would let a
// YAML edit silently redefine what a stored column means, and the phase-3
// backtest reads those columns months later with no way to know.
//
// Tight is the fill window a maker-ish entry can hope for; wide is closer to
// what an exit under stress actually pays. Both are measured from the MID, not
// from the best bid — measuring each side from its own top of book would make a
// venue with a wide spread look deep on both sides.
const (
	WindowTightPct = 0.1
	WindowWidePct  = 0.5
)

const pctPerUnit = 100

// Summary is one venue's book for one pair, reduced to what ranking needs.
//
// Every quantity is in COIN and every depth figure is in the venue's QUOTE
// asset. Quote rather than USD on purpose: Kraken, Hyperliquid and Paradex
// quote USD while the rest quote USDT, and adding them together would be the
// same silent currency mix docs/WS-CONTRACT.md §5.2 refuses elsewhere.
type Summary struct {
	Source string
	Symbol string

	// SampledAtMs is when this snapshot was taken by US — one stamp per
	// collection round, exactly like the step-2.6 price sampler. VenueTimeMs is
	// the venue's own clock and is diagnostic only (CLAUDE.md rule 13).
	SampledAtMs int64
	VenueTimeMs int64

	MidPriceQuote  float64
	BestBidQuote   float64
	BestAskQuote   float64
	BestBidQtyCoin float64
	BestAskQtyCoin float64
	SpreadPct      float64

	BidDepthWithinTightQuote float64
	AskDepthWithinTightQuote float64
	BidDepthWithinWideQuote  float64
	AskDepthWithinWideQuote  float64

	BidLevels int
	AskLevels int

	// BidSpanPct and AskSpanPct are how far from the mid the FARTHEST level the
	// venue returned sits. When a span is smaller than a window, the depth
	// figure for that window is a LOWER BOUND — the venue simply did not
	// publish anything further out. Hyperliquid returns 20 levels spanning
	// 0.025% on BTC, which is narrower than both windows, and a dashboard that
	// did not say so would show the venue as thin rather than as truncated.
	BidSpanPct float64
	AskSpanPct float64

	// IsContractBook records that the venue quoted this book in contracts and
	// the quantities above are the result of a conversion. It is not cosmetic:
	// it says which numbers depend on the instrument registry being right.
	IsContractBook bool

	// ErrVI is set when no book could be produced. The summary is still
	// published, because a venue that disappears from the table reads as a
	// venue with no liquidity rather than one that could not be reached.
	ErrVI string
}

// OK reports whether this summary carries a usable book.
func (s Summary) OK() bool { return s.ErrVI == "" && s.MidPriceQuote > 0 }

// CoversTight and CoversWide say whether the venue published levels far enough
// out for the window's figure to be complete rather than a floor.
func (s Summary) CoversTight() bool {
	return s.BidSpanPct >= WindowTightPct && s.AskSpanPct >= WindowTightPct
}

func (s Summary) CoversWide() bool {
	return s.BidSpanPct >= WindowWidePct && s.AskSpanPct >= WindowWidePct
}

// ContractSizeFn reports how many base coins one contract represents on this
// market, and whether the caller knows at all.
//
// ok=false must NOT be read as 1. Three venues denominate their books in
// contracts (Gate's BTC contract is 0.0001 BTC), so assuming a multiplier of
// one where none is known reports ten thousand times the real liquidity — the
// exact failure that makes a thin market look like the deepest venue in the
// table. Summarize refuses instead.
type ContractSizeFn func(symbol, source string) (contractSizeCoin float64, ok bool)

// Summarize converts one raw book to coin and measures it.
func Summarize(book exchanges.DepthBook, sampledAtMs int64, contractSize ContractSizeFn) Summary {
	summary := Summary{
		Source:      book.Source,
		Symbol:      book.Symbol,
		SampledAtMs: sampledAtMs,
		VenueTimeMs: book.VenueTimeMs,
	}

	sizeCoin, ok := contractSize(book.Symbol, book.Source)
	if !ok || sizeCoin <= 0 {
		summary.ErrVI = "Chưa biết quy đổi contract→coin cho market này (instrument registry chưa có), " +
			"nên không công bố số thanh khoản — số chưa quy đổi sai tới hàng nghìn lần."
		return summary
	}
	summary.IsContractBook = sizeCoin != 1

	if len(book.Bids) == 0 || len(book.Asks) == 0 {
		summary.ErrVI = "Sổ lệnh trả về thiếu một phía."
		return summary
	}

	summary.BestBidQuote = book.Bids[0].PriceQuote
	summary.BestAskQuote = book.Asks[0].PriceQuote
	summary.BestBidQtyCoin = book.Bids[0].QtyNative * sizeCoin
	summary.BestAskQtyCoin = book.Asks[0].QtyNative * sizeCoin
	summary.MidPriceQuote = (summary.BestBidQuote + summary.BestAskQuote) / 2
	summary.SpreadPct = (summary.BestAskQuote - summary.BestBidQuote) / summary.MidPriceQuote * pctPerUnit

	summary.BidLevels = len(book.Bids)
	summary.AskLevels = len(book.Asks)
	summary.BidSpanPct = (summary.MidPriceQuote - book.Bids[len(book.Bids)-1].PriceQuote) / summary.MidPriceQuote * pctPerUnit
	summary.AskSpanPct = (book.Asks[len(book.Asks)-1].PriceQuote - summary.MidPriceQuote) / summary.MidPriceQuote * pctPerUnit

	summary.BidDepthWithinTightQuote = sumBids(book.Bids, sizeCoin, summary.MidPriceQuote, WindowTightPct)
	summary.BidDepthWithinWideQuote = sumBids(book.Bids, sizeCoin, summary.MidPriceQuote, WindowWidePct)
	summary.AskDepthWithinTightQuote = sumAsks(book.Asks, sizeCoin, summary.MidPriceQuote, WindowTightPct)
	summary.AskDepthWithinWideQuote = sumAsks(book.Asks, sizeCoin, summary.MidPriceQuote, WindowWidePct)
	return summary
}

// sumBids adds the notional resting at or above mid × (1 − window).
//
// Notional, not coin: the question is how much money can be taken out of the
// book, and a coin count answers it only after being multiplied by a price that
// differs level by level.
func sumBids(levels []exchanges.DepthLevel, sizeCoin, mid, windowPct float64) float64 {
	floor := mid * (1 - windowPct/pctPerUnit)
	var total float64
	for _, level := range levels {
		if level.PriceQuote < floor {
			break // levels are sorted best-first, so nothing further qualifies
		}
		total += level.PriceQuote * level.QtyNative * sizeCoin
	}
	return total
}

func sumAsks(levels []exchanges.DepthLevel, sizeCoin, mid, windowPct float64) float64 {
	ceiling := mid * (1 + windowPct/pctPerUnit)
	var total float64
	for _, level := range levels {
		if level.PriceQuote > ceiling {
			break
		}
		total += level.PriceQuote * level.QtyNative * sizeCoin
	}
	return total
}

// Job is one source and the pairs to sample on it.
type Job struct {
	Source    string
	Connector string
	Symbols   []exchanges.Symbol
}

// Collector fetches books on a schedule and reduces them to summaries.
type Collector struct {
	jobs         []Job
	fetchers     map[string]exchanges.DepthFetchFunc
	contractSize ContractSizeFn
	levels       int
	now          func() time.Time
}

// New builds a collector. Jobs whose connector has no depth fetcher are dropped
// here, once, with a line saying so — an oracle has no book, and silently
// skipping it would look identical to a venue that answered with nothing.
func New(jobs []Job, contractSize ContractSizeFn, levels int) *Collector {
	return newCollector(jobs, exchanges.DepthFetchers(), contractSize, levels)
}

// newCollector is New with the fetcher table supplied, so the tests can run the
// whole collection path without opening a socket.
func newCollector(jobs []Job, fetchers map[string]exchanges.DepthFetchFunc, contractSize ContractSizeFn, levels int) *Collector {
	kept := make([]Job, 0, len(jobs))
	for _, job := range jobs {
		if _, ok := fetchers[job.Connector]; !ok {
			continue
		}
		kept = append(kept, job)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Source < kept[j].Source })
	return &Collector{jobs: kept, fetchers: fetchers, contractSize: contractSize, levels: levels, now: time.Now}
}

// Jobs is the list actually collectable.
func (c *Collector) Jobs() []Job { return c.jobs }

// depthFetchPause spaces requests inside one round.
//
// A round is 36 requests across nine venues and runs at most once an hour, so
// this is politeness rather than a budget: it keeps a burst of nine simultaneous
// connections off the same venues the live feeds are using.
const depthFetchPause = 150 * time.Millisecond

// CollectOnce samples every job once and returns one summary per series.
//
// A failed fetch produces a summary carrying ErrVI rather than no summary at
// all: the dashboard has to be able to say "this venue could not be read",
// which is a different statement from "this venue is illiquid".
func (c *Collector) CollectOnce(ctx context.Context) []Summary {
	// ONE stamp for the whole round, like the step-2.6 price sampler: rows that
	// belong to the same sweep should compare as the same instant, and stamping
	// per fetch would spread a round across the seconds it took to run.
	sampledAtMs := c.now().UnixMilli()

	var out []Summary
	for _, job := range c.jobs {
		fetch := c.fetchers[job.Connector]
		for _, symbol := range job.Symbols {
			if ctx.Err() != nil {
				return out
			}
			book, err := fetch(ctx, job.Source, symbol, c.levels)
			if err != nil {
				out = append(out, Summary{
					Source: job.Source, Symbol: symbol.Standard, SampledAtMs: sampledAtMs,
					ErrVI: fmt.Sprintf("Không đọc được sổ lệnh: %v", err),
				})
			} else {
				out = append(out, Summarize(book, sampledAtMs, c.contractSize))
			}
			select {
			case <-ctx.Done():
				return out
			case <-time.After(depthFetchPause):
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Source < out[j].Source
	})
	return out
}
