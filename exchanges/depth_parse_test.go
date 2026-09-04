package exchanges

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// Golden tests for the depth parsers (step 2.7b), against the real books
// recorded 2026-09-04 into testdata/depth_*.json.
//
// Re-record with:
//
//	CAPTURE_TESTDATA=1 go test -run TestCaptureDepthTestdata ./exchanges/
//
// What they are for: nine sources publish six different shapes, and three of
// those can be parsed wrongly while still producing plausible numbers. A book
// read with its bids in the wrong order, or with a four-element level read as a
// pair, does not look broken — it looks like a market.

func loadDepthFixture(t *testing.T, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v (record it with %s=1)", err, captureEnv)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
	}
}

// assertBookSane is what every venue's book must satisfy after parsing,
// whatever order or shape it arrived in.
func assertBookSane(t *testing.T, book DepthBook, wantSource, wantSymbol string) {
	t.Helper()
	if book.Source != wantSource || book.Symbol != wantSymbol {
		t.Errorf("identity = %s/%s, want %s/%s", book.Source, book.Symbol, wantSource, wantSymbol)
	}
	if len(book.Bids) == 0 || len(book.Asks) == 0 {
		t.Fatalf("book has %d bids and %d asks", len(book.Bids), len(book.Asks))
	}
	for i := 1; i < len(book.Bids); i++ {
		if book.Bids[i].PriceQuote > book.Bids[i-1].PriceQuote {
			t.Fatalf("bids are not descending at %d: %g then %g",
				i, book.Bids[i-1].PriceQuote, book.Bids[i].PriceQuote)
		}
	}
	for i := 1; i < len(book.Asks); i++ {
		if book.Asks[i].PriceQuote < book.Asks[i-1].PriceQuote {
			t.Fatalf("asks are not ascending at %d: %g then %g",
				i, book.Asks[i-1].PriceQuote, book.Asks[i].PriceQuote)
		}
	}
	if book.Bids[0].PriceQuote >= book.Asks[0].PriceQuote {
		t.Fatalf("book is crossed: best bid %g, best ask %g", book.Bids[0].PriceQuote, book.Asks[0].PriceQuote)
	}
	for _, side := range [][]DepthLevel{book.Bids, book.Asks} {
		for _, level := range side {
			if level.PriceQuote <= 0 || level.QtyNative <= 0 {
				t.Fatalf("level %+v is not usable liquidity", level)
			}
		}
	}
}

func TestBinanceFuturesDepthGolden(t *testing.T) {
	var resp binanceDepthResponse
	loadDepthFixture(t, "depth_binance_futures.json", &resp)

	book, err := parseBinanceDepth(resp, "binance_futures", Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	assertBookSane(t, book, "binance_futures", "BTCUSDT")
	if len(book.Bids) != 100 || len(book.Asks) != 100 {
		t.Errorf("got %d/%d levels, want the 100 a side that was asked for", len(book.Bids), len(book.Asks))
	}
	// Futures carries `E`; the spot response below does not, and that
	// difference is the reason VenueTimeMs has a documented zero.
	if book.VenueTimeMs <= 0 {
		t.Errorf("futures venue_time_ms = %d, want the E field", book.VenueTimeMs)
	}
}

func TestBinanceSpotDepthGolden(t *testing.T) {
	var resp binanceDepthResponse
	loadDepthFixture(t, "depth_binance_spot.json", &resp)

	book, err := parseBinanceDepth(resp, "binance_spot", Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	assertBookSane(t, book, "binance_spot", "BTCUSDT")
	// Spot publishes no event time. 0 is the documented "not supplied", and
	// inventing one from our own clock would be the venue-clock mistake rule 13
	// exists to prevent.
	if book.VenueTimeMs != 0 {
		t.Errorf("spot venue_time_ms = %d, want 0 — the response carries no stamp", book.VenueTimeMs)
	}
}

func TestBybitDepthGolden(t *testing.T) {
	for _, tc := range []struct{ file, source string }{
		{"depth_bybit_futures.json", "bybit_futures"},
		{"depth_bybit_spot.json", "bybit_spot"},
	} {
		t.Run(tc.source, func(t *testing.T) {
			var resp bybitDepthResponse
			loadDepthFixture(t, tc.file, &resp)

			book, err := parseBybitDepth(resp, tc.source, Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"})
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			assertBookSane(t, book, tc.source, "BTCUSDT")
			if book.VenueTimeMs <= 0 {
				t.Errorf("venue_time_ms = %d, want the ts field", book.VenueTimeMs)
			}
		})
	}
}

// OKX levels are FOUR elements: price, size, a deprecated always-"0" field, and
// the order count. Reading position 2 as the size would report every level as
// zero and the whole book as empty.
func TestOKXDepthGolden(t *testing.T) {
	var resp okxDepthResponse
	loadDepthFixture(t, "depth_okx.json", &resp)

	book, err := parseOKXDepth(resp, "okx_futures", Symbol{Standard: "BTCUSDT", Venue: "BTC-USDT-SWAP"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	assertBookSane(t, book, "okx_futures", "BTCUSDT")

	// The raw level really does have four elements in the fixture, so the test
	// above is exercising the shape it claims to.
	if got := len(resp.Data[0].Bids[0]); got != 4 {
		t.Errorf("fixture level has %d elements, want 4 — this test no longer proves what it says", got)
	}
	// Sizes are CONTRACTS here. BTC-USDT-SWAP is 0.01 BTC a contract, so a top
	// level in the tens is coin in the tenths — the number must NOT already
	// look like coin.
	if book.Bids[0].QtyNative < 1 {
		t.Errorf("top bid size %g looks like coin; this venue quotes contracts and conversion is internal/depth's job",
			book.Bids[0].QtyNative)
	}
}

// Gate's level is an object whose size is a NUMBER and whose price is a STRING.
// Declaring the size as a string makes the whole response fail to decode.
func TestGateDepthGolden(t *testing.T) {
	var resp gateDepthResponse
	loadDepthFixture(t, "depth_gate.json", &resp)

	book, err := parseGateDepth(resp, "gate_futures", Symbol{Standard: "BTCUSDT", Venue: "BTC_USDT"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	assertBookSane(t, book, "gate_futures", "BTCUSDT")
	if book.VenueTimeMs <= 0 {
		t.Errorf("venue_time_ms = %d, want `current` seconds converted to ms", book.VenueTimeMs)
	}
	// Contracts, and the multiplier is 0.0001 BTC — so a raw size in the
	// thousands is a coin quantity around one.
	if book.Bids[0].QtyNative < 10 {
		t.Errorf("top bid size %g looks like coin; Gate quotes contracts", book.Bids[0].QtyNative)
	}
}

// THE trap of this step. Kraken returns bids ASCENDING, so the venue's own
// first bid is a resting order at a price of 1.
func TestKrakenDepthGolden_BidsArriveAscendingAndAreReordered(t *testing.T) {
	var resp krakenDepthResponse
	loadDepthFixture(t, "depth_kraken.json", &resp)

	// The fixture must still contain the trap, or this test proves nothing.
	raw := resp.OrderBook.Bids
	if len(raw) < 2 || raw[0][0] >= raw[len(raw)-1][0] {
		t.Fatalf("fixture bids are no longer ascending (%g then %g); re-record and re-read this test",
			raw[0][0], raw[len(raw)-1][0])
	}
	if raw[0][0] > 100 {
		t.Errorf("fixture's first bid is %g; the recorded trap was a resting order at a price of 1", raw[0][0])
	}

	book, err := parseKrakenDepth(resp, "kraken_futures", Symbol{Standard: "BTCUSDT", Venue: "PF_XBTUSD"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	assertBookSane(t, book, "kraken_futures", "BTCUSDT")

	// The best bid must be the venue's LAST entry, not its first.
	wantBest := raw[len(raw)-1][0]
	if book.Bids[0].PriceQuote != wantBest {
		t.Errorf("best bid = %g, want %g (the venue's last ascending entry)", book.Bids[0].PriceQuote, wantBest)
	}
	// And the whole book comes back, because there is no limit parameter.
	if len(book.Bids) < 500 {
		t.Errorf("got %d bids; this endpoint returns the entire book", len(book.Bids))
	}
}

func TestKrakenDepthRefusesAFailedResponse(t *testing.T) {
	resp := krakenDepthResponse{Result: "error", Error: "marketNotFound"}
	if _, err := parseKrakenDepth(resp, "kraken_futures", Symbol{Standard: "BTCUSDT", Venue: "PF_NOPE"}); err == nil {
		t.Fatal("a failed response parsed as a book")
	}
}

// Hyperliquid's sides are positional: levels[0] is bids, levels[1] is asks.
// Nothing in the payload names either.
func TestHyperliquidDepthGolden(t *testing.T) {
	var resp hyperliquidDepthResponse
	loadDepthFixture(t, "depth_hyperliquid.json", &resp)

	book, err := parseHyperliquidDepth(resp, "hyperliquid_futures", Symbol{Standard: "BTCUSDT", Venue: "BTC"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	assertBookSane(t, book, "hyperliquid_futures", "BTCUSDT")

	// Twenty a side and nothing widens it. The number is asserted because the
	// liquidity layer reports a lower bound on this venue BECAUSE of it — if
	// the venue ever returns more, that reasoning should be revisited.
	if len(book.Bids) != 20 || len(book.Asks) != 20 {
		t.Errorf("got %d/%d levels, want 20 a side", len(book.Bids), len(book.Asks))
	}
}

func TestHyperliquidDepthRefusesAResponseWithoutTwoSides(t *testing.T) {
	// A market the venue does not list answers with an empty levels array
	// rather than an error, and an empty book must not read as "no liquidity".
	var resp hyperliquidDepthResponse
	if _, err := parseHyperliquidDepth(resp, "hyperliquid_futures", Symbol{Standard: "NOPEUSDT", Venue: "NOPE"}); err == nil {
		t.Fatal("a response with no sides parsed as a book")
	}
}

func TestParadexDepthGolden(t *testing.T) {
	var resp paradexDepthResponse
	loadDepthFixture(t, "depth_paradex.json", &resp)

	book, err := parseParadexDepth(resp, "paradex_futures", Symbol{Standard: "BTCUSDT", Venue: "BTC-USD-PERP"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	assertBookSane(t, book, "paradex_futures", "BTCUSDT")
	// The two sides are genuinely different lengths here, and that asymmetry is
	// data rather than a defect: it is what liquidity ranking exists to show.
	if len(book.Bids) == len(book.Asks) {
		t.Logf("sides are equal in this recording (%d); the 2026-09-04 probe saw 100 bids and 43 asks", len(book.Bids))
	}
}

func TestFinishDepthBook(t *testing.T) {
	t.Run("levels with no price or no size are dropped", func(t *testing.T) {
		// A padded level contributes a row to the level count and nothing to
		// the liquidity, which is the worst of both.
		book, err := finishDepthBook(DepthBook{
			Source: "s", Symbol: "BTCUSDT",
			Bids: []DepthLevel{{PriceQuote: 100, QtyNative: 1}, {PriceQuote: 99, QtyNative: 0}, {PriceQuote: 0, QtyNative: 5}},
			Asks: []DepthLevel{{PriceQuote: 101, QtyNative: 2}},
		})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(book.Bids) != 1 {
			t.Errorf("kept %d bids, want only the usable one", len(book.Bids))
		}
	})

	t.Run("a one-sided book is refused", func(t *testing.T) {
		// Every figure downstream is measured from the mid, and a mid needs
		// both sides.
		_, err := finishDepthBook(DepthBook{
			Source: "s", Symbol: "BTCUSDT",
			Bids: []DepthLevel{{PriceQuote: 100, QtyNative: 1}},
		})
		if err == nil {
			t.Fatal("a book with no asks was accepted")
		}
	})

	t.Run("a crossed book is refused", func(t *testing.T) {
		// Two halves read at different instants, or a venue bug. Publishing it
		// gives a negative spread and a mid between prices that never coexisted.
		_, err := finishDepthBook(DepthBook{
			Source: "s", Symbol: "BTCUSDT",
			Bids: []DepthLevel{{PriceQuote: 102, QtyNative: 1}},
			Asks: []DepthLevel{{PriceQuote: 101, QtyNative: 1}},
		})
		if err == nil {
			t.Fatal("a crossed book was accepted")
		}
	})

	t.Run("sides are sorted whatever order they arrived in", func(t *testing.T) {
		book, err := finishDepthBook(DepthBook{
			Source: "s", Symbol: "BTCUSDT",
			Bids: []DepthLevel{{PriceQuote: 1, QtyNative: 41}, {PriceQuote: 100, QtyNative: 1}},
			Asks: []DepthLevel{{PriceQuote: 105, QtyNative: 1}, {PriceQuote: 101, QtyNative: 1}},
		})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if book.Bids[0].PriceQuote != 100 || book.Asks[0].PriceQuote != 101 {
			t.Errorf("best bid/ask = %g/%g, want 100/101", book.Bids[0].PriceQuote, book.Asks[0].PriceQuote)
		}
	})
}

// Every fetcher in the table must be reachable, or a source silently has no
// depth and its liquidity column is empty forever.
func TestDepthFetchers_CoverEveryTradableSource(t *testing.T) {
	fetchers := DepthFetchers()
	for _, source := range []string{
		"binance_futures", "binance_spot", "bybit_futures", "bybit_spot",
		"okx_futures", "gate_futures", "kraken_futures", "hyperliquid_futures", "paradex_futures",
	} {
		if _, ok := fetchers[source]; !ok {
			t.Errorf("no depth fetcher for %s", source)
		}
	}
	if _, ok := fetchers["pyth"]; ok {
		t.Error("pyth has a depth fetcher; an oracle has no order book")
	}
	if len(fetchers) != 9 {
		t.Errorf("got %d fetchers, want 9", len(fetchers))
	}
}

// The recorded books are the input to internal/depth's arithmetic, so the
// fixtures must stay realistic enough for that to mean something.
func TestDepthFixtures_HaveARealisticSpread(t *testing.T) {
	var resp binanceDepthResponse
	loadDepthFixture(t, "depth_binance_futures.json", &resp)
	book, err := parseBinanceDepth(resp, "binance_futures", Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	mid := (book.Bids[0].PriceQuote + book.Asks[0].PriceQuote) / 2
	spreadPct := (book.Asks[0].PriceQuote - book.Bids[0].PriceQuote) / mid * 100
	if math.Abs(spreadPct) > 1 {
		t.Errorf("recorded BTC spread is %g%%, which is not a major-pair book", spreadPct)
	}
}

// Each venue is asked for as much as it will actually give, and never more.
// Gate is the reason the clamp is not advisory: 400 comes back HTTP 400, so
// asking beyond its ceiling loses the whole book rather than shortening it.
func TestDepthLevelCeilings_AreVenueFactsNotOneGlobalNumber(t *testing.T) {
	for _, tc := range []struct {
		venue string
		max   int
	}{
		{"binance", binanceDepthMaxLevels},
		{"bybit linear", bybitLinearDepthMaxLevels},
		{"bybit spot", bybitSpotDepthMaxLevels},
		{"okx", okxDepthMaxLevels},
		{"gate", gateDepthMaxLevels},
		{"paradex", paradexDepthMaxLevels},
	} {
		if tc.max < 20 {
			t.Errorf("%s ceiling is %d; below 20 levels no venue reaches even the tight window", tc.venue, tc.max)
		}
	}
	// The measured ceilings differ per venue, which is the whole point: one
	// global number would either be refused by Gate or waste Binance's reach.
	if gateDepthMaxLevels == binanceDepthMaxLevels {
		t.Error("gate and binance share a ceiling; they were measured at 300 and 1000")
	}
}
