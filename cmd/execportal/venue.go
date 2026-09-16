package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/risk"
)

// The two testnet markets, and the reads the portal makes of them.
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
// clock and weight budget the page reports. *binancebroker.Client is the only
// implementation that talks to an exchange; the tests drive the same code
// through brokertest's in-memory venue.
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

// markets holds one client per market. A nil client is a market with no
// credential, and its error says which variables to set — never their values.
type markets struct {
	spot    venue
	perp    perpVenue
	spotErr error
	perpErr error

	// spotSourceVI and perpSourceVI name the environment variables each
	// credential came from. Names only; the values never leave broker.Secret.
	spotSourceVI string
	perpSourceVI string
}

// dialMarkets builds whichever clients the environment allows. Unlike
// execcheck it does not stop at the first missing credential: the portal must
// come up and SAY what is missing, so an operator with half a .env sees a page
// rather than a crash.
func dialMarkets() markets {
	var m markets
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
// On Binance the two markets share a symbol string, and execcheck relies on
// that. The portal checks it anyway: the base and quote must come from what
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
func perpBracket(ctx context.Context, c perpVenue, symbol string, notionalQuote float64) (risk.Bracket, error) {
	mb, err := c.FetchMaintenanceBracket(ctx, symbol, notionalQuote)
	if err != nil {
		return risk.Bracket{}, err
	}
	return risk.Bracket{
		Source:                "binance_futures_testnet",
		MaintenanceMarginFrac: mb.MaintMarginFrac,
		TierCeilingQuote:      mb.NotionalCapQuote,
		MaxLeverage:           mb.MaxLeverage,
		Verified:              true,
		NoteVI: fmt.Sprintf("đọc từ /fapi/v1/leverageBracket của testnet ngày %s: bậc %d, sàn %.0f → trần %.0f, tỷ lệ duy trì %.4f%%, đòn bẩy tối đa %.0fx",
			time.Now().Format("2006-01-02"), mb.Tier, mb.NotionalFloorQuote, mb.NotionalCapQuote, mb.MaintMarginFrac*100, mb.MaxLeverage),
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

func (m markets) readBudgetError() error {
	for _, c := range []venue{m.spot, m.perp} {
		if c == nil {
			continue
		}
		budget := c.HTTP().Budget()
		if banned, _ := budget.Banned(); banned {
			return fmt.Errorf("%s: sàn đang cấm IP (HTTP 418) — không đọc gì thêm", c.Market())
		}
		if used, limit := budget.UsedThisWindow(), budget.LimitPerMin(); float64(used) >= readBudgetFrac*float64(limit) {
			return fmt.Errorf("%s đã dùng %d/%d weight trong phút này — trang tạm ngừng ĐỌC để phần còn lại dành cho lệnh", c.Market(), used, limit)
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
