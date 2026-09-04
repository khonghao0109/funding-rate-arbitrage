package depth

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
)

// coinBook is a book already denominated in coin, with a mid of 100.
func coinBook() exchanges.DepthBook {
	return exchanges.DepthBook{
		Symbol: "BTCUSDT", Source: "binance_futures",
		Bids: []exchanges.DepthLevel{
			{PriceQuote: 100, QtyNative: 1},   // inside 0.1%
			{PriceQuote: 99.95, QtyNative: 2}, // inside 0.1% (0.05% away)
			{PriceQuote: 99.8, QtyNative: 4},  // inside 0.5% only
			{PriceQuote: 99, QtyNative: 8},    // outside both
		},
		Asks: []exchanges.DepthLevel{
			{PriceQuote: 100, QtyNative: 3},
			{PriceQuote: 100.4, QtyNative: 5}, // inside 0.5% only
			{PriceQuote: 101, QtyNative: 7},   // outside both
		},
	}
}

func alwaysCoin(string, string) (float64, bool) { return 1, true }

func TestSummarize_MeasuresEachWindowFromTheMid(t *testing.T) {
	// Mid is exactly 100, so 0.1% is [99.9, 100.1] and 0.5% is [99.5, 100.5].
	// Measuring each side from its own top of book instead would make a venue
	// with a wide spread look deep on both sides.
	summary := Summarize(coinBook(), 1000, alwaysCoin)

	if !summary.OK() {
		t.Fatalf("summary not ok: %s", summary.ErrVI)
	}
	if summary.MidPriceQuote != 100 {
		t.Fatalf("mid = %g, want 100", summary.MidPriceQuote)
	}
	// Bids inside 0.1%: 100×1 + 99.95×2 = 299.90
	if math.Abs(summary.BidDepthWithinTightQuote-299.90) > 1e-9 {
		t.Errorf("bid depth 0.1%% = %g, want 299.90", summary.BidDepthWithinTightQuote)
	}
	// Bids inside 0.5% adds 99.8×4 = 399.2 → 699.10
	if math.Abs(summary.BidDepthWithinWideQuote-699.10) > 1e-9 {
		t.Errorf("bid depth 0.5%% = %g, want 699.10", summary.BidDepthWithinWideQuote)
	}
	// Asks inside 0.1%: 100×3 = 300
	if math.Abs(summary.AskDepthWithinTightQuote-300) > 1e-9 {
		t.Errorf("ask depth 0.1%% = %g, want 300", summary.AskDepthWithinTightQuote)
	}
	// Asks inside 0.5% adds 100.4×5 = 502 → 802
	if math.Abs(summary.AskDepthWithinWideQuote-802) > 1e-9 {
		t.Errorf("ask depth 0.5%% = %g, want 802", summary.AskDepthWithinWideQuote)
	}
}

// The whole reason exchanges/ does not convert: an unconverted Gate book
// reports ten thousand times the liquidity that exists.
func TestSummarize_ConvertsContractsToCoin(t *testing.T) {
	book := exchanges.DepthBook{
		Symbol: "BTCUSDT", Source: "gate_futures", IsContractBook: true,
		Bids: []exchanges.DepthLevel{{PriceQuote: 100, QtyNative: 10000}}, // 10,000 contracts
		Asks: []exchanges.DepthLevel{{PriceQuote: 100.01, QtyNative: 10000}},
	}
	// Gate's BTC contract is 0.0001 BTC, so 10,000 contracts is 1 BTC.
	gate := func(string, string) (float64, bool) { return 0.0001, true }

	summary := Summarize(book, 1000, gate)
	if !summary.IsContractBook {
		t.Error("a converted book must say so")
	}
	if math.Abs(summary.BestBidQtyCoin-1) > 1e-9 {
		t.Errorf("best bid qty = %g coin, want 1", summary.BestBidQtyCoin)
	}
	if math.Abs(summary.BidDepthWithinTightQuote-100) > 1e-6 {
		t.Errorf("bid depth = %g, want 100 (1 BTC at 100)", summary.BidDepthWithinTightQuote)
	}
}

// The most important refusal in this package.
func TestSummarize_RefusesToGuessAnUnknownMultiplier(t *testing.T) {
	unknown := func(string, string) (float64, bool) { return 0, false }

	book := coinBook()
	book.Source = "gate_futures"
	book.IsContractBook = true
	summary := Summarize(book, 1000, unknown)
	if summary.OK() {
		t.Fatal("a contract book with no known multiplier produced figures")
	}
	if summary.ErrVI == "" {
		t.Error("the refusal must say why; a blank cell is indistinguishable from an empty book")
	}
	if summary.BidDepthWithinWideQuote != 0 || summary.BestBidQtyCoin != 0 {
		t.Errorf("figures were published anyway: %+v", summary)
	}
	// The identity survives, so the dashboard can still show the row and say
	// what happened to it.
	if summary.Source == "" || summary.Symbol == "" {
		t.Error("the refusal lost the identity of the series")
	}
}

// A multiplier of exactly 1 is a coin book, not a converted one, and saying
// otherwise would put a "converted" marker on Binance.
func TestSummarize_ACoinBookIsNotMarkedAsContracts(t *testing.T) {
	if Summarize(coinBook(), 1000, alwaysCoin).IsContractBook {
		t.Error("a multiplier of 1 was reported as a contract book")
	}
}

// The reverse of the test above, and why the label comes from the fetcher's
// declaration rather than the multiplier's value: Kraken's PF_ books ARE
// contract-denominated with a multiplier of exactly 1, and the old
// `sizeCoin != 1` inference labeled them coin on the wire and in the store —
// a phase-3 reader auditing which figures depend on the registry got a false
// negative for the whole venue.
func TestSummarize_AContractBookWithMultiplierOneIsStillAContractBook(t *testing.T) {
	book := coinBook()
	book.Source = "kraken_futures"
	book.IsContractBook = true
	summary := Summarize(book, 1000, alwaysCoin)
	if !summary.OK() {
		t.Fatalf("summary not ok: %s", summary.ErrVI)
	}
	if !summary.IsContractBook {
		t.Error("a contract book with multiplier 1 was labeled a coin book")
	}
}

// A registry gap must not blank measurements it was never needed for: six of
// nine venues quote coin, and their books convert with no multiplier at all.
// Before the fetcher-declared flag, a persistent instrument-fetch failure on
// binance_spot blanked binance_spot's depth for the whole outage, blaming a
// contract conversion that does not exist on that venue.
func TestSummarize_ACoinBookNeedsNoRegistry(t *testing.T) {
	unknown := func(string, string) (float64, bool) { return 0, false }

	summary := Summarize(coinBook(), 1000, unknown)
	if !summary.OK() {
		t.Fatalf("a coin book was refused over a missing multiplier: %s", summary.ErrVI)
	}
	if summary.IsContractBook {
		t.Error("a coin book was labeled a contract book")
	}
	if summary.BestBidQtyCoin != 1 {
		t.Errorf("best bid qty = %g coin, want the native 1 unconverted", summary.BestBidQtyCoin)
	}
}

func TestSummarize_ReportsHowFarTheBookReaches(t *testing.T) {
	summary := Summarize(coinBook(), 1000, alwaysCoin)

	// Bids reach 99 from a mid of 100 → 1%. Asks reach 101 → 1%.
	if math.Abs(summary.BidSpanPct-1) > 1e-9 || math.Abs(summary.AskSpanPct-1) > 1e-9 {
		t.Errorf("spans = %g/%g, want 1%% each", summary.BidSpanPct, summary.AskSpanPct)
	}
	if !summary.CoversTight() || !summary.CoversWide() {
		t.Error("a book reaching 1% covers both windows")
	}
}

// Hyperliquid's twenty levels span 0.025% on BTC — narrower than both windows.
// Its depth figure is a floor, and a dashboard that did not know would show the
// venue as thin rather than as truncated.
func TestSummarize_ATruncatedBookIsFlaggedNotCalledThin(t *testing.T) {
	book := exchanges.DepthBook{
		Symbol: "BTCUSDT", Source: "hyperliquid_futures",
		Bids: []exchanges.DepthLevel{{PriceQuote: 100, QtyNative: 1}, {PriceQuote: 99.99, QtyNative: 1}},
		Asks: []exchanges.DepthLevel{{PriceQuote: 100.01, QtyNative: 1}, {PriceQuote: 100.02, QtyNative: 1}},
	}
	summary := Summarize(book, 1000, alwaysCoin)

	if summary.CoversTight() || summary.CoversWide() {
		t.Errorf("a book spanning %g%%/%g%% must not claim to cover 0.1%% or 0.5%%",
			summary.BidSpanPct, summary.AskSpanPct)
	}
	if summary.BidDepthWithinWideQuote <= 0 {
		t.Error("the figure is still published as a lower bound, not suppressed")
	}
}

func TestSummarize_SpreadIsMeasuredFromTheMid(t *testing.T) {
	summary := Summarize(coinBook(), 1000, alwaysCoin)
	// Best bid 100, best ask 100 in the fixture's first levels → 0 spread.
	if summary.SpreadPct != 0 {
		t.Errorf("spread = %g%%, want 0 for a fixture whose tops are equal", summary.SpreadPct)
	}
}

func TestCollector_DropsSourcesWithNoFetcher(t *testing.T) {
	collector := newCollector(
		[]Job{
			{Source: "binance_futures", Connector: "binance_futures"},
			{Source: "pyth", Connector: "pyth"}, // an oracle has no book
		},
		map[string]exchanges.DepthFetchFunc{"binance_futures": nil},
		alwaysCoin, 100,
	)
	if len(collector.Jobs()) != 1 || collector.Jobs()[0].Source != "binance_futures" {
		t.Fatalf("jobs = %+v, want only binance_futures", collector.Jobs())
	}
}

// A venue that cannot be read has to reach the dashboard as a named failure.
// Dropping the row would say "this venue has no liquidity", which is a claim
// about the market rather than about the fetch.
func TestCollectOnce_AFailedFetchBecomesANamedRow(t *testing.T) {
	fetchers := map[string]exchanges.DepthFetchFunc{
		"ok": func(context.Context, string, exchanges.Symbol, int) (exchanges.DepthBook, error) {
			return exchanges.DepthBook{
				Symbol: "BTCUSDT", Source: "ok",
				Bids: []exchanges.DepthLevel{{PriceQuote: 100, QtyNative: 1}},
				Asks: []exchanges.DepthLevel{{PriceQuote: 101, QtyNative: 1}},
			}, nil
		},
		"broken": func(context.Context, string, exchanges.Symbol, int) (exchanges.DepthBook, error) {
			return exchanges.DepthBook{}, errors.New("HTTP 503")
		},
	}
	collector := newCollector([]Job{
		{Source: "ok", Connector: "ok", Symbols: []exchanges.Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}}},
		{Source: "broken", Connector: "broken", Symbols: []exchanges.Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}}},
	}, fetchers, alwaysCoin, 100)
	collector.now = func() time.Time { return time.UnixMilli(1788500000000) }

	summaries := collector.CollectOnce(context.Background())
	if len(summaries) != 2 {
		t.Fatalf("got %d summaries, want one per series including the failure", len(summaries))
	}

	byName := map[string]Summary{}
	for _, summary := range summaries {
		byName[summary.Source] = summary
		// One stamp for the whole round, so rows from one sweep compare as one
		// instant.
		if summary.SampledAtMs != 1788500000000 {
			t.Errorf("%s sampled at %d, want the round's single stamp", summary.Source, summary.SampledAtMs)
		}
	}
	if !byName["ok"].OK() {
		t.Errorf("the healthy source failed: %s", byName["ok"].ErrVI)
	}
	if byName["broken"].OK() || !strings.Contains(byName["broken"].ErrVI, "503") {
		t.Errorf("the failure lost its reason: %+v", byName["broken"])
	}
}

func TestCollectOnce_StopsWhenCancelled(t *testing.T) {
	calls := 0
	fetchers := map[string]exchanges.DepthFetchFunc{
		"slow": func(context.Context, string, exchanges.Symbol, int) (exchanges.DepthBook, error) {
			calls++
			return exchanges.DepthBook{}, errors.New("never mind")
		},
	}
	collector := newCollector([]Job{{
		Source: "slow", Connector: "slow",
		Symbols: []exchanges.Symbol{
			{Standard: "BTCUSDT", Venue: "BTCUSDT"},
			{Standard: "ETHUSDT", Venue: "ETHUSDT"},
			{Standard: "SOLUSDT", Venue: "SOLUSDT"},
		},
	}}, fetchers, alwaysCoin, 100)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	collector.CollectOnce(ctx)

	if calls > 1 {
		t.Errorf("%d fetches after cancellation; shutdown must not wait out a whole sweep", calls)
	}
}
