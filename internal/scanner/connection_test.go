package scanner

import (
	"context"
	"runtime"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
)

// The receive stamp comes from the connector, which took it at the socket read.
// Re-taking it here would restart the clock after the message had already waited
// in a 1000-deep channel, so a backed-up scanner would report prices seconds old
// as freshly received - the staleness filter measuring its own dispatch lag
// instead of the data's age.
func TestUpdatePrice_KeepsTheStampTheConnectorTookAtTheSocket(t *testing.T) {
	s := New([]string{"BTCUSDT"})
	dequeuedAt := time.Now()
	s.now = func() time.Time { return dequeuedAt }

	readAt := dequeuedAt.Add(-3 * time.Second) // three seconds queued behind others
	s.updatePrice(exchanges.PriceData{
		Symbol: "BTCUSDT", Source: "binance_futures", Price: 65000, RecvAt: readAt,
	})

	s.pricesMutex.RLock()
	point := s.prices["BTCUSDT"]["binance_futures"]
	s.pricesMutex.RUnlock()

	if !point.RecvAt.Equal(readAt) {
		t.Errorf("RecvAt = %s, want the connector's stamp %s", point.RecvAt, readAt)
	}
	if point.RecvAt.Equal(dequeuedAt) {
		t.Error("the scanner re-stamped at the dequeue, which hides however long the message waited")
	}
}

// Data that never crossed a socket - a test writing straight into a channel -
// carries no stamp, and the scanner's own clock is then the best available.
// Leaving it zero would make the price permanently "unknown" instead of live.
func TestUpdatePrice_FallsBackToItsOwnClockWhenNothingStampedTheMessage(t *testing.T) {
	s := New([]string{"BTCUSDT"})
	now := time.Now()
	s.now = func() time.Time { return now }

	s.updatePrice(exchanges.PriceData{Symbol: "BTCUSDT", Source: "binance_futures", Price: 65000})

	s.pricesMutex.RLock()
	point := s.prices["BTCUSDT"]["binance_futures"]
	s.pricesMutex.RUnlock()

	if !point.RecvAt.Equal(now) {
		t.Errorf("RecvAt = %s, want the scanner clock %s", point.RecvAt, now)
	}
}

// Counting the first connection would report every venue as having reconnected
// once before anything went wrong, which is precisely the noise that makes the
// number useless for finding the one venue that actually flapped overnight.
func TestApplyConnEvent_TheFirstConnectionIsNotAReconnect(t *testing.T) {
	s := New([]string{"BTCUSDT"})
	at := time.Now()

	s.applyConnEvent(exchanges.ConnEvent{Source: "okx_futures", State: exchanges.ConnConnected, At: at})

	conn := s.snapshotConn()["okx_futures"]
	if conn.ReconnectCount != 0 {
		t.Errorf("reconnect count = %d after the first connection, want 0", conn.ReconnectCount)
	}
	if !conn.ConnectedSince.Equal(at) {
		t.Errorf("ConnectedSince = %s, want %s", conn.ConnectedSince, at)
	}
}

func TestApplyConnEvent_CountsReconnectsAndRestartsUptime(t *testing.T) {
	s := New([]string{"BTCUSDT"})
	first := time.Now()
	second := first.Add(time.Hour)

	s.applyConnEvent(exchanges.ConnEvent{Source: "okx_futures", State: exchanges.ConnConnected, At: first})
	s.applyConnEvent(exchanges.ConnEvent{Source: "okx_futures", State: exchanges.ConnReconnecting, At: second})

	// Uptime measures the CURRENT unbroken connection. Carrying the old start
	// time through an outage would report an hour of uptime for a socket that
	// has just died.
	if since := s.snapshotConn()["okx_futures"].ConnectedSince; !since.IsZero() {
		t.Errorf("ConnectedSince = %s while reconnecting, want zero", since)
	}

	s.applyConnEvent(exchanges.ConnEvent{Source: "okx_futures", State: exchanges.ConnConnected, At: second})

	conn := s.snapshotConn()["okx_futures"]
	if conn.ReconnectCount != 1 {
		t.Errorf("reconnect count = %d, want 1", conn.ReconnectCount)
	}
	if !conn.ConnectedSince.Equal(second) {
		t.Errorf("ConnectedSince = %s, want the new connection's start %s", conn.ConnectedSince, second)
	}
}

// The connector knows things silence cannot: a venue can become unreachable a
// second after its last tick, and the inference would still call it healthy for
// another 45.
func TestResolveSourceState_TheConnectorKnowsBeforeTheSilenceDoes(t *testing.T) {
	reconnecting := sourceConn{State: exchanges.ConnReconnecting}

	if got := resolveSourceState(reconnecting, stateConnected); got != stateReconnecting {
		t.Errorf("state = %q, want reconnecting: the connector is dialling again", got)
	}
}

// And silence knows things the connector cannot. This is the failure that hides:
// a subscription the venue quietly dropped leaves a socket that is genuinely
// open and healthy by every measure the connector has, and delivers nothing
// ever again.
func TestResolveSourceState_AnOpenSocketDeliveringNothingIsStillDead(t *testing.T) {
	connected := sourceConn{State: exchanges.ConnConnected, ConnectedSince: time.Now()}

	if got := resolveSourceState(connected, stateDisconnected); got != stateDisconnected {
		t.Errorf("state = %q, want disconnected: the socket is open but nothing comes through it", got)
	}
	if got := resolveSourceState(connected, stateConnected); got != stateConnected {
		t.Errorf("state = %q, want connected", got)
	}
}

// Before a connector has said anything - at start-up, or for a source whose
// connector was never started - the step 1.1 inference is all there is.
func TestResolveSourceState_FallsBackToTheInferenceWhenNothingWasReported(t *testing.T) {
	for _, inferred := range []string{stateConnected, stateDisconnected, stateUnknown} {
		if got := resolveSourceState(sourceConn{}, inferred); got != inferred {
			t.Errorf("state = %q with nothing reported, want the inference %q", got, inferred)
		}
	}
}

func TestNewWireSourceStatus_PublishesUptimeAndReconnectCount(t *testing.T) {
	now := time.Now()
	conn := sourceConn{
		State:          exchanges.ConnConnected,
		ConnectedSince: now.Add(-90 * time.Second),
		ReconnectCount: 3,
	}

	status := newWireSourceStatus("binance_futures", now, conn, now.Add(-time.Hour), now)

	if status.State != stateConnected {
		t.Errorf("state = %q, want connected", status.State)
	}
	if status.ReconnectCount != 3 {
		t.Errorf("reconnect_count = %d, want 3", status.ReconnectCount)
	}
	if status.UptimeSec != 90 {
		t.Errorf("uptime_sec = %d, want 90", status.UptimeSec)
	}
}

// A socket's age reported beside a state of disconnected reads as a
// contradiction, and that pairing is reachable: the socket is open, the feed has
// gone silent.
func TestNewWireSourceStatus_ReportsNoUptimeWhileDisconnected(t *testing.T) {
	now := time.Now()
	conn := sourceConn{
		State:          exchanges.ConnConnected,
		ConnectedSince: now.Add(-time.Hour),
		ReconnectCount: 1,
	}

	// Last message far beyond the disconnect threshold: the inference overrides.
	status := newWireSourceStatus("binance_futures", now.Add(-time.Hour), conn, now.Add(-2*time.Hour), now)

	if status.State != stateDisconnected {
		t.Fatalf("state = %q, want disconnected", status.State)
	}
	if status.UptimeSec != 0 {
		t.Errorf("uptime_sec = %d beside a disconnected state, want 0", status.UptimeSec)
	}
	// The count is history and survives: it is what says this venue has been
	// flapping.
	if status.ReconnectCount != 1 {
		t.Errorf("reconnect_count = %d, want 1", status.ReconnectCount)
	}
}

// The acceptance criterion at this end. Before step 1.5 these goroutines
// ranged over channels nobody closed and ran until the process died - which is
// also why a test could not stop the one it started, and a leaked goroutine read
// a package-level registry the next test was rewriting.
func TestRun_EveryGoroutineStopsWhenTheContextIsCancelled(t *testing.T) {
	settle := func() {
		for i := 0; i < 50; i++ {
			runtime.Gosched()
			time.Sleep(10 * time.Millisecond)
		}
	}

	settle()
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	s := New([]string{"BTCUSDT"})
	s.Run(ctx)

	settle()
	if during := runtime.NumGoroutine(); during <= before {
		t.Fatalf("goroutine count %d did not rise above %d; Run started nothing", during, before)
	}

	cancel()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("%d goroutines still running 5s after cancellation, started from %d",
		runtime.NumGoroutine(), before)
}

// A source whose FIRST dial fails reports reconnecting before it ever reports
// connected. Keying the counter on "has reported before" would call that first
// working connection a reconnect and put a 1 beside every venue that was merely
// slow to come up.
func TestApplyConnEvent_AFailedFirstDialDoesNotMakeTheFirstConnectionAReconnect(t *testing.T) {
	s := New([]string{"BTCUSDT"})
	at := time.Now()

	s.applyConnEvent(exchanges.ConnEvent{Source: "pyth", State: exchanges.ConnReconnecting, At: at})
	s.applyConnEvent(exchanges.ConnEvent{Source: "pyth", State: exchanges.ConnReconnecting, At: at.Add(time.Second)})
	s.applyConnEvent(exchanges.ConnEvent{Source: "pyth", State: exchanges.ConnConnected, At: at.Add(2 * time.Second)})

	if got := s.snapshotConn()["pyth"].ReconnectCount; got != 0 {
		t.Errorf("reconnect count = %d after a retried first connection, want 0", got)
	}

	// And the one after that is a genuine reconnect.
	s.applyConnEvent(exchanges.ConnEvent{Source: "pyth", State: exchanges.ConnReconnecting, At: at.Add(3 * time.Second)})
	s.applyConnEvent(exchanges.ConnEvent{Source: "pyth", State: exchanges.ConnConnected, At: at.Add(4 * time.Second)})

	if got := s.snapshotConn()["pyth"].ReconnectCount; got != 1 {
		t.Errorf("reconnect count = %d, want 1", got)
	}
}
