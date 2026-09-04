package okx

import (
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// OKX answers the keepalive with the bare word "pong", which is not JSON at all.
// Every Decode in the handler has to fail on it without the frame becoming data.
func TestOKX_TheKeepaliveReplyIsNotMarketData(t *testing.T) {
	r := exchangestest.NewRecorder(t)
	goldenConfigs(r.Feeds)["okx_futures"].Handle([]byte("pong"), time.Now())

	if books := r.Orderbooks(); len(books) != 0 {
		t.Errorf(`the literal "pong" produced %d books`, len(books))
	}
	if trades := r.Trades(); len(trades) != 0 {
		t.Errorf(`the literal "pong" produced %d trades`, len(trades))
	}
}
