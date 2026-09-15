package broker

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Two walls between a credential and a host that is not on the allow-list.
//
// checkTestnetBaseURL decides which host a Client is built for, once. That is
// not the same as deciding where each request goes: http.Client follows a 3xx
// on its own, re-sending to wherever Location points, and for a 307 or 308 it
// re-sends the method and the body as well. Go strips only the standard
// credential headers on a redirect to another host (Authorization, Cookie,
// Proxy-Authorization and their kin — net/http makeHeadersCopier); X-MBX-APIKEY
// is not one of them, and the signature travels in the query or the body. So a
// single redirect from a testnet host — a hijacked edge, a misconfigured proxy,
// a venue moving a path — would carry the key and a signed order to a host this
// package was written never to reach. Measured before this file existed: a
// 307 on POST /fapi/v1/order delivered the key AND
// `symbol=…&quantity=…&timestamp=…&signature=…` to the other host.
//
//  1. The transport (hostPinTransport) refuses any request that is not https to
//     the exact host the client was built for, before a byte is sent. Nothing in
//     the client builds such a URL today — an Endpoint.Path beginning with "@"
//     would, by turning the base host into userinfo — and this is the wall for
//     the day something does. It also takes the Location header off every 3xx
//     it hands up, keeping only its scheme and host for the error: http.Client
//     then has nothing to parse and nothing to follow, so every 3xx — with or
//     without a Location, parseable or not — reaches Client.do as one answer.
//     Binance documents no redirect on any endpoint this package calls, so
//     there is nothing legitimate to follow, and a same-host redirect is refused
//     too: a Location is venue-controlled text, and "same host" is one parsing
//     mistake from "another host".
//  2. CheckRedirect refuses every redirect on every client this package builds.
//     Behind the transport it is not reached today; it is what still holds if
//     that transport is ever unwrapped.
//     https://pkg.go.dev/net/http#Client (CheckRedirect)
//
// There is no way to hand this package a client, a transport or a redirect
// policy: Config takes none (a reflection test walks its types), every
// *http.Client is built here, and the transport under the pin is this package's
// own (newVenueTransport) — never the process-wide http.DefaultTransport, which
// anything linked into the binary can rewire. The one transport hook,
// Config.TestTransport, sits BELOW the host check — it receives the signed
// request and could send it anywhere — so NewClient refuses it unless the
// process is a `go test` binary (testing.Testing), and boundary_test.go fails
// if a non-test file anywhere in the module names it. Until 2026-09-15 the hook
// was Config.HTTPClient, open to production code; cmd/brokercheck, its one
// production user, now records answers through Config.ObserveResponse, which
// sees the response after the read and cannot send.
//
// A refused redirect is NOT a statement about the request. The answer came from
// something in front of the matching engine, so for an order it is as ambiguous
// as a timeout: internal/execution resolves it by asking for the order, never by
// resending (definiteRejection only counts a 4xx as a refusal).

// ErrRedirectAttempted is returned when the venue — or anything answering for
// its host — replies with a 3xx. The request is not re-sent anywhere.
var ErrRedirectAttempted = errors.New("broker: http redirect is refused for security")

// ErrHostNotPinned is returned when a request is about to leave for anything
// other than https to the client's own testnet host. Nothing is sent.
var ErrHostNotPinned = errors.New("broker: request host is not the client's pinned testnet host")

// ErrTestTransportOutsideTest is NewClient's answer to a Config.TestTransport
// in a binary that `go test` did not build.
var ErrTestTransportOutsideTest = errors.New("broker: Config.TestTransport is refused outside a test binary — a transport below the host pin is trusted with the credential")

// refuseTestTransport is NewClient's check. It takes testing.Testing's answer
// as an ARGUMENT, read by NewClient at the call: a package variable holding it
// would be writable through //go:linkname from any package linked into the
// binary, and a function cannot be overwritten that way. The linker also
// refuses a linkname to testing's own flag. A test gives it the answer a
// production binary gets; testdata/refusalprobe is a real `go run` binary.
//
// The refusal is WRAPPED, never the sentinel variable itself: an exported error
// variable is assignable, and `broker.ErrTestTransportOutsideTest = nil` would
// otherwise turn this refusal into a nil error (review, 2026-09-15). A wrapped
// nil is still a non-nil error.
func refuseTestTransport(testTransport http.RoundTripper, isTestBinary bool) error {
	if testTransport != nil && !isTestBinary {
		return fmt.Errorf("%w (the process was not built by go test)", ErrTestTransportOutsideTest)
	}
	return nil
}

// refuseHTTP2DebugLogging refuses to build a client in a process started with
// GODEBUG=http2debug set: net/http's HTTP/2 transport then logs every request
// header — X-MBX-APIKEY among them — and the :path with the signature. It is
// read once at start, so this is checked against the environment NewClient
// sees; a variable set later has no effect on the transport either way.
func refuseHTTP2DebugLogging(godebug string) error {
	for _, setting := range strings.Split(godebug, ",") {
		if name, value, _ := strings.Cut(strings.TrimSpace(setting), "="); name == "http2debug" && value != "" && value != "0" {
			return errors.New("broker: GODEBUG=http2debug is set — the HTTP/2 transport would log the API key header and the signed path; unset it to build a client")
		}
	}
	return nil
}

// refuseRedirect is the CheckRedirect of every broker client. It refuses the
// first hop, so the request is never re-issued.
func refuseRedirect(*http.Request, []*http.Request) error {
	// Wrapped, so an assignment to the exported variable cannot make it nil.
	return fmt.Errorf("%w", ErrRedirectAttempted)
}

// guardedHTTPClient builds the only kind of client a broker.Client uses: this
// package's own, with a bounded timeout, the redirect refusal, and the host pin
// over a transport this package built for this client alone — or over a test's
// transport, which NewClient has already confined to test binaries.
func guardedHTTPClient(testTransport http.RoundTripper, pinnedHost string) *http.Client {
	next := testTransport
	if next == nil {
		next = newVenueTransport()
	}
	return &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: refuseRedirect,
		Transport:     &hostPinTransport{host: pinnedHost, next: next},
	}
}

// newVenueTransport is the transport under the host pin in every non-test
// client, and nothing outside this function holds a pointer to it.
//
// Not http.DefaultTransport. That is a process-wide variable holding a shared
// *http.Transport, and any package linked into the binary can replace it or set
// DialTLSContext, TLSClientConfig or Proxy on it — even after NewClient. Review
// proved it on 2026-09-15: a DialTLSContext set on the default transport after
// the client was built sent the time request and the signed balance request, key
// and signature, in plaintext to the attacker's connection. The pin checks the
// URL; only the transport below it decides where the bytes go and how.
//
// The dialer carries its own Resolver for the same reason (net.DefaultResolver
// is a variable too). No proxy: HTTPS_PROXY is process environment, the testnet
// hosts are reached directly (measured 2026-09-15: no proxy variable set), and
// a proxy this client needs is one to add explicitly, not to inherit.
func newVenueTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Resolver: &net.Resolver{}}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true, // a custom TLSClientConfig turns HTTP/2 off unless asked
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          10,
		ExpectContinueTimeout: time.Second,
	}
}

// hostPinTransport is the second wall: https, to exactly one host, with no
// userinfo and no Host header naming anything else.
type hostPinTransport struct {
	host string
	next http.RoundTripper
}

func (t *hostPinTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL
	if u == nil || u.Scheme != "https" || u.Host != t.host || u.User != nil || (req.Host != "" && req.Host != t.host) {
		// A RoundTripper must close the body it was handed, even on error.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		got := "<no url>"
		if u != nil {
			got = (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
		}
		return nil, fmt.Errorf("%w: refused %s, pinned to https://%s", ErrHostNotPinned, got, t.host)
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return resp, err
	}
	if resp.Header == nil {
		resp.Header = http.Header{}
	}
	loc := resp.Header.Get("Location")
	resp.Header.Del("Location")
	// Anything the venue sent under our own header name is not ours.
	resp.Header.Del(refusedLocationHeader)
	if loc != "" {
		resp.Header.Set(refusedLocationHeader, describeLocation(u, loc))
	}
	return resp, nil
}

// refusedLocationHeader carries hostPinTransport's description of a Location it
// removed, from the transport up to Client.do. It is set on the response only,
// after the venue's own header of that name has been deleted.
const refusedLocationHeader = "X-Broker-Refused-Location"

// Bounds on venue-controlled Location text: how much is parsed at all, and how
// much of the description reaches a message (cut by scrubTo, on its own
// positions).
const (
	maxLocationParseBytes = 4096
	maxLocationShownBytes = 200
)

// describeLocation resolves a Location against the request that drew it and
// returns its scheme and host ONLY. Not the path: a redirect may echo the
// request back, and a key inside a path can arrive percent-encoded, where no
// literal scrub will recognise it. Where a redirect pointed is the fact worth
// reporting; the rest is venue-controlled text.
func describeLocation(requestURL *url.URL, loc string) string {
	if len(loc) > maxLocationParseBytes {
		return fmt.Sprintf("<a Location of %d bytes>", len(loc))
	}
	abs, err := requestURL.Parse(loc)
	if err != nil {
		return "<an unparseable Location>"
	}
	if abs.Host == "" {
		return abs.Scheme + ":<no host>"
	}
	return (&url.URL{Scheme: abs.Scheme, Host: abs.Host}).String()
}

// redirectRefused is the error for every 3xx: the one Client.do reads (its
// Location already replaced by hostPinTransport), and — should that transport
// ever be unwrapped — the one http.Client returns when CheckRedirect refuses,
// with the response's body closed and our sentinel inside a *url.Error.
//
// That *url.Error is never wrapped: its URL is the venue's Location and may
// echo the signed query. Nor is the 3xx body kept — it is not the venue's
// verdict on the request. What callers branch on is kept: errors.Is on
// ErrRedirectAttempted, and errors.As on an *HTTPError carrying the status and
// an empty Body.
//
// fromTransport says which of the two it is. Only a response that came through
// hostPinTransport carries a refusedLocationHeader this package wrote; on the
// fallback path that header is whatever the venue sent, so it is ignored and
// the raw Location is described instead.
func (c *Client) redirectRefused(full string, resp *http.Response, fromTransport bool) error {
	status := 0
	target := "(no Location)"
	if resp != nil {
		status = resp.StatusCode
		if kept := resp.Header.Get(refusedLocationHeader); fromTransport && kept != "" {
			target = kept
		} else if loc := resp.Header.Get("Location"); !fromTransport && loc != "" {
			target = "<an unparseable request URL>"
			if base, err := url.Parse(full); err == nil {
				target = describeLocation(base, loc)
			}
		}
	}
	httpErr := &HTTPError{StatusCode: status, URL: redactURL(full)}
	return fmt.Errorf("%w: redirect to %s not followed: %w", ErrRedirectAttempted, printable(c.scrubTo(target, maxLocationShownBytes)), httpErr)
}

// printable replaces control bytes in venue-controlled text before it lands in
// an error message. Bounding is scrubTo's job, done before this.
func printable(s string) string {
	out := []rune{}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			r = '?'
		}
		out = append(out, r)
	}
	return string(out)
}
