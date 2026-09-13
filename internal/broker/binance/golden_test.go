package binance

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"futures-arbitrage-scanner/internal/broker"
)

// Golden tests against REAL testnet answers, recorded by
//
//	CAPTURE_TESTDATA=1 go run ./cmd/brokercheck -place-cancel -symbol BTCUSDT
//
// on 2026-09-13. The recordings are sanitized — `orderId`, `orderListId`,
// `clientOrderId` and `origClientOrderId` are replaced with fixed placeholders
// before anything is written — and no account endpoint is recorded at all,
// because no amount of scrubbing makes a balance safe to commit.
//
// They exist because encoding/json fails SILENTLY on a wrong field name: it
// leaves the field at its zero value, so a mis-spelled `executedQty` reports
// every order as unfilled and nothing anywhere says so. Two of the field names
// this package depends on are ones nobody would guess — spot's filled notional
// is `cummulativeQuoteQty`, with the venue's own doubled m, and futures calls
// the same thing `cumQuote` while also publishing `avgPrice` directly.
//
// These tests open no socket.

func loadRecording(t *testing.T, name string) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("recording %s is missing; re-record with CAPTURE_TESTDATA=1 go run ./cmd/brokercheck -place-cancel: %v", name, err)
	}
	return json.RawMessage(raw)
}

func testClient(t *testing.T, market broker.Market) *Client {
	t.Helper()
	cfg, err := DefaultConfig(market, broker.Credentials{
		APIKey: broker.NewSecret("k"), APISecret: broker.NewSecret("s"),
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(market, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseOrder_AgainstRealTestnetAnswers(t *testing.T) {
	cases := []struct {
		name   string
		market broker.Market
		file   string

		wantStatus broker.OrderStatus
		wantQty    float64
		wantPrice  float64
		wantFilled float64
		wantSide   broker.Side
		wantType   broker.OrderType
	}{
		{
			name: "futures place", market: broker.MarketFuturesUSDM, file: "futures_order_post_200.json",
			wantStatus: broker.OrderStatusNew, wantQty: 0.0014, wantPrice: 38631.30,
			wantSide: broker.SideBuy, wantType: broker.OrderTypeLimitGTC,
		},
		{
			name: "futures query after cancel", market: broker.MarketFuturesUSDM, file: "futures_order_get_200.json",
			wantStatus: broker.OrderStatusCanceled, wantQty: 0.0014, wantPrice: 38631.30,
			wantSide: broker.SideBuy, wantType: broker.OrderTypeLimitGTC,
		},
		{
			name: "futures cancel", market: broker.MarketFuturesUSDM, file: "futures_order_delete_200.json",
			wantStatus: broker.OrderStatusCanceled, wantQty: 0.0014, wantPrice: 38631.30,
			wantSide: broker.SideBuy, wantType: broker.OrderTypeLimitGTC,
		},
		{
			name: "spot place", market: broker.MarketSpot, file: "spot_order_post_200.json",
			wantStatus: broker.OrderStatusNew, wantQty: 0.00014, wantPrice: 39412.85,
			wantSide: broker.SideBuy, wantType: broker.OrderTypeLimitGTC,
		},
		{
			name: "spot query after cancel", market: broker.MarketSpot, file: "spot_order_get_200.json",
			wantStatus: broker.OrderStatusCanceled, wantQty: 0.00014, wantPrice: 39412.85,
			wantSide: broker.SideBuy, wantType: broker.OrderTypeLimitGTC,
		},
		{
			name: "spot cancel", market: broker.MarketSpot, file: "spot_order_delete_200.json",
			wantStatus: broker.OrderStatusCanceled, wantQty: 0.00014, wantPrice: 39412.85,
			wantSide: broker.SideBuy, wantType: broker.OrderTypeLimitGTC,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := testClient(t, tc.market).parseOrder(loadRecording(t, tc.file))
			if err != nil {
				t.Fatalf("parseOrder: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.QtyCoin != tc.wantQty {
				t.Errorf("QtyCoin = %v, want %v — a wrong field name decodes to 0 and says nothing", got.QtyCoin, tc.wantQty)
			}
			if got.PriceQuote != tc.wantPrice {
				t.Errorf("PriceQuote = %v, want %v", got.PriceQuote, tc.wantPrice)
			}
			if got.FilledQtyCoin != tc.wantFilled {
				t.Errorf("FilledQtyCoin = %v, want %v", got.FilledQtyCoin, tc.wantFilled)
			}
			if got.Side != tc.wantSide {
				t.Errorf("Side = %q, want %q", got.Side, tc.wantSide)
			}
			if got.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", got.Type, tc.wantType)
			}
			if got.Symbol != "BTCUSDT" {
				t.Errorf("Symbol = %q", got.Symbol)
			}
			if got.Market != tc.market {
				t.Errorf("Market = %q, want %q", got.Market, tc.market)
			}
			// The id is a STRING and is not the JSON float it would become if
			// anything on the path had decoded it as a number.
			if got.VenueOrderID != "4200000000000000001" {
				t.Errorf("VenueOrderID = %q — a 19-digit id survived only if nothing decoded it as a JSON number", got.VenueOrderID)
			}
			if got.ClientOrderID != "recorded-order-id" {
				t.Errorf("ClientOrderID = %q, want the caller's id", got.ClientOrderID)
			}
			if got.UpdatedAtMs == 0 {
				t.Error("UpdatedAtMs = 0; no timestamp was read from the answer")
			}
			// Nothing filled, so there must be no average price to mistake for
			// a real one.
			if got.AvgFillPriceQuote != 0 {
				t.Errorf("AvgFillPriceQuote = %v on an unfilled order", got.AvgFillPriceQuote)
			}
		})
	}
}

// The venue's real -2011, as recorded from a second cancel of an order already
// cancelled. This is the answer the Broker contract turns into
// ErrOrderNotFound, and the one that makes a resend safe after a timeout.
func TestClassify_AgainstTheRealCancelRejectedAnswer(t *testing.T) {
	for _, file := range []string{"futures_order_delete_400.json", "spot_order_delete_400.json"} {
		t.Run(file, func(t *testing.T) {
			err := classify(&broker.HTTPError{
				StatusCode: 400,
				URL:        "https://demo-fapi.binance.com/fapi/v1/order",
				Body:       string(loadRecording(t, file)),
			})
			if !errors.Is(err, broker.ErrOrderNotFound) {
				t.Fatalf("the venue's own -2011 did not map to ErrOrderNotFound: %v", err)
			}
			var ve *VenueError
			if !errors.As(err, &ve) || ve.Code != -2011 {
				t.Errorf("the venue's code did not survive: %v", err)
			}
		})
	}
}

// An empty open-orders list is an empty list, not an error — that is how the
// acceptance proves a cancelled order is gone.
func TestParseOrders_AnEmptyOpenOrdersListIsNotAnError(t *testing.T) {
	for _, tc := range []struct {
		market broker.Market
		file   string
	}{
		{broker.MarketFuturesUSDM, "futures_open_orders_get_200.json"},
		{broker.MarketSpot, "spot_open_orders_get_200.json"},
	} {
		orders, err := testClient(t, tc.market).parseOrders(loadRecording(t, tc.file))
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if len(orders) != 0 {
			t.Errorf("%s: got %d orders, want none", tc.file, len(orders))
		}
	}
}

// The recordings themselves must stay clean. This runs on every `go test`, so
// a future re-recording that forgets to sanitize fails here rather than in
// review — or not at all.
func TestRecordings_CarryNoRealIdentifiers(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no recordings at all; the golden tests above would be vacuous")
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join("testdata", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Errorf("%s is not valid JSON: %v", e.Name(), err)
		}
		assertSanitized(t, e.Name(), doc)
	}
}

func assertSanitized(t *testing.T, file string, node any) {
	t.Helper()
	switch v := node.(type) {
	case map[string]any:
		for key, val := range v {
			switch key {
			case "clientOrderId", "origClientOrderId":
				if val != "recorded-order-id" {
					t.Errorf("%s: %s = %v, want the placeholder — a real id reached git", file, key, val)
				}
			case "orderId", "orderListId":
				if num, ok := val.(float64); !ok || num != 4200000000000000001 {
					t.Errorf("%s: %s = %v, want the placeholder", file, key, val)
				}
			}
			assertSanitized(t, file, val)
		}
	case []any:
		for _, item := range v {
			assertSanitized(t, file, item)
		}
	}
}
