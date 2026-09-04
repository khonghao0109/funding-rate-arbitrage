package binance

import (
	"bytes"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// Binance encodes the trade side as "was the buyer the maker", which is the
// opposite polarity to the aggressor everything downstream reasons about.
// Getting it backwards does not fail: it reports every buy as a sell.
// It also carries a deprecated "M" field, and Go's JSON decoder prefers an exact
// tag match but falls back to a case-insensitive one - so before step 1.6 the
// always-true "M" overwrote the "m" that had just been read, and EVERY Binance
// trade came out a sell. See BinanceAggTrade.Ignore.
//
// Each frame is asserted against its own maker flag rather than by counting, so
// a polarity flip - which counting cannot see - fails this too.
func TestBinance_MakerFlagIsInvertedIntoTheAggressorSide(t *testing.T) {
	var maker, taker int

	for _, frame := range exchangestest.ReadFrames(t, "binance_spot") {
		if !bytes.Contains(frame, []byte("@aggTrade")) {
			continue
		}

		// Binance's m is "was the BUYER the maker", so a true means the
		// aggressor was selling into a resting bid.
		want := "buy"
		switch {
		case bytes.Contains(frame, []byte(`"m":true`)):
			want = "sell"
			maker++
		case bytes.Contains(frame, []byte(`"m":false`)):
			taker++
		default:
			t.Fatalf("aggTrade frame with no maker flag: %s", frame)
		}

		r := exchangestest.NewRecorder(t)
		goldenConfigs(r.Feeds)["binance_spot"].Handle(frame, time.Now())

		trades := r.Trades()
		if len(trades) != 1 {
			t.Fatalf("one aggTrade frame produced %d trades", len(trades))
		}
		if trades[0].Side != want {
			t.Errorf("side = %q, want %q for %s", trades[0].Side, want, frame)
		}
	}

	// Without both polarities in the recording this asserts only half the
	// mapping, and the bug it exists for was invisible to exactly that half.
	if maker == 0 || taker == 0 {
		t.Errorf("recording has %d m:true and %d m:false frames; both are needed to pin the mapping",
			maker, taker)
	}
}
