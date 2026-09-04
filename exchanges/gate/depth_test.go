package gate

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// Gate's level is an object whose size is a NUMBER and whose price is a STRING.
// Declaring the size as a string makes the whole response fail to Decode.
func TestGateDepthGolden(t *testing.T) {
	var resp gateDepthResponse
	exchangestest.LoadJSON(t, "depth_gate.json", &resp)

	book, err := parseGateDepth(resp, "gate_futures", exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTC_USDT"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	exchangestest.AssertDepthBookSane(t, book, "gate_futures", "BTCUSDT")
	if book.VenueTimeMs <= 0 {
		t.Errorf("venue_time_ms = %d, want `current` seconds converted to ms", book.VenueTimeMs)
	}
	// Contracts, and the multiplier is 0.0001 BTC — so a raw size in the
	// thousands is a coin quantity around one.
	if book.Bids[0].QtyNative < 10 {
		t.Errorf("top bid size %g looks like coin; Gate quotes contracts", book.Bids[0].QtyNative)
	}
}

// The ceiling is a measured venue fact, not one global number: Gate answers
// HTTP 400 above it, losing the whole book rather than shortening it.
func TestDepthLevelCeiling_IsTheMeasuredVenueFact(t *testing.T) {
	if gateDepthMaxLevels != 300 {
		t.Errorf("gate ceiling = %d, want the measured 300", gateDepthMaxLevels)
	}
}
