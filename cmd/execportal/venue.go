package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
	bybitbroker "futures-arbitrage-scanner/internal/broker/bybit"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/risk"
)

// The two testnet markets, and the reads the portal makes of them — on Binance
// (the default) or on Bybit (-broker=bybit, PLAN 4.5j).
//
// The shape of every read here is cmd/execcheck's, which is what the 4.4b/4.5
// acceptance exercised on the venue: rules and books from the market being sent
// to, read at the moment of the decision, and a maintenance bracket read with
// the key rather than assumed.

var (
	futuresEnv = []broker.EnvPair{
		{KeyVar: "BINANCE_FUTURES_TESTNET_API_KEY", SecretVar: "BINANCE_FUTURES_TESTNET_API_SECRET"},
		{KeyVar: "BINANCE_TESTNET_API_KEY", SecretVar: "BINANCE_TESTNET_API_SECRET"},
	}
	spotEnv = []broker.EnvPair{
		{KeyVar: "BINANCE_SPOT_TESTNET_API_KEY", SecretVar: "BINANCE_SPOT_TESTNET_API_SECRET"},
	}
)

// venue is what the portal needs from ONE market: the order interface, the
// rules and book reads every order path makes first, and the transport whose
// clock and weight budget the page reports. *binancebroker.Client and
// bybitVenue (venue_bybit.go) are the implementations that talk to an exchange;
// the tests drive the same code through brokertest's in-memory venue.
//
// The DATA types in these signatures — MarketRules, CommissionRates,
// FundingRate, MaintenanceBracket — are binancebroker's because the portal was
// built on that venue first. They are plain values with no Binance behaviour:
// bybitVenue fills them from Bybit's own answers, and a field Bybit does not
// publish (BuyPriceFloorFrac, a settlement's RateType) is left at its zero value,
// which each field's own comment defines as "not published".
type venue interface {
	broker.Broker
	Market() broker.Market
	HTTP() *broker.Client
	FetchInstrument(ctx context.Context, symbol string) (binancebroker.MarketRules, error)
	FetchDepthBook(ctx context.Context, symbol string) (exchanges.DepthBook, error)
	// CommissionRates is this account's taker fee, which the auto-trader prices
	// its round trip with (PLAN Q18).
	CommissionRates(ctx context.Context, symbol string) (binancebroker.CommissionRates, error)
}

// perpVenue is the futures market: everything a venue does, plus what only a
// perpetual has. MarkPrice and FundingIncome are also the optional capabilities
// execution.Close looks for by type assertion, so they must stay on the value
// handed to it.
type perpVenue interface {
	venue
	broker.MarkPriceReader
	broker.FundingReader
	FetchMaintenanceBracket(ctx context.Context, symbol string, notionalQuote float64) (binancebroker.MaintenanceBracket, error)
	FundingRateHistory(ctx context.Context, symbol string, startMs, endMs int64) ([]binancebroker.FundingRate, error)
}

var (
	_ venue     = (*binancebroker.Client)(nil)
	_ perpVenue = (*binancebroker.Client)(nil)
)

// venueKind is the exchange both legs trade on. Strategy 1 is spot and perp on
// ONE venue, so there is one kind per portal, never a leg on each.
type venueKind string

const (
	venueBinance venueKind = "binance"
	venueBybit   venueKind = "bybit"
)

func parseVenueKind(raw string) (venueKind, error) {
	switch k := venueKind(strings.ToLower(strings.TrimSpace(raw))); k {
	case venueBinance, venueBybit:
		return k, nil
	}
	return "", fmt.Errorf("-broker %q is neither %q nor %q", raw, venueBinance, venueBybit)
}

// venueProfile is everything about the venue the portal SAYS rather than
// computes: hosts, names, the endpoints a note cites, and whether the two
// markets share one wallet.
type venueProfile struct {
	Kind    venueKind
	LabelVI string

	// SpotFallbackBaseURL and PerpFallbackBaseURL are shown for a market that
	// has no client, so the page still names the host it WOULD use.
	SpotFallbackBaseURL string
	PerpFallbackBaseURL string
	AllowedHosts        []string

	// BracketSource and BracketEndpoint name where perpBracket read the
	// maintenance tier; FundingIncomeEndpoint where the settled rows come from.
	BracketSource         string
	BracketEndpoint       string
	FundingIncomeEndpoint string

	// UnifiedWallet is true when spot and perp draw on ONE wallet (Bybit's
	// Unified Trading Account). Both markets then report the same quote
	// balance, and adding the two counts it twice.
	UnifiedWallet bool

	// SpotBuyFeeInBaseCoin is true when this venue keeps a spot BUY's fee in the
	// base coin, so a spot leg's buy fills are counted net of that commission
	// (hedge.go). Bybit always does; Binance does unless fees are paid in BNB,
	// and its spot testnet charges 0, so its profile leaves this false — the
	// mainnet case is a named debt for step 4.6.
	SpotBuyFeeInBaseCoin bool

	// OrdersBlockedVI, when set, refuses EVERY write on this portal — open,
	// close, reconcile and the bot — with this reason. The venue is then shown
	// read-only. No shipped profile sets it since the spot leg is judged by the
	// wallet (PLAN 4.5j, second half); the gate stays for the next venue that
	// needs one.
	OrdersBlockedVI string

	// AutotradeBlockedVI, when set, refuses to START the auto-trader — the
	// start endpoint and -autotrade — while every manual write stays open. Stop,
	// kill and the pair controls still answer, so nothing that is already
	// running can be left without its off switch.
	AutotradeBlockedVI string

	// StateDir is where this venue's intent files live. Separate per venue:
	// an intent's derived ClientOrderIDs mean something only on the venue that
	// received them, and a Binance intent read back from Bybit is "not found",
	// which the portal would take for a position that never existed.
	StateDir string
}

func profileFor(kind venueKind) venueProfile {
	if kind == venueBybit {
		mode, _ := bybitbroker.ResolveMode(os.Getenv("BYBIT_MODE"), os.Getenv("BYBIT_TESTNET"))
		base := broker.BybitTestnetBaseURL
		if mode == bybitbroker.ModeDemo {
			base = broker.BybitDemoBaseURL
		}
		return venueProfile{
			Kind: venueBybit, LabelVI: "Bybit " + strings.ToUpper(string(orDefaultMode(mode))),
			SpotFallbackBaseURL: base, PerpFallbackBaseURL: base,
			AllowedHosts:          broker.TestnetHostsFor(broker.SchemeBybitV5Header),
			BracketSource:         "bybit_linear_" + string(orDefaultMode(mode)),
			BracketEndpoint:       "/v5/market/risk-limit",
			FundingIncomeEndpoint: "/v5/account/transaction-log (type=SETTLEMENT)",
			UnifiedWallet:         true,
			StateDir:              stateDirBybit,
			// A spot BUY's fee is kept in the base coin. execution buys that leg
			// grossed up and judges it by the wallet (PLAN 4.5j, second half),
			// and the hedge status counts its buys net of that commission — which
			// is what lifted the read-only gate this profile shipped with.
			SpotBuyFeeInBaseCoin: true,
			// Review of 4.5j part 2 (B2): manual open and close through the
			// portal on Bybit testnet have passed with verified delta-neutrality
			// and balance reconciliation. Auto-trader on Bybit is unblocked.
			AutotradeBlockedVI: "",
		}
	}
	return venueProfile{
		Kind: venueBinance, LabelVI: "Binance TESTNET",
		SpotFallbackBaseURL: broker.BinanceSpotTestnetBaseURL, PerpFallbackBaseURL: broker.BinanceFuturesTestnetBaseURL,
		AllowedHosts:          broker.TestnetHosts(),
		BracketSource:         "binance_futures_testnet",
		BracketEndpoint:       "/fapi/v1/leverageBracket",
		FundingIncomeEndpoint: "/fapi/v1/income",
		StateDir:              stateDir,
	}
}

func orDefaultMode(m bybitbroker.Mode) bybitbroker.Mode {
	if m == "" {
		return "chưa-đặt-BYBIT_MODE"
	}
	return m
}

// markets holds one client per market. A nil client is a market with no
// credential, and its error says which variables to set — never their values.
type markets struct {
	profile venueProfile

	spot    venue
	perp    perpVenue
	spotErr error
	perpErr error

	// spotSourceVI and perpSourceVI name the environment variables each
	// credential came from. Names only; the values never leave broker.Secret.
	spotSourceVI string
	perpSourceVI string
}

// dialMarkets builds the Binance markets; dialMarketsFor is what main calls.
func dialMarkets() markets { return dialMarketsFor(venueBinance) }

// dialMarketsFor builds whichever clients the environment allows. Unlike
// execcheck it does not stop at the first missing credential: the portal must
// come up and SAY what is missing, so an operator with half a .env sees a page
// rather than a crash.
func dialMarketsFor(kind venueKind) markets {
	if kind == venueBybit {
		return dialBybitMarkets()
	}
	m := markets{profile: profileFor(venueBinance)}
	// Assigned only on success: a nil *Client stored in an interface is a
	// non-nil interface, and "configured" would read true for a market that
	// has no client at all.
	if c, source, err := dialOne(broker.MarketFuturesUSDM, futuresEnv); err != nil {
		m.perpErr = err
	} else {
		m.perp, m.perpSourceVI = c, source
	}
	if c, source, err := dialOne(broker.MarketSpot, spotEnv); err != nil {
		m.spotErr = err
	} else {
		m.spot, m.spotSourceVI = c, source
	}
	return m
}

func dialOne(market broker.Market, env []broker.EnvPair) (*binancebroker.Client, string, error) {
	creds, err := broker.CredentialsFromEnvAny(env...)
	if err != nil {
		return nil, "", err
	}
	cfg, err := binancebroker.DefaultConfig(market, creds)
	if err != nil {
		return nil, "", err
	}
	// broker.NewClient refuses any host outside the documented testnet list
	// and any plaintext URL. The portal takes no flag that could move the host.
	c, err := binancebroker.New(market, cfg)
	if err != nil {
		return nil, "", err
	}
	return c, creds.SourceVI, nil
}

// both reports whether an order can be placed at all.
func (m markets) both() error {
	var errs []error
	if m.spot == nil {
		errs = append(errs, fmt.Errorf("spot testnet: %w", m.spotErr))
	}
	if m.perp == nil {
		errs = append(errs, fmt.Errorf("futures testnet: %w", m.perpErr))
	}
	return errors.Join(errs...)
}

// market is one leg's rules, book and reference price, read from the TESTNET
// that leg trades on.
type market struct {
	Rules binancebroker.MarketRules
	Book  depth.Summary
	// PriceQuote is the book's mid, the reference execution sizes against.
	PriceQuote float64
}

func readMarket(ctx context.Context, c venue, symbol string) (market, error) {
	var out market
	rules, err := c.FetchInstrument(ctx, symbol)
	if err != nil {
		return out, fmt.Errorf("rules: %w", err)
	}
	out.Rules = rules
	if out.Book, err = readBook(ctx, c, symbol); err != nil {
		return out, err
	}
	out.PriceQuote = out.Book.MidPriceQuote
	return out, nil
}

// readBook reads one market's book and summarizes it.
func readBook(ctx context.Context, c venue, symbol string) (depth.Summary, error) {
	book, err := c.FetchDepthBook(ctx, symbol)
	if err != nil {
		return depth.Summary{}, fmt.Errorf("book: %w", err)
	}
	// Stamped by US at the read, never from the venue's clock (CLAUDE.md rule
	// 13). Binance denominates both books in COIN, so the multiplier is 1 and
	// no other is offered.
	summary := depth.Summarize(book, time.Now().UnixMilli(), func(string, string) (float64, bool) { return 1, true })
	if !summary.OK() {
		return summary, fmt.Errorf("book: %s", summary.ErrVI)
	}
	return summary, nil
}

// sameAsset refuses a pair whose two legs are not the same coin in the same
// quote, as each VENUE declares it.
//
// On Binance and on Bybit the two markets share a symbol string, and execcheck
// relies on that. The portal checks it anyway: the base and quote must come from what
// the venue declares, never from the symbol string (CLAUDE.md's assets trap),
// and a symbol that meant different things on the two markets would open a
// "hedge" in two different coins.
func sameAsset(spot, perp binancebroker.MarketRules) error {
	if spot.BaseAsset == "" || perp.BaseAsset == "" {
		return fmt.Errorf("sàn không khai báo tài sản gốc (spot %q, perp %q)", spot.BaseAsset, perp.BaseAsset)
	}
	if spot.BaseAsset != perp.BaseAsset || spot.QuoteAsset != perp.QuoteAsset {
		return fmt.Errorf("hai chân không cùng tài sản: spot %s/%s, perp %s/%s",
			spot.BaseAsset, spot.QuoteAsset, perp.BaseAsset, perp.QuoteAsset)
	}
	return nil
}

// perpBracket reads the maintenance tier that covers this notional, with the
// key. An unverified schedule is REFUSED by risk.Evaluate inside execution, so
// this read is the difference between a position and a refusal.
func perpBracket(ctx context.Context, c perpVenue, profile venueProfile, symbol string, notionalQuote float64) (risk.Bracket, error) {
	mb, err := c.FetchMaintenanceBracket(ctx, symbol, notionalQuote)
	if err != nil {
		return risk.Bracket{}, err
	}
	return risk.Bracket{
		Source:                profile.BracketSource,
		MaintenanceMarginFrac: mb.MaintMarginFrac,
		TierCeilingQuote:      mb.NotionalCapQuote,
		MaxLeverage:           mb.MaxLeverage,
		Verified:              true,
		NoteVI: fmt.Sprintf("đọc từ %s của %s ngày %s: bậc %d, sàn %.0f → trần %.0f, tỷ lệ duy trì %.4f%%, đòn bẩy tối đa %.0fx",
			profile.BracketEndpoint, profile.LabelVI, time.Now().Format("2006-01-02"), mb.Tier, mb.NotionalFloorQuote, mb.NotionalCapQuote, mb.MaintMarginFrac*100, mb.MaxLeverage),
	}, nil
}

// smallestWorkableNotionalQuote is execcheck's: the smallest size that clears
// BOTH markets' published minimums with two steps of headroom, because
// rounding the quantity DOWN can drop a notional back under the minimum it
// just cleared. It is a HINT for the form; the venue's minimum is enforced by
// execution, before anything is sent, on the rounded size.
func smallestWorkableNotionalQuote(spot, perp binancebroker.MarketRules, priceQuote float64) float64 {
	need := math.Max(spot.MinNotionalQuote, perp.MinNotionalQuote)
	step := math.Max(spot.StepSizeCoin, perp.StepSizeCoin)
	minQty := math.Max(spot.MinQtyCoin, perp.MinQtyCoin)
	return math.Max(need, minQty*priceQuote) + 2*step*priceQuote
}

// readBudgetFrac is the share of each market's per-minute request weight the
// page's READS may spend. The rest belongs to orders.
//
// The two draw on ONE budget (broker.WeightBudget, which adopts the venue's own
// X-MBX-USED-WEIGHT count for the whole IP), and a budget that is full makes
// Reserve WAIT for the next minute. An open whose read-back, cancel or unwind
// waited a minute behind the page's own polling would stretch a 0.4 s
// unhedged window into tens of seconds. So reads stop at half, and orders
// never check this.
const readBudgetFrac = 0.5

func (m markets) readBudgetErrorFor(c venue) error {
	if c == nil {
		return nil
	}
	budget := c.HTTP().Budget()
	if banned, _ := budget.Banned(); banned {
		return fmt.Errorf("%s: sàn đang cấm IP (HTTP 418) — không đọc gì thêm", c.Market())
	}
	if used, limit := budget.UsedThisWindow(), budget.LimitPerMin(); float64(used) >= readBudgetFrac*float64(limit) {
		return fmt.Errorf("%s đã dùng %d/%d weight trong phút này — trang tạm ngừng ĐỌC để phần còn lại dành cho lệnh", c.Market(), used, limit)
	}
	return nil
}

func (m markets) readBudgetError() error {
	for _, c := range []venue{m.spot, m.perp} {
		if err := m.readBudgetErrorFor(c); err != nil {
			return err
		}
	}
	return nil
}

// slippageBps is how far a fill landed from the touch, POSITIVE when worse.
func slippageBps(fillQuote, touchQuote float64, buy bool) (float64, bool) {
	if fillQuote <= 0 || touchQuote <= 0 {
		return 0, false
	}
	d := (fillQuote - touchQuote) / touchQuote * 10_000
	if !buy {
		d = -d
	}
	return d, true
}

// ------------------------------------------------------------------ caching

// ttlCache shares one venue read between the endpoints and browser tabs that
// ask for it inside ttl, and between concurrent callers while it is in flight.
//
// It exists for the request-weight budget, not for speed. The page polls every
// three seconds and /api/account and /api/positions both need the spot account
// (weight 20 each); two tabs would otherwise read it four times where once
// answers all of them. A cached figure always travels with the instant it was
// read (read_at_ms), because a position figure without its age is a claim about
// now that the venue never made.
type ttlCache[T any] struct {
	now       func() time.Time
	entriesMu sync.Mutex
	entries   map[string]*cacheEntry[T]
}

type cacheEntry[T any] struct {
	done   chan struct{}
	value  T
	err    error
	readAt time.Time
}

func newTTLCache[T any](now func() time.Time) *ttlCache[T] {
	return &ttlCache[T]{now: now, entries: map[string]*cacheEntry[T]{}}
}

// errorTTL is how long a FAILED read is shared. Short, so a venue that comes
// back is seen at once; not zero, so a venue that keeps failing is not asked
// again on every poll of every tab — a failed exchangeInfo still costs weight 20.
const errorTTL = 5 * time.Second

// get returns the cached value for key when it is younger than ttl (errorTTL
// for a failed read); otherwise it loads it once, whoever else is asking. The
// load runs WITHOUT the lock held (CONVENTIONS §9: no I/O under a lock).
func (c *ttlCache[T]) get(key string, ttl time.Duration, load func() (T, error)) (T, time.Time, error) {
	c.entriesMu.Lock()
	if e, ok := c.entries[key]; ok {
		select {
		case <-e.done:
			fresh := ttl
			if e.err != nil {
				fresh = min(ttl, errorTTL)
			}
			if c.now().Sub(e.readAt) < fresh {
				c.entriesMu.Unlock()
				return e.value, e.readAt, e.err
			}
		default:
			c.entriesMu.Unlock()
			<-e.done
			return e.value, e.readAt, e.err
		}
	}
	e := &cacheEntry[T]{done: make(chan struct{})}
	c.entries[key] = e
	c.entriesMu.Unlock()

	func() {
		// A load that panics must still release its waiters, or every later
		// caller on this key blocks forever and the page's connections — the
		// same ones Close and Reconcile need — pile up behind it.
		defer func() {
			if r := recover(); r != nil {
				e.err = fmt.Errorf("đọc sàn bị panic: %v", r)
			}
			e.readAt = c.now()
			close(e.done)
		}()
		e.value, e.err = load()
	}()
	return e.value, e.readAt, e.err
}

// forget drops one key. A load already in flight still answers the callers
// waiting on it.
func (c *ttlCache[T]) forget(key string) {
	c.entriesMu.Lock()
	delete(c.entries, key)
	c.entriesMu.Unlock()
}

// invalidate forgets everything, so the next read after an order goes to the
// venue. A load already in flight still answers the callers waiting on it.
func (c *ttlCache[T]) invalidate() {
	c.entriesMu.Lock()
	c.entries = map[string]*cacheEntry[T]{}
	c.entriesMu.Unlock()
}
