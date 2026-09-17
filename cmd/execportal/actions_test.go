package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
	"futures-arbitrage-scanner/internal/broker/brokertest"
	bybitbroker "futures-arbitrage-scanner/internal/broker/bybit"
	"futures-arbitrage-scanner/internal/depth"
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

	feesErr error
	// spotTakerFrac overrides the spot market's fee (0 = Binance spot testnet).
	spotTakerFrac float64
	fundingRates  []binancebroker.FundingRate
	// fundingBySymbol, when it names a symbol, answers that symbol's history
	// instead of fundingRates — so a cache that served one symbol's rows for
	// another would be seen.
	fundingBySymbol map[string][]binancebroker.FundingRate
	// fundingPageRows, when set, answers at most this many of the OLDEST rows
	// in the window, the way the venue answers a window wider than its page.
	fundingPageRows int
	// fundingIgnoresStart answers as if startTime had not been sent.
	fundingIgnoresStart bool
	// fundingCalls counts FundingRateHistory reads; the engine reads several
	// symbols at once, so it is atomic.
	fundingCalls atomic.Int64
	// balanceErr makes this wallet unreadable.
	balanceErr error
	// tradesErr makes the fill list unreadable — Bybit's execution list past
	// its default 7-day window.
	tradesErr error
	// notVisible answers an id the venue does not have the way the Bybit
	// adapter does — bybit.ErrOrderNotVisible, which is NOT ErrOrderNotFound.
	notVisible bool

	// bookMu guards the two book knobs below, which the engine's parallel
	// reads and a test goroutine may touch at once.
	bookMu sync.Mutex
	// bookQueue answers the next book reads in order, one each, before falling
	// back to book — so a scan and the close right after it can see different
	// books, which is exactly the gap the pre-flight spread guard exists for.
	bookQueue []exchanges.DepthBook
	// bookErr makes the book unreadable.
	bookErr error
}

func (f *fakeVenue) queueBooks(books ...exchanges.DepthBook) {
	f.bookMu.Lock()
	defer f.bookMu.Unlock()
	f.bookQueue = append(f.bookQueue, books...)
}

func (f *fakeVenue) setBookErr(err error) {
	f.bookMu.Lock()
	defer f.bookMu.Unlock()
	f.bookErr = err
}

// GetBalance is brokertest's, with a knob for the wallet a venue will not give
// up: the auto-trader's rebalance must leave its size alone rather than read
// the failure as an empty account (autotrade/capital.go).
func (f *fakeVenue) GetBalance(ctx context.Context, market broker.Market) ([]broker.Balance, error) {
	if f.balanceErr != nil {
		return nil, f.balanceErr
	}
	return f.Fake.GetBalance(ctx, market)
}

// GetOrder is brokertest's, answering an unknown id as Bybit's adapter does
// when notVisible is set.
func (f *fakeVenue) GetOrder(ctx context.Context, q broker.OrderQuery) (broker.Order, error) {
	o, err := f.Fake.GetOrder(ctx, q)
	if f.notVisible && errors.Is(err, broker.ErrOrderNotFound) {
		return broker.Order{}, fmt.Errorf("%w: neither list shows it", bybitbroker.ErrOrderNotVisible)
	}
	return o, err
}

// OrderTrades is brokertest's, with a knob for a fill list the venue no longer
// answers.
func (f *fakeVenue) OrderTrades(ctx context.Context, q broker.OrderQuery) ([]broker.Trade, error) {
	if f.tradesErr != nil {
		return nil, f.tradesErr
	}
	return f.Fake.OrderTrades(ctx, q)
}

func (f *fakeVenue) Market() broker.Market { return f.market }
func (f *fakeVenue) HTTP() *broker.Client  { return f.http }
func (f *fakeVenue) FetchInstrument(context.Context, string) (binancebroker.MarketRules, error) {
	return f.rules, nil
}
func (f *fakeVenue) FetchDepthBook(context.Context, string) (exchanges.DepthBook, error) {
	f.bookMu.Lock()
	defer f.bookMu.Unlock()
	if f.bookErr != nil {
		return exchanges.DepthBook{}, f.bookErr
	}
	if len(f.bookQueue) > 0 {
		b := f.bookQueue[0]
		f.bookQueue = f.bookQueue[1:]
		return b, nil
	}
	return f.book, nil
}
func (f *fakeVenue) FetchMaintenanceBracket(_ context.Context, symbol string, _ float64) (binancebroker.MaintenanceBracket, error) {
	return binancebroker.MaintenanceBracket{Symbol: symbol, Tier: 1, NotionalCapQuote: 50_000, MaintMarginFrac: 0.004, MaxLeverage: 125}, nil
}

// CommissionRates answers the testnet's measured fees: spot 0, futures 4 bps.
func (f *fakeVenue) CommissionRates(_ context.Context, symbol string) (binancebroker.CommissionRates, error) {
	if f.feesErr != nil {
		return binancebroker.CommissionRates{}, f.feesErr
	}
	rate := f.spotTakerFrac
	if f.market == broker.MarketFuturesUSDM {
		rate = 0.0004
	}
	return binancebroker.CommissionRates{Market: f.market, Symbol: symbol, TakerBuyFrac: rate, TakerSellFrac: rate, SourceVI: "fake"}, nil
}

// FundingRateHistory answers whatever the test set, filtered to the window.
func (f *fakeVenue) FundingRateHistory(_ context.Context, symbol string, startMs, endMs int64) ([]binancebroker.FundingRate, error) {
	f.fundingCalls.Add(1)
	var out []binancebroker.FundingRate
	rates := f.fundingRates
	if own, ok := f.fundingBySymbol[symbol]; ok {
		rates = own
	}
	for _, r := range rates {
		if (startMs == 0 || f.fundingIgnoresStart || r.SettledAtMs >= startMs) && (endMs == 0 || r.SettledAtMs <= endMs) {
			r.Symbol = symbol
			out = append(out, r)
		}
		if f.fundingPageRows > 0 && len(out) == f.fundingPageRows {
			break
		}
	}
	return out, nil
}

var _ perpVenue = (*fakeVenue)(nil)

const fakeMidQuote = 77_000.0

// fakePerpMidQuote is the PERP book, trading above the spot by the basis the
// auto-trader's shipped Config.MinEntryBasisBps asks for: a fixture standing
// for a normal market has to clear that floor or the bot never enters (PLAN
// "Công cụ vận hành 4.5f"). The manual path does not read it and is unmoved.
const fakePerpBasisBps = 6.0
const fakePerpMidQuote = fakeMidQuote * (1 + fakePerpBasisBps/10_000)

func fakeBookAt(midQuote float64) exchanges.DepthBook {
	b := exchanges.DepthBook{Symbol: "BTCUSDT", Source: "test"}
	for i := 0; i < 30; i++ {
		b.Bids = append(b.Bids, exchanges.DepthLevel{PriceQuote: midQuote - 0.05 - float64(i)*5, QtyNative: 5})
		b.Asks = append(b.Asks, exchanges.DepthLevel{PriceQuote: midQuote + 0.05 + float64(i)*5, QtyNative: 5})
	}
	return b
}

func fakeBook() exchanges.DepthBook { return fakeBookAt(fakeMidQuote) }

// fakeBookWithSpread is fakeBookAt with its touch spreadBps wide around mid.
func fakeBookWithSpread(midQuote, spreadBps float64) exchanges.DepthBook {
	half := midQuote * spreadBps / 10_000 / 2
	b := exchanges.DepthBook{Symbol: "BTCUSDT", Source: "test"}
	for i := 0; i < 30; i++ {
		b.Bids = append(b.Bids, exchanges.DepthLevel{PriceQuote: midQuote - half - float64(i)*5, QtyNative: 5})
		b.Asks = append(b.Asks, exchanges.DepthLevel{PriceQuote: midQuote + half + float64(i)*5, QtyNative: 5})
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
	// The futures WALLET, which the auto-trader's rebalance sizes its slots
	// from beside the spot one (autotrade/capital.go). Two separate
	// registrations on this testnet, so two separate balances.
	perp.SetBalance(broker.MarketFuturesUSDM, broker.Balance{Market: broker.MarketFuturesUSDM, Asset: "USDT", FreeQtyCoin: 5_000})
	perp.book = fakeBookAt(fakePerpMidQuote)
	perp.SetMarkPrice(broker.MarkPrice{MarkPriceQuote: fakePerpMidQuote, NextFundingTimeMs: time.Now().Add(time.Hour).UnixMilli()})

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

// Trụ cột 5: every close writes WHY into the intent file, and the result page
// reads it back. The reason is the bot's own sentence when the bot closed, the
// operator's when a person did, and the file is the only record of it — a
// closed pair is gone from the venue.
func TestActions_ACloseRecordsItsReasonAndThePageReadsItBack(t *testing.T) {
	p, _, _ := fakePortal(t)

	// The page's own close, with no reason given: a person pressed the button.
	v := openOK(t, p)
	code, c := postJSON[closeView](t, p, "close", "/api/close", closeRequest{Symbol: "BTCUSDT", IntentID: v.IntentID})
	if code != http.StatusOK || !c.Flat {
		t.Fatalf("close = %d %+v", code, c)
	}
	st, err := loadState(p.stateDir, v.IntentID)
	if err != nil {
		t.Fatal(err)
	}
	if st.CloseReasonVI != "Đóng thủ công bởi người vận hành" {
		t.Errorf("a close with no reason = %q", st.CloseReasonVI)
	}

	// A close that names its reason keeps that one, verbatim: the bot's
	// take-profit sentence travels from assessExit to this file unchanged.
	const botReason = "Chốt lời hội tụ Basis: Net PnL +0.62% trên vốn ≥ ngưỡng +0.50%"
	v = openOK(t, p)
	code, c = postJSON[closeView](t, p, "close", "/api/close", closeRequest{Symbol: "BTCUSDT", IntentID: v.IntentID, ReasonVI: botReason})
	if code != http.StatusOK || !c.Flat {
		t.Fatalf("close = %d %+v", code, c)
	}
	if st, _ = loadState(p.stateDir, v.IntentID); st.CloseReasonVI != botReason {
		t.Errorf("reason = %q, want %q", st.CloseReasonVI, botReason)
	}

	// And the result page carries it to the history table. That page lists the
	// BOT's intents, so the record is one of those.
	const botIntent = "abtcusdt-20260916-000000-001"
	opened := time.Now().Add(-24 * time.Hour)
	if err := saveState(p.stateDir, intentState{
		IntentID: botIntent, Symbol: "BTCUSDT", Outcome: "both_open", NotionalQuote: 65,
		OpenedAtMs: opened.UnixMilli(), ClosedAtMs: opened.Add(12 * time.Hour).UnixMilli(),
		PerpFilledQtyCoin: 0.0008, ClosedQtyCoin: 0.0008, CloseReasonVI: botReason,
	}); err != nil {
		t.Fatal(err)
	}
	status := p.autotrade.Status()
	status.Portfolio.TotalCapitalCapQuote = 1000
	pnl := p.buildPnL(context.Background(), status, false)
	found, listed := "", false
	for _, tr := range pnl.Trades {
		if tr.IntentID == botIntent {
			found, listed = tr.CloseReasonVI, true
		}
	}
	if !listed || found != botReason {
		t.Errorf("the page shows %q for %s (listed %v), want %q · trades %+v", found, botIntent, listed, botReason, pnl.Trades)
	}
}

// ------------------------------------------------ audit R6: pre-flight spread

// A take-profit close re-reads both books and is DEFERRED when a touch is wider
// than its ceiling: 15 bps against 10 sends nothing and leaves the intent file
// as it was. A risk close on the very same book goes out.
func TestActions_ATakeProfitCloseIsDeferredOnAWideLiveSpreadAndARiskCloseIsNot(t *testing.T) {
	p, spot, perp := fakePortal(t)
	v := openOK(t, p)
	spot.book = fakeBookWithSpread(fakeMidQuote, 15)
	st, err := loadState(p.stateDir, v.IntentID)
	if err != nil {
		t.Fatal(err)
	}
	before := orderCount(spot, perp)

	tp, code := p.close(context.Background(), st, "Chốt lời hội tụ Basis: Net PnL +1.62% trên vốn ≥ ngưỡng +1.50%", closeGuard{MaxSpreadBps: 10})
	if !tp.Deferred || !tp.Refused || tp.Flat || code != http.StatusConflict || orderCount(spot, perp) != before {
		t.Fatalf("take-profit on a 15 bps book = %d %+v, orders %d → %d", code, tp, before, orderCount(spot, perp))
	}
	if !strings.Contains(tp.ErrorVI, "Spread sổ lệnh tức thời bị giãn (Spot 15.0 bps") || !strings.Contains(tp.ErrorVI, "trần 10.0 bps") {
		t.Errorf("reason = %q", tp.ErrorVI)
	}
	if after, _ := loadState(p.stateDir, v.IntentID); after.ClosedAtMs != 0 || after.NoteVI != "" || !after.tracked() {
		t.Errorf("a deferred close touched the intent file: %+v", after)
	}

	// The perp's touch widening defers it just the same.
	spot.book = fakeBook()
	perp.book = fakeBookWithSpread(fakePerpMidQuote, 15)
	if tp, _ = p.close(context.Background(), st, "Chốt lời", closeGuard{MaxSpreadBps: 10}); !tp.Deferred || !strings.Contains(tp.ErrorVI, "Perp 15.0 bps") {
		t.Fatalf("take-profit on a 15 bps perp book = %+v", tp)
	}

	// The basis stop on the same 15 bps book: no guard, sent, flat.
	stop, code := p.close(context.Background(), st, "Cắt lỗ basis nổ: Basis giãn +110.0 bps > 100 bps so với lúc vào", closeGuard{})
	if code != http.StatusOK || !stop.Flat || stop.Deferred || stop.Refused {
		t.Fatalf("basis stop on a 15 bps book = %d %+v", code, stop)
	}
}

// A touch the guard cannot measure defers a take-profit: two MARKET orders are
// not sent into a book nobody could read.
func TestCloseGuard_AnUnmeasurableTouchDefersAndATightOneSends(t *testing.T) {
	g := closeGuard{MaxSpreadBps: 10}
	tight := depth.Summary{MidPriceQuote: 100, BestBidQuote: 99.995, BestAskQuote: 100.005} // 1 bps
	for name, c := range map[string]struct {
		spot, perp depth.Summary
		deferred   bool
		fragment   string
	}{
		"both tight":         {tight, tight, false, ""},
		"exactly the cap":    {depth.Summary{MidPriceQuote: 100, BestBidQuote: 99.95, BestAskQuote: 100.05}, tight, false, ""},
		"spot 15 bps":        {depth.Summary{MidPriceQuote: 100, BestBidQuote: 99.925, BestAskQuote: 100.075}, tight, true, "giãn"},
		"no spot bid":        {depth.Summary{MidPriceQuote: 100, BestAskQuote: 100.005}, tight, true, "không đo được"},
		"crossed perp touch": {tight, depth.Summary{MidPriceQuote: 100, BestBidQuote: 100.01, BestAskQuote: 99.99}, true, "không đo được"},
	} {
		why := g.deferVI(c.spot, c.perp)
		if (why != "") != c.deferred || !strings.Contains(why, c.fragment) {
			t.Errorf("%s: deferVI = %q, want deferred %v", name, why, c.deferred)
		}
	}
	if why := (closeGuard{}).deferVI(depth.Summary{}, depth.Summary{}); why != "" {
		t.Errorf("no guard deferred on %q", why)
	}
}

// PLAN 4.5j, second half, through the page on a Bybit profile: the account's
// spot fee reaches execution, which buys the spot leg grossed up so the WALLET
// holds the perp; the hedge status counts that buy net of the base-coin fee its
// own fills state, so a 20k pair reads HEDGED rather than 0.00026 BTC long; and
// the close sells the perp's size and ends flat on both legs.
//
// This size is the one the first half of 4.5j refused (its 0.00026 BTC fee gap
// is past the 0.0001 tolerance).
func TestActions_BybitOpenStatusCloseWithTheSpotFeeInTheBaseCoin(t *testing.T) {
	p, spot, perp := fakePortal(t)
	p.markets.profile = profileFor(venueBybit)
	// Bybit's adapter answers an id nobody sent as NOT VISIBLE, never as not
	// found — the case review 4.5j part 2 (N1) found every held intent's close,
	// unwind, reconcile and reduce ids in.
	spot.notVisible, perp.notVisible = true, true
	clock := time.Now()
	p.now = func() time.Time { return clock }
	spot.spotTakerFrac = 0.001 // Bybit's measured spot taker, charged in BTC on a buy
	spot.SetBehaviour(brokertest.Behaviour{FillFractionOnPlace: 1, SpotBuyFeeInBaseFrac: 0.001, RefuseSpotSellBeyondBalance: true})
	walletBefore := spotBTC(t, spot)

	code, v := postJSON[openView](t, p, "open", "/api/open", openRequest{Symbol: "BTCUSDT", NotionalQuote: 20_000})
	if code != http.StatusOK || !v.Hedged || v.Outcome != "both_open" || v.Alarm || v.ReducedToMatch {
		t.Fatalf("a 20k open with a 0.1%% base-coin fee = %d %+v", code, v)
	}
	if !near(v.Perp.FilledQtyCoin, 0.2597) || !near(v.Spot.FilledQtyCoin, 0.25996) {
		t.Errorf("perp %v (want 0.2597), spot order %v (want 0.25996 = 0.2597 ÷ 0.999 rounded up)", v.Perp.FilledQtyCoin, v.Spot.FilledQtyCoin)
	}
	gained := spotBTC(t, spot) - walletBefore
	if gained < 0.2597-1e-9 || gained-0.2597 >= spotRulesBTC.StepSizeCoin {
		t.Errorf("the wallet gained %v beside a 0.2597 perp", gained)
	}

	st, err := loadState(p.stateDir, v.IntentID)
	if err != nil || !st.SpotBuyBaseFeeStated || math.Abs(st.SpotBuyBaseFeeQtyCoin-0.00025996) > 1e-12 {
		t.Fatalf("intent file fee = %v stated %v (%v), want the fills' 0.00025996", st.SpotBuyBaseFeeQtyCoin, st.SpotBuyBaseFeeStated, err)
	}
	// Review part 2, B1: the pair outlives the venue's fill list. From the
	// stored fee the status and the close still read the wallet's leg.
	spot.tradesErr = errors.New("execution list: no fills in the default 7-day window")
	p.memo.forget()

	// Inside the grace after a write, an order the venue does not list may
	// still be in flight: the pair is UNKNOWN, not hedged and not flat.
	if pos := getJSON[positionsView](t, p, "/api/positions?symbol=BTCUSDT"); pos.Status != statusUnknown {
		t.Errorf("positions right after the open = %s (%s), want unknown while unlisted ids may be in flight", pos.Status, pos.ReasonVI)
	}
	clock = clock.Add(notVisibleGrace + time.Second)
	p.invalidateVenueReads() // the view cache runs on its own clock

	pos := getJSON[positionsView](t, p, "/api/positions?symbol=BTCUSDT")
	if pos.Status != statusBothOpen || !near(pos.SpotQtyCoin, gained) || math.Abs(pos.DeltaResidualCoin) >= spotRulesBTC.StepSizeCoin {
		t.Errorf("positions after open = %s spot %v (wallet gained %v) delta %v (%s)", pos.Status, pos.SpotQtyCoin, gained, pos.DeltaResidualCoin, pos.ReasonVI)
	}

	code, c := postJSON[closeView](t, p, "close", "/api/close", closeRequest{Symbol: "BTCUSDT", IntentID: v.IntentID})
	if code != http.StatusOK || !c.Flat || c.Refused || c.Alarm || c.SentUnconfirmed {
		t.Fatalf("close = %d %+v", code, c)
	}
	clock = clock.Add(notVisibleGrace + time.Second)
	p.invalidateVenueReads()
	if left := spotBTC(t, spot) - walletBefore; left < 0 || left >= spotRulesBTC.StepSizeCoin {
		t.Errorf("the wallet kept %v BTC of this pair after a flat close; want under one spot step", left)
	}
	if pos := getJSON[positionsView](t, p, "/api/positions?symbol=BTCUSDT"); pos.Status != statusBothFlat {
		t.Errorf("positions after close = %s (%s)", pos.Status, pos.ReasonVI)
	}
	if n := len(perp.Orders()); n != 2 {
		t.Errorf("%d perp orders, want the open and the close", n)
	}
}

func spotBTC(t *testing.T, v *fakeVenue) float64 {
	t.Helper()
	balances, err := v.GetBalance(context.Background(), broker.MarketSpot)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range balances {
		if b.Asset == "BTC" {
			return b.TotalQtyCoin()
		}
	}
	return 0
}

// badFills answers a spot buy's fills the ways that leave its base-coin fee
// unknown.
type badFills struct {
	*brokertest.Fake
	trades []broker.Trade
}

func (b badFills) OrderTrades(context.Context, broker.OrderQuery) ([]broker.Trade, error) {
	return b.trades, nil
}

// On a venue that keeps the fee in the base coin, a spot buy whose fills do not
// say what they charged — or do not add up to the order — leaves the leg
// UNKNOWN, never counted gross, which would read a hedged pair as long by the
// fee.
func TestHedge_ABaseCoinFeeThatCannotBeReadLeavesTheSpotLegUnknown(t *testing.T) {
	ctx := context.Background()
	spotFake, perp := brokertest.New(), brokertest.New()
	spotFake.SetBaseAsset("BTC")
	openPair(t, spotFake, perp, testIntent, 0.0008)
	for name, trades := range map[string][]broker.Trade{
		"a fee in no named asset":  {{TradeID: "1", QtyCoin: 0.0008, CommissionQtyInAsset: 0.0000008}},
		"fills short of the order": {{TradeID: "1", QtyCoin: 0.0004, CommissionQtyInAsset: 0.0000004, CommissionAsset: "BTC"}},
	} {
		h := readIntentHedges(ctx, badFills{spotFake, trades}, perp, newDoneOrders(), "BTCUSDT", []string{testIntent}, hedgeFees{BaseAsset: "BTC"})[0]
		if len(h.unreadable()) == 0 {
			t.Errorf("%s: summed anyway, spot %v", name, h.Spot.QtyCoin)
		}
	}
	// A plain venue with no fee kept in the base coin reads exactly as before,
	// and one that states its fee nets it out.
	h := readIntentHedges(ctx, spotFake, perp, newDoneOrders(), "BTCUSDT", []string{testIntent}, hedgeFees{})[0]
	if len(h.unreadable()) != 0 || !near(h.Spot.QtyCoin, 0.0008) {
		t.Errorf("Binance path: spot %v, unreadable %v", h.Spot.QtyCoin, h.unreadable())
	}
	stated := []broker.Trade{{TradeID: "1", QtyCoin: 0.0008, CommissionQtyInAsset: 0.0000008, CommissionAsset: "BTC"}}
	h = readIntentHedges(ctx, badFills{spotFake, stated}, perp, newDoneOrders(), "BTCUSDT", []string{testIntent}, hedgeFees{BaseAsset: "BTC"})[0]
	if len(h.unreadable()) != 0 || !near(h.Spot.QtyCoin, 0.0007992) {
		t.Errorf("stated fee: spot %v, unreadable %v", h.Spot.QtyCoin, h.unreadable())
	}
}

// Review 4.5j part 4, R2: on Bybit a reconcile's own read-back, taken inside the
// write right after its squaring order, must see that order and read the
// intent's never-sent ids as absent — or every successful square reports
// "still unbalanced".
func TestActions_BybitReconcileReadsItsOwnSquareAsBalanced(t *testing.T) {
	p, spot, perp := fakePortal(t)
	p.markets.profile = profileFor(venueBybit)
	spot.notVisible, perp.notVisible = true, true
	clock := time.Now()
	p.now = func() time.Time { return clock }

	v := openOK(t, p)
	place(t, spot, broker.MarketSpot, broker.SideSell, execution.CloseClientOrderID(v.IntentID, execution.LegSpot), 0.0008)
	clock = clock.Add(notVisibleGrace + time.Second)
	p.invalidateVenueReads()

	code, plan := postJSON[reconcileView](t, p, "reconcile", "/api/reconcile", reconcileRequest{Symbol: "BTCUSDT"})
	if code != http.StatusOK || plan.ToSend != 1 {
		t.Fatalf("dry run = %d %+v", code, plan)
	}
	code, r := postJSON[reconcileView](t, p, "reconcile", "/api/reconcile", reconcileRequest{Symbol: "BTCUSDT", Apply: true, PlanDigest: plan.PlanDigest})
	if code != http.StatusOK || len(r.Results) != 1 || !r.Results[0].Balanced {
		t.Fatalf("apply = %d %+v — a successful square read as still unbalanced", code, r)
	}
}

// hideOpen answers the intent's OPENING spot order as Bybit does once it has
// left the order lists — not visible.
type hideOpen struct {
	*brokertest.Fake
	id string
}

func (h hideOpen) GetOrder(ctx context.Context, q broker.OrderQuery) (broker.Order, error) {
	if q.ClientOrderID == h.id {
		return broker.Order{}, fmt.Errorf("%w: gone from both lists", bybitbroker.ErrOrderNotVisible)
	}
	o, err := h.Fake.GetOrder(ctx, q)
	if errors.Is(err, broker.ErrOrderNotFound) {
		return broker.Order{}, fmt.Errorf("%w: neither list shows it", bybitbroker.ErrOrderNotVisible)
	}
	return o, err
}

// Review 4.5j part 4, R1: a FILLED opening order the venue no longer lists is
// never read as absent, even for an intent long settled. Read as 0 it would
// make a held pair look like a naked perp short, and LÀM PHẲNG would buy the
// perp back off a real spot long.
func TestHedge_AnUnlistedOpeningOrderIsUnknownNeverAbsent(t *testing.T) {
	ctx := context.Background()
	spot, perp := brokertest.New(), brokertest.New()
	openPair(t, spot, perp, testIntent, 0.0008)
	fees := hedgeFees{NotVisibleIsAbsent: map[string]bool{testIntent: true}}
	h := readIntentHedges(ctx, hideOpen{spot, execution.LegClientOrderID(testIntent, execution.LegSpot)},
		hideOpen{perp, "-"}, newDoneOrders(), "BTCUSDT", []string{testIntent}, fees)[0]
	if len(h.Spot.UnreadableVI) == 0 {
		t.Fatalf("an unlisted filled open read as spot %v — absent, not unknown", h.Spot.QtyCoin)
	}
	if len(h.Perp.UnreadableVI) != 0 || !near(h.Perp.QtyCoin, -0.0008) {
		t.Errorf("perp: %v unreadable %v — its never-sent ids should read absent on a settled intent", h.Perp.QtyCoin, h.Perp.UnreadableVI)
	}
	if p := planSquare(h, spotRulesBTC, perpRulesBTC, fakeMidQuote, fakeMidQuote); p.Action == "send" {
		t.Errorf("reconcile would send %s %s %v on an unreadable open", p.Market, p.Side, p.QtyCoin)
	}
}
