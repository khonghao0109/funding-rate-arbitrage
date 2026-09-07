package backtest

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Reporting (step 3.3).
//
// Two forms, one source: a CSV for analysis outside this process — phase 8 is
// Python and reads what Go wrote (PLAN §7.1 Q8) — and a human summary for the
// terminal.
//
// Both are written from Result, and both carry the ASSUMPTIONS beside the
// numbers. A backtest figure detached from "the book was one measurement held
// fixed, and the basis exit was never evaluable" is a number that will be
// quoted later as if it were a measurement of the strategy. That is the same
// failure CLAUDE.md rule 2 forbids for profit, applied to a whole result.

// csvHeader names every column, with its unit in the name exactly as the store
// schema does (CONVENTIONS §1) — these are read from Python months later.
var csvHeader = []string{
	"symbol", "perp_source", "spot_source",
	"window_from_ms", "window_to_ms", "window_days", "covered_days",
	"min_rate_per_8h_bps", "persistence_periods", "min_net_apr_frac",
	"exit_net_apr_frac", "exit_persistence_periods",
	"exit_negative_min_bps", "exit_negative_periods", "exit_negative_cum_cost_frac",
	"notional_quote", "holding_days",
	"round_trip_cost_pct", "cost_book_sampled_at_ms",
	"settlements", "trades", "periods_in_position",
	"total_return_frac", "realized_apr_frac", "max_drawdown_frac",
	"funding_reversals", "positive_funding_period_share", "dropped_special",
	"basis_not_evaluable", "coverage_short", "ok", "reason_vi", "assumptions_vi",
}

// WriteCSV writes a header and one row per result.
func WriteCSV(w io.Writer, results []Result) error {
	out := csv.NewWriter(w)
	if err := out.Write(csvHeader); err != nil {
		return fmt.Errorf("backtest: write csv header: %w", err)
	}
	for _, r := range results {
		windowDays := float64(r.Window.ToMs-r.Window.FromMs) / (msPerSecond * secPerDay)
		row := []string{
			r.Symbol, r.PerpSource, r.SpotSource,
			strconv.FormatInt(r.Window.FromMs, 10), strconv.FormatInt(r.Window.ToMs, 10),
			f(windowDays), f(r.CoveredDays),
			f(r.Params.MinRatePer8hBps), strconv.Itoa(r.Params.PersistencePeriods), f(r.Params.MinNetAPRFrac),
			f(r.Params.ExitNetAPRFrac), strconv.Itoa(r.Params.ExitPersistencePeriods),
			f(r.Params.ExitNegativeMinBps), strconv.Itoa(r.Params.EffectiveExitNegativePeriods()), f(r.Params.ExitNegativeCumCostFrac),
			f(r.Params.NotionalQuote), f(r.Params.HoldingDays),
			f(r.RoundTripCostPct), strconv.FormatInt(r.CostBookSampledAtMs, 10),
			strconv.Itoa(r.Settlements), strconv.Itoa(len(r.Trades)), strconv.Itoa(r.PeriodsInPosition),
			f(r.TotalReturnFrac), f(r.RealizedAPRFrac), f(r.MaxDrawdownFrac),
			strconv.Itoa(r.FundingReversals), f(r.PositiveFundingPeriodShare), strconv.Itoa(r.DroppedSpecial),
			strconv.Itoa(r.BasisNotEvaluable), strconv.FormatBool(r.CoverageShort),
			strconv.FormatBool(r.OK), r.ReasonVI, strings.Join(r.AssumptionsVI, " | "),
		}
		if err := out.Write(row); err != nil {
			return fmt.Errorf("backtest: write csv row: %w", err)
		}
	}
	out.Flush()
	return out.Error()
}

func f(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// SummaryLines renders one result for a human, verdict first.
func (r Result) SummaryLines() []string {
	if !r.OK {
		return []string{fmt.Sprintf("%-8s %-20s TỪ CHỐI — %s", r.Symbol, r.PerpSource, r.ReasonVI)}
	}
	windowDays := float64(r.Window.ToMs-r.Window.FromMs) / (msPerSecond * secPerDay)
	lines := []string{
		fmt.Sprintf("%s %s ← %s · cửa sổ %.0f ngày, corpus phủ %.0f ngày · %d mốc settle · vòng %.4f%%",
			r.Symbol, r.PerpSource, r.SpotSource, windowDays, r.CoveredDays, r.Settlements, r.RoundTripCostPct),
		fmt.Sprintf("  Tổng lợi nhuận  %+.4f%% trên notional  ·  APR thực %+.2f%% (annualize trên %.0f ngày ĐÃ PHỦ)",
			r.TotalReturnFrac*100, r.RealizedAPRFrac*100, r.CoveredDays),
		fmt.Sprintf("  Max drawdown    %.4f%%  ·  funding đảo chiều %d lần",
			r.MaxDrawdownFrac*100, r.FundingReversals),
		fmt.Sprintf("  Lệnh            %d  ·  %d kỳ nắm giữ  ·  %.1f%% số kỳ funding DƯƠNG (thô — không phải 'có lãi')",
			len(r.Trades), r.PeriodsInPosition, r.PositiveFundingPeriodShare*100),
	}
	if r.QuoteBridged {
		lines = append(lines, fmt.Sprintf(
			"  ⚠ QUOTE KHÁC NHAU: spot quote %s, perp quote %s — ghép được là do khai báo "+
				"hedge.quote_equivalents, không phải do hai sàn cùng quote. Vị thế CÒN MỞ rủi ro %s/%s "+
				"và không con số nào ở dòng trên trừ khoản đó.",
			r.SpotQuoteAsset, r.PerpQuoteAsset, r.SpotQuoteAsset, r.PerpQuoteAsset))
	}
	if r.CoverageShort {
		lines = append(lines, "  ⚠ ĐỘ PHỦ: "+r.CoverageNoteVI)
	}
	if r.BasisNotEvaluable > 0 {
		lines = append(lines, fmt.Sprintf(
			"  ⚠ %d lần đánh giá thoát KHÔNG xét được điều kiện basis (không có giá lịch sử) — "+
				"điều kiện này chưa từng được kiểm trong lượt chạy này.", r.BasisNotEvaluable))
	}
	for _, trade := range r.Trades {
		lines = append(lines, fmt.Sprintf("    · %s → %s  %d kỳ  funding %+.4f%%  chi phí %.4f%%  ròng %+.4f%%  %s",
			stamp(trade.OpenAtMs), stamp(trade.CloseAtMs), trade.Settlements,
			trade.FundingFrac*100, trade.CostFrac*100, trade.NetFrac*100, trade.ExitReasonVI))
	}
	return lines
}

// AssumptionLines is the block every report must print once, whatever else it
// shows.
func AssumptionLines(results []Result) []string {
	for _, r := range results {
		if len(r.AssumptionsVI) == 0 {
			continue
		}
		lines := []string{"GIẢ ĐỊNH của lượt backtest này — đọc trước khi tin bất kỳ con số nào ở trên:"}
		for _, a := range r.AssumptionsVI {
			lines = append(lines, "  ! "+a)
		}
		// This block prints ONE result's assumptions, so a per-series fact has
		// to be summarized here too or the block would claim completeness it
		// does not have. Only the count and the names — the detail is on each
		// series' own ⚠ line.
		var bridged []string
		for _, other := range results {
			if other.QuoteBridged {
				bridged = append(bridged, other.Symbol+"/"+other.PerpSource)
			}
		}
		if len(bridged) > 0 {
			lines = append(lines, fmt.Sprintf(
				"  ! %d chuỗi ghép chân spot KHÁC QUOTE theo khai báo hedge.quote_equivalents và "+
					"còn mở rủi ro giữa hai quote (xem ⚠ trên từng chuỗi): %s.",
				len(bridged), strings.Join(bridged, ", ")))
		}
		return lines
	}
	return nil
}

// tradesCSVHeader names one row per trade. Each row carries the parameters
// that produced it: a trade detached from its sweep line still has to say
// which rule set it belongs to, or a hold-length distribution drawn from the
// file mixes every set in the grid and calls the mixture "the strategy".
var tradesCSVHeader = []string{
	"symbol", "perp_source", "spot_source",
	"min_rate_per_8h_bps", "persistence_periods", "min_net_apr_frac",
	"exit_net_apr_frac", "exit_persistence_periods",
	"exit_negative_min_bps", "exit_negative_periods", "exit_negative_cum_cost_frac",
	"notional_quote", "holding_days",
	"open_at_ms", "close_at_ms", "held_days", "settlements",
	"funding_frac", "cost_frac", "net_frac", "exit_reason_vi",
}

// WriteTradesCSV writes a header and one row per trade of every run that
// replayed. A refused run contributes nothing — it traded nothing — and the
// per-run CSV is where its refusal is recorded by name.
func WriteTradesCSV(w io.Writer, results []Result) error {
	out := csv.NewWriter(w)
	if err := out.Write(tradesCSVHeader); err != nil {
		return fmt.Errorf("backtest: write trades csv header: %w", err)
	}
	for _, r := range results {
		if !r.OK {
			continue
		}
		for _, t := range r.Trades {
			heldDays := float64(t.CloseAtMs-t.OpenAtMs) / (msPerSecond * secPerDay)
			row := []string{
				r.Symbol, r.PerpSource, r.SpotSource,
				f(r.Params.MinRatePer8hBps), strconv.Itoa(r.Params.PersistencePeriods), f(r.Params.MinNetAPRFrac),
				f(r.Params.ExitNetAPRFrac), strconv.Itoa(r.Params.ExitPersistencePeriods),
				f(r.Params.ExitNegativeMinBps), strconv.Itoa(r.Params.EffectiveExitNegativePeriods()), f(r.Params.ExitNegativeCumCostFrac),
				f(r.Params.NotionalQuote), f(r.Params.HoldingDays),
				strconv.FormatInt(t.OpenAtMs, 10), strconv.FormatInt(t.CloseAtMs, 10),
				f(heldDays), strconv.Itoa(t.Settlements),
				f(t.FundingFrac), f(t.CostFrac), f(t.NetFrac), t.ExitReasonVI,
			}
			if err := out.Write(row); err != nil {
				return fmt.Errorf("backtest: write trades csv row: %w", err)
			}
		}
	}
	out.Flush()
	return out.Error()
}
