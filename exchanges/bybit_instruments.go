package exchanges

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Bybit v5 instrument rules — one GET per symbol, linear and spot sharing the
// endpoint with different filter shapes.
//
// Linear: lotSizeFilter{qtyStep, minOrderQty, maxOrderQty, minNotionalValue},
// priceFilter.tickSize, leverageFilter.maxLeverage, status "Trading".
// Spot: lotSizeFilter{basePrecision, minOrderQty, maxOrderQty, minOrderAmt —
// the min notional in quote}, priceFilter.tickSize, no leverage.
// https://bybit-exchange.github.io/docs/v5/market/instrument
// Verified live 2026-09-03; both categories denominate orders in coin.
//
// Bybit wraps errors in HTTP 200 + retCode≠0 + an empty list — retCode must
// be checked, or a rate-limit response reads as "symbol not listed" and
// silently blanks the venue's rules.

type bybitInstrumentsResponse struct {
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
	Result  struct {
		List []struct {
			Symbol      string `json:"symbol"`
			Status      string `json:"status"`
			BaseCoin    string `json:"baseCoin"`
			QuoteCoin   string `json:"quoteCoin"`
			PriceFilter struct {
				TickSize string `json:"tickSize"`
			} `json:"priceFilter"`
			LotSizeFilter struct {
				QtyStep          string `json:"qtyStep"`       // linear
				BasePrecision    string `json:"basePrecision"` // spot
				MinOrderQty      string `json:"minOrderQty"`
				MaxOrderQty      string `json:"maxOrderQty"`
				MinNotionalValue string `json:"minNotionalValue"` // linear
				MinOrderAmt      string `json:"minOrderAmt"`      // spot
			} `json:"lotSizeFilter"`
			LeverageFilter struct {
				MaxLeverage string `json:"maxLeverage"`
			} `json:"leverageFilter"`
		} `json:"list"`
	} `json:"result"`
}

func FetchBybitFuturesInstruments(ctx context.Context, source string, symbols []Symbol) ([]Instrument, error) {
	return fetchBybitInstruments(ctx, source, "linear", "perp", symbols)
}

func FetchBybitSpotInstruments(ctx context.Context, source string, symbols []Symbol) ([]Instrument, error) {
	return fetchBybitInstruments(ctx, source, "spot", "spot", symbols)
}

func fetchBybitInstruments(ctx context.Context, source, category, marketType string, symbols []Symbol) ([]Instrument, error) {
	var out []Instrument
	for _, s := range symbols {
		u := "https://api.bybit.com/v5/market/instruments-info?category=" + category + "&symbol=" + s.Venue
		var resp bybitInstrumentsResponse
		if err := fetchInstrumentJSON(ctx, u, &resp); err != nil {
			// Bybit answers HTTP 200 today, but every per-symbol fetcher
			// honours the 404 sentinel: an unlisted market is absent, not a
			// reason to blank the source (Paradex proved the cost live).
			if errors.Is(err, errInstrumentNotListed) {
				continue
			}
			return nil, err
		}
		inst, ok, err := parseBybitInstrument(resp, source, marketType, s)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, inst)
		}
	}
	return out, nil
}

func parseBybitInstrument(resp bybitInstrumentsResponse, source, marketType string, s Symbol) (Instrument, bool, error) {
	// The two categories signal "not listed" DIFFERENTLY (measured
	// 2026-09-03): spot answers retCode 0 with an empty list, but linear
	// answers retCode 10001 "params error: symbol invalid" — see
	// bybitSaysSymbolNotListed for why only that one shape counts as absent.
	if bybitSaysSymbolNotListed(resp.RetCode, resp.RetMsg) {
		return Instrument{}, false, nil
	}
	if resp.RetCode != 0 {
		return Instrument{}, false, fmt.Errorf("%s %s: venue error retCode %d: %s", source, s.Venue, resp.RetCode, resp.RetMsg)
	}
	if len(resp.Result.List) == 0 {
		return Instrument{}, false, nil // venue does not list this market
	}
	e := resp.Result.List[0]
	if e.Symbol != s.Venue {
		return Instrument{}, false, fmt.Errorf("%s: asked for %s, response names %s", source, s.Venue, e.Symbol)
	}

	// The two categories name the same two rules differently; everything else
	// is shared. Selecting both pairs in one place keeps a future divergence
	// from being fixed in one branch and missed in the other.
	stepField, stepValue := "qtyStep", e.LotSizeFilter.QtyStep
	notionalField, notionalValue := "minNotionalValue", e.LotSizeFilter.MinNotionalValue
	if marketType == "spot" {
		stepField, stepValue = "basePrecision", e.LotSizeFilter.BasePrecision
		notionalField, notionalValue = "minOrderAmt", e.LotSizeFilter.MinOrderAmt
	}

	inst := Instrument{
		Symbol:       s.Standard,
		NativeSymbol: e.Symbol,
		Source:       source,
		MarketType:   marketType,
		Status:       normalizeInstrumentStatus(e.Status, e.Status == "Trading"),
		// baseCoin/quoteCoin are first-class fields in both categories.
		BaseAsset:        e.BaseCoin,
		QuoteAsset:       e.QuoteCoin,
		ContractSizeCoin: 1, // Bybit linear and spot orders are in coin
	}
	var err error
	if inst.TickSizeQuote, err = parseInstrumentFloat(source, e.Symbol, "tickSize", e.PriceFilter.TickSize); err != nil {
		return Instrument{}, false, err
	}
	if inst.StepSizeCoin, err = parseInstrumentFloat(source, e.Symbol, stepField, stepValue); err != nil {
		return Instrument{}, false, err
	}
	if inst.MinQtyCoin, err = parseInstrumentFloat(source, e.Symbol, "minOrderQty", e.LotSizeFilter.MinOrderQty); err != nil {
		return Instrument{}, false, err
	}
	if inst.MaxQtyCoin, err = parseInstrumentFloat(source, e.Symbol, "maxOrderQty", e.LotSizeFilter.MaxOrderQty); err != nil {
		return Instrument{}, false, err
	}
	if inst.MinNotionalQuote, err = parseInstrumentFloat(source, e.Symbol, notionalField, notionalValue); err != nil {
		return Instrument{}, false, err
	}
	if e.LeverageFilter.MaxLeverage != "" {
		if inst.MaxLeverageX, err = parseInstrumentFloat(source, e.Symbol, "maxLeverage", e.LeverageFilter.MaxLeverage); err != nil {
			return Instrument{}, false, err
		}
	}
	return inst, true, nil
}

// bybitSaysSymbolNotListed decides whether a Bybit error body means "this market
// does not exist here" rather than "something went wrong".
//
// 10001 is Bybit's GENERIC parameter error, so only the symbol-invalid form
// counts as absent — any other 10001 is a real bug that must stay loud. retMsg
// is human-readable prose, not a contract: Bybit spells this "Symbol Is
// Invalid" on other v5 endpoints, so the match folds case and looks for the two
// words separately. Wording drift still fails toward the loud branch, which is
// the safe direction.
//
// It lives here, shared with the funding history fetcher, so the prose match
// exists once: two copies would drift and one of them would start reading a
// real failure as an empty market.
func bybitSaysSymbolNotListed(retCode int, retMsg string) bool {
	if retCode != 10001 {
		return false
	}
	msg := strings.ToLower(retMsg)
	return strings.Contains(msg, "symbol") &&
		(strings.Contains(msg, "invalid") || strings.Contains(msg, "not exist"))
}
