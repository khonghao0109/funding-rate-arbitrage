package scanner

import (
	"encoding/json"
	"math"
	"sort"
	"testing"
	"time"
)

// expectedSources is the set of sources main() currently connects. The registry
// that feeds the meta message must match it exactly, otherwise the dashboard
// either hides a live source or advertises one that never sends data.
var expectedSources = []string{
	"binance_futures", "bybit_futures", "hyperliquid_futures", "kraken_futures",
	"okx_futures", "gate_futures", "paradex_futures",
	"binance_spot", "bybit_spot", "pyth",
}

func TestSourceRegistry_CoversEverySourceMainConnects(t *testing.T) {
	got := make([]string, 0, len(sourceRegistry))
	for _, s := range sourceRegistry {
		got = append(got, s.Source)
	}
	sort.Strings(got)

	want := append([]string(nil), expectedSources...)
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("registry has %d sources, main connects %d: %v vs %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("registry source %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSourceRegistry_EveryEntryIsRenderable(t *testing.T) {
	shortLabels := make(map[string]string)
	for _, s := range sourceRegistry {
		if s.Label == "" || s.ShortLabel == "" || s.Color == "" {
			t.Errorf("%s: label/short_label/color must all be set, got %q/%q/%q",
				s.Source, s.Label, s.ShortLabel, s.Color)
		}
		if s.Venue == "" {
			t.Errorf("%s: venue must be set", s.Source)
		}
		// The matrix column headers are the short labels; duplicates make two
		// different venues indistinguishable in the UI.
		if prev, dup := shortLabels[s.ShortLabel]; dup {
			t.Errorf("short label %q used by both %s and %s", s.ShortLabel, prev, s.Source)
		}
		shortLabels[s.ShortLabel] = s.Source
	}
}

func TestSourceRegistry_StaticFactsPerVenue(t *testing.T) {
	byID := make(map[string]sourceMeta)
	for _, s := range sourceRegistry {
		byID[s.Source] = s
	}

	// Kraken quotes in USD, not USDT. Comparing PF_XBTUSD against BTCUSDT mixes
	// in the USD/USDT spread. See docs/DATA-REQUIREMENTS.md §3.
	if got := byID["kraken_futures"].QuoteAsset; got != "USD" {
		t.Errorf("kraken_futures quote_asset = %q, want USD", got)
	}
	if got := byID["binance_futures"].QuoteAsset; got != "USDT" {
		t.Errorf("binance_futures quote_asset = %q, want USDT", got)
	}

	// Pyth is an oracle. It must never be advertised as somewhere to trade.
	if byID["pyth"].Tradable {
		t.Error("pyth must not be marked tradable — it is an oracle, not a venue")
	}
	if got := byID["pyth"].MarketType; got != "oracle" {
		t.Errorf("pyth market_type = %q, want oracle", got)
	}
	if got := byID["binance_spot"].MarketType; got != "spot" {
		t.Errorf("binance_spot market_type = %q, want spot", got)
	}
	if got := byID["binance_futures"].MarketType; got != "perp" {
		t.Errorf("binance_futures market_type = %q, want perp", got)
	}
}

func TestNewWireMeta_Defaults(t *testing.T) {
	symbols := []string{"BTCUSDT", "ETHUSDT"}
	meta := newWireMeta(symbols, 1756368000000)

	if meta.Type != "meta" || meta.V != wireVersion {
		t.Errorf("envelope = %q v%d, want meta v%d", meta.Type, meta.V, wireVersion)
	}
	if meta.ServerTimeMs != 1756368000000 {
		t.Errorf("server_time_ms = %d", meta.ServerTimeMs)
	}
	if meta.DefaultSymbol != "BTCUSDT" {
		t.Errorf("default_symbol = %q, want BTCUSDT", meta.DefaultSymbol)
	}

	// No fee model exists yet: every cost must be listed as NOT deducted.
	if meta.CostBasis.Model != "none" {
		t.Errorf("cost_basis.model = %q, want none", meta.CostBasis.Model)
	}
	if len(meta.CostBasis.Applied) != 0 {
		t.Errorf("cost_basis.applied = %v, want empty", meta.CostBasis.Applied)
	}
	for _, want := range []string{"taker_fee", "maker_fee", "slippage", "funding"} {
		found := false
		for _, e := range meta.CostBasis.Excluded {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("cost_basis.excluded missing %q, got %v", want, meta.CostBasis.Excluded)
		}
	}

	for _, s := range meta.Sources {
		// Thresholds are per venue and measured, so they are not all equal. What
		// must hold is that none is below the default: a shorter one would mark
		// a healthy but slower feed dead.
		if s.StaleAfterSec < defaultStaleAfterSec {
			t.Errorf("%s: stale_after_sec = %d, below the %d default",
				s.Source, s.StaleAfterSec, defaultStaleAfterSec)
		}
		if s.MakerFeeBps != 0 || s.TakerFeeBps != 0 {
			t.Errorf("%s: fees must stay 0 until step 1.3, got maker=%d taker=%d",
				s.Source, s.MakerFeeBps, s.TakerFeeBps)
		}
	}
}

func TestNewWireMeta_SerializesEveryContractKey(t *testing.T) {
	raw, err := json.Marshal(newWireMeta([]string{"BTCUSDT"}, 1))
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	for _, key := range []string{
		"type", "v", "server_time_ms", "symbols", "default_symbol",
		"alert_min_spread_pct", "cost_basis", "sources",
	} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("meta message missing contract key %q", key)
		}
	}

	sources, _ := decoded["sources"].([]any)
	if len(sources) == 0 {
		t.Fatal("meta.sources is empty")
	}
	first, _ := sources[0].(map[string]any)
	for _, key := range []string{
		"source", "venue", "market_type", "quote_asset", "tradable", "label",
		"short_label", "color", "line_style", "enabled_by_default",
		"stale_after_sec", "maker_fee_bps", "taker_fee_bps",
	} {
		if _, ok := first[key]; !ok {
			t.Errorf("meta.sources[0] missing contract key %q", key)
		}
	}
}

func TestSpreadGrossPct(t *testing.T) {
	tests := []struct {
		name      string
		buyPrice  float64
		sellPrice float64
		want      float64
	}{
		{"sell above buy is positive", 100, 100.5, 0.5},
		{"sell below buy is negative", 100, 99.5, -0.5},
		{"identical prices are zero", 100, 100, 0},
		{"small spread keeps precision", 65000, 65013, 0.02},
	}
	for _, tt := range tests {
		got := spreadGrossPct(tt.buyPrice, tt.sellPrice)
		if math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("%s: spreadGrossPct(%g, %g) = %g, want %g",
				tt.name, tt.buyPrice, tt.sellPrice, got, tt.want)
		}
	}
}

func TestSpreadGrossPct_ZeroBuyPriceDoesNotProduceInf(t *testing.T) {
	// A venue that reports 0 must not poison the matrix with +Inf, which
	// serializes to invalid JSON and takes the whole message down.
	got := spreadGrossPct(0, 100)
	if math.IsInf(got, 0) || math.IsNaN(got) {
		t.Fatalf("spreadGrossPct(0, 100) = %v, want a finite value", got)
	}
	if got != 0 {
		t.Errorf("spreadGrossPct(0, 100) = %v, want 0", got)
	}
}

func TestNewWireSpreads_SingleUntradableGroupUntilStep12(t *testing.T) {
	prices := map[string]float64{
		"binance_futures": 100,
		"bybit_futures":   101,
		"pyth":            100.5,
	}
	msg := newWireSpreads("BTCUSDT", prices, nil, 1756368000000)

	if msg.Type != "spreads" || msg.V != wireVersion {
		t.Errorf("envelope = %q v%d", msg.Type, msg.V)
	}
	if msg.Symbol != "BTCUSDT" {
		t.Errorf("symbol = %q", msg.Symbol)
	}
	if len(msg.CrossVenueGroups) != 1 {
		t.Fatalf("got %d groups, step 1.0 must emit exactly one", len(msg.CrossVenueGroups))
	}

	g := msg.CrossVenueGroups[0]
	if g.GroupID != "all" {
		t.Errorf("group_id = %q, want all", g.GroupID)
	}
	// The single group still mixes spot, perp and an oracle. Advertising it as
	// tradable would be the exact false claim step 1.2 exists to remove.
	if g.Tradable {
		t.Error("group 'all' must be tradable=false while it still mixes spot/perp/oracle")
	}
	if g.NoteVI == "" {
		t.Error("group 'all' must carry a note explaining what it mixes")
	}

	// Steps 1.1-1.3 fill these; they must already exist and be empty, not absent.
	if msg.Basis == nil || len(msg.Basis) != 0 {
		t.Errorf("basis = %v, want empty non-nil slice", msg.Basis)
	}
	if msg.OracleDeviation == nil || len(msg.OracleDeviation) != 0 {
		t.Errorf("oracle_deviation = %v, want empty non-nil slice", msg.OracleDeviation)
	}
	if msg.ExcludedSources == nil || len(msg.ExcludedSources) != 0 {
		t.Errorf("excluded_sources = %v, want empty non-nil slice", msg.ExcludedSources)
	}
}

func TestNewWireSpreads_MatrixMathAndShape(t *testing.T) {
	prices := map[string]float64{
		"binance_futures": 100,
		"bybit_futures":   101,
	}
	g := newWireSpreads("BTCUSDT", prices, nil, 1).CrossVenueGroups[0]

	cell, ok := g.Matrix["binance_futures"]["bybit_futures"]
	if !ok {
		t.Fatal("missing cell binance_futures -> bybit_futures")
	}
	if math.Abs(cell.SpreadGrossPct-1.0) > 1e-9 {
		t.Errorf("spread_gross_pct = %g, want 1.0", cell.SpreadGrossPct)
	}
	// No fee model exists yet: the after-fee number must be null, never a copy
	// of the gross number.
	if cell.SpreadAfterFeesPct != nil {
		t.Errorf("spread_after_fees_pct = %v, want null until step 1.3", *cell.SpreadAfterFeesPct)
	}

	if _, self := g.Matrix["binance_futures"]["binance_futures"]; self {
		t.Error("matrix must not contain a source compared with itself")
	}

	reverse := g.Matrix["bybit_futures"]["binance_futures"]
	if math.Abs(reverse.SpreadGrossPct-(-0.990099009900990)) > 1e-9 {
		t.Errorf("reverse spread_gross_pct = %g, want -0.9900990099", reverse.SpreadGrossPct)
	}
}

func TestNewWireSpreads_SourceOrderIsDeterministic(t *testing.T) {
	prices := map[string]float64{
		"pyth":            1,
		"bybit_futures":   1,
		"binance_futures": 1,
		"okx_futures":     1,
	}
	// Map iteration order is random; the matrix columns must not reshuffle
	// between messages or the dashboard becomes unreadable.
	first := newWireSpreads("BTCUSDT", prices, nil, 1).CrossVenueGroups[0].Sources
	for i := 0; i < 20; i++ {
		got := newWireSpreads("BTCUSDT", prices, nil, 1).CrossVenueGroups[0].Sources
		if len(got) != len(first) {
			t.Fatalf("source count changed: %v vs %v", got, first)
		}
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("source order changed on run %d: %v vs %v", i, got, first)
			}
		}
	}
	// Order follows the registry, so binance_futures precedes bybit_futures.
	if first[0] != "binance_futures" || first[1] != "bybit_futures" {
		t.Errorf("sources = %v, want registry order", first)
	}
}

func TestNewWireSpreads_EmptyAndSingleSource(t *testing.T) {
	empty := newWireSpreads("BTCUSDT", map[string]float64{}, nil, 1)
	if len(empty.CrossVenueGroups) != 1 || len(empty.CrossVenueGroups[0].Sources) != 0 {
		t.Errorf("empty price map must still produce one group with no sources, got %+v", empty.CrossVenueGroups)
	}

	single := newWireSpreads("BTCUSDT", map[string]float64{"pyth": 100}, nil, 1)
	g := single.CrossVenueGroups[0]
	if len(g.Sources) != 1 {
		t.Errorf("sources = %v, want 1", g.Sources)
	}
	if len(g.Matrix["pyth"]) != 0 {
		t.Errorf("single source must have no pairs, got %v", g.Matrix["pyth"])
	}
}

func TestNewWireOpportunity_NamesGrossAsGross(t *testing.T) {
	opp := newWireOpportunity("BTCUSDT", "binance_futures", "bybit_futures", 100, 101, 1756368000000)

	if opp.Kind != "cross_venue" {
		t.Errorf("kind = %q, want cross_venue", opp.Kind)
	}
	if opp.GroupID != "all" {
		t.Errorf("group_id = %q, want all", opp.GroupID)
	}
	if math.Abs(opp.SpreadGrossPct-1.0) > 1e-9 {
		t.Errorf("spread_gross_pct = %g, want 1.0", opp.SpreadGrossPct)
	}
	if opp.SpreadAfterFeesPct != nil {
		t.Errorf("spread_after_fees_pct = %v, want null until step 1.3", *opp.SpreadAfterFeesPct)
	}
	if opp.DetectedAtMs != 1756368000000 {
		t.Errorf("detected_at_ms = %d", opp.DetectedAtMs)
	}
	if opp.ID == "" {
		t.Error("id must be generated by the backend, not the browser")
	}

	// The wire must not carry a field named profit_* for a number that has had
	// nothing deducted. See CLAUDE.md rule 2.
	raw, err := json.Marshal(opp)
	if err != nil {
		t.Fatalf("marshal opportunity: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal opportunity: %v", err)
	}
	for key := range decoded {
		if key == "profit_pct" || key == "profit" {
			t.Errorf("opportunity carries %q; a gross spread must not be called profit", key)
		}
	}
	for _, key := range []string{
		"id", "symbol", "kind", "group_id", "buy_source", "sell_source",
		"buy_price", "sell_price", "spread_gross_pct", "spread_after_fees_pct",
		"detected_at_ms",
	} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("opportunity missing contract key %q", key)
		}
	}
}

func TestNewWireOpportunity_IDIsStableForSamePairAndInstant(t *testing.T) {
	a := newWireOpportunity("BTCUSDT", "binance_futures", "bybit_futures", 100, 101, 42)
	b := newWireOpportunity("BTCUSDT", "binance_futures", "bybit_futures", 100, 101, 42)
	if a.ID != b.ID {
		t.Errorf("id must be derived, not random: %q vs %q", a.ID, b.ID)
	}
	c := newWireOpportunity("ETHUSDT", "binance_futures", "bybit_futures", 100, 101, 42)
	if a.ID == c.ID {
		t.Error("different symbols must not share an id")
	}
}

// A source reporting a non-positive price must be dropped, not folded into a 0%
// spread. Folding it makes it the minimum for the symbol, drives every spread
// against it and silently suppresses every real alert.
func TestCheckArbitrage_NonPositivePriceDoesNotSuppressAlerts(t *testing.T) {
	scanner := New([]string{"BTCUSDT"})

	captured := make(chan wireOpportunity, 4)
	scanner.onOpportunity = func(o wireOpportunity) { captured <- o }

	// The broken source has to arrive first: if the healthy pair completes
	// before it, the alert it raises would satisfy this test regardless.
	scanner.updatePrice(mustPriceData("BTCUSDT", "okx_futures", 0))
	scanner.updatePrice(mustPriceData("BTCUSDT", "binance_futures", 65000))
	scanner.updatePrice(mustPriceData("BTCUSDT", "bybit_futures", 66000))

	select {
	case opp := <-captured:
		if opp.BuySource == "okx_futures" || opp.SellSource == "okx_futures" {
			t.Errorf("opportunity built from a zero-priced source: %+v", opp)
		}
		if opp.BuyPrice <= 0 {
			t.Errorf("buy_price = %g, want a real price", opp.BuyPrice)
		}
	default:
		t.Fatal("no opportunity raised; a zero-priced source silenced the symbol")
	}
}

func TestCheckArbitrage_SkipsSymbolWithFewerThanTwoUsablePrices(t *testing.T) {
	scanner := New([]string{"BTCUSDT"})

	captured := make(chan wireOpportunity, 4)
	scanner.onOpportunity = func(o wireOpportunity) { captured <- o }

	scanner.updatePrice(mustPriceData("BTCUSDT", "binance_futures", 65000))
	scanner.updatePrice(mustPriceData("BTCUSDT", "bybit_futures", 0))

	select {
	case opp := <-captured:
		t.Errorf("raised an opportunity from one usable price: %+v", opp)
	default:
	}
}

func TestNewWirePrices_DropsNonPositivePriceButKeepsTheSource(t *testing.T) {
	now := time.Now()
	msg := newWirePrices(map[string]map[string]PricePoint{
		"BTCUSDT": {
			"binance_futures": {Price: 65000, RecvAt: now},
			"okx_futures":     {Price: 0, RecvAt: now},
			"gate_futures":    {Price: -1, RecvAt: now},
		},
	}, map[string]time.Time{}, time.Time{}, now)

	points := msg.Prices["BTCUSDT"]
	if _, ok := points["okx_futures"]; ok {
		t.Error("a zero price was shipped; it renders as $0.000000 and drags the chart to zero")
	}
	if _, ok := points["gate_futures"]; ok {
		t.Error("a negative price was shipped")
	}
	if _, ok := points["binance_futures"]; !ok {
		t.Error("the healthy source was dropped")
	}

	// The source is still known - it did send a message, it just has no usable
	// price - so its connection state must still be reported.
	for _, source := range []string{"binance_futures", "okx_futures", "gate_futures"} {
		if _, ok := msg.SourceStatus[source]; !ok {
			t.Errorf("source_status missing %q", source)
		}
	}
}

func TestIsUsablePrice(t *testing.T) {
	tests := []struct {
		name  string
		price float64
		want  bool
	}{
		{"a real price", 65000, true},
		{"zero", 0, false},
		{"negative", -1, false},
		// strconv.ParseFloat accepts the literal "NaN" with a nil error, so a
		// malformed venue payload reaches this. `price <= 0` is FALSE for NaN,
		// which is why the check has to be written positively.
		{"NaN", math.NaN(), false},
		{"positive infinity", math.Inf(1), false},
		{"negative infinity", math.Inf(-1), false},
	}
	for _, tt := range tests {
		if got := isUsablePrice(tt.price); got != tt.want {
			t.Errorf("%s: isUsablePrice(%v) = %v, want %v", tt.name, tt.price, got, tt.want)
		}
	}
}

func TestNewWireSpreads_NaNNeverReachesTheEncoder(t *testing.T) {
	msg := newWireSpreads("BTCUSDT", map[string]float64{
		"binance_futures": 65000,
		"bybit_futures":   66000,
		"okx_futures":     math.NaN(),
	}, nil, 1)

	// json.Marshal fails outright on NaN, which would drop the message and, if
	// the failure were treated as a transport error, every client with it.
	if _, err := json.Marshal(msg); err != nil {
		t.Fatalf("spreads message is not encodable: %v", err)
	}
	for _, source := range msg.CrossVenueGroups[0].Sources {
		if source == "okx_futures" {
			t.Error("a NaN-priced source reached the matrix")
		}
	}
}

func TestNewWireSpreads_ReportsWhyASourceIsMissing(t *testing.T) {
	excluded := []wireExcludedSource{
		{Source: "okx_futures", Reason: "no_price", NoteVI: "…"},
		{Source: "binance_futures", Reason: "no_price", NoteVI: "…"},
	}
	msg := newWireSpreads("BTCUSDT", map[string]float64{"bybit_futures": 1}, excluded, 1)

	if len(msg.ExcludedSources) != 2 {
		t.Fatalf("excluded_sources = %v, want the two dropped sources", msg.ExcludedSources)
	}
	// Registry order, so the list does not reshuffle between messages.
	if msg.ExcludedSources[0].Source != "binance_futures" {
		t.Errorf("excluded_sources not in registry order: %v", msg.ExcludedSources)
	}
	if msg.ExcludedSources[0].Reason != "no_price" {
		t.Errorf("reason = %q, want no_price", msg.ExcludedSources[0].Reason)
	}
}

// Step 1.0 must reserve the top-of-book slot so step 1.2 fills a field instead
// of reshaping the contract, and so phase 2 does not force a second app.js
// rewrite. See PLAN.md §7.4.
func TestNewWirePrices_ReservesTopOfBookSlot(t *testing.T) {
	now := time.Now()
	raw, err := json.Marshal(newWirePrices(map[string]map[string]PricePoint{
		"BTCUSDT": {"binance_futures": {Price: 65000, RecvAt: now}},
	}, map[string]time.Time{}, time.Time{}, now))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded struct {
		Prices map[string]map[string]map[string]any `json:"prices"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	point := decoded.Prices["BTCUSDT"]["binance_futures"]
	for _, key := range []string{"best_bid", "best_ask", "best_bid_qty_coin", "best_ask_qty_coin"} {
		value, ok := point[key]
		if !ok {
			t.Errorf("price point missing liquidity key %q", key)
			continue
		}
		if value != float64(0) {
			t.Errorf("%s = %v, want 0 until step 1.2 collects it", key, value)
		}
	}

	// The quantity is in coin. OKX, Gate and Kraken quote contracts, and a
	// contract count silently used as a coin amount is the sizing bug this
	// naming rule exists to prevent.
	for _, key := range []string{"best_bid_qty", "best_ask_qty", "qty", "size"} {
		if _, ok := point[key]; ok {
			t.Errorf("price point carries %q; a quantity must name its unit", key)
		}
	}
}
