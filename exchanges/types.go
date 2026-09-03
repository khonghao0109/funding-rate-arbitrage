package exchanges

// The types crossing this boundary are public market data only. No credential
// ever appears here - see docs/CONVENTIONS.md §12.1.
//
// VenueTimeMs is the venue's own timestamp, in milliseconds, and is 0 when the
// venue does not provide one. It must NEVER be used to decide whether data is
// fresh: three of the eight venues have no timestamp in the payload the
// connector reads, and filling it with the local clock would make a dead feed
// look current forever. The scanner stamps its own receive time in exactly one
// place and measures staleness from that. See docs/WS-CONTRACT.md §4.1.

type PriceData struct {
	Symbol      string
	Source      string
	Price       float64
	VenueTimeMs int64

	// Top of book behind this price, 0 when the source has no book. An oracle
	// publishes a price and nothing else, so these stay 0 for it.
	BestBid        float64
	BestAsk        float64
	BestBidQtyCoin float64
	BestAskQtyCoin float64
}

type OrderbookData struct {
	Symbol      string
	Source      string
	BestBid     float64
	BestAsk     float64
	VenueTimeMs int64

	// BestBidQtyCoin and BestAskQtyCoin are the size resting at the top of the book,
	// in BASE COIN - never in contracts. OKX, Gate and Kraken denominate their book
	// in contracts, so a connector for one of those must convert with the
	// instrument's ctVal/quanto_multiplier before filling these, or leave them 0.
	// Publishing a contract count in a field named ...Coin produces a number that is
	// wrong without looking wrong.
	//
	// The unit was settled by MEASUREMENT on 2026-09-03, not from documentation: the
	// venue pages that would state it render client-side and could not be read from
	// this environment. With BTC at ~$77.5k and XRP at ~$1.36 the venues published:
	//
	//	binance futures  BTC 7.943    XRP 92867.3   -> coin
	//	binance spot     BTC 16.408   XRP 20374.7   -> coin
	//	bybit linear     BTC 4.944    XRP 8883.8    -> coin
	//	bybit spot       BTC 0.4577   XRP 58.8      -> coin
	//	hyperliquid      BTC 1.6546   XRP 45968     -> coin
	//	okx swap         BTC 1182.68  XRP 334.52    -> contracts
	//	gate futures     BTC 10099    XRP 36        -> contracts
	//
	// A quantity scaling inversely with price is a coin count; the OKX and Gate
	// numbers are orders of magnitude away from that (1182 BTC would be a $91M top
	// of book, 334 XRP a $456 one) and are left at 0 rather than converted by guess.
	// 0 means "not known", and every consumer must treat it as such - it is not
	// "no liquidity". See docs/PLAN.md step 1.2 and docs/DATA-REQUIREMENTS.md §3.
	BestBidQtyCoin float64
	BestAskQtyCoin float64
}

type TradeData struct {
	Symbol      string
	Source      string
	Price       float64
	Quantity    string
	Side        string // "buy" or "sell" (normalized)
	VenueTimeMs int64
}
