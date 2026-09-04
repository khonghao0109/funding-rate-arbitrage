package venues

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// TestCaptureFundingHistoryTestdata re-records one page of every venue's
// SETTLED funding endpoint (step 2.6):
//
//	CAPTURE_TESTDATA=1 go test -run TestCaptureFundingHistoryTestdata ./exchanges/
//
// Seven venues, seven pagination idioms, seven shapes — and three of them
// (Kraken's absolute-vs-relative pair, Gate's second-level stamps, Paradex's
// five-second index samples) are shapes a parser can get plausibly wrong and
// still produce numbers. These fixtures are what makes that visible.
//
// The URLs are built the same way the fetchers build them, but not BY the
// fetchers: a recording made through the code under test could only ever agree
// with it. The one thing shared is the symbol table, so a fixture can never be
// recorded for a market the scanner does not follow.
func TestCaptureFundingHistoryTestdata(t *testing.T) {
	if os.Getenv(exchangestest.CaptureEnv) != "1" {
		t.Skipf("set %s=1 to re-record funding history payloads from the live venues", exchangestest.CaptureEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	nowMs := time.Now().UnixMilli()
	// A week is long enough for every discrete venue to have settled at least
	// a dozen times, and short enough that Gate's 180-day limit and OKX's
	// three-month retention are both irrelevant to the recording.
	startMs := nowMs - 7*24*exchanges.SecPerHour*exchanges.MsPerSecond

	write := func(name string, raw []byte) {
		t.Helper()
		if err := os.WriteFile(fixturePath(t, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %s (%d bytes)", name, len(raw))
	}

	for _, venue := range []struct {
		file   string
		source string
		url    string
		post   string
		trim   func(*testing.T, []byte) []byte
	}{
		{
			file:   "funding_history_binance.json",
			source: "binance_futures",
			url: fmt.Sprintf("https://fapi.binance.com/fapi/v1/fundingRate?symbol=%s&startTime=%d&endTime=%d&limit=%d",
				captureVenueSymbol(t, "binance_futures"), startMs, nowMs, 20),
		},
		{
			file:   "funding_history_bybit.json",
			source: "bybit_futures",
			url: fmt.Sprintf("https://api.bybit.com/v5/market/funding/history?category=linear&symbol=%s&startTime=%d&endTime=%d&limit=%d",
				captureVenueSymbol(t, "bybit_futures"), startMs, nowMs, 20),
		},
		{
			file:   "funding_history_okx.json",
			source: "okx_futures",
			url: fmt.Sprintf("https://www.okx.com/api/v5/public/funding-rate-history?instId=%s&limit=%d&after=%d",
				captureVenueSymbol(t, "okx_futures"), 20, nowMs),
		},
		{
			file:   "funding_history_gate.json",
			source: "gate_futures",
			url: fmt.Sprintf("https://api.gateio.ws/api/v4/futures/%s/funding_rate?contract=%s&limit=%d&from=%d&to=%d",
				"usdt", captureVenueSymbol(t, "gate_futures"), 20, startMs/exchanges.MsPerSecond, nowMs/exchanges.MsPerSecond),
		},
		{
			file:   "funding_history_kraken.json",
			source: "kraken_futures",
			url: "https://futures.kraken.com/derivatives/api/v4/historicalfundingrates?symbol=" +
				captureVenueSymbol(t, "kraken_futures"),
			trim: trimKrakenFundingHistory,
		},
		{
			file:   "funding_history_hyperliquid.json",
			source: "hyperliquid_futures",
			url:    "https://api.hyperliquid.xyz/info",
			post: fmt.Sprintf(`{"type":"fundingHistory","coin":%q,"startTime":%d,"endTime":%d}`,
				captureVenueSymbol(t, "hyperliquid_futures"), nowMs-24*exchanges.SecPerHour*exchanges.MsPerSecond, nowMs),
		},
		{
			// Two hour boundaries, recorded as two separate responses would
			// arrive — enough to prove the parser reads one sample per request
			// and that the five-second stamps are kept verbatim.
			file:   "funding_history_paradex.json",
			source: "paradex_futures",
			url: fmt.Sprintf("https://api.prod.paradex.trade/v1/funding/data?market=%s&end_at=%d&page_size=%d",
				captureVenueSymbol(t, "paradex_futures"),
				(nowMs/(exchanges.SecPerHour*exchanges.MsPerSecond))*exchanges.SecPerHour*exchanges.MsPerSecond, 1),
		},
	} {
		t.Run(venue.source, func(t *testing.T) {
			raw := captureHTTP(t, ctx, venue.url, venue.post)
			if venue.trim != nil {
				raw = venue.trim(t, raw)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, raw); err != nil {
				t.Fatalf("%s: response is not JSON: %v", venue.source, err)
			}
			write(venue.file, compact.Bytes())
		})
	}
}

// captureVenueSymbol is the BTC identifier the scanner uses at one source, so a
// fixture can only ever be recorded for a market this scanner follows.
func captureVenueSymbol(t *testing.T, source string) string {
	t.Helper()
	for _, s := range exchangestest.Symbols(source) {
		if s.Standard == "BTCUSDT" {
			return s.Venue
		}
	}
	t.Fatalf("%s has no BTCUSDT in the symbol table", source)
	return ""
}

func captureHTTP(t *testing.T, ctx context.Context, url, post string) []byte {
	t.Helper()

	method, body := http.MethodGet, io.Reader(nil)
	if post != "" {
		method, body = http.MethodPost, strings.NewReader(post)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if post != "" {
		req.Header.Set("Content-Type", "application/json")
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

// krakenFundingHistoryTailRows is how much of Kraken's year-long response is
// kept. The whole thing is 8,772 rows and about a megabyte; the newest few
// hundred are enough for the golden test to re-measure the hourly cadence,
// which is the reason this fixture exists at all (step 2.3 debt).
const krakenFundingHistoryTailRows = 400

func trimKrakenFundingHistory(t *testing.T, raw []byte) []byte {
	t.Helper()

	var resp struct {
		Result     string            `json:"result"`
		ServerTime string            `json:"serverTime"`
		Rates      []json.RawMessage `json:"rates"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Rates) > krakenFundingHistoryTailRows {
		resp.Rates = resp.Rates[len(resp.Rates)-krakenFundingHistoryTailRows:]
	}
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
