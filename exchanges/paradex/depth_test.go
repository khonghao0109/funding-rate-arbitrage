package paradex

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestParadexDepthGolden(t *testing.T) {
	var resp paradexDepthResponse
	exchangestest.LoadJSON(t, "depth_paradex.json", &resp)

	book, err := parseParadexDepth(resp, "paradex_futures", exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTC-USD-PERP"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	exchangestest.AssertDepthBookSane(t, book, "paradex_futures", "BTCUSDT")
	// The two sides are genuinely different lengths here, and that asymmetry is
	// data rather than a defect: it is what liquidity ranking exists to show.
	if len(book.Bids) == len(book.Asks) {
		t.Logf("sides are equal in this recording (%d); the 2026-09-04 probe saw 100 bids and 43 asks", len(book.Bids))
	}
}

// The ceiling is a measured venue fact, not one global number: Paradex answers
// "Depth: must be no greater than 100." above it, losing the whole book.
func TestDepthLevelCeiling_IsTheMeasuredVenueFact(t *testing.T) {
	if paradexDepthMaxLevels != 100 {
		t.Errorf("paradex ceiling = %d, want the measured 100", paradexDepthMaxLevels)
	}
}
