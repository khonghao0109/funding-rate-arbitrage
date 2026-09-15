package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/cmd/execportal/autotrade"
	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
)

// The auto-trader through the portal's real handler and its real open and
// close, on brokertest's venues (PLAN Q18). What these prove beyond package
// autotrade's own tests is the WIRING: the bot's order is the portal's order —
// same write lock, same intent files, same one-position rule, same venue
// read-back — and its three writes sit behind the same walls as every other.

// botPortal is fakePortal with a week of settled funding at 1 bps per 8h, a
// forming rate of 1 bps and the next settlement seven hours away — one interval
// after the last settled one.
func botPortal(t *testing.T) (*portal, *fakeVenue, *fakeVenue) {
	t.Helper()
	p, spot, perp := fakePortal(t)
	now := time.Now()
	// The next stamp one 8h interval after the last settled one, as the venue
	// publishes it.
	perp.SetMarkPrice(broker.MarkPrice{MarkPriceQuote: fakeMidQuote, LastFundingRateFrac: 0.0001, NextFundingTimeMs: now.Add(7 * time.Hour).UnixMilli()})
	for i := 20; i >= 0; i-- {
		perp.fundingRates = append(perp.fundingRates, binancebroker.FundingRate{
			SettledAtMs: now.Add(-time.Hour - time.Duration(i)*8*time.Hour).UnixMilli(), RatePerIntervalFrac: 0.0001})
	}
	return p, spot, perp
}

func botStatus(t *testing.T, p *portal) autotrade.StatusView {
	t.Helper()
	return getJSON[autotrade.StatusView](t, p, "/api/autotrade/status")
}

func TestAutotradeAPI_EveryWriteIsBehindTheWalls(t *testing.T) {
	p, spot, perp := botPortal(t)
	for path, body := range map[string]string{
		"/api/autotrade/start": `{"symbol":"BTCUSDT"}`,
		"/api/autotrade/stop":  `{"close_now":true}`,
		"/api/autotrade/kill":  `{}`,
	} {
		// The READ header on a write, and a write's header naming another action.
		for name, opts := range map[string][]reqOpt{
			"no action header":   {withHeader("Content-Type", "application/json")},
			"the read header":    {withHeader("Content-Type", "application/json"), withHeader(actionHeader, readAction)},
			"another write name": writeOpts("open"),
			"a foreign origin":   append(writeOpts(strings.Replace(strings.TrimPrefix(path, "/api/"), "/", "-", 1)), withHeader("Origin", "http://evil.example")),
		} {
			if rec := do(t, p, http.MethodPost, path, body, opts...); rec.Code != http.StatusForbidden {
				t.Errorf("%s with %s = %d %s", path, name, rec.Code, rec.Body.String())
			}
		}
		if rec := do(t, p, http.MethodGet, path, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d", path, rec.Code)
		}
	}
	if rec := do(t, p, http.MethodGet, "/api/autotrade/status", "", withHeader(actionHeader, "")); rec.Code != http.StatusForbidden {
		t.Errorf("status without the read header = %d", rec.Code)
	}
	// A typo in a field of an order-switching request is an error, not a default.
	if rec := do(t, p, http.MethodPost, "/api/autotrade/start", `{"symbol":"BTCUSDT","notional":65}`, writeOpts(autotradeStartAction)...); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d", rec.Code)
	}
	if rec := do(t, p, http.MethodPost, "/api/autotrade/start", `{"symbol":"DOGEUSDT"}`, writeOpts(autotradeStartAction)...); rec.Code != http.StatusBadRequest {
		t.Errorf("a symbol off the list = %d", rec.Code)
	}
	if rec := do(t, p, http.MethodPost, "/api/autotrade/start", `{"symbol":"BTCUSDT","notional_quote":90000}`, writeOpts(autotradeStartAction)...); rec.Code != http.StatusBadRequest {
		t.Errorf("a notional over the portal's cap = %d", rec.Code)
	}
	// The no-dialog stop name cannot carry a close, and the close name cannot
	// be sent for a plain stop.
	for body, action := range map[string]string{`{"close_now":true}`: autotradeStopAction, `{"close_now":false}`: autotradeStopCloseAction} {
		if rec := do(t, p, http.MethodPost, "/api/autotrade/stop", body, writeOpts(action)...); rec.Code != http.StatusForbidden || errorCode(t, rec) != "action_header_mismatch" {
			t.Errorf("stop %s under %s = %d %s", body, action, rec.Code, rec.Body.String())
		}
	}
	if st := botStatus(t, p); st.State != autotrade.StateDisabled || orderCount(spot, perp) != 0 {
		t.Errorf("after refused writes: %s, %d orders", st.State, orderCount(spot, perp))
	}
}

func TestAutotradeAPI_WithoutCredentialsTheBotDoesNotStart(t *testing.T) {
	p := testPortal(t, false)
	rec := do(t, p, http.MethodPost, "/api/autotrade/start", `{"symbol":"BTCUSDT"}`, writeOpts(autotradeStartAction)...)
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != "no_credentials" {
		t.Errorf("start without credentials = %d %s", rec.Code, rec.Body.String())
	}
	if st := botStatus(t, p); st.State != autotrade.StateDisabled {
		t.Errorf("state = %s", st.State)
	}
}

// The acceptance path of Q18 on fakes, through the page's own endpoints: switch
// on, the bot opens through openAs under an autotrade intent id, the banner and
// the intent history see it, a manual open is refused by the one-position rule,
// KILL flattens both legs at the venue and halts, and a halted bot starts again
// only after the operator acknowledges.
func TestAutotradeAPI_StartOpensThroughThePortalAndKillFlattens(t *testing.T) {
	p, spot, perp := botPortal(t)
	code, started := postJSON[autotradeActionView](t, p, autotradeStartAction, "/api/autotrade/start",
		map[string]any{"symbol": "BTCUSDT", "notional_quote": 65, "min_net_apr_pct": 5, "max_hold_epochs": 0})
	if code != http.StatusOK || started.Status.State != autotrade.StateIdleScanning || started.Status.Config.NotionalQuote != 65 {
		t.Fatalf("start = %d %+v", code, started.Status)
	}

	p.autotrade.Step(context.Background())
	st := botStatus(t, p)
	if st.State != autotrade.StateInPosition || st.Position == nil {
		t.Fatalf("after one scan: %s · halt %q · signal %+v · log %+v", st.State, st.HaltReasonVI, st.Signal, st.Log)
	}
	if !strings.HasPrefix(st.Position.IntentID, "abtcusdt-") || st.Signal == nil || st.Signal.PerpTakerFeeBps != 4 || st.Signal.SpotTakerFeeBps != 0 {
		t.Errorf("position %+v, fees %v/%v", st.Position, st.Signal.SpotTakerFeeBps, st.Signal.PerpTakerFeeBps)
	}
	pos := getJSON[positionsView](t, p, "/api/positions?symbol=BTCUSDT")
	if pos.Status != statusBothOpen || !near(pos.DeltaResidualCoin, 0) || !near(pos.PerpQtyCoin, -0.0008) {
		t.Fatalf("banner after the bot's open = %s delta %v perp %v (%s)", pos.Status, pos.DeltaResidualCoin, pos.PerpQtyCoin, pos.ReasonVI)
	}
	intents := getJSON[intentsView](t, p, "/api/intents?symbol=BTCUSDT")
	if len(intents.Intents) != 1 || intents.Intents[0].Origin != "autotrade" || intents.Intents[0].SpotRefMidQuote <= 0 {
		t.Errorf("intent history = %+v", intents.Intents)
	}

	// A button press while the bot holds the symbol: the one-position rule.
	before := orderCount(spot, perp)
	if code, o := postJSON[openView](t, p, "open", "/api/open", openRequest{Symbol: "BTCUSDT", NotionalQuote: 65}); code != http.StatusConflict || !strings.Contains(o.ErrorVI, "MỘT vị thế") {
		t.Errorf("manual open beside the bot = %d %q", code, o.ErrorVI)
	}
	if orderCount(spot, perp) != before {
		t.Error("the refused manual open placed orders")
	}

	code, killed := postJSON[autotradeActionView](t, p, autotradeKillAction, "/api/autotrade/kill", map[string]any{})
	if code != http.StatusOK || killed.Status.State != autotrade.StateEmergencyHalted || killed.Close == nil || !killed.Close.Flat {
		t.Fatalf("kill = %d %+v close %+v", code, killed.Status, killed.Close)
	}
	venuePos, err := perp.GetPosition(context.Background(), broker.MarketFuturesUSDM, "BTCUSDT")
	if err != nil || !venuePos.Flat() {
		t.Fatalf("the venue still holds perp %v after the kill (%v)", venuePos.QtyCoin, err)
	}
	if pos := getJSON[positionsView](t, p, "/api/positions?symbol=BTCUSDT"); pos.Status != statusBothFlat {
		t.Errorf("banner after the kill = %s (%s)", pos.Status, pos.ReasonVI)
	}

	rec := do(t, p, http.MethodPost, "/api/autotrade/start", `{"symbol":"BTCUSDT"}`, writeOpts(autotradeStartAction)...)
	if rec.Code != http.StatusConflict {
		t.Errorf("start while halted = %d %s", rec.Code, rec.Body.String())
	}
	if code, stopped := postJSON[autotradeActionView](t, p, autotradeStopAction, "/api/autotrade/stop", map[string]any{"close_now": false}); code != http.StatusOK || stopped.Status.State != autotrade.StateDisabled {
		t.Fatalf("acknowledge = %d %+v", code, stopped.Status)
	}
	if rec := do(t, p, http.MethodPost, "/api/autotrade/start", `{"symbol":"BTCUSDT"}`, writeOpts(autotradeStartAction)...); rec.Code != http.StatusOK {
		t.Errorf("start after the acknowledgement = %d %s", rec.Code, rec.Body.String())
	}
}

// A manual write in flight holds the portal's lock: the bot sends nothing, says
// so, and opens on the next scan.
func TestAutotradeAPI_TheBotWaitsForAManualWrite(t *testing.T) {
	p, spot, perp := botPortal(t)
	if _, err := p.autotrade.Start(autotrade.DefaultConfig("BTCUSDT")); err != nil {
		t.Fatal(err)
	}
	release, _, _, ok := p.acquire("reconcile")
	if !ok {
		t.Fatal("could not take the lock")
	}
	p.autotrade.Step(context.Background())
	st := botStatus(t, p)
	if st.State == autotrade.StateInPosition || orderCount(spot, perp) != 0 || st.TradeFailures != 0 {
		t.Fatalf("with the lock held: %s, %d orders, %d failures", st.State, orderCount(spot, perp), st.TradeFailures)
	}
	if !strings.Contains(st.Log[0].MessageVI, "reconcile") {
		t.Errorf("the console does not say who held the lock: %+v", st.Log[0])
	}
	release()
	p.autotrade.Step(context.Background())
	if st := botStatus(t, p); st.State != autotrade.StateInPosition {
		t.Errorf("after the lock was released: %s", st.State)
	}
}

// A position a PERSON opened on the page is never adopted by the bot, never
// counted as its own, and never closed by its kill switch.
func TestAutotradeAPI_AManualPositionIsNeverTheBots(t *testing.T) {
	p, spot, perp := botPortal(t)
	manual := openOK(t, p)
	if !strings.HasPrefix(manual.IntentID, "pbtcusdt-") {
		t.Fatalf("manual intent id %q", manual.IntentID)
	}
	if _, err := p.autotrade.Start(autotrade.DefaultConfig("BTCUSDT")); err != nil {
		t.Fatal(err)
	}
	p.autotrade.Step(context.Background())
	st := botStatus(t, p)
	if st.State != autotrade.StateIdleScanning || st.Position != nil {
		t.Fatalf("beside a manual position: %s, position %+v", st.State, st.Position)
	}
	before := orderCount(spot, perp)
	code, killed := postJSON[autotradeActionView](t, p, autotradeKillAction, "/api/autotrade/kill", map[string]any{})
	if code != http.StatusOK || killed.Close == nil || killed.Close.Attempted || killed.Close.Flat {
		t.Fatalf("kill beside a manual position = %d %+v", code, killed.Close)
	}
	if orderCount(spot, perp) != before {
		t.Error("the kill switch traded a position a person opened")
	}
	if pos := getJSON[positionsView](t, p, "/api/positions?symbol=BTCUSDT"); pos.Status != statusBothOpen {
		t.Errorf("manual position after the kill = %s", pos.Status)
	}
}

// A second intent holding the symbol beside the bot's (a person's, seeded on the
// venue): the portal's hedge reading names no single intent, and the bot halts
// without closing anything.
func TestAutotradeAPI_ASecondHeldIntentHaltsTheBot(t *testing.T) {
	p, spot, perp := botPortal(t)
	if _, err := p.autotrade.Start(autotrade.DefaultConfig("BTCUSDT")); err != nil {
		t.Fatal(err)
	}
	p.autotrade.Step(context.Background())
	if st := botStatus(t, p); st.State != autotrade.StateInPosition {
		t.Fatalf("bot did not open: %s", st.State)
	}
	seedIntent(t, p, spot, perp, "pbtcusdt-20260915-000000-001", 0.0008, 0.0008)
	before := orderCount(spot, perp)
	p.autotrade.Step(context.Background())
	st := botStatus(t, p)
	if st.State != autotrade.StateEmergencyHalted || !strings.Contains(st.HaltReasonVI, "2 ý định") || orderCount(spot, perp) != before {
		t.Errorf("beside a second intent: %s %q, orders %d → %d", st.State, st.HaltReasonVI, before, orderCount(spot, perp))
	}
}

// The history is read to its newest settlement even when the venue answers in
// short pages, and filtered to the window asked for.
func TestPortalMarket_PagesToTheNewestSettlementAndFiltersTheWindow(t *testing.T) {
	p, _, perp := botPortal(t)
	perp.fundingPageRows = 4
	for _, v := range []*fakeVenue{p.markets.spot.(*fakeVenue), perp} {
		if _, err := v.HTTP().SyncClock(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	snap, err := portalMarket{p}.Snapshot(context.Background(), "BTCUSDT", now.Add(-7*24*time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Settled) != 21 || snap.Settled[20].SettledAtMs != perp.fundingRates[20].SettledAtMs || snap.SettledErrVI != "" {
		t.Fatalf("read %d settlements, newest %d (want 21, %d) %q", len(snap.Settled), snap.Settled[len(snap.Settled)-1].SettledAtMs, perp.fundingRates[20].SettledAtMs, snap.SettledErrVI)
	}
	if snap.SpotTakerFeeBps != 0 || snap.PerpTakerFeeBps != 4 || snap.SpotClockSkewMs == nil || snap.PerpClockSkewMs == nil {
		t.Errorf("fees %v/%v, clocks %v/%v", snap.SpotTakerFeeBps, snap.PerpTakerFeeBps, snap.SpotClockSkewMs, snap.PerpClockSkewMs)
	}
	// A settlement happens: the venue lists it and moves its next stamp. The
	// cached history is keyed by that stamp, so the next scan sees it at once
	// rather than after the cache's two minutes.
	settledNow := now.Add(time.Minute).UnixMilli()
	perp.fundingRates = append(perp.fundingRates, binancebroker.FundingRate{SettledAtMs: settledNow, RatePerIntervalFrac: -0.0002})
	perp.SetMarkPrice(broker.MarkPrice{MarkPriceQuote: fakeMidQuote, LastFundingRateFrac: 0.0001, NextFundingTimeMs: settledNow + (8 * time.Hour).Milliseconds()})
	p.now = func() time.Time { return now.Add(2 * time.Minute) }
	snap, err = portalMarket{p}.Snapshot(context.Background(), "BTCUSDT", now.Add(-7*24*time.Hour).UnixMilli())
	if err != nil || len(snap.Settled) != 22 || snap.Settled[21].SettledAtMs != settledNow {
		t.Fatalf("after a settlement: %d rows, err %v", len(snap.Settled), err)
	}
	// A window starting one millisecond after a settlement: the read is
	// rounded down to the hour, so the venue returns that settlement too, and
	// the window must still leave it out.
	since := perp.fundingRates[14].SettledAtMs + 1
	snap, err = portalMarket{p}.Snapshot(context.Background(), "BTCUSDT", since)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range snap.Settled {
		if r.SettledAtMs < since {
			t.Fatalf("a settlement before the window: %d < %d", r.SettledAtMs, since)
		}
	}
	if len(snap.Settled) != 7 {
		t.Errorf("settlements after index 14 = %d rows, want 7 (6 of the week + the one just settled)", len(snap.Settled))
	}
}

// A venue that ignores startTime and pages back the same oldest rows: the
// newest settlements never arrive, and the history is refused as unreadable
// rather than used short.
func TestPortalMarket_AVenueThatRepeatsItsPageIsRefused(t *testing.T) {
	p, _, perp := botPortal(t)
	perp.fundingPageRows, perp.fundingIgnoresStart = 4, true
	snap, err := portalMarket{p}.Snapshot(context.Background(), "BTCUSDT", time.Now().Add(-7*24*time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if snap.SettledErrVI == "" || len(snap.Settled) != 0 {
		t.Errorf("a repeating venue = %d rows, err %q", len(snap.Settled), snap.SettledErrVI)
	}
}

// Fees the account could not read price nothing, and the bot opens nothing.
func TestAutotradeAPI_UnreadFeesOpenNothing(t *testing.T) {
	p, spot, perp := botPortal(t)
	spot.feesErr = broker.ErrNoCredentials
	if _, err := p.autotrade.Start(autotrade.DefaultConfig("BTCUSDT")); err != nil {
		t.Fatal(err)
	}
	p.autotrade.Step(context.Background())
	st := botStatus(t, p)
	if st.State != autotrade.StateIdleScanning || orderCount(spot, perp) != 0 || st.Signal == nil || st.Signal.NetAPRPct != nil ||
		!strings.Contains(st.Signal.NetAPRReasonVI, "phí") {
		t.Errorf("with unread fees: %s, %d orders, signal %+v", st.State, orderCount(spot, perp), st.Signal)
	}
}
