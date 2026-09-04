package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/store"
)

// The dashboard's read-only HTTP API (step 2.7). Documented in
// docs/WS-CONTRACT.md §10 alongside the WebSocket contract, because it is the
// same backend↔dashboard agreement and breaking it breaks the same page.
//
// It exists because the funding history chart cannot come over the WebSocket.
// The history lives in SQLite, internal/scanner does not know internal/store
// and must not learn - a push contract is the wrong shape for "give me thirty
// days when the user asks" anyway. So the entrypoint, which already owns the
// store, serves it.

// fundingHistoryMaxDays bounds one request. A year of Kraken and Hyperliquid is
// 8,760 hourly settlements each, so an unbounded window would let one URL ask
// for tens of megabytes of JSON.
const fundingHistoryMaxDays = 400

// fundingHistoryDefaultDays is what a request that names no window gets.
const fundingHistoryDefaultDays = 30

type apiFundingPoint struct {
	FundingAtMs  int64   `json:"funding_at_ms"`
	RatePer8hBps float64 `json:"rate_per_8h_bps"`
	APRGrossPct  float64 `json:"apr_gross_pct"`
	IntervalSec  int64   `json:"interval_sec"`
	// GapPrevSec is the MEASURED distance to the previous settlement. It differs
	// from IntervalSec exactly where a settlement was missed or the venue
	// changed cadence, and it is the only honest figure for those rows.
	GapPrevSec int64  `json:"gap_prev_sec"`
	RateType   string `json:"rate_type"`
}

type apiFundingSeries struct {
	Source string `json:"source"`
	Model  string `json:"model"`
	// IntervalSec of the newest row, for the legend. Every point carries its
	// own, because a series can span a cadence change.
	IntervalSec int64             `json:"interval_sec"`
	Points      []apiFundingPoint `json:"points"`
}

// apiCoverage is what the corpus actually holds for this pair, per venue.
//
// It ships WITH the series and not on request, because the depth is not uniform
// and never will be: OKX keeps about three months and Kraken a year, so a chart
// drawn without it shows one venue's line stopping and invites the reading that
// the venue stopped paying funding.
type apiCoverage struct {
	Source     string `json:"source"`
	Model      string `json:"model"`
	Rows       int    `json:"rows"`
	OldestAtMs int64  `json:"oldest_at_ms"`
	NewestAtMs int64  `json:"newest_at_ms"`
}

type apiFundingHistory struct {
	Symbol   string             `json:"symbol"`
	FromMs   int64              `json:"from_ms"`
	ToMs     int64              `json:"to_ms"`
	NoteVI   string             `json:"note_vi"`
	Series   []apiFundingSeries `json:"series"`
	Coverage []apiCoverage      `json:"coverage"`
}

// newFundingHistoryHandler serves settled funding history for one pair.
//
// db is nil when persistence is switched off, and the handler then says so
// rather than returning an empty chart - "no rows" and "no database" look
// identical on a graph and mean completely different things.
func newFundingHistoryHandler(db *store.Store, cfg config.Config) http.HandlerFunc {
	configured := make(map[string]bool, len(cfg.Symbols))
	for _, symbol := range cfg.Symbols {
		configured[symbol.Symbol] = true
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			writeAPIError(w, http.StatusServiceUnavailable,
				"Chưa bật lưu trữ (storage.enabled=false) nên không có lịch sử funding để vẽ.")
			return
		}
		symbol := r.URL.Query().Get("symbol")
		if !configured[symbol] {
			writeAPIError(w, http.StatusBadRequest,
				"Tham số symbol phải là một cặp có trong config.yaml, nhận được: "+strconv.Quote(symbol))
			return
		}
		days, err := historyDays(r.URL.Query().Get("days"))
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}

		now := time.Now()
		fromMs := now.AddDate(0, 0, -days).UnixMilli()
		toMs := now.UnixMilli()

		rows, err := db.FundingHistory(r.Context(), symbol, fromMs, toMs)
		if err != nil {
			log.Printf("api: funding history %s: %v", symbol, err)
			writeAPIError(w, http.StatusInternalServerError, "Không đọc được lịch sử funding.")
			return
		}
		coverage, err := db.FundingCoverage(r.Context())
		if err != nil {
			log.Printf("api: funding coverage: %v", err)
			writeAPIError(w, http.StatusInternalServerError, "Không đọc được độ phủ của kho dữ liệu.")
			return
		}

		writeJSON(w, http.StatusOK, apiFundingHistory{
			Symbol: symbol,
			FromMs: fromMs,
			ToMs:   toMs,
			NoteVI: "Đây là các mốc ĐÃ SETTLE, khác với số realtime trên bảng — số realtime là " +
				"của một kỳ ĐANG CHẠY và còn đổi. Tất cả đều THÔ: chưa trừ phí, slippage hay " +
				"chi phí vay. Kho dữ liệu KHÔNG đều nhau giữa các sàn, xem coverage.",
			Series:   seriesFromRows(rows, symbol),
			Coverage: coverageFor(coverage, symbol),
		})
	}
}

// historyDays validates the window. An unparsable or out-of-range value is
// refused rather than silently clamped: a dashboard asking for 5,000 days has a
// bug, and answering it with 400 days of data hides that.
func historyDays(raw string) (int, error) {
	if raw == "" {
		return fundingHistoryDefaultDays, nil
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 1 || days > fundingHistoryMaxDays {
		return 0, apiErrorf("Tham số days phải là số nguyên từ 1 đến %d, nhận được %s",
			fundingHistoryMaxDays, strconv.Quote(raw))
	}
	return days, nil
}

// seriesFromRows groups the store's flat rows by source, keeping the store's
// ordering (settlement time, then source) inside each series.
func seriesFromRows(rows []store.FundingRow, symbol string) []apiFundingSeries {
	bySource := make(map[string]*apiFundingSeries)
	var order []string
	for _, row := range rows {
		if row.Symbol != symbol {
			continue
		}
		series, ok := bySource[row.Source]
		if !ok {
			series = &apiFundingSeries{Source: row.Source, Model: string(row.Model)}
			bySource[row.Source] = series
			order = append(order, row.Source)
		}
		series.IntervalSec = row.IntervalSec // rows arrive oldest first; the last write is the newest
		series.Points = append(series.Points, apiFundingPoint{
			FundingAtMs:  row.SettledAtMs,
			RatePer8hBps: row.RatePer8hFrac * bpsPerUnit,
			APRGrossPct:  row.APRFrac * pctPerUnit,
			IntervalSec:  row.IntervalSec,
			GapPrevSec:   row.GapPrevSec,
			RateType:     row.RateType,
		})
	}

	out := make([]apiFundingSeries, 0, len(order))
	for _, source := range order {
		out = append(out, *bySource[source])
	}
	return out
}

func coverageFor(coverage []store.Coverage, symbol string) []apiCoverage {
	out := make([]apiCoverage, 0, len(coverage))
	for _, c := range coverage {
		if c.Symbol != symbol {
			continue
		}
		out = append(out, apiCoverage{
			Source:     c.Source,
			Model:      string(c.Model),
			Rows:       c.Rows,
			OldestAtMs: c.OldestAtMs,
			NewestAtMs: c.NewestAtMs,
		})
	}
	return out
}

// bpsPerUnit and pctPerUnit convert the stored FRACTIONS to the units the wire
// speaks, exactly as internal/scanner does for the live table. Kept identical
// on purpose: the chart and the table must not disagree about what a bps is.
const (
	bpsPerUnit = 10000
	pctPerUnit = 100
)

type apiError struct {
	ErrorVI string `json:"error_vi"`
}

func (e apiError) Error() string { return e.ErrorVI }

func apiErrorf(format string, args ...any) error {
	return apiError{ErrorVI: fmt.Sprintf(format, args...)}
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, apiError{ErrorVI: message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so this can only be logged.
		log.Printf("api: write response: %v", err)
	}
}
