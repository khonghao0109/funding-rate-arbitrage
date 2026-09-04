package exchanges

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCaptureDepthTestdata re-records one order book from every tradable source
// (step 2.7b):
//
//	CAPTURE_TESTDATA=1 go test -run TestCaptureDepthTestdata ./exchanges/
//
// Nine sources, six response shapes, and three of them are shapes a parser can
// get plausibly wrong while still producing numbers: Kraken's ascending bids,
// OKX's four-element levels, Gate's number-and-string object. Those are exactly
// what a fixture catches and a live probe does not, because a live probe is
// read by a person who already knows what to expect.
//
// The URLs are built the way the fetchers build them but not BY them: a
// recording made through the code under test could only ever agree with it.
func TestCaptureDepthTestdata(t *testing.T) {
	if os.Getenv(captureEnv) != "1" {
		t.Skipf("set %s=1 to re-record order books from the live venues", captureEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const levels = 100

	for _, venue := range []struct {
		file   string
		source string
		url    string
		post   string
		trim   func(*testing.T, []byte) []byte
	}{
		{
			file:   "depth_binance_futures.json",
			source: "binance_futures",
			url: fmt.Sprintf("https://fapi.binance.com/fapi/v1/depth?symbol=%s&limit=%d",
				captureVenueSymbol(t, "binance_futures"), levels),
		},
		{
			file:   "depth_binance_spot.json",
			source: "binance_spot",
			url: fmt.Sprintf("https://api.binance.com/api/v3/depth?symbol=%s&limit=%d",
				captureVenueSymbol(t, "binance_spot"), levels),
		},
		{
			file:   "depth_bybit_futures.json",
			source: "bybit_futures",
			url: fmt.Sprintf("https://api.bybit.com/v5/market/orderbook?category=linear&symbol=%s&limit=%d",
				captureVenueSymbol(t, "bybit_futures"), levels),
		},
		{
			file:   "depth_bybit_spot.json",
			source: "bybit_spot",
			url: fmt.Sprintf("https://api.bybit.com/v5/market/orderbook?category=spot&symbol=%s&limit=%d",
				captureVenueSymbol(t, "bybit_spot"), levels),
		},
		{
			file:   "depth_okx.json",
			source: "okx_futures",
			url: fmt.Sprintf("https://www.okx.com/api/v5/market/books?instId=%s&sz=%d",
				captureVenueSymbol(t, "okx_futures"), levels),
		},
		{
			file:   "depth_gate.json",
			source: "gate_futures",
			url: fmt.Sprintf("https://api.gateio.ws/api/v4/futures/%s/order_book?contract=%s&limit=%d",
				gateDepthSettle, captureVenueSymbol(t, "gate_futures"), levels),
		},
		{
			// Kept whole, unlike Kraken's funding history: the ascending bid
			// order is the trap this fixture exists for, and trimming to the
			// tail would quietly remove the price-of-1 level that proves it.
			file:   "depth_kraken.json",
			source: "kraken_futures",
			url: "https://futures.kraken.com/derivatives/api/v3/orderbook?symbol=" +
				captureVenueSymbol(t, "kraken_futures"),
		},
		{
			file:   "depth_hyperliquid.json",
			source: "hyperliquid_futures",
			url:    "https://api.hyperliquid.xyz/info",
			post:   fmt.Sprintf(`{"type":"l2Book","coin":%q}`, captureVenueSymbol(t, "hyperliquid_futures")),
		},
		{
			file:   "depth_paradex.json",
			source: "paradex_futures",
			url: fmt.Sprintf("https://api.prod.paradex.trade/v1/orderbook/%s?depth=%d",
				captureVenueSymbol(t, "paradex_futures"), levels),
		},
	} {
		t.Run(venue.source, func(t *testing.T) {
			raw := captureHTTP(t, ctx, venue.url, venue.post)
			if venue.trim != nil {
				raw = venue.trim(t, raw)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, raw); err != nil {
				t.Fatalf("%s: response is not JSON: %v", venue.source, err)
			}
			path := filepath.Join(captureDirectory, venue.file)
			if err := os.WriteFile(path, compact.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("recorded %s (%d bytes)", venue.file, compact.Len())
		})
	}
}
