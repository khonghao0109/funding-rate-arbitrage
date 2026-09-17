package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
)

// Market data from the host this client TRADES on — testnet or demo — never
// from exchanges/bybit, which reads production for the scanner. A testnet book
// is another matching engine; an order is rounded and sized by the rules and the
// book of the venue instance it is sent to (the reasoning of binance/rules.go).
// The demo page lists Market "All | all endpoints" as served on api-demo.

// MarketRules is the instrument in the repo's normalized shape plus what that
// shape has no room for.
type MarketRules struct {
	exchanges.Instrument

	ContractType string // "LinearPerpetual" — anything else is refused
	SettleCoin   string // "USDT" — anything else is refused

	// FundingIntervalSec is the venue's fundingInterval ("Funding interval
	// (minute)") converted to seconds. Read per symbol; never assumed (rule 3).
	FundingIntervalSec int64
}

// FetchInstrument reads one symbol's trading rules.
//
// GET /v5/market/instruments-info?category=linear&symbol=… — lotSizeFilter
// {minNotionalValue, maxOrderQty "for Limit and PostOnly order", maxMktOrderQty
// "for Market order", minOrderQty, qtyStep}, priceFilter {tickSize},
// fundingInterval (minutes). MaxQtyCoin carries the SMALLER of the two ceilings,
// the repo's convention (exchanges.Instrument).
func (c *Client) FetchInstrument(ctx context.Context, symbol string) (MarketRules, error) {
	var res struct {
		List []struct {
			Symbol          string `json:"symbol"`
			ContractType    string `json:"contractType"`
			Status          string `json:"status"`
			BaseCoin        string `json:"baseCoin"`
			QuoteCoin       string `json:"quoteCoin"`
			SettleCoin      string `json:"settleCoin"`
			FundingInterval int64  `json:"fundingInterval"`
			PriceFilter     struct {
				TickSize string `json:"tickSize"`
			} `json:"priceFilter"`
			LotSizeFilter struct {
				MinNotionalValue string `json:"minNotionalValue"`
				MaxOrderQty      string `json:"maxOrderQty"`
				MaxMktOrderQty   string `json:"maxMktOrderQty"`
				MinOrderQty      string `json:"minOrderQty"`
				QtyStep          string `json:"qtyStep"`
			} `json:"lotSizeFilter"`
		} `json:"list"`
	}
	if err := c.getPublic(ctx, epInstruments, []broker.Param{
		{Key: "category", Value: categoryLinear}, {Key: "symbol", Value: symbol},
	}, &res); err != nil {
		return MarketRules{}, err
	}
	for _, s := range res.List {
		if s.Symbol != symbol {
			continue
		}
		if s.ContractType != "LinearPerpetual" {
			return MarketRules{}, fmt.Errorf("bybit: %s is %q, not a LinearPerpetual — refused", symbol, s.ContractType)
		}
		if s.SettleCoin != settleCoinUSDT {
			return MarketRules{}, fmt.Errorf("bybit: %s settles in %q, not USDT — refused", symbol, s.SettleCoin)
		}
		if s.FundingInterval <= 0 {
			return MarketRules{}, fmt.Errorf("bybit: %s publishes no fundingInterval — refused rather than assumed (rule 3)", symbol)
		}
		inst := exchanges.Instrument{
			Symbol: s.Symbol, NativeSymbol: s.Symbol, Source: c.mode.SourceID(), MarketType: "perp",
			BaseAsset: s.BaseCoin, QuoteAsset: s.QuoteCoin,
			Status:           normalizeInstrumentStatus(s.Status),
			ContractSizeCoin: 1, // "Perps, Futures & Option: always order by qty" — in coin
		}
		var maxLimit, maxMarket float64
		for _, f := range []struct {
			name, raw string
			into      *float64
		}{
			{"tickSize", s.PriceFilter.TickSize, &inst.TickSizeQuote},
			{"qtyStep", s.LotSizeFilter.QtyStep, &inst.StepSizeCoin},
			{"minOrderQty", s.LotSizeFilter.MinOrderQty, &inst.MinQtyCoin},
			{"minNotionalValue", s.LotSizeFilter.MinNotionalValue, &inst.MinNotionalQuote},
			{"maxOrderQty", s.LotSizeFilter.MaxOrderQty, &maxLimit},
			{"maxMktOrderQty", s.LotSizeFilter.MaxMktOrderQty, &maxMarket},
		} {
			v, err := parseNumber(f.name, f.raw)
			if err != nil {
				return MarketRules{}, err
			}
			*f.into = v
		}
		inst.MaxQtyCoin = maxLimit
		if maxMarket > 0 && (inst.MaxQtyCoin == 0 || maxMarket < inst.MaxQtyCoin) {
			inst.MaxQtyCoin = maxMarket
		}
		return MarketRules{Instrument: inst, ContractType: s.ContractType, SettleCoin: s.SettleCoin,
			FundingIntervalSec: s.FundingInterval * 60}, nil
	}
	return MarketRules{}, fmt.Errorf("bybit: %s lists no linear symbol %q", c.http.BaseURL(), symbol)
}

func normalizeInstrumentStatus(s string) string {
	if s == "Trading" {
		return exchanges.StatusTrading
	}
	return s
}

// depthLevels asked for per side. The ceiling is 1000; 500 is asked because
// 100 levels did not reach 0.1% of mid on 7 of 9 venues (CLAUDE.md's depth
// trap), and every request costs the same single unit of the IP limit whatever
// its size. It is a REST snapshot, per rule 10.
const depthLevels = 500

// FetchDepthBook reads one symbol's book in the repo's normalized shape.
//
// GET /v5/market/orderbook — b "Bid, buyer. Sorted by price in descending
// order", a "Ask, seller. Sorted by price in ascending order", each [price,
// size] strings in COIN for linear; ts "The timestamp (ms) that the system
// generates the data". Levels are sorted again anyway: exchanges.FinishDepthBook
// does not trust a documented order (the Kraken lesson).
func (c *Client) FetchDepthBook(ctx context.Context, symbol string) (exchanges.DepthBook, error) {
	var res struct {
		Symbol string      `json:"s"`
		Bids   [][2]string `json:"b"`
		Asks   [][2]string `json:"a"`
		TsMs   json.Number `json:"ts"`
	}
	if err := c.getPublic(ctx, epOrderbook, []broker.Param{
		{Key: "category", Value: categoryLinear}, {Key: "symbol", Value: symbol},
		{Key: "limit", Value: strconv.Itoa(depthLevels)},
	}, &res); err != nil {
		return exchanges.DepthBook{}, err
	}
	if res.Symbol != symbol {
		return exchanges.DepthBook{}, fmt.Errorf("bybit: asked for the %s book, the answer names %q", symbol, res.Symbol)
	}
	ts, _ := res.TsMs.Int64()
	book := exchanges.DepthBook{
		Symbol: symbol, Source: c.mode.SourceID(), VenueTimeMs: ts,
		Bids: levels(res.Bids), Asks: levels(res.Asks), IsContractBook: false,
	}
	finished, err := exchanges.FinishDepthBook(book)
	if err != nil {
		return exchanges.DepthBook{}, fmt.Errorf("bybit %s: %w", c.mode.SourceID(), err)
	}
	return finished, nil
}

// levels skips a level that does not parse rather than inventing a value.
func levels(raw [][2]string) []exchanges.DepthLevel {
	out := make([]exchanges.DepthLevel, 0, len(raw))
	for _, lv := range raw {
		price, err1 := strconv.ParseFloat(lv[0], 64)
		qty, err2 := strconv.ParseFloat(lv[1], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, exchanges.DepthLevel{PriceQuote: price, QtyNative: qty})
	}
	return out
}

// FundingRate is one SETTLED funding rate.
type FundingRate struct {
	Symbol string
	// SettledAtMs is fundingRateTimestamp, stored verbatim.
	SettledAtMs int64
	// RatePerIntervalFrac is the rate for that symbol's own period; the period
	// is not stated here and is not assumed (rule 3) — FetchInstrument states
	// the current one, and history may have run on another.
	RatePerIntervalFrac float64
}

// FundingRateHistory reads settled rates ending at endMs (0 = now), oldest
// first. GET /v5/market/funding/history — "Passing only endTime returns 200
// records up till endTime"; "Passing only startTime returns an error", so it is
// never sent alone; limit [1, 200].
func (c *Client) FundingRateHistory(ctx context.Context, symbol string, endMs int64, limit int) ([]FundingRate, error) {
	if limit <= 0 || limit > 200 {
		return nil, fmt.Errorf("bybit: funding history limit %d is outside the documented [1, 200]", limit)
	}
	params := []broker.Param{{Key: "category", Value: categoryLinear}, {Key: "symbol", Value: symbol},
		{Key: "limit", Value: strconv.Itoa(limit)}}
	if endMs > 0 {
		params = append(params, broker.Param{Key: "endTime", Value: strconv.FormatInt(endMs, 10)})
	}
	var res struct {
		List []struct {
			Symbol               string `json:"symbol"`
			FundingRate          string `json:"fundingRate"`
			FundingRateTimestamp string `json:"fundingRateTimestamp"`
		} `json:"list"`
	}
	if err := c.getPublic(ctx, epFundingHistory, params, &res); err != nil {
		return nil, err
	}
	out := make([]FundingRate, 0, len(res.List))
	for _, r := range res.List {
		if r.Symbol != symbol {
			continue
		}
		if r.FundingRate == "" {
			return nil, fmt.Errorf("bybit: a %s settlement carries no fundingRate — refused rather than read as zero", symbol)
		}
		rate, err := parseNumber("fundingRate", r.FundingRate)
		if err != nil {
			return nil, err
		}
		ts, err := parseMs("fundingRateTimestamp", r.FundingRateTimestamp)
		if err != nil || ts <= 0 {
			return nil, fmt.Errorf("bybit: a %s settlement carries no usable fundingRateTimestamp", symbol)
		}
		out = append(out, FundingRate{Symbol: symbol, SettledAtMs: ts, RatePerIntervalFrac: rate})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SettledAtMs < out[j].SettledAtMs })
	return out, nil
}

// SyncClock measures the venue clock through broker.Client — the same
// measurement every signed call is corrected by — and returns the skew, venue
// minus local, in milliseconds.
func (c *Client) SyncClock(ctx context.Context) (int64, error) { return c.http.SyncClock(ctx) }
