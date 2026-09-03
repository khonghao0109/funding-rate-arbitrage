package exchanges

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// BinanceAggTrade and BinanceBookTicker are the two payload shapes on the
// combined stream. Spot and futures publish them identically, so the separate
// BinanceSpot* copies of these structs - byte-for-byte duplicates - went away
// with the shared handler below.
type BinanceAggTrade struct {
	EventType string `json:"e"`
	EventTime int64  `json:"E"`
	Symbol    string `json:"s"`
	TradeID   int64  `json:"a"`
	Price     string `json:"p"`
	Quantity  string `json:"q"`
	TradeTime int64  `json:"T"`
	IsMaker   bool   `json:"m"`
}

type BinanceBookTicker struct {
	EventType    string `json:"e"`
	EventTime    int64  `json:"E"`
	Symbol       string `json:"s"`
	BestBidPrice string `json:"b"`
	BestBidQty   string `json:"B"`
	BestAskPrice string `json:"a"`
	BestAskQty   string `json:"A"`
}

// binanceEnvelope wraps every message on a combined stream.
type binanceEnvelope struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

// binanceStreamURL builds a combined-stream URL: one socket carrying the book
// ticker and the aggregate trade feed for every subscribed symbol.
func binanceStreamURL(host string, symbols []Symbol) string {
	streamNames := make([]string, len(symbols)*2)
	for i, symbol := range symbols {
		streamNames[i*2] = strings.ToLower(symbol.Venue) + "@bookTicker"
		streamNames[i*2+1] = strings.ToLower(symbol.Venue) + "@aggTrade"
	}
	return fmt.Sprintf("%s/stream?streams=%s", host, strings.Join(streamNames, "/"))
}

// Binance's keepalive rules could not be read from this environment: the
// WebSocket pages reachable here document the streams but not the connection
// rules, and the general-information section renders client-side. Rather than
// write an application-level heartbeat from memory (CLAUDE.md rule 5), this
// leaves Ping nil so runStream sends a protocol-level ping frame (RFC 6455) -
// measured 2026-09-03 against wss://fstream.binance.com, the pong comes back.
// Binance also pings us, and runSession's handler answers it and counts it as
// activity.
func ConnectBinanceFutures(source string, symbols []Symbol, f Feeds) {
	runStream(f, streamConfig{
		Source: source,
		URL:    binanceStreamURL("wss://fstream.binance.com", symbols),
		Handle: func(raw []byte, recvAt time.Time) {
			handleBinanceFrame(source, symbols, f, raw, recvAt)
		},
	})
}

// ConnectBinanceSpot connects to Binance spot trading WebSocket API.
func ConnectBinanceSpot(source string, symbols []Symbol, f Feeds) {
	runStream(f, streamConfig{
		Source: source,
		URL:    binanceStreamURL("wss://stream.binance.com:9443", symbols),
		Handle: func(raw []byte, recvAt time.Time) {
			handleBinanceFrame(source, symbols, f, raw, recvAt)
		},
	})
}

// handleBinanceFrame parses one combined-stream message.
//
// Spot and futures publish the same two payload shapes on the same envelope, so
// they share this. The two connectors stay separate because their URLs, their
// venues and their fee schedules are different things that happen to speak the
// same dialect today.
func handleBinanceFrame(source string, symbols []Symbol, f Feeds, raw []byte, recvAt time.Time) {
	var message binanceEnvelope
	if !decode(raw, &message) {
		return
	}

	switch {
	case strings.Contains(message.Stream, "@bookTicker"):
		var bookTicker BinanceBookTicker
		if !decode(message.Data, &bookTicker) {
			return
		}

		bidPrice, err1 := strconv.ParseFloat(bookTicker.BestBidPrice, 64)
		askPrice, err2 := strconv.ParseFloat(bookTicker.BestAskPrice, 64)
		if err1 != nil || err2 != nil {
			return
		}

		// A quantity that will not parse must not discard a good price: the book
		// size is a liquidity filter, the price is the measurement. Unit is base
		// coin - see OrderbookData.
		bidQtyCoin, _ := strconv.ParseFloat(bookTicker.BestBidQty, 64)
		askQtyCoin, _ := strconv.ParseFloat(bookTicker.BestAskQty, 64)

		standardSymbol := StandardOf(symbols, bookTicker.Symbol)
		if standardSymbol == "" {
			return // a market this connector never subscribed to
		}

		f.SendOrderbook(OrderbookData{
			Symbol:         standardSymbol,
			Source:         source,
			BestBid:        bidPrice,
			BestAsk:        askPrice,
			VenueTimeMs:    bookTicker.EventTime,
			RecvAt:         recvAt,
			BestBidQtyCoin: bidQtyCoin,
			BestAskQtyCoin: askQtyCoin,
		})

	case strings.Contains(message.Stream, "@aggTrade"):
		var trade BinanceAggTrade
		if !decode(message.Data, &trade) {
			return
		}

		price, err := strconv.ParseFloat(trade.Price, 64)
		if err != nil {
			return
		}

		// Normalize trade side (isMaker: false = buy aggressor, true = sell aggressor)
		side := "sell"
		if !trade.IsMaker {
			side = "buy"
		}

		standardSymbol := StandardOf(symbols, trade.Symbol)
		if standardSymbol == "" {
			return
		}

		f.SendTrade(TradeData{
			Symbol:      standardSymbol,
			Source:      source,
			Price:       price,
			Quantity:    trade.Quantity,
			Side:        side,
			VenueTimeMs: trade.TradeTime,
			RecvAt:      recvAt,
		})
	}
}
