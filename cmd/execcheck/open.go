package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
	"futures-arbitrage-scanner/internal/execution"
	"futures-arbitrage-scanner/internal/risk"
)

type openArgs struct {
	Symbol         string
	NotionalQuote  float64
	LegOrder       execution.LegOrder
	FailLeg2       string
	MarginFrac     float64
	MaxSlippageBps float64
	LegTimeout     time.Duration
	MaxWidenBps    float64
	JSON           bool
}

func runOpen(ctx context.Context, a openArgs) int {
	cl, err := dial()
	if err != nil {
		if errors.Is(err, broker.ErrNoCredentials) {
			fmt.Printf("BỎ QUA — %v\n", err)
			return exitNoCredentials
		}
		fmt.Printf("HỎNG — %v\n", err)
		return exitFailed
	}

	spotMkt, err := readMarket(ctx, cl.spot, a.Symbol)
	if err != nil {
		fmt.Printf("HỎNG — chân spot: %v\n", err)
		return exitFailed
	}
	perpMkt, err := readMarket(ctx, cl.perp, a.Symbol)
	if err != nil {
		fmt.Printf("HỎNG — chân perp: %v\n", err)
		return exitFailed
	}

	notionalQuote := a.NotionalQuote
	if notionalQuote <= 0 {
		notionalQuote = smallestWorkableNotionalQuote(spotMkt, perpMkt)
		fmt.Printf("cỡ không được chỉ định → lấy cỡ NHỎ NHẤT qua được cả hai mức tối thiểu: %.2f quote\n", notionalQuote)
	}

	// The maintenance bracket, read from the venue with the key PLAN's Q1 said
	// would be needed. An unverified schedule is REFUSED by risk.Evaluate, so
	// this is the difference between a position and a refusal.
	mb, err := cl.perp.FetchMaintenanceBracket(ctx, a.Symbol, notionalQuote)
	if err != nil {
		fmt.Printf("HỎNG — không đọc được biểu ký quỹ duy trì: %v\n", err)
		return exitFailed
	}
	bracket := risk.Bracket{
		Source:                "binance_futures_testnet",
		MaintenanceMarginFrac: mb.MaintMarginFrac,
		TierCeilingQuote:      mb.NotionalCapQuote,
		MaxLeverage:           mb.MaxLeverage,
		Verified:              true,
		NoteVI: fmt.Sprintf("đọc từ /fapi/v1/leverageBracket của testnet ngày %s: bậc %d, sàn %.0f → trần %.0f, tỷ lệ duy trì %.4f%%, đòn bẩy tối đa %.0fx",
			time.Now().Format("2006-01-02"), mb.Tier, mb.NotionalFloorQuote, mb.NotionalCapQuote, mb.MaintMarginFrac*100, mb.MaxLeverage),
	}
	fmt.Printf("ký quỹ duy trì: %s\n", bracket.NoteVI)

	intentID := newIntentID(a.Symbol)
	intent := execution.Intent{
		ID:             intentID,
		Symbol:         a.Symbol,
		SpotInstrument: spotMkt.Rules.Instrument,
		PerpInstrument: perpMkt.Rules.Instrument,
		SpotBook:       spotMkt.Book,
		PerpBook:       perpMkt.Book,
		SpotPriceQuote: spotMkt.Price,
		PerpPriceQuote: perpMkt.Price,
		NotionalQuote:  notionalQuote,
		// The "signal" here is the operator typing the command, so the cost the
		// decision was made at IS the cost the book prices right now. Setting
		// it from the current books means the widening check compares the book
		// against itself and can only pass — which is honest for a hand-typed
		// order and would be a lie for a real signal.
		SignalEntryCostPct: 0,
		PerpMarginFrac:     a.MarginFrac,
		PerpBracket:        bracket,
	}
	// A hand-typed order has no earlier cost to widen from, so the tolerance is
	// the only thing bounding it — and it is stated, not hidden.
	cfg := execution.DefaultConfig()
	cfg.MaxEntryCostWidenBps = math.Inf(1)
	cfg.MaxSlippageBps = a.MaxSlippageBps
	cfg.LegTimeout = a.LegTimeout
	cfg.LegOrder = a.LegOrder

	var spotB, perpB broker.Broker = cl.spot, cl.perp
	if a.FailLeg2 != "" {
		second := &faultInjector{Broker: perpB, mode: a.FailLeg2, symbol: a.Symbol}
		if a.LegOrder == execution.LegOrderParallel {
			fmt.Println("CHÚ Ý: -fail-leg2 với leg-order song song làm hỏng chân PERP, chân nào 'thứ hai' là không xác định")
		}
		perpB = second
		fmt.Printf("BƠM LỖI: chân thứ hai sẽ bị sàn từ chối theo kiểu %q\n", a.FailLeg2)
	}

	rec := execution.NewMemoryRecorder()
	tr, err := execution.NewOpener(spotB, perpB, cfg, rec)
	if err != nil {
		fmt.Printf("HỎNG — %v\n", err)
		return exitFailed
	}

	fmt.Printf("\nMỞ %s · %s · %.2f quote mỗi chân · thứ tự %s · trần trượt %.1f bps · hạn mỗi chân %s\n",
		intentID, a.Symbol, notionalQuote, cfg.LegOrder, cfg.MaxSlippageBps, cfg.LegTimeout)

	startedAt := time.Now()
	res, openErr := tr.Open(ctx, intent)
	elapsed := time.Since(startedAt)

	st := state{
		IntentID: intentID, Symbol: a.Symbol, OpenedAtMs: startedAt.UnixMilli(),
		LegOrder: string(cfg.LegOrder), NotionalQuote: notionalQuote, TargetQtyCoin: res.TargetQtyCoin,
		SpotClientOrderID: res.Spot.ClientOrderID, PerpClientOrderID: res.Perp.ClientOrderID,
		SpotFilledQtyCoin: res.Spot.FilledQtyCoin, PerpFilledQtyCoin: res.Perp.FilledQtyCoin,
		SpotAvgPriceQuote: res.Spot.AvgFillPriceQuote, PerpAvgPriceQuote: res.Perp.AvgFillPriceQuote,
		SpotRefMidQuote: spotMkt.Book.MidPriceQuote, PerpRefMidQuote: perpMkt.Book.MidPriceQuote,
		SpotBestAskQuote: spotMkt.Book.BestAskQuote, PerpBestBidQuote: perpMkt.Book.BestBidQuote,
		BookSampledAtMs: spotMkt.Book.SampledAtMs, UnhedgedWindowMs: res.UnhedgedWindow.Milliseconds(),
		ReducedToMatch: res.ReducedToMatch, Outcome: string(res.Outcome),
	}
	if mp, err := cl.perp.MarkPrice(ctx, broker.MarketFuturesUSDM, a.Symbol); err == nil {
		st.NextFundingTimeMs = mp.NextFundingTimeMs
	}
	if err := saveState(st); err != nil {
		fmt.Printf("CẢNH BÁO — không ghi được file ý định: %v\n", err)
	}

	printOpen(res, openErr, elapsed, spotMkt, perpMkt, st)
	if a.JSON {
		blob, _ := json.MarshalIndent(st, "", "  ")
		fmt.Printf("\n%s\n", blob)
	}
	fmt.Printf("\nsự kiện:%s\n", rec.Dump())

	if openErr != nil && res.Outcome != execution.OutcomeBothOpen {
		// A refusal or a completed unwind is a SUCCESSFUL run of the machine —
		// the invariant held — so it is reported and not called a crash. Only
		// an unwind that could not be completed is a failure of this command.
		if errors.Is(openErr, execution.ErrUnwindIncomplete) || errors.Is(openErr, execution.ErrFlatEvidenceConflict) {
			return exitFailed
		}
		return exitOK
	}
	if openErr != nil {
		return exitFailed
	}
	return exitOK
}

func printOpen(res execution.Result, openErr error, elapsed time.Duration, spotMkt, perpMkt market, st state) {
	fmt.Printf("\nKẾT CỤC: %s   (mất %s)\n", res.Outcome, elapsed.Round(time.Millisecond))
	if res.ReducedToMatch {
		fmt.Println("  cặp đã được THU NHỎ về cỡ chân ngắn hơn thay vì gỡ phẳng")
	}
	fmt.Printf("  cỡ đích        %.8f coin\n", res.TargetQtyCoin)
	fmt.Printf("  chân spot      khớp %.8f coin @ %.4f  (id %s, %s)\n",
		res.Spot.FilledQtyCoin, res.Spot.AvgFillPriceQuote, res.Spot.ClientOrderID, res.Spot.Status)
	fmt.Printf("  chân perp      khớp %.8f coin @ %.4f  (id %s, %s)\n",
		res.Perp.FilledQtyCoin, res.Perp.AvgFillPriceQuote, res.Perp.VenueOrderID, res.Perp.Status)
	fmt.Printf("  dư             %.10f coin (dung sai %.10f)\n", res.ResidualQtyCoin,
		math.Max(spotMkt.Rules.StepSizeCoin, perpMkt.Rules.StepSizeCoin))
	fmt.Printf("  CỬA SỔ TRẦN    %d ms  (từ chân khớp trước tới chân khớp sau)\n", res.UnhedgedWindow.Milliseconds())
	if res.UnwindDuration > 0 {
		fmt.Printf("  GỠ VỊ THẾ      %s\n", res.UnwindDuration)
	}
	fmt.Printf("  sổ cũ          %d ms lúc quyết định\n", res.BookAgeMs)
	if s := slippageBps(res.Spot.AvgFillPriceQuote, st.SpotBestAskQuote, true); !math.IsNaN(s) {
		fmt.Printf("  TRƯỢT spot     %+.2f bps so với giá chào bán tốt nhất %.4f lúc chụp sổ\n", s, st.SpotBestAskQuote)
	}
	if s := slippageBps(res.Perp.AvgFillPriceQuote, st.PerpBestBidQuote, false); !math.IsNaN(s) {
		fmt.Printf("  TRƯỢT perp     %+.2f bps so với giá chào mua tốt nhất %.4f lúc chụp sổ\n", s, st.PerpBestBidQuote)
	}
	if res.SpotFlatEvidenceVI != "" {
		fmt.Printf("  bằng chứng phẳng: %s\n", res.SpotFlatEvidenceVI)
	}
	if openErr != nil {
		fmt.Printf("  LÝ DO          %s\n", openErr.Error())
	}
	fmt.Printf("\nfile ý định: %s  (CACHE — dùng -status để hỏi sàn)\n", statePath(st.IntentID))
}

// slippageBps is how far the fill landed from the touch, in basis points,
// POSITIVE when it was worse than the touch.
func slippageBps(fillQuote, touchQuote float64, buy bool) float64 {
	if fillQuote <= 0 || touchQuote <= 0 {
		return math.NaN()
	}
	d := (fillQuote - touchQuote) / touchQuote * 10_000
	if !buy {
		d = -d
	}
	return d
}

// smallestWorkableNotionalQuote is the smallest size that clears BOTH venues'
// minimums with one step of headroom.
//
// It is computed from the venues' own published rules rather than typed in,
// because the two markets' minimums differ by a factor of ten (spot 5, futures
// 50 on BTCUSDT, measured 2026-09-13) and the binding one is not the same on
// every symbol. One step of headroom is added on purpose: rounding the quantity
// DOWN onto a grid can otherwise drop the notional back under the minimum that
// was just cleared.
func smallestWorkableNotionalQuote(spotMkt, perpMkt market) float64 {
	need := math.Max(spotMkt.Rules.MinNotionalQuote, perpMkt.Rules.MinNotionalQuote)
	step := math.Max(spotMkt.Rules.StepSizeCoin, perpMkt.Rules.StepSizeCoin)
	price := math.Max(spotMkt.Price, perpMkt.Price)
	minQty := math.Max(spotMkt.Rules.MinQtyCoin, perpMkt.Rules.MinQtyCoin)
	fromQty := minQty * price
	return math.Max(need, fromQty) + 2*step*price
}

// newIntentID is a readable, unique id. Every ClientOrderID this run uses is
// DERIVED from it (execution.LegClientOrderID), so this string is the only
// thing a later -status or -close needs in order to ask the venue about orders
// it has no other record of.
func newIntentID(symbol string) string {
	return fmt.Sprintf("x%s-%s", strings.ToLower(symbol), time.Now().UTC().Format("20060102-150405"))
}

// faultInjector makes the venue refuse ONE leg, on purpose.
//
// It is the 4.4b acceptance's fault injection and it lives in the command
// rather than in internal/execution, because it is a way of driving a REAL
// venue into a refusal rather than a behaviour of the code under test. The
// refusal is the venue's own: the order is really sent, and really rejected,
// with the venue's own error code — which is the whole point, since a rejection
// simulated in Go would prove nothing about how long the unwind takes when a
// real exchange says no.
type faultInjector struct {
	broker.Broker
	mode   string
	symbol string
}

func (f *faultInjector) PlaceOrder(ctx context.Context, req broker.PlaceOrderRequest) (broker.Order, error) {
	// Only the OPENING order is sabotaged. A closing MARKET order must work,
	// or the measurement becomes "how long does an unwind take when the unwind
	// is also broken", which is a different and useless number.
	if req.Type != broker.OrderTypeLimitGTC {
		return f.Broker.PlaceOrder(ctx, req)
	}
	switch f.mode {
	case "min-notional":
		// A quantity the venue's MIN_NOTIONAL filter refuses. Sent for real.
		req.QtyCoin = req.QtyCoin / 1000
	case "bad-symbol":
		req.Symbol = f.symbol + "NOSUCH"
	default:
		fmt.Fprintf(os.Stderr, "kiểu bơm lỗi %q không có; gửi lệnh bình thường\n", f.mode)
	}
	return f.Broker.PlaceOrder(ctx, req)
}

var _ = binancebroker.MarketRules{}
