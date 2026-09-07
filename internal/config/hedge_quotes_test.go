package config

import (
	"reflect"
	"strings"
	"testing"
)

// hedge.quote_equivalents is a RISK declaration, not a fact about the venues:
// it lets a USD-quoted perp hedge against a USDT spot. These tests pin the
// three things that make it safe to have — it is optional, it is normalized so
// every message reads in one casing, and a declaration nobody could act on is
// refused at load rather than half-applied.

func TestHedge_AbsentBlockDeclaresNothing(t *testing.T) {
	cfg, err := loadStrategyFixture(t, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Hedge.QuoteEquivalents) != 0 {
		t.Errorf("want no equivalence declared, got %v", cfg.Hedge.QuoteEquivalents)
	}
}

func TestHedge_QuoteEquivalentsAreUpperCased(t *testing.T) {
	cfg, err := loadStrategyFixture(t, "hedge:\n  quote_equivalents:\n    - [\" usd \", usdt]\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := [][]string{{"USD", "USDT"}}
	if !reflect.DeepEqual(cfg.Hedge.QuoteEquivalents, want) {
		t.Errorf("want %v, got %v", want, cfg.Hedge.QuoteEquivalents)
	}
}

func TestHedge_UnusableDeclarationsAreRefused(t *testing.T) {
	cases := []struct {
		name, block, want string
	}{
		{"one asset is not an equivalence", "hedge:\n  quote_equivalents:\n    - [USD]\n", "at least 2"},
		{"empty asset name", "hedge:\n  quote_equivalents:\n    - [USD, \"\"]\n", "empty asset name"},
		{"repeated inside a group", "hedge:\n  quote_equivalents:\n    - [USD, usd]\n", "twice"},
		{"asset in two groups", "hedge:\n  quote_equivalents:\n    - [USD, USDT]\n    - [USDT, USDC]\n", "at most one group"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadStrategyFixture(t, tc.block)
			if err == nil {
				t.Fatal("want a load error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want an error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

// Two separate groups are fine as long as no asset spans them.
func TestHedge_SeveralDisjointGroupsLoad(t *testing.T) {
	cfg, err := loadStrategyFixture(t, "hedge:\n  quote_equivalents:\n    - [USD, USDT]\n    - [EUR, EURC]\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Hedge.QuoteEquivalents) != 2 {
		t.Fatalf("want 2 groups, got %v", cfg.Hedge.QuoteEquivalents)
	}
}
