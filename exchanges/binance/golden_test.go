package binance

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// goldenConfigs builds this venue's PRODUCTION stream configs — the same URL
// and the same subscription production uses. Capture swaps only Handle; a
// harness with its own copy of the subscribe messages would drift from the
// connector and record payloads nobody actually receives.
func goldenConfigs(f exchanges.Feeds) map[string]exchanges.StreamConfig {
	return map[string]exchanges.StreamConfig{
		"binance_futures": binanceStream("binance_futures", exchangestest.Symbols("binance_futures"), f, "wss://fstream.binance.com"),
		"binance_spot":    binanceStream("binance_spot", exchangestest.Symbols("binance_spot"), f, "wss://stream.binance.com:9443"),
	}
}

// Every frame of the recording is replayed through the production handler and
// held to the shared data contract (exchangestest.CheckBookContract) — the
// same assertions every other venue package runs.
func TestGoldenPayloads_HonourTheDataContract(t *testing.T) {
	for source, expect := range map[string]exchangestest.BookExpectation{
		// Recorded 2026-09-03: the futures bookTicker carries "E", the SPOT one
		// carries no event time at all. Same connector, same stream name,
		// different payload — so venue_time_ms is genuinely 0 for Binance spot
		// books, and that is the venue's doing rather than the connector's.
		// binance_futures Trades:false — aggTrade delivered none in the
		// recording window (connection-rate limiting after the capture run);
		// its trade parsing is covered by binance_spot, which shares the
		// handler.
		"binance_futures": {QuantityInCoin: true, VenueClock: true, Trades: false},
		"binance_spot":    {QuantityInCoin: true, VenueClock: false, Trades: true},
	} {
		t.Run(source, func(t *testing.T) {
			r := exchangestest.NewRecorder(t)
			recvAt := exchangestest.Replay(t, r, source, goldenConfigs(r.Feeds)[source])
			exchangestest.CheckBookContract(t, source, r, recvAt, expect)
		})
	}
}

// TestCaptureTestdata re-records this venue's golden payloads from the live
// venue. Skipped unless CAPTURE_TESTDATA=1 — `go test ./...` stays offline.
func TestCaptureTestdata(t *testing.T) {
	exchangestest.SkipUnlessCapture(t)
	for source, cfg := range goldenConfigs(exchanges.Feeds{}) {
		t.Run(source, func(t *testing.T) {
			frames := exchangestest.CaptureFrames(t, source, cfg)
			if len(frames) == 0 {
				t.Fatalf("%s delivered nothing", source)
			}
			exchangestest.WriteFrames(t, source, frames)
			t.Logf("%s: %d frames", source, len(frames))
		})
	}
}
