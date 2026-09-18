package crossperp

import (
	"fmt"
	"math"
	"strings"

	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/strategy"
)

// Everything that happens before an order exists. Each refusal here costs
// nothing: no order has been sent and the intent can be re-made against a
// better book.

// gridEpsilon is the step-count tolerance internal/broker and internal/execution
// use, for the same float64 reason.
const gridEpsilon = 1e-9

// maxBookAheadMs is how far in the future a book's stamp may sit before it is a
// clock that stepped: one second of scheduling slack, nothing more.
const maxBookAheadMs = 1_000

// openPlan is a validated, ready-to-send pair of orders.
type openPlan struct {
	QtyCoin          float64
	CommonStepCoin   float64
	ToleranceQtyCoin float64
	RefMidQuote      float64

	LongOrder  broker.RoundedOrder
	ShortOrder broker.RoundedOrder

	LongFill  strategy.FillEstimate
	ShortFill strategy.FillEstimate

	EntryCostPct float64
	WidenBps     float64
	BookAgeMs    int64
}

// CommonStepCoin is the design's Step_common = max(Step_A, Step_B), and it is
// REFUSED when the coarser step is not a whole number of the finer: a quantity
// on the coarser grid would then not be on the finer one, and one venue would
// round the order the other did not.
func CommonStepCoin(stepACoin, stepBCoin float64) (float64, error) {
	if !positiveFinite(stepACoin) || !positiveFinite(stepBCoin) {
		return 0, fmt.Errorf("%w: bước khối lượng %v và %v — một sàn không công bố bước", ErrIncommensurateSteps, stepACoin, stepBCoin)
	}
	coarse, fine := math.Max(stepACoin, stepBCoin), math.Min(stepACoin, stepBCoin)
	ratio := coarse / fine
	if math.Abs(ratio-math.Round(ratio)) > 1e-6 {
		return 0, fmt.Errorf("%w: %v không phải bội số nguyên của %v", ErrIncommensurateSteps, coarse, fine)
	}
	return coarse, nil
}

// planOpen sizes both legs on the coarser grid, prices both marketable limits,
// and checks every precondition that needs no venue call.
func planOpen(intent Intent, cfg Config) (openPlan, error) {
	var out openPlan
	if err := validateIntent(intent); err != nil {
		return out, err
	}
	long, short := intent.Long, intent.Short

	nowMs := cfg.Now().UnixMilli()
	out.BookAgeMs = max(nowMs-long.Book.SampledAtMs, nowMs-short.Book.SampledAtMs)
	if out.BookAgeMs > cfg.MaxBookAge.Milliseconds() {
		return out, fmt.Errorf("%w: sổ cũ %d ms, quá hạn %d ms", ErrBookStale, out.BookAgeMs, cfg.MaxBookAge.Milliseconds())
	}
	// SampledAtMs is OUR clock's stamp (depth.Summary), so a book from the
	// future is a clock that stepped, not a fresh book (review 4.5k, m11).
	if aheadMs := max(long.Book.SampledAtMs, short.Book.SampledAtMs) - nowMs; aheadMs > maxBookAheadMs {
		return out, fmt.Errorf("%w: sổ đóng dấu %d ms TRONG TƯƠNG LAI — đồng hồ đã nhảy, không phải sổ mới", ErrBookStale, aheadMs)
	}

	common, err := CommonStepCoin(long.Rules.StepSizeCoin, short.Rules.StepSizeCoin)
	if err != nil {
		return out, err
	}
	out.CommonStepCoin, out.ToleranceQtyCoin = common, common

	// P_mid is the mean of the two books' mids: the two perps trade the same
	// coin and neither venue's mid is more the price than the other's.
	out.RefMidQuote = (long.Book.MidPriceQuote + short.Book.MidPriceQuote) / 2
	out.QtyCoin = broker.FloorToStep(intent.NotionalQuote/out.RefMidQuote, common)
	if out.QtyCoin <= 0 {
		return out, fmt.Errorf("%w: notional %v ở giá %v dưới một bước chung %v", ErrSizeBelowMinimum, intent.NotionalQuote, out.RefMidQuote, common)
	}
	minNotionalQuote := math.Max(long.Rules.MinNotionalQuote, short.Rules.MinNotionalQuote)
	if notional := out.QtyCoin * out.RefMidQuote; notional < minNotionalQuote {
		return out, fmt.Errorf("%w: %v coin × %v = %.8g, dưới mức tối thiểu lớn hơn của hai sàn %v — không nâng cỡ",
			ErrSizeBelowMinimum, out.QtyCoin, out.RefMidQuote, notional, minNotionalQuote)
	}

	// The book, at the size that will really be sent.
	out.LongFill = strategy.EstimateFill(long.Book, strategy.SideBuy, out.QtyCoin*long.Book.MidPriceQuote)
	out.ShortFill = strategy.EstimateFill(short.Book, strategy.SideSell, out.QtyCoin*short.Book.MidPriceQuote)
	if !out.LongFill.Fillable {
		return out, fmt.Errorf("%w: chân long (%s) không định giá được: %s", ErrBookWidened, long.Venue.Name, out.LongFill.ReasonVI)
	}
	if !out.ShortFill.Fillable {
		return out, fmt.Errorf("%w: chân short (%s) không định giá được: %s", ErrBookWidened, short.Venue.Name, out.ShortFill.ReasonVI)
	}
	out.EntryCostPct = out.LongFill.SlippagePct + out.ShortFill.SlippagePct
	out.WidenBps = (out.EntryCostPct - intent.SignalEntryCostPct) * 100
	if out.WidenBps > cfg.MaxEntryCostWidenBps {
		return out, fmt.Errorf("%w: chi phí vào giờ %.4f%% so với %.4f%% lúc sinh tín hiệu, rộng thêm %.2f bps > %.2f bps",
			ErrBookWidened, out.EntryCostPct, intent.SignalEntryCostPct, out.WidenBps, cfg.MaxEntryCostWidenBps)
	}

	longCap, err := marketableLimitQuote(long.Book.BestAskQuote, broker.SideBuy, cfg.MaxSlippageBps, "chân long (mua, từ giá chào bán tốt nhất)")
	if err != nil {
		return out, err
	}
	shortFloor, err := marketableLimitQuote(short.Book.BestBidQuote, broker.SideSell, cfg.MaxSlippageBps, "chân short (bán, từ giá chào mua tốt nhất)")
	if err != nil {
		return out, err
	}

	for _, leg := range []struct {
		name  string
		spec  LegSpec
		side  broker.Side
		price float64
		into  *broker.RoundedOrder
	}{
		{"long", long, broker.SideBuy, longCap, &out.LongOrder},
		{"short", short, broker.SideSell, shortFloor, &out.ShortOrder},
	} {
		rounded, err := broker.RoundOrder(broker.RoundRequest{
			Rules: leg.spec.Rules, Side: leg.side, Type: broker.OrderTypeLimitGTC,
			QtyCoin: out.QtyCoin, PriceQuote: leg.price, Price: broker.PriceRoundPassive,
		})
		if err != nil {
			return out, fmt.Errorf("%w: chân %s (%s): %s", ErrSizeBelowMinimum, leg.name, leg.spec.Venue.Name, err.Error())
		}
		// Q sits on the common grid, which is a whole multiple of this venue's
		// step, so the venue's own rounding must leave it exactly as it is.
		if math.Abs(rounded.QtyCoin-out.QtyCoin) > gridEpsilon*common {
			return out, fmt.Errorf("%w: %s làm tròn %v thành %v", ErrIncommensurateSteps, leg.spec.Venue.Name, out.QtyCoin, rounded.QtyCoin)
		}
		*leg.into = rounded
	}
	return out, nil
}

// marketableLimitQuote prices one leg's cap from the touch (execution's rule).
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

// validateIntent refuses anything that is not one coin, one quote, two venues.
func validateIntent(i Intent) error {
	switch {
	case i.ID == "":
		return fmt.Errorf("%w: không có id ý định — không suy ra được ClientOrderID", ErrIntentInvalid)
	case i.Symbol == "":
		return fmt.Errorf("%w: không có symbol", ErrIntentInvalid)
	case !positiveFinite(i.NotionalQuote):
		return fmt.Errorf("%w: notional %v không phải số dương hữu hạn", ErrIntentInvalid, i.NotionalQuote)
	case math.IsNaN(i.SignalEntryCostPct) || math.IsInf(i.SignalEntryCostPct, 0):
		return fmt.Errorf("%w: chi phí vào lúc sinh tín hiệu %v không hữu hạn", ErrIntentInvalid, i.SignalEntryCostPct)
	case i.Long.Venue.Name == "" || i.Short.Venue.Name == "":
		return fmt.Errorf("%w: một chân không có tên sàn", ErrIntentInvalid)
	case i.Long.Venue.Name == i.Short.Venue.Name:
		// One-way mode would net the two legs into nothing on one account.
		return fmt.Errorf("%w: hai chân cùng sàn %q — tài khoản một chiều sẽ bù trừ chúng", ErrIntentInvalid, i.Long.Venue.Name)
	case i.Long.Venue.Broker == nil || i.Short.Venue.Broker == nil:
		return fmt.Errorf("%w: một chân không có broker", ErrIntentInvalid)
	}
	for _, leg := range []struct {
		name string
		spec LegSpec
	}{{"long", i.Long}, {"short", i.Short}} {
		r, b := leg.spec.Rules, leg.spec.Book
		switch {
		case r.Symbol != i.Symbol:
			return fmt.Errorf("%w: quy tắc chân %s là của %q, ý định là %q", ErrIntentInvalid, leg.name, r.Symbol, i.Symbol)
		case r.MarketType != "" && r.MarketType != "perp":
			return fmt.Errorf("%w: chân %s là thị trường %q, không phải perp", ErrIntentInvalid, leg.name, r.MarketType)
		case r.Status != "" && r.Status != "trading":
			return fmt.Errorf("%w: sàn %s báo trạng thái %q", ErrIntentInvalid, leg.spec.Venue.Name, r.Status)
		case r.IsContract && r.ContractSizeCoin != 1:
			// broker.Broker speaks coin; a contract of another size is a
			// conversion this engine has not been given.
			return fmt.Errorf("%w: chân %s tính bằng hợp đồng cỡ %v coin — chưa hỗ trợ", ErrIntentInvalid, leg.name, r.ContractSizeCoin)
		case b.Symbol != "" && b.Symbol != i.Symbol:
			return fmt.Errorf("%w: sổ chân %s là của %q", ErrIntentInvalid, leg.name, b.Symbol)
		case !b.OK():
			return fmt.Errorf("%w: sổ chân %s không dùng được: %s", ErrIntentInvalid, leg.name, b.ErrVI)
		}
	}
	// Same coin, same quote, as each VENUE declared it (step 2.4): no quote
	// bridging on this engine — a USD perp against a USDT perp is open in
	// USDT/USD, which nothing here deducts.
	if !sameAsset(i.Long.Rules.BaseAsset, i.Short.Rules.BaseAsset) || !sameAsset(i.Long.Rules.QuoteAsset, i.Short.Rules.QuoteAsset) {
		return fmt.Errorf("%w: hai chân không cùng tài sản sàn khai báo: %s/%s và %s/%s",
			ErrIntentInvalid, i.Long.Rules.BaseAsset, i.Long.Rules.QuoteAsset, i.Short.Rules.BaseAsset, i.Short.Rules.QuoteAsset)
	}
	return nil
}

func sameAsset(a, b string) bool { return a != "" && strings.EqualFold(a, b) }

func positiveFinite(v float64) bool { return v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v) }
