package bybit

import (
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// goldenConfigs builds this venue's PRODUCTION stream configs by calling the
// SAME two constructors ConnectFutures and ConnectSpot call — the same URL, the
// same subscription, the same args ceiling. It used to call bybitStream with
// its own copy of those three, which meant a connector that stopped applying
// the spot ceiling left every test green (adversarial review 2026-09-12).
// Capture swaps only Handle; a harness with its own copy of the subscribe
// messages would drift from the connector and record payloads nobody receives.
func goldenConfigs(f exchanges.Feeds) map[string]exchanges.StreamConfig {
	return map[string]exchanges.StreamConfig{
		"bybit_futures": futuresStream("bybit_futures", exchangestest.Symbols("bybit_futures"), f),
		"bybit_spot":    spotStream("bybit_spot", exchangestest.Symbols("bybit_spot"), f),
	}
}

// Every frame of the recording is replayed through the production handler and
// held to the shared data contract (exchangestest.CheckBookContract) — the
// same assertions every other venue package runs.
func TestGoldenPayloads_HonourTheDataContract(t *testing.T) {
	for source, expect := range map[string]exchangestest.BookExpectation{
		"bybit_futures": {QuantityInCoin: true, VenueClock: false, Trades: true},
		"bybit_spot":    {QuantityInCoin: true, VenueClock: false, Trades: true},
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
	cfg := goldenConfigs(r.Feeds)["bybit_futures"]
	for _, frame := range exchangestest.ReadFrames(t, "bybit_futures") {
		cfg.Handle(frame, recvAt)
	}
	exchangestest.CheckFundingContract(t, "bybit_futures", r, recvAt, exchangestest.FundingExpectation{
		IntervalSec: 8 * 3600, Model: exchanges.FundingDiscrete, VenueClock: true,
		NextFundingStamp: true, RawRateField: "fundingRate", Estimated: true,
	})
}

// A subscription acknowledgement must never read as market data.
func TestAcknowledgementProducesNoMarketData(t *testing.T) {
	r := exchangestest.NewRecorder(t)
	ack := []byte(`{"success":true,"ret_msg":"subscribe","conn_id":"x","op":"subscribe"}`)
	goldenConfigs(r.Feeds)["bybit_spot"].Handle(ack, time.Now())

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
