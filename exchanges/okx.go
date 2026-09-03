package exchanges

import (
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

type OKXFuturesTrade struct {
	Arg struct {
		Channel string `json:"channel"`
		InstID  string `json:"instId"`
	} `json:"arg"`
	Data []struct {
		InstID    string `json:"instId"`
		TradeID   string `json:"tradeId"`
		Price     string `json:"px"`
		Size      string `json:"sz"`
		Side      string `json:"side"`
		Timestamp string `json:"ts"`
	} `json:"data"`
}

type OKXFuturesOrderbook struct {
	Arg struct {
		Channel string `json:"channel"`
		InstID  string `json:"instId"`
	} `json:"arg"`
	Data []struct {
		InstID    string     `json:"instId"`
		Bids      [][]string `json:"bids"`
		Asks      [][]string `json:"asks"`
		Timestamp string     `json:"ts"`
	} `json:"data"`
}

type okxChannelArg struct {
	Channel string `json:"channel"`
	InstID  string `json:"instId"`
}

type OKXSubscribeMessage struct {
	Op   string          `json:"op"`
	Args []okxChannelArg `json:"args"`
}

// OKX has the shortest documented idle timeout of every venue here, and it does
// not accept a protocol ping frame for it: "The connection will break
// automatically if the subscription is not established or data has not been
// pushed for more than 30 seconds", and the remedy is to "send the String
// 'ping'" - a raw text frame, not JSON - and "expect a 'pong' as a response".
// https://www.okx.com/docs-v5/en/
const (
	okxPingEvery   = 15 * time.Second
	okxReadTimeout = 45 * time.Second
)

func ConnectOKXFutures(source string, symbols []Symbol, f Feeds) {
	runStream(f, streamConfig{
		Source: source,
		URL:    "wss://ws.okx.com:8443/ws/v5/public",
		Subscribe: func(conn *websocket.Conn) error {
			// config.yaml supplies the venue identifier (symbol_format
			// "{base}-{quote}-SWAP").
			args := make([]okxChannelArg, 0, len(symbols)*2)
			for _, symbol := range symbols {
				args = append(args,
					okxChannelArg{Channel: "trades", InstID: symbol.Venue},
					// books5 is the top 5 levels, pushed as a snapshot.
					okxChannelArg{Channel: "books5", InstID: symbol.Venue},
				)
			}
			return conn.WriteJSON(OKXSubscribeMessage{Op: "subscribe", Args: args})
		},
		Ping:        textPing("ping"),
		PingEvery:   okxPingEvery,
		ReadTimeout: okxReadTimeout,
		Handle: func(raw []byte, recvAt time.Time) {
			handleOKXFrame(source, symbols, f, raw, recvAt)
		},
	})
}

func handleOKXFrame(source string, symbols []Symbol, f Feeds, raw []byte, recvAt time.Time) {
	// The keepalive reply is the literal text "pong", which is not JSON and
	// falls through both decodes below.
	var tradeMsg OKXFuturesTrade
	if decode(raw, &tradeMsg) && tradeMsg.Arg.Channel == "trades" && len(tradeMsg.Data) > 0 {
		for _, trade := range tradeMsg.Data {
			price, err := strconv.ParseFloat(trade.Price, 64)
			if err != nil {
				continue
			}

			// An unparseable timestamp means the venue gave us nothing usable;
			// substituting the local clock would report our time as the venue's.
			timestamp, err := strconv.ParseInt(trade.Timestamp, 10, 64)
			if err != nil {
				timestamp = 0
			}

			standardSymbol := StandardOf(symbols, trade.InstID)
			if standardSymbol == "" {
				continue // an instrument this connector never subscribed to
			}

			if !f.SendTrade(TradeData{
				Symbol:      standardSymbol,
				Source:      source,
				Price:       price,
				Quantity:    trade.Size,
				Side:        trade.Side, // OKX already provides "buy" or "sell"
				VenueTimeMs: timestamp,
				RecvAt:      recvAt,
			}) {
				return
			}
		}
		return
	}

	var orderbookMsg OKXFuturesOrderbook
	if decode(raw, &orderbookMsg) && orderbookMsg.Arg.Channel == "books5" && len(orderbookMsg.Data) > 0 {
		for _, book := range orderbookMsg.Data {
			if len(book.Bids) == 0 || len(book.Asks) == 0 {
				continue
			}

			bestBid, err1 := strconv.ParseFloat(book.Bids[0][0], 64)
			bestAsk, err2 := strconv.ParseFloat(book.Asks[0][0], 64)
			if err1 != nil || err2 != nil {
				continue
			}

			timestamp, err := strconv.ParseInt(book.Timestamp, 10, 64)
			if err != nil {
				timestamp = 0
			}

			standardSymbol := StandardOf(symbols, book.InstID)
			if standardSymbol == "" {
				continue // an instrument this connector never subscribed to
			}

			// BestBidQtyCoin/BestAskQtyCoin are deliberately left at 0. A books5
			// level is [price, sz, liqOrders, numOrders] and sz is a CONTRACT
			// count, not coins: measured 2026-09-03, BTC-USDT-SWAP published
			// 1182.68 with BTC near $77.5k, which as coins would be a $91M top of
			// book, and XRP-USDT-SWAP published 334.52, which as coins would be
			// $456. Converting needs ctVal x ctMult per instrument, which arrives
			// with the instrument registry (internal/instruments, phase 2).
			// Publishing the raw number in a ...Coin field would be wrong without
			// looking wrong.
			if !f.SendOrderbook(OrderbookData{
				Symbol:      standardSymbol,
				Source:      source,
				BestBid:     bestBid,
				BestAsk:     bestAsk,
				VenueTimeMs: timestamp,
				RecvAt:      recvAt,
			}) {
				return
			}
		}
	}
}
