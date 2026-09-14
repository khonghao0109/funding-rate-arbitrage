package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"futures-arbitrage-scanner/cmd/execportal/feeds"
)

// The portal's walls in front of the feed routes. The relay and the proxies
// themselves are tested in package feeds; what is tested here is that nothing
// reaches them — and so nothing reaches the step-3.5 gate — unless it came from
// this portal's own page.

// countingUpstream counts every connection attempt, WebSocket or HTTP.
func countingUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "nothing here", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// Another page, a missing Origin, or a same-site-but-other-origin handshake is
// refused BEFORE the scanner is dialed.
func TestScannerSocketRoute_RefusesCrossOriginBeforeDialing(t *testing.T) {
	upstream, hits := countingUpstream(t)
	p := testPortal(t, false)
	p.feeds = feeds.New(strings.TrimPrefix(upstream.URL, "http://"), "", nil)
	h := p.handler()
	cases := map[string]map[string]string{
		"no origin":               {},
		"foreign origin":          {"Origin": "http://evil.example"},
		"null origin":             {"Origin": "null"},
		"https origin, own host":  {"Origin": "https://" + testHost},
		"cross-site fetch meta":   {"Origin": "http://" + testHost, "Sec-Fetch-Site": "cross-site"},
		"same-site, other port":   {"Origin": "http://127.0.0.1:9999", "Sec-Fetch-Site": "same-site"},
		"none is not a WS origin": {"Origin": "http://" + testHost, "Sec-Fetch-Site": "none"},
	}
	for name, headers := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/scanner/ws", nil)
		req.Host = testHost
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s → %d %s, want 403", name, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/scanner/ws", nil)
	req.Host = "evil.example:8087"
	req.Header.Set("Origin", "http://evil.example:8087")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != "host_refused" {
		t.Errorf("foreign Host → %d", rec.Code)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the scanner was contacted %d times by refused handshakes", n)
	}
}

// The proxies are reads like every other: no action header, no upstream call.
func TestFeedProxyRoutes_NeedTheReadHeaderAndRefuseOtherSites(t *testing.T) {
	upstream, hits := countingUpstream(t)
	addr := strings.TrimPrefix(upstream.URL, "http://")
	p := testPortal(t, false)
	p.feeds = feeds.New(addr, addr, nil)
	for _, path := range []string{"/api/scanner/funding-history?symbol=BTCUSDT&days=30", "/api/paper/ledger"} {
		for name, opts := range map[string][]reqOpt{
			"no action header": {withHeader(actionHeader, "")},
			"foreign origin":   {withHeader("Origin", "http://evil.example")},
			"cross-site":       {withHeader("Sec-Fetch-Site", "cross-site")},
		} {
			if rec := do(t, p, http.MethodGet, path, "", opts...); rec.Code != http.StatusForbidden {
				t.Errorf("%s GET %s → %d, want 403", name, path, rec.Code)
			}
		}
		if rec := do(t, p, http.MethodPost, path, "{}", writeOpts("read")...); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s → %d, want 405", path, rec.Code)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("refused requests reached the upstream %d times", n)
	}
}

// With feeds switched off the routes say so, and /api/status reports it.
func TestFeedRoutes_DisabledSaySoAndStatusReportsThem(t *testing.T) {
	p := testPortal(t, false)
	for _, path := range []string{"/api/scanner/funding-history?symbol=BTCUSDT", "/api/paper/ledger"} {
		if rec := do(t, p, http.MethodGet, path, ""); rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != "feed_disabled" {
			t.Errorf("GET %s → %d %s", path, rec.Code, rec.Body.String())
		}
	}
	rec := do(t, p, http.MethodGet, "/api/status", "")
	if !strings.Contains(rec.Body.String(), `"feeds":{"scanner":{"addr":""`) || !strings.Contains(rec.Body.String(), `"relays_max":3`) {
		t.Errorf("status does not carry the feeds' health: %s", rec.Body.String())
	}
}

// The whole chain the page uses: Host guard, socket guard, the logging
// wrapper's Hijack, and the relay.
// Nothing in package feeds' own tests passes through this wiring.
func TestScannerSocketRoute_SameOriginPageGetsTheRelayThroughTheRealHandler(t *testing.T) {
	release := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"meta","v":1}`))
		<-release
	}))
	defer upstream.Close()
	defer close(release)

	p := testPortal(t, false)
	p.feeds = feeds.New(strings.TrimPrefix(upstream.URL, "http://"), "", nil)
	srv := httptest.NewServer(p.handler())
	defer func() {
		p.feeds.CloseAll()
		srv.Close()
	}()

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, resp, err := dialer.Dial("ws://"+strings.TrimPrefix(srv.URL, "http://")+"/api/scanner/ws",
		http.Header{"Host": {testHost}, "Origin": {"http://" + testHost}, "Sec-Fetch-Site": {"same-origin"}})
	if err != nil {
		t.Fatalf("the portal's own page could not open the relay through the real handler: %v (%v)", err, resp)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, msg, err := conn.ReadMessage(); err != nil || string(msg) != `{"type":"meta","v":1}` {
		t.Errorf("first frame = %q, %v", msg, err)
	}
}
