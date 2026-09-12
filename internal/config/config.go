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
	"sort"
	"strings"
	"time"

	"futures-arbitrage-scanner/internal/risk"
	"futures-arbitrage-scanner/internal/strategy"

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

// marketTypeOracle is a price feed that is not tradable and, today, not even a
// WebSocket: Pyth is SSE with its own read loop.
const marketTypeOracle = "oracle"

// MinDataSilenceSec is the floor on data_silence_sec.
//
// Below it the check would stop meaning what it says. The WebSocket lifecycle
// already gives up on a socket that delivers NOTHING - no data, no pong, no
// server ping - after 60 seconds; a data deadline under that would fire first
// on every merely-quiet socket and turn a health check into a reconnect loop.
// The measurement says the same thing from the other side: the worst
// socket-wide gap seen across nine venues on 2026-09-12 was 18.38s, so
// anything near it is inside normal operation.
const MinDataSilenceSec = 60

type Config struct {
	Server   Server   `yaml:"server"`
	Scanner  Scanner  `yaml:"scanner"`
	Storage  Storage  `yaml:"storage"`
	Depth    Depth    `yaml:"depth"`
	Hedge    Hedge    `yaml:"hedge"`
	Strategy Strategy `yaml:"strategy"`
	Symbols  []Symbol `yaml:"symbols"`
	Sources  []Source `yaml:"sources"`
}

// Hedge is what the operator DECLARES about pairing a spot leg with a perp
// leg. Today it holds one decision, and that decision is a risk decision, not
// a fact about the venues: which quote assets may stand in for each other.
//
// instruments.BuildHedgeMapping refuses to pair legs whose venue-declared
// quotes differ, because a USD-quoted perp against a USDT spot is delta-neutral
// in the coin and OPEN in USDT/USD. That exposure is real (USDT has traded
// several percent off par) and nothing in this project prices it. Writing the
// equivalence here is how an operator takes it on knowingly; leaving the block
// out keeps the older, stricter behaviour.
type Hedge struct {
	// QuoteEquivalents groups quote assets that may hedge each other, e.g.
	// [[USD, USDT]]. Case-insensitive; an asset may appear in at most one
	// group. Empty (or absent) means only identical quotes pair.
	QuoteEquivalents [][]string `yaml:"quote_equivalents"`
}

// validate refuses a declaration that could not mean anything: a group needs
// two distinct assets to be an equivalence at all, and an asset in two groups
// would make "equivalent" non-transitive — USD≡USDT and USDT≡USDC would leave
// USD and USDC related through USDT but not to each other, which is a rule
// nobody could read off the file.
func (h Hedge) validate() error {
	seen := make(map[string]int, len(h.QuoteEquivalents)*2)
	for i, group := range h.QuoteEquivalents {
		if len(group) < 2 {
			return fmt.Errorf("hedge.quote_equivalents[%d] lists %d asset(s); an equivalence needs at least 2", i, len(group))
		}
		inGroup := make(map[string]bool, len(group))
		for _, asset := range group {
			asset = strings.ToUpper(strings.TrimSpace(asset))
			if asset == "" {
				return fmt.Errorf("hedge.quote_equivalents[%d] has an empty asset name", i)
			}
			if inGroup[asset] {
				return fmt.Errorf("hedge.quote_equivalents[%d] lists %s twice", i, asset)
			}
			if j, dup := seen[asset]; dup {
				return fmt.Errorf("%s appears in hedge.quote_equivalents[%d] and [%d]; an asset belongs to at most one group", asset, j, i)
			}
			inGroup[asset] = true
			seen[asset] = i
		}
	}
	return nil
}

// Strategy is the live signal evaluator's tuning (step 3.5) — the same numbers
// internal/strategy.Params carries, spelled out in YAML with their units.
//
// Every threshold is REQUIRED when the block is enabled and none of them has a
// default: these are the exact parameters the step-3.5 gate compares against a
// backtest, and a threshold quietly filled in by code would make the two runs
// incomparable without anyone having chosen it. Only the two cadences default.
type Strategy struct {
	// Enabled false leaves the scanner exactly as before this step: nothing is
	// evaluated and the signal journal stays empty.
	Enabled bool `yaml:"enabled"`

	// EvaluateEveryMin is how often the live path re-evaluates every hedgeable
	// pair. Funding settles hourly at the fastest, so minutes is plenty.
	EvaluateEveryMin int64 `yaml:"evaluate_every_min"`
	// MaxBookAgeMin bounds how old a depth sample may be and still price a
	// fill (strategy.RoundTripInput.MaxBookAge). The sweep runs hourly, so this
	// must exceed depth.refresh_every_min or every fill is refused as stale.
	MaxBookAgeMin int64 `yaml:"max_book_age_min"`

	MinRatePer8hBps    float64 `yaml:"min_rate_per_8h_bps"`
	PersistencePeriods int     `yaml:"persistence_periods"`
	MinNetAPRFrac      float64 `yaml:"min_net_apr_frac"`
	NotionalQuote      float64 `yaml:"notional_quote"`
	HoldingDays        float64 `yaml:"holding_days"`

	ExitNetAPRFrac         float64 `yaml:"exit_net_apr_frac"`
	ExitPersistencePeriods int     `yaml:"exit_persistence_periods"`

	// Sign-flip exit gates (strategy.Params.ExitNegative*). OPTIONAL, and the
	// zero values are the rule as step 3.2 wrote it — close on any settled
	// negative print — so a config written before 2026-09-07 (the one the 3.5
	// process loaded) still means exactly what it meant. A non-zero value is a
	// rule change and belongs after the 3.5 gate.
	ExitNegativeMinBps      float64 `yaml:"exit_negative_min_bps"`
	ExitNegativePeriods     int     `yaml:"exit_negative_periods"`
	ExitNegativeCumCostFrac float64 `yaml:"exit_negative_cum_cost_frac"`

	// MinHoldRecoveredCostFrac blocks the two YIELD exits until the position
	// has collected this fraction of its round-trip cost back. OPTIONAL, and 0
	// is off — the rule exactly as it stood before the key existed, which is
	// what the running 3.5 journal loaded. Risk exits are never blocked; the
	// jurisdiction is written on strategy.Params.
	MinHoldRecoveredCostFrac float64 `yaml:"min_hold_recovered_cost_frac"`

	// PerpMarginFrac is collateral posted on the short perp leg as a fraction
	// of its notional — the reciprocal of leverage, so 0.1 is 10x. 0 turns the
	// margin condition off, which is every run before it existed.
	PerpMarginFrac float64 `yaml:"perp_margin_frac"`
	// MinLiquidationBufferPct closes the position when the price is within
	// this many percent of the perp leg's liquidation price. Required to be
	// non-zero whenever PerpMarginFrac is set: leaving only once the venue has
	// already liquidated is not a rule, it is a report.
	MinLiquidationBufferPct float64 `yaml:"min_liquidation_buffer_pct"`

	MaxBasisPct      float64 `yaml:"max_basis_pct"`
	MaxBasisWidenPct float64 `yaml:"max_basis_widen_pct"`

	// Series selection (strategy.Params.MinTrailingMeanBps / TrailingMeanDays,
	// added 2026-09-09): the series' mean settled rate over the last
	// trailing_mean_days must clear min_trailing_mean_bps before a position is
	// opened on it. OPTIONAL; 0 is off, which is the rule as it stood before
	// the keys existed and what the running 3.5 journal loaded. A floor needs
	// a horizon: a positive floor with zero days is refused at load.
	MinTrailingMeanBps float64 `yaml:"min_trailing_mean_bps"`
	TrailingMeanDays   float64 `yaml:"trailing_mean_days"`
	// TrailingMeanMinCostFrac (strategy.Params.TrailingMeanMinCostFrac, added
	// 2026-09-10) selects on the series' OWN cost-crossing: the trailing
	// mean over trailing_mean_days, held for holding_days, must pay this
	// fraction of the priced round trip. OPTIONAL; 0 is off, and like the
	// absolute floor it needs the horizon.
	TrailingMeanMinCostFrac float64 `yaml:"trailing_mean_min_cost_frac"`
}

// Depth configures the periodic order book sampling (step 2.7b).
//
// The WINDOWS are deliberately absent: they are Go constants in internal/depth
// because the store's column names carry them (bid_depth_within_0_1pct_quote),
// and a YAML edit must not be able to redefine what a stored column means to a
// reader six months later.
type Depth struct {
	// Enabled false leaves the scanner exactly as it was before this step:
	// no book is fetched and the liquidity columns stay empty.
	Enabled bool `yaml:"enabled"`

	// RefreshEveryMin is the sweep period. PLAN §7.4 asks for 1-4 hours: a
	// funding position is held for days, so there is no reason to know the
	// book minute by minute, and each sweep is one REST request per market.
	RefreshEveryMin int64 `yaml:"refresh_every_min"`

	// Levels is how many price levels to ask each venue for. Venues that cap
	// lower return what they have (Hyperliquid gives 20 whatever is asked) and
	// Kraken has no limit parameter at all and always returns its whole book.
	Levels int `yaml:"levels"`

	// RetainDays keeps the snapshots. 0 means KEEP EVERYTHING, the same
	// asymmetry as the other retention fields — and it matters more here,
	// because depth is the one series that can never be re-fetched.
	RetainDays int `yaml:"retain_days"`
}

type Server struct {
	Port string `yaml:"port"`
}

type Scanner struct {
	// AlertMinSpreadPct is measured on the GROSS spread. See config.yaml.
	AlertMinSpreadPct float64 `yaml:"alert_min_spread_pct"`
	// DefaultStaleAfterSec fills in for a source that declares no threshold.
	DefaultStaleAfterSec int64 `yaml:"default_stale_after_sec"`

	// DefaultDataSilenceSec fills in for a source that declares no
	// data_silence_sec. Zero leaves the check OFF everywhere it is not set
	// per source, which is what a config written before 2026-09-12 gets.
	DefaultDataSilenceSec int64 `yaml:"default_data_silence_sec"`
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

	// RetainPriceHistoryDays keeps the hourly CANDLES (price_history), and is
	// separate from RetainPriceDays for the reason written on
	// store.Retention: the two tables have row counts three orders of
	// magnitude apart and one of them is a backtest corpus. Defaults to the
	// funding retention, because the two are collected for the same replay
	// and a basis series shorter than its funding series makes the basis exit
	// unevaluable exactly where the funding data still exists.
	RetainPriceHistoryDays int `yaml:"retain_price_history_days"`
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

// Margin is one perp venue's MAINTENANCE margin bracket, in the same shape and
// with the same discipline as Fee.
//
// Verified separates "the venue publishes this and it was read on the stated
// day" from "nobody looked it up". It is not cosmetic here: an unverified 0
// would put the liquidation price further from the market than any venue would
// allow, which is the most dangerous direction a missing number can round, so
// internal/risk REFUSES to produce a liquidation price for an unverified
// bracket rather than treating it as free.
//
// TierCeilingQuote is the top of the tier the rate applies to, because
// maintenance is a STEP function of position size. Left at 0 when the venue
// expresses its ceiling in CONTRACTS (OKX), because converting it needs the
// instrument registry and a wrong ceiling is worse than none.
type Margin struct {
	MaintenanceFrac  float64 `yaml:"maintenance_frac"`
	TierCeilingQuote float64 `yaml:"tier_ceiling_quote"`
	MaxLeverage      float64 `yaml:"max_leverage"`
	Verified         bool    `yaml:"verified"`
	DocURL           string  `yaml:"doc_url"`
	NoteVI           string  `yaml:"note_vi"`
}

// Bracket is the internal/risk view of this block.
func (m Margin) Bracket(source string) risk.Bracket {
	return risk.Bracket{
		Source: source, MaintenanceMarginFrac: m.MaintenanceFrac,
		TierCeilingQuote: m.TierCeilingQuote, MaxLeverage: m.MaxLeverage,
		Verified: m.Verified, NoteVI: m.NoteVI,
	}
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

	// DataSilenceSec is how long ONE WebSocket session may keep answering
	// while delivering no usable data for THIS source before the connector
	// tears it down and dials again, which re-sends the subscription.
	//
	// It is a different claim from StaleAfterSec and must be much larger.
	// StaleAfterSec says "stop believing this number"; this says "the
	// subscription behind the socket is gone". Getting it wrong in the small
	// direction reconnects a healthy venue during a quiet minute; getting it
	// wrong in the large direction is what cost bybit_spot 19 hours of the
	// step-3.5 gate (PLAN step 1.6).
	//
	// Absent (or zero) inherits scanner.default_data_silence_sec; the check is
	// off only where that default is itself absent, and on oracles, which run
	// their own read loop and would silently ignore it.
	DataSilenceSec int64 `yaml:"data_silence_sec"`

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
	// Margin is the perp maintenance bracket. Absent on spot sources and on
	// the oracle, which hold no leveraged position.
	Margin Margin `yaml:"margin"`
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
	c.Depth.applyDefaults()
	c.Strategy.applyDefaults()
	for i := range c.Sources {
		if c.Sources[i].StaleAfterSec <= 0 {
			c.Sources[i].StaleAfterSec = c.Scanner.DefaultStaleAfterSec
		}
		// == 0, not <= 0: a NEGATIVE value must survive to be refused by
		// validation rather than be quietly replaced by the default. "Absent"
		// and "written wrong" are different mistakes and only one of them is
		// safe to fix silently.
		//
		// An ORACLE is skipped: Pyth is SSE and keeps its own read loop, which
		// the WebSocket lifecycle's data clock does not reach. Filling the
		// field there would be a setting that looks like protection and is
		// none — and Pyth answering 401 for a whole 72h soak with nobody
		// noticing is exactly the mistake worth not repeating.
		if c.Sources[i].DataSilenceSec == 0 && c.Sources[i].MarketType != marketTypeOracle {
			c.Sources[i].DataSilenceSec = c.Scanner.DefaultDataSilenceSec
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
	// Same reason as the source quotes above: the mapping compares assets
	// case-insensitively, but every message printed from this declaration
	// should read in the one casing the rest of the file uses.
	for i := range c.Hedge.QuoteEquivalents {
		for j := range c.Hedge.QuoteEquivalents[i] {
			c.Hedge.QuoteEquivalents[i][j] = strings.ToUpper(strings.TrimSpace(c.Hedge.QuoteEquivalents[i][j]))
		}
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

// The defensible price-sampling range, from the step-2.6 measurement of
// 133.3 bytes/row (MEASURE_STORE=1 go test -run TestPriceSnapshotRowCost):
// at 36 series and 90-day retention, 30s ≈ 1.24 GB while 5s ≈ 7.46 GB —
// below the floor storage grows past what retention was sized for, above the
// ceiling a days-long funding position loses its price resolution.
const (
	minPriceSampleEverySec = 10
	maxPriceSampleEverySec = 60
)

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
	if s.RetainPriceHistoryDays == 0 {
		s.RetainPriceHistoryDays = s.RetainFundingDays
	}
}

// depthDefaults: hourly is the slow end of PLAN §7.4's 1-4 hours and still 24
// samples a day per market, and 100 levels is what every venue with a limit
// parameter answered in full when measured. A year of hourly sweeps over 36
// markets is ~315k rows, which is small beside one day of price samples.
const (
	defaultDepthRefreshEveryMin = 60
	defaultDepthLevels          = 100
	defaultDepthRetainDays      = 365
)

func (d *Depth) applyDefaults() {
	if d.RefreshEveryMin <= 0 {
		d.RefreshEveryMin = defaultDepthRefreshEveryMin
	}
	if d.Levels <= 0 {
		d.Levels = defaultDepthLevels
	}
	if d.RetainDays == 0 {
		d.RetainDays = defaultDepthRetainDays
	}
}

// minDepthRefreshEveryMin stops a config edit from turning a screening tool
// into a polling loop. Every sweep is one REST request per market against the
// same IP budget the live feeds use, and a book sampled every minute answers no
// question this project asks — full depth while an order is live is phase 4.4's
// job and belongs on a WebSocket.
const minDepthRefreshEveryMin = 5

func (d Depth) validate() error {
	if !d.Enabled {
		return nil
	}
	switch {
	case d.RefreshEveryMin < minDepthRefreshEveryMin:
		return fmt.Errorf("depth.refresh_every_min is %d; below %d it polls venues for a book nothing reads that often",
			d.RefreshEveryMin, minDepthRefreshEveryMin)
	case d.Levels < 1:
		return fmt.Errorf("depth.levels must be at least 1, got %d", d.Levels)
	case d.RetainDays < 0:
		return fmt.Errorf("depth.retain_days is %d; use 0 to keep everything", d.RetainDays)
	}
	return nil
}

func (s Storage) validate() error {
	if !s.Enabled {
		return nil
	}
	switch {
	case s.Path == "":
		return fmt.Errorf("storage.path is empty")
	case s.PriceSampleEverySec < minPriceSampleEverySec || s.PriceSampleEverySec > maxPriceSampleEverySec:
		// Bounded in VALIDATION, not only by the repo test that pins the
		// shipped config.yaml: that test never sees a file passed via -config,
		// and the row count is linear in this number — a typo'd 3 (meant 30)
		// is ~12 GB per 90 days on the measured 133.3 bytes/row, with no
		// warning until the disk fills.
		return fmt.Errorf("storage.price_sample_every_sec is %d; the defensible range is %d–%d "+
			"(measured 133.3 bytes/row: below it storage grows past what the retention was sized for, "+
			"above it a days-long funding position loses its price resolution)",
			s.PriceSampleEverySec, minPriceSampleEverySec, maxPriceSampleEverySec)
	case s.FundingTopUpEveryMin < 1:
		return fmt.Errorf("storage.funding_topup_every_min is %d; each tick queries seven venues over REST",
			s.FundingTopUpEveryMin)
	case s.InstrumentSnapshotEveryHours < 1:
		return fmt.Errorf("storage.instrument_snapshot_every_hours is %d", s.InstrumentSnapshotEveryHours)
	case s.PruneEveryHours < 1:
		return fmt.Errorf("storage.prune_every_hours is %d", s.PruneEveryHours)
	case s.RetainPriceHistoryDays < 0:
		return fmt.Errorf("storage.retain_price_history_days cannot be negative (%d); 0 means keep everything",
			s.RetainPriceHistoryDays)
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
	if err := c.Depth.validate(); err != nil {
		return err
	}
	if err := c.Storage.validate(); err != nil {
		return err
	}
	if err := c.Strategy.validate(c.Depth); err != nil {
		return err
	}
	if err := c.Hedge.validate(); err != nil {
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
	case source.DataSilenceSec < 0:
		return fmt.Errorf("source %s has data_silence_sec %d; a negative deadline is not a deadline",
			source.Source, source.DataSilenceSec)
	case source.DataSilenceSec > 0 && source.DataSilenceSec < MinDataSilenceSec:
		return fmt.Errorf("source %s has data_silence_sec %d, below the %d-second floor; "+
			"the socket's own read deadline is 60s, so a shorter data deadline would re-dial every quiet minute",
			source.Source, source.DataSilenceSec, MinDataSilenceSec)
	case source.DataSilenceSec > 0 && source.DataSilenceSec <= source.StaleAfterSec:
		// Tearing a socket down before its own data is even called stale
		// would reconnect a venue the scanner still trusts, and on a venue
		// that publishes on change (Bybit funding) it would reconnect
		// constantly. The two thresholds answer different questions and the
		// silence one is always the longer.
		return fmt.Errorf("source %s has data_silence_sec %d at or below stale_after_sec %d; "+
			"the session would be re-dialled before its own prices were even called stale",
			source.Source, source.DataSilenceSec, source.StaleAfterSec)
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

	if err := validateFee(source); err != nil {
		return err
	}
	return validateMargin(source)
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

// validateMargin checks the maintenance bracket the same way validateFee checks
// the commission, and for a sharper reason: an unverified 0 here does not make
// a position look free, it makes it look UNLIQUIDATABLE.
func validateMargin(source Source) error {
	m := source.Margin
	switch {
	case m.MaintenanceFrac < 0 || m.MaintenanceFrac >= 1:
		return fmt.Errorf("source %s has margin.maintenance_frac %g; it is a fraction and must be in [0,1)",
			source.Source, m.MaintenanceFrac)
	case m.TierCeilingQuote < 0:
		return fmt.Errorf("source %s has a negative margin.tier_ceiling_quote (%g)", source.Source, m.TierCeilingQuote)
	case m.MaxLeverage < 0:
		return fmt.Errorf("source %s has a negative margin.max_leverage (%g)", source.Source, m.MaxLeverage)
	case m.Verified && m.MaintenanceFrac <= 0:
		return fmt.Errorf("source %s declares margin.verified: true with maintenance_frac %g — a venue that "+
			"required no maintenance margin would never liquidate anything, so this is a transcription "+
			"error, not a measurement", source.Source, m.MaintenanceFrac)
	case m.Verified && m.DocURL == "":
		return fmt.Errorf("source %s declares margin.verified: true with no doc_url; a verified figure has "+
			"to say where it was read", source.Source)
	case source.MarketType != "perp" && m.MaintenanceFrac > 0:
		return fmt.Errorf("source %s is %s, not a perp, but declares a maintenance margin — spot holds no "+
			"leveraged position and the number would be read as one", source.Source, source.MarketType)
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

// The two strategy cadences that may default. Ten minutes re-evaluates well
// inside the shortest settlement interval (1h); a book age of two hours covers
// one missed hourly sweep without pricing fills on a book from yesterday.
const (
	defaultStrategyEvaluateEveryMin = 10
	defaultStrategyMaxBookAgeMin    = 120
)

func (st *Strategy) applyDefaults() {
	if st.EvaluateEveryMin <= 0 {
		st.EvaluateEveryMin = defaultStrategyEvaluateEveryMin
	}
	if st.MaxBookAgeMin <= 0 {
		st.MaxBookAgeMin = defaultStrategyMaxBookAgeMin
	}
}

func (st Strategy) validate(d Depth) error {
	if !st.Enabled {
		return nil
	}
	switch {
	case st.MinRatePer8hBps <= 0:
		return fmt.Errorf("strategy.min_rate_per_8h_bps must be > 0 when the strategy is enabled")
	case st.PersistencePeriods < 1:
		return fmt.Errorf("strategy.persistence_periods must be >= 1, got %d", st.PersistencePeriods)
	case st.MinNetAPRFrac <= 0:
		return fmt.Errorf("strategy.min_net_apr_frac must be > 0 (a fraction: 0.05 = 5%%)")
	case st.NotionalQuote <= 0:
		return fmt.Errorf("strategy.notional_quote must be > 0 — slippage is a function of size")
	case st.HoldingDays <= 0:
		return fmt.Errorf("strategy.holding_days must be > 0 — the round trip is amortized over it")
	case st.ExitNetAPRFrac >= st.MinNetAPRFrac:
		return fmt.Errorf("strategy.exit_net_apr_frac (%g) must be below min_net_apr_frac (%g), "+
			"or the position churns across the entry threshold paying commission each way",
			st.ExitNetAPRFrac, st.MinNetAPRFrac)
	case st.ExitPersistencePeriods < 0:
		return fmt.Errorf("strategy.exit_persistence_periods must be >= 0, got %d", st.ExitPersistencePeriods)
	case st.MaxBasisPct < 0 || st.MaxBasisWidenPct < 0:
		return fmt.Errorf("strategy basis limits must be >= 0")
	case st.ExitNegativeMinBps < 0:
		return fmt.Errorf("strategy.exit_negative_min_bps must be >= 0 (a depth below zero), got %g", st.ExitNegativeMinBps)
	case st.ExitNegativePeriods < 0:
		return fmt.Errorf("strategy.exit_negative_periods must be >= 0, got %d", st.ExitNegativePeriods)
	case st.ExitNegativeCumCostFrac < 0:
		return fmt.Errorf("strategy.exit_negative_cum_cost_frac must be >= 0 (a fraction of the round trip), got %g", st.ExitNegativeCumCostFrac)
	case st.PerpMarginFrac < 0 || st.PerpMarginFrac > 1:
		return fmt.Errorf("strategy.perp_margin_frac must be in [0,1] — it is collateral as a fraction "+
			"of the perp notional, so 0.1 is 10x; got %g", st.PerpMarginFrac)
	case st.MinLiquidationBufferPct < 0:
		return fmt.Errorf("strategy.min_liquidation_buffer_pct must be >= 0, got %g", st.MinLiquidationBufferPct)
	case st.PerpMarginFrac > 0 && st.MinLiquidationBufferPct == 0:
		return fmt.Errorf("strategy.perp_margin_frac is set but min_liquidation_buffer_pct is 0: the position " +
			"would only leave once the venue had ALREADY liquidated it, which is not a rule")
	case st.MinHoldRecoveredCostFrac < 0:
		return fmt.Errorf("strategy.min_hold_recovered_cost_frac must be >= 0 (a fraction of the round trip), got %g", st.MinHoldRecoveredCostFrac)
	case st.MinTrailingMeanBps < 0:
		return fmt.Errorf("strategy.min_trailing_mean_bps must be >= 0 (a floor on the mean settled rate, bps per 8h), got %g", st.MinTrailingMeanBps)
	case st.TrailingMeanDays < 0:
		return fmt.Errorf("strategy.trailing_mean_days must be >= 0, got %g", st.TrailingMeanDays)
	case st.TrailingMeanDays > MaxTrailingMeanDays:
		return fmt.Errorf("strategy.trailing_mean_days (%g) exceeds %d: cmd/backtest loads 200 days before its "+
			"window and the live path reads the horizon plus a week, so a longer horizon would be judged live and "+
			"refused for coverage in the replay — the drift the 3.5 gate exists to catch", st.TrailingMeanDays, MaxTrailingMeanDays)
	case st.MinTrailingMeanBps > 0 && st.TrailingMeanDays <= 0:
		return fmt.Errorf("strategy.min_trailing_mean_bps is set but trailing_mean_days is 0: a floor on a mean " +
			"needs the horizon the mean is taken over")
	case st.TrailingMeanMinCostFrac < 0:
		return fmt.Errorf("strategy.trailing_mean_min_cost_frac must be >= 0 (a fraction of the round trip), got %g", st.TrailingMeanMinCostFrac)
	case st.TrailingMeanMinCostFrac > 0 && st.TrailingMeanDays <= 0:
		return fmt.Errorf("strategy.trailing_mean_min_cost_frac is set but trailing_mean_days is 0: the crossing is " +
			"tested on a mean, and a mean needs the horizon it is taken over")
	case d.Enabled && st.MaxBookAgeMin < d.RefreshEveryMin:
		return fmt.Errorf("strategy.max_book_age_min (%d) is below depth.refresh_every_min (%d): every fill "+
			"would be refused as stale before the next sweep", st.MaxBookAgeMin, d.RefreshEveryMin)
	}
	return nil
}

// MaxTrailingMeanDays bounds the selection horizon so that BOTH readers of
// the rule can cover it: cmd/backtest loads fundingHistoryLookbackDays (200)
// before its window and cmd/scanner's live path reads the horizon plus a
// week of slack per tick. A horizon one side can see and the other cannot
// is a decision the two sides make on different histories, which is the
// exact drift the step-3.5 gate is built to catch (review of 2026-09-12).
const MaxTrailingMeanDays = 199

// StrategyParams is the strategy.Params this block means — the ONE mapping
// from config to rule parameters. cmd/scanner's live journal and
// cmd/backtest's plain run both go through it, so the step-3.5 gate compares
// two runs of the same numbers by construction rather than by coincidence
// (found by review 2026-09-07: cmd/backtest carried a hand-written copy).
func (st Strategy) StrategyParams() strategy.Params {
	return strategy.Params{
		MinRatePer8hBps: st.MinRatePer8hBps, PersistencePeriods: st.PersistencePeriods,
		MinNetAPRFrac: st.MinNetAPRFrac, NotionalQuote: st.NotionalQuote, HoldingDays: st.HoldingDays,
		MaxBookAge:     time.Duration(st.MaxBookAgeMin) * time.Minute,
		ExitNetAPRFrac: st.ExitNetAPRFrac, ExitPersistencePeriods: st.ExitPersistencePeriods,
		ExitNegativeMinBps: st.ExitNegativeMinBps, ExitNegativePeriods: st.ExitNegativePeriods,
		ExitNegativeCumCostFrac:  st.ExitNegativeCumCostFrac,
		MinHoldRecoveredCostFrac: st.MinHoldRecoveredCostFrac,
		PerpMarginFrac:           st.PerpMarginFrac,
		MinLiquidationBufferPct:  st.MinLiquidationBufferPct,
		MaxBasisPct:              st.MaxBasisPct, MaxBasisWidenPct: st.MaxBasisWidenPct,
		MinTrailingMeanBps: st.MinTrailingMeanBps, TrailingMeanDays: st.TrailingMeanDays,
		TrailingMeanMinCostFrac: st.TrailingMeanMinCostFrac,
	}
}

// CheapestVerifiedSpot picks which of several valid spot legs a perp is hedged
// against, and explains the choice when there was one to make.
//
// This is THE rule, used by cmd/scanner (the dashboard's hedge column and the
// live signal path) and by cmd/backtest alike. Step 3.5 compares the two
// paths' positions, and if each command chose its own leg the comparison would
// be between different trades. Cheapest VERIFIED taker fee wins, because the
// only number derived from the choice is fee-based and an unverified schedule
// produces no number at all; with none verified the first candidate stands, in
// the mapping's deterministic order.
func (c Config) CheapestVerifiedSpot(candidates []string) (source, noteVI string) {
	if len(candidates) == 0 {
		return "", ""
	}
	// Ties resolve by CONFIG order, never by the order the caller handed the
	// candidates over in: cmd/scanner takes them from the hedge mapping and
	// cmd/backtest from its own loop, and the step-3.5 gate compares the two
	// commands' positions leg for leg. Sorting first (stably) makes the strict
	// "<" below keep the earlier-configured leg on equal fees.
	ordered := append([]string(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return c.sourceIndex(ordered[i]) < c.sourceIndex(ordered[j])
	})
	best := ordered[0]
	bestFee, bestVerified := c.takerFee(best)
	ties := 0
	for _, candidate := range ordered[1:] {
		fee, verified := c.takerFee(candidate)
		switch {
		case verified && !bestVerified:
		case verified == bestVerified && fee < bestFee:
		default:
			if verified == bestVerified && fee == bestFee {
				ties++
			}
			continue
		}
		best, bestFee, bestVerified, ties = candidate, fee, verified, 0
	}
	if len(candidates) == 1 {
		return best, ""
	}
	if ties > 0 {
		return best, fmt.Sprintf("Chọn %s trong %d chân spot ghép được (phí taker thấp nhất đã xác minh; "+
			"%d chân khác hoà phí, lấy theo thứ tự trong config).", best, len(candidates), ties)
	}
	return best, fmt.Sprintf("Chọn %s trong %d chân spot ghép được (phí taker thấp nhất đã xác minh).",
		best, len(candidates))
}

// sourceIndex is a source's position in the config, the one order the file
// documents; an unknown name sorts last.
func (c Config) sourceIndex(source string) int {
	for i, s := range c.Sources {
		if s.Source == source {
			return i
		}
	}
	return len(c.Sources)
}

func (c Config) takerFee(source string) (bps float64, verified bool) {
	if s, ok := c.SourceByName(source); ok {
		return s.Fee.TakerBps, s.Fee.Verified
	}
	return 0, false
}
