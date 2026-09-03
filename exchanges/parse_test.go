package exchanges

import (
	"context"
	"math"
	"testing"
	"time"
)

// Golden tests: every payload here was recorded from the live venue by
// TestCaptureTestdata, and every frame is replayed through the venue's REAL
// handler - the one production wires into runStream, taken from the same
// streamConfig. A test that reimplemented the dispatch would pass while the
// connector was broken.
//
// What these assert is not "it parses". It is the set of claims the rest of the
// system relies on and cannot check for itself: the symbol on the wire is one we
// asked for, the source name is configuration rather than a literal, the receive
// stamp survives, a venue clock is never our clock, and a quantity is in coin or
// is zero - never a contract count wearing a ...Coin name.

// recorder collects everything a connector publishes.
type recorder struct {
	feeds Feeds

	priceChan     chan PriceData
	orderbookChan chan OrderbookData
	tradeChan     chan TradeData
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	r := &recorder{
		// Large enough that no send in a replay can block: a blocked send would
		// hang the test rather than fail it.
		priceChan:     make(chan PriceData, 8192),
		orderbookChan: make(chan OrderbookData, 8192),
		tradeChan:     make(chan TradeData, 8192),
	}
	r.feeds = Feeds{Ctx: ctx, Price: r.priceChan, Orderbook: r.orderbookChan, Trade: r.tradeChan}
	return r
}

func (r *recorder) orderbooks() []OrderbookData {
	var out []OrderbookData
	for {
		select {
		case data := <-r.orderbookChan:
			out = append(out, data)
		default:
			return out
		}
	}
}

func (r *recorder) trades() []TradeData {
	var out []TradeData
	for {
		select {
		case data := <-r.tradeChan:
			out = append(out, data)
		default:
			return out
		}
	}
}

func (r *recorder) prices() []PriceData {
	var out []PriceData
	for {
		select {
		case data := <-r.priceChan:
			out = append(out, data)
		default:
			return out
		}
	}
}

// replay pushes every frame of a golden file through the venue's production
// handler, in the order the venue sent them. Order matters for Kraken, whose top
// of book is the result of every frame that came before.
func replay(t *testing.T, source string) (*recorder, time.Time) {
	t.Helper()

	r := newRecorder(t)
	handle := captureStreams(r.feeds)[source].Handle
	if handle == nil {
		t.Fatalf("no stream config for %s", source)
	}

	recvAt := time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC)
	for _, frame := range readFrames(t, source) {
		handle(frame, recvAt)
	}
	return r, recvAt
}

// venueExpectation records what each venue's payloads are known to contain, and
// is the table the shared assertions run against.
type venueExpectation struct {
	// QuantityInCoin says the venue publishes a top-of-book size this connector
	// can put in a ...Coin field. False means the venue denominates its book in
	// CONTRACTS (OKX, Gate, Kraken) or publishes no size at all (Paradex), and
	// the connector must leave the field at 0 - "not known", never "no
	// liquidity". Converting needs the instrument registry from phase 2.
	QuantityInCoin bool
	// VenueClock says the payload this connector reads carries the venue's own
	// timestamp. False means it must stay 0: filling it with our clock would
	// report our time as theirs and make a dead feed look current forever
	// (CLAUDE.md rule 13).
	VenueClock bool
	// Trades says the RECORDING contains trades, which is a weaker claim than
	// "the venue has a trade feed" and is deliberately the one asserted: it can
	// be checked. binance_futures subscribes to aggTrade and delivered none in
	// the recording window, and a follow-up probe could not settle why - the
	// futures endpoint stopped delivering anything at all to this host, which
	// looks like connection-rate limiting after the capture run. Its trade
	// parsing is covered by binance_spot, which shares the handler.
	Trades bool
}

var venueExpectations = map[string]venueExpectation{
	// Recorded 2026-09-03: the futures bookTicker carries "E", the SPOT one
	// carries no event time at all. Same connector, same stream name, different
	// payload - so venue_time_ms is genuinely 0 for Binance spot books, and that
	// is the venue's doing rather than the connector's.
	"binance_futures":     {QuantityInCoin: true, VenueClock: true, Trades: false},
	"binance_spot":        {QuantityInCoin: true, VenueClock: false, Trades: true},
	"bybit_futures":       {QuantityInCoin: true, VenueClock: false, Trades: true},
	"bybit_spot":          {QuantityInCoin: true, VenueClock: false, Trades: true},
	"hyperliquid_futures": {QuantityInCoin: true, VenueClock: true, Trades: true},
	"okx_futures":         {QuantityInCoin: false, VenueClock: true, Trades: true},
	"gate_futures":        {QuantityInCoin: false, VenueClock: true, Trades: false},
	"kraken_futures":      {QuantityInCoin: false, VenueClock: false, Trades: false},
	"paradex_futures":     {QuantityInCoin: false, VenueClock: false, Trades: false},
}

// Every venue is replayed through the same assertions. These are the claims the
// scanner makes on a connector's behalf and cannot verify itself.
func TestGoldenPayloads_EveryVenueHonoursTheDataContract(t *testing.T) {
	for source, expect := range venueExpectations {
		t.Run(source, func(t *testing.T) {
			r, recvAt := replay(t, source)
			books := r.orderbooks()
			trades := r.trades()

			if len(books) == 0 {
				t.Fatalf("%s produced no top of book from its recording", source)
			}

			subscribed := map[string]bool{}
			for _, symbol := range captureSymbols[source] {
				subscribed[symbol.Standard] = true
			}

			var sawCoinQuantity bool
			for i, book := range books {
				// A market we never asked for must never reach the scanner. This
				// is not theoretical: Paradex publishes a summary for every
				// market it lists, options included.
				if !subscribed[book.Symbol] {
					t.Fatalf("book %d is for %q, which was never subscribed", i, book.Symbol)
				}
				// The source name is configuration. Hardcoding it inside a
				// connector is what made two config entries sharing a connector
				// report under one name at step 1.4.
				if book.Source != source {
					t.Errorf("book %d reports source %q, want %q", i, book.Source, source)
				}
				if !book.RecvAt.Equal(recvAt) {
					t.Errorf("book %d lost the receive stamp: %s", i, book.RecvAt)
				}
				if !isUsable(book.BestBid) || !isUsable(book.BestAsk) {
					t.Errorf("book %d has unusable prices %g/%g", i, book.BestBid, book.BestAsk)
				}
				if book.BestBid > book.BestAsk {
					t.Errorf("book %d is crossed: bid %g above ask %g", i, book.BestBid, book.BestAsk)
				}

				if expect.VenueClock {
					if book.VenueTimeMs <= 0 {
						t.Errorf("book %d has no venue timestamp, but this venue publishes one", i)
					}
				} else if book.VenueTimeMs != 0 {
					t.Errorf("book %d carries venue_time_ms %d from a venue that publishes none; only our own clock could have produced it",
						i, book.VenueTimeMs)
				}

				switch {
				case !expect.QuantityInCoin:
					if book.BestBidQtyCoin != 0 || book.BestAskQtyCoin != 0 {
						t.Errorf("book %d published %g/%g in a ...Coin field, but this venue denominates in contracts or publishes no size",
							i, book.BestBidQtyCoin, book.BestAskQtyCoin)
					}
				case book.BestBidQtyCoin > 0 && book.BestAskQtyCoin > 0:
					sawCoinQuantity = true
				}
			}

			if expect.QuantityInCoin && !sawCoinQuantity {
				t.Errorf("%s publishes book sizes in coin, but not one was collected", source)
			}

			if expect.Trades && len(trades) == 0 {
				t.Errorf("%s subscribes to a trade feed but the recording produced none", source)
			}
			for i, trade := range trades {
				if !subscribed[trade.Symbol] {
					t.Fatalf("trade %d is for %q, which was never subscribed", i, trade.Symbol)
				}
				if trade.Source != source {
					t.Errorf("trade %d reports source %q, want %q", i, trade.Source, source)
				}
				if !trade.RecvAt.Equal(recvAt) {
					t.Errorf("trade %d lost the receive stamp", i)
				}
				// The scanner and everything downstream branch on exactly these
				// two spellings. A venue's own casing reaching them is a silent
				// mis-classification.
				if trade.Side != "buy" && trade.Side != "sell" {
					t.Errorf("trade %d has side %q, want the normalized buy or sell", i, trade.Side)
				}
				if !isUsable(trade.Price) {
					t.Errorf("trade %d has unusable price %g", i, trade.Price)
				}
			}
		})
	}
}

func isUsable(value float64) bool {
	return value > 0 && !math.IsInf(value, 0) && !math.IsNaN(value)
}
