package exchanges

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// These test the connection lifecycle, not any venue's payloads - connector
// parsing gets golden tests from captured payloads in step 1.6. What is exercised
// here is what step 1.5 exists to fix: a stop that actually stops, a backoff that
// backs off, a read deadline that notices a socket which stopped delivering, and
// a receive stamp taken before the queue rather than after it.

var testUpgrader = websocket.Upgrader{}

// wsTestServer starts a WebSocket server and returns its ws:// URL. handle runs
// once per accepted connection and owns the socket.
func wsTestServer(t *testing.T, handle func(conn *websocket.Conn)) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		handle(conn)
	}))
	t.Cleanup(server.Close)

	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// testFeeds is a Feeds with buffered channels and a cancel function.
func testFeeds(t *testing.T) (Feeds, context.CancelFunc, chan OrderbookData, chan ConnEvent) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	orderbooks := make(chan OrderbookData, 64)
	events := make(chan ConnEvent, 64)

	return Feeds{
		Ctx:       ctx,
		Price:     make(chan PriceData, 64),
		Orderbook: orderbooks,
		Trade:     make(chan TradeData, 64),
		Funding:   make(chan FundingData, 64),
		Conn:      events,
	}, cancel, orderbooks, events
}

// waitForState blocks until the given state is reported, or fails.
func waitForState(t *testing.T, events <-chan ConnEvent, want ConnState, within time.Duration) {
	t.Helper()

	deadline := time.After(within)
	for {
		select {
		case event := <-events:
			if event.State == want {
				return
			}
		case <-deadline:
			t.Fatalf("no %s event within %s", want, within)
		}
	}
}

func TestBackoff_DoublesUpToTheCeiling(t *testing.T) {
	retry := NewBackoff()

	want := []time.Duration{2, 4, 8, 16, 32, 60, 60, 60}
	for i, wantSec := range want {
		got := retry.next()
		if got != wantSec*time.Second {
			t.Errorf("attempt %d: delay = %s, want %s", i+1, got, wantSec*time.Second)
		}
	}
}

// A venue that comes back must not be punished for the outage that just ended.
func TestBackoff_ResetReturnsToTheFloor(t *testing.T) {
	retry := NewBackoff()
	for i := 0; i < 5; i++ {
		retry.next()
	}

	retry.Reset()

	if got := retry.next(); got != backoffMin {
		t.Errorf("delay after reset = %s, want %s", got, backoffMin)
	}
}

// The fixed time.Sleep this replaces is the reason shutdown was unbounded: a
// connector asleep in the retry gap could not be told to stop.
func TestBackoffWait_ReturnsImmediatelyWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if NewBackoff().Wait(ctx) {
		t.Error("wait returned true on a cancelled context")
	}
	if elapsed := time.Since(start); elapsed > backoffMin/2 {
		t.Errorf("wait took %s on a cancelled context; it must not sleep", elapsed)
	}
}

// A full ingestion channel is the normal state during shutdown, once the
// scanner's consumers have stopped draining. A connector blocked on a plain
// channel send there would never see the cancellation.
func TestFeedsSend_GivesUpWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Unbuffered and unread: every send blocks until the context says stop.
	feeds := Feeds{
		Ctx:       ctx,
		Price:     make(chan PriceData),
		Orderbook: make(chan OrderbookData),
		Trade:     make(chan TradeData),
		Funding:   make(chan FundingData),
	}

	for name, send := range map[string]func() bool{
		"price":     func() bool { return feeds.SendPrice(PriceData{}) },
		"orderbook": func() bool { return feeds.SendOrderbook(OrderbookData{}) },
		"trade":     func() bool { return feeds.SendTrade(TradeData{}) },
		"funding":   func() bool { return feeds.SendFunding(FundingData{}) },
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan bool, 1)
			go func() { done <- send() }()

			select {
			case <-done:
				t.Fatal("send returned while the channel was blocked and the context live")
			case <-time.After(50 * time.Millisecond):
			}

			cancel()

			select {
			case delivered := <-done:
				if delivered {
					t.Error("send reported delivery of a message nobody received")
				}
			case <-time.After(time.Second):
				t.Fatal("send did not return after cancellation")
			}

			ctx, cancel = context.WithCancel(context.Background())
			feeds.Ctx = ctx
		})
	}
	cancel()
}

// The whole point of moving the stamp: the ingestion channels hold 1000
// messages, and a stamp taken when the scanner dequeues restarts the clock at
// the far end - so a backed-up scanner reports every venue as freshly updated
// while serving prices that have been waiting in a queue.
func TestRunStream_StampsReceiveTimeAtTheSocketReadNotAtTheQueue(t *testing.T) {
	url := wsTestServer(t, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"hello":1}`))
		time.Sleep(2 * time.Second) // hold the connection open
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Unbuffered: the connector's send blocks until this test reads, which is
	// the queue delay being simulated.
	orderbooks := make(chan OrderbookData)
	feeds := Feeds{Ctx: ctx, Orderbook: orderbooks}

	go RunStream(feeds, StreamConfig{
		Source: "test",
		URL:    url,
		Handle: func(raw []byte, recvAt time.Time) bool {
			return feeds.SendOrderbook(OrderbookData{Symbol: "BTCUSDT", RecvAt: recvAt})
		},
	})

	const queueDelay = 200 * time.Millisecond
	time.Sleep(queueDelay)

	select {
	case data := <-orderbooks:
		if data.RecvAt.IsZero() {
			t.Fatal("RecvAt was not stamped")
		}
		// Stamped at the read, RecvAt is the whole queue delay old by the time
		// the consumer sees it. Stamped at the queue - what step 1.1 did - it
		// would be near zero, which is what this distinguishes. The margin
		// absorbs the dial and the first read.
		age := time.Since(data.RecvAt)
		if age < queueDelay*3/4 {
			t.Errorf("RecvAt is only %s old at dequeue, but the message waited %s in the queue: it was stamped at the dequeue, not at the read", age, queueDelay)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no message arrived")
	}
}

// The acceptance criterion: cancelling the context stops every connector
// cleanly. The hard case is this one - blocked in ReadMessage on a server that
// is alive and silent, which no read deadline will interrupt for a minute.
func TestRunStream_StopsPromptlyWhileBlockedOnASilentServer(t *testing.T) {
	url := wsTestServer(t, func(conn *websocket.Conn) {
		<-make(chan struct{}) // never speaks, never closes
	})

	feeds, cancel, _, events := testFeeds(t)

	stopped := make(chan struct{})
	go func() {
		RunStream(feeds, StreamConfig{Source: "test", URL: url, Handle: func([]byte, time.Time) bool { return false }})
		close(stopped)
	}()

	waitForState(t, events, ConnConnected, 3*time.Second)

	cancel()
	start := time.Now()

	select {
	case <-stopped:
		// Far tighter than the 5s the acceptance criterion allows: cancellation
		// closes the socket underneath the reader rather than waiting for the
		// read deadline.
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("RunStream took %s to stop, want well under the 5s budget", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunStream did not stop within the 5s acceptance budget")
	}
}

// A socket that stays open and stops delivering is indistinguishable from a
// quiet market without this, and the connector waits on it forever - which is
// what the old loop did.
func TestRunStream_ReconnectsWhenTheServerGoesSilent(t *testing.T) {
	var connections int32
	url := wsTestServer(t, func(conn *websocket.Conn) {
		atomic.AddInt32(&connections, 1)
		<-make(chan struct{})
	})

	feeds, cancel, _, events := testFeeds(t)
	defer cancel()

	go RunStream(feeds, StreamConfig{
		Source: "test",
		URL:    url,
		Handle: func([]byte, time.Time) bool { return false },
		// No keepalive, so nothing extends the deadline and it fires.
		PingEvery:   time.Hour,
		ReadTimeout: 200 * time.Millisecond,
	})

	waitForState(t, events, ConnConnected, 3*time.Second)
	waitForState(t, events, ConnReconnecting, 2*time.Second)

	if got := atomic.LoadInt32(&connections); got < 1 {
		t.Fatalf("server saw %d connections", got)
	}
}

// A venue that pings US keeps the socket alive, and that has to count as
// activity. Gorilla's default ping handler replies with a pong but leaves the
// read deadline alone, and control frames never surface from ReadMessage - so
// without the handler this asserts, a healthy Paradex socket (it pings every 55s
// and expects a pong within 5) would be dropped on every read timeout.
func TestRunSession_AServerPingKeepsTheConnectionAlive(t *testing.T) {
	url := wsTestServer(t, func(conn *websocket.Conn) {
		for i := 0; i < 20; i++ {
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(time.Second)
	})

	feeds, cancel, _, events := testFeeds(t)
	defer cancel()

	go RunStream(feeds, StreamConfig{
		Source:      "test",
		URL:         url,
		Handle:      func([]byte, time.Time) bool { return false },
		PingEvery:   time.Hour, // only the SERVER's ping can keep this alive
		ReadTimeout: 300 * time.Millisecond,
	})

	waitForState(t, events, ConnConnected, 3*time.Second)

	// Three read timeouts' worth of server pings and no data at all.
	select {
	case event := <-events:
		if event.State == ConnReconnecting {
			t.Fatal("the connection was dropped despite the server pinging it")
		}
	case <-time.After(900 * time.Millisecond):
	}
}

// A dead venue used to be retried every 2 seconds indefinitely - 43,200 dials a
// day, which turns an outage into a rate-limit ban that outlives it.
func TestRunStream_WaitsBeforeReconnectingAfterTheServerHangsUp(t *testing.T) {
	var connections int32
	url := wsTestServer(t, func(conn *websocket.Conn) {
		atomic.AddInt32(&connections, 1)
	})

	feeds, cancel, _, events := testFeeds(t)
	defer cancel()

	start := time.Now()
	go RunStream(feeds, StreamConfig{Source: "test", URL: url, Handle: func([]byte, time.Time) bool { return false }})

	// Two connections means one reconnect happened, and the gap between them is
	// the backoff.
	deadline := time.After(backoffMin + 3*time.Second)
	for atomic.LoadInt32(&connections) < 2 {
		select {
		case <-deadline:
			t.Fatalf("only %d connections; the connector did not reconnect", atomic.LoadInt32(&connections))
		case <-time.After(20 * time.Millisecond):
		}
	}

	if elapsed := time.Since(start); elapsed < backoffMin {
		t.Errorf("reconnected after %s, less than the %s floor: the backoff is not being applied", elapsed, backoffMin)
	}

	waitForState(t, events, ConnReconnecting, time.Second)
}

// A subscription that fails leaves a connected socket delivering nothing, which
// looks healthy. It has to end the session.
func TestRunSession_ASubscribeFailureEndsTheSession(t *testing.T) {
	url := wsTestServer(t, func(conn *websocket.Conn) {
		<-make(chan struct{})
	})

	feeds, cancel, _, events := testFeeds(t)
	defer cancel()

	var attempts int32
	go RunStream(feeds, StreamConfig{
		Source: "test",
		URL:    url,
		Subscribe: func(Subscriber) error {
			atomic.AddInt32(&attempts, 1)
			return context.DeadlineExceeded
		},
		Handle: func([]byte, time.Time) bool { return false },
	})

	waitForState(t, events, ConnReconnecting, 3*time.Second)

	if got := atomic.LoadInt32(&attempts); got == 0 {
		t.Fatal("Subscribe was never called")
	}
	// No connected event may be reported for a session that never subscribed.
	select {
	case event := <-events:
		if event.State == ConnConnected {
			t.Error("reported connected despite the subscription failing")
		}
	default:
	}
}

// Subscribe is where a connector drops what it cached from the previous socket.
// An order book assembled over a connection that no longer exists describes a
// session that ended.
func TestRunSession_SubscribeRunsOncePerConnection(t *testing.T) {
	var mu sync.Mutex
	var connections int
	url := wsTestServer(t, func(conn *websocket.Conn) {
		mu.Lock()
		connections++
		mu.Unlock()
	})

	feeds, cancel, _, _ := testFeeds(t)
	defer cancel()

	var subscribes int32
	go RunStream(feeds, StreamConfig{
		Source:    "test",
		URL:       url,
		Subscribe: func(Subscriber) error { atomic.AddInt32(&subscribes, 1); return nil },
		Handle:    func([]byte, time.Time) bool { return false },
	})

	deadline := time.After(backoffMin + 3*time.Second)
	for atomic.LoadInt32(&subscribes) < 2 {
		select {
		case <-deadline:
			t.Fatalf("Subscribe ran %d times across reconnects", atomic.LoadInt32(&subscribes))
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// A venue that accepts a connection and then says nothing is killed by the read
// deadline at exactly defaultReadTimeout. If that counted as a healthy session
// the backoff would reset every time and the venue would be re-dialled at a
// fixed interval forever, which is the failure backoff exists to prevent.
func TestShouldResetBackoff_ASilentSessionIsNotAHealthyOne(t *testing.T) {
	if HealthySession <= defaultReadTimeout {
		t.Errorf("HealthySession %s must outlast defaultReadTimeout %s, or a socket killed by the read deadline always qualifies",
			HealthySession, defaultReadTimeout)
	}

	cases := []struct {
		name       string
		dataFrames int64
		lasted     time.Duration
		want       bool
	}{
		{"delivered nothing for a long time", 0, HealthySession * 2, false},
		{"killed by the read deadline having said nothing", 0, defaultReadTimeout, false},
		{"delivered, but dropped immediately", 5, time.Second, false},
		{"delivered for a long time", 5, HealthySession, true},
		// The bybit_spot shape: frames all session long, none of them data.
		// Counting frames rather than data made this the "true" row and reset
		// the backoff on a socket subscribed to nothing (2026-09-12).
		{"answered keepalives for hours and delivered no data", 0, 19 * time.Hour, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldResetBackoff(tc.dataFrames, tc.lasted); got != tc.want {
				t.Errorf("shouldResetBackoff(%d data frames, %s) = %v, want %v",
					tc.dataFrames, tc.lasted, got, tc.want)
			}
		})
	}
}

// websocket.DefaultDialer, which every connector used before step 1.5, honours
// HTTPS_PROXY. Replacing it with a bare Dialer would silently take that away
// from all nine WebSocket connectors while Pyth, on http.DefaultClient, went on
// using it.
func TestStreamDialer_StillHonoursTheProxyEnvironment(t *testing.T) {
	if streamDialer.Proxy == nil {
		t.Error("streamDialer has no Proxy; HTTPS_PROXY would stop working for every WebSocket venue")
	}
	if streamDialer.HandshakeTimeout <= 0 {
		t.Error("streamDialer has no handshake timeout; a venue that never upgrades would hold the connector forever")
	}
}

// The failure this whole mechanism exists for, reproduced end to end: a socket
// that connects, answers every keepalive, and delivers no market data at all.
//
// It is not hypothetical. bybit_spot spent 19 hours in exactly this state
// during step-3.5 run 2 (2026-09-11 → 12) because a 26-arg subscribe exceeded
// Bybit spot's documented 10-arg ceiling and was refused as a whole; the
// connector never read the refusal, the pings kept the read deadline fresh, and
// the scanner recorded 0 price samples against 7,904 for every other source.
// Before this test the lifecycle had no signal that could tell the difference.
func TestRunSession_EndsASessionThatAnswersKeepalivesButDeliversNoData(t *testing.T) {
	var framesSent int32
	url := wsTestServer(t, func(conn *websocket.Conn) {
		// The server answers the keepalive the way Bybit, OKX and Hyperliquid
		// do: with an ordinary DATA frame, not a protocol pong. That detail is
		// the whole point. A server that merely stayed silent would be caught
		// by any deadline at all, and an earlier version of this test used one
		// - so decoupling lastDataAt from Handle's verdict left the suite
		// green while removing the semantic this change exists to add
		// (adversarial review 2026-09-12 proved it by mutation). Here frames
		// keep arriving, Handle keeps saying "not data", and only the data
		// clock can end it.
		go func() {
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for range ticker.C {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"op":"pong","success":true}`)); err != nil {
					return
				}
				atomic.AddInt32(&framesSent, 1)
			}
		}()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	feeds, cancel, _, _ := testFeeds(t)
	defer cancel()
	feeds.DataSilenceTimeout = 300 * time.Millisecond

	var handled int32
	dataFrames, err := runSession(feeds, StreamConfig{
		Source: "test",
		URL:    url,
		Handle: func([]byte, time.Time) bool {
			atomic.AddInt32(&handled, 1)
			return false // a keepalive reply is not market data
		},
		// The frame clock is kept deliberately alive and enormous: every one of
		// those frames refreshes it and it would not fire for ten seconds.
		PingEvery:   50 * time.Millisecond,
		ReadTimeout: 10 * time.Second,
	})

	if !errors.Is(err, ErrDataSilence) {
		t.Fatalf("session ended with %v, want ErrDataSilence — a socket whose only traffic is keepalive replies must not look healthy", err)
	}
	if got := atomic.LoadInt32(&handled); got < 2 {
		t.Fatalf("Handle saw %d frames; the test is not exercising the case it describes unless frames really kept arriving", got)
	}
	if dataFrames != 0 {
		t.Errorf("dataFrames = %d, want 0", dataFrames)
	}
	if shouldResetBackoff(dataFrames, time.Hour) {
		t.Error("a session that delivered no data reset the backoff; a permanently refused subscription would then be re-dialled forever at the floor")
	}
}

// The other half of the same rule: a subscription that WORKED and then died is
// measured from the last data, not from the connect. This is the shape the
// step-1.6 debt described - the venue drops a subscription silently mid-session
// - as opposed to the refusal above, which never delivers anything.
func TestRunSession_MeasuresDataSilenceFromTheLastDataNotFromTheConnect(t *testing.T) {
	url := wsTestServer(t, func(conn *websocket.Conn) {
		go func() {
			// Four frames over ~200ms, then silence while the socket stays up.
			for i := 0; i < 4; i++ {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"tick":1}`)); err != nil {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		}()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	feeds, cancel, _, _ := testFeeds(t)
	defer cancel()
	feeds.DataSilenceTimeout = 300 * time.Millisecond

	startedAt := time.Now()
	dataFrames, err := runSession(feeds, StreamConfig{
		Source:      "test",
		URL:         url,
		Handle:      func([]byte, time.Time) bool { return true },
		PingEvery:   50 * time.Millisecond,
		ReadTimeout: 10 * time.Second,
	})
	lasted := time.Since(startedAt)

	if !errors.Is(err, ErrDataSilence) {
		t.Fatalf("session ended with %v, want ErrDataSilence", err)
	}
	if dataFrames != 4 {
		t.Errorf("dataFrames = %d, want 4", dataFrames)
	}
	// Had the deadline been measured from the connect it would have fired at
	// 300ms, before the fourth frame. It must fire after the last one instead.
	if lasted < 400*time.Millisecond {
		t.Errorf("session lasted %s; the data clock was not restarted by the frames that did arrive", lasted)
	}
}

// A feed that keeps delivering must never be torn down by this, however short
// the deadline is next to its cadence. A check that reconnects a working venue
// is worse than no check: it would cost a snapshot and a resubscribe every few
// minutes on every source at once.
func TestRunSession_LeavesAFeedThatKeepsDeliveringAlone(t *testing.T) {
	serverDone := make(chan struct{})
	url := wsTestServer(t, func(conn *websocket.Conn) {
		deadline := time.Now().Add(700 * time.Millisecond)
		for time.Now().Before(deadline) {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"tick":1}`)); err != nil {
				break
			}
			time.Sleep(40 * time.Millisecond)
		}
		close(serverDone)
	})

	feeds, cancel, _, _ := testFeeds(t)
	defer cancel()
	feeds.DataSilenceTimeout = 300 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := runSession(feeds, StreamConfig{
			Source:      "test",
			URL:         url,
			Handle:      func([]byte, time.Time) bool { return true },
			PingEvery:   50 * time.Millisecond,
			ReadTimeout: 10 * time.Second,
		})
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("session ended after %v while the server was still delivering every 40ms", err)
	case <-serverDone:
	}
}

// Zero is off, which is what every harness that builds a Feeds by hand gets -
// including the testdata capture tool, which publishes nothing by design.
//
// What "off" means here is precisely the behaviour that hid bybit_spot: the
// server answers every ping, the pong refreshes the frame clock, and the
// session lives forever with nothing behind it. So this test asserts the OLD
// behaviour survives when the field is unset — it is the compatibility half of
// the pair above, not a second way of catching the bug.
func TestRunSession_DataSilenceZeroLeavesTheOldBehaviourExactly(t *testing.T) {
	url := wsTestServer(t, func(conn *websocket.Conn) {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	feeds, cancel, _, _ := testFeeds(t)
	defer cancel()
	// DataSilenceTimeout deliberately left at its zero value.

	done := make(chan error, 1)
	go func() {
		_, err := runSession(feeds, StreamConfig{
			Source:      "test",
			URL:         url,
			Handle:      func([]byte, time.Time) bool { return false },
			PingEvery:   50 * time.Millisecond,
			ReadTimeout: 10 * time.Second,
		})
		done <- err
	}()

	// Six times the deadline the other tests use. With the field set, this
	// session would have been torn down five times over.
	select {
	case err := <-done:
		t.Fatalf("session ended with %v; with DataSilenceTimeout unset nothing may end a socket that is still answering", err)
	case <-time.After(600 * time.Millisecond):
	}
}
