package config

import (
	"os"
	"path/filepath"
	"testing"
)

const strategyFixtureTail = `
symbols:
  - { symbol: BTCUSDT, base: BTC, quote: USDT }
sources:
  - source: s
    connector: binance_futures
    venue: v
    market_type: perp
    quote_asset: USDT
    label: S
    short_label: S
    funding_stale_after_sec: 60
    funding_publish_mode: periodic
    fee:
      verified: false
      note_vi: fixture
`

func loadStrategyFixture(t *testing.T, strategyBlock string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.yaml")
	body := "scanner:\n  alert_min_spread_pct: 0.05\n  default_stale_after_sec: 12\n" + strategyBlock + strategyFixtureTail
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

// A config with no strategy block must behave exactly as before step 3.5:
// nothing is evaluated and nothing is journaled.
func TestStrategy_AbsentBlockIsDisabled(t *testing.T) {
	cfg, err := loadStrategyFixture(t, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Strategy.Enabled {
		t.Error("an absent strategy block must not enable evaluation")
	}
}

func TestStrategy_DefaultsFillTheCadenceButNeverTheThresholds(t *testing.T) {
	cfg, err := loadStrategyFixture(t, `
strategy:
  enabled: true
  min_rate_per_8h_bps: 0.5
  persistence_periods: 3
  min_net_apr_frac: 0.02
  notional_quote: 50000
  holding_days: 30
  exit_net_apr_frac: 0.005
  exit_persistence_periods: 3
  max_basis_pct: 1.0
  max_basis_widen_pct: 0.5
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := cfg.Strategy
	if s.EvaluateEveryMin <= 0 {
		t.Error("evaluate_every_min must get a default")
	}
	if s.MaxBookAgeMin <= 0 {
		t.Error("max_book_age_min must get a default — a fill priced on an unbounded-age book is not an estimate")
	}
	// The unit-suffixed fields must arrive as written: these are the numbers
	// step 3.5 compares against the backtest, and a silently defaulted
	// threshold would make the two runs incomparable.
	if s.MinRatePer8hBps != 0.5 || s.PersistencePeriods != 3 || s.NotionalQuote != 50000 || s.HoldingDays != 30 {
		t.Errorf("thresholds not loaded verbatim: %+v", s)
	}
}

func TestStrategy_EnabledBlockMustCarryEveryThreshold(t *testing.T) {
	cases := map[string]string{
		"no notional": `
strategy:
  enabled: true
  min_rate_per_8h_bps: 0.5
  persistence_periods: 3
  min_net_apr_frac: 0.02
  holding_days: 30
`,
		"no persistence": `
strategy:
  enabled: true
  min_rate_per_8h_bps: 0.5
  min_net_apr_frac: 0.02
  notional_quote: 50000
  holding_days: 30
`,
		"no holding period": `
strategy:
  enabled: true
  min_rate_per_8h_bps: 0.5
  persistence_periods: 3
  min_net_apr_frac: 0.02
  notional_quote: 50000
`,
		"exit floor above entry floor": `
strategy:
  enabled: true
  min_rate_per_8h_bps: 0.5
  persistence_periods: 3
  min_net_apr_frac: 0.02
  notional_quote: 50000
  holding_days: 30
  exit_net_apr_frac: 0.05
`,
	}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadStrategyFixture(t, block); err == nil {
				t.Errorf("%s: an enabled strategy block missing a threshold must be refused at load, "+
					"not defaulted into a number step 3.5 never chose", name)
			}
		})
	}
}
