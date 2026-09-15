package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"futures-arbitrage-scanner/internal/broker"
)

// The two reads the testnet auto-trader makes before it may decide (PLAN Q18):
// settled funding rates and this account's commission. Replayed from the
// TESTNET's own answers of 2026-09-15 — a fee rate is not a balance, so unlike
// the account endpoints these may be recorded — and from hand-broken copies of
// them, because the failure that matters is a missing field read as zero.

// recordedVenue answers every non-clock request with one fixed body, in
// process, and remembers what was asked.
type recordedVenue struct {
	t    *testing.T
	body string

	mu    sync.Mutex
	paths []string
	query []string
}

func (v *recordedVenue) RoundTrip(r *http.Request) (*http.Response, error) {
	body := v.body
	if strings.HasSuffix(r.URL.Path, "/time") {
		body = fmt.Sprintf(`{"serverTime":%d}`, time.Now().UnixMilli())
	} else {
		v.mu.Lock()
		v.paths = append(v.paths, r.URL.Path)
		v.query = append(v.query, r.URL.RawQuery)
		v.mu.Unlock()
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func recordedClient(t *testing.T, market broker.Market, body string) (*Client, *recordedVenue) {
	t.Helper()
	cfg, err := DefaultConfig(market, broker.Credentials{APIKey: broker.NewSecret("k"), APISecret: broker.NewSecret("s")})
	if err != nil {
		t.Fatal(err)
	}
	v := &recordedVenue{t: t, body: body}
	cfg.TestTransport = v
	c, err := New(market, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, v
}

func TestFundingRateHistory_AgainstTheTestnetAnswer(t *testing.T) {
	c, v := recordedClient(t, broker.MarketFuturesUSDM, string(loadRecording(t, "futures_funding_rate_get_200.json")))
	rows, err := c.FundingRateHistory(context.Background(), "BTCUSDT", 1_789_300_000_000, 1_789_447_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want the 3 recorded", len(rows))
	}
	// Stamps verbatim — the first one lands 1 ms past the hour and stays there.
	if rows[0].SettledAtMs != 1_789_372_800_001 || rows[2].SettledAtMs != 1_789_430_400_000 {
		t.Errorf("stamps = %d … %d, want the venue's own", rows[0].SettledAtMs, rows[2].SettledAtMs)
	}
	if math.Abs(rows[1].RatePerIntervalFrac-0.00007617) > 1e-15 || math.Abs(rows[2].RatePerIntervalFrac-0.0001) > 1e-15 {
		t.Errorf("rates = %v, %v", rows[1].RatePerIntervalFrac, rows[2].RatePerIntervalFrac)
	}
	if rows[2].MarkPriceQuote != 78149.51586957 || rows[0].RateType != "" {
		t.Errorf("mark %v, rateType %q", rows[2].MarkPriceQuote, rows[0].RateType)
	}
	if len(v.paths) != 1 || v.paths[0] != "/fapi/v1/fundingRate" {
		t.Fatalf("asked %v", v.paths)
	}
	for _, want := range []string{"symbol=BTCUSDT", "startTime=1789300000000", "endTime=1789447000000", "limit=1000"} {
		if !strings.Contains(v.query[0], want) {
			t.Errorf("query %q lacks %s", v.query[0], want)
		}
	}
	if strings.Contains(v.query[0], "signature=") {
		t.Error("a PUBLIC read was signed — the key went somewhere it did not need to")
	}
}

func TestFundingRateHistory_RefusesARowItCannotRead(t *testing.T) {
	for name, body := range map[string]string{
		"a rate that is not a number": `[{"symbol":"BTCUSDT","fundingTime":1,"fundingRate":"x"},{"symbol":"BTCUSDT","fundingTime":2,"fundingRate":"0.0001"}]`,
		"a missing rate":              `[{"symbol":"BTCUSDT","fundingTime":1}]`,
		"a missing stamp":             `[{"symbol":"BTCUSDT","fundingRate":"0.0001"}]`,
		"another symbol's rows":       `[{"symbol":"ETHUSDT","fundingTime":1,"fundingRate":"0.0001"}]`,
	} {
		if rows, err := parseFundingRates(json.RawMessage(body), "BTCUSDT"); err == nil {
			t.Errorf("%s: accepted %v", name, rows)
		}
	}
	// Oldest first whatever order they arrive in: callers read the last row as
	// the newest settlement.
	rows, err := parseFundingRates(json.RawMessage(`[{"symbol":"BTCUSDT","fundingTime":20,"fundingRate":"-0.0002"},{"symbol":"BTCUSDT","fundingTime":10,"fundingRate":"0.0001"}]`), "BTCUSDT")
	if err != nil || rows[0].SettledAtMs != 10 || rows[1].RatePerIntervalFrac != -0.0002 {
		t.Errorf("sorted = %+v, %v", rows, err)
	}
	spot, _ := recordedClient(t, broker.MarketSpot, "[]")
	if _, err := spot.FundingRateHistory(context.Background(), "BTCUSDT", 0, 0); err == nil {
		t.Error("the spot client read a funding history")
	}
}

func TestCommissionRates_AgainstTheTestnetAnswers(t *testing.T) {
	perp, pv := recordedClient(t, broker.MarketFuturesUSDM, string(loadRecording(t, "futures_commission_rate_get_200.json")))
	fr, err := perp.CommissionRates(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(fr.TakerBps()-4) > 1e-9 || fr.TakerBuyFrac != fr.TakerSellFrac {
		t.Errorf("futures taker = %v bps (buy %v sell %v), want the testnet's 4", fr.TakerBps(), fr.TakerBuyFrac, fr.TakerSellFrac)
	}
	if pv.paths[0] != "/fapi/v1/commissionRate" || !strings.Contains(pv.query[0], "signature=") || !strings.Contains(pv.query[0], "symbol=BTCUSDT") {
		t.Errorf("futures asked %v %v — want the signed commissionRate read", pv.paths, pv.query)
	}

	spot, sv := recordedClient(t, broker.MarketSpot, string(loadRecording(t, "spot_account_commission_get_200.json")))
	sr, err := spot.CommissionRates(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	// Measured: the spot TESTNET charges nothing on any component. That is a
	// fact about this account, and the parser must be able to say 0 when the
	// venue states 0 — as long as it was stated.
	if sr.TakerBps() != 0 || sr.Market != broker.MarketSpot {
		t.Errorf("spot taker = %v bps", sr.TakerBps())
	}
	if sv.paths[0] != "/api/v3/account/commission" || !strings.Contains(sv.query[0], "signature=") {
		t.Errorf("spot asked %v", sv.paths)
	}
}

// The commission FAQ's formula, per component and per side, summed.
func TestCommissionRates_SpotSumsEveryComponentForTheSide(t *testing.T) {
	body := `{"symbol":"BTCUSDT",
	  "standardCommission":{"maker":"0.001","taker":"0.001","buyer":"0.0001","seller":"0.0003"},
	  "specialCommission":{"maker":"0","taker":"0.00002","buyer":"0","seller":"0"},
	  "taxCommission":{"maker":"0","taker":"0.00001","buyer":"0.00002","seller":"0"}}`
	r, err := parseSpotCommission(json.RawMessage(body), "BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.TakerBuyFrac-(0.001+0.0001+0.00002+0.00001+0.00002)) > 1e-12 {
		t.Errorf("buy = %v", r.TakerBuyFrac)
	}
	if math.Abs(r.TakerSellFrac-(0.001+0.0003+0.00002+0.00001)) > 1e-12 {
		t.Errorf("sell = %v", r.TakerSellFrac)
	}
	// One number per market takes the dearer side.
	if math.Abs(r.TakerBps()-r.TakerSellFrac*10_000) > 1e-9 {
		t.Errorf("TakerBps = %v, want the sell side's %v", r.TakerBps(), r.TakerSellFrac*10_000)
	}
}

// Every way a fee could silently become zero is a refusal.
func TestCommissionRates_AMissingFeeIsNeverFree(t *testing.T) {
	futures := map[string]string{
		"no taker field":        `{"symbol":"BTCUSDT","makerCommissionRate":"0.0002"}`,
		"an empty taker":        `{"symbol":"BTCUSDT","takerCommissionRate":""}`,
		"a negative taker":      `{"symbol":"BTCUSDT","takerCommissionRate":"-0.0004"}`,
		"a percent, not a rate": `{"symbol":"BTCUSDT","takerCommissionRate":"4"}`,
		"another symbol":        `{"symbol":"ETHUSDT","takerCommissionRate":"0.0004"}`,
	}
	for name, body := range futures {
		if r, err := parseFuturesCommission(json.RawMessage(body), "BTCUSDT"); err == nil {
			t.Errorf("futures, %s: accepted %+v", name, r)
		}
	}
	full := `"standardCommission":{"maker":"0","taker":"0","buyer":"0","seller":"0"},"specialCommission":{"maker":"0","taker":"0","buyer":"0","seller":"0"}`
	spot := map[string]string{
		"no tax component":          `{"symbol":"BTCUSDT",` + full + `}`,
		"a component with no buyer": `{"symbol":"BTCUSDT",` + full + `,"taxCommission":{"maker":"0","taker":"0","seller":"0"}}`,
		"nothing at all":            `{"symbol":"BTCUSDT"}`,
	}
	for name, body := range spot {
		if r, err := parseSpotCommission(json.RawMessage(body), "BTCUSDT"); err == nil {
			t.Errorf("spot, %s: accepted %+v", name, r)
		}
	}
}
