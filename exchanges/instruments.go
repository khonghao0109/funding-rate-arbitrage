package exchanges

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Instrument metadata: the trading rules each venue publishes for a market
// (step 2.3). These are the numbers the rest of the system must never guess
// at — a wrong step size makes a "delta-neutral" position not neutral from
// the first order, and a wrong contract size is a 100× error that does not
// look wrong.
//
// Quantities are normalized to BASE COIN here, exactly as OrderbookData does:
// OKX, Gate and Kraken denominate orders in CONTRACTS, so their step/min/max
// arrive multiplied by the measured contract size, and IsContract plus
// ContractSizeCoin let phase 4 convert an order back into the venue's native
// contract count. The per-venue conversions live in <venue>_instruments.go —
// one file per venue, next to that venue's other traps.
//
// The 2026-09-03 verification settled the step-1.2 Kraken contradiction:
// PF_ perpetuals ARE contract-denominated, but contractSize is 1 base unit
// (PF_XBTUSD: contractSize=1, contractValueTradePrecision=4 → orders in
// 0.0001-contract steps of 1 BTC each), so quantities are numerically equal
// to coin — which is why the step-1.2 measurement saw a coin-like book
// quantity. Both the survey row and the measurement were right.

// Instrument is one market's trading rules, normalized.
type Instrument struct {
	Symbol       string // normalized: BTCUSDT
	NativeSymbol string // the venue's own identifier: BTC-USDT-SWAP, PF_XBTUSD
	Source       string // wire id: binance_futures, ...
	MarketType   string // "spot" | "perp"

	// BaseAsset and QuoteAsset are the assets the VENUE declares for this
	// market, VERBATIM (step 2.4). Spot↔perp pairing is validated against
	// them — never against symbol spelling, which is how "DOGEUSDT" once
	// became base "DOG". Empty means the venue declares none; such an
	// instrument cannot be validated and the mapping refuses it rather than
	// guessing. Kraken declares base "BTC" for PF_XBTUSD itself, so no
	// XBT-alias table exists anywhere in this codebase — keep it that way.
	//
	// Do NOT normalize the case here. Hyperliquid lists seven mixed-case
	// markets whose prefix is meaningful (kPEPE is 1000 PEPE, kSHIB, kBONK,
	// kLUNC, kFLOKI, kDOGS, kNEIRO — seen in testdata), and upper-casing
	// them invents an asset name the venue never published. Case-insensitive
	// COMPARISON is the mapping's job (internal/instruments/mapping.go); the
	// declaration itself stays as the venue wrote it.
	BaseAsset  string
	QuoteAsset string

	// Status is "trading" when the venue reports the market tradable
	// (normalized across TRADING/Trading/live/true), otherwise the venue's
	// own word lower-cased — sizing refuses anything but "trading" and the
	// raw word lands in the refusal message.
	Status string

	// TickSizeQuote is the price increment in quote units. 0 means the venue
	// defines price granularity by RULE, not by constant (Hyperliquid: at
	// most 5 significant figures), and order pricing must apply that rule.
	TickSizeQuote float64

	// Order-size rules in BASE COIN (converted from contracts where needed).
	// MaxQtyCoin and MinNotionalQuote are 0 when the venue publishes none —
	// 0 means "not stated", never "no limit is enforced".
	StepSizeCoin     float64
	MinQtyCoin       float64
	MaxQtyCoin       float64
	MinNotionalQuote float64

	// IsContract: the venue's native order unit is CONTRACTS;
	// ContractSizeCoin is how many base coins one contract represents
	// (1 when the native unit is already coin).
	IsContract       bool
	ContractSizeCoin float64

	// MaxLeverageX is 0 when the venue does not publish it on a public
	// endpoint (Binance puts it behind the signed leverageBracket call —
	// phase 4).
	MaxLeverageX float64
}

// StatusTrading is the normalized value of Status for a tradable market.
const StatusTrading = "trading"

// InstrumentFetchFunc fetches the instrument rules this source publishes for
// the given symbols, over public REST. Symbols the venue does not list are
// simply absent from the result — the caller decides whether that is an
// error.
type InstrumentFetchFunc func(ctx context.Context, source string, symbols []Symbol) ([]Instrument, error)

// InstrumentFetchers maps connector names (the same names Connectors uses) to
// their instrument fetcher. Pyth has no entry: an oracle has no instruments.
func InstrumentFetchers() map[string]InstrumentFetchFunc {
	return map[string]InstrumentFetchFunc{
		"binance_futures":     FetchBinanceFuturesInstruments,
		"binance_spot":        FetchBinanceSpotInstruments,
		"bybit_futures":       FetchBybitFuturesInstruments,
		"bybit_spot":          FetchBybitSpotInstruments,
		"okx_futures":         FetchOKXInstruments,
		"gate_futures":        FetchGateInstruments,
		"kraken_futures":      FetchKrakenInstruments,
		"hyperliquid_futures": FetchHyperliquidInstruments,
		"paradex_futures":     FetchParadexInstruments,
	}
}

// instrumentHTTPClient is shared by the instrument fetchers. Instrument
// refreshes run once a day off the hot path, so a generous timeout beats a
// spurious failure.
var instrumentHTTPClient = &http.Client{Timeout: 30 * time.Second}

// errInstrumentNotListed marks a per-symbol request the venue answered with
// 404: the market does not exist there. Fetchers that query one symbol per
// request treat it as "absent", per the InstrumentFetchFunc contract — one
// delisted pair must not fail a whole source.
var errInstrumentNotListed = errors.New("instrument not listed")

func fetchInstrumentJSON(ctx context.Context, url string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	return doInstrumentRequest(req, into)
}

// postInstrumentJSON is the POST sibling — Hyperliquid's info endpoint is the
// one venue that takes its query in a request body.
func postInstrumentJSON(ctx context.Context, url, payload string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return doInstrumentRequest(req, into)
}

func doInstrumentRequest(req *http.Request, into any) error {
	resp, err := instrumentHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s: %w", req.URL, errInstrumentNotListed)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: HTTP %d: %s", req.URL, resp.StatusCode, body)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// normalizeInstrumentStatus maps a venue's own tradable marker onto
// StatusTrading, and keeps the venue's word (lower-cased) for everything else
// so a refusal can show it.
func normalizeInstrumentStatus(venueWord string, tradable bool) string {
	if tradable {
		return StatusTrading
	}
	// A venue word that lower-cases to "trading" while the predicate says
	// NOT tradable must never collide with the sentinel — Gate's
	// status="trading" + in_delisting=true is exactly that shape.
	if word := strings.ToLower(venueWord); word != StatusTrading {
		return word
	}
	return "not-trading"
}

func parseInstrumentFloat(source, nativeSymbol, field, value string) (float64, error) {
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %s: field %s = %q does not parse as float", source, nativeSymbol, field, value)
	}
	return f, nil
}
