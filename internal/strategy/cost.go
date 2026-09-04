package strategy

import (
	"fmt"
	"strings"
	"time"

	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/fees"
)

// The one-off cost of opening and closing a delta-neutral funding position
// (step 3.1).
//
// A funding position is spot long and perp short of equal coin quantity, so
// getting in and out is FOUR taker fills, not two: buy spot and sell perp at
// entry, sell spot and buy perp at exit. internal/fees already charges four
// commissions; what step 3.1 adds is the four fills' slippage, each priced
// against the side of the book that fill actually takes.
//
// The exit sides are not cosmetic. docs/PLAN.md §7.4 item 2: an entry happens
// when funding is attractive and the market is calm, while an exit happens when
// funding has turned, which correlates with stress and a thinner book. The
// painful leg is SELLING the spot, and pricing it against the ask side would
// hide exactly that.

// RoundTripInput names everything the round-trip cost consumes. A struct rather
// than parameters because two fee schedules and two books adjacent in a
// signature transpose silently (CONVENTIONS §6.2).
type RoundTripInput struct {
	// NotionalQuote is the size the position is INTENDED to be, not a venue
	// minimum: slippage is a function of size, and pricing the minimum would
	// report a cost nobody is going to pay (docs/PLAN.md step 3.1).
	NotionalQuote float64

	SpotFee fees.Schedule
	PerpFee fees.Schedule

	SpotBook depth.Summary
	PerpBook depth.Summary

	// At is the instant this is being evaluated AT. It is passed in and never
	// read from the clock: internal/strategy/doc.go makes that the package's
	// contract, because the backtest evaluates historical instants and the live
	// path evaluates now, and the step-3.5 gate compares what the two decided.
	//
	// MaxBookAge is how old a depth sample may be and still price a fill. A
	// depth sweep runs at most hourly (internal/depth), so a book here is
	// routinely minutes to an hour old, and a fill priced against a book from
	// before a move is not an estimate of anything. 0 disables the check —
	// which the backtest needs, since there is no historical depth to age.
	At         time.Time
	MaxBookAge time.Duration
}

// RoundTrip is what getting into and out of one position costs, and what that
// figure does and does not include.
//
// OK false means NO number here may be used. It is not "the cost is zero": an
// unverified fee schedule and a book too thin to fill are both reasons a cost
// cannot be stated, and treating either as free is how a loss is presented as a
// profit (CLAUDE.md rule 2).
type RoundTrip struct {
	// Identity of the position this cost was priced FOR. Carried so a caller
	// cannot pair it with a different pair's funding rate: NetAPR checks these
	// against the reading it is annualizing, because a cost computed on one
	// venue's book silently applied to another venue's rate is a mispricing
	// that produces a plausible number.
	Symbol     string
	SpotSource string
	PerpSource string

	NotionalQuote float64

	// FeesPct is taker commission on all four fills; SlippagePct is the four
	// fills' distance from mid. TotalPct is their sum, as a percentage of the
	// position's notional, charged ONCE for the whole trip.
	FeesPct     float64
	SlippagePct float64
	TotalPct    float64

	EntrySpotBuy  FillEstimate
	EntryPerpSell FillEstimate
	ExitSpotSell  FillEstimate
	ExitPerpBuy   FillEstimate

	OK       bool
	ReasonVI string

	// DepthIsLowerBound is set when ANY of the four fills ran past the levels
	// its venue published. The total is then an upper bound on cost.
	DepthIsLowerBound bool
	NoteVI            string

	// AppliedVI and ExcludedVI are the cost basis in words, carried with the
	// number so no caller can display it without being able to say what it
	// covers. Same contract as the wire's cost_basis (WS-CONTRACT §3).
	AppliedVI  []string
	ExcludedVI []string
}

// Fills returns the four legs in execution order.
func (r RoundTrip) Fills() []FillEstimate {
	return []FillEstimate{r.EntrySpotBuy, r.EntryPerpSell, r.ExitSpotSell, r.ExitPerpBuy}
}

// excludedFromRoundTrip is everything this figure does NOT deduct.
//
// It is a constant list rather than a comment because it travels with the
// number to the dashboard and to the alert: a reader who is told "net" is owed
// the boundary of that word.
func excludedFromRoundTrip() []string {
	return []string{
		"Chi phí vay/ký quỹ chân spot — mô hình giả định spot mua bằng vốn tự có, không đòn bẩy.",
		"Basis giãn giữa lúc vào và lúc thoát: hai chân được định giá trên sổ CÙNG một thời điểm.",
		"Sổ lệnh lúc THOÁT — độ sâu đo lúc vào không dự báo được độ sâu lúc funding đảo chiều (PLAN §7.4).",
		"Phí rút/chuyển tài sản giữa hai sàn.",
		"Rủi ro thanh lý chân perp và chi phí bổ sung ký quỹ.",
	}
}

// RoundTripCost prices the four fills, or refuses and says why.
func RoundTripCost(in RoundTripInput) RoundTrip {
	out := RoundTrip{
		Symbol:        in.PerpBook.Symbol,
		SpotSource:    in.SpotBook.Source,
		PerpSource:    in.PerpBook.Source,
		NotionalQuote: in.NotionalQuote,
		ExcludedVI:    excludedFromRoundTrip(),
	}

	if !isPositiveFinite(in.NotionalQuote) {
		out.ReasonVI = fmt.Sprintf("Vốn dự kiến %v không phải số dương hữu hạn.", in.NotionalQuote)
		return out
	}
	if in.SpotBook.Symbol != "" && in.PerpBook.Symbol != "" && in.SpotBook.Symbol != in.PerpBook.Symbol {
		out.ReasonVI = fmt.Sprintf("Hai chân không cùng một cặp: spot %s (%s) vs perp %s (%s).",
			in.SpotBook.Symbol, in.SpotBook.Source, in.PerpBook.Symbol, in.PerpBook.Source)
		return out
	}
	if reason := bookAgeRefusal(in); reason != "" {
		out.ReasonVI = reason
		return out
	}

	// The commission first: a pair touching an unverified schedule produces no
	// figure at all, and there is no point pricing four books for it.
	feesPct, verified := fees.RoundTripTakerPct(in.SpotFee, in.PerpFee)
	if !verified {
		out.ReasonVI = fmt.Sprintf(
			"Chưa xác minh biểu phí của %s — 'chưa tra' không phải 'miễn phí', nên không công bố chi phí.",
			strings.Join(unverifiedNames(in.SpotFee, in.PerpFee), " và "))
		return out
	}

	out.EntrySpotBuy = EstimateFill(in.SpotBook, SideBuy, in.NotionalQuote)
	out.EntryPerpSell = EstimateFill(in.PerpBook, SideSell, in.NotionalQuote)
	out.ExitSpotSell = EstimateFill(in.SpotBook, SideSell, in.NotionalQuote)
	out.ExitPerpBuy = EstimateFill(in.PerpBook, SideBuy, in.NotionalQuote)

	// Labelled rather than indexed: "the second fill was refused" tells a
	// reader nothing, and the exit legs are the ones worth naming out loud.
	legs := []struct {
		labelVI string
		fill    FillEstimate
	}{
		{"vào lệnh — MUA spot", out.EntrySpotBuy},
		{"vào lệnh — BÁN perp", out.EntryPerpSell},
		{"thoát — BÁN spot", out.ExitSpotSell},
		{"thoát — MUA lại perp", out.ExitPerpBuy},
	}

	var slippagePct float64
	var notes []string
	for _, leg := range legs {
		if !leg.fill.Fillable {
			out.ReasonVI = fmt.Sprintf("Chân %s trên %s không khớp được: %s",
				leg.labelVI, leg.fill.Source, leg.fill.ReasonVI)
			return out
		}
		slippagePct += leg.fill.SlippagePct
		if leg.fill.DepthIsLowerBound {
			out.DepthIsLowerBound = true
			notes = append(notes, fmt.Sprintf("%s trên %s: %s", leg.labelVI, leg.fill.Source, leg.fill.NoteVI))
		}
	}

	out.OK = true
	out.FeesPct = feesPct
	out.SlippagePct = slippagePct
	out.TotalPct = feesPct + slippagePct
	out.NoteVI = strings.Join(notes, " ")
	out.AppliedVI = []string{
		fmt.Sprintf("Phí taker 4 lượt khớp (vào + ra, cả hai chân): %.4f%%.", feesPct),
		fmt.Sprintf("Slippage ước từ sổ lệnh ĐO ĐƯỢC cho đúng vốn %.0f, 4 lượt khớp: %.4f%%.",
			in.NotionalQuote, slippagePct),
		"MẪU SỐ: mọi con số %% ở đây tính trên notional MỘT CHÂN, không phải trên vốn thực bỏ ra. " +
			"Vị thế delta-neutral khoá trọn notional chân spot CỘNG ký quỹ chân perp, nên lợi suất trên " +
			"vốn triển khai thấp hơn — cỡ một nửa nếu perp dùng đòn bẩy cao, thấp hơn nữa nếu không.",
	}
	return out
}

// bookAgeRefusal enforces the caller's freshness budget, or says nothing when
// there is none.
//
// Both fields or neither: a budget without an evaluation instant cannot be
// applied, and silently skipping the check the caller asked for is worse than
// refusing to price.
func bookAgeRefusal(in RoundTripInput) string {
	if in.MaxBookAge <= 0 {
		return ""
	}
	if in.At.IsZero() {
		return fmt.Sprintf("Có hạn tuổi sổ %s nhưng không truyền thời điểm đánh giá — không kiểm được độ tươi.",
			in.MaxBookAge)
	}
	for _, book := range []depth.Summary{in.SpotBook, in.PerpBook} {
		if book.SampledAtMs <= 0 {
			return fmt.Sprintf("Sổ lệnh của %s không mang mốc lấy mẫu nên không biết cũ bao lâu.", book.Source)
		}
		age := in.At.Sub(time.UnixMilli(book.SampledAtMs))
		if age > in.MaxBookAge {
			return fmt.Sprintf("Sổ lệnh của %s đã %s tuổi, quá hạn %s — định giá lượt khớp trên sổ cũ hơn "+
				"một nhịp quét là ước lượng của quá khứ, không phải của hiện tại.", book.Source, age.Round(time.Second), in.MaxBookAge)
		}
	}
	return ""
}

// unverifiedNames lists ONLY the venues actually missing a schedule, so a
// half-unverified pair does not render "kraken_spot hoặc (đã xác minh)".
func unverifiedNames(schedules ...fees.Schedule) []string {
	var names []string
	for _, schedule := range schedules {
		if schedule.Verified {
			continue
		}
		if schedule.Source == "" {
			names = append(names, "(nguồn không tên)")
			continue
		}
		names = append(names, schedule.Source)
	}
	return names
}
