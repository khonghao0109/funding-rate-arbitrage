package okx

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// OKX levels are FOUR elements: price, size, a deprecated always-"0" field, and
// the order count. Reading position 2 as the size would report every level as
// zero and the whole book as empty.
func TestOKXDepthGolden(t *testing.T) {
	var resp okxDepthResponse
	exchangestest.LoadJSON(t, "depth_okx.json", &resp)

	book, err := parseOKXDepth(resp, "okx_futures", exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTC-USDT-SWAP"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	exchangestest.AssertDepthBookSane(t, book, "okx_futures", "BTCUSDT")

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

// The ceiling is a measured venue fact, not one global number.
func TestDepthLevelCeiling_IsTheMeasuredVenueFact(t *testing.T) {
	if okxDepthMaxLevels != 400 {
		t.Errorf("okx ceiling = %d, want the measured 400", okxDepthMaxLevels)
	}
}
