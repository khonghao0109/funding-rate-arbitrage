package bybit

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestBybitDepthGolden(t *testing.T) {
	for _, tc := range []struct{ file, source string }{
		{"depth_bybit_futures.json", "bybit_futures"},
		{"depth_bybit_spot.json", "bybit_spot"},
	} {
		t.Run(tc.source, func(t *testing.T) {
			var resp bybitDepthResponse
			exchangestest.LoadJSON(t, tc.file, &resp)

			book, err := parseBybitDepth(resp, tc.source, exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"})
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			exchangestest.AssertDepthBookSane(t, book, tc.source, "BTCUSDT")
			if book.VenueTimeMs <= 0 {
				t.Errorf("venue_time_ms = %d, want the ts field", book.VenueTimeMs)
			}
		})
	}
}

// The ceiling is a measured venue fact, not one global number.
func TestDepthLevelCeiling_IsTheMeasuredVenueFact(t *testing.T) {
	if bybitLinearDepthMaxLevels != 500 || bybitSpotDepthMaxLevels != 200 {
		t.Errorf("bybit ceilings = %d/%d, want the documented 500 linear / 200 spot",
			bybitLinearDepthMaxLevels, bybitSpotDepthMaxLevels)
	}
}
