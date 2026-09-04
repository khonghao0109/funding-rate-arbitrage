package hyperliquid

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// Hyperliquid's sides are positional: levels[0] is bids, levels[1] is asks.
// Nothing in the payload names either.
func TestHyperliquidDepthGolden(t *testing.T) {
	var resp hyperliquidDepthResponse
	exchangestest.LoadJSON(t, "depth_hyperliquid.json", &resp)

	book, err := parseHyperliquidDepth(resp, "hyperliquid_futures", exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTC"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	exchangestest.AssertDepthBookSane(t, book, "hyperliquid_futures", "BTCUSDT")

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
	if _, err := parseHyperliquidDepth(resp, "hyperliquid_futures", exchanges.Symbol{Standard: "NOPEUSDT", Venue: "NOPE"}); err == nil {
		t.Fatal("a response with no sides parsed as a book")
	}
}
