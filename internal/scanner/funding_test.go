package scanner

import (
	"context"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
)

func TestUpdateFunding_KeepsLatestPerSymbolAndSource(t *testing.T) {
	s := New([]string{"BTCUSDT"})

	if _, ok := s.latestFunding("BTCUSDT", "binance_futures"); ok {
		t.Fatal("latestFunding before any update should report nothing")
	}

	first := exchanges.FundingData{
		Symbol: "BTCUSDT", Source: "binance_futures",
		Model:               exchanges.FundingDiscrete,
		RatePerIntervalFrac: 0.0001, IntervalSec: 28800, RatePer8hFrac: 0.0001,
	}
	second := first
	second.RatePerIntervalFrac, second.RatePer8hFrac = 0.0002, 0.0002
	other := exchanges.FundingData{
		Symbol: "BTCUSDT", Source: "okx_futures",
		Model:               exchanges.FundingDiscrete,
		RatePerIntervalFrac: 0.00005, IntervalSec: 28800, RatePer8hFrac: 0.00005,
	}

	s.updateFunding(first)
	s.updateFunding(second)
	s.updateFunding(other)

	got, ok := s.latestFunding("BTCUSDT", "binance_futures")
	if !ok || got.RatePer8hFrac != 0.0002 {
		t.Fatalf("latestFunding(binance) = %+v, %v; want the second reading", got, ok)
	}
	if got, ok := s.latestFunding("BTCUSDT", "okx_futures"); !ok || got.RatePer8hFrac != 0.00005 {
		t.Fatalf("latestFunding(okx) = %+v, %v; want the okx reading untouched", got, ok)
	}
}

// A symbol nobody configured must be dropped at the door, so an all-market
// funding stream cannot grow the map — and later SQLite and the dashboard —
// with the venue's whole universe.
func TestUpdateFunding_DropsUnconfiguredSymbols(t *testing.T) {
	s := New([]string{"BTCUSDT"})

	s.updateFunding(exchanges.FundingData{
		Symbol: "DOGEUSDT", Source: "binance_futures",
		Model: exchanges.FundingDiscrete, RatePerIntervalFrac: 0.0001, IntervalSec: 28800,
	})

	if _, ok := s.latestFunding("DOGEUSDT", "binance_futures"); ok {
		t.Fatal("a symbol outside the configured set must not be stored")
	}
}

// A funding message must prove its source alive, the way a trade does: at step
// 2.5 several venues' funding rides streams that carry no price, and a venue
// only sending funding must not be shown as disconnected. The stored reading
// must carry the same normalized stamp — a zero RecvAt stored verbatim would
// read as a ~56-year age downstream while liveness said "just now".
func TestUpdateFunding_MarksSourceAliveAndNormalizesRecvAt(t *testing.T) {
	s := New([]string{"BTCUSDT"})
	fixed := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }

	// No RecvAt stamped: the scanner's clock must fill in, both in the
	// liveness record and in the stored reading.
	s.updateFunding(exchanges.FundingData{
		Symbol: "BTCUSDT", Source: "gate_futures",
		Model: exchanges.FundingDiscrete, RatePerIntervalFrac: 0.0001, IntervalSec: 28800,
	})

	s.lastMsgMutex.RLock()
	lastAt, ok := s.sourceLastMsgAt["gate_futures"]
	s.lastMsgMutex.RUnlock()
	if !ok || !lastAt.Equal(fixed) {
		t.Fatalf("sourceLastMsgAt[gate_futures] = %v, %v; want the fallback clock %v", lastAt, ok, fixed)
	}
	stored, ok := s.latestFunding("BTCUSDT", "gate_futures")
	if !ok || !stored.RecvAt.Equal(fixed) {
		t.Fatalf("stored RecvAt = %v, %v; want normalized to %v like updatePrice does", stored.RecvAt, ok, fixed)
	}

	// A real socket stamp must be carried through untouched, not re-stamped.
	socketAt := fixed.Add(-3 * time.Second)
	s.updateFunding(exchanges.FundingData{
		Symbol: "BTCUSDT", Source: "gate_futures", RecvAt: socketAt,
		Model: exchanges.FundingDiscrete, RatePerIntervalFrac: 0.0001, IntervalSec: 28800,
	})
	stored, _ = s.latestFunding("BTCUSDT", "gate_futures")
	if !stored.RecvAt.Equal(socketAt) {
		t.Fatalf("stored RecvAt = %v; want the socket stamp %v carried through", stored.RecvAt, socketAt)
	}
}

// End to end through the same path a connector uses: Feeds → channel →
// processFunding → latestFunding.
func TestFundingFlowsThroughFeeds(t *testing.T) {
	s := New([]string{"BTCUSDT"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.processFunding(ctx)

	feeds := s.Feeds(ctx)
	if ok := feeds.SendFunding(exchanges.FundingData{
		Symbol: "BTCUSDT", Source: "kraken_futures",
		Model:               exchanges.FundingDiscrete,
		RatePerIntervalFrac: 0.00013 / 8, IntervalSec: 3600, RatePer8hFrac: 0.00013,
	}); !ok {
		t.Fatal("SendFunding should deliver into a running scanner")
	}

	deadline := time.After(2 * time.Second)
	for {
		if got, ok := s.latestFunding("BTCUSDT", "kraken_futures"); ok {
			if got.RatePer8hFrac != 0.00013 {
				t.Fatalf("latestFunding = %+v; want RatePer8hFrac 0.00013", got)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("funding sent through Feeds never became visible")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
