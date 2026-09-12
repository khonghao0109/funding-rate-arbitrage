package paper

import (
	"math"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/strategy"
)

// A deep, symmetric book: 1M inside 0.1% and 5M inside 0.5% on each side,
// spanning the whole window, so a 50k fill sits comfortably inside it.
func deepBook(source string, mid float64, sampledAtMs int64) depth.Summary {
	return depth.Summary{
		Source: source, Symbol: "BTCUSDT", SampledAtMs: sampledAtMs,
		MidPriceQuote: mid, BestBidQuote: mid * 0.9999, BestAskQuote: mid * 1.0001, SpreadPct: 0.02,
		BidDepthWithinTightQuote: 1_000_000, AskDepthWithinTightQuote: 1_000_000,
		BidDepthWithinWideQuote: 5_000_000, AskDepthWithinWideQuote: 5_000_000,
		BidLevels: 100, AskLevels: 100, BidSpanPct: 1, AskSpanPct: 1,
	}
}

// thinBook holds only 10k inside 0.5% — a 50k fill is refused by production.
func thinBook(source string, mid float64, sampledAtMs int64) depth.Summary {
	b := deepBook(source, mid, sampledAtMs)
	b.BidDepthWithinTightQuote, b.AskDepthWithinTightQuote = 5_000, 5_000
	b.BidDepthWithinWideQuote, b.AskDepthWithinWideQuote = 10_000, 10_000
	return b
}

func verified(source string, bps float64) FeeState {
	return FeeState{Source: source, TakerBps: bps, Verified: true, Known: true}
}

const (
	t0        = int64(1_789_110_000_000)
	notional  = 50_000.0
	spotMid   = 100_000.0
	perpMid   = 100_050.0
	spotFee   = 10.0 // bps
	perpFee   = 5.0
	hourMs    = int64(3_600_000)
	tolerance = 1e-9
)

func openReq(at int64, spot, perp depth.Summary) OpenRequest {
	return OpenRequest{
		Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot",
		SpotQuoteAsset: "USDT", PerpQuoteAsset: "USDT",
		AtMs: at, NotionalQuote: notional, MaxBookAge: 2 * time.Hour,
		SpotFee: verified("binance_spot", spotFee), PerpFee: verified("binance_futures", perpFee),
		SpotBook: spot, PerpBook: perp,
		ProjectedNetAPRFrac: 0.05, ProjectedNetAPROK: true, JournalCostTotalPct: 0.30,
	}
}

func closeReq(at int64, spot, perp depth.Summary) CloseRequest {
	return CloseRequest{
		Symbol: "BTCUSDT", PerpSource: "binance_futures", AtMs: at, MaxBookAge: 2 * time.Hour,
		SpotFee: verified("binance_spot", spotFee), PerpFee: verified("binance_futures", perpFee),
		SpotBook: spot, PerpBook: perp,
	}
}

func near(a, b float64) bool { return math.Abs(a-b) <= tolerance*math.Max(1, math.Abs(b)) }

// PLAN 4.3 acceptance (1): the fill price IS strategy.EstimateFill on the
// given snapshot — buy at the ask side above mid, sell at the bid side below
// it — and the fee is the row's taker rate on the leg's notional.
func TestOpen_FillPriceIsEstimateFillOnTheGivenSnapshot(t *testing.T) {
	l := New(1_000_000)
	spot, perp := deepBook("binance_spot", spotMid, t0-hourMs), deepBook("binance_futures", perpMid, t0-hourMs)
	if err := l.Open(openReq(t0, spot, perp)); err != nil {
		t.Fatalf("open refused: %v", err)
	}
	p, ok := l.Position("BTCUSDT", "binance_futures")
	if !ok {
		t.Fatal("no open position after Open")
	}

	wantSpot := strategy.EstimateFill(spot, strategy.SideBuy, notional)
	// The perp leg is the SAME coin quantity, sized at its value on the
	// perp's own book — not the spot notional.
	wantPerp := strategy.EstimateFill(perp, strategy.SideSell, p.QtyCoin*perpMid)
	if !wantSpot.Fillable || !wantPerp.Fillable {
		t.Fatal("fixture books must be fillable")
	}
	if !near(p.EntrySpot.SlippagePct, wantSpot.SlippagePct) || !near(p.EntryPerp.SlippagePct, wantPerp.SlippagePct) {
		t.Fatalf("slippage %v/%v, want EstimateFill's %v/%v", p.EntrySpot.SlippagePct, p.EntryPerp.SlippagePct, wantSpot.SlippagePct, wantPerp.SlippagePct)
	}
	if !near(p.EntrySpot.FillPriceQuote, spotMid*(1+wantSpot.SlippagePct/100)) {
		t.Fatalf("spot buy filled at %v, want mid moved UP by slippage %v", p.EntrySpot.FillPriceQuote, spotMid*(1+wantSpot.SlippagePct/100))
	}
	if !near(p.EntryPerp.FillPriceQuote, perpMid*(1-wantPerp.SlippagePct/100)) {
		t.Fatalf("perp sell filled at %v, want mid moved DOWN by slippage", p.EntryPerp.FillPriceQuote)
	}
	if p.EntrySpot.FillPriceQuote <= spotMid || p.EntryPerp.FillPriceQuote >= perpMid {
		t.Fatal("a buy must pay above mid and a sell must receive below mid")
	}
	if !near(p.QtyCoin, notional/p.EntrySpot.FillPriceQuote) || !near(p.EntryPerp.QtyCoin, p.QtyCoin) {
		t.Fatalf("qty %v / perp qty %v: both legs must carry the same coin quantity = notional / spot fill", p.QtyCoin, p.EntryPerp.QtyCoin)
	}
	if !near(p.EntrySpot.FeeQuote, notional*spotFee/10_000) || !near(p.EntryPerp.FeeQuote, p.EntryPerp.NotionalQuote*perpFee/10_000) {
		t.Fatalf("fees %v/%v, want taker bps on each leg's notional", p.EntrySpot.FeeQuote, p.EntryPerp.FeeQuote)
	}
	wantCash := 1_000_000 - notional - p.EntrySpot.FeeQuote - p.EntryPerp.NotionalQuote - p.EntryPerp.FeeQuote
	if !near(l.CashQuote, wantCash) {
		t.Fatalf("cash %v, want %v (spot notional + fees + full perp notional as margin at perp_margin_frac 0)", l.CashQuote, wantCash)
	}
	if p.ProjectedNetAPRFrac != 0.05 || !p.ProjectedNetAPROK || p.JournalCostTotalPct != 0.30 {
		t.Fatal("the row's projected figures must travel onto the position")
	}
}

// PLAN 4.3 acceptance (1): a snapshot AFTER the decision is refused — by a
// millisecond — and one older than the decision's own book-age budget too.
func TestOpen_RefusesSnapshotAfterDecisionOrBeyondBookAge(t *testing.T) {
	cases := []struct {
		name          string
		spotAt        int64
		perpAt        int64
		wantRefusedVI string
	}{
		{"spot book one ms after the decision", t0 + 1, t0 - hourMs, "SAU quyết định"},
		{"perp book after the decision", t0 - hourMs, t0 + 60_000, "SAU quyết định"},
		{"spot book older than max_book_age", t0 - 3*hourMs, t0 - hourMs, "quá hạn"},
		{"book with no sample stamp", 0, t0 - hourMs, "không mang mốc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New(1_000_000)
			err := l.Open(openReq(t0, deepBook("binance_spot", spotMid, tc.spotAt), deepBook("binance_futures", perpMid, tc.perpAt)))
			if err == nil {
				t.Fatal("open accepted")
			}
			if !strings.Contains(err.Error(), tc.wantRefusedVI) {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
			if len(l.OpenPositions()) != 0 || l.CashQuote != 1_000_000 || l.Refusals != 1 {
				t.Fatalf("a refusal must leave the account untouched: %d open, cash %v, refusals %d", len(l.OpenPositions()), l.CashQuote, l.Refusals)
			}
		})
	}
	// Exactly AT the decision instant is allowed: the book existed when the
	// decision was made.
	l := New(1_000_000)
	if err := l.Open(openReq(t0, deepBook("binance_spot", spotMid, t0), deepBook("binance_futures", perpMid, t0))); err != nil {
		t.Fatalf("a book sampled exactly at the decision must price: %v", err)
	}
}

// PLAN 4.3 acceptance (1): a size the measured book cannot absorb inside
// 0.5% is refused like production, and BOTH legs or neither — a thin perp
// book refuses the spot leg too, leaving no half position.
func TestOpen_RefusesBeyondDepthAndNeverOpensOneLeg(t *testing.T) {
	l := New(1_000_000)
	err := l.Open(openReq(t0, deepBook("binance_spot", spotMid, t0-hourMs), thinBook("binance_futures", perpMid, t0-hourMs)))
	if err == nil {
		t.Fatal("a 50k fill on a 10k book was accepted")
	}
	if !strings.Contains(err.Error(), "BÁN perp") {
		t.Fatalf("the refusal must name the leg: %v", err)
	}
	if len(l.OpenPositions()) != 0 || l.CashQuote != 1_000_000 {
		t.Fatal("a refused perp leg must not leave a spot leg open or cash spent")
	}
	// And the same from the other side.
	l = New(1_000_000)
	if err := l.Open(openReq(t0, thinBook("binance_spot", spotMid, t0-hourMs), deepBook("binance_futures", perpMid, t0-hourMs))); err == nil || !strings.Contains(err.Error(), "MUA spot") {
		t.Fatalf("thin spot book: %v", err)
	}
	if len(l.OpenPositions()) != 0 {
		t.Fatal("half position after a spot refusal")
	}
}

func TestOpen_RefusesUnverifiedOrUnknownFeesAndInsufficientCapital(t *testing.T) {
	spot, perp := deepBook("binance_spot", spotMid, t0-hourMs), deepBook("binance_futures", perpMid, t0-hourMs)

	req := openReq(t0, spot, perp)
	req.PerpFee = FeeState{Source: "binance_futures", TakerBps: 5, Verified: false, Known: true}
	if err := New(1_000_000).Open(req); err == nil || !strings.Contains(err.Error(), "chưa xác minh") {
		t.Fatalf("unverified perp fee: %v", err)
	}
	req = openReq(t0, spot, perp)
	req.SpotFee = FeeState{} // a run-1 row: no fee state at all
	if err := New(1_000_000).Open(req); err == nil || !strings.Contains(err.Error(), "không ghi biểu phí") {
		t.Fatalf("unknown spot fee: %v", err)
	}
	// 50k spot + 50k perp margin + fees needs a little over 100k.
	if err := New(100_000).Open(openReq(t0, spot, perp)); err == nil || !strings.Contains(err.Error(), "vốn ảo không đủ") {
		t.Fatalf("insufficient capital: %v", err)
	}
	// With margin at 20% the same account affords it.
	req = openReq(t0, spot, perp)
	req.PerpMarginFrac = 0.2
	l := New(100_000)
	if err := l.Open(req); err != nil {
		t.Fatalf("20%% margin should fit in 100k: %v", err)
	}
	p, _ := l.Position("BTCUSDT", "binance_futures")
	if !near(p.MarginQuote, 0.2*p.EntryPerp.NotionalQuote) {
		t.Fatalf("margin %v, want 20%% of the perp leg's notional", p.MarginQuote)
	}
}

// PLAN 4.3 acceptance (1): funding is credited only at a settlement stamp,
// only while the position was open at it — strictly after the open, never
// pro-rated — and a continuous-model row is never a payment.
func TestCreditFunding_OnlyAtSettlementsThePositionWasOpenAcross(t *testing.T) {
	l := New(1_000_000)
	spot, perp := deepBook("binance_spot", spotMid, t0-hourMs), deepBook("binance_futures", perpMid, t0-hourMs)
	if err := l.Open(openReq(t0, spot, perp)); err != nil {
		t.Fatal(err)
	}
	p, _ := l.Position("BTCUSDT", "binance_futures")
	settle := func(at int64, rate float64, model exchanges.FundingModel) FundingSettlement {
		return FundingSettlement{SettledAtMs: at, Model: model, RatePerIntervalFrac: rate, IntervalSec: 28800,
			MarkPriceQuote: 100_100, MarkSourceVI: "test"}
	}
	cashBefore := l.CashQuote
	if l.CreditFunding("BTCUSDT", "binance_futures", settle(t0-1, 0.0001, exchanges.FundingDiscrete)) {
		t.Fatal("credited a settlement BEFORE the open")
	}
	if l.CreditFunding("BTCUSDT", "binance_futures", settle(t0, 0.0001, exchanges.FundingDiscrete)) {
		t.Fatal("credited the settlement AT the open instant — the position earns from the next one")
	}
	if l.CreditFunding("BTCUSDT", "binance_futures", settle(t0+hourMs, 0.0001, exchanges.FundingContinuous)) {
		t.Fatal("credited a continuous-model sample as a payment")
	}
	if p.FundingContinuousSkipped != 1 {
		t.Fatalf("continuous sample must be counted as skipped, got %d", p.FundingContinuousSkipped)
	}
	unpriced := settle(t0+hourMs, 0.0001, exchanges.FundingDiscrete)
	unpriced.MarkPriceQuote = 0
	if l.CreditFunding("BTCUSDT", "binance_futures", unpriced) {
		// An unpriced settlement (no mark) is a counted gap, never a zero.
		t.Fatal("credited without a mark")
	}
	if p.FundingUnpriced != 1 {
		t.Fatalf("unpriced settlement must be counted, got %d", p.FundingUnpriced)
	}
	if l.CashQuote != cashBefore || p.FundingQuote != 0 {
		t.Fatal("nothing above may have moved money")
	}

	if !l.CreditFunding("BTCUSDT", "binance_futures", settle(t0+8*hourMs, 0.0001, exchanges.FundingDiscrete)) {
		t.Fatal("a discrete settlement after the open was not credited")
	}
	want := 100_100 * p.QtyCoin * 0.0001
	if !near(p.FundingQuote, want) || !near(l.CashQuote-cashBefore, want) {
		t.Fatalf("credited %v (cash +%v), want mark × qty × rate = %v", p.FundingQuote, l.CashQuote-cashBefore, want)
	}
	// A negative rate is money OUT for the short.
	if !l.CreditFunding("BTCUSDT", "binance_futures", settle(t0+16*hourMs, -0.0002, exchanges.FundingDiscrete)) {
		t.Fatal("a negative settlement must still be applied")
	}
	if !near(p.FundingQuote, want-2*want) || p.FundingSettlements != 2 {
		t.Fatalf("after a −2× print funding is %v over %d settlements, want %v over 2", p.FundingQuote, p.FundingSettlements, -want)
	}
	// Closed positions collect nothing.
	if err := l.Close(closeReq(t0+20*hourMs, deepBook("binance_spot", spotMid, t0+19*hourMs), deepBook("binance_futures", perpMid, t0+19*hourMs))); err != nil {
		t.Fatal(err)
	}
	if l.CreditFunding("BTCUSDT", "binance_futures", settle(t0+24*hourMs, 0.0001, exchanges.FundingDiscrete)) {
		t.Fatal("credited a closed position")
	}
}

// PLAN 4.3 acceptance (1): the invariant "both legs open or both closed"
// holds through a refused exit — the position stays open with the reason on
// it, and a later fillable book closes both legs together.
func TestClose_BothLegsOrNeither(t *testing.T) {
	l := New(1_000_000)
	if err := l.Open(openReq(t0, deepBook("binance_spot", spotMid, t0-hourMs), deepBook("binance_futures", perpMid, t0-hourMs))); err != nil {
		t.Fatal(err)
	}
	cashAfterOpen := l.CashQuote
	err := l.Close(closeReq(t0+hourMs, thinBook("binance_spot", spotMid, t0), deepBook("binance_futures", perpMid, t0)))
	if err == nil {
		t.Fatal("closed on a spot book that cannot absorb the size")
	}
	p, still := l.Position("BTCUSDT", "binance_futures")
	if !still || len(l.Closed) != 0 {
		t.Fatal("a refused exit must leave BOTH legs open")
	}
	if p.ExitRefusedAtMs != t0+hourMs || !strings.Contains(p.ExitRefusedVI, "BÁN spot") {
		t.Fatalf("the refusal must be on the position: %q at %d", p.ExitRefusedVI, p.ExitRefusedAtMs)
	}
	if l.CashQuote != cashAfterOpen {
		t.Fatal("a refused exit moved cash")
	}

	if err := l.Close(closeReq(t0+2*hourMs, deepBook("binance_spot", spotMid, t0+hourMs), deepBook("binance_futures", perpMid, t0+hourMs))); err != nil {
		t.Fatalf("second exit on a deep book: %v", err)
	}
	if _, open := l.Position("BTCUSDT", "binance_futures"); open || len(l.Closed) != 1 {
		t.Fatal("after a filled exit the position must be closed, once")
	}
	closed := l.Closed[0]
	if closed.ExitRefusedVI != "" || closed.ExitSpot.Side != strategy.SideSell || closed.ExitPerp.Side != strategy.SideBuy {
		t.Fatalf("exit legs: spot %s, perp %s, refusal %q", closed.ExitSpot.Side, closed.ExitPerp.Side, closed.ExitRefusedVI)
	}
	// Realized P&L with flat prices and no funding is exactly minus the four
	// fills' slippage and fees.
	wantLoss := closed.EntrySpot.FeeQuote + closed.EntryPerp.FeeQuote + closed.ExitSpot.FeeQuote + closed.ExitPerp.FeeQuote +
		closed.QtyCoin*(closed.EntrySpot.FillPriceQuote-closed.ExitSpot.FillPriceQuote) +
		closed.QtyCoin*(closed.ExitPerp.FillPriceQuote-closed.EntryPerp.FillPriceQuote)
	if !near(closed.RealizedPnLQuote, -wantLoss) {
		t.Fatalf("realized %v, want −(fees + slippage) = %v", closed.RealizedPnLQuote, -wantLoss)
	}
	if !near(l.CashQuote, 1_000_000+closed.RealizedPnLQuote) {
		t.Fatalf("cash %v, want capital + realized %v", l.CashQuote, 1_000_000+closed.RealizedPnLQuote)
	}
	if closed.PaperRoundTripCostPct <= closed.PaperEntryCostPct {
		t.Fatal("the round trip must add the exit half")
	}
}

// The mark is delta-neutral in the coin: both legs moving together leaves
// equity where it was; only the basis moving against the entry costs money,
// and a leg with no price keeps its last mark and is flagged stale.
func TestMark_IsCoinFlatAndShowsBasisMoves(t *testing.T) {
	l := New(1_000_000)
	if err := l.Open(openReq(t0, deepBook("binance_spot", spotMid, t0-hourMs), deepBook("binance_futures", perpMid, t0-hourMs))); err != nil {
		t.Fatal(err)
	}
	p, _ := l.Position("BTCUSDT", "binance_futures")
	prices := map[string]float64{"binance_spot": spotMid, "binance_futures": perpMid}
	priceFn := func(source, symbol string) (float64, bool) {
		v, ok := prices[source]
		return v, ok
	}

	at := l.Mark(t0, priceFn)
	entryCost := p.EntrySpot.FeeQuote + p.EntryPerp.FeeQuote +
		p.QtyCoin*(p.EntrySpot.FillPriceQuote-spotMid) + p.QtyCoin*(perpMid-p.EntryPerp.FillPriceQuote)
	if !near(at.EquityQuote, 1_000_000-entryCost) {
		t.Fatalf("equity at open %v, want capital − entry fees − entry slippage = %v", at.EquityQuote, 1_000_000-entryCost)
	}

	prices["binance_spot"], prices["binance_futures"] = spotMid*1.1, perpMid*1.1 // both up 10%
	up := l.Mark(t0+hourMs, priceFn)
	basisMove := p.QtyCoin * ((perpMid*1.1 - spotMid*1.1) - (perpMid - spotMid))
	if !near(up.EquityQuote, at.EquityQuote-basisMove) {
		t.Fatalf("equity after a parallel 10%% move %v, want %v (only the basis widening by the move counts)", up.EquityQuote, at.EquityQuote-basisMove)
	}

	prices["binance_spot"], prices["binance_futures"] = spotMid, perpMid+500 // perp alone up 500: basis widens against the short
	widened := l.Mark(t0+2*hourMs, priceFn)
	if !near(widened.EquityQuote, at.EquityQuote-p.QtyCoin*500) {
		t.Fatalf("equity with the basis 500 wider %v, want %v", widened.EquityQuote, at.EquityQuote-p.QtyCoin*500)
	}
	if !near(p.BasisPnLQuote, -p.QtyCoin*500-(entryCost-p.EntrySpot.FeeQuote-p.EntryPerp.FeeQuote)) {
		t.Fatalf("basis P&L %v must carry the entry slippage and the 500 move", p.BasisPnLQuote)
	}
	if widened.DrawdownFromPeakQuote <= 0 {
		t.Fatal("drawdown from peak must be positive after the basis widened")
	}

	delete(prices, "binance_futures")
	stale := l.Mark(t0+3*hourMs, priceFn)
	if stale.StaleMarks != 1 || !p.MarkIsStale || p.PerpMarkQuote != perpMid+500 {
		t.Fatalf("a missing perp price must keep the last mark and be flagged: stale=%d flag=%v mark=%v", stale.StaleMarks, p.MarkIsStale, p.PerpMarkQuote)
	}
	if len(l.Equity) != 4 {
		t.Fatalf("every mark is one equity point, got %d", len(l.Equity))
	}
}

func TestOpenAndClose_JournalAnomaliesAreRecordedNeverDoubled(t *testing.T) {
	l := New(1_000_000)
	books := func(at int64) (depth.Summary, depth.Summary) {
		return deepBook("binance_spot", spotMid, at), deepBook("binance_futures", perpMid, at)
	}
	s, p := books(t0 - hourMs)
	if err := l.Open(openReq(t0, s, p)); err != nil {
		t.Fatal(err)
	}
	if err := l.Open(openReq(t0+hourMs, s, p)); err == nil {
		t.Fatal("a second enter while holding opened a second position")
	}
	if len(l.OpenPositions()) != 1 {
		t.Fatal("doubled")
	}
	if err := l.Close(CloseRequest{Symbol: "ETHUSDT", PerpSource: "binance_futures", AtMs: t0 + hourMs}); err == nil {
		t.Fatal("an exit on a pair the ledger does not hold was accepted")
	}
	anomalies := 0
	for _, e := range l.Events {
		if e.Kind == EventAnomaly {
			anomalies++
		}
	}
	if anomalies != 2 {
		t.Fatalf("both anomalies must be in the event log, got %d", anomalies)
	}
	if p2, _ := l.Position("BTCUSDT", "binance_futures"); p2.HeldDays(t0+24*hourMs) != 1 {
		t.Fatalf("held days %v, want 1", p2.HeldDays(t0+24*hourMs))
	}
}

func TestOpen_FlagsQuoteBridgedPairs(t *testing.T) {
	l := New(1_000_000)
	req := openReq(t0, deepBook("binance_spot", spotMid, t0-hourMs), deepBook("kraken_futures", perpMid, t0-hourMs))
	req.PerpSource, req.PerpQuoteAsset = "kraken_futures", "USD"
	req.PerpFee = verified("kraken_futures", 5)
	if err := l.Open(req); err != nil {
		t.Fatal(err)
	}
	p, _ := l.Position("BTCUSDT", "kraken_futures")
	if !p.QuoteBridged {
		t.Fatal("a USD perp against a USDT spot must be flagged quote-bridged")
	}
}

// Review finding of 2026-09-11 (blocking): the exit legs must be sized at
// the coin quantity's CURRENT value, not at the notional the position was
// opened with — after a rally the same coins are a bigger order, and the
// production depth gate has to be asked about THAT order.
func TestClose_SizesTheExitAtTheCurrentValueNotTheEntryNotional(t *testing.T) {
	l := New(1_000_000)
	if err := l.Open(openReq(t0, deepBook("binance_spot", spotMid, t0-hourMs), deepBook("binance_futures", perpMid, t0-hourMs))); err != nil {
		t.Fatal(err)
	}
	p, _ := l.Position("BTCUSDT", "binance_futures")
	// Price up 30%: the 0.5 BTC is now a ~65k order. Books hold 60k inside
	// 0.5% — enough for the entry notional, not for the current value.
	sixtyK := func(source string, mid float64, at int64) depth.Summary {
		b := deepBook(source, mid, at)
		b.BidDepthWithinTightQuote, b.AskDepthWithinTightQuote = 30_000, 30_000
		b.BidDepthWithinWideQuote, b.AskDepthWithinWideQuote = 60_000, 60_000
		return b
	}
	if strategy.EstimateFill(sixtyK("binance_spot", spotMid*1.3, t0), strategy.SideSell, notional).Fillable == false {
		t.Fatal("fixture: the entry notional must fit the 60k book, or the test proves nothing")
	}
	err := l.Close(closeReq(t0+hourMs, sixtyK("binance_spot", spotMid*1.3, t0), sixtyK("binance_futures", perpMid*1.3, t0)))
	if err == nil {
		t.Fatalf("closed %.0f quote of coins on a 60k book because the entry notional was 50k", p.QtyCoin*spotMid*1.3)
	}
	if !strings.Contains(err.Error(), "BÁN spot") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if _, open := l.Position("BTCUSDT", "binance_futures"); !open {
		t.Fatal("a refused exit must leave the position open")
	}
	// Deep books at the new price close it, and the exit leg's notional is
	// the current value of the coins.
	if err := l.Close(closeReq(t0+2*hourMs, deepBook("binance_spot", spotMid*1.3, t0+hourMs), deepBook("binance_futures", perpMid*1.3, t0+hourMs))); err != nil {
		t.Fatal(err)
	}
	closed := l.Closed[0]
	if !near(closed.ExitSpot.NotionalQuote, closed.QtyCoin*closed.ExitSpot.FillPriceQuote) || closed.ExitSpot.NotionalQuote < notional*1.29 {
		t.Fatalf("exit spot notional %v, want qty × fill at the new price (≈ %v)", closed.ExitSpot.NotionalQuote, closed.QtyCoin*spotMid*1.3)
	}
}
