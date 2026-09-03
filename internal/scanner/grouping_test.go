package scanner

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
)

// allSourcesAt returns every registered source priced identically, as a starting
// point a test can then perturb.
func allSourcesAt(price float64) map[string]float64 {
	prices := make(map[string]float64, len(sourceRegistry))
	for _, meta := range sourceRegistry {
		prices[meta.Source] = price
	}
	return prices
}

func groupIDs(msg wireSpreads) []string {
	ids := make([]string, 0, len(msg.CrossVenueGroups))
	for _, g := range msg.CrossVenueGroups {
		ids = append(ids, g.GroupID)
	}
	return ids
}

func excludedReason(msg wireSpreads, source string) string {
	for _, e := range msg.ExcludedSources {
		if e.Source == source {
			return e.Reason
		}
	}
	return ""
}

// The whole point of step 1.2: one comparison per (market type, quote asset),
// never one comparison across all of them.
func TestPartitionSources_SplitsByMarketTypeAndQuote(t *testing.T) {
	groups, _ := partitionSources(allSourcesAt(100))

	want := map[string][]string{
		"perp_usdt": {"binance_futures", "bybit_futures", "okx_futures", "gate_futures"},
		"perp_usd":  {"hyperliquid_futures", "kraken_futures", "paradex_futures"},
		"spot_usdt": {"binance_spot", "bybit_spot"},
	}

	if len(groups) != len(want) {
		t.Fatalf("got %d groups, want %d", len(groups), len(want))
	}
	for _, g := range groups {
		expected, known := want[g.GroupID]
		if !known {
			t.Fatalf("unexpected group %q", g.GroupID)
		}
		if len(g.Sources) != len(expected) {
			t.Fatalf("group %s = %v, want %v", g.GroupID, g.Sources, expected)
		}
		for i := range expected {
			if g.Sources[i] != expected[i] {
				t.Errorf("group %s = %v, want %v", g.GroupID, g.Sources, expected)
				break
			}
		}
	}
}

// A source is only ever compared with a source it shares a market type and a
// quote asset with. This is the property the step exists to guarantee, checked
// over the whole message rather than over one example.
func TestNewWireSpreads_NoCellEverCrossesMarketTypeOrQuote(t *testing.T) {
	msg := newWireSpreads("BTCUSDT", allSourcesAt(100), nil, 1)

	for _, g := range msg.CrossVenueGroups {
		for buySource, row := range g.Matrix {
			buy, ok := sourceMetaFor(buySource)
			if !ok {
				t.Fatalf("group %s holds unregistered source %q", g.GroupID, buySource)
			}
			for sellSource := range row {
				sell, _ := sourceMetaFor(sellSource)
				if buy.MarketType != sell.MarketType {
					t.Errorf("group %s compares %s (%s) with %s (%s)",
						g.GroupID, buySource, buy.MarketType, sellSource, sell.MarketType)
				}
				if buy.QuoteAsset != sell.QuoteAsset {
					t.Errorf("group %s compares %s (quote %s) with %s (quote %s)",
						g.GroupID, buySource, buy.QuoteAsset, sellSource, sell.QuoteAsset)
				}
			}
			if buy.MarketType != g.MarketType || buy.QuoteAsset != g.QuoteAsset {
				t.Errorf("source %s (%s/%s) is in group %s (%s/%s)",
					buySource, buy.MarketType, buy.QuoteAsset, g.GroupID, g.MarketType, g.QuoteAsset)
			}
		}
	}
}

// Pyth is an oracle. Nothing can be bought or sold on it, so it must not appear
// in any comparison - the bug this step closes.
func TestPartitionSources_OracleIsNeverInAGroup(t *testing.T) {
	msg := newWireSpreads("BTCUSDT", allSourcesAt(100), nil, 1)

	for _, g := range msg.CrossVenueGroups {
		for _, source := range g.Sources {
			if source == "pyth" {
				t.Fatalf("pyth appears in group %s", g.GroupID)
			}
		}
		if _, present := g.Matrix["pyth"]; present {
			t.Fatalf("pyth has a matrix row in group %s", g.GroupID)
		}
	}

	if got := excludedReason(msg, "pyth"); got != "oracle" {
		t.Errorf("pyth exclusion reason = %q, want oracle", got)
	}
}

// Kraken quotes in USD. Comparing PF_XBTUSD against a USDT perp measures the
// USD/USDT spread as much as any venue difference, so the two must not share a
// group.
func TestPartitionSources_KrakenIsNotComparedWithUSDTVenues(t *testing.T) {
	groups, _ := partitionSources(allSourcesAt(100))

	for _, g := range groups {
		var hasKraken, hasUSDT bool
		for _, source := range g.Sources {
			if source == "kraken_futures" {
				hasKraken = true
			}
			meta, _ := sourceMetaFor(source)
			if meta.QuoteAsset == "USDT" {
				hasUSDT = true
			}
		}
		if hasKraken && hasUSDT {
			t.Errorf("group %s mixes kraken (USD) with a USDT venue: %v", g.GroupID, g.Sources)
		}
	}
}

// A group whose quote asset is not USDT has to say so, or the operator reads a
// USD/USDT spread as a venue spread.
func TestPartitionSources_NonUSDTGroupCarriesANote(t *testing.T) {
	groups, _ := partitionSources(allSourcesAt(100))

	for _, g := range groups {
		if g.QuoteAsset != "USDT" && g.NoteVI == "" {
			t.Errorf("group %s (quote %s) must carry a note", g.GroupID, g.QuoteAsset)
		}
	}
}

// A comparison needs two sides. One source alone in its group is not a
// degenerate matrix, it is a source with nothing to compare against - and the
// dashboard has to be told why it vanished.
func TestPartitionSources_LoneSourceIsExcludedAsNoPeer(t *testing.T) {
	msg := newWireSpreads("BTCUSDT", map[string]float64{
		"binance_futures": 100,
		"bybit_futures":   101,
		"binance_spot":    99,
	}, nil, 1)

	if ids := groupIDs(msg); len(ids) != 1 || ids[0] != "perp_usdt" {
		t.Fatalf("groups = %v, want only perp_usdt", ids)
	}
	if got := excludedReason(msg, "binance_spot"); got != "no_peer" {
		t.Errorf("binance_spot exclusion reason = %q, want no_peer", got)
	}
}

func TestPartitionSources_GroupOrderIsDeterministic(t *testing.T) {
	first, _ := partitionSources(allSourcesAt(100))
	for i := 0; i < 20; i++ {
		got, _ := partitionSources(allSourcesAt(100))
		if len(got) != len(first) {
			t.Fatalf("group count changed on run %d", i)
		}
		for j := range first {
			if got[j].GroupID != first[j].GroupID {
				t.Fatalf("group order changed on run %d: %s vs %s", i, got[j].GroupID, first[j].GroupID)
			}
		}
	}
	// Registry order decides: binance_futures is first, so its group leads.
	if first[0].GroupID != "perp_usdt" {
		t.Errorf("first group = %s, want perp_usdt", first[0].GroupID)
	}
}

// Every group emitted at step 1.2 compares like with like, so unlike the single
// mixed group of step 1.0 every one of them is tradable.
func TestPartitionSources_EveryGroupIsTradable(t *testing.T) {
	groups, _ := partitionSources(allSourcesAt(100))
	if len(groups) == 0 {
		t.Fatal("no groups")
	}
	for _, g := range groups {
		if !g.Tradable {
			t.Errorf("group %s is not tradable", g.GroupID)
		}
		if g.GroupID == "all" {
			t.Error("the mixed 'all' group of step 1.0 must be gone")
		}
	}
}

// Basis is spot against perp on the SAME venue - the foundation of the funding
// strategy, and a different thing from a cross-venue spread.
func TestNewWireSpreads_BasisIsSameVenueSpotVsPerp(t *testing.T) {
	msg := newWireSpreads("BTCUSDT", map[string]float64{
		"binance_futures": 65100,
		"binance_spot":    65000,
		"bybit_futures":   64900,
		"bybit_spot":      65000,
		"okx_futures":     65050,
	}, nil, 1)

	if len(msg.Basis) != 2 {
		t.Fatalf("got %d basis rows, want 2 (binance, bybit)", len(msg.Basis))
	}

	byVenue := map[string]wireBasis{}
	for _, b := range msg.Basis {
		byVenue[b.Venue] = b
		if b.SpotSource == b.PerpSource {
			t.Errorf("basis row compares %s with itself", b.SpotSource)
		}
		spot, _ := sourceMetaFor(b.SpotSource)
		perp, _ := sourceMetaFor(b.PerpSource)
		if spot.Venue != perp.Venue {
			t.Errorf("basis row crosses venues: %s vs %s", b.SpotSource, b.PerpSource)
		}
		if spot.MarketType != "spot" || perp.MarketType != "perp" {
			t.Errorf("basis row is not spot vs perp: %s/%s", spot.MarketType, perp.MarketType)
		}
	}

	binance := byVenue["binance"]
	if math.Abs(binance.BasisAbsQuote-100) > 1e-9 {
		t.Errorf("binance basis_abs_quote = %g, want 100", binance.BasisAbsQuote)
	}
	// (65100 - 65000) / 65000 * 100
	if math.Abs(binance.BasisPct-0.15384615384615385) > 1e-9 {
		t.Errorf("binance basis_pct = %g, want 0.1538", binance.BasisPct)
	}
	// Perp below spot is a negative basis, not an absolute distance.
	if byVenue["bybit"].BasisAbsQuote >= 0 {
		t.Errorf("bybit basis_abs_quote = %g, want negative", byVenue["bybit"].BasisAbsQuote)
	}
}

func TestNewWireSpreads_NoBasisWithoutBothLegs(t *testing.T) {
	msg := newWireSpreads("BTCUSDT", map[string]float64{
		"binance_futures": 65100,
		"bybit_futures":   65000,
	}, nil, 1)

	if len(msg.Basis) != 0 {
		t.Errorf("basis = %+v, want empty when no venue has both legs", msg.Basis)
	}
}

// Oracle deviation is reference only: it says how far a venue has drifted from
// the oracle, and never produces an alert.
func TestNewWireSpreads_OracleDeviationIsReferenceOnly(t *testing.T) {
	msg := newWireSpreads("BTCUSDT", map[string]float64{
		"pyth":            100,
		"binance_futures": 101,
		"bybit_futures":   99,
	}, nil, 1)

	if len(msg.OracleDeviation) != 2 {
		t.Fatalf("got %d deviation rows, want 2", len(msg.OracleDeviation))
	}

	bySource := map[string]wireOracleDeviation{}
	for _, d := range msg.OracleDeviation {
		if d.OracleSource != "pyth" {
			t.Errorf("oracle_source = %q, want pyth", d.OracleSource)
		}
		if d.Source == "pyth" {
			t.Error("oracle compared with itself")
		}
		bySource[d.Source] = d
	}

	if math.Abs(bySource["binance_futures"].DeviationPct-1.0) > 1e-9 {
		t.Errorf("binance deviation = %g, want +1.0", bySource["binance_futures"].DeviationPct)
	}
	if math.Abs(bySource["bybit_futures"].DeviationPct-(-1.0)) > 1e-9 {
		t.Errorf("bybit deviation = %g, want -1.0", bySource["bybit_futures"].DeviationPct)
	}
}

// A frozen oracle price must not produce deviations: the number would describe
// a market that has moved on since.
func TestNewWireSpreads_NoOracleDeviationWhenTheOracleIsAbsent(t *testing.T) {
	msg := newWireSpreads("BTCUSDT", map[string]float64{
		"binance_futures": 101,
		"bybit_futures":   99,
	}, nil, 1)

	if len(msg.OracleDeviation) != 0 {
		t.Errorf("oracle_deviation = %+v, want empty without a usable oracle", msg.OracleDeviation)
	}
}

// THE ACCEPTANCE CRITERION of step 1.2. The oracle is far from every venue, so
// under the old all-sources min/max it would win both ends of the comparison and
// produce an alert nobody can execute.
func TestCheckArbitrage_NeverNamesTheOracleInAnAlert(t *testing.T) {
	s := New([]string{"BTCUSDT"})
	base := time.Now()
	s.now = func() time.Time { return base }

	captured := make(chan wireOpportunity, 16)
	s.onOpportunity = func(o wireOpportunity) { captured <- o }

	// Two perps a hair apart, an oracle 5% away. Mixing them yields a 5%
	// "opportunity"; keeping them apart yields nothing at all.
	s.updatePrice(mustPriceData("BTCUSDT", "binance_futures", 65000))
	s.updatePrice(mustPriceData("BTCUSDT", "bybit_futures", 65001))
	s.updatePrice(mustPriceData("BTCUSDT", "pyth", 68250))

	close(captured)
	for opp := range captured {
		if opp.BuySource == "pyth" || opp.SellSource == "pyth" {
			t.Fatalf("alert names the oracle: %+v", opp)
		}
	}
}

// An alert belongs to the group that produced it, and two groups must be able to
// raise one independently.
func TestCheckArbitrage_AlertsPerTradableGroup(t *testing.T) {
	s := New([]string{"BTCUSDT"})
	base := time.Now()
	s.now = func() time.Time { return base }

	captured := make(chan wireOpportunity, 16)
	s.onOpportunity = func(o wireOpportunity) { captured <- o }

	s.updatePrice(mustPriceData("BTCUSDT", "binance_spot", 65000))
	s.updatePrice(mustPriceData("BTCUSDT", "bybit_spot", 65200))
	s.updatePrice(mustPriceData("BTCUSDT", "binance_futures", 65000))
	s.updatePrice(mustPriceData("BTCUSDT", "bybit_futures", 65300))

	close(captured)
	seen := map[string]wireOpportunity{}
	for opp := range captured {
		seen[opp.GroupID] = opp
	}

	spot, gotSpot := seen["spot_usdt"]
	perp, gotPerp := seen["perp_usdt"]
	if !gotSpot || !gotPerp {
		t.Fatalf("groups that alerted = %v, want both spot_usdt and perp_usdt", seen)
	}
	for _, opp := range []wireOpportunity{spot, perp} {
		buy, _ := sourceMetaFor(opp.BuySource)
		sell, _ := sourceMetaFor(opp.SellSource)
		if buy.MarketType != sell.MarketType {
			t.Errorf("alert mixes market types: %+v", opp)
		}
		if opp.Kind != "cross_venue" {
			t.Errorf("kind = %q, want cross_venue", opp.Kind)
		}
	}
	if spot.GroupID == perp.GroupID {
		t.Error("two groups produced the same group_id")
	}
}

// The cooldown is claimed per group, so a quiet perp group cannot suppress a
// spot alert that has never fired.
func TestCheckArbitrage_CooldownIsPerGroup(t *testing.T) {
	s := New([]string{"BTCUSDT"})
	base := time.Now()
	s.now = func() time.Time { return base }

	captured := make(chan wireOpportunity, 16)
	s.onOpportunity = func(o wireOpportunity) { captured <- o }

	s.updatePrice(mustPriceData("BTCUSDT", "binance_futures", 65000))
	s.updatePrice(mustPriceData("BTCUSDT", "bybit_futures", 65300))
	drain(captured)

	// Same instant, so the perp group is still inside its cooldown. The spot
	// group has never alerted and must not be blocked by it.
	s.updatePrice(mustPriceData("BTCUSDT", "binance_spot", 65000))
	s.updatePrice(mustPriceData("BTCUSDT", "bybit_spot", 65200))

	close(captured)
	var spotAlerts int
	for opp := range captured {
		if opp.GroupID == "spot_usdt" {
			spotAlerts++
		}
		if opp.GroupID == "perp_usdt" {
			t.Errorf("perp group alerted again inside its cooldown: %+v", opp)
		}
	}
	if spotAlerts == 0 {
		t.Error("spot group never alerted")
	}
}

func drain(ch chan wireOpportunity) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// The opportunity id names the group that produced it, so two groups alerting on
// the same pair at the same instant stay distinguishable.
func TestNewWireOpportunity_IDNamesTheGroup(t *testing.T) {
	opp := newWireOpportunity("BTCUSDT", "perp_usdt", "binance_futures", "bybit_futures", 100, 101, 1756368000000)

	if opp.GroupID != "perp_usdt" {
		t.Errorf("group_id = %q, want perp_usdt", opp.GroupID)
	}
	want := "BTCUSDT|perp_usdt|binance_futures|bybit_futures|1756368000000"
	if opp.ID != want {
		t.Errorf("id = %q, want %q", opp.ID, want)
	}
}

// Top of book was parsed by the connectors and thrown away. It is the first-order
// liquidity filter, and it has to survive the trip to the wire.
func TestNewWirePrices_CarriesTopOfBookFromThePricePoint(t *testing.T) {
	now := time.Now()
	prices := map[string]map[string]PricePoint{
		"BTCUSDT": {
			"binance_futures": {
				Price: 65000, RecvAt: now,
				BestBid: 64999.5, BestAsk: 65000.5,
				BestBidQtyCoin: 7.943, BestAskQtyCoin: 1.397,
			},
		},
	}

	msg := newWirePrices(prices, map[string]time.Time{"binance_futures": now}, now, now)
	point := msg.Prices["BTCUSDT"]["binance_futures"]

	if point.BestBid != 64999.5 || point.BestAsk != 65000.5 {
		t.Errorf("best bid/ask = %g/%g, want 64999.5/65000.5", point.BestBid, point.BestAsk)
	}
	if point.BestBidQtyCoin != 7.943 || point.BestAskQtyCoin != 1.397 {
		t.Errorf("qty = %g/%g, want 7.943/1.397", point.BestBidQtyCoin, point.BestAskQtyCoin)
	}

	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"best_bid_qty_coin", "best_ask_qty_coin"} {
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		sym := decoded["prices"].(map[string]any)["BTCUSDT"].(map[string]any)
		if _, ok := sym["binance_futures"].(map[string]any)[key]; !ok {
			t.Errorf("wire is missing %q", key)
		}
	}
}

// The book quantities travel with the book. An oracle publishes a price and no
// book at all, and must not end up reporting a quantity of anything.
func TestUpdatePrice_OracleReportsNoQuantity(t *testing.T) {
	s := New([]string{"BTCUSDT"})
	now := time.Now()
	s.now = func() time.Time { return now }

	s.updatePrice(exchanges.PriceData{Symbol: "BTCUSDT", Source: "pyth", Price: 65000})

	s.pricesMutex.RLock()
	point := s.prices["BTCUSDT"]["pyth"]
	s.pricesMutex.RUnlock()

	if point.BestBid != 0 || point.BestAsk != 0 || point.BestBidQtyCoin != 0 || point.BestAskQtyCoin != 0 {
		t.Errorf("oracle point carries a book: %+v", point)
	}
}

// A book update has to reach the stored point through the same single receive
// stamping site, quantities included.
func TestProcessOrderbooks_StoresTopOfBook(t *testing.T) {
	s := New([]string{"BTCUSDT"})
	now := time.Now()
	s.now = func() time.Time { return now }

	// The goroutine must not outlive the test: it reads the package-level source
	// registry through evaluate, and a later test swaps that registry out.
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		s.processOrderbooks()
	}()
	defer func() {
		close(s.orderbookChan)
		<-stopped
	}()

	s.orderbookChan <- exchanges.OrderbookData{
		Symbol: "BTCUSDT", Source: "binance_futures",
		BestBid: 64999, BestAsk: 65001,
		BestBidQtyCoin: 3.5, BestAskQtyCoin: 0.25,
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.pricesMutex.RLock()
		point, ok := s.prices["BTCUSDT"]["binance_futures"]
		s.pricesMutex.RUnlock()
		if ok {
			if point.Price != 65000 {
				t.Errorf("price = %g, want the 65000 mid", point.Price)
			}
			if point.BestBidQtyCoin != 3.5 || point.BestAskQtyCoin != 0.25 {
				t.Errorf("qty = %g/%g, want 3.5/0.25", point.BestBidQtyCoin, point.BestAskQtyCoin)
			}
			if point.RecvAt.IsZero() {
				t.Error("book update was stored without a receive time")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("book update never reached the price map")
}

// withRegistry swaps the source registry for one test. sourceOrder is derived
// from it at init, so both have to move together or sourceMetaFor stops finding
// anything.
func withRegistry(t *testing.T, registry []sourceMeta) {
	t.Helper()
	oldRegistry, oldOrder := sourceRegistry, sourceOrder

	sourceRegistry = registry
	order := make(map[string]int, len(registry))
	for i, meta := range registry {
		order[meta.Source] = i
	}
	sourceOrder = order

	t.Cleanup(func() { sourceRegistry, sourceOrder = oldRegistry, oldOrder })
}

// Tradable is a registry fact and has to be consulted, not inferred from the
// market type alone. A venue we can read but not trade on must never be named as
// one side of an executable spread.
func TestPartitionSources_UntradableMemberMakesTheGroupReferenceOnly(t *testing.T) {
	withRegistry(t, []sourceMeta{
		{Source: "tradable_perp", Venue: "a", MarketType: marketTypePerp, QuoteAsset: "USDT", Tradable: true},
		{Source: "readonly_perp", Venue: "b", MarketType: marketTypePerp, QuoteAsset: "USDT", Tradable: false},
	})

	groups, _ := partitionSources(map[string]float64{"tradable_perp": 100, "readonly_perp": 101})
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if groups[0].Tradable {
		t.Error("a group holding an untradable source must be reference only")
	}
}

// And the alert path must honour that: a reference-only group raises nothing.
func TestCheckArbitrage_RaisesNoAlertFromAReferenceOnlyGroup(t *testing.T) {
	withRegistry(t, []sourceMeta{
		{Source: "tradable_perp", Venue: "a", MarketType: marketTypePerp, QuoteAsset: "USDT", Tradable: true},
		{Source: "readonly_perp", Venue: "b", MarketType: marketTypePerp, QuoteAsset: "USDT", Tradable: false},
	})

	s := New([]string{"BTCUSDT"})
	base := time.Now()
	s.now = func() time.Time { return base }

	captured := make(chan wireOpportunity, 8)
	s.onOpportunity = func(o wireOpportunity) { captured <- o }

	s.updatePrice(mustPriceData("BTCUSDT", "tradable_perp", 65000))
	s.updatePrice(mustPriceData("BTCUSDT", "readonly_perp", 66000))

	close(captured)
	for opp := range captured {
		t.Errorf("reference-only group raised an alert: %+v", opp)
	}
}

// Pyth quotes in USD while most venues quote in USDT, so most deviation rows
// carry the USD/USDT spread too. The row has to say so rather than pass as a
// clean measurement of the venue's drift.
func TestNewWireSpreads_OracleDeviationFlagsAQuoteMismatch(t *testing.T) {
	msg := newWireSpreads("BTCUSDT", map[string]float64{
		"pyth":            100,
		"binance_futures": 101, // USDT against a USD oracle
		"kraken_futures":  102, // USD against a USD oracle
	}, nil, 1)

	flagged := map[string]bool{}
	for _, d := range msg.OracleDeviation {
		flagged[d.Source] = d.QuoteAssetMismatch
	}
	if len(flagged) != 2 {
		t.Fatalf("oracle_deviation = %+v, want a row per venue", msg.OracleDeviation)
	}
	if !flagged["binance_futures"] {
		t.Error("a USDT venue against a USD oracle must be flagged as a quote mismatch")
	}
	if flagged["kraken_futures"] {
		t.Error("a USD venue against a USD oracle is not a quote mismatch")
	}
}

// A source no registry entry describes cannot be grouped, and saying
// "quote_mismatch" would state something we do not know.
func TestPartitionSources_UnregisteredSourceIsNamedAsSuch(t *testing.T) {
	msg := newWireSpreads("BTCUSDT", map[string]float64{
		"binance_futures": 100,
		"bybit_futures":   101,
		"some_new_venue":  102,
	}, nil, 1)

	for _, g := range msg.CrossVenueGroups {
		for _, source := range g.Sources {
			if source == "some_new_venue" {
				t.Errorf("an unregistered source was grouped into %s", g.GroupID)
			}
		}
	}
	if got := excludedReason(msg, "some_new_venue"); got != reasonUnregistered {
		t.Errorf("reason = %q, want %q", got, reasonUnregistered)
	}
}
