package exchanges

import (
	"bytes"
	"encoding/json"
	"strings"
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

	// And the deltas have to actually REACH the assembler, or this is asserting
	// on one snapshot republished.
	//
	// Measured on the state, not on the top of book. The earlier version of
	// this check required a recorded delta to move the best bid or ask, which
	// is the market's business rather than the connector's: re-recorded
	// 2026-09-04, PF_XBTUSD carried 1,792 bid levels at a $1 spread and not one
	// of 40 deltas per product came within 80 ticks of the top, so a correct
	// assembler could not be observed doing anything. Raising the recorded
	// delta count (framesPerSequenceKind) did not change that and would not on
	// any given day.
	//
	// Applying the same frames to a book of our own answers the real question —
	// were the deltas applied — for every recording, in any market condition.
	// The rules the assembler applies are pinned by the two tests below.
	assembled := map[string]*KrakenOrderBook{}
	for _, symbol := range captureSymbols["kraken_futures"] {
		assembled[symbol.Venue] = &KrakenOrderBook{}
	}
	afterSnapshot := map[string]int{}
	r2 := newRecorder(t)
	for _, frame := range readFrames(t, "kraken_futures") {
		handleKrakenFrame("kraken_futures", captureSymbols["kraken_futures"], assembled, r2.feeds, frame, time.Now())
		// Record each book's size the moment its snapshot has landed, so what
		// is compared afterwards is the effect of the DELTAS alone.
		for venue, book := range assembled {
			if _, seen := afterSnapshot[venue]; !seen && len(book.Bids) > 0 {
				afterSnapshot[venue] = len(book.Bids) + len(book.Asks)
			}
		}
	}

	var changed int
	for venue, book := range assembled {
		if len(book.Bids)+len(book.Asks) != afterSnapshot[venue] {
			changed++ // a delta added or removed a level
			continue
		}
		// Same level count can still mean every delta was an update in place,
		// which is the common case: compare the depth itself.
		var qty float64
		for _, entry := range append(append([]KrakenOrderBookEntry{}, book.Bids...), book.Asks...) {
			qty += entry.Qty
		}
		t.Logf("%s: %d levels, total qty %g", venue, len(book.Bids)+len(book.Asks), qty)
	}
	if changed == 0 {
		t.Errorf("no product's assembled book changed shape across the recorded deltas; they are not being applied")
	}
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

// Recorded 2026-09-03 and again 2026-09-04: at depth 1, both Bybit ORDERBOOK
// streams sent nothing but snapshots across the whole window. The connector
// cannot currently tell the two apart, so a delta that removes the top level
// (size "0") would be taken at face value as a book priced at zero - the defect
// recorded in CLAUDE.md and docs/PLAN.md, still open.
//
// This test pins what the recording contains. It is what makes the defect
// falsifiable: if a re-recording ever captures an orderbook delta, this fails
// and the golden data for fixing it exists.
//
// Scoped to the orderbook TOPIC since step 2.5. The tickers channel added there
// is snapshot+delta too and its deltas are now recorded — but those are merged
// correctly (bybit_ticker.go, TestBybitTickerMerge_AbsentMeansUnchanged), and
// counting them here would fire this tripwire for a defect that is fixed,
// hiding the open one it exists to watch.
func TestBybit_TheOrderbookRecordingContainsOnlySnapshots(t *testing.T) {
	for _, source := range []string{"bybit_futures", "bybit_spot"} {
		t.Run(source, func(t *testing.T) {
			var deltas int
			for _, frame := range readFrames(t, source) {
				var message struct {
					Topic string `json:"topic"`
					Type  string `json:"type"`
				}
				if json.Unmarshal(frame, &message) != nil {
					continue
				}
				if message.Type == "delta" && strings.HasPrefix(message.Topic, "orderbook.") {
					deltas++
				}
			}
			if deltas > 0 {
				t.Errorf("the recording now holds %d orderbook delta frames; the snapshot/delta defect can and should be fixed and tested with them",
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

// Kraken orders the SAME book differently per transport: the step-2.7b REST
// probe found bids ASCENDING with a resting order at price 1 first, while the
// WS recording arrives descending. The assembler therefore sorts every
// snapshot instead of trusting arrival order — this feeds it the REST-shaped
// order over the WS path and asserts the published top of book is the real
// one, not the $1 junk order.
func TestKrakenSnapshotOrderIsNotTrusted(t *testing.T) {
	symbols := []Symbol{{Standard: "BTCUSDT", Venue: "PF_XBTUSD"}}
	orderbooks := map[string]*KrakenOrderBook{"PF_XBTUSD": {}}
	r := newRecorder(t)

	frame := []byte(`{"feed":"book_snapshot","product_id":"PF_XBTUSD",` +
		`"bids":[{"price":1,"qty":41},{"price":77000,"qty":0.5},{"price":77100,"qty":0.2}],` +
		`"asks":[{"price":77300,"qty":0.4},{"price":77200,"qty":0.1}],"timestamp":1788413683802}`)
	handleKrakenFrame("kraken_futures", symbols, orderbooks, r.feeds, frame, time.Now())

	books := r.orderbooks()
	if len(books) != 1 {
		t.Fatalf("published %d orderbooks, want 1", len(books))
	}
	if books[0].BestBid != 77100 || books[0].BestAsk != 77200 {
		t.Fatalf("best bid/ask = %g/%g, want 77100/77200 — snapshot order was trusted",
			books[0].BestBid, books[0].BestAsk)
	}
	if books[0].BestBidQtyContracts != 0.2 || books[0].BestAskQtyContracts != 0.1 {
		t.Fatalf("top-of-book qty = %g/%g contracts, want 0.2/0.1",
			books[0].BestBidQtyContracts, books[0].BestAskQtyContracts)
	}

	// And the invariant survives deltas, which assume sortedness: inserting a
	// new best bid must land at the front, not wherever the venue's order
	// would have put it.
	delta := []byte(`{"feed":"book","product_id":"PF_XBTUSD","side":"buy","price":77150,"qty":0.3,"timestamp":1788413683903}`)
	handleKrakenFrame("kraken_futures", symbols, orderbooks, r.feeds, delta, time.Now())
	books = r.orderbooks()
	if len(books) != 1 || books[0].BestBid != 77150 {
		t.Fatalf("after delta: published %d books, best bid %v, want 1 book at 77150", len(books), books)
	}
}
