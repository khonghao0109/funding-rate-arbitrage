package binance

import (
	"math"
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestBinanceFuturesDepthGolden(t *testing.T) {
	var resp binanceDepthResponse
	exchangestest.LoadJSON(t, "depth_binance_futures.json", &resp)

	book, err := parseBinanceDepth(resp, "binance_futures", exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	exchangestest.AssertDepthBookSane(t, book, "binance_futures", "BTCUSDT")
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
	exchangestest.LoadJSON(t, "depth_binance_spot.json", &resp)

	book, err := parseBinanceDepth(resp, "binance_spot", exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	exchangestest.AssertDepthBookSane(t, book, "binance_spot", "BTCUSDT")
	// Spot publishes no event time. 0 is the documented "not supplied", and
	// inventing one from our own clock would be the venue-clock mistake rule 13
	// exists to prevent.
	if book.VenueTimeMs != 0 {
		t.Errorf("spot venue_time_ms = %d, want 0 — the response carries no stamp", book.VenueTimeMs)
	}
}

// The recorded books are the input to internal/depth's arithmetic, so the
// fixtures must stay realistic enough for that to mean something.
func TestDepthFixtures_HaveARealisticSpread(t *testing.T) {
	var resp binanceDepthResponse
	exchangestest.LoadJSON(t, "depth_binance_futures.json", &resp)
	book, err := parseBinanceDepth(resp, "binance_futures", exchanges.Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	mid := (book.Bids[0].PriceQuote + book.Asks[0].PriceQuote) / 2
	spreadPct := (book.Asks[0].PriceQuote - book.Bids[0].PriceQuote) / mid * 100
	if math.Abs(spreadPct) > 1 {
		t.Errorf("recorded BTC spread is %g%%, which is not a major-pair book", spreadPct)
	}
}

// The ceiling is a measured venue fact, not one global number: exceeding a
// venue's limit LOSES the whole book rather than shortening it.
func TestDepthLevelCeiling_IsTheMeasuredVenueFact(t *testing.T) {
	if binanceDepthMaxLevels != 1000 {
		t.Errorf("binance ceiling = %d, want the measured 1000", binanceDepthMaxLevels)
	}
}
