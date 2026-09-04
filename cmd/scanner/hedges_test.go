package main

import (
	"strings"
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/instruments"
	"futures-arbitrage-scanner/internal/scanner"
)

func legFor(legs []scanner.HedgeLeg, symbol, perp string) (scanner.HedgeLeg, bool) {
	for _, leg := range legs {
		if leg.Symbol == symbol && leg.PerpSource == perp {
			return leg, true
		}
	}
	return scanner.HedgeLeg{}, false
}

// The refusal is the point. Kraken, Hyperliquid and Paradex quote USD against
// spot markets that only quote USDT, so their funding rates are real and their
// trades cannot be opened — and a dashboard that showed the rate without the
// refusal would be advertising an impossible position.
func TestHedgeLegs_ARefusedPerpKeepsTheVenuesOwnReason(t *testing.T) {
	cfg := repoConfig(t)
	mapping := instruments.HedgeMapping{
		Rejections: []instruments.Rejection{{
			Symbol: "BTCUSDT",
			Source: "kraken_futures",
			Reason: "no spot market shares quote USD for BTCUSDT (spot quotes: USDT) — refused, not guessed",
		}},
	}

	leg, ok := legFor(hedgeLegs(cfg, mapping), "BTCUSDT", "kraken_futures")
	if !ok {
		t.Fatal("a refused perp produced no leg at all; the dashboard would call it 'not known yet'")
	}
	if leg.SpotSource != "" {
		t.Errorf("a refused perp got spot leg %q", leg.SpotSource)
	}
	if !strings.Contains(leg.NoteVI, "quote USD") {
		t.Errorf("the refusal lost the venue's own words: %q", leg.NoteVI)
	}
}

// A rejection about a SPOT source is not an answer to "can this perp be
// hedged", and turning it into one would put a spot market's problem in a perp's
// row.
func TestHedgeLegs_IgnoresRejectionsThatAreNotAboutAPerp(t *testing.T) {
	cfg := repoConfig(t)
	mapping := instruments.HedgeMapping{
		Rejections: []instruments.Rejection{{
			Symbol: "BTCUSDT", Source: "binance_spot", Reason: "no perp market shares quote USDT",
		}},
	}

	if legs := hedgeLegs(cfg, mapping); len(legs) != 0 {
		t.Errorf("a spot rejection produced %d legs: %+v", len(legs), legs)
	}
}

func TestHedgeLegs_ASinglePairNeedsNoExplanation(t *testing.T) {
	cfg := repoConfig(t)
	mapping := instruments.HedgeMapping{Pairs: []instruments.HedgePair{{
		Symbol: "BTCUSDT",
		Spot:   exchanges.Instrument{Source: "binance_spot"},
		Perp:   exchanges.Instrument{Source: "binance_futures"},
	}}}

	leg, ok := legFor(hedgeLegs(cfg, mapping), "BTCUSDT", "binance_futures")
	if !ok || leg.SpotSource != "binance_spot" {
		t.Fatalf("got %+v, want the binance_spot leg", leg)
	}
	if leg.NoteVI != "" {
		t.Errorf("a single candidate was explained anyway: %q", leg.NoteVI)
	}
}

// With several valid spot legs the choice changes the only number derived from
// it — the fee-based breakeven — so the pick has to be stated, and an
// unverified schedule must never win on a fee of zero it never had.
func TestHedgeLegs_PrefersTheCheapestVerifiedSpotLeg(t *testing.T) {
	cfg := repoConfig(t)
	binanceSpot, ok := cfg.SourceByName("binance_spot")
	if !ok {
		t.Fatal("binance_spot is not configured")
	}
	if !binanceSpot.Fee.Verified {
		t.Skip("binance_spot's fee is unverified in the shipped config; this test needs one verified leg")
	}

	mapping := instruments.HedgeMapping{Pairs: []instruments.HedgePair{
		{
			Symbol: "BTCUSDT",
			Spot:   exchanges.Instrument{Source: "bybit_spot"}, // unverified in config.yaml
			Perp:   exchanges.Instrument{Source: "binance_futures"},
		},
		{
			Symbol: "BTCUSDT",
			Spot:   exchanges.Instrument{Source: "binance_spot"},
			Perp:   exchanges.Instrument{Source: "binance_futures"},
		},
	}}

	leg, found := legFor(hedgeLegs(cfg, mapping), "BTCUSDT", "binance_futures")
	if !found {
		t.Fatal("no leg produced")
	}
	if leg.SpotSource != "binance_spot" {
		t.Errorf("chose %q; an unverified schedule must not win on a fee it never published", leg.SpotSource)
	}
	if leg.NoteVI == "" {
		t.Error("two candidates and no note; the breakeven beside it belongs to one particular pair of venues")
	}
}

// The real mapping the scanner runs on: every configured perp must end up with
// an answer, either a spot leg or a named refusal. A perp with neither renders
// as "not known yet" forever.
func TestHedgeLegs_EveryConfiguredPerpGetsAnAnswer(t *testing.T) {
	cfg := repoConfig(t)

	var claims []instruments.SourceClaim
	var insts []exchanges.Instrument
	for _, source := range cfg.Sources {
		claims = append(claims, instruments.SourceClaim{
			Source: source.Source, MarketType: source.MarketType,
			QuoteAsset: source.QuoteAsset, Tradable: source.Tradable,
		})
		if source.MarketType != "perp" && source.MarketType != "spot" {
			continue
		}
		for _, symbol := range cfg.Symbols {
			native, ok := source.VenueSymbol(symbol)
			if !ok {
				continue
			}
			insts = append(insts, exchanges.Instrument{
				Symbol: symbol.Symbol, NativeSymbol: native, Source: source.Source,
				MarketType: source.MarketType, BaseAsset: symbol.Base,
				QuoteAsset: source.QuoteAsset, Status: exchanges.StatusTrading,
			})
		}
	}
	var pairs []instruments.PairAssets
	for _, symbol := range cfg.Symbols {
		pairs = append(pairs, instruments.PairAssets{Symbol: symbol.Symbol, BaseAsset: symbol.Base})
	}

	legs := hedgeLegs(cfg, instruments.BuildHedgeMapping(insts, pairs, claims))

	for _, source := range cfg.Sources {
		if source.MarketType != "perp" {
			continue
		}
		for _, symbol := range cfg.Symbols {
			if _, ok := source.VenueSymbol(symbol); !ok {
				continue
			}
			leg, found := legFor(legs, symbol.Symbol, source.Source)
			if !found {
				t.Errorf("%s/%s got no hedge answer at all", symbol.Symbol, source.Source)
				continue
			}
			if leg.SpotSource == "" && leg.NoteVI == "" {
				t.Errorf("%s/%s has no spot leg and no reason", symbol.Symbol, source.Source)
			}
		}
	}
}
