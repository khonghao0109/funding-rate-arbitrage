package crowding

import (
	"fmt"
	"math"
)

// BarsPerDay is the 4h UTC grid the research package is frozen on; BarsPerYear
// is its annualisation factor, 6 × 365.25.
const (
	BarsPerDay  = 6
	BarsPerYear = BarsPerDay * 365.25
)

// Side restricts which extremes are faded: both, only crowded shorts (long
// entries), or only crowded longs (short entries).
type Side string

// SideBoth fades both extremes; SideLong only enters longs (crowded shorts);
// SideShort only enters shorts (crowded longs).
const (
	SideBoth  Side = "both"
	SideLong  Side = "long"
	SideShort Side = "short"
)

// Member is one member of the 12-way ensemble: a z-score window and its
// hysteresis thresholds. Its volatility window is LookbackBars too.
type Member struct {
	LookbackBars int
	EntryZ       float64 // dimensionless
	ExitZ        float64 // dimensionless
}

// EnsembleMembers is the frozen 12-member set: 90/180/360 bars × entry
// |z| 1.0/1.25 × exit |z| 0.25/0.50, equal weights. Order matters for the
// parity test only insofar as the average is summed in this order, which
// is also the manifest's order.
var EnsembleMembers = []Member{
	{90, 1.0, 0.25}, {90, 1.0, 0.5}, {90, 1.25, 0.25}, {90, 1.25, 0.5},
	{180, 1.0, 0.25}, {180, 1.0, 0.5}, {180, 1.25, 0.25}, {180, 1.25, 0.5},
	{360, 1.0, 0.25}, {360, 1.0, 0.5}, {360, 1.25, 0.25}, {360, 1.25, 0.5},
}

// Config is the research package's StrategyConfig, all thirteen fields,
// each with its unit. Only Side, TargetVolAnnualFrac, MaxGrossFrac,
// UseTrendFilter, TrendHorizonsDays and TrendRequiredVotes are read by the
// ensemble path in this package: LookbackBars/EntryZ/ExitZ describe the
// SINGLE-member variant (the members carry their own), FeeFracPerSide,
// LagBars and IncludeFunding belong to the backtest (step 6.3), and
// BoundaryOffsetHours to the aggregation layer (step 6.2). They are carried
// so the frozen configuration is one struct that the manifest can be
// checked against field by field.
type Config struct {
	LookbackBars        int
	EntryZ              float64
	ExitZ               float64
	TargetVolAnnualFrac float64
	MaxGrossFrac        float64
	FeeFracPerSide      float64
	LagBars             int
	BoundaryOffsetHours int
	Side                Side
	IncludeFunding      bool
	UseTrendFilter      bool
	TrendHorizonsDays   []int
	TrendRequiredVotes  int
}

// DefaultConfig is the frozen 24% target-volatility configuration the
// fixture was generated with (rust_reference_manifest.json "configuration").
func DefaultConfig() Config {
	return Config{
		LookbackBars: 180, EntryZ: 1.0, ExitZ: 0.25,
		TargetVolAnnualFrac: 0.24, MaxGrossFrac: 1.0,
		FeeFracPerSide: 0.0005, LagBars: 1, BoundaryOffsetHours: 0,
		Side: SideBoth, IncludeFunding: true, UseTrendFilter: true,
		TrendHorizonsDays: []int{30, 60, 120}, TrendRequiredVotes: 2,
	}
}

// Validate refuses a configuration the arithmetic would silently misread: a
// non-positive volatility target or gross cap (a negative target flips
// every position's sign, in Python too), a bad side, or a trend vote the
// horizons cannot cast. Members are validated where they are used.
func (c Config) Validate() error {
	if !(c.TargetVolAnnualFrac > 0) || math.IsInf(c.TargetVolAnnualFrac, 0) {
		return fmt.Errorf("crowding: target_vol %v must be a positive finite fraction", c.TargetVolAnnualFrac)
	}
	if !(c.MaxGrossFrac > 0) || math.IsInf(c.MaxGrossFrac, 0) {
		return fmt.Errorf("crowding: max_gross %v must be a positive finite fraction", c.MaxGrossFrac)
	}
	switch c.Side {
	case SideBoth, SideLong, SideShort:
	default:
		return fmt.Errorf("crowding: unknown side %q", c.Side)
	}
	if c.UseTrendFilter && (len(c.TrendHorizonsDays) == 0 || c.TrendRequiredVotes < 1 || c.TrendRequiredVotes > len(c.TrendHorizonsDays)) {
		return fmt.Errorf("crowding: invalid trend-confirmation vote: %d of %d horizons", c.TrendRequiredVotes, len(c.TrendHorizonsDays))
	}
	return nil
}

// Panel is the 4h input grid: one close and one global long/short ACCOUNT
// ratio per asset per bar, aligned on the same bars. Missing values are NaN.
// Exactly two assets are supported, in the research package's column order
// (BTC then ETH): the risk scale is a two-asset covariance.
type Panel struct {
	Assets []string
	Close  [][]float64 // [asset][bar]
	Ratio  [][]float64 // [asset][bar]
}

// Bars is the grid length.
func (p Panel) Bars() int {
	if len(p.Close) == 0 {
		return 0
	}
	return len(p.Close[0])
}

// Validate checks the shape the arithmetic assumes.
func (p Panel) Validate() error {
	if len(p.Assets) != 2 || len(p.Close) != 2 || len(p.Ratio) != 2 {
		return fmt.Errorf("crowding: panel needs exactly two assets (got %d assets, %d close series, %d ratio series)", len(p.Assets), len(p.Close), len(p.Ratio))
	}
	n := len(p.Close[0])
	for a := range p.Assets {
		if len(p.Close[a]) != n || len(p.Ratio[a]) != n {
			return fmt.Errorf("crowding: series lengths differ for %s (close %d, ratio %d, want %d)", p.Assets[a], len(p.Close[a]), len(p.Ratio[a]), n)
		}
	}
	return nil
}

// Prefix is the panel cut to its first n bars — what a decision at bar n−1
// could see; n is clamped to the panel. Shares the underlying arrays;
// callers must not mutate.
func (p Panel) Prefix(n int) Panel {
	if n > p.Bars() {
		n = p.Bars()
	}
	if n < 0 {
		n = 0
	}
	out := Panel{Assets: p.Assets, Close: make([][]float64, len(p.Close)), Ratio: make([][]float64, len(p.Ratio))}
	for a := range p.Close {
		out.Close[a] = p.Close[a][:n]
		out.Ratio[a] = p.Ratio[a][:n]
	}
	return out
}
