package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/store"
)

func apiStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedFunding writes a short series so the handler has something to group.
func seedFunding(t *testing.T, db *store.Store, source string, model exchanges.FundingModel, settlements int, spacing time.Duration) {
	t.Helper()
	entries := make([]exchanges.FundingHistoryEntry, 0, settlements)
	for i := 0; i < settlements; i++ {
		at := time.Now().Add(-time.Duration(settlements-i) * spacing)
		entries = append(entries, exchanges.FundingHistoryEntry{
			Symbol: "BTCUSDT", Source: source, Model: model,
			SettledAtMs:         at.UnixMilli(),
			RawRate:             0.0001,
			RawRateField:        "fundingRate",
			RatePerIntervalFrac: 0.0001,
			IntervalSec:         int64(spacing / time.Second),
			RatePer8hFrac:       0.0001,
			APRFrac:             0.1095,
			GapPrevSec:          int64(spacing / time.Second),
		})
	}
	if _, err := db.PutFundingHistory(context.Background(), entries); err != nil {
		t.Fatalf("seed %s: %v", source, err)
	}
}

func getHistory(t *testing.T, handler http.HandlerFunc, query string) (*httptest.ResponseRecorder, apiFundingHistory) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodGet, "/api/funding/history"+query, nil))

	var body apiFundingHistory
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v (%s)", err, recorder.Body.String())
		}
	}
	return recorder, body
}

// "No database" and "no rows" render as the same empty chart and mean entirely
// different things, so the handler has to say which.
func TestFundingHistoryHandler_SaysSoWhenPersistenceIsOff(t *testing.T) {
	handler := newFundingHistoryHandler(nil, repoConfig(t))

	recorder, _ := getHistory(t, handler, "?symbol=BTCUSDT")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	var body apiError
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.ErrorVI == "" {
		t.Fatalf("a disabled store must explain itself, got %q", recorder.Body.String())
	}
}

func TestFundingHistoryHandler_RefusesAPairNobodyConfigured(t *testing.T) {
	handler := newFundingHistoryHandler(apiStore(t), repoConfig(t))

	for _, query := range []string{"", "?symbol=", "?symbol=NOSUCHUSDT"} {
		recorder, _ := getHistory(t, handler, query)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", query, recorder.Code)
		}
	}
}

// An out-of-range window is refused rather than clamped: a dashboard asking for
// five thousand days has a bug, and answering it with 400 days hides it.
func TestFundingHistoryHandler_RefusesAnImpossibleWindow(t *testing.T) {
	handler := newFundingHistoryHandler(apiStore(t), repoConfig(t))

	for _, query := range []string{"?symbol=BTCUSDT&days=0", "?symbol=BTCUSDT&days=5000", "?symbol=BTCUSDT&days=soon"} {
		recorder, _ := getHistory(t, handler, query)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", query, recorder.Code)
		}
	}
}

func TestFundingHistoryHandler_GroupsRowsIntoOneSeriesPerVenue(t *testing.T) {
	db := apiStore(t)
	seedFunding(t, db, "binance_futures", exchanges.FundingDiscrete, 3, 8*time.Hour)
	seedFunding(t, db, "kraken_futures", exchanges.FundingDiscrete, 24, time.Hour)
	handler := newFundingHistoryHandler(db, repoConfig(t))

	recorder, body := getHistory(t, handler, "?symbol=BTCUSDT&days=30")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(body.Series) != 2 {
		t.Fatalf("got %d series, want one per venue", len(body.Series))
	}

	bySource := map[string]apiFundingSeries{}
	for _, series := range body.Series {
		bySource[series.Source] = series
	}
	if got := len(bySource["kraken_futures"].Points); got != 24 {
		t.Errorf("kraken series has %d points, want 24", got)
	}
	// The interval is per series and per row, never assumed: these two venues
	// settle 8 hours apart and 1 hour apart, and flattening that is the 8x
	// error CLAUDE.md rule 3 exists to prevent.
	if bySource["kraken_futures"].IntervalSec != 3600 || bySource["binance_futures"].IntervalSec != 28800 {
		t.Errorf("intervals = %d and %d, want 3600 and 1 settlement period apart",
			bySource["kraken_futures"].IntervalSec, bySource["binance_futures"].IntervalSec)
	}
	// The stored fraction becomes bps on the wire, the same conversion the live
	// table does. A chart and a table disagreeing about a bps is worse than
	// either being wrong alone.
	if got := bySource["kraken_futures"].Points[0].RatePer8hBps; got != 1 {
		t.Errorf("rate_per_8h_bps = %g, want 1 (0.0001 as bps)", got)
	}
}

// Coverage ships with the series because the corpus is deliberately uneven: OKX
// keeps about three months where Kraken keeps a year, and a line that simply
// stops reads as a venue that stopped paying funding.
func TestFundingHistoryHandler_ReportsCoverageBesideTheSeries(t *testing.T) {
	db := apiStore(t)
	seedFunding(t, db, "okx_futures", exchanges.FundingDiscrete, 3, 8*time.Hour)
	handler := newFundingHistoryHandler(db, repoConfig(t))

	_, body := getHistory(t, handler, "?symbol=BTCUSDT&days=30")
	if len(body.Coverage) != 1 || body.Coverage[0].Source != "okx_futures" {
		t.Fatalf("coverage = %+v, want one okx_futures row", body.Coverage)
	}
	if body.Coverage[0].Rows != 3 || body.Coverage[0].OldestAtMs == 0 {
		t.Errorf("coverage row is not filled in: %+v", body.Coverage[0])
	}
	if body.NoteVI == "" {
		t.Error("the response carries no note; a settled rate is not the live one and both are gross")
	}
}

// The window is honoured, not ignored: a 1-day request must not return a
// settlement from last week.
func TestFundingHistoryHandler_HonoursTheRequestedWindow(t *testing.T) {
	db := apiStore(t)
	seedFunding(t, db, "binance_futures", exchanges.FundingDiscrete, 21, 8*time.Hour) // a week
	handler := newFundingHistoryHandler(db, repoConfig(t))

	_, body := getHistory(t, handler, "?symbol=BTCUSDT&days=1")
	if len(body.Series) != 1 {
		t.Fatalf("got %d series", len(body.Series))
	}
	if got := len(body.Series[0].Points); got > 4 {
		t.Errorf("a 1-day window returned %d settlements of an 8h cadence", got)
	}
	for _, point := range body.Series[0].Points {
		if point.FundingAtMs < body.FromMs || point.FundingAtMs >= body.ToMs {
			t.Errorf("settlement %d is outside the reported window %d..%d",
				point.FundingAtMs, body.FromMs, body.ToMs)
		}
	}
}
