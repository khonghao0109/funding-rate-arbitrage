package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"futures-arbitrage-scanner/cmd/execportal/autotrade"
	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
)

// The testnet auto-trader's side of the portal (PLAN §7.1 Q18).
//
// package autotrade decides; this file is everything it is allowed to touch,
// and it is deliberately thin:
//
//   - portalMarket reads the two testnet markets — books, mark price and
//     forming rate, the settled rates, this account's fees — through the same
//     venue clients the page reads with, under the same half-of-the-budget
//     read wall. The engine reads several symbols at once through it.
//   - portalTrader reads the hedge through readHedge (the venue's orders and
//     position, exactly what the banner shows, without the wallet read the
//     verdict does not use) and opens and closes through openAs and close —
//     the functions a button press runs, under the same write lock, so a manual
//     write and a bot write can never overlap and neither can reach the venue
//     by a road the other does not take. guard_test.go holds this file to that:
//     no order is placed here, and no execution machine is built here.
//   - The endpoints: status and PnL, and start / stop / kill / close-pair /
//     pair behind the same header, origin and JSON walls as every other write.

// The action header values of the bot's writes. A write is refused unless it
// carries its own name, which only the page's confirmed path sends.
const (
	autotradeStartAction = "autotrade-start"
	// autotradeStopAction keeps the positions and sends no order, so the page
	// sends it without a dialog; a stop that CLOSES sends orders and carries
	// its own name, which only a confirmed path may send.
	autotradeStopAction      = "autotrade-stop"
	autotradeStopCloseAction = "autotrade-stop-close"
	autotradeKillAction      = "autotrade-kill"
	// autotradeClosePairAction closes one pair: orders, so a confirmed path.
	autotradeClosePairAction = "autotrade-close-pair"
	// The three switches on one pair; each name must agree with the body's
	// action. PAUSE sends nothing and leads to nothing. RESUME lets the bot
	// open the pair again, and ACK releases a halted pair's position to
	// adoption — its exits may then send a close — so both go behind a dialog.
	autotradePairPauseAction  = "autotrade-pair-pause"
	autotradePairResumeAction = "autotrade-pair-resume"
	autotradePairAckAction    = "autotrade-pair-ack"

	// The portal's write lock names what holds it; these appear on the page's
	// busy line when a button press finds the bot mid-trade.
	autotradeOpenLock  = "autotrade-open"
	autotradeCloseLock = "autotrade-close"
)

// Caching of the two slow-moving reads a scan makes every few seconds.
const (
	// commissionTTL: a fee schedule does not change between scans, and each
	// read costs weight 20 on each market.
	commissionTTL = 10 * time.Minute
	// fundingRatesTTL: a new settlement is seen at most this late, which the
	// exits can afford — they act on what settled, and the next settlement is
	// hours away. The endpoint's own limit is 500 per five minutes.
	fundingRatesTTL = 2 * time.Minute
	// maxFundingRatePages bounds the paging of a long hold's history.
	maxFundingRatePages = 10
)

type commissionPair struct {
	Spot binancebroker.CommissionRates
	Perp binancebroker.CommissionRates
}

// newAutotrade builds the portal's engine over every symbol the portal trades.
// It does not run until main starts Run, and it trades nothing until someone
// presses BẬT (or -autotrade).
func newAutotrade(p *portal) *autotrade.Engine {
	symbols := p.symbols
	if len(symbols) == 0 {
		symbols = []string{"BTCUSDT"}
	}
	eng, err := autotrade.New(autotrade.Options{
		Market: portalMarket{p}, Trader: portalTrader{p},
		Symbols: symbols, MaxNotionalQuote: maxNotionalQuote,
		PerpMarginFrac: p.exec.MarginFrac, ActionTimeout: max(p.exec.ActionTimeout, time.Minute),
		Now: p.now, Logf: log.Printf,
	})
	if err != nil {
		// Every option is the portal's own; a refusal is a programming error.
		panic(err)
	}
	return eng
}

// ------------------------------------------------------------------ market

type portalMarket struct{ p *portal }

func (m portalMarket) Snapshot(ctx context.Context, symbol string, settledSinceMs int64) (autotrade.Snapshot, error) {
	p := m.p
	snap := autotrade.Snapshot{Symbol: symbol}
	if err := p.markets.both(); err != nil {
		return snap, err
	}
	if err := p.markets.readBudgetError(); err != nil {
		return snap, err
	}
	var err error
	if snap.SpotBook, err = readBook(ctx, p.markets.spot, symbol); err != nil {
		return snap, fmt.Errorf("sổ spot: %w", err)
	}
	if snap.PerpBook, err = readBook(ctx, p.markets.perp, symbol); err != nil {
		return snap, fmt.Errorf("sổ perp: %w", err)
	}
	mp, err := p.markets.perp.MarkPrice(ctx, broker.MarketFuturesUSDM, symbol)
	if err != nil {
		return snap, fmt.Errorf("premiumIndex: %w", err)
	}
	snap.ForecastRatePerIntervalFrac, snap.NextFundingTimeMs = mp.LastFundingRateFrac, mp.NextFundingTimeMs
	snap.ReadAtMs = p.now().UnixMilli()

	// The STRICTER of the two markets' order rules, so the bot can refuse a
	// size the venue would refuse rather than learn it from a rejected order.
	// Shared for ten minutes by rulesFor; every ORDER path still reads them
	// fresh. A failure is reported, never defaulted: an unread step size that
	// read as zero would turn the quantization guard off.
	if rules, err := p.rulesFor(ctx, symbol); err != nil {
		snap.RulesErrVI = err.Error()
	} else {
		snap.StepSizeCoin = math.Max(rules.Spot.StepSizeCoin, rules.Perp.StepSizeCoin)
		snap.MinQtyCoin = math.Max(rules.Spot.MinQtyCoin, rules.Perp.MinQtyCoin)
		snap.MinNotionalQuote = math.Max(rules.Spot.MinNotionalQuote, rules.Perp.MinNotionalQuote)
	}

	if rows, err := p.settledRates(ctx, symbol, settledSinceMs, mp.NextFundingTimeMs); err != nil {
		snap.SettledErrVI = err.Error()
	} else {
		for _, r := range rows {
			if r.SettledAtMs < settledSinceMs {
				continue
			}
			snap.Settled = append(snap.Settled, autotrade.SettledRate{
				SettledAtMs: r.SettledAtMs, RatePerIntervalFrac: r.RatePerIntervalFrac, Special: strings.EqualFold(r.RateType, "Special"),
			})
		}
	}

	fees, _, err := p.commissions.get(symbol, commissionTTL, func() (commissionPair, error) {
		spot, err := p.markets.spot.CommissionRates(ctx, symbol)
		if err != nil {
			return commissionPair{}, fmt.Errorf("spot: %w", err)
		}
		perp, err := p.markets.perp.CommissionRates(ctx, symbol)
		if err != nil {
			return commissionPair{}, fmt.Errorf("futures: %w", err)
		}
		return commissionPair{Spot: spot, Perp: perp}, nil
	})
	if err != nil {
		snap.FeesErrVI = err.Error()
	} else {
		snap.SpotTakerFeeBps, snap.PerpTakerFeeBps = fees.Spot.TakerBps(), fees.Perp.TakerBps()
		snap.FeeSourceVI = fees.Spot.SourceVI + " · " + fees.Perp.SourceVI
	}
	// The offset every signed request is corrected by. Re-measured only on the
	// account tiles' own cadence (syncClockIfStale: at most once per clockEvery
	// per market, whoever asks), because a sync taken while an order is being
	// signed is how a -1021 happens (api.go). A market whose clock could not be
	// measured now reports none, and the entry check refuses on it.
	for _, pair := range []struct {
		c   venue
		dst **int64
	}{{p.markets.spot, &snap.SpotClockSkewMs}, {p.markets.perp, &snap.PerpClockSkewMs}} {
		h := pair.c.HTTP()
		if err := p.syncClockIfStale(ctx, pair.c.Market(), h); err != nil {
			continue
		}
		if !h.ClockMeasuredAt().IsZero() {
			skew := h.ClockSkewMs()
			*pair.dst = &skew
		}
	}
	return snap, nil
}

// settledRates reads every settlement from sinceMs to now, oldest first.
//
// It pages until the venue answers with nothing more, rather than stopping at
// a short page: the venue returns the OLDEST rows first when a window holds
// more than it sends, and the testnet's real page ceiling has not been
// measured, so a short page is not proof the newest settlements — the ones the
// exits read — were included.
//
// The cache is keyed by the symbol, the hour the window starts in AND the
// venue's next settlement stamp, so a settlement that has just happened moves
// the key and is read at the next scan instead of after the TTL. The caller
// filters to its exact start.
func (p *portal) settledRates(ctx context.Context, symbol string, sinceMs, nextFundingTimeMs int64) ([]binancebroker.FundingRate, error) {
	hourMs := time.Hour.Milliseconds()
	startMs := sinceMs / hourMs * hourMs
	key := fmt.Sprintf("%s|%d|%d", symbol, startMs, nextFundingTimeMs)
	// The key moves every hour and every settlement; the cache keeps only each
	// symbol's newest key rather than growing for the life of the process, and
	// several symbols scanned in one pass do not evict one another.
	p.fundingKeyMu.Lock()
	if old, ok := p.fundingKeys[symbol]; ok && old != key {
		p.fundingRates.forget(old)
	}
	p.fundingKeys[symbol] = key
	p.fundingKeyMu.Unlock()
	rows, _, err := p.fundingRates.get(key, fundingRatesTTL, func() ([]binancebroker.FundingRate, error) {
		var all []binancebroker.FundingRate
		from := startMs
		endMs := p.now().UnixMilli()
		seen := map[int64]bool{}
		for page := 0; page < maxFundingRatePages; page++ {
			batch, err := p.markets.perp.FundingRateHistory(ctx, symbol, from, endMs)
			if err != nil {
				return nil, err
			}
			// Only rows inside the window asked for, each stamp once: a venue
			// that ignored startTime and answered the same page again would
			// otherwise double every settlement — and with it the count a
			// MaxHoldEpochs exit closes on.
			var last int64
			added := 0
			for _, r := range batch {
				if r.SettledAtMs < from || r.SettledAtMs > endMs || seen[r.SettledAtMs] {
					continue
				}
				seen[r.SettledAtMs] = true
				all = append(all, r)
				added++
				last = max(last, r.SettledAtMs)
			}
			switch {
			case len(batch) > 0 && added == 0:
				// A page of rows none of which is new: the venue answered
				// outside the window asked for, and the newest settlements —
				// the ones the exits read — may be missing.
				return nil, errors.New("sàn trả lại các dòng funding ngoài cửa sổ đã hỏi — không dùng một chuỗi có thể thiếu mốc mới nhất")
			case added == 0 || last >= endMs:
				return all, nil
			}
			from = last + 1
		}
		return nil, fmt.Errorf("hơn %d trang lịch sử funding — không dùng một chuỗi đọc chưa hết", maxFundingRatePages)
	})
	return rows, err
}

// ------------------------------------------------------------------ trader

type portalTrader struct{ p *portal }

// Holding is the banner's own verdict: readHedge, from the venue.
func (t portalTrader) Holding(ctx context.Context, symbol string) (autotrade.Holding, error) {
	p := t.p
	v := p.readHedge(ctx, symbol, false)
	h := autotrade.Holding{Status: autotrade.HedgeStatus(v.Status), ReasonVI: strings.TrimPrefix(v.ReasonVI+" · "+v.ErrorVI, " · "),
		ResidualQtyCoin: v.DeltaResidualCoin, ToleranceQtyCoin: v.ToleranceQtyCoin}
	h.ReasonVI = strings.TrimSuffix(h.ReasonVI, " · ")
	tol := v.ToleranceQtyCoin + gridEpsilon
	var held []intentHedgeView
	for _, in := range v.Intents {
		if math.Abs(in.Spot.QtyCoin) > tol || math.Abs(in.Perp.QtyCoin) > tol {
			held = append(held, in)
		}
	}
	h.HeldIntents = len(held)
	if len(held) == 1 {
		h.IntentID = held[0].IntentID
		h.FromAutotrade = originOf(h.IntentID) == "autotrade"
		h.QtyCoin = -held[0].Perp.QtyCoin
		// The cache is read only for what the venue cannot say: when the
		// intent opened, its notional, the mids its entry was decided on and
		// the average prices its two legs filled at.
		if st, err := loadState(p.stateDir, h.IntentID); err == nil {
			h.OpenedAtMs, h.NotionalQuote = st.OpenedAtMs, st.NotionalQuote
			h.SpotRefMidQuote, h.PerpRefMidQuote = st.SpotRefMidQuote, st.PerpRefMidQuote
			h.SpotAvgFillQuote, h.PerpAvgFillQuote = st.SpotAvgPriceQuote, st.PerpAvgPriceQuote
		}
	}
	return h, nil
}

func (t portalTrader) Open(ctx context.Context, order autotrade.OpenOrder) autotrade.OpenResult {
	p := t.p
	cost := order.SignalEntryCostPct
	req := openRequest{Symbol: order.Symbol, NotionalQuote: order.NotionalQuote, SignalEntryCostPct: &cost}
	if err := p.validateOpen(&req); err != nil {
		return autotrade.OpenResult{Refused: true, ErrorVI: err.Error()}
	}
	if err := p.markets.both(); err != nil {
		return autotrade.OpenResult{Refused: true, ErrorVI: "thiếu credential: " + err.Error()}
	}
	release, heldBy, _, ok := p.acquire(autotradeOpenLock)
	if !ok {
		return autotrade.OpenResult{Busy: true, ErrorVI: "khoá ghi đang do " + orUnknown(heldBy) + " giữ"}
	}
	defer release()
	defer p.invalidateVenueReads()
	p.memo.forget()

	v, _ := p.openAs(ctx, req, intentPrefixAutotrade)
	log.Printf("execportal: AUTOTRADE OPEN %s %s %.2f quote → %s hedged=%v alarm=%v %s",
		v.IntentID, v.Symbol, v.NotionalQuote, v.Outcome, v.Hedged, v.Alarm, v.ErrorVI)
	errorVI := v.ErrorVI
	if v.CacheErrorVI != "" {
		errorVI = strings.TrimPrefix(errorVI+" · "+v.CacheErrorVI, " · ")
	}
	return autotrade.OpenResult{
		IntentID: v.IntentID, Refused: v.RefusedBeforePlacing, Hedged: v.Hedged, Alarm: v.Alarm,
		OpenedAtMs: v.OpenedAtMs, QtyCoin: v.Perp.FilledQtyCoin, ResidualQtyCoin: v.ResidualQtyCoin,
		SpotRefMidQuote: v.SpotRefMidQuote, PerpRefMidQuote: v.PerpRefMidQuote,
		SpotAvgFillQuote: v.Spot.AvgFillPriceQuote, PerpAvgFillQuote: v.Perp.AvgFillPriceQuote,
		UnhedgedWindowMs: v.UnhedgedWindowMs, UnwindDurationMs: v.UnwindDurationMs, ErrorVI: errorVI,
	}
}

func (t portalTrader) Close(ctx context.Context, symbol, intentID, reasonVI string) autotrade.CloseResult {
	p := t.p
	out := autotrade.CloseResult{IntentID: intentID, Refused: true}
	symbol, err := p.allowedSymbol(symbol)
	if err != nil {
		out.ErrorVI = err.Error()
		return out
	}
	if err := validIntentID(intentID); err != nil {
		out.ErrorVI = err.Error()
		return out
	}
	if err := p.markets.both(); err != nil {
		out.ErrorVI = "thiếu credential: " + err.Error()
		return out
	}
	release, heldBy, _, ok := p.acquire(autotradeCloseLock)
	if !ok {
		return autotrade.CloseResult{IntentID: intentID, Busy: true, ErrorVI: "khoá ghi đang do " + orUnknown(heldBy) + " giữ"}
	}
	defer release()
	defer p.invalidateVenueReads()
	p.memo.forget()

	st, err := loadState(p.stateDir, intentID)
	if err != nil {
		out.ErrorVI = "không đọc được file ý định " + intentID + ": " + err.Error()
		return out
	}
	if st.Symbol != symbol {
		out.ErrorVI = fmt.Sprintf("ý định %s là %s, không phải %s", intentID, st.Symbol, symbol)
		return out
	}
	v, _ := p.close(ctx, st, reasonVI)
	log.Printf("execportal: AUTOTRADE CLOSE %s %s → %s flat=%v refused=%v alarm=%v closed=%.8f %s",
		v.IntentID, v.Symbol, v.Outcome, v.Flat, v.Refused, v.Alarm, v.ClosedQtyCoin, v.ErrorVI)
	errorVI := v.ErrorVI
	if v.CacheErrorVI != "" {
		errorVI = strings.TrimPrefix(errorVI+" · "+v.CacheErrorVI, " · ")
	}
	return autotrade.CloseResult{
		IntentID: intentID, Refused: v.Refused, SentUnconfirmed: v.SentUnconfirmed, Flat: v.Flat, Alarm: v.Alarm,
		ClosedQtyCoin: v.ClosedQtyCoin, RemainingQtyCoin: v.RemainingQtyCoin,
		FundingReceivedQuote: v.FundingReceivedQuote, SettlementsCounted: v.SettlementsCounted,
		RealizedQuote: v.RealizedQuote, ErrorVI: errorVI,
	}
}

// Account is the two wallets' quote equity, read from the venues (rule 7) for
// the bot's periodic rebalance. It is read at most once per rebalance interval,
// never on the scan cadence, and it costs weight 20 on spot plus 5 on futures.
//
// The QUOTE asset is the one the PERP declares for the run's first symbol, not
// the string "USDT": the portal reads base and quote from what the venue
// declares and never from the symbol text (CLAUDE.md's assets trap). Both
// markets must agree, or nothing is priced — a spot leg funded in one asset
// and a perp margined in another is not one pool of equity by any arithmetic.
func (t portalTrader) Account(ctx context.Context) (autotrade.Account, error) {
	p := t.p
	if err := p.markets.both(); err != nil {
		return autotrade.Account{}, err
	}
	if err := p.markets.readBudgetError(); err != nil {
		return autotrade.Account{}, err
	}
	symbols := p.symbols
	if len(symbols) == 0 {
		return autotrade.Account{}, errors.New("portal không có symbol nào để đọc tài sản định giá")
	}
	rules, err := p.rulesFor(ctx, symbols[0])
	if err != nil {
		return autotrade.Account{}, fmt.Errorf("luật sàn của %s: %w", symbols[0], err)
	}
	quote := rules.Perp.QuoteAsset
	if quote == "" || quote != rules.Spot.QuoteAsset {
		return autotrade.Account{}, fmt.Errorf("hai sàn khai tài sản định giá khác nhau cho %s (spot %q, perp %q) — không cộng chung được",
			symbols[0], rules.Spot.QuoteAsset, rules.Perp.QuoteAsset)
	}
	out := autotrade.Account{QuoteAsset: quote}
	for _, m := range []struct {
		c   venue
		dst *float64
	}{{p.markets.spot, &out.SpotQuoteTotal}, {p.markets.perp, &out.FuturesQuoteTotal}} {
		balances, err := m.c.GetBalance(ctx, m.c.Market())
		if err != nil {
			return autotrade.Account{}, fmt.Errorf("số dư %s: %w", m.c.Market(), err)
		}
		found := false
		for _, b := range balances {
			if b.Asset != quote {
				continue
			}
			// Free PLUS locked: on spot the locked half is committed to resting
			// orders, on futures it is the margin already posted against open
			// positions. Reading only the free half reports an account with
			// everything deployed as empty, and would size every slot to zero
			// the moment the bot was fully invested.
			*m.dst = b.TotalQtyCoin()
			found = true
			break
		}
		if !found {
			return autotrade.Account{}, fmt.Errorf("sàn %s không liệt kê tài sản %s — 'không liệt kê' không phải 'bằng 0'", m.c.Market(), quote)
		}
	}
	out.ReadAtMs = p.now().UnixMilli()
	return out, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "một thao tác khác"
	}
	return s
}

// --------------------------------------------------------------- endpoints

// autotradeActionView answers every bot write: the status after it, what a
// stop-and-close, a kill or a close-pair did to each symbol, and the positions
// a stop kept.
type autotradeActionView struct {
	Status        autotrade.StatusView      `json:"status"`
	Closes        []autotrade.OperatorClose `json:"closes"`
	KeptIntentIDs []string                  `json:"kept_intent_ids"`
}

func (p *portal) handleAutotradeStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.autotrade.Status())
}

// autotradeOverrideRequest is one pair's own values; an absent field takes the
// run's default. The three convergence knobs are here too, so a pair may be
// tuned exactly as the run's default may be (PLAN "Công cụ vận hành 4.5f").
// Everything NOT here — the hysteresis, the basis stop, the depth multiple, the
// failure count — is a safety threshold pinned by
// TestDefaults_AreTheAuditedSafetyThresholds and is not on a form.
type autotradeOverrideRequest struct {
	NotionalQuote          *float64 `json:"notional_quote"`
	MinNetAPRPct           *float64 `json:"min_net_apr_pct"`
	MinEntryBasisBps       *float64 `json:"min_entry_basis_bps"`
	MaxHoldEpochs          *int     `json:"max_hold_epochs"`
	MinHoldEpochs          *int     `json:"min_hold_epochs"`
	TargetTakeProfitNetPct *float64 `json:"target_take_profit_net_pct"`
}

// apply overwrites only the fields the request states. Every one of them is
// then validated by autotrade.Config, never clamped here: a value a person
// typed is either run or refused by name.
func (o autotradeOverrideRequest) apply(c *autotrade.Config) {
	if o.NotionalQuote != nil {
		c.NotionalQuote = *o.NotionalQuote
	}
	if o.MinNetAPRPct != nil {
		c.MinNetAPRPct = *o.MinNetAPRPct
	}
	if o.MinEntryBasisBps != nil {
		c.MinEntryBasisBps = *o.MinEntryBasisBps
	}
	if o.MaxHoldEpochs != nil {
		c.MaxHoldEpochs = *o.MaxHoldEpochs
	}
	if o.MinHoldEpochs != nil {
		c.MinHoldEpochs = *o.MinHoldEpochs
	}
	if o.TargetTakeProfitNetPct != nil {
		c.TargetTakeProfitNetPct = *o.TargetTakeProfitNetPct
	}
}

// autotradeStartRequest is the page's form. An absent field takes the shipped
// default; a present one is validated, never clamped. symbols is required: a
// run that trades "whatever the portal lists" is not one a person chose.
//
// The embedded override carries the per-pair keys at the TOP level, which is
// what makes "the run's default" and "this pair's value" the same set of names.
type autotradeStartRequest struct {
	autotradeOverrideRequest
	Symbols                []string                            `json:"symbols"`
	MaxConcurrentPositions *int                                `json:"max_concurrent_positions"`
	TotalCapitalCapQuote   *float64                            `json:"total_capital_cap_quote"`
	PairOverrides          map[string]autotradeOverrideRequest `json:"pair_overrides"`

	// The buffered-slot sizing (PLAN "Công cụ vận hành 4.5g"). With
	// auto_rebalance on, notional_quote is only the SEED the first scan
	// replaces from the account's own equity.
	AutoRebalance          *bool    `json:"auto_rebalance"`
	MarginBufferPct        *float64 `json:"margin_buffer_pct"`
	RebalanceIntervalHours *float64 `json:"rebalance_interval_hours"`
}

// portfolioFromRequest turns the form into a run, every symbol through the
// portal's allow-list.
func (p *portal) portfolioFromRequest(req autotradeStartRequest) (autotrade.PortfolioConfig, error) {
	if len(req.Symbols) == 0 {
		return autotrade.PortfolioConfig{}, errors.New("symbols trống — chọn ít nhất một cặp")
	}
	var symbols []string
	for _, raw := range req.Symbols {
		s, err := p.allowedSymbol(raw)
		if err != nil {
			return autotrade.PortfolioConfig{}, err
		}
		symbols = append(symbols, s)
	}
	pc := autotrade.DefaultPortfolioConfig(symbols)
	req.autotradeOverrideRequest.apply(&pc.DefaultPairConfig)
	if req.MaxConcurrentPositions != nil {
		pc.MaxConcurrentPositions = *req.MaxConcurrentPositions
	}
	if req.TotalCapitalCapQuote != nil {
		pc.TotalCapitalCapQuote = *req.TotalCapitalCapQuote
	}
	if req.AutoRebalance != nil {
		pc.AutoRebalance = *req.AutoRebalance
	}
	if req.MarginBufferPct != nil {
		pc.MarginBufferPct = *req.MarginBufferPct
	}
	if req.RebalanceIntervalHours != nil {
		pc.RebalanceIntervalHours = *req.RebalanceIntervalHours
	}
	if len(req.PairOverrides) > 0 {
		pc.PairOverrides = map[string]autotrade.Config{}
		for raw, o := range req.PairOverrides {
			s, err := p.allowedSymbol(raw)
			if err != nil {
				return autotrade.PortfolioConfig{}, fmt.Errorf("pair_overrides: %w", err)
			}
			if _, dup := pc.PairOverrides[s]; dup {
				return autotrade.PortfolioConfig{}, fmt.Errorf("pair_overrides nêu %s hai lần", s)
			}
			c := pc.DefaultPairConfig
			c.Symbol = s
			o.apply(&c)
			pc.PairOverrides[s] = c
		}
	}
	return pc, nil
}

func (p *portal) handleAutotradeStart(w http.ResponseWriter, r *http.Request) {
	var req autotradeStartRequest
	if !decodeBody(w, r, &req) {
		return
	}
	pc, err := p.portfolioFromRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_symbol", err.Error()+" — bot KHÔNG bật")
		return
	}
	if err := p.markets.both(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "no_credentials", "thiếu credential, bot không bật: "+err.Error())
		return
	}
	st, err := p.autotrade.Start(pc)
	if err != nil {
		writeAutotradeError(w, err, " — bot KHÔNG bật")
		return
	}
	writeJSON(w, http.StatusOK, autotradeActionView{Status: st})
}

// writeAutotradeError maps an engine refusal to a status.
func writeAutotradeError(w http.ResponseWriter, err error, suffixVI string) {
	code, status := "autotrade_refused", http.StatusBadRequest
	switch {
	case errors.Is(err, autotrade.ErrBusy), errors.Is(err, autotrade.ErrNotStartable),
		errors.Is(err, autotrade.ErrHaltedWhileStopping), errors.Is(err, autotrade.ErrStaleAcknowledgement):
		code, status = "autotrade_conflict", http.StatusConflict
	case errors.Is(err, autotrade.ErrUnknownSymbol):
		code = "bad_symbol"
	}
	writeError(w, status, code, err.Error()+suffixVI)
}

type autotradeStopRequest struct {
	CloseNow bool `json:"close_now"`
	// HaltSeq is the status's halt_seq as the page showed it when the operator
	// pressed. Required: a stop acknowledges halts, and only the ones read.
	HaltSeq *int `json:"halt_seq"`
}

func (p *portal) handleAutotradeStop(w http.ResponseWriter, r *http.Request) {
	var req autotradeStopRequest
	if !decodeBody(w, r, &req) {
		return
	}
	// The route accepts both names; the body must agree with the one sent, so
	// the no-dialog name can never carry a close.
	if want := map[bool]string{false: autotradeStopAction, true: autotradeStopCloseAction}[req.CloseNow]; r.Header.Get(actionHeader) != want {
		writeError(w, http.StatusForbidden, "action_header_mismatch",
			"close_now="+fmt.Sprint(req.CloseNow)+" cần header "+actionHeader+": "+want+" — dừng-và-đóng gửi lệnh và chỉ đi sau hộp xác nhận")
		return
	}
	if req.HaltSeq == nil || *req.HaltSeq < 0 {
		writeError(w, http.StatusBadRequest, "halt_seq_required",
			"thiếu halt_seq — lệnh dừng xác nhận các DỪNG BẢO VỆ, nên phải nói trang đã hiển thị tới DỪNG BẢO VỆ số mấy")
		return
	}
	// Detached from the request: a tab closed half-way through a close must not
	// cancel the close (actions.go).
	st, out, err := p.autotrade.Stop(context.WithoutCancel(r.Context()), req.CloseNow, *req.HaltSeq)
	if err != nil {
		writeAutotradeError(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, autotradeActionView{Status: st, Closes: out.Closes, KeptIntentIDs: out.KeptIntentIDs})
}

type autotradeKillRequest struct{}

func (p *portal) handleAutotradeKill(w http.ResponseWriter, r *http.Request) {
	var req autotradeKillRequest
	if !decodeBody(w, r, &req) {
		return
	}
	st, closes, err := p.autotrade.Kill(context.WithoutCancel(r.Context()))
	if err != nil {
		writeAutotradeError(w, err, "")
		return
	}
	log.Printf("execportal: AUTOTRADE KILL → %s %s", st.State, st.HaltReasonVI)
	writeJSON(w, http.StatusOK, autotradeActionView{Status: st, Closes: closes})
}

type autotradeClosePairRequest struct {
	Symbol string `json:"symbol"`
}

// handleAutotradeClosePair closes the bot's position on ONE symbol and pauses
// that pair; every other pair keeps running.
func (p *portal) handleAutotradeClosePair(w http.ResponseWriter, r *http.Request) {
	var req autotradeClosePairRequest
	if !decodeBody(w, r, &req) {
		return
	}
	symbol, err := p.allowedSymbol(req.Symbol)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_symbol", err.Error()+" — không đóng gì")
		return
	}
	if err := p.markets.both(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "no_credentials", "thiếu credential, không đóng được: "+err.Error())
		return
	}
	st, oc, err := p.autotrade.ClosePair(context.WithoutCancel(r.Context()), symbol)
	if err != nil {
		writeAutotradeError(w, err, " — không đóng gì")
		return
	}
	log.Printf("execportal: AUTOTRADE CLOSE-PAIR %s → attempted=%v flat=%v %s", symbol, oc.Attempted, oc.Flat, oc.DetailVI)
	writeJSON(w, http.StatusOK, autotradeActionView{Status: st, Closes: []autotrade.OperatorClose{oc}})
}

type autotradePairRequest struct {
	Symbol string `json:"symbol"`
	Action string `json:"action"`
	// HaltSeq is the pair's halt number as the page showed it; an
	// acknowledgement of any other halt is refused.
	HaltSeq int `json:"halt_seq"`
}

var pairActionHeaders = map[autotrade.PairAction]string{
	autotrade.PairPause:       autotradePairPauseAction,
	autotrade.PairResume:      autotradePairResumeAction,
	autotrade.PairAcknowledge: autotradePairAckAction,
}

// handleAutotradePair pauses, resumes or acknowledges one pair. It never sends
// an order itself; RESUME lets the bot send them again.
func (p *portal) handleAutotradePair(w http.ResponseWriter, r *http.Request) {
	var req autotradePairRequest
	if !decodeBody(w, r, &req) {
		return
	}
	action := autotrade.PairAction(req.Action)
	want, known := pairActionHeaders[action]
	if !known {
		writeError(w, http.StatusBadRequest, "bad_action", "action "+quoteForMessage(req.Action)+" không phải pause, resume hay ack")
		return
	}
	if r.Header.Get(actionHeader) != want {
		writeError(w, http.StatusForbidden, "action_header_mismatch",
			"action="+string(action)+" cần header "+actionHeader+": "+want+" — tiếp tục một cặp cho bot đặt lệnh và chỉ đi sau hộp xác nhận")
		return
	}
	symbol, err := p.allowedSymbol(req.Symbol)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_symbol", err.Error())
		return
	}
	if action == autotrade.PairResume {
		if err := p.markets.both(); err != nil {
			writeError(w, http.StatusServiceUnavailable, "no_credentials", "thiếu credential, không tiếp tục cặp: "+err.Error())
			return
		}
	}
	st, err := p.autotrade.PairControl(symbol, action, req.HaltSeq)
	if err != nil {
		writeAutotradeError(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, autotradeActionView{Status: st})
}

// handleAutotradePnL is the auto-trader's result page: closed pairs from the
// intent cache, open pairs marked to mid with the funding the venue paid, the
// settlement bars and the equity samples (pnl.go).
func (p *portal) handleAutotradePnL(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readContext(r)
	defer cancel()
	writeJSON(w, http.StatusOK, p.buildPnL(ctx, p.autotrade.Status(), true))
}
