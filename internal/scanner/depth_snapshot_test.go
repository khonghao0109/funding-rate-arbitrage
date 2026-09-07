package scanner

import (
	"testing"

	"futures-arbitrage-scanner/internal/depth"
)

// The live signal path (step 3.5) prices its fills on the newest book the
// scanner holds, so the scanner has to hand that book back out — sorted, so
// two reads of an unchanged table compare equal.
func TestDepthSnapshot_ReturnsWhatSetDepthInstalled(t *testing.T) {
	s := New([]string{"BTCUSDT"})
	s.SetDepth([]depth.Summary{
		{Source: "bybit_futures", Symbol: "BTCUSDT", SampledAtMs: 5, MidPriceQuote: 100},
		{Source: "binance_spot", Symbol: "BTCUSDT", SampledAtMs: 5, MidPriceQuote: 100},
	})

	got := s.DepthSnapshot()
	if len(got) != 2 {
		t.Fatalf("got %d summaries, want 2", len(got))
	}
	if got[0].Source != "binance_spot" || got[1].Source != "bybit_futures" {
		t.Errorf("not sorted by source within a symbol: %s, %s", got[0].Source, got[1].Source)
	}

	// A later sweep replaces the table wholesale, exactly as the wire does.
	s.SetDepth([]depth.Summary{{Source: "binance_spot", Symbol: "BTCUSDT", SampledAtMs: 9, MidPriceQuote: 101}})
	got = s.DepthSnapshot()
	if len(got) != 1 || got[0].SampledAtMs != 9 {
		t.Errorf("stale summaries survived a new sweep: %+v", got)
	}
}

func TestHedges_ReturnsWhatSetHedgesInstalled(t *testing.T) {
	s := New([]string{"BTCUSDT"})
	s.SetHedges([]HedgeLeg{
		{Symbol: "BTCUSDT", PerpSource: "kraken_futures", NoteVI: "USD vs USDT"},
		{Symbol: "BTCUSDT", PerpSource: "binance_futures", SpotSource: "binance_spot"},
	})
	got := s.Hedges()
	if len(got) != 2 {
		t.Fatalf("got %d legs, want 2", len(got))
	}
	if got[0].PerpSource != "binance_futures" || got[1].PerpSource != "kraken_futures" {
		t.Errorf("not sorted by perp source: %s, %s", got[0].PerpSource, got[1].PerpSource)
	}
	if got[1].SpotSource != "" || got[1].NoteVI == "" {
		t.Error("a refused hedge must come back as a refusal with its note, not be dropped")
	}
}
