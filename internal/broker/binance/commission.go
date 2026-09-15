package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"futures-arbitrage-scanner/internal/broker"
)

// CommissionRates is what THIS ACCOUNT is charged for a taker fill on one
// symbol, read from the venue with the key (PLAN Q18).
//
// It exists because the auto-trader may not price a round trip on fees nobody
// looked up (CLAUDE.md rule 2: "not looked up" is not "free"), and the binary it
// runs in may not read config.yaml (guard_test.go: it links no internal/config).
// So the figure comes from the account itself — which on the testnets is NOT
// the public schedule: measured 2026-09-15, spot testnet charges 0 on every
// component and futures testnet charges taker 4 bps. Every figure built from
// these says it is the testnet's.
type CommissionRates struct {
	Market broker.Market
	Symbol string

	// TakerBuyFrac and TakerSellFrac are the rate on one taker fill's notional,
	// as a FRACTION. Futures publishes one taker rate for both sides; spot
	// sums every component for the side (standard + special + tax, each
	// taker + buyer or taker + seller), before any BNB discount — the discount
	// applies only when commission is paid in BNB, which this account is not
	// assumed to do, so leaving it out over-states the fee, the safe direction.
	TakerBuyFrac  float64
	TakerSellFrac float64

	SourceVI string
}

// TakerBps is the dearer of the two sides in basis points — one number per
// market, which is the shape fees.Schedule has, taken on the expensive side so a
// round trip priced with it is never cheaper than the account really pays.
func (r CommissionRates) TakerBps() float64 {
	return max(r.TakerBuyFrac, r.TakerSellFrac) * 10_000
}

// CommissionRates reads this account's commission for one symbol on this
// client's market.
func (c *Client) CommissionRates(ctx context.Context, symbol string) (CommissionRates, error) {
	if symbol == "" {
		return CommissionRates{}, fmt.Errorf("%w: no symbol", broker.ErrInvalidOrder)
	}
	ep := broker.FuturesCommissionRate
	if c.market == broker.MarketSpot {
		ep = broker.SpotAccountCommission
	}
	var raw json.RawMessage
	if err := c.http.GetSigned(ctx, ep, []broker.Param{{Key: "symbol", Value: symbol}}, &raw); err != nil {
		return CommissionRates{}, classify(err)
	}
	if c.market == broker.MarketSpot {
		return parseSpotCommission(raw, symbol)
	}
	return parseFuturesCommission(raw, symbol)
}

func parseFuturesCommission(raw json.RawMessage, symbol string) (CommissionRates, error) {
	var v struct {
		Symbol              string `json:"symbol"`
		MakerCommissionRate string `json:"makerCommissionRate"`
		TakerCommissionRate string `json:"takerCommissionRate"`
	}
	if err := decode(raw, &v); err != nil {
		return CommissionRates{}, err
	}
	if v.Symbol != symbol {
		return CommissionRates{}, fmt.Errorf("binance: phí futures trả cho %q, đã hỏi %q", v.Symbol, symbol)
	}
	taker, err := strictRate("takerCommissionRate", v.TakerCommissionRate)
	if err != nil {
		return CommissionRates{}, err
	}
	return CommissionRates{
		Market: broker.MarketFuturesUSDM, Symbol: symbol, TakerBuyFrac: taker, TakerSellFrac: taker,
		SourceVI: "GET /fapi/v1/commissionRate của tài khoản TESTNET (takerCommissionRate)",
	}, nil
}

// venueCommission is one of spot's commission objects.
type venueCommission struct {
	Maker  string `json:"maker"`
	Taker  string `json:"taker"`
	Buyer  string `json:"buyer"`
	Seller string `json:"seller"`
}

func parseSpotCommission(raw json.RawMessage, symbol string) (CommissionRates, error) {
	var v struct {
		Symbol             string           `json:"symbol"`
		StandardCommission *venueCommission `json:"standardCommission"`
		SpecialCommission  *venueCommission `json:"specialCommission"`
		TaxCommission      *venueCommission `json:"taxCommission"`
	}
	if err := decode(raw, &v); err != nil {
		return CommissionRates{}, err
	}
	if v.Symbol != symbol {
		return CommissionRates{}, fmt.Errorf("binance: phí spot trả cho %q, đã hỏi %q", v.Symbol, symbol)
	}
	out := CommissionRates{Market: broker.MarketSpot, Symbol: symbol,
		SourceVI: "GET /api/v3/account/commission của tài khoản TESTNET (standard + special + tax, taker + buyer/seller, chưa trừ giảm BNB)"}
	// All three components are on the documentation page. One that is absent
	// is refused rather than counted as zero: a component the venue stopped
	// sending is a fee nobody can see, not a fee that went away.
	for _, part := range []struct {
		name string
		c    *venueCommission
	}{{"standardCommission", v.StandardCommission}, {"specialCommission", v.SpecialCommission}, {"taxCommission", v.TaxCommission}} {
		if part.c == nil {
			return CommissionRates{}, fmt.Errorf("binance: câu trả lời phí spot thiếu %s — không coi là miễn phí", part.name)
		}
		taker, err := strictRate(part.name+".taker", part.c.Taker)
		if err != nil {
			return CommissionRates{}, err
		}
		buyer, err := strictRate(part.name+".buyer", part.c.Buyer)
		if err != nil {
			return CommissionRates{}, err
		}
		seller, err := strictRate(part.name+".seller", part.c.Seller)
		if err != nil {
			return CommissionRates{}, err
		}
		out.TakerBuyFrac += taker + buyer
		out.TakerSellFrac += taker + seller
	}
	return out, nil
}

// strictRate parses a venue rate string, refusing the empty or unreadable one
// that parseFloat would quietly turn into a zero fee.
func strictRate(field, s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || v >= 1 {
		return 0, fmt.Errorf("binance: trường phí %s không đọc được thành một tỷ lệ trong [0, 1) — không coi là miễn phí", field)
	}
	return v, nil
}
