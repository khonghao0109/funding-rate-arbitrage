package broker

import "context"

// What a position really cost, read from the venue — step 4.5.
//
// Step 4.2's Order carries no commission, because neither venue puts one on the
// order answer: USDⓈ-M states it only in /fapi/v1/userTrades and spot only in
// /api/v3/myTrades. And funding is not on any order at all; it is an account
// event, published as income. So closing a position and saying what it made
// needs two reads the order interface does not have, and they are here as
// OPTIONAL capabilities rather than as methods on Broker.
//
// Optional, because they are not universal. Spot has no funding and no
// position; a venue that cannot answer must say ErrNotSupported rather than
// return an empty list, since "no funding was paid" and "I cannot tell you what
// was paid" are different facts and only one of them may be added up.
//
// # Commission is NOT converted here (CLAUDE.md rule 5, rule 4)
//
// A venue reports commission in whatever asset it took it in: USDⓈ-M takes it
// in the margin asset (USDT), spot takes it in the asset received — BTC on a
// BUY — or in BNB when the account elects that. Converting BTC to USDT needs a
// price, at a moment, from somewhere, and picking one silently is how a cost
// becomes a number that is wrong but not obviously wrong. So the quantity and
// its asset travel together, unconverted, and the caller decides what it can
// price.

// Trade is one fill as the venue reports it, GROSS.
type Trade struct {
	Market Market
	Symbol string

	// TradeID is the venue's own fill id; VenueOrderID is the order it belongs
	// to. Both are STRINGS for the reason Order.VenueOrderID is: they are
	// 64-bit integers at the venue and a float64 loses the low digits.
	TradeID      string
	VenueOrderID string

	Side       Side
	QtyCoin    float64
	PriceQuote float64

	// CommissionQtyInAsset is the fee the venue took, denominated in
	// CommissionAsset — NOT in quote, and never converted. See the file
	// comment.
	CommissionQtyInAsset float64
	CommissionAsset      string

	IsMaker bool
	TimeMs  int64
}

// NotionalQuote is what this fill traded, GROSS of every cost.
func (t Trade) NotionalQuote() float64 { return t.QtyCoin * t.PriceQuote }

// FundingIncome is one SETTLEMENT that actually happened, as the venue reports
// it.
//
// It is a discrete event, which is CLAUDE.md rule 6 made concrete: a position
// earns nothing unless it was open at the settlement timestamp, and there is no
// APR here to multiply by a holding time. Counting these rows IS counting
// settlements crossed.
type FundingIncome struct {
	Market Market
	Symbol string

	// IncomeQuote is SIGNED as the venue signs it: positive was received,
	// negative was paid. A short perp leg receives it when funding is
	// positive, which is the whole strategy, and pays it when funding turns —
	// so taking an absolute value here would turn a cost into a profit.
	IncomeQuote float64
	Asset       string

	SettledAtMs int64
	TranID      string
}

// TradeReader is an OPTIONAL Broker capability: the fills of one order, which
// is the only place either venue states a commission.
type TradeReader interface {
	// OrderTrades returns the fills of ONE order, identified the same way
	// GetOrder identifies it. An order with no fills returns an empty slice
	// and no error; an order the venue does not know returns ErrOrderNotFound.
	OrderTrades(ctx context.Context, q OrderQuery) ([]Trade, error)
}

// FundingReader is an OPTIONAL Broker capability: the settlements that actually
// paid, over a window.
type FundingReader interface {
	// FundingIncome returns every funding settlement for one symbol between
	// two instants, inclusive. A market with no funding concept returns
	// ErrNotSupported — never an empty slice, which would read as "nothing was
	// paid".
	FundingIncome(ctx context.Context, market Market, symbol string, startMs, endMs int64) ([]FundingIncome, error)
}

// MarkPriceReader is an OPTIONAL Broker capability: the venue's mark price and,
// more importantly for step 4.5, WHEN IT SETTLES NEXT.
//
// A lifecycle test that must hold a position across a settlement cannot guess
// the cadence (rule 3: not 8h, not anything). It asks.
type MarkPriceReader interface {
	MarkPrice(ctx context.Context, market Market, symbol string) (MarkPrice, error)
}

// MarkPrice is the venue's mark, its last settled funding rate, and the next
// settlement stamp.
type MarkPrice struct {
	Market Market
	Symbol string

	MarkPriceQuote  float64
	IndexPriceQuote float64

	// LastFundingRateFrac is the venue's own most recent figure, as a
	// FRACTION, unannualized and uninterpreted. The interval is NOT derived
	// from it and NOT assumed: NextFundingTimeMs is the only cadence fact this
	// struct carries.
	LastFundingRateFrac float64
	NextFundingTimeMs   int64

	VenueTimeMs int64
}
