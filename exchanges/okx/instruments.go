package okx

import (
	"futures-arbitrage-scanner/exchanges"

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

// okxCodeMeansNotListed is the ONE OKX body code that means "this instrument
// does not exist here": 51001, "Instrument ID ... doesn't exist"
// (https://www.okx.com/docs-v5/en/#error-code, measured 2026-09-03). That is
// "absent" per the InstrumentFetchFunc contract — treating it as an error
// would let one unsupported pair blank the whole source. Every OTHER non-zero
// code stays a loud failure: a "system busy" read as "not listed" silently
// blanks contract sizes or truncates the funding corpus. Shared by the
// instrument, depth and funding-history fetchers so the three cannot drift —
// the same reason bybitSaysSymbolNotListed is one function.
func okxCodeMeansNotListed(code string) bool {
	return code == "51001"
}

type okxInstrumentsResponse struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []struct {
		InstID    string `json:"instId"`
		State     string `json:"state"`
		TickSz    string `json:"tickSz"`
		LotSz     string `json:"lotSz"`
		MinSz     string `json:"minSz"`
		MaxLmtSz  string `json:"maxLmtSz"`
		MaxMktSz  string `json:"maxMktSz"`
		CtVal     string `json:"ctVal"`
		CtMult    string `json:"ctMult"`
		CtValCcy  string `json:"ctValCcy"`
		SettleCcy string `json:"settleCcy"`
		CtType    string `json:"ctType"`
		Lever     string `json:"lever"`
	} `json:"data"`
}

func FetchInstruments(ctx context.Context, source string, symbols []exchanges.Symbol) ([]exchanges.Instrument, error) {
	var out []exchanges.Instrument
	for _, s := range symbols {
		u := "https://www.okx.com/api/v5/public/instruments?instType=SWAP&instId=" + s.Venue
		var resp okxInstrumentsResponse
		if err := exchanges.FetchJSON(ctx, u, &resp); err != nil {
			// OKX answers HTTP 200 + code 51001 today, but every per-symbol
			// fetcher honours the 404 sentinel too: an unlisted market is
			// absent, never a reason to blank the whole source.
			if errors.Is(err, exchanges.ErrNotListed) {
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

func parseOKXInstrument(resp okxInstrumentsResponse, source string, s exchanges.Symbol) (exchanges.Instrument, bool, error) {
	// OKX wraps errors in HTTP 200 + code≠"0" + empty data — without this
	// check a "system busy" response reads as "market not listed" and blanks
	// the venue's 100×-sensitive contract sizes.
	if okxCodeMeansNotListed(resp.Code) {
		return exchanges.Instrument{}, false, nil
	}
	if resp.Code != "" && resp.Code != "0" {
		return exchanges.Instrument{}, false, fmt.Errorf("%s %s: venue error code %s: %s", source, s.Venue, resp.Code, resp.Msg)
	}
	if len(resp.Data) == 0 {
		return exchanges.Instrument{}, false, nil // venue does not list this market
	}
	e := resp.Data[0]
	if e.InstID != s.Venue {
		return exchanges.Instrument{}, false, fmt.Errorf("%s: asked for %s, response names %s", source, s.Venue, e.InstID)
	}
	if e.CtType != "linear" {
		// An inverse contract's ctVal is in QUOTE currency; converting it
		// with this code would produce coin quantities that are wrong
		// without looking wrong.
		return exchanges.Instrument{}, false, fmt.Errorf("%s %s: ctType %q is not linear — inverse contracts are not supported", source, e.InstID, e.CtType)
	}
	ctVal, err := exchanges.ParseFloatField(source, e.InstID, "ctVal", e.CtVal)
	if err != nil {
		return exchanges.Instrument{}, false, err
	}
	ctMult, err := exchanges.ParseFloatField(source, e.InstID, "ctMult", e.CtMult)
	if err != nil {
		return exchanges.Instrument{}, false, err
	}
	contractSizeCoin := ctVal * ctMult
	if contractSizeCoin <= 0 {
		return exchanges.Instrument{}, false, fmt.Errorf("%s %s: ctVal×ctMult = %v is not a positive contract size", source, e.InstID, contractSizeCoin)
	}
	lotSzContracts, err := exchanges.ParseFloatField(source, e.InstID, "lotSz", e.LotSz)
	if err != nil {
		return exchanges.Instrument{}, false, err
	}
	minSzContracts, err := exchanges.ParseFloatField(source, e.InstID, "minSz", e.MinSz)
	if err != nil {
		return exchanges.Instrument{}, false, err
	}
	tickSize, err := exchanges.ParseFloatField(source, e.InstID, "tickSz", e.TickSz)
	if err != nil {
		return exchanges.Instrument{}, false, err
	}
	lever, err := exchanges.ParseFloatField(source, e.InstID, "lever", e.Lever)
	if err != nil {
		return exchanges.Instrument{}, false, err
	}
	// OKX publishes TWO order-size ceilings, both in contracts: maxLmtSz for
	// limit orders and maxMktSz for market orders (same instruments endpoint,
	// measured 2026-09-04: BTC-USDT-SWAP maxLmtSz 100,000,000 · maxMktSz
	// 35,000 = 350 BTC). The cap kept is the SMALLER of the two: a size above
	// it cannot be done as one order of at least one type, and this strategy's
	// fee model already assumes taker fills — which the tighter maxMktSz
	// governs. Ignoring these fields left MaxQtyCoin 0 = "not stated", and
	// sizing waved a 400 BTC leg past a venue that refuses it at 350.
	maxLmtContracts, err := exchanges.ParseFloatField(source, e.InstID, "maxLmtSz", e.MaxLmtSz)
	if err != nil {
		return exchanges.Instrument{}, false, err
	}
	maxMktContracts, err := exchanges.ParseFloatField(source, e.InstID, "maxMktSz", e.MaxMktSz)
	if err != nil {
		return exchanges.Instrument{}, false, err
	}
	maxQtyContracts := exchanges.SmallerPositiveCap(maxLmtContracts, maxMktContracts)
	return exchanges.Instrument{
		Symbol:       s.Standard,
		NativeSymbol: e.InstID,
		Source:       source,
		MarketType:   "perp",
		Status:       exchanges.NormalizeStatus(e.State, e.State == "live"),
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
		MaxQtyCoin:       maxQtyContracts * contractSizeCoin,
		IsContract:       true,
		ContractSizeCoin: contractSizeCoin,
		MaxLeverageX:     lever,
	}, true, nil
}
