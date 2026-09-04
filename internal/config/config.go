// Package config loads config.yaml, the single source of truth for which pairs
// the scanner watches, which venues it connects to, and what it knows about each
// of them.
//
// It exists so that adding a pair or a venue is a change to one YAML file rather
// than a change to Go and JavaScript. The dashboard builds its source list,
// colours, labels, symbol selector and cost disclaimer from the `meta` message,
// and `meta` is built from here - so nothing in static/ needs touching either.
//
// The one thing configuration cannot supply is a connector for a venue nobody
// has written one for yet. `connector` names one that exists; the registry of
// those lives in the exchanges package, and cmd/scanner checks the two agree.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Market types the grouping logic understands. A source declaring anything else
// would form a comparison group of its own and nobody would notice.
var knownMarketTypes = map[string]bool{
	"spot": true, "perp": true, "future": true, "oracle": true,
}

// Funding publish modes (step 2.7). They are configuration rather than a Go
// table because they are a MEASURED property of each venue's stream, and the
// dashboard shows which one applies beside every age it prints.
const (
	// FundingPeriodic: the venue republishes on a cadence of its own, so the
	// age of a reading measures liveness.
	FundingPeriodic = "periodic"
	// FundingOnChange: the venue publishes only when a funding field moves, so
	// silence is the normal state and age measures nothing. Its upper bound is
	// the settlement interval — every venue in this mode republishes when the
	// next settlement stamp changes.
	FundingOnChange = "on_change"
)

var knownFundingPublishModes = map[string]bool{
	FundingPeriodic: true, FundingOnChange: true,
}

// marketTypePerp is the only market type that has funding at all.
const marketTypePerp = "perp"

type Config struct {
	Server  Server   `yaml:"server"`
	Scanner Scanner  `yaml:"scanner"`
	Storage Storage  `yaml:"storage"`
	Symbols []Symbol `yaml:"symbols"`
	Sources []Source `yaml:"sources"`
}

type Server struct {
	Port string `yaml:"port"`
}

type Scanner struct {
	// AlertMinSpreadPct is measured on the GROSS spread. See config.yaml.
	AlertMinSpreadPct float64 `yaml:"alert_min_spread_pct"`
	// DefaultStaleAfterSec fills in for a source that declares no threshold.
	DefaultStaleAfterSec int64 `yaml:"default_stale_after_sec"`
}

// Storage configures the SQLite persistence layer (step 2.6).
//
// Every period here is a row-count multiplier, which is why they are
// configuration and not constants. Measured on the shipped schema: a price
// sample costs 133.3 bytes, and 4 pairs × 9 sources kept 90 days is 7.46 GB at
// 5s against 1.24 GB at 30s (internal/store/size_test.go).
type Storage struct {
	// Enabled false leaves the scanner exactly as it was before this step:
	// nothing is opened, nothing is written, no file appears.
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`

	PriceSampleEverySec          int64 `yaml:"price_sample_every_sec"`
	FundingTopUpEveryMin         int64 `yaml:"funding_topup_every_min"`
	InstrumentSnapshotEveryHours int64 `yaml:"instrument_snapshot_every_hours"`
	PruneEveryHours              int64 `yaml:"prune_every_hours"`

	// Retention in days. 0 means KEEP EVERYTHING — the opposite default would
	// let an unset field in a YAML file delete a year of collected funding,
	// which is the one thing here that cannot be re-fetched (three venues cap
	// their published history at 90-180 days).
	RetainFundingDays int `yaml:"retain_funding_days"`
	RetainPriceDays   int `yaml:"retain_price_days"`
}

// Symbol is one tradable pair. Base and Quote are declared rather than parsed
// out of Symbol: chopping a known suffix off the string is how "DOGEUSDT"
// becomes base "DOG", which is the bug this design removes.
type Symbol struct {
	Symbol string `yaml:"symbol"`
	Base   string `yaml:"base"`
	Quote  string `yaml:"quote"`
}

// Fee is one venue's commission at its default tier, in FRACTIONAL basis points.
//
// Verified separates "the fee is zero" from "the fee was never looked up". Both
// leave the numbers at zero and one of the configured venues really does charge
// retail nothing, so the flag is the only thing that can tell them apart.
type Fee struct {
	MakerBps float64 `yaml:"maker_bps"`
	TakerBps float64 `yaml:"taker_bps"`
	Verified bool    `yaml:"verified"`
	DocURL   string  `yaml:"doc_url"`
	NoteVI   string  `yaml:"note_vi"`
}

type Source struct {
	Source    string `yaml:"source"`
	Connector string `yaml:"connector"`

	Venue      string `yaml:"venue"`
	MarketType string `yaml:"market_type"`
	QuoteAsset string `yaml:"quote_asset"`
	Tradable   bool   `yaml:"tradable"`

	Label            string `yaml:"label"`
	ShortLabel       string `yaml:"short_label"`
	Color            string `yaml:"color"`
	LineStyle        string `yaml:"line_style"`
	EnabledByDefault bool   `yaml:"enabled_by_default"`

	StaleAfterSec int64 `yaml:"stale_after_sec"`

	// FundingStaleAfterSec is the staleness threshold for this source's FUNDING
	// readings, which is a different measurement from the price one and much
	// larger (step 2.7). A price arrives on every book change; a funding rate
	// arrives when the venue decides to republish it, which on some venues is
	// only when the number itself moves.
	//
	// Required for perp sources and forbidden on every other kind: spot markets
	// and oracles have no funding, so a threshold there would be a copy-paste
	// nobody ever reads.
	FundingStaleAfterSec int64 `yaml:"funding_stale_after_sec"`

	// FundingPublishMode is how this venue emits funding: "periodic" (a fixed
	// cadence, so age is a real liveness measure) or "on_change" (only when a
	// funding field moves, so a long silence is normal and age proves nothing).
	// The dashboard renders the distinction — measured 2026-09-04, Bybit went
	// 19 minutes without republishing an unchanged rate while the socket carried
	// book updates the whole time.
	FundingPublishMode string `yaml:"funding_publish_mode"`

	// SymbolFormat builds the venue's own identifier from a Symbol. {base},
	// {quote} and {symbol} are substituted.
	SymbolFormat string `yaml:"symbol_format"`
	// SymbolMap overrides SymbolFormat per symbol, and - when SymbolFormat is
	// empty - is the ONLY thing this source serves. Pyth is the second case:
	// its identifiers are price feed ids that no template can produce.
	SymbolMap map[string]string `yaml:"symbol_map"`

	Fee Fee `yaml:"fee"`
}

// VenueSymbol translates one pair into the identifier this source uses for it.
//
// Order: an explicit SymbolMap entry wins, then SymbolFormat, then - when a map
// exists and does not name the symbol - the source does not serve it and ok is
// false. With neither, the standard symbol is used unchanged.
func (s Source) VenueSymbol(symbol Symbol) (string, bool) {
	if venueSymbol, ok := s.SymbolMap[symbol.Symbol]; ok {
		return venueSymbol, true
	}
	if s.SymbolFormat != "" {
		return strings.NewReplacer(
			"{base}", symbol.Base,
			"{quote}", symbol.Quote,
			"{symbol}", symbol.Symbol,
		).Replace(s.SymbolFormat), true
	}
	if len(s.SymbolMap) > 0 {
		return "", false
	}
	return symbol.Symbol, true
}

// SourceByName finds one configured source.
func (c Config) SourceByName(name string) (Source, bool) {
	for _, source := range c.Sources {
		if source.Source == name {
			return source, true
		}
	}
	return Source{}, false
}

// SymbolNames is the plain list of pairs, in configured order.
func (c Config) SymbolNames() []string {
	names := make([]string, 0, len(c.Symbols))
	for _, symbol := range c.Symbols {
		names = append(names, symbol.Symbol)
	}
	return names
}

// Load reads and validates a config file. A missing or malformed file is an
// error, never an empty config: silently starting with no sources would look
// like every venue being down.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}

	var cfg Config
	// KnownFields makes a typo in a key an error rather than a silently ignored
	// setting - a mistyped stale_after_sec would otherwise read as the default
	// and nobody would find out.
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Port == "" {
		c.Server.Port = "8082"
	}
	c.Storage.applyDefaults()
	for i := range c.Sources {
		if c.Sources[i].StaleAfterSec <= 0 {
			c.Sources[i].StaleAfterSec = c.Scanner.DefaultStaleAfterSec
		}
		// Comparison groups key on the quote asset, and three separate blocks
		// compare it exactly while the group id lowercases it. Left as written,
		// "Usdt" would join the USDT group and be treated as a quote mismatch by
		// the basis and oracle blocks at the same time.
		c.Sources[i].QuoteAsset = strings.ToUpper(c.Sources[i].QuoteAsset)
		c.Sources[i].MarketType = strings.ToLower(c.Sources[i].MarketType)
	}
	for i := range c.Symbols {
		c.Symbols[i].Quote = strings.ToUpper(c.Symbols[i].Quote)
	}
}

// storageDefaults are the periods used when the block names none. They are the
// slowest cadence each job is still useful at: funding settles in hours, venue
// rules change on a venue's own schedule, and a 30s price sample is fine enough
// to reconstruct a basis series a position is held across for days. The price
// period is 30 rather than the plan's 5 because the cost was measured:
// 133.3 bytes a row, which is 7.46 GB at 5s over the 90-day retention against
// 1.24 GB at 30s (internal/store/size_test.go).
const (
	defaultStoragePath                  = "data/scanner.db"
	defaultPriceSampleEverySec          = 30
	defaultFundingTopUpEveryMin         = 60
	defaultInstrumentSnapshotEveryHours = 6
	defaultPruneEveryHours              = 24
	defaultRetainFundingDays            = 365
	defaultRetainPriceDays              = 90
)

// minRetainFundingDays guards the one irreplaceable series. Three of the seven
// venues publish only 90-180 days of history, so a corpus pruned below that
// cannot be rebuilt from the venues at any price — a mistyped retention is
// permanent in a way a mistyped sampling period is not.
const minRetainFundingDays = 30

func (s *Storage) applyDefaults() {
	if s.Path == "" {
		s.Path = defaultStoragePath
	}
	if s.PriceSampleEverySec == 0 {
		s.PriceSampleEverySec = defaultPriceSampleEverySec
	}
	if s.FundingTopUpEveryMin == 0 {
		s.FundingTopUpEveryMin = defaultFundingTopUpEveryMin
	}
	if s.InstrumentSnapshotEveryHours == 0 {
		s.InstrumentSnapshotEveryHours = defaultInstrumentSnapshotEveryHours
	}
	if s.PruneEveryHours == 0 {
		s.PruneEveryHours = defaultPruneEveryHours
	}
	// Retention is NOT defaulted when set to 0: 0 is the documented "keep
	// everything". It is defaulted only when the whole block is absent, which
	// applyDefaults cannot distinguish — so absence is handled by the config
	// file shipping both numbers explicitly, and a deliberate 0 stays 0.
	if !s.Enabled {
		return
	}
	if s.RetainFundingDays == 0 && s.RetainPriceDays == 0 {
		s.RetainFundingDays, s.RetainPriceDays = defaultRetainFundingDays, defaultRetainPriceDays
	}
}

func (s Storage) validate() error {
	if !s.Enabled {
		return nil
	}
	switch {
	case s.Path == "":
		return fmt.Errorf("storage.path is empty")
	case s.PriceSampleEverySec < 1:
		return fmt.Errorf("storage.price_sample_every_sec is %d; below one second the sampler is a spin loop and the row count stops being bounded",
			s.PriceSampleEverySec)
	case s.FundingTopUpEveryMin < 1:
		return fmt.Errorf("storage.funding_topup_every_min is %d; each tick queries seven venues over REST",
			s.FundingTopUpEveryMin)
	case s.InstrumentSnapshotEveryHours < 1:
		return fmt.Errorf("storage.instrument_snapshot_every_hours is %d", s.InstrumentSnapshotEveryHours)
	case s.PruneEveryHours < 1:
		return fmt.Errorf("storage.prune_every_hours is %d", s.PruneEveryHours)
	case s.RetainFundingDays < 0 || s.RetainPriceDays < 0:
		return fmt.Errorf("storage retention cannot be negative (funding %d, price %d); 0 means keep everything",
			s.RetainFundingDays, s.RetainPriceDays)
	case s.RetainFundingDays > 0 && s.RetainFundingDays < minRetainFundingDays:
		return fmt.Errorf("storage.retain_funding_days is %d; below %d the backtest corpus is pruned faster than the venues can refill it (OKX publishes ~90 days, Gate 180). Use 0 to keep everything",
			s.RetainFundingDays, minRetainFundingDays)
	}
	return nil
}

// Validate rejects a configuration the rest of the scanner would misread rather
// than reject. Everything checked here is something that would otherwise fail
// silently: a venue that connects and never speaks, a source in no comparison
// group, a fee that looks checked but is not.
func (c Config) Validate() error {
	if c.Scanner.AlertMinSpreadPct <= 0 {
		return fmt.Errorf("scanner.alert_min_spread_pct must be > 0, got %g", c.Scanner.AlertMinSpreadPct)
	}
	if c.Scanner.DefaultStaleAfterSec <= 0 {
		return fmt.Errorf("scanner.default_stale_after_sec must be > 0, got %d", c.Scanner.DefaultStaleAfterSec)
	}
	if c.Server.Port == "" {
		return fmt.Errorf("server.port is empty; ListenAndServe would bind a random port")
	}
	if len(c.Symbols) == 0 {
		return fmt.Errorf("no symbols configured")
	}
	if len(c.Sources) == 0 {
		return fmt.Errorf("no sources configured")
	}
	if err := c.Storage.validate(); err != nil {
		return err
	}

	seenSymbol := make(map[string]bool, len(c.Symbols))
	for _, symbol := range c.Symbols {
		switch {
		case symbol.Symbol == "":
			return fmt.Errorf("a symbol entry has no symbol")
		case symbol.Base == "" || symbol.Quote == "":
			return fmt.Errorf("symbol %s must declare base and quote", symbol.Symbol)
		case seenSymbol[symbol.Symbol]:
			return fmt.Errorf("symbol %s is configured twice", symbol.Symbol)
		}
		seenSymbol[symbol.Symbol] = true
	}

	seenSource := make(map[string]bool, len(c.Sources))
	for _, source := range c.Sources {
		if err := c.validateSource(source, seenSource); err != nil {
			return err
		}
		seenSource[source.Source] = true
	}
	return nil
}

func (c Config) validateSource(source Source, seen map[string]bool) error {
	switch {
	case source.Source == "":
		return fmt.Errorf("a source entry has no source name")
	case seen[source.Source]:
		return fmt.Errorf("source %s is configured twice", source.Source)
	case source.Connector == "":
		return fmt.Errorf("source %s names no connector", source.Source)
	case source.Venue == "":
		return fmt.Errorf("source %s has no venue", source.Source)
	case !knownMarketTypes[source.MarketType]:
		return fmt.Errorf("source %s has market_type %q, which the grouping logic does not know",
			source.Source, source.MarketType)
	case source.QuoteAsset == "":
		return fmt.Errorf("source %s has no quote_asset; it could not be put in a comparison group",
			source.Source)
	case source.Label == "" || source.ShortLabel == "":
		return fmt.Errorf("source %s needs both label and short_label to be renderable", source.Source)
	case source.StaleAfterSec <= 0:
		return fmt.Errorf("source %s has stale_after_sec %d; zero would mark every price stale on arrival",
			source.Source, source.StaleAfterSec)
	}

	if err := validateFunding(source); err != nil {
		return err
	}

	// A source that can serve none of the configured pairs would connect,
	// subscribe to nothing and sit there looking healthy.
	//
	// Two pairs mapping to ONE venue identifier is worse: the reverse lookup
	// takes the first match, so the second pair silently never receives a price.
	// A "{base}" format collapses BTCUSDT and BTCUSDC onto "BTC" exactly this
	// way.
	var served int
	claimedBy := make(map[string]string, len(c.Symbols))
	for _, symbol := range c.Symbols {
		venueSymbol, ok := source.VenueSymbol(symbol)
		if !ok {
			continue
		}
		if owner, taken := claimedBy[venueSymbol]; taken {
			return fmt.Errorf("source %s maps both %s and %s to %q; the reverse lookup would drop one of them",
				source.Source, owner, symbol.Symbol, venueSymbol)
		}
		claimedBy[venueSymbol] = symbol.Symbol
		served++
	}
	if served == 0 {
		return fmt.Errorf("source %s maps none of the configured symbols", source.Source)
	}

	return validateFee(source)
}

// validateFunding checks the funding freshness settings, which exist for perp
// sources and only for them.
//
// Both directions are enforced. A perp without them would have its funding
// readings judged by nothing, and every dashboard cell would have to invent a
// default; a spot source or an oracle carrying them is a copy-paste that reads
// as configuration nobody will ever consult, which is worse than an error
// because it looks deliberate.
func validateFunding(source Source) error {
	if source.MarketType != marketTypePerp {
		if source.FundingStaleAfterSec != 0 || source.FundingPublishMode != "" {
			return fmt.Errorf("source %s is %s and has no funding, but declares funding settings",
				source.Source, source.MarketType)
		}
		return nil
	}
	if source.FundingStaleAfterSec <= 0 {
		return fmt.Errorf("source %s is a perp and needs funding_stale_after_sec; zero would mark every funding reading stale on arrival",
			source.Source)
	}
	if !knownFundingPublishModes[source.FundingPublishMode] {
		return fmt.Errorf("source %s has funding_publish_mode %q, want %q or %q",
			source.Source, source.FundingPublishMode, FundingPeriodic, FundingOnChange)
	}
	return nil
}

func validateFee(source Source) error {
	fee := source.Fee
	if fee.Verified {
		if fee.DocURL == "" {
			return fmt.Errorf("source %s claims a verified fee with no doc_url; a remembered fee is not a verified one",
				source.Source)
		}
		if fee.TakerBps <= 0 {
			return fmt.Errorf("source %s claims a verified fee but taker_bps is %g", source.Source, fee.TakerBps)
		}
		if fee.MakerBps > fee.TakerBps {
			return fmt.Errorf("source %s has maker %g > taker %g bps", source.Source, fee.MakerBps, fee.TakerBps)
		}
		// A misplaced decimal point turns 0.05% into 0.5%. These bounds do not
		// verify the numbers, they catch a slip.
		if fee.TakerBps > 30 || fee.MakerBps < 0 {
			return fmt.Errorf("source %s fees %g/%g bps are outside the plausible range",
				source.Source, fee.MakerBps, fee.TakerBps)
		}
		return nil
	}

	// An unverified entry carrying numbers is worse than one carrying none: it
	// looks checked.
	if fee.MakerBps != 0 || fee.TakerBps != 0 {
		return fmt.Errorf("source %s has fees %g/%g but verified is false; set verified: true and cite doc_url, or leave the numbers out",
			source.Source, fee.MakerBps, fee.TakerBps)
	}
	if fee.NoteVI == "" {
		return fmt.Errorf("source %s has no verified fee and no note saying why", source.Source)
	}
	return nil
}
