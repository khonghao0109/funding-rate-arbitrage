package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
	"futures-arbitrage-scanner/static"
)

// The HTTP walls, exercised through the real handler with no socket at all.
// Every client built here carries a transport that FAILS THE TEST if it is
// ever used, so a refusal that reached a venue cannot pass.

const testHost = "127.0.0.1:8087"

// staticDir is the page on disk — the tree package static embeds.
const staticDir = "../../static"

type noNetwork struct{ t *testing.T }

func (n noNetwork) RoundTrip(r *http.Request) (*http.Response, error) {
	n.t.Errorf("a request reached the network: %s %s — this path must refuse before any venue call", r.Method, r.URL.Host+r.URL.Path)
	return nil, io.ErrUnexpectedEOF
}

func offlineClient(t *testing.T, market broker.Market) *binancebroker.Client {
	t.Helper()
	cfg, err := binancebroker.DefaultConfig(market, broker.Credentials{APIKey: broker.NewSecret("k"), APISecret: broker.NewSecret("s")})
	if err != nil {
		t.Fatal(err)
	}
	cfg.TestTransport = noNetwork{t}
	c, err := binancebroker.New(market, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// testPortal builds a portal on a temp state dir. withClients gives it
// credentialed clients that cannot reach any network.
func testPortal(t *testing.T, withClients bool) *portal {
	t.Helper()
	m := markets{spotErr: broker.ErrNoCredentials, perpErr: broker.ErrNoCredentials}
	if withClients {
		m = markets{spot: offlineClient(t, broker.MarketSpot), perp: offlineClient(t, broker.MarketFuturesUSDM),
			spotSourceVI: "test", perpSourceVI: "test"}
	}
	p := newPortal(m, []string{"BTCUSDT", "ETHUSDT"}, "127.0.0.1", "8087", execSettings{
		MarginFrac: 0.5, MaxSlippageBps: 10, LegTimeout: 10 * time.Second, ActionTimeout: time.Minute,
	}, nil)
	p.stateDir = t.TempDir()
	return p
}

type reqOpt func(*http.Request)

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func withHost(h string) reqOpt      { return func(r *http.Request) { r.Host = h } }

func do(t *testing.T, p *portal, method, path, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Host = testHost
	if method == http.MethodGet && strings.HasPrefix(path, "/api/") {
		// What the page's own fetch sends; tests of the guard remove it.
		req.Header.Set(actionHeader, readAction)
	}
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	p.handler().ServeHTTP(rec, req)
	return rec
}

func writeOpts(action string) []reqOpt {
	return []reqOpt{withHeader("Content-Type", "application/json"), withHeader(actionHeader, action),
		withHeader("Origin", "http://"+testHost), withHeader("Sec-Fetch-Site", "same-origin")}
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not an error body: %v — %s", err, rec.Body.String())
	}
	if body.ErrorVI == "" {
		t.Errorf("error response without error_vi: %s", rec.Body.String())
	}
	return body.ErrorCode
}

func TestHandler_ServesThePageWithItsSecurityHeaders(t *testing.T) {
	p := testPortal(t, false)
	rec := do(t, p, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "[BINANCE TESTNET DEMO]") {
		t.Error("the page does not carry the testnet badge")
	}
	h := rec.Header()
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"frame-ancestors 'none'", "script-src 'self'", "style-src 'self'", "font-src 'self'",
		"require-trusted-types-for 'script'", "trusted-types 'none'",
		"connect-src 'self' ws://127.0.0.1:8087 ws://localhost:8087;"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %q", want, csp)
		}
	}
	if strings.Contains(csp, "http:") || strings.Contains(csp, "https:") || strings.Contains(csp, "*") {
		t.Errorf("CSP allows a remote origin: %q", csp)
	}
	if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("framing/sniffing headers missing: %v", h)
	}
	for asset, contentType := range map[string]string{
		"/js/main.js":                      "javascript",
		"/js/scanner.js":                   "javascript",
		"/css/portal.css":                  "text/css",
		"/fonts/fonts.css":                 "text/css",
		"/fonts/inter-vietnamese.woff2":    "font/woff2",
		"/vendor/" + vendoredChartFile:     "javascript",
		"/research/crowding-research.json": "application/json",
	} {
		rec := do(t, p, http.MethodGet, asset, "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d", asset, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); !strings.Contains(got, contentType) {
			t.Errorf("GET %s Content-Type %q, want %s — under nosniff a wrong type is a refused asset", asset, got, contentType)
		}
	}
}

// vendoredChartFile is TradingView Lightweight Charts 4.2.1, taken from the npm
// registry tarball whose sha1 and sha512 matched the registry's published
// dist.shasum and dist.integrity on 2026-09-14. It is served from the portal
// because a page that can place orders loads no script from a CDN: whoever
// serves that script can press the buttons.
const (
	vendoredChartFile   = "lightweight-charts-4.2.1.standalone.production.js"
	vendoredChartSHA256 = "197180bdf2185bb33f3cad878d0d29d2128b4fca96ed847a0de4dc64da1c5e97"
)

func TestUI_VendoredFilesAreTheOnesThatWereVerified(t *testing.T) {
	blob, err := os.ReadFile(filepath.Join(staticDir, "vendor", vendoredChartFile))
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(blob); hex.EncodeToString(sum[:]) != vendoredChartSHA256 {
		t.Errorf("%s changed (sha256 %x) — re-verify it against the npm registry before trusting it with the order page", vendoredChartFile, sum)
	}
	for _, license := range []string{"vendor/LICENSE-lightweight-charts.txt", "fonts/OFL-inter.txt", "fonts/OFL-jetbrainsmono.txt"} {
		if info, err := os.Stat(filepath.Join(staticDir, license)); err != nil || info.Size() < 1000 {
			t.Errorf("%s missing: redistributing the file requires its licence", license)
		}
	}
}

// The binary serves what it was built with, and the build takes only the
// directories static/embed.go names. A file added to static/ outside those
// patterns — or named with a leading underscore, which a directory pattern
// skips — would be in git and in cmd/scanner's answer but a 404 on this page.
func TestUI_EmbeddedTreeIsTheStaticDirectory(t *testing.T) {
	onDisk := map[string][]byte{}
	err := filepath.WalkDir(staticDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(staticDir, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && rel != "." {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil // .DS_Store and the like are nobody's page
		}
		// The package's own source is not part of the page.
		if d.IsDir() || strings.HasSuffix(path, ".go") {
			return nil
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		onDisk[filepath.ToSlash(rel)] = blob
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	embedded := map[string][]byte{}
	err = fs.WalkDir(static.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		blob, err := fs.ReadFile(static.FS(), path)
		embedded[path] = blob
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk) < 20 || embedded["index.html"] == nil {
		t.Fatalf("%d files on disk, index.html embedded: %v — the test is not seeing the page", len(onDisk), embedded["index.html"] != nil)
	}
	for path, blob := range onDisk {
		got, ok := embedded[path]
		if !ok {
			t.Errorf("static/%s is on disk but not in the binary — name its directory on the go:embed line", path)
		} else if !bytes.Equal(got, blob) {
			t.Errorf("static/%s differs from the embedded copy", path)
		}
	}
	for path := range embedded {
		if _, ok := onDisk[path]; !ok {
			t.Errorf("static/%s is embedded but not on disk", path)
		}
	}
}

// uiSources is every file the page is built from, vendored code excluded.
func uiSources(t *testing.T, ext string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(staticDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "vendor" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ext) {
			return nil
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = string(blob)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The page is served under script-src 'self' and style-src 'self', so an inline
// script, an inline handler or a style attribute would be refused by the
// browser and logged as a console error on every load.
func TestUI_HasNothingTheCSPWouldRefuse(t *testing.T) {
	blob, err := os.ReadFile(filepath.Join(staticDir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	html := string(blob)
	for name, re := range map[string]*regexp.Regexp{
		"style attribute":      regexp.MustCompile(`\sstyle\s*=`),
		"inline <style>":       regexp.MustCompile(`<style[\s>]`),
		"inline script":        regexp.MustCompile(`<script(?:\s+[^>]*)?>\s*[^<\s]`),
		"inline event handler": regexp.MustCompile(`\son[a-z]+\s*=`),
		"remote script/style":  regexp.MustCompile(`<(?:script|link|img|iframe|source)\b[^>]*\s(?:href|src)\s*=\s*"(?:https?:)?//`),
	} {
		if loc := re.FindStringIndex(html); loc != nil {
			t.Errorf("index.html contains an %s near %q", name, html[loc[0]:min(loc[1]+40, len(html))])
		}
	}

	scripts := uiSources(t, ".js")
	if len(scripts) < 6 {
		t.Fatalf("found %d scripts under static/ — the test is not seeing the page", len(scripts))
	}
	htmlWrite := regexp.MustCompile(`\.(?:innerHTML|outerHTML)\s*[+]?=|insertAdjacentHTML\s*\(|document\.write\s*\(|\beval\s*\(|new\s+Function\s*\(`)
	// A WebSocket URL has to be absolute; the one allowed is this page's own
	// host. Go's regexp has no lookahead, so that spelling is removed first.
	absolute := regexp.MustCompile(`["'` + "`" + `](?:https?|wss?)://`)
	for path, js := range scripts {
		if loc := htmlWrite.FindStringIndex(js); loc != nil {
			t.Errorf("%s writes HTML or evaluates a string near %q; venue and feed text must go through textContent", path, js[loc[0]:loc[1]])
		}
		scan := strings.ReplaceAll(js, "`ws://${location.host}/", "")
		if loc := absolute.FindStringIndex(scan); loc != nil {
			t.Errorf("%s names an absolute URL near %q — the page talks to its own origin only", path, scan[loc[0]:min(loc[1]+40, len(scan))])
		}
	}
	for path, css := range uiSources(t, ".css") {
		if strings.Contains(css, "@import") || regexp.MustCompile(`url\(\s*["']?(?:https?:)?//`).MatchString(css) {
			t.Errorf("%s pulls a remote resource", path)
		}
	}
}

// The order buttons live on the Execution tab only, and only execution.js
// sends a write or presses a button: a feed tab that could post an order, or
// click one, would be a path from a signal to an order drawn in JavaScript
// instead of Go. core.js holds the one fetch and the post helper; everything
// else reaches the network through api() with no options.
func TestUI_OnlyTheExecutionTabWrites(t *testing.T) {
	scripts := uiSources(t, ".js")
	forbidden := map[string]*regexp.Regexp{
		"a POST":          regexp.MustCompile(`["'` + "`" + `]POST["'` + "`" + `]|method\s*:`),
		"the post helper": regexp.MustCompile(`\bpost\b`),
		"an order endpoint": regexp.MustCompile(`/api/(?:open|close|reconcile)|["'` + "`" + `](?:open|close|reconcile)["'` + "`" + `]` +
			`|/api/autotrade/(?:start|stop|kill|close-pair|pair|ack-all)|autotrade-(?:start|stop|kill|close-pair|pair|ack-all)`),
		"a raw request": regexp.MustCompile(`\bfetch\s*\(|XMLHttpRequest|sendBeacon|\bimport\s*\(`),
		// A name assembled at run time reaches what the patterns above look for
		// by spelling: window["fe"+"tch"], a namespace import, Reflect.
		"a dynamic lookup":   regexp.MustCompile(`window\s*\[|\bglobalThis\b|\bimport\s*\*|\bReflect\.|\[\s*["'` + "`" + `](?:api|post|fetch|method)`),
		"api() with options": regexp.MustCompile(`\bapi\s*\([^()]*,`),
		"a synthetic press":  regexp.MustCompile(`\.click\s*\(|dispatchEvent\s*\(|requestSubmit|\.submit\s*\(`),
	}
	allowedIn := map[string]map[string]bool{
		"execution.js": {"a POST": true, "the post helper": true, "an order endpoint": true},
		"core.js":      {"a POST": true, "the post helper": true, "a raw request": true, "api() with options": true},
	}
	// The auto-trader's view draws and reads nothing: it may not even hold the
	// api helper, so a write cannot be assembled in it from pieces.
	viewOnly := map[string]*regexp.Regexp{"autotrade.js": regexp.MustCompile(`\bapi\b`)}
	sockets := regexp.MustCompile(`new\s+WebSocket\b`)
	seenExecution := false
	for path, js := range scripts {
		base := filepath.Base(path)
		if base == "execution.js" {
			seenExecution = forbidden["an order endpoint"].MatchString(js)
		}
		for name, re := range forbidden {
			if allowedIn[base][name] {
				continue
			}
			if loc := re.FindStringIndex(js); loc != nil {
				t.Errorf("%s contains %s near %q — only the Execution tab may write, and no page code presses a button", path, name, js[loc[0]:min(loc[1]+30, len(js))])
			}
		}
		if re, ok := viewOnly[base]; ok {
			if loc := re.FindStringIndex(js); loc != nil {
				t.Errorf("%s names the api helper near %q — the auto-trader's view reads and sends nothing", path, js[loc[0]:min(loc[1]+30, len(js))])
			}
		}
		if base != "scanner.js" && sockets.MatchString(js) {
			t.Errorf("%s opens a WebSocket — only the Scanner tab's relay may", path)
		}
	}
	if !seenExecution {
		t.Error("execution.js names no order endpoint — the test is not reading the real file")
	}
}

// Every write that sends an order — or lets the bot send one — goes out only
// after the operator confirmed its dialog in the same function: a POST placed
// before confirmDialog, or in a function that has none, is a click that trades
// without the second look. The reconcile dry run (apply: false) sends no order
// and fills that dialog; a stop that keeps the positions and a pair's pause send
// none and lead to none.
func TestUI_EveryOrderLeadingWriteFollowsItsDialog(t *testing.T) {
	blob, err := os.ReadFile(filepath.Join(staticDir, "js", "execution.js"))
	if err != nil {
		t.Fatal(err)
	}
	js := string(blob)
	leading := map[string]bool{"open": true, "close": true, "reconcile": true, "autotrade-start": true, "autotrade-stop-close": true,
		"autotrade-kill": true, "autotrade-close-pair": true, "autotrade-pair-resume": true, "autotrade-pair-ack": true,
		"autotrade-ack-all": true,
		// Engine 2 (PLAN 4.5k step 4). The first two send orders. The other
		// three send none and are here anyway, because each one REMOVES a stop
		// that is holding the machine back — a pair the engine refused to close
		// on conflicting evidence, a red margin latch, a halted pilot — and
		// decision Q21 makes each of those a person's decision, which is exactly
		// what a dialog is. A click that quietly re-armed the machine would be
		// the same defect as a click that quietly traded.
		"crossperp-open": true, "crossperp-close": true,
		"crossperp-unblock": true, "crossperp-ack-margin": true, "crossperp-pilot": true}
	// Reconcile only READS both venues and rewrites the lock table from what
	// they say; it can refuse an open but can never cause one.
	noOrder := map[string]bool{"autotrade-stop": true, "autotrade-pair-pause": true, "crossperp-reconcile": true}
	fnStart := regexp.MustCompile(`(?m)^(?:export\s+)?(?:async\s+)?function\s+\w+`)
	postCall := regexp.MustCompile(`\bpost\(\s*"([a-z-]+)"\s*,[^;]*`)
	starts := fnStart.FindAllStringIndex(js, -1)
	seen := 0
	for i, st := range starts {
		end := len(js)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		body := js[st[0]:end]
		name := js[st[0]:st[1]]
		for _, m := range postCall.FindAllStringSubmatchIndex(body, -1) {
			action := body[m[2]:m[3]]
			call := body[m[0]:m[1]]
			switch {
			case noOrder[action]:
				continue
			case !leading[action]:
				t.Errorf("%s sends %q, which this test does not classify — add it to one list or the other", name, action)
				continue
			case action == "reconcile" && strings.Contains(call, "apply: false"):
				continue
			}
			seen++
			if dialog := strings.Index(body, "confirmDialog("); dialog < 0 || dialog > m[0] {
				t.Errorf("%s sends %q without a confirmDialog before it in the same function", name, action)
			}
		}
	}
	if seen < 8 {
		t.Fatalf("found %d order-leading writes in execution.js — the test is not reading the real file", seen)
	}
}

// DNS rebinding: the Host header names somebody else.
func TestHandler_RefusesAForeignHost(t *testing.T) {
	p := testPortal(t, false)
	for _, host := range []string{"evil.example:8087", "127.0.0.1:9999", "attacker.test", ""} {
		rec := do(t, p, http.MethodGet, "/api/status", "", withHost(host))
		if rec.Code != http.StatusForbidden || errorCode(t, rec) != "host_refused" {
			t.Errorf("Host %q → %d %s", host, rec.Code, rec.Body.String())
		}
	}
	for _, host := range []string{testHost, "localhost:8087", "LOCALHOST:8087"} {
		if rec := do(t, p, http.MethodGet, "/api/status", "", withHost(host)); rec.Code != http.StatusOK {
			t.Errorf("Host %q → %d", host, rec.Code)
		}
	}
}

// Reads are guarded too: each one spends the weight budget an unwind needs.
func TestAPIGuard_ReadsRefuseAnotherSite(t *testing.T) {
	p := testPortal(t, false)
	for name, opts := range map[string][]reqOpt{
		"no action header":   {withHeader(actionHeader, "")},
		"a write's header":   {withHeader(actionHeader, "open")},
		"cross-site no-cors": {withHeader(actionHeader, ""), withHeader("Sec-Fetch-Site", "cross-site")},
		"foreign origin":     {withHeader("Origin", "http://evil.example")},
	} {
		for _, path := range []string{"/api/status", "/api/positions?symbol=BTCUSDT", "/api/orders?symbol=BTCUSDT"} {
			if rec := do(t, p, http.MethodGet, path, "", opts...); rec.Code != http.StatusForbidden {
				t.Errorf("%s GET %s → %d, want 403", name, path, rec.Code)
			}
		}
	}
	// The page itself needs no header.
	if rec := do(t, p, http.MethodGet, "/", "", withHeader("Sec-Fetch-Site", "none")); rec.Code != http.StatusOK {
		t.Errorf("GET / = %d", rec.Code)
	}
}

// CSRF: every missing wall on its own is a refusal, and none of them reaches a
// venue (the offline clients would fail the test).
func TestWriteGuard_EachWallRefusesOnItsOwn(t *testing.T) {
	p := testPortal(t, true)
	body := `{"symbol":"BTCUSDT","notional_quote":65,"leg_order":"sequential_spot_first"}`
	cases := []struct {
		name   string
		opts   []reqOpt
		status int
		code   string
	}{
		{"no content type", []reqOpt{withHeader(actionHeader, "open")}, http.StatusUnsupportedMediaType, "json_required"},
		{"form content type", []reqOpt{withHeader("Content-Type", "application/x-www-form-urlencoded"), withHeader(actionHeader, "open")}, http.StatusUnsupportedMediaType, "json_required"},
		{"no action header", []reqOpt{withHeader("Content-Type", "application/json")}, http.StatusForbidden, "action_header_missing"},
		{"another action's header", []reqOpt{withHeader("Content-Type", "application/json"), withHeader(actionHeader, "close")}, http.StatusForbidden, "action_header_missing"},
		{"foreign origin", append(writeOpts("open"), withHeader("Origin", "http://evil.example")), http.StatusForbidden, "cross_origin_refused"},
		{"https origin of our own host", append(writeOpts("open"), withHeader("Origin", "https://"+testHost)), http.StatusForbidden, "cross_origin_refused"},
		{"cross-site fetch", append(writeOpts("open"), withHeader("Sec-Fetch-Site", "cross-site")), http.StatusForbidden, "cross_site_refused"},
		{"same-site but not same-origin", append(writeOpts("open"), withHeader("Sec-Fetch-Site", "same-site")), http.StatusForbidden, "cross_site_refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, p, http.MethodPost, "/api/open", body, tc.opts...)
			if rec.Code != tc.status || errorCode(t, rec) != tc.code {
				t.Errorf("got %d %s, want %d %s", rec.Code, rec.Body.String(), tc.status, tc.code)
			}
		})
	}
	if rec := do(t, p, http.MethodGet, "/api/open", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/open = %d, want 405", rec.Code)
	}
}

// A write request that clears every wall but is malformed is refused before
// the lock, the credentials or any venue.
func TestOpen_RefusesABadRequestBeforeAnything(t *testing.T) {
	p := testPortal(t, true)
	for name, body := range map[string]string{
		"not json":           `symbol=BTCUSDT`,
		"unknown field":      `{"symbol":"BTCUSDT","notional_quote":65,"leverage":20}`,
		"two values":         `{"symbol":"BTCUSDT","notional_quote":65}{"symbol":"BTCUSDT","notional_quote":65}`,
		"symbol not allowed": `{"symbol":"DOGEUSDT","notional_quote":65}`,
		"symbol injection":   `{"symbol":"BTCUSDT&side=BUY","notional_quote":65}`,
		"zero notional":      `{"symbol":"BTCUSDT","notional_quote":0}`,
		"negative notional":  `{"symbol":"BTCUSDT","notional_quote":-65}`,
		"over the ceiling":   `{"symbol":"BTCUSDT","notional_quote":50000.01}`,
		"bad leg order":      `{"symbol":"BTCUSDT","notional_quote":65,"leg_order":"perp_first"}`,
		"notional as string": `{"symbol":"BTCUSDT","notional_quote":"65"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := do(t, p, http.MethodPost, "/api/open", body, writeOpts("open")...)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("got %d %s, want 400", rec.Code, rec.Body.String())
			}
		})
	}
	big := `{"symbol":"BTCUSDT","notional_quote":65,"leg_order":"` + strings.Repeat("x", maxRequestBodyBytes) + `"}`
	if rec := do(t, p, http.MethodPost, "/api/open", big, writeOpts("open")...); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body → %d, want 413", rec.Code)
	}
}

func TestOpen_WithoutCredentialsSaysWhichAndSendsNothing(t *testing.T) {
	p := testPortal(t, false)
	rec := do(t, p, http.MethodPost, "/api/open", `{"symbol":"BTCUSDT","notional_quote":65}`, writeOpts("open")...)
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != "no_credentials" {
		t.Errorf("got %d %s", rec.Code, rec.Body.String())
	}
}

// The double-click: a second write while one runs is REFUSED, not queued, and
// refused before any venue is asked anything.
func TestWrites_ASecondWriteWhileOneRunsIsRefused(t *testing.T) {
	p := testPortal(t, true)
	release, _, _, ok := p.acquire("open")
	if !ok {
		t.Fatal("could not take the lock on an idle portal")
	}
	defer release()

	for _, tc := range []struct{ action, path, body string }{
		{"open", "/api/open", `{"symbol":"BTCUSDT","notional_quote":65}`},
		{"close", "/api/close", `{"symbol":"BTCUSDT","intent_id":"pbtcusdt-20260914-101500-123"}`},
		{"reconcile", "/api/reconcile", `{"symbol":"BTCUSDT","apply":true}`},
	} {
		rec := do(t, p, http.MethodPost, tc.path, tc.body, writeOpts(tc.action)...)
		if rec.Code != http.StatusConflict || errorCode(t, rec) != "busy" {
			t.Errorf("%s while open runs → %d %s, want 409 busy", tc.action, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "open") {
			t.Errorf("%s: the refusal does not say what is running: %s", tc.action, rec.Body.String())
		}
	}

	var status statusView
	rec := do(t, p, http.MethodGet, "/api/status", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Busy || status.BusyAction != "open" {
		t.Errorf("status while busy = busy %v action %q", status.Busy, status.BusyAction)
	}
}

// The intent id becomes a file path; a path is refused before the lock.
func TestClose_RefusesAnIntentIDThatIsAPath(t *testing.T) {
	p := testPortal(t, true)
	for _, id := range []string{"../../.env", "..", "a/b", "x.json", "", "UPPER", strings.Repeat("a", 65)} {
		body, _ := json.Marshal(closeRequest{Symbol: "BTCUSDT", IntentID: id})
		rec := do(t, p, http.MethodPost, "/api/close", string(body), writeOpts("close")...)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "bad_intent_id" {
			t.Errorf("intent id %q → %d %s", id, rec.Code, rec.Body.String())
		}
	}
}

func TestClose_UnknownIntentAndWrongSymbolAreRefusedBeforeAnyVenue(t *testing.T) {
	p := testPortal(t, true)
	rec := do(t, p, http.MethodPost, "/api/close", `{"symbol":"BTCUSDT","intent_id":"pbtcusdt-20260914-000000-000"}`, writeOpts("close")...)
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "intent_not_found" {
		t.Errorf("unknown intent → %d %s", rec.Code, rec.Body.String())
	}

	if err := saveState(p.stateDir, intentState{IntentID: "pethusdt-20260914-000000-000", Symbol: "ETHUSDT", Outcome: "both_open"}); err != nil {
		t.Fatal(err)
	}
	rec = do(t, p, http.MethodPost, "/api/close", `{"symbol":"BTCUSDT","intent_id":"pethusdt-20260914-000000-000"}`, writeOpts("close")...)
	if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "symbol_mismatch" {
		t.Errorf("symbol mismatch → %d %s", rec.Code, rec.Body.String())
	}
}

// The digest fingerprints what an apply would send; any change between the
// dry run and the apply changes it.
func TestPlanDigest_ChangesWithAnythingTheApplyWouldSend(t *testing.T) {
	base := reconcileView{Symbol: "BTCUSDT", VenuePerpQtyCoin: -0.0008, Plans: []squarePlan{
		{IntentID: testIntent, Action: "send", Market: "spot", Side: "SELL", ClientOrderID: "fa1sabc", QtyCoin: 0.0008},
	}}
	d := planDigest(base)
	if d == "" || planDigest(base) != d {
		t.Fatal("the digest is empty or not deterministic")
	}
	mutations := map[string]func(v *reconcileView){
		"venue perp moved":    func(v *reconcileView) { v.VenuePerpQtyCoin = -0.0009 },
		"quantity changed":    func(v *reconcileView) { v.Plans[0].QtyCoin = 0.0007 },
		"side changed":        func(v *reconcileView) { v.Plans[0].Side = "BUY" },
		"another plan":        func(v *reconcileView) { v.Plans = append(v.Plans, squarePlan{IntentID: "x", Action: "send"}) },
		"refused now":         func(v *reconcileView) { v.Plans[0].Action = "refuse" },
		"conflict appeared":   func(v *reconcileView) { v.ConflictVI = "x" },
		"reduce-only flipped": func(v *reconcileView) { v.Plans[0].ReduceOnly = true },
	}
	for name, mutate := range mutations {
		v := base
		v.Plans = append([]squarePlan(nil), base.Plans...)
		mutate(&v)
		if planDigest(v) == d {
			t.Errorf("%s: digest unchanged", name)
		}
	}
}

// Past half the per-minute weight the page stops READING, so what is left of
// the budget belongs to an order's read-back, cancel or unwind. The offline
// clients fail the test if any venue call is made.
func TestReads_StopAtHalfTheWeightBudget(t *testing.T) {
	p := testPortal(t, true)
	budget := p.markets.spot.HTTP().Budget()
	if err := budget.Reserve(context.Background(), budget.LimitPerMin()/2); err != nil {
		t.Fatal(err)
	}
	if err := p.markets.readBudgetError(); err == nil {
		t.Fatal("half the spot budget spent, and reads are still allowed")
	}

	rec := do(t, p, http.MethodGet, "/api/positions?symbol=BTCUSDT", "")
	var pos positionsView
	if err := json.Unmarshal(rec.Body.Bytes(), &pos); err != nil {
		t.Fatal(err)
	}
	if pos.Status != statusUnknown || !strings.Contains(pos.ReasonVI, "weight") {
		t.Errorf("positions over budget = %s / %q", pos.Status, pos.ReasonVI)
	}
	var acct accountView
	rec = do(t, p, http.MethodGet, "/api/account?symbol=BTCUSDT", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &acct); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(acct.Spot.ErrorVI, "weight") || acct.Spot.WeightUsed1m < budget.LimitPerMin()/2 {
		t.Errorf("account over budget = %+v — it should say why and still show the tally", acct.Spot)
	}
	for _, path := range []string{"/api/orders?symbol=BTCUSDT", "/api/funding?symbol=BTCUSDT", "/api/market?symbol=BTCUSDT"} {
		if rec := do(t, p, http.MethodGet, path, ""); !strings.Contains(rec.Body.String(), "weight") {
			t.Errorf("GET %s over budget did not say so: %s", path, rec.Body.String())
		}
	}
}

func TestReadEndpoints_RefuseASymbolOffTheList(t *testing.T) {
	p := testPortal(t, false)
	for _, path := range []string{"/api/positions", "/api/orders", "/api/funding", "/api/market", "/api/positions?symbol=DOGEUSDT", "/api/account?symbol=x"} {
		if rec := do(t, p, http.MethodGet, path, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", path, rec.Code)
		}
	}
}

// With no credentials the page still works and says what is missing.
func TestReadEndpoints_WithoutCredentialsStillAnswer(t *testing.T) {
	p := testPortal(t, false)
	rec := do(t, p, http.MethodGet, "/api/positions?symbol=BTCUSDT", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("positions = %d", rec.Code)
	}
	var pos positionsView
	if err := json.Unmarshal(rec.Body.Bytes(), &pos); err != nil {
		t.Fatal(err)
	}
	if pos.Status != statusUnknown || !strings.Contains(pos.ReasonVI, "credential") {
		t.Errorf("positions without credentials = %s / %q, want unknown naming the credential", pos.Status, pos.ReasonVI)
	}
	// A position figure without its age is a claim about now the venue never
	// made. Found live: a defer stamping a COPY left read_at_ms at 0.
	if pos.ReadAtMs <= 0 || pos.StatusVI == "" {
		t.Errorf("positions carries read_at_ms %d and status_vi %q — both must be set on every return path", pos.ReadAtMs, pos.StatusVI)
	}

	rec = do(t, p, http.MethodGet, "/api/account", "")
	var acct accountView
	if err := json.Unmarshal(rec.Body.Bytes(), &acct); err != nil {
		t.Fatal(err)
	}
	if acct.Spot.Configured || acct.Spot.ErrorVI == "" || acct.Futures.Configured {
		t.Errorf("account without credentials = %+v", acct)
	}

	rec = do(t, p, http.MethodGet, "/api/intents", "")
	if rec.Code != http.StatusOK {
		t.Errorf("intents = %d", rec.Code)
	}
}

// Every id the page's scripts look up exists in the markup. $ is
// getElementById, so a script naming an element the page does not have throws a
// TypeError on the line that reads it — silently, half-way through a render,
// and only on the tab that reaches it. A form field added to one file and not
// the other is exactly that bug (PLAN "Công cụ vận hành 4.5f").
func TestUI_EveryIdTheScriptsLookUpIsInTheMarkup(t *testing.T) {
	blob, err := os.ReadFile(filepath.Join(staticDir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, m := range regexp.MustCompile(`\sid="([^"]+)"`).FindAllStringSubmatch(string(blob), -1) {
		have[m[1]] = true
	}
	if len(have) < 50 {
		t.Fatalf("read %d ids from index.html — the test is not seeing the page", len(have))
	}
	lookup := regexp.MustCompile(`\$\("([^"]+)"\)`)
	scripts := uiSources(t, ".js")
	if len(scripts) < 6 {
		t.Fatalf("found %d scripts under static/ — the test is not seeing the page", len(scripts))
	}
	found := 0
	for path, js := range scripts {
		for _, m := range lookup.FindAllStringSubmatch(js, -1) {
			found++
			if !have[m[1]] {
				t.Errorf("%s reads $(%q); index.html has no element with that id", path, m[1])
			}
		}
	}
	if found < 50 {
		t.Fatalf("matched %d lookups across the scripts — the test is not seeing them", found)
	}
}
