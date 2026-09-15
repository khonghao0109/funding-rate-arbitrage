package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Found by the step-4.1 acceptance run on 2026-09-13, which is what an
// acceptance run is for.
//
// The response body was read through io.LimitReader(resp.Body, 4096). That
// bound is right for an ERROR body — it is venue-controlled text that gets
// scrubbed into an error message — but it was applied to the SUCCESS body as
// well. A Binance futures balance is eight assets and fits; a spot
// /api/v3/account lists hundreds and does not. The venue answered HTTP 200
// with correct JSON and the client reported
//
//	decode: unexpected end of JSON input
//
// which reads like the venue sent nonsense. The failure mode that matters is
// not the error, it is that the size at which it appears depends on how many
// assets an account happens to hold: a client tested on a small account breaks
// later, on a bigger one, with a message pointing at the wrong party.
func TestClient_DecodesAResponseLargerThanAnErrorBodyLimit(t *testing.T) {
	// Comfortably past 4096 bytes, in the shape the spot account really has.
	const assets = 400
	type balance struct {
		Asset  string `json:"asset"`
		Free   string `json:"free"`
		Locked string `json:"locked"`
	}
	want := make([]balance, assets)
	for i := range want {
		want[i] = balance{Asset: fmt.Sprintf("ASSET%03d", i), Free: "0.00000000", Locked: "0.00000000"}
	}
	payload, err := json.Marshal(map[string]any{"accountType": "SPOT", "balances": want})
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) <= 4096 {
		t.Fatalf("the fixture is only %d bytes — it must exceed the old 4096 limit to prove anything", len(payload))
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	client := leakTestClient(t, &pinnedTransport{to: u, serverTimeMs: time.Now().UnixMilli()})
	var got struct {
		AccountType string    `json:"accountType"`
		Balances    []balance `json:"balances"`
	}
	if err := client.GetSigned(context.Background(), FuturesAccountBalance, nil, &got); err != nil {
		t.Fatalf("a %d-byte HTTP 200 must decode: %v", len(payload), err)
	}
	if len(got.Balances) != assets {
		t.Errorf("decoded %d balances, want %d — the body was truncated", len(got.Balances), assets)
	}
}

// A body beyond the ceiling must say SO, rather than arriving at the decoder as
// truncated JSON and being reported as the venue's fault.
func TestClient_ABodyOverTheCeilingIsNamedNotMisreportedAsBadJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[\"" + strings.Repeat("x", maxResponseBytes+64) + "\"]"))
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	client := leakTestClient(t, &pinnedTransport{to: u, serverTimeMs: time.Now().UnixMilli()})
	var into []string
	err = client.GetSigned(context.Background(), FuturesAccountBalance, nil, &into)
	if err == nil {
		t.Fatal("a body past the ceiling must be an error")
	}
	if strings.Contains(err.Error(), "unexpected end of JSON input") {
		t.Errorf("an oversized body is reported as malformed JSON, which blames the venue: %v", err)
	}
	if !strings.Contains(err.Error(), "quá lớn") {
		t.Errorf("the error must say the answer was too large, got: %v", err)
	}
}

// The error body stays SHORT. It is venue-controlled text that lands in an
// error message, a log and a report, and raising the success ceiling must not
// raise what a hostile 400 can paste into all three.
func TestClient_AnErrorBodyIsStillTruncatedShort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(strings.Repeat("E", 100_000)))
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	client := leakTestClient(t, &pinnedTransport{to: u, serverTimeMs: time.Now().UnixMilli()})
	var into map[string]any
	err = client.GetSigned(context.Background(), FuturesAccountBalance, nil, &into)
	if err == nil {
		t.Fatal("HTTP 400 must be an error")
	}
	if len(err.Error()) > maxErrorBodyBytes+1024 {
		t.Errorf("the error message is %d bytes — a venue can paste an arbitrary amount into a log", len(err.Error()))
	}
}

// ObserveResponse is the only way production code sees a response the broker
// did not decode for it (cmd/brokercheck's testdata capture). It gets the
// answer — method, path, status, body — and nothing of the request: no query,
// so no signature, and no header, so no key. It can send nothing.
func TestClient_ObserveResponseSeesTheAnswerAndNothingOfTheRequest(t *testing.T) {
	want := reflect.TypeOf(ResponseRecord{})
	fields := []string{}
	for i := 0; i < want.NumField(); i++ {
		fields = append(fields, want.Field(i).Name+" "+want.Field(i).Type.String())
	}
	if got := strings.Join(fields, ", "); got != "Method string, Path string, StatusCode int, Body []uint8" {
		t.Errorf("ResponseRecord = {%s}; a field added here reaches production code — keep it to the answer", got)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == BinanceFuturesTimePath {
			fmt.Fprintf(w, `{"serverTime":%d}`, time.Now().UnixMilli())
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":-2013,"msg":"Order does not exist."}`)
	}))
	defer srv.Close()
	to, _ := url.Parse(srv.URL)
	var seen []ResponseRecord
	c, err := NewClient(Config{
		BaseURL:           BinanceFuturesTestnetBaseURL,
		Credentials:       Credentials{APIKey: NewSecret("KEY-" + sentinel), APISecret: NewSecret(sentinel)},
		TimePath:          BinanceFuturesTimePath,
		WeightLimitPerMin: BinanceFuturesWeightPerMin,
		TestTransport:     &pinnedTransport{to: to},
		ObserveResponse:   func(rec ResponseRecord) { seen = append(seen, rec) },
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.GetSigned(context.Background(), FuturesNewOrder, []Param{{"symbol", "BTCUSDT"}}, nil)
	if len(seen) != 2 {
		t.Fatalf("observed %d responses, want 2 (the clock, then the order query): %+v", len(seen), seen)
	}
	order := seen[1]
	if order.Method != http.MethodGet || order.Path != FuturesNewOrder.Path || order.StatusCode != http.StatusBadRequest || !strings.Contains(string(order.Body), "-2013") {
		t.Errorf("observed %+v", order)
	}
	if strings.ContainsAny(order.Path, "?&") || strings.Contains(fmt.Sprintf("%+v", seen), sentinel[:12]) {
		t.Errorf("the observer saw part of the request: %+v", seen)
	}
}

// The observer runs after the request was sent — for an order, after it is
// live — so it cannot be allowed to take the return away (a panic is
// recovered), it is not shown a 3xx (not a venue answer), and the body it is
// shown is scrubbed like any error text, since Capture commits it to git.
func TestClient_ObserveResponseIsScrubbedSkipsRedirectsAndCannotPanicTheCaller(t *testing.T) {
	status := http.StatusBadRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"code":-1022,"msg":"echo KEY-%s signature=deadbeefdeadbeefdeadbeef"}`, sentinel)
	}))
	defer srv.Close()
	to, _ := url.Parse(srv.URL)
	var seen []ResponseRecord
	c, err := NewClient(Config{
		BaseURL:           BinanceFuturesTestnetBaseURL,
		Credentials:       Credentials{APIKey: NewSecret("KEY-" + sentinel), APISecret: NewSecret(sentinel)},
		WeightLimitPerMin: BinanceFuturesWeightPerMin,
		TestTransport:     &pinnedTransport{to: to},
		ObserveResponse: func(rec ResponseRecord) {
			seen = append(seen, rec)
			panic("an observer that panics")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ep := Endpoint{Path: "/fapi/v1/exchangeInfo", WeightIP: 1}
	err = c.GetPublic(context.Background(), ep, nil, nil)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("the caller did not get its return after the observer panicked: %v", err)
	}
	if len(seen) != 1 || strings.Contains(string(seen[0].Body), sentinel[:12]) || strings.Contains(string(seen[0].Body), "deadbeef") {
		t.Errorf("observed %q — want one record with the key and the signature scrubbed", seen)
	}
	status = http.StatusTemporaryRedirect
	seen = nil
	if err := c.GetPublic(context.Background(), ep, nil, nil); !errors.Is(err, ErrRedirectAttempted) {
		t.Fatalf("err = %v", err)
	}
	if len(seen) != 0 {
		t.Errorf("a 3xx was shown to the observer: %+v", seen)
	}
}
