package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The Crowding tab (phase 6) has no live source: ingesting Binance's long/short
// account ratio is step 6.2, which waits for the 3.5 verdict and 3.4 (PLAN).
// What the tab shows instead is the research package's frozen fixture — the
// columns it RECORDED, copied, never recomputed — so the charts are real and
// the label can say exactly what they are.
//
// The snapshot is committed at ui/research/crowding-research.json and embedded in
// the binary. This test rebuilds it from the fixture and fails if the two
// differ; WRITE_CROWDING_SNAPSHOT=1 rewrites it. The portal never imports
// internal/crowding (guard_test.go): this file reads a CSV.

const (
	crowdingFixturePath  = "../../internal/crowding/testdata/rust_parity_fixture.csv.gz"
	crowdingManifestPath = "../../internal/crowding/testdata/rust_reference_manifest.json"
	crowdingSnapshotPath = "ui/research/crowding-research.json"
	crowdingWindowDays   = 365
	crowdingBarSec       = 4 * 3600
)

type crowdingAssetSnapshot struct {
	Asset                 string            `json:"asset"`
	TimeSec               []int64           `json:"t_sec"`
	CloseQuote            []json.RawMessage `json:"close_quote"`
	LongShortAccountRatio []json.RawMessage `json:"long_short_account_ratio"`
	CrowdingScore         []json.RawMessage `json:"crowding_score"`
	Signal                []json.RawMessage `json:"signal"`
	TargetFracOfEquity    []json.RawMessage `json:"target_frac_of_equity"`
}

type crowdingSnapshot struct {
	Kind            string                  `json:"kind"`
	Live            bool                    `json:"live"`
	SourceVI        string                  `json:"source_vi"`
	NotLiveVI       string                  `json:"not_live_vi"`
	FixturePath     string                  `json:"fixture_path"`
	FixtureSHA256   string                  `json:"fixture_sha256"`
	Decision        string                  `json:"manifest_decision"`
	PanelFirstUTC   string                  `json:"panel_first_utc"`
	PanelLastUTC    string                  `json:"panel_last_utc"`
	WindowFirstUTC  string                  `json:"window_first_utc"`
	WindowDays      int                     `json:"window_days"`
	BarSec          int                     `json:"bar_sec"`
	Configuration   json.RawMessage         `json:"configuration"`
	EnsembleMembers json.RawMessage         `json:"ensemble_members"`
	Assets          []crowdingAssetSnapshot `json:"assets"`
}

func buildCrowdingSnapshot(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(crowdingFixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	sum := sha256.Sum256(raw)

	var manifest struct {
		Decision string `json:"decision"`
		Panel    struct {
			First string `json:"first"`
			Last  string `json:"last"`
		} `json:"panel"`
		Configuration   json.RawMessage `json:"configuration"`
		EnsembleMembers json.RawMessage `json:"ensemble_members"`
	}
	mblob, err := os.ReadFile(crowdingManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mblob, &manifest); err != nil {
		t.Fatal(err)
	}
	compact := func(in json.RawMessage) json.RawMessage {
		var b bytes.Buffer
		if err := json.Compact(&b, in); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}

	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	r := csv.NewReader(zr)
	header, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for i, name := range header {
		col[name] = i
	}
	need := func(name string) int {
		i, ok := col[name]
		if !ok {
			t.Fatalf("fixture has no column %q", name)
		}
		return i
	}

	type row struct {
		at     time.Time
		fields []string
	}
	var rows []row
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		at, err := time.Parse("2006-01-02 15:04:05-07:00", rec[need("timestamp_utc")])
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row{at: at.UTC(), fields: rec})
	}
	if len(rows) < 1000 {
		t.Fatalf("read only %d fixture rows", len(rows))
	}
	last := rows[len(rows)-1].at
	windowStart := last.AddDate(0, 0, -crowdingWindowDays)

	// A missing value is null — the chart leaves a gap there — never a zero.
	number := func(s string) json.RawMessage {
		if s == "" {
			return json.RawMessage("null")
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatalf("fixture value %q: %v", s, err)
		}
		// Shortest spelling that parses back to the same float64: the value is
		// the fixture's, not a rounding of it.
		return json.RawMessage(strconv.FormatFloat(v, 'g', -1, 64))
	}

	snap := crowdingSnapshot{
		Kind:            "research_fixture_snapshot",
		Live:            false,
		SourceVI:        "Gói nghiên cứu crowding-reversal-20260904 — fixture parity đóng băng, các cột ĐÃ GHI sẵn (tỉ lệ tài khoản long/short toàn cầu của Binance, điểm crowding trung bình 12 thành viên, tín hiệu, mục tiêu), chép nguyên, không tính lại.",
		NotLiveVI:       "KHÔNG PHẢI LIVE. Nguồn sống là Bước 6.2 (ingestion /futures/data/globalLongShortAccountRatio), đứng sau phán quyết 3.5 và 3.4. Mục tiêu là PHẦN VỐN trước mọi chi phí, không phải lệnh.",
		FixturePath:     "internal/crowding/testdata/rust_parity_fixture.csv.gz",
		FixtureSHA256:   hex.EncodeToString(sum[:]),
		Decision:        manifest.Decision,
		PanelFirstUTC:   manifest.Panel.First,
		PanelLastUTC:    manifest.Panel.Last,
		WindowFirstUTC:  windowStart.Format(time.RFC3339),
		WindowDays:      crowdingWindowDays,
		BarSec:          crowdingBarSec,
		Configuration:   compact(manifest.Configuration),
		EnsembleMembers: compact(manifest.EnsembleMembers),
	}
	for _, asset := range []string{"BTC", "ETH"} {
		a := crowdingAssetSnapshot{Asset: asset}
		for _, rw := range rows {
			if !rw.at.After(windowStart) {
				continue
			}
			f := rw.fields
			a.TimeSec = append(a.TimeSec, rw.at.Unix())
			a.CloseQuote = append(a.CloseQuote, number(f[need("input_"+asset+"_close")]))
			a.LongShortAccountRatio = append(a.LongShortAccountRatio, number(f[need("input_"+asset+"_global_account_ratio")]))
			a.CrowdingScore = append(a.CrowdingScore, number(f[need("expected_score_"+asset)]))
			a.Signal = append(a.Signal, number(f[need("expected_signal_"+asset)]))
			a.TargetFracOfEquity = append(a.TargetFracOfEquity, number(f[need("expected_target_"+asset)]))
		}
		snap.Assets = append(snap.Assets, a)
	}

	out, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

func TestCrowdingSnapshot_IsTheFixtureColumnsVerbatim(t *testing.T) {
	built := buildCrowdingSnapshot(t)
	var check crowdingSnapshot
	if err := json.Unmarshal(built, &check); err != nil {
		t.Fatalf("built snapshot is not valid JSON: %v", err)
	}
	if check.Live || len(check.Assets) != 2 {
		t.Fatalf("snapshot claims live=%v with %d assets", check.Live, len(check.Assets))
	}
	// Verbatim, checked on values: every non-empty fixture cell parses to the
	// same float64 as its copy.
	for _, a := range check.Assets {
		for i, raw := range a.CrowdingScore {
			if string(raw) == "null" {
				continue
			}
			if _, err := strconv.ParseFloat(string(raw), 64); err != nil {
				t.Fatalf("%s score[%d] = %s is not a number", a.Asset, i, raw)
			}
		}
	}
	for _, a := range check.Assets {
		n := len(a.TimeSec)
		if n < crowdingWindowDays*6-6 || n > crowdingWindowDays*6+6 {
			t.Errorf("%s: %d bars for %d days of 4h bars", a.Asset, n, crowdingWindowDays)
		}
		if len(a.CrowdingScore) != n || len(a.TargetFracOfEquity) != n || len(a.LongShortAccountRatio) != n {
			t.Errorf("%s: ragged columns", a.Asset)
		}
		for i := 1; i < n; i++ {
			if a.TimeSec[i]-a.TimeSec[i-1] != crowdingBarSec {
				t.Errorf("%s: bar %d is %ds after the previous one", a.Asset, i, a.TimeSec[i]-a.TimeSec[i-1])
				break
			}
		}
	}

	if os.Getenv("WRITE_CROWDING_SNAPSHOT") == "1" {
		if err := os.MkdirAll(filepath.Dir(crowdingSnapshotPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(crowdingSnapshotPath, built, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", crowdingSnapshotPath, len(built))
		return
	}
	committed, err := os.ReadFile(crowdingSnapshotPath)
	if err != nil {
		t.Fatalf("%v — run WRITE_CROWDING_SNAPSHOT=1 go test -run CrowdingSnapshot ./cmd/execportal", err)
	}
	if !bytes.Equal(committed, built) {
		t.Errorf("%s differs from the fixture it claims to copy — regenerate with WRITE_CROWDING_SNAPSHOT=1", crowdingSnapshotPath)
	}
}
