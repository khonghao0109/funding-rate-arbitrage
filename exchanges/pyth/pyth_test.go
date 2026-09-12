package pyth

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// Pyth is the one connector with no recorded payload. hermes.pyth.network
// answers 401 Unauthorized on both the SSE stream and the REST latest endpoint
// as of 2026-09-03 - which is how step 1.5 discovered the oracle had been dead
// for some time behind a status code the old code never checked. Nothing could
// be captured, so the payload below is SYNTHETIC: it is built from the struct
// definitions this connector has always decoded into, and it is labelled as such
// rather than passed off as a recording.
//
// What that limits is real and worth stating: these tests prove the arithmetic
// and the routing, and prove nothing about whether Pyth's current wire format
// still matches PythSSEResponse. Only a live payload can settle that, and it has
// to be re-checked before the oracle is trusted again.

const pythBTCFeedID = "e62df6c8b4a85fe1a67db44dc12de5db330f7ac66b72dc658afedf0f4a415b43"

func pythSymbols() []exchanges.Symbol {
	return []exchanges.Symbol{{Standard: "BTCUSDT", Venue: pythBTCFeedID}}
}

// syntheticPythLine builds one SSE data line. Shape from PythSSEResponse.
func syntheticPythLine(feedID, price string, expo int, publishTimeSec int64) string {
	return fmt.Sprintf(
		`data:{"parsed":[{"id":"%s","price":{"price":"%s","conf":"1234","expo":%d,"publish_time":%d},`+
			`"ema_price":{"price":"%s","conf":"1234","expo":%d,"publish_time":%d}}]}`,
		feedID, price, expo, publishTimeSec, price, expo, publishTimeSec)
}

// Pyth publishes an integer and a decimal exponent rather than a decimal number,
// so the exponent IS the price. Getting it wrong does not look wrong: it moves
// the oracle by a factor of ten and the deviation block reports the venues as
// mispriced instead of the parser.
func TestParsePythPrice_AppliesTheExponent(t *testing.T) {
	cases := []struct {
		raw  string
		expo int
		want float64
	}{
		{"7765012345678", -8, 77650.12345678},
		{"7765012345678", -6, 7765012.345678},
		{"12345", 0, 12345},
		{"12345", 2, 1234500},
	}

	for _, tc := range cases {
		got, err := ParsePrice(tc.raw, tc.expo)
		if err != nil {
			t.Errorf("ParsePrice(%q, %d): %v", tc.raw, tc.expo, err)
			continue
		}
		if math.Abs(got-tc.want) > math.Abs(tc.want)*1e-12 {
			t.Errorf("ParsePrice(%q, %d) = %g, want %g", tc.raw, tc.expo, got, tc.want)
		}
	}
}

func TestParsePythPrice_RejectsAPriceThatIsNotANumber(t *testing.T) {
	if _, err := ParsePrice("not-a-number", -8); err == nil {
		t.Error("a malformed price was accepted; it would reach the scanner as 0")
	}
}

// Pyth timestamps in SECONDS while every other venue here uses milliseconds.
// Publishing the raw value would put the oracle's clock 1000x in the past.
func TestHandlePythLine_ConvertsPublishTimeToMilliseconds(t *testing.T) {
	r := exchangestest.NewRecorder(t)
	recvAt := time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC)

	const publishTimeSec = 1788415749
	line := syntheticPythLine(pythBTCFeedID, "7765012345678", -8, publishTimeSec)

	if keepGoing, _ := handlePythLine("pyth", pythSymbols(), r.Feeds, line, recvAt); !keepGoing {
		t.Fatal("handlePythLine reported cancellation on a live context")
	}

	prices := r.Prices()
	if len(prices) != 1 {
		t.Fatalf("got %d prices, want 1", len(prices))
	}
	price := prices[0]

	if want := int64(publishTimeSec) * 1000; price.VenueTimeMs != want {
		t.Errorf("venue_time_ms = %d, want %d (seconds converted to milliseconds)", price.VenueTimeMs, want)
	}
	if math.Abs(price.Price-77650.12345678) > 1e-6 {
		t.Errorf("price = %g, want 77650.12345678", price.Price)
	}
	if price.Symbol != "BTCUSDT" {
		t.Errorf("symbol = %q, want the standard BTCUSDT rather than the feed id", price.Symbol)
	}
	if price.Source != "pyth" {
		t.Errorf("source = %q, want pyth", price.Source)
	}
	if !price.RecvAt.Equal(recvAt) {
		t.Errorf("RecvAt = %s, want the stamp taken at the read", price.RecvAt)
	}
}

// The stream carries more than price updates, and none of the rest may become a
// price. A feed id we never subscribed to is the important one: the oracle would
// otherwise publish some other asset's price under our symbol.
func TestHandlePythLine_IgnoresEverythingThatIsNotOurPriceUpdate(t *testing.T) {
	cases := map[string]string{
		"SSE comment":    ": keep-alive",
		"event line":     "event: price_update",
		"empty data":     "data:",
		"heartbeat":      "data: heartbeat",
		"malformed JSON": `data:{"parsed":[`,
		"a feed we never sub'd": syntheticPythLine(
			"ff61491a931112ddf1bd8147cd1b641375f79f5825126d665480874634fd0ace", "1", -2, 1788415749),
	}

	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			r := exchangestest.NewRecorder(t)
			if keepGoing, _ := handlePythLine("pyth", pythSymbols(), r.Feeds, line, time.Now()); !keepGoing {
				t.Fatal("handlePythLine reported cancellation on a live context")
			}
			if prices := r.Prices(); len(prices) != 0 {
				t.Errorf("produced %d prices from %q: %+v", len(prices), line, prices)
			}
		})
	}
}

// --- the data watchdog (2026-09-12) ---

// sseTestServer streams lines to every client until the client goes away. Each
// line is flushed on its own so the reader sees it immediately, which is what
// makes the watchdogs measurable in milliseconds rather than in the 60s the
// production constants use.
func sseTestServer(t *testing.T, lines func(write func(string) bool)) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("the test server cannot flush, so nothing would stream")
			return
		}
		flusher.Flush()
		lines(func(line string) bool {
			if _, err := io.WriteString(w, line+"\n"); err != nil {
				return false
			}
			flusher.Flush()
			select {
			case <-r.Context().Done():
				return false
			default:
				return true
			}
		})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// The failure this exists for, in Pyth's transport: a stream that keeps sending
// and never sends a PRICE. Hermes carries SSE comments and `data: heartbeat`,
// and the frame watchdog is reset by every one of them — so before 2026-09-12 a
// Hermes that stopped publishing while still heartbeating would have held this
// connection open for as long as the process ran, with no price on it and every
// health signal green. That is the bybit_spot failure in another transport, and
// the oracle is where it would hide longest: Pyth answered 401 for a whole 72h
// soak and nothing noticed.
func TestStreamPyth_EndsAStreamThatHeartbeatsAndNeverPrices(t *testing.T) {
	var sent int32
	url := sseTestServer(t, func(write func(string) bool) {
		for {
			if !write(": keep-alive") || !write("data: heartbeat") {
				return
			}
			atomic.AddInt32(&sent, 2)
			time.Sleep(20 * time.Millisecond)
		}
	})

	r := exchangestest.NewRecorder(t)
	feeds := r.Feeds
	feeds.DataSilenceTimeout = 300 * time.Millisecond

	err := streamPyth("pyth", pythSymbols(), feeds, url)

	if !errors.Is(err, exchanges.ErrDataSilence) {
		t.Fatalf("stream ended with %v, want ErrDataSilence — a heartbeat-only oracle must not look alive", err)
	}
	if got := atomic.LoadInt32(&sent); got < 4 {
		t.Fatalf("the server sent %d lines; the test is not exercising a LIVE stream unless lines kept arriving", got)
	}
	if prices := r.Prices(); len(prices) != 0 {
		t.Errorf("a heartbeat-only stream produced %d prices", len(prices))
	}
}

// The data clock is restarted by a price, not by any line, and is measured from
// the last PRICE — so a stream that prices and then goes to heartbeats is cut
// the same silence later, not from the connect.
func TestStreamPyth_MeasuresSilenceFromTheLastPriceNotTheConnect(t *testing.T) {
	url := sseTestServer(t, func(write func(string) bool) {
		for i := 0; i < 4; i++ {
			if !write(syntheticPythLine(pythBTCFeedID, "7765012345678", -8, 1788415749)) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		for {
			if !write("data: heartbeat") {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	r := exchangestest.NewRecorder(t)
	feeds := r.Feeds
	feeds.DataSilenceTimeout = 300 * time.Millisecond

	startedAt := time.Now()
	err := streamPyth("pyth", pythSymbols(), feeds, url)
	lasted := time.Since(startedAt)

	if !errors.Is(err, exchanges.ErrDataSilence) {
		t.Fatalf("stream ended with %v, want ErrDataSilence", err)
	}
	if prices := r.Prices(); len(prices) != 4 {
		t.Errorf("got %d prices, want the 4 the server sent", len(prices))
	}
	// Measured from the connect it would have fired at 300ms, before the fourth
	// price at ~200ms could restart it.
	if lasted < 450*time.Millisecond {
		t.Errorf("stream lasted %s; the data clock was not restarted by the prices that did arrive", lasted)
	}
}

// A stream that keeps pricing is never cut, however short the threshold is next
// to its cadence — the failure mode that would be worse than the bug.
func TestStreamPyth_LeavesAStreamThatKeepsPricingAlone(t *testing.T) {
	url := sseTestServer(t, func(write func(string) bool) {
		for {
			if !write(syntheticPythLine(pythBTCFeedID, "7765012345678", -8, 1788415749)) {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
	})

	r := exchangestest.NewRecorder(t)
	feeds := r.Feeds
	feeds.DataSilenceTimeout = 300 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- streamPyth("pyth", pythSymbols(), feeds, url) }()
	select {
	case err := <-done:
		t.Fatalf("stream ended with %v while the server was still pricing every 30ms", err)
	case <-time.After(700 * time.Millisecond):
	}
}

// Zero is off, and off is what every harness and every config written before
// this existed gets: only the frame watchdog may end the stream then.
func TestStreamPyth_DataSilenceZeroLeavesTheOldBehaviourExactly(t *testing.T) {
	url := sseTestServer(t, func(write func(string) bool) {
		for {
			if !write("data: heartbeat") {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	r := exchangestest.NewRecorder(t)
	// DataSilenceTimeout deliberately left at its zero value.
	done := make(chan error, 1)
	go func() { done <- streamPyth("pyth", pythSymbols(), r.Feeds, url) }()
	select {
	case err := <-done:
		t.Fatalf("stream ended with %v; with the threshold unset only the 60s frame watchdog may end it", err)
	case <-time.After(600 * time.Millisecond):
	}
}
