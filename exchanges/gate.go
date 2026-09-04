package exchanges

import (
	"log"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

type GateFuturesTrade struct {
	Size         int64  `json:"size"`
	ID           int64  `json:"id"`
	CreateTime   int64  `json:"create_time"`
	CreateTimeMs int64  `json:"create_time_ms"`
	Price        string `json:"price"`
	Contract     string `json:"contract"`
	IsInternal   bool   `json:"is_internal,omitempty"`
}

type GateWebSocketMessage struct {
	Time    int64  `json:"time"`
	Channel string `json:"channel"`
	Event   string `json:"event"`
	Result  struct {
		Status string `json:"status"`
	} `json:"result,omitempty"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type GateBookTickerResult struct {
	Symbol      string `json:"s"` // Contract symbol
	BestBid     string `json:"b"` // Best bid price
	BestBidSize int64  `json:"B"` // Best bid size
	BestAsk     string `json:"a"` // Best ask price
	BestAskSize int64  `json:"A"` // Best ask size
	Timestamp   int64  `json:"t"` // Timestamp in milliseconds
}

type GateBookTickerMessage struct {
	Time    int64                `json:"time"`
	Channel string               `json:"channel"`
	Event   string               `json:"event"`
	Result  GateBookTickerResult `json:"result"`
}

type GateTradeMessage struct {
	Time    int64              `json:"time"`
	Channel string             `json:"channel"`
	Event   string             `json:"event"`
	Result  []GateFuturesTrade `json:"result"`
}

type GateFuturesOrderbook struct {
	Contract     string     `json:"contract"`
	Ask          [][]string `json:"asks"`
	Bid          [][]string `json:"bids"`
	UpdateTime   int64      `json:"update_time"`
	UpdateTimeMs int64      `json:"update_time_ms"`
	UpdateID     int64      `json:"update_id"`
}

type GateOrderbookMessage struct {
	Time    int64                  `json:"time"`
	Channel string                 `json:"channel"`
	Event   string                 `json:"event"`
	Result  []GateFuturesOrderbook `json:"result"`
}

type GateSubscribeMessage struct {
	Time    int64    `json:"time"`
	Channel string   `json:"channel"`
	Event   string   `json:"event"`
	Payload []string `json:"payload"`
}

// Gate is the one venue that documents the protocol-level mechanism as the
// preferred one: "the server will initiate a ping message actively. If the
// client does not reply, the client will be disconnected", and it recommends the
// WebSocket protocol layer ping/pong over its application-level `futures.ping`
// channel. https://www.gate.com/docs/developers/futures/ws/en/
//
// So Ping is left nil - runStream sends a protocol ping frame - and the ping
// handler there answers the server's own pings and counts them as activity.
func ConnectGateFutures(source string, symbols []Symbol, f Feeds) {
	runStream(f, gateStream(source, symbols, f))
}

func gateStream(source string, symbols []Symbol, f Feeds) streamConfig {
	return streamConfig{
		Source: source,
		URL:    "wss://fx-ws.gateio.ws/v4/ws/usdt",
		Subscribe: func(conn *websocket.Conn) error {
			// config.yaml supplies the venue identifiers (symbol_format
			// "{base}_{quote}"), so this connector no longer keeps its own table.
			//
			// futures.tickers is the funding source (step 2.5) and carries the
			// rate, the interval in SECONDS and the next settlement in epoch
			// SECONDS in one payload — the only venue here that needs no
			// second endpoint for any of the three.
			for _, channel := range []string{"futures.book_ticker", "futures.tickers"} {
				err := conn.WriteJSON(GateSubscribeMessage{
					Time:    time.Now().Unix(),
					Channel: channel,
					Event:   "subscribe",
					Payload: VenueSymbols(symbols),
				})
				if err != nil {
					return err
				}
			}
			return nil
		},
		Handle: func(raw []byte, recvAt time.Time) {
			handleGateFrame(source, symbols, f, raw, recvAt)
		},
	}
}

func handleGateFrame(source string, symbols []Symbol, f Feeds, raw []byte, recvAt time.Time) {
	// First, check for errors and subscription acknowledgements.
	var wsMsg GateWebSocketMessage
	if decode(raw, &wsMsg) {
		if wsMsg.Error != nil {
			log.Printf("%s: WebSocket error %d - %s", source, wsMsg.Error.Code, wsMsg.Error.Message)
			return
		}
		if wsMsg.Event == "subscribe" {
			return
		}
	}

	if handleGateFunding(source, symbols, f, raw, recvAt) {
		return
	}

	var bookTickerMsg GateBookTickerMessage
	if !decode(raw, &bookTickerMsg) ||
		bookTickerMsg.Channel != "futures.book_ticker" ||
		bookTickerMsg.Event != "update" {
		return // any other message type is ignored
	}

	bestBid, err1 := strconv.ParseFloat(bookTickerMsg.Result.BestBid, 64)
	bestAsk, err2 := strconv.ParseFloat(bookTickerMsg.Result.BestAsk, 64)
	if err1 != nil || err2 != nil {
		log.Printf("%s: error parsing prices - bid: %v, ask: %v", source, err1, err2)
		return
	}

	standardSymbol := StandardOf(symbols, bookTickerMsg.Result.Symbol)
	if standardSymbol == "" {
		return // a contract this connector never subscribed to
	}

	// A missing venue timestamp stays 0. Substituting the local clock would
	// report our own time as the venue's; RecvAt is our clock and says so.
	var venueTimeMs int64
	if bookTickerMsg.Result.Timestamp > 0 {
		venueTimeMs = bookTickerMsg.Result.Timestamp
	}

	// The size is a CONTRACT count. BestBidSize/BestAskSize are declared int64,
	// which cannot express a fractional coin amount at all, and measured
	// 2026-09-03 BTC_USDT published 10099 with BTC near $77.5k.
	//
	// Until step 2.7b it was dropped rather than published in a ...Coin field.
	// It now travels as contracts and the scanner multiplies by
	// quanto_multiplier from the instrument registry.
	f.SendOrderbook(OrderbookData{
		Symbol:              standardSymbol,
		Source:              source,
		BestBid:             bestBid,
		BestAsk:             bestAsk,
		BestBidQtyContracts: float64(bookTickerMsg.Result.BestBidSize),
		BestAskQtyContracts: float64(bookTickerMsg.Result.BestAskSize),
		VenueTimeMs:         venueTimeMs,
		RecvAt:              recvAt,
	})
}
