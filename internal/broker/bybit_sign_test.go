package broker

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The vectors below were computed OUTSIDE Go, with
//
//	printf '%s' '<timestamp><key><recv_window><payload>' | openssl dgst -sha256 -hmac '<secret>'
//
// so this test cannot pass by agreeing with its own implementation.
func TestSignBybitV5_MatchesAnIndependentHMAC(t *testing.T) {
	secret, key := NewSecret("YYYYYYYYYY"), NewSecret("XXXXXXXXXX")
	cases := []struct {
		name        string
		timestampMs int64
		payload     string
		want        string
	}{
		{"GET query string", 1658385579423, "category=option&symbol=BTC-29JUL22-25000-C",
			"c2bc57ed60d70ba97f91c7cced8aa51da98f136d2793873d0fd064295c380c38"},
		{"POST raw JSON body", 1658384314791,
			`{"category":"linear","symbol":"BTCUSDT","side":"Buy","orderType":"Market","qty":"0.001","timeInForce":"IOC","orderLinkId":"a-1","reduceOnly":false,"positionIdx":0}`,
			"ff5fd6292bb6d1315911a39b852ed50e787e7dbf294b630253dbf90d3e9d2da2"},
	}
	for _, tc := range cases {
		if got := SignBybitV5(secret, tc.timestampMs, key, 5000, tc.payload); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestNewClient_BybitSchemeAcceptsOnlyTestnetAndDemo(t *testing.T) {
	creds := Credentials{APIKey: NewSecret("k"), APISecret: NewSecret("s")}
	build := func(base string, scheme SigningScheme) error {
		_, err := NewClient(Config{BaseURL: base, Scheme: scheme, Credentials: creds,
			TimePath: BybitTimePath, WeightLimitPerMin: BybitRequestsPerMin})
		return err
	}
	for _, base := range []string{BybitTestnetBaseURL, BybitDemoBaseURL, "https://api-demo.bybit.com/"} {
		if err := build(base, SchemeBybitV5Header); err != nil {
			t.Errorf("refused the documented non-production Bybit host %q: %v", base, err)
		}
	}
	refuse := map[string]string{
		"https://api.bybit.com":                  "PRODUCTION",
		"https://api.bytick.com":                 "PRODUCTION (alternate domain)",
		"https://api-testnet.bybit.com.evil.com": "a suffix that only looks like the host",
		"http://api-testnet.bybit.com":           "plaintext",
		"https://demo-fapi.binance.com":          "a BINANCE host under the Bybit scheme",
	}
	for base, why := range refuse {
		if err := build(base, SchemeBybitV5Header); err == nil {
			t.Errorf("ACCEPTED %q under the Bybit scheme (%s)", base, why)
		}
	}
	// And the reverse: a Bybit host under the Binance scheme.
	if err := build(BybitTestnetBaseURL, SchemeBinanceQuery); err == nil {
		t.Error("a Bybit host was accepted under the Binance scheme — one venue's allow-list widened the other's")
	}
	if err := build(BybitTestnetBaseURL, SigningScheme("made_up")); err == nil {
		t.Error("an unknown scheme was accepted")
	}
}

// bybitEcho answers /v5/market/time and records what the signed call carried.
type bybitEcho struct {
	gotHeader http.Header
	gotQuery  string
	gotBody   string
}

func (e *bybitEcho) RoundTrip(r *http.Request) (*http.Response, error) {
	body := `{"retCode":0,"retMsg":"OK","result":{},"time":1}`
	if r.URL.Path == BybitTimePath {
		nano := time.Now().UnixNano()
		body = `{"retCode":0,"retMsg":"OK","result":{"timeSecond":"1","timeNano":"` + strconv.FormatInt(nano, 10) + `"},"time":1}`
	} else {
		e.gotHeader, e.gotQuery = r.Header.Clone(), r.URL.RawQuery
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			e.gotBody = string(raw)
		}
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func TestBybitClient_SignsInHeadersAndNeverOnTheURL(t *testing.T) {
	echo := &bybitEcho{}
	c, err := NewClient(Config{BaseURL: BybitTestnetBaseURL, Scheme: SchemeBybitV5Header,
		Credentials: Credentials{APIKey: NewSecret("the-key"), APISecret: NewSecret("the-secret")},
		TimePath:    BybitTimePath, WeightLimitPerMin: BybitRequestsPerMin, TestTransport: echo})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := c.GetSignedV5(ctx, Endpoint{Path: "/v5/position/list", WeightIP: 1},
		[]Param{{"category", "linear"}, {"symbol", "BTCUSDT"}}, nil); err != nil {
		t.Fatal(err)
	}
	if echo.gotQuery != "category=linear&symbol=BTCUSDT" {
		t.Errorf("query carried more than the caller's parameters: %q", echo.gotQuery)
	}
	ts := echo.gotHeader.Get("X-BAPI-TIMESTAMP")
	want := SignBybitV5(NewSecret("the-secret"), parseInt(t, ts), NewSecret("the-key"), 5000, echo.gotQuery)
	if echo.gotHeader.Get("X-BAPI-SIGN") != want || echo.gotHeader.Get("X-BAPI-API-KEY") != "the-key" ||
		echo.gotHeader.Get("X-BAPI-RECV-WINDOW") != "5000" {
		t.Errorf("headers do not carry the signature of what was sent: %v", echo.gotHeader)
	}
	if echo.gotHeader.Get("X-MBX-APIKEY") != "" || strings.Contains(echo.gotQuery, "signature") {
		t.Error("a Binance-style credential leaked onto a Bybit request")
	}

	if err := c.PostSignedV5JSON(ctx, Endpoint{Path: "/v5/order/cancel", WeightIP: 1},
		map[string]string{"category": "linear", "symbol": "BTCUSDT", "orderLinkId": "x"}, nil); err != nil {
		t.Fatal(err)
	}
	ts = echo.gotHeader.Get("X-BAPI-TIMESTAMP")
	if got := echo.gotHeader.Get("X-BAPI-SIGN"); got != SignBybitV5(NewSecret("the-secret"), parseInt(t, ts), NewSecret("the-key"), 5000, echo.gotBody) {
		t.Error("the POST signature does not cover the exact body bytes sent")
	}
	if echo.gotHeader.Get("Content-Type") != "application/json" || echo.gotQuery != "" {
		t.Errorf("POST must be a JSON body with no query: content-type %q, query %q", echo.gotHeader.Get("Content-Type"), echo.gotQuery)
	}

	// The other venue's verbs are refused on this client, before any request.
	if err := c.GetSigned(ctx, Endpoint{Path: "/v5/x"}, nil, nil); err == nil {
		t.Error("a Binance-signed GET was allowed on a Bybit client")
	}
}

// TestnetHosts keeps its Binance-only meaning: guards in cmd/execcheck and
// cmd/execportal assert a Binance market resolves into it.
func TestTestnetHosts_IsBinanceOnly(t *testing.T) {
	for _, h := range TestnetHosts() {
		if h == BybitTestnetHost || h == BybitDemoHost {
			t.Errorf("TestnetHosts() lists the Bybit host %s — a Binance guard would now accept it", h)
		}
	}
}

// The 403 cool-down is per IP: every production client shares one, and only a
// client on a test transport (no IP of its own) gets a private one.
func TestBybitCooldown_SharedAcrossProductionClients(t *testing.T) {
	build := func(tt http.RoundTripper) *Client {
		c, err := NewClient(Config{BaseURL: BybitDemoBaseURL, Scheme: SchemeBybitV5Header,
			Credentials: Credentials{APIKey: NewSecret("k"), APISecret: NewSecret("s")},
			TimePath:    BybitTimePath, WeightLimitPerMin: BybitRequestsPerMin, TestTransport: tt})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := build(nil), build(nil)
	if a.cooldown != b.cooldown || a.cooldown != bybitIPCooldown {
		t.Error("two production Bybit clients do not share the IP cool-down")
	}
	if build(&bybitEcho{}).cooldown == bybitIPCooldown {
		t.Error("a test-transport client shares the process cool-down")
	}
}

func TestBinanceClient_RefusesBybitVerbs(t *testing.T) {
	c, err := NewClient(Config{BaseURL: BinanceFuturesTestnetBaseURL,
		Credentials: Credentials{APIKey: NewSecret("k"), APISecret: NewSecret("s")},
		TimePath:    BinanceFuturesTimePath, WeightLimitPerMin: BinanceFuturesWeightPerMin})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.GetSignedV5(context.Background(), Endpoint{Path: "/fapi/v1/x"}, nil, nil); err == nil {
		t.Error("a Bybit-signed GET was allowed on a Binance client")
	}
}

func parseInt(t *testing.T, s string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("timestamp header %q is not an integer", s)
	}
	return v
}
