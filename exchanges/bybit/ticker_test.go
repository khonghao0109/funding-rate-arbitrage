package bybit

import (
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// Bybit's ticker is snapshot+delta and an absent field means UNCHANGED. This
// is the trap of the step, so it is asserted directly on the recording: the
// deltas that follow a snapshot must never blank the funding rate.
func TestGoldenFunding_BybitDeltaNeverBlanksTheRate(t *testing.T) {
	r := exchangestest.NewRecorder(t)
	cfg := goldenConfigs(r.Feeds)["bybit_futures"]
	for _, frame := range exchangestest.ReadFrames(t, "bybit_futures") {
		cfg.Handle(frame, exchangestest.ReplayAt())
	}
	readings := r.Fundings()
	if len(readings) < 2 {
		t.Fatalf("recording produced %d readings; the snapshot+delta merge needs several", len(readings))
	}
	for _, got := range readings {
		if got.RawRate == 0 {
			t.Fatal("a funding rate came out 0 — a delta overwrote the merged state instead of updating it")
		}
		if got.IntervalSec != 8*3600 {
			t.Fatalf("IntervalSec = %d; fundingIntervalHour arrives only on the SNAPSHOT, so a delta lost it",
				got.IntervalSec)
		}
	}
}

// The merge itself, exercised on the two message shapes the venue sends: only
// fields a message CONTAINS may change the cached state.
func TestBybitTickerMerge_AbsentMeansUnchanged(t *testing.T) {
	snapshot := []byte(`{"topic":"tickers.BTCUSDT","type":"snapshot","ts":1788487585984,"data":{` +
		`"symbol":"BTCUSDT","fundingRate":"0.00005908","nextFundingTime":"1788508800000",` +
		`"fundingIntervalHour":"8","fundingCap":"0.00333","markPrice":"80808.79","indexPrice":"80843.27"}}`)
	// A real delta, measured 2026-09-04: no funding fields at all.
	delta := []byte(`{"topic":"tickers.BTCUSDT","type":"delta","ts":1788487586083,"data":{` +
		`"symbol":"BTCUSDT","markPrice":"80808.82","ask1Price":"80807.00"}}`)

	// A delta that DOES move the rate, which must publish the merged view:
	// the new rate, with the interval and settlement stamp only the snapshot
	// ever carried.
	rateDelta := []byte(`{"topic":"tickers.BTCUSDT","type":"delta","ts":1788487587000,"data":{` +
		`"symbol":"BTCUSDT","fundingRate":"0.00006100"}}`)

	r := exchangestest.NewRecorder(t)
	tickers := map[string]*bybitTicker{}
	symbols := []exchanges.Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}}
	recvAt := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)

	_, _ = handleBybitTicker("bybit_futures", symbols, tickers, r.Feeds, snapshot, recvAt)
	_, _ = handleBybitTicker("bybit_futures", symbols, tickers, r.Feeds, delta, recvAt)

	// The price-only delta must publish NOTHING. Republishing on it would
	// refresh RecvAt roughly ten times a second per market, which is what
	// makes a dead funding subscription look permanently fresh.
	readings := r.Fundings()
	if len(readings) != 1 {
		t.Fatalf("got %d readings from a snapshot plus a price-only delta, want only the snapshot's", len(readings))
	}
	if readings[0].RawRate != 0.00005908 {
		t.Errorf("snapshot RawRate = %v, want 0.00005908", readings[0].RawRate)
	}

	_, _ = handleBybitTicker("bybit_futures", symbols, tickers, r.Feeds, rateDelta, recvAt)
	readings = r.Fundings()
	if len(readings) != 1 {
		t.Fatalf("a delta carrying a NEW rate produced %d readings, want 1", len(readings))
	}
	after := readings[0]
	if after.RawRate != 0.00006100 {
		t.Errorf("RawRate = %v, want the delta's 0.00006100", after.RawRate)
	}
	// These came from the snapshot and must survive into a reading published
	// by a delta that did not repeat them — the whole point of merging.
	if after.IntervalSec != 8*3600 {
		t.Errorf("IntervalSec = %d, want the snapshot's 28800", after.IntervalSec)
	}
	if after.NextFundingAtMs != 1788508800000 {
		t.Errorf("NextFundingAtMs = %d, want the snapshot's", after.NextFundingAtMs)
	}
	if after.RateCapFrac != 0.00333 || !after.HasCap {
		t.Errorf("cap = %v (has %v), want the snapshot's 0.00333", after.RateCapFrac, after.HasCap)
	}
	// Bybit sends no floor at all, so claiming one would say funding here can
	// never go negative.
	if after.HasFloor {
		t.Errorf("HasFloor = true, but Bybit publishes no floor (RateFloorFrac %v)", after.RateFloorFrac)
	}
	// The price-only delta's mark price was merged even though it published
	// nothing, so it shows up here.
	if after.MarkPrice != 80808.82 {
		t.Errorf("MarkPrice = %v, want the price-only delta's 80808.82", after.MarkPrice)
	}
	if after.VenueTimeMs != 1788487587000 {
		t.Errorf("VenueTimeMs = %d, want the publishing frame's own stamp", after.VenueTimeMs)
	}
}
