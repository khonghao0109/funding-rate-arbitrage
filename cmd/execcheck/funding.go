package main

import (
	"context"
	"fmt"
	"math"
	"time"

	"futures-arbitrage-scanner/internal/broker"
)

// -funding-check: the venue's own funding against what the book computes.
//
// This is step 4.5's acceptance question — "does the figure we read from the
// exchange match rate x notional?" — and the two numbers are printed side by
// side with their difference, never one in place of the other.
//
// CLAUDE.md rule 6 is what makes the comparison meaningful rather than
// circular: the venue's figure is a SETTLEMENT that happened, one row per
// event, and the book's figure is that same event priced by hand. Nothing here
// multiplies an APR by a holding time, and a position that was not open at the
// stamp simply has no row to compare.
func runFundingCheck(ctx context.Context, intentID string) int {
	if intentID == "" {
		fmt.Println("HỎNG — cần -intent <id>")
		return exitFailed
	}
	st, err := loadState(intentID)
	if err != nil {
		fmt.Printf("HỎNG — không đọc được file ý định: %v\n", err)
		return exitFailed
	}
	cl, err := dial()
	if err != nil {
		fmt.Printf("HỎNG — %v\n", err)
		return exitFailed
	}

	endMs := time.Now().UnixMilli()
	if st.ClosedAtMs > 0 {
		endMs = st.ClosedAtMs
	}
	rows, err := cl.perp.FundingIncome(ctx, broker.MarketFuturesUSDM, st.Symbol, st.OpenedAtMs, endMs)
	if err != nil {
		fmt.Printf("HỎNG — không đọc được funding từ sàn: %v\n", err)
		return exitFailed
	}
	mp, err := cl.perp.MarkPrice(ctx, broker.MarketFuturesUSDM, st.Symbol)
	if err != nil {
		fmt.Printf("HỎNG — không đọc được premiumIndex: %v\n", err)
		return exitFailed
	}

	qtyCoin := st.PerpFilledQtyCoin
	fmt.Printf("ĐỐI CHIẾU FUNDING · %s · %s\n", intentID, st.Symbol)
	fmt.Printf("  cửa sổ giữ     %s → %s\n",
		time.UnixMilli(st.OpenedAtMs).Format(time.RFC3339), time.UnixMilli(endMs).Format(time.RFC3339))
	fmt.Printf("  chân perp      SHORT %.8f coin\n", qtyCoin)
	fmt.Printf("  sàn báo lastFundingRate = %.8f, mốc kế tiếp %s\n",
		mp.LastFundingRateFrac, time.UnixMilli(mp.NextFundingTimeMs).Format(time.RFC3339))

	if len(rows) == 0 {
		fmt.Println("\n  SÀN: 0 mốc settle trong cửa sổ — vị thế chưa từng mở qua một mốc nào (quy tắc 6)")
		fmt.Println("  SỔ : 0 theo định nghĩa; không có gì để đối chiếu")
		return exitOK
	}

	fmt.Printf("\n  (1) SÀN — /fapi/v1/income, incomeType=FUNDING_FEE, %d dòng:\n", len(rows))
	venueTotal := 0.0
	for _, r := range rows {
		venueTotal += r.IncomeQuote
		fmt.Printf("      %s  %+.8f %s  (tranId %s)\n",
			time.UnixMilli(r.SettledAtMs).Format(time.RFC3339), r.IncomeQuote, r.Asset, r.TranID)
	}
	fmt.Printf("      cộng lại %+.8f\n", venueTotal)

	// The book's figure. Binance settles funding as positionValue x rate, with
	// the LONG paying when the rate is positive — so a SHORT's income carries
	// the SAME sign as the rate.
	//
	// The first version of this line flipped it, on the reasoning that "a short
	// receives, so the sign is reversed". The magnitude matched the venue to
	// four decimal places and the sign did not, which is how the error showed
	// up in one run — and is the whole reason this comparison exists instead of
	// a comment claiming the arithmetic is obvious.
	bookTotal := mp.LastFundingRateFrac * qtyCoin * mp.MarkPriceQuote
	fmt.Printf("\n  (2) SỔ  — rate × cỡ × giá đánh dấu = %.8f × %.8f × %.4f = %+.8f\n",
		mp.LastFundingRateFrac, qtyCoin, mp.MarkPriceQuote, bookTotal)
	fmt.Println("      (sàn tính funding = giá trị vị thế × rate, BÊN LONG trả khi rate dương,")
	fmt.Println("       nên thu nhập của bên SHORT cùng dấu với rate)")

	diff := venueTotal - bookTotal
	fmt.Printf("\n  (3) SAI SỐ      %+.8f   (%.4f%% của số sàn)\n", diff, pctOf(diff, venueTotal))
	fmt.Println("\n  Vì sao hai số KHÔNG bắt buộc bằng nhau, và cái nào đúng:")
	fmt.Println("    · số của SÀN là bằng chứng — nó là khoản thực đã ghi vào tài khoản;")
	fmt.Println("    · số của SỔ dùng lastFundingRate và giá đánh dấu ĐỌC BÂY GIỜ, không phải")
	fmt.Println("      giá đánh dấu tại đúng mốc settle, mà sàn không công bố lại;")
	fmt.Println("    · sàn tính trên giá đánh dấu tại mốc, không trên giá vào của ta.")
	fmt.Println("    Sai số nhỏ là sai số của phép dựng lại, không phải của khoản tiền.")
	return exitOK
}

func pctOf(part, whole float64) float64 {
	if whole == 0 || math.IsNaN(whole) {
		return math.NaN()
	}
	return part / whole * 100
}
