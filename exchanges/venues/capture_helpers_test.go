package venues

import (
	"path/filepath"
	"strings"
	"testing"
)

// fixturePath routes a REST fixture to the venue package that owns it, derived
// from the file's own name (instruments_<venue>_*.json, depth_<venue>.json,
// funding_history_<venue>.json, funding_<venue>_*.json). The capture tests
// live HERE, in the one package that spans every venue, because each records
// all venues in one run — but the fixtures belong beside the parsers that
// consume them.
func fixturePath(t *testing.T, name string) string {
	t.Helper()
	rest := name
	for _, prefix := range []string{"instruments_", "depth_", "funding_history_", "funding_", "price_history_"} {
		if strings.HasPrefix(name, prefix) {
			rest = strings.TrimPrefix(name, prefix)
			break
		}
	}
	for _, venue := range []string{"binance", "bybit", "gate", "hyperliquid", "kraken", "okx", "paradex", "pyth"} {
		if strings.HasPrefix(rest, venue) {
			return filepath.Join("..", venue, "testdata", name)
		}
	}
	t.Fatalf("fixture %q does not name a venue this table knows", name)
	return ""
}
