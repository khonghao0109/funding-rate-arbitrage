package exchanges

import (
	"time"

	"github.com/gorilla/websocket"
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
// So the obvious guess is the wrong one, and Ping is left nil: runStream sends
// an RFC 6455 ping frame. Half the documented interval is used - see
// defaultPingEvery - so one lost ping is not fatal.

func processKrakenOrderbook(source string, symbols []Symbol, productID string, orderBook *KrakenOrderBook, f Feeds, recvAt time.Time) bool {
	if len(orderBook.Bids) == 0 || len(orderBook.Asks) == 0 {
		return true
	}

	symbol := StandardOf(symbols, productID)
	if symbol == "" {
		return true // a product this connector never subscribed to
	}

	// BestBidQtyCoin/BestAskQtyCoin are deliberately left at 0. The book entries
	// do carry a Qty, and measured 2026-09-03 it looks coin denominated
	// (PF_XBTUSD 0.0929 with BTC near $77.5k, PF_XRPUSD 95000) - but
	// docs/DATA-REQUIREMENTS.md §3 records Kraken as denominating in contracts,
	// and a field named ...Coin must not be filled from a measurement that
	// contradicts the survey. The instrument registry (internal/instruments,
	// phase 2) settles which is right; until then 0 means "not known".
	return f.SendOrderbook(OrderbookData{
		Symbol:  symbol,
		Source:  source,
		BestBid: orderBook.Bids[0].Price, // bids are held highest first
		BestAsk: orderBook.Asks[0].Price, // asks lowest first
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

func ConnectKrakenFutures(source string, symbols []Symbol, f Feeds) {
	runStream(f, krakenStream(source, symbols, f))
}

func krakenStream(source string, symbols []Symbol, f Feeds) streamConfig {
	// One assembled book per product, rebuilt from scratch on every connection.
	// Seeded here rather than only in Subscribe so the handler has somewhere to
	// put a frame from the moment the config exists - a book that is empty is a
	// different thing from a product this connector does not follow, and only
	// the second may be dropped.
	orderbooks := make(map[string]*KrakenOrderBook, len(symbols))
	for _, symbol := range symbols {
		orderbooks[symbol.Venue] = &KrakenOrderBook{}
	}

	return streamConfig{
		Source: source,
		URL:    "wss://futures.kraken.com/ws/v1",
		Subscribe: func(conn *websocket.Conn) error {
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
				err := conn.WriteJSON(map[string]any{
					"event":       "subscribe",
					"feed":        "book",
					"product_ids": []string{symbol.Venue},
				})
				if err != nil {
					return err
				}
				orderbooks[symbol.Venue] = &KrakenOrderBook{}
			}
			return nil
		},
		Handle: func(raw []byte, recvAt time.Time) {
			handleKrakenFrame(source, symbols, orderbooks, f, raw, recvAt)
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
func handleKrakenFrame(source string, symbols []Symbol, orderbooks map[string]*KrakenOrderBook, f Feeds, raw []byte, recvAt time.Time) {
	var data KrakenOrderBookData
	if !decode(raw, &data) || data.Feed == "" {
		return
	}

	orderbook, exists := orderbooks[data.ProductID]
	if !exists {
		return
	}

	switch data.Feed {
	case "book_snapshot":
		orderbook.Bids = data.Bids
		orderbook.Asks = data.Asks
	case "book":
		updateKrakenOrderbook(orderbook, data)
	default:
		// Subscription acknowledgements, the pong, heartbeats.
		return
	}

	processKrakenOrderbook(source, symbols, data.ProductID, orderbook, f, recvAt)
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
