package exchanges

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCaptureFundingTestdata re-records the REST payloads behind funding
// collection — the ones no WebSocket capture can hold (step 2.5):
//
//		CAPTURE_TESTDATA=1 go test -run TestCaptureFundingTestdata ./exchanges/
//
//	  - Binance premiumIndex and fundingInfo: Binance funding comes from REST
//	    because its mark-price stream delivers nothing here (see
//	    binance_funding_rest.go), so its golden fixture is a REST response.
//	  - Hyperliquid predictedFundings: the WebSocket carries the rate, this
//	    carries the cadence and the settlement stamp.
//
// The two Binance lists cover every listed perpetual (~780 and ~778 entries),
// so they are trimmed to the configured symbols before writing — kept entries
// are byte-verbatim, exactly like the instrument recordings.
func TestCaptureFundingTestdata(t *testing.T) {
	if os.Getenv(captureEnv) != "1" {
		t.Skipf("set %s=1 to re-record funding REST payloads from the live venues", captureEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	natives := map[string]bool{}
	for _, s := range captureSymbols["binance_futures"] {
		natives[s.Venue] = true
	}

	get := func(url string) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", url, err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: HTTP %d, read err %v", url, resp.StatusCode, err)
		}
		return raw
	}
	write := func(name string, raw []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(captureDirectory, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %s (%d bytes)", name, len(raw))
	}
	// trimSymbolArray keeps only the configured symbols from a TOP-LEVEL JSON
	// array of objects carrying a "symbol" field. trimInstrumentList cannot be
	// reused: these responses are bare arrays, not an object wrapping one.
	trimSymbolArray := func(raw []byte) []byte {
		t.Helper()
		var entries []json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			t.Fatal(err)
		}
		kept := make([]json.RawMessage, 0, len(natives))
		for _, entry := range entries {
			var head struct {
				Symbol string `json:"symbol"`
			}
			if err := json.Unmarshal(entry, &head); err != nil {
				t.Fatal(err)
			}
			if natives[head.Symbol] {
				kept = append(kept, entry)
			}
		}
		out, err := json.Marshal(kept)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	write("funding_binance_premium_index.json", trimSymbolArray(get("https://fapi.binance.com/fapi/v1/premiumIndex")))
	write("funding_binance_info.json", trimSymbolArray(get("https://fapi.binance.com/fapi/v1/fundingInfo")))

	// Hyperliquid: POST, and the response is one row per coin — kept whole so
	// the parser is exercised against the other venues' rows it must ignore.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.hyperliquid.xyz/info", strings.NewReader(`{"type":"predictedFundings"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("predictedFundings: HTTP %d, read err %v", resp.StatusCode, err)
	}
	write("funding_hyperliquid_predicted.json", raw)
}
