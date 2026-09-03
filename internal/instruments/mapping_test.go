package instruments

import (
	"reflect"
	"strings"
	"testing"

	"futures-arbitrage-scanner/exchanges"
)

// The mapping is validated against VENUE-DECLARED assets, never symbol
// spelling. These helpers build instruments the way the fetchers do; the
// asset values in realInstruments mirror the golden expectations in
// exchanges/instruments_parse_test.go (recorded 2026-09-03), so this test
// exercises the mapping over reality-shaped input.

func mapInst(source, symbol, native, marketType, base, quote string) exchanges.Instrument {
	return exchanges.Instrument{
		Symbol: symbol, NativeSymbol: native, Source: source, MarketType: marketType,
		Status: exchanges.StatusTrading, BaseAsset: base, QuoteAsset: quote,
		StepSizeCoin: 0.001, ContractSizeCoin: 1,
	}
}

// realSourceClaims mirrors config.yaml's sources (market_type, quote_asset).
func realSourceClaims() []SourceClaim {
	return []SourceClaim{
		{Source: "binance_futures", MarketType: "perp", QuoteAsset: "USDT", Tradable: true},
		{Source: "bybit_futures", MarketType: "perp", QuoteAsset: "USDT", Tradable: true},
		{Source: "hyperliquid_futures", MarketType: "perp", QuoteAsset: "USD", Tradable: true},
		{Source: "kraken_futures", MarketType: "perp", QuoteAsset: "USD", Tradable: true},
		{Source: "okx_futures", MarketType: "perp", QuoteAsset: "USDT", Tradable: true},
		{Source: "gate_futures", MarketType: "perp", QuoteAsset: "USDT", Tradable: true},
		{Source: "paradex_futures", MarketType: "perp", QuoteAsset: "USD", Tradable: true},
		{Source: "binance_spot", MarketType: "spot", QuoteAsset: "USDT", Tradable: true},
		{Source: "bybit_spot", MarketType: "spot", QuoteAsset: "USDT", Tradable: true},
		{Source: "pyth", MarketType: "oracle", QuoteAsset: "USD", Tradable: false},
	}
}

func realBTCInstruments() []exchanges.Instrument {
	return []exchanges.Instrument{
		mapInst("binance_futures", "BTCUSDT", "BTCUSDT", "perp", "BTC", "USDT"),
		mapInst("bybit_futures", "BTCUSDT", "BTCUSDT", "perp", "BTC", "USDT"),
		mapInst("okx_futures", "BTCUSDT", "BTC-USDT-SWAP", "perp", "BTC", "USDT"),
		mapInst("gate_futures", "BTCUSDT", "BTC_USDT", "perp", "BTC", "USDT"),
		mapInst("kraken_futures", "BTCUSDT", "PF_XBTUSD", "perp", "BTC", "USD"),
		mapInst("hyperliquid_futures", "BTCUSDT", "BTC", "perp", "BTC", "USD"),
		mapInst("paradex_futures", "BTCUSDT", "BTC-USD-PERP", "perp", "BTC", "USD"),
		mapInst("binance_spot", "BTCUSDT", "BTCUSDT", "spot", "BTC", "USDT"),
		mapInst("bybit_spot", "BTCUSDT", "BTCUSDT", "spot", "BTC", "USDT"),
	}
}

var btcPair = []PairAssets{{Symbol: "BTCUSDT", BaseAsset: "BTC"}}

// The real 9-source shape: the four USDT perps pair with both USDT spots
// (8 pairs); the three USD-quoted perps (Kraken, Hyperliquid, Paradex) have
// no USD spot market and must be REFUSED with the quote named — hedging them
// with a USDT spot would carry USD/USDT exposure (PLAN.md step 2.4).
func TestBuildHedgeMapping_RealVenueDeclarations(t *testing.T) {
	m := BuildHedgeMapping(realBTCInstruments(), btcPair, realSourceClaims())

	if len(m.Pairs) != 8 {
		t.Fatalf("built %d pairs, want 8: %+v", len(m.Pairs), m.Pairs)
	}
	got := map[string]bool{}
	for _, p := range m.Pairs {
		if p.Symbol != "BTCUSDT" || p.BaseAsset != "BTC" || p.QuoteAsset != "USDT" {
			t.Errorf("pair %s×%s: identity = %s/%s/%s, want BTCUSDT/BTC/USDT",
				p.Spot.Source, p.Perp.Source, p.Symbol, p.BaseAsset, p.QuoteAsset)
		}
		got[p.Spot.Source+"×"+p.Perp.Source] = true
	}
	for _, spot := range []string{"binance_spot", "bybit_spot"} {
		for _, perp := range []string{"binance_futures", "bybit_futures", "gate_futures", "okx_futures"} {
			if !got[spot+"×"+perp] {
				t.Errorf("missing pair %s×%s", spot, perp)
			}
		}
	}

	if len(m.Rejections) != 3 {
		t.Fatalf("got %d rejections, want 3 (the USD-quoted perps): %+v", len(m.Rejections), m.Rejections)
	}
	rejected := map[string]string{}
	for _, r := range m.Rejections {
		rejected[r.Source] = r.Reason
	}
	for _, source := range []string{"kraken_futures", "hyperliquid_futures", "paradex_futures"} {
		reason, ok := rejected[source]
		if !ok {
			t.Errorf("%s: expected a rejection, got none", source)
			continue
		}
		if !strings.Contains(reason, "USD") || !strings.Contains(reason, "refused") {
			t.Errorf("%s: reason %q should name the quote USD and say refused", source, reason)
		}
	}
}

// R11: a symbol_map typo (PF_ETHUSD recorded under BTCUSDT) must be refused
// on the venue's own base declaration — a mispaired hedge is a coin-mismatched
// position. It must never appear in Pairs.
func TestBuildHedgeMapping_MispairedBaseIsRefused(t *testing.T) {
	insts := []exchanges.Instrument{
		mapInst("kraken_futures", "BTCUSDT", "PF_ETHUSD", "perp", "ETH", "USD"),
		mapInst("binance_spot", "BTCUSDT", "BTCUSDT", "spot", "BTC", "USDT"),
	}
	m := BuildHedgeMapping(insts, btcPair, realSourceClaims())
	if len(m.Pairs) != 0 {
		t.Fatalf("a base-mismatched instrument produced pairs: %+v", m.Pairs)
	}
	if !rejectionFor(m, "kraken_futures", "base") {
		t.Fatalf("want kraken_futures rejected naming the base mismatch, got %+v", m.Rejections)
	}
}

// A venue that declares no base/quote cannot be validated — refuse, never
// fall back to parsing the symbol string.
func TestBuildHedgeMapping_VenueSilentOnAssetsIsRefused(t *testing.T) {
	insts := []exchanges.Instrument{
		mapInst("gate_futures", "BTCUSDT", "BTC_USDT", "perp", "", ""),
	}
	m := BuildHedgeMapping(insts, btcPair, realSourceClaims())
	if len(m.Pairs) != 0 || !rejectionFor(m, "gate_futures", "declare") {
		t.Fatalf("want a 'venue declares no assets' rejection, got pairs=%+v rejections=%+v", m.Pairs, m.Rejections)
	}
}

// config.yaml claiming quote USDT for a source whose venue declares USD is a
// config lie the scanner's comparison groups would silently inherit — it must
// surface here, named.
func TestBuildHedgeMapping_SourceQuoteClaimMismatch(t *testing.T) {
	claims := []SourceClaim{{Source: "kraken_futures", MarketType: "perp", QuoteAsset: "USDT", Tradable: true}}
	insts := []exchanges.Instrument{
		mapInst("kraken_futures", "BTCUSDT", "PF_XBTUSD", "perp", "BTC", "USD"),
	}
	m := BuildHedgeMapping(insts, btcPair, claims)
	if len(m.Pairs) != 0 || !rejectionFor(m, "kraken_futures", "USDT") {
		t.Fatalf("want the config's quote claim named in a rejection, got %+v", m.Rejections)
	}
}

// The fetcher and config disagreeing on market type means one of them is
// wrong about what this market IS — refuse.
func TestBuildHedgeMapping_MarketTypeDisagreement(t *testing.T) {
	claims := []SourceClaim{{Source: "binance_spot", MarketType: "spot", QuoteAsset: "USDT", Tradable: true}}
	insts := []exchanges.Instrument{
		mapInst("binance_spot", "BTCUSDT", "BTCUSDT", "perp", "BTC", "USDT"),
	}
	m := BuildHedgeMapping(insts, btcPair, claims)
	if len(m.Pairs) != 0 || !rejectionFor(m, "binance_spot", "market type") {
		t.Fatalf("want a market-type rejection, got %+v", m.Rejections)
	}
}

// A market that is not trading today must not form a hedge pair, however
// well its assets line up.
func TestBuildHedgeMapping_NonTradingNeverPairs(t *testing.T) {
	perp := mapInst("gate_futures", "BTCUSDT", "BTC_USDT", "perp", "BTC", "USDT")
	perp.Status = "delisting"
	insts := []exchanges.Instrument{
		perp,
		mapInst("binance_spot", "BTCUSDT", "BTCUSDT", "spot", "BTC", "USDT"),
	}
	m := BuildHedgeMapping(insts, btcPair, realSourceClaims())
	if len(m.Pairs) != 0 || !rejectionFor(m, "gate_futures", "delisting") {
		t.Fatalf("a delisting market paired anyway: pairs=%+v rejections=%+v", m.Pairs, m.Rejections)
	}
}

// Two standard symbols resolving to ONE native market on one source is a
// config mapping that is not one-to-one — both claimants are refused, since
// there is no way to know which one is right.
func TestBuildHedgeMapping_DuplicateNativeRefused(t *testing.T) {
	pairs := []PairAssets{{Symbol: "BTCUSDT", BaseAsset: "BTC"}, {Symbol: "XBTUSDT", BaseAsset: "BTC"}}
	insts := []exchanges.Instrument{
		mapInst("kraken_futures", "BTCUSDT", "PF_XBTUSD", "perp", "BTC", "USD"),
		mapInst("kraken_futures", "XBTUSDT", "PF_XBTUSD", "perp", "BTC", "USD"),
	}
	m := BuildHedgeMapping(insts, pairs, realSourceClaims())
	if len(m.Pairs) != 0 || len(m.Rejections) != 2 {
		t.Fatalf("want both claimants of PF_XBTUSD refused, got pairs=%+v rejections=%+v", m.Pairs, m.Rejections)
	}
	for _, r := range m.Rejections {
		if !strings.Contains(r.Reason, "PF_XBTUSD") {
			t.Errorf("rejection %q should name the contested native symbol", r.Reason)
		}
	}
}

// A spot leg with no perp counterpart is named too — validation runs in both
// directions, not just perp-looking-for-spot.
func TestBuildHedgeMapping_UnpairedSpotIsNamed(t *testing.T) {
	insts := []exchanges.Instrument{
		mapInst("binance_spot", "BTCUSDT", "BTCUSDT", "spot", "BTC", "USDT"),
		mapInst("kraken_futures", "BTCUSDT", "PF_XBTUSD", "perp", "BTC", "USD"),
	}
	m := BuildHedgeMapping(insts, btcPair, realSourceClaims())
	if len(m.Pairs) != 0 {
		t.Fatalf("USD perp paired with USDT spot: %+v", m.Pairs)
	}
	if !rejectionFor(m, "binance_spot", "no perp") || !rejectionFor(m, "kraken_futures", "no spot") {
		t.Fatalf("want BOTH unpaired legs named, got %+v", m.Rejections)
	}
}

// An instrument for a symbol config.yaml does not declare cannot be checked
// against anything — refused.
func TestBuildHedgeMapping_UnknownSymbolRefused(t *testing.T) {
	insts := []exchanges.Instrument{
		mapInst("binance_futures", "DOGEUSDT", "DOGEUSDT", "perp", "DOGE", "USDT"),
	}
	m := BuildHedgeMapping(insts, btcPair, realSourceClaims())
	if len(m.Pairs) != 0 || !rejectionFor(m, "binance_futures", "symbol") {
		t.Fatalf("want an unknown-symbol rejection, got %+v", m.Rejections)
	}
}

// A source config.yaml does not declare has no quote claim to validate
// against — refused, not trusted.
func TestBuildHedgeMapping_UnknownSourceRefused(t *testing.T) {
	insts := []exchanges.Instrument{
		mapInst("mystery_futures", "BTCUSDT", "BTCUSDT", "perp", "BTC", "USDT"),
	}
	m := BuildHedgeMapping(insts, btcPair, realSourceClaims())
	if len(m.Pairs) != 0 || !rejectionFor(m, "mystery_futures", "config") {
		t.Fatalf("want an unknown-source rejection, got %+v", m.Rejections)
	}
}

// The mapping must be a pure function of its inputs: same instruments in any
// order → identical output, so a log diff means the RULES changed.
func TestBuildHedgeMapping_Deterministic(t *testing.T) {
	insts := realBTCInstruments()
	forward := BuildHedgeMapping(insts, btcPair, realSourceClaims())
	reversed := make([]exchanges.Instrument, 0, len(insts))
	for i := len(insts) - 1; i >= 0; i-- {
		reversed = append(reversed, insts[i])
	}
	backward := BuildHedgeMapping(reversed, btcPair, realSourceClaims())
	if !reflect.DeepEqual(forward, backward) {
		t.Fatalf("input order changed the mapping:\nforward  %+v\nbackward %+v", forward, backward)
	}
}

func TestHedgeMappingLogLines(t *testing.T) {
	m := BuildHedgeMapping(realBTCInstruments(), btcPair, realSourceClaims())
	text := strings.Join(m.LogLines(), "\n")
	if !strings.Contains(text, "BTCUSDT") || !strings.Contains(text, "binance_spot×binance_futures") {
		t.Errorf("log lines should list the pairs per symbol, got:\n%s", text)
	}
	if !strings.Contains(text, "kraken_futures") {
		t.Errorf("log lines should carry the refusals, got:\n%s", text)
	}
	if empty := (HedgeMapping{}).LogLines(); len(empty) == 0 {
		t.Error("an empty mapping should still say something in the log")
	}
}

// Case is compared, not imposed: config.yaml never upper-cases `base:`
// (internal/config normalizes Quote but not Base), and venues declare assets
// in their own casing — so a lowercase config base must still pair.
func TestBuildHedgeMapping_AssetCaseIsFolded(t *testing.T) {
	pairs := []PairAssets{{Symbol: "BTCUSDT", BaseAsset: "btc"}}
	insts := []exchanges.Instrument{
		mapInst("binance_futures", "BTCUSDT", "BTCUSDT", "perp", "BTC", "USDT"),
		mapInst("binance_spot", "BTCUSDT", "BTCUSDT", "spot", "BTC", "usdt"),
	}
	m := BuildHedgeMapping(insts, pairs, realSourceClaims())
	if len(m.Pairs) != 1 {
		t.Fatalf("case difference broke pairing: pairs=%+v rejections=%+v", m.Pairs, m.Rejections)
	}
}

// Hyperliquid's mixed-case markets (kPEPE is 1000 PEPE, kSHIB, kBONK…) must
// pair with the exact spelling config.yaml has to use for symbol_format
// "{base}" — and must NOT pair with the plain coin, which is a different
// asset at a 1000× denomination.
func TestBuildHedgeMapping_MixedCaseVenueAsset(t *testing.T) {
	pairs := []PairAssets{{Symbol: "KPEPEUSDT", BaseAsset: "kPEPE"}}
	claims := []SourceClaim{
		{Source: "hyperliquid_futures", MarketType: "perp", QuoteAsset: "USDT", Tradable: true},
		{Source: "binance_spot", MarketType: "spot", QuoteAsset: "USDT", Tradable: true},
	}
	insts := []exchanges.Instrument{
		mapInst("hyperliquid_futures", "KPEPEUSDT", "kPEPE", "perp", "kPEPE", "USDT"),
		mapInst("binance_spot", "KPEPEUSDT", "KPEPEUSDT", "spot", "kPEPE", "USDT"),
	}
	if m := BuildHedgeMapping(insts, pairs, claims); len(m.Pairs) != 1 {
		t.Fatalf("mixed-case base did not pair: pairs=%+v rejections=%+v", m.Pairs, m.Rejections)
	}

	// The 1000× market must not pair against plain PEPE.
	insts[1] = mapInst("binance_spot", "KPEPEUSDT", "PEPEUSDT", "spot", "PEPE", "USDT")
	m := BuildHedgeMapping(insts, pairs, claims)
	if len(m.Pairs) != 0 || !rejectionFor(m, "binance_spot", "base") {
		t.Fatalf("kPEPE paired with PEPE — a 1000x denomination mismatch: %+v", m.Pairs)
	}
}

// A market type with no hedge role is refused in the validation chain, where
// every other refusal lives — config.yaml's validation accepts "future"
// (dated futures, phase 6) which no fetcher produces yet.
func TestBuildHedgeMapping_UnhedgeableMarketTypeRefused(t *testing.T) {
	claims := []SourceClaim{{Source: "dated_futures", MarketType: "future", QuoteAsset: "USDT", Tradable: true}}
	insts := []exchanges.Instrument{
		mapInst("dated_futures", "BTCUSDT", "BTC-20260930", "future", "BTC", "USDT"),
	}
	m := BuildHedgeMapping(insts, btcPair, claims)
	if len(m.Pairs) != 0 || !rejectionFor(m, "dated_futures", "no hedge role") {
		t.Fatalf("want a no-hedge-role rejection, got pairs=%+v rejections=%+v", m.Pairs, m.Rejections)
	}
}

// Two copies of ONE instrument are duplicate input, not a contested market —
// the diagnosis must not blame the config's symbol_map.
func TestBuildHedgeMapping_DuplicateInputNamedAsSuch(t *testing.T) {
	inst := mapInst("binance_futures", "BTCUSDT", "BTCUSDT", "perp", "BTC", "USDT")
	m := BuildHedgeMapping([]exchanges.Instrument{inst, inst}, btcPair, realSourceClaims())
	if len(m.Pairs) != 0 || !rejectionFor(m, "binance_futures", "duplicate input") {
		t.Fatalf("want a duplicate-input rejection, got %+v", m.Rejections)
	}
	if rejectionFor(m, "binance_futures", "one-to-one") {
		t.Error("duplicate input must not be diagnosed as a config mapping collision")
	}
}

func rejectionFor(m HedgeMapping, source, substr string) bool {
	for _, r := range m.Rejections {
		if r.Source == source && strings.Contains(r.Reason, substr) {
			return true
		}
	}
	return false
}
