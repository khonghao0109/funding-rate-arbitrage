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
}

type OrderbookData struct {
	Symbol      string
	Source      string
	BestBid     float64
	BestAsk     float64
	VenueTimeMs int64
}

type TradeData struct {
	Symbol      string
	Source      string
	Price       float64
	Quantity    string
	Side        string // "buy" or "sell" (normalized)
	VenueTimeMs int64
}
