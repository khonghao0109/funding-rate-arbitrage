package main

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/instruments"
)

// emptyRegistry is a registry that has never refreshed — the state the process
// is in for the first half-minute after every start.
func emptyRegistry(t *testing.T) *instruments.Registry {
	t.Helper()
	return instruments.New(nil)
}

// Every tradable source must get a depth job, or its liquidity column is empty
// forever and nothing says why.
func TestDepthJobs_CoverEveryTradableSource(t *testing.T) {
	cfg := repoConfig(t)
	jobs := depthJobs(cfg)

	byName := map[string]int{}
	for _, job := range jobs {
		byName[job.Source] = len(job.Symbols)
	}
	fetchers := exchanges.DepthFetchers()
	for _, source := range cfg.Sources {
		if _, hasFetcher := fetchers[source.Connector]; !hasFetcher {
			continue // the oracle
		}
		if byName[source.Source] == 0 {
			t.Errorf("%s has a depth fetcher but no job with symbols", source.Source)
		}
	}
}

func TestContractSizes_KeyedBySymbolAndSource(t *testing.T) {
	sizes := contractSizes([]exchanges.Instrument{
		{Symbol: "BTCUSDT", Source: "gate_futures", ContractSizeCoin: 0.0001},
		{Symbol: "BTCUSDT", Source: "okx_futures", ContractSizeCoin: 0.01},
		{Symbol: "BTCUSDT", Source: "binance_futures", ContractSizeCoin: 1},
		// A registry entry with no multiplier is NOT a multiplier of 1: the
		// venue simply did not say, and guessing reports the wrong depth.
		{Symbol: "ETHUSDT", Source: "gate_futures", ContractSizeCoin: 0},
	})

	if got := sizes["BTCUSDT|gate_futures"]; got != 0.0001 {
		t.Errorf("gate multiplier = %g, want 0.0001", got)
	}
	if got := sizes["BTCUSDT|binance_futures"]; got != 1 {
		t.Errorf("a coin venue must still be present with 1, got %g", got)
	}
	if _, ok := sizes["ETHUSDT|gate_futures"]; ok {
		t.Error("an unpublished contract size was turned into a multiplier")
	}
}

// The lookup the collector uses must refuse rather than default, for the same
// reason: an unknown Gate multiplier applied as 1 reports 10,000x the depth.
func TestContractSizeFrom_RefusesAMarketTheRegistryDoesNotKnow(t *testing.T) {
	lookup := contractSizeFrom(emptyRegistry(t))
	if _, ok := lookup("BTCUSDT", "gate_futures"); ok {
		t.Error("an empty registry reported a known multiplier")
	}
}
