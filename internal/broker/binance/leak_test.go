package binance

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

	"futures-arbitrage-scanner/internal/broker"
)

// The step-4.1 leak proof, extended to the step-4.2 write verbs.
//
// 4.1's proof covered GET, where the signature rides on the QUERY STRING and
// the door that had to be closed was Go's *url.Error printing the whole URL.
// POST moves the parameters — and therefore the signature — into the REQUEST
// BODY, which is a different door: the URL is now clean, and the thing that can
// leak is a body echoed back by the venue, or logged on the way out.
//
// So the same assertions are re-run over PlaceOrder and CancelOrder rather than
// assumed to carry over. They do not carry over by construction: nothing in
// redactURL looks at a body.
const sentinel = "SENTINEL-9f31c07a-NEVER-PRINT-THIS-SECRET"

// echoTransport answers locally while the client still believes it is talking
// to the testnet host, so the host guard stays in force and no socket is
// opened to a venue.
type echoTransport struct {
	to       *url.URL
	timePath string
	failErr  error

	lastURL  string
	lastBody string
}

func (t *echoTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == t.timePath {
		return &http.Response{
			StatusCode: 200, Status: "200 OK",
			Header:  http.Header{"Content-Type": []string{"application/json"}},
			Body:    io.NopCloser(strings.NewReader(fmt.Sprintf(`{"serverTime":%d}`, time.Now().UnixMilli()))),
			Request: r,
		}, nil
	}
	t.lastURL = r.URL.String()
	// Reset per request: retaining the previous call's body would replay a
	// POST's parameters onto a DELETE and make the test lie about both.
	t.lastBody = ""
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		t.lastBody = string(raw)
		r.Body = io.NopCloser(bytes.NewReader(raw))
	}
	if t.failErr != nil {
		return nil, t.failErr
	}
	routed := *r.URL
	routed.Scheme, routed.Host = t.to.Scheme, t.to.Host
	clone := r.Clone(r.Context())
	clone.URL = &routed
	clone.Host = ""
	if t.lastBody != "" {
		clone.Body = io.NopCloser(strings.NewReader(t.lastBody))
		clone.ContentLength = int64(len(t.lastBody))
	}
	return http.DefaultTransport.RoundTrip(clone)
}

func leakClient(t *testing.T, market broker.Market, tr *echoTransport) *Client {
	t.Helper()
	creds := broker.Credentials{APIKey: broker.NewSecret("KEY-" + sentinel), APISecret: broker.NewSecret(sentinel)}
	cfg, err := DefaultConfig(market, creds)
	if err != nil {
		t.Fatal(err)
	}
	tr.timePath = cfg.TimePath
	cfg.TestTransport = tr
	c, err := New(market, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestWriteVerbs_NeitherTheSecretNorTheSignatureReachesALogOrAnError(t *testing.T) {
	req := broker.PlaceOrderRequest{
		Symbol: "BTCUSDT", Side: broker.SideBuy, Type: broker.OrderTypeLimitGTC,
		ClientOrderID: "leak-1", QtyCoin: 0.01, PriceQuote: 30_000,
	}
	q := broker.OrderQuery{Symbol: "BTCUSDT", ClientOrderID: "leak-1"}

	// The venue answers hostilely in three ways, on each of the two verbs.
	handlers := map[string]http.HandlerFunc{
		"a 400 that echoes the REQUEST BODY back": func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusBadRequest)
			// The worst case: the signed body, signature and all, reflected.
			fmt.Fprintf(w, `{"code":-1022,"msg":"Signature for this request is not valid: %s %s"}`, r.RequestURI, raw)
		},
		"a 200 the decoder will choke on, quoting its input": func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			fmt.Fprintf(w, "not json, and here is your request: %s %s", r.URL.RawQuery, raw)
		},
	}

	for _, market := range []broker.Market{broker.MarketFuturesUSDM, broker.MarketSpot} {
		for name, handler := range handlers {
			for _, verb := range []string{"PlaceOrder", "CancelOrder"} {
				t.Run(string(market)+"/"+verb+"/"+name, func(t *testing.T) {
					server := httptest.NewServer(handler)
					defer server.Close()
					u, err := url.Parse(server.URL)
					if err != nil {
						t.Fatal(err)
					}
					tr := &echoTransport{to: u}
					c := leakClient(t, market, tr)

					var logged bytes.Buffer
					restore := log.Writer()
					log.SetOutput(&logged)
					defer log.SetOutput(restore)

					r, qq := req, q
					r.Market, qq.Market = market, market
					var callErr error
					if verb == "PlaceOrder" {
						_, callErr = c.PlaceOrder(context.Background(), r)
					} else {
						_, callErr = c.CancelOrder(context.Background(), qq)
					}
					if callErr == nil {
						t.Fatal("this case must fail; a success proves nothing about the error path")
					}
					log.Printf("%s failed: %v", verb, callErr)

					// The signature this very call produced, recovered from
					// what the transport actually saw — so the assertion is
					// about the real digest, not one the test computed.
					signature := signatureFrom(tr.lastURL, tr.lastBody)
					if signature == "" {
						t.Fatal("no signature was sent at all; this test would pass vacuously")
					}

					for surface, s := range map[string]string{
						"err.Error()": callErr.Error(),
						"%v":          fmt.Sprintf("%v", callErr),
						"%+v":         fmt.Sprintf("%+v", callErr),
						"%#v":         fmt.Sprintf("%#v", callErr),
						"wrapped":     fmt.Errorf("placing the testnet order: %w", callErr).Error(),
						"log output":  logged.String(),
					} {
						if strings.Contains(s, sentinel) {
							t.Errorf("%s leaked the SECRET: %q", surface, strings.ReplaceAll(s, sentinel, broker.Redacted))
						}
						if strings.Contains(s, signature) {
							t.Errorf("%s leaked the SIGNATURE: %q", surface, strings.ReplaceAll(s, signature, broker.Redacted))
						}
					}
				})
			}
		}
	}
}

// A transport failure on a POST: Go's *url.Error prints the URL, which for a
// body-carrying request holds no parameters at all — but the assertion is made
// rather than reasoned about, because that is the whole point of this file.
func TestWriteVerbs_ATransportFailureLeaksNothing(t *testing.T) {
	tr := &echoTransport{failErr: errors.New("dial tcp: connection refused")}
	c := leakClient(t, broker.MarketFuturesUSDM, tr)

	var logged bytes.Buffer
	restore := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(restore)

	_, err := c.PlaceOrder(context.Background(), broker.PlaceOrderRequest{
		Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", Side: broker.SideBuy,
		Type: broker.OrderTypeLimitGTC, ClientOrderID: "leak-2", QtyCoin: 0.01, PriceQuote: 30_000,
	})
	if err == nil {
		t.Fatal("a refused dial must be an error")
	}
	log.Printf("place failed: %v", err)

	signature := signatureFrom(tr.lastURL, tr.lastBody)
	if signature == "" {
		t.Fatal("nothing was signed; the test would pass vacuously")
	}
	for surface, s := range map[string]string{
		"err.Error()": err.Error(),
		"log output":  logged.String(),
	} {
		if strings.Contains(s, sentinel) || strings.Contains(s, signature) {
			t.Errorf("%s leaked a credential or signature", surface)
		}
		// It still has to be useful.
		if !strings.Contains(s, "/fapi/v1/order") {
			t.Errorf("%s does not say which endpoint failed: %q", surface, s)
		}
	}
}

// The request really is signed, the signature really is in the BODY for a POST
// and on the QUERY for a DELETE, and the key really does travel in the
// documented header. Without this, the leak tests above could pass on a client
// that sends no credential at all.
func TestWriteVerbs_SignTheDocumentedHalfOfTheRequest(t *testing.T) {
	for _, market := range []broker.Market{broker.MarketFuturesUSDM, broker.MarketSpot} {
		t.Run(string(market), func(t *testing.T) {
			var gotKeyHeader, gotContentType, gotQuery, gotBody, gotMethod string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				gotKeyHeader, gotContentType = r.Header.Get("X-MBX-APIKEY"), r.Header.Get("Content-Type")
				gotQuery, gotBody, gotMethod = r.URL.RawQuery, string(raw), r.Method
				fmt.Fprint(w, `{"symbol":"BTCUSDT","orderId":1,"clientOrderId":"sign-1","status":"NEW","origQty":"0.01","executedQty":"0","price":"30000"}`)
			}))
			defer server.Close()
			u, _ := url.Parse(server.URL)
			c := leakClient(t, market, &echoTransport{to: u})

			// POST: the parameters, and the signature, in the body.
			if _, err := c.PlaceOrder(context.Background(), broker.PlaceOrderRequest{
				Market: market, Symbol: "BTCUSDT", Side: broker.SideBuy,
				Type: broker.OrderTypeLimitGTC, ClientOrderID: "sign-1", QtyCoin: 0.01, PriceQuote: 30_000,
			}); err != nil {
				t.Fatalf("PlaceOrder: %v", err)
			}
			if gotMethod != http.MethodPost {
				t.Errorf("method = %s, want POST", gotMethod)
			}
			if gotKeyHeader != "KEY-"+sentinel {
				t.Error("the API key did not travel in X-MBX-APIKEY")
			}
			if gotContentType != "application/x-www-form-urlencoded" {
				t.Errorf("Content-Type = %q, want the documented form encoding", gotContentType)
			}
			if gotQuery != "" {
				t.Errorf("a body-carrying POST also put parameters on the URL: %q", gotQuery)
			}
			assertSignatureCovers(t, gotBody)
			for _, want := range []string{"symbol=BTCUSDT", "side=BUY", "type=LIMIT", "timeInForce=GTC", "newClientOrderId=sign-1"} {
				if !strings.Contains(gotBody, want) {
					t.Errorf("the body is missing %s", want)
				}
			}

			// DELETE: the parameters, and the signature, on the query string.
			gotQuery, gotBody = "", ""
			if _, err := c.CancelOrder(context.Background(), broker.OrderQuery{
				Market: market, Symbol: "BTCUSDT", ClientOrderID: "sign-1",
			}); err != nil {
				t.Fatalf("CancelOrder: %v", err)
			}
			if gotMethod != http.MethodDelete {
				t.Errorf("method = %s, want DELETE", gotMethod)
			}
			if gotBody != "" {
				t.Errorf("the DELETE carried a body: %q", gotBody)
			}
			assertSignatureCovers(t, gotQuery)
			if !strings.Contains(gotQuery, "origClientOrderId=sign-1") {
				t.Errorf("the cancel did not ask by the caller's id: %q", gotQuery)
			}
		})
	}
}

// assertSignatureCovers checks that the signature is the LAST parameter and
// covers exactly the bytes before it — the property that makes signing the
// same string that is sent, rather than a second rendering of it.
func assertSignatureCovers(t *testing.T, encoded string) {
	t.Helper()
	cut := strings.Index(encoded, "&signature=")
	if cut < 0 {
		t.Fatalf("no signature parameter in %q", encoded)
	}
	totalParams, sent := encoded[:cut], encoded[cut+len("&signature="):]
	if strings.Contains(sent, "&") {
		t.Errorf("the signature is not the last parameter: %q", encoded)
	}
	if want := broker.Sign(broker.NewSecret(sentinel), totalParams); sent != want {
		t.Errorf("the signature does not cover the bytes as sent:\n got %s\nwant %s", sent, want)
	}
}

func signatureFrom(rawURL, body string) string {
	if v, err := url.ParseQuery(body); err == nil && v.Get("signature") != "" {
		return v.Get("signature")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("signature")
}
