package exchanges

import (
	"context"
	"math"
)

// Hyperliquid instrument rules — POST /info {"type":"meta"}.
// https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/info-endpoint/perpetuals
//
// Sizes are in coin with 10^-szDecimals steps. Prices have no constant tick:
// the venue's rule is at most 5 significant figures (and at most
// 6−szDecimals decimals), so TickSizeQuote stays 0 — the "defined by rule" marker.
// The minimum order value is a documented flat $10.
// https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/tick-and-lot-size
// Verified live 2026-09-03 (BTC: szDecimals 5, maxLeverage 40).

// hyperliquidMinOrderNotionalUSD is the venue-wide minimum order value —
// policy, not a per-asset API field, so no capture can pin it: "Minimum order
// value is $10" per the contract specifications page (re-verify there when
// sizing refusals disagree with the venue):
// https://hyperliquid.gitbook.io/hyperliquid-docs/trading/contract-specifications
const hyperliquidMinOrderNotionalUSD = 10

type hyperliquidMetaResponse struct {
	Universe []struct {
		Name        string  `json:"name"`
		SzDecimals  float64 `json:"szDecimals"`
		MaxLeverage float64 `json:"maxLeverage"`
		IsDelisted  bool    `json:"isDelisted"`
	} `json:"universe"`
}

func FetchHyperliquidInstruments(ctx context.Context, source string, symbols []Symbol) ([]Instrument, error) {
	var resp hyperliquidMetaResponse
	if err := postInstrumentJSON(ctx, "https://api.hyperliquid.xyz/info", `{"type":"meta"}`, &resp); err != nil {
		return nil, err
	}
	return parseHyperliquidInstruments(resp, source, symbols)
}

func parseHyperliquidInstruments(resp hyperliquidMetaResponse, source string, symbols []Symbol) ([]Instrument, error) {
	standardByNative := make(map[string]string, len(symbols))
	for _, s := range symbols {
		standardByNative[s.Venue] = s.Standard
	}
	var out []Instrument
	for _, e := range resp.Universe {
		standard, wanted := standardByNative[e.Name]
		if !wanted {
			continue
		}
		stepCoin := math.Pow(10, -e.SzDecimals)
		out = append(out, Instrument{
			Symbol:           standard,
			NativeSymbol:     e.Name,
			Source:           source,
			MarketType:       "perp",
			Status:           normalizeInstrumentStatus("delisted", !e.IsDelisted),
			TickSizeQuote:    0, // 5-significant-figure rule, no constant tick
			StepSizeCoin:     stepCoin,
			MinQtyCoin:       stepCoin,
			MinNotionalQuote: hyperliquidMinOrderNotionalUSD,
			ContractSizeCoin: 1,
			MaxLeverageX:     e.MaxLeverage,
		})
	}
	return out, nil
}
