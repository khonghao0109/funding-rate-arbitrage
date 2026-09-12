package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The leak test. It runs the whole client against a hostile server with a
// sentinel secret, captures EVERY string the run can produce — the standard
// logger's output and each error's message — and asserts that neither the
// secret nor the signature is anywhere in them.
//
// Three cases, because the three error paths fail differently:
//
//  1. a transport failure, where Go's own *url.Error prints the WHOLE URL and
//     the signature is a query parameter. This is the path that leaks by
//     default and the reason redactURL exists.
//  2. a non-2xx answer whose BODY echoes the request back. No Binance error
//     does this today, but a body is venue-controlled text and the client
//     cannot promise what is in it — so it is scrubbed rather than trusted.
//  3. a 200 whose body is not JSON, where the decoder quotes its input.

// pinnedTransport answers every request locally while the client still believes
// it is talking to the real testnet host. That keeps the testnet guard in force
// and the URL-building path real, and opens no socket to a venue.
type pinnedTransport struct {
	to      *url.URL
	failErr error
	lastURL string

	// serverTimeMs, when set, answers the server-time endpoint from here
	// instead of from `to`. Every signed call measures the clock first, and a
	// test about the BALANCE path must not be a test about the clock path.
	serverTimeMs int64

	// beforeRoundTrip lets a test move its own clock across the call, which is
	// how the round-trip midpoint is measured.
	beforeRoundTrip func()
}

func (t *pinnedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.beforeRoundTrip != nil {
		t.beforeRoundTrip()
	}
	if t.serverTimeMs > 0 && r.URL.Path == BinanceFuturesTimePath {
		return jsonResponse(r, fmt.Sprintf(`{"serverTime":%d}`, t.serverTimeMs)), nil
	}
	t.lastURL = r.URL.String()
	if t.failErr != nil {
		// http.Client wraps this in *url.Error carrying the full URL — exactly
		// what happens on a DNS failure or a dropped connection in production.
		return nil, t.failErr
	}
	routed := *r.URL
	routed.Scheme, routed.Host = t.to.Scheme, t.to.Host
	clone := r.Clone(r.Context())
	clone.URL = &routed
	clone.Host = ""
	return http.DefaultTransport.RoundTrip(clone)
}

func jsonResponse(r *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

func leakTestClient(t *testing.T, tr *pinnedTransport) *Client {
	t.Helper()
	c, err := NewClient(Config{
		BaseURL:      BinanceFuturesTestnetBaseURL,
		Credentials:  Credentials{APIKey: NewSecret("KEY-" + sentinel), APISecret: NewSecret(sentinel)},
		RecvWindowMs: 5000,
		TimePath:     BinanceFuturesTimePath,
		HTTPClient:   &http.Client{Transport: tr, Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestClient_NeitherTheSecretNorTheSignatureReachesALogOrAnError(t *testing.T) {
	// Every case ends in an error, on purpose: the success path prints
	// nothing, and it is the failure path that writes to logs.
	cases := []struct {
		name    string
		handler http.HandlerFunc
		failErr error
	}{
		{
			name:    "transport failure — Go's url.Error prints the whole URL",
			failErr: errors.New("dial tcp: connection refused"),
		},
		{
			name: "non-2xx whose body echoes the request back",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				// The worst case a venue could produce: the full request URI,
				// signature and all, reflected into the error body.
				fmt.Fprintf(w, `{"code":-1022,"msg":"Signature for this request is not valid: %s"}`, r.RequestURI)
			},
		},
		{
			name: "200 with a body the decoder will quote",
			handler: func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "not json, and here is your query again: %s", r.URL.RawQuery)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The clock endpoint always answers, agreeing with this machine:
			// in production the skew would have been measured long before the
			// call that fails, and these cases are about the failure, not the
			// clock.
			tr := &pinnedTransport{failErr: tc.failErr, serverTimeMs: time.Now().UnixMilli()}
			if tc.handler != nil {
				server := httptest.NewServer(tc.handler)
				defer server.Close()
				u, err := url.Parse(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				tr.to = u
			}
			client := leakTestClient(t, tr)

			// Capture the standard logger for the whole call: anything this
			// package logs, now or later, lands in here.
			var logged bytes.Buffer
			restore := log.Writer()
			log.SetOutput(&logged)
			defer log.SetOutput(restore)

			var into map[string]any
			err := client.GetSigned(context.Background(), "/fapi/v3/balance", nil, &into)
			if err == nil {
				t.Fatal("this case must fail; a success proves nothing about the error path")
			}

			// What a caller does with the error: print it plainly, print it
			// verbosely, wrap it, and log it. All four are fmt.
			surfaces := map[string]string{
				"err.Error()": err.Error(),
				"%v":          fmt.Sprintf("%v", err),
				"%+v":         fmt.Sprintf("%+v", err),
				"%#v":         fmt.Sprintf("%#v", err),
				"wrapped":     fmt.Errorf("reading the testnet balance: %w", err).Error(),
			}
			log.Printf("balance failed: %v", err)
			surfaces["log output"] = logged.String()

			// The signature this very call generated — recovered from what the
			// transport actually saw, so the test checks the real digest and
			// not one it computed for itself.
			signature := signatureOf(t, tr.lastURL)

			for name, s := range surfaces {
				if strings.Contains(s, sentinel) {
					t.Errorf("%s leaked the SECRET: %q", name, scrubValue(s, sentinel))
				}
				if signature != "" && strings.Contains(s, signature) {
					t.Errorf("%s leaked the SIGNATURE: %q", name, strings.ReplaceAll(s, signature, Redacted))
				}
				// The message still has to be useful, or the next person
				// removes the redaction to debug something.
				if !strings.Contains(s, "/fapi/v3/balance") {
					t.Errorf("%s says nothing about which endpoint failed: %q", name, s)
				}
			}
		})
	}
}

// The request really was signed and really did carry the key header — otherwise
// the test above would pass on a client that sends no credential at all.
func TestClient_SignsTheRequestAndSendsTheKeyInTheDocumentedHeader(t *testing.T) {
	var gotHeader, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader, gotQuery = r.Header.Get("X-MBX-APIKEY"), r.URL.RawQuery
		fmt.Fprint(w, `[{"asset":"USDT","balance":"1000"}]`)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := leakTestClient(t, &pinnedTransport{to: u, serverTimeMs: time.Now().UnixMilli()})

	var into []map[string]any
	if err := client.GetSigned(context.Background(), "/fapi/v3/balance", nil, &into); err != nil {
		t.Fatalf("GetSigned: %v", err)
	}
	if gotHeader != "KEY-"+sentinel {
		t.Error("the API key did not travel in X-MBX-APIKEY")
	}

	values, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatal(err)
	}
	if values.Get("recvWindow") != "5000" {
		t.Errorf("recvWindow = %q, want it sent explicitly rather than left to the venue's 5000 default", values.Get("recvWindow"))
	}
	if values.Get("timestamp") == "" {
		t.Error("no timestamp was sent; a SIGNED endpoint requires one")
	}
	// The signature must cover exactly the bytes before it.
	cut := strings.Index(gotQuery, "&signature=")
	if cut < 0 {
		t.Fatal("no signature parameter was sent")
	}
	totalParams, sent := gotQuery[:cut], gotQuery[cut+len("&signature="):]
	if want := Sign(NewSecret(sentinel), totalParams); sent != want {
		t.Errorf("the signature does not cover the query as sent:\n got %s\nwant %s", sent, want)
	}

	// An unsigned call must carry NO identity at all.
	gotHeader = ""
	if err := client.GetPublic(context.Background(), "/fapi/v1/time", nil, nil); err != nil {
		t.Fatalf("GetPublic: %v", err)
	}
	if gotHeader != "" {
		t.Error("GetPublic sent the API key; an endpoint that needs no identity must not be handed one")
	}
}

// signatureOf recovers the signature from the URL the transport saw.
func signatureOf(t *testing.T, raw string) string {
	t.Helper()
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Query().Get("signature")
}
