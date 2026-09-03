package exchanges

import (
	"context"
	"encoding/json"
)

// Binance instrument rules.
//
// Futures: GET /fapi/v1/exchangeInfo. Spot: GET /api/v3/exchangeInfo. BOTH are
// fetched unfiltered and the requested symbols selected locally: the futures
// endpoint takes no filter at all, and the spot endpoint's ?symbols=[...] form
// fails the WHOLE request with HTTP 400 (-1121) when any one symbol is
// unknown — one delisted pair in config.yaml would blank every spot rule.
// Fetching the full list keeps the InstrumentFetchFunc contract: an unlisted
// symbol is simply absent.
// Rules live in the filters array: PRICE_FILTER.tickSize, LOT_SIZE
// (stepSize/minQty/maxQty), and MIN_NOTIONAL.notional (futures) /
// NOTIONAL.minNotional (spot).
// https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Exchange-Information
// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/general-endpoints
//
// Max leverage is NOT here: Binance publishes leverage brackets only behind
// the signed leverageBracket endpoint, which is phase-4 territory —
// MaxLeverageX stays 0.

type binanceExchangeInfo struct {
	Symbols []struct {
		Symbol       string          `json:"symbol"`
		Status       string          `json:"status"`
		ContractType string          `json:"contractType"` // futures only; "" on spot
		Filters      []binanceFilter `json:"filters"`
	} `json:"symbols"`
}

type binanceFilter struct {
	FilterType  string `json:"filterType"`
	TickSize    string `json:"tickSize"`
	StepSize    string `json:"stepSize"`
	MinQty      string `json:"minQty"`
	MaxQty      string `json:"maxQty"`
	Notional    string `json:"notional"`    // futures MIN_NOTIONAL
	MinNotional string `json:"minNotional"` // spot NOTIONAL
}

func FetchBinanceFuturesInstruments(ctx context.Context, source string, symbols []Symbol) ([]Instrument, error) {
	var info binanceExchangeInfo
	if err := fetchInstrumentJSON(ctx, "https://fapi.binance.com/fapi/v1/exchangeInfo", &info); err != nil {
		return nil, err
	}
	return parseBinanceInstruments(info, source, "perp", symbols)
}

func FetchBinanceSpotInstruments(ctx context.Context, source string, symbols []Symbol) ([]Instrument, error) {
	var info binanceExchangeInfo
	if err := fetchInstrumentJSON(ctx, "https://api.binance.com/api/v3/exchangeInfo", &info); err != nil {
		return nil, err
	}
	return parseBinanceInstruments(info, source, "spot", symbols)
}

func parseBinanceInstruments(info binanceExchangeInfo, source, marketType string, symbols []Symbol) ([]Instrument, error) {
	standardByNative := make(map[string]string, len(symbols))
	for _, s := range symbols {
		standardByNative[s.Venue] = s.Standard
	}

	var out []Instrument
	for _, entry := range info.Symbols {
		standard, wanted := standardByNative[entry.Symbol]
		if !wanted {
			continue
		}
		// The futures list carries delivery contracts under the same symbol
		// family; only the perpetual is this project's market.
		if marketType == "perp" && entry.ContractType != "PERPETUAL" {
			continue
		}
		inst := Instrument{
			Symbol:           standard,
			NativeSymbol:     entry.Symbol,
			Source:           source,
			MarketType:       marketType,
			Status:           normalizeInstrumentStatus(entry.Status, entry.Status == "TRADING"),
			ContractSizeCoin: 1, // Binance orders are denominated in coin
		}
		for _, f := range entry.Filters {
			var err error
			switch f.FilterType {
			case "PRICE_FILTER":
				inst.TickSizeQuote, err = parseInstrumentFloat(source, entry.Symbol, "tickSize", f.TickSize)
			case "LOT_SIZE":
				if inst.StepSizeCoin, err = parseInstrumentFloat(source, entry.Symbol, "stepSize", f.StepSize); err != nil {
					return nil, err
				}
				if inst.MinQtyCoin, err = parseInstrumentFloat(source, entry.Symbol, "minQty", f.MinQty); err != nil {
					return nil, err
				}
				inst.MaxQtyCoin, err = parseInstrumentFloat(source, entry.Symbol, "maxQty", f.MaxQty)
			case "MIN_NOTIONAL":
				inst.MinNotionalQuote, err = parseInstrumentFloat(source, entry.Symbol, "notional", f.Notional)
			case "NOTIONAL":
				inst.MinNotionalQuote, err = parseInstrumentFloat(source, entry.Symbol, "minNotional", f.MinNotional)
			}
			if err != nil {
				return nil, err
			}
		}
		out = append(out, inst)
	}
	return out, nil
}

// trimInstrumentList re-encodes a { "<arrayKey>": [ {symbol: ...}, ... ] }
// response keeping only the requested symbols. Used by the capture test for
// the two venues whose only endpoint is an unfiltered ~1MB list (Binance
// futures, Kraken): kept entries are byte-verbatim, the array is filtered.
func trimInstrumentList(raw []byte, arrayKey string, natives map[string]bool) ([]byte, error) {
	var full map[string]json.RawMessage
	if err := json.Unmarshal(raw, &full); err != nil {
		return nil, err
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(full[arrayKey], &entries); err != nil {
		return nil, err
	}
	kept := make([]json.RawMessage, 0, len(natives))
	for _, entry := range entries {
		var head struct {
			Symbol string `json:"symbol"`
		}
		if err := json.Unmarshal(entry, &head); err != nil {
			return nil, err
		}
		if natives[head.Symbol] {
			kept = append(kept, entry)
		}
	}
	keptRaw, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{arrayKey: keptRaw})
}
