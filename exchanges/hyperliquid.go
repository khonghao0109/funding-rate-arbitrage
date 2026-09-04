package exchanges

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type HyperliquidTrade struct {
	Channel string          `json:"channel"`
	Data    json.RawMessage `json:"data"`
}

type HyperliquidTradeData struct {
	Coin      string `json:"coin"`
	Price     string `json:"px"`
	Size      string `json:"sz"`
	Side      string `json:"side"`
	Timestamp int64  `json:"time"`
}

type HyperliquidL2Book struct {
	Channel string          `json:"channel"`
	Data    json.RawMessage `json:"data"`
}

type HyperliquidLevel struct {
	Price string `json:"px"`
	Size  string `json:"sz"`
	Count int    `json:"n"`
}

type HyperliquidL2BookData struct {
	Coin   string               `json:"coin"`
	Levels [][]HyperliquidLevel `json:"levels"`
	Time   int64                `json:"time"`
}

// Hyperliquid closes a connection it has not spoken on: "The server will close
// any connection if it hasn't sent a message to it in the last 60 seconds", and
// the documented heartbeat is `{ "method": "ping" }`.
// https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/websocket/timeouts-and-heartbeats
//
// A third of the documented window is used, so two lost pings in a row are still
// survivable.
const hyperliquidPingEvery = 20 * time.Second

// ConnectHyperliquidFutures runs the market-data socket and, beside it, the
// REST refresher for the funding cadence and settlement stamp the WebSocket
// payload does not carry (step 2.5).
//
// A REST failure costs funding readings only: the socket, and with it every
// price, keeps running.
func ConnectHyperliquidFutures(source string, symbols []Symbol, f Feeds) {
	meta := newFundingMetaCache()

	var running sync.WaitGroup
	running.Add(1)
	go func() {
		defer running.Done()
		pollFunding(f.Ctx, source, "predictedFundings", fundingMetaEvery, func(ctx context.Context) error {
			return refreshHyperliquidFundingMeta(ctx, meta, symbols)
		})
	}()

	runStream(f, hyperliquidStream(source, symbols, f, meta))
	running.Wait()
}

func hyperliquidStream(source string, symbols []Symbol, f Feeds, meta *fundingMetaCache) streamConfig {
	return streamConfig{
		Source: source,
		URL:    "wss://api.hyperliquid.xyz/ws",
		Subscribe: func(conn *websocket.Conn) error {
			for _, symbol := range symbols {
				// The venue identifier is the coin name, built by config.yaml's
				// symbol_format. It used to be symbol[:3], which worked only
				// because every configured base happened to be three characters
				// long and would have subscribed to "DOG" for DOGEUSDT.
				for _, feed := range []string{"trades", "l2Book", "activeAssetCtx"} {
					err := conn.WriteJSON(map[string]any{
						"method": "subscribe",
						"subscription": map[string]any{
							"type": feed,
							"coin": symbol.Venue,
						},
					})
					if err != nil {
						return err
					}
				}
			}
			return nil
		},
		Ping:      jsonPing(map[string]string{"method": "ping"}),
		PingEvery: hyperliquidPingEvery,
		Handle: func(raw []byte, recvAt time.Time) {
			handleHyperliquidFrame(source, symbols, meta, f, raw, recvAt)
		},
	}
}

func handleHyperliquidFrame(source string, symbols []Symbol, meta *fundingMetaCache, f Feeds, raw []byte, recvAt time.Time) {
	if handleHyperliquidFunding(source, symbols, meta, f, raw, recvAt) {
		return
	}

	var tradeMessage HyperliquidTrade
	if decode(raw, &tradeMessage) && tradeMessage.Channel == "trades" && len(tradeMessage.Data) > 0 {
		// Handle both array and single object formats
		var trades []HyperliquidTradeData
		if err := json.Unmarshal(tradeMessage.Data, &trades); err != nil {
			var singleTrade HyperliquidTradeData
			if err := json.Unmarshal(tradeMessage.Data, &singleTrade); err != nil {
				log.Printf("%s: trade data parse error: %v", source, err)
				return
			}
			trades = []HyperliquidTradeData{singleTrade}
		}

		for _, trade := range trades {
			price, err := strconv.ParseFloat(trade.Price, 64)
			if err != nil {
				continue
			}

			symbol := StandardOf(symbols, trade.Coin)
			if symbol == "" {
				continue // a coin this connector never subscribed to
			}

			// Normalize trade side (Hyperliquid uses "A" for ask/sell, "B" for bid/buy)
			side := "sell"
			if trade.Side == "B" {
				side = "buy"
			}

			if !f.SendTrade(TradeData{
				Symbol:      symbol,
				Source:      source,
				Price:       price,
				Quantity:    trade.Size,
				Side:        side,
				VenueTimeMs: trade.Timestamp,
				RecvAt:      recvAt,
			}) {
				return
			}
		}
		return
	}

	var l2BookMessage HyperliquidL2Book
	if !decode(raw, &l2BookMessage) || l2BookMessage.Channel != "l2Book" || len(l2BookMessage.Data) == 0 {
		return
	}

	var l2BookData HyperliquidL2BookData
	if err := json.Unmarshal(l2BookMessage.Data, &l2BookData); err != nil {
		log.Printf("%s: l2Book data parse error: %v", source, err)
		return
	}

	// levels[0] is bids, levels[1] is asks; each level has px, sz and n.
	if len(l2BookData.Levels) < 2 || len(l2BookData.Levels[0]) == 0 || len(l2BookData.Levels[1]) == 0 {
		return
	}

	bestBid, err1 := strconv.ParseFloat(l2BookData.Levels[0][0].Price, 64)
	bestAsk, err2 := strconv.ParseFloat(l2BookData.Levels[1][0].Price, 64)
	if err1 != nil || err2 != nil {
		return
	}

	symbol := StandardOf(symbols, l2BookData.Coin)
	if symbol == "" {
		return // a coin this connector never subscribed to
	}

	// sz was decoded into HyperliquidLevel and dropped before step 1.2. A size
	// that will not parse leaves 0 ("not known") rather than discarding a good
	// price. Unit is base coin, see OrderbookData.
	bidQtyCoin, _ := strconv.ParseFloat(l2BookData.Levels[0][0].Size, 64)
	askQtyCoin, _ := strconv.ParseFloat(l2BookData.Levels[1][0].Size, 64)

	f.SendOrderbook(OrderbookData{
		Symbol:         symbol,
		Source:         source,
		BestBid:        bestBid,
		BestAsk:        bestAsk,
		VenueTimeMs:    l2BookData.Time,
		RecvAt:         recvAt,
		BestBidQtyCoin: bidQtyCoin,
		BestAskQtyCoin: askQtyCoin,
	})
}
