package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func (rig *redirectRig) client(httpClient *http.Client) *Client {
	rig.t.Helper()
	if httpClient == nil {
		httpClient = &http.Client{Transport: rig, Timeout: 5 * time.Second}
	}
	c, err := NewClient(Config{
		BaseURL:           BinanceFuturesTestnetBaseURL,
		Credentials:       Credentials{APIKey: NewSecret("KEY-" + sentinel), APISecret: NewSecret(sentinel)},
		TimePath:          BinanceFuturesTimePath,
		WeightLimitPerMin: BinanceFuturesWeightPerMin,
		HTTPClient:        httpClient,
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
					c := r.client(nil)
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
		err := r.client(nil).GetSigned(context.Background(), FuturesAccountBalance, nil, &[]map[string]any{})
		if !errors.Is(err, ErrRedirectAttempted) {
			t.Errorf("HTTP %d without Location: err = %v, want ErrRedirectAttempted", status, err)
		}
		r.assertNothingLeft()
	}
}

// The HTTPClient hook exists for tests, and a caller's client may carry its own
// CheckRedirect — including one that follows everything. The broker must not
// inherit it, and must not change the caller's client either.
func TestClient_AnInjectedClientCannotReenableRedirects(t *testing.T) {
	r := newRedirectRig(t, redirectAnswer{status: http.StatusTemporaryRedirect, location: attackerURL})
	followAll := func(*http.Request, []*http.Request) error { return nil }
	injected := &http.Client{Transport: r, Timeout: 5 * time.Second, CheckRedirect: followAll}
	err := r.client(injected).PostSigned(context.Background(), FuturesNewOrder, []Param{{"symbol", "BTCUSDT"}}, nil)
	r.assertNothingLeft()
	if !errors.Is(err, ErrRedirectAttempted) {
		t.Fatalf("err = %v, want ErrRedirectAttempted", err)
	}
	if injected.CheckRedirect == nil || injected.Transport != r {
		t.Error("NewClient modified the caller's http.Client")
	}
}

// Behind CheckRedirect sits a second wall: every request handed to the network
// must be https to the pinned host. Nothing in the client builds another URL
// today; this is what catches the day something does. An Endpoint path that
// begins with "@" turns the base host into userinfo and another host into the
// real one.
func TestClient_EveryRequestIsPinnedToTheTestnetHost(t *testing.T) {
	r := newRedirectRig(t, redirectAnswer{status: http.StatusOK})
	c := r.client(nil)
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
		err := r.client(nil).GetSigned(context.Background(), FuturesAccountBalance, nil, nil)
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
		err := r.client(nil).PostSigned(context.Background(), FuturesNewOrder, []Param{{"symbol", "BTCUSDT"}}, nil)
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
		err := r.client(nil).GetPublic(context.Background(), Endpoint{Path: "/fapi/v1/exchangeInfo", WeightIP: 1}, nil, nil)
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
		HTTPClient:        &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("no network") })},
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
// holds if the transport is ever unwrapped, so its presence is pinned here.
func TestGuardedHTTPClient_InstallsBothWallsOnACopy(t *testing.T) {
	followAll := func(*http.Request, []*http.Request) error { return nil }
	caller := &http.Client{CheckRedirect: followAll, Timeout: 7 * time.Second}
	hc := guardedHTTPClient(caller, BinanceFuturesTestnetHost)
	if hc == caller || caller.Transport != nil {
		t.Fatal("the caller's client was used or modified")
	}
	if hc.CheckRedirect == nil || !errors.Is(hc.CheckRedirect(nil, nil), ErrRedirectAttempted) {
		t.Error("CheckRedirect does not refuse")
	}
	if pin, ok := hc.Transport.(*hostPinTransport); !ok || pin.host != BinanceFuturesTestnetHost || pin.next != http.DefaultTransport {
		t.Errorf("transport = %#v, want the host pin over http.DefaultTransport", hc.Transport)
	}
	if hc.Timeout != caller.Timeout {
		t.Errorf("timeout %s not carried over from the caller's %s", hc.Timeout, caller.Timeout)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
