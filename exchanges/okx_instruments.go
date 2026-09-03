package exchanges

import (
	"context"
	"errors"
	"fmt"
)

// OKX instrument rules — GET /api/v5/public/instruments?instType=SWAP, one
// call per symbol.
//
// OKX denominates swap orders in CONTRACTS: lotSz and minSz are contract
// counts (fractional — BTC-USDT-SWAP quotes lotSz=minSz=0.01), and one
// contract is ctVal × ctMult of ctValCcy, the BASE coin for linear swaps
// (BTC-USDT-SWAP: ctVal 0.01, ctMult 1, ctValCcy BTC → 0.01 BTC/contract,
// verified live 2026-09-03). Inverse contracts value ctVal in quote currency,
// which this conversion cannot represent — ctType must be "linear".
// state "live" is the tradable status.
// https://www.okx.com/docs-v5/en/#public-data-rest-api-get-instruments

type okxInstrumentsResponse struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []struct {
		InstID    string `json:"instId"`
		State     string `json:"state"`
		TickSz    string `json:"tickSz"`
		LotSz     string `json:"lotSz"`
		MinSz     string `json:"minSz"`
		CtVal     string `json:"ctVal"`
		CtMult    string `json:"ctMult"`
		CtValCcy  string `json:"ctValCcy"`
		SettleCcy string `json:"settleCcy"`
		CtType    string `json:"ctType"`
		Lever     string `json:"lever"`
	} `json:"data"`
}

func FetchOKXInstruments(ctx context.Context, source string, symbols []Symbol) ([]Instrument, error) {
	var out []Instrument
	for _, s := range symbols {
		u := "https://www.okx.com/api/v5/public/instruments?instType=SWAP&instId=" + s.Venue
		var resp okxInstrumentsResponse
		if err := fetchInstrumentJSON(ctx, u, &resp); err != nil {
			// OKX answers HTTP 200 + code 51001 today, but every per-symbol
			// fetcher honours the 404 sentinel too: an unlisted market is
			// absent, never a reason to blank the whole source.
			if errors.Is(err, errInstrumentNotListed) {
				continue
			}
			return nil, err
		}
		inst, ok, err := parseOKXInstrument(resp, source, s)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, inst)
		}
	}
	return out, nil
}

func parseOKXInstrument(resp okxInstrumentsResponse, source string, s Symbol) (Instrument, bool, error) {
	// OKX wraps errors in HTTP 200 + code≠"0" + empty data — without this
	// check a "system busy" response reads as "market not listed" and blanks
	// the venue's 100×-sensitive contract sizes. The one code that really
	// MEANS "not listed" is 51001 ("Instrument ID ... doesn't exist",
	// https://www.okx.com/docs-v5/en/#error-code, measured 2026-09-03):
	// that is "absent" per the InstrumentFetchFunc contract, and treating it
	// as an error would let one unsupported pair blank the whole source.
	if resp.Code == "51001" {
		return Instrument{}, false, nil
	}
	if resp.Code != "" && resp.Code != "0" {
		return Instrument{}, false, fmt.Errorf("%s %s: venue error code %s: %s", source, s.Venue, resp.Code, resp.Msg)
	}
	if len(resp.Data) == 0 {
		return Instrument{}, false, nil // venue does not list this market
	}
	e := resp.Data[0]
	if e.InstID != s.Venue {
		return Instrument{}, false, fmt.Errorf("%s: asked for %s, response names %s", source, s.Venue, e.InstID)
	}
	if e.CtType != "linear" {
		// An inverse contract's ctVal is in QUOTE currency; converting it
		// with this code would produce coin quantities that are wrong
		// without looking wrong.
		return Instrument{}, false, fmt.Errorf("%s %s: ctType %q is not linear — inverse contracts are not supported", source, e.InstID, e.CtType)
	}
	ctVal, err := parseInstrumentFloat(source, e.InstID, "ctVal", e.CtVal)
	if err != nil {
		return Instrument{}, false, err
	}
	ctMult, err := parseInstrumentFloat(source, e.InstID, "ctMult", e.CtMult)
	if err != nil {
		return Instrument{}, false, err
	}
	contractSizeCoin := ctVal * ctMult
	if contractSizeCoin <= 0 {
		return Instrument{}, false, fmt.Errorf("%s %s: ctVal×ctMult = %v is not a positive contract size", source, e.InstID, contractSizeCoin)
	}
	lotSzContracts, err := parseInstrumentFloat(source, e.InstID, "lotSz", e.LotSz)
	if err != nil {
		return Instrument{}, false, err
	}
	minSzContracts, err := parseInstrumentFloat(source, e.InstID, "minSz", e.MinSz)
	if err != nil {
		return Instrument{}, false, err
	}
	tickSize, err := parseInstrumentFloat(source, e.InstID, "tickSz", e.TickSz)
	if err != nil {
		return Instrument{}, false, err
	}
	lever, err := parseInstrumentFloat(source, e.InstID, "lever", e.Lever)
	if err != nil {
		return Instrument{}, false, err
	}
	return Instrument{
		Symbol:       s.Standard,
		NativeSymbol: e.InstID,
		Source:       source,
		MarketType:   "perp",
		Status:       normalizeInstrumentStatus(e.State, e.State == "live"),
		// For SWAP instruments OKX leaves baseCcy/quoteCcy EMPTY (they are
		// spot fields). The declared equivalents for a linear swap — the only
		// ctType accepted above — are ctValCcy (the currency the contract
		// value is denominated in, i.e. the base) and settleCcy (a linear
		// swap settles in its quote). Verified live 2026-09-03:
		// BTC-USDT-SWAP → baseCcy "", ctValCcy BTC, settleCcy USDT.
		// https://www.okx.com/docs-v5/en/#public-data-rest-api-get-instruments
		BaseAsset:        e.CtValCcy,
		QuoteAsset:       e.SettleCcy,
		TickSizeQuote:    tickSize,
		StepSizeCoin:     lotSzContracts * contractSizeCoin,
		MinQtyCoin:       minSzContracts * contractSizeCoin,
		IsContract:       true,
		ContractSizeCoin: contractSizeCoin,
		MaxLeverageX:     lever,
	}, true, nil
}
