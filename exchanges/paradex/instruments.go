package paradex

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"errors"
	"fmt"
)

// Paradex instrument rules — GET /v1/markets?market={symbol}, one call per
// symbol.
// https://docs.paradex.trade/api-reference/prod/markets/get-markets
//
// Sizes are in coin (order_size_increment 0.00001 BTC for BTC-USD-PERP,
// verified live 2026-09-03), price_tick_size is the tick, min_notional the
// order floor. The endpoint carries no status field and lists only live
// markets — absence from the response is how a dead market shows up, so a
// listed market is recorded as trading. Max leverage is not published
// directly (margin factors imf_base exist, but deriving leverage from them
// would be a guess) — MaxLeverageX stays 0.

type paradexMarketsResponse struct {
	Results []struct {
		Symbol    string `json:"symbol"`
		AssetKind string `json:"asset_kind"`
		// Declared directly. Note quote_currency is USD while
		// settlement_currency is USDC — the quote is what pairing compares.
		BaseCurrency       string `json:"base_currency"`
		QuoteCurrency      string `json:"quote_currency"`
		OrderSizeIncrement string `json:"order_size_increment"`
		PriceTickSize      string `json:"price_tick_size"`
		MinNotional        string `json:"min_notional"`
		MaxOrderSize       string `json:"max_order_size"`
	} `json:"results"`
}

func FetchInstruments(ctx context.Context, source string, symbols []exchanges.Symbol) ([]exchanges.Instrument, error) {
	var out []exchanges.Instrument
	for _, s := range symbols {
		u := "https://api.prod.paradex.trade/v1/markets?market=" + s.Venue
		var resp paradexMarketsResponse
		if err := exchanges.FetchJSON(ctx, u, &resp); err != nil {
			// An unknown market 404s (measured 2026-09-03 with
			// XLM-USD-PERP; a LISTED market answers 200 + results). That is
			// "absent", not a venue outage — without this skip one
			// unsupported pair blanks every Paradex rule.
			if errors.Is(err, exchanges.ErrNotListed) {
				continue
			}
			return nil, err
		}
		inst, ok, err := parseParadexInstrument(resp, source, s)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, inst)
		}
	}
	return out, nil
}

func parseParadexInstrument(resp paradexMarketsResponse, source string, s exchanges.Symbol) (exchanges.Instrument, bool, error) {
	if len(resp.Results) == 0 {
		return exchanges.Instrument{}, false, nil // not listed = not tradable here
	}
	e := resp.Results[0]
	if e.Symbol != s.Venue {
		return exchanges.Instrument{}, false, fmt.Errorf("%s: asked for %s, response names %s", source, s.Venue, e.Symbol)
	}
	if e.AssetKind != "PERP" {
		return exchanges.Instrument{}, false, fmt.Errorf("%s %s: asset_kind %q is not PERP", source, e.Symbol, e.AssetKind)
	}
	stepCoin, err := exchanges.ParseFloatField(source, e.Symbol, "order_size_increment", e.OrderSizeIncrement)
	if err != nil {
		return exchanges.Instrument{}, false, err
	}
	tickSize, err := exchanges.ParseFloatField(source, e.Symbol, "price_tick_size", e.PriceTickSize)
	if err != nil {
		return exchanges.Instrument{}, false, err
	}
	minNotional, err := exchanges.ParseFloatField(source, e.Symbol, "min_notional", e.MinNotional)
	if err != nil {
		return exchanges.Instrument{}, false, err
	}
	maxQtyCoin, err := exchanges.ParseFloatField(source, e.Symbol, "max_order_size", e.MaxOrderSize)
	if err != nil {
		return exchanges.Instrument{}, false, err
	}
	return exchanges.Instrument{
		Symbol:           s.Standard,
		NativeSymbol:     e.Symbol,
		Source:           source,
		MarketType:       "perp",
		Status:           exchanges.StatusTrading, // the endpoint lists only live markets; no status field exists
		BaseAsset:        e.BaseCurrency,
		QuoteAsset:       e.QuoteCurrency,
		TickSizeQuote:    tickSize,
		StepSizeCoin:     stepCoin,
		MinQtyCoin:       0, // not published — 0 means "not stated", sizing owns the floor
		MaxQtyCoin:       maxQtyCoin,
		MinNotionalQuote: minNotional,
		ContractSizeCoin: 1,
	}, true, nil
}
