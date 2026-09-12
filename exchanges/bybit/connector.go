package bybit

import (
	"futures-arbitrage-scanner/exchanges"

	"fmt"
	"log"
	"strconv"
	"time"
)

// bybitOpReply is the answer to an "op" request — subscribe or ping. It carries
// no market data and must never count as data on the wire.
//
// https://bybit-exchange.github.io/docs/v5/ws/connect
type bybitOpReply struct {
	Success bool   `json:"success"`
	RetMsg  string `json:"ret_msg"`
	Op      string `json:"op"`
	ConnID  string `json:"conn_id"`
}

// decodeBybitOpReply reports whether the frame is one of those replies. The
// "op" field is what distinguishes it: a data frame carries "topic" instead.
func decodeBybitOpReply(raw []byte) (bybitOpReply, bool) {
	var reply bybitOpReply
	if !exchanges.Decode(raw, &reply) || reply.Op == "" {
		return bybitOpReply{}, false
	}
	return reply, true
}

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

var bybitPing = exchanges.JSONPing(map[string]string{"op": "ping"})

// bybitSpotMaxArgsPerRequest is the venue's documented ceiling on one subscribe
// request, and it applies to SPOT only: "Spot can input up to 10 args for each
// subscription request sent to one connection", against "No args limit for
// Futures and Spread for now".
// https://bybit-exchange.github.io/docs/v5/ws/connect
//
// Exceeding it does not truncate the request — it refuses the WHOLE thing.
// Measured against the live venue 2026-09-12: 13 symbols × 2 topics = 26 args
// answers {"success":false,"ret_msg":"args size \u003e10","op":"subscribe"} and
// then 0 data frames, while 4 symbols = 8 args answers success and 99 data
// frames in 12 seconds. That is exactly what happened to bybit_spot when the
// pair list grew from 4 to 13 on 2026-09-09: the socket connected, answered
// every ping for 19 hours, and delivered nothing at all (PLAN step 1.6).
//
// 0 means "no documented ceiling", which is what the linear feed passes.
const bybitSpotMaxArgsPerRequest = 10

// bybitSubscribe asks for the top of book and the trade feed for every symbol,
// plus the ticker when this feed carries funding.
//
// tickers is a LINEAR-only channel here: it is where Bybit publishes the
// funding rate, and spot has no funding to publish. Subscribing to it on the
// spot socket would ask a venue for a market that does not exist.
func bybitSubscribe(symbols []exchanges.Symbol, tickers map[string]*bybitTicker, withFunding bool, maxArgs int) func(exchanges.Subscriber) error {
	return func(conn exchanges.Subscriber) error {
		return bybitSubscribeTo(conn, symbols, tickers, withFunding, maxArgs)
	}
}

func bybitSubscribeTo(conn exchanges.Subscriber, symbols []exchanges.Symbol, tickers map[string]*bybitTicker, withFunding bool, maxArgs int) error {
	args := bybitTopics(symbols, withFunding)
	// Ticker state describes the socket that carried it: a snapshot arrives
	// once per subscription and every later delta builds on it, so state
	// kept across a reconnect could supply a field the new session never
	// confirmed. Bybit resends the snapshot, so nothing is lost.
	clear(tickers)
	for _, batch := range bybitArgBatches(args, maxArgs) {
		if err := conn.WriteJSON(map[string]any{"op": "subscribe", "args": batch}); err != nil {
			return err
		}
	}
	return nil
}

// bybitTopics is every channel this feed subscribes to, in symbol order.
//
// tickers is a LINEAR-only channel — see bybitSubscribe.
func bybitTopics(symbols []exchanges.Symbol, withFunding bool) []string {
	topics := make([]string, 0, len(symbols)*3)
	for _, symbol := range symbols {
		topics = append(topics,
			fmt.Sprintf("orderbook.1.%s", symbol.Venue),
			fmt.Sprintf("publicTrade.%s", symbol.Venue))
		if withFunding {
			topics = append(topics, fmt.Sprintf("tickers.%s", symbol.Venue))
		}
	}
	return topics
}

// bybitArgBatches splits the topic list into requests the venue will accept.
// maxArgs <= 0 means the feed documents no ceiling and the list travels whole.
//
// Splitting rather than dropping is the point: a topic left out would be a
// market silently missing from the dashboard, which is the same class of
// failure as the refusal it avoids — just quieter.
func bybitArgBatches(args []string, maxArgs int) [][]string {
	if maxArgs <= 0 || len(args) <= maxArgs {
		return [][]string{args}
	}
	batches := make([][]string, 0, (len(args)+maxArgs-1)/maxArgs)
	for start := 0; start < len(args); start += maxArgs {
		end := min(start+maxArgs, len(args))
		batches = append(batches, args[start:end])
	}
	return batches
}

func ConnectFutures(source string, symbols []exchanges.Symbol, f exchanges.Feeds) {
	exchanges.RunStream(f, futuresStream(source, symbols, f))
}

// ConnectSpot connects to Bybit spot trading WebSocket API.
func ConnectSpot(source string, symbols []exchanges.Symbol, f exchanges.Feeds) {
	exchanges.RunStream(f, spotStream(source, symbols, f))
}

// futuresStream and spotStream are where each feed's own rules are written, and
// they exist so those rules are written EXACTLY ONCE. The ceiling below used to
// sit in ConnectSpot, with the golden test restating it in its own call to
// bybitStream — so a connector that stopped applying it would have left every
// test green, which is how the 10-arg refusal would come back unnoticed
// (adversarial review of 2026-09-12 proved that by mutation). Tests build the
// stream through these two, not around them.
func futuresStream(source string, symbols []exchanges.Symbol, f exchanges.Feeds) exchanges.StreamConfig {
	// No documented args ceiling on the linear feed, so the topic list travels
	// whole: https://bybit-exchange.github.io/docs/v5/ws/connect
	return bybitStream(source, symbols, f, "wss://stream.bybit.com/v5/public/linear", true, 0)
}

func spotStream(source string, symbols []exchanges.Symbol, f exchanges.Feeds) exchanges.StreamConfig {
	return bybitStream(source, symbols, f, "wss://stream.bybit.com/v5/public/spot", false, bybitSpotMaxArgsPerRequest)
}

// bybitStream builds the config for one Bybit feed. withFunding is passed in
// rather than inferred from the URL: a testnet host, a regional mirror or one
// added query parameter would defeat a suffix test, and the failure would be
// silent — a healthy socket, live prices, and no funding at all.
func bybitStream(source string, symbols []exchanges.Symbol, f exchanges.Feeds, url string, withFunding bool, maxArgs int) exchanges.StreamConfig {
	tickers := make(map[string]*bybitTicker, len(symbols))

	// How many requests the subscription takes. Carried into the handler
	// because Bybit's refusal does NOT echo the request it refused: with the
	// topic list split into batches, one refused batch leaves the other batches
	// delivering, so the data clock never fires and the only evidence is the
	// log line. It must at least say how many requests were sent and how many
	// markets are at stake.
	requests := len(bybitArgBatches(bybitTopics(symbols, withFunding), maxArgs))

	return exchanges.StreamConfig{
		Source:    source,
		URL:       url,
		Subscribe: bybitSubscribe(symbols, tickers, withFunding, maxArgs),
		Ping:      bybitPing,
		PingEvery: bybitPingEvery,
		Handle: func(raw []byte, recvAt time.Time) bool {
			return handleBybitFrame(source, symbols, tickers, requests, f, raw, recvAt)
		},
	}
}

// handleBybitFrame reports whether the frame became a message on a feed — the
// contract exchanges.StreamConfig.Handle documents. On this venue false is the
// answer for the keepalive reply ({"op":"pong"}) AND for the subscribe
// acknowledgement, including the refusal {"success":false,"ret_msg":"args size
// >10"} that left bybit_spot subscribed to nothing for 19 hours on 2026-09-11.
func handleBybitFrame(source string, symbols []exchanges.Symbol, tickers map[string]*bybitTicker, requests int, f exchanges.Feeds, raw []byte, recvAt time.Time) bool {
	// Tried first: a ticker frame decodes as an orderbook too (both carry a
	// data object), and the orderbook branch below only rejects it because the
	// bids/asks arrays are empty — an accident, not a check.
	if handled, produced := handleBybitTicker(source, symbols, tickers, f, raw, recvAt); handled {
		return produced
	}

	// Try to parse as orderbook first
	var orderbookMsg BybitOrderbook
	if exchanges.Decode(raw, &orderbookMsg) &&
		len(orderbookMsg.Data.Asks) > 0 && len(orderbookMsg.Data.Bids) > 0 {

		bidPrice, err1 := strconv.ParseFloat(orderbookMsg.Data.Bids[0][0], 64)
		askPrice, err2 := strconv.ParseFloat(orderbookMsg.Data.Asks[0][0], 64)
		if err1 != nil || err2 != nil {
			return false
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

		standardSymbol := exchanges.StandardOf(symbols, orderbookMsg.Data.Symbol)
		if standardSymbol == "" {
			return false // a market this connector never subscribed to
		}

		return f.SendOrderbook(exchanges.OrderbookData{
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
	}

	// Try to parse as trade message
	var tradeMsg BybitTrade
	sent := false
	if exchanges.Decode(raw, &tradeMsg) && (tradeMsg.Type == "snapshot" || tradeMsg.Type == "delta") {
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

			standardSymbol := exchanges.StandardOf(symbols, trade.Symbol)
			if standardSymbol == "" {
				continue
			}

			if !f.SendTrade(exchanges.TradeData{
				Symbol:      standardSymbol,
				Source:      source,
				Price:       price,
				Quantity:    trade.Size,
				Side:        side,
				VenueTimeMs: trade.Timestamp,
				RecvAt:      recvAt,
			}) {
				return sent // shutting down; the rest of the batch is not worth parsing
			}
			sent = true
		}
	}
	if sent {
		return true
	}

	// Last, not first: a refused subscribe is the one frame that must never
	// pass unread, but it is also the rarest, and decoding it costs a whole
	// json.Unmarshal — encoding/json walks the entire document however few
	// fields the struct declares. handleKrakenFrame refuses a second
	// speculative pass for exactly that reason. Nothing above can claim an op
	// reply (it carries no topic, no data object, no bids and no asks), so
	// running the check only on frames nothing else wanted is identical in
	// behaviour and free on the hot path.
	//
	// The venue says exactly what is wrong and then delivers nothing forever.
	// The stream's data deadline will re-dial, but only this line says WHY, and
	// without it the next refusal costs another 19 hours to diagnose.
	if reply, ok := decodeBybitOpReply(raw); ok && reply.Op == "subscribe" && !reply.Success {
		log.Printf("%s: Bybit REFUSED a subscribe request: %q. The reply does not say WHICH of the %d request(s) "+
			"was refused, so any of this source's %d markets may be delivering nothing until the socket is "+
			"re-dialled — check the dashboard before trusting a quiet pair here",
			source, reply.RetMsg, requests, len(symbols))
	}
	return false
}
