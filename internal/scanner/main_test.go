package scanner

import (
	"os"
	"path/filepath"
	"testing"

	"futures-arbitrage-scanner/internal/config"
)

// The registry is built from config.yaml, so every test in this package runs
// against the file the scanner actually ships with rather than a fixture.
//
// That is deliberate. A test fixture would let config.yaml drift - a source
// renamed, a threshold zeroed, a fee marked verified with no citation - while
// every test stayed green. Loading the real file means the configuration is
// itself under test, which is what step 1.4 needs: it is now the only place the
// source list exists.
func TestMain(m *testing.M) {
	cfg, err := config.Load(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		panic("load config.yaml: " + err.Error())
	}
	Configure(cfg)
	os.Exit(m.Run())
}
