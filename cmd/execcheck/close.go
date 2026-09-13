package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/execution"
)

func runClose(ctx context.Context, intentID, waitUntil string, asJSON bool) int {
	if intentID == "" {
		fmt.Println("HỎNG — cần -intent <id>; xem -list")
		return exitFailed
	}
	st, err := loadState(intentID)
	if err != nil {
		fmt.Printf("HỎNG — không đọc được file ý định: %v\n", err)
		fmt.Println("  (đóng cần các con số lúc VÀO — giá khớp và mid tham chiếu — mà chỉ file này giữ)")
		return exitFailed
	}
	if st.ClosedAtMs > 0 {
		fmt.Printf("CẢNH BÁO — cache nói ý định này đã đóng lúc %s; vẫn hỏi sàn và đóng nốt phần sàn còn giữ\n",
			time.UnixMilli(st.ClosedAtMs).Format(time.RFC3339))
	}

	if waitUntil != "" {
		until, err := time.Parse(time.RFC3339, waitUntil)
		if err != nil {
			fmt.Printf("HỎNG — -wait-until không phải RFC3339: %v\n", err)
			return exitFailed
		}
		if d := time.Until(until); d > 0 {
			fmt.Printf("chờ tới %s (%s nữa) rồi mới đóng\n", until.Format(time.RFC3339), d.Round(time.Second))
			select {
			case <-ctx.Done():
				fmt.Println("HỎNG — hết hạn trước khi tới mốc chờ")
				return exitFailed
			case <-time.After(d):
			}
		}
	}

	cl, err := dial()
	if err != nil {
		if errors.Is(err, broker.ErrNoCredentials) {
			fmt.Printf("BỎ QUA — %v\n", err)
			return exitNoCredentials
		}
		fmt.Printf("HỎNG — %v\n", err)
		return exitFailed
	}

	// Fresh rules, fresh books, fresh prices. The exit decision is made on the
	// market as it is now, not on the one the position was opened against.
	spotMkt, err := readMarket(ctx, cl.spot, st.Symbol)
	if err != nil {
		fmt.Printf("HỎNG — chân spot: %v\n", err)
		return exitFailed
	}
	perpMkt, err := readMarket(ctx, cl.perp, st.Symbol)
	if err != nil {
		fmt.Printf("HỎNG — chân perp: %v\n", err)
		return exitFailed
	}

	cfg := execution.DefaultConfig()
	rec := execution.NewMemoryRecorder()
	tr, err := execution.NewOpener(cl.spot, cl.perp, cfg, rec)
	if err != nil {
		fmt.Printf("HỎNG — %v\n", err)
		return exitFailed
	}

	req := execution.CloseRequest{
		Intent: execution.Intent{
			ID:             intentID,
			Symbol:         st.Symbol,
			SpotInstrument: spotMkt.Rules.Instrument,
			PerpInstrument: perpMkt.Rules.Instrument,
			SpotBook:       spotMkt.Book,
			PerpBook:       perpMkt.Book,
			SpotPriceQuote: spotMkt.Price,
			PerpPriceQuote: perpMkt.Price,
		},
		// 0 means "ask the venue what is open", which is rule 7's answer.
		QtyCoin:                0,
		OpenedAtMs:             st.OpenedAtMs,
		EntrySpotAvgPriceQuote: st.SpotAvgPriceQuote,
		EntryPerpAvgPriceQuote: st.PerpAvgPriceQuote,
		EntrySpotRefMidQuote:   st.SpotRefMidQuote,
		EntryPerpRefMidQuote:   st.PerpRefMidQuote,
	}

	fmt.Printf("ĐÓNG %s · %s\n", intentID, st.Symbol)
	startedAt := time.Now()
	res, closeErr := tr.Close(ctx, req)
	elapsed := time.Since(startedAt)

	fmt.Printf("\nKẾT CỤC: %s   (mất %s)\n", res.Outcome, elapsed.Round(time.Millisecond))
	fmt.Printf("  sàn giữ trước  %+.8f coin (âm là SHORT) — đọc từ sàn, không từ file\n", res.VenuePositionQtyCoin)
	fmt.Printf("  yêu cầu đóng   %.8f coin\n", res.RequestedQtyCoin)
	fmt.Printf("  đã đóng        %.8f coin (spot %.8f, perp %.8f)\n",
		res.ClosedQtyCoin, res.Spot.UnwoundQtyCoin, res.Perp.UnwoundQtyCoin)
	fmt.Printf("  còn lại        %.8f coin mỗi chân\n", res.RemainingQtyCoin)
	fmt.Printf("  bằng chứng phẳng: %s\n", res.SpotFlatEvidenceVI)

	fmt.Println("\nCON SỐ — GỘP (chưa trừ trôi giá cặp, chưa trừ phí bằng tài sản khác quote, chưa trừ chi phí vốn):")
	fmt.Printf("  funding nhận   %+.8f   · %s\n", res.FundingReceivedQuote, res.FundingSourceVI)
	fmt.Printf("  mốc settle     %d\n", res.SettlementsCounted)
	fmt.Printf("  phí            %.8f   · %s\n", res.CommissionQuote, res.CommissionSourceVI)
	if res.CommissionOtherVI != "" {
		fmt.Printf("                 %s\n", res.CommissionOtherVI)
	}
	fmt.Printf("  trượt giá      %+.8f   · %s\n", res.SlippageQuote, res.PriceDriftPricedVI)
	fmt.Printf("  RealizedQuote  %+.8f   = funding − phí − trượt, KHÔNG phải lãi ròng\n", res.RealizedQuote)
	fmt.Printf("  trôi giá cặp   %+.8f   (BÊN NGOÀI con số trên — basis giữa vào và ra)\n", res.PairPriceDriftQuote)

	st.ClosedAtMs = time.Now().UnixMilli()
	st.ClosedQtyCoin = res.ClosedQtyCoin
	st.RealizedQuote = res.RealizedQuote
	st.FundingQuote = res.FundingReceivedQuote
	st.CommissionQuote = res.CommissionQuote
	st.SlippageQuote = res.SlippageQuote
	st.SettlementsCounted = res.SettlementsCounted
	st.PairPriceDriftQuote = res.PairPriceDriftQuote
	if closeErr != nil {
		st.NoteVI = closeErr.Error()
	}
	if err := saveState(st); err != nil {
		fmt.Printf("CẢNH BÁO — không ghi được file ý định: %v\n", err)
	}
	if asJSON {
		blob, _ := json.MarshalIndent(st, "", "  ")
		fmt.Printf("\n%s\n", blob)
	}
	fmt.Printf("\nsự kiện:%s\n", rec.Dump())

	if closeErr != nil {
		fmt.Printf("\nLÝ DO: %s\n", closeErr.Error())
		return exitFailed
	}
	return exitOK
}
