package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// The three-year backtest report the Backtest tab draws.
//
// The file is built OUTSIDE this process by tools/report/bt3y.py, which replays
// the auto-trader's own 4.5f rules over a COPY of the corpus. Nothing here
// computes a rule or a profit figure: the portal only hands the browser bytes
// that were already written to disk, exactly as the Paper tab only relays
// cmd/paperledger. Keeping the replay out of the portal matters for two
// reasons — the portal is the binary that holds a credential, and a three-year
// replay must never run on the request path of a page that also sends orders.
//
// The report is served from memory and re-read only when the file's modification
// time or size changes, so a page polling this endpoint costs a stat() and no
// venue weight at all.

// maxBacktestBytes bounds what will be held in memory. The measured report is
// well under a megabyte; the ceiling exists so a mistake upstream — a sweep
// written to the wrong path — cannot make the order portal swap.
const maxBacktestBytes = 64 << 20

// backtestFile caches one JSON report and the stat that proves it current.
type backtestFile struct {
	mu      sync.Mutex
	path    string
	modTime time.Time
	size    int64
	body    []byte
}

func newBacktestFile(path string) *backtestFile { return &backtestFile{path: path} }

// load returns the report's bytes, re-reading only when the file changed.
func (b *backtestFile) load() ([]byte, time.Time, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.path == "" {
		return nil, time.Time{}, fmt.Errorf("chưa cấu hình đường dẫn báo cáo backtest")
	}
	st, err := os.Stat(b.path)
	if err != nil {
		b.body = nil
		return nil, time.Time{}, err
	}
	if st.IsDir() {
		b.body = nil
		return nil, time.Time{}, fmt.Errorf("%s là thư mục, không phải tệp báo cáo", b.path)
	}
	if st.Size() > maxBacktestBytes {
		b.body = nil
		return nil, time.Time{}, fmt.Errorf("báo cáo %.1f MB vượt trần %d MB — không nạp vào tiến trình đang giữ credential",
			float64(st.Size())/(1<<20), maxBacktestBytes>>20)
	}
	if b.body != nil && st.ModTime().Equal(b.modTime) && st.Size() == b.size {
		return b.body, b.modTime, nil
	}
	raw, err := os.ReadFile(b.path)
	if err != nil {
		b.body = nil
		return nil, time.Time{}, err
	}
	// A half-written file is the expected failure here: the report is rebuilt
	// by a separate process and the page may ask while it is mid-write. Saying
	// so beats handing the browser a truncated object to choke on.
	if !json.Valid(raw) {
		b.body = nil
		return nil, time.Time{}, fmt.Errorf("tệp báo cáo không phải JSON hợp lệ (có thể đang được ghi dở) — dựng lại rồi thử lại")
	}
	b.body, b.modTime, b.size = raw, st.ModTime(), st.Size()
	return b.body, b.modTime, nil
}

// handleBacktest serves the report, or names why it cannot.
func (p *portal) handleBacktest(w http.ResponseWriter, r *http.Request) {
	body, modTime, err := p.backtest.load()
	if err != nil {
		// 503 rather than 404: the endpoint exists and the page should keep
		// its tab, it is the artefact that is not ready.
		writeError(w, http.StatusServiceUnavailable, "backtest_unavailable",
			"chưa đọc được báo cáo backtest 3 năm: "+err.Error()+
				" — dựng bằng: python3 tools/report/bt3y.py --db <bản sao corpus> --out docs/reports/backtest-3y-latest.json")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Last-Modified", modTime.UTC().Format(http.TimeFormat))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}
