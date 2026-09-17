package bybit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"futures-arbitrage-scanner/internal/broker"
)

// maxOrderLinkIDLen and the alphabet are quoted from create-order: "A max of 36
// characters. Combinations of numbers, letters (upper and lower cases), dashes,
// and underscores are supported." An id outside them is REFUSED locally, never
// truncated or rewritten: the id is the caller's only handle after an
// ambiguous timeout (broker.Broker), and a rewritten one is a different handle.
const maxOrderLinkIDLen = 36

func checkOrderLinkID(id string) error {
	if len(id) > maxOrderLinkIDLen {
		return fmt.Errorf("%w: ClientOrderID is %d characters; Bybit accepts at most %d and this package does not shorten it", broker.ErrInvalidOrder, len(id), maxOrderLinkIDLen)
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-' || r == '_') {
			return fmt.Errorf("%w: ClientOrderID carries a character Bybit does not accept (letters, digits, - and _ only)", broker.ErrInvalidOrder)
		}
	}
	return nil
}

func (c *Client) checkMarket(m broker.Market) error {
	if m != broker.MarketFuturesUSDM {
		return fmt.Errorf("%w: this Bybit client trades USDT linear perpetuals only, not %q", broker.ErrNotSupported, m)
	}
	return nil
}

// createOrderBody is POST /v5/order/create. A STRUCT, not a map, so the JSON
// that is signed has one fixed field order and the same bytes every time.
type createOrderBody struct {
	Category    string `json:"category"`
	Symbol      string `json:"symbol"`
	Side        string `json:"side"`
	OrderType   string `json:"orderType"`
	Qty         string `json:"qty"`
	Price       string `json:"price,omitempty"`
	TimeInForce string `json:"timeInForce"`
	OrderLinkID string `json:"orderLinkId"`
	ReduceOnly  bool   `json:"reduceOnly"`
	// "0: one-way mode". The package refuses to read a hedge-mode position
	// (GetPosition), so it never sends 1 or 2 either.
	PositionIdx int `json:"positionIdx"`
}

func venueSide(s broker.Side) string {
	if s == broker.SideBuy {
		return "Buy"
	}
	return "Sell"
}

// PlaceOrder implements broker.Broker.
//
// MARKET is sent with timeInForce IOC — "Market order will always use IOC" —
// and the venue "will convert the market order into an IOC limit order for
// matching. If there are no orderbook entries within price slippage limit, the
// order will not be executed." So a MARKET order here can fill partly or not at
// all, and the answer says neither: it is an acknowledgement, and the returned
// Order is status NEW with FilledQtyCoin 0. Read the fill back with GetOrder.
func (c *Client) PlaceOrder(ctx context.Context, req broker.PlaceOrderRequest) (broker.Order, error) {
	if err := req.Validate(); err != nil {
		return broker.Order{}, err
	}
	if err := c.checkMarket(req.Market); err != nil {
		return broker.Order{}, err
	}
	if err := checkOrderLinkID(req.ClientOrderID); err != nil {
		return broker.Order{}, err
	}
	body := createOrderBody{
		Category:    categoryLinear,
		Symbol:      req.Symbol,
		Side:        venueSide(req.Side),
		Qty:         formatNumber(req.QtyCoin),
		OrderLinkID: req.ClientOrderID,
		ReduceOnly:  req.ReduceOnly,
		PositionIdx: 0,
	}
	switch req.Type {
	case broker.OrderTypeMarket:
		body.OrderType, body.TimeInForce = "Market", "IOC"
	case broker.OrderTypeLimitGTC:
		body.OrderType, body.TimeInForce = "Limit", "GTC"
		body.Price = formatNumber(req.PriceQuote)
	}
	var ack struct {
		OrderID     string `json:"orderId"`
		OrderLinkID string `json:"orderLinkId"`
	}
	if err := c.postSigned(ctx, epOrderCreate, callPlace, body, &ack); err != nil {
		if errors.Is(err, ErrDuplicateClientOrderID) {
			// 110072 is the venue saying an order under this id ALREADY EXISTS —
			// typically the first send of a request that timed out. It is proof
			// of existence, not a refusal: read that order back and hand it to
			// the caller beside the error, so nobody treats the leg as unplaced.
			q := broker.OrderQuery{Market: req.Market, Symbol: req.Symbol, ClientOrderID: req.ClientOrderID}
			existing, gerr := c.GetOrder(ctx, q)
			if gerr != nil {
				// gerr is %v, NOT %w: wrapping it would let a 4xx on the READ-BACK
				// satisfy errors.As(*broker.HTTPError) and make a caller's
				// "definitely refused" test true for an order the venue has just
				// said exists (review round 2, 2026-09-17).
				return broker.Order{}, fmt.Errorf("%w; reading it back: %v", err, gerr)
			}
			return existing, err
		}
		return broker.Order{}, err
	}
	if ack.OrderID == "" {
		return broker.Order{}, errors.New("bybit: the order was acknowledged with no orderId — its state is UNKNOWN; resolve it by ClientOrderID before any resend")
	}
	if ack.OrderLinkID != req.ClientOrderID {
		return broker.Order{}, fmt.Errorf("bybit: the acknowledgement names orderLinkId %q, the request sent %q — its state is UNKNOWN", ack.OrderLinkID, req.ClientOrderID)
	}
	return broker.Order{
		Market: req.Market, Symbol: req.Symbol, Side: req.Side, Type: req.Type,
		Status:        broker.OrderStatusNew,
		ClientOrderID: req.ClientOrderID, VenueOrderID: ack.OrderID,
		QtyCoin: req.QtyCoin, PriceQuote: req.PriceQuote, ReduceOnly: req.ReduceOnly,
	}, nil
}

// cancelOrderBody is POST /v5/order/cancel: "Either orderId or orderLinkId is
// required", and only ONE is sent, so the venue never resolves a disagreement
// between them silently ("If orderId and orderLinkId do not match, the system
// will process orderId first").
type cancelOrderBody struct {
	Category    string `json:"category"`
	Symbol      string `json:"symbol"`
	OrderID     string `json:"orderId,omitempty"`
	OrderLinkID string `json:"orderLinkId,omitempty"`
}

// How long CancelOrder waits for the venue to show a cancelled order in a final
// state, how often it asks, and how many times it re-sends the cancel to an order
// it can SEE is still live. Variables so a test need not wait seconds.
var (
	cancelConfirmWithin = 3 * time.Second
	cancelPollEvery     = 150 * time.Millisecond
	maxCancelResends    = 2
)

// CancelOrder implements broker.Broker.
//
// Bybit's cancel is asynchronous — "The acknowledgement of an cancel order
// request indicates that the request was sucessfully accepted" — and a Binance
// cancel is not, which internal/execution silently assumes: it takes the order
// it gets back as the FINAL fill. So this method does not return success until
// the order reads back in a terminal state (Status.Done), within
// cancelConfirmWithin — a deadline that covers the requests themselves:
//
//   - terminal, after an accepted cancel → the order, nil;
//   - terminal, after 110001/110008/110010 → the order AND broker.ErrOrderNotFound
//     ("already gone"), with the fill it ended on;
//   - still LIVE when read back — after a "gone" answer (a cancel that beat an
//     asynchronous create), or on the second live read after an accepted one —
//     the cancel is SENT AGAIN, at most maxCancelResends times, because the
//     order is visible and leaving it resting is how it fills later;
//   - anything else by the deadline → ErrCancelNotConfirmed, AMBIGUOUS, with the
//     last order seen. A cool-down or a cancelled context ends the wait at once,
//     still ErrCancelNotConfirmed.
func (c *Client) CancelOrder(ctx context.Context, q broker.OrderQuery) (broker.Order, error) {
	if err := c.checkQuery(q); err != nil {
		return broker.Order{}, err
	}
	body := cancelOrderBody{Category: categoryLinear, Symbol: q.Symbol}
	if q.ClientOrderID != "" {
		body.OrderLinkID = q.ClientOrderID
	} else {
		body.OrderID = q.VenueOrderID
	}
	cancelErr := c.postSigned(ctx, epOrderCancel, callCancel, body, nil)
	gone := errors.Is(cancelErr, broker.ErrOrderNotFound)
	// accepted records that SOME cancel was taken by the venue: a later resend
	// answered "already gone" is then that first cancel completing, not news.
	accepted := cancelErr == nil
	if cancelErr != nil && !gone {
		return broker.Order{}, cancelErr
	}

	loopCtx, stop := context.WithTimeout(ctx, cancelConfirmWithin)
	defer stop()
	notConfirmed := func(last broker.Order, cause error) (broker.Order, error) {
		if cause != nil {
			return last, fmt.Errorf("%w within %s: %v", ErrCancelNotConfirmed, cancelConfirmWithin, cause)
		}
		return last, fmt.Errorf("%w within %s: last read %s", ErrCancelNotConfirmed, cancelConfirmWithin, last.Status)
	}
	var last broker.Order
	var lastErr error
	liveReads, resent := 0, 0
	for {
		o, err := c.GetOrder(loopCtx, q)
		switch {
		case err == nil && o.Status.Done():
			if gone && !accepted {
				return o, fmt.Errorf("%w: the venue answered the cancel with %v; the order ended %s", broker.ErrOrderNotFound, cancelErr, o.Status)
			}
			return o, nil
		case err == nil:
			last, lastErr = o, nil
			liveReads++
			if (gone || liveReads >= 2) && resent < maxCancelResends {
				resent++
				again := c.postSigned(loopCtx, epOrderCancel, callCancel, body, nil)
				switch {
				case again == nil:
					gone, accepted, liveReads = false, true, 0
				case errors.Is(again, broker.ErrOrderNotFound):
					gone, cancelErr = true, again
				default:
					lastErr = again
				}
			}
		case errors.Is(err, broker.ErrIPCoolingDown) || loopCtx.Err() != nil:
			if lastErr != nil {
				err = fmt.Errorf("%v (before it: %v)", err, lastErr)
			}
			return notConfirmed(last, err)
		default:
			// ErrOrderNotVisible, a transport failure, 110079: keep the last
			// order seen and keep asking until the deadline.
			lastErr = err
		}
		select {
		case <-loopCtx.Done():
			return notConfirmed(last, lastErr)
		case <-time.After(cancelPollEvery):
		}
	}
}

func (c *Client) checkQuery(q broker.OrderQuery) error {
	if err := q.Validate(); err != nil {
		return err
	}
	if err := c.checkMarket(q.Market); err != nil {
		return err
	}
	if q.ClientOrderID != "" {
		return checkOrderLinkID(q.ClientOrderID)
	}
	return nil
}

func identify(q broker.OrderQuery) []broker.Param {
	params := []broker.Param{{Key: "category", Value: categoryLinear}, {Key: "symbol", Value: q.Symbol}}
	if q.ClientOrderID != "" {
		return append(params, broker.Param{Key: "orderLinkId", Value: q.ClientOrderID})
	}
	return append(params, broker.Param{Key: "orderId", Value: q.VenueOrderID})
}

// GetOrder implements broker.Broker.
//
// It asks /v5/order/realtime first ("openOnly param will be ignored when query
// by orderId or orderLinkId", so a closed order among the recent 500 is found
// too) and, when that list is empty, /v5/order/history, because "After a server
// release or restart, filled, cancelled, and rejected orders of Unified account
// should only be queried through order history."
//
// When BOTH are empty the answer is ErrOrderNotVisible, which is AMBIGUOUS and
// deliberately does not wrap broker.ErrOrderNotFound. Two empty lists are not
// the venue saying "no such order": create is asynchronous, history "may
// delay", history keeps a fully cancelled or rejected order 24 hours only, and
// realtime forgets closed orders on a venue restart. internal/execution reads
// ErrOrderNotFound as "nothing filled" after a cancel and as "safe to resend",
// and on this venue either reading could leave one leg naked (review,
// 2026-09-17). A resend under the SAME id is still safe — 110072 — and
// PlaceOrder turns that answer into a read-back.
func (c *Client) GetOrder(ctx context.Context, q broker.OrderQuery) (broker.Order, error) {
	if err := c.checkQuery(q); err != nil {
		return broker.Order{}, err
	}
	for _, ep := range []broker.Endpoint{epOrderRealtime, epOrderHistory} {
		orders, _, err := c.listOrders(ctx, ep, identify(q))
		if err != nil {
			return broker.Order{}, err
		}
		for _, o := range orders {
			if (q.ClientOrderID != "" && o.ClientOrderID == q.ClientOrderID) ||
				(q.ClientOrderID == "" && o.VenueOrderID == q.VenueOrderID) {
				return o, nil
			}
		}
		if len(orders) > 0 {
			return broker.Order{}, fmt.Errorf("bybit: %s answered %d order(s), none with the id asked for — its state is UNKNOWN", ep.Path, len(orders))
		}
	}
	return broker.Order{}, fmt.Errorf("%w: neither /v5/order/realtime nor /v5/order/history lists it", ErrOrderNotVisible)
}

// OpenOrders implements broker.Broker. An empty symbol lists every USDT-settled
// linear order: "For linear, either symbol, baseCoin, settleCoin is required".
func (c *Client) OpenOrders(ctx context.Context, market broker.Market, symbol string) ([]broker.Order, error) {
	if err := c.checkMarket(market); err != nil {
		return nil, err
	}
	params := []broker.Param{{Key: "category", Value: categoryLinear}}
	if symbol != "" {
		params = append(params, broker.Param{Key: "symbol", Value: symbol})
	} else {
		params = append(params, broker.Param{Key: "settleCoin", Value: settleCoinUSDT})
	}
	// "0(default): query open status orders (e.g., New, PartiallyFilled) only";
	// limit is [1, 50].
	params = append(params, broker.Param{Key: "openOnly", Value: "0"}, broker.Param{Key: "limit", Value: "50"})

	var out []broker.Order
	const maxPages = 20 // 1,000 orders, past the 500-per-symbol active cap
	cursor := ""
	for page := 0; page < maxPages; page++ {
		p := params
		if cursor != "" {
			p = append(append([]broker.Param{}, params...), broker.Param{Key: "cursor", Value: cursor})
		}
		orders, next, err := c.listOrders(ctx, epOrderRealtime, p)
		if err != nil {
			return nil, err
		}
		out = append(out, orders...)
		if next == "" {
			return out, nil
		}
		cursor = next
	}
	return nil, fmt.Errorf("bybit: more than %d pages of open orders — refused rather than returning a partial list that reads as complete", maxPages)
}

// venueOrder is one element of the realtime / history list.
type venueOrder struct {
	OrderID      string `json:"orderId"`
	OrderLinkID  string `json:"orderLinkId"`
	Symbol       string `json:"symbol"`
	Side         string `json:"side"`
	OrderType    string `json:"orderType"`
	TimeInForce  string `json:"timeInForce"`
	OrderStatus  string `json:"orderStatus"`
	Price        string `json:"price"`
	Qty          string `json:"qty"`
	AvgPrice     string `json:"avgPrice"`
	CumExecQty   string `json:"cumExecQty"`
	ReduceOnly   bool   `json:"reduceOnly"`
	CreatedTime  string `json:"createdTime"`
	UpdatedTime  string `json:"updatedTime"`
	PositionIdx  int    `json:"positionIdx"`
	RejectReason string `json:"rejectReason"`
}

func (c *Client) listOrders(ctx context.Context, ep broker.Endpoint, params []broker.Param) ([]broker.Order, string, error) {
	var res struct {
		List           []venueOrder `json:"list"`
		NextPageCursor string       `json:"nextPageCursor"`
	}
	if err := c.getSigned(ctx, ep, params, &res); err != nil {
		return nil, "", err
	}
	out := make([]broker.Order, 0, len(res.List))
	for _, v := range res.List {
		o, err := v.normalize()
		if err != nil {
			return nil, "", err
		}
		out = append(out, o)
	}
	return out, res.NextPageCursor, nil
}

func (v venueOrder) normalize() (broker.Order, error) {
	o := broker.Order{
		Market: broker.MarketFuturesUSDM, Symbol: v.Symbol,
		ClientOrderID: v.OrderLinkID, VenueOrderID: v.OrderID,
		Status: normalizeStatus(v.OrderStatus), ReduceOnly: v.ReduceOnly,
	}
	switch v.Side {
	case "Buy":
		o.Side = broker.SideBuy
	case "Sell":
		o.Side = broker.SideSell
	default:
		return broker.Order{}, fmt.Errorf("bybit: order %s has side %q, neither Buy nor Sell", v.OrderID, v.Side)
	}
	switch {
	case v.OrderType == "Market":
		o.Type = broker.OrderTypeMarket
	case v.OrderType == "Limit" && v.TimeInForce == "GTC":
		o.Type = broker.OrderTypeLimitGTC
		// Any other combination leaves Type empty: this package sends only
		// those two, and naming another would claim a type it never checked.
	}
	var err error
	if o.QtyCoin, err = parseRequiredNumber("qty", v.Qty); err != nil {
		return broker.Order{}, err
	}
	if o.PriceQuote, err = parseNumber("price", v.Price); err != nil {
		return broker.Order{}, err
	}
	// cumExecQty and avgPrice are GROSS. cumExecFee is deliberately NOT read:
	// "linear, spot: Deprecated. Use cumFeeDetail instead."
	if o.FilledQtyCoin, err = parseRequiredNumber("cumExecQty", v.CumExecQty); err != nil {
		return broker.Order{}, err
	}
	if o.AvgFillPriceQuote, err = parseNumber("avgPrice", v.AvgPrice); err != nil {
		return broker.Order{}, err
	}
	if o.TransactTimeMs, err = parseMs("createdTime", v.CreatedTime); err != nil {
		return broker.Order{}, err
	}
	if o.UpdatedAtMs, err = parseMs("updatedTime", v.UpdatedTime); err != nil {
		return broker.Order{}, err
	}
	return o, nil
}

// normalizeStatus maps orderStatus (https://bybit-exchange.github.io/docs/v5/enum).
//
// "Cancelled — In derivatives, orders with this status may have an executed
// qty", so CANCELED here does NOT mean nothing filled: read FilledQtyCoin.
// Deactivated ("cancelled before they are triggered") is CANCELED as well.
// Untriggered and Triggered belong to conditional orders this package never
// sends, and are UNKNOWN rather than a guessed equivalent.
func normalizeStatus(s string) broker.OrderStatus {
	switch s {
	case "New":
		return broker.OrderStatusNew
	case "PartiallyFilled":
		return broker.OrderStatusPartiallyFilled
	case "Filled":
		return broker.OrderStatusFilled
	case "Cancelled", "PartiallyFilledCanceled", "Deactivated":
		return broker.OrderStatusCanceled
	case "Rejected":
		return broker.OrderStatusRejected
	}
	return broker.OrderStatusUnknown
}
