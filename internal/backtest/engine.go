package backtest

import (
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/fees"
	"futures-arbitrage-scanner/internal/strategy"
)

// The replay engine (step 3.3).
//
// It walks a venue's SETTLED funding series in time order and, at every
// settlement, asks package strategy the same question the live path asks:
// EvaluateEntry when flat, EvaluateExit when holding. There is no second copy
// of the rules here and there must never be one — docs/PLAN.md step 3.5 gates
// the project on this engine's output matching live paper trading, and a gate
// between two implementations cannot tell "the strategy is wrong" from "the two
// implementations drifted" (Q8, §7.1).
//
// # What this backtest is, and what it is not
//
// Measured 2026-09-04 on the shipped corpus: funding_history holds up to 365
// days, but depth_snapshots holds ONE sample, price_snapshots two hours, and
// instrument_snapshots one day. Depth cannot be backfilled — a book is gone the
// instant it changes — so this is a permanent boundary, not a gap that fills in
// later.
//
// Therefore:
//
//   - The funding PATH is real history.
//   - The COST is a stated assumption: one measured book, held fixed across the
//     window. Today's book is not the book of a stressed market, and a stressed
//     market is exactly when funding turns and a position has to leave
//     (docs/PLAN.md §7.4 item 2). Result.AssumptionsVI carries this to every
//     reader.
//   - The basis exit cannot be evaluated at all, because it needs a spot and a
//     perp price at the same past instant. The engine passes no prices, so the
//     check reports its own inability instead of being fed an invented number,
//     and Result.BasisNotEvaluable counts how often that happened.
//
// A reader who wants a fill simulation needs stored order book LEVELS first,
// and that decision can only ever apply forward.

const (
	msPerSecond = 1000
	secPerDay   = 86400
	daysPerYear = 365.0
)

// Series is one hedgeable combination's inputs for a replay.
//
// Settled must be OLDEST FIRST, which is the order store.FundingHistory and
// every venue fetcher produce.
type Series struct {
	Symbol     string
	PerpSource string
	SpotSource string

	Settled []exchanges.FundingHistoryEntry

	SpotFee fees.Schedule
	PerpFee fees.Schedule

	// The books the cost is priced against. ONE measurement, held fixed for the
	// whole window — see the header.
	SpotBook depth.Summary
	PerpBook depth.Summary
}

// Window is the closed-open replay range.
type Window struct {
	FromMs int64
	ToMs   int64
}

// Trade is one completed round trip.
type Trade struct {
	OpenAtMs  int64
	CloseAtMs int64

	// Settlements is how many funding payments the position was open ACROSS —
	// counted, never derived from elapsed time (CLAUDE.md rule 6). A position
	// opened on the decision at settlement i earns from i+1 onward.
	Settlements int

	FundingFrac float64 // collected, as a fraction of notional
	CostFrac    float64 // the four fills, charged once
	NetFrac     float64 // FundingFrac - CostFrac

	ExitReasonVI string
}

// Result is one replay's outcome and the assumptions it rests on.
type Result struct {
	Symbol     string
	PerpSource string
	SpotSource string
	Params     strategy.Params
	Window     Window

	Settlements int
	Trades      []Trade

	// CoveredDays is the span the corpus actually filled inside the window —
	// what RealizedAPRFrac is annualized over. OKX holds ~3 months; dividing
	// its return by a 6-month window halves its APR against Binance's and then
	// ranks on that, which is the "a year against a quarter" comparison this
	// package warns about, embodied in the number itself.
	CoveredDays     float64
	TotalReturnFrac float64
	RealizedAPRFrac float64
	MaxDrawdownFrac float64

	// RoundTripCostPct is the ONE cost every trade in this run was charged,
	// and CostBookSampledAtMs is when the (older of the two) book it was
	// priced on was measured. Both travel to the CSV: a run's figures cannot
	// be re-derived without them, and the CSV is the artifact that gets
	// detached from everything else.
	RoundTripCostPct    float64
	CostBookSampledAtMs int64

	FundingReversals  int
	PeriodsInPosition int
	// PositiveFundingPeriodShare is the share of held settlements whose rate
	// was POSITIVE — a GROSS fact about the regime, not about profit: a held
	// period with positive funding still loses once the round trip is charged.
	// It is therefore not called "profitable" anywhere.
	PositiveFundingPeriodShare float64
	// DroppedSpecial is how many Binance "Special" (dividend) rows the replay
	// excluded, so a run can say the filter did something — or that the corpus
	// gave it nothing to do (today: zero such rows exist).
	DroppedSpecial int

	// BasisNotEvaluable counts exit evaluations where the basis condition could
	// not be judged for lack of historical prices. A report that omitted this
	// would imply the condition had been tested.
	BasisNotEvaluable int

	// CoverageShort marks a window the corpus does not fill. Three venues cap
	// their published history (OKX ~3 months, Gate 180 days, Paradex none), so
	// comparing two venues over "the same" window can silently compare a year
	// against a quarter.
	CoverageShort  bool
	CoverageNoteVI string

	OK            bool
	ReasonVI      string
	AssumptionsVI []string
}

// assumptions is what every result must carry, in words.
func assumptions(series Series, params strategy.Params) []string {
	return []string{
		fmt.Sprintf("Chi phí vào/ra định giá trên MỘT phép đo sổ lệnh (%s / %s), giữ CỐ ĐỊNH suốt cửa sổ — "+
			"độ sâu không backfill được, nên đây là tham số được NÊU chứ không phải đo từ quá khứ.",
			series.SpotSource, series.PerpSource),
		"Sổ lệnh hôm nay không đại diện cho sổ lúc thị trường căng — mà đó đúng là lúc funding đảo chiều " +
			"và vị thế phải thoát (PLAN §7.4 mục 2). Chi phí thoát thực tế cao hơn con số này.",
		"Điều kiện thoát theo basis KHÔNG đánh giá được: cần giá spot và perp cùng một thời điểm quá khứ, " +
			"mà price_snapshots chỉ có vài giờ. Xem BasisNotEvaluable.",
		fmt.Sprintf("Giả định giữ %.0f ngày để khấu hao chi phí, vốn %.0f mỗi vị thế, không tái đầu tư lãi.",
			params.HoldingDays, params.NotionalQuote),
		"Quyết định chạy trên mốc ĐÃ SETTLE, nên khi funding đảo dấu vị thế luôn TRẢ mốc âm đầu tiên rồi mới " +
			"thoát ở đúng mốc đó — 'thoát ngay' nghĩa là ngay mốc kế, không phải trước nó.",
		"Đường equity ghi nhận toàn bộ chi phí vòng lúc ĐÓNG; trong lúc giữ nó là số thô của một khoản chắc " +
			"chắn phải trả. Lệnh vào ở mốc cuối cửa sổ bị đóng cưỡng bức với 0 kỳ funding và trọn phí — cố ý, thận trọng.",
	}
}

// Run replays one series over one window with one parameter set.
func Run(series Series, window Window, params strategy.Params) Result {
	out := Result{
		Symbol: series.Symbol, PerpSource: series.PerpSource, SpotSource: series.SpotSource,
		Params: params, Window: window, AssumptionsVI: assumptions(series, params),
	}

	if window.ToMs <= window.FromMs {
		out.ReasonVI = fmt.Sprintf("Cửa sổ rỗng hoặc ngược: [%d, %d).", window.FromMs, window.ToMs)
		return out
	}
	if len(series.Settled) == 0 {
		out.ReasonVI = "Chuỗi không có mốc settle nào để phát lại."
		return out
	}
	// A continuous series is a sample of a funding INDEX, not a settlement
	// list. This engine counts settlements, so it refuses by name rather than
	// counting 8,760 payments a year on a venue that makes none.
	for _, entry := range series.Settled {
		if entry.Model == exchanges.FundingContinuous {
			out.ReasonVI = fmt.Sprintf(
				"%s/%s là chuỗi model='continuous' — hàng của nó là MẪU chỉ số funding theo giờ, "+
					"không phải mốc settle. Engine này đếm settle nên từ chối, thay vì ghi khống 8.760 kỳ/năm.",
				series.PerpSource, series.Symbol)
			return out
		}
	}

	// The same filter the signal layer applies, taken from it rather than
	// repeated here: what counts as a usable settlement is a strategy rule.
	usable, droppedSpecial := strategy.UsableSettled(series.Settled)
	out.DroppedSpecial = droppedSpecial
	if len(usable) == 0 {
		out.ReasonVI = "Không còn mốc settle nào dùng được sau khi lọc."
		return out
	}
	// Oldest-first is an INPUT contract, not something to sort into shape:
	// the store and every fetcher deliver it, and a caller handing over a
	// shuffled series has a bug the persistence window would otherwise hide.
	for i := 1; i < len(usable); i++ {
		if usable[i].SettledAtMs < usable[i-1].SettledAtMs {
			out.ReasonVI = fmt.Sprintf("Chuỗi không theo thứ tự cũ→mới tại chỉ số %d — từ chối phát lại.", i)
			return out
		}
	}

	out.CoverageShort, out.CoverageNoteVI = coverage(usable, window)

	// The round trip is priced ONCE: the books, the fee schedules and the
	// notional are fixed for the whole window by construction (see the header),
	// so the cost cannot change between trades. Taking it from whichever
	// Decision happened to close a trade is what left a position closed at the
	// window's edge — where there is no exit decision at all — paying nothing.
	roundTrip := strategy.RoundTripCost(strategy.RoundTripInput{
		NotionalQuote: params.NotionalQuote,
		SpotFee:       series.SpotFee, PerpFee: series.PerpFee,
		SpotBook: series.SpotBook, PerpBook: series.PerpBook,
	})
	if !roundTrip.OK {
		// No cost means no net figure, and a run that silently traded nothing
		// would read like "the strategy found nothing here" — 12 of the 16
		// shipped series (unverified fee schedules) used to look exactly that.
		out.ReasonVI = "Không định giá được vòng vào/ra nên không phát lại: " + roundTrip.ReasonVI
		return out
	}
	costFrac := roundTrip.TotalPct / 100
	out.RoundTripCostPct = roundTrip.TotalPct
	out.CostBookSampledAtMs = series.PerpBook.SampledAtMs
	if series.SpotBook.SampledAtMs < out.CostBookSampledAtMs {
		out.CostBookSampledAtMs = series.SpotBook.SampledAtMs
	}

	inWindow := func(stampMs int64) bool { return stampMs >= window.FromMs && stampMs < window.ToMs }

	var (
		open     bool
		position strategy.Position
		trade    Trade
		equity   float64 // cumulative net, as a fraction of notional
		peak     float64

		paidPeriods, positivePeriods    int
		firstInWindowMs, lastInWindowMs int64
	)

	for i, entry := range usable {
		if !inWindow(entry.SettledAtMs) {
			continue
		}
		if firstInWindowMs == 0 {
			firstInWindowMs = entry.SettledAtMs
		}
		lastInWindowMs = entry.SettledAtMs
		out.Settlements++
		at := time.UnixMilli(entry.SettledAtMs)

		// A position open BEFORE this settlement collects it. Rule 6: the
		// payment is counted because the position existed at the stamp, not
		// because time passed.
		if open {
			trade.Settlements++
			trade.FundingFrac += entry.RatePerIntervalFrac
			equity += entry.RatePerIntervalFrac
			paidPeriods++
			if entry.RatePerIntervalFrac > 0 {
				positivePeriods++
			}
			if equity > peak {
				peak = equity
			}
			if drawdown := peak - equity; drawdown > out.MaxDrawdownFrac {
				out.MaxDrawdownFrac = drawdown
			}
		}

		candidate := strategy.Candidate{
			Symbol: series.Symbol, PerpSource: series.PerpSource, SpotSource: series.SpotSource,
			Settled: usable[:i+1],
			SpotFee: series.SpotFee, PerpFee: series.PerpFee,
			SpotBook: series.SpotBook, PerpBook: series.PerpBook,
			// No prices: there is no historical spot/perp pair to read, and an
			// invented one would make the basis exit look tested.
		}

		if !open {
			if strategy.EvaluateEntry(at, candidate, params).Action == strategy.ActionEnter {
				open = true
				position = strategy.Position{
					Symbol: series.Symbol, PerpSource: series.PerpSource, SpotSource: series.SpotSource,
					OpenedAtMs: entry.SettledAtMs, NotionalQuote: params.NotionalQuote,
				}
				trade = Trade{OpenAtMs: entry.SettledAtMs}
			}
			continue
		}

		decision := strategy.EvaluateExit(at, position, candidate, params)
		for _, check := range decision.Checks {
			if check.Name == "basis_widened" && check.NotEvaluated {
				out.BasisNotEvaluable++
			}
		}
		if decision.Action == strategy.ActionExit {
			equity -= closeTrade(&trade, entry.SettledAtMs, costFrac, decision)
			if drawdown := peak - equity; drawdown > out.MaxDrawdownFrac {
				out.MaxDrawdownFrac = drawdown
			}
			out.Trades = append(out.Trades, trade)
			open = false
		}
	}

	// A position still open at the end of the window is closed there: leaving
	// it out would report the funding it collected without the exit cost it
	// has not paid yet, which flatters every result that ends mid-position.
	if open {
		// Closed at the LAST settlement the window covered, not the series'
		// last row: with lookback rows past the window the two differ, and a
		// trade must not report a close date the window never reached.
		equity -= closeTrade(&trade, lastInWindowMs, costFrac, strategy.Decision{})
		if drawdown := peak - equity; drawdown > out.MaxDrawdownFrac {
			out.MaxDrawdownFrac = drawdown // the forced close is a real cost, and it counts
		}
		trade.ExitReasonVI = "Đóng ở cuối cửa sổ backtest (không phải tín hiệu thoát) — chi phí ra vẫn bị tính."
		out.Trades = append(out.Trades, trade)
	}

	if out.Settlements == 0 {
		out.ReasonVI = "Không có mốc settle nào trong cửa sổ."
		return out
	}

	out.TotalReturnFrac = equity
	// Annualized over the span the corpus COVERED inside the window — first to
	// last in-window settlement plus one interval, so a fully covered window
	// reads as its full length — never over the window that was asked for.
	coveredMs := lastInWindowMs - firstInWindowMs + usable[len(usable)-1].IntervalSec*msPerSecond
	out.CoveredDays = float64(coveredMs) / (msPerSecond * secPerDay)
	if out.CoveredDays > 0 {
		out.RealizedAPRFrac = out.TotalReturnFrac * daysPerYear / out.CoveredDays
	}
	out.FundingReversals = signReversals(usable, window)
	out.PeriodsInPosition = paidPeriods
	if paidPeriods > 0 {
		out.PositiveFundingPeriodShare = float64(positivePeriods) / float64(paidPeriods)
	}
	out.OK = true
	return out
}

// closeTrade charges the round trip once and finalises the trade, returning the
// cost as a fraction of notional.
func closeTrade(trade *Trade, closeAtMs int64, costFrac float64, decision strategy.Decision) float64 {
	trade.CloseAtMs = closeAtMs
	trade.CostFrac = costFrac
	trade.NetFrac = trade.FundingFrac - trade.CostFrac
	for _, check := range decision.Checks {
		if check.Passed {
			trade.ExitReasonVI = check.DetailVI
			break
		}
	}
	return trade.CostFrac
}

// coverage compares what the corpus holds against what was asked for.
//
// Tolerance is ONE modal interval at each end. Without it every result was
// flagged: a window ending "now" is always hours past the newest settlement,
// so the flag could not tell OKX's 3 months from Binance's full 6 — the only
// thing it exists to do.
func coverage(usable []exchanges.FundingHistoryEntry, window Window) (short bool, noteVI string) {
	oldest, newest := usable[0].SettledAtMs, usable[len(usable)-1].SettledAtMs
	toleranceMs := usable[len(usable)-1].IntervalSec * msPerSecond
	missingStart := oldest > window.FromMs+toleranceMs
	missingEnd := newest < window.ToMs-toleranceMs
	if !missingStart && !missingEnd {
		return false, ""
	}
	return true, fmt.Sprintf(
		"Corpus chỉ phủ [%s, %s] trong khi cửa sổ hỏi [%s, %s) — so hai sàn trên 'cùng' cửa sổ này "+
			"có thể là so một năm với một quý.",
		stamp(oldest), stamp(newest), stamp(window.FromMs), stamp(window.ToMs))
}

func stamp(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02") }

// signReversals counts how many times the funding rate changed sign inside the
// window. Zero is not a sign and never starts or breaks a run.
func signReversals(usable []exchanges.FundingHistoryEntry, window Window) int {
	previous, reversals := 0, 0
	for _, entry := range usable {
		if entry.SettledAtMs < window.FromMs || entry.SettledAtMs >= window.ToMs {
			continue
		}
		sign := 0
		switch {
		case entry.RatePerIntervalFrac > 0:
			sign = 1
		case entry.RatePerIntervalFrac < 0:
			sign = -1
		default:
			continue
		}
		if previous != 0 && sign != previous {
			reversals++
		}
		previous = sign
	}
	return reversals
}

// Sweep replays every (series, params) combination in parallel.
//
// Output order is by series then by the order of the grid, never by which
// goroutine finished first: a sweep whose rows move between runs cannot be
// diffed, and comparing two sweeps is the entire point of running one.
func Sweep(series []Series, window Window, grid []strategy.Params) []Result {
	type job struct{ s, p int }
	var jobs []job
	for si := range series {
		for pi := range grid {
			jobs = append(jobs, job{si, pi})
		}
	}
	results := make([]Result, len(jobs))

	var wg sync.WaitGroup
	// Bounded rather than one goroutine per job: a sweep is CPU-bound and a
	// grid can be thousands of combinations.
	workers := runtime.NumCPU()
	queue := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range queue {
				j := jobs[index]
				results[index] = Run(series[j.s], window, grid[j.p])
			}
		}()
	}
	for index := range jobs {
		queue <- index
	}
	close(queue)
	wg.Wait()
	return results
}

// SortByRealizedAPR orders results best first: traded runs by realized APR,
// then runs that never traded, then refused runs. A run with no trades returns
// exactly zero, which would otherwise outrank every losing run and put "the
// strategy found nothing here" at the top of a table titled best.
func SortByRealizedAPR(results []Result) {
	sort.SliceStable(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.OK != b.OK {
			return a.OK
		}
		if (len(a.Trades) > 0) != (len(b.Trades) > 0) {
			return len(a.Trades) > 0
		}
		return a.RealizedAPRFrac > b.RealizedAPRFrac
	})
}
