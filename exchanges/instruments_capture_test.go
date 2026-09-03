package exchanges

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCaptureInstrumentTestdata re-records exchanges/testdata/instruments_*.json
// from the live venues. Like TestCaptureTestdata it opens real connections, so
// it only runs when CAPTURE_TESTDATA=1:
//
//	CAPTURE_TESTDATA=1 go test -run TestCaptureInstrumentTestdata -timeout 5m ./exchanges/
//
// Binance futures and Kraken publish only unfiltered lists (~1MB each), so
// those two are trimmed to the configured symbols before writing — the kept
// entries are byte-verbatim, the array is filtered. Everything else is stored
// exactly as received.
func TestCaptureInstrumentTestdata(t *testing.T) {
	if os.Getenv(captureEnv) != "1" {
		t.Skip("set CAPTURE_TESTDATA=1 to re-record instrument testdata from live venues")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// The four configured pairs in each venue's own naming. Hardcoded like
	// captureSymbols (see its comment): exchanges must not import
	// internal/config, so this table MIRRORS config.yaml by hand — adding a
	// pair there means adding it here before re-recording. Kept separate from
	// captureSymbols because WS capture deliberately uses only two pairs.
	natives := map[string][]string{
		"binance":     {"BTCUSDT", "ETHUSDT", "XRPUSDT", "SOLUSDT"},
		"okx":         {"BTC-USDT-SWAP", "ETH-USDT-SWAP", "XRP-USDT-SWAP", "SOL-USDT-SWAP"},
		"gate":        {"BTC_USDT", "ETH_USDT", "XRP_USDT", "SOL_USDT"},
		"kraken":      {"PF_XBTUSD", "PF_ETHUSD", "PF_XRPUSD", "PF_SOLUSD"},
		"paradex":     {"BTC-USD-PERP", "ETH-USD-PERP", "XRP-USD-PERP", "SOL-USD-PERP"},
		"hyperliquid": {"BTC", "ETH", "XRP", "SOL"},
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

	trim := func(raw []byte, arrayKey string, names []string) []byte {
		t.Helper()
		wanted := map[string]bool{}
		for _, n := range names {
			wanted[n] = true
		}
		trimmed, err := trimInstrumentList(raw, arrayKey, wanted)
		if err != nil {
			t.Fatal(err)
		}
		return trimmed
	}

	// Binance futures: full list, trimmed. Binance spot: also fetched
	// server-side filtered here (?symbols=[...]) purely to keep the recording
	// small and verbatim — the FETCHER deliberately pulls the unfiltered list
	// instead, because that endpoint 400s the whole request over one unknown
	// symbol; the parser input shape is identical either way.
	write("instruments_binance_futures.json",
		trim(get("https://fapi.binance.com/fapi/v1/exchangeInfo"), "symbols", natives["binance"]))
	{
		quoted := make([]string, len(natives["binance"]))
		for i, n := range natives["binance"] {
			quoted[i] = `"` + n + `"`
		}
		write("instruments_binance_spot.json",
			get("https://api.binance.com/api/v3/exchangeInfo?symbols=%5B"+strings.Join(quoted, ",")+"%5D"))
	}
	// Bybit, OKX, Gate, Paradex: one representative per-symbol response
	// (verbatim) — the parsers consume one response at a time.
	write("instruments_bybit_futures.json", get("https://api.bybit.com/v5/market/instruments-info?category=linear&symbol=BTCUSDT"))
	write("instruments_bybit_spot.json", get("https://api.bybit.com/v5/market/instruments-info?category=spot&symbol=BTCUSDT"))
	write("instruments_okx_futures.json", get("https://www.okx.com/api/v5/public/instruments?instType=SWAP&instId=BTC-USDT-SWAP"))
	write("instruments_gate_futures.json", get("https://api.gateio.ws/api/v4/futures/usdt/contracts/BTC_USDT"))
	write("instruments_paradex_futures.json", get("https://api.prod.paradex.trade/v1/markets?market=BTC-USD-PERP"))

	// Kraken: full list, trimmed the same way as Binance futures.
	write("instruments_kraken_futures.json",
		trim(get("https://futures.kraken.com/derivatives/api/v3/instruments"), "instruments", natives["kraken"]))

	// Hyperliquid: POST, small enough to store whole.
	{
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"https://api.hyperliquid.xyz/info", strings.NewReader(`{"type":"meta"}`))
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
			t.Fatal(fmt.Errorf("hyperliquid meta: HTTP %d, read err %v", resp.StatusCode, err))
		}
		write("instruments_hyperliquid_futures.json", raw)
	}
}
