package execution

import (
	"fmt"
	"math"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/instruments"
	"futures-arbitrage-scanner/internal/risk"
	"futures-arbitrage-scanner/internal/strategy"
)

// Everything that happens BEFORE an order exists.
//
// Each refusal below costs nothing at all — no order has been sent, no capital
// is committed, and the intent can be re-made on the next tick against a better
// book. That asymmetry is why this stage is deliberately strict: the cheapest
// possible moment to decide against a trade is before it starts.

// entryPlan is a validated, ready-to-send pair of orders.
type entryPlan struct {
	QtyCoin float64

	// SpotOrder and PerpOrder carry the rounded quantity AND the marketable
	// limit price for each leg, already on that venue's grids.
	SpotOrder broker.RoundedOrder
	PerpOrder broker.RoundedOrder

	// ResidualToleranceQtyCoin is the coarser of the two step sizes — the
	// closest the two legs can possibly be, and therefore the invariant's
	// tolerance. It is computed here, once, so no later comparison invents its
	// own.
	ResidualToleranceQtyCoin float64

	// EntryCostPct is the ONE-WAY entry cost the current books price, in
	// percent of notional, GROSS of commission. WidenBps is how much worse
	// that is than the cost the signal was made at.
	EntryCostPct float64
	WidenBps     float64

	SpotFill strategy.FillEstimate
	PerpFill strategy.FillEstimate

	BookAgeMs int64
}

// planEntry sizes both legs, prices them, and checks every precondition.
//
// The order of operations is load-bearing and is the one PLAN's step 2.3 fixed:
// size on the COARSER grid first, then round each leg on its OWN grid, then
// take the SMALLER result as the common quantity and round that again on both.
// Rounding each leg independently from the target is exactly how a
// "delta-neutral" position starts life unbalanced, and the second rounding can
// move a number again — so the invariant is checked AFTER it, never before.
func planEntry(intent Intent, cfg Config) (entryPlan, error) {
	var out entryPlan
	if err := validateIntent(intent); err != nil {
		return out, err
	}

	// The perp leg's liquidation risk, before its size is even known: an
	// unverified maintenance bracket is refused rather than assumed zero,
	// exactly as internal/strategy refuses an unverified fee schedule. Priced
	// at the intended notional and entry price, which is the position this
	// intent would create.
	state := risk.Evaluate(risk.Position{
		NotionalQuote:   intent.NotionalQuote,
		EntryPriceQuote: intent.PerpPriceQuote,
		MarginFrac:      intent.PerpMarginFrac,
	}, intent.PerpBracket, intent.PerpPriceQuote)
	if !state.OK {
		return out, fmt.Errorf("%w: %s", ErrMarginUnverified, state.ReasonVI)
	}

	// The books have to be recent enough to be evidence. The OLDER of the two
	// decides: a fresh spot book does not make a ten-minute-old perp book
	// current, and the pair is only as good as its worse half.
	nowMs := cfg.Now().UnixMilli()
	out.BookAgeMs = maxInt64(nowMs-intent.SpotBook.SampledAtMs, nowMs-intent.PerpBook.SampledAtMs)
	if cfg.MaxBookAge > 0 && out.BookAgeMs > cfg.MaxBookAge.Milliseconds() {
		return out, fmt.Errorf("%w: sổ cũ %d ms, quá hạn %d ms", ErrBookStale, out.BookAgeMs, cfg.MaxBookAge.Milliseconds())
	}

	// Step 2.3's rules, called rather than re-grown.
	size, err := instruments.SizeDeltaNeutral(intent.SpotInstrument, intent.PerpInstrument, instruments.SizingRequest{
		SpotPriceQuote: intent.SpotPriceQuote,
		PerpPriceQuote: intent.PerpPriceQuote,
		NotionalQuote:  intent.NotionalQuote,
	})
	if err != nil {
		return out, fmt.Errorf("%w: %s", ErrSizeBelowMinimum, err.Error())
	}

	// Price the two fills at the size that will actually be sent, and derive
	// the marketable limit from what the book says this size reaches.
	out.SpotFill = strategy.EstimateFill(intent.SpotBook, strategy.SideBuy, size.QtyCoin*intent.SpotPriceQuote)
	out.PerpFill = strategy.EstimateFill(intent.PerpBook, strategy.SideSell, size.QtyCoin*intent.PerpPriceQuote)
	if !out.SpotFill.Fillable {
		return out, fmt.Errorf("%w: chân spot không định giá được: %s", ErrBookWidened, out.SpotFill.ReasonVI)
	}
	if !out.PerpFill.Fillable {
		return out, fmt.Errorf("%w: chân perp không định giá được: %s", ErrBookWidened, out.PerpFill.ReasonVI)
	}

	// One-way entry cost: buy the spot leg, sell the perp leg. GROSS — this is
	// slippage against the mid and nothing else; commission belongs to
	// strategy's figures, not to this one (CLAUDE.md rule 2).
	out.EntryCostPct = out.SpotFill.SlippagePct + out.PerpFill.SlippagePct
	out.WidenBps = (out.EntryCostPct - intent.SignalEntryCostPct) * 100
	if out.WidenBps > cfg.MaxEntryCostWidenBps {
		return out, fmt.Errorf(
			"%w: chi phí vào giờ là %.4f%% so với %.4f%% lúc sinh tín hiệu, rộng thêm %.2f bps > %.2f bps cho phép",
			ErrBookWidened, out.EntryCostPct, intent.SignalEntryCostPct, out.WidenBps, cfg.MaxEntryCostWidenBps)
	}

	// The marketable limit, from the BOOK'S OWN BEST PRICE and a stated
	// tolerance. See Config.MaxSlippageBps for why it is no longer derived
	// from out.SpotFill.ReachedOffsetPct: that number's error points the safe
	// way for a cost and the unsafe way for a cap.
	spotCap, err := marketableLimitQuote(intent.SpotBook.BestAskQuote, broker.SideBuy, cfg.MaxSlippageBps, "chân spot (mua, đo từ giá chào bán tốt nhất)")
	if err != nil {
		return out, err
	}
	perpFloor, err := marketableLimitQuote(intent.PerpBook.BestBidQuote, broker.SideSell, cfg.MaxSlippageBps, "chân perp (bán, đo từ giá chào mua tốt nhất)")
	if err != nil {
		return out, err
	}

	// Round each leg on its OWN grid.
	spotOrder, err := broker.RoundOrder(broker.RoundRequest{
		Rules: intent.SpotInstrument, Side: broker.SideBuy, Type: broker.OrderTypeLimitGTC,
		QtyCoin: size.QtyCoin, PriceQuote: spotCap, Price: broker.PriceRoundPassive,
	})
	if err != nil {
		return out, fmt.Errorf("%w: chân spot: %s", ErrSizeBelowMinimum, err.Error())
	}
	perpOrder, err := broker.RoundOrder(broker.RoundRequest{
		Rules: intent.PerpInstrument, Side: broker.SideSell, Type: broker.OrderTypeLimitGTC,
		QtyCoin: size.QtyCoin, PriceQuote: perpFloor, Price: broker.PriceRoundPassive,
	})
	if err != nil {
		return out, fmt.Errorf("%w: chân perp: %s", ErrSizeBelowMinimum, err.Error())
	}

	// The common quantity is the SMALLER of the two, re-rounded on both grids.
	// Taking the smaller is what keeps the pair hedged; re-rounding is what
	// keeps each leg legal, and it can move the number a second time.
	common := math.Min(spotOrder.QtyCoin, perpOrder.QtyCoin)
	if common < spotOrder.QtyCoin || common < perpOrder.QtyCoin {
		if spotOrder, err = broker.RoundOrder(broker.RoundRequest{
			Rules: intent.SpotInstrument, Side: broker.SideBuy, Type: broker.OrderTypeLimitGTC,
			QtyCoin: common, PriceQuote: spotCap, Price: broker.PriceRoundPassive,
		}); err != nil {
			return out, fmt.Errorf("%w: chân spot sau khi hạ về cỡ chung: %s", ErrSizeBelowMinimum, err.Error())
		}
		if perpOrder, err = broker.RoundOrder(broker.RoundRequest{
			Rules: intent.PerpInstrument, Side: broker.SideSell, Type: broker.OrderTypeLimitGTC,
			QtyCoin: common, PriceQuote: perpFloor, Price: broker.PriceRoundPassive,
		}); err != nil {
			return out, fmt.Errorf("%w: chân perp sau khi hạ về cỡ chung: %s", ErrSizeBelowMinimum, err.Error())
		}
	}

	out.SpotOrder, out.PerpOrder = spotOrder, perpOrder
	out.QtyCoin = math.Min(spotOrder.QtyCoin, perpOrder.QtyCoin)
	out.ResidualToleranceQtyCoin = math.Max(intent.SpotInstrument.StepSizeCoin, intent.PerpInstrument.StepSizeCoin)

	// The invariant, checked here — where the only cost of failing it is not
	// trading — rather than discovered after two orders are live. Same
	// function Open and the property test use, so there is one invariant and
	// not three.
	if err := out.invariant(intent).check(spotOrder.QtyCoin, perpOrder.QtyCoin); err != nil {
		return out, fmt.Errorf("%w: %s — mở thế này là không phòng hộ", ErrSizeBelowMinimum, err.Error())
	}
	return out, nil
}

// invariant is the pair test this plan will be held to, built once from the
// venues' own rules.
func (p entryPlan) invariant(intent Intent) pairInvariant {
	return pairInvariant{
		ToleranceQtyCoin:     p.ResidualToleranceQtyCoin,
		SpotPriceQuote:       intent.SpotPriceQuote,
		PerpPriceQuote:       intent.PerpPriceQuote,
		SpotMinNotionalQuote: intent.SpotInstrument.MinNotionalQuote,
		PerpMinNotionalQuote: intent.PerpInstrument.MinNotionalQuote,
	}
}

// gridEpsilon is the same step-count tolerance internal/instruments and
// internal/broker use, for the same float64 reason: 128 x 0.0001 is not exactly
// 0.0128, and a comparison that does not allow for it rejects valid sizes.
const gridEpsilon = 1e-9

// marketableLimitQuote prices one leg's cap from the touch.
//
// A BUY may pay up to bestAsk x (1 + bps/10000) and a SELL may accept down to
// bestBid x (1 - bps/10000). Rounding happens afterwards, PASSIVELY, which
// tightens the cap by at most one tick — a buy cap rounds DOWN and a sell floor
// rounds UP — so the order can never end up authorised for a worse price than
// the tolerance states. That is the opposite of the old derivation, which added
// a tick of slack to make sure the order still filled.
func marketableLimitQuote(bestQuote float64, side broker.Side, maxSlippageBps float64, whatVI string) (float64, error) {
	if !positiveFinite(bestQuote) {
		return 0, fmt.Errorf("%w: %s — giá tốt nhất là %v", ErrNoBestPrice, whatVI, bestQuote)
	}
	if maxSlippageBps < 0 || math.IsNaN(maxSlippageBps) || math.IsInf(maxSlippageBps, 0) {
		return 0, fmt.Errorf("%w: MaxSlippageBps = %v", ErrIntentInvalid, maxSlippageBps)
	}
	if side == broker.SideBuy {
		return bestQuote * (1 + maxSlippageBps/10_000), nil
	}
	limit := bestQuote * (1 - maxSlippageBps/10_000)
	if limit <= 0 {
		return 0, fmt.Errorf("%w: %s — sàn giá tính ra %v", ErrIntentInvalid, whatVI, limit)
	}
	return limit, nil
}

// validateIntent refuses anything that does not describe a position, before a
// single venue rule is consulted.
func validateIntent(i Intent) error {
	switch {
	case i.ID == "":
		return fmt.Errorf("%w: không có id ý định — không suy ra được ClientOrderID, nên một tiến trình lên lại sẽ không tìm được lệnh của chính nó", ErrIntentInvalid)
	case i.Symbol == "":
		return fmt.Errorf("%w: không có symbol", ErrIntentInvalid)
	case !positiveFinite(i.NotionalQuote):
		return fmt.Errorf("%w: notional %v không phải số dương hữu hạn", ErrIntentInvalid, i.NotionalQuote)
	case !positiveFinite(i.SpotPriceQuote):
		return fmt.Errorf("%w: giá spot %v không phải số dương hữu hạn", ErrIntentInvalid, i.SpotPriceQuote)
	case !positiveFinite(i.PerpPriceQuote):
		return fmt.Errorf("%w: giá perp %v không phải số dương hữu hạn", ErrIntentInvalid, i.PerpPriceQuote)
	case !positiveFinite(i.PerpMarginFrac):
		// risk.Evaluate would refuse this too, but saying it here names the
		// field the caller left out instead of describing a liquidation price
		// that could not be derived.
		return fmt.Errorf("%w: chưa khai báo tỷ lệ ký quỹ cho chân perp", ErrIntentInvalid)
	case i.SpotInstrument.Status != "" && i.SpotInstrument.Status != "trading":
		return fmt.Errorf("%w: sàn spot báo trạng thái %q cho %s", ErrIntentInvalid, i.SpotInstrument.Status, i.Symbol)
	case i.PerpInstrument.Status != "" && i.PerpInstrument.Status != "trading":
		return fmt.Errorf("%w: sàn perp báo trạng thái %q cho %s", ErrIntentInvalid, i.PerpInstrument.Status, i.Symbol)
	case i.SpotInstrument.Symbol != i.PerpInstrument.Symbol:
		// Not pedantry: hedging BTCUSDT with an ETHUSDT perp is delta-neutral
		// in neither coin, and every quantity check below would still pass.
		return fmt.Errorf("%w: hai chân là hai thị trường khác nhau, %q và %q",
			ErrIntentInvalid, i.SpotInstrument.Symbol, i.PerpInstrument.Symbol)
	}
	return nil
}

func positiveFinite(v float64) bool { return v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v) }

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
