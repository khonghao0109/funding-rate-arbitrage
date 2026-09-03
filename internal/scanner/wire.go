package scanner

import (
	"fmt"
	"math"
	"sort"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/fees"
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

// defaultStaleAfterSec is the fallback staleness threshold for a source whose
// own value is missing. config.yaml normally supplies one per source - a thinly
// traded pair going quiet for a minute is normal, and a single global threshold
// would mark a healthy venue dead - so this only ever applies to a source the
// registry does not know at all.
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
// checkArbitrage uses to decide an opportunity is worth broadcasting. Configure
// sets it from config.yaml.
var alertMinSpreadPct = 0.05

// costModelTakerRoundTrip names the cost basis on the wire: a taker fill on all
// four legs of opening and closing a two-venue position.
//
// alertMinSpreadPct above stays measured on the GROSS spread, deliberately.
// Gating alerts on the after-fee figure would silence almost everything - a
// round trip costs about 0.19% and cross-venue spreads on majors are a fraction
// of that - and deciding what is worth acting on is the signal work of phase 3,
// not a side effect of adding a fee table. Every alert now carries its after-fee
// figure, so nothing is overstated in the meantime.
const costModelTakerRoundTrip = "taker_round_trip"

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

	// Commission at the venue's default tier, in FRACTIONAL basis points. Real
	// schedules are not whole bps - Hyperliquid's maker leg is 1.5 and Paradex's
	// is 0.3 - so step 1.3 amended docs/CONVENTIONS.md §1.2 rather than round
	// them away. Filled from internal/fees, the single source of truth.
	//
	// FeeVerified separates "the fee is zero" from "the fee was never looked
	// up": both leave the numbers at 0, and four venues are in the second case.
	// Nothing may compute a cost from an unverified schedule.
	MakerFeeBps float64 `json:"maker_fee_bps"`
	TakerFeeBps float64 `json:"taker_fee_bps"`
	FeeVerified bool    `json:"fee_verified"`
}

// sourceRegistry is what the dashboard renders, built from config.yaml by
// Configure. Order matters: it fixes the column order of the spread matrix and
// the row order of the source list, which used to reshuffle on every message
// because it came from Go map iteration.
//
// It is package-level and written exactly once, before any goroutine starts.
// Everything downstream reads it.
var sourceRegistry []sourceMeta

// Configure installs the configuration the scanner runs on. It must be called
// before New, and before anything reads the registry.
//
// The registry used to be a literal here, with the venue facts, the measured
// staleness thresholds and the fee citations as Go comments beside them. Step
// 1.4 moved all of it to config.yaml, where the operator who has to change it
// can read the reasoning next to the value - and where adding a pair or a venue
// is no longer a Go edit.
func Configure(cfg config.Config) {
	registry := make([]sourceMeta, 0, len(cfg.Sources))
	for _, source := range cfg.Sources {
		registry = append(registry, sourceMeta{
			Source:           source.Source,
			Venue:            source.Venue,
			MarketType:       source.MarketType,
			QuoteAsset:       source.QuoteAsset,
			Tradable:         source.Tradable,
			Label:            source.Label,
			ShortLabel:       source.ShortLabel,
			Color:            source.Color,
			LineStyle:        source.LineStyle,
			EnabledByDefault: source.EnabledByDefault,
			StaleAfterSec:    source.StaleAfterSec,
			MakerFeeBps:      source.Fee.MakerBps,
			TakerFeeBps:      source.Fee.TakerBps,
			FeeVerified:      source.Fee.Verified,
		})
	}

	sourceRegistry = registry
	sourceOrder = indexRegistry(registry)
	alertMinSpreadPct = cfg.Scanner.AlertMinSpreadPct
}

// scheduleFor is the fee schedule of one source, for the cost calculation.
func scheduleFor(source string) fees.Schedule {
	meta, ok := sourceMetaFor(source)
	if !ok {
		return fees.Schedule{Source: source}
	}
	return fees.Schedule{
		Source:      meta.Source,
		MakerFeeBps: meta.MakerFeeBps,
		TakerFeeBps: meta.TakerFeeBps,
		Verified:    meta.FeeVerified,
	}
}

// sourceOrder maps a source to its position in sourceRegistry, for deterministic
// ordering of anything derived from a map. Configure rebuilds it with the
// registry; the two must never be set apart.
var sourceOrder = map[string]int{}

func indexRegistry(registry []sourceMeta) map[string]int {
	order := make(map[string]int, len(registry))
	for i, meta := range registry {
		order[meta.Source] = i
	}
	return order
}

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

// sourceConn is what one connector reported about its own socket: the last
// transition it announced, when the current connection was established, and how
// many times it has had to reconnect since start-up.
//
// Filled at step 1.5. Before that the scanner had no channel back from the
// connectors and could only infer from silence.
type sourceConn struct {
	State          exchanges.ConnState
	ConnectedSince time.Time
	ReconnectCount int

	// EverConnected separates "has connected before" from "has reported before".
	// Without it, a source whose FIRST dial fails reports reconnecting and then
	// connected, and that first working connection counts as a reconnect - which
	// contradicts docs/WS-CONTRACT.md §4.2 and would put a 1 beside every venue
	// that was merely slow to come up.
	EverConnected bool
}

// resolveSourceState combines what the connector reports about its socket with
// what silence implies about the feed. Both are needed, and neither is
// sufficient.
//
// The connector is the only thing that knows the socket is down while the last
// message is still recent - a venue can go unreachable a second after its last
// tick. Silence is the only thing that catches the opposite: a subscription the
// venue quietly dropped leaves a connection that is genuinely open, healthy by
// every measure the connector has, and delivering nothing ever again. So a
// reported connection is downgraded when nothing arrives through it, and that
// downgrade is the more important half - it is the failure that hides.
func resolveSourceState(conn sourceConn, inferred string) string {
	switch conn.State {
	case exchanges.ConnConnected:
		if inferred == stateDisconnected {
			return stateDisconnected
		}
		return stateConnected
	case exchanges.ConnReconnecting:
		return stateReconnecting
	case exchanges.ConnDisconnected:
		return stateDisconnected
	default:
		// No connector has reported yet - at start-up, or for a source whose
		// connector was never started. Until step 1.5 this inference was all
		// there was.
		return inferred
	}
}

// sourceState is connection health inferred from silence across every symbol.
//
// A venue can be connected while one thin pair goes quiet, which is why this is
// separate from priceStatus. startedAt is when the scanner came up, used to
// judge a source that has never sent anything: "nothing yet" is unknown for the
// first few seconds and disconnected after that.
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
	// Model is none | taker_round_trip. taker_round_trip charges a taker fill on
	// all FOUR legs - open both venues, close both venues - because the spread
	// is only realised by unwinding. See internal/fees.RoundTripTakerPct.
	Model    string   `json:"model"`
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
			Model:   costModelTakerRoundTrip,
			Applied: []string{"taker_fee_entry", "taker_fee_exit"},
			// Slippage needs order book depth, which does not arrive before
			// phase 2, and funding is what phase 2 exists to collect. Naming
			// them here is what stops the number being read as profit.
			// maker_rebate is deliberately NOT listed: the dashboard renders
			// this list as "not yet deducted", and a rebate is income, so
			// naming it there would point the reader the wrong way.
			Excluded: []string{"slippage", "funding", "withdrawal"},
			// Says what the model IS. It must not re-list `excluded`, which the
			// dashboard already renders right after it.
			NoteVI: "Số đã trừ phí giao dịch: taker cả bốn lượt khớp — mở và đóng cả hai chân. " +
				"Đây KHÔNG phải lợi nhuận ròng. Sàn chưa xác minh được biểu phí thì không có " +
				"số sau phí, không phải miễn phí.",
		},
		Sources: sourceRegistry,
	}
}

// newWirePrices wraps the current snapshot.
//
// A stale price is kept and labelled, not dropped: removing the row would make a
// dead venue disappear from the dashboard, which reads as "nothing to report"
// rather than "this feed died".
func newWirePrices(prices map[string]map[string]PricePoint, lastMsgAt map[string]time.Time, conns map[string]sourceConn, startedAt, now time.Time) wirePrices {
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
				out.SourceStatus[source] = newWireSourceStatus(source, lastMsgAt[source], conns[source], startedAt, now)
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
			out.SourceStatus[meta.Source] = newWireSourceStatus(meta.Source, lastMsgAt[meta.Source], conns[meta.Source], startedAt, now)
		}
	}

	return out
}

func newWireSourceStatus(source string, lastMsgAt time.Time, conn sourceConn, startedAt, now time.Time) wireSourceStatus {
	lastMsgAtMs := int64(0)
	if !lastMsgAt.IsZero() {
		lastMsgAtMs = lastMsgAt.UnixMilli()
	}

	state := resolveSourceState(conn, sourceState(lastMsgAt, disconnectAfter(source), startedAt, now))

	// Uptime is the CURRENT unbroken connection, so it is only meaningful while
	// the source is actually connected. Reporting the socket's age beside a
	// state of disconnected - which happens when the socket is open but the feed
	// has gone silent - would read as a contradiction.
	uptimeSec := int64(0)
	if state == stateConnected && !conn.ConnectedSince.IsZero() {
		uptimeSec = int64(now.Sub(conn.ConnectedSince).Seconds())
	}

	return wireSourceStatus{
		State:          state,
		LastMsgAtMs:    lastMsgAtMs,
		ReconnectCount: conn.ReconnectCount,
		UptimeSec:      uptimeSec,
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
	grossPct := spreadGrossPct(buyPrice, sellPrice)
	return wireOpportunity{
		ID:                 fmt.Sprintf("%s|%s|%s|%s|%d", symbol, groupID, buySource, sellSource, detectedAtMs),
		Symbol:             symbol,
		Kind:               "cross_venue",
		GroupID:            groupID,
		BuySource:          buySource,
		SellSource:         sellSource,
		BuyPrice:           buyPrice,
		SellPrice:          sellPrice,
		SpreadGrossPct:     grossPct,
		SpreadAfterFeesPct: afterFeesPct(grossPct, buySource, sellSource),
		DetectedAtMs:       detectedAtMs,
	}
}
