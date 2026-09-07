package instruments

import (
	"fmt"
	"sort"
	"strings"

	"futures-arbitrage-scanner/exchanges"
)

// The spot↔perp hedge mapping (step 2.4): which spot market can hedge which
// perpetual, decided ONLY from what each venue itself declares.
//
// Risk R11 (docs/PLAN.md): a mispaired hedge is a coin-mismatched position —
// long one asset, short another, delta-neutral in neither. So the mapping is
// built by VALIDATION, never by symbol spelling, and everything that fails
// validation comes out as a named Rejection instead of a guess. Validation
// runs in both directions:
//
//   - config → venue: the base config.yaml maps under a standard symbol must
//     match the base the venue declares, and the quote config.yaml claims for
//     a source must match the quote the venue declares.
//   - venue → config: each native market may be claimed by exactly ONE
//     standard symbol, and every leg that finds no counterpart is reported,
//     not dropped.
//
// Two legs pair when they venue-declare the same quote, or when config.yaml
// DECLARES their two quotes equivalent (hedge.quote_equivalents — USD ≡ USDT
// is what lets the USD-quoted perps of Kraken, Hyperliquid and Paradex hedge
// against a USDT spot). That is still not a guess: the equivalence is an
// operator's written decision, it is carried on the pair as QuoteBridged so
// every figure derived from it can be labelled, and with nothing declared the
// mapping behaves exactly as it did before — a differing quote is refused.
//
// A venue that does not list a symbol at all simply has no instrument here —
// absence, not rejection, is how an unsupported venue self-excludes.

// The two market types that can take a hedge role. They match the values the
// fetchers stamp into Instrument.MarketType and the values config.yaml
// accepts for market_type; everything else (oracle, and "future" once dated
// futures arrive in phase 6) is refused by name.
const (
	marketSpot = "spot"
	marketPerp = "perp"
)

// PairAssets is what config.yaml declares one standard symbol to MEAN: which
// base coin it is. Deliberately no quote field: a standard symbol's quote
// does not constrain a venue market's quote — kraken_futures maps BTCUSDT to
// a USD-quoted market by design, and the quote a market must match is its
// SOURCE's declared quote_asset (SourceClaim), never the symbol suffix.
type PairAssets struct {
	Symbol    string // BTCUSDT
	BaseAsset string // BTC
}

// sameAsset compares two asset names case-insensitively. This function owns
// the case rule for the whole hedge mapping, deliberately:
//
//   - Venues declare assets in their own casing and the fetchers keep them
//     VERBATIM — Hyperliquid lists kPEPE, kSHIB, kBONK, kLUNC, kFLOKI, kDOGS
//     and kNEIRO, where the k- prefix means 1000× and upper-casing it would
//     invent an asset the venue never published.
//   - config.yaml's `base:` is equally load-bearing as written: with
//     `symbol_format: "{base}"` it IS the venue identifier, so config cannot
//     upper-case it either (and internal/config normalizes Quote but not
//     Base — this comparison is what makes that asymmetry harmless).
//
// Case-folding here, rather than at either producer, is what lets both sides
// stay literal. It does mean two venues spelling one asset with different
// case are treated as the same asset; that is the intended reading of "BTC"
// vs "btc", and a venue that ever ships two DIFFERENT assets separated only
// by case would need this rule revisited.
func sameAsset(a, b string) bool { return strings.EqualFold(a, b) }

// SourceClaim is what config.yaml claims about one source. Venue declarations
// are validated against it — a config that lies about a source's quote or
// market type must surface as a named rejection here, not stay hidden inside
// a silently mispriced comparison group.
type SourceClaim struct {
	Source     string
	MarketType string // "spot" | "perp" | "oracle"
	QuoteAsset string
	Tradable   bool
}

// QuoteEquivalents is config.yaml's declaration of which quote assets may
// hedge each other, as groups of asset names. It is deliberately a DECLARATION
// and not a rule this package derives: whether USDT may stand in for USD is an
// operator's risk decision (a depeg moves the two legs apart), so the code
// refuses to invent it and only honours what is written down.
//
// Comparison is case-insensitive, by sameAsset. An asset may appear in at most
// one group; internal/config refuses anything else at load.
type QuoteEquivalents [][]string

// quoteGroups indexes the declaration for lookup: upper-cased asset → group
// number. Assets in no group are absent, which makes equivalentQuotes false
// for them without a special case.
func quoteGroups(equivalents QuoteEquivalents) map[string]int {
	if len(equivalents) == 0 {
		return nil
	}
	groups := make(map[string]int, len(equivalents)*2)
	for i, group := range equivalents {
		for _, asset := range group {
			groups[strings.ToUpper(strings.TrimSpace(asset))] = i
		}
	}
	return groups
}

// equivalentQuotes reports whether config declared these two quotes able to
// hedge each other. Identical quotes never reach here — the caller checks
// sameAsset first, so this answers only the cross-quote question.
func equivalentQuotes(groups map[string]int, a, b string) bool {
	if groups == nil {
		return false
	}
	ga, oka := groups[strings.ToUpper(a)]
	gb, okb := groups[strings.ToUpper(b)]
	return oka && okb && ga == gb
}

// HedgePair is one validated spot↔perp combination: both legs venue-declare
// the same base, both are trading today, and their quotes either match or were
// DECLARED equivalent in config.yaml.
type HedgePair struct {
	Symbol    string
	BaseAsset string
	// QuoteAsset is the PERP leg's quote — the side the funding rate is
	// denominated in. When the two legs' quotes differ it does not describe
	// the spot leg; SpotQuoteAsset does, and QuoteBridged says they differ.
	QuoteAsset     string
	SpotQuoteAsset string
	// QuoteBridged is true when the two legs' quotes are different assets
	// joined only by a config declaration. Everything computed from such a
	// pair carries USDT/USD (or whatever the pair of quotes is) exposure that
	// no figure in this project deducts, so callers must LABEL it rather than
	// present it as a plain delta-neutral hedge.
	QuoteBridged bool
	Spot         exchanges.Instrument
	Perp         exchanges.Instrument
}

// Rejection is one instrument the mapping refused, and why. Refusals are
// first-class output: anything not provably pairable is named, never guessed
// at and never silently dropped.
type Rejection struct {
	Symbol string
	Source string
	Reason string
}

// HedgeMapping is the built table: every valid spot↔perp pair, and every
// refusal. Rebuild it whenever the registry refreshes — it is a pure
// function of its inputs, so identical rules produce an identical mapping.
type HedgeMapping struct {
	Pairs      []HedgePair
	Rejections []Rejection
}

// BuildHedgeMapping validates every instrument against the config's
// declarations and pairs the survivors. Output ordering is deterministic:
// pairs by (symbol, spot source, perp source), rejections by (symbol,
// source, reason).
func BuildHedgeMapping(insts []exchanges.Instrument, pairs []PairAssets, claims []SourceClaim, equivalents QuoteEquivalents) HedgeMapping {
	groups := quoteGroups(equivalents)
	baseBySymbol := make(map[string]string, len(pairs))
	for _, p := range pairs {
		baseBySymbol[p.Symbol] = p.BaseAsset
	}
	claimBySource := make(map[string]SourceClaim, len(claims))
	for _, c := range claims {
		claimBySource[c.Source] = c
	}

	var m HedgeMapping
	reject := func(inst exchanges.Instrument, reason string) {
		m.Rejections = append(m.Rejections, Rejection{Symbol: inst.Symbol, Source: inst.Source, Reason: reason})
	}

	// venue → config direction first: a native market claimed by two DIFFERENT
	// standard symbols is a mapping that is not one-to-one, and no
	// per-instrument check can decide which claimant is right — every claimant
	// is refused. internal/config refuses the same shape at load, so this is
	// defense in depth for the exported function, which any caller may feed.
	//
	// nativeClaims counts DISTINCT standard symbols, not entries: two copies
	// of one instrument are duplicate input (reported as such below), not a
	// contested market, and counting entries would give that case the wrong
	// diagnosis.
	nativeSymbols := make(map[string]map[string]bool, len(insts))
	symbolCopies := make(map[string]int, len(insts))
	for _, inst := range insts {
		nativeKey := inst.Source + "\x00" + inst.NativeSymbol
		if nativeSymbols[nativeKey] == nil {
			nativeSymbols[nativeKey] = make(map[string]bool, 1)
		}
		nativeSymbols[nativeKey][inst.Symbol] = true
		symbolCopies[inst.Source+"\x00"+inst.Symbol]++
	}

	validBySymbol := make(map[string][]exchanges.Instrument)
	for _, inst := range insts {
		if claimants := len(nativeSymbols[inst.Source+"\x00"+inst.NativeSymbol]); claimants > 1 {
			reject(inst, fmt.Sprintf("native market %s is claimed by %d standard symbols on %s — the config mapping is not one-to-one",
				inst.NativeSymbol, claimants, inst.Source))
			continue
		}
		if symbolCopies[inst.Source+"\x00"+inst.Symbol] > 1 {
			reject(inst, fmt.Sprintf("symbol appears %d times for %s — duplicate input", symbolCopies[inst.Source+"\x00"+inst.Symbol], inst.Source))
			continue
		}
		claim, known := claimBySource[inst.Source]
		if !known {
			reject(inst, "source is not declared in config — no quote claim to validate against")
			continue
		}
		if !claim.Tradable {
			reject(inst, fmt.Sprintf("source is declared not tradable (%s) — it can never be a hedge leg", claim.MarketType))
			continue
		}
		base, known := baseBySymbol[inst.Symbol]
		if !known {
			reject(inst, "not a configured symbol — nothing declares which base coin this should be")
			continue
		}
		if claim.MarketType != inst.MarketType {
			reject(inst, fmt.Sprintf("config declares market type %q but the venue data parsed as %q", claim.MarketType, inst.MarketType))
			continue
		}
		// Only spot and perp have a hedge role. config.yaml's validation also
		// accepts "future" (dated futures, phase 6) and "oracle" — refusing
		// them HERE, in the validation chain, keeps every refusal in one place
		// instead of leaving a silent drop in the pairing loop below.
		if inst.MarketType != marketSpot && inst.MarketType != marketPerp {
			reject(inst, fmt.Sprintf("market type %q is neither %s nor %s — no hedge role", inst.MarketType, marketSpot, marketPerp))
			continue
		}
		if inst.BaseAsset == "" || inst.QuoteAsset == "" {
			reject(inst, "venue declares no base/quote assets — refusing to pair by symbol spelling")
			continue
		}
		if !sameAsset(inst.BaseAsset, base) {
			reject(inst, fmt.Sprintf("venue declares base %s but config maps this market under %s (base %s) — a mispaired hedge is a coin-mismatched position",
				inst.BaseAsset, inst.Symbol, base))
			continue
		}
		if !sameAsset(inst.QuoteAsset, claim.QuoteAsset) {
			reject(inst, fmt.Sprintf("venue declares quote %s but config claims this source quotes %s", inst.QuoteAsset, claim.QuoteAsset))
			continue
		}
		if inst.Status != exchanges.StatusTrading {
			reject(inst, fmt.Sprintf("status %q — not tradable today", inst.Status))
			continue
		}
		validBySymbol[inst.Symbol] = append(validBySymbol[inst.Symbol], inst)
	}

	for symbol, valid := range validBySymbol {
		var spots, perps []exchanges.Instrument
		for _, inst := range valid {
			if inst.MarketType == marketSpot {
				spots = append(spots, inst)
			} else {
				perps = append(perps, inst) // validation left only spot and perp
			}
		}

		paired := make(map[string]bool, len(valid))
		for _, perp := range perps {
			for _, spot := range spots {
				bridged := !sameAsset(spot.QuoteAsset, perp.QuoteAsset)
				if bridged && !equivalentQuotes(groups, spot.QuoteAsset, perp.QuoteAsset) {
					continue
				}
				m.Pairs = append(m.Pairs, HedgePair{
					Symbol:         symbol,
					BaseAsset:      perp.BaseAsset,
					QuoteAsset:     perp.QuoteAsset,
					SpotQuoteAsset: spot.QuoteAsset,
					QuoteBridged:   bridged,
					Spot:           spot,
					Perp:           perp,
				})
				paired[perp.Source] = true
				paired[spot.Source] = true
			}
		}

		// Both directions of "no counterpart" are reported. A USD-quoted perp
		// among USDT spots hedges only with added USD/USDT exposure, so it
		// pairs when — and ONLY when — config declared those two quotes
		// equivalent; the message says which of the two cases this is, so a
		// refusal never reads as "impossible" when it is really "not declared".
		for _, perp := range perps {
			if !paired[perp.Source] {
				reject(perp, fmt.Sprintf("no spot market shares quote %s for %s (spot quotes: %s%s) — refused, not guessed",
					perp.QuoteAsset, symbol, quoteList(spots), equivalentsNote(groups, perp.QuoteAsset)))
			}
		}
		for _, spot := range spots {
			if !paired[spot.Source] {
				reject(spot, fmt.Sprintf("no perp market shares quote %s for %s (perp quotes: %s%s) — refused, not guessed",
					spot.QuoteAsset, symbol, quoteList(perps), equivalentsNote(groups, spot.QuoteAsset)))
			}
		}
	}

	sort.Slice(m.Pairs, func(i, j int) bool {
		a, b := m.Pairs[i], m.Pairs[j]
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		if a.Spot.Source != b.Spot.Source {
			return a.Spot.Source < b.Spot.Source
		}
		return a.Perp.Source < b.Perp.Source
	})
	sort.Slice(m.Rejections, func(i, j int) bool {
		a, b := m.Rejections[i], m.Rejections[j]
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.Reason < b.Reason
	})
	return m
}

// equivalentsNote explains, inside a refusal, whether config.yaml had anything
// to say about this quote — the difference between "these quotes cannot be
// hedged" and "nobody declared that they may be".
func equivalentsNote(groups map[string]int, quote string) string {
	if len(groups) == 0 {
		return "; no quote equivalence declared in config"
	}
	g, ok := groups[strings.ToUpper(quote)]
	if !ok {
		return fmt.Sprintf("; %s is in no declared quote-equivalence group", strings.ToUpper(quote))
	}
	var peers []string
	for asset, id := range groups {
		if id == g && !sameAsset(asset, quote) {
			peers = append(peers, asset)
		}
	}
	sort.Strings(peers)
	return fmt.Sprintf("; config declares %s equivalent to %s, and none of those is present either",
		strings.ToUpper(quote), strings.Join(peers, ", "))
}

// quoteList names the distinct quotes present on one side, sorted — "none"
// when the side is empty, so a rejection never trails an empty list.
func quoteList(side []exchanges.Instrument) string {
	seen := map[string]bool{}
	for _, inst := range side {
		seen[inst.QuoteAsset] = true
	}
	if len(seen) == 0 {
		return "none"
	}
	quotes := make([]string, 0, len(seen))
	for q := range seen {
		quotes = append(quotes, q)
	}
	sort.Strings(quotes)
	return strings.Join(quotes, ", ")
}

// LogLines renders the mapping for the startup/refresh log: one line per
// symbol listing its pairs, then one line per refusal.
func (m HedgeMapping) LogLines() []string {
	if len(m.Pairs) == 0 && len(m.Rejections) == 0 {
		return []string{"hedge mapping: no instruments to map yet"}
	}
	var lines []string
	for i := 0; i < len(m.Pairs); {
		j := i
		var combos []string
		for ; j < len(m.Pairs) && m.Pairs[j].Symbol == m.Pairs[i].Symbol; j++ {
			combo := m.Pairs[j].Spot.Source + "×" + m.Pairs[j].Perp.Source
			if m.Pairs[j].QuoteBridged {
				// Never let a bridged pair read like a same-quote one in a log
				// a human scans for what is actually hedgeable.
				combo += fmt.Sprintf(" (%s↔%s, declared equivalent)", m.Pairs[j].SpotQuoteAsset, m.Pairs[j].QuoteAsset)
			}
			combos = append(combos, combo)
		}
		lines = append(lines, fmt.Sprintf("%s: %d hedge pairs — %s",
			m.Pairs[i].Symbol, len(combos), strings.Join(combos, ", ")))
		i = j
	}
	for _, r := range m.Rejections {
		lines = append(lines, fmt.Sprintf("%s %s refused: %s", r.Symbol, r.Source, r.Reason))
	}
	return lines
}
