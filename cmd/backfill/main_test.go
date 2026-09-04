package main

import (
	"path/filepath"
	"strings"
	"testing"

	"futures-arbitrage-scanner/exchanges/venues"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/history"
)

func repoConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatalf("load config.yaml: %v", err)
	}
	return cfg
}

func TestJobsFromCoversEveryConfiguredPair(t *testing.T) {
	cfg := repoConfig(t)
	jobs := jobsFrom(cfg, "", "")

	if len(jobs) != len(cfg.Sources) {
		t.Fatalf("got %d jobs for %d sources", len(jobs), len(cfg.Sources))
	}
	fetchers := venues.FundingHistoryFetchers()
	for _, job := range jobs {
		if _, ok := fetchers[job.Connector]; !ok {
			continue // dropped by history.New; a spot source or the oracle
		}
		if len(job.Symbols) == 0 {
			t.Errorf("perp source %q was given no symbols", job.Source)
		}
		for _, symbol := range job.Symbols {
			if symbol.Standard == "" || symbol.Venue == "" {
				t.Errorf("%s: symbol %+v is missing half its identity", job.Source, symbol)
			}
		}
	}
}

func TestJobsFromNarrowsToOnePair(t *testing.T) {
	cfg := repoConfig(t)
	jobs := jobsFrom(cfg, "BTCUSDT", "")

	for _, job := range jobs {
		for _, symbol := range job.Symbols {
			if symbol.Standard != "BTCUSDT" {
				t.Errorf("-symbol BTCUSDT still collected %s on %s", symbol.Standard, job.Source)
			}
		}
	}
	// A filter that matched nothing would look identical to a successful run
	// that had nothing to do.
	collectable := history.New(nil, jobs, venues.FundingHistoryFetchers())
	if len(collectable.Jobs()) == 0 {
		t.Fatal("no source is collectable for BTCUSDT")
	}
}

func TestJobsFromRejectsAnUnknownPairByCollectingNothing(t *testing.T) {
	cfg := repoConfig(t)
	collectable := history.New(nil, jobsFrom(cfg, "NOSUCHUSDT", ""), venues.FundingHistoryFetchers())
	// main turns this into a fatal error rather than a silent no-op run that
	// prints an empty coverage table and exits 0.
	if len(collectable.Jobs()) != 0 {
		t.Fatalf("an unknown pair produced %d collectable jobs", len(collectable.Jobs()))
	}
}

func TestJobsFromNarrowsToOneSource(t *testing.T) {
	// Repairing one rate-limited series must not re-read the twenty-seven that
	// are already complete — on Paradex that alone is two thousand requests.
	cfg := repoConfig(t)
	jobs := jobsFrom(cfg, "XRPUSDT", "hyperliquid_futures")

	if len(jobs) != 1 || jobs[0].Source != "hyperliquid_futures" {
		t.Fatalf("got %d jobs %+v; want only hyperliquid_futures", len(jobs), jobs)
	}
	if len(jobs[0].Symbols) != 1 || jobs[0].Symbols[0].Standard != "XRPUSDT" {
		t.Fatalf("got symbols %+v; want only XRPUSDT", jobs[0].Symbols)
	}
}

func TestJobsFromRejectsAnUnknownSourceByCollectingNothing(t *testing.T) {
	cfg := repoConfig(t)
	collectable := history.New(nil, jobsFrom(cfg, "", "nosuch_futures"), venues.FundingHistoryFetchers())
	if len(collectable.Jobs()) != 0 {
		t.Fatalf("an unknown source produced %d collectable jobs", len(collectable.Jobs()))
	}
}

func TestFormatGapsIsOrderedAndCounted(t *testing.T) {
	got := formatGaps(map[int64]int{7200: 6, 3600: 8764, 10800: 1})
	want := "3600s×8764 7200s×6 10800s×1"
	if got != want {
		t.Errorf("formatGaps = %q, want %q", got, want)
	}
	if !strings.Contains(got, "3600s") {
		t.Error("the modal gap must be visible in the rendering")
	}
}
