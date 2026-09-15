package broker

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
//  2. CheckRedirect refuses every redirect on every client this package builds,
//     including one handed in through Config.HTTPClient. Behind the transport it
//     is not reached today; it is what still holds if that transport is ever
//     unwrapped. https://pkg.go.dev/net/http#Client (CheckRedirect)
//
// What neither wall covers: a Transport injected through Config.HTTPClient runs
// BELOW the host check, and can send a request wherever it likes. Injecting one
// is trusting it with the credential (see Config.HTTPClient).
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

// refuseRedirect is the CheckRedirect of every broker client. It refuses the
// first hop, so the request is never re-issued.
func refuseRedirect(*http.Request, []*http.Request) error {
	return ErrRedirectAttempted
}

// guardedHTTPClient returns the client a broker.Client uses: a COPY of the
// caller's, so the caller's own client is left as it was and cannot re-enable
// redirects on ours, with both walls installed.
func guardedHTTPClient(base *http.Client, pinnedHost string) *http.Client {
	var hc http.Client
	if base != nil {
		hc = *base
	} else {
		hc = http.Client{Timeout: 20 * time.Second}
	}
	hc.CheckRedirect = refuseRedirect
	next := hc.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	hc.Transport = &hostPinTransport{host: pinnedHost, next: next}
	return &hc
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
