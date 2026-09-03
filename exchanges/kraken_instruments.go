package exchanges

import (
	"context"
	"math"
)

// Kraken Futures instrument rules — GET /derivatives/api/v3/instruments (the
// whole list; no filter parameter exists), requested symbols selected here.
// https://docs.kraken.com/api/docs/futures-api/trading/get-instruments
//
// This endpoint SETTLED the step-1.2 contradiction (trap table, "Contracts"
// row): PF_ perpetuals really are denominated in contracts, but
// contractSize=1 means one contract is ONE BASE UNIT (1 BTC for PF_XBTUSD),
// traded in 10^-contractValueTradePrecision steps (precision 4 → 0.0001
// contracts). Contract counts are therefore numerically equal to coin —
// which is why the 1.2 measurement saw a coin-looking book quantity of
// 0.0929 with BTC near $77.5k. Both the survey ("contracts") and the
// measurement ("looks like coin") were right.
//
// No explicit minimum order size or minimum notional is published on this
// endpoint, so MinQtyCoin stays 0 = "not stated" — the sizing layer owns the
// at-least-one-step floor. maxPositionSize is a POSITION cap, not an
// order-size cap, so
// MaxQtyCoin stays 0. Max leverage is derived from the first margin level:
// initialMargin 0.01 → 100×.
// Verified live 2026-09-03.

type krakenInstrumentsResponse struct {
	Instruments []struct {
		Symbol string `json:"symbol"`
		Type   string `json:"type"`
		// Kraken declares base/quote directly — and declares base "BTC" for
		// PF_XBTUSD, resolving its own XBT naming, so no alias table is
		// needed anywhere downstream. Verified live 2026-09-03.
		Base                        string  `json:"base"`
		Quote                       string  `json:"quote"`
		Tradeable                   bool    `json:"tradeable"`
		TickSize                    float64 `json:"tickSize"`
		ContractSize                float64 `json:"contractSize"`
		ContractValueTradePrecision float64 `json:"contractValueTradePrecision"`
		MarginLevels                []struct {
			InitialMargin float64 `json:"initialMargin"`
		} `json:"marginLevels"`
	} `json:"instruments"`
}

func FetchKrakenInstruments(ctx context.Context, source string, symbols []Symbol) ([]Instrument, error) {
	var resp krakenInstrumentsResponse
	if err := fetchInstrumentJSON(ctx, "https://futures.kraken.com/derivatives/api/v3/instruments", &resp); err != nil {
		return nil, err
	}
	return parseKrakenInstruments(resp, source, symbols)
}

func parseKrakenInstruments(resp krakenInstrumentsResponse, source string, symbols []Symbol) ([]Instrument, error) {
	standardByNative := make(map[string]string, len(symbols))
	for _, s := range symbols {
		standardByNative[s.Venue] = s.Standard
	}
	var out []Instrument
	for _, e := range resp.Instruments {
		standard, wanted := standardByNative[e.Symbol]
		if !wanted {
			continue
		}
		stepContracts := math.Pow(10, -e.ContractValueTradePrecision)
		inst := Instrument{
			Symbol:       standard,
			NativeSymbol: e.Symbol,
			Source:       source,
			MarketType:   "perp",
			// Kraken publishes only a tradeable bool, no status word — the
			// fallback is ours, and must be a STATE, not the product type.
			Status:        normalizeInstrumentStatus("untradeable", e.Tradeable),
			BaseAsset:     e.Base,
			QuoteAsset:    e.Quote,
			TickSizeQuote: e.TickSize,
			StepSizeCoin:  stepContracts * e.ContractSize,
			// Kraken publishes no minimum order size — 0 means "not stated"
			// (the sizing layer enforces its own at-least-one-step floor).
			MinQtyCoin:       0,
			IsContract:       true,
			ContractSizeCoin: e.ContractSize,
		}
		if len(e.MarginLevels) > 0 && e.MarginLevels[0].InitialMargin > 0 {
			inst.MaxLeverageX = 1 / e.MarginLevels[0].InitialMargin
		}
		out = append(out, inst)
	}
	return out, nil
}
