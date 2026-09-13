package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
)

// This command places real orders on a real exchange. The only thing standing
// between it and somebody's money is that the exchange is a testnet, so the
// tests here are about that and nothing else.

// Both markets resolve to a host on the documented testnet allow-list, and
// broker.NewClient is the thing that enforces it.
func TestExeccheck_BothMarketsResolveToATestnetHost(t *testing.T) {
	creds := broker.Credentials{APIKey: broker.NewSecret("k"), APISecret: broker.NewSecret("s")}
	allowed := map[string]bool{}
	for _, h := range broker.TestnetHosts() {
		allowed[h] = true
	}
	if len(allowed) == 0 {
		t.Fatal("the allow-list is empty, so this test would pass on anything")
	}

	for _, market := range []broker.Market{broker.MarketFuturesUSDM, broker.MarketSpot} {
		cfg, err := binancebroker.DefaultConfig(market, creds)
		if err != nil {
			t.Fatalf("%s: %v", market, err)
		}
		u, err := url.Parse(cfg.BaseURL)
		if err != nil {
			t.Fatalf("%s: %v", market, err)
		}
		if !allowed[u.Hostname()] {
			t.Errorf("%s resolves to %q, which is not on the testnet allow-list %v", market, u.Hostname(), broker.TestnetHosts())
		}
		if u.Scheme != "https" {
			t.Errorf("%s uses scheme %q", market, u.Scheme)
		}
		// And the guard itself accepts it, which is the thing that actually
		// runs — a URL that merely looks right is not a URL the client took.
		if _, err := broker.NewClient(cfg); err != nil {
			t.Errorf("%s: NewClient refused its own default config: %v", market, err)
		}
	}
}

// No flag may move the host. The guard lives in broker.NewClient and there is
// no way to widen it before step 4.6 — but a command that took a -base-url
// would route around it without touching that package, so the absence of such
// a flag is asserted here, where the flags are declared.
func TestExeccheck_HasNoFlagThatCouldChangeTheHost(t *testing.T) {
	src := readOwnSource(t)
	for _, bad := range []string{
		`flag.String("base-url"`, `flag.String("host"`, `flag.String("url"`,
		`flag.Bool("mainnet"`, `flag.Bool("live"`, `flag.Bool("real"`,
	} {
		if strings.Contains(src, bad) {
			t.Errorf("this command declares %s — the testnet guard can be routed around without touching internal/broker", bad)
		}
	}
	// And no mainnet host is named anywhere in it.
	for _, host := range []string{"fapi.binance.com", "api.binance.com", "api1.binance.com", "testnet.binancefuture.com"} {
		if strings.Contains(src, host) {
			t.Errorf("this command names %q", host)
		}
	}
}

// The state directory is a FILE cache and must never become a schema: the
// step-3.5 gate has data/scanner.db open for writing, and a migration under it
// is the one change a fortnight-long unattended run cannot survive.
func TestExeccheck_WritesNoDatabase(t *testing.T) {
	// Imports and SQL, not prose: the package comment says out loud that it
	// stays away from data/scanner.db, and a test that banned the string would
	// ban saying so.
	src := readOwnSource(t)
	for _, bad := range []string{
		`"futures-arbitrage-scanner/internal/store"`, `"database/sql"`,
		"modernc.org/sqlite", "CREATE TABLE", "ALTER TABLE", "PRAGMA ",
	} {
		if strings.Contains(src, bad) {
			t.Errorf("this command contains %q; it must write nothing but JSON files under %s", bad, stateDir)
		}
	}
	if !strings.HasPrefix(stateDir, ".paper/") {
		t.Errorf("stateDir is %q, want somewhere under .paper/ — which is gitignored", stateDir)
	}
}

func readOwnSource(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		blob, err := os.ReadFile(filepath.Clean(e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		all.Write(blob)
	}
	if all.Len() == 0 {
		t.Fatal("no source was read, so this test would pass on an empty package")
	}
	return all.String()
}
