package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"futures-arbitrage-scanner/internal/broker"
)

// The three reads step 4.5 needs and step 4.2 did not have: what a fill cost in
// commission, what the settlements actually paid, and when the next one is.
//
// They are OPTIONAL capabilities (broker.TradeReader, broker.FundingReader,
// broker.MarkPriceReader) rather than methods on broker.Broker, because they do
// not exist on every market: spot has no funding and no mark price, and it says
// so with ErrNotSupported rather than answering an empty list. "Nothing was
// paid" and "I cannot tell you what was paid" are different facts, and only one
// of them may be added into a realized figure.

var (
	_ broker.TradeReader     = (*Client)(nil)
	_ broker.FundingReader   = (*Client)(nil)
	_ broker.MarkPriceReader = (*Client)(nil)
)

// venueTrade is the union of the two venues' fill shapes.
//
//	futures  https://developers.binance.com/docs/derivatives/usds-margined-futures/trade/rest-api/Account-Trade-List
//	spot     https://developers.binance.com/docs/binance-spot-api-docs/rest-api/account-endpoints
//
// They agree on symbol/id/orderId/price/qty/commission/commissionAsset/time and
// disagree on how the side is stated: futures publishes `side` ("BUY"/"SELL")
// and `maker`, spot publishes `isBuyer` and `isMaker`. Both members of each
// pair are declared, including the one not used, for the reason CLAUDE.md's
// aggTrade `m`/`M` row gives: Go's JSON decoder falls back to a
// case-insensitive match, so an undeclared sibling can overwrite its partner.
type venueTrade struct {
	Symbol  string `json:"symbol"`
	ID      int64  `json:"id"`
	OrderID int64  `json:"orderId"`

	Price string `json:"price"`
	Qty   string `json:"qty"`

	Commission      string `json:"commission"`
	CommissionAsset string `json:"commissionAsset"`

	// futures
	Side  string `json:"side"`
	Maker bool   `json:"maker"`
	Buyer bool   `json:"buyer"`

	// spot
	IsBuyer bool `json:"isBuyer"`
	IsMaker bool `json:"isMaker"`

	Time int64 `json:"time"`
}

// OrderTrades implements broker.TradeReader: the fills of ONE order.
//
// The order is identified by its VENUE id, because that is the only handle
// either trades endpoint takes — neither accepts a clientOrderId. A query that
// carries only the caller's id is therefore resolved through GetOrder first,
// which costs one extra weight-1 read and keeps the caller's handle working
// everywhere in this package.
func (c *Client) OrderTrades(ctx context.Context, q broker.OrderQuery) ([]broker.Trade, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if q.Market != c.market {
		return nil, fmt.Errorf("%w: this client speaks to %q, the query names %q", broker.ErrInvalidOrder, c.market, q.Market)
	}
	venueOrderID := q.VenueOrderID
	if venueOrderID == "" {
		order, err := c.GetOrder(ctx, q)
		if err != nil {
			return nil, err
		}
		venueOrderID = order.VenueOrderID
	}
	if venueOrderID == "" {
		return nil, fmt.Errorf("%w: the venue named no orderId for %s", broker.ErrOrderNotFound, q.Symbol)
	}

	ep := broker.FuturesUserTrades
	if c.market == broker.MarketSpot {
		ep = broker.SpotMyTrades
	}
	var raw json.RawMessage
	if err := c.http.GetSigned(ctx, ep, []broker.Param{
		{Key: "symbol", Value: q.Symbol},
		{Key: "orderId", Value: venueOrderID},
	}, &raw); err != nil {
		return nil, classify(err)
	}
	var list []venueTrade
	if err := decode(raw, &list); err != nil {
		return nil, err
	}
	out := make([]broker.Trade, 0, len(list))
	for _, vt := range list {
		out = append(out, c.toTrade(vt))
	}
	return out, nil
}

func (c *Client) toTrade(vt venueTrade) broker.Trade {
	side := broker.Side(vt.Side)
	isMaker := vt.Maker
	if c.market == broker.MarketSpot {
		side = broker.SideSell
		if vt.IsBuyer {
			side = broker.SideBuy
		}
		isMaker = vt.IsMaker
	}
	return broker.Trade{
		Market:               c.market,
		Symbol:               vt.Symbol,
		TradeID:              strconv.FormatInt(vt.ID, 10),
		VenueOrderID:         strconv.FormatInt(vt.OrderID, 10),
		Side:                 side,
		QtyCoin:              parseFloat(vt.Qty),
		PriceQuote:           parseFloat(vt.Price),
		CommissionQtyInAsset: parseFloat(vt.Commission),
		CommissionAsset:      vt.CommissionAsset,
		IsMaker:              isMaker,
		TimeMs:               vt.Time,
	}
}

// venueIncome is one row of GET /fapi/v1/income.
type venueIncome struct {
	Symbol     string `json:"symbol"`
	IncomeType string `json:"incomeType"`
	Income     string `json:"income"`
	Asset      string `json:"asset"`
	Info       string `json:"info"`
	Time       int64  `json:"time"`
	TranID     int64  `json:"tranId"`
	TradeID    string `json:"tradeId"`
}

// FundingIncome implements broker.FundingReader.
//
// It asks for incomeType=FUNDING_FEE over an explicit window and returns what
// the venue lists. Each row IS one settlement that really happened, which is
// what CLAUDE.md rule 6 requires: nothing here multiplies a rate by a holding
// time, and a position that was not open at a settlement simply has no row.
//
// The sign is the venue's. A short perp leg RECEIVES funding when the rate is
// positive — the whole strategy — and PAYS it when the rate turns, so taking an
// absolute value would turn a cost into a profit.
func (c *Client) FundingIncome(ctx context.Context, market broker.Market, symbol string, startMs, endMs int64) ([]broker.FundingIncome, error) {
	if market != broker.MarketFuturesUSDM {
		return nil, fmt.Errorf("%w: %q settles no funding", broker.ErrNotSupported, market)
	}
	if market != c.market {
		return nil, fmt.Errorf("%w: this client speaks to %q", broker.ErrInvalidOrder, c.market)
	}
	if symbol == "" {
		return nil, fmt.Errorf("%w: no symbol", broker.ErrInvalidOrder)
	}
	params := []broker.Param{
		{Key: "symbol", Value: symbol},
		{Key: "incomeType", Value: "FUNDING_FEE"},
		{Key: "limit", Value: "1000"},
	}
	if startMs > 0 {
		params = append(params, broker.Param{Key: "startTime", Value: strconv.FormatInt(startMs, 10)})
	}
	if endMs > 0 {
		params = append(params, broker.Param{Key: "endTime", Value: strconv.FormatInt(endMs, 10)})
	}
	var raw json.RawMessage
	if err := c.http.GetSigned(ctx, broker.FuturesIncome, params, &raw); err != nil {
		return nil, classify(err)
	}
	var list []venueIncome
	if err := decode(raw, &list); err != nil {
		return nil, err
	}
	out := make([]broker.FundingIncome, 0, len(list))
	for _, vi := range list {
		if vi.IncomeType != "FUNDING_FEE" {
			// The filter is the venue's job and it does it, but a row of
			// another type summed into a funding figure would be silent and
			// wrong, so it is dropped here too rather than trusted twice.
			continue
		}
		out = append(out, broker.FundingIncome{
			Market:      c.market,
			Symbol:      vi.Symbol,
			IncomeQuote: parseFloat(vi.Income),
			Asset:       vi.Asset,
			SettledAtMs: vi.Time,
			TranID:      strconv.FormatInt(vi.TranID, 10),
		})
	}
	return out, nil
}

// venuePremiumIndex is GET /fapi/v1/premiumIndex for one symbol.
type venuePremiumIndex struct {
	Symbol               string `json:"symbol"`
	MarkPrice            string `json:"markPrice"`
	IndexPrice           string `json:"indexPrice"`
	LastFundingRate      string `json:"lastFundingRate"`
	InterestRate         string `json:"interestRate"`
	NextFundingTime      int64  `json:"nextFundingTime"`
	EstimatedSettlePrice string `json:"estimatedSettlePrice"`
	Time                 int64  `json:"time"`
}

// MarkPrice implements broker.MarkPriceReader.
//
// The field that matters for 4.5 is nextFundingTime: a lifecycle that has to
// hold a position ACROSS a settlement cannot assume the cadence (rule 3 — not
// 8h, not anything), and this endpoint states the next stamp outright. The
// INTERVAL is deliberately not derived from it: one stamp does not give a
// period, and Binance runs 8h, 4h and 1h per symbol.
func (c *Client) MarkPrice(ctx context.Context, market broker.Market, symbol string) (broker.MarkPrice, error) {
	if market != broker.MarketFuturesUSDM {
		return broker.MarkPrice{}, fmt.Errorf("%w: %q publishes no mark price", broker.ErrNotSupported, market)
	}
	if market != c.market {
		return broker.MarkPrice{}, fmt.Errorf("%w: this client speaks to %q", broker.ErrInvalidOrder, c.market)
	}
	if symbol == "" {
		return broker.MarkPrice{}, fmt.Errorf("%w: no symbol", broker.ErrInvalidOrder)
	}
	var raw json.RawMessage
	if err := c.http.GetPublic(ctx, broker.FuturesPremiumIndex, []broker.Param{{Key: "symbol", Value: symbol}}, &raw); err != nil {
		return broker.MarkPrice{}, classify(err)
	}
	var vp venuePremiumIndex
	if err := decode(raw, &vp); err != nil {
		return broker.MarkPrice{}, err
	}
	return broker.MarkPrice{
		Market:              c.market,
		Symbol:              vp.Symbol,
		MarkPriceQuote:      parseFloat(vp.MarkPrice),
		IndexPriceQuote:     parseFloat(vp.IndexPrice),
		LastFundingRateFrac: parseFloat(vp.LastFundingRate),
		NextFundingTimeMs:   vp.NextFundingTime,
		VenueTimeMs:         vp.Time,
	}, nil
}
