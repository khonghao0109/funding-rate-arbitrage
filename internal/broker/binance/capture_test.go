package binance

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"futures-arbitrage-scanner/internal/broker"
)

// Capture is an observer now, not a transport: it is handed the answer the
// broker already read. It must still record ORDER answers only, with both
// identifiers replaced, and never an account endpoint.
func TestCapture_RecordsOrderAnswersOnlyWithTheirIdentifiersReplaced(t *testing.T) {
	dir := t.TempDir()
	c := &Capture{Dir: dir}
	c.Observe(broker.ResponseRecord{
		Method: http.MethodPost, Path: "/fapi/v1/order", StatusCode: http.StatusOK,
		Body: []byte(`{"orderId":28582559181,"clientOrderId":"pbtcusdt-20260914-leg2","status":"NEW"}`),
	})
	c.Observe(broker.ResponseRecord{
		Method: http.MethodGet, Path: "/fapi/v3/balance", StatusCode: http.StatusOK,
		Body: []byte(`[{"asset":"USDT","balance":"4995.71"}]`),
	})
	if len(c.Written) != 1 || c.Written[0] != "futures_order_post_200.json" {
		t.Fatalf("written = %v, want only the order answer", c.Written)
	}
	raw, err := os.ReadFile(filepath.Join(dir, c.Written[0]))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if strings.Contains(got, "28582559181") || strings.Contains(got, "pbtcusdt") || !strings.Contains(got, `"status": "NEW"`) {
		t.Errorf("recording = %s", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("%d files in the recording dir, want 1 — an account answer was written", len(entries))
	}
}
