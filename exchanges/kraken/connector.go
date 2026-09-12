package kraken

import (
	"futures-arbitrage-scanner/exchanges"

	"sort"
	"time"
)

type KrakenOrderBookEntry struct {
	Price float64 `json:"price"`
	Qty   float64 `json:"qty"`
}

type KrakenOrderBookData struct {
	Feed      string                 `json:"feed"`
	ProductID string                 `json:"product_id"`
	Side      string                 `json:"side,omitempty"`
	Seq       int64                  `json:"seq"`
	Price     float64                `json:"price,omitempty"`
	Qty       float64                `json:"qty,omitempty"`
	Bids      []KrakenOrderBookEntry `json:"bids,omitempty"`
	Asks      []KrakenOrderBookEntry `json:"asks,omitempty"`
	Timestamp float64                `json:"timestamp,omitempty"`
}

type KrakenOrderBook struct {
	Bids []KrakenOrderBookEntry
	Asks []KrakenOrderBookEntry
}

// Kraken Futures requires a client-driven keepalive: "Send a ping request at
// least every 60 seconds to keep the connection open."
// https://docs.kraken.com/api/docs/guides/futures-websockets/
//
// That page states the interval but not the shape of the message, and the pages
// that would give it were not reachable from here. Measured against
// wss://futures.kraken.com/ws/v1 on 2026-09-03:
//
//	{"event":"ping"}  -> {"event":"alert","message":"Bad websocket message"}
//	protocol ping     -> pong, including while subscribed and streaming a book
//
// So the obvious guess is the wrong one, and Ping is left nil: RunStream sends
// an RFC 6455 ping frame. Half the documented interval is used - see
// defaultPingEvery - so one lost ping is not fatal.

// processKrakenOrderbook publishes the assembled book and reports whether it
// produced a message. Until 2026-09-12 the two early exits below answered true,
// meaning "not shutting down"; the value is now the one StreamConfig.Handle
// needs — a half-assembled book is not data.
func processKrakenOrderbook(source string, symbols []exchanges.Symbol, productID string, orderBook *KrakenOrderBook, f exchanges.Feeds, recvAt time.Time) bool {
	if len(orderBook.Bids) == 0 || len(orderBook.Asks) == 0 {
		return false
	}

	symbol := exchanges.StandardOf(symbols, productID)
	if symbol == "" {
		return false // a product this connector never subscribed to
	}

	// The Qty is a CONTRACT count, and step 2.3 settled the apparent
	// contradiction that made step 1.2 drop it: PF_ markets ARE contract
	// denominated, but one contract is 1 base unit, so the count is
	// NUMERICALLY coin. The measurement (PF_XBTUSD 0.0929 with BTC near $77.5k)
	// and the survey were both right.
	//
	// It still travels as contracts rather than as coin. The multiplier being 1
	// is a fact about today's PF_ contract specification, not about the field,
	// and the registry is where that fact is measured — a venue that revised it
	// would silently make every quantity here wrong if this file assumed it.
	return f.SendOrderbook(exchanges.OrderbookData{
		Symbol:              symbol,
		Source:              source,
		BestBid:             orderBook.Bids[0].Price, // bids are held highest first
		BestAsk:             orderBook.Asks[0].Price, // asks lowest first
		BestBidQtyContracts: orderBook.Bids[0].Qty,
		BestAskQtyContracts: orderBook.Asks[0].Qty,
		// Left at 0 although Kraken does publish one: measured 2026-09-03 both
		// book_snapshot and every book delta carry `timestamp` in milliseconds
		// (1788413683802). This function receives the ASSEMBLED book rather than
		// the message that changed it, so threading the value here means
		// changing what the assembler passes on. That is listed as deferred debt
		// for this phase - "venue time thật cho Bybit/Kraken/Paradex" in
		// docs/PLAN.md - and doing it here would be scope this step did not
		// take. Writing the local clock instead would report our time as the
		// venue's; RecvAt is our clock and says so.
		VenueTimeMs: 0,
		RecvAt:      recvAt,
	})
}

func ConnectFutures(source string, symbols []exchanges.Symbol, f exchanges.Feeds) {
	exchanges.RunStream(f, krakenStream(source, symbols, f))
}

func krakenStream(source string, symbols []exchanges.Symbol, f exchanges.Feeds) exchanges.StreamConfig {
	// One assembled book per product, rebuilt from scratch on every connection.
	// Seeded here rather than only in Subscribe so the handler has somewhere to
	// put a frame from the moment the config exists - a book that is empty is a
	// different thing from a product this connector does not follow, and only
	// the second may be dropped.
	orderbooks := make(map[string]*KrakenOrderBook, len(symbols))
	for _, symbol := range symbols {
		orderbooks[symbol.Venue] = &KrakenOrderBook{}
	}

	return exchanges.StreamConfig{
		Source: source,
		URL:    "wss://futures.kraken.com/ws/v1",
		Subscribe: func(conn exchanges.Subscriber) error {
			// Books assembled over the previous socket describe a session that
			// no longer exists, and Kraken resends a full book_snapshot on
			// subscribe. Keeping them across a reconnect would leave levels that
			// were deleted while we were away. This map was built once outside
			// the reconnect loop before step 1.5.
			clear(orderbooks)

			for _, symbol := range symbols {
				// config.yaml supplies the venue identifier (symbol_format
				// "PF_{base}USD", with BTCUSDT overridden to PF_XBTUSD because
				// Kraken calls bitcoin XBT).
				//
				// "ticker" is the funding source (step 2.5): it carries
				// relative_funding_rate and the absolute next_funding_rate_time.
				for _, feed := range []string{"book", "ticker"} {
					err := conn.WriteJSON(map[string]any{
						"event":       "subscribe",
						"feed":        feed,
						"product_ids": []string{symbol.Venue},
					})
					if err != nil {
						return err
					}
				}
				orderbooks[symbol.Venue] = &KrakenOrderBook{}
			}
			return nil
		},
		Handle: func(raw []byte, recvAt time.Time) bool {
			return handleKrakenFrame(source, symbols, orderbooks, f, raw, recvAt)
		},
	}
}

// handleKrakenFrame folds one message into the assembled book for its product
// and publishes the new top of book.
//
// Kraken is the only venue here whose top of book is not in the message: a
// snapshot arrives once and every later frame moves ONE level, so the book has
// to be maintained locally and the connector's output depends on every frame
// that came before. orderbooks is that state, keyed by the venue's product id
// and reset on each reconnect by Subscribe.
//
// It reports whether the frame became a message on a feed — the contract
// exchanges.StreamConfig.Handle documents. False is the answer for the
// keepalive reply, a heartbeat, and a subscribe acknowledgement, which is what
// lets the lifecycle tell a live subscription from a socket that is merely open
// (docs/PLAN.md step 1.6). A delta that moves a level without changing the top
// of book still counts: it produced a publish.
func handleKrakenFrame(source string, symbols []exchanges.Symbol, orderbooks map[string]*KrakenOrderBook, f exchanges.Feeds, raw []byte, recvAt time.Time) bool {
	var data KrakenOrderBookData
	if !exchanges.Decode(raw, &data) || data.Feed == "" {
		return false
	}
	// Dispatched on the feed this Decode already read, rather than by
	// speculatively unmarshalling every frame into the funding shape first: a
	// PF_XBTUSD book snapshot carries ~1,800 levels and encoding/json walks the
	// whole document even to read one field, so a second pass would double the
	// cost of the busiest connector's hottest path.
	if data.Feed == "ticker" {
		_, produced := handleKrakenFunding(source, symbols, f, raw, recvAt)
		return produced
	}

	orderbook, exists := orderbooks[data.ProductID]
	if !exists {
		return false
	}

	switch data.Feed {
	case "book_snapshot":
		orderbook.Bids = data.Bids
		orderbook.Asks = data.Asks
		// Sorted unconditionally, like FinishDepthBook on the REST side, and
		// for the same measured reason: this SAME venue orders the SAME book
		// differently per transport. The step-2.7b probe found the REST book's
		// bids ASCENDING with a resting order at price 1 first, while the WS
		// recording arrives descending — trusting the WS order means one
		// venue-side change makes every published Kraken best bid the $1
		// order, silently. upsertPriceLevel assumes sortedness from here on,
		// so the snapshot is the one place the order must be established.
		sort.Slice(orderbook.Bids, func(i, j int) bool { return orderbook.Bids[i].Price > orderbook.Bids[j].Price })
		sort.Slice(orderbook.Asks, func(i, j int) bool { return orderbook.Asks[i].Price < orderbook.Asks[j].Price })
	case "book":
		updateKrakenOrderbook(orderbook, data)
	default:
		// Subscription acknowledgements, the pong, heartbeats.
		return false
	}

	return processKrakenOrderbook(source, symbols, data.ProductID, orderbook, f, recvAt)
}

func updateKrakenOrderbook(orderbook *KrakenOrderBook, data KrakenOrderBookData) {
	if data.Qty == 0 {
		// Remove price level
		removePriceLevel(orderbook, data.Side, data.Price)
	} else {
		// Update or add price level
		upsertPriceLevel(orderbook, data.Side, data.Price, data.Qty)
	}
}

func removePriceLevel(orderbook *KrakenOrderBook, side string, price float64) {
	if side == "buy" {
		for i, entry := range orderbook.Bids {
			if entry.Price == price {
				orderbook.Bids = append(orderbook.Bids[:i], orderbook.Bids[i+1:]...)
				break
			}
		}
	} else if side == "sell" {
		for i, entry := range orderbook.Asks {
			if entry.Price == price {
				orderbook.Asks = append(orderbook.Asks[:i], orderbook.Asks[i+1:]...)
				break
			}
		}
	}
}

func upsertPriceLevel(orderbook *KrakenOrderBook, side string, price, qty float64) {
	newEntry := KrakenOrderBookEntry{Price: price, Qty: qty}

	if side == "buy" {
		// Bids are sorted in descending order (highest first)
		for i, entry := range orderbook.Bids {
			if entry.Price == price {
				// Update existing level
				orderbook.Bids[i].Qty = qty
				return
			}

			if price > entry.Price {
				// Insert new level
				orderbook.Bids = append(orderbook.Bids[:i], append([]KrakenOrderBookEntry{newEntry}, orderbook.Bids[i:]...)...)
				return
			}
		}
		// Append to end if not inserted
		orderbook.Bids = append(orderbook.Bids, newEntry)
	} else if side == "sell" {
		// Asks are sorted in ascending order (lowest first)
		for i, entry := range orderbook.Asks {
			if entry.Price == price {
				// Update existing level
				orderbook.Asks[i].Qty = qty
				return
			}

			if price < entry.Price {
				// Insert new level
				orderbook.Asks = append(orderbook.Asks[:i], append([]KrakenOrderBookEntry{newEntry}, orderbook.Asks[i:]...)...)
				return
			}
		}
		// Append to end if not inserted
		orderbook.Asks = append(orderbook.Asks, newEntry)
	}
}
