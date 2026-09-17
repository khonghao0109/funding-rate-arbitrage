package main

import (
	"context"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/scanner"
	"futures-arbitrage-scanner/internal/store"
)

// The cross-venue funding radar's HTTP routes and its episode log (PLAN 4.5i,
// direction 1). READ-ONLY with respect to every venue: the radar reads what the
// scanner already holds, and the log writes only to this process's own store.
// Documented in docs/WS-CONTRACT.md §12, after the funding history route (§10).

// crossEventsMaxDays bounds one events request.
const (
	crossEventsMaxDays     = 90
	crossEventsDefaultDays = 7
	crossEventsMaxRows     = 5000
)

// newCrossRadarHandler serves the radar as it stands right now.
func newCrossRadarHandler(s *scanner.Scanner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeAPIError(w, http.StatusMethodNotAllowed, "Endpoint này chỉ nhận GET, nhận được: "+r.Method)
			return
		}
		writeJSON(w, http.StatusOK, s.CrossRadar())
	}
}

// apiCrossEvent is one logged episode on the wire.
type apiCrossEvent struct {
	Symbol          string  `json:"symbol"`
	ThresholdAPRPct float64 `json:"threshold_apr_pct"`
	Direction       string  `json:"direction"`
	ShortSource     string  `json:"short_source"`
	LongSource      string  `json:"long_source"`
	StartedAtMs     int64   `json:"started_at_ms"`
	EndedAtMs       int64   `json:"ended_at_ms"` // 0 while open
	DurationSec     float64 `json:"duration_sec"`
	EndReason       string  `json:"end_reason"`
	PeakGrossAPRPct float64 `json:"peak_gross_apr_pct"`
	SampleEverySec  int64   `json:"sample_every_sec"`
}

// apiCrossSummary is one threshold's statistics over the window. Durations are
// taken over episodes that ended because the SPREAD ended (below or flip) —
// an episode cut by stale data or a restart measured the feed, not the market,
// and is counted separately rather than mixed in.
type apiCrossSummary struct {
	ThresholdAPRPct float64        `json:"threshold_apr_pct"`
	Episodes        int            `json:"episodes"`
	Open            int            `json:"open"`
	ByReason        map[string]int `json:"by_reason"`
	MeasuredCount   int            `json:"measured_count"`
	// Censored episodes were cut by stale data or a restart: their duration is a
	// LOWER BOUND on how long the spread lasted, so they are reported beside the
	// measured ones rather than dropped (dropping them biases durations short,
	// because the longest episodes are the likeliest to cross a gap).
	CensoredCount          int            `json:"censored_count"`
	CensoredMedianLowerSec *float64       `json:"censored_median_lower_bound_sec"`
	MedianSec              *float64       `json:"median_duration_sec"`
	MeanSec                *float64       `json:"mean_duration_sec"`
	P90Sec                 *float64       `json:"p90_duration_sec"`
	MaxSec                 *float64       `json:"max_duration_sec"`
	BySymbol               map[string]int `json:"episodes_by_symbol"`
}

type apiCrossEvents struct {
	FromMs    int64             `json:"from_ms"`
	ToMs      int64             `json:"to_ms"`
	RateModel string            `json:"rate_model"`
	NoteVI    string            `json:"note_vi"`
	Truncated bool              `json:"truncated"`
	Summary   []apiCrossSummary `json:"summary"`
	Events    []apiCrossEvent   `json:"events"`
}

// newCrossEventsHandler serves the episode log with per-threshold statistics.
func newCrossEventsHandler(db *store.Store, s *scanner.Scanner, writer string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeAPIError(w, http.StatusMethodNotAllowed, "Endpoint này chỉ nhận GET, nhận được: "+r.Method)
			return
		}
		if db == nil {
			writeAPIError(w, http.StatusServiceUnavailable,
				"Chưa bật lưu trữ (storage.enabled=false) nên không có nhật ký đợt chênh.")
			return
		}
		days := crossEventsDefaultDays
		if raw := r.URL.Query().Get("days"); raw != "" {
			d, err := strconv.Atoi(raw)
			if err != nil || d < 1 || d > crossEventsMaxDays {
				writeAPIError(w, http.StatusBadRequest,
					"Tham số days phải là số nguyên từ 1 đến "+strconv.Itoa(crossEventsMaxDays)+", nhận được "+strconv.Quote(raw))
				return
			}
			days = d
		}
		now := time.Now()
		fromMs := now.AddDate(0, 0, -days).UnixMilli()
		rows, err := db.CrossSpreadEvents(r.Context(), writer, fromMs, crossEventsMaxRows+1)
		if err != nil {
			log.Printf("api: cross events: %v", err)
			writeAPIError(w, http.StatusInternalServerError, "Không đọc được nhật ký đợt chênh.")
			return
		}
		truncated := len(rows) > crossEventsMaxRows
		if truncated {
			rows = rows[:crossEventsMaxRows]
		}
		note := "Đợt chênh đo trên funding ĐANG HÌNH THÀNH, lấy mẫu mỗi vài giây — không phải mốc đã settle. " +
			"Một đợt có thể mở và đóng trong cùng một kỳ và không trả đồng nào. Thời lượng thống kê chỉ lấy các đợt " +
			"kết thúc vì CHÊNH hết (below, flip); đợt bị cắt vì dữ liệu cũ (stale) hay khởi động lại (restart) là CẬN DƯỚI, báo riêng. " +
			"Chỉ các đợt do " + writer + " ghi."
		if truncated {
			note += " Danh sách và thống kê chỉ gồm " + strconv.Itoa(crossEventsMaxRows) + " đợt mới nhất trong cửa sổ."
		}
		writeJSON(w, http.StatusOK, apiCrossEvents{
			FromMs: fromMs, ToMs: now.UnixMilli(), RateModel: "forming_gross", Truncated: truncated,
			NoteVI:  note,
			Summary: summarizeCrossEvents(rows, s.CrossRadarThresholds()),
			Events:  wireCrossEvents(rows),
		})
	}
}

func wireCrossEvents(rows []store.CrossSpreadEvent) []apiCrossEvent {
	out := make([]apiCrossEvent, 0, len(rows))
	for _, e := range rows {
		out = append(out, apiCrossEvent{
			Symbol: e.Symbol, ThresholdAPRPct: e.ThresholdAPRPct,
			Direction:   scanner.CrossDirection(e.ShortSource, e.LongSource),
			ShortSource: e.ShortSource, LongSource: e.LongSource,
			StartedAtMs: e.StartedAtMs, EndedAtMs: e.EndedAtMs, DurationSec: e.DurationSec,
			EndReason: e.EndReason, PeakGrossAPRPct: e.PeakGrossAPRPct, SampleEverySec: e.SampleEverySec,
		})
	}
	return out
}

func summarizeCrossEvents(rows []store.CrossSpreadEvent, configured []float64) []apiCrossSummary {
	// Every configured threshold, plus any a row was logged under before the
	// config changed — a list that shows those rows must summarize them too.
	seen := map[float64]bool{}
	thresholds := []float64{}
	for _, th := range configured {
		if !seen[th] {
			seen[th] = true
			thresholds = append(thresholds, th)
		}
	}
	for _, e := range rows {
		if !seen[e.ThresholdAPRPct] {
			seen[e.ThresholdAPRPct] = true
			thresholds = append(thresholds, e.ThresholdAPRPct)
		}
	}
	sort.Float64s(thresholds)
	out := make([]apiCrossSummary, 0, len(thresholds))
	for _, th := range thresholds {
		sum := apiCrossSummary{ThresholdAPRPct: th, ByReason: map[string]int{}, BySymbol: map[string]int{}}
		var durations, censored []float64
		for _, e := range rows {
			if e.ThresholdAPRPct != th {
				continue
			}
			sum.Episodes++
			sum.BySymbol[e.Symbol]++
			if e.EndedAtMs == 0 {
				sum.Open++
				continue
			}
			sum.ByReason[e.EndReason]++
			switch e.EndReason {
			case scanner.CrossEndBelow, scanner.CrossEndFlip:
				durations = append(durations, e.DurationSec)
			case scanner.CrossEndStale, scanner.CrossEndRestart:
				censored = append(censored, e.DurationSec)
			}
		}
		sum.MeasuredCount = len(durations)
		sum.CensoredCount = len(censored)
		if len(censored) > 0 {
			sort.Float64s(censored)
			lower := quantile(censored, 0.5)
			sum.CensoredMedianLowerSec = &lower
		}
		if len(durations) > 0 {
			sort.Float64s(durations)
			total := 0.0
			for _, d := range durations {
				total += d
			}
			median, mean := quantile(durations, 0.5), total/float64(len(durations))
			p90, maxV := quantile(durations, 0.9), durations[len(durations)-1]
			sum.MedianSec, sum.MeanSec, sum.P90Sec, sum.MaxSec = &median, &mean, &p90, &maxV
		}
		out = append(out, sum)
	}
	return out
}

// quantile is linear interpolation between order statistics of sorted values.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

// startCrossEvents runs the episode log on the scanner's fixed-period
// scheduler, so its late ticks are counted like every other job's.
// crossWriteEvery bounds how often an OPEN episode is rewritten when nothing
// but its last-seen stamp moved: a restart then closes it at most this late.
const crossWriteEvery = time.Minute

// crossWriter names this process in the event log. The port is what tells two
// scanners on one machine apart.
func crossWriter(port string) string { return "scanner:" + port }

func startCrossEvents(ctx context.Context, cfg config.Config, s *scanner.Scanner, db *store.Store, writer string, start func(func())) {
	rc := cfg.CrossRadar
	if db != nil {
		// Before the enabled check: a run that crashed with episodes open and
		// was then restarted with the radar off must still not leave them
		// counted as open forever. The writer is the PORT, so a relaunch on
		// another port cannot close them — documented in schema.sql.
		if n, err := db.CloseOpenCrossSpreadEvents(ctx, writer); err != nil {
			log.Printf("cross radar: %v", err)
		} else if n > 0 {
			log.Printf("cross radar: closed %d episode(s) a previous run of %s left open, reason 'restart'", n, writer)
		}
	}
	if !rc.Enabled {
		log.Printf("cross radar: disabled")
		return
	}
	if db == nil {
		log.Printf("cross radar: storage is off — the radar is served, but no episode is logged")
		return
	}
	every := time.Duration(rc.SampleEverySec) * time.Second
	tracker := scanner.NewCrossEventTracker(rc.EventThresholdsGrossAPRPct,
		time.Duration(rc.EventEndBelowSec)*time.Second, rc.SampleEverySec)
	log.Printf("cross radar: %s vs %s, events at %v%% gross APR, sampled every %s, closed after %ds below, logged as %s",
		rc.SourceA, rc.SourceB, rc.EventThresholdsGrossAPRPct, every, rc.EventEndBelowSec, writer)
	lastWritten := map[string]crossWritten{}
	// pendingClosed holds closes whose write failed: the tracker has already
	// forgotten them, so they are retried here or the row stays open until a
	// restart closes it under the wrong reason.
	var pendingClosed []store.CrossSpreadEvent

	start(func() {
		tickLoop(ctx, "cross radar: events", every, every, func(time.Time) {
			tr := tracker.Observe(s.CrossRadar())
			rows := append([]store.CrossSpreadEvent{}, pendingClosed...)
			nowMs := time.Now().UnixMilli()
			written := map[string]crossWritten{}
			for _, e := range tr.Opened {
				rows = append(rows, crossEventRow(writer, e))
				written[crossKey(e)] = crossWritten{atMs: nowMs, peak: e.PeakGrossAPRPct}
			}
			for _, e := range tr.Updated {
				// A new peak is written at once; a moving last-seen stamp only
				// every crossWriteEvery.
				if w, ok := lastWritten[crossKey(e)]; ok && e.PeakGrossAPRPct == w.peak && nowMs-w.atMs < crossWriteEvery.Milliseconds() {
					continue
				}
				rows = append(rows, crossEventRow(writer, e))
				written[crossKey(e)] = crossWritten{atMs: nowMs, peak: e.PeakGrossAPRPct}
			}
			closedRows := make([]store.CrossSpreadEvent, 0, len(tr.Closed))
			for _, e := range tr.Closed {
				closedRows = append(closedRows, crossEventRow(writer, e))
			}
			rows = append(rows, closedRows...)
			for _, e := range tr.Opened {
				log.Printf("cross radar: đợt MỞ %s ≥ %g%% — %s, chênh %.1f%%/năm",
					e.Symbol, e.ThresholdAPRPct, scanner.CrossDirection(e.ShortSource, e.LongSource), e.PeakGrossAPRPct)
			}
			for _, e := range tr.Closed {
				log.Printf("cross radar: đợt ĐÓNG %s ≥ %g%% — %s sau %.0fs, đỉnh %.1f%%/năm",
					e.Symbol, e.ThresholdAPRPct, e.EndReason, e.DurationSec(), e.PeakGrossAPRPct)
			}
			if err := db.PutCrossSpreadEvents(ctx, rows); err != nil {
				log.Printf("cross radar: %v — %d close(s) kept for the next tick", err, len(pendingClosed)+len(closedRows))
				pendingClosed = append(pendingClosed, closedRows...)
				// Opens and updates are rewritten on a later tick anyway: nothing
				// is recorded as written.
				return
			}
			pendingClosed = nil
			for k, w := range written {
				lastWritten[k] = w
			}
			for _, e := range tr.Closed {
				delete(lastWritten, crossKey(e))
			}
		})
	})
}

type crossWritten struct {
	atMs int64
	peak float64
}

func crossKey(e scanner.CrossEpisode) string {
	return e.Symbol + "|" + strconv.FormatFloat(e.ThresholdAPRPct, 'g', -1, 64) + "|" + strconv.FormatInt(e.StartedAtMs, 10)
}

func crossEventRow(writer string, e scanner.CrossEpisode) store.CrossSpreadEvent {
	return store.CrossSpreadEvent{
		Writer: writer, Symbol: e.Symbol, ThresholdAPRPct: e.ThresholdAPRPct, StartedAtMs: e.StartedAtMs,
		ShortSource: e.ShortSource, LongSource: e.LongSource,
		PeakGrossAPRPct: e.PeakGrossAPRPct, PeakAtMs: e.PeakAtMs, LastSeenAboveMs: e.LastSeenAboveMs,
		EndedAtMs: e.EndedAtMs, DurationSec: e.DurationSec(), EndReason: e.EndReason,
		SampleEverySec: e.SampleEverySec, RateModel: "forming_gross",
	}
}
