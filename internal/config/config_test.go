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

// Funding freshness is measured per venue and belongs to perps only (step 2.7).
// A perp without it is a dashboard cell whose staleness nothing decides.
func TestValidate_RejectsAPerpWithNoFundingFreshness(t *testing.T) {
	cfg := repoConfig(t)
	for i := range cfg.Sources {
		if cfg.Sources[i].MarketType == "perp" {
			cfg.Sources[i].FundingStaleAfterSec = 0
			break
		}
	}
	if err := cfg.Validate(); err == nil {
		t.Error("a perp with no funding_stale_after_sec must be rejected")
	}
}

func TestValidate_RejectsAnUnknownFundingPublishMode(t *testing.T) {
	cfg := repoConfig(t)
	for i := range cfg.Sources {
		if cfg.Sources[i].MarketType == "perp" {
			cfg.Sources[i].FundingPublishMode = "sometimes"
			break
		}
	}
	if err := cfg.Validate(); err == nil {
		t.Error("an unknown funding_publish_mode must be rejected")
	}
}

// The other direction matters too: a spot market has no funding, so a threshold
// there is a copy-paste that reads as configuration nobody will consult.
func TestValidate_RejectsFundingSettingsOnASourceWithNoFunding(t *testing.T) {
	cfg := repoConfig(t)
	for i := range cfg.Sources {
		if cfg.Sources[i].MarketType == "spot" {
			cfg.Sources[i].FundingStaleAfterSec = 60
			break
		}
	}
	if err := cfg.Validate(); err == nil {
		t.Error("funding settings on a spot source must be rejected")
	}
}

// The shipped values are what the dashboard judges freshness by, so they are
// under test rather than merely present: an on_change venue needs a threshold
// far larger than a periodic one, because silence there is normal.
func TestRepoConfig_FundingThresholdsMatchThePublishMode(t *testing.T) {
	cfg := repoConfig(t)
	onChange := 0
	for _, source := range cfg.Sources {
		if source.MarketType != "perp" {
			continue
		}
		if source.FundingStaleAfterSec < 60 {
			t.Errorf("%s: funding_stale_after_sec %d is below the measured floor of 60s",
				source.Source, source.FundingStaleAfterSec)
		}
		if source.FundingPublishMode == FundingOnChange {
			onChange++
			// Its only provable bound is the settlement interval: the venue
			// must republish when the next settlement stamp changes. Anything
			// shorter marks a healthy feed dead.
			if source.FundingStaleAfterSec < 8*3600 {
				t.Errorf("%s publishes on change but its threshold is %ds, under one 8h settlement",
					source.Source, source.FundingStaleAfterSec)
			}
		}
	}
	if onChange == 0 {
		t.Log("no on_change venue configured; the bound above is untested against real config")
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
    funding_stale_after_sec: 60
    funding_publish_mode: periodic
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
			FundingStaleAfterSec: 60, FundingPublishMode: FundingPeriodic,
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
    funding_stale_after_sec: 60
    funding_publish_mode: periodic
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

// Storage (step 2.6). Every check here exists because the failure it prevents
// is silent: a spin-loop sampler, a corpus pruned below what the venues can
// refill, or a persistence layer that quietly writes nothing.

func TestStorage_DisabledNeedsNoSettings(t *testing.T) {
	cfg := repoConfig(t)
	cfg.Storage = Storage{Enabled: false}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a disabled storage block must not need settings: %v", err)
	}
}

func TestStorage_RejectsPeriodsThatWouldNotBound(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*Storage)
		mention string
	}{
		{"no path", func(s *Storage) { s.Path = "" }, "path"},
		{"sub-second sampling", func(s *Storage) { s.PriceSampleEverySec = 0 }, "spin loop"},
		{"top-up every zero minutes", func(s *Storage) { s.FundingTopUpEveryMin = 0 }, "seven venues"},
		{"negative retention", func(s *Storage) { s.RetainFundingDays = -1 }, "keep everything"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := repoConfig(t)
			cfg.Storage.Enabled = true
			tc.mutate(&cfg.Storage)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("the error should explain the consequence, got %v", err)
			}
		})
	}
}

func TestStorage_RejectsAFundingRetentionTheVenuesCannotRefill(t *testing.T) {
	cfg := repoConfig(t)
	cfg.Storage.Enabled = true
	cfg.Storage.RetainFundingDays = 7

	// OKX publishes about 90 days of history and Gate 180. Pruning below that
	// throws away rows that cannot be fetched again at any price, so a small
	// number is far more likely to be a mistake than an intention.
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a 7-day funding retention was accepted")
	}
	if !strings.Contains(err.Error(), "0 to keep everything") {
		t.Errorf("the error should point at the way to keep everything, got %v", err)
	}
}

func TestStorage_ZeroRetentionMeansKeepEverything(t *testing.T) {
	cfg := repoConfig(t)
	cfg.Storage.Enabled = true
	cfg.Storage.RetainFundingDays = 0
	cfg.Storage.RetainPriceDays = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("0 must be a valid retention meaning keep everything: %v", err)
	}
}

func TestLoad_ShippedStorageIsUsable(t *testing.T) {
	cfg := repoConfig(t)
	if !cfg.Storage.Enabled {
		t.Fatal("config.yaml ships storage disabled; step 2.6 is what turns it on")
	}
	// The shipped period is what determines the row count, so it is pinned here
	// and its measured cost is quoted in config.yaml and PLAN 2.6. The upper
	// bound is not taste: past a minute the series stops being fine enough to
	// reconstruct a basis move, and below ten seconds the 90-day retention is
	// measured in multiple gigabytes.
	if cfg.Storage.PriceSampleEverySec < 10 || cfg.Storage.PriceSampleEverySec > 60 {
		t.Errorf("price_sample_every_sec = %d; outside 10-60s the measured storage cost or the resolution stops being defensible",
			cfg.Storage.PriceSampleEverySec)
	}
	if cfg.Storage.RetainFundingDays != 365 || cfg.Storage.RetainPriceDays != 90 {
		t.Errorf("retention = %dd funding / %dd price, want the 12 months / 3 months PLAN 2.6 states",
			cfg.Storage.RetainFundingDays, cfg.Storage.RetainPriceDays)
	}
}

func TestStorage_DefaultsFillAnAbsentBlock(t *testing.T) {
	var cfg Config
	cfg.applyDefaults()
	if cfg.Storage.Enabled {
		t.Error("an absent storage block must not enable persistence")
	}
	// The defaults still have to be coherent, because -db on cmd/backfill can
	// enable a store the config never described.
	if cfg.Storage.Path == "" || cfg.Storage.PriceSampleEverySec < 1 {
		t.Errorf("defaults are unusable: %+v", cfg.Storage)
	}
}

// Depth sampling (step 2.7b). The windows are Go constants, so only the cadence
// and the level count are configurable — and the cadence has a floor.
func TestValidate_RejectsADepthCadenceThatPolls(t *testing.T) {
	cfg := repoConfig(t)
	cfg.Depth.RefreshEveryMin = 1
	if err := cfg.Validate(); err == nil {
		t.Error("a one-minute depth sweep must be rejected; that is polling, not screening")
	}
}

func TestValidate_RejectsNonsenseDepthSettings(t *testing.T) {
	cfg := repoConfig(t)
	cfg.Depth.Levels = 0
	if err := cfg.Validate(); err == nil {
		t.Error("zero depth levels must be rejected")
	}

	cfg = repoConfig(t)
	cfg.Depth.RetainDays = -1
	if err := cfg.Validate(); err == nil {
		t.Error("a negative retention must be rejected; 0 already means keep everything")
	}
}

// Disabled depth must not be validated into a failure: switching the feature off
// is a supported state, not a broken config.
func TestValidate_ADisabledDepthBlockNeedsNoValues(t *testing.T) {
	cfg := repoConfig(t)
	cfg.Depth = Depth{Enabled: false}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a disabled depth block was rejected: %v", err)
	}
}

func TestLoad_DepthDefaultsAreApplied(t *testing.T) {
	// The shipped file sets these explicitly; this checks the fallbacks a file
	// that omits them would get, since an omitted cadence of 0 would otherwise
	// mean "sweep continuously".
	var cfg Config
	cfg.Depth.applyDefaults()
	if cfg.Depth.RefreshEveryMin != defaultDepthRefreshEveryMin || cfg.Depth.Levels != defaultDepthLevels {
		t.Errorf("depth defaults = %+v", cfg.Depth)
	}
	if cfg.Depth.RetainDays != defaultDepthRetainDays {
		t.Errorf("depth retention default = %d", cfg.Depth.RetainDays)
	}
}

// The shipped values are what actually runs.
func TestRepoConfig_DepthIsSaneAndWithinPlanGuidance(t *testing.T) {
	depth := repoConfig(t).Depth
	if !depth.Enabled {
		t.Log("depth is disabled in the shipped config; the liquidity columns will be empty")
		return
	}
	// PLAN §7.4 asks for a sweep every 1-4 hours.
	if depth.RefreshEveryMin < 60 || depth.RefreshEveryMin > 240 {
		t.Errorf("depth.refresh_every_min = %d, outside PLAN §7.4's 60-240", depth.RefreshEveryMin)
	}
	if depth.Levels < 20 {
		t.Errorf("depth.levels = %d; below 20 no venue reaches the 0.5%% window", depth.Levels)
	}
}
