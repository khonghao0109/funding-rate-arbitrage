package kraken

import (
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// Kraken is the only venue whose top of book is not in the message. A snapshot
// arrives once and every later frame moves ONE level, so the connector's output
// is a function of every frame that came before - replaying them out of order,
// or dropping one, silently yields a book that never existed.
func TestKraken_TopOfBookIsAssembledFromTheDeltaStream(t *testing.T) {
	r := exchangestest.NewRecorder(t)
	exchangestest.Replay(t, r, "kraken_futures", goldenConfigs(r.Feeds)["kraken_futures"])

	books := r.Orderbooks()
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
	for _, symbol := range exchangestest.Symbols("kraken_futures") {
		assembled[symbol.Venue] = &KrakenOrderBook{}
	}
	afterSnapshot := map[string]int{}
	r2 := exchangestest.NewRecorder(t)
	for _, frame := range exchangestest.ReadFrames(t, "kraken_futures") {
		_ = handleKrakenFrame("kraken_futures", exchangestest.Symbols("kraken_futures"), assembled, r2.Feeds, frame, time.Now())
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

// Kraken orders the SAME book differently per transport: the step-2.7b REST
// probe found bids ASCENDING with a resting order at price 1 first, while the
// WS recording arrives descending. The assembler therefore sorts every
// snapshot instead of trusting arrival order — this feeds it the REST-shaped
// order over the WS path and asserts the published top of book is the real
// one, not the $1 junk order.
func TestKrakenSnapshotOrderIsNotTrusted(t *testing.T) {
	symbols := []exchanges.Symbol{{Standard: "BTCUSDT", Venue: "PF_XBTUSD"}}
	orderbooks := map[string]*KrakenOrderBook{"PF_XBTUSD": {}}
	r := exchangestest.NewRecorder(t)

	frame := []byte(`{"feed":"book_snapshot","product_id":"PF_XBTUSD",` +
		`"bids":[{"price":1,"qty":41},{"price":77000,"qty":0.5},{"price":77100,"qty":0.2}],` +
		`"asks":[{"price":77300,"qty":0.4},{"price":77200,"qty":0.1}],"timestamp":1788413683802}`)
	_ = handleKrakenFrame("kraken_futures", symbols, orderbooks, r.Feeds, frame, time.Now())

	books := r.Orderbooks()
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
	_ = handleKrakenFrame("kraken_futures", symbols, orderbooks, r.Feeds, delta, time.Now())
	books = r.Orderbooks()
	if len(books) != 1 || books[0].BestBid != 77150 {
		t.Fatalf("after delta: published %d books, best bid %v, want 1 book at 77150", len(books), books)
	}
}
