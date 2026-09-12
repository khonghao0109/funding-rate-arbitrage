package paradex

import (
	"futures-arbitrage-scanner/exchanges"

	"strconv"
	"time"
)

type ParadexWSRequest struct {
	ID      int64          `json:"id"`
	JSONRPC string         `json:"jsonrpc"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

type ParadexWSResponse struct {
	ID      int64  `json:"id"`
	JSONRPC string `json:"jsonrpc"`
	Result  struct {
		Channel string `json:"channel"`
		Status  string `json:"status"`
	} `json:"result,omitempty"`
}

type ParadexTradeEvent struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		Channel string `json:"channel"`
		Data    struct {
			ID        string `json:"id"`
			Market    string `json:"market"`
			Price     string `json:"price"`
			Size      string `json:"size"`
			Side      string `json:"side"`
			CreatedAt int64  `json:"created_at"`
			TradeType string `json:"trade_type"`
		} `json:"data"`
	} `json:"params"`
}

type ParadexMarketSummaryEvent struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		Channel string `json:"channel"`
		Data    struct {
			Symbol string `json:"symbol"`
			Bid    string `json:"bid"`
			Ask    string `json:"ask"`
		} `json:"data"`
	} `json:"params"`
}

// Paradex is the only venue here that keeps the connection alive from its own
// end: "The server sends a ping message every 55 seconds" and "the client must
// respond with a pong within 5 seconds", after which "the connection is renewed
// for 60 seconds". https://docs.paradex.trade/ws/general-information/introduction
//
// That reply is what runSession's ping handler does - and, critically, why that
// handler also extends the read deadline: gorilla consumes control frames inside
// ReadMessage without returning, so a socket kept alive purely by server pings
// would look silent to a read deadline that only data resets.
func ConnectFutures(source string, symbols []exchanges.Symbol, f exchanges.Feeds) {
	exchanges.RunStream(f, paradexStream(source, symbols, f))
}

func paradexStream(source string, symbols []exchanges.Symbol, f exchanges.Feeds) exchanges.StreamConfig {
	return exchanges.StreamConfig{
		Source: source,
		URL:    "wss://ws.api.prod.paradex.trade/v1",
		Subscribe: func(conn exchanges.Subscriber) error {
			// markets_summary carries bid and ask for every market at once, so
			// one subscription covers whatever symbols are configured.
			//
			// The error from this write used to be discarded by an empty if
			// body, which left a connected socket subscribed to nothing and
			// looking healthy.
			err := conn.WriteJSON(ParadexWSRequest{
				ID:      1,
				JSONRPC: "2.0",
				Method:  "subscribe",
				Params:  map[string]any{"channel": "markets_summary"},
			})
			if err != nil {
				return err
			}
			// funding_data is per market, so unlike markets_summary it needs
			// one subscription each. It is the only Paradex channel that
			// states the quote window its rate belongs to (step 2.5).
			for i, symbol := range symbols {
				err := conn.WriteJSON(ParadexWSRequest{
					ID:      int64(i + 2),
					JSONRPC: "2.0",
					Method:  "subscribe",
					Params:  map[string]any{"channel": paradexFundingChannelPrefix + symbol.Venue},
				})
				if err != nil {
					return err
				}
			}
			return nil
		},
		Handle: func(raw []byte, recvAt time.Time) bool {
			return handleParadexFrame(source, symbols, f, raw, recvAt)
		},
	}
}

// handleParadexFrame reports whether the frame became a message on a feed — the
// contract exchanges.StreamConfig.Handle documents. False is the answer for
// the keepalive reply and for a subscribe acknowledgement or refusal, which
// is what lets the lifecycle tell a live subscription from a socket that is
// merely open (docs/PLAN.md step 1.6).
func handleParadexFrame(source string, symbols []exchanges.Symbol, f exchanges.Feeds, raw []byte, recvAt time.Time) bool {
	if handled, produced := handleParadexFunding(source, symbols, f, raw, recvAt); handled {
		return produced
	}

	// The subscription acknowledgement shares the envelope with the data.
	var subResponse ParadexWSResponse
	if exchanges.Decode(raw, &subResponse) && subResponse.Result.Channel == "markets_summary" {
		return false
	}

	var marketEvent ParadexMarketSummaryEvent
	if !exchanges.Decode(raw, &marketEvent) ||
		marketEvent.Method != "subscription" ||
		marketEvent.Params.Channel != "markets_summary" {
		return false
	}

	symbol := exchanges.StandardOf(symbols, marketEvent.Params.Data.Symbol)
	if symbol == "" {
		return false // a market this connector never subscribed to
	}

	bidPrice, err1 := strconv.ParseFloat(marketEvent.Params.Data.Bid, 64)
	askPrice, err2 := strconv.ParseFloat(marketEvent.Params.Data.Ask, 64)
	if err1 != nil || err2 != nil {
		return false
	}

	// BestBidQtyCoin/BestAskQtyCoin stay 0: markets_summary publishes a bid and
	// an ask price and no size at all, so there is nothing to collect here. A
	// depth channel would be needed, and phase 2 takes book depth over REST
	// instead - see docs/PLAN.md §7.4.
	return f.SendOrderbook(exchanges.OrderbookData{
		Symbol:  symbol,
		Source:  source,
		BestBid: bidPrice,
		BestAsk: askPrice,
		// No venue timestamp in this message. Writing the local clock here would
		// report our own time as the venue's; RecvAt is our clock and says so.
		VenueTimeMs: 0,
		RecvAt:      recvAt,
	})
}
