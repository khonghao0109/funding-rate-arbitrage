package scanner

import (
	"fmt"
	"sort"
	"strings"

	"futures-arbitrage-scanner/internal/fees"
)

// This file decides WHAT MAY BE COMPARED WITH WHAT.
//
// Until step 1.2 the scanner took the minimum and the maximum across every
// source it had a price for, which put spot, perpetual and an oracle into one
// comparison. That produces numbers nobody can act on: buying spot and selling a
// perpetual is a basis trade, not an arbitrage, and buying on Pyth is not
// possible at all because Pyth is a price oracle with no order book.
//
// The rule enforced here is that two sources may only be compared when they
// share a market type AND a quote asset. Everything that fails the rule is
// excluded with a reason the dashboard renders, so a source never simply
// disappears from the screen.

// Market types, as recorded on each sourceMeta.
const (
	marketTypeSpot   = "spot"
	marketTypePerp   = "perp"
	marketTypeFuture = "future"
	marketTypeOracle = "oracle"
)

// Reasons a source with a perfectly good price is still absent from every group.
// The set is fixed by docs/WS-CONTRACT.md §5.5.
const (
	reasonNoPrice       = "no_price"
	reasonOracle        = "oracle"
	reasonQuoteMismatch = "quote_mismatch"
	reasonStale         = "stale"
	reasonNoPeer        = "no_peer"
	// reasonUnregistered is an addition of step 1.2. quote_mismatch would be a
	// false statement here: nothing is known about the source at all, including
	// its quote. Documented in docs/WS-CONTRACT.md §5.5.
	reasonUnregistered = "unregistered"
)

// sourceGroup is a set of sources that may be compared with each other. It is
// the internal form of wireCrossVenueGroup, without the matrix: the alert path
// needs the membership but not the O(n^2) cells.
type sourceGroup struct {
	GroupID    string
	LabelVI    string
	MarketType string
	QuoteAsset string
	NoteVI     string
	Tradable   bool
	Sources    []string
}

// sourceMetaFor returns what the registry knows about a source.
func sourceMetaFor(source string) (sourceMeta, bool) {
	index, ok := sourceOrder[source]
	if !ok {
		return sourceMeta{}, false
	}
	return sourceRegistry[index], true
}

// groupIDFor is the comparison key: market type and quote asset together.
// "perp_usdt", "perp_usd", "spot_usdt".
func groupIDFor(meta sourceMeta) string {
	return meta.MarketType + "_" + strings.ToLower(meta.QuoteAsset)
}

func marketTypeLabelVI(marketType string) string {
	switch marketType {
	case marketTypeSpot:
		return "Spot"
	case marketTypePerp:
		return "Perpetual"
	case marketTypeFuture:
		return "Futures có kỳ hạn"
	case marketTypeOracle:
		return "Oracle"
	default:
		return marketType
	}
}

// groupNoteVI warns about anything that makes a group's numbers less than
// self-explanatory. A USDT group needs no warning; anything else does, because
// the operator's instinct is to compare it with the USDT venues.
func groupNoteVI(marketType, quoteAsset string) string {
	if quoteAsset == "USDT" {
		return ""
	}
	return fmt.Sprintf(
		"Nhóm này quote bằng %s chứ không phải USDT — không so trực tiếp với nhóm USDT, "+
			"vì chênh lệch giữa hai nhóm sẽ lẫn cả chênh %s/USDT. Tài sản ký quỹ của từng sàn "+
			"trong nhóm chưa được khảo sát (xem docs/DATA-REQUIREMENTS.md §3).",
		quoteAsset, quoteAsset)
}

// partitionSources splits the sources that have a usable, fresh price into
// groups that may be compared internally, and returns everything it had to leave
// out together with the reason.
//
// It walks sourceRegistry rather than the price map so that both the groups and
// the sources inside them come out in registry order. Deriving order from map
// iteration is what used to make the matrix columns reshuffle between messages.
func partitionSources(usable map[string]float64) ([]sourceGroup, []wireExcludedSource) {
	excluded := []wireExcludedSource{}
	byID := make(map[string]*sourceGroup, len(sourceRegistry))
	order := make([]string, 0, len(sourceRegistry))

	for _, meta := range sourceRegistry {
		price, present := usable[meta.Source]
		if !present || !isUsablePrice(price) {
			continue
		}

		// An oracle reports what a price should be, not where one can be
		// traded. It never belongs to a comparison, however fresh it is.
		if meta.MarketType == marketTypeOracle {
			excluded = append(excluded, wireExcludedSource{
				Source: meta.Source,
				Reason: reasonOracle,
				NoteVI: "Oracle, không giao dịch được — chỉ dùng để đối chiếu, không sinh cảnh báo",
			})
			continue
		}

		id := groupIDFor(meta)
		group, seen := byID[id]
		if !seen {
			group = &sourceGroup{
				GroupID:    id,
				LabelVI:    marketTypeLabelVI(meta.MarketType) + " · quote " + meta.QuoteAsset,
				MarketType: meta.MarketType,
				QuoteAsset: meta.QuoteAsset,
				NoteVI:     groupNoteVI(meta.MarketType, meta.QuoteAsset),
				// Every group here compares like with like, which is exactly
				// what the mixed "all" group of step 1.0 could not claim. The
				// flag is narrowed below by the members it actually gets.
				Tradable: true,
			}
			byID[id] = group
			order = append(order, id)
		}
		group.Sources = append(group.Sources, meta.Source)
		// The registry's own Tradable flag decides, not just the market type. A
		// source marked untradable for any other reason - a venue we can read
		// but not trade on - would otherwise be named as one side of an
		// executable spread. One such member makes the whole group reference
		// only, which is what tradable=false means to the dashboard, and the
		// alert path skips it. See docs/WS-CONTRACT.md §5.1.
		group.Tradable = group.Tradable && meta.Tradable
	}

	// A source no registry entry describes cannot be grouped: its market type
	// and quote asset are unknown, so there is no way to tell what it may be
	// compared with. Saying so beats both guessing and silence.
	unregistered := make([]string, 0)
	for source, price := range usable {
		if _, known := sourceOrder[source]; known {
			continue
		}
		if !isUsablePrice(price) {
			continue
		}
		unregistered = append(unregistered, source)
	}
	sort.Strings(unregistered)
	for _, source := range unregistered {
		excluded = append(excluded, wireExcludedSource{
			Source: source,
			Reason: reasonUnregistered,
			NoteVI: "Nguồn không có trong registry nên chưa biết loại thị trường và đồng quote",
		})
	}

	groups := make([]sourceGroup, 0, len(order))
	for _, id := range order {
		group := byID[id]
		// One source is not a comparison. Dropping it without a word would look
		// identical to the venue having died.
		if len(group.Sources) < 2 {
			excluded = append(excluded, wireExcludedSource{
				Source: group.Sources[0],
				Reason: reasonNoPeer,
				NoteVI: fmt.Sprintf(
					"Không có nguồn nào khác cùng %s và cùng quote %s để so sánh",
					marketTypeLabelVI(group.MarketType), group.QuoteAsset),
			})
			continue
		}
		groups = append(groups, *group)
	}

	return groups, excluded
}

// bestPair returns the cheapest and the dearest source in one group: buy where
// it is cheapest, sell where it is dearest.
//
// ok is false when the group cannot produce a comparison at all - fewer than two
// usable prices, or every price identical.
func bestPair(sources []string, prices map[string]float64) (buySource, sellSource string, ok bool) {
	var lowest, highest float64

	for _, source := range sources {
		price, present := prices[source]
		if !present || !isUsablePrice(price) {
			continue
		}
		if buySource == "" || price < lowest {
			lowest, buySource = price, source
		}
		if sellSource == "" || price > highest {
			highest, sellSource = price, source
		}
	}

	if buySource == "" || buySource == sellSource {
		return "", "", false
	}
	return buySource, sellSource, true
}

// buildMatrix computes every ordered pair inside one group.
func buildMatrix(sources []string, prices map[string]float64) map[string]map[string]wireSpreadCell {
	matrix := make(map[string]map[string]wireSpreadCell, len(sources))
	for _, buySource := range sources {
		row := make(map[string]wireSpreadCell, len(sources)-1)
		for _, sellSource := range sources {
			if buySource == sellSource {
				continue
			}
			grossPct := spreadGrossPct(prices[buySource], prices[sellSource])
			row[sellSource] = wireSpreadCell{
				SpreadGrossPct:     grossPct,
				SpreadAfterFeesPct: afterFeesPct(grossPct, buySource, sellSource),
			}
		}
		matrix[buySource] = row
	}
	return matrix
}

// buildBasis pairs spot against perpetual ON THE SAME VENUE.
//
// This is the number the funding strategy is built on, and it is deliberately a
// separate block from the cross-venue matrix: spot against perp is a basis, not
// an executable venue-to-venue spread, and step 1.0's single mixed group
// presented the two as the same thing.
func buildBasis(usable map[string]float64) []wireBasis {
	type legs struct{ spot, perp string }

	byVenue := make(map[string]*legs, len(sourceRegistry))
	order := make([]string, 0, len(sourceRegistry))

	for _, meta := range sourceRegistry {
		price, present := usable[meta.Source]
		if !present || !isUsablePrice(price) {
			continue
		}
		if meta.MarketType != marketTypeSpot && meta.MarketType != marketTypePerp {
			continue
		}

		venue, seen := byVenue[meta.Venue]
		if !seen {
			venue = &legs{}
			byVenue[meta.Venue] = venue
			order = append(order, meta.Venue)
		}
		if meta.MarketType == marketTypeSpot {
			venue.spot = meta.Source
		} else {
			venue.perp = meta.Source
		}
	}

	basis := []wireBasis{}
	for _, venueName := range order {
		venue := byVenue[venueName]
		if venue.spot == "" || venue.perp == "" {
			continue
		}

		spotMeta, _ := sourceMetaFor(venue.spot)
		perpMeta, _ := sourceMetaFor(venue.perp)
		// Same venue is not enough: a USD perp against a USDT spot would put the
		// currency spread into a number read as a funding basis.
		if spotMeta.QuoteAsset != perpMeta.QuoteAsset {
			continue
		}

		spotPrice, perpPrice := usable[venue.spot], usable[venue.perp]
		basis = append(basis, wireBasis{
			Venue:      venueName,
			SpotSource: venue.spot,
			PerpSource: venue.perp,
			SpotPrice:  spotPrice,
			PerpPrice:  perpPrice,
			// Signed, not a distance: a perp below spot is a negative basis and
			// pays the other way.
			BasisAbsQuote: perpPrice - spotPrice,
			BasisPct:      (perpPrice - spotPrice) / spotPrice * 100,
		})
	}
	return basis
}

// buildOracleDeviation reports how far each venue sits from the oracle. It is
// reference only and never produces an alert: an oracle cannot be traded, so a
// gap against it is information, not an opportunity.
//
// A stale oracle produces nothing, because usable holds only fresh prices - a
// deviation measured against a frozen oracle would describe a market that has
// since moved.
func buildOracleDeviation(usable map[string]float64) []wireOracleDeviation {
	deviations := []wireOracleDeviation{}

	for _, oracle := range sourceRegistry {
		if oracle.MarketType != marketTypeOracle {
			continue
		}
		oraclePrice, present := usable[oracle.Source]
		if !present || !isUsablePrice(oraclePrice) {
			continue
		}

		for _, meta := range sourceRegistry {
			if meta.MarketType == marketTypeOracle {
				continue
			}
			price, present := usable[meta.Source]
			if !present || !isUsablePrice(price) {
				continue
			}
			deviations = append(deviations, wireOracleDeviation{
				OracleSource: oracle.Source,
				Source:       meta.Source,
				DeviationPct: (price - oraclePrice) / oraclePrice * 100,
				// Pyth quotes in USD and six of the nine venues quote in USDT,
				// so most of these rows carry the USD/USDT spread as well as the
				// venue's own drift. buildBasis drops such a pair outright; this
				// block keeps it, because checking a venue against the oracle is
				// the whole point and the drift is usually the larger term - but
				// it must not pass as a clean measurement. Pricing the USDT leg
				// out needs a USDT/USD reference, which phase 2 adds.
				QuoteAssetMismatch: meta.QuoteAsset != oracle.QuoteAsset,
			})
		}
	}
	return deviations
}

// afterFeesPct is the gross spread with the commission of a complete round trip
// removed, or nil when either venue's fee schedule was never verified.
//
// nil is not a formality. Substituting zero for an unverified fee would publish
// the full gross spread as though it cost nothing to capture, which is the exact
// overstatement the contract keeps a separate field to prevent. The figure this
// returns is "after trading fees" and never "net profit": slippage and funding
// are still not in it. See internal/fees and CLAUDE.md rule 2.
func afterFeesPct(grossPct float64, buySource, sellSource string) *float64 {
	costPct, ok := fees.RoundTripTakerPct(fees.For(buySource), fees.For(sellSource))
	if !ok {
		return nil
	}
	afterFees := grossPct - costPct
	return &afterFees
}
