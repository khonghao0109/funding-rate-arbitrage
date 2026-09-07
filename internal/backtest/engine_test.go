package backtest

import (
	"math"
	"strconv"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/fees"
	"futures-arbitrage-scanner/internal/strategy"
)

const (
	secPer8h = 8 * 3600
	msPerSec = 1000
	epoch    = int64(1_750_000_000_000) // an arbitrary but fixed start
)

// discreteSeries builds a settled funding series at a fixed cadence, oldest
// first, from rates quoted per 8h in basis points.
func discreteSeries(source string, intervalSec int64, ratesPer8hBps ...float64) []exchanges.FundingHistoryEntry {
	out := make([]exchanges.FundingHistoryEntry, 0, len(ratesPer8hBps))
	for i, bps := range ratesPer8hBps {
		per8h := bps / 10000
		out = append(out, exchanges.FundingHistoryEntry{
			Symbol: "BTCUSDT", Source: source, Model: exchanges.FundingDiscrete,
			SettledAtMs:         epoch + int64(i)*intervalSec*msPerSec,
			RatePerIntervalFrac: per8h * float64(intervalSec) / secPer8h,
			IntervalSec:         intervalSec,
			RatePer8hFrac:       per8h,
			GapPrevSec:          intervalSec,
		})
	}
	return out
}

// deepBook is liquid enough that slippage is not what these tests measure.
func deepBook(source string) depth.Summary {
	return depth.Summary{
		Source: source, Symbol: "BTCUSDT",
		MidPriceQuote: 100, BestBidQuote: 99.999, BestAskQuote: 100.001, SpreadPct: 0.002,
		BidDepthWithinTightQuote: 50_000_000, AskDepthWithinTightQuote: 50_000_000,
		BidDepthWithinWideQuote: 250_000_000, AskDepthWithinWideQuote: 250_000_000,
		BidSpanPct: 1.0, AskSpanPct: 1.0,
	}
}

func verifiedFee(source string, takerBps float64) fees.Schedule {
	return fees.Schedule{Source: source, TakerFeeBps: takerBps, Verified: true}
}

func seriesOf(entries []exchanges.FundingHistoryEntry) Series {
	return Series{
		Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot",
		Settled: entries,
		SpotFee: verifiedFee("binance_spot", 10), PerpFee: verifiedFee("binance_futures", 5),
		SpotBook: deepBook("binance_spot"), PerpBook: deepBook("binance_futures"),
	}
}

func testParams() strategy.Params {
	return strategy.Params{
		MinRatePer8hBps: 1.0, PersistencePeriods: 3, MinNetAPRFrac: 0.02,
		NotionalQuote: 50_000, HoldingDays: 30,
		ExitNetAPRFrac: -1.0, ExitPersistencePeriods: 1,
		MaxBasisPct: 1.0, MaxBasisWidenPct: 0.5,
	}
}

func fullWindow(entries []exchanges.FundingHistoryEntry) Window {
	return Window{FromMs: entries[0].SettledAtMs, ToMs: entries[len(entries)-1].SettledAtMs + 1}
}

// CLAUDE.md rule 6: funding is a discrete event. The engine must COUNT the
// settlements a position was open across — never multiply a rate by elapsed
// time — and a position opened at settlement i earns from i+1 onward.
func TestRun_CountsSettlementsNotElapsedTime(t *testing.T) {
	// Three periods build persistence, entry fires on the third, then four more
	// settlements pay while the position is open.
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2, 2, 2)
	got := Run(seriesOf(entries), fullWindow(entries), testParams())
	if !got.OK {
		t.Fatalf("Run: %s", got.ReasonVI)
	}
	if len(got.Trades) != 1 {
		t.Fatalf("want exactly one open trade, got %d", len(got.Trades))
	}
	trade := got.Trades[0]

	// Entry is decided on the 3rd settlement (index 2), so the position is open
	// across indices 3..6 — four settlements at 2 bps/8h = 0.0002 each.
	if trade.Settlements != 4 {
		t.Errorf("settlements = %d, want 4 (opened at index 2, series ends at index 6)", trade.Settlements)
	}
	if want := 4 * 0.0002; math.Abs(trade.FundingFrac-want) > 1e-12 {
		t.Errorf("funding = %.12f, want %.12f", trade.FundingFrac, want)
	}
}

func TestRun_ChargesTheRoundTripOncePerTrade(t *testing.T) {
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2)
	got := Run(seriesOf(entries), fullWindow(entries), testParams())
	if !got.OK || len(got.Trades) != 1 {
		t.Fatalf("want one trade: %s", got.ReasonVI)
	}
	// 10 bps + 5 bps, entry and exit = 30 bps = 0.30% = 0.003 as a fraction,
	// plus a little slippage on a deep book.
	trade := got.Trades[0]
	if trade.CostFrac < 0.003 || trade.CostFrac > 0.0031 {
		t.Errorf("cost = %.6f, want ~0.0030 (four taker fills, charged once)", trade.CostFrac)
	}
	if math.Abs(trade.NetFrac-(trade.FundingFrac-trade.CostFrac)) > 1e-12 {
		t.Errorf("net %.12f is not funding %.12f minus cost %.12f",
			trade.NetFrac, trade.FundingFrac, trade.CostFrac)
	}
}

// A continuous series is a sample of a funding INDEX, not a settlement list.
// Counting its rows as payments credits 8,760 a year on a venue that settles
// nothing at all.
func TestRun_RefusesAContinuousSeries(t *testing.T) {
	entries := discreteSeries("paradex_futures", secPer8h, 2, 2, 2, 2, 2)
	for i := range entries {
		entries[i].Model = exchanges.FundingContinuous
	}
	series := seriesOf(entries)
	series.PerpSource = "paradex_futures"

	got := Run(series, fullWindow(entries), testParams())
	if got.OK {
		t.Fatal("a settlement-counting engine must refuse a continuous series, not count it")
	}
	if got.ReasonVI == "" {
		t.Error("the refusal must name the reason")
	}
	if len(got.Trades) != 0 || got.TotalReturnFrac != 0 {
		t.Errorf("a refused run must produce no figures: %d trades, return %v",
			len(got.Trades), got.TotalReturnFrac)
	}
}

// Binance labels dividend-driven rates "Special"; they are not a funding regime
// and must not reach either the signal or the accrual.
func TestRun_ExcludesSpecialRatesFromAccrual(t *testing.T) {
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2, 2, 2)
	// A huge dividend rate in the middle. If it were accrued it would dominate.
	entries[4].RateType = "Special"
	entries[4].RatePer8hFrac = 0.05
	entries[4].RatePerIntervalFrac = 0.05

	got := Run(seriesOf(entries), fullWindow(entries), testParams())
	if !got.OK || len(got.Trades) != 1 {
		t.Fatalf("want one trade: %s", got.ReasonVI)
	}
	if got.Trades[0].FundingFrac > 0.01 {
		t.Errorf("funding = %v — the Special rate was accrued; it must be filtered",
			got.Trades[0].FundingFrac)
	}
}

func TestRun_CountsFundingSignReversals(t *testing.T) {
	// + + - - + -  → three sign changes.
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, -1, -1, 2, -1)
	got := Run(seriesOf(entries), fullWindow(entries), testParams())
	if !got.OK {
		t.Fatalf("Run: %s", got.ReasonVI)
	}
	if got.FundingReversals != 3 {
		t.Errorf("reversals = %d, want 3", got.FundingReversals)
	}
}

func TestRun_MaxDrawdownIsMeasuredOnTheEquityCurve(t *testing.T) {
	// Rise, then a sustained negative stretch, then recovery.
	entries := discreteSeries("binance_futures", secPer8h, 3, 3, 3, 3, -8, -8, 3, 3)
	got := Run(seriesOf(entries), fullWindow(entries), testParams())
	if !got.OK {
		t.Fatalf("Run: %s", got.ReasonVI)
	}
	if got.MaxDrawdownFrac <= 0 {
		t.Error("a series that goes negative mid-run must show a drawdown")
	}
	if got.MaxDrawdownFrac > 1 {
		t.Errorf("drawdown = %v: a fraction of notional cannot exceed 1 here", got.MaxDrawdownFrac)
	}
}

// The basis exit needs a spot and a perp price at the SAME past instant, and
// price_snapshots holds hours, not months. The engine must let the check say so
// rather than feeding it an invented number — and must count how often that
// happened, so a report cannot imply the condition was tested.
func TestRun_ReportsThatTheBasisExitWasNeverEvaluable(t *testing.T) {
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2)
	got := Run(seriesOf(entries), fullWindow(entries), testParams())
	if !got.OK {
		t.Fatalf("Run: %s", got.ReasonVI)
	}
	if got.BasisNotEvaluable == 0 {
		t.Error("with no historical prices the basis exit is never evaluable, and the report must say so")
	}
	var mentions bool
	for _, line := range got.AssumptionsVI {
		if len(line) > 0 {
			mentions = true
		}
	}
	if !mentions {
		t.Error("a run built on stated assumptions must carry them with the result")
	}
}

func TestRun_FlagsAWindowTheCorpusCannotCover(t *testing.T) {
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2)
	// Ask for a window starting a year before the series begins.
	window := Window{FromMs: entries[0].SettledAtMs - 365*24*3600*msPerSec, ToMs: entries[len(entries)-1].SettledAtMs + 1}

	got := Run(seriesOf(entries), window, testParams())
	if !got.CoverageShort {
		t.Error("a window wider than the series must be flagged, not silently truncated")
	}
	if got.CoverageNoteVI == "" {
		t.Error("the coverage shortfall must be stated in words")
	}
}

func TestRun_RealizedAPRAnnualizesTheCoveredSpanNotTheHold(t *testing.T) {
	// 90 settlements at 8h = 30 days exactly.
	rates := make([]float64, 93)
	for i := range rates {
		rates[i] = 2
	}
	entries := discreteSeries("binance_futures", secPer8h, rates...)
	got := Run(seriesOf(entries), fullWindow(entries), testParams())
	if !got.OK {
		t.Fatalf("Run: %s", got.ReasonVI)
	}
	// Annualized over the span the corpus covered — here the whole window —
	// and NEVER over the holding period, which would be rule 6 backwards.
	want := got.TotalReturnFrac * 365 / got.CoveredDays
	if math.Abs(got.RealizedAPRFrac-want) > 1e-9 {
		t.Errorf("realized APR = %v, want %v (total return annualized over the covered span)",
			got.RealizedAPRFrac, want)
	}
	if got.CoveredDays < 30.9 || got.CoveredDays > 31.1 {
		t.Errorf("covered days = %.3f, want 31 (93 settlements x 8h)", got.CoveredDays)
	}
	// 2 bps/8h is 1095 settlements x 0.0002 = 21.9% a year gross, so the
	// realized figure must land BELOW that: a backtest that matches or beats
	// the gross rate has lost its costs somewhere. (These are invented rates —
	// the 5-15% band CLAUDE.md states is checked against the real corpus in
	// the acceptance run, not here.)
	if got.RealizedAPRFrac > 0.219 {
		t.Errorf("realized APR %.4f exceeds the gross rate 21.9%% — costs went missing",
			got.RealizedAPRFrac)
	}
	if got.RealizedAPRFrac <= 0 {
		t.Errorf("realized APR %.4f: a 21.9%% gross series must survive one round trip", got.RealizedAPRFrac)
	}
}

func TestSweep_RunsEveryCombinationAndIsDeterministic(t *testing.T) {
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2, 2)
	series := []Series{seriesOf(entries)}
	grid := []strategy.Params{}
	for _, minBps := range []float64{0.5, 1.0, 1.5, 2.5} {
		p := testParams()
		p.MinRatePer8hBps = minBps
		grid = append(grid, p)
	}

	first := Sweep(series, fullWindow(entries), grid)
	if len(first) != len(grid) {
		t.Fatalf("sweep produced %d results for %d parameter sets", len(first), len(grid))
	}
	// Order must not depend on which goroutine finished first.
	second := Sweep(series, fullWindow(entries), grid)
	for i := range first {
		if first[i].Params.MinRatePer8hBps != second[i].Params.MinRatePer8hBps {
			t.Fatalf("result %d: %v then %v — sweep output must be deterministic",
				i, first[i].Params.MinRatePer8hBps, second[i].Params.MinRatePer8hBps)
		}
	}
	// A threshold above every rate in the series can never enter.
	last := first[len(first)-1]
	if len(last.Trades) != 0 {
		t.Errorf("threshold 2.5 bps against a 2 bps series must never enter, got %d trades", len(last.Trades))
	}
}

func TestRun_RefusesEmptyOrUnusableInput(t *testing.T) {
	if got := Run(seriesOf(nil), Window{FromMs: 0, ToMs: 1}, testParams()); got.OK {
		t.Error("no settlements means no backtest")
	}
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2)
	if got := Run(seriesOf(entries), Window{FromMs: 100, ToMs: 100}, testParams()); got.OK {
		t.Error("an empty window must be refused")
	}
}

var _ = time.Second

func TestRun_ForcedCloseAtWindowEndCountsInDrawdown(t *testing.T) {
	// Entry fires on the 3rd settlement and the window ends right after: the
	// trade is force-closed, pays a full round trip and collected nothing. That
	// loss IS the drawdown; a report that showed 0.0000% would be lying.
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2)
	got := Run(seriesOf(entries), fullWindow(entries), testParams())
	if !got.OK || len(got.Trades) != 1 {
		t.Fatalf("want one forced-closed trade: %s", got.ReasonVI)
	}
	if got.Trades[0].Settlements != 0 {
		t.Errorf("settlements = %d, want 0 — nothing settled after the entry", got.Trades[0].Settlements)
	}
	if got.MaxDrawdownFrac < 0.003 {
		t.Errorf("max drawdown = %.6f, want the round trip (~0.0030): the forced close was not charged to the curve",
			got.MaxDrawdownFrac)
	}
}

func TestRun_ForcedCloseIsStampedAtTheLastInWindowSettlement(t *testing.T) {
	// Ten rows, a window covering only indices 2..6: rows 7..9 are lookback
	// beyond the window and must not lend their date to the close.
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2)
	window := Window{FromMs: entries[2].SettledAtMs, ToMs: entries[6].SettledAtMs + 1}
	got := Run(seriesOf(entries), window, testParams())
	if !got.OK || len(got.Trades) != 1 {
		t.Fatalf("want one trade: %s", got.ReasonVI)
	}
	if got.Trades[0].CloseAtMs != entries[6].SettledAtMs {
		t.Errorf("close stamped at index %d's time, want index 6 (the last settlement inside the window)",
			indexOf(entries, got.Trades[0].CloseAtMs))
	}
	if got.Settlements != 5 {
		t.Errorf("settlements in window = %d, want 5", got.Settlements)
	}
}

func indexOf(entries []exchanges.FundingHistoryEntry, stampMs int64) int {
	for i, e := range entries {
		if e.SettledAtMs == stampMs {
			return i
		}
	}
	return -1
}

// A series whose round trip cannot be priced must be REFUSED, not reported as
// "traded nothing": 12 of the 16 shipped series carry an unverified fee
// schedule, and they used to read identically to a strategy that found nothing.
func TestRun_RefusesASeriesWhoseCostCannotBePriced(t *testing.T) {
	entries := discreteSeries("bybit_futures", secPer8h, 2, 2, 2, 2, 2)
	series := seriesOf(entries)
	series.PerpFee = fees.Schedule{Source: "bybit_futures"} // never looked up
	got := Run(series, fullWindow(entries), testParams())
	if got.OK {
		t.Fatal("an unpriceable cost must refuse the run, not silently trade nothing at zero cost")
	}
	if got.ReasonVI == "" {
		t.Error("the refusal must name the reason")
	}
}

func TestRun_SignalExitIsPinnedToTheSettlementThatFiredIt(t *testing.T) {
	// Entry on index 2; funding turns negative at index 5, which is the first
	// settlement the position PAYS and the one whose settled rate fires the
	// exit. So: settlements 3..5 = 3, close stamped at index 5.
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2, -1, 2, 2)
	got := Run(seriesOf(entries), fullWindow(entries), testParams())
	if !got.OK || len(got.Trades) < 1 {
		t.Fatalf("want at least one trade: %s", got.ReasonVI)
	}
	first := got.Trades[0]
	if first.Settlements != 3 || first.CloseAtMs != entries[5].SettledAtMs {
		t.Errorf("first trade: %d settlements closed at index %d; want 3 settlements closed at index 5",
			first.Settlements, indexOf(entries, first.CloseAtMs))
	}
	// 2+2-1 bps over three 8h settlements = 0.0003, minus the round trip.
	wantFunding := (0.0002 + 0.0002 - 0.0001)
	if math.Abs(first.FundingFrac-wantFunding) > 1e-12 {
		t.Errorf("funding = %.12f, want %.12f (the negative settlement IS paid before the exit)", first.FundingFrac, wantFunding)
	}
	// Drawdown after that trade: the round trip less the tiny net funding.
	if got.MaxDrawdownFrac < first.CostFrac-wantFunding-1e-9 {
		t.Errorf("max drawdown = %.6f, want at least %.6f", got.MaxDrawdownFrac, first.CostFrac-wantFunding)
	}
}

func TestRun_CoverageIsShortOnlyWhenTheCorpusReallyIs(t *testing.T) {
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2, 2)
	// A window ending a few hours after the newest settlement — what "now"
	// always looks like from the CLI — is fully covered.
	window := Window{FromMs: entries[0].SettledAtMs, ToMs: entries[len(entries)-1].SettledAtMs + 3*3600*msPerSec}
	if got := Run(seriesOf(entries), window, testParams()); got.CoverageShort {
		t.Errorf("a window ending within one interval of the newest settlement is NOT short: %s", got.CoverageNoteVI)
	}
	// Starting a month before the corpus does IS short.
	window.FromMs -= 30 * 24 * 3600 * msPerSec
	if got := Run(seriesOf(entries), window, testParams()); !got.CoverageShort {
		t.Error("a window starting a month before the oldest settlement must be flagged")
	}
}

func TestRun_RealizedAPRIsOverTheCoveredSpan(t *testing.T) {
	// 90 settlements = 30 days of data, asked for over a 180-day window.
	rates := make([]float64, 91)
	for i := range rates {
		rates[i] = 2
	}
	entries := discreteSeries("binance_futures", secPer8h, rates...)
	window := Window{FromMs: entries[0].SettledAtMs - 150*24*3600*msPerSec, ToMs: entries[len(entries)-1].SettledAtMs + 1}
	got := Run(seriesOf(entries), window, testParams())
	if !got.OK {
		t.Fatalf("Run: %s", got.ReasonVI)
	}
	if got.CoveredDays < 30 || got.CoveredDays > 31 {
		t.Errorf("covered days = %.2f, want ~30.33 (91 settlements at 8h)", got.CoveredDays)
	}
	if want := got.TotalReturnFrac * 365 / got.CoveredDays; math.Abs(got.RealizedAPRFrac-want) > 1e-9 {
		t.Errorf("realized APR = %v, want %v — annualized over the COVERED span, not the 180-day window",
			got.RealizedAPRFrac, want)
	}
}

func TestSweep_KeepsSeriesMajorOrderAcrossTwoSeries(t *testing.T) {
	a := seriesOf(discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2))
	b := seriesOf(discreteSeries("binance_futures", secPer8h, 3, 3, 3, 3))
	b.Symbol = "ETHUSDT"
	grid := []strategy.Params{testParams(), testParams()}
	grid[1].MinRatePer8hBps = 2.5
	got := Sweep([]Series{a, b}, fullWindow(a.Settled), grid)
	want := []string{"BTCUSDT/1", "BTCUSDT/2.5", "ETHUSDT/1", "ETHUSDT/2.5"}
	for i, r := range got {
		key := r.Symbol + "/" + strconv.FormatFloat(r.Params.MinRatePer8hBps, 'g', -1, 64)
		if key != want[i] {
			t.Fatalf("result %d = %s, want %s (series-major, grid order)", i, key, want[i])
		}
	}
}

func TestSortByRealizedAPR_PutsTradedRunsAboveUntradedOnes(t *testing.T) {
	traded := Result{OK: true, RealizedAPRFrac: -0.05, Trades: []Trade{{}}}
	untraded := Result{OK: true, RealizedAPRFrac: 0}
	refused := Result{OK: false}
	results := []Result{untraded, refused, traded}
	SortByRealizedAPR(results)
	if len(results[0].Trades) == 0 || results[2].OK {
		t.Errorf("order = [%v %v %v]; want traded, untraded, refused",
			len(results[0].Trades) > 0, len(results[1].Trades) > 0, results[2].OK)
	}
}

// The behavioural half of the Q8 guarantee: replaying step by step through the
// production functions must reproduce Run's trades exactly. The AST tests in
// contract_test.go are tripwires on names; this one is on behaviour, and an
// engine that added a private pre-filter before EvaluateEntry would fail here.
func TestRun_MatchesAStepByStepReplayThroughTheProductionRules(t *testing.T) {
	entries := discreteSeries("binance_futures", secPer8h, 2, 2, 2, 2, 2, -1, 2, 2, 2, 2, 2, -1, 2)
	series := seriesOf(entries)
	params := testParams()
	got := Run(series, fullWindow(entries), params)
	if !got.OK {
		t.Fatalf("Run: %s", got.ReasonVI)
	}

	usable, _ := strategy.UsableSettled(entries)
	var want []Trade
	open := false
	var pos strategy.Position
	var trade Trade
	for i, e := range usable {
		at := time.UnixMilli(e.SettledAtMs)
		if open {
			trade.Settlements++
			trade.FundingFrac += e.RatePerIntervalFrac
		}
		c := strategy.Candidate{Symbol: series.Symbol, PerpSource: series.PerpSource, SpotSource: series.SpotSource,
			Settled: usable[:i+1], SpotFee: series.SpotFee, PerpFee: series.PerpFee,
			SpotBook: series.SpotBook, PerpBook: series.PerpBook}
		if !open {
			if strategy.EvaluateEntry(at, c, params).Action == strategy.ActionEnter {
				open, pos = true, strategy.Position{Symbol: c.Symbol, PerpSource: c.PerpSource, SpotSource: c.SpotSource,
					OpenedAtMs: e.SettledAtMs, NotionalQuote: params.NotionalQuote}
				trade = Trade{OpenAtMs: e.SettledAtMs}
			}
			continue
		}
		if strategy.EvaluateExit(at, pos, c, params).Action == strategy.ActionExit {
			trade.CloseAtMs = e.SettledAtMs
			want = append(want, trade)
			open = false
		}
	}
	if open {
		trade.CloseAtMs = usable[len(usable)-1].SettledAtMs
		want = append(want, trade)
	}

	if len(got.Trades) != len(want) {
		t.Fatalf("Run produced %d trades, the step-by-step replay %d", len(got.Trades), len(want))
	}
	for i := range want {
		g, w := got.Trades[i], want[i]
		if g.OpenAtMs != w.OpenAtMs || g.CloseAtMs != w.CloseAtMs || g.Settlements != w.Settlements ||
			math.Abs(g.FundingFrac-w.FundingFrac) > 1e-12 {
			t.Errorf("trade %d differs: Run %+v vs replay %+v", i, g, w)
		}
	}
}
