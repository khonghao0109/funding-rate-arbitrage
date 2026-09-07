package venues

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// TestCapturePriceHistoryTestdata re-records one page of every venue's hourly
// candle endpoint (step 3.3b):
//
//	CAPTURE_TESTDATA=1 go test -run TestCapturePriceHistoryTestdata ./exchanges/venues/
//
// Eight sources, six shapes, and every one of the differences is a way a parser
// can be plausibly wrong and still produce numbers: Binance and Bybit are
// positional arrays with the fields in DIFFERENT orders, Bybit and OKX come
// back newest-first, Gate stamps in seconds and names its fields, Kraken mixes
// second-level request bounds with millisecond response stamps, and
// Hyperliquid's payload carries both "t" and "T" — the case collision that
// silently shifted every candle by a millisecond on the first run here.
//
// The URLs are built the same way the fetchers build them, but not BY the
// fetchers: a recording made through the code under test could only ever agree
// with it.
func TestCapturePriceHistoryTestdata(t *testing.T) {
	if os.Getenv(exchangestest.CaptureEnv) != "1" {
		t.Skipf("set %s=1 to re-record candle payloads from the live venues", exchangestest.CaptureEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const hourMs = exchanges.SecPerHour * exchanges.MsPerSecond
	// Anchor on a whole hour so the recorded stamps are the boundaries a reader
	// can check by eye, and take a short span: twenty candles is enough for the
	// golden tests to see ordering, units and field order.
	nowMs := (time.Now().UnixMilli() / hourMs) * hourMs
	startMs := nowMs - 20*hourMs

	write := func(name string, raw []byte) {
		t.Helper()
		if err := os.WriteFile(fixturePath(t, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %s (%d bytes)", name, len(raw))
	}

	for _, venue := range []struct {
		file   string
		source string
		url    string
		post   string
	}{
		{
			file:   "price_history_binance_futures.json",
			source: "binance_futures",
			url: fmt.Sprintf("https://fapi.binance.com/fapi/v1/klines?symbol=%s&interval=1h&startTime=%d&endTime=%d&limit=%d",
				captureVenueSymbol(t, "binance_futures"), startMs, nowMs, 20),
		},
		{
			file:   "price_history_binance_spot.json",
			source: "binance_spot",
			url: fmt.Sprintf("https://api.binance.com/api/v3/klines?symbol=%s&interval=1h&startTime=%d&endTime=%d&limit=%d",
				captureVenueSymbol(t, "binance_spot"), startMs, nowMs, 20),
		},
		{
			file:   "price_history_bybit_futures.json",
			source: "bybit_futures",
			url: fmt.Sprintf("https://api.bybit.com/v5/market/kline?category=linear&symbol=%s&interval=60&start=%d&end=%d&limit=%d",
				captureVenueSymbol(t, "bybit_futures"), startMs, nowMs, 20),
		},
		{
			file:   "price_history_bybit_spot.json",
			source: "bybit_spot",
			url: fmt.Sprintf("https://api.bybit.com/v5/market/kline?category=spot&symbol=%s&interval=60&start=%d&end=%d&limit=%d",
				captureVenueSymbol(t, "bybit_spot"), startMs, nowMs, 20),
		},
		{
			file:   "price_history_okx.json",
			source: "okx_futures",
			url: fmt.Sprintf("https://www.okx.com/api/v5/market/history-candles?instId=%s&bar=1H&after=%d&limit=%d",
				captureVenueSymbol(t, "okx_futures"), nowMs, 20),
		},
		{
			file:   "price_history_gate.json",
			source: "gate_futures",
			url: fmt.Sprintf("https://api.gateio.ws/api/v4/futures/usdt/candlesticks?contract=%s&interval=1h&from=%d&limit=%d",
				captureVenueSymbol(t, "gate_futures"), startMs/exchanges.MsPerSecond, 20),
		},
		{
			file:   "price_history_kraken.json",
			source: "kraken_futures",
			url: fmt.Sprintf("https://futures.kraken.com/api/charts/v1/trade/%s/1h?from=%d&to=%d",
				captureVenueSymbol(t, "kraken_futures"), startMs/exchanges.MsPerSecond, nowMs/exchanges.MsPerSecond),
		},
		{
			file:   "price_history_hyperliquid.json",
			source: "hyperliquid_futures",
			url:    "https://api.hyperliquid.xyz/info",
			post: fmt.Sprintf(`{"type":"candleSnapshot","req":{"coin":%q,"interval":"1h","startTime":%d,"endTime":%d}}`,
				captureVenueSymbol(t, "hyperliquid_futures"), startMs, nowMs),
		},
	} {
		t.Run(venue.source, func(t *testing.T) {
			raw := captureHTTP(t, ctx, venue.url, venue.post)
			var compact bytes.Buffer
			if err := json.Compact(&compact, raw); err != nil {
				t.Fatalf("%s: response is not JSON: %v", venue.source, err)
			}
			write(venue.file, compact.Bytes())
		})
	}
}
