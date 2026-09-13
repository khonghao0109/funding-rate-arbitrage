// Package binance implements broker.Broker against Binance TESTNET.
//
// Two markets, two clients, two credentials: USDⓈ-M futures on
// demo-fapi.binance.com and spot on testnet.binance.vision. They are separate
// systems with separate registrations — measured 2026-09-13, a futures key sent
// to the spot host is refused -2015 — so a Client is built per market and holds
// exactly one of them.
//
// It cannot reach a mainnet host. broker.NewClient refuses any base URL outside
// the documented testnet allow-list, there is no flag to widen it before step
// 4.6, and a test reads this package's own source for one.
//
// Every endpoint path, weight, parameter name and error code used here is
// quoted from Binance's documentation with the URL beside it — in
// broker/endpoints.go for the paths and weights, in errors.go for the codes.
// Where a page could not be read from this environment the number carries an
// UNVERIFIED label and a conservative value, rather than a guess (rule 5).
package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"futures-arbitrage-scanner/internal/broker"
)

// Client is one market's broker.Broker.
type Client struct {
	http   *broker.Client
	market broker.Market
	eps    endpoints
}

var _ broker.Broker = (*Client)(nil)

// endpoints is the per-market table, so no method has to branch on the market.
type endpoints struct {
	newOrder    broker.Endpoint
	queryOrder  broker.Endpoint
	cancelOrder broker.Endpoint
	openOrders  broker.Endpoint
	account     broker.Endpoint
}

var (
	futuresEndpoints = endpoints{
		newOrder:    broker.FuturesNewOrder,
		queryOrder:  broker.FuturesQueryOrder,
		cancelOrder: broker.FuturesCancelOrder,
		openOrders:  broker.FuturesOpenOrders,
		account:     broker.FuturesAccount,
	}
	spotEndpoints = endpoints{
		newOrder:    broker.SpotNewOrder,
		queryOrder:  broker.SpotQueryOrder,
		cancelOrder: broker.SpotCancelOrder,
		openOrders:  broker.SpotOpenOrders,
		account:     broker.SpotAccount,
	}
)

// DefaultConfig fills in the host, clock path and weight ceiling for a market,
// so a caller supplies only the credential. It exists to stop those three from
// being typed out — and mismatched — at each call site.
func DefaultConfig(market broker.Market, creds broker.Credentials) (broker.Config, error) {
	switch market {
	case broker.MarketFuturesUSDM:
		return broker.Config{
			BaseURL:           broker.BinanceFuturesTestnetBaseURL,
			Credentials:       creds,
			TimePath:          broker.BinanceFuturesTimePath,
			WeightLimitPerMin: broker.BinanceFuturesWeightPerMin,
		}, nil
	case broker.MarketSpot:
		return broker.Config{
			BaseURL:           broker.BinanceSpotTestnetBaseURL,
			Credentials:       creds,
			TimePath:          broker.BinanceSpotTimePath,
			WeightLimitPerMin: broker.BinanceSpotWeightPerMin,
		}, nil
	}
	return broker.Config{}, fmt.Errorf("binance: no configuration for market %q", market)
}

// New builds a Client for one market.
func New(market broker.Market, cfg broker.Config) (*Client, error) {
	var eps endpoints
	switch market {
	case broker.MarketFuturesUSDM:
		eps = futuresEndpoints
	case broker.MarketSpot:
		eps = spotEndpoints
	default:
		return nil, fmt.Errorf("binance: market %q is neither %q nor %q", market, broker.MarketFuturesUSDM, broker.MarketSpot)
	}
	c, err := broker.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{http: c, market: market, eps: eps}, nil
}

// Market is which market this client speaks to.
func (c *Client) Market() broker.Market { return c.market }

// HTTP exposes the underlying signed client, for the clock and the weight
// budget a diagnostic wants to print.
func (c *Client) HTTP() *broker.Client { return c.http }

// PlaceOrder implements broker.Broker.
//
// Parameters travel in the REQUEST BODY on both venues, as both New Order pages
// document. That also keeps the signature off the URL, and so out of reach of
// every URL-printing error path in Go.
func (c *Client) PlaceOrder(ctx context.Context, req broker.PlaceOrderRequest) (broker.Order, error) {
	if err := req.Validate(); err != nil {
		return broker.Order{}, err
	}
	if req.Market != c.market {
		return broker.Order{}, fmt.Errorf("%w: this client speaks to %q, the request names %q", broker.ErrInvalidOrder, c.market, req.Market)
	}

	params := []broker.Param{
		{Key: "symbol", Value: req.Symbol},
		{Key: "side", Value: string(req.Side)},
	}
	switch req.Type {
	case broker.OrderTypeLimitGTC:
		// "LIMIT: requires timeInForce, quantity, price".
		params = append(params,
			broker.Param{Key: "type", Value: "LIMIT"},
			broker.Param{Key: "timeInForce", Value: "GTC"},
			broker.Param{Key: "quantity", Value: trimNumber(req.QtyCoin)},
			broker.Param{Key: "price", Value: trimNumber(req.PriceQuote)},
		)
	case broker.OrderTypeMarket:
		// "MARKET: requires quantity".
		params = append(params,
			broker.Param{Key: "type", Value: "MARKET"},
			broker.Param{Key: "quantity", Value: trimNumber(req.QtyCoin)},
		)
	}
	// The caller's id, verbatim. Never generated here, never truncated: it is
	// the only handle the caller has after an ambiguous timeout.
	params = append(params, broker.Param{Key: "newClientOrderId", Value: req.ClientOrderID})
	if req.ReduceOnly {
		// Futures only, and PlaceOrderRequest.Validate has already refused it
		// anywhere else. The venue documents it as a string enum.
		params = append(params, broker.Param{Key: "reduceOnly", Value: "true"})
	}

	var raw json.RawMessage
	if err := c.http.PostSigned(ctx, c.eps.newOrder, params, &raw); err != nil {
		return broker.Order{}, classify(err)
	}
	return c.parseOrder(raw)
}

// CancelOrder implements broker.Broker. Parameters travel on the QUERY STRING,
// as both cancel pages document.
func (c *Client) CancelOrder(ctx context.Context, q broker.OrderQuery) (broker.Order, error) {
	params, err := c.identify(q)
	if err != nil {
		return broker.Order{}, err
	}
	var raw json.RawMessage
	if err := c.http.DeleteSigned(ctx, c.eps.cancelOrder, params, &raw); err != nil {
		return broker.Order{}, classify(err)
	}
	return c.parseOrder(raw)
}

// GetOrder implements broker.Broker.
func (c *Client) GetOrder(ctx context.Context, q broker.OrderQuery) (broker.Order, error) {
	params, err := c.identify(q)
	if err != nil {
		return broker.Order{}, err
	}
	var raw json.RawMessage
	if err := c.http.GetSigned(ctx, c.eps.queryOrder, params, &raw); err != nil {
		return broker.Order{}, classify(err)
	}
	return c.parseOrder(raw)
}

// identify builds the symbol + id parameters both query and cancel take.
//
// "Either orderId or origClientOrderId must be sent, and the orderId will
// prevail if both are sent." Only ONE is sent here, deliberately: sending both
// lets the venue silently resolve a disagreement between them, and a caller
// that asked by client id must be answered about that id.
func (c *Client) identify(q broker.OrderQuery) ([]broker.Param, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if q.Market != c.market {
		return nil, fmt.Errorf("%w: this client speaks to %q, the query names %q", broker.ErrInvalidOrder, c.market, q.Market)
	}
	params := []broker.Param{{Key: "symbol", Value: q.Symbol}}
	if q.ClientOrderID != "" {
		return append(params, broker.Param{Key: "origClientOrderId", Value: q.ClientOrderID}), nil
	}
	return append(params, broker.Param{Key: "orderId", Value: q.VenueOrderID}), nil
}

// OpenOrders implements broker.Broker.
func (c *Client) OpenOrders(ctx context.Context, market broker.Market, symbol string) ([]broker.Order, error) {
	if market != c.market {
		return nil, fmt.Errorf("%w: this client speaks to %q", broker.ErrInvalidOrder, c.market)
	}
	var params []broker.Param
	if symbol != "" {
		params = append(params, broker.Param{Key: "symbol", Value: symbol})
	}
	var raw json.RawMessage
	if err := c.http.GetSigned(ctx, c.eps.openOrders, params, &raw); err != nil {
		return nil, classify(err)
	}
	return c.parseOrders(raw)
}

// GetPosition implements broker.Broker, reading FROM THE VENUE (rule 7).
func (c *Client) GetPosition(ctx context.Context, market broker.Market, symbol string) (broker.Position, error) {
	if market != broker.MarketFuturesUSDM {
		return broker.Position{}, fmt.Errorf("%w: %q holds no position", broker.ErrNotSupported, market)
	}
	if market != c.market {
		return broker.Position{}, fmt.Errorf("%w: this client speaks to %q", broker.ErrInvalidOrder, c.market)
	}
	var raw json.RawMessage
	if err := c.http.GetSigned(ctx, broker.FuturesAccount, nil, &raw); err != nil {
		return broker.Position{}, classify(err)
	}
	return parsePosition(raw, symbol)
}

// GetBalance implements broker.Broker.
func (c *Client) GetBalance(ctx context.Context, market broker.Market) ([]broker.Balance, error) {
	if market != c.market {
		return nil, fmt.Errorf("%w: this client speaks to %q", broker.ErrInvalidOrder, c.market)
	}
	var raw json.RawMessage
	ep := c.eps.account
	if market == broker.MarketFuturesUSDM {
		// The balance endpoint is cheaper than the account one (weight 5 both,
		// but the balance answer is a short array) and is what step 4.1
		// accepted against.
		ep = broker.FuturesAccountBalance
	}
	if err := c.http.GetSigned(ctx, ep, nil, &raw); err != nil {
		return nil, classify(err)
	}
	if market == broker.MarketFuturesUSDM {
		return parseFuturesBalances(raw)
	}
	return parseSpotBalances(raw)
}

// trimNumber renders a float for the wire without exponent form.
//
// %v prints 1e-05 for a small quantity and no venue accepts that; 'f' with -1
// gives the shortest representation that round-trips. Callers should pass the
// value broker.RoundOrder produced, whose text form is already on the grid.
func trimNumber(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return s
}
