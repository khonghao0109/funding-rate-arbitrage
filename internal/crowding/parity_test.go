package crowding

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"os"
	"strconv"
	"testing"
	"time"
)

// The parity fixture: the research package's golden 4h panel and its
// expected ensemble outputs, gzip-compressed, verified by SHA-256 against the
// package's own manifest BEFORE a single row is read. Offline, like every
// test here.

const (
	fixturePath  = "testdata/rust_parity_fixture.csv.gz"
	manifestPath = "testdata/rust_reference_manifest.json"

	// PLAN 6.1's thresholds. The score tolerance is stated, not implied:
	// pandas' rolling mean and variance are online (Kahan/Welford) sums and
	// drift from a two-pass computation at the 1e-13 level over ten thousand
	// bars — measured 2.5e-13 by the 2026-09-11 spike. The signal has NO
	// tolerance: it is a multiple of 1/12 and a difference there is a state
	// machine disagreeing, which rounding cannot explain.
	targetToleranceAbs = 1e-10
	scoreToleranceAbs  = 1e-10

	// goldenSHA256 is the hash PLAN 6.1 names for the DECOMPRESSED fixture.
	// The manifest beside the file carries the same value, but the test is
	// pinned to THIS constant so that regenerating fixture and manifest
	// together cannot pass silently.
	goldenSHA256 = "fa9eae27c979f7fe031f6d54b1db954afbc81c3634e9542460a4533b49ec3bb2"
)

type manifest struct {
	Configuration struct {
		Lookback          int     `json:"lookback"`
		EntryZ            float64 `json:"entry_z"`
		ExitZ             float64 `json:"exit_z"`
		TargetVol         float64 `json:"target_vol"`
		MaxGross          float64 `json:"max_gross"`
		Fee               float64 `json:"fee"`
		LagBars           int     `json:"lag_bars"`
		Side              string  `json:"side"`
		UseTrendFilter    bool    `json:"use_trend_filter"`
		TrendHorizonDays  []int   `json:"trend_horizon_days"`
		TrendRequired     int     `json:"trend_required"`
		BoundaryOffsetHrs int     `json:"boundary_offset_hours"`
		IncludeFunding    bool    `json:"include_funding"`
	} `json:"configuration"`
	EnsembleMembers [][]float64 `json:"ensemble_members"`
	Panel           struct {
		Rows4h int `json:"rows_4h"`
	} `json:"panel"`
	Integrity struct {
		PrefixChecks []struct {
			Timestamp string  `json:"timestamp"`
			MaxErr    float64 `json:"max_absolute_target_error"`
		} `json:"prefix_checks"`
	} `json:"integrity"`
	Fixture struct {
		Rows   int      `json:"rows"`
		SHA256 string   `json:"sha256"`
		Cols   []string `json:"columns"`
	} `json:"rust_parity_fixture"`
}

type fixture struct {
	times    []time.Time
	panel    Panel
	expected map[string][]float64 // column → values (NaN for empty)
}

func loadManifest(t *testing.T) manifest {
	t.Helper()
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// loadFixture decompresses, hashes, refuses on a mismatch, then parses.
func loadFixture(t *testing.T, m manifest) fixture {
	t.Helper()
	gz, err := os.Open(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	zr, err := gzip.NewReader(gz)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if m.Fixture.SHA256 != goldenSHA256 {
		t.Fatalf("manifest names fixture SHA-256 %s, PLAN 6.1 names %s — the manifest is not the frozen one", m.Fixture.SHA256, goldenSHA256)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != goldenSHA256 {
		t.Fatalf("fixture SHA-256 %s, want %s — the golden file is not the one PLAN 6.1 froze; refusing to compare against it", got, goldenSHA256)
	}

	rows, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	header, rows := rows[0], rows[1:]
	if len(rows) != m.Fixture.Rows {
		t.Fatalf("fixture has %d rows, manifest says %d", len(rows), m.Fixture.Rows)
	}
	col := map[string]int{}
	for i, h := range header {
		col[h] = i
	}
	for _, want := range m.Fixture.Cols {
		if _, ok := col[want]; !ok {
			t.Fatalf("fixture lacks column %s the manifest lists", want)
		}
	}
	f := fixture{expected: map[string][]float64{}}
	f.panel = Panel{Assets: []string{"BTC", "ETH"}, Close: [][]float64{make([]float64, len(rows)), make([]float64, len(rows))},
		Ratio: [][]float64{make([]float64, len(rows)), make([]float64, len(rows))}}
	for _, name := range header[1:] {
		f.expected[name] = make([]float64, len(rows))
	}
	for i, row := range rows {
		ts, err := time.Parse("2006-01-02 15:04:05-07:00", row[col["timestamp_utc"]])
		if err != nil {
			t.Fatalf("row %d timestamp %q: %v", i, row[0], err)
		}
		f.times = append(f.times, ts)
		for name, j := range col {
			if name == "timestamp_utc" {
				continue
			}
			f.expected[name][i] = parseCell(t, row[j])
		}
		for a, asset := range f.panel.Assets {
			f.panel.Close[a][i] = f.expected["input_"+asset+"_close"][i]
			f.panel.Ratio[a][i] = f.expected["input_"+asset+"_global_account_ratio"][i]
		}
	}
	return f
}

// parseCell reads pandas' CSV rendering: an empty cell is NaN.
func parseCell(t *testing.T, s string) float64 {
	t.Helper()
	if s == "" {
		return math.NaN()
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("cell %q: %v", s, err)
	}
	return v
}

// The frozen configuration in Go must be the one the manifest froze; a
// drift here would make every other assertion a comparison of two
// different strategies.
func TestDefaultConfig_MatchesTheFrozenManifest(t *testing.T) {
	m := loadManifest(t)
	cfg := DefaultConfig()
	c := m.Configuration
	if cfg.LookbackBars != c.Lookback || cfg.EntryZ != c.EntryZ || cfg.ExitZ != c.ExitZ ||
		cfg.TargetVolAnnualFrac != c.TargetVol || cfg.MaxGrossFrac != c.MaxGross || cfg.FeeFracPerSide != c.Fee ||
		cfg.LagBars != c.LagBars || string(cfg.Side) != c.Side || cfg.UseTrendFilter != c.UseTrendFilter ||
		cfg.IncludeFunding != c.IncludeFunding || cfg.BoundaryOffsetHours != c.BoundaryOffsetHrs ||
		cfg.TrendRequiredVotes != c.TrendRequired || len(cfg.TrendHorizonsDays) != len(c.TrendHorizonDays) {
		t.Fatalf("DefaultConfig %+v differs from the manifest's configuration %+v", cfg, c)
	}
	for i := range c.TrendHorizonDays {
		if cfg.TrendHorizonsDays[i] != c.TrendHorizonDays[i] {
			t.Fatalf("trend horizons %v vs manifest %v", cfg.TrendHorizonsDays, c.TrendHorizonDays)
		}
	}
	if c.BoundaryOffsetHrs != 0 {
		t.Fatalf("manifest boundary offset %d — the fixture is not the 0h panel", c.BoundaryOffsetHrs)
	}
	if len(EnsembleMembers) != len(m.EnsembleMembers) {
		t.Fatalf("%d members in Go, %d in the manifest", len(EnsembleMembers), len(m.EnsembleMembers))
	}
	for i, mm := range m.EnsembleMembers {
		if int(mm[0]) != EnsembleMembers[i].LookbackBars || mm[1] != EnsembleMembers[i].EntryZ || mm[2] != EnsembleMembers[i].ExitZ {
			t.Fatalf("member %d: Go %+v, manifest %v", i, EnsembleMembers[i], mm)
		}
	}
}

// PLAN 6.1 acceptance: over all 10,957 bars, |target error| ≤ 1e-10, the
// signal EXACTLY equal, the score within its stated tolerance (NaN ↔ NaN),
// and zero rows over any threshold.
func TestEnsembleTargets_ParityWithTheResearchFixture(t *testing.T) {
	m := loadManifest(t)
	f := loadFixture(t, m)
	got, err := EnsembleTargets(f.panel, DefaultConfig(), EnsembleMembers)
	if err != nil {
		t.Fatal(err)
	}
	var maxTarget, maxScore float64
	over := 0
	compared := 0
	for a, asset := range f.panel.Assets {
		wantTarget := f.expected["expected_target_"+asset]
		wantSignal := f.expected["expected_signal_"+asset]
		wantScore := f.expected["expected_score_"+asset]
		for i := range wantTarget {
			compared++
			if d := math.Abs(got.TargetFracOfEquity[a][i] - wantTarget[i]); d > maxTarget || math.IsNaN(d) {
				if math.IsNaN(d) {
					over++
					t.Errorf("%s bar %d (%s): target NaN vs %v", asset, i, f.times[i].Format(time.RFC3339), wantTarget[i])
					continue
				}
				maxTarget = d
			}
			if math.Abs(got.TargetFracOfEquity[a][i]-wantTarget[i]) > targetToleranceAbs {
				over++
				t.Errorf("%s bar %d (%s): target %v vs %v", asset, i, f.times[i].Format(time.RFC3339), got.TargetFracOfEquity[a][i], wantTarget[i])
			}
			if got.MeanMemberSignal[a][i] != wantSignal[i] {
				over++
				t.Errorf("%s bar %d (%s): signal %v vs %v — a state machine disagreement, not rounding", asset, i, f.times[i].Format(time.RFC3339), got.MeanMemberSignal[a][i], wantSignal[i])
			}
			gs, ws := got.MeanMemberScore[a][i], wantScore[i]
			switch {
			case math.IsNaN(gs) && math.IsNaN(ws):
			case math.IsNaN(gs) != math.IsNaN(ws):
				over++
				t.Errorf("%s bar %d (%s): score %v vs %v — one side has a valid window and the other does not", asset, i, f.times[i].Format(time.RFC3339), gs, ws)
			default:
				if d := math.Abs(gs - ws); d > maxScore {
					maxScore = d
				}
				if math.Abs(gs-ws) > scoreToleranceAbs {
					over++
					t.Errorf("%s bar %d (%s): score %v vs %v", asset, i, f.times[i].Format(time.RFC3339), gs, ws)
				}
			}
			if over > 20 {
				t.Fatalf("more than 20 rows over threshold; stopping")
			}
		}
	}
	if over != 0 {
		t.Fatalf("%d asset-bars over threshold", over)
	}
	t.Logf("parity over %d asset-bars (%d rows × 2): max |target err| %.3g (≤ %g), max |score err| %.3g (≤ %g), signal exact on every bar",
		compared, len(f.times), maxTarget, targetToleranceAbs, maxScore, scoreToleranceAbs)
	if maxTarget > targetToleranceAbs || maxScore > scoreToleranceAbs {
		t.Fatalf("max errors target %g score %g exceed the thresholds", maxTarget, maxScore)
	}
}

// The manifest's own causality audit, ported: at each of its seven
// cut-offs, the ensemble computed on the panel PREFIX must give, at its last
// bar, exactly the target the full run gives there. The manifest recorded
// an error of 0.0 and so must Go — a difference would be a lookahead.
func TestEnsembleTargets_PrefixCausalityAtTheManifestCutoffs(t *testing.T) {
	m := loadManifest(t)
	f := loadFixture(t, m)
	full, err := EnsembleTargets(f.panel, DefaultConfig(), EnsembleMembers)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Integrity.PrefixChecks) == 0 {
		t.Fatal("manifest carries no prefix checks")
	}
	for _, check := range m.Integrity.PrefixChecks {
		cutoff, err := time.Parse(time.RFC3339, check.Timestamp)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for i, ts := range f.times {
			if !ts.After(cutoff) {
				n = i + 1
			}
		}
		if n == 0 {
			t.Fatalf("cut-off %s is before the panel", check.Timestamp)
		}
		prefix, err := EnsembleTargets(f.panel.Prefix(n), DefaultConfig(), EnsembleMembers)
		if err != nil {
			t.Fatal(err)
		}
		for a, asset := range f.panel.Assets {
			got, want := prefix.TargetFracOfEquity[a][n-1], full.TargetFracOfEquity[a][n-1]
			if got != want {
				t.Errorf("cut-off %s %s: prefix target %v, full-run target %v (manifest error %g) — the full run used data after the cut-off",
					check.Timestamp, asset, got, want, check.MaxErr)
			}
		}
	}
}
