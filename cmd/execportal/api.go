package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"futures-arbitrage-scanner/cmd/execportal/autotrade"
	"futures-arbitrage-scanner/cmd/execportal/feeds"
	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
	"futures-arbitrage-scanner/internal/execution"
)

// The REST API. Every body is JSON; every failure carries error_vi.
//
// # What is read from where (CLAUDE.md rule 7)
//
// Balances, positions, orders and funding come from the VENUE on every refresh
// (a couple of seconds of sharing, never more, and always with read_at_ms).
// The intent files are a cache and are labelled as one wherever they surface.
//
// # What the figures are (CLAUDE.md rule 2)
//
// Nothing here is called net and no field is named profit_*. realized_quote is
// execution's: funding the venue paid, minus the commission it charged in the
// quote asset, minus slippage against the decision's reference mids. The pair's
// price drift, commission in another asset and the cost of capital are reported
// BESIDE it. The perp position's unrealized figure is the venue's, gross.

const (
	// maxNotionalQuote is the portal's own ceiling per leg. It is a SAFETY
	// limit on a form, not a venue fact: the testnet minimums are read from
	// the venue and enforced by execution before anything is sent.
	maxNotionalQuote = 50_000.0

	readTimeout = 20 * time.Second

	accountTTL   = 2 * time.Second
	clockEvery   = 30 * time.Second
	positionsTTL = 2 * time.Second
	ordersTTL    = 5 * time.Second
	fundingTTL   = 60 * time.Second
	marketTTL    = 15 * time.Second
	rulesTTL     = 10 * time.Minute

	// fundingLookback is how far back /api/funding asks. Seven days is the
	// window the venue returns when asked for none, so asking for exactly that
	// cannot run into a limit on the width of the window.
	fundingLookback = 7 * 24 * time.Hour
)

// symbolPattern bounds a symbol before it reaches a request parameter or an
// intent id; the allow-list decides which ones may trade.
var symbolPattern = regexp.MustCompile(`^[A-Z0-9]{5,20}$`)

// execSettings are the execution parameters every open uses, from flags.
type execSettings struct {
	MarginFrac     float64
	MaxSlippageBps float64
	LegTimeout     time.Duration
	ActionTimeout  time.Duration
}

type portal struct {
	markets  markets
	stateDir string
	symbols  []string
	hosts    map[string]bool
	listen   string
	now      func() time.Time
	exec     execSettings

	startedAt time.Time

	// writeMu is held for the whole of one open, close or reconcile. A second
	// write while one runs is REFUSED rather than queued: a double-click that
	// waited its turn would still open a second position.
	writeMu sync.Mutex

	busyMu      sync.Mutex
	busyAction  string
	busySinceMs int64

	accounts   *ttlCache[accountView]
	positions  *ttlCache[positionsView]
	orders     *ttlCache[ordersView]
	funding    *ttlCache[fundingView]
	marketInfo *ttlCache[marketView]
	rules      *ttlCache[rulesPair]
	memo       *doneOrders

	// pingsMs is each market's last clock round trip, in milliseconds.
	pingsMu sync.Mutex
	pingsMs map[broker.Market]int64

	// feeds are the read-only scanner and paper-ledger feeds (package feeds,
	// PLAN Q17). All the portal can do with them is register their handlers,
	// report their health and close them; nothing here can read what they carry.
	feeds *feeds.Feeds

	// autotrade is the testnet auto-trader (PLAN Q18, autotrade.go). It decides
	// on the testnet's own funding, books and fees — never on a feed — and
	// trades only through openAs and close under writeMu.
	autotrade    *autotrade.Engine
	fundingRates *ttlCache[[]binancebroker.FundingRate]
	commissions  *ttlCache[commissionPair]
	fundingKeyMu sync.Mutex
	fundingKey   string
}

func newPortal(m markets, symbols []string, bindIP, port string, settings execSettings, now func() time.Time) *portal {
	if now == nil {
		now = time.Now
	}
	p := &portal{
		markets:    m,
		stateDir:   stateDir,
		symbols:    symbols,
		hosts:      allowedHosts(bindIP, port),
		listen:     bindIP + ":" + port,
		now:        now,
		exec:       settings,
		startedAt:  now(),
		accounts:   newTTLCache[accountView](now),
		positions:  newTTLCache[positionsView](now),
		orders:     newTTLCache[ordersView](now),
		funding:    newTTLCache[fundingView](now),
		marketInfo: newTTLCache[marketView](now),
		rules:      newTTLCache[rulesPair](now),
		memo:       newDoneOrders(),
		pingsMs:    map[broker.Market]int64{},
		feeds:      feeds.New("", "", now),

		fundingRates: newTTLCache[[]binancebroker.FundingRate](now),
		commissions:  newTTLCache[commissionPair](now),
	}
	p.autotrade = newAutotrade(p)
	return p
}

// acquire takes the write lock or reports what holds it.
func (p *portal) acquire(action string) (release func(), heldBy string, heldSinceMs int64, ok bool) {
	if !p.writeMu.TryLock() {
		p.busyMu.Lock()
		defer p.busyMu.Unlock()
		return nil, p.busyAction, p.busySinceMs, false
	}
	p.busyMu.Lock()
	p.busyAction, p.busySinceMs = action, p.now().UnixMilli()
	p.busyMu.Unlock()
	return func() {
		p.busyMu.Lock()
		p.busyAction, p.busySinceMs = "", 0
		p.busyMu.Unlock()
		p.writeMu.Unlock()
	}, "", 0, true
}

// invalidateVenueReads makes the next poll after an order ask the venue.
func (p *portal) invalidateVenueReads() {
	p.memo.forget()
	p.accounts.invalidate()
	p.positions.invalidate()
	p.orders.invalidate()
	p.funding.invalidate()
	p.marketInfo.invalidate()
}

// readContext detaches a venue read from the request that triggered it: the
// read is shared with every caller waiting on the cache, and one closed tab
// must not cancel it for all of them.
func readContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), readTimeout)
}

// ------------------------------------------------------------------ helpers

type errorBody struct {
	ErrorCode string `json:"error_code"`
	ErrorVI   string `json:"error_vi"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, messageVI string) {
	writeJSON(w, status, errorBody{ErrorCode: code, ErrorVI: messageVI})
}

// decodeBody reads exactly one JSON object with no unknown fields. A typo in a
// field name of an ORDER request must be an error, not a default.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "thân yêu cầu quá lớn")
			return false
		}
		writeError(w, http.StatusBadRequest, "bad_json", "thân yêu cầu không phải JSON hợp lệ: "+err.Error())
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_json", "thân yêu cầu chứa nhiều hơn một giá trị JSON")
		return false
	}
	return true
}

// allowedSymbol normalizes and checks one symbol against the allow-list.
func (p *portal) allowedSymbol(raw string) (string, error) {
	symbol := strings.ToUpper(strings.TrimSpace(raw))
	if !symbolPattern.MatchString(symbol) {
		return "", fmt.Errorf("symbol %s không hợp lệ", quoteForMessage(raw))
	}
	for _, s := range p.symbols {
		if s == symbol {
			return symbol, nil
		}
	}
	return "", fmt.Errorf("symbol %s không nằm trong danh sách được phép %v (cờ -symbols)", symbol, p.symbols)
}

func (p *portal) symbolQuery(w http.ResponseWriter, r *http.Request) (string, bool) {
	symbol, err := p.allowedSymbol(r.URL.Query().Get("symbol"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_symbol", err.Error())
		return "", false
	}
	return symbol, true
}

func bpsPtr(v float64, ok bool) *float64 {
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func hostOf(c venue, fallbackBaseURL string) string {
	base := fallbackBaseURL
	if c != nil {
		base = c.HTTP().BaseURL()
	}
	return strings.TrimPrefix(base, "https://")
}

// rulesPair is one symbol's rules on both markets, read from the testnets.
type rulesPair struct {
	Spot binancebroker.MarketRules
	Perp binancebroker.MarketRules
}

func (r rulesPair) toleranceQtyCoin() float64 {
	return math.Max(r.Spot.StepSizeCoin, r.Perp.StepSizeCoin)
}

// rulesFor reads the exchangeInfo of both markets, shared for ten minutes. The
// spot call costs weight 20, and a step size does not change between two polls;
// every ORDER path reads the rules fresh instead (readMarket).
func (p *portal) rulesFor(ctx context.Context, symbol string) (rulesPair, error) {
	if err := p.markets.both(); err != nil {
		return rulesPair{}, err
	}
	v, _, err := p.rules.get(symbol, rulesTTL, func() (rulesPair, error) {
		if err := p.markets.readBudgetError(); err != nil {
			return rulesPair{}, err
		}
		spot, err := p.markets.spot.FetchInstrument(ctx, symbol)
		if err != nil {
			return rulesPair{}, fmt.Errorf("luật spot: %w", err)
		}
		perp, err := p.markets.perp.FetchInstrument(ctx, symbol)
		if err != nil {
			return rulesPair{}, fmt.Errorf("luật perp: %w", err)
		}
		return rulesPair{Spot: spot, Perp: perp}, nil
	})
	return v, err
}

// ------------------------------------------------------------------- status

type marketStatus struct {
	Configured   bool   `json:"configured"`
	Host         string `json:"host"`
	CredentialVI string `json:"credential_vi"`
}

type statusView struct {
	Mode             string       `json:"mode"`
	NowMs            int64        `json:"now_ms"`
	StartedAtMs      int64        `json:"started_at_ms"`
	UptimeSec        int64        `json:"uptime_sec"`
	Listen           string       `json:"listen"`
	TestnetHosts     []string     `json:"testnet_hosts"`
	Symbols          []string     `json:"symbols"`
	Spot             marketStatus `json:"spot"`
	Futures          marketStatus `json:"futures"`
	Busy             bool         `json:"busy"`
	BusyAction       string       `json:"busy_action"`
	BusySinceMs      int64        `json:"busy_since_ms"`
	StateDir         string       `json:"state_dir"`
	MaxNotionalQuote float64      `json:"max_notional_quote"`
	MarginFrac       float64      `json:"perp_margin_frac"`
	MaxSlippageBps   float64      `json:"max_slippage_bps"`
	LegTimeoutMs     int64        `json:"leg_timeout_ms"`
	LegOrders        []string     `json:"leg_orders"`
	Feeds            feeds.View   `json:"feeds"`
	NoticeVI         string       `json:"notice_vi"`
}

func (p *portal) handleStatus(w http.ResponseWriter, r *http.Request) {
	now := p.now()
	p.busyMu.Lock()
	busyAction, busySince := p.busyAction, p.busySinceMs
	p.busyMu.Unlock()
	writeJSON(w, http.StatusOK, statusView{
		Mode:  "testnet_demo",
		NowMs: now.UnixMilli(), StartedAtMs: p.startedAt.UnixMilli(),
		UptimeSec:        int64(now.Sub(p.startedAt).Seconds()),
		Listen:           p.listen,
		TestnetHosts:     broker.TestnetHosts(),
		Symbols:          p.symbols,
		Spot:             p.marketStatus(p.markets.spot, p.markets.spotErr, p.markets.spotSourceVI, broker.BinanceSpotTestnetBaseURL),
		Futures:          p.marketStatus(p.markets.perp, p.markets.perpErr, p.markets.perpSourceVI, broker.BinanceFuturesTestnetBaseURL),
		Busy:             busyAction != "",
		BusyAction:       busyAction,
		BusySinceMs:      busySince,
		StateDir:         p.stateDir,
		MaxNotionalQuote: maxNotionalQuote,
		MarginFrac:       p.exec.MarginFrac,
		MaxSlippageBps:   p.exec.MaxSlippageBps,
		LegTimeoutMs:     p.exec.LegTimeout.Milliseconds(),
		LegOrders:        []string{string(execution.LegOrderSequentialSpotFirst), string(execution.LegOrderParallel)},
		Feeds:            p.feeds.View(),
		NoticeVI: "CHỈ TESTNET — không tiền thật. Vị thế do người vận hành bấm, hoặc do Auto-Trader khi người vận hành BẬT nó (PLAN Q18); " +
			"không tín hiệu nào từ scanner hay nhật ký cổng 3.5 tới được lệnh.",
	})
}

func (p *portal) marketStatus(c venue, err error, sourceVI, fallbackBaseURL string) marketStatus {
	s := marketStatus{Configured: c != nil, Host: hostOf(c, fallbackBaseURL), CredentialVI: sourceVI}
	if c == nil && err != nil {
		s.CredentialVI = err.Error()
	}
	return s
}

// ------------------------------------------------------------------ account

type balanceView struct {
	Asset            string  `json:"asset"`
	FreeQtyInAsset   float64 `json:"free_qty_in_asset"`
	LockedQtyInAsset float64 `json:"locked_qty_in_asset"`
	TotalQtyInAsset  float64 `json:"total_qty_in_asset"`
}

type marketAccountView struct {
	Market            string        `json:"market"`
	Configured        bool          `json:"configured"`
	Host              string        `json:"host"`
	PingMs            *int64        `json:"ping_ms"`
	ClockSkewMs       *int64        `json:"clock_skew_ms"`
	ClockMeasuredAtMs int64         `json:"clock_measured_at_ms"`
	RecvWindowMs      int64         `json:"recv_window_ms"`
	WeightUsed1m      int           `json:"weight_used_1m"`
	WeightLimit1m     int           `json:"weight_limit_1m"`
	Banned            bool          `json:"banned"`
	BannedUntilMs     int64         `json:"banned_until_ms"`
	Balances          []balanceView `json:"balances"`
	ErrorVI           string        `json:"error_vi"`
}

type accountView struct {
	ReadAtMs int64             `json:"read_at_ms"`
	Symbol   string            `json:"symbol"`
	Spot     marketAccountView `json:"spot"`
	Futures  marketAccountView `json:"futures"`
}

func (p *portal) handleAccount(w http.ResponseWriter, r *http.Request) {
	symbol := ""
	if r.URL.Query().Has("symbol") {
		s, ok := p.symbolQuery(w, r)
		if !ok {
			return
		}
		symbol = s
	}
	ctx, cancel := readContext(r)
	defer cancel()
	writeJSON(w, http.StatusOK, p.accountFor(ctx, symbol))
}

// accountFor is shared by /api/account and /api/positions, which both need the
// spot account and would otherwise each pay weight 20 for it.
func (p *portal) accountFor(ctx context.Context, symbol string) accountView {
	v, _, _ := p.accounts.get("account|"+symbol, accountTTL, func() (accountView, error) {
		assets := []string{"USDT", "BTC"}
		if symbol != "" {
			if rules, err := p.rulesFor(ctx, symbol); err == nil {
				// The venue's declared assets, never a slice of the symbol.
				assets = appendUnique(assets, rules.Spot.QuoteAsset, rules.Spot.BaseAsset)
			}
		}
		out := accountView{Symbol: symbol}
		budgetErr := p.markets.readBudgetError()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			out.Spot = p.readMarketAccount(ctx, p.markets.spot, p.markets.spotErr, broker.MarketSpot, broker.BinanceSpotTestnetBaseURL, assets, budgetErr)
		}()
		go func() {
			defer wg.Done()
			out.Futures = p.readMarketAccount(ctx, p.markets.perp, p.markets.perpErr, broker.MarketFuturesUSDM, broker.BinanceFuturesTestnetBaseURL, assets, budgetErr)
		}()
		wg.Wait()
		out.ReadAtMs = p.now().UnixMilli()
		return out, nil
	})
	return v
}

// readMarketAccount reads one market's clock and balances. With budgetErr set
// it reads NOTHING from the venue and reports only the weight tally, so the
// operator can see why the tiles stopped moving.
func (p *portal) readMarketAccount(ctx context.Context, c venue, dialErr error, market broker.Market, fallbackBaseURL string, assets []string, budgetErr error) marketAccountView {
	v := marketAccountView{Market: string(market), Configured: c != nil, Host: hostOf(c, fallbackBaseURL)}
	if c == nil {
		v.ErrorVI = "chưa cấu hình credential: " + errString(dialErr)
		return v
	}
	h := c.HTTP()
	var problems []string

	if budgetErr != nil {
		problems = append(problems, budgetErr.Error())
	} else {
		// The ping IS the clock read: one weight-1 public call, timed around
		// the round trip. It is taken every clockEvery, not on every poll:
		// SyncClock REPLACES the skew every signed request is corrected by,
		// and one lopsided round trip measured under an order being signed is
		// how a -1021 happens. Between measurements the last one is shown,
		// with its age.
		p.pingsMu.Lock()
		_, pinged := p.pingsMs[market]
		p.pingsMu.Unlock()
		// A signed call elsewhere may already have measured the clock, which
		// leaves no ping to show; the first account read measures its own.
		if at := h.ClockMeasuredAt(); at.IsZero() || time.Since(at) >= clockEvery || !pinged {
			sentAt := time.Now()
			if _, err := h.SyncClock(ctx); err != nil {
				problems = append(problems, "đồng hồ sàn: "+err.Error())
			} else {
				p.pingsMu.Lock()
				p.pingsMs[market] = time.Since(sentAt).Milliseconds()
				p.pingsMu.Unlock()
			}
		}
		if !h.ClockMeasuredAt().IsZero() {
			skewMs := h.ClockSkewMs()
			v.ClockSkewMs = &skewMs
			p.pingsMu.Lock()
			if pingMs, ok := p.pingsMs[market]; ok {
				v.PingMs = &pingMs
			}
			p.pingsMu.Unlock()
		}
		if balances, err := c.GetBalance(ctx, market); err != nil {
			problems = append(problems, "số dư: "+err.Error())
		} else {
			v.Balances = pickBalances(balances, assets)
		}
	}
	if at := h.ClockMeasuredAt(); !at.IsZero() {
		v.ClockMeasuredAtMs = at.UnixMilli()
	}
	v.RecvWindowMs = h.RecvWindowMs()

	budget := h.Budget()
	v.WeightUsed1m, v.WeightLimit1m = budget.UsedThisWindow(), budget.LimitPerMin()
	if banned, until := budget.Banned(); banned {
		v.Banned = true
		if !until.IsZero() {
			v.BannedUntilMs = until.UnixMilli()
		}
	}
	v.ErrorVI = strings.Join(problems, " · ")
	return v
}

// pickBalances keeps the named assets, in the order named. An asset the venue
// did not list is left out rather than shown as zero: absent is not empty.
func pickBalances(balances []broker.Balance, assets []string) []balanceView {
	byAsset := map[string]broker.Balance{}
	for _, b := range balances {
		byAsset[b.Asset] = b
	}
	var out []balanceView
	for _, a := range assets {
		if b, ok := byAsset[a]; ok {
			out = append(out, balanceView{Asset: a, FreeQtyInAsset: b.FreeQtyCoin, LockedQtyInAsset: b.LockedQtyCoin, TotalQtyInAsset: b.TotalQtyCoin()})
		}
	}
	return out
}

func appendUnique(list []string, more ...string) []string {
	for _, m := range more {
		if m == "" {
			continue
		}
		seen := false
		for _, l := range list {
			if l == m {
				seen = true
				break
			}
		}
		if !seen {
			list = append(list, m)
		}
	}
	return list
}

func errString(err error) string {
	if err == nil {
		return "không rõ"
	}
	return err.Error()
}

// ---------------------------------------------------------------- positions

type intentHedgeView struct {
	intentHedge
	ResidualCoin float64     `json:"residual_coin"`
	Status       hedgeStatus `json:"status"`
	ReasonVI     string      `json:"reason_vi"`
}

type positionsView struct {
	Symbol   string      `json:"symbol"`
	ReadAtMs int64       `json:"read_at_ms"`
	Status   hedgeStatus `json:"status"`
	StatusVI string      `json:"status_vi"`
	ReasonVI string      `json:"reason_vi"`

	// SpotQtyCoin is what this strategy's spot leg holds, summed over the
	// tracked intents' own orders at the venue. PerpQtyCoin is the venue's own
	// signed position. DeltaResidualCoin is their sum — spot minus |perp| for a
	// short.
	SpotQtyCoin        float64 `json:"spot_qty_coin"`
	PerpQtyCoin        float64 `json:"perp_qty_coin"`
	IntentsPerpQtyCoin float64 `json:"intents_perp_qty_coin"`
	DeltaResidualCoin  float64 `json:"delta_residual_coin"`
	ToleranceQtyCoin   float64 `json:"tolerance_qty_coin"`

	// SpotBaseBalanceQtyCoin is the whole spot wallet's balance of the base
	// asset, testnet faucet included — CONTEXT, not the leg.
	BaseAsset              string   `json:"base_asset"`
	SpotBaseBalanceQtyCoin *float64 `json:"spot_base_balance_qty_coin"`

	PerpUnrealizedPnLGrossQuote float64 `json:"perp_unrealized_pnl_gross_quote"`
	PerpUpdatedAtMs             int64   `json:"perp_updated_at_ms"`

	TrackedIntents    int               `json:"tracked_intents"`
	Intents           []intentHedgeView `json:"intents"`
	UnreadableVI      []string          `json:"unreadable_vi"`
	WorkingVI         []string          `json:"working_vi"`
	CacheUnreadableVI []string          `json:"cache_unreadable_vi"`
	ErrorVI           string            `json:"error_vi"`
}

func (p *portal) handlePositions(w http.ResponseWriter, r *http.Request) {
	symbol, ok := p.symbolQuery(w, r)
	if !ok {
		return
	}
	ctx, cancel := readContext(r)
	defer cancel()
	v, _, _ := p.positions.get(symbol, positionsTTL, func() (positionsView, error) {
		return p.readPositions(ctx, symbol), nil
	})
	writeJSON(w, http.StatusOK, v)
}

// readPositions has a NAMED result on purpose: the deferred stamp below must
// write into the value being returned, not into a copy of it.
func (p *portal) readPositions(ctx context.Context, symbol string) (v positionsView) {
	v = positionsView{Symbol: symbol, Status: statusUnknown}
	defer func() {
		v.ReadAtMs = p.now().UnixMilli()
		v.StatusVI = statusVI[v.Status]
	}()
	if err := p.markets.both(); err != nil {
		v.ReasonVI = "thiếu credential — không đọc được hai chân: " + err.Error()
		return v
	}
	if err := p.markets.readBudgetError(); err != nil {
		v.ReasonVI = err.Error()
		return v
	}

	ev := hedgeEvidence{}
	if rules, err := p.rulesFor(ctx, symbol); err != nil {
		v.ErrorVI = err.Error()
	} else {
		ev.ToleranceQtyCoin = rules.toleranceQtyCoin()
		v.ToleranceQtyCoin = ev.ToleranceQtyCoin
		v.BaseAsset = rules.Spot.BaseAsset
	}

	pos, err := p.markets.perp.GetPosition(ctx, broker.MarketFuturesUSDM, symbol)
	if err != nil {
		v.ErrorVI = strings.TrimPrefix(v.ErrorVI+" · vị thế perp: "+err.Error(), " · ")
	} else {
		ev.PerpPositionRead = true
		ev.VenuePerpQtyCoin = pos.QtyCoin
		v.PerpQtyCoin = pos.QtyCoin
		v.PerpUnrealizedPnLGrossQuote = pos.UnrealizedPnLQuote
		v.PerpUpdatedAtMs = pos.UpdatedAtMs
	}

	if v.BaseAsset != "" {
		for _, b := range p.accountFor(ctx, symbol).Spot.Balances {
			if b.Asset == v.BaseAsset {
				total := b.TotalQtyInAsset
				v.SpotBaseBalanceQtyCoin = &total
			}
		}
	}

	states, unreadableFiles, err := listStates(p.stateDir, symbol)
	if err != nil {
		v.CacheUnreadableVI = append(v.CacheUnreadableVI, err.Error())
	}
	v.CacheUnreadableVI = append(v.CacheUnreadableVI, unreadableFiles...)
	for _, f := range v.CacheUnreadableVI {
		// An intent file nobody can read is an intent whose orders nobody is
		// looking at: the answer is "unknown", not the green banner.
		ev.UnreadableVI = append(ev.UnreadableVI, "file ý định "+f)
	}
	var ids []string
	for _, s := range states {
		if s.tracked() {
			ids = append(ids, s.IntentID)
		}
	}
	v.TrackedIntents = len(ids)
	for _, h := range readIntentHedges(ctx, p.markets.spot, p.markets.perp, p.memo, symbol, ids) {
		ev.SpotLegQtyCoin += h.Spot.QtyCoin
		ev.IntentsPerpQtyCoin += h.Perp.QtyCoin
		ev.UnreadableVI = append(ev.UnreadableVI, h.unreadable()...)
		ev.WorkingVI = append(ev.WorkingVI, h.working()...)

		one := intentHedgeView{intentHedge: h, ResidualCoin: h.residualCoin()}
		one.Status, one.ReasonVI = classifyHedge(hedgeEvidence{
			SpotLegQtyCoin: h.Spot.QtyCoin, IntentsPerpQtyCoin: h.Perp.QtyCoin, VenuePerpQtyCoin: h.Perp.QtyCoin,
			ToleranceQtyCoin: ev.ToleranceQtyCoin, PerpPositionRead: true,
			UnreadableVI: h.unreadable(), WorkingVI: h.working(),
		})
		v.Intents = append(v.Intents, one)
	}
	v.SpotQtyCoin = ev.SpotLegQtyCoin
	v.IntentsPerpQtyCoin = ev.IntentsPerpQtyCoin
	v.DeltaResidualCoin = ev.SpotLegQtyCoin + ev.VenuePerpQtyCoin
	v.UnreadableVI, v.WorkingVI = ev.UnreadableVI, ev.WorkingVI
	v.Status, v.ReasonVI = classifyHedge(ev)
	if v.Status == statusBothFlat || v.Status == statusBothOpen {
		// Say what the verdict covers. An intent whose close or unwind proved
		// it flat is not read again, so the banner speaks for the tracked ones
		// and the venue's perp position; LÀM PHẲNG scans every intent.
		v.ReasonVI += fmt.Sprintf(" — theo vị thế perp sàn báo và lệnh của %d ý định đang theo dõi; ý định đã đóng/gỡ phẳng sạch không đọc lại (LÀM PHẲNG quét tất cả)", len(ids))
	}
	return v
}

// ------------------------------------------------------------------- orders

type orderView struct {
	Market            string  `json:"market"`
	Side              string  `json:"side"`
	Type              string  `json:"type"`
	Status            string  `json:"status"`
	ClientOrderID     string  `json:"client_order_id"`
	VenueOrderID      string  `json:"venue_order_id"`
	QtyCoin           float64 `json:"qty_coin"`
	PriceQuote        float64 `json:"price_quote"`
	FilledQtyCoin     float64 `json:"filled_qty_coin"`
	AvgFillPriceQuote float64 `json:"avg_fill_price_quote"`
	ReduceOnly        bool    `json:"reduce_only"`
	UpdatedAtMs       int64   `json:"updated_at_ms"`

	// IntentID and KindVI name the cached intent whose derived id this is,
	// empty when the order belongs to no intent this machine knows.
	IntentID string `json:"intent_id"`
	KindVI   string `json:"kind_vi"`
}

type ordersView struct {
	Symbol         string      `json:"symbol"`
	ReadAtMs       int64       `json:"read_at_ms"`
	Spot           []orderView `json:"spot"`
	Futures        []orderView `json:"futures"`
	SpotErrorVI    string      `json:"spot_error_vi"`
	FuturesErrorVI string      `json:"futures_error_vi"`
}

func (p *portal) handleOrders(w http.ResponseWriter, r *http.Request) {
	symbol, ok := p.symbolQuery(w, r)
	if !ok {
		return
	}
	if err := p.markets.both(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "no_credentials", "thiếu credential: "+err.Error())
		return
	}
	ctx, cancel := readContext(r)
	defer cancel()
	v, _, _ := p.orders.get(symbol, ordersTTL, func() (ordersView, error) {
		owners := p.orderOwners(symbol)
		out := ordersView{Symbol: symbol}
		if err := p.markets.readBudgetError(); err != nil {
			out.SpotErrorVI, out.FuturesErrorVI = err.Error(), err.Error()
			out.ReadAtMs = p.now().UnixMilli()
			return out, nil
		}
		if list, err := p.markets.spot.OpenOrders(ctx, broker.MarketSpot, symbol); err != nil {
			out.SpotErrorVI = err.Error()
		} else {
			out.Spot = orderViews(list, owners)
		}
		if list, err := p.markets.perp.OpenOrders(ctx, broker.MarketFuturesUSDM, symbol); err != nil {
			out.FuturesErrorVI = err.Error()
		} else {
			out.Futures = orderViews(list, owners)
		}
		out.ReadAtMs = p.now().UnixMilli()
		return out, nil
	})
	writeJSON(w, http.StatusOK, v)
}

type orderOwner struct{ intentID, kindVI string }

// orderOwners maps every derived id of every cached intent to that intent.
func (p *portal) orderOwners(symbol string) map[string]orderOwner {
	owners := map[string]orderOwner{}
	states, _, _ := listStates(p.stateDir, symbol)
	for _, s := range states {
		for _, leg := range []execution.LegName{execution.LegSpot, execution.LegPerp} {
			for _, ref := range intentOrderIDs(s.IntentID, leg) {
				owners[ref.id] = orderOwner{intentID: s.IntentID, kindVI: ref.kind + " " + string(leg)}
			}
		}
	}
	return owners
}

func orderViews(list []broker.Order, owners map[string]orderOwner) []orderView {
	out := make([]orderView, 0, len(list))
	for _, o := range list {
		owner := owners[o.ClientOrderID]
		out = append(out, orderView{
			Market: string(o.Market), Side: string(o.Side), Type: string(o.Type), Status: string(o.Status),
			ClientOrderID: o.ClientOrderID, VenueOrderID: o.VenueOrderID,
			QtyCoin: o.QtyCoin, PriceQuote: o.PriceQuote,
			FilledQtyCoin: o.FilledQtyCoin, AvgFillPriceQuote: o.AvgFillPriceQuote,
			ReduceOnly: o.ReduceOnly, UpdatedAtMs: o.UpdatedAtMs,
			IntentID: owner.intentID, KindVI: owner.kindVI,
		})
	}
	return out
}

// ------------------------------------------------------------------ funding

type fundingRowView struct {
	SettledAtMs int64 `json:"settled_at_ms"`
	// IncomeQtyInAsset is the venue's figure in Asset, which is not always
	// the quote asset (a BNFCR row is not USDT) and is never converted.
	IncomeQtyInAsset float64  `json:"income_qty_in_asset"`
	Asset            string   `json:"asset"`
	TranID           string   `json:"tran_id"`
	IntentIDs        []string `json:"intent_ids"`
}

// intentFundingView sets one intent's cached funding figure — what execution
// read from the venue when it closed — beside the venue's rows inside that
// intent's holding window as the venue lists them now. Two readings, printed
// side by side, never merged.
type intentFundingView struct {
	IntentID           string  `json:"intent_id"`
	OpenedAtMs         int64   `json:"opened_at_ms"`
	ClosedAtMs         int64   `json:"closed_at_ms"`
	PerpQtyCoin        float64 `json:"perp_qty_coin"`
	CachedFundingQuote float64 `json:"cached_funding_received_quote"`
	VenueRowsQuote     float64 `json:"venue_rows_quote"`
	OtherAssetRows     int     `json:"other_asset_rows"`
	VenueRows          int     `json:"venue_rows"`
	SharedRows         int     `json:"shared_rows"`
	OutsideWindow      bool    `json:"outside_window"`
}

type fundingView struct {
	Symbol                      string              `json:"symbol"`
	ReadAtMs                    int64               `json:"read_at_ms"`
	WindowStartMs               int64               `json:"window_start_ms"`
	WindowEndMs                 int64               `json:"window_end_ms"`
	Rows                        []fundingRowView    `json:"rows"`
	TotalsByAsset               map[string]float64  `json:"totals_by_asset"`
	Intents                     []intentFundingView `json:"intents"`
	MarkPriceQuote              float64             `json:"mark_price_quote"`
	LastFundingRatePerPeriodBps *float64            `json:"last_funding_rate_per_period_bps"`
	NextFundingTimeMs           int64               `json:"next_funding_time_ms"`
	NoteVI                      string              `json:"note_vi"`
	ErrorVI                     string              `json:"error_vi"`
}

func (p *portal) handleFunding(w http.ResponseWriter, r *http.Request) {
	symbol, ok := p.symbolQuery(w, r)
	if !ok {
		return
	}
	if err := p.markets.both(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "no_credentials", "thiếu credential: "+err.Error())
		return
	}
	ctx, cancel := readContext(r)
	defer cancel()
	v, _, _ := p.funding.get(symbol, fundingTTL, func() (fundingView, error) {
		now := p.now()
		out := fundingView{
			Symbol: symbol, WindowStartMs: now.Add(-fundingLookback).UnixMilli(), WindowEndMs: now.UnixMilli(),
			NoteVI: "Mỗi dòng là MỘT mốc settle sàn đã thực trả/thu (quy tắc 6), đọc từ /fapi/v1/income. " +
				"Sàn trả cho vị thế của TÀI KHOẢN; một dòng chỉ gán được cho ý định khi đúng một ý định đang giữ qua mốc đó.",
		}
		if err := p.markets.readBudgetError(); err != nil {
			// Returned as an error so the refusal is shared for errorTTL
			// only, and carried in the view so the page can say why.
			out.ErrorVI, out.ReadAtMs = err.Error(), now.UnixMilli()
			return out, err
		}
		rows, err := p.markets.perp.FundingIncome(ctx, broker.MarketFuturesUSDM, symbol, out.WindowStartMs, out.WindowEndMs)
		if err != nil {
			out.ErrorVI = "funding: " + err.Error()
		}
		if mp, err := p.markets.perp.MarkPrice(ctx, broker.MarketFuturesUSDM, symbol); err == nil {
			out.MarkPriceQuote = mp.MarkPriceQuote
			out.LastFundingRatePerPeriodBps = bpsPtr(mp.LastFundingRateFrac*10_000, true)
			out.NextFundingTimeMs = mp.NextFundingTimeMs
		}
		quoteAsset := ""
		if rules, err := p.rulesFor(ctx, symbol); err == nil {
			quoteAsset = rules.Perp.QuoteAsset
		}
		states, _, _ := listStates(p.stateDir, symbol)
		out.Rows, out.TotalsByAsset, out.Intents = attributeFunding(rows, states, quoteAsset, out.WindowStartMs, out.WindowEndMs, now.UnixMilli())
		out.ReadAtMs = p.now().UnixMilli()
		return out, nil
	})
	writeJSON(w, http.StatusOK, v)
}

// attributeFunding matches the venue's settlement rows to the intents whose
// perp leg was held across them. The venue pays the ACCOUNT's position, so a
// row that falls inside two intents' windows belongs to both and is split
// between neither: splitting it needs each position's size at the stamp, and an
// invented split is rule 5's wrong-but-not-obviously-wrong number.
//
// VenueRowsQuote sums only rows paid in quoteAsset; a row in any other asset is
// counted in OtherAssetRows and never added in. An unknown quoteAsset ("") adds
// nothing, rather than guessing which asset is the quote.
func attributeFunding(rows []broker.FundingIncome, states []intentState, quoteAsset string, windowStartMs, windowEndMs, nowMs int64) ([]fundingRowView, map[string]float64, []intentFundingView) {
	type span struct {
		s          intentState
		startMs    int64
		endMs      int64
		perpQty    float64
		rowsQuote  float64
		rowsCount  int
		sharedRows int
		otherAsset int
	}
	var spans []*span
	for _, s := range states {
		if s.PerpFilledQtyCoin <= 0 || s.Outcome != string(execution.OutcomeBothOpen) {
			continue
		}
		end := s.ClosedAtMs
		if end == 0 {
			end = nowMs
		}
		spans = append(spans, &span{s: s, startMs: s.OpenedAtMs, endMs: end, perpQty: s.PerpFilledQtyCoin})
	}

	totals := map[string]float64{}
	out := make([]fundingRowView, 0, len(rows))
	for _, row := range rows {
		totals[row.Asset] += row.IncomeQuote
		view := fundingRowView{SettledAtMs: row.SettledAtMs, IncomeQtyInAsset: row.IncomeQuote, Asset: row.Asset, TranID: row.TranID}
		var holders []*span
		for _, sp := range spans {
			if row.SettledAtMs >= sp.startMs && row.SettledAtMs <= sp.endMs {
				holders = append(holders, sp)
				view.IntentIDs = append(view.IntentIDs, sp.s.IntentID)
			}
		}
		for _, sp := range holders {
			sp.rowsCount++
			switch {
			case len(holders) > 1:
				sp.sharedRows++
			case quoteAsset == "" || row.Asset != quoteAsset:
				sp.otherAsset++
			default:
				sp.rowsQuote += row.IncomeQuote
			}
		}
		out = append(out, view)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SettledAtMs > out[j].SettledAtMs })

	intents := make([]intentFundingView, 0, len(spans))
	for _, sp := range spans {
		intents = append(intents, intentFundingView{
			IntentID: sp.s.IntentID, OpenedAtMs: sp.s.OpenedAtMs, ClosedAtMs: sp.s.ClosedAtMs,
			PerpQtyCoin: sp.perpQty, CachedFundingQuote: sp.s.FundingQuote,
			VenueRowsQuote: sp.rowsQuote, VenueRows: sp.rowsCount, SharedRows: sp.sharedRows, OtherAssetRows: sp.otherAsset,
			OutsideWindow: sp.startMs < windowStartMs || sp.endMs > windowEndMs,
		})
	}
	return out, totals, intents
}

// ------------------------------------------------------------------ intents

type intentView struct {
	intentState
	Origin               string   `json:"origin"`
	Tracked              bool     `json:"tracked"`
	SpotEntrySlippageBps *float64 `json:"spot_entry_slippage_bps"`
	PerpEntrySlippageBps *float64 `json:"perp_entry_slippage_bps"`
}

type intentsView struct {
	ReadAtMs     int64        `json:"read_at_ms"`
	Symbol       string       `json:"symbol"`
	Intents      []intentView `json:"intents"`
	UnreadableVI []string     `json:"unreadable_vi"`
	NoteVI       string       `json:"note_vi"`
}

func (p *portal) handleIntents(w http.ResponseWriter, r *http.Request) {
	symbol := ""
	if r.URL.Query().Has("symbol") {
		s, ok := p.symbolQuery(w, r)
		if !ok {
			return
		}
		symbol = s
	}
	states, unreadable, err := listStates(p.stateDir, symbol)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cache_unreadable", "không đọc được thư mục ý định: "+err.Error())
		return
	}
	out := intentsView{
		ReadAtMs: p.now().UnixMilli(), Symbol: symbol, UnreadableVI: unreadable,
		NoteVI: "CACHE — các con số đúng tại một thời điểm đã qua; vị thế hiện tại đọc ở /api/positions từ sàn.",
	}
	for _, s := range states {
		spotBps, spotOK := slippageBps(s.SpotAvgPriceQuote, s.SpotBestAskQuote, true)
		perpBps, perpOK := slippageBps(s.PerpAvgPriceQuote, s.PerpBestBidQuote, false)
		out.Intents = append(out.Intents, intentView{
			intentState: s, Origin: originOf(s.IntentID), Tracked: s.tracked(),
			SpotEntrySlippageBps: bpsPtr(spotBps, spotOK), PerpEntrySlippageBps: bpsPtr(perpBps, perpOK),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func originOf(intentID string) string {
	switch {
	case strings.HasPrefix(intentID, intentPrefixAutotrade):
		return "autotrade"
	case strings.HasPrefix(intentID, intentPrefixPortal):
		return "execportal"
	case strings.HasPrefix(intentID, "x"):
		return "execcheck"
	}
	return "không rõ"
}

// ------------------------------------------------------------------- market

type ruleView struct {
	Status           string  `json:"status"`
	StepSizeCoin     float64 `json:"step_size_coin"`
	MinQtyCoin       float64 `json:"min_qty_coin"`
	TickSizeQuote    float64 `json:"tick_size_quote"`
	MinNotionalQuote float64 `json:"min_notional_quote"`
}

type marketView struct {
	Symbol                        string   `json:"symbol"`
	ReadAtMs                      int64    `json:"read_at_ms"`
	BaseAsset                     string   `json:"base_asset"`
	QuoteAsset                    string   `json:"quote_asset"`
	Spot                          ruleView `json:"spot"`
	Futures                       ruleView `json:"futures"`
	MarkPriceQuote                float64  `json:"mark_price_quote"`
	LastFundingRatePerPeriodBps   *float64 `json:"last_funding_rate_per_period_bps"`
	NextFundingTimeMs             int64    `json:"next_funding_time_ms"`
	SmallestWorkableNotionalQuote float64  `json:"smallest_workable_notional_quote"`
	MaxNotionalQuote              float64  `json:"max_notional_quote"`
	NoteVI                        string   `json:"note_vi"`
	ErrorVI                       string   `json:"error_vi"`
}

func (p *portal) handleMarket(w http.ResponseWriter, r *http.Request) {
	symbol, ok := p.symbolQuery(w, r)
	if !ok {
		return
	}
	if err := p.markets.both(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "no_credentials", "thiếu credential: "+err.Error())
		return
	}
	ctx, cancel := readContext(r)
	defer cancel()
	v, _, _ := p.marketInfo.get(symbol, marketTTL, func() (marketView, error) {
		out := marketView{
			Symbol: symbol, MaxNotionalQuote: maxNotionalQuote,
			NoteVI: "last_funding_rate_per_period_bps là mức sàn công bố gần nhất cho CHU KỲ RIÊNG của symbol, portal không suy ra chu kỳ và không quy đổi (quy tắc 3); " +
				"cỡ nhỏ nhất là GỢI Ý từ luật sàn, mức tối thiểu thật do execution kiểm trên cỡ đã làm tròn.",
		}
		if err := p.markets.readBudgetError(); err != nil {
			out.ErrorVI, out.ReadAtMs = err.Error(), p.now().UnixMilli()
			return out, err
		}
		rules, err := p.rulesFor(ctx, symbol)
		if err != nil {
			out.ErrorVI = err.Error()
			out.ReadAtMs = p.now().UnixMilli()
			return out, err
		}
		out.BaseAsset, out.QuoteAsset = rules.Spot.BaseAsset, rules.Spot.QuoteAsset
		out.Spot = ruleView{Status: rules.Spot.Status, StepSizeCoin: rules.Spot.StepSizeCoin, MinQtyCoin: rules.Spot.MinQtyCoin,
			TickSizeQuote: rules.Spot.TickSizeQuote, MinNotionalQuote: rules.Spot.MinNotionalQuote}
		out.Futures = ruleView{Status: rules.Perp.Status, StepSizeCoin: rules.Perp.StepSizeCoin, MinQtyCoin: rules.Perp.MinQtyCoin,
			TickSizeQuote: rules.Perp.TickSizeQuote, MinNotionalQuote: rules.Perp.MinNotionalQuote}
		if mp, err := p.markets.perp.MarkPrice(ctx, broker.MarketFuturesUSDM, symbol); err != nil {
			out.ErrorVI = "giá đánh dấu: " + err.Error()
		} else {
			out.MarkPriceQuote = mp.MarkPriceQuote
			out.LastFundingRatePerPeriodBps = bpsPtr(mp.LastFundingRateFrac*10_000, true)
			out.NextFundingTimeMs = mp.NextFundingTimeMs
			out.SmallestWorkableNotionalQuote = smallestWorkableNotionalQuote(rules.Spot, rules.Perp, mp.MarkPriceQuote)
		}
		out.ReadAtMs = p.now().UnixMilli()
		return out, nil
	})
	writeJSON(w, http.StatusOK, v)
}
