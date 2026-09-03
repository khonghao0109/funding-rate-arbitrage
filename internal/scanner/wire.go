package scanner

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// The types in this file are the frozen WebSocket contract between the scanner
// and the dashboard. The full specification, including which field carries real
// data at which step, is docs/WS-CONTRACT.md. Everything named wire* is on the
// wire: renaming a field or changing its type breaks static/app.js, which has no
// tests to catch it.
//
// Steps 1.1 to 1.3 fill these fields with real values. They must not reshape
// them.

// wireVersion is the contract version echoed in every message. The dashboard
// warns when it receives anything else.
const wireVersion = 1

// defaultStaleAfterSec is the fallback staleness threshold. It is per source
// because a thinly traded pair going quiet for a minute is normal, and a single
// global threshold would mark a healthy venue dead. Step 1.4 moves the value to
// config.yaml; step 1.1 starts enforcing it.
const defaultStaleAfterSec = 10

// startupGrace is how long a source may take to deliver its first message before
// it is called disconnected.
//
// It must be at least minDisconnectAfter: a venue that has never delivered
// should not be called dead sooner than one that delivered and then stopped. The
// clock starts before the connectors have even dialled, so this is the more
// forgiving of the two cases, not the less.
const startupGrace = 90 * time.Second

// minDisconnectAfter is the floor for the silence that counts as a dead venue.
const minDisconnectAfter = 45 * time.Second

// Data-level status of one price, decided by the backend. The browser clock is
// not comparable with the server clock, so the frontend only renders these.
const (
	statusLive    = "live"
	statusStale   = "stale"
	statusUnknown = "unknown" // no staleness measurement exists yet (step 1.1)
)

// Connection-level state of one source, filled at step 1.5.
const (
	stateConnected    = "connected"
	stateReconnecting = "reconnecting"
	stateDisconnected = "disconnected"
	stateUnknown      = "unknown"
)

// alertMinSpreadPct is the dashboard's default alert filter, and the threshold
// checkArbitrage uses to decide an opportunity is worth broadcasting.
const alertMinSpreadPct = 0.05

// sourceMeta describes one data source to the dashboard: what it is, how to draw
// it, and how it should be treated. It exists so the frontend stops hardcoding
// the source list, which it previously did in five separate places.
//
// Step 1.4 loads these from config.yaml instead of the literal below.
type sourceMeta struct {
	Source     string `json:"source"`      // wire key, venue + "_" + market type
	Venue      string `json:"venue"`       // binance, bybit, ...
	MarketType string `json:"market_type"` // spot | perp | future | oracle
	QuoteAsset string `json:"quote_asset"`
	Tradable   bool   `json:"tradable"`

	Label            string `json:"label"`
	ShortLabel       string `json:"short_label"` // matrix column header
	Color            string `json:"color"`
	LineStyle        string `json:"line_style"` // solid | dashed | dotted
	EnabledByDefault bool   `json:"enabled_by_default"`

	StaleAfterSec int64 `json:"stale_after_sec"`
	MakerFeeBps   int   `json:"maker_fee_bps"` // 0 until step 1.3
	TakerFeeBps   int   `json:"taker_fee_bps"` // 0 until step 1.3
}

// sourceRegistry is the single source of truth for what the dashboard renders.
// Order matters: it fixes the column order of the spread matrix, which used to
// reshuffle on every message because it came from Go map iteration.
//
// StaleAfterSec is measured, not guessed. Over a 5 minute observation of all
// four symbols during active trading, the worst gap between two consecutive
// updates was:
//
//	hyperliquid 6.05s · bybit_spot 4.02s · binance_spot 3.85s · gate 3.37s
//	kraken 2.95s · bybit 2.25s · paradex 1.86s · okx 1.00s · binance 0.85s
//
// Each threshold leaves roughly 3x headroom over its own worst observed gap.
// These feeds are change-driven, so a genuinely quiet market produces long gaps
// with nothing wrong; a single global threshold would mark the slower venues
// dead. The numbers hold for four majors in active hours - thin pairs and quiet
// hours will need revisiting, which is the adaptive-threshold work PLAN.md
// records as phase 1 debt.
//
// Pyth is the exception: it delivered nothing at all during the observation, so
// it has no measurement and keeps the default. Its real cadence has to be
// measured once its feed works.
//
// MarketType and QuoteAsset are static facts about each venue and are recorded
// here now; step 1.2 is what starts *using* them to split the comparison into
// separate blocks. Fee fields stay at zero until step 1.3 builds the fee table.
var sourceRegistry = []sourceMeta{
	{Source: "binance_futures", Venue: "binance", MarketType: "perp", QuoteAsset: "USDT", Tradable: true,
		Label: "Binance Futures", ShortLabel: "BIN-F", Color: "#f0b90b", LineStyle: "solid",
		EnabledByDefault: true, StaleAfterSec: defaultStaleAfterSec},
	{Source: "bybit_futures", Venue: "bybit", MarketType: "perp", QuoteAsset: "USDT", Tradable: true,
		Label: "Bybit Futures", ShortLabel: "BYB-F", Color: "#f7931a", LineStyle: "solid",
		EnabledByDefault: true, StaleAfterSec: defaultStaleAfterSec},
	{Source: "hyperliquid_futures", Venue: "hyperliquid", MarketType: "perp", QuoteAsset: "USD", Tradable: true,
		Label: "Hyperliquid Futures", ShortLabel: "HYP", Color: "#97FCE4", LineStyle: "solid",
		EnabledByDefault: true, StaleAfterSec: 20}, // worst observed gap 6.05s
	// Kraken quotes in USD, not USDT: PF_XBTUSD against BTCUSDT carries the
	// USD/USDT spread as well. Step 1.2 puts it in its own group for that reason.
	// See docs/DATA-REQUIREMENTS.md §3.
	{Source: "kraken_futures", Venue: "kraken", MarketType: "perp", QuoteAsset: "USD", Tradable: true,
		Label: "Kraken Futures", ShortLabel: "KRK-F", Color: "#5a5aff", LineStyle: "solid",
		EnabledByDefault: true, StaleAfterSec: defaultStaleAfterSec},
	{Source: "okx_futures", Venue: "okx", MarketType: "perp", QuoteAsset: "USDT", Tradable: true,
		Label: "OKX Futures", ShortLabel: "OKX-F", Color: "#1890ff", LineStyle: "solid",
		EnabledByDefault: true, StaleAfterSec: defaultStaleAfterSec},
	{Source: "gate_futures", Venue: "gate", MarketType: "perp", QuoteAsset: "USDT", Tradable: true,
		Label: "Gate.io Futures", ShortLabel: "GAT-F", Color: "#6c5ce7", LineStyle: "solid",
		EnabledByDefault: true, StaleAfterSec: 15}, // worst observed gap 3.37s, and its book_ticker is change-driven
	{Source: "paradex_futures", Venue: "paradex", MarketType: "perp", QuoteAsset: "USD", Tradable: true,
		Label: "Paradex Futures", ShortLabel: "PDX", Color: "#ff6b6b", LineStyle: "solid",
		EnabledByDefault: true, StaleAfterSec: defaultStaleAfterSec},
	{Source: "binance_spot", Venue: "binance", MarketType: "spot", QuoteAsset: "USDT", Tradable: true,
		Label: "Binance Spot", ShortLabel: "BIN-S", Color: "#ffb347", LineStyle: "dashed",
		EnabledByDefault: true, StaleAfterSec: 15}, // worst observed gap 3.85s
	{Source: "bybit_spot", Venue: "bybit", MarketType: "spot", QuoteAsset: "USDT", Tradable: true,
		Label: "Bybit Spot", ShortLabel: "BYB-S", Color: "#f7931a", LineStyle: "dashed",
		EnabledByDefault: true, StaleAfterSec: 15}, // worst observed gap 4.02s
	// Pyth is a price oracle, not a venue. Nothing can be bought or sold on it,
	// so it must never appear in a tradable comparison. Step 1.2 enforces this
	// in the grouping logic; the flag is recorded here.
	{Source: "pyth", Venue: "pyth", MarketType: "oracle", QuoteAsset: "USD", Tradable: false,
		Label: "Pyth Oracle", ShortLabel: "PYTH", Color: "#00ff88", LineStyle: "dotted",
		EnabledByDefault: true, StaleAfterSec: defaultStaleAfterSec},
}

// sourceOrder maps a source to its position in sourceRegistry, for deterministic
// ordering of anything derived from a map.
var sourceOrder = func() map[string]int {
	order := make(map[string]int, len(sourceRegistry))
	for i, s := range sourceRegistry {
		order[s.Source] = i
	}
	return order
}()

// sortExcluded keeps the exclusion list in registry order so it does not
// reshuffle between messages.
func sortExcluded(excluded []wireExcludedSource) {
	sort.Slice(excluded, func(i, j int) bool {
		oi, iKnown := sourceOrder[excluded[i].Source]
		oj, jKnown := sourceOrder[excluded[j].Source]
		switch {
		case iKnown && jKnown:
			return oi < oj
		case iKnown != jKnown:
			return iKnown
		default:
			return excluded[i].Source < excluded[j].Source
		}
	})
}

// staleAfter is how long a source may go without sending before its data stops
// being usable.
//
// It is per source on purpose: a thinly traded pair going quiet for a minute is
// normal, and one global threshold would either mark a healthy venue dead or be
// so loose that a genuinely dead feed keeps producing signals. An unregistered
// source gets the default rather than zero, which would mark it stale instantly.
func staleAfter(source string) time.Duration {
	if index, ok := sourceOrder[source]; ok {
		// A registered source with the threshold left unset must not get zero:
		// that marks every one of its prices stale on arrival and drops it from
		// every comparison while it is streaming perfectly well.
		if sec := sourceRegistry[index].StaleAfterSec; sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	return defaultStaleAfterSec * time.Second
}

// disconnectAfter is how long a source may be silent before it is called
// disconnected, as opposed to merely holding a stale quote.
//
// It is deliberately much longer than staleAfter. Those are different claims:
// "this quote is too old to compare" is routine on a change-driven feed in a
// quiet market, while "this venue is gone" is an alarm. Gate, Kraken, Paradex
// and Pyth deliver no trades at all, so their only liveness signal is a book
// change - sharing the price threshold would paint a healthy socket dead.
func disconnectAfter(source string) time.Duration {
	if d := staleAfter(source) * 3; d > minDisconnectAfter {
		return d
	}
	return minDisconnectAfter
}

// priceStatus decides whether a price can still be trusted.
//
// It measures from RecvAt and nothing else. VenueTimeMs is not consulted: it is
// 0 for the venues that publish no timestamp, and where it does exist it
// measures the venue's clock against ours, which is skew, not freshness.
func priceStatus(point PricePoint, threshold time.Duration, now time.Time) string {
	if point.RecvAt.IsZero() {
		return statusUnknown
	}
	if now.Sub(point.RecvAt) > threshold {
		return statusStale
	}
	return statusLive
}

// sourceState is connection health, inferred at step 1.1 from silence across
// every symbol rather than reported by the connector.
//
// A venue can be connected while one thin pair goes quiet, which is why this is
// separate from priceStatus. Step 1.5 replaces the inference with what the
// connector actually knows, and fills reconnect_count and uptime_sec.
// startedAt is when the scanner came up, used to judge a source that has never
// sent anything: "nothing yet" is unknown for the first few seconds and
// disconnected after that.
func sourceState(lastMsgAt time.Time, threshold time.Duration, startedAt, now time.Time) string {
	if lastMsgAt.IsZero() {
		// A registered source that has never delivered. Reporting it as unknown
		// forever would hide a venue that never connected at all - which is how
		// Pyth silently disappeared from the dashboard entirely.
		if !startedAt.IsZero() && now.Sub(startedAt) > startupGrace {
			return stateDisconnected
		}
		return stateUnknown
	}
	if now.Sub(lastMsgAt) > threshold {
		return stateDisconnected
	}
	return stateConnected
}

// wireCostBasis states which costs have been deducted from the after-fee numbers
// and which have not. It is the contract's guard against presenting a gross
// figure as profit: the dashboard renders `excluded` verbatim.
type wireCostBasis struct {
	Model    string   `json:"model"` // none | taker_both_legs (step 1.3)
	Applied  []string `json:"applied"`
	Excluded []string `json:"excluded"`
	NoteVI   string   `json:"note_vi"`
}

type wireMeta struct {
	Type              string        `json:"type"`
	V                 int           `json:"v"`
	ServerTimeMs      int64         `json:"server_time_ms"`
	Symbols           []string      `json:"symbols"`
	DefaultSymbol     string        `json:"default_symbol"`
	AlertMinSpreadPct float64       `json:"alert_min_spread_pct"`
	CostBasis         wireCostBasis `json:"cost_basis"`
	Sources           []sourceMeta  `json:"sources"`
}

// wirePricePoint is one price plus everything needed to judge whether it can be
// trusted. Staleness is measured from RecvAtMs, never from VenueTimeMs: three of
// the eight venues fill their own timestamp field with time.Now(), so it always
// looks fresh even when the venue has stopped sending. See docs/WS-CONTRACT.md §4.1.
type wirePricePoint struct {
	Price       float64 `json:"price"`
	VenueTimeMs int64   `json:"venue_time_ms"` // 0 when the venue does not provide one
	RecvAtMs    int64   `json:"recv_at_ms"`    // 0 until step 1.1
	AgeMs       int64   `json:"age_ms"`        // -1 when not measurable
	Status      string  `json:"status"`

	// Top of book. Reserved by step 1.0 and filled by step 1.2.
	//
	// This is a first-order liquidity filter only: it says what is available at
	// the best price, not what a $60k order would actually fill at. Full depth
	// arrives over REST at step 2.7. See PLAN.md §7.4.
	//
	// 0 means "not known", NOT "no liquidity". Quantities are in coin, never in
	// contracts: OKX, Gate, Kraken and Paradex leave them 0 because converting
	// their contract counts needs an instrument registry that does not exist
	// yet. See exchanges.OrderbookData for the measurements behind that.
	BestBid        float64 `json:"best_bid"`
	BestAsk        float64 `json:"best_ask"`
	BestBidQtyCoin float64 `json:"best_bid_qty_coin"`
	BestAskQtyCoin float64 `json:"best_ask_qty_coin"`
}

// wireSourceStatus is connection health, which is a property of the source and
// not of any one symbol: a venue can stay connected while a thin pair goes quiet.
type wireSourceStatus struct {
	State          string `json:"state"`
	LastMsgAtMs    int64  `json:"last_msg_at_ms"`  // step 1.1
	ReconnectCount int    `json:"reconnect_count"` // step 1.5
	UptimeSec      int64  `json:"uptime_sec"`      // step 1.5
}

type wirePrices struct {
	Type         string                               `json:"type"`
	V            int                                  `json:"v"`
	ServerTimeMs int64                                `json:"server_time_ms"`
	Prices       map[string]map[string]wirePricePoint `json:"prices"`
	SourceStatus map[string]wireSourceStatus          `json:"source_status"`
}

// wireSpreadCell keeps the gross figure and the after-fee figure in separate
// fields so neither can be mistaken for the other. SpreadAfterFeesPct is null
// until step 1.3, and even then it is not net profit: slippage needs order book
// depth, which does not arrive before phase 2.
type wireSpreadCell struct {
	SpreadGrossPct     float64  `json:"spread_gross_pct"`
	SpreadAfterFeesPct *float64 `json:"spread_after_fees_pct"`
}

// wireCrossVenueGroup is a set of sources that can meaningfully be compared with
// each other: same market type and same quote asset. A matrix is only ever built
// within a group, never across two.
type wireCrossVenueGroup struct {
	GroupID    string `json:"group_id"`
	LabelVI    string `json:"label_vi"`
	MarketType string `json:"market_type"`
	QuoteAsset string `json:"quote_asset"`
	Tradable   bool   `json:"tradable"`
	NoteVI     string `json:"note_vi"`

	Sources []string                             `json:"sources"`
	Matrix  map[string]map[string]wireSpreadCell `json:"matrix"`
}

// wireBasis is spot against perp on the same venue. Empty until step 1.2.
type wireBasis struct {
	Venue         string  `json:"venue"`
	SpotSource    string  `json:"spot_source"`
	PerpSource    string  `json:"perp_source"`
	SpotPrice     float64 `json:"spot_price"`
	PerpPrice     float64 `json:"perp_price"`
	BasisAbsQuote float64 `json:"basis_abs_quote"` // in the group's quote asset, not USD
	BasisPct      float64 `json:"basis_pct"`
}

// wireOracleDeviation is reference only and never produces an alert.
type wireOracleDeviation struct {
	OracleSource string  `json:"oracle_source"`
	Source       string  `json:"source"`
	DeviationPct float64 `json:"deviation_pct"`

	// QuoteAssetMismatch says the venue and the oracle do not quote in the same
	// asset, so the deviation carries a currency spread on top of the venue's
	// own drift. Added by step 1.2 with a documented default of false; the
	// contract permits adding a field, never reshaping one. See WS-CONTRACT §5.3.
	QuoteAssetMismatch bool `json:"quote_asset_mismatch"`
}

// wireExcludedSource explains why a source with a price is absent from every
// group, so the dashboard can say what happened instead of silently dropping it.
type wireExcludedSource struct {
	Source string `json:"source"`
	// no_price is the only reason emitted at step 1.0; the rest arrive with the
	// grouping and staleness work in steps 1.1 and 1.2.
	Reason string `json:"reason"` // no_price | oracle | quote_mismatch | stale | disconnected | no_peer
	NoteVI string `json:"note_vi"`
}

type wireSpreads struct {
	Type         string `json:"type"`
	V            int    `json:"v"`
	ServerTimeMs int64  `json:"server_time_ms"`
	Symbol       string `json:"symbol"`

	CrossVenueGroups []wireCrossVenueGroup `json:"cross_venue_groups"`
	Basis            []wireBasis           `json:"basis"`
	OracleDeviation  []wireOracleDeviation `json:"oracle_deviation"`
	ExcludedSources  []wireExcludedSource  `json:"excluded_sources"`
}

type wireOpportunity struct {
	ID     string `json:"id"`
	Symbol string `json:"symbol"`
	// Kind is cross_venue | basis.
	//
	// Only cross_venue is ever emitted: an alert is raised inside one tradable
	// group, so both sources share a market type and a quote asset and an oracle
	// can never appear at either end. A basis is reported in wireSpreads.Basis
	// and deliberately raises no alert - it is the funding trade's input, not an
	// arbitrage.
	Kind       string `json:"kind"`
	GroupID    string `json:"group_id"`
	BuySource  string `json:"buy_source"`
	SellSource string `json:"sell_source"`

	BuyPrice  float64 `json:"buy_price"`
	SellPrice float64 `json:"sell_price"`

	SpreadGrossPct     float64  `json:"spread_gross_pct"`
	SpreadAfterFeesPct *float64 `json:"spread_after_fees_pct"`

	DetectedAtMs int64 `json:"detected_at_ms"`
}

type wireArbitrage struct {
	Type         string          `json:"type"`
	V            int             `json:"v"`
	ServerTimeMs int64           `json:"server_time_ms"`
	Opportunity  wireOpportunity `json:"opportunity"`
}

// newWireMeta builds the one-off message the dashboard uses to construct its
// source list, symbol selector and cost disclaimer.
func newWireMeta(symbols []string, nowMs int64) wireMeta {
	defaultSymbol := ""
	if len(symbols) > 0 {
		defaultSymbol = symbols[0]
	}

	return wireMeta{
		Type:              "meta",
		V:                 wireVersion,
		ServerTimeMs:      nowMs,
		Symbols:           symbols,
		DefaultSymbol:     defaultSymbol,
		AlertMinSpreadPct: alertMinSpreadPct,
		CostBasis: wireCostBasis{
			Model:    "none",
			Applied:  []string{},
			Excluded: []string{"taker_fee", "maker_fee", "slippage", "funding"},
			NoteVI:   "Số hiển thị là chênh lệch THÔ, chưa trừ bất kỳ chi phí nào.",
		},
		Sources: sourceRegistry,
	}
}

// newWirePrices wraps the current snapshot.
//
// A stale price is kept and labelled, not dropped: removing the row would make a
// dead venue disappear from the dashboard, which reads as "nothing to report"
// rather than "this feed died".
func newWirePrices(prices map[string]map[string]PricePoint, lastMsgAt map[string]time.Time, startedAt, now time.Time) wirePrices {
	out := wirePrices{
		Type:         "prices",
		V:            wireVersion,
		ServerTimeMs: now.UnixMilli(),
		Prices:       make(map[string]map[string]wirePricePoint, len(prices)),
		SourceStatus: make(map[string]wireSourceStatus),
	}

	for symbol, sourcePrices := range prices {
		points := make(map[string]wirePricePoint, len(sourcePrices))
		for source, point := range sourcePrices {
			// The source is known either way, but a non-positive price is not
			// data: shipping it renders as "$0.000000" and drags the chart line
			// to zero. checkArbitrage drops it for the same reason.
			if _, seen := out.SourceStatus[source]; !seen {
				out.SourceStatus[source] = newWireSourceStatus(source, lastMsgAt[source], startedAt, now)
			}
			if !isUsablePrice(point.Price) {
				continue
			}

			ageMs := int64(-1)
			if !point.RecvAt.IsZero() {
				ageMs = now.Sub(point.RecvAt).Milliseconds()
			}
			recvAtMs := int64(0)
			if !point.RecvAt.IsZero() {
				recvAtMs = point.RecvAt.UnixMilli()
			}

			points[source] = wirePricePoint{
				Price:          point.Price,
				VenueTimeMs:    point.VenueTimeMs,
				RecvAtMs:       recvAtMs,
				AgeMs:          ageMs,
				Status:         priceStatus(point, staleAfter(source), now),
				BestBid:        point.BestBid,
				BestAsk:        point.BestAsk,
				BestBidQtyCoin: point.BestBidQtyCoin,
				BestAskQtyCoin: point.BestAskQtyCoin,
			}
		}
		out.Prices[symbol] = points
	}

	// Every registered source belongs in the status map, including one that has
	// never delivered a single message. Omitting it makes a venue that never
	// connected indistinguishable from one that does not exist - which is
	// exactly how a dead Pyth feed vanished from the dashboard without a trace.
	for _, meta := range sourceRegistry {
		if _, seen := out.SourceStatus[meta.Source]; !seen {
			out.SourceStatus[meta.Source] = newWireSourceStatus(meta.Source, lastMsgAt[meta.Source], startedAt, now)
		}
	}

	return out
}

func newWireSourceStatus(source string, lastMsgAt, startedAt, now time.Time) wireSourceStatus {
	lastMsgAtMs := int64(0)
	if !lastMsgAt.IsZero() {
		lastMsgAtMs = lastMsgAt.UnixMilli()
	}
	return wireSourceStatus{
		State:       sourceState(lastMsgAt, disconnectAfter(source), startedAt, now),
		LastMsgAtMs: lastMsgAtMs,
		// Filled by step 1.5, which is where the connector reports what it
		// actually knows about its own connection.
		ReconnectCount: 0,
		UptimeSec:      0,
	}
}

// isUsablePrice reports whether a price can be compared and serialized.
//
// The test is deliberately positive: `price <= 0` is false for NaN, so a NaN
// would pass an exclusion test, survive to json.Marshal and fail there. A venue
// can produce one - strconv.ParseFloat accepts the literal "NaN" with a nil
// error - so a malformed payload reaches this.
func isUsablePrice(price float64) bool {
	return price > 0 && !math.IsInf(price, 0)
}

// spreadGrossPct is the percentage move from buyPrice to sellPrice, before any
// cost is deducted. A venue reporting zero yields zero rather than an infinity,
// which would serialize to invalid JSON and drop the whole message.
func spreadGrossPct(buyPrice, sellPrice float64) float64 {
	if !isUsablePrice(buyPrice) || !isUsablePrice(sellPrice) {
		return 0
	}
	return ((sellPrice - buyPrice) / buyPrice) * 100
}

// newWireSpreads builds the spread message for one symbol.
//
// The three blocks are deliberately separate calculations, not three views of
// one number: a cross-venue spread is executable, a basis is the funding trade's
// input, and an oracle deviation is a reference. Step 1.0 shipped a single group
// holding every source because the grouping rules did not exist yet; step 1.2
// replaced it with one group per (market type, quote asset). The frontend
// already iterated the array, so it needed no change.
func newWireSpreads(symbol string, sourcePrices map[string]float64, excluded []wireExcludedSource, nowMs int64) wireSpreads {
	groups, groupingExcluded := partitionSources(sourcePrices)

	// Copy before appending: the caller's slice must not grow under it.
	allExcluded := make([]wireExcludedSource, 0, len(excluded)+len(groupingExcluded))
	allExcluded = append(allExcluded, excluded...)
	allExcluded = append(allExcluded, groupingExcluded...)
	sortExcluded(allExcluded)

	crossVenue := make([]wireCrossVenueGroup, 0, len(groups))
	for _, group := range groups {
		crossVenue = append(crossVenue, wireCrossVenueGroup{
			GroupID:    group.GroupID,
			LabelVI:    group.LabelVI,
			MarketType: group.MarketType,
			QuoteAsset: group.QuoteAsset,
			Tradable:   group.Tradable,
			NoteVI:     group.NoteVI,
			Sources:    group.Sources,
			Matrix:     buildMatrix(group.Sources, sourcePrices),
		})
	}

	return wireSpreads{
		Type:             "spreads",
		V:                wireVersion,
		ServerTimeMs:     nowMs,
		Symbol:           symbol,
		CrossVenueGroups: crossVenue,
		Basis:            buildBasis(sourcePrices),
		OracleDeviation:  buildOracleDeviation(sourcePrices),
		ExcludedSources:  allExcluded,
	}
}

// newWireOpportunity builds one detected opportunity. The identifier is derived
// from the pair and the instant rather than generated in the browser, so the same
// event keeps the same identity across reconnects and future persistence.
func newWireOpportunity(symbol, groupID, buySource, sellSource string, buyPrice, sellPrice float64, detectedAtMs int64) wireOpportunity {
	return wireOpportunity{
		ID:                 fmt.Sprintf("%s|%s|%s|%s|%d", symbol, groupID, buySource, sellSource, detectedAtMs),
		Symbol:             symbol,
		Kind:               "cross_venue",
		GroupID:            groupID,
		BuySource:          buySource,
		SellSource:         sellSource,
		BuyPrice:           buyPrice,
		SellPrice:          sellPrice,
		SpreadGrossPct:     spreadGrossPct(buyPrice, sellPrice),
		SpreadAfterFeesPct: nil,
		DetectedAtMs:       detectedAtMs,
	}
}
