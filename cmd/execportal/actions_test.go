package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
	"futures-arbitrage-scanner/internal/broker/brokertest"
	"futures-arbitrage-scanner/internal/execution"
)

// The three order-sending endpoints, driven end to end through the real
// handler against brokertest's in-memory venue: execution.Open and Close run
// for real, only the exchange is fake. No socket is opened — the one HTTP call
// left, the clock read, is answered in-process.

// clockOnly answers the venue clock and fails the test on anything else.
type clockOnly struct{ t *testing.T }

func (c clockOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/time") {
		body := fmt.Sprintf(`{"serverTime":%d}`, time.Now().UnixMilli())
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
	c.t.Errorf("a request reached the network: %s", r.URL.Path)
	return nil, io.ErrUnexpectedEOF
}

// fakeVenue is brokertest.Fake plus the reads a venue client makes before an
// order: rules, book, maintenance bracket. MarkPrice, FundingIncome and
// OrderTrades come from the Fake itself.
type fakeVenue struct {
	*brokertest.Fake
	market broker.Market
	http   *broker.Client
	rules  binancebroker.MarketRules
	book   exchanges.DepthBook
}

func (f *fakeVenue) Market() broker.Market { return f.market }
func (f *fakeVenue) HTTP() *broker.Client  { return f.http }
func (f *fakeVenue) FetchInstrument(context.Context, string) (binancebroker.MarketRules, error) {
	return f.rules, nil
}
func (f *fakeVenue) FetchDepthBook(context.Context, string) (exchanges.DepthBook, error) {
	return f.book, nil
}
func (f *fakeVenue) FetchMaintenanceBracket(_ context.Context, symbol string, _ float64) (binancebroker.MaintenanceBracket, error) {
	return binancebroker.MaintenanceBracket{Symbol: symbol, Tier: 1, NotionalCapQuote: 50_000, MaintMarginFrac: 0.004, MaxLeverage: 125}, nil
}

var _ perpVenue = (*fakeVenue)(nil)

const fakeMidQuote = 77_000.0

func fakeBook() exchanges.DepthBook {
	b := exchanges.DepthBook{Symbol: "BTCUSDT", Source: "test"}
	for i := 0; i < 30; i++ {
		b.Bids = append(b.Bids, exchanges.DepthLevel{PriceQuote: fakeMidQuote - 0.05 - float64(i)*5, QtyNative: 5})
		b.Asks = append(b.Asks, exchanges.DepthLevel{PriceQuote: fakeMidQuote + 0.05 + float64(i)*5, QtyNative: 5})
	}
	return b
}

func newFakeVenue(t *testing.T, market broker.Market, rules exchanges.Instrument) *fakeVenue {
	t.Helper()
	cfg, err := binancebroker.DefaultConfig(market, broker.Credentials{APIKey: broker.NewSecret("k"), APISecret: broker.NewSecret("s")})
	if err != nil {
		t.Fatal(err)
	}
	cfg.TestTransport = clockOnly{t}
	hc, err := broker.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rules.BaseAsset, rules.QuoteAsset, rules.Status = "BTC", "USDT", exchanges.StatusTrading
	f := &fakeVenue{Fake: brokertest.New(), market: market, http: hc,
		rules: binancebroker.MarketRules{Instrument: rules}, book: fakeBook()}
	f.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1})
	f.SetStepSizeCoin(rules.StepSizeCoin)
	return f
}

func fakePortal(t *testing.T) (*portal, *fakeVenue, *fakeVenue) {
	t.Helper()
	spot := newFakeVenue(t, broker.MarketSpot, spotRulesBTC)
	spot.SetBaseAsset("BTC")
	spot.SetBalance(broker.MarketSpot, broker.Balance{Market: broker.MarketSpot, Asset: "BTC", FreeQtyCoin: 1},
		broker.Balance{Market: broker.MarketSpot, Asset: "USDT", FreeQtyCoin: 10_000})
	perp := newFakeVenue(t, broker.MarketFuturesUSDM, perpRulesBTC)
	perp.SetMarkPrice(broker.MarkPrice{MarkPriceQuote: fakeMidQuote, NextFundingTimeMs: time.Now().Add(time.Hour).UnixMilli()})

	p := newPortal(markets{spot: spot, perp: perp, spotSourceVI: "test", perpSourceVI: "test"},
		[]string{"BTCUSDT"}, "127.0.0.1", "8087", execSettings{
			MarginFrac: 0.5, MaxSlippageBps: 10, LegTimeout: 2 * time.Second, ActionTimeout: time.Minute,
		}, nil)
	p.stateDir = t.TempDir()
	return p, spot, perp
}

func postJSON[T any](t *testing.T, p *portal, action, path string, body any) (int, T) {
	t.Helper()
	blob, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, p, http.MethodPost, path, string(blob), writeOpts(action)...)
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("POST %s: %v — %s", path, err, rec.Body.String())
	}
	return rec.Code, out
}

func getJSON[T any](t *testing.T, p *portal, path string) T {
	t.Helper()
	rec := do(t, p, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body.String())
	}
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func orderCount(venues ...*fakeVenue) int {
	n := 0
	for _, v := range venues {
		n += len(v.Orders())
	}
	return n
}

func openOK(t *testing.T, p *portal) openView {
	t.Helper()
	code, v := postJSON[openView](t, p, "open", "/api/open", openRequest{Symbol: "BTCUSDT", NotionalQuote: 65})
	if code != http.StatusOK || !v.Hedged || v.Outcome != "both_open" || v.Alarm {
		t.Fatalf("open = %d %+v", code, v)
	}
	return v
}

// The whole life of one position through the page's own endpoints — the
// acceptance path of this step, on a fake venue.
func TestActions_OpenReadCloseOnTheFake(t *testing.T) {
	p, spot, perp := fakePortal(t)

	v := openOK(t, p)
	if !near(v.Spot.FilledQtyCoin, 0.0008) || !near(v.Perp.FilledQtyCoin, 0.0008) || v.ResidualQtyCoin > 1e-12 {
		t.Errorf("legs = spot %v perp %v residual %v, want 0.0008 each and 0", v.Spot.FilledQtyCoin, v.Perp.FilledQtyCoin, v.ResidualQtyCoin)
	}
	st, err := loadState(p.stateDir, v.IntentID)
	if err != nil {
		t.Fatalf("no intent file: %v", err)
	}
	if !st.tracked() || st.Outcome != "both_open" || !strings.HasPrefix(st.IntentID, "pbtcusdt-") {
		t.Errorf("cache = %+v", st)
	}

	pos := getJSON[positionsView](t, p, "/api/positions?symbol=BTCUSDT")
	if pos.Status != statusBothOpen || pos.StatusVI != "DELTA-NEUTRAL (HEDGED)" || !near(pos.DeltaResidualCoin, 0) ||
		!near(pos.SpotQtyCoin, 0.0008) || !near(pos.PerpQtyCoin, -0.0008) {
		t.Errorf("positions after open = %s %q spot %v perp %v delta %v (%s)", pos.Status, pos.StatusVI, pos.SpotQtyCoin, pos.PerpQtyCoin, pos.DeltaResidualCoin, pos.ReasonVI)
	}

	code, c := postJSON[closeView](t, p, "close", "/api/close", closeRequest{Symbol: "BTCUSDT", IntentID: v.IntentID})
	if code != http.StatusOK || !c.Flat || c.Refused || c.Alarm || c.SentUnconfirmed || !near(c.ClosedQtyCoin, 0.0008) {
		t.Fatalf("close = %d %+v", code, c)
	}
	if !strings.Contains(c.RealizedLabelVI, "KHÔNG phải lãi ròng") {
		t.Errorf("the realized figure reached the page without its label: %q", c.RealizedLabelVI)
	}
	st, _ = loadState(p.stateDir, v.IntentID)
	if st.ClosedAtMs == 0 || st.NoteVI != "" || st.tracked() {
		t.Errorf("cache after a flat close = %+v", st)
	}
	pos = getJSON[positionsView](t, p, "/api/positions?symbol=BTCUSDT")
	if pos.Status != statusBothFlat {
		t.Errorf("positions after close = %s (%s)", pos.Status, pos.ReasonVI)
	}

	before := orderCount(spot, perp)
	code, again := postJSON[closeView](t, p, "close", "/api/close", closeRequest{Symbol: "BTCUSDT", IntentID: v.IntentID})
	if code != http.StatusConflict || orderCount(spot, perp) != before || !strings.Contains(again.ErrorVI, "không có gì để đóng") {
		t.Errorf("a second close of a closed intent = %d %q, orders %d → %d — want 'nothing to close', not 'handle by hand'", code, again.ErrorVI, before, orderCount(spot, perp))
	}
}

// One position per symbol: execution's flat proof reads the ACCOUNT's perp.
func TestActions_ASecondOpenOnAHeldSymbolIsRefusedBeforeAnyOrder(t *testing.T) {
	p, spot, perp := fakePortal(t)
	openOK(t, p)
	before := orderCount(spot, perp)

	code, v := postJSON[openView](t, p, "open", "/api/open", openRequest{Symbol: "BTCUSDT", NotionalQuote: 65})
	if code != http.StatusConflict || !v.RefusedBeforePlacing || !strings.Contains(v.ErrorVI, "MỘT vị thế") {
		t.Errorf("second open = %d %+v", code, v)
	}
	if orderCount(spot, perp) != before {
		t.Errorf("a refused open placed %d orders", orderCount(spot, perp)-before)
	}
}

// A perp position nobody's intent explains: the close would read it as a
// failed close, so the portal does not close.
func TestActions_CloseIsRefusedWhenTheVenueHoldsMoreThanTheIntent(t *testing.T) {
	p, spot, perp := fakePortal(t)
	v := openOK(t, p)
	perp.SetPosition(broker.Position{Market: broker.MarketFuturesUSDM, Symbol: "BTCUSDT", QtyCoin: -0.0016})
	before := orderCount(spot, perp)

	code, c := postJSON[closeView](t, p, "close", "/api/close", closeRequest{Symbol: "BTCUSDT", IntentID: v.IntentID})
	if code != http.StatusConflict || !strings.Contains(c.ErrorVI, "SÀN báo vị thế perp") || orderCount(spot, perp) != before {
		t.Errorf("close beside a foreign perp = %d %q, orders %d → %d", code, c.ErrorVI, before, orderCount(spot, perp))
	}
}

// A close id the venue already knows is never sent again.
func TestActions_CloseRefusesToReuseACloseID(t *testing.T) {
	p, spot, perp := fakePortal(t)
	v := openOK(t, p)
	place(t, perp, broker.MarketFuturesUSDM, broker.SideBuy, execution.CloseClientOrderID(v.IntentID, execution.LegPerp), 0.0004)
	before := orderCount(spot, perp)

	code, c := postJSON[closeView](t, p, "close", "/api/close", closeRequest{Symbol: "BTCUSDT", IntentID: v.IntentID})
	if code != http.StatusConflict || !strings.Contains(c.ErrorVI, "ĐÓNG của ý định này đã tồn tại") || orderCount(spot, perp) != before {
		t.Errorf("close with a spent close id = %d %q", code, c.ErrorVI)
	}
}

// A closing order that reached the venue and filled nothing is NOT "refused,
// both legs untouched": it is reported as sent, and the intent stays tracked.
func TestActions_ACloseThatWasSentButFilledNothingIsNotCalledRefused(t *testing.T) {
	p, _, perp := fakePortal(t)
	v := openOK(t, p)
	// The perp market order is accepted and fills nothing on the 0.0001 grid.
	perp.SetBehaviour(brokertest.Behaviour{MarketFillFraction: 0.01})

	code, c := postJSON[closeView](t, p, "close", "/api/close", closeRequest{Symbol: "BTCUSDT", IntentID: v.IntentID})
	if code != http.StatusOK || c.Refused || !c.SentUnconfirmed || c.Flat {
		t.Fatalf("close whose order filled nothing = %d refused=%v sent_unconfirmed=%v flat=%v %s", code, c.Refused, c.SentUnconfirmed, c.Flat, c.ErrorVI)
	}
	st, _ := loadState(p.stateDir, v.IntentID)
	if st.ClosedAtMs != 0 || st.NoteVI == "" || !st.tracked() {
		t.Errorf("cache after an unconfirmed close = closed %d note %q tracked %v", st.ClosedAtMs, st.NoteVI, st.tracked())
	}
}

// The 2026-09-13 shape: one leg closed, the other not. Opening is refused, and
// reconcile squares it — but only the plan the operator confirmed.
func TestActions_ReconcileSquaresOnlyTheConfirmedPlan(t *testing.T) {
	p, spot, perp := fakePortal(t)
	v := openOK(t, p)
	place(t, spot, broker.MarketSpot, broker.SideSell, execution.CloseClientOrderID(v.IntentID, execution.LegSpot), 0.0008)

	if pos := getJSON[positionsView](t, p, "/api/positions?symbol=BTCUSDT"); pos.Status != statusUnhedged {
		t.Errorf("naked perp short reads as %s (%s)", pos.Status, pos.ReasonVI)
	}
	// The perp position is non-zero, so no second position may be opened.
	if code, o := postJSON[openView](t, p, "open", "/api/open", openRequest{Symbol: "BTCUSDT", NotionalQuote: 65}); code != http.StatusConflict {
		t.Errorf("open beside a naked leg = %d %s", code, o.ErrorVI)
	}

	code, plan := postJSON[reconcileView](t, p, "reconcile", "/api/reconcile", reconcileRequest{Symbol: "BTCUSDT"})
	if code != http.StatusOK || plan.ToSend != 1 || plan.PlanDigest == "" || plan.ConflictVI != "" {
		t.Fatalf("dry run = %d %+v", code, plan)
	}
	if got := plan.Plans[0]; got.Market != broker.MarketFuturesUSDM || got.Side != broker.SideBuy || !got.ReduceOnly || !near(got.QtyCoin, 0.0008) {
		t.Errorf("plan = %+v", got)
	}

	before := orderCount(spot, perp)
	for name, digest := range map[string]string{"no digest": "", "a stale digest": "0000000000000000"} {
		code, r := postJSON[reconcileView](t, p, "reconcile", "/api/reconcile", reconcileRequest{Symbol: "BTCUSDT", Apply: true, PlanDigest: digest})
		if code != http.StatusConflict || len(r.Results) != 0 || orderCount(spot, perp) != before {
			t.Errorf("apply with %s = %d, %d results, orders %d → %d", name, code, len(r.Results), before, orderCount(spot, perp))
		}
	}

	code, r := postJSON[reconcileView](t, p, "reconcile", "/api/reconcile", reconcileRequest{Symbol: "BTCUSDT", Apply: true, PlanDigest: plan.PlanDigest})
	if code != http.StatusOK || len(r.Results) != 1 || !r.Results[0].Balanced || math.Abs(r.Results[0].ResidualAfterCoin) > 1e-12 {
		t.Fatalf("apply = %d %+v", code, r)
	}
	if r.Results[0].ClientOrderID != execution.ReconcileClientOrderID(v.IntentID, execution.LegPerp) {
		t.Errorf("squaring id %q is not the derived one", r.Results[0].ClientOrderID)
	}
	if pos := getJSON[positionsView](t, p, "/api/positions?symbol=BTCUSDT"); pos.Status != statusBothFlat {
		t.Errorf("after reconcile = %s (%s)", pos.Status, pos.ReasonVI)
	}
	st, _ := loadState(p.stateDir, v.IntentID)
	if !strings.Contains(st.NoteVI, "đã cân lại") {
		t.Errorf("note = %q", st.NoteVI)
	}

	// A second reconcile has nothing to do, and sends nothing.
	before = orderCount(spot, perp)
	if code, again := postJSON[reconcileView](t, p, "reconcile", "/api/reconcile", reconcileRequest{Symbol: "BTCUSDT"}); code != http.StatusOK || again.ToSend != 0 {
		t.Errorf("second dry run = %d %+v", code, again)
	}
	if orderCount(spot, perp) != before {
		t.Error("a dry run placed an order")
	}
}

// An unreadable intent file is an intent nobody is watching: not green.
func TestPositions_AnUnreadableIntentFileIsUnknown(t *testing.T) {
	p, _, _ := fakePortal(t)
	if err := os.WriteFile(filepath.Join(p.stateDir, "pbtcusdt-broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	pos := getJSON[positionsView](t, p, "/api/positions?symbol=BTCUSDT")
	if pos.Status != statusUnknown {
		t.Errorf("status with an unreadable intent file = %s (%s)", pos.Status, pos.ReasonVI)
	}
}

// seedIntent places an intent's two opening orders directly on the fake and
// writes its cache file — a pair execution would call hedged, of any shape.
func seedIntent(t *testing.T, p *portal, spot, perp *fakeVenue, id string, spotQtyCoin, perpQtyCoin float64) {
	t.Helper()
	place(t, spot, broker.MarketSpot, broker.SideBuy, execution.LegClientOrderID(id, execution.LegSpot), spotQtyCoin)
	place(t, perp, broker.MarketFuturesUSDM, broker.SideSell, execution.LegClientOrderID(id, execution.LegPerp), perpQtyCoin)
	if err := saveState(p.stateDir, intentState{IntentID: id, Symbol: "BTCUSDT", Outcome: "both_open",
		OpenedAtMs: time.Now().UnixMilli(), SpotFilledQtyCoin: spotQtyCoin, PerpFilledQtyCoin: perpQtyCoin,
		SpotAvgPriceQuote: fakeMidQuote, PerpAvgPriceQuote: fakeMidQuote, SpotRefMidQuote: fakeMidQuote, PerpRefMidQuote: fakeMidQuote}); err != nil {
		t.Fatal(err)
	}
}

// Found by the re-review: legs one sub-step apart (spot 0.00079, perp 0.0008)
// are a hedge, and a close sized at the smaller leg left 0.0001 of perp on the
// account that nothing on the page could reach. Sized at the perp, the perp
// ends flat and the symbol is free again.
func TestActions_ACloseOfLegsASubStepApartLeavesThePerpFlat(t *testing.T) {
	p, spot, perp := fakePortal(t)
	const id = "pbtcusdt-20260914-000000-001"
	seedIntent(t, p, spot, perp, id, 0.00079, 0.0008)
	if pos := getJSON[positionsView](t, p, "/api/positions?symbol=BTCUSDT"); pos.Status != statusBothOpen {
		t.Fatalf("a sub-step pair reads as %s (%s)", pos.Status, pos.ReasonVI)
	}

	code, c := postJSON[closeView](t, p, "close", "/api/close", closeRequest{Symbol: "BTCUSDT", IntentID: id})
	if code != http.StatusOK || !c.Flat || !near(c.IntentQtyCoin, 0.0008) {
		t.Fatalf("close = %d flat=%v intent_qty=%v %s", code, c.Flat, c.IntentQtyCoin, c.ErrorVI)
	}
	venuePos, err := perp.GetPosition(context.Background(), broker.MarketFuturesUSDM, "BTCUSDT")
	if err != nil || !venuePos.Flat() {
		t.Fatalf("the venue still holds perp %+v after the close (%v)", venuePos.QtyCoin, err)
	}
	if pos := getJSON[positionsView](t, p, "/api/positions?symbol=BTCUSDT"); pos.Status != statusBothFlat {
		t.Errorf("after close = %s (%s)", pos.Status, pos.ReasonVI)
	}
	// And the symbol is usable: a new position may be opened.
	openOK(t, p)
}

// Selling the perp's quantity on spot needs the wallet to hold it; when it does
// not, nothing is sent rather than leaving a perp step nobody can reach.
func TestActions_ACloseTheSpotWalletCannotCoverIsRefusedBeforeAnyOrder(t *testing.T) {
	p, spot, perp := fakePortal(t)
	const id = "pbtcusdt-20260914-000000-002"
	seedIntent(t, p, spot, perp, id, 0.00079, 0.0008)
	// Plenty of BTC in total, but all but 0.00079 of it LOCKED under a resting
	// order — coin a market sell cannot use.
	spot.SetBalance(broker.MarketSpot, broker.Balance{Market: broker.MarketSpot, Asset: "BTC", FreeQtyCoin: 0.00079, LockedQtyCoin: 0.01})
	before := orderCount(spot, perp)

	code, c := postJSON[closeView](t, p, "close", "/api/close", closeRequest{Symbol: "BTCUSDT", IntentID: id})
	if code != http.StatusConflict || !strings.Contains(c.ErrorVI, "tự do") || orderCount(spot, perp) != before {
		t.Errorf("close the wallet cannot cover = %d %q, orders %d → %d", code, c.ErrorVI, before, orderCount(spot, perp))
	}
}

// Perp flat, spot still long by more than a step: reconcile can square that,
// so close points there instead of at "handle by hand".
func TestActions_CloseOfANakedSpotLegPointsToReconcile(t *testing.T) {
	p, _, perp := fakePortal(t)
	v := openOK(t, p)
	place(t, perp, broker.MarketFuturesUSDM, broker.SideBuy, execution.CloseClientOrderID(v.IntentID, execution.LegPerp), 0.0008)
	code, c := postJSON[closeView](t, p, "close", "/api/close", closeRequest{Symbol: "BTCUSDT", IntentID: v.IntentID})
	if code != http.StatusConflict || !strings.Contains(c.ErrorVI, "LÀM PHẲNG") {
		t.Errorf("close of a naked spot leg = %d %q", code, c.ErrorVI)
	}
}
