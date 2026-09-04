package kraken

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// THE trap of this step. Kraken returns bids ASCENDING, so the venue's own
// first bid is a resting order at a price of 1.
func TestKrakenDepthGolden_BidsArriveAscendingAndAreReordered(t *testing.T) {
	var resp krakenDepthResponse
	exchangestest.LoadJSON(t, "depth_kraken.json", &resp)

	// The fixture must still contain the trap, or this test proves nothing.
	raw := resp.OrderBook.Bids
	if len(raw) < 2 || raw[0][0] >= raw[len(raw)-1][0] {
		t.Fatalf("fixture bids are no longer ascending (%g then %g); re-record and re-read this test",
			raw[0][0], raw[len(raw)-1][0])
	}
	if raw[0][0] > 100 {
		t.Errorf("fixture's first bid is %g; the recorded trap was a resting order at a price of 1", raw[0][0])
	}

	book, err := parseKrakenDepth(resp, "kraken_futures", exchanges.Symbol{Standard: "BTCUSDT", Venue: "PF_XBTUSD"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	exchangestest.AssertDepthBookSane(t, book, "kraken_futures", "BTCUSDT")

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
	if _, err := parseKrakenDepth(resp, "kraken_futures", exchanges.Symbol{Standard: "BTCUSDT", Venue: "PF_NOPE"}); err == nil {
		t.Fatal("a failed response parsed as a book")
	}
}
