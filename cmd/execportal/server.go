package main

import (
	"bufio"
	"errors"
	"log"
	"mime"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"futures-arbitrage-scanner/static"
)

// The HTTP layer, and the reason it is more guarded than cmd/paperledger's.
//
// Binding to loopback keeps other MACHINES out. It does not keep other WEB
// PAGES out: any site open in the operator's browser can make that browser
// send a request to 127.0.0.1:8087, and this server places orders. Three
// attacks follow from that, and each has its own wall:
//
//   - Cross-site request forgery — a hidden form on another page POSTing to
//     /api/open. Every write needs Content-Type application/json AND the
//     X-Execportal-Action header, neither of which a page can send cross-origin
//     without a CORS preflight, and this server answers no preflight. An Origin
//     or Sec-Fetch-Site that names another site is refused outright.
//   - DNS rebinding — a hostile name re-pointed at 127.0.0.1 so the browser
//     treats this server as same-origin with the attacker. The Host header
//     then carries the attacker's name, and hostGuard refuses every request
//     whose Host is not this listener's own loopback address.
//   - Clickjacking — this page framed invisibly under a decoy button.
//     frame-ancestors 'none' and X-Frame-Options DENY.
//   - Budget exhaustion — a page looping reads at /api/orders spends the same
//     per-minute weight an unwind needs. Reads carry the same header wall
//     (X-Execportal-Action: read), and stop at half the budget.
//   - Cross-site WebSocket hijacking — another page opening the scanner relay
//     (/api/scanner/ws). A browser cannot put a custom header on a WebSocket,
//     but it always sends Origin, and page script cannot forge it: socketGuard
//     requires it to be this listener. The relay is public market data, and it
//     is capped, because each session is one more client of the step-3.5 gate.
//
// What it does NOT stop: another process on this machine can send the headers
// a browser cannot. That process can also read .env, so a token here would
// guard a door whose key is lying next to it.
//
// Testnet holds no real money, so none of this protects capital today. It is
// here because the page is the one PLAN 4.6 would be tempted to point at a
// real account, and a guard added later is a guard added after the incident.

func init() {
	// Go's built-in table has no entry for the embedded fonts, and on a machine
	// without a system mime.types they would be served as octet-stream.
	_ = mime.AddExtensionType(".woff2", "font/woff2")
}

// actionHeader names what a request is for: "read" on every GET under /api/,
// the action's own name on a write. Its presence is what forces a browser to
// preflight a cross-origin request. The UI sends a write's name only after the
// operator confirmed the dialog — except the reconcile DRY RUN, which sends no
// order and exists to fill that dialog.
const actionHeader = "X-Execportal-Action"

const maxRequestBodyBytes = 8 << 10

func (p *portal) handler() http.Handler {
	mux := http.NewServeMux()

	// The page lives in the repository's static/ and is embedded at build time
	// (package static): what this binary serves is what it was built with.
	mux.Handle("/", onlyMethod(http.MethodGet, http.FileServer(http.FS(static.FS()))))

	// API routes are registered without a method and check it themselves, so
	// a wrong method answers 405 rather than falling through to the static
	// file server's 404.
	get := func(path string, h http.HandlerFunc) {
		mux.Handle(path, onlyMethod(http.MethodGet, p.apiGuard(readAction, h)))
	}
	post := func(path, action string, h http.HandlerFunc) {
		mux.Handle(path, onlyMethod(http.MethodPost, p.apiGuard(action, p.writeGuard(h))))
	}
	// postAny is post for a route several action names may reach; the handler
	// then checks the body agrees with the name that was sent.
	postAny := func(path string, actions []string, h http.HandlerFunc) {
		mux.Handle(path, onlyMethod(http.MethodPost, p.apiGuardAny(actions, p.writeGuard(h))))
	}
	get("/api/status", p.handleStatus)
	get("/api/account", p.handleAccount)
	get("/api/positions", p.handlePositions)
	get("/api/orders", p.handleOrders)
	get("/api/funding", p.handleFunding)
	get("/api/intents", p.handleIntents)
	get("/api/market", p.handleMarket)
	// The three-year backtest report, read from disk and cached by mtime
	// (backtest.go). It touches no venue and costs no request weight.
	get("/api/backtest", p.handleBacktest)
	post("/api/open", "open", p.handleOpen)
	post("/api/close", "close", p.handleClose)
	post("/api/reconcile", "reconcile", p.handleReconcile)
	// The testnet auto-trader (PLAN Q18): its state and result, and the
	// operator's writes. Its own orders go through handleOpen's and
	// handleClose's functions, not through these routes.
	get("/api/autotrade/status", p.handleAutotradeStatus)
	get("/api/autotrade/pnl", p.handleAutotradePnL)
	post("/api/autotrade/start", autotradeStartAction, p.handleAutotradeStart)
	postAny("/api/autotrade/stop", []string{autotradeStopAction, autotradeStopCloseAction}, p.handleAutotradeStop)
	post("/api/autotrade/kill", autotradeKillAction, p.handleAutotradeKill)
	post("/api/autotrade/close-pair", autotradeClosePairAction, p.handleAutotradeClosePair)
	postAny("/api/autotrade/pair", []string{autotradePairPauseAction, autotradePairResumeAction, autotradePairAckAction}, p.handleAutotradePair)
	post("/api/autotrade/ack-all", autotradeAckAllAction, p.handleAutotradeAckAll)
	// Read-only feeds from cmd/scanner and cmd/paperledger (PLAN Q17): bytes
	// relayed, never decoded, and not reachable from any order path.
	get("/api/scanner/funding-history", p.feeds.ScannerHistory)
	get("/api/paper/ledger", p.feeds.PaperLedger)
	mux.Handle("/api/scanner/ws", onlyMethod(http.MethodGet, p.socketGuard(http.HandlerFunc(p.feeds.ScannerSocket))))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "không có endpoint "+quoteForMessage(r.URL.Path))
	})

	return logRequests(p.securityHeaders(p.hostGuard(mux)))
}

func onlyMethod(method string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method && !(method == http.MethodGet && r.Method == http.MethodHead) {
			w.Header().Set("Allow", method)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "endpoint này chỉ nhận "+method)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowedHosts is every spelling of this listener's own address a browser on
// this machine may put in the Host header.
func allowedHosts(bindIP, port string) map[string]bool {
	hosts := map[string]bool{
		strings.ToLower(net.JoinHostPort(bindIP, port)): true,
		"localhost:" + port:                             true,
	}
	return hosts
}

// hostGuard refuses a request whose Host is not this listener — the DNS
// rebinding wall.
func (p *portal) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.hosts[strings.ToLower(r.Host)] {
			writeError(w, http.StatusForbidden, "host_refused",
				"Host "+quoteForMessage(r.Host)+" không phải địa chỉ loopback của portal này — từ chối (chống DNS rebinding)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// readAction is the action header every GET under /api/ must carry.
const readAction = "read"

// apiGuard is the cross-site wall in front of EVERY endpoint under /api/, reads
// included.
//
// Reads are guarded too because they are not free: each one is a signed call
// against the same per-minute weight budget the order path draws on, and a
// hostile page looping no-cors fetches at /api/orders could fill that budget
// and make an unwind wait for the next minute. A no-cors request cannot carry
// the custom header, and a cors one needs a preflight this server never grants.
func (p *portal) apiGuard(action string, next http.Handler) http.Handler {
	return p.apiGuardAny([]string{action}, next)
}

// apiGuardAny is apiGuard accepting any one of several action names.
func (p *portal) apiGuardAny(actions []string, next http.Handler) http.Handler {
	action := actions[0]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			writeError(w, http.StatusForbidden, "cross_site_refused",
				"yêu cầu đến từ trang khác (Sec-Fetch-Site: "+quoteForMessage(site)+") — từ chối")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !p.sameOrigin(origin) {
			writeError(w, http.StatusForbidden, "cross_origin_refused",
				"yêu cầu mang Origin "+quoteForMessage(origin)+" không phải portal này — từ chối")
			return
		}
		if got := r.Header.Get(actionHeader); !slices.Contains(actions, got) {
			why := "— lệnh chỉ được gửi sau khi người vận hành xác nhận"
			if action == readAction {
				why = "— chỉ trang của portal được đọc API"
			}
			writeError(w, http.StatusForbidden, "action_header_missing",
				"thiếu hoặc sai header "+actionHeader+" (cần "+quoteForMessage(action)+") "+why)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// socketGuard is apiGuard for the WebSocket relay, where no custom header can
// be sent: Origin is REQUIRED and must be this listener, and a Sec-Fetch-Site
// naming another site is refused. Both are checked before the upgrade, so a
// refused page never costs the scanner a connection.
func (p *portal) socketGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
			writeError(w, http.StatusForbidden, "cross_site_refused",
				"WebSocket mở từ trang khác (Sec-Fetch-Site: "+quoteForMessage(site)+") — từ chối")
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" || !p.sameOrigin(origin) {
			writeError(w, http.StatusForbidden, "cross_origin_refused",
				"WebSocket cần Origin là chính portal này, nhận "+quoteForMessage(origin)+" — từ chối")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeGuard adds what only an order-sending endpoint needs: a JSON body, of
// bounded size. A cross-site form can post neither without a preflight.
func (p *portal) writeGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json") {
			writeError(w, http.StatusUnsupportedMediaType, "json_required",
				"thân yêu cầu phải là application/json")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		next.ServeHTTP(w, r)
	})
}

func (p *portal) sameOrigin(origin string) bool {
	rest, ok := strings.CutPrefix(strings.ToLower(origin), "http://")
	return ok && p.hosts[rest]
}

// securityHeaders sets the CSP. connect-src names this listener's ws:// origins
// explicitly: not every browser reads 'self' as covering a WebSocket.
func (p *portal) securityHeaders(next http.Handler) http.Handler {
	var sockets []string
	for host := range p.hosts {
		sockets = append(sockets, "ws://"+host)
	}
	slices.Sort(sockets)
	// Trusted Types with no policy make every string-to-HTML sink throw, so a
	// later innerHTML with feed or venue text fails loudly instead of running
	// in the origin that can place orders. The vendored chart library's only
	// such sink is its attribution logo, which the page turns off.
	csp := "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self' " +
		strings.Join(sockets, " ") + "; font-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'; " +
		"require-trusted-types-for 'script'; trusted-types 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("X-Execution-Mode", "testnet")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status for the log line.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Hijack lets the WebSocket upgrade through the logging wrapper.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("execportal: the response writer cannot be hijacked")
	}
	s.status = http.StatusSwitchingProtocols
	return h.Hijack()
}

// logRequests writes one line per request: method, path, status, duration.
// Never a body and never a query string — neither carries a credential today,
// and a log line that never prints them cannot start to.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		elapsed := time.Since(startedAt)
		// The page polls every three seconds; a successful poll is not news.
		if r.Method == http.MethodGet && rec.status < 400 && elapsed < 2*time.Second {
			return
		}
		log.Printf("execportal: %s %s → %d (%s)", r.Method, r.URL.Path, rec.status, elapsed.Round(time.Millisecond))
	})
}

// quoteForMessage bounds an attacker-controlled header before it is echoed.
func quoteForMessage(s string) string {
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return "\"" + strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s) + "\""
}
