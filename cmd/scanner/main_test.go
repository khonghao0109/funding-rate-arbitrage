package main

import (
	"path/filepath"
	"testing"

	"futures-arbitrage-scanner/exchanges/venues"
	"futures-arbitrage-scanner/internal/config"
)

// repoConfig is the config.yaml the scanner actually ships with. Tests here run
// against it rather than a fixture, for the same reason internal/config's do:
// the file is the only place the source list exists.
func repoConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatalf("load config.yaml: %v", err)
	}
	return cfg
}

// The debt this closes, recorded at step 1.0: wire_test.go held a hand-copied
// list of the ten sources main() connected, so adding a connector and forgetting
// to register it was invisible. config.yaml is now the only list, and this
// checks the two halves of it agree - every configured source names a connector
// that exists, and every connector that exists is reachable from config.
func TestShippedConfig_NamesOnlyConnectorsThatExist(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatalf("load config.yaml: %v", err)
	}

	available := venues.Connectors()
	for _, source := range cfg.Sources {
		if _, ok := available[source.Connector]; !ok {
			t.Errorf("source %q names connector %q, which does not exist", source.Source, source.Connector)
		}
	}
}

// A connector nobody can reach from configuration is dead weight, and more
// likely a source somebody forgot to add to config.yaml.
func TestEveryConnector_IsReachableFromTheShippedConfig(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatalf("load config.yaml: %v", err)
	}

	used := map[string]bool{}
	for _, source := range cfg.Sources {
		used[source.Connector] = true
	}
	for name := range venues.Connectors() {
		if !used[name] {
			t.Errorf("connector %q exists but no configured source uses it", name)
		}
	}
}
