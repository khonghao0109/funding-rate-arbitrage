package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// backtestPortal is testPortal with the report path pointed at a temp file.
func backtestPortal(t *testing.T, path string) *portal {
	t.Helper()
	p := testPortal(t, false)
	p.backtest = newBacktestFile(path)
	return p
}

func writeReport(t *testing.T, path, body string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Set the stamp explicitly: two writes inside one filesystem tick can share
	// a modification time, and then a cache keyed on it would look correct in
	// a test for the wrong reason.
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestBacktestAPI_ServesTheReportAndRereadsItWhenItChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backtest-3y-latest.json")
	writeReport(t, path, `{"summary":{"total_trades":7}}`, time.Now().Add(-time.Hour))
	p := backtestPortal(t, path)

	rec := do(t, p, http.MethodGet, "/api/backtest", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 — %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	var first struct {
		Summary struct {
			TotalTrades int `json:"total_trades"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if first.Summary.TotalTrades != 7 {
		t.Errorf("total_trades = %d, want 7", first.Summary.TotalTrades)
	}

	// Served again from cache: the same bytes, no error.
	if rec2 := do(t, p, http.MethodGet, "/api/backtest", ""); rec2.Body.String() != rec.Body.String() {
		t.Errorf("second read differs from the first")
	}

	// A rebuild must reach the page. This is the mutation that matters: a cache
	// that never re-reads would leave the tab showing yesterday's measurement
	// for ever, which is worse than showing nothing.
	writeReport(t, path, `{"summary":{"total_trades":9}}`, time.Now())
	rec3 := do(t, p, http.MethodGet, "/api/backtest", "")
	if err := json.Unmarshal(rec3.Body.Bytes(), &first); err != nil {
		t.Fatalf("not JSON after rebuild: %v", err)
	}
	if first.Summary.TotalTrades != 9 {
		t.Errorf("after rebuild total_trades = %d, want 9 — the cache did not re-read", first.Summary.TotalTrades)
	}
}

func TestBacktestAPI_SaysWhyRatherThanServingSomethingBroken(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) string
	}{
		{"không cấu hình", func(t *testing.T) string { return "" }},
		{"tệp chưa tồn tại", func(t *testing.T) string { return filepath.Join(dir, "absent.json") }},
		{"là thư mục", func(t *testing.T) string {
			d := filepath.Join(dir, "adir.json")
			if err := os.Mkdir(d, 0o755); err != nil {
				t.Fatal(err)
			}
			return d
		}},
		{"JSON ghi dở", func(t *testing.T) string {
			p := filepath.Join(dir, "truncated.json")
			writeReport(t, p, `{"summary":{"total_tra`, time.Now())
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := backtestPortal(t, tc.setup(t))
			rec := do(t, p, http.MethodGet, "/api/backtest", "")
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("code = %d, want 503 — %s", rec.Code, rec.Body.String())
			}
			if code := errorCode(t, rec); code != "backtest_unavailable" {
				t.Errorf("error_code = %q", code)
			}
		})
	}
}

// The report is a read like any other, so it sits behind the same cross-site
// wall: a page on another origin must not be able to spend this process's time.
func TestBacktestAPI_IsBehindTheReadGuard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.json")
	writeReport(t, path, `{}`, time.Now())
	p := backtestPortal(t, path)

	rec := do(t, p, http.MethodGet, "/api/backtest", "", withHeader(actionHeader, ""))
	if rec.Code != http.StatusForbidden {
		t.Errorf("without the action header: code = %d, want 403", rec.Code)
	}
	rec = do(t, p, http.MethodGet, "/api/backtest", "", withHeader("Sec-Fetch-Site", "cross-site"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site: code = %d, want 403", rec.Code)
	}
	if rec := do(t, p, http.MethodPost, "/api/backtest", `{}`, writeOpts("open")...); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: code = %d, want 405", rec.Code)
	}
}
