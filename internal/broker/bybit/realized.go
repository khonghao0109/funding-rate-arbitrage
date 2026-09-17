package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"futures-arbitrage-scanner/internal/broker"
)

// The optional capabilities internal/execution and cmd/execportal look for by
// type assertion or call by name — the same set binance.Client carries — so
// Strategy 1 can run on this venue's two markets (PLAN 4.5j). Every path and
// field is quoted from its page, docs repository master branch, read
// 2026-09-17; the two places a LIVE answer disagreed with the page are said
// where they are read.

var (
	_ broker.TradeReader     = (*Client)(nil)
	_ broker.FundingReader   = (*Client)(nil)
	_ broker.MarkPriceReader = (*Client)(nil)
)

// maxPages bounds every cursor walk. A list longer than this is refused, never
// returned partial: a partial list of fills or settlements reads as complete.
const maxPages = 50

// ------------------------------------------------------------------ fills

// OrderTrades implements broker.TradeReader.
//
// GET /v5/execution/list?category=…&symbol=…&orderLinkId=… — "orderId and
// orderLinkId have a higher priority and as long as these two parameters are in
// the input parameters, other input parameters will be ignored"; limit [1, 100].
// Each row: execId, orderId, side, execQty, execPrice, execFee "Executed trading
// fee", feeCurrency "Trading fee currency", isMaker, execTime (ms).
//
// # The fee's asset
//
// On SPOT the fee is taken in the asset received on a taker buy — "Side = Buy ->
// base currency (BTC)" — and feeCurrency names it; it travels unconverted
// (broker/realized.go). On LINEAR the page's own example answers feeCurrency ""
// while the fee is charged in the settle coin, and this package trades USDT-settled
// linear only (FetchInstrument refuses anything else), so a blank linear
// feeCurrency is reported as USDT. A blank SPOT feeCurrency is left blank, which
// execution reports as "the venue stated none" rather than as zero.
//
// # "No fills" versus "fills not visible"
//
// The page also says "startTime and endTime are not passed, return 7 days by
// default". Whether an orderLinkId lifts that window is not stated beyond the
// sentence above, so the list is cross-checked against the order itself EVERY
// time: fills summing to other than the order's cumExecQty — none listed for an
// order that filled, or a window that cut some of them — is an error, never a
// partial list that would sum to too little commission.
func (c *Client) OrderTrades(ctx context.Context, q broker.OrderQuery) ([]broker.Trade, error) {
	if err := c.checkQuery(q); err != nil {
		return nil, err
	}
	params := c.identify(q)
	params = append(params, broker.Param{Key: "limit", Value: "100"})
	var out []broker.Trade
	cursor := ""
	for page := 0; ; page++ {
		if page == maxPages {
			return nil, fmt.Errorf("bybit: more than %d pages of fills for one order — refused rather than returned partial", maxPages)
		}
		p := params
		if cursor != "" {
			p = append(append([]broker.Param{}, params...), broker.Param{Key: "cursor", Value: cursor})
		}
		var res struct {
			List []struct {
				Symbol      string `json:"symbol"`
				OrderID     string `json:"orderId"`
				OrderLinkID string `json:"orderLinkId"`
				Side        string `json:"side"`
				ExecID      string `json:"execId"`
				ExecPrice   string `json:"execPrice"`
				ExecQty     string `json:"execQty"`
				ExecFee     string `json:"execFee"`
				FeeCurrency string `json:"feeCurrency"`
				ExecTime    string `json:"execTime"`
				IsMaker     bool   `json:"isMaker"`
			} `json:"list"`
			NextPageCursor string `json:"nextPageCursor"`
		}
		if err := c.getSigned(ctx, epExecutionList, p, &res); err != nil {
			return nil, err
		}
		for _, r := range res.List {
			if (q.ClientOrderID != "" && r.OrderLinkID != q.ClientOrderID) ||
				(q.ClientOrderID == "" && r.OrderID != q.VenueOrderID) {
				return nil, fmt.Errorf("bybit: /v5/execution/list answered a fill of another order (%s) — refused rather than summed", r.OrderID)
			}
			t := broker.Trade{Market: c.market, Symbol: r.Symbol, TradeID: r.ExecID, VenueOrderID: r.OrderID,
				IsMaker: r.IsMaker, CommissionAsset: strings.TrimSpace(r.FeeCurrency)}
			switch r.Side {
			case "Buy":
				t.Side = broker.SideBuy
			case "Sell":
				t.Side = broker.SideSell
			default:
				return nil, fmt.Errorf("bybit: fill %s has side %q", r.ExecID, r.Side)
			}
			var err error
			if t.QtyCoin, err = parseRequiredNumber("execQty", r.ExecQty); err != nil {
				return nil, err
			}
			if t.PriceQuote, err = parseRequiredNumber("execPrice", r.ExecPrice); err != nil {
				return nil, err
			}
			if t.CommissionQtyInAsset, err = parseRequiredNumber("execFee", r.ExecFee); err != nil {
				return nil, err
			}
			if t.TimeMs, err = parseMs("execTime", r.ExecTime); err != nil {
				return nil, err
			}
			if t.CommissionAsset == "" && c.market == broker.MarketFuturesUSDM {
				t.CommissionAsset = settleCoinUSDT
			}
			out = append(out, t)
		}
		if res.NextPageCursor == "" {
			break
		}
		cursor = res.NextPageCursor
	}
	o, err := c.GetOrder(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("bybit: %d fill(s) listed and the order could not be read to confirm they are all of them: %w", len(out), err)
	}
	listedQtyCoin := 0.0
	for _, t := range out {
		listedQtyCoin += t.QtyCoin
	}
	if math.Abs(listedQtyCoin-o.FilledQtyCoin) > 1e-9*math.Max(1, o.FilledQtyCoin) {
		return nil, fmt.Errorf("bybit: the order filled %v coin but /v5/execution/list lists %v — refused rather than summing a partial list", o.FilledQtyCoin, listedQtyCoin)
	}
	return out, nil
}

// ---------------------------------------------------------------- funding

// fundingLogWindow is the transaction log's widest request: "If both are
// passed, the rule is endTime - startTime <= 7 days".
const fundingLogWindow = 7 * 24 * time.Hour

// FundingIncome implements broker.FundingReader.
//
// GET /v5/account/transaction-log?accountType=UNIFIED&category=linear&currency=USDT&type=SETTLEMENT
// — "SETTLEMENT  USDT Perp funding settlement"; each row's funding "Positive fee
// value means receive funding; negative fee value means pay funding", which is
// broker.FundingIncome's sign exactly. The request takes no symbol, so rows of
// other symbols are dropped here. A window wider than seven days is walked in
// seven-day pieces, each paged by cursor (limit [1, 50]).
//
// Both ends are REQUIRED: with none the venue answers "24 hours by default", and
// a caller asking for a holding period would silently get a day of it.
func (c *Client) FundingIncome(ctx context.Context, market broker.Market, symbol string, startMs, endMs int64) ([]broker.FundingIncome, error) {
	if market != broker.MarketFuturesUSDM {
		return nil, fmt.Errorf("%w: %q settles no funding", broker.ErrNotSupported, market)
	}
	if err := c.checkMarket(market); err != nil {
		return nil, err
	}
	if symbol == "" || startMs <= 0 || endMs < startMs {
		return nil, fmt.Errorf("%w: FundingIncome needs a symbol and a window with both ends (got %d..%d)", broker.ErrInvalidOrder, startMs, endMs)
	}
	var out []broker.FundingIncome
	seen := map[string]bool{}
	step := fundingLogWindow.Milliseconds()
	for from := startMs; from <= endMs; from += step + 1 {
		to := min(from+step, endMs)
		base := []broker.Param{
			{Key: "accountType", Value: "UNIFIED"}, {Key: "category", Value: categoryLinear},
			{Key: "currency", Value: settleCoinUSDT}, {Key: "type", Value: "SETTLEMENT"},
			{Key: "startTime", Value: strconv.FormatInt(from, 10)}, {Key: "endTime", Value: strconv.FormatInt(to, 10)},
			{Key: "limit", Value: "50"},
		}
		cursor := ""
		for page := 0; ; page++ {
			if page == maxPages {
				return nil, fmt.Errorf("bybit: more than %d pages of settlements in one week — refused rather than returned partial", maxPages)
			}
			p := base
			if cursor != "" {
				p = append(append([]broker.Param{}, base...), broker.Param{Key: "cursor", Value: cursor})
			}
			var res struct {
				List []struct {
					ID              string `json:"id"`
					Symbol          string `json:"symbol"`
					Type            string `json:"type"`
					Currency        string `json:"currency"`
					Funding         string `json:"funding"`
					TransactionTime string `json:"transactionTime"`
				} `json:"list"`
				NextPageCursor string `json:"nextPageCursor"`
			}
			if err := c.getSigned(ctx, epTransactionLog, p, &res); err != nil {
				return nil, err
			}
			for _, r := range res.List {
				// The filter is the venue's job; a row of another type or
				// symbol summed into funding would be silent and wrong, so it
				// is dropped here too rather than trusted twice.
				if r.Type != "SETTLEMENT" || r.Symbol != symbol {
					continue
				}
				// Two windows share their boundary millisecond only if a row
				// lands exactly on it; the id keeps it counted once.
				if seen[r.ID] {
					continue
				}
				seen[r.ID] = true
				income, err := parseRequiredNumber("funding", r.Funding)
				if err != nil {
					return nil, err
				}
				ts, err := parseMs("transactionTime", r.TransactionTime)
				if err != nil || ts <= 0 {
					return nil, fmt.Errorf("bybit: a %s settlement carries no usable transactionTime", symbol)
				}
				out = append(out, broker.FundingIncome{Market: c.market, Symbol: symbol,
					IncomeQuote: income, Asset: r.Currency, SettledAtMs: ts, TranID: r.ID})
			}
			if res.NextPageCursor == "" {
				break
			}
			cursor = res.NextPageCursor
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SettledAtMs < out[j].SettledAtMs })
	return out, nil
}

// MarkPrice implements broker.MarkPriceReader.
//
// GET /v5/market/tickers?category=linear&symbol=… — markPrice, indexPrice,
// fundingRate "Funding rate", nextFundingTime "Next funding time (ms)". The
// ticker's fundingRate is the rate that will apply at nextFundingTime — the
// FORMING rate, not a settled one (the settled ones are FundingRateHistory) —
// and it is carried in LastFundingRateFrac as binance.Client carries
// premiumIndex's lastFundingRate, which is the same kind of figure. VenueTimeMs
// is left 0: the ticker row states no time of its own.
func (c *Client) MarkPrice(ctx context.Context, market broker.Market, symbol string) (broker.MarkPrice, error) {
	if market != broker.MarketFuturesUSDM {
		return broker.MarkPrice{}, fmt.Errorf("%w: %q publishes no mark price", broker.ErrNotSupported, market)
	}
	if err := c.checkMarket(market); err != nil {
		return broker.MarkPrice{}, err
	}
	var res struct {
		List []struct {
			Symbol          string `json:"symbol"`
			MarkPrice       string `json:"markPrice"`
			IndexPrice      string `json:"indexPrice"`
			FundingRate     string `json:"fundingRate"`
			NextFundingTime string `json:"nextFundingTime"`
		} `json:"list"`
	}
	if err := c.getPublic(ctx, epTickers, []broker.Param{
		{Key: "category", Value: categoryLinear}, {Key: "symbol", Value: symbol},
	}, &res); err != nil {
		return broker.MarkPrice{}, err
	}
	for _, r := range res.List {
		if r.Symbol != symbol {
			continue
		}
		out := broker.MarkPrice{Market: c.market, Symbol: symbol}
		var err error
		if out.MarkPriceQuote, err = parseRequiredNumber("markPrice", r.MarkPrice); err != nil {
			return broker.MarkPrice{}, err
		}
		if out.IndexPriceQuote, err = parseNumber("indexPrice", r.IndexPrice); err != nil {
			return broker.MarkPrice{}, err
		}
		if out.LastFundingRateFrac, err = parseRequiredNumber("fundingRate", r.FundingRate); err != nil {
			return broker.MarkPrice{}, err
		}
		if out.NextFundingTimeMs, err = parseMs("nextFundingTime", r.NextFundingTime); err != nil {
			return broker.MarkPrice{}, err
		}
		return out, nil
	}
	return broker.MarkPrice{}, fmt.Errorf("bybit: tickers lists no linear symbol %q", symbol)
}

// FundingRateHistoryRange returns every SETTLED rate in [startMs, endMs], oldest
// first, walking FundingRateHistory backwards from endMs — the endpoint is
// anchored on endTime ("Passing only endTime returns 200 records up till
// endTime"), which is the Bybit candle-paging trap in CLAUDE.md applied to
// funding: walking a start forward re-reads the newest page.
func (c *Client) FundingRateHistoryRange(ctx context.Context, symbol string, startMs, endMs int64) ([]FundingRate, error) {
	if startMs <= 0 || endMs < startMs {
		return nil, fmt.Errorf("%w: funding history window %d..%d", broker.ErrInvalidOrder, startMs, endMs)
	}
	var out []FundingRate
	cursorEnd := endMs
	for page := 0; ; page++ {
		if page == maxPages {
			return nil, fmt.Errorf("bybit: more than %d pages of funding history — refused rather than returned partial", maxPages)
		}
		batch, err := c.FundingRateHistory(ctx, symbol, cursorEnd, 200)
		if err != nil {
			return nil, err
		}
		oldest := int64(0)
		for _, r := range batch {
			if r.SettledAtMs >= startMs && r.SettledAtMs <= endMs {
				out = append(out, r)
			}
			if oldest == 0 || r.SettledAtMs < oldest {
				oldest = r.SettledAtMs
			}
		}
		if len(batch) < 200 || oldest <= startMs {
			break
		}
		cursorEnd = oldest - 1
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SettledAtMs < out[j].SettledAtMs })
	// The pages overlap by nothing (cursorEnd = oldest - 1), so a duplicate
	// here would mean the venue answered a settlement twice.
	for i := 1; i < len(out); i++ {
		if out[i].SettledAtMs == out[i-1].SettledAtMs {
			return nil, fmt.Errorf("bybit: settlement %d listed twice for %s", out[i].SettledAtMs, symbol)
		}
	}
	return out, nil
}

// ----------------------------------------------------------------- fees

// CommissionRates is this account's taker fee on one symbol of one market, as
// FRACTIONS. Bybit publishes one taker rate for both sides.
type CommissionRates struct {
	Market        broker.Market
	Symbol        string
	TakerBuyFrac  float64
	TakerSellFrac float64
	SourceVI      string
}

// TakerBps is the dearer side in basis points.
func (r CommissionRates) TakerBps() float64 { return max(r.TakerBuyFrac, r.TakerSellFrac) * 10_000 }

// CommissionRates reads GET /v5/account/fee-rate?category=…&symbol=… —
// takerFeeRate "Taker fee rate". A rate that is blank, negative or ≥ 1 is
// refused, never read as free: a taker REBATE is not something this account is
// assumed to get.
func (c *Client) CommissionRates(ctx context.Context, symbol string) (CommissionRates, error) {
	if symbol == "" {
		return CommissionRates{}, fmt.Errorf("%w: no symbol", broker.ErrInvalidOrder)
	}
	var res struct {
		List []struct {
			Symbol       string `json:"symbol"`
			TakerFeeRate string `json:"takerFeeRate"`
		} `json:"list"`
	}
	if err := c.getSigned(ctx, epFeeRate, []broker.Param{
		{Key: "category", Value: c.category()}, {Key: "symbol", Value: symbol},
	}, &res); err != nil {
		return CommissionRates{}, err
	}
	for _, r := range res.List {
		if r.Symbol != symbol {
			continue
		}
		taker, err := strconv.ParseFloat(strings.TrimSpace(r.TakerFeeRate), 64)
		if err != nil || taker < 0 || taker >= 1 {
			return CommissionRates{}, fmt.Errorf("bybit: takerFeeRate of %s is not a rate in [0, 1) — not read as free", symbol)
		}
		return CommissionRates{Market: c.market, Symbol: symbol, TakerBuyFrac: taker, TakerSellFrac: taker,
			SourceVI: fmt.Sprintf("GET /v5/account/fee-rate?category=%s của tài khoản Bybit %s (takerFeeRate)", c.category(), c.mode)}, nil
	}
	return CommissionRates{}, fmt.Errorf("bybit: fee-rate lists no %s symbol %q", c.category(), symbol)
}

// ------------------------------------------------------------ maintenance

// MaintenanceBracket is the risk-limit tier covering one notional.
type MaintenanceBracket struct {
	Symbol             string
	Tier               int
	NotionalFloorQuote float64
	NotionalCapQuote   float64
	MaintMarginFrac    float64
	MaxLeverage        float64
}

// FetchMaintenanceBracket reads GET /v5/market/risk-limit?category=linear&symbol=…
// — riskLimitValue "Position limit", maintenanceMargin "Maintain margin rate",
// maxLeverage — and returns the lowest tier whose limit covers the notional. The
// floor is the previous tier's limit.
//
// # The unit of maintenanceMargin was MEASURED, not taken from the page
//
// The page types it "number" and its only example is an INVERSE contract
// answering "0.5" beside maxLeverage "100.00" — which reads as a PERCENT. The
// linear testnet answered STRINGS, all 35 BTCUSDT tiers on 2026-09-17, from
// "0.0033" / initialMargin "0.0066" / maxLeverage "150.00" down to "0.6" / "1" /
// "1.00": FRACTIONS, with initialMargin × maxLeverage = 1 on every tier.
//
// So the unit is not bounded by a guessed ceiling — a first version refused
// anything ≥ 0.5 and the live run refused the venue's own last tier — it is
// checked by that identity on EVERY tier: 0 < maintenance ≤ initial ≤ 1 and
// initial × maxLeverage within 5% of 1. A venue answering in percent gives
// ≈ 100 and is refused rather than read as a hundred times the margin.
//
// mmDeduction ("The maintenance margin deduction value when risk limit tier
// changed") is NOT subtracted: the requirement above tier 1 is
// notional × rate − deduction, so using the rate alone overstates it, which is
// the safe direction for a liquidation guard. Tier 1's deduction is "".
func (c *Client) FetchMaintenanceBracket(ctx context.Context, symbol string, notionalQuote float64) (MaintenanceBracket, error) {
	if c.market != broker.MarketFuturesUSDM {
		return MaintenanceBracket{}, fmt.Errorf("%w: %q publishes no maintenance schedule", broker.ErrNotSupported, c.market)
	}
	type tier struct {
		ID                int             `json:"id"`
		Symbol            string          `json:"symbol"`
		RiskLimitValue    string          `json:"riskLimitValue"`
		MaintenanceMargin json.RawMessage `json:"maintenanceMargin"`
		InitialMargin     json.RawMessage `json:"initialMargin"`
		MaxLeverage       string          `json:"maxLeverage"`
	}
	var tiers []tier
	cursor := ""
	for page := 0; ; page++ {
		if page == maxPages {
			return MaintenanceBracket{}, fmt.Errorf("bybit: more than %d pages of risk limits — refused", maxPages)
		}
		params := []broker.Param{{Key: "category", Value: categoryLinear}, {Key: "symbol", Value: symbol}}
		if cursor != "" {
			params = append(params, broker.Param{Key: "cursor", Value: cursor})
		}
		var res struct {
			List           []tier `json:"list"`
			NextPageCursor string `json:"nextPageCursor"`
		}
		if err := c.getPublic(ctx, epRiskLimit, params, &res); err != nil {
			return MaintenanceBracket{}, err
		}
		for _, t := range res.List {
			if t.Symbol == symbol {
				tiers = append(tiers, t)
			}
		}
		if res.NextPageCursor == "" {
			break
		}
		cursor = res.NextPageCursor
	}
	type parsed struct {
		id           int
		capQuote     float64
		mmFrac, maxX float64
	}
	rows := make([]parsed, 0, len(tiers))
	for _, t := range tiers {
		capQuote, err := parseRequiredNumber("riskLimitValue", t.RiskLimitValue)
		if err != nil {
			return MaintenanceBracket{}, err
		}
		mm, err := parseRequiredNumber("maintenanceMargin", strings.Trim(string(t.MaintenanceMargin), `"`))
		if err != nil {
			return MaintenanceBracket{}, err
		}
		im, err := parseRequiredNumber("initialMargin", strings.Trim(string(t.InitialMargin), `"`))
		if err != nil {
			return MaintenanceBracket{}, err
		}
		maxX, err := parseRequiredNumber("maxLeverage", t.MaxLeverage)
		if err != nil {
			return MaintenanceBracket{}, err
		}
		if !(mm > 0 && mm <= im && im <= 1 && maxX > 0 && math.Abs(im*maxX-1) <= 0.05) {
			return MaintenanceBracket{}, fmt.Errorf("bybit: %s tier %d answers maintenance %v, initial %v, max leverage %v — not fractions with initial × leverage = 1; refused rather than guessed as a percent",
				symbol, t.ID, mm, im, maxX)
		}
		rows = append(rows, parsed{id: t.ID, capQuote: capQuote, mmFrac: mm, maxX: maxX})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].capQuote < rows[j].capQuote })
	floor := 0.0
	for _, r := range rows {
		if notionalQuote <= r.capQuote {
			return MaintenanceBracket{Symbol: symbol, Tier: r.id, NotionalFloorQuote: floor, NotionalCapQuote: r.capQuote,
				MaintMarginFrac: r.mmFrac, MaxLeverage: r.maxX}, nil
		}
		floor = r.capQuote
	}
	return MaintenanceBracket{}, fmt.Errorf("bybit: %s publishes no risk-limit tier covering a notional of %v", symbol, notionalQuote)
}
