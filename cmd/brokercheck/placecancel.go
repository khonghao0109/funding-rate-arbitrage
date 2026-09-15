package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
)

// The step-4.2 acceptance: place a resting order on TESTNET, find it, cancel
// it, and prove it is gone.
//
// # Why the order cannot fill
//
// It is a LIMIT GTC BUY priced far BELOW the market — the factor is -far-frac,
// 0.5 by default, so half the going price. A buy that far under the book rests
// and is never matched, which is what makes this safe to run repeatedly. The
// size is the SMALLEST the venue's own rules allow, which on futures BTCUSDT
// testnet means clearing a MIN_NOTIONAL of 50 rather than a minQty of 0.0001.
//
// If it fills anyway, this command says so loudly, closes the position with the
// opposite order immediately, and exits non-zero. A fill is not a disaster on
// testnet, but an unexplained one means the price model here is wrong, and the
// same wrongness at 4.6 is a real position.
//
// # Rule 7
//
// The position and the balance printed at the end are read FROM THE VENUE after
// the cancel, never accumulated from what this process believes it did.

type placeCancelResult struct {
	Checks []check
	Filled bool
}

func runPlaceCancel(ctx context.Context, market broker.Market, symbol string, creds broker.Credentials,
	recvWindowMs int64, farFrac float64, capture *binancebroker.Capture) placeCancelResult {

	nameVI := fmt.Sprintf("%s %s", market, symbol)
	fmt.Printf("── ĐẶT & HUỶ · %s\n", nameVI)

	cfg, err := binancebroker.DefaultConfig(market, creds)
	if err != nil {
		return placeCancelResult{Checks: []check{{nameVI + ": cấu hình", false, err.Error()}}}
	}
	cfg.RecvWindowMs = recvWindowMs
	cfg.UserAgentVI = "funding-rate-arbitrage/brokercheck"
	if capture != nil {
		cfg.ObserveResponse = capture.Observe
	}
	client, err := binancebroker.New(market, cfg)
	if err != nil {
		return placeCancelResult{Checks: []check{{nameVI + ": dựng client", false, err.Error()}}}
	}

	var out []check
	fail := func(step string, err error) placeCancelResult {
		fmt.Printf("   %s: %v\n\n", step, err)
		return placeCancelResult{Checks: append(out, check{nameVI + ": " + step, false, err.Error()})}
	}

	// The clock, before anything signed.
	skewMs, err := client.HTTP().SyncClock(ctx)
	if err != nil {
		return fail("giờ server", err)
	}
	fmt.Printf("   giờ server · HTTP 200 · lệch %d ms\n", skewMs)

	// The rules of THIS market on THIS testnet, read now — not from a snapshot.
	rules, err := client.FetchInstrument(ctx, symbol)
	if err != nil {
		return fail("exchangeInfo", err)
	}
	fmt.Printf("   luật %s (đọc từ exchangeInfo của chính testnet): tick %g · step %g · minQty %g · minNotional %g · sàn giá MUA tối thiểu %g × giá · trạng thái %s\n",
		symbol, rules.TickSizeQuote, rules.StepSizeCoin, rules.MinQtyCoin, rules.MinNotionalQuote, rules.BuyPriceFloorFrac, rules.Status)
	out = append(out, check{nameVI + ": luật sàn", rules.StepSizeCoin > 0 && rules.TickSizeQuote > 0,
		fmt.Sprintf("tick %g, step %g, minQty %g, minNotional %g", rules.TickSizeQuote, rules.StepSizeCoin, rules.MinQtyCoin, rules.MinNotionalQuote)})

	refPrice, err := client.FetchPriceQuote(ctx, symbol)
	if err != nil {
		return fail("giá tham chiếu", err)
	}

	// The price: far below the market, rounded the passive way for a buy.
	farPrice, qtyCoin, err := restingOrderSize(rules, refPrice, farFrac)
	if err != nil {
		return fail("tính cỡ lệnh", err)
	}
	rounded, err := broker.RoundOrder(broker.RoundRequest{
		Rules: rules.Instrument, Side: broker.SideBuy, Type: broker.OrderTypeLimitGTC,
		QtyCoin: qtyCoin, PriceQuote: farPrice, Price: broker.PriceRoundPassive,
	})
	if err != nil {
		return fail("làm tròn", err)
	}
	fmt.Printf("   giá thị trường %g → đặt MUA ở %s (%.0f%% dưới), qty %s, notional %.2f\n",
		refPrice, rounded.PriceQuoteText, farFrac*100, rounded.QtyCoinText, rounded.NotionalQuote)

	clientOrderID := fmt.Sprintf("bc-%s-%d", shortMarket(market), time.Now().UnixMilli())
	q := broker.OrderQuery{Market: market, Symbol: symbol, ClientOrderID: clientOrderID}

	// 1. PLACE
	placed, err := client.PlaceOrder(ctx, broker.PlaceOrderRequest{
		Market: market, Symbol: symbol, Side: broker.SideBuy, Type: broker.OrderTypeLimitGTC,
		ClientOrderID: clientOrderID, QtyCoin: rounded.QtyCoin, PriceQuote: rounded.PriceQuote,
	})
	if err != nil {
		return fail("đặt lệnh", err)
	}
	fmt.Printf("   ĐẶT   · orderId %s · clientOrderId %s · trạng thái %s · khớp %g/%g\n",
		placed.VenueOrderID, placed.ClientOrderID, placed.Status, placed.FilledQtyCoin, placed.QtyCoin)
	out = append(out, check{nameVI + ": đặt lệnh", placed.VenueOrderID != "" && placed.ClientOrderID == clientOrderID,
		fmt.Sprintf("orderId %s, clientOrderId %s, trạng thái %s", placed.VenueOrderID, placed.ClientOrderID, placed.Status)})

	// From here the order exists at the venue. Whatever fails below, it must
	// not be left resting.
	defer func() {
		// Only if it is still live. Cancelling a finished order sends a
		// request the venue can only refuse, and leaves a -2011 in the log of
		// every clean run.
		safeCtx := context.WithoutCancel(ctx)
		if o, err := client.GetOrder(safeCtx, q); err == nil && !o.Status.Done() {
			_, _ = client.CancelOrder(safeCtx, q)
		}
	}()

	// 2. QUERY BY THE CALLER'S ID — the lookup the whole interface is built on.
	found, err := client.GetOrder(ctx, q)
	if err != nil {
		return fail("tra theo clientOrderId", err)
	}
	fmt.Printf("   TRA   · theo clientOrderId · orderId %s · trạng thái %s\n", found.VenueOrderID, found.Status)
	out = append(out, check{nameVI + ": tra theo clientOrderId",
		found.VenueOrderID == placed.VenueOrderID,
		fmt.Sprintf("tìm thấy orderId %s, trạng thái %s", found.VenueOrderID, found.Status)})

	// An unexpected fill: close it at once and refuse the acceptance.
	if found.FilledQtyCoin > 0 {
		fmt.Printf("   ⚠ LỆNH ĐÃ KHỚP %g — đóng ngay bằng lệnh đối ứng\n", found.FilledQtyCoin)
		out = append(out, closeUnexpectedFill(ctx, client, market, symbol, rules, found)...)
		return placeCancelResult{Checks: out, Filled: true}
	}

	// 3. CANCEL
	cancelled, err := client.CancelOrder(ctx, q)
	if err != nil {
		return fail("huỷ lệnh", err)
	}
	fmt.Printf("   HUỶ   · trạng thái %s\n", cancelled.Status)
	out = append(out, check{nameVI + ": huỷ lệnh", cancelled.Status == broker.OrderStatusCanceled,
		fmt.Sprintf("trạng thái sau huỷ: %s", cancelled.Status)})

	// 3b. CANCEL AGAIN. broker.Broker says cancelling an order that is already
	//     gone returns ErrOrderNotFound rather than succeeding, because "there
	//     is nothing there" and "I removed it" are different facts to a caller
	//     unwinding two legs. This is where that is proved against the real
	//     venue, and it is what produces the -2011 recording.
	_, againErr := client.CancelOrder(ctx, q)
	fmt.Printf("   HUỶ2  · huỷ lại lệnh đã huỷ → %v\n", againErr)
	out = append(out, check{nameVI + ": huỷ lại báo không tìm thấy",
		errors.Is(againErr, broker.ErrOrderNotFound),
		fmt.Sprintf("huỷ lần hai trả về %v (cần ErrOrderNotFound)", againErr)})

	// 4. RE-QUERY: must be CANCELED at the venue, not merely in our belief.
	after, err := client.GetOrder(ctx, q)
	if err != nil {
		return fail("tra lại sau huỷ", err)
	}
	fmt.Printf("   TRA   · sau huỷ · trạng thái %s\n", after.Status)
	out = append(out, check{nameVI + ": tra lại sau huỷ", after.Status == broker.OrderStatusCanceled,
		fmt.Sprintf("trạng thái %s (cần CANCELED)", after.Status)})

	// 5. OPEN ORDERS: it must be gone from the venue's own list.
	open, err := client.OpenOrders(ctx, market, symbol)
	if err != nil {
		return fail("lệnh mở", err)
	}
	still := false
	for _, o := range open {
		if o.ClientOrderID == clientOrderID {
			still = true
		}
	}
	fmt.Printf("   MỞ    · %d lệnh mở cho %s · lệnh vừa huỷ còn trong danh sách: %v\n", len(open), symbol, still)
	out = append(out, check{nameVI + ": không còn trong lệnh mở", !still,
		fmt.Sprintf("%d lệnh mở, lệnh vừa huỷ %s", len(open), presentVI(still))})

	// 6. STATE FROM THE VENUE (rule 7), never from what this process believes.
	if market == broker.MarketFuturesUSDM {
		pos, err := client.GetPosition(ctx, market, symbol)
		if err != nil {
			return fail("vị thế", err)
		}
		fmt.Printf("   VỊ THẾ· đọc từ SÀN · %s = %g coin (lãi/lỗ chưa thực hiện %g, GỘP)\n", symbol, pos.QtyCoin, pos.UnrealizedPnLQuote)
		out = append(out, check{nameVI + ": vị thế đọc từ sàn", pos.Flat(),
			fmt.Sprintf("%g coin — phải bằng 0 sau khi huỷ mà không khớp", pos.QtyCoin)})
	}
	balances, err := client.GetBalance(ctx, market)
	if err != nil {
		return fail("số dư", err)
	}
	nonZero := 0
	for _, b := range balances {
		if b.TotalQtyCoin() != 0 {
			nonZero++
		}
	}
	fmt.Printf("   SỐ DƯ · đọc từ SÀN · %d tài sản (%d khác 0) — không in số lượng\n", len(balances), nonZero)
	out = append(out, check{nameVI + ": số dư đọc từ sàn", len(balances) > 0,
		fmt.Sprintf("%d tài sản (%d khác 0)", len(balances), nonZero)})

	fmt.Printf("   %s\n\n", client.HTTP().Budget().ReportVI())
	return placeCancelResult{Checks: out}
}

// restingOrderSize returns a price far under the market and the SMALLEST
// quantity the venue's rules allow at that price.
//
// The size-up to reach minNotional happens HERE, at the call site, deliberately
// and in the open — broker.RoundOrder refuses to do it, precisely so that
// trading more than was asked for is always somebody's explicit decision.
func restingOrderSize(rules binancebroker.MarketRules, refPrice, farFrac float64) (priceQuote, qtyCoin float64, err error) {
	if refPrice <= 0 {
		return 0, 0, fmt.Errorf("giá tham chiếu không dùng được: %v", refPrice)
	}
	if farFrac <= 0 || farFrac >= 1 {
		return 0, 0, fmt.Errorf("-far-frac phải nằm trong (0,1), nhận %v", farFrac)
	}
	if rules.StepSizeCoin <= 0 || rules.TickSizeQuote <= 0 {
		return 0, 0, fmt.Errorf("sàn không công bố step/tick cho %s", rules.Symbol)
	}
	target := refPrice * (1 - farFrac)

	// The venue may forbid resting that far out. Spot's PERCENT_PRICE_BY_SIDE
	// puts a floor under a BUY at bidMultiplierDown x an average price, and
	// measured 2026-09-13 that average is taken over the last FIVE MINUTES —
	// so pricing at exactly the multiple of the LAST price lands just under
	// the limit whenever the average has drifted up, which is how this run
	// first failed with "-1013 Filter failure: PERCENT_PRICE_BY_SIDE".
	//
	// The margin is against that drift, not against the multiplier.
	if rules.BuyPriceFloorFrac > 0 {
		const driftMargin = 1.02
		if floor := refPrice * rules.BuyPriceFloorFrac * driftMargin; floor > target {
			target = floor
		}
	}
	priceQuote = math.Floor(target/rules.TickSizeQuote) * rules.TickSizeQuote

	qtyCoin = rules.MinQtyCoin
	if qtyCoin <= 0 {
		qtyCoin = rules.StepSizeCoin
	}
	// Raise to clear minNotional at the FAR price, rounding UP to the step.
	if rules.MinNotionalQuote > 0 {
		needed := rules.MinNotionalQuote / priceQuote
		steps := math.Ceil(needed/rules.StepSizeCoin - 1e-9)
		if byNotional := steps * rules.StepSizeCoin; byNotional > qtyCoin {
			qtyCoin = byNotional
		}
		// One extra step of headroom: the notional is checked against the
		// rounded price, and landing exactly on the boundary is where a
		// venue's own rounding and ours disagree.
		qtyCoin += rules.StepSizeCoin
	}
	if rules.MaxQtyCoin > 0 && qtyCoin > rules.MaxQtyCoin {
		return 0, 0, fmt.Errorf("cỡ tối thiểu %v vượt maxQty %v của %s", qtyCoin, rules.MaxQtyCoin, rules.Symbol)
	}
	return priceQuote, qtyCoin, nil
}

// closeUnexpectedFill sends the opposite MARKET order at once.
func closeUnexpectedFill(ctx context.Context, client *binancebroker.Client, market broker.Market,
	symbol string, rules binancebroker.MarketRules, filled broker.Order) []check {

	nameVI := fmt.Sprintf("%s %s", market, symbol)
	rounded, err := broker.RoundOrder(broker.RoundRequest{
		Rules: rules.Instrument, Side: broker.SideSell, Type: broker.OrderTypeMarket,
		QtyCoin: filled.FilledQtyCoin, PriceQuote: filled.AvgFillPriceQuote,
	})
	if err != nil {
		return []check{{nameVI + ": đóng lệnh khớp ngoài ý muốn", false,
			"KHÔNG tính được cỡ lệnh đối ứng: " + err.Error() + " — ĐÓNG BẰNG TAY trên testnet"}}
	}
	closeID := fmt.Sprintf("bc-close-%d", time.Now().UnixMilli())
	closed, err := client.PlaceOrder(ctx, broker.PlaceOrderRequest{
		Market: market, Symbol: symbol, Side: broker.SideSell, Type: broker.OrderTypeMarket,
		ClientOrderID: closeID, QtyCoin: rounded.QtyCoin, ReduceOnly: market == broker.MarketFuturesUSDM,
	})
	if err != nil {
		return []check{{nameVI + ": đóng lệnh khớp ngoài ý muốn", false,
			"lệnh đối ứng HỎNG: " + err.Error() + " — ĐÓNG BẰNG TAY trên testnet"}}
	}
	return []check{{nameVI + ": đóng lệnh khớp ngoài ý muốn", false,
		fmt.Sprintf("đã gửi lệnh đối ứng %s (trạng thái %s) — 4.2 KHÔNG ✅ cho tới khi rõ vì sao lệnh khớp", closed.VenueOrderID, closed.Status)}}
}

func shortMarket(m broker.Market) string {
	if m == broker.MarketFuturesUSDM {
		return "fut"
	}
	return "spot"
}

func presentVI(b bool) string {
	if b {
		return "VẪN CÒN"
	}
	return "đã biến mất"
}
