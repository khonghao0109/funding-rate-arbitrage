package main

import (
	"path/filepath"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges/venues"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/scanner"
)

func TestPriceSamplesShareOneSampleInstant(t *testing.T) {
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	recvAt := at.Add(-250 * time.Millisecond)

	samples := priceSamples([]scanner.PriceReading{
		{Symbol: "BTCUSDT", Source: "binance_futures", PricePoint: scanner.PricePoint{
			Price: 80000, BestBid: 79999, BestAsk: 80001,
			BestBidQtyCoin: 1.5, BestAskQtyCoin: 2, RecvAt: recvAt,
		}},
		{Symbol: "BTCUSDT", Source: "okx_futures", PricePoint: scanner.PricePoint{
			Price: 80010, BestBid: 80009, BestAsk: 80011, RecvAt: recvAt.Add(-time.Second),
		}},
	}, at)

	if len(samples) != 2 {
		t.Fatalf("got %d samples, want 2", len(samples))
	}
	// Every row of a round carries the SAME instant: a cross-venue spread is
	// only meaningful between prices sampled together, and per-row stamps would
	// turn that query into a join on approximate times.
	if samples[0].SampledAtMs != samples[1].SampledAtMs || samples[0].SampledAtMs != at.UnixMilli() {
		t.Errorf("sample instants = %d and %d, want both %d",
			samples[0].SampledAtMs, samples[1].SampledAtMs, at.UnixMilli())
	}
	// The receive stamps stay per row — that is what makes staleness
	// reconstructible from the stored data later.
	if samples[0].RecvAtMs == samples[1].RecvAtMs {
		t.Error("both rows got the same RecvAtMs; the two feeds were read a second apart")
	}
	if samples[1].BestBidQtyCoin != 0 {
		t.Errorf("okx quantity = %g, want 0 — a contract-denominated book is NOT KNOWN, not empty",
			samples[1].BestBidQtyCoin)
	}
}

func TestPriceSamplesDropUnstampedReadings(t *testing.T) {
	// A zero time.Time is -6795364578 in Unix milliseconds. Stored, it would be
	// a price sample dated 1754 that passes every window filter phase 3 writes.
	samples := priceSamples([]scanner.PriceReading{
		{Symbol: "BTCUSDT", Source: "binance_futures", PricePoint: scanner.PricePoint{Price: 80000}},
	}, time.Now())

	if len(samples) != 0 {
		t.Fatalf("stored %+v, want nothing: the reading carries no receive time", samples)
	}
}

// The scanner's funding history jobs are built from the same config the
// connectors are, so a source added to config.yaml starts being collected
// without a second list to keep in step.
func TestFundingHistoryJobsCoverEveryPerpSource(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatalf("load config.yaml: %v", err)
	}

	jobs := fundingHistoryJobs(cfg)
	byConnector := map[string]int{}
	for _, job := range jobs {
		byConnector[job.Connector] = len(job.Symbols)
	}

	fetchers := venues.FundingHistoryFetchers()
	for connector := range fetchers {
		symbols, ok := byConnector[connector]
		if !ok {
			t.Errorf("connector %q has a funding history fetcher but no configured source reaches it", connector)
			continue
		}
		if symbols == 0 {
			t.Errorf("connector %q was given no symbols to collect", connector)
		}
	}
	// And the other direction: a spot source must not be handed to a history
	// fetcher, because a spot market has no funding at all.
	for _, source := range cfg.Sources {
		if source.MarketType == "perp" {
			continue
		}
		if _, ok := fetchers[source.Connector]; ok {
			t.Errorf("source %q is %s but its connector has a funding history fetcher",
				source.Source, source.MarketType)
		}
	}
}
