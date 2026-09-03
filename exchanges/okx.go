package exchanges

import (
	"encoding/json"
	"log"
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

type OKXSubscribeMessage struct {
	Op   string `json:"op"`
	Args []struct {
		Channel string `json:"channel"`
		InstID  string `json:"instId"`
	} `json:"args"`
}

func ConnectOKXFutures(source string, symbols []Symbol, priceChan chan<- PriceData, orderbookChan chan<- OrderbookData, tradeChan chan<- TradeData) {
	wsURL := "wss://ws.okx.com:8443/ws/v5/public"

	for {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			log.Printf("OKX connection error: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		log.Printf("Connected to OKX futures WebSocket")

		// Subscribe to both trades and orderbooks for all symbols
		var subscribeArgs []struct {
			Channel string `json:"channel"`
			InstID  string `json:"instId"`
		}

		for _, symbol := range symbols {
			// config.yaml supplies the venue identifier (symbol_format
			// "{base}-{quote}-SWAP").
			okxSymbol := symbol.Venue

			// Subscribe to trades
			subscribeArgs = append(subscribeArgs, struct {
				Channel string `json:"channel"`
				InstID  string `json:"instId"`
			}{
				Channel: "trades",
				InstID:  okxSymbol,
			})

			// Subscribe to orderbooks (books5 for top 5 levels)
			subscribeArgs = append(subscribeArgs, struct {
				Channel string `json:"channel"`
				InstID  string `json:"instId"`
			}{
				Channel: "books5",
				InstID:  okxSymbol,
			})
		}

		subscribeMsg := OKXSubscribeMessage{
			Op:   "subscribe",
			Args: subscribeArgs,
		}

		err = conn.WriteJSON(subscribeMsg)
		if err != nil {
			log.Printf("OKX subscription error: %v", err)
			conn.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		for {
			var message json.RawMessage
			err := conn.ReadJSON(&message)
			if err != nil {
				log.Printf("OKX read error: %v", err)
				conn.Close()
				break
			}

			// Check if it's a trade message
			var tradeMsg OKXFuturesTrade
			if err := json.Unmarshal(message, &tradeMsg); err == nil && tradeMsg.Arg.Channel == "trades" && len(tradeMsg.Data) > 0 {
				for _, trade := range tradeMsg.Data {
					price, err := strconv.ParseFloat(trade.Price, 64)
					if err != nil {
						continue
					}

					// Convert timestamp from string to int64. An unparseable
					// value means the venue gave us nothing usable; substituting
					// the local clock would report our time as the venue's.
					timestamp, err := strconv.ParseInt(trade.Timestamp, 10, 64)
					if err != nil {
						timestamp = 0
					}

					standardSymbol := StandardOf(symbols, trade.InstID)
					if standardSymbol == "" {
						continue // an instrument this connector never subscribed to
					}

					tradeData := TradeData{
						Symbol:      standardSymbol,
						Source:      source,
						Price:       price,
						Quantity:    trade.Size,
						Side:        trade.Side, // OKX already provides "buy" or "sell"
						VenueTimeMs: timestamp,
					}

					tradeChan <- tradeData
				}
				continue
			}

			// Check if it's an orderbook message
			var orderbookMsg OKXFuturesOrderbook
			if err := json.Unmarshal(message, &orderbookMsg); err == nil && orderbookMsg.Arg.Channel == "books5" && len(orderbookMsg.Data) > 0 {
				for _, book := range orderbookMsg.Data {
					if len(book.Bids) == 0 || len(book.Asks) == 0 {
						continue
					}

					// Parse best bid and ask
					bestBid, err1 := strconv.ParseFloat(book.Bids[0][0], 64)
					bestAsk, err2 := strconv.ParseFloat(book.Asks[0][0], 64)
					if err1 != nil || err2 != nil {
						continue
					}

					// Convert timestamp from string to int64. An unparseable
					// value means the venue gave us nothing usable; substituting
					// the local clock would report our time as the venue's.
					timestamp, err := strconv.ParseInt(book.Timestamp, 10, 64)
					if err != nil {
						timestamp = 0
					}

					standardSymbol := StandardOf(symbols, book.InstID)
					if standardSymbol == "" {
						continue // an instrument this connector never subscribed to
					}

					// BestBidQtyCoin/BestAskQtyCoin are deliberately left at 0.
					// A books5 level is [price, sz, liqOrders, numOrders] and sz
					// is a CONTRACT count, not coins: measured 2026-09-03,
					// BTC-USDT-SWAP published 1182.68 with BTC near $77.5k, which
					// as coins would be a $91M top of book, and XRP-USDT-SWAP
					// published 334.52, which as coins would be $456. Converting
					// needs ctVal x ctMult per instrument, which arrives with the
					// instrument registry (internal/instruments, phase 2).
					// Publishing the raw number in a ...Coin field would be wrong
					// without looking wrong.
					orderbookData := OrderbookData{
						Symbol:      standardSymbol,
						Source:      source,
						BestBid:     bestBid,
						BestAsk:     bestAsk,
						VenueTimeMs: timestamp,
					}

					orderbookChan <- orderbookData
				}
				continue
			}
		}

		time.Sleep(2 * time.Second)
	}
}
