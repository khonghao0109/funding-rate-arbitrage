package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dataSilenceFixture writes a one-source config with the given scanner and
// source lines, so each case states its own numbers rather than borrowing them
// from the shipped file.
func dataSilenceFixture(t *testing.T, scannerLines, sourceLines string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.yaml")
	body := `
scanner:
  alert_min_spread_pct: 0.05
  default_stale_after_sec: 12
` + scannerLines + `symbols:
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
` + sourceLines + `    fee:
      verified: false
      note_vi: fixture
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

// A config written before 2026-09-12 has neither key, and must keep meaning
// exactly what it meant: the check off, the lifecycle as it was. The step-3.5
// process is running such a config right now.
func TestDataSilence_AbsentEverywhereIsOff(t *testing.T) {
	cfg, err := dataSilenceFixture(t, "", "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Sources[0].DataSilenceSec; got != 0 {
		t.Errorf("data_silence_sec = %d with the key absent everywhere, want 0 (off)", got)
	}
}

func TestDataSilence_TheSourceKeyWinsOverTheDefault(t *testing.T) {
	cfg, err := dataSilenceFixture(t, "  default_data_silence_sec: 300\n", "    data_silence_sec: 900\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Sources[0].DataSilenceSec; got != 900 {
		t.Errorf("data_silence_sec = %d, want the source's own 900", got)
	}

	cfg, err = dataSilenceFixture(t, "  default_data_silence_sec: 300\n", "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Sources[0].DataSilenceSec; got != 300 {
		t.Errorf("data_silence_sec = %d, want the default 300", got)
	}
}

// The two thresholds answer different questions and the silence one is always
// the longer. Set the other way round, the connector would re-dial a venue
// whose prices the scanner still considers fresh — a reconnect storm dressed as
// a health check, and worse than the defect it is meant to catch.
//
// Above the staleness threshold is necessary and NOT sufficient: staleness
// thresholds here are 10–20s while the worst socket-wide gap measured across
// nine venues was 18.38s, so a deadline of 13 would clear stale_after_sec and
// still re-dial every source at once. MinDataSilenceSec is the real floor.
func TestDataSilence_RefusesADeadlineBelowTheFloorOrBelowStaleness(t *testing.T) {
	for _, sec := range []string{"-1", "5", "12", "13", "59"} {
		t.Run(sec, func(t *testing.T) {
			_, err := dataSilenceFixture(t, "", "    data_silence_sec: "+sec+"\n")
			if err == nil {
				t.Fatalf("data_silence_sec %s loaded; floor is %d and stale_after_sec is 12", sec, MinDataSilenceSec)
			}
			if !strings.Contains(err.Error(), "data_silence_sec") {
				t.Errorf("error %q does not name the key the operator has to fix", err)
			}
		})
	}
	if _, err := dataSilenceFixture(t, "", "    data_silence_sec: 60\n"); err != nil {
		t.Errorf("the floor itself must load: %v", err)
	}
	// The floor alone is not enough either: a source whose own prices are
	// called stale after 90s must not have its socket re-dialled at 60.
	if _, err := dataSilenceFixture(t, "", "    stale_after_sec: 90\n    data_silence_sec: 60\n"); err == nil {
		t.Error("data_silence_sec 60 loaded beside stale_after_sec 90")
	}
}

// The oracle was exempt for one day, because Pyth is SSE and had no data
// watchdog of its own. It has one now (exchanges/pyth), so it is covered like
// every other source — and it is the source where a silent stream would hide
// longest: Pyth answered 401 for a whole 72h soak and nothing noticed.
func TestDataSilence_TheOracleIsCoveredToo(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatalf("load the shipped config: %v", err)
	}
	if cfg.Scanner.DefaultDataSilenceSec <= 0 {
		t.Fatal("the shipped config sets no default, so this test proves nothing")
	}
	oracles := 0
	for _, source := range cfg.Sources {
		if source.MarketType != "oracle" {
			continue
		}
		oracles++
		if source.DataSilenceSec <= 0 {
			t.Errorf("%s is an oracle with no data_silence_sec; its SSE stream could heartbeat forever "+
				"without a price and nothing would notice", source.Source)
		}
	}
	if oracles == 0 {
		t.Fatal("the shipped config has no oracle, so this test proves nothing")
	}
}

// The shipped file is the one that runs, so its numbers are asserted here
// rather than described in a comment somebody has to trust.
//
// Every source, the oracle included since 2026-09-13: Pyth keeps its own read
// loop but now runs the same two watchdogs.
func TestShippedConfig_GivesEverySourceASilenceDeadline(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatalf("load the shipped config: %v", err)
	}
	for _, source := range cfg.Sources {
		if source.DataSilenceSec <= 0 {
			t.Errorf("%s has no data_silence_sec; a refused or dropped subscription there would go unnoticed for as long as the process runs",
				source.Source)
			continue
		}
		if source.DataSilenceSec <= source.StaleAfterSec {
			t.Errorf("%s: data_silence_sec %d is at or below stale_after_sec %d",
				source.Source, source.DataSilenceSec, source.StaleAfterSec)
		}
	}
}
