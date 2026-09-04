package exchanges

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
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

	// Ignore is Binance's deprecated "M" field, and it is declared for one
	// reason: encoding/json prefers an exact tag match but FALLS BACK to a
	// case-insensitive one. With only `m` declared, the payload's "M" - which is
	// always true - found no exact match, matched `m` case-insensitively and
	// overwrote IsMaker after it had already been read correctly. Every Binance
	// trade came out with IsMaker true, so every one was labelled a sell.
	//
	// Nothing consumes the side yet, which is why nobody noticed; the strategy
	// and backtest work in phases 2 and 3 would have. Found by step 1.6's golden
	// test, against a recording holding both m:true and m:false.
	//
	// Every other case-colliding pair across all nine venues - b/B, a/A, e/E,
	// s/S - already declares both members, so an exact match wins there and this
	// was the only field exposed to the fallback.
	Ignore bool `json:"M"`
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
// ConnectBinanceFutures runs the market-data socket and, beside it, the REST
// funding poller (step 2.5).
//
// The two are independent on purpose: funding comes from REST because the
// mark-price stream delivers nothing here (measured — see
// binance_funding_rest.go), and a REST outage must cost funding readings only,
// never the price feed. Both stop when the context does, and this returns when
// both have.
func ConnectBinanceFutures(source string, symbols []Symbol, f Feeds) {
	var running sync.WaitGroup
	running.Add(1)
	go func() {
		defer running.Done()
		runBinanceFunding(f, source, symbols)
	}()

	runStream(f, binanceStream(source, symbols, f, "wss://fstream.binance.com"))
	running.Wait()
}

// ConnectBinanceSpot connects to Binance spot trading WebSocket API.
func ConnectBinanceSpot(source string, symbols []Symbol, f Feeds) {
	runStream(f, binanceStream(source, symbols, f, "wss://stream.binance.com:9443"))
}

func binanceStream(source string, symbols []Symbol, f Feeds, host string) streamConfig {
	return streamConfig{
		Source: source,
		URL:    binanceStreamURL(host, symbols),
		Handle: func(raw []byte, recvAt time.Time) {
			handleBinanceFrame(source, symbols, f, raw, recvAt)
		},
	}
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
