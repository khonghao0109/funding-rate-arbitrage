package exchanges

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// This file holds the connection lifecycle every WebSocket connector shares:
// dial, subscribe, read, detect death, back off, reconnect - and stop when the
// context is cancelled.
//
// Before step 1.5 each of the nine connectors carried its own copy of that loop,
// and every copy had the same three defects: a fixed 2 or 5 second sleep that
// hammered a venue that was down, no read deadline (so a half-open socket that
// never delivers another byte was indistinguishable from a quiet market and the
// connector waited on it forever), and no way to stop. Nine copies also meant
// nine chances for a fix to be applied eight times.
//
// A connector now supplies only what is venue-specific: the URL, how to
// subscribe, how to keep alive, and how to parse a frame.

const (
	// dialTimeout bounds the handshake. Without it a venue that accepts the TCP
	// connection and never completes the upgrade holds the connector forever.
	dialTimeout = 10 * time.Second

	// writeTimeout bounds a subscribe or a ping write.
	writeTimeout = 10 * time.Second

	// backoffMin and backoffMax bound the reconnect delay: 2s, 4s, 8s, 16s,
	// 32s, then 60s for as long as the venue stays down. The fixed sleep this
	// replaces retried a dead venue every 2 seconds indefinitely - 43,200 dials
	// a day, which is how an outage turns into a rate-limit ban that outlives
	// it.
	backoffMin = 2 * time.Second
	backoffMax = 60 * time.Second

	// HealthySession is how long a connection must last to count as genuinely
	// established, resetting the backoff. A venue that is rate limiting accepts
	// the socket and drops it immediately; resetting on connect alone would let
	// that flap at full speed forever, which is the same failure as no backoff
	// at all.
	//
	// It must stay clear of defaultReadTimeout. Equal to it, a venue that accepts
	// connections and then says nothing would be killed by the read deadline at
	// exactly the qualifying duration, reset the backoff every time, and be
	// re-dialled every ~60s forever instead of escalating to the ceiling. A
	// session must also have DELIVERED something to qualify - see RunStream -
	// because a socket that carried no data was never working, however long it
	// stayed open.
	HealthySession = 2 * defaultReadTimeout

	// defaultReadTimeout is how long a socket may deliver NOTHING - no data, no
	// pong, no server ping - before it is treated as dead.
	//
	// It must stay comfortably above twice the ping interval: our own ping
	// extends the deadline every pingEvery, so a threshold below 2x would kill a
	// healthy socket over one lost pong.
	defaultReadTimeout = 60 * time.Second

	// defaultPingEvery is the client-side keepalive interval for a venue with no
	// documented requirement of its own. It is below every documented idle
	// timeout the survey found (OKX 30s is the shortest).
	defaultPingEvery = 20 * time.Second
)

// ConnState is what a connector knows about its own socket - as opposed to what
// the scanner infers from silence. Both are needed: this catches a venue that is
// unreachable, silence catches a socket that stays open and stops delivering.
type ConnState int

const (
	// ConnConnected is sent once per successful session, after subscribing.
	ConnConnected ConnState = iota + 1
	// ConnReconnecting is sent when a session ends and another will be tried.
	ConnReconnecting
	// ConnDisconnected is sent when the connector stops for good (ctx cancelled).
	ConnDisconnected
)

func (s ConnState) String() string {
	switch s {
	case ConnConnected:
		return "connected"
	case ConnReconnecting:
		return "reconnecting"
	case ConnDisconnected:
		return "disconnected"
	default:
		return "unknown"
	}
}

// ConnEvent is one connection-state transition, reported by the connector.
type ConnEvent struct {
	Source string
	State  ConnState
	At     time.Time
}

// backoff produces the reconnect delay sequence.
type Backoff struct {
	current time.Duration
}

func NewBackoff() *Backoff {
	return &Backoff{current: backoffMin}
}

// next returns the delay to wait before the next attempt and advances the
// sequence, doubling up to the ceiling.
func (b *Backoff) next() time.Duration {
	delay := b.current
	if b.current < backoffMax {
		b.current *= 2
		if b.current > backoffMax {
			b.current = backoffMax
		}
	}
	return delay
}

func (b *Backoff) Reset() {
	b.current = backoffMin
}

// shouldResetBackoff reports whether a finished session counts as a real
// connection, which lets the next outage start its delays from the bottom.
//
// Both conditions matter. Duration alone is not enough: a socket accepted and
// then ignored dies at exactly defaultReadTimeout, and if that qualified, a
// venue that stops speaking would be re-dialled at a fixed interval forever
// instead of escalating to the ceiling. Frames alone are not enough either: a
// venue that sends one frame and drops the connection is still flapping.
//
// dataFrames counts only the frames the venue's handler turned into a message
// on a feed channel. Until 2026-09-12 this counted EVERY frame off the socket,
// keepalive replies included — and Bybit, OKX and Hyperliquid answer a
// keepalive with an ordinary data message, so on those three a session carrying
// nothing but pongs qualified as healthy and reset the backoff forever. That
// was recorded as debt in docs/PLAN.md step 1.6 and paid when bybit_spot spent
// 19 hours in exactly that state; Handle now reports what it produced.
func shouldResetBackoff(dataFrames int64, lasted time.Duration) bool {
	return dataFrames > 0 && lasted >= HealthySession
}

// wait sleeps for the next delay, or returns false immediately if the context is
// cancelled. A plain time.Sleep here is what made shutdown take up to a minute:
// the connector would be asleep in the retry gap and could not be told to stop.
func (b *Backoff) Wait(ctx context.Context) bool {
	timer := time.NewTimer(b.next())
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Subscriber is all a Subscribe function needs from a socket: every one of the
// six connectors that has a subscription sends it with WriteJSON.
// *websocket.Conn satisfies it.
type Subscriber interface {
	WriteJSON(v any) error
}

// StreamConfig is everything venue-specific about one WebSocket feed.
type StreamConfig struct {
	// Source is the configured source name, used for logs and ConnEvents.
	Source string
	URL    string

	// Subscribe runs once per connection, before any read. It is also where a
	// connector resets per-connection state: anything cached from the previous
	// socket (an assembled order book, for instance) describes a session that no
	// longer exists.
	//
	// It takes a Subscriber rather than a *websocket.Conn so a connector's
	// subscription can be exercised without dialling the venue. That matters
	// because the subscription is where a venue's own limits live — Bybit spot
	// refuses more than 10 topics per request and answers with silence — and a
	// limit nothing tests is a limit that comes back (docs/PLAN.md step 1.6).
	Subscribe func(conn Subscriber) error

	// Handle is given one frame and the instant it was read off the socket. It
	// reports whether the frame produced at least one message on a feed
	// channel — which is what separates market data from a keepalive reply, a
	// subscribe acknowledgement, or an error the venue sent back.
	//
	// "Produced" means a Send* call accepted it. A frame this connector could
	// not use is not a fault: every connector speculatively decodes each frame
	// into several shapes, so false is the ordinary answer for most frames on
	// most venues. What matters is that SOMETHING returns true regularly.
	Handle func(raw []byte, recvAt time.Time) bool

	// Ping sends the venue's keepalive. nil means a protocol-level ping frame
	// (RFC 6455), which every compliant server answers with a pong. A venue that
	// documents its own application-level heartbeat supplies it here.
	Ping func(conn *websocket.Conn) error

	// PingEvery and ReadTimeout override the defaults for a venue whose
	// documented idle timeout demands it. ReadTimeout must exceed 2*PingEvery.
	PingEvery   time.Duration
	ReadTimeout time.Duration
}

func (c StreamConfig) pingEvery() time.Duration {
	if c.PingEvery > 0 {
		return c.PingEvery
	}
	return defaultPingEvery
}

func (c StreamConfig) readTimeout() time.Duration {
	if c.ReadTimeout > 0 {
		return c.ReadTimeout
	}
	return defaultReadTimeout
}

// streamDialer replaces websocket.DefaultDialer, which every connector used
// before step 1.5. Proxy is carried over deliberately: DefaultDialer sets it and
// dropping it would make HTTPS_PROXY stop working for all nine WebSocket
// connectors while Pyth, on http.DefaultClient, went on honouring it.
var streamDialer = &websocket.Dialer{
	Proxy:            http.ProxyFromEnvironment,
	HandshakeTimeout: dialTimeout,
}

// ErrDataSilence ends a session that kept answering while delivering nothing
// usable. It is a sentinel so a caller — and a test — can tell this apart from
// an ordinary read timeout, which means the socket went quiet altogether.
//
// The two are different failures. A read timeout is a socket that died; this is
// a socket that is alive, answers every keepalive, and is subscribed to
// nothing. The second is the one that hides, because every other signal the
// lifecycle has says the venue is healthy.
var ErrDataSilence = errors.New("socket kept answering but delivered no usable data")

// RunStream connects, reads until the connection dies, then reconnects with
// exponential backoff - until the context is cancelled.
//
// It returns only on cancellation, so a connector's whole body is a call to it.
func RunStream(f Feeds, cfg StreamConfig) {
	retry := NewBackoff()

	for {
		if f.Ctx.Err() != nil {
			log.Printf("%s: stopped", cfg.Source)
			f.ReportConn(cfg.Source, ConnDisconnected)
			return
		}

		startedAt := time.Now()
		dataFrames, err := runSession(f, cfg)
		lasted := time.Since(startedAt)

		if f.Ctx.Err() != nil {
			// Cancellation closes the socket underneath the reader, so the error
			// this session ended with describes the shutdown, not a fault.
			log.Printf("%s: stopped", cfg.Source)
			f.ReportConn(cfg.Source, ConnDisconnected)
			return
		}

		if shouldResetBackoff(dataFrames, lasted) {
			retry.Reset()
		}

		f.ReportConn(cfg.Source, ConnReconnecting)
		log.Printf("%s: connection lost after %s: %v", cfg.Source, lasted.Round(time.Second), err)

		if !retry.Wait(f.Ctx) {
			// Cancelled while waiting out the backoff. Logged with the same
			// wording as the other exit so an unattended run's log accounts for
			// every connector, including the ones that were asleep.
			log.Printf("%s: stopped", cfg.Source)
			f.ReportConn(cfg.Source, ConnDisconnected)
			return
		}
	}
}

// runSession owns exactly one connection, from dial to death. It returns how
// many frames the connection turned into DATA, which is what tells a real
// connection apart both from a socket that was accepted and then ignored and
// from one that answers every keepalive with nothing behind it.
func runSession(f Feeds, cfg StreamConfig) (int64, error) {
	var framesRead, dataFrames int64

	conn, _, err := streamDialer.DialContext(f.Ctx, cfg.URL, nil)
	if err != nil {
		return dataFrames, fmt.Errorf("dial: %w", err)
	}

	// gorilla has no context-aware read, and ReadMessage blocks until a frame,
	// an error or the read deadline. Closing the socket from another goroutine
	// is the supported way to interrupt it - Close may be called concurrently
	// with every other method - and it is what makes cancellation take
	// milliseconds instead of up to a full read timeout.
	sessionDone := make(chan struct{})
	defer close(sessionDone)
	go func() {
		select {
		case <-f.Ctx.Done():
		case <-sessionDone:
		}
		conn.Close()
	}()

	readTimeout := cfg.readTimeout()
	dataSilence := f.DataSilenceTimeout

	// Two clocks, one deadline. readTimeout measures silence of any kind and
	// catches a dead socket; dataSilence measures silence of USABLE data and
	// catches a live socket with nothing behind it. The socket gets whichever
	// expires first, so no second goroutine and no extra lock: every write to
	// lastDataAt below happens on this same reading goroutine, including the
	// ones inside the pong and ping handlers, which gorilla runs inside
	// ReadMessage.
	//
	// lastDataAt starts at the session's beginning rather than at zero, so a
	// session that never delivers anything is killed dataSilence after CONNECT
	// — which is the bybit_spot case exactly.
	lastDataAt := time.Now()
	deadlineFrom := func(now time.Time) time.Time {
		deadline := now.Add(readTimeout)
		if dataSilence > 0 {
			if byData := lastDataAt.Add(dataSilence); byData.Before(deadline) {
				deadline = byData
			}
		}
		return deadline
	}
	extendDeadline := func() error {
		return conn.SetReadDeadline(deadlineFrom(time.Now()))
	}
	if err := extendDeadline(); err != nil {
		return dataFrames, fmt.Errorf("set read deadline: %w", err)
	}

	// A pong is proof the socket is alive even though no data crossed it, so it
	// has to count as activity. Gorilla's default pong handler does nothing at
	// all, which would let the deadline expire on a healthy but quiet feed.
	conn.SetPongHandler(func(string) error { return extendDeadline() })

	// Likewise for a server-initiated ping. Gorilla's default ping handler
	// replies with a pong but does NOT touch the read deadline, and control
	// frames are consumed inside ReadMessage without returning to the caller -
	// so on a venue that keeps the connection alive by pinging US (Paradex pings
	// every 55s and expects a pong within 5), the default would drop a perfectly
	// healthy socket every readTimeout.
	conn.SetPingHandler(func(appData string) error {
		_ = extendDeadline()
		err := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(writeTimeout))
		if err == websocket.ErrCloseSent {
			return nil
		}
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return nil
		}
		return err
	})

	if cfg.Subscribe != nil {
		if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			return dataFrames, fmt.Errorf("set write deadline: %w", err)
		}
		if err := cfg.Subscribe(conn); err != nil {
			return dataFrames, fmt.Errorf("subscribe: %w", err)
		}
	}

	log.Printf("%s: connected", cfg.Source)
	f.ReportConn(cfg.Source, ConnConnected)

	// Started only after Subscribe has finished writing: gorilla allows one
	// writer at a time, and WriteControl - which is what a protocol ping and the
	// pong handler use - is the one exception that may run concurrently with it.
	go pingLoop(f.Ctx, sessionDone, conn, cfg)

	for {
		_, raw, err := conn.ReadMessage()

		// THE receive stamp. This is the only place in the WebSocket path where
		// a message's arrival time is recorded, and it is taken before the
		// message is parsed and before it is queued, because both can be delayed
		// by an arbitrary amount: the ingestion channels hold 1000 messages, and
		// a stamp taken at the far end measures our own queue, not the venue's
		// silence. That is the debt step 1.1 recorded. See CLAUDE.md rule 13.
		recvAt := time.Now()

		if err != nil {
			// A timeout with the DATA clock expired is the failure that hides:
			// the socket was alive the whole time. Name it, so the log says
			// which of the two silences ended the session and the caller can
			// test for it.
			var netErr net.Error
			if dataSilence > 0 && errors.As(err, &netErr) && netErr.Timeout() &&
				time.Since(lastDataAt) >= dataSilence {
				return dataFrames, fmt.Errorf("%w in the last %s: the session read %d frames and %d of them "+
					"carried data — the subscription is refused, expired or was dropped; re-dialling to re-subscribe",
					ErrDataSilence, dataSilence, framesRead, dataFrames)
			}
			return dataFrames, err
		}
		framesRead++

		// Handle runs BEFORE the deadline is extended so the new deadline can
		// take this frame's verdict into account, and it is still measured from
		// recvAt rather than from now: a handler that blocked on a full channel
		// must not buy the socket extra silence. The cost of that choice is a
		// label, not a teardown: a handler blocked longer than dataSilence ends
		// the session it was already ending (the pre-existing readTimeout would
		// have done it) and the error says ErrDataSilence although the frame
		// did carry data. A backed-up scanner is a real problem either way.
		if cfg.Handle(raw, recvAt) {
			dataFrames++
			lastDataAt = recvAt
		}

		if err := conn.SetReadDeadline(deadlineFrom(recvAt)); err != nil {
			return dataFrames, fmt.Errorf("extend read deadline: %w", err)
		}
	}
}

// pingLoop keeps the connection alive until the session ends.
func pingLoop(ctx context.Context, sessionDone <-chan struct{}, conn *websocket.Conn, cfg StreamConfig) {
	ticker := time.NewTicker(cfg.pingEvery())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sessionDone:
			return
		case <-ticker.C:
			var err error
			if cfg.Ping != nil {
				if err = conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err == nil {
					err = cfg.Ping(conn)
				}
			} else {
				err = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout))
			}
			if err != nil {
				// The read side sees the same broken socket and ends the
				// session; saying so twice would double every log line.
				return
			}
		}
	}
}

// JSONPing sends one JSON keepalive message. Venues that document an
// application-level heartbeat use it instead of a protocol ping frame.
func JSONPing(message any) func(*websocket.Conn) error {
	return func(conn *websocket.Conn) error {
		return conn.WriteJSON(message)
	}
}

// TextPing sends a raw text keepalive. OKX documents the literal string "ping",
// which is not JSON and not a protocol ping frame.
func TextPing(payload string) func(*websocket.Conn) error {
	return func(conn *websocket.Conn) error {
		return conn.WriteMessage(websocket.TextMessage, []byte(payload))
	}
}

// Decode unmarshals a frame, reporting nothing on failure. Every connector
// speculatively decodes each frame into several shapes to find out what it is,
// so a failure here is the normal case, not an error.
func Decode(raw []byte, into any) bool {
	return json.Unmarshal(raw, into) == nil
}
