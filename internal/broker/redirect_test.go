package broker

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// A redirect is the one answer that turns the testnet host allow-list into a
// suggestion: http.Client re-sends the request to wherever Location points, and
// for a 307 or 308 it re-sends the METHOD and the BODY too — the X-MBX-APIKEY
// header and a signed order included. Go strips only the standard credential
// headers on a redirect to another host (Authorization, Www-Authenticate,
// Cookie, Cookie2, Proxy-Authorization, Proxy-Authenticate — net/http
// makeHeadersCopier) and copies every other one, so the key goes wherever the
// venue, or anything in front of it, says.
//
// These tests stand up two servers: the VENUE, answering the pinned testnet
// host, and an ATTACKER, answering every other host. The venue answers with a
// redirect to the attacker. Passing means the attacker saw nothing at all.

type redirectRig struct {
	t        *testing.T
	venue    *httptest.Server
	attacker *httptest.Server

	mu            sync.Mutex
	venueHits     map[string]int // path → requests
	attackerHits  int
	attackerSaw   []string // key header and body of every request that got there
	transportSeen []string // scheme://host of every request handed to the network
}

// redirectAnswer is what the venue says to the endpoint under test.
type redirectAnswer struct {
	status   int
	location func(req *http.Request) string // "" sends no Location header
	body     string
}

func newRedirectRig(t *testing.T, answer redirectAnswer) *redirectRig {
	t.Helper()
	rig := &redirectRig{t: t, venueHits: map[string]int{}}
	rig.attacker = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rig.mu.Lock()
		rig.attackerHits++
		rig.attackerSaw = append(rig.attackerSaw, r.Header.Get("X-MBX-APIKEY")+" "+string(body))
		rig.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"serverTime":1}`)
	}))
	t.Cleanup(rig.attacker.Close)
	rig.venue = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.mu.Lock()
		rig.venueHits[r.URL.Path]++
		rig.mu.Unlock()
		if r.URL.Path == BinanceFuturesTimePath {
			fmt.Fprintf(w, `{"serverTime":%d}`, time.Now().UnixMilli())
			return
		}
		if loc := answer.location; loc != nil {
			if target := loc(r); target != "" {
				w.Header().Set("Location", target)
			}
		}
		w.WriteHeader(answer.status)
		_, _ = io.WriteString(w, answer.body)
	}))
	t.Cleanup(rig.venue.Close)
	return rig
}

// RoundTrip is the network. The pinned host goes to the venue server, every
// other host to the attacker server, and each request is written down first.
func (rig *redirectRig) RoundTrip(r *http.Request) (*http.Response, error) {
	rig.mu.Lock()
	rig.transportSeen = append(rig.transportSeen, r.URL.Scheme+"://"+r.URL.Host)
	rig.mu.Unlock()
	to := rig.attacker.URL
	if r.URL.Scheme == "https" && r.URL.Host == BinanceFuturesTestnetHost {
		to = rig.venue.URL
	}
	target, _ := url.Parse(to)
	clone := r.Clone(r.Context())
	routed := *r.URL
	routed.Scheme, routed.Host, routed.User = target.Scheme, target.Host, nil
	clone.URL, clone.Host = &routed, ""
	return http.DefaultTransport.RoundTrip(clone)
}

func (rig *redirectRig) client() *Client {
	rig.t.Helper()
	c, err := NewClient(Config{
		BaseURL:           BinanceFuturesTestnetBaseURL,
		Credentials:       Credentials{APIKey: NewSecret("KEY-" + sentinel), APISecret: NewSecret(sentinel)},
		TimePath:          BinanceFuturesTimePath,
		WeightLimitPerMin: BinanceFuturesWeightPerMin,
		TestTransport:     rig,
	})
	if err != nil {
		rig.t.Fatalf("NewClient: %v", err)
	}
	return c
}

// assertNothingLeft checks the two places a followed redirect would show up.
func (rig *redirectRig) assertNothingLeft() {
	rig.t.Helper()
	rig.mu.Lock()
	defer rig.mu.Unlock()
	if rig.attackerHits != 0 {
		rig.t.Errorf("the other host received %d request(s): %q — the redirect was followed", rig.attackerHits, rig.attackerSaw)
	}
	for _, seen := range rig.transportSeen {
		if seen != "https://"+BinanceFuturesTestnetHost {
			rig.t.Errorf("a request was handed to the network for %s — only the pinned testnet host may be", seen)
		}
	}
}

// attackerURL is an https Location on a host that is not on the allow-list:
// what a hijacked edge, or a venue moving a path to another domain, would send.
func attackerURL(*http.Request) string { return "https://evil.example/fapi/v1/order" }

// signatureValue matches a signature that survived redaction.
var signatureValue = regexp.MustCompile(`signature=[0-9a-fA-F]{16,}`)

var redirectStatuses = []int{
	http.StatusMovedPermanently,  // 301
	http.StatusFound,             // 302
	http.StatusSeeOther,          // 303
	http.StatusTemporaryRedirect, // 307 — re-sends method and body
	http.StatusPermanentRedirect, // 308 — re-sends method and body
}

func TestClient_RefusesEveryRedirectAndNeverResendsToAnotherHost(t *testing.T) {
	calls := []struct {
		name string
		ep   Endpoint
		call func(c *Client, ep Endpoint) error
	}{
		{"signed GET", FuturesAccountBalance, func(c *Client, ep Endpoint) error {
			return c.GetSigned(context.Background(), ep, nil, &[]map[string]any{})
		}},
		{"signed POST (an order, parameters in the body)", FuturesNewOrder, func(c *Client, ep Endpoint) error {
			return c.PostSigned(context.Background(), ep, []Param{{"symbol", "BTCUSDT"}, {"side", "BUY"}, {"quantity", "1"}}, nil)
		}},
		{"signed DELETE", FuturesNewOrder, func(c *Client, ep Endpoint) error {
			return c.DeleteSigned(context.Background(), ep, []Param{{"symbol", "BTCUSDT"}, {"origClientOrderId", "x"}}, nil)
		}},
		{"public GET", Endpoint{Path: "/fapi/v1/exchangeInfo", WeightIP: 1}, func(c *Client, ep Endpoint) error {
			return c.GetPublic(context.Background(), ep, nil, &map[string]any{})
		}},
	}
	locations := map[string]func(*http.Request) string{
		"to another https host":      attackerURL,
		"downgraded to plain http":   func(*http.Request) string { return "http://" + BinanceFuturesTestnetHost + "/fapi/v1/order" },
		"relative, on the same host": func(*http.Request) string { return "/fapi/v2/order" },
		// The common shape of a real redirect: same path, query kept — which for
		// a signed GET or DELETE is the signature, echoed back.
		"to another host, echoing the signed query": func(r *http.Request) string {
			return "https://evil.example" + r.URL.Path + "?" + r.URL.RawQuery
		},
	}
	for _, status := range redirectStatuses {
		for locName, loc := range locations {
			for _, call := range calls {
				t.Run(fmt.Sprintf("%d %s %s", status, locName, call.name), func(t *testing.T) {
					r := newRedirectRig(t, redirectAnswer{status: status, location: loc})
					c := r.client()
					err := call.call(c, call.ep)
					r.assertNothingLeft()
					if !errors.Is(err, ErrRedirectAttempted) {
						t.Fatalf("err = %v, want ErrRedirectAttempted", err)
					}
					var httpErr *HTTPError
					if !errors.As(err, &httpErr) || httpErr.StatusCode != status {
						t.Errorf("the refusal does not carry the venue's HTTP %d: %v", status, err)
					}
					if httpErr != nil && httpErr.Body != "" {
						t.Errorf("a 3xx body was kept as the venue's answer: %q", httpErr.Body)
					}
					if msg := err.Error(); strings.Contains(msg, sentinel) || signatureValue.MatchString(msg) {
						t.Errorf("the refusal message carries the credential or the signature: %s", msg)
					}
					r.mu.Lock()
					defer r.mu.Unlock()
					if got := r.venueHits[call.ep.Path]; got != 1 {
						t.Errorf("the venue saw %s %d times, want exactly 1 — a refused redirect is not retried", call.ep.Path, got)
					}
					for path, n := range r.venueHits {
						if path != call.ep.Path && path != BinanceFuturesTimePath {
							t.Errorf("the venue saw %d request(s) for %s — the Location was followed on the same host", n, path)
						}
					}
				})
			}
		}
	}
}

// A 3xx with no Location is not followed by http.Client — it comes back as a
// plain answer. It is still not the API's answer, and it is refused the same.
func TestClient_ARedirectStatusWithoutLocationIsRefusedToo(t *testing.T) {
	for _, status := range append([]int{http.StatusMultipleChoices}, redirectStatuses...) {
		r := newRedirectRig(t, redirectAnswer{status: status})
		err := r.client().GetSigned(context.Background(), FuturesAccountBalance, nil, &[]map[string]any{})
		if !errors.Is(err, ErrRedirectAttempted) {
			t.Errorf("HTTP %d without Location: err = %v, want ErrRedirectAttempted", status, err)
		}
		r.assertNothingLeft()
	}
}

// Config offers nothing that reaches a client, a transport, a dialer, a TLS
// configuration or a redirect policy — at any depth: a nested options struct,
// a pointer, a function parameter or result. The one transport hook is
// TestTransport, refused outside a test binary. Pinned by walking the types, so
// a field added later — `HTTPClient *http.Client`, `TLS *tls.Config`,
// `DialContext func(...) (net.Conn, error)` for one afternoon of debugging —
// fails here instead of silently reopening what 9cb39c4 recorded.
func TestConfig_ReachesNoClientTransportDialerTLSOrRedirectPolicy(t *testing.T) {
	forbidden := map[reflect.Type]string{
		reflect.TypeOf(http.Client{}):           "an http.Client",
		reflect.TypeOf(http.Transport{}):        "an http.Transport",
		reflect.TypeOf(http.Request{}):          "an http.Request (a redirect policy or a proxy function takes one)",
		reflect.TypeOf(tls.Config{}):            "a TLS configuration",
		reflect.TypeOf(net.Dialer{}):            "a dialer",
		reflect.TypeOf(net.Resolver{}):          "a resolver",
		reflect.TypeOf((*net.Conn)(nil)).Elem(): "a connection",
		reflect.TypeOf(url.URL{}):               "a URL (a proxy function returns one)",
	}
	roundTripper := reflect.TypeOf((*http.RoundTripper)(nil)).Elem()
	var transports []string
	seen := map[reflect.Type]bool{}
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		if typ.Implements(roundTripper) {
			transports = append(transports, path)
			return
		}
		if what, bad := forbidden[typ]; bad {
			t.Errorf("Config.%s reaches %s", path, what)
			return
		}
		if seen[typ] {
			return
		}
		seen[typ] = true
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
			walk(path+"[]", typ.Elem())
		case reflect.Map:
			walk(path+"[key]", typ.Key())
			walk(path+"[value]", typ.Elem())
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				walk(path+"."+typ.Field(i).Name, typ.Field(i).Type)
			}
		case reflect.Func:
			for i := 0; i < typ.NumIn(); i++ {
				walk(fmt.Sprintf("%s(in %d)", path, i), typ.In(i))
			}
			for i := 0; i < typ.NumOut(); i++ {
				walk(fmt.Sprintf("%s(out %d)", path, i), typ.Out(i))
			}
		}
	}
	cfg := reflect.TypeOf(Config{})
	for i := 0; i < cfg.NumField(); i++ {
		walk(cfg.Field(i).Name, cfg.Field(i).Type)
	}
	if len(transports) != 1 || transports[0] != "TestTransport" {
		t.Errorf("transport hooks reachable from Config = %v, want exactly [TestTransport]", transports)
	}
	if len(seen) < 5 {
		t.Fatalf("walked %d types — the test is not reading Config", len(seen))
	}

	// And the whole field list, because a walk by type cannot see an `any`, an
	// io.Writer key log or a dial func returning io.ReadWriteCloser. A new
	// field fails here until someone has read it against this test.
	var fields []string
	for i := 0; i < cfg.NumField(); i++ {
		fields = append(fields, cfg.Field(i).Name+" "+cfg.Field(i).Type.String())
	}
	const want = "BaseURL string; Scheme broker.SigningScheme; Credentials broker.Credentials; RecvWindowMs int64; TimePath string; ClockSyncEvery time.Duration; " +
		"WeightLimitPerMin int; TestTransport http.RoundTripper; ObserveResponse func(broker.ResponseRecord); _ struct {}; Now func() time.Time; UserAgentVI string"
	if got := strings.Join(fields, "; "); got != want {
		t.Errorf("Config fields changed:\n got %s\nwant %s\n— check the new field reaches no transport, dialer, TLS setting or redirect policy, then update this list", got, want)
	}
}

// Outside a test binary a TestTransport is refused before any client exists:
// whatever it does with a request happens BELOW the host pin, so the only safe
// place for it is a test. The check is given testing.Testing's answer as an
// argument; here it is given a production binary's.
func TestNewClient_RefusesATestTransportOutsideATestBinary(t *testing.T) {
	anywhere := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") })
	if err := refuseTestTransport(anywhere, false); !errors.Is(err, ErrTestTransportOutsideTest) {
		t.Errorf("a test transport in a production binary: err = %v, want ErrTestTransportOutsideTest", err)
	}
	if err := refuseTestTransport(nil, false); err != nil {
		t.Errorf("no test transport in a production binary: err = %v", err)
	}
	if err := refuseTestTransport(anywhere, true); err != nil {
		t.Errorf("a test transport in a test binary: err = %v", err)
	}
	// The exported sentinels are assignable. Nil-ing them must not turn either
	// refusal into a nil error.
	defer func(a, b error) { ErrTestTransportOutsideTest, ErrRedirectAttempted = a, b }(ErrTestTransportOutsideTest, ErrRedirectAttempted)
	ErrTestTransportOutsideTest, ErrRedirectAttempted = nil, nil
	if refuseTestTransport(anywhere, false) == nil {
		t.Error("with the sentinel variable set to nil the test transport is accepted")
	}
	if refuseRedirect(nil, nil) == nil {
		t.Error("with the sentinel variable set to nil a redirect is followed")
	}
}

// A process started with GODEBUG=http2debug logs every HTTP/2 request header,
// the API key among them. NewClient refuses to build a client there.
func TestNewClient_RefusesAProcessThatLogsHTTP2Headers(t *testing.T) {
	for godebug, refuse := range map[string]bool{
		"":                         false,
		"http2debug=0":             false,
		"http2client=0":            false,
		"http2debug=1":             true,
		"tlsmlkem=1, http2debug=2": true,
	} {
		t.Setenv("GODEBUG", godebug)
		_, err := NewClient(Config{
			BaseURL:           BinanceFuturesTestnetBaseURL,
			Credentials:       Credentials{APIKey: NewSecret("k"), APISecret: NewSecret("s")},
			WeightLimitPerMin: BinanceFuturesWeightPerMin,
		})
		if (err != nil) != refuse {
			t.Errorf("GODEBUG=%q: err = %v, want refused %v", godebug, err, refuse)
		}
	}
}

// The same refusal, end to end, in a real binary `go test` did not build: the
// probe under testdata/ is compiled and run with `go run`, hands NewClient a
// TestTransport, and reports. No socket is opened.
func TestNewClient_RefusesATestTransportInABinaryGoTestDidNotBuild(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "./internal/broker/testdata/refusalprobe")
	cmd.Dir = root
	// The toolchain on PATH, not one it would download; no inherited flags.
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run refusalprobe: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "refused=true client=false reached=0" {
		t.Errorf("refusalprobe said %q, want the test transport refused and never reached", got)
	}
}

// The transport under the pin is the client's own, never the process-wide
// http.DefaultTransport: replacing that variable, or rewiring the object it
// held, reaches no broker client — including one built before the change.
func TestClient_NeverUsesTheProcessWideDefaultTransport(t *testing.T) {
	c, err := NewClient(Config{
		BaseURL:           BinanceFuturesTestnetBaseURL,
		Credentials:       Credentials{APIKey: NewSecret("KEY-" + sentinel), APISecret: NewSecret(sentinel)},
		WeightLimitPerMin: BinanceFuturesWeightPerMin,
	})
	if err != nil {
		t.Fatal(err)
	}
	var replaced, rewired int
	original := http.DefaultTransport
	shared := original.(*http.Transport)
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		replaced++
		return nil, errors.New("the process-wide transport was used")
	})
	shared.DialTLSContext = func(context.Context, string, string) (net.Conn, error) {
		rewired++
		return nil, errors.New("the shared transport's dialer was used")
	}
	t.Cleanup(func() {
		http.DefaultTransport = original
		shared.DialTLSContext = nil
	})

	// A context cancelled before the call: the broker's own transport returns
	// without dialing, while a replaced or rewired default would already have
	// been handed the request.
	after, err := NewClient(Config{
		BaseURL:           BinanceFuturesTestnetBaseURL,
		Credentials:       Credentials{APIKey: NewSecret("KEY-" + sentinel), APISecret: NewSecret(sentinel)},
		WeightLimitPerMin: BinanceFuturesWeightPerMin,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, client := range []*Client{c, after} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, BinanceFuturesTestnetBaseURL+BinanceFuturesTimePath, nil)
		if _, err := client.http.Do(req); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled from the client's own transport", err)
		}
	}
	if replaced != 0 || rewired != 0 {
		t.Errorf("the process-wide transport was reached: replaced %d, rewired %d", replaced, rewired)
	}
	// The cancelled context returns before any dial, so identity is what
	// proves the client built BEFORE the change does not share the object.
	for name, client := range map[string]*Client{"before": c, "after": after} {
		if inner := client.http.Transport.(*hostPinTransport).next; inner == original || inner == http.DefaultTransport {
			t.Errorf("the client built %s the change holds the process-wide transport", name)
		}
	}
}

// Inside a test, a TestTransport still sits BELOW both walls: a request built
// for another host never reaches it, and a redirect it answers with is refused
// rather than followed — whatever the transport would have done next.
func TestClient_ATestTransportStaysUnderBothWalls(t *testing.T) {
	var reachedHosts []string
	followWherever := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reachedHosts = append(reachedHosts, r.URL.Scheme+"://"+r.URL.Host)
		h := http.Header{}
		h.Set("Location", "https://evil.example/fapi/v1/order")
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	c, err := NewClient(Config{
		BaseURL:           BinanceFuturesTestnetBaseURL,
		Credentials:       Credentials{APIKey: NewSecret("KEY-" + sentinel), APISecret: NewSecret(sentinel)},
		WeightLimitPerMin: BinanceFuturesWeightPerMin,
		TestTransport:     followWherever,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.GetPublic(context.Background(), Endpoint{Path: "@evil.example/fapi/v1/order", WeightIP: 1}, nil, nil); !errors.Is(err, ErrHostNotPinned) {
		t.Errorf("a request for another host: err = %v, want ErrHostNotPinned", err)
	}
	if err := c.GetPublic(context.Background(), Endpoint{Path: "/fapi/v1/exchangeInfo", WeightIP: 1}, nil, nil); !errors.Is(err, ErrRedirectAttempted) {
		t.Errorf("a redirect from the transport: err = %v, want ErrRedirectAttempted", err)
	}
	if len(reachedHosts) != 1 || reachedHosts[0] != "https://"+BinanceFuturesTestnetHost {
		t.Errorf("the transport was handed %q; want exactly one request, for the pinned host", reachedHosts)
	}
}

// Behind CheckRedirect sits a second wall: every request handed to the network
// must be https to the pinned host. Nothing in the client builds another URL
// today; this is what catches the day something does. An Endpoint path that
// begins with "@" turns the base host into userinfo and another host into the
// real one.
func TestClient_EveryRequestIsPinnedToTheTestnetHost(t *testing.T) {
	r := newRedirectRig(t, redirectAnswer{status: http.StatusOK})
	c := r.client()
	ep := Endpoint{Path: "@evil.example/fapi/v1/order", WeightIP: 1}
	err := c.GetPublic(context.Background(), ep, nil, nil)
	r.assertNothingLeft()
	if !errors.Is(err, ErrHostNotPinned) {
		t.Fatalf("err = %v, want ErrHostNotPinned", err)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("the refusal message carries the credential: %s", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.transportSeen) != 0 {
		t.Errorf("the request reached the network layer for %q; it must be refused before it", r.transportSeen)
	}
}

// A 3xx's body is not the matching engine's verdict. A Binance-shaped body on a
// 302 must not survive as "order not found" (which makes a resend look safe) or
// as a filter refusal (which skips asking whether the order arrived).
func TestClient_ARedirectBodyIsNotKept(t *testing.T) {
	for _, status := range []int{http.StatusMultipleChoices, http.StatusFound, http.StatusTemporaryRedirect} {
		r := newRedirectRig(t, redirectAnswer{status: status, body: `{"code":-2013,"msg":"Order does not exist."}`})
		err := r.client().GetSigned(context.Background(), FuturesAccountBalance, nil, nil)
		var httpErr *HTTPError
		if !errors.Is(err, ErrRedirectAttempted) || !errors.As(err, &httpErr) {
			t.Fatalf("HTTP %d: err = %v, want ErrRedirectAttempted with an *HTTPError", status, err)
		}
		if httpErr.Body != "" || strings.Contains(err.Error(), "-2013") {
			t.Errorf("HTTP %d: the redirect's body reached the error: %v", status, err)
		}
	}
}

// http.Client refuses to parse a malformed Location BEFORE it asks
// CheckRedirect, with an error that carries neither sentinel nor bound. The
// transport takes Location away first, so this is the same refusal as any 3xx.
func TestClient_AnUnparseableOrHugeLocationIsRefusedTheSame(t *testing.T) {
	for name, loc := range map[string]string{
		"unparseable":     "https://evil.example/" + strings.Repeat("a", 64) + "%zz",
		"five kilobytes":  "https://evil.example/" + strings.Repeat("a", 5000) + "%zz",
		"an empty scheme": "://nothing",
	} {
		r := newRedirectRig(t, redirectAnswer{status: http.StatusTemporaryRedirect, location: func(*http.Request) string { return loc }})
		err := r.client().PostSigned(context.Background(), FuturesNewOrder, []Param{{"symbol", "BTCUSDT"}}, nil)
		r.assertNothingLeft()
		if !errors.Is(err, ErrRedirectAttempted) {
			t.Errorf("%s: err = %v, want ErrRedirectAttempted", name, err)
			continue
		}
		if n := len(err.Error()); n > 600 {
			t.Errorf("%s: the refusal is %d bytes — venue-controlled text must stay bounded", name, n)
		}
	}
}

// A redirect's message names where it pointed — scheme and host — and nothing
// of its path, however the path tries to smuggle the key: in plain text across
// a cut, behind a long `signature=` run that scrubs short, or percent-encoded
// where no literal scrub can see it.
func TestClient_ARedirectMessageNamesTheHostAndNothingOfThePath(t *testing.T) {
	hexRun := strings.Repeat("ab", 1000)
	for name, loc := range map[string]string{
		"key across the shown bound": "/" + strings.Repeat("a", maxLocationShownBytes-40) + "KEY-" + sentinel,
		"key behind a signature run": "https://evil.example/signature=" + hexRun + "/KEY-" + sentinel,
		"key percent-encoded":        "https://evil.example/%4BEY-" + sentinel,
	} {
		r := newRedirectRig(t, redirectAnswer{status: http.StatusFound, location: func(*http.Request) string { return loc }})
		err := r.client().GetPublic(context.Background(), Endpoint{Path: "/fapi/v1/exchangeInfo", WeightIP: 1}, nil, nil)
		if !errors.Is(err, ErrRedirectAttempted) {
			t.Fatalf("%s: err = %v", name, err)
		}
		msg := err.Error()
		if strings.Contains(msg, "EY-SEN") || strings.Contains(msg, sentinel[:12]) || strings.Contains(msg, "signature=") {
			t.Errorf("%s: the path reached the message: %s", name, msg)
		}
	}
}

// On the CheckRedirect fallback the refused-Location header is the venue's, not
// ours: it is ignored, and the Location itself is described.
func TestRedirectRefused_TheFallbackTrustsNoVenueHeader(t *testing.T) {
	c := (&redirectRig{t: t}).clientWithoutServers()
	h := http.Header{}
	h.Set(refusedLocationHeader, "https://forged.example/"+strings.Repeat("x", 1<<16)+"KEY-"+sentinel)
	h.Set("Location", "https://evil.example/x")
	err := c.redirectRefused(BinanceFuturesTestnetBaseURL+"/fapi/v1/order", &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: h}, false)
	if msg := err.Error(); !strings.Contains(msg, "https://evil.example") || strings.Contains(msg, "forged") || strings.Contains(msg, sentinel[:12]) {
		t.Errorf("fallback message = %.300s", msg)
	}
}

func (rig *redirectRig) clientWithoutServers() *Client {
	rig.t.Helper()
	c, err := NewClient(Config{
		BaseURL:           BinanceFuturesTestnetBaseURL,
		Credentials:       Credentials{APIKey: NewSecret("KEY-" + sentinel), APISecret: NewSecret(sentinel)},
		TimePath:          BinanceFuturesTimePath,
		WeightLimitPerMin: BinanceFuturesWeightPerMin,
		TestTransport:     roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("no network") }),
	})
	if err != nil {
		rig.t.Fatal(err)
	}
	return c
}

// The transport's own rules, one condition at a time — each would otherwise be
// shadowed by another (the "@" path also changes the host).
func TestHostPinTransport_RefusesAnythingButHTTPSToThePinnedHost(t *testing.T) {
	var reached []string
	next := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reached = append(reached, r.URL.String())
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	})
	pin := &hostPinTransport{host: BinanceFuturesTestnetHost, next: next}
	request := func(raw string) *http.Request {
		req, err := http.NewRequest(http.MethodPost, raw, strings.NewReader("signature=abc"))
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	refused := map[string]*http.Request{
		"plain http to the pinned host": request("http://" + BinanceFuturesTestnetHost + "/fapi/v1/order"),
		"userinfo on the pinned host":   request("https://u:p@" + BinanceFuturesTestnetHost + "/fapi/v1/order"),
		"another host":                  request("https://evil.example/fapi/v1/order"),
		"the pinned host with a port":   request("https://" + BinanceFuturesTestnetHost + ":8443/fapi/v1/order"),
	}
	hostHeader := request("https://" + BinanceFuturesTestnetHost + "/fapi/v1/order")
	hostHeader.Host = "evil.example"
	refused["a Host header naming another host"] = hostHeader
	for name, req := range refused {
		if _, err := pin.RoundTrip(req); !errors.Is(err, ErrHostNotPinned) {
			t.Errorf("%s: err = %v, want ErrHostNotPinned", name, err)
		}
	}
	if len(reached) != 0 {
		t.Errorf("refused requests reached the next transport: %q", reached)
	}
	if _, err := pin.RoundTrip(request("https://" + BinanceFuturesTestnetHost + "/fapi/v1/order")); err != nil || len(reached) != 1 {
		t.Errorf("the pinned request itself: err = %v, reached %d", err, len(reached))
	}
}

// The transport hands no Location up, and replaces anything the venue sent
// under the header name it uses for its own note.
func TestHostPinTransport_TakesTheLocationOffEvery3xx(t *testing.T) {
	next := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		h := http.Header{}
		h.Set("Location", "https://evil.example/x?signature=deadbeefdeadbeefdeadbeef")
		h.Set(refusedLocationHeader, "forged by the venue")
		return &http.Response{StatusCode: http.StatusPermanentRedirect, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	pin := &hostPinTransport{host: BinanceFuturesTestnetHost, next: next}
	req, _ := http.NewRequest(http.MethodGet, "https://"+BinanceFuturesTestnetHost+"/fapi/v3/balance", nil)
	resp, err := pin.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Location"); got != "" {
		t.Errorf("Location handed up: %q", got)
	}
	if got := resp.Header.Get(refusedLocationHeader); got != "https://evil.example" {
		t.Errorf("kept note = %q, want the scheme and host only", got)
	}
}

// Behind the transport, CheckRedirect is not reached today. It is still what
// holds if the transport is ever unwrapped, so its presence is pinned here —
// with the settings of the production transport under the pin.
func TestGuardedHTTPClient_InstallsBothWalls(t *testing.T) {
	test := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") })
	for name, given := range map[string]http.RoundTripper{"production": nil, "test": test} {
		hc := guardedHTTPClient(given, BinanceFuturesTestnetHost)
		if hc.CheckRedirect == nil || !errors.Is(hc.CheckRedirect(nil, nil), ErrRedirectAttempted) {
			t.Errorf("%s: CheckRedirect does not refuse", name)
		}
		if hc.Timeout <= 0 {
			t.Errorf("%s: no timeout", name)
		}
		pin, ok := hc.Transport.(*hostPinTransport)
		if !ok || pin.host != BinanceFuturesTestnetHost {
			t.Fatalf("%s: transport = %#v, want the host pin", name, hc.Transport)
		}
		if given != nil {
			if fmt.Sprint(pin.next) != fmt.Sprint(given) {
				t.Errorf("test: under the pin is %T, want the test's transport", pin.next)
			}
			continue
		}
		own, ok := pin.next.(*http.Transport)
		switch {
		case !ok || pin.next == http.DefaultTransport:
			t.Errorf("production: under the pin is %T — want a transport of the client's own, not http.DefaultTransport", pin.next)
		case own.Proxy != nil || own.DialTLSContext != nil || own.DialTLS != nil || own.DialContext == nil:
			t.Errorf("production: proxy %v, DialTLSContext %v, DialTLS %v, DialContext %v", own.Proxy != nil, own.DialTLSContext != nil, own.DialTLS != nil, own.DialContext != nil)
		case own.TLSClientConfig == nil || own.TLSClientConfig.InsecureSkipVerify || own.TLSClientConfig.MinVersion < tls.VersionTLS12:
			t.Errorf("production: TLS config %+v", own.TLSClientConfig)
		}
		if other := guardedHTTPClient(nil, BinanceFuturesTestnetHost).Transport.(*hostPinTransport).next; other == pin.next {
			t.Error("production: two clients share one transport")
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
