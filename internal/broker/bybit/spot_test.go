package bybit

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"futures-arbitrage-scanner/internal/broker"
)

// PLAN 4.5j: the spot market and the capabilities Strategy 1 needs on Bybit.

func TestSpotPlaceOrder_MarketBodyIsInCoinAndNeverBorrows(t *testing.T) {
	cases := []struct {
		name string
		req  broker.PlaceOrderRequest
		want string
	}{
		{"market buy", broker.PlaceOrderRequest{Market: broker.MarketSpot, Symbol: "BTCUSDT", Side: broker.SideBuy,
			Type: broker.OrderTypeMarket, ClientOrderID: "o-1", QtyCoin: 0.001},
			`{"category":"spot","symbol":"BTCUSDT","side":"Buy","orderType":"Market","qty":"0.001","timeInForce":"IOC","orderLinkId":"o-1","isLeverage":0,"marketUnit":"baseCoin"}`},
		// The SELL default is already baseCoin; it is sent anyway, so the unit
		// never depends on a default the venue may change.
		{"market sell", broker.PlaceOrderRequest{Market: broker.MarketSpot, Symbol: "BTCUSDT", Side: broker.SideSell,
			Type: broker.OrderTypeMarket, ClientOrderID: "o-1", QtyCoin: 0.001},
			`{"category":"spot","symbol":"BTCUSDT","side":"Sell","orderType":"Market","qty":"0.001","timeInForce":"IOC","orderLinkId":"o-1","isLeverage":0,"marketUnit":"baseCoin"}`},
		{"limit buy", broker.PlaceOrderRequest{Market: broker.MarketSpot, Symbol: "BTCUSDT", Side: broker.SideBuy,
			Type: broker.OrderTypeLimitGTC, ClientOrderID: "o-1", QtyCoin: 0.001, PriceQuote: 30000},
			`{"category":"spot","symbol":"BTCUSDT","side":"Buy","orderType":"Limit","qty":"0.001","price":"30000","timeInForce":"GTC","orderLinkId":"o-1","isLeverage":0}`},
	}
	for _, tc := range cases {
		m := newMock(t)
		var sent string
		m.on("POST /v5/order/create", func(_ string, body []byte) (int, string) {
			sent = string(body)
			return 200, envelopeOK(`{"orderId":"77","orderLinkId":"o-1"}`)
		})
		o, err := newTestClientFor(t, m, broker.MarketSpot).PlaceOrder(ctx, tc.req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if sent != tc.want {
			t.Errorf("%s body\n got %s\nwant %s", tc.name, sent, tc.want)
		}
		if strings.Contains(sent, "reduceOnly") || strings.Contains(sent, "positionIdx") {
			t.Errorf("%s: a spot order carried a linear-only field: %s", tc.name, sent)
		}
		if o.Market != broker.MarketSpot || o.Status != broker.OrderStatusNew || o.FilledQtyCoin != 0 {
			t.Errorf("%s: %+v", tc.name, o)
		}
	}
}

func TestClient_RefusesTheOtherMarketBeforeSending(t *testing.T) {
	m := newMock(t) // any call reaching it is an "unexpected call" failure
	spot := newTestClientFor(t, m, broker.MarketSpot)
	perpReq := broker.PlaceOrderRequest{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", Side: broker.SideSell,
		Type: broker.OrderTypeMarket, ClientOrderID: "o-1", QtyCoin: 0.001}
	if _, err := spot.PlaceOrder(ctx, perpReq); !errors.Is(err, broker.ErrInvalidOrder) {
		t.Errorf("a linear order on the spot client: %v", err)
	}
	linear := newTestClient(t, m)
	spotReq := perpReq
	spotReq.Market, spotReq.Side = broker.MarketSpot, broker.SideBuy
	if _, err := linear.PlaceOrder(ctx, spotReq); !errors.Is(err, broker.ErrInvalidOrder) {
		t.Errorf("a spot order on the linear client: %v", err)
	}
	if _, err := linear.GetOrder(ctx, broker.OrderQuery{Market: broker.MarketSpot, Symbol: "BTCUSDT", ClientOrderID: "o-1"}); !errors.Is(err, broker.ErrInvalidOrder) {
		t.Errorf("a spot query on the linear client: %v", err)
	}
	if len(m.calls) != 0 {
		t.Errorf("refusals reached the venue: %v", m.calls)
	}
	if _, err := New(ModeTestnet, "option", broker.Config{}); !errors.Is(err, broker.ErrNotSupported) {
		t.Errorf("an unknown market was accepted: %v", err)
	}
}

func TestWithMarket_SharesOneTransport(t *testing.T) {
	m := newMock(t)
	linear := newTestClient(t, m)
	spot, err := linear.WithMarket(broker.MarketSpot)
	if err != nil {
		t.Fatal(err)
	}
	if spot.HTTP() != linear.HTTP() || spot.Market() != broker.MarketSpot || linear.Market() != broker.MarketFuturesUSDM {
		t.Error("the sibling must share the signed client — one budget, one clock, one cool-down")
	}
	if spot.SourceID() != "bybit_spot_testnet" || linear.SourceID() != "bybit_linear_testnet" {
		t.Errorf("source ids %q %q", spot.SourceID(), linear.SourceID())
	}
}

func TestSpotGetOrderAndOpenOrders(t *testing.T) {
	m := newMock(t)
	m.on("GET /v5/order/realtime", func(q string, _ []byte) (int, string) {
		v, _ := url.ParseQuery(q)
		if v.Get("category") != "spot" || v.Has("settleCoin") {
			t.Errorf("spot query %s", q)
		}
		return 200, envelopeOK(`{"list":[{"orderId":"77","orderLinkId":"o-1","symbol":"BTCUSDT","side":"Buy","orderType":"Market","timeInForce":"IOC","orderStatus":"PartiallyFilledCanceled","price":"0","qty":"0.002","avgPrice":"60000","cumExecQty":"0.001","createdTime":"1","updatedTime":"2"}],"nextPageCursor":""}`)
	})
	spot := newTestClientFor(t, m, broker.MarketSpot)
	o, err := spot.GetOrder(ctx, broker.OrderQuery{Market: broker.MarketSpot, Symbol: "BTCUSDT", ClientOrderID: "o-1"})
	if err != nil || o.Market != broker.MarketSpot || o.Status != broker.OrderStatusCanceled || o.FilledQtyCoin != 0.001 {
		t.Fatalf("PartiallyFilledCanceled is CANCELED with its fill, on the spot market: %+v %v", o, err)
	}
	list, err := spot.OpenOrders(ctx, broker.MarketSpot, "")
	if err != nil || len(list) != 1 {
		t.Fatalf("%v %v", list, err)
	}
}

const spotInstrument = `{"category":"spot","list":[{"symbol":"ETHUSDT","baseCoin":"ETH","quoteCoin":"USDT","innovation":"0","status":"Trading","marginTrading":"utaOnly","lotSizeFilter":{"basePrecision":"0.00001","quotePrecision":"0.0000001","minOrderQty":"0.00001","maxOrderQty":"8118","minOrderAmt":"5","maxOrderAmt":"7000000","maxLimitOrderQty":"8118","maxMarketOrderQty":"2706"},"priceFilter":{"tickSize":"0.01"}}]}`

func TestSpotFetchInstrument(t *testing.T) {
	m := newMock(t)
	m.on("GET /v5/market/instruments-info", func(q string, _ []byte) (int, string) {
		if q != "category=spot&symbol=ETHUSDT" {
			t.Errorf("query %s", q)
		}
		return 200, envelopeOK(spotInstrument)
	})
	r, err := newTestClientFor(t, m, broker.MarketSpot).FetchInstrument(ctx, "ETHUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if r.StepSizeCoin != 0.00001 || r.TickSizeQuote != 0.01 || r.MinNotionalQuote != 5 || r.MinOrderAmtQuote != 5 ||
		r.MinQtyCoin != 0.00001 || r.MaxQtyCoin != 2706 || r.MarketType != "spot" || r.BaseAsset != "ETH" ||
		r.QuoteAsset != "USDT" || r.Source != "bybit_spot_testnet" || r.FundingIntervalSec != 0 || r.Status != "trading" {
		t.Errorf("basePrecision is the step, minOrderAmt the minimum notional, the smaller ceiling wins: %+v", r)
	}
	// Rounded through the repo's own function, as execution does.
	got, err := broker.RoundOrder(broker.RoundRequest{Rules: r.Instrument, Side: broker.SideBuy, Type: broker.OrderTypeMarket,
		QtyCoin: 0.0123456, PriceQuote: 2000})
	if err != nil || got.QtyCoin != 0.01234 {
		t.Errorf("floored onto basePrecision: %+v %v", got, err)
	}
	if _, err := broker.RoundOrder(broker.RoundRequest{Rules: r.Instrument, Side: broker.SideBuy, Type: broker.OrderTypeMarket,
		QtyCoin: 0.002, PriceQuote: 2000}); !errors.Is(err, broker.ErrBelowMinNotional) {
		t.Errorf("4 USDT is under minOrderAmt 5: %v", err)
	}

	for name, bad := range map[string]string{
		"no step":  strings.Replace(spotInstrument, `"basePrecision":"0.00001"`, `"basePrecision":""`, 1),
		"no base":  strings.Replace(spotInstrument, `"baseCoin":"ETH"`, `"baseCoin":""`, 1),
		"bad tick": strings.Replace(spotInstrument, `"tickSize":"0.01"`, `"tickSize":"x"`, 1),
	} {
		m := newMock(t)
		m.ok("GET /v5/market/instruments-info", bad)
		if _, err := newTestClientFor(t, m, broker.MarketSpot).FetchInstrument(ctx, "ETHUSDT"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	m = newMock(t)
	m.ok("GET /v5/market/instruments-info", strings.Replace(spotInstrument, `,"maxMarketOrderQty":"2706"`, ``, 1))
	if r, err := newTestClientFor(t, m, broker.MarketSpot).FetchInstrument(ctx, "ETHUSDT"); err != nil || r.MaxQtyCoin != 8118 {
		t.Errorf("absent maxMarketOrderQty is not assumed: %+v %v", r, err)
	}
}

func TestSpotDepthBook_AsksTheSpotCeiling(t *testing.T) {
	m := newMock(t)
	m.on("GET /v5/market/orderbook", func(q string, _ []byte) (int, string) {
		if q != "category=spot&symbol=BTCUSDT&limit=200" {
			t.Errorf("query %s", q)
		}
		return 200, envelopeOK(`{"s":"BTCUSDT","b":[["59999.9","2"],["60000","1.5"]],"a":[["60000.2","3"],["60000.1","0.5"]],"ts":1700000000000,"u":1}`)
	})
	book, err := newTestClientFor(t, m, broker.MarketSpot).FetchDepthBook(ctx, "BTCUSDT")
	if err != nil || book.Source != "bybit_spot_testnet" || book.Bids[0].PriceQuote != 60000 || book.Asks[0].PriceQuote != 60000.1 {
		t.Fatalf("%+v %v", book, err)
	}
}

func TestSpotHasNoPositionAndNoFunding(t *testing.T) {
	m := newMock(t) // nothing may be sent
	spot := newTestClientFor(t, m, broker.MarketSpot)
	if p, err := spot.GetPosition(ctx, broker.MarketSpot, "BTCUSDT"); !errors.Is(err, broker.ErrNotSupported) || p != (broker.Position{}) {
		t.Errorf("GetPosition on spot: %+v %v", p, err)
	}
	if _, err := spot.FundingRateHistory(ctx, "BTCUSDT", 0, 10); !errors.Is(err, broker.ErrNotSupported) {
		t.Errorf("funding history on spot: %v", err)
	}
	if _, err := spot.FundingIncome(ctx, broker.MarketSpot, "BTCUSDT", 1, 2); !errors.Is(err, broker.ErrNotSupported) {
		t.Errorf("funding income on spot: %v", err)
	}
	if _, err := spot.MarkPrice(ctx, broker.MarketSpot, "BTCUSDT"); !errors.Is(err, broker.ErrNotSupported) {
		t.Errorf("mark price on spot: %v", err)
	}
	if _, err := spot.FetchMaintenanceBracket(ctx, "BTCUSDT", 100); !errors.Is(err, broker.ErrNotSupported) {
		t.Errorf("maintenance bracket on spot: %v", err)
	}
	if len(m.calls) != 0 {
		t.Errorf("sent: %v", m.calls)
	}
}

const twoCoinWallet = `{"list":[{"accountType":"UNIFIED","accountIMRate":"0","accountMMRate":"0","totalEquity":"1100","totalWalletBalance":"1100","totalMarginBalance":"1100","totalAvailableBalance":"1000","totalInitialMargin":"0","totalMaintenanceMargin":"0","coin":[{"coin":"USDT","equity":"1000","walletBalance":"1000","locked":"20","totalOrderIM":"0","totalPositionIM":"30","totalPositionMM":"1","unrealisedPnl":"0","usdValue":"1000"},{"coin":"BTC","equity":"0.001998","walletBalance":"0.001998","locked":"0","totalOrderIM":"0","totalPositionIM":"0","totalPositionMM":"0","unrealisedPnl":"0","usdValue":"100"}]}]}`

func TestGetBalance_OneWalletTwoViews(t *testing.T) {
	m := newMock(t)
	var queries []string
	m.on("GET /v5/account/wallet-balance", func(q string, _ []byte) (int, string) {
		queries = append(queries, q)
		return 200, envelopeOK(twoCoinWallet)
	})
	linear := newTestClient(t, m)
	spot, _ := linear.WithMarket(broker.MarketSpot)

	sb, err := spot.GetBalance(ctx, broker.MarketSpot)
	if err != nil || len(sb) != 2 {
		t.Fatalf("spot lists every coin: %+v %v", sb, err)
	}
	for _, b := range sb {
		if b.Market != broker.MarketSpot {
			t.Errorf("%+v", b)
		}
		if b.Asset == "BTC" && (b.FreeQtyCoin != 0.001998 || b.LockedQtyCoin != 0) {
			t.Errorf("the base coin a spot leg bought: %+v", b)
		}
		if b.Asset == "USDT" && (b.LockedQtyCoin != 50 || b.FreeQtyCoin != 950) {
			t.Errorf("USDT locked = spot orders + position margin: %+v", b)
		}
	}
	fb, err := linear.GetBalance(ctx, broker.MarketFuturesUSDM)
	if err != nil || len(fb) != 1 || fb[0].Asset != "USDT" || fb[0].Market != broker.MarketFuturesUSDM {
		t.Fatalf("futures lists the settle coin only: %+v %v", fb, err)
	}
	if queries[0] != "accountType=UNIFIED" || queries[1] != "accountType=UNIFIED&coin=USDT" {
		t.Errorf("queries %v", queries)
	}
}

func TestOrderTrades_FeeAssetAndInvisibleFills(t *testing.T) {
	fill := func(link, fee, cur string) string {
		return `{"symbol":"BTCUSDT","orderId":"77","orderLinkId":"` + link + `","side":"Buy","execId":"e1","execPrice":"60000","execQty":"0.002","execFee":"` + fee + `","feeCurrency":"` + cur + `","execTime":"1700","isMaker":false}`
	}
	filledOrder := `{"list":[{"orderId":"77","orderLinkId":"o-1","symbol":"BTCUSDT","side":"Buy","orderType":"Market","timeInForce":"IOC","orderStatus":"Filled","price":"0","qty":"0.002","avgPrice":"60000","cumExecQty":"0.002"}],"nextPageCursor":""}`
	m := newMock(t)
	m.on("GET /v5/execution/list", func(q string, _ []byte) (int, string) {
		v, _ := url.ParseQuery(q)
		if v.Get("category") != "spot" || v.Get("orderLinkId") != "o-1" {
			t.Errorf("query %s", q)
		}
		return 200, envelopeOK(`{"list":[` + fill("o-1", "0.000002", "BTC") + `],"nextPageCursor":""}`)
	})
	m.ok("GET /v5/order/realtime", filledOrder)
	spot := newTestClientFor(t, m, broker.MarketSpot)
	q := broker.OrderQuery{Market: broker.MarketSpot, Symbol: "BTCUSDT", ClientOrderID: "o-1"}
	tr, err := spot.OrderTrades(ctx, q)
	if err != nil || len(tr) != 1 || tr[0].CommissionAsset != "BTC" || tr[0].CommissionQtyInAsset != 0.000002 || tr[0].QtyCoin != 0.002 {
		t.Fatalf("a spot buy's fee is in the BASE coin, unconverted: %+v %v", tr, err)
	}

	// Linear: the page's own example answers feeCurrency "" — the settle coin.
	m = newMock(t)
	m.ok("GET /v5/execution/list", `{"list":[`+fill("o-1", "0.072", "")+`],"nextPageCursor":""}`)
	m.ok("GET /v5/order/realtime", filledOrder)
	if tr, err := newTestClient(t, m).OrderTrades(ctx, broker.OrderQuery{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", ClientOrderID: "o-1"}); err != nil || tr[0].CommissionAsset != "USDT" {
		t.Errorf("%+v %v", tr, err)
	}
	// A blank SPOT currency stays blank — "not stated", not USDT.
	m = newMock(t)
	m.ok("GET /v5/execution/list", `{"list":[`+fill("o-1", "0.1", "")+`],"nextPageCursor":""}`)
	m.ok("GET /v5/order/realtime", filledOrder)
	if tr, err := newTestClientFor(t, m, broker.MarketSpot).OrderTrades(ctx, q); err != nil || tr[0].CommissionAsset != "" {
		t.Errorf("%+v %v", tr, err)
	}
	// A fill of another order is refused, not summed.
	m = newMock(t)
	m.ok("GET /v5/execution/list", `{"list":[`+fill("o-2", "0.1", "USDT")+`],"nextPageCursor":""}`)
	if _, err := newTestClientFor(t, m, broker.MarketSpot).OrderTrades(ctx, q); err == nil {
		t.Error("a foreign fill was accepted")
	}
	// Fewer fills listed than the order filled — a window that cut some: refused.
	m = newMock(t)
	m.ok("GET /v5/execution/list", `{"list":[`+fill("o-1", "0.1", "USDT")+`],"nextPageCursor":""}`)
	m.ok("GET /v5/order/realtime", strings.Replace(filledOrder, `"cumExecQty":"0.002"`, `"cumExecQty":"0.004"`, 1))
	if _, err := newTestClientFor(t, m, broker.MarketSpot).OrderTrades(ctx, q); err == nil {
		t.Error("a partial fill list was summed as the whole order")
	}
	// No fills listed for an order that filled: an error, never "no fee".
	m = newMock(t)
	m.ok("GET /v5/execution/list", `{"list":[],"nextPageCursor":""}`)
	m.ok("GET /v5/order/realtime", `{"list":[{"orderId":"77","orderLinkId":"o-1","symbol":"BTCUSDT","side":"Buy","orderType":"Market","timeInForce":"IOC","orderStatus":"Filled","price":"0","qty":"0.002","avgPrice":"60000","cumExecQty":"0.002"}],"nextPageCursor":""}`)
	if _, err := newTestClientFor(t, m, broker.MarketSpot).OrderTrades(ctx, q); err == nil {
		t.Error("a filled order with no visible fills read as no commission")
	}
	// ...and an order that really filled nothing is an honest empty list.
	m = newMock(t)
	m.ok("GET /v5/execution/list", `{"list":[],"nextPageCursor":""}`)
	m.ok("GET /v5/order/realtime", `{"list":[{"orderId":"77","orderLinkId":"o-1","symbol":"BTCUSDT","side":"Buy","orderType":"Limit","timeInForce":"GTC","orderStatus":"Cancelled","price":"1","qty":"0.002","avgPrice":"","cumExecQty":"0"}],"nextPageCursor":""}`)
	if tr, err := newTestClientFor(t, m, broker.MarketSpot).OrderTrades(ctx, q); err != nil || len(tr) != 0 {
		t.Errorf("%+v %v", tr, err)
	}
}

func TestFundingIncome_WalksSevenDayWindowsAndFilters(t *testing.T) {
	const day = int64(86_400_000)
	m := newMock(t)
	var windows [][2]string
	m.on("GET /v5/account/transaction-log", func(q string, _ []byte) (int, string) {
		v, _ := url.ParseQuery(q)
		if v.Get("accountType") != "UNIFIED" || v.Get("category") != "linear" || v.Get("currency") != "USDT" || v.Get("type") != "SETTLEMENT" {
			t.Errorf("query %s", q)
		}
		windows = append(windows, [2]string{v.Get("startTime"), v.Get("endTime")})
		if len(windows) == 1 {
			if v.Get("cursor") == "" {
				return 200, envelopeOK(`{"list":[{"id":"a","symbol":"BTCUSDT","type":"SETTLEMENT","currency":"USDT","funding":"0.5","transactionTime":"2000"},{"id":"x","symbol":"ETHUSDT","type":"SETTLEMENT","currency":"USDT","funding":"9","transactionTime":"2001"}],"nextPageCursor":"p2"}`)
			}
		}
		if v.Get("cursor") == "p2" {
			return 200, envelopeOK(`{"list":[{"id":"b","symbol":"BTCUSDT","type":"TRADE","currency":"USDT","funding":"","transactionTime":"2100"}],"nextPageCursor":""}`)
		}
		return 200, envelopeOK(`{"list":[{"id":"c","symbol":"BTCUSDT","type":"SETTLEMENT","currency":"USDT","funding":"-0.25","transactionTime":"` + "900000000" + `"},{"id":"a","symbol":"BTCUSDT","type":"SETTLEMENT","currency":"USDT","funding":"0.5","transactionTime":"2000"}],"nextPageCursor":""}`)
	})
	rows, err := newTestClient(t, m).FundingIncome(ctx, broker.MarketFuturesUSDM, "BTCUSDT", 1000, 1000+10*day)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 3 || windows[0][0] != "1000" || windows[2][1] != "864001000" {
		t.Errorf("ten days is two seven-day windows (the first paged): %v", windows)
	}
	if len(rows) != 2 || rows[0].IncomeQuote != 0.5 || rows[1].IncomeQuote != -0.25 || rows[1].TranID != "c" {
		t.Errorf("other symbols and types dropped, the venue's sign kept, a repeated id counted once: %+v", rows)
	}
	if _, err := newTestClient(t, newMock(t)).FundingIncome(ctx, broker.MarketFuturesUSDM, "BTCUSDT", 0, 5); !errors.Is(err, broker.ErrInvalidOrder) {
		t.Errorf("an open-ended window would silently be the venue's 24 h default: %v", err)
	}
}

func TestMarkPriceFeeRateAndRiskLimit(t *testing.T) {
	m := newMock(t)
	m.ok("GET /v5/market/tickers", `{"category":"linear","list":[{"symbol":"BTCUSDT","markPrice":"75986.00","indexPrice":"76475.46","fundingRate":"0.0001","nextFundingTime":"1789632000000","fundingIntervalHour":"8"}]}`)
	var feeQuery string
	m.on("GET /v5/account/fee-rate", func(q string, _ []byte) (int, string) {
		feeQuery = q
		return 200, envelopeOK(`{"list":[{"symbol":"BTCUSDT","takerFeeRate":"0.00055","makerFeeRate":"0.0002"}]}`)
	})
	// Measured on api-testnet 2026-09-17, abridged.
	m.ok("GET /v5/market/risk-limit", `{"category":"linear","list":[{"id":2,"symbol":"BTCUSDT","riskLimitValue":"2000000","maintenanceMargin":"0.005","initialMargin":"0.01","isLowestRisk":0,"maxLeverage":"100.00"},{"id":1,"symbol":"BTCUSDT","riskLimitValue":"300000","maintenanceMargin":"0.0033","initialMargin":"0.0066","isLowestRisk":1,"maxLeverage":"150.00"}],"nextPageCursor":""}`)
	c := newTestClient(t, m)
	mp, err := c.MarkPrice(ctx, broker.MarketFuturesUSDM, "BTCUSDT")
	if err != nil || mp.MarkPriceQuote != 75986 || mp.LastFundingRateFrac != 0.0001 || mp.NextFundingTimeMs != 1789632000000 {
		t.Fatalf("%+v %v", mp, err)
	}
	fr, err := c.CommissionRates(ctx, "BTCUSDT")
	if err != nil || fr.TakerBps() != 5.5 || feeQuery != "category=linear&symbol=BTCUSDT" {
		t.Fatalf("%+v %v %s", fr, err, feeQuery)
	}
	b, err := c.FetchMaintenanceBracket(ctx, "BTCUSDT", 500_000)
	if err != nil || b.Tier != 2 || b.NotionalFloorQuote != 300_000 || b.MaintMarginFrac != 0.005 || b.MaxLeverage != 100 {
		t.Fatalf("tiers sorted by limit, floor = previous limit: %+v %v", b, err)
	}
	if b, err := c.FetchMaintenanceBracket(ctx, "BTCUSDT", 100); err != nil || b.Tier != 1 || b.MaintMarginFrac != 0.0033 {
		t.Errorf("%+v %v", b, err)
	}
	if _, err := c.FetchMaintenanceBracket(ctx, "BTCUSDT", 3_000_000); err == nil {
		t.Error("a notional past every tier took the last one")
	}

	// The page's own inverse example — "0.5" / "1" beside 100× — is PERCENT;
	// refused, not read as a 50% margin.
	m = newMock(t)
	m.ok("GET /v5/market/risk-limit", `{"category":"linear","list":[{"id":1,"symbol":"BTCUSDT","riskLimitValue":"150","maintenanceMargin":"0.5","initialMargin":"1","maxLeverage":"100.00"}],"nextPageCursor":""}`)
	if _, err := newTestClient(t, m).FetchMaintenanceBracket(ctx, "BTCUSDT", 100); err == nil {
		t.Error("a percent-unit tier was accepted as a fraction")
	}
	// The live testnet's LAST tier is a real 0.6 fraction at 1× (measured
	// 2026-09-17) — a first version refused it with a guessed "< 0.5" ceiling.
	m = newMock(t)
	m.ok("GET /v5/market/risk-limit", `{"category":"linear","list":[{"id":34,"symbol":"BTCUSDT","riskLimitValue":"800000000","maintenanceMargin":"0.42","initialMargin":"0.6993","maxLeverage":"1.43"},{"id":35,"symbol":"BTCUSDT","riskLimitValue":"1200000000","maintenanceMargin":"0.6","initialMargin":"1","maxLeverage":"1.00","mmDeduction":"242382510"}],"nextPageCursor":""}`)
	if b, err := newTestClient(t, m).FetchMaintenanceBracket(ctx, "BTCUSDT", 1_000_000_000); err != nil || b.Tier != 35 || b.MaintMarginFrac != 0.6 {
		t.Errorf("the deepest real tier: %+v %v", b, err)
	}
	// A number (the page's declared type) parses the same as a string.
	m = newMock(t)
	m.ok("GET /v5/market/risk-limit", `{"category":"linear","list":[{"id":1,"symbol":"BTCUSDT","riskLimitValue":"150","maintenanceMargin":0.004,"initialMargin":0.01,"maxLeverage":"100.00"}],"nextPageCursor":""}`)
	if b, err := newTestClient(t, m).FetchMaintenanceBracket(ctx, "BTCUSDT", 100); err != nil || b.MaintMarginFrac != 0.004 {
		t.Errorf("%+v %v", b, err)
	}
	// Maintenance above initial is not a schedule.
	m = newMock(t)
	m.ok("GET /v5/market/risk-limit", `{"category":"linear","list":[{"id":1,"symbol":"BTCUSDT","riskLimitValue":"150","maintenanceMargin":"0.02","initialMargin":"0.01","maxLeverage":"100.00"}],"nextPageCursor":""}`)
	if _, err := newTestClient(t, m).FetchMaintenanceBracket(ctx, "BTCUSDT", 100); err == nil {
		t.Error("maintenance above initial was accepted")
	}
	// A negative or blank taker rate is not "free".
	m = newMock(t)
	m.ok("GET /v5/account/fee-rate", `{"list":[{"symbol":"BTCUSDT","takerFeeRate":""}]}`)
	if _, err := newTestClient(t, m).CommissionRates(ctx, "BTCUSDT"); err == nil {
		t.Error("a blank taker rate was accepted")
	}
}

func TestFundingRateHistoryRange_WalksBackwardsFromTheEnd(t *testing.T) {
	m := newMock(t)
	var calls atomic.Int32
	m.on("GET /v5/market/funding/history", func(q string, _ []byte) (int, string) {
		v, _ := url.ParseQuery(q)
		n := calls.Add(1)
		var rows []string
		switch n {
		case 1: // 200 rows ending at the requested end
			if v.Get("endTime") != "100000" {
				t.Errorf("first page %s", q)
			}
			for i := 0; i < 200; i++ {
				rows = append(rows, `{"symbol":"BTCUSDT","fundingRate":"0.0001","fundingRateTimestamp":"`+strconv.Itoa(100000-i*10)+`"}`)
			}
		case 2:
			if v.Get("endTime") != strconv.Itoa(100000-199*10-1) {
				t.Errorf("second page must end just before the oldest seen: %s", q)
			}
			for i := 0; i < 3; i++ {
				rows = append(rows, `{"symbol":"BTCUSDT","fundingRate":"0.0002","fundingRateTimestamp":"`+strconv.Itoa(98000-i*10)+`"}`)
			}
		default:
			t.Errorf("page %d", n)
		}
		return 200, envelopeOK(`{"category":"linear","list":[` + strings.Join(rows, ",") + `]}`)
	})
	got, err := newTestClient(t, m).FundingRateHistoryRange(ctx, "BTCUSDT", 97990, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 202 || got[0].SettledAtMs != 97990 || got[len(got)-1].SettledAtMs != 100000 {
		t.Errorf("oldest first, clipped to the window: %d rows, %v..%v", len(got), got[0], got[len(got)-1])
	}
}
