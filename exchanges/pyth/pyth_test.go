package pyth

import (
	"fmt"
	"math"
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

	if !handlePythLine("pyth", pythSymbols(), r.Feeds, line, recvAt) {
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
			if !handlePythLine("pyth", pythSymbols(), r.Feeds, line, time.Now()) {
				t.Fatal("handlePythLine reported cancellation on a live context")
			}
			if prices := r.Prices(); len(prices) != 0 {
				t.Errorf("produced %d prices from %q: %+v", len(prices), line, prices)
			}
		})
	}
}
