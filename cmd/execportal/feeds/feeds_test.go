package feeds

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The feeds are the only thing in the portal that talks to the step-3.5 gate,
// so the tests here are about what that conversation can cost the gate: a slow
// page must not slow it, nothing a page sends may reach it, and a page cannot
// steer what is fetched from it. Every upstream is an httptest server on
// loopback. The portal's own walls (Host, Origin, action header) are tested in
// cmd/execportal, in front of these handlers.

// fakeScanner is a WebSocket server shaped like cmd/scanner's /ws: it writes
// what the test gives it and records everything a client sends it.
type fakeScanner struct {
	srv      *httptest.Server
	accepted atomic.Int64
	received atomic.Int64
	paths    chan string
}

func newFakeScanner(t *testing.T, onConn func(c *websocket.Conn)) *fakeScanner {
	t.Helper()
	fs := &fakeScanner{paths: make(chan string, 64)}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case fs.paths <- r.URL.RequestURI():
		default:
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		fs.accepted.Add(1)
		defer c.Close()
		go func() {
			for {
				kind, _, err := c.ReadMessage()
				if err != nil {
					return
				}
				if kind == websocket.TextMessage || kind == websocket.BinaryMessage {
					fs.received.Add(1)
				}
			}
		}()
		onConn(c)
	}))
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *fakeScanner) addr() string { return strings.TrimPrefix(fs.srv.URL, "http://") }

// relayServer serves f's three handlers on their portal paths.
func relayServer(t *testing.T, f *Feeds, configure func(*http.Server)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/scanner/ws", f.ScannerSocket)
	mux.HandleFunc("/api/scanner/funding-history", f.ScannerHistory)
	mux.HandleFunc("/api/paper/ledger", f.PaperLedger)
	mux.HandleFunc("/api/scanner/cross-radar", f.ScannerCrossRadar)
	mux.HandleFunc("/api/scanner/cross-radar/events", f.ScannerCrossEvents)
	srv := httptest.NewUnstartedServer(mux)
	if configure != nil {
		configure(srv.Config)
	}
	srv.Start()
	t.Cleanup(func() {
		f.CloseAll()
		srv.Close()
	})
	return srv
}

func dialRelay(srv *httptest.Server) (*websocket.Conn, *http.Response, error) {
	return websocket.DefaultDialer.Dial("ws://"+strings.TrimPrefix(srv.URL, "http://")+"/api/scanner/ws", nil)
}

func get(t *testing.T, srv *httptest.Server, path string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String(), resp.Header
}

func TestCheckAddr(t *testing.T) {
	for raw, want := range map[string]string{
		"127.0.0.1:8085": "127.0.0.1:8085",
		" [::1]:8086 ":   "[::1]:8086",
		"":               "",
	} {
		got, err := CheckAddr("scanner-addr", raw, "8087")
		if err != nil || got != want {
			t.Errorf("CheckAddr(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, bad := range []string{"localhost:8085", "192.168.1.5:8085", "0.0.0.0:8085", "8085", "127.0.0.1", "127.0.0.1:0",
		"127.0.0.1:99999", "127.0.0.1:8087", "127.0.0.1:08087", "example.com:80", "127.0.0.1:abc", "[fe80::1%lo0]:8085"} {
		if _, err := CheckAddr("scanner-addr", bad, "8087"); err == nil {
			t.Errorf("CheckAddr(%q) accepted", bad)
		}
	}
}

// Frames reach the page byte for byte and in order, and what the page sends
// reaches nobody.
func TestScannerRelay_ForwardsFramesVerbatimAndNothingBack(t *testing.T) {
	frames := []string{`{"type":"meta","v":1}`, `{"type":"funding","v":1,"funding":{}}`, `{"type":"prices","v":1,"x":"ü✓"}`}
	release := make(chan struct{})
	upstream := newFakeScanner(t, func(c *websocket.Conn) {
		for _, f := range frames {
			if err := c.WriteMessage(websocket.TextMessage, []byte(f)); err != nil {
				return
			}
		}
		<-release
	})
	defer close(release)
	srv := relayServer(t, New(upstream.addr(), "", nil), nil)

	browser, _, err := dialRelay(srv)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer browser.Close()
	_ = browser.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i, want := range frames {
		kind, got, err := browser.ReadMessage()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if kind != websocket.TextMessage || !bytes.Equal(got, []byte(want)) {
			t.Errorf("frame %d = %q, want %q verbatim", i, got, want)
		}
	}
	if path := <-upstream.paths; path != "/ws" {
		t.Errorf("relay dialed %q, want the scanner's fixed /ws", path)
	}

	for _, msg := range []string{`{"subscribe":"x"}`, "hello"} {
		if err := browser.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := upstream.received.Load(); n != 0 {
		t.Errorf("the scanner received %d data frames from the page — the relay must be one-way", n)
	}
}

// THE property: a page that stops reading must never make the scanner's write
// wait. The fake scanner writes like the real one (2-second deadline per
// write); every write has to finish fast or fail fast, and none may time out.
func TestScannerRelay_ASlowBrowserNeverStallsTheScanner(t *testing.T) {
	type result struct {
		slowest  time.Duration
		timeouts int
		written  int
	}
	done := make(chan result, 1)
	payload := []byte(`{"type":"spreads","pad":"` + strings.Repeat("x", 64<<10) + `"}`)
	upstream := newFakeScanner(t, func(c *websocket.Conn) {
		var r result
		for i := 0; i < 4000; i++ {
			_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
			start := time.Now()
			err := c.WriteMessage(websocket.TextMessage, payload)
			if d := time.Since(start); d > r.slowest {
				r.slowest = d
			}
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					r.timeouts++
				}
				break
			}
			r.written++
		}
		done <- r
	})
	f := New(upstream.addr(), "", nil)
	f.queueFrames = 8
	srv := relayServer(t, f, nil)

	browser, _, err := dialRelay(srv)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer browser.Close()
	// The browser reads nothing at all.

	select {
	case r := <-done:
		if r.timeouts > 0 {
			t.Fatalf("a scanner write timed out after %d frames — the relay let a stalled page block the gate", r.written)
		}
		if r.slowest > 500*time.Millisecond {
			t.Errorf("the slowest scanner write took %s; the relay must keep draining", r.slowest)
		}
		if r.written >= 4000 {
			t.Errorf("all %d frames were accepted with nobody reading — the relay did not drop the stalled page", r.written)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the fake scanner never finished writing — its writes are blocked on the relay")
	}

	// A stalled page cannot be TOLD why — the close frame sits behind the
	// writes it is not reading — so the only claim is that the socket ends.
	_ = browser.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		if _, _, err := browser.ReadMessage(); err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) && ce.Code != websocket.CloseTryAgainLater && ce.Code != websocket.CloseAbnormalClosure {
				t.Errorf("close code %d %q, want 1013 or abnormal", ce.Code, ce.Text)
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Error("the stalled page's relay was never closed")
			}
			break
		}
	}
}

// However many tabs or reloads, the portal adds at most maxRelays clients to
// the gate, and opens at most perMin sessions a minute.
func TestScannerSocket_CapsConcurrentAndPerMinuteSessions(t *testing.T) {
	release := make(chan struct{})
	upstream := newFakeScanner(t, func(c *websocket.Conn) {
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"meta"}`))
		<-release
	})
	defer close(release)
	f := New(upstream.addr(), "", nil)
	f.maxRelays = 1
	srv := relayServer(t, f, nil)

	first, _, err := dialRelay(srv)
	if err != nil {
		t.Fatal(err)
	}
	_ = first.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := first.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	second, resp, err := dialRelay(srv)
	if err == nil {
		second.Close()
		t.Fatal("a second relay opened past the cap")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("second relay → %v, want 503", resp)
	}
	if n := upstream.accepted.Load(); n != 1 {
		t.Errorf("the scanner saw %d clients, want 1", n)
	}
	if v := f.View(); v.Scanner.RelaysActive != 1 || v.Scanner.RelaysMax != 1 {
		t.Errorf("status = %+v", v.Scanner)
	}
	first.Close()

	// The per-minute budget: a reconnect loop runs out of sessions.
	f.maxRelays, f.perMin = 5, 3
	f.mu.Lock()
	f.opened = nil
	f.mu.Unlock()
	opened := 0
	for i := 0; i < 6; i++ {
		c, resp, err := dialRelay(srv)
		if err == nil {
			opened++
			c.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if opened != 3 {
		t.Errorf("%d sessions opened inside one minute, want the budget of 3", opened)
	}
}

// A scanner that is not there is a close frame that says so, and a status
// line — not a page that spins.
func TestScannerSocket_UpstreamDownClosesWithAReason(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	ln.Close()
	f := New(deadAddr, "", nil)
	srv := relayServer(t, f, nil)

	browser, _, err := dialRelay(srv)
	if err != nil {
		t.Fatalf("the page's own handshake must succeed so it can be told why: %v", err)
	}
	defer browser.Close()
	_ = browser.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, _, err = browser.ReadMessage()
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.CloseTryAgainLater || !strings.Contains(ce.Text, deadAddr) {
		t.Errorf("close = %v, want 1013 naming %s", err, deadAddr)
	}
	if v := f.View(); !strings.Contains(v.Scanner.LastErrorVI, deadAddr) || v.Scanner.LastErrorAtMs == 0 {
		t.Errorf("status after a failed dial = %+v", v.Scanner)
	}
}

// Shutdown ends the relays; http.Server.Shutdown alone would not.
func TestScannerSocket_CloseAllEndsEveryRelay(t *testing.T) {
	release := make(chan struct{})
	upstream := newFakeScanner(t, func(c *websocket.Conn) {
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"meta"}`))
		<-release
	})
	defer close(release)
	f := New(upstream.addr(), "", nil)
	srv := relayServer(t, f, nil)
	browser, _, err := dialRelay(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	_ = browser.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := browser.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	f.CloseAll()
	_, _, err = browser.ReadMessage()
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.CloseGoingAway {
		t.Errorf("after CloseAll = %v, want 1001", err)
	}
	if _, resp, err := dialRelay(srv); err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a relay opened after shutdown began: %v", err)
	}
}

// The portal's http.Server carries a WriteTimeout sized for an order
// (minutes). A relay outlives that by design, so the deadline must not follow
// the hijacked connection: gorilla clears it at upgrade, and this pins it.
func TestScannerRelay_OutlivesTheServerWriteTimeout(t *testing.T) {
	release := make(chan struct{})
	upstream := newFakeScanner(t, func(c *websocket.Conn) {
		for i := 0; ; i++ {
			select {
			case <-release:
				return
			case <-time.After(50 * time.Millisecond):
			}
			if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"prices","n":`+strconv.Itoa(i)+`}`)); err != nil {
				return
			}
		}
	})
	defer close(release)
	srv := relayServer(t, New(upstream.addr(), "", nil), func(s *http.Server) {
		s.WriteTimeout = 200 * time.Millisecond
		s.ReadTimeout = 200 * time.Millisecond
	})

	browser, _, err := dialRelay(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	deadline := time.Now().Add(1200 * time.Millisecond)
	frames := 0
	_ = browser.SetReadDeadline(time.Now().Add(5 * time.Second))
	for time.Now().Before(deadline) {
		if _, _, err := browser.ReadMessage(); err != nil {
			t.Fatalf("relay died after %d frames, well past the server's 200ms write timeout: %v", frames, err)
		}
		frames++
	}
	if frames < 10 {
		t.Errorf("only %d frames in 1.2s", frames)
	}
}

// fakeHTTP records every upstream request.
type fakeHTTP struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string
}

func newFakeHTTP(t *testing.T, h http.HandlerFunc) *fakeHTTP {
	t.Helper()
	f := &fakeHTTP{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Method+" "+r.URL.RequestURI())
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHTTP) addr() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeHTTP) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// Only a validated symbol and one of the page's own day counts leave the
// portal, on the scanner's fixed path; the answer comes back verbatim, labelled
// as a feed and not as testnet, and is shared for a minute.
func TestScannerHistory_ForwardsOnlyTheFixedPathAndPassesTheAnswerThrough(t *testing.T) {
	upstream := newFakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch r.URL.Query().Get("symbol") {
		case "NOPEUSDT":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error_vi":"Tham số symbol phải là một cặp có trong config.yaml"}`))
		case "DOWNUSDT":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error_vi":"Không đọc được lịch sử funding."}`))
		default:
			_, _ = w.Write([]byte(`{"symbol":"BTCUSDT","series":[]}`))
		}
	})
	f := New(upstream.addr(), "", nil)
	srv := relayServer(t, f, nil)

	status, got, h := get(t, srv, "/api/scanner/funding-history?symbol=BTCUSDT&days=90&path=../../etc&x=1")
	if status != http.StatusOK || got != `{"symbol":"BTCUSDT","series":[]}` {
		t.Fatalf("got %d %q", status, got)
	}
	if h.Get("X-Execution-Mode") != "scanner-feed" {
		t.Errorf("X-Execution-Mode = %q — a market-data body must not carry the portal's testnet label", h.Get("X-Execution-Mode"))
	}
	if reqs := upstream.requests(); len(reqs) != 1 || reqs[0] != "GET /api/funding/history?days=90&symbol=BTCUSDT" {
		t.Errorf("upstream saw %v", reqs)
	}
	get(t, srv, "/api/scanner/funding-history?symbol=BTCUSDT&days=90")
	if reqs := upstream.requests(); len(reqs) != 1 {
		t.Errorf("a second read inside the TTL reached the gate's database again: %v", reqs)
	}

	// The scanner's own refusal reaches the page with its words; its failure
	// is a 502, and not remembered for a minute.
	if status, got, _ := get(t, srv, "/api/scanner/funding-history?symbol=NOPEUSDT"); status != http.StatusBadRequest || !strings.Contains(got, "config.yaml") {
		t.Errorf("upstream refusal → %d %s", status, got)
	}
	if status, _, _ := get(t, srv, "/api/scanner/funding-history?symbol=DOWNUSDT"); status != http.StatusBadGateway {
		t.Errorf("upstream 500 → %d, want 502", status)
	}
	f.history.mu.Lock()
	e := f.history.entries["DOWNUSDT|30"]
	f.history.mu.Unlock()
	if e == nil || e.ttl > errorTTL {
		t.Errorf("a 5xx answer is cached for %+v", e)
	}

	before := len(upstream.requests())
	for _, q := range []string{"symbol=btcusdt", "symbol=BTC%2F..%2Fx", "symbol=", "symbol=BTCUSDT&days=0", "symbol=BTCUSDT&days=31",
		"symbol=BTCUSDT&days=400", "symbol=BTCUSDT&days=7d"} {
		if status, _, _ := get(t, srv, "/api/scanner/funding-history?"+q); status != http.StatusBadRequest {
			t.Errorf("%s → %d, want 400", q, status)
		}
	}
	if reqs := upstream.requests(); len(reqs) != before {
		t.Errorf("an invalid query reached the scanner: %v", reqs[before:])
	}
}

// An expired entry does not outlive its TTL in memory.
func TestCache_DropsExpiredEntries(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newCache(func() time.Time { return now })
	load := func() (body, error) { return body{status: 200, bytes: []byte("{}")}, nil }
	for _, k := range []string{"A|7", "B|7", "C|7"} {
		c.get(k, time.Minute, load)
	}
	now = now.Add(2 * time.Minute)
	c.get("D|7", time.Minute, load)
	if n := c.size(); n != 1 {
		t.Errorf("cache holds %d entries after the others expired, want 1", n)
	}
}

// An upstream that redirects is not followed, and one that is not JSON is not
// served as if it were.
func TestScannerHistory_NeitherFollowsRedirectsNorServesNonJSON(t *testing.T) {
	elsewhere := newFakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	upstream := newFakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("symbol") == "HTMLUSDT" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<script>alert(1)</script>"))
			return
		}
		http.Redirect(w, r, elsewhere.srv.URL+"/steal", http.StatusTemporaryRedirect)
	})
	srv := relayServer(t, New(upstream.addr(), "", nil), nil)
	for _, symbol := range []string{"BTCUSDT", "HTMLUSDT"} {
		if status, got, _ := get(t, srv, "/api/scanner/funding-history?symbol="+symbol); status != http.StatusBadGateway || !strings.Contains(got, "feed_unreachable") {
			t.Errorf("%s → %d %s, want 502", symbol, status, got)
		}
	}
	if reqs := elsewhere.requests(); len(reqs) != 0 {
		t.Errorf("the redirect was followed: %v", reqs)
	}
}

// Whatever answers on the paper port must BE the paper ledger, and its answer
// goes out labelled paper.
func TestPaperLedger_RequiresThePaperLedgerItself(t *testing.T) {
	var mode atomic.Value
	mode.Store("")
	upstream := newFakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if m := mode.Load().(string); m != "" {
			w.Header().Set("X-Execution-Mode", m)
		}
		_, _ = w.Write([]byte(`{"mode":"paper","equity":[]}`))
	})
	f := New("", upstream.addr(), nil)
	srv := relayServer(t, f, nil)

	if status, got, _ := get(t, srv, "/api/paper/ledger"); status != http.StatusBadGateway || !strings.Contains(got, "not_paper_ledger") {
		t.Errorf("no paper header → %d %s", status, got)
	}

	mode.Store("paper")
	f.ledger.invalidate()
	status, got, h := get(t, srv, "/api/paper/ledger")
	if status != http.StatusOK || got != `{"mode":"paper","equity":[]}` || h.Get("X-Execution-Mode") != "paper" {
		t.Errorf("paper ledger → %d %q mode %q", status, got, h.Get("X-Execution-Mode"))
	}
	if reqs := upstream.requests(); reqs[len(reqs)-1] != "GET /api/ledger" {
		t.Errorf("upstream saw %v", reqs)
	}

	// Not running is a hint, not a stack trace.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	f2 := New("", dead, nil)
	srv2 := relayServer(t, f2, nil)
	if status, got, _ := get(t, srv2, "/api/paper/ledger"); status != http.StatusBadGateway || !strings.Contains(got, "cmd/paperledger") {
		t.Errorf("paper down → %d %s", status, got)
	}
	if v := f2.View(); v.Paper.LastErrorVI == "" {
		t.Error("status does not remember the failure")
	}
}

// A feed switched off says so; it is not an empty chart.
func TestFeeds_DisabledSaySo(t *testing.T) {
	f := New("", "", nil)
	srv := relayServer(t, f, nil)
	for _, path := range []string{"/api/scanner/funding-history?symbol=BTCUSDT", "/api/paper/ledger"} {
		if status, got, _ := get(t, srv, path); status != http.StatusServiceUnavailable || !strings.Contains(got, "feed_disabled") {
			t.Errorf("GET %s → %d %s", path, status, got)
		}
	}
	if v := f.View(); v.Scanner.Enabled || v.Paper.Enabled {
		t.Errorf("disabled feeds report enabled: %+v", v)
	}
}

// A refused relay cannot tell the browser why (a refused handshake is a bare
// 1006 to page script), so the reason must be in the status the page asks for;
// and a feed's own error is labelled as a feed, not as testnet.
func TestScannerSocket_RefusalIsRecordedAndErrorsAreLabelledFeed(t *testing.T) {
	release := make(chan struct{})
	upstream := newFakeScanner(t, func(c *websocket.Conn) { <-release })
	defer close(release)
	f := New(upstream.addr(), "", nil)
	f.maxRelays = 0
	srv := relayServer(t, f, nil)
	_, resp, err := dialRelay(srv)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("relay past a cap of 0 → %v %v", resp, err)
	}
	if mode := resp.Header.Get("X-Execution-Mode"); mode != "feed" {
		t.Errorf("refusal X-Execution-Mode = %q, want feed", mode)
	}
	if v := f.View(); !strings.Contains(v.Scanner.LastErrorVI, "từ chối") || v.Scanner.LastErrorAtMs == 0 {
		t.Errorf("the refusal is not in the status: %+v", v.Scanner)
	}
}

// The radar and its log are relayed verbatim from FIXED upstream paths: the
// page's query reaches the scanner only as one of the offered windows.
func TestScannerCrossRadar_FixedPathsAndWindows(t *testing.T) {
	upstream := newFakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"path":"` + r.URL.Path + `"}`))
	})
	f := New(upstream.addr(), "", nil)
	srv := relayServer(t, f, nil)

	status, got, h := get(t, srv, "/api/scanner/cross-radar?symbol=../x&days=365")
	if status != http.StatusOK || got != `{"path":"/api/cross-radar"}` || h.Get("X-Execution-Mode") != "scanner-feed" {
		t.Fatalf("radar: %d %s %q", status, got, h.Get("X-Execution-Mode"))
	}
	if status, got, _ := get(t, srv, "/api/scanner/cross-radar/events?days=30&x=1"); status != http.StatusOK || got != `{"path":"/api/cross-radar/events"}` {
		t.Fatalf("events: %d %s", status, got)
	}
	reqs := upstream.requests()
	if len(reqs) != 2 || reqs[0] != "GET /api/cross-radar" || reqs[1] != "GET /api/cross-radar/events?days=30" {
		t.Errorf("upstream saw %v", reqs)
	}
	get(t, srv, "/api/scanner/cross-radar")
	if len(upstream.requests()) != 2 {
		t.Error("a second radar read inside the TTL reached the scanner again")
	}
	for _, q := range []string{"days=2", "days=90", "days=abc"} {
		if status, _, _ := get(t, srv, "/api/scanner/cross-radar/events?"+q); status != http.StatusBadRequest {
			t.Errorf("%s → %d, want 400", q, status)
		}
	}
	if len(upstream.requests()) != 2 {
		t.Error("a refused window reached the scanner")
	}
	off := relayServer(t, New("", "", nil), nil)
	if status, _, _ := get(t, off, "/api/scanner/cross-radar"); status != http.StatusServiceUnavailable {
		t.Errorf("no scanner → %d", status)
	}
}
