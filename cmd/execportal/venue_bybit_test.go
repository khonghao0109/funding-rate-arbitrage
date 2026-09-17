package main

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/cmd/execportal/autotrade"
	"futures-arbitrage-scanner/internal/broker"
	bybitbroker "futures-arbitrage-scanner/internal/broker/bybit"
)

// cannedV5 answers Bybit V5 paths with fixed bodies and fails the test on any
// other path, so an adapter read that reached for something unexpected is seen.
type cannedV5 struct {
	t       *testing.T
	answers map[string]string
}

func (c cannedV5) RoundTrip(r *http.Request) (*http.Response, error) {
	body, ok := c.answers[r.URL.Path+"?"+r.URL.RawQuery]
	if !ok {
		body, ok = c.answers[r.URL.Path]
	}
	if r.URL.Path == broker.BybitTimePath {
		body, ok = `{"timeSecond":"1","timeNano":"`+strconv.FormatInt(time.Now().UnixNano(), 10)+`"}`, true
	}
	if !ok {
		c.t.Errorf("unexpected Bybit call %s?%s", r.URL.Path, r.URL.RawQuery)
		return nil, io.ErrUnexpectedEOF
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"retCode":0,"retMsg":"OK","result":` + body + `,"time":1}`)), Request: r}, nil
}

func cannedBybitVenues(t *testing.T, answers map[string]string) (spot, perp bybitVenue) {
	t.Helper()
	cfg, err := bybitbroker.DefaultConfig(bybitbroker.ModeTestnet, broker.Credentials{APIKey: broker.NewSecret("k"), APISecret: broker.NewSecret("s")})
	if err != nil {
		t.Fatal(err)
	}
	cfg.TestTransport = cannedV5{t, answers}
	c, err := bybitbroker.New(bybitbroker.ModeTestnet, broker.MarketFuturesUSDM, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.WithMarket(broker.MarketSpot)
	if err != nil {
		t.Fatal(err)
	}
	return bybitVenue{s}, bybitVenue{c}
}

func TestBybitVenue_ConvertsTheReadsAndKeepsTheCapabilities(t *testing.T) {
	spot, perp := cannedBybitVenues(t, map[string]string{
		"/v5/market/instruments-info?category=spot&symbol=BTCUSDT": `{"category":"spot","list":[{"symbol":"BTCUSDT","baseCoin":"BTC","quoteCoin":"USDT","status":"Trading","lotSizeFilter":{"basePrecision":"0.000001","minOrderQty":"0.000001","maxOrderQty":"10","minOrderAmt":"5","maxMarketOrderQty":"10"},"priceFilter":{"tickSize":"0.1"}}]}`,
		"/v5/account/fee-rate?category=spot&symbol=BTCUSDT":        `{"list":[{"symbol":"BTCUSDT","takerFeeRate":"0.001","makerFeeRate":"0.001"}]}`,
		"/v5/market/risk-limit?category=linear&symbol=BTCUSDT":     `{"category":"linear","list":[{"id":1,"symbol":"BTCUSDT","riskLimitValue":"300000","maintenanceMargin":"0.0033","initialMargin":"0.0066","isLowestRisk":1,"maxLeverage":"150.00"}],"nextPageCursor":""}`,
		"/v5/market/funding/history":                               `{"category":"linear","list":[{"symbol":"BTCUSDT","fundingRate":"0.0001","fundingRateTimestamp":"1789600000000"},{"symbol":"BTCUSDT","fundingRate":"0.00005","fundingRateTimestamp":"1789571200000"}]}`,
	})
	ctx := context.Background()
	rules, err := spot.FetchInstrument(ctx, "BTCUSDT")
	if err != nil || rules.StepSizeCoin != 0.000001 || rules.MinNotionalQuote != 5 || rules.BaseAsset != "BTC" || rules.BuyPriceFloorFrac != 0 {
		t.Fatalf("rules %+v %v", rules, err)
	}
	fee, err := spot.CommissionRates(ctx, "BTCUSDT")
	if err != nil || fee.TakerBps() != 10 || fee.Market != broker.MarketSpot {
		t.Fatalf("fee %+v %v", fee, err)
	}
	b, err := perp.FetchMaintenanceBracket(ctx, "BTCUSDT", 1_000)
	if err != nil || b.MaintMarginFrac != 0.0033 || b.NotionalCapQuote != 300_000 {
		t.Fatalf("bracket %+v %v", b, err)
	}
	rows, err := perp.FundingRateHistory(ctx, "BTCUSDT", 1789571200000, 1789600000000)
	if err != nil || len(rows) != 2 || rows[0].SettledAtMs != 1789571200000 || rows[1].RatePerIntervalFrac != 0.0001 || rows[0].RateType != "" {
		t.Fatalf("funding %+v %v", rows, err)
	}
	bracket, err := perpBracket(ctx, perp, profileFor(venueBybit), "BTCUSDT", 1_000)
	if err != nil || !bracket.Verified || !strings.Contains(bracket.NoteVI, "/v5/market/risk-limit") || !strings.HasPrefix(bracket.Source, "bybit_linear_") {
		t.Errorf("risk bracket %+v %v", bracket, err)
	}
	// execution finds its optional capabilities on the value it is handed.
	var handed broker.Broker = perp
	if _, ok := handed.(broker.FundingReader); !ok {
		t.Error("the perp venue lost FundingReader behind the adapter")
	}
	if _, ok := handed.(broker.TradeReader); !ok {
		t.Error("the perp venue lost TradeReader behind the adapter")
	}
	if _, ok := handed.(broker.MarkPriceReader); !ok {
		t.Error("the perp venue lost MarkPriceReader behind the adapter")
	}
}

// On a unified wallet the bot's rebalance reads the quote ONCE. Two readings
// added — the Binance arithmetic — would size every slot on twice the money.
func TestPortalTrader_UnifiedWalletIsReadOnce(t *testing.T) {
	p, _, perp := fakePortal(t)
	p.markets.profile = profileFor(venueBybit)
	acct, err := portalTrader{p}.Account(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !acct.UnifiedWallet || acct.SpotQuoteTotal != 5_000 || acct.FuturesQuoteTotal != 0 {
		t.Errorf("unified account %+v, want the one wallet's 5000 once", acct)
	}
	perp.balanceErr = io.ErrUnexpectedEOF
	p.rawBalances.invalidate()
	if _, err := (portalTrader{p}).Account(context.Background()); err == nil {
		t.Error("an unreadable wallet produced an account")
	}

	// The Binance portal still reads two wallets.
	p2, _, _ := fakePortal(t)
	acct, err = portalTrader{p2}.Account(context.Background())
	if err != nil || acct.UnifiedWallet || acct.SpotQuoteTotal != 10_000 || acct.FuturesQuoteTotal != 5_000 {
		t.Errorf("binance account %+v %v", acct, err)
	}
	var _ autotrade.Account = acct
}

// Round 2 of the 4.5j review left the spot leg judged by its orders in three
// places, so the Bybit portal is READ-ONLY: every write is refused before any
// handler runs, reads still answer, and nothing reaches a venue.
func TestBybitPortal_IsReadOnlyUntilTheSpotLegIsJudgedByTheWallet(t *testing.T) {
	p, spot, perp := fakePortal(t)
	p.markets.profile = profileFor(venueBybit)
	if p.markets.profile.OrdersBlockedVI == "" {
		t.Fatal("the Bybit profile does not block orders")
	}
	for _, w := range []struct{ action, path string }{
		{"open", "/api/open"}, {"close", "/api/close"}, {"reconcile", "/api/reconcile"}, {"autotrade-start", "/api/autotrade/start"},
	} {
		rec := do(t, p, http.MethodPost, w.path, `{"symbol":"BTCUSDT","notional_quote":65}`, writeOpts(w.action)...)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "venue_read_only") {
			t.Errorf("%s = %d %s", w.path, rec.Code, rec.Body.String())
		}
	}
	if n := len(spot.Orders()) + len(perp.Orders()); n != 0 {
		t.Errorf("%d orders reached the venue", n)
	}
	if rec := do(t, p, http.MethodGet, "/api/status", "", withHeader(actionHeader, "read")); rec.Code != http.StatusOK {
		t.Errorf("status on a read-only portal = %d", rec.Code)
	}
}
