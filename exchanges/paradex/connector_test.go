package paradex

import (
	"bytes"
	"testing"

	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// Paradex publishes a summary for EVERY market it lists - hundreds, including
// options with their own strikes and expiries. Only the ones this scanner
// subscribed to may reach it; anything else would appear under a symbol it was
// never quoted for.
func TestParadex_MarketsWeNeverSubscribedToAreDropped(t *testing.T) {
	frames := exchangestest.ReadFrames(t, "paradex_futures")

	// Guard against the assertion below being vacuous: the recording must
	// actually contain markets we do not follow, or dropping them proves nothing.
	var foreign int
	for _, frame := range frames {
		if bytes.Contains(frame, []byte(`"symbol":`)) &&
			!bytes.Contains(frame, []byte(`"BTC-USD-PERP"`)) &&
			!bytes.Contains(frame, []byte(`"ETH-USD-PERP"`)) {
			foreign++
		}
	}
	if foreign == 0 {
		t.Fatal("the recording holds no unsubscribed market, so this proves nothing; re-record")
	}

	r := exchangestest.NewRecorder(t)
	exchangestest.Replay(t, r, "paradex_futures", goldenConfigs(r.Feeds)["paradex_futures"])
	for _, book := range r.Orderbooks() {
		if book.Symbol != "BTCUSDT" && book.Symbol != "ETHUSDT" {
			t.Errorf("published %q, which was never subscribed", book.Symbol)
		}
	}
	t.Logf("recording carried %d frames for markets we do not follow, none reached the scanner", foreign)
}
