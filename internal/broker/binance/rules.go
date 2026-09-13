package binance

import (
	"context"
	"fmt"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/broker"
)

// Trading rules and a reference price, read from the TESTNET this client is
// about to send to.
//
// # Why this is not a duplicate of exchanges/binance's instrument fetcher
//
// That one reads PUBLIC MAINNET market data, which is what the scanner needs
// and all it is allowed to touch — exchanges/ may not import internal/ and
// holds nothing credential-adjacent. This reads a DIFFERENT VENUE INSTANCE:
// Binance's testnet, whose rules are its own. They really do differ, measured
// 2026-09-13 for BTCUSDT alone: futures testnet wants tick 0.10, step 0.0001,
// MIN_NOTIONAL 50, while spot testnet wants 0.01, 0.00001 and 5.
//
// An order is rounded by the rules of the market it is sent to, read at run
// time. A snapshot in the corpus is a different venue's answer, and where the
// two agree it is luck.
//
// Both endpoints are PUBLIC — no signature, no credential — so this costs
// weight and nothing else.

// MarketRules is the instrument in the repo's normalized shape, plus the one
// venue limit that shape has no room for.
//
// exchanges.Instrument carries what the SCANNER needs — the size and price
// grids. It has no field for how far from the market a resting order may be
// priced, because nothing before step 4.2 places one, and exchanges/ is not
// mine to widen (CLAUDE.md's dependency rules, and this package is on the
// credential side of that line). So the band rides alongside.
type MarketRules struct {
	exchanges.Instrument

	// BuyPriceFloorFrac is the LOWEST fraction of the reference price at which
	// a BUY may be priced. 0 means this market publishes no such limit.
	//
	// Measured 2026-09-13, and the two markets differ in kind, not degree:
	//
	//   spot     PERCENT_PRICE_BY_SIDE  bidMultiplierDown 0.5, against an
	//            average over avgPriceMins=5 — so a buy may not rest below
	//            half the FIVE-MINUTE AVERAGE price.
	//   futures  PERCENT_PRICE          multiplierDown 0.95, multiplierUp 1.05,
	//            and for a BUY only the UPPER bound binds (price <= mark x
	//            1.05). A buy far below the mark is not restricted, which is
	//            why the same order that spot refused rested happily here.
	//
	// So this is 0.5 on spot and 0 on futures, and a caller that assumed one
	// number for "Binance" would be wrong on one of them.
	BuyPriceFloorFrac float64
}

// FetchInstrument reads one symbol's trading rules from the market's own
// exchangeInfo and returns them in the repo's normalized shape, so
// broker.RoundOrder takes the same type it takes everywhere else.
func (c *Client) FetchInstrument(ctx context.Context, symbol string) (MarketRules, error) {
	ep := broker.FuturesExchangeInfo
	if c.market == broker.MarketSpot {
		ep = broker.SpotExchangeInfo
	}
	var info struct {
		Symbols []struct {
			Symbol     string `json:"symbol"`
			Status     string `json:"status"`
			BaseAsset  string `json:"baseAsset"`
			QuoteAsset string `json:"quoteAsset"`
			Filters    []struct {
				FilterType string `json:"filterType"`
				// PRICE_FILTER
				TickSize string `json:"tickSize"`
				// LOT_SIZE / MARKET_LOT_SIZE
				StepSize string `json:"stepSize"`
				MinQty   string `json:"minQty"`
				MaxQty   string `json:"maxQty"`
				// MIN_NOTIONAL (futures) publishes `notional`;
				// NOTIONAL / MIN_NOTIONAL (spot) publishes `minNotional`.
				Notional    string `json:"notional"`
				MinNotional string `json:"minNotional"`
				// PERCENT_PRICE_BY_SIDE (spot) / PERCENT_PRICE (futures)
				BidMultiplierDown string `json:"bidMultiplierDown"`
			} `json:"filters"`
		} `json:"symbols"`
	}
	if err := c.http.GetPublic(ctx, ep, []broker.Param{{Key: "symbol", Value: symbol}}, &info); err != nil {
		return MarketRules{}, classify(err)
	}

	for _, s := range info.Symbols {
		if s.Symbol != symbol {
			continue
		}
		out := MarketRules{}
		inst := exchanges.Instrument{
			Symbol: s.Symbol, NativeSymbol: s.Symbol,
			Source:           string(c.market),
			BaseAsset:        s.BaseAsset,
			QuoteAsset:       s.QuoteAsset,
			Status:           normalizeStatus(s.Status),
			ContractSizeCoin: 1, // both markets denominate orders in coin
		}
		for _, f := range s.Filters {
			switch f.FilterType {
			case "PRICE_FILTER":
				inst.TickSizeQuote = parseFloat(f.TickSize)
			case "LOT_SIZE":
				inst.StepSizeCoin = parseFloat(f.StepSize)
				inst.MinQtyCoin = parseFloat(f.MinQty)
				inst.MaxQtyCoin = parseFloat(f.MaxQty)
			case "MARKET_LOT_SIZE":
				// The repo's convention: MaxQtyCoin carries the SMALLER of the
				// limit and market ceilings, because above it the size cannot
				// be done as one order of at least one type.
				if m := parseFloat(f.MaxQty); m > 0 && (inst.MaxQtyCoin == 0 || m < inst.MaxQtyCoin) {
					inst.MaxQtyCoin = m
				}
			case "MIN_NOTIONAL", "NOTIONAL":
				if v := parseFloat(f.Notional); v > 0 {
					inst.MinNotionalQuote = v
				} else if v := parseFloat(f.MinNotional); v > 0 {
					inst.MinNotionalQuote = v
				}
			case "PERCENT_PRICE_BY_SIDE":
				// Only the BID floor is read: it is the only side step 4.2
				// places on, and a limit that is not used is a limit nobody
				// has checked the spelling of.
				out.BuyPriceFloorFrac = parseFloat(f.BidMultiplierDown)
			}
		}
		out.Instrument = inst
		return out, nil
	}
	return MarketRules{}, fmt.Errorf("binance: %s lists no symbol %q on %s", c.market, symbol, c.http.BaseURL())
}

// normalizeStatus maps the venue's word to the repo's normalized value, the
// same way exchanges/ does, so a caller comparing against
// exchanges.StatusTrading gets the answer it expects.
func normalizeStatus(s string) string {
	switch s {
	case "TRADING", "Trading", "live":
		return exchanges.StatusTrading
	}
	return s
}

// FetchPriceQuote reads the symbol's latest price, for sizing an order that
// must rest far from the market rather than fill.
func (c *Client) FetchPriceQuote(ctx context.Context, symbol string) (float64, error) {
	ep := broker.FuturesTickerPrice
	if c.market == broker.MarketSpot {
		ep = broker.SpotTickerPrice
	}
	var ticker struct {
		Symbol string `json:"symbol"`
		Price  string `json:"price"`
	}
	if err := c.http.GetPublic(ctx, ep, []broker.Param{{Key: "symbol", Value: symbol}}, &ticker); err != nil {
		return 0, classify(err)
	}
	price := parseFloat(ticker.Price)
	if price <= 0 {
		return 0, fmt.Errorf("binance: %s answered no usable price for %s", ep.Path, symbol)
	}
	return price, nil
}

// MaintenanceBracket is one tier of the venue's maintenance-margin schedule.
//
// It is deliberately NOT internal/risk.Bracket: this package sits on the
// credential side of the dependency line and has no business importing the risk
// model. The caller maps it, and in mapping it is the one that decides the
// schedule is "verified" — which it is, because it came from the venue.
//
// PLAN's Q1 named this as Binance's one real cost: `leverageBracket` needs an
// API key, so `margin.verified: false` in config.yaml and internal/strategy
// REFUSES to open a levered position there rather than assuming a rate. A
// testnet key is what turns that refusal into a number.
type MaintenanceBracket struct {
	Symbol             string
	Tier               int
	NotionalFloorQuote float64
	NotionalCapQuote   float64
	MaintMarginFrac    float64
	MaxLeverage        float64
}

// FetchMaintenanceBracket reads the tier that covers one notional.
//
// GET /fapi/v1/leverageBracket — USER_DATA, "Request Weight: 1 IP weight", read
// 2026-09-13. Response rows carry {symbol, bracket, initialLeverage,
// notionalCap, notionalFloor, maintMarginRatio, cum}.
// https://developers.binance.com/docs/derivatives/usds-margined-futures/account/rest-api/Notional-and-Leverage-Brackets
//
// A notional that falls in no published tier is an ERROR, not the first tier: a
// schedule that does not cover the size being traded says nothing about that
// size, and picking the nearest tier would understate the requirement exactly
// where the position is largest.
func (c *Client) FetchMaintenanceBracket(ctx context.Context, symbol string, notionalQuote float64) (MaintenanceBracket, error) {
	if c.market != broker.MarketFuturesUSDM {
		return MaintenanceBracket{}, fmt.Errorf("%w: %q publishes no maintenance schedule", broker.ErrNotSupported, c.market)
	}
	var rows []struct {
		Symbol   string `json:"symbol"`
		Brackets []struct {
			Bracket          int     `json:"bracket"`
			InitialLeverage  float64 `json:"initialLeverage"`
			NotionalCap      float64 `json:"notionalCap"`
			NotionalFloor    float64 `json:"notionalFloor"`
			MaintMarginRatio float64 `json:"maintMarginRatio"`
			Cum              float64 `json:"cum"`
		} `json:"brackets"`
	}
	if err := c.http.GetSigned(ctx, broker.FuturesLeverageBracket,
		[]broker.Param{{Key: "symbol", Value: symbol}}, &rows); err != nil {
		return MaintenanceBracket{}, classify(err)
	}
	for _, row := range rows {
		if row.Symbol != symbol {
			continue
		}
		for _, b := range row.Brackets {
			if notionalQuote >= b.NotionalFloor && (b.NotionalCap <= 0 || notionalQuote <= b.NotionalCap) {
				return MaintenanceBracket{
					Symbol: symbol, Tier: b.Bracket,
					NotionalFloorQuote: b.NotionalFloor, NotionalCapQuote: b.NotionalCap,
					MaintMarginFrac: b.MaintMarginRatio, MaxLeverage: b.InitialLeverage,
				}, nil
			}
		}
	}
	return MaintenanceBracket{}, fmt.Errorf("binance: %s publishes no maintenance tier covering a notional of %v", symbol, notionalQuote)
}
