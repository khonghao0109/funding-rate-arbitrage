package gate

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"errors"
	"fmt"
	"strings"
)

// Gate futures instrument rules — GET /api/v4/futures/usdt/contracts/{name},
// one call per symbol.
//
// Gate denominates orders in WHOLE contracts (order_size_min/max are
// integers) of quanto_multiplier base coins each (BTC_USDT: 0.0001 BTC,
// verified live 2026-09-03), so the coin step is 1 × quanto_multiplier.
// order_price_round is the tick. No minimum notional is published — the
// floor is order_size_min contracts. in_delisting marks a dying market
// before status changes.
// https://www.gate.com/docs/developers/apiv4/en/#get-a-single-contract

type gateContractResponse struct {
	Name             string  `json:"name"`
	Status           string  `json:"status"`
	InDelisting      bool    `json:"in_delisting"`
	QuantoMultiplier string  `json:"quanto_multiplier"`
	OrderPriceRound  string  `json:"order_price_round"`
	OrderSizeMin     float64 `json:"order_size_min"` // contracts; integer-valued
	OrderSizeMax     float64 `json:"order_size_max"`
	LeverageMax      string  `json:"leverage_max"`
}

func FetchInstruments(ctx context.Context, source string, symbols []exchanges.Symbol) ([]exchanges.Instrument, error) {
	var out []exchanges.Instrument
	for _, s := range symbols {
		u := "https://api.gateio.ws/api/v4/futures/usdt/contracts/" + s.Venue
		var resp gateContractResponse
		if err := exchanges.FetchJSON(ctx, u, &resp); err != nil {
			// A contract Gate no longer lists 404s; that is "absent", not a
			// venue outage — the other symbols must still refresh.
			if errors.Is(err, exchanges.ErrNotListed) {
				continue
			}
			return nil, err
		}
		inst, err := parseGateInstrument(resp, source, s)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, nil
}

func parseGateInstrument(resp gateContractResponse, source string, s exchanges.Symbol) (exchanges.Instrument, error) {
	if resp.Name != s.Venue {
		return exchanges.Instrument{}, fmt.Errorf("%s: asked for %s, response names %s", source, s.Venue, resp.Name)
	}
	quantoMultiplierCoin, err := exchanges.ParseFloatField(source, resp.Name, "quanto_multiplier", resp.QuantoMultiplier)
	if err != nil {
		return exchanges.Instrument{}, err
	}
	if quantoMultiplierCoin <= 0 {
		return exchanges.Instrument{}, fmt.Errorf("%s %s: quanto_multiplier = %v is not a positive contract size", source, resp.Name, quantoMultiplierCoin)
	}
	tickSize, err := exchanges.ParseFloatField(source, resp.Name, "order_price_round", resp.OrderPriceRound)
	if err != nil {
		return exchanges.Instrument{}, err
	}
	leverageMax, err := exchanges.ParseFloatField(source, resp.Name, "leverage_max", resp.LeverageMax)
	if err != nil {
		return exchanges.Instrument{}, err
	}
	status := exchanges.NormalizeStatus(resp.Status, resp.Status == "trading" && !resp.InDelisting)
	if resp.InDelisting {
		status = "delisting"
	}
	// Gate declares no base/quote fields; the contract's own name is its
	// declaration — the docs define futures contract names as
	// "{base}_{quote}" ("BTC_USDT"), and this fetcher reads the USDT-settled
	// book (/futures/usdt/), where quote and settle coincide. Splitting on
	// the venue's separator is reading declared structure, unlike slicing a
	// fixed prefix length; a name without "_" declares nothing and stays
	// empty for the mapping to refuse.
	// https://www.gate.com/docs/developers/apiv4/en/#futures
	var baseAsset, quoteAsset string
	if i := strings.LastIndex(resp.Name, "_"); i > 0 {
		baseAsset = resp.Name[:i]
		quoteAsset = resp.Name[i+1:]
	}
	return exchanges.Instrument{
		Symbol:           s.Standard,
		NativeSymbol:     resp.Name,
		Source:           source,
		MarketType:       "perp",
		Status:           status,
		BaseAsset:        baseAsset,
		QuoteAsset:       quoteAsset,
		TickSizeQuote:    tickSize,
		StepSizeCoin:     quantoMultiplierCoin, // orders move in whole contracts
		MinQtyCoin:       resp.OrderSizeMin * quantoMultiplierCoin,
		MaxQtyCoin:       resp.OrderSizeMax * quantoMultiplierCoin,
		IsContract:       true,
		ContractSizeCoin: quantoMultiplierCoin,
		MaxLeverageX:     leverageMax,
	}, nil
}
