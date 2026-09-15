package binance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"futures-arbitrage-scanner/internal/broker"
)

// Recording real testnet answers into testdata/, the way exchanges/exchangestest
// does for the connectors: the capture runs only when CAPTURE_TESTDATA=1, and
// the tests that replay the recordings open no socket.
//
// The recordings exist because encoding/json fails SILENTLY on a wrong field
// name — it leaves the field at its zero value, so a mis-spelled `executedQty`
// reports every order as unfilled and nothing says so. A payload the venue
// really sent is the only thing that catches it, and it is why this package
// records rather than hand-writing fixtures from the documentation.
//
// # What is removed before anything is written
//
// Order answers carry two identifiers. `orderId` is the venue's, `clientOrderId`
// is ours, and both are replaced with fixed placeholders — a recording goes into
// git, and git is forever. Everything else is kept, because the field NAMES and
// the value SHAPES are the whole point.
//
// No account endpoint is recorded at all. Balances and positions are account
// state, and no amount of scrubbing makes a balance safe to commit.

const captureEnv = "CAPTURE_TESTDATA"

// CaptureEnabled reports whether recordings should be written.
func CaptureEnabled() bool { return os.Getenv(captureEnv) == "1" }

// Capture writes each ORDER answer to Dir as a sanitized .json file. It is used
// by cmd/brokercheck's acceptance run, as broker.Config.ObserveResponse.
//
// It was a RoundTripper until 2026-09-15, which made it a transport BELOW the
// broker's host pin — handed every signed request, with nothing checking what
// it did with one. It only ever needed the answer, so it now receives the
// answer after the broker has read it, and cannot send anything.
type Capture struct {
	Dir string

	mu sync.Mutex
	// Written lists the files produced, for the run's report.
	Written []string
}

// recordable is the set of paths worth keeping. Account and balance endpoints
// are deliberately absent.
var recordable = map[string]string{
	"/fapi/v1/order":      "futures_order",
	"/fapi/v1/openOrders": "futures_open_orders",
	"/api/v3/order":       "spot_order",
	"/api/v3/openOrders":  "spot_open_orders",
}

// Observe is the broker.Config.ObserveResponse hook.
func (c *Capture) Observe(rec broker.ResponseRecord) {
	name, ok := recordable[rec.Path]
	if !ok {
		return
	}
	// The verb distinguishes place from cancel from query on the same path.
	file := fmt.Sprintf("%s_%s_%d.json", name, strings.ToLower(rec.Method), rec.StatusCode)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := writeRecording(filepath.Join(c.Dir, file), rec.Body); err == nil {
		c.Written = append(c.Written, file)
	}
}

// orderIDPattern and clientIDPattern replace the two identifiers wherever they
// appear, including inside nested objects and arrays.
var (
	orderIDPattern  = regexp.MustCompile(`"(orderId|orderListId)":\s*-?\d+`)
	clientIDPattern = regexp.MustCompile(`"(clientOrderId|origClientOrderId)":\s*"[^"]*"`)
)

func writeRecording(path string, raw []byte) error {
	// Reject anything that is not JSON rather than committing a stray HTML
	// error page as though it were a venue payload.
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	sanitized := orderIDPattern.ReplaceAll(raw, []byte(`"$1": 4200000000000000001`))
	sanitized = clientIDPattern.ReplaceAll(sanitized, []byte(`"$1": "recorded-order-id"`))

	// Re-indent so a human can read the diff, and prove the result is still
	// valid JSON after the substitution.
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, sanitized, "", "  "); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(pretty.Bytes(), '\n'), 0o644)
}
