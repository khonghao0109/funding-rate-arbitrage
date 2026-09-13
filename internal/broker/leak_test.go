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

	// timePath is which path that answer is given on. Empty means the futures
	// one; the spot testnet serves its clock at a different path, and a
	// transport that answered only the futures path would make a spot test
	// fail in the clock instead of reaching the call under test.
	timePath string

	// beforeRoundTrip lets a test move its own clock across the call, which is
	// how the round-trip midpoint is measured.
	beforeRoundTrip func()
}

func (t *pinnedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.beforeRoundTrip != nil {
		t.beforeRoundTrip()
	}
	timePath := t.timePath
	if timePath == "" {
		timePath = BinanceFuturesTimePath
	}
	if t.serverTimeMs > 0 && r.URL.Path == timePath {
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
		BaseURL:           BinanceFuturesTestnetBaseURL,
		Credentials:       Credentials{APIKey: NewSecret("KEY-" + sentinel), APISecret: NewSecret(sentinel)},
		RecvWindowMs:      5000,
		TimePath:          BinanceFuturesTimePath,
		WeightLimitPerMin: BinanceFuturesWeightPerMin,
		HTTPClient:        &http.Client{Transport: tr, Timeout: 5 * time.Second},
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
			err := client.GetSigned(context.Background(), FuturesAccountBalance, nil, &into)
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
	if err := client.GetSigned(context.Background(), FuturesAccountBalance, nil, &into); err != nil {
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
	if err := client.GetPublic(context.Background(), FuturesServerTime, nil, nil); err != nil {
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

// The same leak proof, run once per VENUE credential pair (2026-09-13).
//
// Splitting the credential by venue added a second way for a secret to enter
// the process — a different pair of environment variables, a different base URL
// and a different time path — and a redaction proven on one path is not proven
// on the other. This drives the whole thing from the environment, the way the
// command really loads it, so the loader and SourceVI are on the covered path
// too: SourceVI is a string built next to a secret, which is precisely where a
// well-meant "…and the key starts with" gets appended one day.
func TestClient_NeitherVenuesCredentialPairReachesALogOrAnError(t *testing.T) {
	venues := []struct {
		nameVI   string
		baseURL  string
		timePath string
		account  Endpoint
		weight   int
		pairs    []EnvPair
	}{
		{
			nameVI: "futures", baseURL: BinanceFuturesTestnetBaseURL,
			timePath: BinanceFuturesTimePath, account: FuturesAccountBalance,
			weight: BinanceFuturesWeightPerMin,
			pairs: []EnvPair{
				{KeyVar: "BROKER_LEAK_FUT_KEY", SecretVar: "BROKER_LEAK_FUT_SECRET"},
				{KeyVar: "BROKER_LEAK_LEGACY_KEY", SecretVar: "BROKER_LEAK_LEGACY_SECRET"},
			},
		},
		{
			nameVI: "spot", baseURL: BinanceSpotTestnetBaseURL,
			timePath: BinanceSpotTimePath, account: SpotAccount,
			weight: BinanceSpotWeightPerMin,
			pairs:  []EnvPair{{KeyVar: "BROKER_LEAK_SPOT_KEY", SecretVar: "BROKER_LEAK_SPOT_SECRET"}},
		},
	}

	for _, v := range venues {
		for _, via := range v.pairs {
			t.Run(v.nameVI+" via "+via.KeyVar, func(t *testing.T) {
				t.Setenv(via.KeyVar, "KEY-"+sentinel)
				t.Setenv(via.SecretVar, sentinel)

				creds, err := CredentialsFromEnvAny(v.pairs...)
				if err != nil {
					t.Fatalf("loading %s: %v", via.KeyVar, err)
				}
				// SourceVI is built beside the secret; it must name variables.
				if strings.Contains(creds.SourceVI, sentinel) {
					t.Fatalf("SourceVI carries a credential value: %q", scrubValue(creds.SourceVI, sentinel))
				}

				// The venue's own time path must answer, or this becomes a
				// test about the clock instead of about the balance call.
				tr := &pinnedTransport{
					failErr:      errors.New("dial tcp: connection refused"),
					serverTimeMs: time.Now().UnixMilli(),
					timePath:     v.timePath,
				}
				client, err := NewClient(Config{
					BaseURL: v.baseURL, Credentials: creds, RecvWindowMs: 5000,
					TimePath: v.timePath, WeightLimitPerMin: v.weight,
					HTTPClient: &http.Client{Transport: tr, Timeout: 5 * time.Second},
				})
				if err != nil {
					t.Fatalf("NewClient: %v", err)
				}

				var logged bytes.Buffer
				restore := log.Writer()
				log.SetOutput(&logged)
				defer log.SetOutput(restore)

				var into map[string]any
				err = client.GetSigned(context.Background(), v.account, nil, &into)
				if err == nil {
					t.Fatal("this case must fail; a success proves nothing about the error path")
				}
				log.Printf("%s balance failed: %v", v.nameVI, err)

				signature := signatureOf(t, tr.lastURL)
				for name, s := range map[string]string{
					"err.Error()": err.Error(),
					"%v":          fmt.Sprintf("%v", err),
					"%+v":         fmt.Sprintf("%+v", err),
					"%#v":         fmt.Sprintf("%#v", err),
					"wrapped":     fmt.Errorf("reading the testnet balance: %w", err).Error(),
					"creds %+v":   fmt.Sprintf("%+v", creds),
					"log output":  logged.String(),
				} {
					if strings.Contains(s, sentinel) {
						t.Errorf("%s leaked the credential: %q", name, scrubValue(s, sentinel))
					}
					if signature != "" && strings.Contains(s, signature) {
						t.Errorf("%s leaked the SIGNATURE: %q", name, strings.ReplaceAll(s, signature, Redacted))
					}
				}
			})
		}
	}
}
