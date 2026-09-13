package binance

import (
	"encoding/json"
	"fmt"
	"strconv"

	"futures-arbitrage-scanner/internal/broker"
)

// Response parsing.
//
// Every field name here is one the venue's documentation lists, and the golden
// tests replay REAL recordings so a name that is merely plausible fails loudly
// rather than decoding to zero. That failure mode is the reason the recordings
// exist: encoding/json leaves an unmatched field at its zero value, so a
// mis-spelled `executedQty` reports every order as unfilled and nothing
// anywhere says so.
//
//	futures order  https://developers.binance.com/docs/derivatives/usds-margined-futures/trade/rest-api/New-Order
//	spot order     https://developers.binance.com/docs/binance-spot-api-docs/rest-api/trading-endpoints
//	futures acct   https://developers.binance.com/docs/derivatives/usds-margined-futures/account/rest-api/Account-Information-V3

// venueOrder is the union of the two venues' order shapes. They overlap in
// almost every field and differ in how the filled notional is reported, which
// is the one number that cannot be recovered from the others.
type venueOrder struct {
	Symbol        string `json:"symbol"`
	OrderID       int64  `json:"orderId"`
	ClientOrderID string `json:"clientOrderId"`
	// Cancel answers carry the id the order was PLACED under here, while
	// clientOrderId becomes the id of the cancel itself. Reading only
	// clientOrderId off a cancel therefore loses the caller's handle.
	OrigClientOrderID string `json:"origClientOrderId"`

	Price       string `json:"price"`
	OrigQty     string `json:"origQty"`
	ExecutedQty string `json:"executedQty"`
	Status      string `json:"status"`
	TimeInForce string `json:"timeInForce"`
	Type        string `json:"type"`
	Side        string `json:"side"`
	ReduceOnly  bool   `json:"reduceOnly"`

	// The filled notional, spelled differently per venue:
	//   spot     "cummulativeQuoteQty"  (the venue's own double-m spelling)
	//   futures  "cumQuote"
	// and futures additionally publishes an average price directly.
	CummulativeQuoteQty string `json:"cummulativeQuoteQty"`
	CumQuote            string `json:"cumQuote"`
	AvgPrice            string `json:"avgPrice"`

	TransactTime int64 `json:"transactTime"`
	UpdateTime   int64 `json:"updateTime"`
	Time         int64 `json:"time"`
}

func (c *Client) parseOrder(raw json.RawMessage) (broker.Order, error) {
	var vo venueOrder
	if err := decode(raw, &vo); err != nil {
		return broker.Order{}, err
	}
	return c.toOrder(vo)
}

func (c *Client) parseOrders(raw json.RawMessage) ([]broker.Order, error) {
	var list []venueOrder
	if err := decode(raw, &list); err != nil {
		return nil, err
	}
	out := make([]broker.Order, 0, len(list))
	for _, vo := range list {
		o, err := c.toOrder(vo)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

func (c *Client) toOrder(vo venueOrder) (broker.Order, error) {
	// The caller's id first: on a cancel answer it lives in origClientOrderId,
	// and falling back the other way round would hand back the cancel's own id.
	clientID := vo.OrigClientOrderID
	if clientID == "" {
		clientID = vo.ClientOrderID
	}

	qty := parseFloat(vo.OrigQty)
	filled := parseFloat(vo.ExecutedQty)

	// The average fill price, in order of how directly the venue states it.
	// Computing it from a notional is exact; there is no estimate here.
	avg := parseFloat(vo.AvgPrice)
	if avg == 0 && filled > 0 {
		notional := parseFloat(vo.CumQuote)
		if notional == 0 {
			notional = parseFloat(vo.CummulativeQuoteQty)
		}
		if notional > 0 {
			avg = notional / filled
		}
	}

	updated := vo.UpdateTime
	if updated == 0 {
		updated = vo.Time
	}
	if updated == 0 {
		updated = vo.TransactTime
	}

	return broker.Order{
		Market:        c.market,
		Symbol:        vo.Symbol,
		Side:          broker.Side(vo.Side),
		Type:          orderType(vo.Type, vo.TimeInForce),
		Status:        orderStatus(vo.Status),
		ClientOrderID: clientID,
		// A STRING, always: Binance's orderId is 64-bit and the low digits are
		// gone the moment it passes through anything using JSON numbers.
		VenueOrderID:      strconv.FormatInt(vo.OrderID, 10),
		QtyCoin:           qty,
		PriceQuote:        parseFloat(vo.Price),
		FilledQtyCoin:     filled,
		AvgFillPriceQuote: avg,
		ReduceOnly:        vo.ReduceOnly,
		TransactTimeMs:    vo.TransactTime,
		UpdatedAtMs:       updated,
	}, nil
}

// orderStatus maps the venue's words. An unmapped one becomes
// OrderStatusUnknown rather than anything that reads as "not filled" — a status
// this code has not seen is a state it cannot make claims about.
func orderStatus(s string) broker.OrderStatus {
	switch s {
	case "NEW":
		return broker.OrderStatusNew
	case "PARTIALLY_FILLED":
		return broker.OrderStatusPartiallyFilled
	case "FILLED":
		return broker.OrderStatusFilled
	case "CANCELED":
		return broker.OrderStatusCanceled
	case "REJECTED":
		return broker.OrderStatusRejected
	// EXPIRED_IN_MATCH is spot's self-trade-prevention expiry; it is an expiry
	// as far as a caller is concerned.
	case "EXPIRED", "EXPIRED_IN_MATCH":
		return broker.OrderStatusExpired
	// PENDING_CANCEL is not Done: the venue still holds it.
	case "PENDING_CANCEL":
		return broker.OrderStatusNew
	}
	return broker.OrderStatusUnknown
}

func orderType(t, tif string) broker.OrderType {
	switch t {
	case "MARKET":
		return broker.OrderTypeMarket
	case "LIMIT":
		if tif == "GTC" || tif == "" {
			return broker.OrderTypeLimitGTC
		}
	}
	// A type this package does not send. Reported as-is so the caller sees
	// something true rather than a LIMIT it was not.
	return broker.OrderType(t)
}

// parsePosition reads one symbol out of GET /fapi/v3/account's `positions`.
//
// WHAT THIS ENDPOINT DOES NOT PUBLISH, read from the V3 page on 2026-09-13:
// the positions array carries symbol, positionSide, positionAmt,
// unrealizedProfit, isolatedMargin, notional, isolatedWallet, initialMargin,
// maintMargin and updateTime — and NO entryPrice, markPrice, liquidationPrice
// or leverage. entryPrice was in the V2 answer and is absent from V3.
//
// So those four fields of broker.Position are left ZERO here, and that is
// deliberate: inventing a field name to fill them would decode to zero anyway
// while looking like it worked (rule 5). A caller that needs an entry price
// must read an endpoint that publishes one, and no such endpoint has been
// verified from this environment yet — recorded as a step-4.4 debt, since
// nothing before 4.4 needs it.
func parsePosition(raw json.RawMessage, symbol string) (broker.Position, error) {
	var account struct {
		Positions []struct {
			Symbol           string `json:"symbol"`
			PositionSide     string `json:"positionSide"`
			PositionAmt      string `json:"positionAmt"`
			UnrealizedProfit string `json:"unrealizedProfit"`
			Notional         string `json:"notional"`
			InitialMargin    string `json:"initialMargin"`
			MaintMargin      string `json:"maintMargin"`
			UpdateTime       int64  `json:"updateTime"`
		} `json:"positions"`
	}
	if err := decode(raw, &account); err != nil {
		return broker.Position{}, err
	}
	out := broker.Position{Market: broker.MarketFuturesUSDM, Symbol: symbol}
	for _, p := range account.Positions {
		if p.Symbol != symbol {
			continue
		}
		// positionAmt is SIGNED at the venue in one-way mode, which is what
		// broker.Position.QtyCoin wants. In hedge mode the same symbol appears
		// twice (LONG and SHORT) with positive amounts, so the sides are
		// summed rather than the first being taken — taking the first would
		// report a flat account as long.
		amt := parseFloat(p.PositionAmt)
		if p.PositionSide == "SHORT" && amt > 0 {
			amt = -amt
		}
		out.QtyCoin += amt
		out.UnrealizedPnLQuote += parseFloat(p.UnrealizedProfit)
		if p.UpdateTime > out.UpdatedAtMs {
			out.UpdatedAtMs = p.UpdateTime
		}
	}
	return out, nil
}

// parseFuturesBalances reads GET /fapi/v3/balance.
func parseFuturesBalances(raw json.RawMessage) ([]broker.Balance, error) {
	var rows []struct {
		Asset            string `json:"asset"`
		Balance          string `json:"balance"`
		AvailableBalance string `json:"availableBalance"`
	}
	if err := decode(raw, &rows); err != nil {
		return nil, err
	}
	out := make([]broker.Balance, 0, len(rows))
	for _, r := range rows {
		total, free := parseFloat(r.Balance), parseFloat(r.AvailableBalance)
		locked := total - free
		if locked < 0 {
			// Unrealized profit can put availableBalance above the wallet
			// balance; a negative "locked" is arithmetic, not state.
			locked = 0
		}
		out = append(out, broker.Balance{
			Market: broker.MarketFuturesUSDM, Asset: r.Asset,
			FreeQtyCoin: free, LockedQtyCoin: locked,
		})
	}
	return out, nil
}

// parseSpotBalances reads GET /api/v3/account's `balances` of {asset, free, locked}.
func parseSpotBalances(raw json.RawMessage) ([]broker.Balance, error) {
	var account struct {
		Balances []struct {
			Asset  string `json:"asset"`
			Free   string `json:"free"`
			Locked string `json:"locked"`
		} `json:"balances"`
	}
	if err := decode(raw, &account); err != nil {
		return nil, err
	}
	out := make([]broker.Balance, 0, len(account.Balances))
	for _, b := range account.Balances {
		out = append(out, broker.Balance{
			Market: broker.MarketSpot, Asset: b.Asset,
			FreeQtyCoin: parseFloat(b.Free), LockedQtyCoin: parseFloat(b.Locked),
		})
	}
	return out, nil
}

// parseFloat reads a venue's decimal STRING. Binance sends numbers as strings
// throughout; an empty or unparsable one is 0, which every caller here treats
// as "not stated".
func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// decode keeps the venue's body out of the error message. The body is
// venue-controlled text and these errors are pasted into reports.
func decode(raw json.RawMessage, into any) error {
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("binance: không giải mã được câu trả lời (thân phản hồi không in ra): %s", err.Error())
	}
	return nil
}
