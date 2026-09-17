package bybit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/execution"
)

// The tests run against a mock Bybit V5 server: an http.Handler served through
// httptest.NewRecorder behind Config.TestTransport, so the client still believes
// it is talking to api-testnet.bybit.com — the host pin stays in force — and no
// socket is opened. The mock VERIFIES every signed request's X-BAPI-SIGN
// against the exact bytes it received, so a test cannot pass on a client that
// signs one thing and sends another.

const (
	testKey    = "TESTKEY-bybit-0000"
	testSecret = "SENTINEL-bybit-secret-NEVER-PRINT"
)

type mockV5 struct {
	t  *testing.T
	mu sync.Mutex
	// routes answers METHOD+" "+path with a status and a body.
	routes map[string]func(query string, body []byte) (int, string)
	calls  []string
}

func newMock(t *testing.T) *mockV5 {
	return &mockV5{t: t, routes: map[string]func(string, []byte) (int, string){}}
}

func (m *mockV5) on(route string, f func(query string, body []byte) (int, string)) {
	m.routes[route] = f
}

func (m *mockV5) ok(route, result string) {
	m.on(route, func(string, []byte) (int, string) { return 200, envelopeOK(result) })
}

func envelopeOK(result string) string {
	return `{"retCode":0,"retMsg":"OK","result":` + result + `,"retExtInfo":{},"time":1}`
}

func envelopeErr(code int, msg string) string {
	return fmt.Sprintf(`{"retCode":%d,"retMsg":%q,"result":{},"retExtInfo":{},"time":1}`, code, msg)
}

func (m *mockV5) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	route := r.Method + " " + r.URL.Path
	if r.URL.Path == broker.BybitTimePath {
		fmt.Fprintf(w, `{"retCode":0,"retMsg":"OK","result":{"timeSecond":"1","timeNano":"%d"},"time":1}`, time.Now().UnixNano())
		return
	}
	m.mu.Lock()
	m.calls = append(m.calls, route)
	m.mu.Unlock()
	private := !strings.HasPrefix(r.URL.Path, "/v5/market/")
	if private && r.Header.Get("X-BAPI-SIGN") == "" {
		m.t.Errorf("mock: private route %s reached the venue unsigned", route)
	}
	if !private && r.Header.Get("X-BAPI-API-KEY") != "" {
		m.t.Errorf("mock: public route %s carried the API key", route)
	}
	if private {
		payload := r.URL.RawQuery
		if r.Method == http.MethodPost {
			payload = string(body)
		}
		ts, _ := strconv.ParseInt(r.Header.Get("X-BAPI-TIMESTAMP"), 10, 64)
		recv, _ := strconv.ParseInt(r.Header.Get("X-BAPI-RECV-WINDOW"), 10, 64)
		want := broker.SignBybitV5(broker.NewSecret(testSecret), ts, broker.NewSecret(testKey), recv, payload)
		if r.Header.Get("X-BAPI-SIGN") != want {
			w.WriteHeader(200)
			io.WriteString(w, envelopeErr(10004, "Error sign"))
			return
		}
	}
	f, ok := m.routes[route]
	if !ok {
		m.t.Errorf("mock: unexpected call %s?%s", route, r.URL.RawQuery)
		w.WriteHeader(404)
		return
	}
	status, out := f(r.URL.RawQuery, body)
	w.WriteHeader(status)
	io.WriteString(w, out)
}

type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

func newTestClient(t *testing.T, m *mockV5) *Client {
	t.Helper()
	cfg, err := DefaultConfig(ModeTestnet, broker.Credentials{APIKey: broker.NewSecret(testKey), APISecret: broker.NewSecret(testSecret)})
	if err != nil {
		t.Fatal(err)
	}
	cfg.TestTransport = handlerTransport{m}
	c, err := New(ModeTestnet, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var ctx = context.Background()

func TestPlaceOrder_SendsTheDocumentedBodyAndReportsAnAckNotAFill(t *testing.T) {
	m := newMock(t)
	var sent []byte
	m.on("POST /v5/order/create", func(_ string, body []byte) (int, string) {
		sent = body
		return 200, envelopeOK(`{"orderId":"1321003749386327552","orderLinkId":"fa1p0123456789abcdef01234567"}`)
	})
	c := newTestClient(t, m)
	o, err := c.PlaceOrder(ctx, broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT",
		Side: broker.SideSell, Type: broker.OrderTypeMarket, ClientOrderID: "fa1p0123456789abcdef01234567", QtyCoin: 0.001})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"category":"linear","symbol":"BTCUSDT","side":"Sell","orderType":"Market","qty":"0.001","timeInForce":"IOC","orderLinkId":"fa1p0123456789abcdef01234567","reduceOnly":false,"positionIdx":0}`
	if string(sent) != want {
		t.Errorf("body\n got %s\nwant %s", sent, want)
	}
	if o.Status != broker.OrderStatusNew || o.FilledQtyCoin != 0 || o.VenueOrderID != "1321003749386327552" {
		t.Errorf("an acknowledgement must read as NEW with nothing filled: %+v", o)
	}
}

func TestPlaceOrder_LimitGTCAndReduceOnly(t *testing.T) {
	m := newMock(t)
	var got map[string]any
	m.on("POST /v5/order/create", func(_ string, body []byte) (int, string) {
		_ = json.Unmarshal(body, &got)
		return 200, envelopeOK(`{"orderId":"9","orderLinkId":"x-1"}`)
	})
	c := newTestClient(t, m)
	if _, err := c.PlaceOrder(ctx, broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: "ETHUSDT",
		Side: broker.SideBuy, Type: broker.OrderTypeLimitGTC, ClientOrderID: "x-1", QtyCoin: 0.25, PriceQuote: 1234.5, ReduceOnly: true}); err != nil {
		t.Fatal(err)
	}
	if got["orderType"] != "Limit" || got["timeInForce"] != "GTC" || got["price"] != "1234.5" || got["reduceOnly"] != true || got["side"] != "Buy" {
		t.Errorf("limit body wrong: %v", got)
	}
}

func TestPlaceOrder_RefusesBeforeSending(t *testing.T) {
	m := newMock(t) // no routes: any request fails the test
	c := newTestClient(t, m)
	base := broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", Side: broker.SideBuy,
		Type: broker.OrderTypeMarket, QtyCoin: 0.001}
	cases := map[string]broker.PlaceOrderRequest{}
	long := base
	long.ClientOrderID = strings.Repeat("a", 37)
	cases["37-character id"] = long
	bad := base
	bad.ClientOrderID = "has|pipe"
	cases["illegal character"] = bad
	spot := base
	spot.ClientOrderID, spot.Market = "ok", broker.MarketSpot
	cases["spot market"] = spot
	for name, req := range cases {
		if _, err := c.PlaceOrder(ctx, req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("refused requests reached the venue: %v", m.calls)
	}
}

// Every id internal/execution derives must be accepted as it is, never shortened.
func TestExecutionDerivedIDs_FitBybitsOrderLinkID(t *testing.T) {
	for _, leg := range []execution.LegName{execution.LegSpot, execution.LegPerp} {
		for _, id := range []string{
			execution.LegClientOrderID("abtcusdt-20260915-054920-740", leg),
			execution.CloseClientOrderID("abtcusdt-20260915-054920-740", leg),
			execution.ReconcileClientOrderID("abtcusdt-20260915-054920-740", leg),
		} {
			if err := checkOrderLinkID(id); err != nil {
				t.Errorf("execution id %q refused: %v", id, err)
			}
		}
	}
}

func TestPlaceOrder_AckNamingAnotherIDIsUnknownNotSuccess(t *testing.T) {
	m := newMock(t)
	m.ok("POST /v5/order/create", `{"orderId":"1","orderLinkId":"someone-else"}`)
	c := newTestClient(t, m)
	_, err := c.PlaceOrder(ctx, broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT",
		Side: broker.SideBuy, Type: broker.OrderTypeMarket, ClientOrderID: "mine", QtyCoin: 0.001})
	if err == nil || errors.Is(err, broker.ErrOrderNotFound) {
		t.Fatalf("a mismatched acknowledgement must be an ambiguous error, got %v", err)
	}
}

const filledOrder = `{"list":[{"orderId":"77","orderLinkId":"lk","symbol":"BTCUSDT","side":"Sell","orderType":"Market","timeInForce":"IOC","orderStatus":"Filled","price":"59000","qty":"0.002","avgPrice":"60001.5","cumExecQty":"0.002","reduceOnly":false,"createdTime":"1700000000000","updatedTime":"1700000000100","positionIdx":0}],"nextPageCursor":""}`

func TestGetOrder_RealtimeThenHistoryThenNotFound(t *testing.T) {
	q := broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", ClientOrderID: "lk"}

	m := newMock(t)
	m.ok("GET /v5/order/realtime", filledOrder)
	o, err := newTestClient(t, m).GetOrder(ctx, q)
	if err != nil || o.Status != broker.OrderStatusFilled || o.FilledQtyCoin != 0.002 || o.AvgFillPriceQuote != 60001.5 || o.Side != broker.SideSell {
		t.Fatalf("realtime: %+v %v", o, err)
	}

	m = newMock(t)
	m.ok("GET /v5/order/realtime", `{"list":[],"nextPageCursor":""}`)
	m.ok("GET /v5/order/history", filledOrder)
	if o, err := newTestClient(t, m).GetOrder(ctx, q); err != nil || o.VenueOrderID != "77" {
		t.Fatalf("history fallback: %+v %v", o, err)
	}

	m = newMock(t)
	m.ok("GET /v5/order/realtime", `{"list":[],"nextPageCursor":""}`)
	m.ok("GET /v5/order/history", `{"list":[],"nextPageCursor":""}`)
	if _, err := newTestClient(t, m).GetOrder(ctx, q); !errors.Is(err, ErrOrderNotVisible) || errors.Is(err, broker.ErrOrderNotFound) {
		t.Fatalf("both empty must be AMBIGUOUS (ErrOrderNotVisible), never ErrOrderNotFound — got %v", err)
	}

	m = newMock(t)
	m.ok("GET /v5/order/realtime", strings.Replace(filledOrder, `"orderLinkId":"lk"`, `"orderLinkId":"other"`, 1))
	if _, err := newTestClient(t, m).GetOrder(ctx, q); err == nil || errors.Is(err, broker.ErrOrderNotFound) {
		t.Fatalf("a list naming another id is UNKNOWN, not not-found: %v", err)
	}
}

func TestStatusMapping(t *testing.T) {
	cases := map[string]broker.OrderStatus{
		"New": broker.OrderStatusNew, "PartiallyFilled": broker.OrderStatusPartiallyFilled,
		"Filled": broker.OrderStatusFilled, "Cancelled": broker.OrderStatusCanceled,
		"PartiallyFilledCanceled": broker.OrderStatusCanceled, "Deactivated": broker.OrderStatusCanceled,
		"Rejected": broker.OrderStatusRejected, "Untriggered": broker.OrderStatusUnknown,
		"Triggered": broker.OrderStatusUnknown, "SomethingNew": broker.OrderStatusUnknown,
	}
	for in, want := range cases {
		if got := normalizeStatus(in); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
}

// "Cancelled — In derivatives, orders with this status may have an executed qty".
func TestCancelledOrderKeepsItsFill(t *testing.T) {
	m := newMock(t)
	m.ok("GET /v5/order/realtime", strings.NewReplacer(`"orderStatus":"Filled"`, `"orderStatus":"Cancelled"`,
		`"cumExecQty":"0.002"`, `"cumExecQty":"0.001"`).Replace(filledOrder))
	o, err := newTestClient(t, m).GetOrder(ctx, broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", ClientOrderID: "lk"})
	if err != nil || o.Status != broker.OrderStatusCanceled || o.FilledQtyCoin != 0.001 {
		t.Fatalf("%+v %v", o, err)
	}
}

func fastCancelConfirm(t *testing.T) {
	within, every := cancelConfirmWithin, cancelPollEvery
	cancelConfirmWithin, cancelPollEvery = 200*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { cancelConfirmWithin, cancelPollEvery = within, every })
}

func withStatus(status, cum string) string {
	return strings.NewReplacer(`"orderStatus":"Filled"`, `"orderStatus":"`+status+`"`, `"cumExecQty":"0.002"`, `"cumExecQty":"`+cum+`"`).Replace(filledOrder)
}

func TestCancelOrder_GoneIsNotFoundOnlyWhenReadBackFinal(t *testing.T) {
	fastCancelConfirm(t)
	q := broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", ClientOrderID: "lk"}
	for _, code := range []int{110001, 110008, 110010} {
		m := newMock(t)
		m.on("POST /v5/order/cancel", func(string, []byte) (int, string) { return 200, envelopeErr(code, "gone") })
		m.ok("GET /v5/order/realtime", withStatus("Cancelled", "0.001"))
		o, err := newTestClient(t, m).CancelOrder(ctx, q)
		if !errors.Is(err, broker.ErrOrderNotFound) || o.FilledQtyCoin != 0.001 {
			t.Errorf("cancel %d on a final order: want ErrOrderNotFound WITH its fill, got %+v %v", code, o, err)
		}
	}
	if errors.Is(mapCode(110008, callRead), broker.ErrOrderNotFound) || errors.Is(mapCode(110010, callPlace), broker.ErrOrderNotFound) {
		t.Error("110008/110010 outside a cancel must not read as 'never existed'")
	}

	// 110001 met before an asynchronous create was processed: nothing visible.
	m := newMock(t)
	m.on("POST /v5/order/cancel", func(string, []byte) (int, string) { return 200, envelopeErr(110001, "Order does not exist") })
	m.ok("GET /v5/order/realtime", `{"list":[],"nextPageCursor":""}`)
	m.ok("GET /v5/order/history", `{"list":[],"nextPageCursor":""}`)
	if _, err := newTestClient(t, m).CancelOrder(ctx, q); !errors.Is(err, ErrCancelNotConfirmed) || errors.Is(err, broker.ErrOrderNotFound) {
		t.Errorf("110001 with nothing visible must be AMBIGUOUS, got %v", err)
	}
}

func TestCancelOrder_WaitsForAFinalState(t *testing.T) {
	fastCancelConfirm(t)
	q := broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", ClientOrderID: "lk"}
	m := newMock(t)
	var sent string
	m.on("POST /v5/order/cancel", func(_ string, b []byte) (int, string) {
		sent = string(b)
		return 200, envelopeOK(`{"orderId":"77","orderLinkId":"lk"}`)
	})
	reads := 0
	m.on("GET /v5/order/realtime", func(string, []byte) (int, string) {
		reads++
		if reads < 3 {
			return 200, envelopeOK(withStatus("PartiallyFilled", "0.001"))
		}
		return 200, envelopeOK(withStatus("Cancelled", "0.0015"))
	})
	o, err := newTestClient(t, m).CancelOrder(ctx, q)
	if err != nil || o.Status != broker.OrderStatusCanceled || o.FilledQtyCoin != 0.0015 || reads != 3 {
		t.Fatalf("cancel must return the FINAL fill, not the first read-back: %+v %v after %d reads", o, err, reads)
	}
	if sent != `{"category":"linear","symbol":"BTCUSDT","orderLinkId":"lk"}` {
		t.Errorf("cancel body sends one id only: %s", sent)
	}

	m = newMock(t)
	m.ok("POST /v5/order/cancel", `{"orderId":"77","orderLinkId":"lk"}`)
	m.ok("GET /v5/order/realtime", withStatus("New", "0"))
	o, err = newTestClient(t, m).CancelOrder(ctx, q)
	if !errors.Is(err, ErrCancelNotConfirmed) || o.Status != broker.OrderStatusNew {
		t.Fatalf("a cancel never confirmed must say so and hand back the last read: %+v %v", o, err)
	}
}

func TestPlaceOrder_DuplicateIDIsProofOfExistence(t *testing.T) {
	m := newMock(t)
	m.on("POST /v5/order/create", func(string, []byte) (int, string) { return 200, envelopeErr(110072, "OrderLinkedID is duplicate") })
	m.ok("GET /v5/order/realtime", withStatus("Filled", "0.002"))
	o, err := newTestClient(t, m).PlaceOrder(ctx, broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT",
		Side: broker.SideSell, Type: broker.OrderTypeMarket, ClientOrderID: "lk", QtyCoin: 0.002})
	if !errors.Is(err, ErrDuplicateClientOrderID) || o.Status != broker.OrderStatusFilled || o.FilledQtyCoin != 0.002 {
		t.Fatalf("110072 must hand back the existing order: %+v %v", o, err)
	}
}

// Round 2: a 4xx on the READ-BACK after 110072 must not make the whole error
// look like the venue definitely refused the order — it just said it exists.
func TestPlaceOrder_DuplicateIDReadBackFailureIsNotADefiniteRefusal(t *testing.T) {
	m := newMock(t)
	m.on("POST /v5/order/create", func(string, []byte) (int, string) { return 200, envelopeErr(110072, "OrderLinkedID is duplicate") })
	m.on("GET /v5/order/realtime", func(string, []byte) (int, string) { return 403, "access too frequent" })
	_, err := newTestClient(t, m).PlaceOrder(ctx, broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT",
		Side: broker.SideSell, Type: broker.OrderTypeMarket, ClientOrderID: "lk", QtyCoin: 0.002})
	var httpErr *broker.HTTPError
	if !errors.Is(err, ErrDuplicateClientOrderID) || errors.As(err, &httpErr) {
		t.Fatalf("want 110072 without a reachable HTTPError, got %v", err)
	}
}

func TestCancelOrder_ResendsToAnOrderItCanSeeIsLive(t *testing.T) {
	fastCancelConfirm(t)
	q := broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", ClientOrderID: "lk"}
	m := newMock(t)
	cancels := 0
	m.on("POST /v5/order/cancel", func(string, []byte) (int, string) {
		cancels++
		if cancels == 1 {
			// The first cancel beats the asynchronous create.
			return 200, envelopeErr(110001, "Order does not exist")
		}
		return 200, envelopeOK(`{"orderId":"77","orderLinkId":"lk"}`)
	})
	m.on("GET /v5/order/realtime", func(string, []byte) (int, string) {
		if cancels < 2 {
			return 200, envelopeOK(withStatus("New", "0"))
		}
		return 200, envelopeOK(withStatus("Cancelled", "0"))
	})
	o, err := newTestClient(t, m).CancelOrder(ctx, q)
	if err != nil || o.Status != broker.OrderStatusCanceled || cancels != 2 {
		t.Fatalf("a live order seen after 'gone' must be cancelled again: %+v %v after %d cancels", o, err, cancels)
	}
	if mapCode(110001, callRead) != nil {
		t.Error("110001 on a READ must not map to ErrOrderNotFound")
	}
}

func TestCancelOrder_ResendIsBoundedAndCoolDownEndsTheWait(t *testing.T) {
	fastCancelConfirm(t)
	q := broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", ClientOrderID: "lk"}
	m := newMock(t)
	cancels := 0
	m.on("POST /v5/order/cancel", func(string, []byte) (int, string) {
		cancels++
		return 200, envelopeErr(110001, "Order does not exist")
	})
	m.ok("GET /v5/order/realtime", withStatus("New", "0"))
	if _, err := newTestClient(t, m).CancelOrder(ctx, q); !errors.Is(err, ErrCancelNotConfirmed) || cancels != 1+maxCancelResends {
		t.Fatalf("a live order must be re-cancelled at most %d times: %d cancels, %v", maxCancelResends, cancels, err)
	}

	m = newMock(t)
	m.ok("POST /v5/order/cancel", `{"orderId":"77","orderLinkId":"lk"}`)
	reads := 0
	m.on("GET /v5/order/realtime", func(string, []byte) (int, string) {
		reads++
		return 403, "access too frequent"
	})
	start := time.Now()
	_, err := newTestClient(t, m).CancelOrder(ctx, q)
	if !errors.Is(err, ErrCancelNotConfirmed) || reads != 1 || time.Since(start) >= cancelConfirmWithin {
		t.Fatalf("a 403 cool-down must end the wait with nothing more sent: %d reads, %v", reads, err)
	}
}

// Review round 3 (m2): a resend answered "gone" after an ACCEPTED first cancel
// is that cancel completing — success, not "already gone".
func TestCancelOrder_AcceptedThenGoneOnResendIsSuccess(t *testing.T) {
	fastCancelConfirm(t)
	q := broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", ClientOrderID: "lk"}
	m := newMock(t)
	cancels := 0
	m.on("POST /v5/order/cancel", func(string, []byte) (int, string) {
		cancels++
		if cancels == 1 {
			return 200, envelopeOK(`{"orderId":"77","orderLinkId":"lk"}`)
		}
		return 200, envelopeErr(110008, "The order has been completed or cancelled.")
	})
	m.on("GET /v5/order/realtime", func(string, []byte) (int, string) {
		if cancels < 2 {
			return 200, envelopeOK(withStatus("New", "0"))
		}
		return 200, envelopeOK(withStatus("Cancelled", "0"))
	})
	if o, err := newTestClient(t, m).CancelOrder(ctx, q); err != nil || o.Status != broker.OrderStatusCanceled || cancels != 2 {
		t.Fatalf("%+v %v after %d cancels", o, err, cancels)
	}
}

func TestBlankRequiredNumbersAreRefused(t *testing.T) {
	m := newMock(t)
	m.ok("GET /v5/order/realtime", strings.Replace(filledOrder, `"cumExecQty":"0.002",`, ``, 1))
	if _, err := newTestClient(t, m).GetOrder(ctx, broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", ClientOrderID: "lk"}); err == nil {
		t.Error("an order with no cumExecQty read as 'nothing filled'")
	}
	m = newMock(t)
	m.ok("GET /v5/position/list", positionList(`{"positionIdx":0,"symbol":"BTCUSDT","side":"Buy","avgPrice":"1","markPrice":"1","unrealisedPnl":"0","liqPrice":"","leverage":"2","updatedTime":"1"}`))
	if _, err := newTestClient(t, m).GetPosition(ctx, broker.MarketFuturesUSDM, "BTCUSDT"); err == nil {
		t.Error("a position with no size read as flat")
	}
}

func TestHTTP403StopsEveryLaterCallLocally(t *testing.T) {
	m := newMock(t)
	m.on("GET /v5/position/list", func(string, []byte) (int, string) { return 403, "access too frequent" })
	c := newTestClient(t, m)
	if _, err := c.GetPosition(ctx, broker.MarketFuturesUSDM, "BTCUSDT"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	before := len(m.calls)
	if _, err := c.GetPosition(ctx, broker.MarketFuturesUSDM, "BTCUSDT"); !errors.Is(err, broker.ErrIPCoolingDown) {
		t.Fatalf("a call inside the 403 cool-down must be refused locally, got %v", err)
	}
	if len(m.calls) != before {
		t.Error("a call inside the cool-down reached the venue")
	}
}

func TestFormatNumber_DropsFloatResidue(t *testing.T) {
	for in, want := range map[float64]string{0.1 + 0.2: "0.3", 0.001: "0.001", 60000: "60000", 1234.5: "1234.5"} {
		if got := formatNumber(in); got != want {
			t.Errorf("%v → %s, want %s", in, got, want)
		}
	}
}

func TestOpenOrders_PagesAndUsesSettleCoinWithoutSymbol(t *testing.T) {
	m := newMock(t)
	pages := 0
	m.on("GET /v5/order/realtime", func(q string, _ []byte) (int, string) {
		if !strings.Contains(q, "settleCoin=USDT") || !strings.Contains(q, "openOnly=0") {
			t.Errorf("query %s", q)
		}
		pages++
		if strings.Contains(q, "cursor=p2") {
			return 200, envelopeOK(strings.Replace(filledOrder, `"orderId":"77"`, `"orderId":"78"`, 1))
		}
		return 200, envelopeOK(strings.Replace(filledOrder, `"nextPageCursor":""`, `"nextPageCursor":"p2"`, 1))
	})
	orders, err := newTestClient(t, m).OpenOrders(ctx, broker.MarketFuturesUSDM, "")
	if err != nil || len(orders) != 2 || pages != 2 {
		t.Fatalf("%d orders over %d pages: %v", len(orders), pages, err)
	}
}

func positionList(rows string) string { return `{"list":[` + rows + `],"category":"linear"}` }

func TestGetPosition_SignedFromTheVenue(t *testing.T) {
	row := func(idx int, side, size string) string {
		return fmt.Sprintf(`{"positionIdx":%d,"symbol":"BTCUSDT","side":%q,"size":%q,"avgPrice":"60000","markPrice":"60010","unrealisedPnl":"1.5","liqPrice":"","leverage":"2","updatedTime":"1700000000000"}`, idx, side, size)
	}
	cases := []struct {
		name    string
		list    string
		wantQty float64
		wantErr bool
	}{
		{"long", row(0, "Buy", "0.01"), 0.01, false},
		{"short", row(0, "Sell", "0.01"), -0.01, false},
		{"flat", row(0, "", "0"), 0, false},
		{"side and size disagree", row(0, "", "0.01"), 0, true},
		{"hedge mode", row(1, "Buy", "0.01") + "," + row(2, "Sell", "0.01"), 0, true},
		{"hedge mode, one side listed", row(1, "Buy", "0.01"), 0, true},
		{"empty list is not flat", "", 0, true},
	}
	for _, tc := range cases {
		m := newMock(t)
		m.ok("GET /v5/position/list", positionList(tc.list))
		p, err := newTestClient(t, m).GetPosition(ctx, broker.MarketFuturesUSDM, "BTCUSDT")
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: accepted %+v", tc.name, p)
			}
			continue
		}
		if err != nil || p.QtyCoin != tc.wantQty || p.LiquidationPriceQuote != 0 || p.LeverageX != 2 {
			t.Errorf("%s: %+v %v", tc.name, p, err)
		}
	}
}

const walletAnswer = `{"list":[{"accountType":"UNIFIED","accountIMRate":"0.0123","accountMMRate":"0.0045","totalEquity":"1000.5","totalWalletBalance":"1000","totalMarginBalance":"1000.5","totalAvailableBalance":"900","totalInitialMargin":"100","totalMaintenanceMargin":"45","coin":[{"coin":"USDT","equity":"1000.5","walletBalance":"1000","locked":"0","totalOrderIM":"10","totalPositionIM":"90","totalPositionMM":"45","unrealisedPnl":"0.5","usdValue":"1000.4","availableToWithdraw":"","free":""}]}]}`

func TestWalletAndDerivedBalance(t *testing.T) {
	m := newMock(t)
	m.on("GET /v5/account/wallet-balance", func(q string, _ []byte) (int, string) {
		if q != "accountType=UNIFIED&coin=USDT" && q != "accountType=UNIFIED" {
			t.Errorf("query %s", q)
		}
		return 200, envelopeOK(walletAnswer)
	})
	c := newTestClient(t, m)
	w, err := c.FetchWallet(ctx, "USDT")
	if err != nil || !w.RatesPublished || w.AccountMMRateFrac != 0.0045 || w.TotalAvailableBalanceUSD != 900 || len(w.Coins) != 1 {
		t.Fatalf("%+v %v", w, err)
	}
	b, err := c.GetBalance(ctx, broker.MarketFuturesUSDM)
	if err != nil || len(b) != 1 || b[0].LockedQtyCoin != 100 || b[0].FreeQtyCoin != 900 {
		t.Fatalf("Locked = locked+orderIM+positionIM, Free = walletBalance−Locked: %+v %v", b, err)
	}

	m = newMock(t)
	m.ok("GET /v5/account/wallet-balance", strings.Replace(walletAnswer, `"accountMMRate":"0.0045"`, `"accountMMRate":""`, 1))
	if w, err := newTestClient(t, m).FetchWallet(ctx); err != nil || w.RatesPublished {
		t.Errorf("a blank MM rate must read as NOT PUBLISHED: %+v %v", w, err)
	}
}

func TestFetchInstrument(t *testing.T) {
	answer := func(settle, ctype string) string {
		return `{"category":"linear","list":[{"symbol":"BTCUSDT","contractType":"` + ctype + `","status":"Trading","baseCoin":"BTC","quoteCoin":"USDT","settleCoin":"` + settle + `","fundingInterval":480,"priceFilter":{"minPrice":"0.10","maxPrice":"1999999.80","tickSize":"0.10"},"lotSizeFilter":{"minNotionalValue":"5","maxOrderQty":"1190","maxMktOrderQty":"119","minOrderQty":"0.001","qtyStep":"0.001"}}],"nextPageCursor":""}`
	}
	m := newMock(t)
	m.ok("GET /v5/market/instruments-info", answer("USDT", "LinearPerpetual"))
	r, err := newTestClient(t, m).FetchInstrument(ctx, "BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if r.StepSizeCoin != 0.001 || r.TickSizeQuote != 0.1 || r.MinNotionalQuote != 5 || r.MaxQtyCoin != 119 ||
		r.FundingIntervalSec != 28800 || r.Status != "trading" || r.Source != "bybit_linear_testnet" {
		t.Errorf("%+v", r)
	}
	for _, bad := range [][2]string{{"USDC", "LinearPerpetual"}, {"USDT", "LinearFutures"}} {
		m := newMock(t)
		m.ok("GET /v5/market/instruments-info", answer(bad[0], bad[1]))
		if _, err := newTestClient(t, m).FetchInstrument(ctx, "BTCUSDT"); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestFetchDepthBookAndFunding(t *testing.T) {
	m := newMock(t)
	m.on("GET /v5/market/orderbook", func(q string, _ []byte) (int, string) {
		if !strings.Contains(q, "limit=500") {
			t.Errorf("query %s", q)
		}
		return 200, envelopeOK(`{"s":"BTCUSDT","b":[["60000","1.5"],["59999.9","2"]],"a":[["60000.1","0.5"],["60000.2","3"]],"ts":1700000000000,"u":1,"seq":2,"cts":3}`)
	})
	m.ok("GET /v5/market/funding/history", `{"category":"linear","list":[{"symbol":"BTCUSDT","fundingRate":"0.0001","fundingRateTimestamp":"1700028800000"},{"symbol":"BTCUSDT","fundingRate":"-0.00005","fundingRateTimestamp":"1700000000000"}]}`)
	c := newTestClient(t, m)
	book, err := c.FetchDepthBook(ctx, "BTCUSDT")
	if err != nil || len(book.Bids) != 2 || book.Bids[0].PriceQuote != 60000 || book.Asks[0].PriceQuote != 60000.1 {
		t.Fatalf("%+v %v", book, err)
	}
	rates, err := c.FundingRateHistory(ctx, "BTCUSDT", 0, 200)
	if err != nil || len(rates) != 2 || rates[0].SettledAtMs != 1700000000000 || rates[0].RatePerIntervalFrac != -0.00005 {
		t.Fatalf("oldest first: %+v %v", rates, err)
	}
	if _, err := c.FundingRateHistory(ctx, "BTCUSDT", 0, 201); err == nil {
		t.Error("limit above 200 accepted")
	}
}

func TestAPIKeyInfo(t *testing.T) {
	m := newMock(t)
	m.ok("GET /v5/user/query-api", `{"id":"1","note":"n","apiKey":"`+testKey+`","readOnly":0,"secret":"","permissions":{"ContractTrade":["Order","Position"],"Wallet":["AccountTransfer","Withdraw"]},"ips":["*"],"uta":1,"expiredAt":"1970-01-01T00:00:00Z"}`)
	info, err := newTestClient(t, m).FetchAPIKeyInfo(ctx)
	if err != nil || !info.CanTradeContracts() || !info.CanWithdraw() || !info.UTA {
		t.Fatalf("%+v %v", info, err)
	}
	if (APIKeyInfo{ReadOnly: true, Permissions: map[string][]string{"ContractTrade": {"Order", "Position"}}}).CanTradeContracts() {
		t.Error("a read-only key cannot trade")
	}
}

func TestErrors_MappedAndNeverCarryTheSecret(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"10002", 200, envelopeErr(10002, "time window"), ErrTimestampOutOfWindow},
		{"10003", 200, envelopeErr(10003, "API key is invalid"), ErrKeyInvalid},
		{"10006", 200, envelopeErr(10006, "Too many visits!"), ErrRateLimited},
		{"110072", 200, envelopeErr(110072, "OrderLinkedID is duplicate"), ErrDuplicateClientOrderID},
		{"110094", 200, envelopeErr(110094, "below"), broker.ErrBelowMinNotional},
		{"echoed secret", 200, envelopeErr(10001, "bad param "+testSecret+" key "+testKey), nil},
		{"403", 403, "access too frequent " + testSecret, ErrForbidden},
		{"401", 401, "", ErrUnauthorized},
	}
	for _, tc := range cases {
		m := newMock(t)
		m.on("GET /v5/position/list", func(string, []byte) (int, string) { return tc.status, tc.body })
		_, err := newTestClient(t, m).GetPosition(ctx, broker.MarketFuturesUSDM, "BTCUSDT")
		if err == nil {
			t.Errorf("%s: no error", tc.name)
			continue
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: want %v, got %v", tc.name, tc.want, err)
		}
		for _, rendered := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
			if strings.Contains(rendered, testSecret) || strings.Contains(rendered, testKey) {
				t.Errorf("%s: the credential reached the error text: %s", tc.name, rendered)
			}
		}
	}
}

func TestModeAndConfig(t *testing.T) {
	if _, err := ParseMode("prod"); err == nil {
		t.Error("an unknown mode was accepted")
	}
	cfg, _ := DefaultConfig(ModeDemo, broker.Credentials{APIKey: broker.NewSecret("k"), APISecret: broker.NewSecret("s")})
	if cfg.BaseURL != broker.BybitDemoBaseURL || cfg.Scheme != broker.SchemeBybitV5Header {
		t.Errorf("%+v", cfg)
	}
	binanceCfg := cfg
	binanceCfg.Scheme = broker.SchemeBinanceQuery
	if _, err := New(ModeDemo, binanceCfg); err == nil {
		t.Error("a config without the Bybit scheme was accepted")
	}
}
