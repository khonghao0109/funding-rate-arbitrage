package config

import "testing"

// One rule for which spot leg a perp is hedged against, shared by cmd/scanner
// and cmd/backtest — the step-3.5 gate compares positions, and two commands
// picking different legs for the same perp would compare different trades.
func TestCheapestVerifiedSpot_PicksTheCheapestVerifiedFeeAndSaysSo(t *testing.T) {
	cfg := Config{Sources: []Source{
		{Source: "a_spot", MarketType: "spot", Fee: Fee{TakerBps: 10, Verified: true}},
		{Source: "b_spot", MarketType: "spot", Fee: Fee{TakerBps: 2, Verified: false}}, // cheaper but unverified
		{Source: "c_spot", MarketType: "spot", Fee: Fee{TakerBps: 5, Verified: true}},
	}}
	got, note := cfg.CheapestVerifiedSpot([]string{"a_spot", "b_spot", "c_spot"})
	if got != "c_spot" {
		t.Errorf("chose %q, want c_spot (cheapest VERIFIED — b_spot's 2 bps was never looked up)", got)
	}
	if note == "" {
		t.Error("a choice among several must be explained in the note")
	}
	if got, note := cfg.CheapestVerifiedSpot([]string{"a_spot"}); got != "a_spot" || note != "" {
		t.Errorf("a single candidate needs no explanation: %q %q", got, note)
	}
	if got, _ := cfg.CheapestVerifiedSpot(nil); got != "" {
		t.Errorf("no candidates → no leg, got %q", got)
	}
}
