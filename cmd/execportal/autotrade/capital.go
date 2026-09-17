package autotrade

import (
	"fmt"
	"math"
)

// Buffered-slot capital allocation and the periodic rebalance
// (PLAN "Công cụ vận hành 4.5g").
//
// The bot ran on a notional a person typed once. This sizes every slot from
// what the account actually holds, and re-sizes it on a schedule so profit is
// reinvested without anyone touching the form. Two things it never does: it
// never closes or shrinks a position that is already open, and it never
// produces a size one of the two wallets cannot actually fund.

// Account is the two testnet wallets' QUOTE-asset equity, read from the venues
// (rule 7) and never from local bookkeeping.
//
// They are reported APART and the reader never simply adds them, because on
// this testnet they are two separate registrations — a futures key on the spot
// host is refused -2015 (PLAN 4.1) — so nothing here can move a quote from one
// to the other. A plan that spends the sum would size a spot leg the spot
// wallet cannot buy, or a perp leg the futures wallet cannot margin.
type Account struct {
	QuoteAsset string

	// SpotQuoteTotal is free + locked quote on the SPOT account. It does NOT
	// include coins: what the bot's own positions hold is added by the engine
	// from its own positions, and a coin balance belonging to nobody's position
	// is deliberately left out — the bot must not size itself on capital it
	// does not control.
	SpotQuoteTotal float64

	// FuturesQuoteTotal is the futures WALLET balance (Binance's `balance`, not
	// `availableBalance`), which already includes the margin posted against
	// open positions. That makes it the futures side's whole equity, which is
	// what a slot's share must be measured against.
	FuturesQuoteTotal float64

	// UnifiedWallet is true when spot and futures are ONE wallet — Bybit's
	// Unified Trading Account (PLAN 4.5j). Both markets then report the SAME
	// quote balance, so the reader carries it ONCE, in SpotQuoteTotal, and
	// FuturesQuoteTotal must be 0: the Binance arithmetic above adds the two,
	// which here would count every quote twice and size every slot on money the
	// account does not have.
	UnifiedWallet bool

	ReadAtMs int64
}

// The shipped rebalance values, pinned by
// TestDefaults_AreTheAuditedSafetyThresholds.
const (
	// DefaultAutoRebalance ships ON: a bot whose size never follows its equity
	// either compounds nothing or, after a drawdown, keeps sizing on money it
	// no longer has.
	DefaultAutoRebalance = true

	// DefaultMarginBufferPct is the share of equity held BACK from the slots,
	// as a fraction. The perp leg is the one that can be liquidated, and its
	// margin is what the buffer protects: at 0.30 a slot's perp leg can lose
	// most of its posted margin before the wallet is the binding constraint.
	DefaultMarginBufferPct = 0.30
	// The buffer is refused outside this band. Under 0.10 there is no room for
	// an adverse move on the perp leg before the wallet is dry; over 0.50 more
	// than half the account is idle and the slots are not the size the operator
	// thinks they are.
	MinMarginBufferPct = 0.10
	MaxMarginBufferPct = 0.50

	// DefaultRebalanceIntervalHours is a week. Short enough to follow a real
	// change in equity, long enough that a size is stable across the hold the
	// entry was priced over (ProjectionHoldDays is 30).
	DefaultRebalanceIntervalHours = 168.0
	// A rebalance oftener than this is re-sizing on noise: the figures it reads
	// are two wallet balances that move on every settlement.
	MinRebalanceIntervalHours = 1.0
	MaxRebalanceIntervalHours = 24 * 365.0

	// maxQuantizationErrorFrac is how much of a slot's notional may be lost to
	// the venue's step size, as a fraction. A notional only a few steps wide is
	// mostly rounding: at 0.05 it must be at least 20 steps.
	//
	// WHAT THIS BOUNDS IS DEPLOYED CAPITAL, NOT HEDGE ERROR. The brief asked
	// for a guard on "sai số hedge ≤ 5%", but internal/execution already makes
	// that zero by construction: it rounds each leg on its own venue's grid,
	// takes the SMALLER of the two quantities and re-rounds it on the other, so
	// both legs open at the SAME quantity or the intent is refused whole
	// (execution/doc.go, "Sizing, before anything is sent"). What the grid
	// really costs is the part of the slot that never reaches the market: ask
	// for 65 quote of BTC at a 0.0001 step and 77,000 a coin and 0.0008 goes on,
	// which is 61.6 — 5.2% of the slot left behind. That matters here because
	// the whole point of the buffered slot is that each one deploys its share of
	// equity; a slot quietly deploying 90% of its share makes the real buffer
	// larger than the configured one and the allocation a fiction.
	//
	// On this testnet BTCUSDT futures steps 0.0001 BTC — about 7.7 quote at
	// 77,000 — so BTC needs a slot of ~154 quote to stay inside the tolerance.
	// Below that it is refused by name and its slot goes to another pair, which
	// is what the brief's "loại BTC khỏi danh mục phân bổ" asks for.
	maxQuantizationErrorFrac = 0.05
)

// notionalPlanInput is everything one sizing decision reads. A struct rather
// than eight parameters because five of them are quote amounts and two are
// fractions, and adjacent floats transpose silently (CONVENTIONS §6.2).
type notionalPlanInput struct {
	Account Account

	// OpenSpotValueQuote is what the bot's OWN open positions hold on the spot
	// side, marked to the newest mid. It is spot equity that is currently in
	// coin rather than in quote, so leaving it out would shrink the plan every
	// time a position opens and grow it again on every close — a ratchet, not a
	// measurement.
	OpenSpotValueQuote float64

	// Slots is how many positions the run may hold at once, and MarginFrac the
	// collateral posted on a perp leg as a fraction of its notional. Capital
	// per notional is 1 + MarginFrac: the spot leg in full plus that margin.
	Slots      int
	MarginFrac float64

	// BufferPct is held back from the slots, as a fraction.
	BufferPct float64

	// CapQuote is the run's own TotalCapitalCapQuote, and MaxNotionalQuote the
	// portal's ceiling on ONE leg. Both are hard limits the plan may not exceed
	// however much equity there is.
	CapQuote         float64
	MaxNotionalQuote float64
}

// notionalPlan is one sizing decision, kept whole so the console line, the wire
// and the test all read the same numbers.
//
// OK false means NO number here may be used and nothing is re-sized — never
// that the size is zero.
type notionalPlan struct {
	OK       bool
	ReasonVI string

	// The pools, apart. SpotPoolQuote is the spot wallet's quote plus what the
	// bot's own positions hold in coin; FuturesPoolQuote is the futures wallet.
	SpotPoolQuote    float64
	FuturesPoolQuote float64
	TotalEquityQuote float64

	// UnifiedWallet and OpenSpotValueQuote restate the input for the console
	// line: on one wallet SpotPoolQuote IS the whole pool.
	UnifiedWallet      bool
	OpenSpotValueQuote float64

	BufferQuote   float64
	TradableQuote float64

	CapitalPerSlotQuote float64
	NotionalQuote       float64
	CapitalPerNotional  float64

	// BoundByVI names the limit that actually set the size — the equity share,
	// one of the two wallets, the capital cap or the portal's ceiling. It is
	// the first thing to read when a size is not what was expected.
	BoundByVI string
}

// planNotional sizes one slot from what the account holds.
//
// The spec's arithmetic is the first three lines: hold the buffer back, split
// what is left across the slots, and divide by the capital one quote of
// notional ties up.
//
//	tradable = equity × (1 − buffer);  slot = tradable / N;  notional = slot / (1 + marginFrac)
//
// That is exact only when the two wallets happen to be split in the same ratio
// the position needs them — spot 1, futures marginFrac. They are not, and they
// drift apart on every settlement, so the plan is the SMALLEST of that figure
// and what each wallet could fund on its own:
//
//	from the spot wallet:     spotPool / N                          (the spot leg is the whole notional)
//	from the futures wallet:  futuresPool × (1 − buffer) / (N × marginFrac)
//
// The buffer is charged against the FUTURES wallet in that second line and not
// against the spot one, because the spot leg is fully paid for and cannot be
// liquidated: the buffer is margin headroom, and margin lives on one side.
//
// On a UNIFIED wallet there are no two wallets to bound separately: one pool
// pays for the spot leg and posts the perp margin, which is exactly what the
// first line already divides by (1 + marginFrac). The two per-wallet lines are
// therefore not applied — the second would read a futures wallet of 0 and size
// every slot to nothing.
func planNotional(in notionalPlanInput) notionalPlan {
	var out notionalPlan
	switch {
	case in.Slots < 1:
		out.ReasonVI = fmt.Sprintf("số chỗ %d không dương — không chia được vốn", in.Slots)
		return out
	case !finite(in.MarginFrac) || in.MarginFrac <= 0:
		out.ReasonVI = fmt.Sprintf("tỷ lệ ký quỹ perp %v phải dương — chân perp không ký quỹ thì không có mẫu số", in.MarginFrac)
		return out
	case !finite(in.BufferPct) || in.BufferPct < MinMarginBufferPct || in.BufferPct > MaxMarginBufferPct:
		out.ReasonVI = fmt.Sprintf("đệm ký quỹ %v phải trong [%.2f, %.2f]", in.BufferPct, MinMarginBufferPct, MaxMarginBufferPct)
		return out
	case !finite(in.Account.SpotQuoteTotal) || in.Account.SpotQuoteTotal < 0 ||
		!finite(in.Account.FuturesQuoteTotal) || in.Account.FuturesQuoteTotal < 0:
		out.ReasonVI = fmt.Sprintf("số dư đọc được không phải số không âm hữu hạn (spot %v, futures %v)",
			in.Account.SpotQuoteTotal, in.Account.FuturesQuoteTotal)
		return out
	case in.Account.UnifiedWallet && in.Account.FuturesQuoteTotal != 0:
		out.ReasonVI = fmt.Sprintf("ví hợp nhất nhưng có số futures riêng %v — một ví không được cộng hai lần", in.Account.FuturesQuoteTotal)
		return out
	case !finite(in.OpenSpotValueQuote) || in.OpenSpotValueQuote < 0:
		out.ReasonVI = fmt.Sprintf("giá trị chân spot đang giữ %v không phải số không âm hữu hạn", in.OpenSpotValueQuote)
		return out
	case !finite(in.CapQuote) || in.CapQuote <= 0 || !finite(in.MaxNotionalQuote) || in.MaxNotionalQuote <= 0:
		out.ReasonVI = fmt.Sprintf("hạn mức vốn %v hoặc trần notional %v không phải số dương hữu hạn", in.CapQuote, in.MaxNotionalQuote)
		return out
	}

	out.CapitalPerNotional = 1 + in.MarginFrac
	out.UnifiedWallet, out.OpenSpotValueQuote = in.Account.UnifiedWallet, in.OpenSpotValueQuote
	out.SpotPoolQuote = in.Account.SpotQuoteTotal + in.OpenSpotValueQuote
	out.FuturesPoolQuote = in.Account.FuturesQuoteTotal
	out.TotalEquityQuote = out.SpotPoolQuote + out.FuturesPoolQuote
	out.BufferQuote = out.TotalEquityQuote * in.BufferPct
	out.TradableQuote = out.TotalEquityQuote - out.BufferQuote
	slots := float64(in.Slots)
	out.CapitalPerSlotQuote = out.TradableQuote / slots

	// Every ceiling on one leg's notional, named. The smallest wins, and which
	// one it was is the whole diagnosis when a size surprises somebody.
	type limit struct {
		nameVI string
		quote  float64
	}
	limits := []limit{
		{fmt.Sprintf("phần vốn mỗi chỗ (%.2f quote ÷ %.2f)", out.CapitalPerSlotQuote, out.CapitalPerNotional), out.CapitalPerSlotQuote / out.CapitalPerNotional},
	}
	if !in.Account.UnifiedWallet {
		limits = append(limits,
			limit{fmt.Sprintf("ví spot (%.2f quote ÷ %d chỗ)", out.SpotPoolQuote, in.Slots), out.SpotPoolQuote / slots},
			limit{fmt.Sprintf("ví futures (%.2f quote × %.0f%% ÷ %d chỗ ÷ ký quỹ %.2f)", out.FuturesPoolQuote, (1-in.BufferPct)*100, in.Slots, in.MarginFrac),
				out.FuturesPoolQuote * (1 - in.BufferPct) / (slots * in.MarginFrac)})
	}
	limits = append(limits,
		limit{fmt.Sprintf("hạn mức vốn %.2f quote", in.CapQuote), in.CapQuote / (slots * out.CapitalPerNotional)},
		limit{fmt.Sprintf("trần notional mỗi chân của portal %.0f quote", in.MaxNotionalQuote), in.MaxNotionalQuote},
	)
	out.NotionalQuote, out.BoundByVI = math.Inf(1), ""
	for _, l := range limits {
		if l.quote < out.NotionalQuote {
			out.NotionalQuote, out.BoundByVI = l.quote, l.nameVI
		}
	}
	if !finite(out.NotionalQuote) || out.NotionalQuote <= 0 {
		out.ReasonVI = fmt.Sprintf("vốn không đủ để cấp cho %d chỗ: tổng %.2f quote (spot %.2f + futures %.2f), giới hạn chặt nhất là %s",
			in.Slots, out.TotalEquityQuote, out.SpotPoolQuote, out.FuturesPoolQuote, out.BoundByVI)
		out.NotionalQuote = 0
		return out
	}
	out.OK = true
	return out
}

// logLineVI is the REBALANCE console line: the pools, the buffer, the size and
// which limit set it.
func (p notionalPlan) logLineVI(slots int) string {
	if !p.OK {
		return "KHÔNG cân bằng được: " + p.ReasonVI
	}
	if p.UnifiedWallet {
		return fmt.Sprintf("tổng vốn %.2f quote (MỘT ví hợp nhất, gồm %.2f đang nằm ở coin của vị thế bot), đệm %.0f%% = %.2f quote, còn %.2f quote chia %d chỗ ⇒ %.2f quote mỗi chỗ ⇒ %.2f quote notional mỗi chân (chặn bởi %s). Vị thế ĐANG MỞ giữ nguyên quy mô cũ.",
			p.TotalEquityQuote, p.OpenSpotValueQuote,
			p.BufferQuote/nonZero(p.TotalEquityQuote)*100, p.BufferQuote, p.TradableQuote, slots,
			p.CapitalPerSlotQuote, p.NotionalQuote, p.BoundByVI)
	}
	return fmt.Sprintf("tổng vốn %.2f quote (ví spot %.2f + ví futures %.2f), đệm %.0f%% = %.2f quote, còn %.2f quote chia %d chỗ ⇒ %.2f quote mỗi chỗ ⇒ %.2f quote notional mỗi chân (chặn bởi %s). Vị thế ĐANG MỞ giữ nguyên quy mô cũ.",
		p.TotalEquityQuote, p.SpotPoolQuote, p.FuturesPoolQuote,
		p.BufferQuote/nonZero(p.TotalEquityQuote)*100, p.BufferQuote, p.TradableQuote, slots,
		p.CapitalPerSlotQuote, p.NotionalQuote, p.BoundByVI)
}

func nonZero(v float64) float64 {
	if v == 0 {
		return 1
	}
	return v
}

// sizeFloorQuote is the smallest notional one leg may be asked for on this
// symbol, and why. It reads the venue's OWN rules for both markets, never a
// mainnet snapshot and never a constant (PLAN 4.2: BTCUSDT is step 0.0001 on
// futures against 0.00001 on spot, minNotional 50 against 5).
//
// Three things bound it, and the tightest wins:
//
//   - the venue's minimum notional, the stricter of the two markets';
//   - the venue's minimum quantity at this price, likewise;
//   - the quantization guard: the quantity floors onto the coarser step, so a
//     notional worth only a few steps leaves most of the slot unspent.
//
// A zero rule means it could not be read, and the floor is then unknown rather
// than zero: ok false. Costing an unread rule at zero is how a refused order
// becomes a trade failure and, five in a row, a halted pair.
func sizeFloorQuote(stepCoin, minQtyCoin, minNotionalQuote, priceQuote float64) (floor float64, reasonVI string, ok bool) {
	if !(priceQuote > 0) {
		return 0, "chưa có giá để quy đổi luật sàn ra quote", false
	}
	if !(stepCoin > 0) || !(minQtyCoin > 0) || !(minNotionalQuote > 0) {
		return 0, fmt.Sprintf("chưa đọc được luật sàn (bước %v, lượng tối thiểu %v, notional tối thiểu %v) — 'chưa đọc' không phải 'không có giới hạn'",
			stepCoin, minQtyCoin, minNotionalQuote), false
	}
	parts := []struct {
		nameVI string
		quote  float64
	}{
		{fmt.Sprintf("notional tối thiểu của sàn %.2f", minNotionalQuote), minNotionalQuote},
		{fmt.Sprintf("lượng tối thiểu %.8f × giá %.2f", minQtyCoin, priceQuote), minQtyCoin * priceQuote},
		{fmt.Sprintf("bước nhảy %.8f × giá %.2f ÷ sai số quy mô %.0f%%", stepCoin, priceQuote, maxQuantizationErrorFrac*100), stepCoin * priceQuote / maxQuantizationErrorFrac},
	}
	for _, p := range parts {
		if p.quote > floor {
			floor, reasonVI = p.quote, p.nameVI
		}
	}
	return floor, reasonVI, true
}
