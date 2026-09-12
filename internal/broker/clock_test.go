package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A fake venue whose clock the test moves. It answers the documented server
// time endpoint and echoes back the timestamp of any signed request, which is
// what lets the tests assert what was actually SENT rather than what was
// computed.
type fakeVenue struct {
	serverNowMs atomic.Int64
	timeCalls   atomic.Int32
	lastQuery   atomic.Value // string
}

func newFakeVenue(t *testing.T, serverNowMs int64) (*fakeVenue, *pinnedTransport) {
	t.Helper()
	v := &fakeVenue{}
	v.serverNowMs.Store(serverNowMs)
	v.lastQuery.Store("")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.lastQuery.Store(r.URL.RawQuery)
		switch r.URL.Path {
		case BinanceFuturesTimePath:
			v.timeCalls.Add(1)
			fmt.Fprintf(w, `{"serverTime":%d}`, v.serverNowMs.Load())
		default:
			fmt.Fprint(w, `[{"asset":"USDT","balance":"1000"}]`)
		}
	}))
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return v, &pinnedTransport{to: u}
}

func (v *fakeVenue) sentTimestampMs(t *testing.T) int64 {
	t.Helper()
	values, err := url.ParseQuery(v.lastQuery.Load().(string))
	if err != nil {
		t.Fatalf("parse the query the venue saw: %v", err)
	}
	ms, err := strconv.ParseInt(values.Get("timestamp"), 10, 64)
	if err != nil {
		t.Fatalf("no usable timestamp was sent (query had %q)", values.Encode())
	}
	return ms
}

// clockTestClient pins a local clock the test controls. The venue's clock and
// ours are then two independent numbers, which is the only way to tell a
// correction from an accident.
func clockTestClient(t *testing.T, tr *pinnedTransport, localNowMs int64, recvWindowMs int64) (*Client, *atomic.Int64) {
	t.Helper()
	local := &atomic.Int64{}
	local.Store(localNowMs)
	c, err := NewClient(Config{
		BaseURL:      BinanceFuturesTestnetBaseURL,
		Credentials:  Credentials{APIKey: NewSecret("k"), APISecret: NewSecret(sentinel)},
		RecvWindowMs: recvWindowMs,
		TimePath:     BinanceFuturesTimePath,
		HTTPClient:   &http.Client{Transport: tr, Timeout: 5 * time.Second},
		Now:          func() time.Time { return time.UnixMilli(local.Load()) },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, local
}

// The correction itself: a venue 30 s ahead of us must receive a timestamp in
// ITS frame, not ours. Without this every signed request fails at the venue
// with -1021 and the only diagnosis is a number the venue chose.
func TestClient_SendsATimestampInTheVenuesFrameNotOurs(t *testing.T) {
	const localMs, serverMs = int64(1_789_000_000_000), int64(1_789_000_030_000)
	venue, tr := newFakeVenue(t, serverMs)
	client, _ := clockTestClient(t, tr, localMs, 60000)

	var into []map[string]any
	if err := client.GetSigned(context.Background(), "/fapi/v3/balance", nil, &into); err != nil {
		t.Fatalf("GetSigned: %v", err)
	}
	if got := client.ClockSkewMs(); got != 30_000 {
		t.Errorf("ClockSkewMs = %d, want 30000 (server − local)", got)
	}
	// The local clock never moved, so the sent timestamp is exactly local+skew.
	if got := venue.sentTimestampMs(t); got != serverMs {
		t.Errorf("timestamp sent = %d, want %d (local %d + skew 30000)", got, serverMs, localMs)
	}
	if venue.timeCalls.Load() != 1 {
		t.Errorf("the time endpoint was called %d times, want exactly 1 before the first signed call", venue.timeCalls.Load())
	}
}

// Skew is measured against the MIDPOINT of the request, so half the round trip
// is not charged to the venue's clock. Here the local clock advances 400 ms
// across the call, so the midpoint is local+200 and a venue reading exactly
// local+200 is in perfect agreement — skew 0, not 400 and not −400.
func TestClient_MeasuresSkewAgainstTheMidpointOfTheRoundTrip(t *testing.T) {
	const localMs = int64(1_789_000_000_000)
	venue, tr := newFakeVenue(t, localMs+200)
	client, local := clockTestClient(t, tr, localMs, 60000)
	tr.beforeRoundTrip = func() { local.Add(400) }

	if _, err := client.SyncClock(context.Background()); err != nil {
		t.Fatalf("SyncClock: %v", err)
	}
	if got := client.ClockSkewMs(); got != 0 {
		t.Errorf("ClockSkewMs = %d, want 0: the venue read the midpoint of our own request window", got)
	}
	_ = venue
}

// A machine whose clock is further out than the window it declares cannot make
// a request the venue will accept — the correction would be the only thing
// holding it together, and a correction measured once and drifting is exactly
// how -1021 arrives at 3am. Refuse locally, in milliseconds, before spending
// the call.
func TestClient_RefusesToSignWhenTheSkewExceedsTheDeclaredRecvWindow(t *testing.T) {
	const localMs = int64(1_789_000_000_000)
	_, tr := newFakeVenue(t, localMs+90_000) // 90 s out, window 5 s
	client, _ := clockTestClient(t, tr, localMs, 5000)

	err := client.GetSigned(context.Background(), "/fapi/v3/balance", nil, nil)
	if err == nil {
		t.Fatal("a 90 s skew against a 5 s recvWindow must be refused")
	}
	if !errors.Is(err, ErrClockSkew) {
		t.Errorf("error = %v, want it to wrap ErrClockSkew", err)
	}
	for _, want := range []string{"90000", "5000"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name the measured skew and the window in ms; got %q", err)
		}
	}
}

// Re-synced on a schedule, not once at start-up: a laptop that suspends comes
// back with a clock that moved and a skew that did not.
func TestClient_ResyncsTheClockWhenTheMeasurementGoesStale(t *testing.T) {
	const localMs = int64(1_789_000_000_000)
	venue, tr := newFakeVenue(t, localMs)
	client, local := clockTestClient(t, tr, localMs, 60000)
	client.SetClockSyncEvery(time.Minute)

	if err := client.GetSigned(context.Background(), "/fapi/v3/balance", nil, nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Still fresh: no second measurement.
	local.Add(30 * 1000)
	if err := client.GetSigned(context.Background(), "/fapi/v3/balance", nil, nil); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := venue.timeCalls.Load(); got != 1 {
		t.Errorf("the clock was measured %d times inside one sync interval, want 1", got)
	}

	// Past the interval, and the venue's clock has moved differently from ours.
	local.Add(2 * 60 * 1000)
	venue.serverNowMs.Store(local.Load() + 4000)
	if err := client.GetSigned(context.Background(), "/fapi/v3/balance", nil, nil); err != nil {
		t.Fatalf("third call: %v", err)
	}
	if got := venue.timeCalls.Load(); got != 2 {
		t.Errorf("the clock was measured %d times, want a re-measurement once the interval passed", got)
	}
	if got := client.ClockSkewMs(); got != 4000 {
		t.Errorf("ClockSkewMs = %d, want the NEW 4000 — a stale skew is worse than none", got)
	}
}

// If the clock cannot be measured, a signed request must not go out on an
// unknown skew and hope. The failure names the time endpoint.
func TestClient_WillNotSignOnAnUnmeasurableClock(t *testing.T) {
	tr := &pinnedTransport{failErr: errors.New("dial tcp: connection refused")}
	client, _ := clockTestClient(t, tr, 1_789_000_000_000, 5000)

	err := client.GetSigned(context.Background(), "/fapi/v3/balance", nil, nil)
	if err == nil {
		t.Fatal("a signed call must not proceed when the clock could not be measured")
	}
	if !strings.Contains(err.Error(), BinanceFuturesTimePath) {
		t.Errorf("the error must name the endpoint that failed; got %q", err)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
