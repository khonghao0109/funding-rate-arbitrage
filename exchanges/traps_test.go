package exchanges

import (
	"bytes"
	"testing"
	"time"
)

// The traps each venue sets, one test each. These are the differences that look
// like they should not exist - the reason docs/DATA-REQUIREMENTS.md §3 has a
// table and CLAUDE.md has a rule against normalising them away.

// Binance encodes the trade side as "was the buyer the maker", which is the
// opposite polarity to the aggressor everything downstream reasons about.
// Getting it backwards does not fail: it reports every buy as a sell.
// It also carries a deprecated "M" field, and Go's JSON decoder prefers an exact
// tag match but falls back to a case-insensitive one - so before step 1.6 the
// always-true "M" overwrote the "m" that had just been read, and EVERY Binance
// trade came out a sell. See BinanceAggTrade.Ignore.
//
// Each frame is asserted against its own maker flag rather than by counting, so
// a polarity flip - which counting cannot see - fails this too.
func TestBinance_MakerFlagIsInvertedIntoTheAggressorSide(t *testing.T) {
	var maker, taker int

	for _, frame := range readFrames(t, "binance_spot") {
		if !bytes.Contains(frame, []byte("@aggTrade")) {
			continue
		}

		// Binance's m is "was the BUYER the maker", so a true means the
		// aggressor was selling into a resting bid.
		want := "buy"
		switch {
		case bytes.Contains(frame, []byte(`"m":true`)):
			want = "sell"
			maker++
		case bytes.Contains(frame, []byte(`"m":false`)):
			taker++
		default:
			t.Fatalf("aggTrade frame with no maker flag: %s", frame)
		}

		r := newRecorder(t)
		captureStreams(r.feeds)["binance_spot"].Handle(frame, time.Now())

		trades := r.trades()
		if len(trades) != 1 {
			t.Fatalf("one aggTrade frame produced %d trades", len(trades))
		}
		if trades[0].Side != want {
			t.Errorf("side = %q, want %q for %s", trades[0].Side, want, frame)
		}
	}

	// Without both polarities in the recording this asserts only half the
	// mapping, and the bug it exists for was invisible to exactly that half.
	if maker == 0 || taker == 0 {
		t.Errorf("recording has %d m:true and %d m:false frames; both are needed to pin the mapping",
			maker, taker)
	}
}

// Kraken is the only venue whose top of book is not in the message. A snapshot
// arrives once and every later frame moves ONE level, so the connector's output
// is a function of every frame that came before - replaying them out of order,
// or dropping one, silently yields a book that never existed.
func TestKraken_TopOfBookIsAssembledFromTheDeltaStream(t *testing.T) {
	r, _ := replay(t, "kraken_futures")

	books := r.orderbooks()
	if len(books) < 2 {
		t.Fatalf("got %d books from the delta stream, want the snapshot plus updates", len(books))
	}

	// Every intermediate state has to be a valid book, not just the last one:
	// each is broadcast as it is produced.
	for i, book := range books {
		if book.BestBid >= book.BestAsk {
			t.Fatalf("book %d is crossed after a delta: bid %g, ask %g", i, book.BestBid, book.BestAsk)
		}
	}

	// And the deltas have to actually move the price, or this is asserting on one
	// snapshot republished.
	//
	// PER SYMBOL. Comparing across symbols is what made the first version of this
	// test vacuous: the recording covers two products, so "the top of book
	// changed" was satisfied by BTC and ETH simply having different prices, and
	// the test stayed green with delta application removed entirely.
	firstBySymbol := map[string]OrderbookData{}
	movedBySymbol := map[string]bool{}
	for _, book := range books {
		first, seen := firstBySymbol[book.Symbol]
		if !seen {
			firstBySymbol[book.Symbol] = book
			continue
		}
		if book.BestBid != first.BestBid || book.BestAsk != first.BestAsk {
			movedBySymbol[book.Symbol] = true
		}
	}

	// At least one, not every one: whether a given symbol's TOP moved is the
	// market's business, and ETHUSDT's recorded deltas all landed on levels
	// behind the best. One symbol moving is enough to prove the recorded deltas
	// reach the assembler, and the assembler's own rules are pinned by the two
	// tests below, which do not depend on what the market happened to do.
	if len(movedBySymbol) == 0 {
		t.Errorf("no symbol's top of book moved across %d published books; the deltas are not being applied",
			len(books))
	}
	t.Logf("%d of %d symbols moved on the recorded deltas", len(movedBySymbol), len(firstBySymbol))
}

// A Kraken delta with qty 0 DELETES the level. Treating it as a level priced at
// zero size - or ignoring it - leaves a top of book the venue has already
// withdrawn, which is a price that can no longer be traded on.
//
// The recording holds no such delta (they are less frequent than updates), so
// this drives the assembler directly. The message shape is taken from the
// recorded deltas, not invented: only the quantity differs.
func TestKraken_ADeltaWithZeroQuantityRemovesTheLevel(t *testing.T) {
	book := &KrakenOrderBook{
		Bids: []KrakenOrderBookEntry{{Price: 77300, Qty: 1.5}, {Price: 77200, Qty: 2}},
		Asks: []KrakenOrderBookEntry{{Price: 77400, Qty: 1}, {Price: 77500, Qty: 3}},
	}

	updateKrakenOrderbook(book, KrakenOrderBookData{
		Feed: "book", ProductID: "PF_XBTUSD", Side: "buy", Price: 77300, Qty: 0,
	})

	if len(book.Bids) != 1 || book.Bids[0].Price != 77200 {
		t.Fatalf("bids = %+v, want the 77300 level removed and 77200 promoted", book.Bids)
	}

	updateKrakenOrderbook(book, KrakenOrderBookData{
		Feed: "book", ProductID: "PF_XBTUSD", Side: "sell", Price: 77400, Qty: 0,
	})

	if len(book.Asks) != 1 || book.Asks[0].Price != 77500 {
		t.Fatalf("asks = %+v, want the 77400 level removed and 77500 promoted", book.Asks)
	}
}

// A better bid has to land at the front, not at the back. The bids are held
// highest-first and the connector reads index 0 as the top of book, so an insert
// in the wrong place reports a price nobody is offering.
func TestKraken_ABetterPriceBecomesTheTopOfBook(t *testing.T) {
	book := &KrakenOrderBook{
		Bids: []KrakenOrderBookEntry{{Price: 77300, Qty: 1}, {Price: 77200, Qty: 1}},
		Asks: []KrakenOrderBookEntry{{Price: 77400, Qty: 1}, {Price: 77500, Qty: 1}},
	}

	updateKrakenOrderbook(book, KrakenOrderBookData{Side: "buy", Price: 77350, Qty: 4})
	if book.Bids[0].Price != 77350 {
		t.Errorf("best bid = %g, want the new 77350 level", book.Bids[0].Price)
	}

	updateKrakenOrderbook(book, KrakenOrderBookData{Side: "sell", Price: 77380, Qty: 4})
	if book.Asks[0].Price != 77380 {
		t.Errorf("best ask = %g, want the new 77380 level", book.Asks[0].Price)
	}

	// An update to an existing level changes its size, it does not add a level.
	before := len(book.Bids)
	updateKrakenOrderbook(book, KrakenOrderBookData{Side: "buy", Price: 77350, Qty: 9})
	if len(book.Bids) != before {
		t.Errorf("bids grew from %d to %d on an update to an existing level", before, len(book.Bids))
	}
	if book.Bids[0].Qty != 9 {
		t.Errorf("best bid qty = %g, want the updated 9", book.Bids[0].Qty)
	}
}

// Paradex publishes a summary for EVERY market it lists - hundreds, including
// options with their own strikes and expiries. Only the ones this scanner
// subscribed to may reach it; anything else would appear under a symbol it was
// never quoted for.
func TestParadex_MarketsWeNeverSubscribedToAreDropped(t *testing.T) {
	frames := readFrames(t, "paradex_futures")

	// Guard against the assertion below being vacuous: the recording must
	// actually contain markets we do not follow, or dropping them proves nothing.
	var foreign int
	for _, frame := range frames {
		if bytes.Contains(frame, []byte(`"symbol":`)) &&
			!bytes.Contains(frame, []byte(`"BTC-USD-PERP"`)) &&
			!bytes.Contains(frame, []byte(`"ETH-USD-PERP"`)) {
			foreign++
		}
	}
	if foreign == 0 {
		t.Fatal("the recording holds no unsubscribed market, so this proves nothing; re-record")
	}

	r, _ := replay(t, "paradex_futures")
	for _, book := range r.orderbooks() {
		if book.Symbol != "BTCUSDT" && book.Symbol != "ETHUSDT" {
			t.Errorf("published %q, which was never subscribed", book.Symbol)
		}
	}
	t.Logf("recording carried %d frames for markets we do not follow, none reached the scanner", foreign)
}

// Subscription acknowledgements share the data envelope on several venues. One
// parsed as data would publish a price of zero, which the scanner drops - but
// only after the venue has been marked alive by a message carrying no market
// data at all.
func TestAcknowledgementsProduceNoMarketData(t *testing.T) {
	acks := map[string][]byte{
		"bybit_spot":          []byte(`{"success":true,"ret_msg":"subscribe","conn_id":"x","op":"subscribe"}`),
		"gate_futures":        []byte(`{"time":1788415757,"channel":"futures.book_ticker","event":"subscribe","payload":["BTC_USDT"],"result":{"status":"success"}}`),
		"okx_futures":         []byte(`{"event":"subscribe","arg":{"channel":"books5","instId":"BTC-USDT-SWAP"},"connId":"b2400c07"}`),
		"paradex_futures":     []byte(`{"jsonrpc":"2.0","id":1,"result":{"channel":"markets_summary"},"usIn":1,"usDiff":1,"usOut":1}`),
		"hyperliquid_futures": []byte(`{"channel":"subscriptionResponse","data":{"method":"subscribe","subscription":{"type":"l2Book","coin":"BTC"}}}`),
		"kraken_futures":      []byte(`{"event":"subscribed","feed":"book","product_ids":["PF_XBTUSD"]}`),
	}

	for source, ack := range acks {
		t.Run(source, func(t *testing.T) {
			r := newRecorder(t)
			captureStreams(r.feeds)[source].Handle(ack, time.Now())

			if books := r.orderbooks(); len(books) != 0 {
				t.Errorf("an acknowledgement produced %d books: %+v", len(books), books)
			}
			if trades := r.trades(); len(trades) != 0 {
				t.Errorf("an acknowledgement produced %d trades: %+v", len(trades), trades)
			}
			if prices := r.prices(); len(prices) != 0 {
				t.Errorf("an acknowledgement produced %d prices: %+v", len(prices), prices)
			}
		})
	}
}

// OKX answers the keepalive with the bare word "pong", which is not JSON at all.
// Every decode in the handler has to fail on it without the frame becoming data.
func TestOKX_TheKeepaliveReplyIsNotMarketData(t *testing.T) {
	r := newRecorder(t)
	captureStreams(r.feeds)["okx_futures"].Handle([]byte("pong"), time.Now())

	if books := r.orderbooks(); len(books) != 0 {
		t.Errorf(`the literal "pong" produced %d books`, len(books))
	}
	if trades := r.trades(); len(trades) != 0 {
		t.Errorf(`the literal "pong" produced %d trades`, len(trades))
	}
}

// Recorded 2026-09-03: at depth 1, both Bybit streams sent nothing but
// snapshots across the whole window. The connector cannot currently tell the two
// apart, so a delta that removes the top level (size "0") would be taken at face
// value as a book priced at zero - the defect recorded in CLAUDE.md and
// docs/PLAN.md, still open.
//
// This test pins what the recording contains. It is what makes the defect
// falsifiable: if a re-recording ever captures a delta, this fails and the
// golden data for fixing it exists.
func TestBybit_TheRecordingContainsOnlySnapshots(t *testing.T) {
	for _, source := range []string{"bybit_futures", "bybit_spot"} {
		t.Run(source, func(t *testing.T) {
			var deltas int
			for _, frame := range readFrames(t, source) {
				if bytes.Contains(frame, []byte(`"type":"delta"`)) {
					deltas++
				}
			}
			if deltas > 0 {
				t.Errorf("the recording now holds %d delta frames; the snapshot/delta defect can and should be fixed and tested with them",
					deltas)
			}
		})
	}
}

func TestStandardOf_AnUnknownVenueSymbolResolvesToNothing(t *testing.T) {
	symbols := []Symbol{
		{Standard: "BTCUSDT", Venue: "PF_XBTUSD"},
		{Standard: "ETHUSDT", Venue: "PF_ETHUSD"},
	}

	if got := StandardOf(symbols, "PF_XBTUSD"); got != "BTCUSDT" {
		t.Errorf("StandardOf(PF_XBTUSD) = %q, want BTCUSDT", got)
	}
	// Empty, never a guess: a connector uses this to drop a market it never
	// subscribed to, and any non-empty fallback would file that market's price
	// under some other symbol.
	if got := StandardOf(symbols, "PF_SOLUSD"); got != "" {
		t.Errorf("StandardOf(PF_SOLUSD) = %q, want the empty string", got)
	}
	if got := StandardOf(nil, "PF_XBTUSD"); got != "" {
		t.Errorf("StandardOf on no symbols = %q, want the empty string", got)
	}
}

func TestVenueSymbols_IsTheSubscriptionList(t *testing.T) {
	got := VenueSymbols([]Symbol{
		{Standard: "BTCUSDT", Venue: "BTC-USDT-SWAP"},
		{Standard: "ETHUSDT", Venue: "ETH-USDT-SWAP"},
	})

	want := []string{"BTC-USDT-SWAP", "ETH-USDT-SWAP"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d = %q, want %q", i, got[i], want[i])
		}
	}
}
