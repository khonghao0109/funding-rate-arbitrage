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
	"futures-arbitrage-scanner/internal/risk"
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
//   - The basis exit IS evaluable since step 3.3b, from stored hourly candles
//     (Series.SpotCandles / PerpCandles). Candles, unlike depth, can be
//     backfilled. The price used is the newest candle to have FULLY CLOSED at
//     or before the settlement — never the one containing it, whose close is
//     stamped up to an hour in the future — so it is at most one interval old.
//     Where a leg has no candle within two intervals the check still reports
//     its own inability rather than being fed an invented number, and
//     Result.BasisNotEvaluable counts how often that happened.
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

	// QuoteBridged marks a series whose two legs quote DIFFERENT assets and
	// were paired only because config.yaml declared those quotes equivalent
	// (instruments.HedgePair.QuoteBridged — USD perp against a USDT spot).
	// Such a replay is delta-neutral in the coin and OPEN in the quote pair,
	// and no figure here deducts that, so it goes in the assumptions block.
	// SpotQuoteAsset/PerpQuoteAsset name the two sides for the message.
	QuoteBridged   bool
	SpotQuoteAsset string
	PerpQuoteAsset string

	// The books the cost is priced against. ONE measurement, held fixed for the
	// whole window — see the header.
	SpotBook depth.Summary
	PerpBook depth.Summary

	// SpotCandles and PerpCandles are hourly closes for the two legs
	// (store.PriceHistory, step 3.3b). They are what makes the basis exit
	// evaluable in hindsight: unlike depth, candles CAN be backfilled, so a
	// past instant really does have a spot and a perp price.
	//
	// Both may be empty, and then the basis check reports "not evaluable" at
	// every settlement exactly as it did before this existed — a series whose
	// prices were never collected must not silently be replayed as one whose
	// basis never moved. Result.BasisNotEvaluable counts it either way.
	SpotCandles []exchanges.PriceCandle
	PerpCandles []exchanges.PriceCandle

	// PerpMargin is the perp venue's maintenance bracket. An unverified one
	// makes the margin condition refuse rather than assume a zero maintenance
	// requirement — see internal/risk.
	PerpMargin risk.Bracket
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

	// BasisEvaluable counts settlements where BOTH legs had a usable close, so
	// a reader can tell "the rule was tested and never fired" from "there were
	// no prices to test it with". The two counters are not complements:
	// BasisNotEvaluable is per EXIT evaluation and this is per settlement,
	// including the ones the position was flat for.
	BasisEvaluable int

	// EnteredWithoutBasis counts positions opened at a settlement where one leg
	// had no price, so EntryBasisPct is 0 rather than measured. Such a position
	// cannot exit on basis DRIFT honestly, and the number says how many there
	// were instead of letting a 0 pass for a measurement.
	EnteredWithoutBasis int

	// Liquidations counts positions the perp venue would have force-closed
	// between two settlements — detected from the candle HIGH, not the close,
	// because a short dies on a spike and an hourly close steps right over it.
	//
	// It is reported apart from Trades on purpose. A liquidation is not an
	// exit the strategy chose, and folding it into the trade count would let a
	// run that was force-closed read like a run that decided to leave.
	Liquidations int

	// CoverageShort marks a window the corpus does not fill. Three venues cap
	// their published history (OKX ~3 months, Gate 180 days, Paradex none), so
	// comparing two venues over "the same" window can silently compare a year
	// against a quarter.
	CoverageShort  bool
	CoverageNoteVI string

	// QuoteBridged carries Series.QuoteBridged onto the result, because the
	// assumptions block prints ONE result's assumptions for a whole run — a
	// per-series fact stated only there would be invisible in every run whose
	// first series is not bridged. SummaryLines prints it per series instead.
	QuoteBridged   bool
	SpotQuoteAsset string
	PerpQuoteAsset string

	OK            bool
	ReasonVI      string
	AssumptionsVI []string
}

// assumptions is what every result must carry, in words.
// basisAssumptionVI says what the basis condition was actually able to do in
// this run — which changed at step 3.3b and must not read the same either way.
//
// A series with no candles replays exactly as it did before the table existed,
// and a reader comparing two runs has to be able to tell those apart: "the rule
// never fired" and "the rule was never tested" are the same output and
// different facts.
func basisAssumptionVI(series Series) string {
	if len(series.SpotCandles) == 0 || len(series.PerpCandles) == 0 {
		return "Điều kiện thoát theo basis KHÔNG đánh giá được: chuỗi này chưa có nến giá cho cả hai chân " +
			"(price_history). Chạy `go run ./cmd/backfill -prices` rồi phát lại. Xem BasisNotEvaluable."
	}
	note := "Điều kiện thoát theo basis ĐƯỢC đánh giá từ nến 1h đã lưu (price_history). Giá dùng là nến " +
		"ĐÃ ĐÓNG gần nhất tại hoặc trước mốc settle — không phải nến đang chứa mốc đó, vì giá đóng của nến " +
		"ấy nằm ở TƯƠNG LAI so với quyết định — nên nó cũ tối đa 1 giờ. Chân nào không có nến trong 2 chu kỳ " +
		"thì mốc đó vẫn báo không đánh giá được. Xem BasisEvaluable / BasisNotEvaluable."
	if series.QuoteBridged {
		// On a bridged series the "basis" is not purely a coin basis: one leg
		// is priced in USD and the other in USDT, so the number the exit rule
		// tests is the coin basis PLUS the quote spread. That is arguably the
		// right thing to exit on — the quote exposure is real and undeducted —
		// but a reader comparing this series' basis exits against an unbridged
		// one is comparing two different measurements.
		note += fmt.Sprintf(" LƯU Ý chuỗi khác quote: basis đo được ở đây là basis theo coin CỘNG chênh "+
			"%s/%s, nên lối thoát basis trên chuỗi này một phần đang canh đúng rủi ro quote chưa ai trừ.",
			series.PerpQuoteAsset, series.SpotQuoteAsset)
	}
	return note
}

func assumptions(series Series, params strategy.Params) []string {
	out := []string{
		fmt.Sprintf("Chi phí vào/ra định giá trên MỘT phép đo sổ lệnh (%s / %s), giữ CỐ ĐỊNH suốt cửa sổ — "+
			"độ sâu không backfill được, nên đây là tham số được NÊU chứ không phải đo từ quá khứ.",
			series.SpotSource, series.PerpSource),
		"Sổ lệnh hôm nay không đại diện cho sổ lúc thị trường căng — mà đó đúng là lúc funding đảo chiều " +
			"và vị thế phải thoát (PLAN §7.4 mục 2). Chi phí thoát thực tế cao hơn con số này.",
		basisAssumptionVI(series),
		fmt.Sprintf("Giả định giữ %.0f ngày để khấu hao chi phí, vốn %.0f mỗi vị thế, không tái đầu tư lãi.",
			params.HoldingDays, params.NotionalQuote),
		"Quyết định chạy trên mốc ĐÃ SETTLE, nên khi funding đảo dấu vị thế luôn TRẢ mốc âm đầu tiên rồi mới " +
			"thoát ở đúng mốc đó — 'thoát ngay' nghĩa là ngay mốc kế, không phải trước nó.",
		"Đường equity ghi nhận toàn bộ chi phí vòng lúc ĐÓNG; trong lúc giữ nó là số thô của một khoản chắc " +
			"chắn phải trả. Lệnh vào ở mốc cuối cửa sổ bị đóng cưỡng bức với 0 kỳ funding và trọn phí — cố ý, thận trọng.",
	}
	if params.PerpMarginFrac > 0 {
		out = append(out, fmt.Sprintf(
			"MÔ HÌNH KÝ QUỸ chân perp BẬT: ký quỹ %.1f%% notional (đòn bẩy %.1fx), thoát khi còn dưới %.2f%% "+
				"tới giá thanh lý, và một cú thanh lý được phát hiện từ ĐỈNH nến 1h chứ không phải giá đóng — "+
				"short chết ở cú nhọn, giá đóng bước qua nó. CHƯA mô hình hoá: funding đã thu vào tài khoản "+
				"perp (sẽ đẩy giá thanh lý ra xa), cross-margin, nạp thêm ký quỹ, và cơ chế thanh lý từng "+
				"phần kèm phí của sàn — nên mô hình này nổ SỚM hơn thực tế, là hướng an toàn. Số ghi ở lệnh "+
				"bị thanh lý chỉ là vòng phí, chưa gồm phần ký quỹ mất.",
			params.PerpMarginFrac*100, 1/params.PerpMarginFrac, params.MinLiquidationBufferPct))
		if !series.PerpMargin.Verified {
			out = append(out, fmt.Sprintf(
				"⚠ Biểu ký quỹ duy trì của %s CHƯA XÁC MINH: mô hình từ chối suy ra giá thanh lý, nên điều "+
					"kiện ký quỹ sẽ THOÁT ngay thay vì đoán. Xem margin.verified trong config.yaml.",
				series.PerpSource))
		}
	}
	if params.MinHoldRecoveredCostFrac > 0 {
		// A floor that holds a position through a signal is a change to what
		// the run MEANS, not a tuning detail: some of the held periods below
		// were held against the rule's own verdict, and a reader comparing
		// this run with an ungated one has to know that before comparing
		// trade counts.
		out = append(out, fmt.Sprintf(
			"CỔNG GIỮ TỐI THIỂU %.2f× chi phí vòng: hai lối thoát vì LỢI SUẤT (đảo dấu, suy giảm) bị chặn "+
				"cho tới khi vị thế hoàn lại chừng đó chi phí vào/ra. Lối thoát vì RỦI RO (mất chân hedge, "+
				"basis vượt hạn, không định giá được) KHÔNG bị chặn. Nên một phần số kỳ giữ dưới đây là giữ "+
				"NGƯỢC lại phán quyết của chính luật.", params.MinHoldRecoveredCostFrac))
	}
	if series.QuoteBridged {
		// Named FIRST for a bridged series: it is the one assumption that
		// changes what the position IS, not just how precisely it is priced.
		out = append([]string{fmt.Sprintf(
			"CHÂN SPOT KHÁC QUOTE: spot quote %s ghép với perp quote %s theo khai báo hedge.quote_equivalents. "+
				"Vị thế trung tính về coin nhưng CÒN MỞ rủi ro %s/%s — không con số nào ở đây trừ khoản đó, "+
				"và funding thu được tính bằng %s trong khi vốn spot nằm ở %s.",
			series.SpotQuoteAsset, series.PerpQuoteAsset,
			series.SpotQuoteAsset, series.PerpQuoteAsset,
			series.PerpQuoteAsset, series.SpotQuoteAsset)}, out...)
	}
	return out
}

// Run replays one series over one window with one parameter set.
func Run(series Series, window Window, params strategy.Params) Result {
	out := Result{
		Symbol: series.Symbol, PerpSource: series.PerpSource, SpotSource: series.SpotSource,
		Params: params, Window: window, AssumptionsVI: assumptions(series, params),
		QuoteBridged:   series.QuoteBridged,
		SpotQuoteAsset: series.SpotQuoteAsset, PerpQuoteAsset: series.PerpQuoteAsset,
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

	spotPrices := newPriceSeries(series.SpotCandles)
	perpPrices := newPriceSeries(series.PerpCandles)
	// The instant the intrabar liquidation scan has already covered.
	var lastCheckedMs int64

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

		// The two legs' prices as of THIS settlement, from the newest candle
		// that had already closed. Absent on either side leaves both at 0,
		// which is what strategy reads as "not evaluable" — a one-legged
		// basis is not a basis.
		spotQuote, haveSpot := spotPrices.closeAt(entry.SettledAtMs)
		perpQuote, havePerp := perpPrices.closeAt(entry.SettledAtMs)
		if !haveSpot || !havePerp {
			spotQuote, perpQuote = 0, 0
		} else {
			out.BasisEvaluable++
		}

		candidate := strategy.Candidate{
			Symbol: series.Symbol, PerpSource: series.PerpSource, SpotSource: series.SpotSource,
			Settled: usable[:i+1],
			SpotFee: series.SpotFee, PerpFee: series.PerpFee,
			SpotBook: series.SpotBook, PerpBook: series.PerpBook,
			SpotPriceQuote: spotQuote, PerpPriceQuote: perpQuote,
			PerpMargin: series.PerpMargin,
		}

		if !open {
			if strategy.EvaluateEntry(at, candidate, params).Action == strategy.ActionEnter {
				open = true
				position = strategy.Position{
					Symbol: series.Symbol, PerpSource: series.PerpSource, SpotSource: series.SpotSource,
					OpenedAtMs: entry.SettledAtMs, NotionalQuote: params.NotionalQuote,
				}
				// The basis the position was OPENED at, which the exit rule
				// measures movement against. Left at 0 when either leg had no
				// price: a zero entry basis with a real current one would read
				// as a move of the whole current basis, inventing a drift the
				// position never had. exitBasisWidened refuses to evaluate
				// whenever a price is missing, so a position opened blind can
				// only exit on basis once BOTH legs are priced again — and its
				// entry basis is then honestly unknown, which the assumptions
				// block says.
				if haveSpot && havePerp {
					position.EntryBasisPct = basisPct(spotQuote, perpQuote)
				} else {
					out.EnteredWithoutBasis++
				}
				// The price the short was opened at, which with the notional
				// fixes the quantity and hence the liquidation price. 0 when
				// the perp had no candle, and the margin condition then
				// reports that rather than dividing by it.
				position.PerpEntryPriceQuote = perpQuote
				trade = Trade{OpenAtMs: entry.SettledAtMs}
				lastCheckedMs = entry.SettledAtMs
			}
			continue
		}

		// A liquidation happens INTRABAR. Between the previous settlement and
		// this one the perp may have spiked through the liquidation price and
		// come back, and an hourly close steps straight over it — so the
		// highs in that span are checked before the decision is asked for.
		// Checked first because there is no decision to make afterwards: the
		// venue has already closed the position.
		if params.PerpMarginFrac > 0 && position.PerpEntryPriceQuote > 0 {
			// From the PREVIOUS settlement, not from the open: this check runs
			// at every settlement while the position is held, so scanning back
			// to the open would re-read the same candles once per settlement —
			// O(n²) on the 8,760-settlement hourly venues, which is the same
			// defect UsableSettled had. Any spike is still caught, at the
			// settlement immediately after it, and the trade is stamped with
			// the bar it happened in.
			if high, at, ok := perpPrices.highBetween(lastCheckedMs, entry.SettledAtMs); ok {
				state := risk.Evaluate(risk.Position{
					NotionalQuote:   position.NotionalQuote,
					EntryPriceQuote: position.PerpEntryPriceQuote,
					MarginFrac:      params.PerpMarginFrac,
				}, series.PerpMargin, high)
				if state.OK && state.Liquidated {
					out.Liquidations++
					equity -= closeTrade(&trade, at, costFrac, strategy.Decision{})
					if drawdown := peak - equity; drawdown > out.MaxDrawdownFrac {
						out.MaxDrawdownFrac = drawdown
					}
					trade.ExitReasonVI = fmt.Sprintf(
						"THANH LÝ: đỉnh %.2f chạm giá thanh lý %.2f của chân perp (vào %.2f, ký quỹ %.1f%%, "+
							"duy trì %.4f%%). Đây là MẤT VỐN — chi phí ghi ở đây chỉ là vòng phí, chưa gồm "+
							"phần ký quỹ bị mất và phí thanh lý của sàn.",
						high, state.LiquidationPriceQuote, position.PerpEntryPriceQuote,
						params.PerpMarginFrac*100, series.PerpMargin.MaintenanceMarginFrac*100)
					out.Trades = append(out.Trades, trade)
					open = false
					continue
				}
			}
		}

		lastCheckedMs = entry.SettledAtMs

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
