package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoConfig is the config.yaml the scanner actually ships with. Every test here
// runs against it rather than a fixture: a config file nothing exercises is a
// config file that breaks the next time somebody edits it.
func repoConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := Load(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatalf("load config.yaml: %v", err)
	}
	return cfg
}

func TestLoad_ShippedConfigIsValid(t *testing.T) {
	cfg := repoConfig(t)

	if len(cfg.Symbols) == 0 {
		t.Fatal("no symbols configured")
	}
	if len(cfg.Sources) == 0 {
		t.Fatal("no sources configured")
	}
	if cfg.Scanner.AlertMinSpreadPct <= 0 {
		t.Errorf("alert_min_spread_pct = %g, want > 0", cfg.Scanner.AlertMinSpreadPct)
	}
	if cfg.Scanner.DefaultStaleAfterSec <= 0 {
		t.Errorf("default_stale_after_sec = %d, want > 0", cfg.Scanner.DefaultStaleAfterSec)
	}
}

// Base and quote are declared, never parsed out of the symbol string. Deriving
// them by chopping the suffix is how "DOGEUSDT" acquires the base "DOG".
func TestSymbols_DeclareBaseAndQuoteExplicitly(t *testing.T) {
	for _, symbol := range repoConfig(t).Symbols {
		if symbol.Base == "" || symbol.Quote == "" {
			t.Errorf("%s: base=%q quote=%q, both must be declared", symbol.Symbol, symbol.Base, symbol.Quote)
		}
		if symbol.Base+symbol.Quote != symbol.Symbol {
			t.Errorf("%s: base+quote = %q, which does not reconstruct the symbol",
				symbol.Symbol, symbol.Base+symbol.Quote)
		}
	}
}

// The mapping rules, in order: an explicit entry wins, then the format template,
// then - when a map exists but has no entry - the symbol is skipped entirely.
func TestVenueSymbol_MappingPrecedence(t *testing.T) {
	btc := Symbol{Symbol: "BTCUSDT", Base: "BTC", Quote: "USDT"}
	doge := Symbol{Symbol: "DOGEUSDT", Base: "DOGE", Quote: "USDT"}

	cases := []struct {
		name   string
		source Source
		symbol Symbol
		want   string
		ok     bool
	}{
		{
			name:   "explicit map wins over the template",
			source: Source{SymbolFormat: "PF_{base}USD", SymbolMap: map[string]string{"BTCUSDT": "PF_XBTUSD"}},
			symbol: btc, want: "PF_XBTUSD", ok: true,
		},
		{
			name:   "template applies to anything not overridden",
			source: Source{SymbolFormat: "PF_{base}USD", SymbolMap: map[string]string{"BTCUSDT": "PF_XBTUSD"}},
			symbol: doge, want: "PF_DOGEUSD", ok: true,
		},
		{
			name:   "a map with no template skips what it does not name",
			source: Source{SymbolMap: map[string]string{"BTCUSDT": "feed-id"}},
			symbol: doge, want: "", ok: false,
		},
		{
			name:   "no mapping at all is the identity",
			source: Source{},
			symbol: doge, want: "DOGEUSDT", ok: true,
		},
		{
			name:   "quote is substituted too",
			source: Source{SymbolFormat: "{base}-{quote}-SWAP"},
			symbol: btc, want: "BTC-USDT-SWAP", ok: true,
		},
		{
			name:   "the whole symbol can be substituted",
			source: Source{SymbolFormat: "x{symbol}x"},
			symbol: btc, want: "xBTCUSDTx", ok: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.source.VenueSymbol(tc.symbol)
			if ok != tc.ok || got != tc.want {
				t.Errorf("VenueSymbol(%s) = (%q, %v), want (%q, %v)", tc.symbol.Symbol, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// The bug this replaces: Hyperliquid took the first three characters of the
// symbol, which happens to work for BTC/ETH/XRP/SOL and silently subscribes to
// "DOG" for DOGEUSDT.
func TestVenueSymbol_HyperliquidNoLongerTruncatesTheBase(t *testing.T) {
	var hyperliquid Source
	for _, source := range repoConfig(t).Sources {
		if source.Source == "hyperliquid_futures" {
			hyperliquid = source
		}
	}
	if hyperliquid.Source == "" {
		t.Fatal("hyperliquid_futures is not configured")
	}

	got, ok := hyperliquid.VenueSymbol(Symbol{Symbol: "DOGEUSDT", Base: "DOGE", Quote: "USDT"})
	if !ok || got != "DOGE" {
		t.Errorf("DOGEUSDT maps to (%q, %v), want (\"DOGE\", true)", got, ok)
	}
}

// Kraken calls bitcoin XBT, so the template alone would produce PF_BTCUSD and
// subscribe to a market that does not exist.
func TestVenueSymbol_KrakenKnowsBitcoinIsXBT(t *testing.T) {
	cfg := repoConfig(t)
	kraken, ok := cfg.SourceByName("kraken_futures")
	if !ok {
		t.Fatal("kraken_futures is not configured")
	}

	if got, _ := kraken.VenueSymbol(Symbol{Symbol: "BTCUSDT", Base: "BTC", Quote: "USDT"}); got != "PF_XBTUSD" {
		t.Errorf("BTCUSDT maps to %q, want PF_XBTUSD", got)
	}
	if got, _ := kraken.VenueSymbol(Symbol{Symbol: "ETHUSDT", Base: "ETH", Quote: "USDT"}); got != "PF_ETHUSD" {
		t.Errorf("ETHUSDT maps to %q, want PF_ETHUSD", got)
	}
}

// A source that names no symbols it can serve would connect and sit silent.
func TestValidate_RejectsASourceThatServesNoSymbol(t *testing.T) {
	cfg := Config{
		Scanner: Scanner{AlertMinSpreadPct: 0.05, DefaultStaleAfterSec: 10},
		Symbols: []Symbol{{Symbol: "BTCUSDT", Base: "BTC", Quote: "USDT"}},
		Sources: []Source{{
			Source: "nowhere", Connector: "binance_futures", Venue: "v",
			MarketType: "perp", QuoteAsset: "USDT", Label: "x", ShortLabel: "x",
			SymbolMap: map[string]string{"ETHUSDT": "eth"},
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("a source that maps none of the configured symbols must be rejected")
	}
}

func TestValidate_RejectsDuplicateSourceNames(t *testing.T) {
	cfg := repoConfig(t)
	cfg.Sources = append(cfg.Sources, cfg.Sources[0])
	if err := cfg.Validate(); err == nil {
		t.Error("a duplicated source name must be rejected")
	} else if !strings.Contains(err.Error(), cfg.Sources[0].Source) {
		t.Errorf("the error should name the duplicate, got %v", err)
	}
}

func TestValidate_RejectsDuplicateSymbols(t *testing.T) {
	cfg := repoConfig(t)
	cfg.Symbols = append(cfg.Symbols, cfg.Symbols[0])
	if err := cfg.Validate(); err == nil {
		t.Error("a duplicated symbol must be rejected")
	}
}

// A market type the grouping logic does not know would silently form a group of
// its own, or worse, land in one it does not belong to.
func TestValidate_RejectsAnUnknownMarketType(t *testing.T) {
	cfg := repoConfig(t)
	cfg.Sources[0].MarketType = "swap"
	if err := cfg.Validate(); err == nil {
		t.Error("an unknown market_type must be rejected")
	}
}

// A fee marked verified without a citation is exactly the remembered number
// CLAUDE.md rule 5 forbids.
func TestValidate_RejectsAVerifiedFeeWithNoCitation(t *testing.T) {
	cfg := repoConfig(t)
	for i := range cfg.Sources {
		if cfg.Sources[i].Fee.Verified {
			cfg.Sources[i].Fee.DocURL = ""
			break
		}
	}
	if err := cfg.Validate(); err == nil {
		t.Error("a verified fee with no doc_url must be rejected")
	}
}

func TestValidate_RejectsAnUnverifiedFeeCarryingNumbers(t *testing.T) {
	cfg := repoConfig(t)
	for i := range cfg.Sources {
		if !cfg.Sources[i].Fee.Verified {
			cfg.Sources[i].Fee.TakerBps = 5
			break
		}
	}
	if err := cfg.Validate(); err == nil {
		t.Error("an unverified fee carrying a number must be rejected: it would look checked")
	}
}

func TestLoad_MissingFileIsAnError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("a missing config file must be an error, not an empty config")
	}
}

func TestLoad_MalformedYAMLIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(path, []byte("symbols: [\n  - broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("malformed YAML must be an error")
	}
}

// Defaults must be applied on load, not left for every reader to remember.
func TestLoad_AppliesTheDefaultStalenessThreshold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	body := `
scanner:
  alert_min_spread_pct: 0.05
  default_stale_after_sec: 12
symbols:
  - { symbol: BTCUSDT, base: BTC, quote: USDT }
sources:
  - source: s
    connector: binance_futures
    venue: v
    market_type: perp
    quote_asset: USDT
    label: S
    short_label: S
    fee:
      verified: false
      note_vi: fixture
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Sources[0].StaleAfterSec != 12 {
		t.Errorf("stale_after_sec = %d, want the default 12", cfg.Sources[0].StaleAfterSec)
	}
}

// The shipped config has to keep satisfying the rules the code depends on.
func TestShippedConfig_PassesEveryValidation(t *testing.T) {
	if err := repoConfig(t).Validate(); err != nil {
		t.Fatalf("the shipped config.yaml is invalid: %v", err)
	}
}

// Every source must name a connector. Whether that connector EXISTS is checked
// where the wiring lives (cmd/scanner), because the registry of connector
// functions belongs to the exchanges package, not to configuration.
func TestValidate_RejectsASourceWithNoConnector(t *testing.T) {
	cfg := repoConfig(t)
	cfg.Sources[0].Connector = ""
	if err := cfg.Validate(); err == nil {
		t.Error("a source naming no connector must be rejected")
	}
}

// Two pairs mapping to one venue identifier is silent data loss: the reverse
// lookup takes the first match and the second pair never receives a price. A
// "{base}" template collapses BTCUSDT and BTCUSDC onto "BTC" exactly this way.
func TestValidate_RejectsTwoSymbolsMappingToOneVenueIdentifier(t *testing.T) {
	cfg := Config{
		Server:  Server{Port: "8082"},
		Scanner: Scanner{AlertMinSpreadPct: 0.05, DefaultStaleAfterSec: 10},
		Symbols: []Symbol{
			{Symbol: "BTCUSDT", Base: "BTC", Quote: "USDT"},
			{Symbol: "BTCUSDC", Base: "BTC", Quote: "USDC"},
		},
		Sources: []Source{{
			Source: "hyperliquid_futures", Connector: "hyperliquid_futures", Venue: "hyperliquid",
			MarketType: "perp", QuoteAsset: "USD", Label: "H", ShortLabel: "H",
			StaleAfterSec: 10, SymbolFormat: "{base}",
			Fee: Fee{NoteVI: "fixture"},
		}},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("both pairs map to \"BTC\"; one of them would silently never get data")
	}
	if !strings.Contains(err.Error(), "BTC") {
		t.Errorf("the error should name the colliding identifier, got %v", err)
	}
}

func TestValidate_RejectsAnEmptyPort(t *testing.T) {
	cfg := repoConfig(t)
	cfg.Server.Port = ""
	if err := cfg.Validate(); err == nil {
		t.Error("an empty port must be rejected: ListenAndServe would bind a random one")
	}
}

// The group id lowercases the quote asset while the basis and oracle blocks
// compare it exactly, so a differently-cased value would be in the USDT group
// and a quote mismatch at the same time.
func TestLoad_NormalisesCase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	body := `
server: { port: "1" }
scanner:
  alert_min_spread_pct: 0.05
  default_stale_after_sec: 10
symbols:
  - { symbol: BTCUSDT, base: BTC, quote: usdt }
sources:
  - source: s
    connector: binance_futures
    venue: v
    market_type: PERP
    quote_asset: Usdt
    label: S
    short_label: S
    fee: { verified: false, note_vi: fixture }
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Sources[0].QuoteAsset != "USDT" {
		t.Errorf("quote_asset = %q, want USDT", cfg.Sources[0].QuoteAsset)
	}
	if cfg.Sources[0].MarketType != "perp" {
		t.Errorf("market_type = %q, want perp", cfg.Sources[0].MarketType)
	}
	if cfg.Symbols[0].Quote != "USDT" {
		t.Errorf("symbol quote = %q, want USDT", cfg.Symbols[0].Quote)
	}
}
