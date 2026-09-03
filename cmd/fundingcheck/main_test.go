package main

import (
	"math"
	"strings"
	"testing"
)

// Values taken from the live readings of 2026-09-03 recorded in
// docs/DATA-REQUIREMENTS.md §3: one case per distinct interval so a wrong
// normalization factor cannot cancel out.
func TestRatePer8hFrac(t *testing.T) {
	cases := []struct {
		name                string
		ratePerIntervalFrac float64
		intervalSec         int64
		want                float64
	}{
		{"binance 8h rate stays as-is", 0.00006235, 28800, 0.00006235},
		{"kraken hourly rate scales by 8", 0.000010682912500000, 3600, 0.0000854633},
		{"hyperliquid hourly rate scales by 8", 0.0000125, 3600, 0.0001},
		{"binance 4h override scales by 2", 0.0001, 14400, 0.0002},
	}
	for _, c := range cases {
		got := ratePer8hFrac(c.ratePerIntervalFrac, c.intervalSec)
		if math.Abs(got-c.want) > 1e-12 {
			t.Errorf("%s: ratePer8hFrac(%v, %d) = %v, want %v", c.name, c.ratePerIntervalFrac, c.intervalSec, got, c.want)
		}
	}
}

func TestAPRFrac(t *testing.T) {
	cases := []struct {
		name                string
		ratePerIntervalFrac float64
		intervalSec         int64
		want                float64
	}{
		// 0.01%/8h × 1095 intervals/year = 10.95%/year
		{"8h interval has 1095 settlements a year", 0.0001, 28800, 0.1095},
		// The 8× trap: an hourly venue annualizes with 8760 intervals, not 1095.
		{"1h interval has 8760 settlements a year", 0.0000125, 3600, 0.1095},
	}
	for _, c := range cases {
		got := aprFrac(c.ratePerIntervalFrac, c.intervalSec)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: aprFrac(%v, %d) = %v, want %v", c.name, c.ratePerIntervalFrac, c.intervalSec, got, c.want)
		}
	}
}

// The coherence check must catch multiplicative unit bugs (÷8/×8, 60×,
// absolute-vs-relative) while never failing legitimate market regimes:
// negative funding, mixed signs, and near-zero clusters are all routine.
// The first version of this check compared signed values and was
// unsatisfiable for all-negative clusters — pinned here.
func TestCoherenceVerdict(t *testing.T) {
	venues := []string{"a", "b", "c", "d", "e", "f", "g"}
	cases := []struct {
		name       string
		per8hFracs []float64
		wantPass   bool
		wantInWhy  string
	}{
		{
			// The actual 2026-09-03 readings.
			name:       "live positive cluster passes",
			per8hFracs: []float64{6.1e-5, 9.3e-5, 4.4e-5, 7.8e-5, 8.5e-5, 1.0e-4, 9.1e-5},
			wantPass:   true,
		},
		{
			name:       "all-negative cluster passes",
			per8hFracs: []float64{-6.1e-5, -9.3e-5, -4.4e-5, -7.8e-5, -8.5e-5, -1.0e-4, -9.1e-5},
			wantPass:   true,
		},
		{
			name:       "mixed signs of similar magnitude pass",
			per8hFracs: []float64{6.1e-5, -9.3e-5, 4.4e-5, -7.8e-5, 8.5e-5, 1.0e-4, 9.1e-5},
			wantPass:   true,
		},
		{
			// Hyperliquid read without the ×8: 1.25e-5 vs median ~8.5e-5.
			name:       "8x-low outlier fails",
			per8hFracs: []float64{6.1e-5, 9.3e-5, 1.25e-5, 7.8e-5, 8.5e-5, 1.0e-4, 9.1e-5},
			wantPass:   false,
			wantInWhy:  "×",
		},
		{
			// Bybit minutes read as seconds: 60× high.
			name:       "60x-high outlier fails",
			per8hFracs: []float64{6.1e-5, 9.3e-5 * 60, 4.4e-5, 7.8e-5, 8.5e-5, 1.0e-4, 9.1e-5},
			wantPass:   false,
		},
		{
			name:       "near-zero cluster abstains as pass",
			per8hFracs: []float64{1e-8, -2e-8, 0, 3e-8, -1e-8, 2e-8, 0},
			wantPass:   true,
			wantInWhy:  "near zero",
		},
		{
			name:       "one venue near zero among a live cluster passes",
			per8hFracs: []float64{6.1e-5, 9.3e-5, 1e-8, 7.8e-5, 8.5e-5, 1.0e-4, 9.1e-5},
			wantPass:   true,
		},
		{
			name:       "NaN from an upstream parse fails with the venue named",
			per8hFracs: []float64{6.1e-5, math.NaN(), 4.4e-5, 7.8e-5, 8.5e-5, 1.0e-4, 9.1e-5},
			wantPass:   false,
			wantInWhy:  "b",
		},
		{
			// Even venue count (fetch failures shrink the set): the median is
			// the mean of the middles, so a single shared outlier cannot
			// become the reference point.
			name:       "even count healthy cluster passes",
			per8hFracs: []float64{6.1e-5, 9.3e-5, 7.8e-5, 8.5e-5, 1.0e-4, 9.1e-5},
			wantPass:   true,
		},
		{
			name:       "even count with a 60x outlier names it",
			per8hFracs: []float64{6.1e-5, 9.3e-5 * 60, 7.8e-5, 8.5e-5, 1.0e-4, 9.1e-5},
			wantPass:   false,
			wantInWhy:  "b",
		},
	}
	for _, c := range cases {
		got := coherenceVerdict(venues[:len(c.per8hFracs)], c.per8hFracs)
		if got.Pass != c.wantPass {
			t.Errorf("%s: Pass = %v, want %v (detail: %s)", c.name, got.Pass, c.wantPass, got.Detail)
		}
		if c.wantInWhy != "" && !strings.Contains(got.Detail, c.wantInWhy) {
			t.Errorf("%s: detail %q does not mention %q", c.name, got.Detail, c.wantInWhy)
		}
	}
}
