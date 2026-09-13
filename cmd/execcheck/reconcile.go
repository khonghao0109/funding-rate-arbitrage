package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/execution"
)

// -reconcile: put a pair back on its feet after something left it unbalanced.
//
// It exists because that happened. On 2026-09-13 the 4.5 acceptance read a
// futures MARKET acknowledgement as a fill, reported both legs intact, and left
// 0.0008 BTC of spot long with no hedge. The code that caused it is fixed, but
// an operator facing an unbalanced pair still needs a way to square it, and
// "place the opposite order by hand" is the way mistakes get compounded.
//
// Everything it decides is decided from the VENUE's own record of THIS intent's
// orders — every id derived from the intent id — so it can run against a
// position no file describes, and it never touches an order that is not this
// intent's.

// legNet is what one market holds because of this intent's own orders: buys
// minus sells over the ids derived from the intent id, read from the venue.
type legNet struct {
	QtyCoin  float64
	SeenVI   []string
	Unreadab []string
}

func netFromVenue(ctx context.Context, b broker.Broker, market broker.Market, symbol string, ids map[string]string) legNet {
	var out legNet
	for nameVI, id := range ids {
		order, err := b.GetOrder(ctx, broker.OrderQuery{Market: market, Symbol: symbol, ClientOrderID: id})
		switch {
		case errors.Is(err, broker.ErrOrderNotFound):
			continue
		case err != nil:
			// NOT treated as zero: an order we cannot read is an order whose
			// quantity is unknown, and reconciling on a guess is how a hedge
			// becomes a double position.
			out.Unreadab = append(out.Unreadab, fmt.Sprintf("%s (%s)", nameVI, err.Error()))
			continue
		}
		signed := order.FilledQtyCoin
		if order.Side == broker.SideSell {
			signed = -signed
		}
		out.QtyCoin += signed
		if order.FilledQtyCoin > 0 {
			out.SeenVI = append(out.SeenVI, fmt.Sprintf("%s %s %.8f", nameVI, order.Side, order.FilledQtyCoin))
		}
	}
	return out
}

func intentNets(ctx context.Context, cl clients, intentID, symbol string) (spot, perp legNet) {
	spot = netFromVenue(ctx, cl.spot, broker.MarketSpot, symbol, map[string]string{
		"mở":   execution.LegClientOrderID(intentID, execution.LegSpot),
		"đóng": execution.CloseClientOrderID(intentID, execution.LegSpot),
		"gỡ":   execution.UnwindClientOrderID(intentID, execution.LegSpot),
		"cân":  reconcileClientOrderID(intentID, execution.LegSpot),
	})
	perp = netFromVenue(ctx, cl.perp, broker.MarketFuturesUSDM, symbol, map[string]string{
		"mở":   execution.LegClientOrderID(intentID, execution.LegPerp),
		"đóng": execution.CloseClientOrderID(intentID, execution.LegPerp),
		"gỡ":   execution.UnwindClientOrderID(intentID, execution.LegPerp),
		"cân":  reconcileClientOrderID(intentID, execution.LegPerp),
	})
	return spot, perp
}

// reconcileClientOrderID is derived like every other id here, so a second run
// of -reconcile finds the first one's order instead of sending another.
func reconcileClientOrderID(intentID string, leg execution.LegName) string {
	return execution.LegClientOrderID(intentID+"|reconcile", leg)
}

func runReconcile(ctx context.Context, intentID string, apply bool) int {
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

	spotNet, perpNet := intentNets(ctx, cl, intentID, st.Symbol)
	tolerance := math.Max(spotMkt.Rules.StepSizeCoin, perpMkt.Rules.StepSizeCoin)
	residual := spotNet.QtyCoin + perpNet.QtyCoin

	fmt.Printf("CÂN LẠI %s · %s\n", intentID, st.Symbol)
	fmt.Printf("  theo lệnh CỦA CHÍNH Ý ĐỊNH NÀY, đọc từ sàn:\n")
	fmt.Printf("    spot ròng  %+.8f coin  [%s]\n", spotNet.QtyCoin, joinOr(spotNet.SeenVI, "không lệnh nào khớp"))
	fmt.Printf("    perp ròng  %+.8f coin  [%s]\n", perpNet.QtyCoin, joinOr(perpNet.SeenVI, "không lệnh nào khớp"))
	fmt.Printf("    LỆCH       %+.8f coin (dung sai %.8f)\n", residual, tolerance)
	for _, u := range append(spotNet.Unreadab, perpNet.Unreadab...) {
		fmt.Printf("    ⚠ KHÔNG đọc được %s — con số trên CHƯA đầy đủ, không cân khi còn dòng này\n", u)
	}
	if len(spotNet.Unreadab)+len(perpNet.Unreadab) > 0 {
		return exitFailed
	}
	if math.Abs(residual) <= tolerance {
		fmt.Println("  cặp đã cân trong dung sai — không làm gì")
		return exitOK
	}

	// The longer side is sold down. Which market that is follows from the sign:
	// a positive residual is too much LONG (spot), a negative one too much
	// SHORT (perp).
	var (
		b          broker.Broker = cl.spot
		market                   = broker.MarketSpot
		side                     = broker.SideSell
		rules                    = spotMkt.Rules.Instrument
		priceQuote               = spotMkt.Price
		leg                      = execution.LegSpot
		reduceOnly bool
	)
	if residual < 0 {
		b, market, side = cl.perp, broker.MarketFuturesUSDM, broker.SideBuy
		rules, priceQuote, leg, reduceOnly = perpMkt.Rules.Instrument, perpMkt.Price, execution.LegPerp, true
	}
	rounded, err := broker.RoundOrder(broker.RoundRequest{
		Rules: rules, Side: side, Type: broker.OrderTypeMarket,
		QtyCoin: math.Abs(residual), PriceQuote: priceQuote,
	})
	if err != nil {
		fmt.Printf("  KHÔNG cân được: %v\n", err)
		fmt.Println("  (một phần dư dưới mức tối thiểu của sàn là phần KHÔNG lệnh nào đóng được — phải xử lý tay)")
		return exitFailed
	}
	id := reconcileClientOrderID(intentID, leg)
	fmt.Printf("  sẽ gửi: %s MARKET %.8f coin trên %s, id %s\n", side, rounded.QtyCoin, market, id)
	if !apply {
		fmt.Println("  (chạy lại với -apply để thực sự gửi)")
		return exitOK
	}

	order, err := b.PlaceOrder(ctx, broker.PlaceOrderRequest{
		Market: market, Symbol: st.Symbol, Side: side, Type: broker.OrderTypeMarket,
		ClientOrderID: id, QtyCoin: rounded.QtyCoin, ReduceOnly: reduceOnly,
	})
	if err != nil {
		fmt.Printf("  lệnh cân HỎNG: %v\n", err)
		return exitFailed
	}
	// An acknowledgement is not a fill — the very mistake this command exists
	// to clean up after.
	q := broker.OrderQuery{Market: market, Symbol: st.Symbol, ClientOrderID: id}
	deadline := time.Now().Add(10 * time.Second)
	for !order.Status.Done() && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if latest, err := b.GetOrder(ctx, q); err == nil {
			order = latest
		}
	}
	fmt.Printf("  sàn báo: %s, khớp %.8f coin @ %.4f (orderId %s)\n",
		order.Status, order.FilledQtyCoin, order.AvgFillPriceQuote, order.VenueOrderID)

	spotNet, perpNet = intentNets(ctx, cl, intentID, st.Symbol)
	after := spotNet.QtyCoin + perpNet.QtyCoin
	fmt.Printf("  LỆCH sau khi cân: %+.8f coin\n", after)
	if math.Abs(after) > tolerance {
		fmt.Println("  VẪN LỆCH — dừng và xử lý tay")
		return exitFailed
	}
	st.NoteVI = fmt.Sprintf("đã cân lại %s: gửi %s %.8f coin trên %s, lệch còn %+.8f",
		time.Now().Format(time.RFC3339), side, rounded.QtyCoin, market, after)
	if err := saveState(st); err != nil {
		fmt.Printf("  CẢNH BÁO — không ghi được file: %v\n", err)
	}
	return exitOK
}

func joinOr(parts []string, emptyVI string) string {
	if len(parts) == 0 {
		return emptyVI
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
