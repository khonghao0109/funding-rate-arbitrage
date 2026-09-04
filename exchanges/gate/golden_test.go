package gate

import (
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// goldenConfigs builds this venue's PRODUCTION stream configs — the same URL
// and the same subscription production uses. Capture swaps only Handle; a
// harness with its own copy of the subscribe messages would drift from the
// connector and record payloads nobody actually receives.
func goldenConfigs(f exchanges.Feeds) map[string]exchanges.StreamConfig {
	return map[string]exchanges.StreamConfig{
		"gate_futures": gateStream("gate_futures", exchangestest.Symbols("gate_futures"), f),
	}
}

// Every frame of the recording is replayed through the production handler and
// held to the shared data contract (exchangestest.CheckBookContract) — the
// same assertions every other venue package runs.
func TestGoldenPayloads_HonourTheDataContract(t *testing.T) {
	for source, expect := range map[string]exchangestest.BookExpectation{
		"gate_futures": {QuantityInCoin: false, VenueClock: true, Trades: false},
	} {
		t.Run(source, func(t *testing.T) {
			r := exchangestest.NewRecorder(t)
			recvAt := exchangestest.Replay(t, r, source, goldenConfigs(r.Feeds)[source])
			exchangestest.CheckBookContract(t, source, r, recvAt, expect)
		})
	}
}

// The same recording, held to the shared FUNDING contract.
func TestGoldenFunding_HonoursTheContract(t *testing.T) {
	r := exchangestest.NewRecorder(t)
	recvAt := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	cfg := goldenConfigs(r.Feeds)["gate_futures"]
	for _, frame := range exchangestest.ReadFrames(t, "gate_futures") {
		cfg.Handle(frame, recvAt)
	}
	exchangestest.CheckFundingContract(t, "gate_futures", r, recvAt, exchangestest.FundingExpectation{
		IntervalSec: 8 * 3600, Model: exchanges.FundingDiscrete, VenueClock: true,
		NextFundingStamp: true, RawRateField: "funding_rate", Estimated: true,
	})
}

// A subscription acknowledgement must never read as market data.
func TestAcknowledgementProducesNoMarketData(t *testing.T) {
	r := exchangestest.NewRecorder(t)
	ack := []byte(`{"time":1788415757,"channel":"futures.book_ticker","event":"subscribe","payload":["BTC_USDT"],"result":{"status":"success"}}`)
	goldenConfigs(r.Feeds)["gate_futures"].Handle(ack, time.Now())

	if books := r.Orderbooks(); len(books) != 0 {
		t.Errorf("an acknowledgement produced %d books: %+v", len(books), books)
	}
	if trades := r.Trades(); len(trades) != 0 {
		t.Errorf("an acknowledgement produced %d trades: %+v", len(trades), trades)
	}
	if prices := r.Prices(); len(prices) != 0 {
		t.Errorf("an acknowledgement produced %d prices: %+v", len(prices), prices)
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
