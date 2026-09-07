package main

import (
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/instruments"
	"futures-arbitrage-scanner/internal/scanner"
)

// hedgeLegs projects the instrument registry's hedge mapping onto the one
// question the funding table asks: for this pair on this perpetual, which spot
// market can hold the other leg, and if none, why not.
//
// This translation lives here, not in internal/scanner, so the wire layer never
// has to import the instrument registry. It is also the whole reason the
// dashboard can tell an attractive rate apart from an actionable one: three of
// the seven perp venues quote USD against spot markets that only quote USDT, so
// their rates are real and their trades are not.
func hedgeLegs(cfg config.Config, mapping instruments.HedgeMapping) []scanner.HedgeLeg {
	// Candidate spot legs per perp, in the mapping's own deterministic order.
	candidates := make(map[string][]string)
	for _, pair := range mapping.Pairs {
		key := pair.Symbol + "|" + pair.Perp.Source
		candidates[key] = append(candidates[key], pair.Spot.Source)
	}

	legs := make([]scanner.HedgeLeg, 0, len(candidates)+len(mapping.Rejections))
	seen := make(map[string]bool, len(candidates))

	for _, pair := range mapping.Pairs {
		key := pair.Symbol + "|" + pair.Perp.Source
		if seen[key] {
			continue
		}
		seen[key] = true
		chosen, note := chooseSpotLeg(cfg, candidates[key])
		legs = append(legs, scanner.HedgeLeg{
			Symbol:     pair.Symbol,
			PerpSource: pair.Perp.Source,
			SpotSource: chosen,
			NoteVI:     note,
		})
	}

	// A refusal is first-class output: it is the difference between "no hedge
	// here" and "the mapping has not been built yet", and only the venue's own
	// words explain which.
	for _, rejection := range mapping.Rejections {
		key := rejection.Symbol + "|" + rejection.Source
		if seen[key] || !isPerpSource(cfg, rejection.Source) {
			continue
		}
		seen[key] = true
		legs = append(legs, scanner.HedgeLeg{
			Symbol:     rejection.Symbol,
			PerpSource: rejection.Source,
			NoteVI:     "Không ghép được chân spot: " + rejection.Reason,
		})
	}
	return legs
}

// chooseSpotLeg is config.CheapestVerifiedSpot — kept as a name here so the
// call site reads as what it decides, but the RULE lives in one place, because
// cmd/backtest must pick the same leg (step 3.5 compares the two).
func chooseSpotLeg(cfg config.Config, candidates []string) (source, noteVI string) {
	return cfg.CheapestVerifiedSpot(candidates)
}

func isPerpSource(cfg config.Config, source string) bool {
	s, ok := cfg.SourceByName(source)
	return ok && s.MarketType == "perp"
}
