package exchanges

import (
	"fmt"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

// BybitTrade and BybitOrderbook are the two payload shapes. Linear futures and
// spot publish them identically, so the separate BybitSpot* copies of these
// structs - byte-for-byte duplicates - went away with the shared handler below.
type BybitTrade struct {
	Topic string `json:"topic"`
	Type  string `json:"type"`
	Data  []struct {
		Symbol    string `json:"s"`
		Price     string `json:"p"`
		Size      string `json:"v"`
		Side      string `json:"S"`
		Timestamp int64  `json:"T"`
		TradeID   string `json:"i"`
	} `json:"data"`
}

type BybitOrderbook struct {
	Topic string `json:"topic"`
	Type  string `json:"type"`
	Data  struct {
		Symbol   string     `json:"s"`
		Bids     [][]string `json:"b"`
		Asks     [][]string `json:"a"`
		UpdateID int64      `json:"u"`
		SeqNum   int64      `json:"seq"`
	} `json:"data"`
}

// Bybit documents an application-level heartbeat rather than a protocol ping:
// "send the ping heartbeat packet every 20 seconds to maintain the WebSocket
// connection", and "if there is no ping-pong and no stream data sent from server
// end, the connection will be cut off after 10 minutes".
// https://bybit-exchange.github.io/docs/v5/ws/connect
const bybitPingEvery = 20 * time.Second

var bybitPing = jsonPing(map[string]string{"op": "ping"})

// bybitSubscribe asks for the top of book and the trade feed for every symbol,
// plus the ticker when this feed carries funding.
//
// tickers is a LINEAR-only channel here: it is where Bybit publishes the
// funding rate, and spot has no funding to publish. Subscribing to it on the
// spot socket would ask a venue for a market that does not exist.
func bybitSubscribe(symbols []Symbol, tickers map[string]*bybitTicker, withFunding bool) func(*websocket.Conn) error {
	return func(conn *websocket.Conn) error {
		args := make([]string, 0, len(symbols)*3)
		for _, symbol := range symbols {
			args = append(args,
				fmt.Sprintf("orderbook.1.%s", symbol.Venue),
				fmt.Sprintf("publicTrade.%s", symbol.Venue))
			if withFunding {
				args = append(args, fmt.Sprintf("tickers.%s", symbol.Venue))
			}
		}
		// Ticker state describes the socket that carried it: a snapshot arrives
		// once per subscription and every later delta builds on it, so state
		// kept across a reconnect could supply a field the new session never
		// confirmed. Bybit resends the snapshot, so nothing is lost.
		clear(tickers)
		return conn.WriteJSON(map[string]any{"op": "subscribe", "args": args})
	}
}

func ConnectBybitFutures(source string, symbols []Symbol, f Feeds) {
	runStream(f, bybitStream(source, symbols, f, "wss://stream.bybit.com/v5/public/linear", true))
}

// ConnectBybitSpot connects to Bybit spot trading WebSocket API.
func ConnectBybitSpot(source string, symbols []Symbol, f Feeds) {
	runStream(f, bybitStream(source, symbols, f, "wss://stream.bybit.com/v5/public/spot", false))
}

// bybitStream builds the config for one Bybit feed. withFunding is passed in
// rather than inferred from the URL: a testnet host, a regional mirror or one
// added query parameter would defeat a suffix test, and the failure would be
// silent — a healthy socket, live prices, and no funding at all.
func bybitStream(source string, symbols []Symbol, f Feeds, url string, withFunding bool) streamConfig {
	tickers := make(map[string]*bybitTicker, len(symbols))

	return streamConfig{
		Source:    source,
		URL:       url,
		Subscribe: bybitSubscribe(symbols, tickers, withFunding),
		Ping:      bybitPing,
		PingEvery: bybitPingEvery,
		Handle: func(raw []byte, recvAt time.Time) {
			handleBybitFrame(source, symbols, tickers, f, raw, recvAt)
		},
	}
}

func handleBybitFrame(source string, symbols []Symbol, tickers map[string]*bybitTicker, f Feeds, raw []byte, recvAt time.Time) {
	// Tried first: a ticker frame decodes as an orderbook too (both carry a
	// data object), and the orderbook branch below only rejects it because the
	// bids/asks arrays are empty — an accident, not a check.
	if handleBybitTicker(source, symbols, tickers, f, raw, recvAt) {
		return
	}

	// Try to parse as orderbook first
	var orderbookMsg BybitOrderbook
	if decode(raw, &orderbookMsg) &&
		len(orderbookMsg.Data.Asks) > 0 && len(orderbookMsg.Data.Bids) > 0 {

		bidPrice, err1 := strconv.ParseFloat(orderbookMsg.Data.Bids[0][0], 64)
		askPrice, err2 := strconv.ParseFloat(orderbookMsg.Data.Asks[0][0], 64)
		if err1 != nil || err2 != nil {
			return
		}

		// Each level is [price, size]; only index 0 was read before step 1.2, so
		// the size was decoded and dropped. A malformed level must not discard
		// the price, so a missing or unparseable size is left at 0 - which means
		// "not known". Unit is base coin, see OrderbookData.
		var bidQtyCoin, askQtyCoin float64
		if len(orderbookMsg.Data.Bids[0]) > 1 {
			bidQtyCoin, _ = strconv.ParseFloat(orderbookMsg.Data.Bids[0][1], 64)
		}
		if len(orderbookMsg.Data.Asks[0]) > 1 {
			askQtyCoin, _ = strconv.ParseFloat(orderbookMsg.Data.Asks[0][1], 64)
		}

		standardSymbol := StandardOf(symbols, orderbookMsg.Data.Symbol)
		if standardSymbol == "" {
			return // a market this connector never subscribed to
		}

		f.SendOrderbook(OrderbookData{
			Symbol:         standardSymbol,
			Source:         source,
			BestBid:        bidPrice,
			BestAsk:        askPrice,
			BestBidQtyCoin: bidQtyCoin,
			BestAskQtyCoin: askQtyCoin,
			// This message carries no venue timestamp this connector parses.
			// Writing the local clock here would report our own time as the
			// venue's; RecvAt is our clock and is labelled as such.
			VenueTimeMs: 0,
			RecvAt:      recvAt,
		})
		return
	}

	// Try to parse as trade message
	var tradeMsg BybitTrade
	if decode(raw, &tradeMsg) && (tradeMsg.Type == "snapshot" || tradeMsg.Type == "delta") {
		for _, trade := range tradeMsg.Data {
			price, err := strconv.ParseFloat(trade.Price, 64)
			if err != nil {
				continue
			}

			// Normalize trade side (Bybit uses "Buy" and "Sell")
			side := "sell"
			if trade.Side == "Buy" {
				side = "buy"
			}

			standardSymbol := StandardOf(symbols, trade.Symbol)
			if standardSymbol == "" {
				continue
			}

			if !f.SendTrade(TradeData{
				Symbol:      standardSymbol,
				Source:      source,
				Price:       price,
				Quantity:    trade.Size,
				Side:        side,
				VenueTimeMs: trade.Timestamp,
				RecvAt:      recvAt,
			}) {
				return // shutting down; the rest of the batch is not worth parsing
			}
		}
	}
}
