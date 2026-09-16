package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"slices"
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
// same write lock, same intent files, same one-position-per-symbol rule, same
// venue read-back — and its writes sit behind the same walls as every other.

// botPortal is fakePortal with a week of settled funding at 1 bps per 8h, a
// forming rate of 1 bps and the next settlement seven hours away — one interval
// after the last settled one.
// fixedSizePortfolio is a run whose slot size is the one the test typed: the
// shipped default re-sizes from the account on its first scan (capital.go), and
// a test asserting a quantity has to say which size it means.
func fixedSizePortfolio(symbols ...string) autotrade.PortfolioConfig {
	pc := autotrade.DefaultPortfolioConfig(symbols)
	pc.AutoRebalance = false
	pc.DefaultPairConfig.NotionalQuote = 200
	return pc
}

func botPortal(t *testing.T) (*portal, *fakeVenue, *fakeVenue) {
	t.Helper()
	p, spot, perp := fakePortal(t)
	now := time.Now()
	// The next stamp one 8h interval after the last settled one, as the venue
	// publishes it.
	perp.SetMarkPrice(broker.MarkPrice{MarkPriceQuote: fakePerpMidQuote, LastFundingRateFrac: 0.0001, NextFundingTimeMs: now.Add(7 * time.Hour).UnixMilli()})
	for i := 20; i >= 0; i-- {
		perp.fundingRates = append(perp.fundingRates, binancebroker.FundingRate{
			SettledAtMs: now.Add(-time.Hour - time.Duration(i)*8*time.Hour).UnixMilli(), RatePerIntervalFrac: 0.0001})
	}
	return p, spot, perp
}

// multiBotPortal is botPortal trading several symbols on the same fake venues:
// each symbol has its own perp position there, and the rules and book are the
// fake's for every one of them.
func multiBotPortal(t *testing.T, symbols ...string) (*portal, *fakeVenue, *fakeVenue) {
	t.Helper()
	p, spot, perp := botPortal(t)
	p.symbols = symbols
	p.autotrade = newAutotrade(p)
	return p, spot, perp
}

func botStatus(t *testing.T, p *portal) autotrade.StatusView {
	t.Helper()
	return getJSON[autotrade.StatusView](t, p, "/api/autotrade/status")
}

func botPair(t *testing.T, st autotrade.StatusView, symbol string) autotrade.PairView {
	t.Helper()
	for _, pv := range st.Pairs {
		if pv.Symbol == symbol {
			return pv
		}
	}
	t.Fatalf("no pair %s", symbol)
	return autotrade.PairView{}
}

func perpQty(t *testing.T, perp *fakeVenue, symbol string) float64 {
	t.Helper()
	pos, err := perp.GetPosition(context.Background(), broker.MarketFuturesUSDM, symbol)
	if err != nil {
		t.Fatal(err)
	}
	return pos.QtyCoin
}

func TestAutotradeAPI_EveryWriteIsBehindTheWalls(t *testing.T) {
	p, spot, perp := botPortal(t)
	for path, c := range map[string]struct{ body, action string }{
		"/api/autotrade/start":      {`{"symbols":["BTCUSDT"],"notional_quote":200,"auto_rebalance":false}`, autotradeStartAction},
		"/api/autotrade/stop":       {`{"close_now":true}`, autotradeStopCloseAction},
		"/api/autotrade/kill":       {`{}`, autotradeKillAction},
		"/api/autotrade/close-pair": {`{"symbol":"BTCUSDT"}`, autotradeClosePairAction},
		"/api/autotrade/pair":       {`{"symbol":"BTCUSDT","action":"resume"}`, autotradePairResumeAction},
	} {
		// The READ header on a write, and a write's header naming another action.
		for name, opts := range map[string][]reqOpt{
			"no action header":   {withHeader("Content-Type", "application/json")},
			"the read header":    {withHeader("Content-Type", "application/json"), withHeader(actionHeader, readAction)},
			"another write name": writeOpts("open"),
			"a foreign origin":   append(writeOpts(c.action), withHeader("Origin", "http://evil.example")),
		} {
			if rec := do(t, p, http.MethodPost, path, c.body, opts...); rec.Code != http.StatusForbidden {
				t.Errorf("%s with %s = %d %s", path, name, rec.Code, rec.Body.String())
			}
		}
		if rec := do(t, p, http.MethodGet, path, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/api/autotrade/status", "/api/autotrade/pnl"} {
		if rec := do(t, p, http.MethodGet, path, "", withHeader(actionHeader, "")); rec.Code != http.StatusForbidden {
			t.Errorf("%s without the read header = %d", path, rec.Code)
		}
	}
	// A typo in a field of an order-switching request is an error, not a default.
	if rec := do(t, p, http.MethodPost, "/api/autotrade/start", `{"symbols":["BTCUSDT"],"notional":65}`, writeOpts(autotradeStartAction)...); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d", rec.Code)
	}
	for body, why := range map[string]string{
		`{"symbols":["DOGEUSDT"]}`:                                                   "a symbol off the list",
		`{"symbols":[]}`:                                                             "no symbol",
		`{"symbol":"BTCUSDT"}`:                                                       "the single-pair field of Q18",
		`{"symbols":["BTCUSDT"],"notional_quote":90000}`:                             "a notional over the portal's cap",
		`{"symbols":["BTCUSDT"],"max_concurrent_positions":51}`:                      "51 concurrent pairs",
		`{"symbols":["BTCUSDT"],"total_capital_cap_quote":50}`:                       "a cap under one pair's capital",
		`{"symbols":["BTCUSDT"],"pair_overrides":{"ETHUSDT":{"notional_quote":80}}}`: "an override off the list",
		// The convergence set's own refusals (4.5f): each is validated by
		// autotrade.Config and refused by name, never clamped.
		`{"symbols":["BTCUSDT"],"min_entry_basis_bps":20000}`:                                     "a basis floor above 100% of mid",
		`{"symbols":["BTCUSDT"],"target_take_profit_net_pct":-1}`:                                 "a negative take-profit",
		`{"symbols":["BTCUSDT"],"min_hold_epochs":-1}`:                                            "a negative amortization floor",
		`{"symbols":["BTCUSDT"],"max_hold_epochs":6,"min_hold_epochs":6}`:                         "a floor at the ceiling",
		`{"symbols":["BTCUSDT"],"pair_overrides":{"BTCUSDT":{"target_take_profit_net_pct":500}}}`: "an override no pair could reach",
	} {
		if rec := do(t, p, http.MethodPost, "/api/autotrade/start", body, writeOpts(autotradeStartAction)...); rec.Code != http.StatusBadRequest {
			t.Errorf("start with %s = %d %s", why, rec.Code, rec.Body.String())
		}
	}
	// The no-dialog stop name cannot carry a close, and the close name cannot
	// be sent for a plain stop.
	for body, action := range map[string]string{`{"close_now":true}`: autotradeStopAction, `{"close_now":false}`: autotradeStopCloseAction} {
		if rec := do(t, p, http.MethodPost, "/api/autotrade/stop", body, writeOpts(action)...); rec.Code != http.StatusForbidden || errorCode(t, rec) != "action_header_mismatch" {
			t.Errorf("stop %s under %s = %d %s", body, action, rec.Code, rec.Body.String())
		}
	}
	// A pair switch's name must be its action: the no-dialog PAUSE name cannot
	// carry a RESUME, which lets the bot trade again.
	for _, action := range []string{"pause", "resume", "ack"} {
		for _, header := range []string{autotradePairPauseAction, autotradePairResumeAction, autotradePairAckAction} {
			if header == "autotrade-pair-"+action {
				continue
			}
			body := `{"symbol":"BTCUSDT","action":"` + action + `","halt_seq":1}`
			if rec := do(t, p, http.MethodPost, "/api/autotrade/pair", body, writeOpts(header)...); rec.Code != http.StatusForbidden || errorCode(t, rec) != "action_header_mismatch" {
				t.Errorf("%s under the %s name = %d %s", action, header, rec.Code, rec.Body.String())
			}
		}
	}
	if rec := do(t, p, http.MethodPost, "/api/autotrade/pair", `{"symbol":"BTCUSDT","action":"open"}`, writeOpts(autotradePairPauseAction)...); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown pair action = %d %s", rec.Code, rec.Body.String())
	}
	if st := botStatus(t, p); st.State != autotrade.StateDisabled || orderCount(spot, perp) != 0 {
		t.Errorf("after refused writes: %s, %d orders", st.State, orderCount(spot, perp))
	}
}

// The three convergence knobs reach the run, as the default and per pair, and
// the parameters a form does not carry keep their audited values.
func TestAutotradeAPI_StartCarriesTheConvergenceKnobs(t *testing.T) {
	p, _, _ := botPortal(t) // the portal trades BTCUSDT alone here
	code, started := postJSON[autotradeActionView](t, p, autotradeStartAction, "/api/autotrade/start",
		map[string]any{
			"symbols": []string{"BTCUSDT"}, "notional_quote": 200, "auto_rebalance": false,
			"min_entry_basis_bps": 12.5, "min_hold_epochs": 9, "target_take_profit_net_pct": 0.8,
			"max_exit_spread_bps": 6.5,
			"pair_overrides":      map[string]any{"BTCUSDT": map[string]any{"min_entry_basis_bps": 3, "target_take_profit_net_pct": 0.25, "max_exit_spread_bps": 25}},
		})
	if code != http.StatusOK || started.Status.State != autotrade.StateRunning {
		t.Fatalf("start = %d %s", code, started.Status.State)
	}
	d := started.Status.Portfolio.DefaultPairConfig
	if d.MinEntryBasisBps != 12.5 || d.MinHoldEpochs != 9 || d.TargetTakeProfitNetPct != 0.8 || d.MaxExitSpreadBps != 6.5 {
		t.Errorf("default pair config = %+v", d)
	}
	// Not on any form, so still the audited safety values.
	if d.MaxBasisWidenBps != autotrade.DefaultMaxBasisWidenBps ||
		d.ExitNegativeFundingRateBps != autotrade.DefaultExitNegativeFundingRateBps ||
		d.ExitNegativeConsecutiveEpochs != autotrade.DefaultExitNegativeConsecutiveEpochs {
		t.Errorf("a safety threshold moved with the form: %+v", d)
	}
	// An override replaces only what it names; the rest is the run's default.
	own := started.Status.Portfolio.PairOverrides["BTCUSDT"]
	if own.MinEntryBasisBps != 3 || own.TargetTakeProfitNetPct != 0.25 || own.MinHoldEpochs != 9 || own.NotionalQuote != 200 || own.MaxExitSpreadBps != 25 {
		t.Errorf("BTCUSDT override = %+v", own)
	}
	// And the page shows the pair running with it.
	if got := botPair(t, botStatus(t, p), "BTCUSDT"); got.Config.MinEntryBasisBps != 3 || !got.Overridden {
		t.Errorf("BTCUSDT on the page = %+v", got.Config)
	}
}

func TestAutotradeAPI_WithoutCredentialsTheBotDoesNotStart(t *testing.T) {
	p := testPortal(t, false)
	rec := do(t, p, http.MethodPost, "/api/autotrade/start", `{"symbols":["BTCUSDT"],"notional_quote":200,"auto_rebalance":false}`, writeOpts(autotradeStartAction)...)
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
		map[string]any{"symbols": []string{"BTCUSDT"}, "notional_quote": 200, "auto_rebalance": false, "min_net_apr_pct": 5, "max_hold_epochs": 0})
	if code != http.StatusOK || started.Status.State != autotrade.StateRunning || started.Status.Portfolio.DefaultPairConfig.NotionalQuote != 200 {
		t.Fatalf("start = %d %+v", code, started.Status)
	}

	p.autotrade.Step(context.Background())
	st := botStatus(t, p)
	btc := botPair(t, st, "BTCUSDT")
	if btc.State != autotrade.StateInPosition || btc.Position == nil {
		t.Fatalf("after one scan: %s · halt %q · signal %+v · log %+v", btc.State, btc.HaltReasonVI, btc.Signal, st.Log)
	}
	if !strings.HasPrefix(btc.Position.IntentID, "abtcusdt-") || btc.Signal == nil || btc.Signal.PerpTakerFeeBps != 4 || btc.Signal.SpotTakerFeeBps != 0 {
		t.Errorf("position %+v, fees %v/%v", btc.Position, btc.Signal.SpotTakerFeeBps, btc.Signal.PerpTakerFeeBps)
	}
	if btc.Position.SpotEntryAvgQuote <= 0 || btc.Position.PerpEntryAvgQuote <= 0 || btc.HedgeStatus != autotrade.HedgeBothOpen {
		t.Errorf("the open's fills did not reach the position: %+v, hedge %s", btc.Position, btc.HedgeStatus)
	}
	pos := getJSON[positionsView](t, p, "/api/positions?symbol=BTCUSDT")
	if pos.Status != statusBothOpen || !near(pos.DeltaResidualCoin, 0) || !near(pos.PerpQtyCoin, -0.0025) {
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
	if code != http.StatusOK || killed.Status.State != autotrade.StateEmergencyHalted || len(killed.Closes) != 1 || !killed.Closes[0].Flat {
		t.Fatalf("kill = %d %+v closes %+v", code, killed.Status, killed.Closes)
	}
	if q := perpQty(t, perp, "BTCUSDT"); math.Abs(q) > gridEpsilon {
		t.Fatalf("the venue still holds perp %v after the kill", q)
	}
	if pos := getJSON[positionsView](t, p, "/api/positions?symbol=BTCUSDT"); pos.Status != statusBothFlat {
		t.Errorf("banner after the kill = %s (%s)", pos.Status, pos.ReasonVI)
	}

	rec := do(t, p, http.MethodPost, "/api/autotrade/start", `{"symbols":["BTCUSDT"],"notional_quote":200,"auto_rebalance":false}`, writeOpts(autotradeStartAction)...)
	if rec.Code != http.StatusConflict {
		t.Errorf("start while halted = %d %s", rec.Code, rec.Body.String())
	}
	if code, stopped := postJSON[autotradeActionView](t, p, autotradeStopAction, "/api/autotrade/stop", map[string]any{"close_now": false, "halt_seq": killed.Status.HaltSeq}); code != http.StatusOK || stopped.Status.State != autotrade.StateDisabled {
		t.Fatalf("acknowledge = %d %+v", code, stopped.Status)
	}
	if rec := do(t, p, http.MethodPost, "/api/autotrade/start", `{"symbols":["BTCUSDT"],"notional_quote":200,"auto_rebalance":false}`, writeOpts(autotradeStartAction)...); rec.Code != http.StatusOK {
		t.Errorf("start after the acknowledgement = %d %s", rec.Code, rec.Body.String())
	}
}

// Two pairs through the real portal: both open under their own autotrade intent
// ids, each symbol its own perp position on the venue; ĐÓNG CẶP closes ONE and
// pauses it, the other stays held and managed; the PnL page lists both, the
// closed one with its figures; KILL flattens what is left.
func TestAutotradeAPI_TwoPairsOpenAndOneClosesAlone(t *testing.T) {
	p, spot, perp := multiBotPortal(t, "BTCUSDT", "ETHUSDT")
	code, _ := postJSON[autotradeActionView](t, p, autotradeStartAction, "/api/autotrade/start",
		map[string]any{"symbols": []string{"BTCUSDT", "ETHUSDT"}, "max_concurrent_positions": 2, "total_capital_cap_quote": 900,
			"notional_quote": 200, "auto_rebalance": false})
	if code != http.StatusOK {
		t.Fatalf("start = %d", code)
	}
	p.autotrade.Step(context.Background())
	st := botStatus(t, p)
	if st.OpenPositions != 2 || !near(perpQty(t, perp, "BTCUSDT"), -0.0025) || !near(perpQty(t, perp, "ETHUSDT"), -0.0025) {
		t.Fatalf("after one scan: %d held, perp BTC %v ETH %v · %+v", st.OpenPositions, perpQty(t, perp, "BTCUSDT"), perpQty(t, perp, "ETHUSDT"), st.Log)
	}
	eth := botPair(t, st, "ETHUSDT")
	if eth.Position == nil || !strings.HasPrefix(eth.Position.IntentID, "aethusdt-") {
		t.Fatalf("ETH position = %+v", eth.Position)
	}

	code, closed := postJSON[autotradeActionView](t, p, autotradeClosePairAction, "/api/autotrade/close-pair", map[string]any{"symbol": "ETHUSDT"})
	if code != http.StatusOK || len(closed.Closes) != 1 || !closed.Closes[0].Flat || !closed.Closes[0].Attempted {
		t.Fatalf("close pair = %d %+v", code, closed.Closes)
	}
	if !near(perpQty(t, perp, "ETHUSDT"), 0) || !near(perpQty(t, perp, "BTCUSDT"), -0.0025) {
		t.Fatalf("after closing ETH: perp BTC %v ETH %v", perpQty(t, perp, "BTCUSDT"), perpQty(t, perp, "ETHUSDT"))
	}
	if st := closed.Status; st.State != autotrade.StateRunning || st.OpenPositions != 1 || !botPair(t, st, "ETHUSDT").Paused {
		t.Errorf("status after closing ETH: %s, %d held, ETH paused %v", st.State, st.OpenPositions, botPair(t, st, "ETHUSDT").Paused)
	}
	// Another scan: ETH is still eligible but paused, and stays flat.
	before := orderCount(spot, perp)
	p.autotrade.Step(context.Background())
	if orderCount(spot, perp) != before || !near(perpQty(t, perp, "ETHUSDT"), 0) {
		t.Errorf("a paused pair was re-entered (%d new orders)", orderCount(spot, perp)-before)
	}

	pnl := getJSON[pnlView](t, p, "/api/autotrade/pnl")
	if len(pnl.Trades) != 2 || pnl.ClosedTrades != 1 || pnl.OpenPositions != 1 {
		t.Fatalf("pnl = %d trades, %d closed, %d open · %+v", len(pnl.Trades), pnl.ClosedTrades, pnl.OpenPositions, pnl.Trades)
	}
	var ethTrade, btcTrade pnlTradeView
	for _, tr := range pnl.Trades {
		switch tr.Symbol {
		case "ETHUSDT":
			ethTrade = tr
		case "BTCUSDT":
			btcTrade = tr
		}
	}
	if ethTrade.Status != "closed" || ethTrade.CashResultQuote == nil || btcTrade.Status != "open" || btcTrade.CashResultQuote == nil {
		t.Errorf("ETH %+v · BTC %+v", ethTrade, btcTrade)
	}
	if want := ethTrade.FundingReceivedQuote - ethTrade.CommissionQuote + ethTrade.PairPriceDriftQuote; ethTrade.CashResultQuote != nil && math.Abs(*ethTrade.CashResultQuote-want) > 1e-12 {
		t.Errorf("ETH cash result %v, want funding − commission + drift = %v", *ethTrade.CashResultQuote, want)
	}
	if len(pnl.Points) == 0 || pnl.Points[len(pnl.Points)-1].Source != "now" || pnl.TotalCapitalCapQuote != 900 || pnl.ReturnOnPeakCapitalPct == nil || !near(pnl.PeakCapitalQuote, 600) {
		t.Errorf("points %+v, cap %v, peak %v, roi %v", pnl.Points, pnl.TotalCapitalCapQuote, pnl.PeakCapitalQuote, pnl.ReturnOnPeakCapitalPct)
	}

	code, killed := postJSON[autotradeActionView](t, p, autotradeKillAction, "/api/autotrade/kill", map[string]any{})
	if code != http.StatusOK || len(killed.Closes) != 2 {
		t.Fatalf("kill = %d %+v", code, killed.Closes)
	}
	if !near(perpQty(t, perp, "BTCUSDT"), 0) || !near(perpQty(t, perp, "ETHUSDT"), 0) {
		t.Errorf("after the kill: perp BTC %v ETH %v", perpQty(t, perp, "BTCUSDT"), perpQty(t, perp, "ETHUSDT"))
	}
}

// The pair switches through the page: pause needs no dialog and sends nothing;
// an acknowledgement quoting the wrong halt number is a 409 and changes
// nothing.
func TestAutotradeAPI_PairSwitches(t *testing.T) {
	p, spot, perp := multiBotPortal(t, "BTCUSDT", "ETHUSDT")
	if _, err := p.autotrade.Start(fixedSizePortfolio("BTCUSDT", "ETHUSDT")); err != nil {
		t.Fatal(err)
	}
	code, paused := postJSON[autotradeActionView](t, p, autotradePairPauseAction, "/api/autotrade/pair", map[string]any{"symbol": "ETHUSDT", "action": "pause"})
	if code != http.StatusOK || !botPair(t, paused.Status, "ETHUSDT").Paused {
		t.Fatalf("pause = %d %+v", code, botPair(t, paused.Status, "ETHUSDT"))
	}
	p.autotrade.Step(context.Background())
	if !near(perpQty(t, perp, "ETHUSDT"), 0) || !near(perpQty(t, perp, "BTCUSDT"), -0.0025) {
		t.Errorf("paused ETH / running BTC: perp ETH %v BTC %v", perpQty(t, perp, "ETHUSDT"), perpQty(t, perp, "BTCUSDT"))
	}
	if code, _ := postJSON[autotradeActionView](t, p, autotradePairAckAction, "/api/autotrade/pair", map[string]any{"symbol": "ETHUSDT", "action": "ack", "halt_seq": 7}); code != http.StatusConflict {
		t.Errorf("ack of a pair that is not halted = %d", code)
	}
	before := orderCount(spot, perp)
	if code, resumed := postJSON[autotradeActionView](t, p, autotradePairResumeAction, "/api/autotrade/pair", map[string]any{"symbol": "ETHUSDT", "action": "resume"}); code != http.StatusOK || botPair(t, resumed.Status, "ETHUSDT").Paused {
		t.Fatalf("resume = %d", code)
	}
	if orderCount(spot, perp) != before {
		t.Error("the resume itself placed an order — it only lets the next scan trade")
	}
	p.autotrade.Step(context.Background())
	if !near(perpQty(t, perp, "ETHUSDT"), -0.0025) {
		t.Errorf("after resume and a scan: perp ETH %v", perpQty(t, perp, "ETHUSDT"))
	}
}

// A manual write in flight holds the portal's lock: the bot sends nothing, says
// so, and opens on the next scan.
func TestAutotradeAPI_TheBotWaitsForAManualWrite(t *testing.T) {
	p, spot, perp := botPortal(t)
	if _, err := p.autotrade.Start(autotrade.DefaultPortfolioConfig([]string{"BTCUSDT"})); err != nil {
		t.Fatal(err)
	}
	release, _, _, ok := p.acquire("reconcile")
	if !ok {
		t.Fatal("could not take the lock")
	}
	p.autotrade.Step(context.Background())
	st := botStatus(t, p)
	btc := botPair(t, st, "BTCUSDT")
	if btc.State == autotrade.StateInPosition || orderCount(spot, perp) != 0 || btc.TradeFailures != 0 {
		t.Fatalf("with the lock held: %s, %d orders, %d failures", btc.State, orderCount(spot, perp), btc.TradeFailures)
	}
	if !strings.Contains(st.Log[0].MessageVI, "reconcile") {
		t.Errorf("the console does not say who held the lock: %+v", st.Log[0])
	}
	release()
	p.autotrade.Step(context.Background())
	if btc := botPair(t, botStatus(t, p), "BTCUSDT"); btc.State != autotrade.StateInPosition {
		t.Errorf("after the lock was released: %s", btc.State)
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
	if _, err := p.autotrade.Start(autotrade.DefaultPortfolioConfig([]string{"BTCUSDT"})); err != nil {
		t.Fatal(err)
	}
	p.autotrade.Step(context.Background())
	st := botStatus(t, p)
	if btc := botPair(t, st, "BTCUSDT"); btc.State != autotrade.StateIdleScanning || btc.Position != nil {
		t.Fatalf("beside a manual position: %s, position %+v", btc.State, btc.Position)
	}
	before := orderCount(spot, perp)
	code, killed := postJSON[autotradeActionView](t, p, autotradeKillAction, "/api/autotrade/kill", map[string]any{})
	if code != http.StatusOK || len(killed.Closes) != 1 || killed.Closes[0].Attempted || !killed.Closes[0].NotTheBots {
		t.Fatalf("kill beside a manual position = %d %+v", code, killed.Closes)
	}
	if orderCount(spot, perp) != before {
		t.Error("the kill switch traded a position a person opened")
	}
	if pos := getJSON[positionsView](t, p, "/api/positions?symbol=BTCUSDT"); pos.Status != statusBothOpen {
		t.Errorf("manual position after the kill = %s", pos.Status)
	}
	// And it is no trade of the bot's on the PnL page.
	if pnl := getJSON[pnlView](t, p, "/api/autotrade/pnl"); len(pnl.Trades) != 0 {
		t.Errorf("a person's position on the bot's PnL page: %+v", pnl.Trades)
	}
}

// A second intent holding the symbol beside the bot's (a person's, seeded on the
// venue): the portal's hedge reading names no single intent, and the pair halts
// without closing anything.
func TestAutotradeAPI_ASecondHeldIntentHaltsThePair(t *testing.T) {
	p, spot, perp := botPortal(t)
	if _, err := p.autotrade.Start(fixedSizePortfolio("BTCUSDT")); err != nil {
		t.Fatal(err)
	}
	p.autotrade.Step(context.Background())
	if btc := botPair(t, botStatus(t, p), "BTCUSDT"); btc.State != autotrade.StateInPosition {
		t.Fatalf("bot did not open: %s", btc.State)
	}
	seedIntent(t, p, spot, perp, "pbtcusdt-20260915-000000-001", 0.0008, 0.0008)
	before := orderCount(spot, perp)
	p.autotrade.Step(context.Background())
	btc := botPair(t, botStatus(t, p), "BTCUSDT")
	if btc.State != autotrade.StateEmergencyHalted || !strings.Contains(btc.HaltReasonVI, "2 ý định") || orderCount(spot, perp) != before {
		t.Errorf("beside a second intent: %s %q, orders %d → %d", btc.State, btc.HaltReasonVI, before, orderCount(spot, perp))
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
	perp.SetMarkPrice(broker.MarkPrice{MarkPriceQuote: fakePerpMidQuote, LastFundingRateFrac: 0.0001, NextFundingTimeMs: settledNow + (8 * time.Hour).Milliseconds()})
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

// Several symbols scanned in one pass keep their own settled-history cache
// entries: reading ETH does not evict BTC's, so the pass after reads neither
// from the venue again.
func TestPortalMarket_SeveralSymbolsDoNotEvictEachOthersHistory(t *testing.T) {
	p, _, perp := multiBotPortal(t, "BTCUSDT", "ETHUSDT")
	since := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
	for _, s := range []string{"BTCUSDT", "ETHUSDT"} {
		if _, err := (portalMarket{p}).Snapshot(context.Background(), s, since); err != nil {
			t.Fatal(err)
		}
	}
	calls := perp.fundingCalls.Load()
	if calls < 2 {
		t.Fatalf("%d history reads for two symbols", calls)
	}
	for _, s := range []string{"BTCUSDT", "ETHUSDT", "BTCUSDT"} {
		if _, err := (portalMarket{p}).Snapshot(context.Background(), s, since); err != nil {
			t.Fatal(err)
		}
	}
	if got := perp.fundingCalls.Load(); got != calls {
		t.Errorf("history read %d more times — one symbol's key evicted another's", got-calls)
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
	if _, err := p.autotrade.Start(autotrade.DefaultPortfolioConfig([]string{"BTCUSDT"})); err != nil {
		t.Fatal(err)
	}
	p.autotrade.Step(context.Background())
	btc := botPair(t, botStatus(t, p), "BTCUSDT")
	if btc.State != autotrade.StateIdleScanning || orderCount(spot, perp) != 0 || btc.Signal == nil || btc.Signal.NetAPRPct != nil ||
		!strings.Contains(btc.Signal.NetAPRReasonVI, "phí") {
		t.Errorf("with unread fees: %s, %d orders, signal %+v", btc.State, orderCount(spot, perp), btc.Signal)
	}
}

// ---------------------------------------------------------------------- PnL

// A closed pair returned funding − commission + the drift between its FILLS.
// Slippage is inside that drift and is not taken off again; execution's
// RealizedQuote, which subtracts slippage and leaves the drift out, is carried
// beside it untouched. Only the bot's intents count.
func TestPnL_AClosedPairCountsSlippageOnceAndOnlyTheBotsIntents(t *testing.T) {
	p, _, _ := fakePortal(t)
	base := time.Now().Add(-48 * time.Hour)
	for _, st := range []intentState{
		{IntentID: "abtcusdt-20260913-000000-001", Symbol: "BTCUSDT", Outcome: "both_open", NotionalQuote: 65,
			OpenedAtMs: base.UnixMilli(), ClosedAtMs: base.Add(24 * time.Hour).UnixMilli(), PerpFilledQtyCoin: 0.0008, ClosedQtyCoin: 0.0008,
			FundingQuote: 0.20, CommissionQuote: 0.05, SlippageQuote: 0.03, PairPriceDriftQuote: -0.04, RealizedQuote: 0.12},
		{IntentID: "aethusdt-20260913-000000-002", Symbol: "ETHUSDT", Outcome: "both_open", NotionalQuote: 65,
			OpenedAtMs: base.Add(time.Hour).UnixMilli(), ClosedAtMs: base.Add(30 * time.Hour).UnixMilli(), PerpFilledQtyCoin: 0.02, ClosedQtyCoin: 0.02,
			FundingQuote: 0.10, CommissionQuote: 0.05, SlippageQuote: 0.01, PairPriceDriftQuote: 0.02, RealizedQuote: 0.04},
		// A person's position, and an open that unwound on the way in: neither
		// is a pair the bot held.
		{IntentID: "pbtcusdt-20260913-000000-003", Symbol: "BTCUSDT", Outcome: "both_open", NotionalQuote: 65,
			OpenedAtMs: base.UnixMilli(), ClosedAtMs: base.Add(time.Hour).UnixMilli(), FundingQuote: 9},
		{IntentID: "abtcusdt-20260913-000000-004", Symbol: "BTCUSDT", Outcome: "both_flat", OpenedAtMs: base.UnixMilli()},
	} {
		if err := saveState(p.stateDir, st); err != nil {
			t.Fatal(err)
		}
	}
	st := p.autotrade.Status()
	st.Portfolio.TotalCapitalCapQuote = 1000
	v := p.buildPnL(context.Background(), st, false)
	if v.ClosedTrades != 2 || len(v.Trades) != 2 {
		t.Fatalf("closed %d, trades %+v", v.ClosedTrades, v.Trades)
	}
	// (0.20 − 0.05 − 0.04) + (0.10 − 0.05 + 0.02) = 0.11 + 0.07
	if !near(v.ClosedCashResultQuote, 0.18) || !near(v.ClosedRealizedQuote, 0.16) || !near(v.ClosedSlippageQuote, 0.04) || !near(v.TotalQuote, 0.18) {
		t.Errorf("closed cash %v (want 0.18), realized %v (want 0.16, carried), slippage %v", v.ClosedCashResultQuote, v.ClosedRealizedQuote, v.ClosedSlippageQuote)
	}
	// The two pairs overlapped (hours 1–24), so the capital they needed at once
	// was both of them: 195, whatever the cap says.
	if !near(v.PeakCapitalQuote, 195) || v.ReturnOnPeakCapitalPct == nil || math.Abs(*v.ReturnOnPeakCapitalPct-0.18/195*100) > 1e-9 {
		t.Errorf("peak capital %v, return on it %v, want 195 and 0.18 / 195 × 100", v.PeakCapitalQuote, v.ReturnOnPeakCapitalPct)
	}
	for _, tr := range v.Trades {
		if tr.CashResultOnCapitalPct == nil || !near(tr.CapitalQuote, 97.5) {
			t.Errorf("%s: capital %v, on capital %v", tr.IntentID, tr.CapitalQuote, tr.CashResultOnCapitalPct)
		}
	}
	// The series: the first open at 0, a step at each close, and this read.
	if len(v.Points) != 4 || v.Points[0].TotalQuote != 0 || !near(v.Points[1].TotalQuote, 0.11) || !near(v.Points[2].TotalQuote, 0.18) || v.Points[3].Source != "now" {
		t.Errorf("points = %+v", v.Points)
	}
	for _, pt := range v.Points[:3] {
		if pt.Source != "closed" || pt.OpenQuote != nil {
			t.Errorf("a cache point claims to know the open part: %+v", pt)
		}
	}
}

// The funding bars count one settlement's rows in the quote asset that exactly
// one of the BOT's intents held across — a person's intent's row is left out —
// and an open pair adds its rows to its marked-to-mid drift.
func TestPnL_FundingBarsAndOpenPairsReadTheVenuesRows(t *testing.T) {
	p, _, perp := botPortal(t)
	now := time.Now()
	botOpen := now.Add(-20 * time.Hour)
	manualOpen, manualClose := now.Add(-100*time.Hour), now.Add(-90*time.Hour)
	if err := saveState(p.stateDir, intentState{IntentID: "abtcusdt-20260915-000000-001", Symbol: "BTCUSDT", Outcome: "both_open",
		NotionalQuote: 65, OpenedAtMs: botOpen.UnixMilli(), PerpFilledQtyCoin: 0.0008, SpotFilledQtyCoin: 0.0008}); err != nil {
		t.Fatal(err)
	}
	if err := saveState(p.stateDir, intentState{IntentID: "pbtcusdt-20260915-000000-002", Symbol: "BTCUSDT", Outcome: "both_open",
		NotionalQuote: 65, OpenedAtMs: manualOpen.UnixMilli(), ClosedAtMs: manualClose.UnixMilli(), PerpFilledQtyCoin: 0.0008}); err != nil {
		t.Fatal(err)
	}
	perp.SetFundingIncome(
		broker.FundingIncome{Symbol: "BTCUSDT", SettledAtMs: now.Add(-16 * time.Hour).UnixMilli(), IncomeQuote: 0.006, Asset: "USDT", TranID: "1"},
		broker.FundingIncome{Symbol: "BTCUSDT", SettledAtMs: now.Add(-8 * time.Hour).UnixMilli(), IncomeQuote: 0.004, Asset: "USDT", TranID: "2"},
		broker.FundingIncome{Symbol: "BTCUSDT", SettledAtMs: now.Add(-8 * time.Hour).UnixMilli(), IncomeQuote: 0.001, Asset: "BNFCR", TranID: "3"},
		broker.FundingIncome{Symbol: "BTCUSDT", SettledAtMs: now.Add(-96 * time.Hour).UnixMilli(), IncomeQuote: 0.5, Asset: "USDT", TranID: "4"},
	)
	drift := -0.002
	st := p.autotrade.Status()
	st.CapitalDeployedQuote = 97.5
	st.Positions = []autotrade.PositionView{{Symbol: "BTCUSDT", IntentID: "abtcusdt-20260915-000000-001", OpenedAtMs: botOpen.UnixMilli(),
		QtyCoin: 0.0008, NotionalQuote: 65, CapitalQuote: 97.5, PairDriftQuote: &drift, MarkedAtMs: now.UnixMilli()}}
	v := p.buildPnL(context.Background(), st, true)

	if len(v.Bars) != 2 || !near(v.Bars[0].IncomeQuote, 0.006) || !near(v.Bars[1].IncomeQuote, 0.004) {
		t.Fatalf("bars = %+v (the person's 0.5 and the BNFCR row must be left out)", v.Bars)
	}
	if v.OpenPositions != 1 || v.OpenPriced != 1 || !near(v.OpenFundingQuote, 0.010) || !near(v.OpenResultQuote, 0.008) || !v.OpenComplete {
		t.Errorf("open: %d/%d priced, funding %v, result %v", v.OpenPriced, v.OpenPositions, v.OpenFundingQuote, v.OpenResultQuote)
	}
	if len(v.Trades) != 1 || v.Trades[0].Status != "open" || v.Trades[0].FundingRows != 3 || v.Trades[0].FundingIncomplete == "" {
		t.Errorf("trades = %+v (three rows in the hold; the other-asset one must be named as left out)", v.Trades)
	}
	if v.OpenReturnOnDeployedPct == nil || !near(*v.OpenReturnOnDeployedPct, 0.008/97.5*100) {
		t.Errorf("open return on deployed = %v", v.OpenReturnOnDeployedPct)
	}
	last := v.Points[len(v.Points)-1]
	if last.OpenQuote == nil || !near(*last.OpenQuote, 0.008) || !near(last.TotalQuote, 0.008) {
		t.Errorf("now point = %+v", last)
	}
}

// One sample a minute, a week of them, oldest first.
func TestPnLTracker_KeepsOneSampleAMinuteForAWeek(t *testing.T) {
	tr := newPnLTracker()
	at := time.Unix(1_789_000_000, 0)
	if !tr.record(pnlPoint{TotalQuote: 1}, at) || tr.record(pnlPoint{TotalQuote: 2}, at.Add(30*time.Second)) {
		t.Fatal("a second sample inside the minute was kept")
	}
	if !tr.record(pnlPoint{TotalQuote: 3}, at.Add(58*time.Second)) {
		t.Error("a ticker a hair early dropped its minute")
	}
	for i := 2; i < pnlSampleCap+10; i++ {
		tr.record(pnlPoint{TotalQuote: float64(i)}, at.Add(time.Duration(i)*time.Minute))
	}
	got := tr.list()
	if len(got) != pnlSampleCap || got[0].AtMs >= got[len(got)-1].AtMs || got[len(got)-1].TotalQuote != float64(pnlSampleCap+9) {
		t.Errorf("%d samples, first %d last %d (%v)", len(got), got[0].AtMs, got[len(got)-1].AtMs, got[len(got)-1].TotalQuote)
	}
	for i := 1; i < len(got); i++ {
		if got[i].AtMs <= got[i-1].AtMs || got[i].Source != "sample" {
			t.Fatalf("sample %d out of order or unlabelled: %+v after %+v", i, got[i], got[i-1])
		}
	}
}

// ------------------------------------------- review of the multi-pair change

// Each symbol's settled history is its own: ETH's scan never reads BTC's rows
// out of the cache, whatever order the scan reads them in.
func TestPortalMarket_EachSymbolReadsItsOwnHistory(t *testing.T) {
	p, _, perp := multiBotPortal(t, "BTCUSDT", "ETHUSDT")
	now := time.Now()
	eth := make([]binancebroker.FundingRate, 0, 21)
	for i := 20; i >= 0; i-- {
		eth = append(eth, binancebroker.FundingRate{SettledAtMs: now.Add(-time.Hour - time.Duration(i)*8*time.Hour).UnixMilli(), RatePerIntervalFrac: -0.0003})
	}
	perp.fundingBySymbol = map[string][]binancebroker.FundingRate{"ETHUSDT": eth}
	since := now.Add(-7 * 24 * time.Hour).UnixMilli()
	for _, s := range []string{"BTCUSDT", "ETHUSDT", "BTCUSDT"} {
		snap, err := (portalMarket{p}).Snapshot(context.Background(), s, since)
		if err != nil || len(snap.Settled) == 0 {
			t.Fatalf("%s: %v, %d rows", s, err, len(snap.Settled))
		}
		want := 0.0001
		if s == "ETHUSDT" {
			want = -0.0003
		}
		if got := snap.Settled[len(snap.Settled)-1].RatePerIntervalFrac; got != want {
			t.Errorf("%s read a newest rate of %v, want its own %v", s, got, want)
		}
	}
}

// A stop carries the halt count the page showed; without it, or with an older
// one, nothing is acknowledged.
func TestAutotradeAPI_AStopQuotesTheHaltCountThePageShowed(t *testing.T) {
	p, _, _ := botPortal(t)
	if _, err := p.autotrade.Start(autotrade.DefaultPortfolioConfig([]string{"BTCUSDT"})); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, p, http.MethodPost, "/api/autotrade/stop", `{"close_now":false}`, writeOpts(autotradeStopAction)...); rec.Code != http.StatusBadRequest || errorCode(t, rec) != "halt_seq_required" {
		t.Errorf("stop without halt_seq = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, p, http.MethodPost, "/api/autotrade/stop", `{"close_now":false,"halt_seq":5}`, writeOpts(autotradeStopAction)...); rec.Code != http.StatusConflict {
		t.Errorf("stop quoting a halt count the bot never reached = %d %s", rec.Code, rec.Body.String())
	}
	if st := botStatus(t, p); st.State != autotrade.StateRunning {
		t.Errorf("after refused stops: %s", st.State)
	}
	if code, out := postJSON[autotradeActionView](t, p, autotradeStopAction, "/api/autotrade/stop", map[string]any{"close_now": false, "halt_seq": 0}); code != http.StatusOK || out.Status.State != autotrade.StateDisabled {
		t.Errorf("stop quoting the current count = %d %s", code, out.Status.State)
	}
}

// The bot's reads stop at half the venue's weight budget, like the page's: a
// minute already half spent opens nothing, and the order path keeps the rest.
func TestAutotradeAPI_TheBotsReadsStopAtHalfTheBudget(t *testing.T) {
	p, spot, perp := botPortal(t)
	h := http.Header{}
	h.Set("X-Mbx-Used-Weight-1m", "5000")
	perp.HTTP().Budget().Observe(h)
	if _, err := p.autotrade.Start(autotrade.DefaultPortfolioConfig([]string{"BTCUSDT"})); err != nil {
		t.Fatal(err)
	}
	p.autotrade.Step(context.Background())
	btc := botPair(t, botStatus(t, p), "BTCUSDT")
	if orderCount(spot, perp) != 0 || btc.State == autotrade.StateInPosition || btc.HedgeStatus != autotrade.HedgeUnknown || !strings.Contains(btc.HedgeReasonVI, "weight") {
		t.Errorf("with the read budget spent: %d orders, %s, hedge %s %q", orderCount(spot, perp), btc.State, btc.HedgeStatus, btc.HedgeReasonVI)
	}
}

// An open that unwound to flat is listed and named as uncosted; an open pair
// whose mark is stale is not priced, and this read then joins no point of the
// series; rows two of the bot's intents shared are no bar; the open return
// divides by the capital of the pairs it prices.
func TestPnL_UnwoundOpensStaleMarksAndSharedRows(t *testing.T) {
	p, _, perp := botPortal(t)
	now := time.Now()
	for _, st := range []intentState{
		// An unwind as execution caches it: orders placed, both legs' filled
		// quantity taken back to zero, no note.
		{IntentID: "abtcusdt-20260915-000000-011", Symbol: "BTCUSDT", Outcome: "both_flat", NotionalQuote: 65, OpenedAtMs: now.Add(-50 * time.Hour).UnixMilli(),
			SpotClientOrderID: "abtcusdt-20260915-000000-011-s", PerpClientOrderID: "abtcusdt-20260915-000000-011-p"},
		{IntentID: "abtcusdt-20260915-000000-012", Symbol: "BTCUSDT", Outcome: "both_open", NotionalQuote: 65, OpenedAtMs: now.Add(-30 * time.Hour).UnixMilli(), PerpFilledQtyCoin: 0.0008},
		{IntentID: "abtcusdt-20260915-000000-013", Symbol: "BTCUSDT", Outcome: "both_open", NotionalQuote: 65, OpenedAtMs: now.Add(-20 * time.Hour).UnixMilli(), PerpFilledQtyCoin: 0.0008},
	} {
		if err := saveState(p.stateDir, st); err != nil {
			t.Fatal(err)
		}
	}
	// One row inside both open intents' holds: shared, no bar.
	perp.SetFundingIncome(broker.FundingIncome{Symbol: "BTCUSDT", SettledAtMs: now.Add(-8 * time.Hour).UnixMilli(), IncomeQuote: 0.004, Asset: "USDT", TranID: "9"})
	fresh, stale := 0.01, -0.5
	st := p.autotrade.Status()
	st.Positions = []autotrade.PositionView{
		{Symbol: "BTCUSDT", IntentID: "abtcusdt-20260915-000000-012", OpenedAtMs: now.Add(-30 * time.Hour).UnixMilli(), QtyCoin: 0.0008, NotionalQuote: 65, CapitalQuote: 97.5, PairDriftQuote: &fresh, MarkedAtMs: now.UnixMilli()},
		{Symbol: "BTCUSDT", IntentID: "abtcusdt-20260915-000000-013", OpenedAtMs: now.Add(-20 * time.Hour).UnixMilli(), QtyCoin: 0.0008, NotionalQuote: 65, CapitalQuote: 97.5, PairDriftQuote: &stale, MarkedAtMs: now.Add(-10 * time.Minute).UnixMilli()},
	}
	st.CapitalDeployedQuote = 195
	v := p.buildPnL(context.Background(), st, true)
	if v.UnwoundOpens != 1 || len(v.ProblemsVI) == 0 {
		t.Errorf("unwound %d, problems %v", v.UnwoundOpens, v.ProblemsVI)
	}
	var unwound, staleRow pnlTradeView
	for _, tr := range v.Trades {
		switch tr.IntentID {
		case "abtcusdt-20260915-000000-011":
			unwound = tr
		case "abtcusdt-20260915-000000-013":
			staleRow = tr
		}
	}
	if unwound.Status != "unwound" || unwound.CashResultQuote != nil {
		t.Errorf("unwound row = %+v", unwound)
	}
	if staleRow.CashResultQuote != nil || staleRow.PairDriftQuote != nil || !strings.Contains(staleRow.NoteVI, "cũ") {
		t.Errorf("stale-marked row = %+v", staleRow)
	}
	if v.OpenPriced != 1 || v.OpenComplete || !near(v.OpenResultQuote, 0.01) {
		t.Errorf("open priced %d of %d, complete %v, result %v", v.OpenPriced, v.OpenPositions, v.OpenComplete, v.OpenResultQuote)
	}
	if v.OpenReturnOnDeployedPct == nil || !near(*v.OpenReturnOnDeployedPct, 0.01/97.5*100) {
		t.Errorf("open return = %v, want over the ONE priced pair's capital", v.OpenReturnOnDeployedPct)
	}
	if len(v.Bars) != 0 {
		t.Errorf("bars = %+v — a row two intents shared is split between neither", v.Bars)
	}
	for _, pt := range v.Points {
		if pt.Source == "now" {
			t.Errorf("an incomplete read joined the series: %+v", pt)
		}
	}
	if p.samplePnL(context.Background(), st) {
		t.Error("an incomplete total was sampled")
	}
}

// Peak capital is what the pairs tied up AT ONCE: two pairs one after the other
// needed one pair's capital; a close and an open at the same instant hand the
// capital over; an open pair runs to now; an unwound open is left out.
func TestPnL_PeakCapitalCountsOnlyWhatWasHeldAtOnce(t *testing.T) {
	trades := []pnlTradeView{
		{Status: "closed", CapitalQuote: 97.5, OpenedAtMs: 1_000, ClosedAtMs: 2_000},
		{Status: "closed", CapitalQuote: 97.5, OpenedAtMs: 2_000, ClosedAtMs: 3_000},
		{Status: "unwound", CapitalQuote: 97.5, OpenedAtMs: 2_500},
	}
	if got := peakCapital(trades, 10_000); got != 97.5 {
		t.Errorf("sequential pairs = %v, want 97.5", got)
	}
	trades = append(trades, pnlTradeView{Status: "open", CapitalQuote: 120, OpenedAtMs: 2_900})
	if got := peakCapital(trades, 10_000); got != 217.5 {
		t.Errorf("with an open pair overlapping the second = %v, want 217.5", got)
	}
	// Opened and closed inside one millisecond — a fake venue does it, and so
	// can a fast testnet: the pair still held its capital.
	if got := peakCapital([]pnlTradeView{{Status: "closed", CapitalQuote: 97.5, OpenedAtMs: 5_000, ClosedAtMs: 5_000}}, 10_000); got != 97.5 {
		t.Errorf("a pair open for under a millisecond = %v, want 97.5", got)
	}
}

// An open whose unwind raised an alarm is recorded by execution as both_flat
// with a note; the page names it an alarm, not an unwind back to flat.
func TestPnL_AnAlarmOpenIsNotListedAsUnwound(t *testing.T) {
	p, _, _ := botPortal(t)
	now := time.Now()
	// As execution caches it: the unwind took the filled quantity back to
	// zero, and the note is the only mark of the alarm (review round 3).
	if err := saveState(p.stateDir, intentState{IntentID: "abtcusdt-20260915-000000-021", Symbol: "BTCUSDT", Outcome: "both_flat", NotionalQuote: 65,
		OpenedAtMs: now.Add(-time.Hour).UnixMilli(), SpotClientOrderID: "abtcusdt-20260915-000000-021-s",
		NoteVI: "execution: UNWIND INCOMPLETE — the account may be unhedged"}); err != nil {
		t.Fatal(err)
	}
	// And an open refused before placing leaves no ids: no row at all.
	if err := saveState(p.stateDir, intentState{IntentID: "abtcusdt-20260915-000000-022", Symbol: "BTCUSDT", Outcome: "both_flat", NotionalQuote: 65,
		OpenedAtMs: now.Add(-time.Hour).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	v := p.buildPnL(context.Background(), p.autotrade.Status(), false)
	if len(v.Trades) != 1 || v.Trades[0].Status != "alarm" || v.AlarmOpens != 1 || v.UnwoundOpens != 0 || len(v.ProblemsVI) == 0 {
		t.Errorf("alarm open = %+v, alarms %d, unwound %d, problems %v", v.Trades, v.AlarmOpens, v.UnwoundOpens, v.ProblemsVI)
	}
}

// heldClock answers the server-time ping only once release is closed, or fails
// with the request's own context — a ping that hangs.
type heldClock struct {
	t       *testing.T
	release <-chan struct{}
}

func (c heldClock) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case <-c.release:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	return clockOnly{c.t}.RoundTrip(r)
}

// A caller waits on the shared clock measurement only as long as its own
// context allows — a stop or a kill cancels the scan and must not wait out a
// hung ping — and the measurement runs on for everyone else, on its own
// deadline: the caller that went away does not fail it (review round 4).
func TestSyncClock_ACallerStopsWaitingWhenItsContextEndsAndTheMeasurementRunsOn(t *testing.T) {
	p, _, _ := botPortal(t)
	cfg, err := binancebroker.DefaultConfig(broker.MarketSpot, broker.Credentials{APIKey: broker.NewSecret("k"), APISecret: broker.NewSecret("s")})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	cfg.TestTransport = heldClock{t: t, release: release}
	h, err := broker.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- p.syncClockIfStale(ctx, broker.MarketSpot, h) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the cancelled caller got %v", err)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("the caller kept waiting on a hung clock sync after its context ended")
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for h.ClockMeasuredAt().IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("the shared measurement failed with the caller that went away")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The buffered slot through the portal's own endpoints (PLAN 4.5g): the bot
// reads BOTH testnet wallets, sizes every slot from what they hold, and opens
// at that size rather than at the seed the form carried.
func TestAutotradeAPI_TheRebalanceSizesSlotsFromBothWallets(t *testing.T) {
	p, _, perp := botPortal(t) // spot holds 10,000 USDT, futures 5,000
	code, started := postJSON[autotradeActionView](t, p, autotradeStartAction, "/api/autotrade/start",
		map[string]any{
			"symbols": []string{"BTCUSDT"}, "notional_quote": 65, // a seed, not a size
			"auto_rebalance": true, "margin_buffer_pct": 0.30, "rebalance_interval_hours": 168.0,
			"max_concurrent_positions": 6, "total_capital_cap_quote": 100_000,
		})
	if code != http.StatusOK || started.Status.State != autotrade.StateRunning {
		t.Fatalf("start = %d %+v", code, started.Status)
	}
	pf := started.Status.Portfolio
	if !pf.AutoRebalance || pf.MarginBufferPct != 0.30 || pf.RebalanceIntervalHours != 168 {
		t.Fatalf("portfolio = %+v", pf)
	}

	p.autotrade.Step(context.Background())
	st := botStatus(t, p)
	// (10,000 + 5,000) × 0.70 ÷ 6 slots ÷ 1.5 = 1,166.67 a leg.
	want := 15_000 * 0.70 / 6 / 1.5
	if got := st.Portfolio.DefaultPairConfig.NotionalQuote; math.Abs(got-want) > 1e-9 {
		t.Fatalf("sized to %v, want %v · %+v", got, want, st.Log)
	}
	if st.Portfolio.LastRebalancedAtMs == 0 || st.Portfolio.NextRebalanceAtMs <= st.Portfolio.LastRebalancedAtMs {
		t.Errorf("schedule = last %d next %d", st.Portfolio.LastRebalancedAtMs, st.Portfolio.NextRebalanceAtMs)
	}
	// It opened at the sized notional, and the VENUE shows that quantity.
	btc := botPair(t, st, "BTCUSDT")
	if btc.State != autotrade.StateInPosition || btc.Position == nil || math.Abs(btc.Position.NotionalQuote-want) > 1e-9 {
		t.Fatalf("BTC = %s %+v · %+v", btc.State, btc.Position, st.Log)
	}
	if q := perpQty(t, perp, "BTCUSDT"); math.Abs(q+btc.Position.QtyCoin) > 1e-9 || q >= 0 {
		t.Errorf("venue perp %v against the position's %v coin", q, btc.Position.QtyCoin)
	}
	// A wallet the bot cannot read is not a wallet worth zero: the size stays.
	perp.balanceErr = errors.New("timeout")
	p.autotrade.Step(context.Background())
	if got := botStatus(t, p).Portfolio.DefaultPairConfig.NotionalQuote; math.Abs(got-want) > 1e-9 {
		t.Errorf("size after an unreadable wallet = %v, want the %v it had", got, want)
	}
}

// The portal's -symbols default is an ALLOW-LIST: the page draws one checkbox
// per entry, and a run enters the subset the operator ticks. The sizing then
// divides the account across exactly those N pairs, so a wider allow-list costs
// nothing until a box is ticked (PLAN 4.5g, restored to twelve 2026-09-16).
func TestSymbols_TheAllowListIsTwelveAndARunSizesForWhatIsTicked(t *testing.T) {
	list, err := parseSymbols(defaultSymbolsFlag)
	if err != nil {
		t.Fatalf("the shipped -symbols default does not parse: %v", err)
	}
	if len(list) != 12 {
		t.Errorf("%d symbols in the default allow-list, want 12: %v", len(list), list)
	}
	for _, want := range []string{"BTCUSDT", "ETHUSDT", "SOLUSDT", "BNBUSDT", "XRPUSDT", "DOGEUSDT",
		"LTCUSDT", "SUIUSDT", "LINKUSDT", "UNIUSDT", "NEARUSDT", "AAVEUSDT"} {
		if !slices.Contains(list, want) {
			t.Errorf("%s is not in the allow-list %v", want, list)
		}
	}
	// Twelve pairs must be a startable run: the concurrency cap, the shipped
	// capital cap and every pair's own Config have to admit them all at once.
	pc := autotrade.DefaultPortfolioConfig(list)
	if pc.MaxConcurrentPositions != 12 {
		t.Errorf("a twelve-symbol run allows %d concurrent positions", pc.MaxConcurrentPositions)
	}
	if err := pc.Validate(list, maxNotionalQuote, 1.5); err != nil {
		t.Errorf("a twelve-symbol run does not validate: %v", err)
	}
	// Ticking a subset is what the page sends, and the slot count follows it:
	// the capital is divided across the ticked pairs, not across the twelve.
	for _, n := range []int{1, 3, 7, 12} {
		chosen := list[:n]
		sub := autotrade.DefaultPortfolioConfig(chosen)
		if sub.MaxConcurrentPositions != n {
			t.Errorf("%d ticked pairs → %d slots", n, sub.MaxConcurrentPositions)
		}
		if err := sub.Validate(list, maxNotionalQuote, 1.5); err != nil {
			t.Errorf("%d ticked pairs do not validate: %v", n, err)
		}
	}
}
