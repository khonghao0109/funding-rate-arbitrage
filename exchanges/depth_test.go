package exchanges

import "testing"

func TestFinishDepthBook(t *testing.T) {
	t.Run("levels with no price or no size are dropped", func(t *testing.T) {
		// A padded level contributes a row to the level count and nothing to
		// the liquidity, which is the worst of both.
		book, err := FinishDepthBook(DepthBook{
			Source: "s", Symbol: "BTCUSDT",
			Bids: []DepthLevel{{PriceQuote: 100, QtyNative: 1}, {PriceQuote: 99, QtyNative: 0}, {PriceQuote: 0, QtyNative: 5}},
			Asks: []DepthLevel{{PriceQuote: 101, QtyNative: 2}},
		})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(book.Bids) != 1 {
			t.Errorf("kept %d bids, want only the usable one", len(book.Bids))
		}
	})

	t.Run("a one-sided book is refused", func(t *testing.T) {
		// Every figure downstream is measured from the mid, and a mid needs
		// both sides.
		_, err := FinishDepthBook(DepthBook{
			Source: "s", Symbol: "BTCUSDT",
			Bids: []DepthLevel{{PriceQuote: 100, QtyNative: 1}},
		})
		if err == nil {
			t.Fatal("a book with no asks was accepted")
		}
	})

	t.Run("a crossed book is refused", func(t *testing.T) {
		// Two halves read at different instants, or a venue bug. Publishing it
		// gives a negative spread and a mid between prices that never coexisted.
		_, err := FinishDepthBook(DepthBook{
			Source: "s", Symbol: "BTCUSDT",
			Bids: []DepthLevel{{PriceQuote: 102, QtyNative: 1}},
			Asks: []DepthLevel{{PriceQuote: 101, QtyNative: 1}},
		})
		if err == nil {
			t.Fatal("a crossed book was accepted")
		}
	})

	t.Run("sides are sorted whatever order they arrived in", func(t *testing.T) {
		book, err := FinishDepthBook(DepthBook{
			Source: "s", Symbol: "BTCUSDT",
			Bids: []DepthLevel{{PriceQuote: 1, QtyNative: 41}, {PriceQuote: 100, QtyNative: 1}},
			Asks: []DepthLevel{{PriceQuote: 105, QtyNative: 1}, {PriceQuote: 101, QtyNative: 1}},
		})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if book.Bids[0].PriceQuote != 100 || book.Asks[0].PriceQuote != 101 {
			t.Errorf("best bid/ask = %g/%g, want 100/101", book.Bids[0].PriceQuote, book.Asks[0].PriceQuote)
		}
	})
}
