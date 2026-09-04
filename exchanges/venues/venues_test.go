package venues

import "testing"

// Every tradable connector must have an instrument fetcher and vice versa —
// the same two-way agreement cmd/scanner enforces between config and
// Connectors(). Pyth is the deliberate exception: an oracle has no
// instruments.
func TestInstrumentFetchersMatchConnectors(t *testing.T) {
	connectors := Connectors()
	fetchers := InstrumentFetchers()
	for name := range connectors {
		if name == "pyth" {
			continue
		}
		if _, ok := fetchers[name]; !ok {
			t.Errorf("connector %q has no instrument fetcher — its trading rules would stay unknown", name)
		}
	}
	for name := range fetchers {
		if _, ok := connectors[name]; !ok {
			t.Errorf("instrument fetcher %q names a connector that does not exist", name)
		}
	}
}

// Every fetcher in the table must be reachable, or a source silently has no
// depth and its liquidity column is empty forever.
func TestDepthFetchers_CoverEveryTradableSource(t *testing.T) {
	fetchers := DepthFetchers()
	for _, source := range []string{
		"binance_futures", "binance_spot", "bybit_futures", "bybit_spot",
		"okx_futures", "gate_futures", "kraken_futures", "hyperliquid_futures", "paradex_futures",
	} {
		if _, ok := fetchers[source]; !ok {
			t.Errorf("no depth fetcher for %s", source)
		}
	}
	if _, ok := fetchers["pyth"]; ok {
		t.Error("pyth has a depth fetcher; an oracle has no order book")
	}
	if len(fetchers) != 9 {
		t.Errorf("got %d fetchers, want 9", len(fetchers))
	}
}
