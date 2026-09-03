package instruments

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
)

func fixedFetch(insts []exchanges.Instrument, err error) exchanges.InstrumentFetchFunc {
	return func(ctx context.Context, source string, symbols []exchanges.Symbol) ([]exchanges.Instrument, error) {
		return insts, err
	}
}

func TestRegistry_RefreshAndLookup(t *testing.T) {
	btc := exchanges.Instrument{Symbol: "BTCUSDT", Source: "a", StepSizeCoin: 0.001}
	r := New([]Source{{
		Name:    "a",
		Symbols: []exchanges.Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}},
		Fetch:   fixedFetch([]exchanges.Instrument{btc}, nil),
	}})

	if _, ok := r.Instrument("a", "BTCUSDT"); ok {
		t.Fatal("lookup before any refresh should miss")
	}
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, ok := r.Instrument("a", "BTCUSDT")
	if !ok || got.StepSizeCoin != 0.001 {
		t.Fatalf("Instrument = %+v, %v; want the fetched rules", got, ok)
	}
	if r.Count() != 1 {
		t.Fatalf("Count = %d, want 1", r.Count())
	}
	if r.RefreshedAt("a").IsZero() {
		t.Fatal("RefreshedAt should be stamped after a successful refresh")
	}
}

// A failing source must not erase what it published yesterday, must not stop
// the other sources from refreshing, and must be named in the error.
func TestRegistry_FailedSourceKeepsOldRulesAndIsNamed(t *testing.T) {
	good := exchanges.Instrument{Symbol: "BTCUSDT", Source: "good", StepSizeCoin: 0.01}
	calls := 0
	flaky := func(ctx context.Context, source string, symbols []exchanges.Symbol) ([]exchanges.Instrument, error) {
		calls++
		if calls == 1 {
			return []exchanges.Instrument{{Symbol: "BTCUSDT", Source: "flaky", StepSizeCoin: 0.5}}, nil
		}
		return nil, errors.New("venue down")
	}
	r := New([]Source{
		{Name: "flaky", Symbols: nil, Fetch: flaky},
		{Name: "good", Symbols: nil, Fetch: fixedFetch([]exchanges.Instrument{good}, nil)},
	})

	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	err := r.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "flaky") {
		t.Fatalf("second refresh error = %v; want the failing source named", err)
	}
	if got, ok := r.Instrument("flaky", "BTCUSDT"); !ok || got.StepSizeCoin != 0.5 {
		t.Fatalf("flaky source after failed refresh = %+v, %v; want yesterday's rules kept", got, ok)
	}
	if _, ok := r.Instrument("good", "BTCUSDT"); !ok {
		t.Fatal("good source must refresh even when a sibling fails")
	}
}

// A "successful" fetch of zero instruments for a source that asked for
// symbols is a wipe, not a refresh: a decode-to-empty response (venue error
// wrapped in HTTP 200, renamed field) must keep yesterday's rules and be
// named in the error, exactly like a transport failure.
func TestRegistry_EmptyFetchKeepsOldRulesAndIsNamed(t *testing.T) {
	calls := 0
	emptying := func(ctx context.Context, source string, symbols []exchanges.Symbol) ([]exchanges.Instrument, error) {
		calls++
		if calls == 1 {
			return []exchanges.Instrument{{Symbol: "BTCUSDT", Source: "a", StepSizeCoin: 0.001}}, nil
		}
		return nil, nil
	}
	r := New([]Source{{
		Name:    "a",
		Symbols: []exchanges.Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}},
		Fetch:   emptying,
	}})

	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	err := r.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "0 instruments") {
		t.Fatalf("empty refresh error = %v; want the wipe named", err)
	}
	if _, ok := r.Instrument("a", "BTCUSDT"); !ok {
		t.Fatal("an empty fetch must not erase yesterday's rules")
	}
}

// Run must do its first refresh immediately, invoke afterRefresh once the
// fresh rules are readable, and stop when the context ends.
func TestRegistry_RunRefreshesAndStops(t *testing.T) {
	refreshed := make(chan int, 1)
	r := New([]Source{{
		Name:    "a",
		Symbols: []exchanges.Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}},
		Fetch: func(ctx context.Context, source string, symbols []exchanges.Symbol) ([]exchanges.Instrument, error) {
			return []exchanges.Instrument{{Symbol: "BTCUSDT", Source: "a"}}, nil
		},
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx, func() {
			select {
			case refreshed <- r.Count(): // afterRefresh must see the applied rules
			default:
			}
		})
		close(done)
	}()

	select {
	case count := <-refreshed:
		if count != 1 {
			t.Fatalf("afterRefresh saw %d instruments, want 1 — it must run after the swap", count)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run never performed its initial refresh")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on context cancellation")
	}
}

// A symbol the venue does not return is absent everywhere downstream (the
// hedge mapping self-excludes it), so the registry — the only layer that
// knows what was ASKED for — must name it. This is what makes a symbol_map
// typo visible now that fetchers report unlisted markets as absent.
func TestRegistry_NamesSymbolsTheVenueDidNotReturn(t *testing.T) {
	asked := []exchanges.Symbol{
		{Standard: "BTCUSDT", Venue: "BTCUSDT"},
		{Standard: "XLMUSDT", Venue: "XLM-USD-PERP"},
	}
	if missing := missingSymbols(asked, []exchanges.Instrument{{Symbol: "BTCUSDT"}}); len(missing) != 1 || missing[0] != "XLMUSDT" {
		t.Fatalf("missingSymbols = %v, want [XLMUSDT]", missing)
	}
	if missing := missingSymbols(asked, []exchanges.Instrument{{Symbol: "BTCUSDT"}, {Symbol: "XLMUSDT"}}); len(missing) != 0 {
		t.Fatalf("missingSymbols = %v, want none when everything came back", missing)
	}
}

// Snapshot must return every cached instrument in a stable order and hand out
// copies — mutating the result must not reach the cache.
func TestRegistry_Snapshot(t *testing.T) {
	r := New([]Source{
		{Name: "b", Fetch: fixedFetch([]exchanges.Instrument{{Symbol: "BTCUSDT", Source: "b", StepSizeCoin: 0.1}}, nil)},
		{Name: "a", Fetch: fixedFetch([]exchanges.Instrument{
			{Symbol: "ETHUSDT", Source: "a"},
			{Symbol: "BTCUSDT", Source: "a"},
		}, nil)},
	})
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := r.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot returned %d instruments, want 3", len(snap))
	}
	wantOrder := []string{"a/BTCUSDT", "a/ETHUSDT", "b/BTCUSDT"}
	for i, inst := range snap {
		if got := inst.Source + "/" + inst.Symbol; got != wantOrder[i] {
			t.Fatalf("Snapshot[%d] = %s, want %s", i, got, wantOrder[i])
		}
	}
	snap[2].StepSizeCoin = 999
	if inst, _ := r.Instrument("b", "BTCUSDT"); inst.StepSizeCoin != 0.1 {
		t.Fatal("mutating a snapshot reached the cache")
	}
}

// A successful refresh replaces the source's whole map: a market the venue
// stopped listing disappears instead of surviving as a stale entry.
func TestRegistry_RefreshDropsUnlistedMarkets(t *testing.T) {
	calls := 0
	shrinking := func(ctx context.Context, source string, symbols []exchanges.Symbol) ([]exchanges.Instrument, error) {
		calls++
		if calls == 1 {
			return []exchanges.Instrument{
				{Symbol: "BTCUSDT", Source: "a"},
				{Symbol: "DOGEUSDT", Source: "a"},
			}, nil
		}
		return []exchanges.Instrument{{Symbol: "BTCUSDT", Source: "a"}}, nil
	}
	r := New([]Source{{Name: "a", Fetch: shrinking}})

	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Instrument("a", "DOGEUSDT"); ok {
		t.Fatal("a market the venue no longer lists must not survive a refresh")
	}
}
