package hyperliquid

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// A rate with no interval is not a reading: publishing one would mean dividing
// by an interval nobody supplied. Hyperliquid is the venue where this happens
// in production, while its REST metadata is still being fetched.
func TestHyperliquidFunding_NoMetaNoReading(t *testing.T) {
	frame := []byte(`{"channel":"activeAssetCtx","data":{"coin":"BTC","ctx":{` +
		`"funding":"0.0000105002","markPx":"80808.0","oraclePx":"80842.7"}}}`)
	symbols := []exchanges.Symbol{{Standard: "BTCUSDT", Venue: "BTC"}}
	recvAt := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)

	r := exchangestest.NewRecorder(t)
	empty := exchanges.NewFundingMetaCache()
	if handled, _ := handleHyperliquidFunding("hyperliquid_futures", symbols, empty, r.Feeds, frame, recvAt); !handled {
		t.Fatal("the frame should be recognised as activeAssetCtx even with no metadata")
	}
	if got := r.Fundings(); len(got) != 0 {
		t.Fatalf("published %d readings with no interval known: %+v", len(got), got)
	}

	// With the metadata, the same frame becomes a reading — and the settlement
	// stamp is the venue's field PLUS one interval, because predictedFundings
	// publishes the period already running (measured across an hour boundary
	// 2026-09-04, see hyperliquid_funding_ws.go).
	currentPeriodMs := recvAt.Add(-30 * time.Minute).UnixMilli()
	meta := exchanges.NewFundingMetaCache()
	meta.Put(map[string]exchanges.FundingMetaEntry{"BTCUSDT": {IntervalHours: 1, NextFundingAtMs: currentPeriodMs}})
	_, _ = handleHyperliquidFunding("hyperliquid_futures", symbols, meta, r.Feeds, frame, recvAt)
	got := r.Fundings()
	if len(got) != 1 || got[0].IntervalSec != 3600 {
		t.Fatalf("with metadata, got %+v; want one hourly reading", got)
	}
	wantStamp := currentPeriodMs + 3600*1000
	if got[0].NextFundingAtMs != wantStamp {
		t.Errorf("NextFundingAtMs = %d, want the venue's stamp plus one interval %d — taking it at face value publishes a settlement that already happened",
			got[0].NextFundingAtMs, wantStamp)
	}

	// And a stamp so old that even the correction leaves it in the past must
	// be dropped rather than published.
	stale := exchanges.NewFundingMetaCache()
	stale.Put(map[string]exchanges.FundingMetaEntry{"BTCUSDT": {IntervalHours: 1, NextFundingAtMs: recvAt.Add(-3 * time.Hour).UnixMilli()}})
	_, _ = handleHyperliquidFunding("hyperliquid_futures", symbols, stale, r.Feeds, frame, recvAt)
	got = r.Fundings()
	if len(got) != 1 || got[0].NextFundingAtMs != 0 {
		t.Fatalf("a stamp still in the past after correction must be dropped, got %+v", got)
	}
}

// Hyperliquid's predictedFundings lists competitors beside itself. Reading the
// wrong row would annualize an 8h venue's cadence onto an hourly one.
func TestParseHyperliquidPredictedFundings(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "funding_hyperliquid_predicted.json"))
	if err != nil {
		t.Fatalf("missing recording: %v (re-record with %s=1)", err, exchangestest.CaptureEnv)
	}
	var rows [][]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("recording does not Decode: %v", err)
	}

	symbols := []exchanges.Symbol{{Standard: "BTCUSDT", Venue: "BTC"}, {Standard: "ETHUSDT", Venue: "ETH"}}
	meta, err := parseHyperliquidPredictedFundings(rows, symbols)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range symbols {
		entry, ok := meta[s.Standard]
		if !ok {
			t.Fatalf("%s missing from the parsed metadata", s.Standard)
		}
		// HlPerp settles hourly; BinPerp and BybitPerp in the same rows are 8h.
		if entry.IntervalHours != 1 {
			t.Errorf("%s: IntervalHours = %d, want Hyperliquid's own 1 — an 8h value means a competitor's row was read",
				s.Standard, entry.IntervalHours)
		}
		if entry.NextFundingAtMs == 0 {
			t.Errorf("%s: predictedFundings carries nextFundingTime", s.Standard)
		}
	}

	// A payload with only competitors must be an error, not a silent empty map
	// that leaves every symbol without an interval forever.
	onlyOthers := [][]any{{"BTC", []any{[]any{"BinPerp", map[string]any{"fundingIntervalHours": float64(8)}}}}}
	if _, err := parseHyperliquidPredictedFundings(onlyOthers, symbols); err == nil {
		t.Error("a response with no HlPerp row must be reported, not accepted as empty")
	}
}
