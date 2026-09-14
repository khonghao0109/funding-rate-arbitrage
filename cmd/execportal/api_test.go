package main

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
)

func TestValidateOpen(t *testing.T) {
	p := testPortal(t, false)
	good := openRequest{Symbol: " btcusdt ", NotionalQuote: 65}
	if err := p.validateOpen(&good); err != nil {
		t.Fatalf("a $65 BTCUSDT open was refused: %v", err)
	}
	if good.Symbol != "BTCUSDT" || good.LegOrder != "sequential_spot_first" {
		t.Errorf("normalized to %+v — symbol upper-cased, leg order defaulted to execution's", good)
	}
	for name, req := range map[string]openRequest{
		"ceiling exactly": {Symbol: "BTCUSDT", NotionalQuote: maxNotionalQuote},
		"parallel":        {Symbol: "ETHUSDT", NotionalQuote: 100, LegOrder: "parallel"},
		"below any venue minimum still reaches execution, which refuses it by name": {Symbol: "BTCUSDT", NotionalQuote: 1},
	} {
		if err := p.validateOpen(&req); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	for name, req := range map[string]openRequest{
		"NaN":         {Symbol: "BTCUSDT", NotionalQuote: math.NaN()},
		"Inf":         {Symbol: "BTCUSDT", NotionalQuote: math.Inf(1)},
		"over":        {Symbol: "BTCUSDT", NotionalQuote: maxNotionalQuote + 0.01},
		"zero":        {Symbol: "BTCUSDT", NotionalQuote: 0},
		"off list":    {Symbol: "SOLUSDT", NotionalQuote: 65},
		"leg order":   {Symbol: "BTCUSDT", NotionalQuote: 65, LegOrder: "spot_only"},
		"empty":       {},
		"lower-cased": {Symbol: "btc", NotionalQuote: 65},
	} {
		if err := p.validateOpen(&req); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestParseSymbols(t *testing.T) {
	got, err := parseSymbols(" btcusdt, ETHUSDT,,BTCUSDT ")
	if err != nil || len(got) != 2 || got[0] != "BTCUSDT" || got[1] != "ETHUSDT" {
		t.Errorf("parseSymbols = %v, %v", got, err)
	}
	for _, bad := range []string{"", ",", "BTC-USDT", "BTCUSDT;ETHUSDT"} {
		if _, err := parseSymbols(bad); err == nil {
			t.Errorf("parseSymbols(%q) accepted", bad)
		}
	}
}

// Slippage is POSITIVE when the fill is worse than the touch, on either side.
func TestSlippageBps_SignIsWorseIsPositive(t *testing.T) {
	if bps, ok := slippageBps(77_000*1.0001, 77_000, true); !ok || math.Abs(bps-1) > 1e-6 {
		t.Errorf("buy 1 bp above the ask = %v %v, want +1", bps, ok)
	}
	if bps, ok := slippageBps(77_000*0.9999, 77_000, false); !ok || math.Abs(bps-1) > 1e-6 {
		t.Errorf("sell 1 bp below the bid = %v %v, want +1", bps, ok)
	}
	if bps, _ := slippageBps(77_000*0.9999, 77_000, true); bps >= 0 {
		t.Errorf("buy below the ask = %v, want negative (better)", bps)
	}
	if _, ok := slippageBps(0, 77_000, true); ok {
		t.Error("no fill priced as a slippage")
	}
	if bpsPtr(slippageBps(0, 77_000, true)) != nil {
		t.Error("an unpriced slippage reached the wire as a number")
	}
}

func TestSmallestWorkableNotional_ClearsBothMinimumsAfterRounding(t *testing.T) {
	spot := binancebroker.MarketRules{Instrument: spotRulesBTC}
	perp := binancebroker.MarketRules{Instrument: perpRulesBTC}
	got := smallestWorkableNotionalQuote(spot, perp, 77_000)
	if want := 50 + 2*0.0001*77_000; math.Abs(got-want) > 1e-9 {
		t.Errorf("smallest = %v, want %v (futures' 50 plus two perp steps)", got, want)
	}
	// Rounding that notional DOWN onto the perp grid must still clear 50.
	qty := math.Floor(got/77_000/perpRulesBTC.StepSizeCoin) * perpRulesBTC.StepSizeCoin
	if qty*77_000 < perpRulesBTC.MinNotionalQuote {
		t.Errorf("%v quote rounds to %v coin = %v quote, under the minimum", got, qty, qty*77_000)
	}
}

func TestSameAsset_UsesWhatTheVenueDeclares(t *testing.T) {
	mk := func(base, quote string) binancebroker.MarketRules {
		return binancebroker.MarketRules{Instrument: exchanges.Instrument{BaseAsset: base, QuoteAsset: quote}}
	}
	if err := sameAsset(mk("BTC", "USDT"), mk("BTC", "USDT")); err != nil {
		t.Error(err)
	}
	for name, pair := range map[string][2]binancebroker.MarketRules{
		"different coin":  {mk("BTC", "USDT"), mk("BTCDOM", "USDT")},
		"different quote": {mk("BTC", "USDT"), mk("BTC", "USDC")},
		"undeclared":      {mk("", "USDT"), mk("BTC", "USDT")},
	} {
		if err := sameAsset(pair[0], pair[1]); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestPickBalances_KeepsOrderAndNeverInventsAZero(t *testing.T) {
	got := pickBalances([]broker.Balance{
		{Asset: "BTC", FreeQtyCoin: 1, LockedQtyCoin: 0.5},
		{Asset: "ETH", FreeQtyCoin: 9},
		{Asset: "USDT", FreeQtyCoin: 100},
	}, []string{"USDT", "BTC", "BNB"})
	if len(got) != 2 || got[0].Asset != "USDT" || got[1].Asset != "BTC" || got[1].TotalQtyInAsset != 1.5 {
		t.Errorf("pickBalances = %+v", got)
	}
}

// The venue pays the ACCOUNT's position. A settlement inside exactly one
// intent's holding window is that intent's; one inside two is both intents'
// and split between neither.
func TestAttributeFunding(t *testing.T) {
	const hour = int64(3_600_000)
	states := []intentState{
		{IntentID: "a", Outcome: "both_open", PerpFilledQtyCoin: 0.02, OpenedAtMs: 1 * hour, ClosedAtMs: 5 * hour, FundingQuote: 0.30},
		{IntentID: "b", Outcome: "both_open", PerpFilledQtyCoin: 0.01, OpenedAtMs: 4 * hour},
		{IntentID: "never-hedged", Outcome: "both_flat", PerpFilledQtyCoin: 0, OpenedAtMs: 0},
	}
	rows := []broker.FundingIncome{
		{SettledAtMs: 2 * hour, IncomeQuote: 0.10, Asset: "USDT", TranID: "1"},  // a only
		{SettledAtMs: 3 * hour, IncomeQuote: 0.20, Asset: "USDT", TranID: "2"},  // a only
		{SettledAtMs: 4 * hour, IncomeQuote: 0.09, Asset: "USDT", TranID: "3"},  // a and b — shared
		{SettledAtMs: 6 * hour, IncomeQuote: -0.01, Asset: "USDT", TranID: "4"}, // b only, paid
		{SettledAtMs: 0, IncomeQuote: 0.05, Asset: "BNFCR", TranID: "5"},        // nobody's
		{SettledAtMs: 2 * hour, IncomeQuote: 0.07, Asset: "BNFCR", TranID: "6"}, // a's, but not quote
	}
	out, totals, intents := attributeFunding(rows, states, "USDT", 0, 10*hour, 7*hour)

	if len(out) != 6 || out[0].TranID != "4" {
		t.Fatalf("rows not newest-first: %+v", out)
	}
	byTran := map[string][]string{}
	for _, r := range out {
		byTran[r.TranID] = r.IntentIDs
	}
	if len(byTran["3"]) != 2 || len(byTran["1"]) != 1 || len(byTran["5"]) != 0 {
		t.Errorf("attribution = %v", byTran)
	}
	if math.Abs(totals["USDT"]-0.38) > 1e-12 || math.Abs(totals["BNFCR"]-0.12) > 1e-12 {
		t.Errorf("totals = %v — each asset summed separately, never converted", totals)
	}
	if len(intents) != 2 {
		t.Fatalf("intents = %+v, want only the two that held a perp leg", intents)
	}
	for _, it := range intents {
		switch it.IntentID {
		case "a":
			if math.Abs(it.VenueRowsQuote-0.30) > 1e-12 || it.VenueRows != 4 || it.SharedRows != 1 || it.OtherAssetRows != 1 || it.CachedFundingQuote != 0.30 {
				t.Errorf("a = %+v, want 0.30 over 2 sole USDT rows, 1 shared row left unsplit, 1 BNFCR row never added", it)
			}
		case "b":
			if math.Abs(it.VenueRowsQuote-(-0.01)) > 1e-12 || it.SharedRows != 1 || it.ClosedAtMs != 0 {
				t.Errorf("b = %+v", it)
			}
		}
	}
	_, _, windowed := attributeFunding(nil, states[:1], "USDT", 2*hour, 10*hour, 7*hour)
	if !windowed[0].OutsideWindow {
		t.Error("an intent opened before the lookback is not flagged as partially read")
	}
}

// One venue read answers every concurrent caller; a failure is not cached; a
// stale value is re-read; invalidate forgets.
func TestTTLCache(t *testing.T) {
	var clock atomic.Int64
	now := func() time.Time { return time.UnixMilli(clock.Load()) }
	c := newTTLCache[int](now)

	var loads atomic.Int64
	gate := make(chan struct{})
	load := func() (int, error) {
		loads.Add(1)
		<-gate
		return 42, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, _, err := c.get("k", time.Second, load); v != 42 || err != nil {
				t.Errorf("get = %v %v", v, err)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	if loads.Load() != 1 {
		t.Errorf("20 concurrent callers caused %d loads, want 1", loads.Load())
	}

	quick := func() (int, error) { loads.Add(1); return 7, nil }
	c.get("k", time.Second, quick)
	if loads.Load() != 1 {
		t.Error("a fresh value was re-read")
	}
	clock.Add(1000)
	if v, _, _ := c.get("k", time.Second, quick); v != 7 || loads.Load() != 2 {
		t.Errorf("a stale value was not re-read: %v after %d loads", v, loads.Load())
	}

	failing := func() (int, error) { loads.Add(1); return 0, errors.New("venue down") }
	if _, _, err := c.get("e", time.Hour, failing); err == nil {
		t.Error("a failed read came back without its error")
	}
	c.get("e", time.Hour, failing)
	if loads.Load() != 3 {
		t.Errorf("a failure was not shared for errorTTL: %d loads", loads.Load())
	}
	clock.Add(errorTTL.Milliseconds())
	c.get("e", time.Hour, failing)
	if loads.Load() != 4 {
		t.Errorf("a failure was held longer than errorTTL: %d loads", loads.Load())
	}

	c.invalidate()
	c.get("k", time.Hour, quick)
	if loads.Load() != 5 {
		t.Error("invalidate did not force a re-read")
	}

	// A load that panics releases its waiters with an error instead of
	// hanging every later caller on the key.
	done := make(chan error, 1)
	go func() {
		_, _, err := c.get("p", time.Hour, func() (int, error) { panic("boom") })
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a panicking load returned no error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a panicking load never returned")
	}
	clock.Add(errorTTL.Milliseconds())
	if v, _, err := c.get("p", time.Hour, quick); err != nil || v != 7 {
		t.Errorf("the key stayed poisoned after a panic: %v %v", v, err)
	}
}
