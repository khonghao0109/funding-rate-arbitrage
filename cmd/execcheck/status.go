package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/execution"
)

// -status and -close: everything here asks the VENUE.
//
// The state file supplies the intent id and the figures that were true at an
// instant this process cannot revisit. It supplies nothing about NOW. Both
// commands below rebuild the ClientOrderIDs from the intent id and read the
// orders, the position and the balances back from the venues — and print the
// cache beside the venue whenever the two disagree, rather than picking one.
// CLAUDE.md rule 7: local bookkeeping is a cache and is assumed stale until
// reconciled.

func runStatus(ctx context.Context, intentID string, asJSON bool) int {
	if intentID == "" {
		fmt.Println("HỎNG — cần -intent <id>; xem -list")
		return exitFailed
	}
	st, err := loadState(intentID)
	if err != nil {
		fmt.Printf("CẢNH BÁO — không đọc được file cache: %v\n", err)
		fmt.Println("  vẫn hỏi sàn được, vì mọi ClientOrderID đều SUY RA từ id ý định")
		st = state{IntentID: intentID}
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
	symbol := st.Symbol
	if symbol == "" {
		fmt.Println("HỎNG — file cache không nêu symbol, và symbol không suy ra được từ id")
		return exitFailed
	}

	fmt.Printf("Ý ĐỊNH %s · %s\n", intentID, symbol)
	fmt.Println("CACHE (file — không phải bằng chứng):")
	fmt.Printf("  mở lúc         %s\n", time.UnixMilli(st.OpenedAtMs).Format(time.RFC3339))
	fmt.Printf("  chân spot      %.8f coin @ %.4f\n", st.SpotFilledQtyCoin, st.SpotAvgPriceQuote)
	fmt.Printf("  chân perp      %.8f coin @ %.4f\n", st.PerpFilledQtyCoin, st.PerpAvgPriceQuote)
	if st.ClosedAtMs > 0 {
		fmt.Printf("  ĐÃ ĐÓNG        %s, %.8f coin\n", time.UnixMilli(st.ClosedAtMs).Format(time.RFC3339), st.ClosedQtyCoin)
	}

	fmt.Println("\nSÀN (đọc lại bây giờ — đây mới là bằng chứng):")
	var mismatches []string

	// The four orders this intent could have produced, every id DERIVED.
	for _, o := range []struct {
		nameVI string
		b      broker.Broker
		market broker.Market
		id     string
		expect float64
	}{
		{"mở spot", cl.spot, broker.MarketSpot, execution.LegClientOrderID(intentID, execution.LegSpot), st.SpotFilledQtyCoin},
		{"mở perp", cl.perp, broker.MarketFuturesUSDM, execution.LegClientOrderID(intentID, execution.LegPerp), st.PerpFilledQtyCoin},
		{"đóng spot", cl.spot, broker.MarketSpot, execution.CloseClientOrderID(intentID, execution.LegSpot), 0},
		{"đóng perp", cl.perp, broker.MarketFuturesUSDM, execution.CloseClientOrderID(intentID, execution.LegPerp), 0},
	} {
		order, err := o.b.GetOrder(ctx, broker.OrderQuery{Market: o.market, Symbol: symbol, ClientOrderID: o.id})
		switch {
		case errors.Is(err, broker.ErrOrderNotFound):
			fmt.Printf("  %-10s sàn KHÔNG có lệnh id %s\n", o.nameVI, o.id)
			if o.expect > 0 {
				mismatches = append(mismatches, fmt.Sprintf("%s: cache nói khớp %.8f coin, sàn không có lệnh này", o.nameVI, o.expect))
			}
		case err != nil:
			fmt.Printf("  %-10s KHÔNG đọc được: %v\n", o.nameVI, err)
			mismatches = append(mismatches, fmt.Sprintf("%s: không đọc được từ sàn — trạng thái VẪN MƠ HỒ", o.nameVI))
		default:
			fmt.Printf("  %-10s %s  khớp %.8f/%.8f coin @ %.4f  (orderId %s)\n",
				o.nameVI, order.Status, order.FilledQtyCoin, order.QtyCoin, order.AvgFillPriceQuote, order.VenueOrderID)
			if o.expect > 0 && math.Abs(order.FilledQtyCoin-o.expect) > 1e-9 {
				mismatches = append(mismatches, fmt.Sprintf("%s: cache %.8f coin, SÀN %.8f coin", o.nameVI, o.expect, order.FilledQtyCoin))
			}
		}
	}

	// The position, the balances, and anything still resting.
	pos, err := cl.perp.GetPosition(ctx, broker.MarketFuturesUSDM, symbol)
	if err != nil {
		fmt.Printf("  vị thế perp KHÔNG đọc được: %v\n", err)
	} else {
		fmt.Printf("  vị thế perp    %+.8f coin (âm là SHORT) — sàn báo\n", pos.QtyCoin)
		expected := -st.PerpFilledQtyCoin
		if st.ClosedAtMs > 0 {
			expected = 0
		}
		if math.Abs(pos.QtyCoin-expected) > 1e-8 {
			mismatches = append(mismatches, fmt.Sprintf("vị thế perp: cache ngụ ý %+.8f coin, SÀN báo %+.8f coin", expected, pos.QtyCoin))
		}
	}
	printOpenOrders(ctx, cl.spot, broker.MarketSpot, symbol, "spot")
	printOpenOrders(ctx, cl.perp, broker.MarketFuturesUSDM, symbol, "perp")

	if bal, err := cl.spot.GetBalance(ctx, broker.MarketSpot); err == nil {
		base := baseAssetOf(symbol)
		for _, b := range bal {
			if b.Asset == base {
				fmt.Printf("  số dư %-8s %.8f (tự do %.8f + khoá %.8f)\n", b.Asset, b.TotalQtyCoin(), b.FreeQtyCoin, b.LockedQtyCoin)
			}
		}
	}

	// THE question, and it is not "does the cache match the venue" — a cache
	// that agrees with the venue about a pair that is not hedged is two
	// consistent descriptions of a naked position. So the hedge is computed
	// from the VENUE's own record of this intent's own orders, every id
	// derived, and it is checked even when nothing else disagreed.
	spotNet, perpNet := intentNets(ctx, cl, intentID, symbol)
	residual := spotNet.QtyCoin + perpNet.QtyCoin
	fmt.Println("\nPHÒNG HỘ, theo lệnh CỦA CHÍNH Ý ĐỊNH NÀY (đọc từ sàn):")
	fmt.Printf("  spot ròng  %+.8f coin  [%s]\n", spotNet.QtyCoin, joinOr(spotNet.SeenVI, "không lệnh nào khớp"))
	fmt.Printf("  perp ròng  %+.8f coin  [%s]\n", perpNet.QtyCoin, joinOr(perpNet.SeenVI, "không lệnh nào khớp"))
	fmt.Printf("  LỆCH       %+.8f coin\n", residual)
	for _, u := range append(spotNet.Unreadab, perpNet.Unreadab...) {
		fmt.Printf("  ⚠ KHÔNG đọc được %s — con số trên chưa đầy đủ\n", u)
	}
	if math.Abs(residual) > 1e-9 {
		fmt.Println("  ⚠ CẶP KHÔNG PHÒNG HỘ — chạy -reconcile để xem cách cân lại")
		mismatches = append(mismatches, fmt.Sprintf("cặp lệch %+.8f coin theo chính lệnh của ý định này", residual))
	}

	if len(mismatches) > 0 {
		fmt.Println("\nLỆCH GIỮA CACHE VÀ SÀN — in cả hai, KHÔNG tự hoà giải:")
		for _, m := range mismatches {
			fmt.Printf("  · %s\n", m)
		}
	} else {
		fmt.Println("\ncache và sàn khớp nhau")
	}
	if asJSON {
		blob, _ := json.MarshalIndent(st, "", "  ")
		fmt.Printf("\n%s\n", blob)
	}
	if len(mismatches) > 0 {
		return exitFailed
	}
	return exitOK
}

func printOpenOrders(ctx context.Context, b broker.Broker, market broker.Market, symbol, nameVI string) {
	open, err := b.OpenOrders(ctx, market, symbol)
	if err != nil {
		fmt.Printf("  lệnh mở %-6s KHÔNG đọc được: %v\n", nameVI, err)
		return
	}
	fmt.Printf("  lệnh mở %-6s %d\n", nameVI, len(open))
	for _, o := range open {
		fmt.Printf("      %s %s %.8f @ %.4f (id %s)\n", o.Side, o.Status, o.QtyCoin, o.PriceQuote, o.ClientOrderID)
	}
}

// baseAssetOf strips the quote suffix this project's symbols use.
//
// It is a LAST RESORT and only for a display line: CLAUDE.md's assets trap says
// base and quote must come from what the venue DECLARES, never from slicing the
// symbol string. Everything that decides anything reads
// exchanges.Instrument.BaseAsset, which the rules fetch fills from the venue's
// own exchangeInfo; this is here so -status can label a balance row when the
// cache holds no rules.
func baseAssetOf(symbol string) string {
	for _, quote := range []string{"USDT", "USDC", "BUSD", "USD"} {
		if len(symbol) > len(quote) && symbol[len(symbol)-len(quote):] == quote {
			return symbol[:len(symbol)-len(quote)]
		}
	}
	return symbol
}
