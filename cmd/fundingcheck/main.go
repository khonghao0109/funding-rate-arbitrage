// Command fundingcheck is the step 2.1 verification script: it reads the BTC
// funding rate from all 7 futures venues over public REST, prints the RAW
// fields next to the normalized values, and turns the 🟡 items of
// docs/DATA-REQUIREMENTS.md §3 into live-checked verdicts.
//
// It is a diagnostic, not an ingestion path: it deliberately does NOT share
// code or config with the scanner, so that a bug in the connectors cannot
// silently confirm itself. Native symbols are hardcoded here for the same
// reason. Every field read from a venue cites the document or the live
// measurement that justifies it.
//
// A verdict FAIL means "the venue changed something, or the market is in a
// regime the check cannot discriminate — look at it"; it does not
// automatically mean this code is wrong. Verdicts assert units and semantics,
// never today's configuration values, so a legitimate venue reconfiguration
// (a new interval, a different cap) keeps passing.
//
// Run: go run ./cmd/fundingcheck
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const secPerYear = 31_536_000

// ratePer8hFrac converts a per-interval rate to the common 8h comparison
// window. docs/CONVENTIONS.md §1.3: cross-venue comparisons happen per 8h.
func ratePer8hFrac(ratePerIntervalFrac float64, intervalSec int64) float64 {
	return ratePerIntervalFrac * 28800 / float64(intervalSec)
}

// aprFrac annualizes a per-interval rate. PLAN.md step 3.1:
// APR = RatePerInterval × (31 536 000 / IntervalSec).
func aprFrac(ratePerIntervalFrac float64, intervalSec int64) float64 {
	return ratePerIntervalFrac * secPerYear / float64(intervalSec)
}

// row is one venue's funding reading, raw next to normalized.
type row struct {
	Venue            string
	NativeSymbol     string
	RawRateField     string
	RawRate          string // exactly as the venue returned it
	IntervalAsQuoted string // the venue's own unit, verbatim
	IntervalSec      int64
	// RatePerIntervalFrac is the rate for exactly one settlement interval of
	// this venue, as a fraction (0.0001 = 0.01%).
	RatePerIntervalFrac float64
	// NextFundingAtMs is 0 when the endpoint publishes no discrete settlement
	// timestamp: the continuous model (Paradex) and Kraken's v3 REST ticker
	// (its WS ticker does publish one).
	NextFundingAtMs int64
	Model           string
	Note            string
}

type verdict struct {
	Name   string
	Pass   bool
	Detail string
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

func decodeResponse(resp *http.Response, into any) error {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: HTTP %d: %s", resp.Request.URL, resp.StatusCode, body)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

func getJSON(ctx context.Context, url string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	return decodeResponse(resp, into)
}

func postJSON(ctx context.Context, url string, payload string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	return decodeResponse(resp, into)
}

// parseFloatFrac and parseInt64 fail loudly: this script exists to catch bad
// fields, so a value that does not parse must surface as an error, never as a
// silent zero flowing into arithmetic.
func parseFloatFrac(venue, field, s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: field %s = %q does not parse as float", venue, field, s)
	}
	return f, nil
}

func parseInt64(venue, field, s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: field %s = %q does not parse as int", venue, field, s)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Binance USDⓈ-M futures.
// GET /fapi/v1/premiumIndex — lastFundingRate, nextFundingTime (ms), time.
// https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Mark-Price
// GET /fapi/v1/fundingInfo — per the doc, ONLY symbols whose cap/floor/
// interval was adjusted; absence means "default 8h", not "no funding".
// https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Get-Funding-Rate-Info
// Measured 2026-09-03: the list held 777 symbols covering EVERY trading
// perpetual (BTCUSDT included — its ±0.3% cap counts as adjusted), with
// intervals of 4h (443), 8h (331) and 1h (3). The doc promises no such
// coverage, so the default-then-override pattern stays, and the verdict below
// does NOT require BTCUSDT to be in the list — only that the interval the
// pattern resolves matches the observed settlement spacing.
// GET /fapi/v1/fundingRate — settled history for that spacing.
// https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Get-Funding-Rate-History
func fetchBinance(ctx context.Context) (row, []verdict, error) {
	var premium struct {
		Symbol          string `json:"symbol"`
		LastFundingRate string `json:"lastFundingRate"`
		NextFundingAtMs int64  `json:"nextFundingTime"`
	}
	if err := getJSON(ctx, "https://fapi.binance.com/fapi/v1/premiumIndex?symbol=BTCUSDT", &premium); err != nil {
		return row{}, nil, err
	}
	rateFrac, err := parseFloatFrac("binance", "lastFundingRate", premium.LastFundingRate)
	if err != nil {
		return row{}, nil, err
	}

	var fundingInfo []struct {
		Symbol               string `json:"symbol"`
		FundingIntervalHours int64  `json:"fundingIntervalHours"`
	}
	if err := getJSON(ctx, "https://fapi.binance.com/fapi/v1/fundingInfo", &fundingInfo); err != nil {
		return row{}, nil, err
	}
	intervalHours := int64(8) // default: symbols absent from fundingInfo settle every 8h
	inFundingInfo := false
	for _, fi := range fundingInfo {
		if fi.Symbol == premium.Symbol {
			intervalHours = fi.FundingIntervalHours
			inFundingInfo = true
			break
		}
	}

	var history []struct {
		FundingAtMs int64 `json:"fundingTime"`
	}
	if err := getJSON(ctx, "https://fapi.binance.com/fapi/v1/fundingRate?symbol=BTCUSDT&limit=2", &history); err != nil {
		return row{}, nil, err
	}
	if len(history) < 2 {
		return row{}, nil, fmt.Errorf("binance: need 2 settled funding entries, got %d", len(history))
	}
	observedSpacingMs := history[1].FundingAtMs - history[0].FundingAtMs

	r := row{
		Venue:               "binance",
		NativeSymbol:        "BTCUSDT",
		RawRateField:        "lastFundingRate",
		RawRate:             premium.LastFundingRate,
		IntervalAsQuoted:    fmt.Sprintf("%dh (fundingInfo; in list=%v of %d symbols)", intervalHours, inFundingInfo, len(fundingInfo)),
		IntervalSec:         intervalHours * 3600,
		RatePerIntervalFrac: rateFrac,
		NextFundingAtMs:     premium.NextFundingAtMs,
		Model:               "discrete",
	}
	// Settlement timestamps carry a little jitter; a minute of tolerance still
	// separates 1h/4h/8h unambiguously. Right after Binance changes a symbol's
	// interval this can FAIL transiently (history still shows the old spacing)
	// — that is a real "venue changed something", worth a look, not a bug here.
	deviationMs := observedSpacingMs - intervalHours*3_600_000
	v := []verdict{{
		Name: "Binance: default-then-override interval matches observed settlement spacing (trap ⑦)",
		Pass: deviationMs > -60_000 && deviationMs < 60_000,
		Detail: fmt.Sprintf("resolved %dh (in fundingInfo=%v, list=%d symbols), last two settlements %dms apart",
			intervalHours, inFundingInfo, len(fundingInfo), observedSpacingMs),
	}}
	return r, v, nil
}

// ---------------------------------------------------------------------------
// Bybit v5 linear.
// GET /v5/market/tickers — fundingRate, nextFundingTime (ms), and
// fundingIntervalHour ("Funding interval hour", whole hours only):
// https://bybit-exchange.github.io/docs/v5/market/tickers
// GET /v5/market/instruments-info — fundingInterval, "Funding interval (minute)":
// https://bybit-exchange.github.io/docs/v5/market/instrument
// Same venue, same concept, two endpoints, two units — the verdict checks the
// two quotes agree once both are normalized, which is exactly trap ③.
func fetchBybit(ctx context.Context) (row, []verdict, error) {
	var ticker struct {
		Result struct {
			List []struct {
				FundingRate         string `json:"fundingRate"`
				NextFundingAtMs     string `json:"nextFundingTime"`
				FundingIntervalHour string `json:"fundingIntervalHour"`
			} `json:"list"`
		} `json:"result"`
	}
	if err := getJSON(ctx, "https://api.bybit.com/v5/market/tickers?category=linear&symbol=BTCUSDT", &ticker); err != nil {
		return row{}, nil, err
	}
	if len(ticker.Result.List) == 0 {
		return row{}, nil, fmt.Errorf("bybit: empty ticker list")
	}
	t := ticker.Result.List[0]
	rateFrac, err := parseFloatFrac("bybit", "fundingRate", t.FundingRate)
	if err != nil {
		return row{}, nil, err
	}
	nextFundingAtMs, err := parseInt64("bybit", "nextFundingTime", t.NextFundingAtMs)
	if err != nil {
		return row{}, nil, err
	}

	var info struct {
		Result struct {
			List []struct {
				FundingIntervalMin int64 `json:"fundingInterval"`
			} `json:"list"`
		} `json:"result"`
	}
	if err := getJSON(ctx, "https://api.bybit.com/v5/market/instruments-info?category=linear&symbol=BTCUSDT", &info); err != nil {
		return row{}, nil, err
	}
	if len(info.Result.List) == 0 {
		return row{}, nil, fmt.Errorf("bybit: empty instruments-info list")
	}
	intervalMin := info.Result.List[0].FundingIntervalMin

	r := row{
		Venue:               "bybit",
		NativeSymbol:        "BTCUSDT",
		RawRateField:        "fundingRate",
		RawRate:             t.FundingRate,
		IntervalAsQuoted:    fmt.Sprintf("%d min (instruments-info) / %q h (ticker)", intervalMin, t.FundingIntervalHour),
		IntervalSec:         intervalMin * 60,
		RatePerIntervalFrac: rateFrac,
		NextFundingAtMs:     nextFundingAtMs,
		Model:               "discrete",
	}
	// fundingIntervalHour is a newer ticker field; if Bybit ships it empty the
	// unit cross-check is inconclusive, not failed — fall back to a sanity
	// check on the minutes quote alone and say so.
	unitCheck := verdict{Name: "Bybit: instruments-info minutes agree with ticker hours (trap ③)"}
	if t.FundingIntervalHour == "" {
		unitCheck.Pass = intervalMin > 0 && intervalMin%60 == 0
		unitCheck.Detail = fmt.Sprintf("ticker fundingIntervalHour absent; fundingInterval=%d min stands alone", intervalMin)
	} else {
		tickerHours, err := parseInt64("bybit", "fundingIntervalHour", t.FundingIntervalHour)
		if err != nil {
			return row{}, nil, err
		}
		unitCheck.Pass = intervalMin == tickerHours*60
		unitCheck.Detail = fmt.Sprintf("fundingInterval=%d min, fundingIntervalHour=%d — two units for one concept on one venue", intervalMin, tickerHours)
	}
	return r, []verdict{unitCheck}, nil
}

// ---------------------------------------------------------------------------
// OKX v5.
// GET /api/v5/public/funding-rate — fundingRate, fundingTime, nextFundingTime:
// https://www.okx.com/docs-v5/en/#public-data-rest-api-get-funding-rate
// Trap ①: fundingTime is the UPCOMING settlement, nextFundingTime the one
// after. The verdict asserts exactly that ordering — ts < fundingTime <
// nextFundingTime — and nothing about spacing regularity, which OKX may
// legitimately change. Verified live 2026-09-03. Under method
// "current_period" nextFundingRate arrives EMPTY (a real finding, §3.3).
func fetchOKX(ctx context.Context) (row, []verdict, error) {
	var fr struct {
		Data []struct {
			FundingRate     string `json:"fundingRate"`
			FundingAtMs     string `json:"fundingTime"`
			NextFundingRate string `json:"nextFundingRate"`
			NextFundingAtMs string `json:"nextFundingTime"`
			PrevFundingAtMs string `json:"prevFundingTime"`
			Method          string `json:"method"`
			MaxFundingRate  string `json:"maxFundingRate"`
			TsMs            string `json:"ts"`
		} `json:"data"`
	}
	if err := getJSON(ctx, "https://www.okx.com/api/v5/public/funding-rate?instId=BTC-USDT-SWAP", &fr); err != nil {
		return row{}, nil, err
	}
	if len(fr.Data) == 0 {
		return row{}, nil, fmt.Errorf("okx: empty data")
	}
	d := fr.Data[0]
	rateFrac, err := parseFloatFrac("okx", "fundingRate", d.FundingRate)
	if err != nil {
		return row{}, nil, err
	}
	nowMs, err := parseInt64("okx", "ts", d.TsMs)
	if err != nil {
		return row{}, nil, err
	}
	fundingAtMs, err := parseInt64("okx", "fundingTime", d.FundingAtMs)
	if err != nil {
		return row{}, nil, err
	}
	nextFundingAtMs, err := parseInt64("okx", "nextFundingTime", d.NextFundingAtMs)
	if err != nil {
		return row{}, nil, err
	}
	if nextFundingAtMs <= fundingAtMs {
		return row{}, nil, fmt.Errorf("okx: nextFundingTime %d not after fundingTime %d", nextFundingAtMs, fundingAtMs)
	}
	intervalSec := (nextFundingAtMs - fundingAtMs) / 1000

	// prevFundingTime is present in the live payload (measured 2026-09-03)
	// but detail-only: the trap-① claim needs no spacing regularity.
	spacingNote := ""
	if prevAtMs, err := parseInt64("okx", "prevFundingTime", d.PrevFundingAtMs); err == nil && prevAtMs > 0 {
		spacingNote = fmt.Sprintf(", prev→funding %ds", (fundingAtMs-prevAtMs)/1000)
	}

	r := row{
		Venue:               "okx",
		NativeSymbol:        "BTC-USDT-SWAP",
		RawRateField:        "fundingRate",
		RawRate:             d.FundingRate,
		IntervalAsQuoted:    fmt.Sprintf("%ds (derived: nextFundingTime−fundingTime)", intervalSec),
		IntervalSec:         intervalSec,
		RatePerIntervalFrac: rateFrac,
		NextFundingAtMs:     fundingAtMs, // fundingTime, NOT nextFundingTime — trap ①
		Model:               "discrete",
		Note:                fmt.Sprintf("method=%s, nextFundingRate=%q, cap=±%s", d.Method, d.NextFundingRate, d.MaxFundingRate),
	}
	v := []verdict{{
		Name: "OKX: fundingTime is the UPCOMING settlement (trap ①)",
		Pass: nowMs < fundingAtMs && fundingAtMs < nextFundingAtMs,
		Detail: fmt.Sprintf("ts=%d < fundingTime=%d < nextFundingTime=%d%s",
			nowMs, fundingAtMs, nextFundingAtMs, spacingNote),
	}}
	return r, v, nil
}

// ---------------------------------------------------------------------------
// Gate v4 futures.
// GET /api/v4/futures/usdt/contracts/BTC_USDT — funding_rate, funding_interval
// (seconds), funding_next_apply (epoch SECONDS):
// https://www.gate.com/docs/developers/apiv4/en/#get-a-single-contract
// Gate's own SDK types these as numbers that may carry a fractional part, so
// they decode as float64. The verdict asserts the UNIT — the interval is a
// whole number of hours expressed in seconds, and next_apply lands within one
// interval of now — never the specific value 28800, which Gate may change.
func fetchGate(ctx context.Context) (row, []verdict, error) {
	var c struct {
		FundingRate           string  `json:"funding_rate"`
		FundingRateIndicative string  `json:"funding_rate_indicative"`
		FundingIntervalSec    float64 `json:"funding_interval"`
		FundingNextApplySec   float64 `json:"funding_next_apply"`
	}
	if err := getJSON(ctx, "https://api.gateio.ws/api/v4/futures/usdt/contracts/BTC_USDT", &c); err != nil {
		return row{}, nil, err
	}
	rateFrac, err := parseFloatFrac("gate", "funding_rate", c.FundingRate)
	if err != nil {
		return row{}, nil, err
	}
	intervalSec := int64(c.FundingIntervalSec)
	nextApplySec := int64(c.FundingNextApplySec)
	if intervalSec <= 0 {
		return row{}, nil, fmt.Errorf("gate: funding_interval = %v is not a positive interval", c.FundingIntervalSec)
	}

	r := row{
		Venue:               "gate",
		NativeSymbol:        "BTC_USDT",
		RawRateField:        "funding_rate",
		RawRate:             c.FundingRate,
		IntervalAsQuoted:    fmt.Sprintf("%ds (funding_interval, already seconds)", intervalSec),
		IntervalSec:         intervalSec,
		RatePerIntervalFrac: rateFrac,
		NextFundingAtMs:     nextApplySec * 1000,
		Model:               "discrete",
		Note:                fmt.Sprintf("funding_rate_indicative=%s", c.FundingRateIndicative),
	}
	nowSec := time.Now().Unix()
	v := []verdict{{
		Name: "Gate: funding_interval is in SECONDS, funding_next_apply in epoch seconds (trap ③)",
		// Seconds-unit shape: hours would be ~8, minutes ~480; whole hours in
		// seconds are ≥3600 and divisible by 3600. Epoch-seconds shape: the
		// next settlement lies within one interval of now (60s of slack).
		Pass: intervalSec >= 3600 && intervalSec%3600 == 0 &&
			nextApplySec > nowSec-60 && nextApplySec <= nowSec+intervalSec+60,
		Detail: fmt.Sprintf("funding_interval=%d, funding_next_apply=%d, now=%d", intervalSec, nextApplySec, nowSec),
	}}
	return r, v, nil
}

// ---------------------------------------------------------------------------
// Kraken Futures.
// GET /derivatives/api/v3/tickers/PF_XBTUSD — fundingRate (ABSOLUTE, USD per
// contract unit per hour), fundingRatePrediction, indexPrice:
// https://docs.kraken.com/api/docs/futures-api/trading/get-tickers
// GET /derivatives/api/v4/historicalfundingrates — both fundingRate (absolute)
// and relativeFundingRate per settled hour (measured 2026-09-03: one year of
// hourly entries, ~1MB, ascending; no limit parameter is documented, so the
// download cost is accepted and the newest entry is found by timestamp, not
// by position):
// https://docs.kraken.com/api/docs/futures-api/trading/historical-funding-rates
// Contract spec: "The absolute rate is the amount of funding an account will
// receive by maintaining a 1 contract unit short position for 1 hour. The
// relative rate is the absolute funding rate relative to the spot price at the
// time of the funding rate calculation."
// https://support.kraken.com/articles/4844359082772-linear-multi-collateral-derivatives-contract-specifications
// ⚠️ The relative rate is a PER-1-HOUR rate, applied in full each hourly
// settlement — NOT an 8h-window quote to divide by 8. Verified 2026-09-03:
// ×8 lands in the cross-venue 8h cluster; ÷8 would sit 8× below every venue.
func fetchKraken(ctx context.Context) (row, []verdict, error) {
	var ticker struct {
		Ticker struct {
			FundingRate           float64 `json:"fundingRate"`
			FundingRatePrediction float64 `json:"fundingRatePrediction"`
			IndexPrice            float64 `json:"indexPrice"`
		} `json:"ticker"`
	}
	if err := getJSON(ctx, "https://futures.kraken.com/derivatives/api/v3/tickers/PF_XBTUSD", &ticker); err != nil {
		return row{}, nil, err
	}
	var hist struct {
		Rates []struct {
			Timestamp           string  `json:"timestamp"`
			FundingRate         float64 `json:"fundingRate"`
			RelativeFundingRate float64 `json:"relativeFundingRate"`
		} `json:"rates"`
	}
	if err := getJSON(ctx, "https://futures.kraken.com/derivatives/api/v4/historicalfundingrates?symbol=PF_XBTUSD", &hist); err != nil {
		return row{}, nil, err
	}
	if len(hist.Rates) < 2 {
		return row{}, nil, fmt.Errorf("kraken: need ≥2 settled entries, got %d", len(hist.Rates))
	}
	// RFC3339 timestamps sort lexicographically, so ordering needs no parsing;
	// newest-by-timestamp guards against the endpoint changing its sort order.
	newest, secondNewest := 0, -1
	for i := range hist.Rates {
		if hist.Rates[i].Timestamp > hist.Rates[newest].Timestamp {
			newest = i
		}
	}
	for i := range hist.Rates {
		if i != newest && (secondNewest < 0 || hist.Rates[i].Timestamp > hist.Rates[secondNewest].Timestamp) {
			secondNewest = i
		}
	}
	last := hist.Rates[newest]

	// The settlement cadence is measured from the history, not hardcoded
	// (CLAUDE.md rule 3): spacing of the two newest settled entries.
	newestAt, err := time.Parse(time.RFC3339, last.Timestamp)
	if err != nil {
		return row{}, nil, fmt.Errorf("kraken: timestamp %q does not parse: %w", last.Timestamp, err)
	}
	secondAt, err := time.Parse(time.RFC3339, hist.Rates[secondNewest].Timestamp)
	if err != nil {
		return row{}, nil, fmt.Errorf("kraken: timestamp %q does not parse: %w", hist.Rates[secondNewest].Timestamp, err)
	}
	intervalSec := int64(newestAt.Sub(secondAt) / time.Second)
	if intervalSec <= 0 {
		return row{}, nil, fmt.Errorf("kraken: non-positive settlement spacing %ds", intervalSec)
	}

	r := row{
		Venue:               "kraken",
		NativeSymbol:        "PF_XBTUSD",
		RawRateField:        "relativeFundingRate",
		RawRate:             strconv.FormatFloat(last.RelativeFundingRate, 'g', -1, 64),
		IntervalAsQuoted:    fmt.Sprintf("%ds (measured spacing of settled entries)", intervalSec),
		IntervalSec:         intervalSec,
		RatePerIntervalFrac: last.RelativeFundingRate,
		NextFundingAtMs:     0, // v3 REST ticker publishes no next-settlement timestamp; WS does
		Model:               "discrete",
		Note: fmt.Sprintf("quote=USD not USDT; absolute fundingRate=%.6f, prediction=%.6f",
			ticker.Ticker.FundingRate, ticker.Ticker.FundingRatePrediction),
	}

	// Trap ②: absolute = relative × spot at calculation time. Picking the
	// wrong field is a ~5-orders-of-magnitude error, so the ratio only has to
	// land within 2× of the index — wide enough that a volatile hour between
	// the settled entry and the live index cannot false-FAIL it. A settled
	// hour of exactly 0 cannot form the ratio; use the newest nonzero entry.
	checkIdx := -1
	for i := range hist.Rates {
		if hist.Rates[i].RelativeFundingRate == 0 {
			continue
		}
		if checkIdx < 0 || hist.Rates[i].Timestamp > hist.Rates[checkIdx].Timestamp {
			checkIdx = i
		}
	}
	v := verdict{Name: "Kraken: fundingRate is ABSOLUTE, relativeFundingRate is the rate (trap ②)"}
	if checkIdx < 0 {
		v.Pass = false
		v.Detail = "every settled entry has relativeFundingRate=0 — cannot form the ratio; inspect manually"
	} else {
		check := hist.Rates[checkIdx]
		impliedPrice := check.FundingRate / check.RelativeFundingRate
		ratio := impliedPrice / ticker.Ticker.IndexPrice
		v.Pass = ratio > 0.5 && ratio < 2
		v.Detail = fmt.Sprintf("absolute/relative=%.0f vs indexPrice=%.0f (ratio %.3f) at %s",
			impliedPrice, ticker.Ticker.IndexPrice, ratio, check.Timestamp)
	}
	return r, []verdict{v}, nil
}

// ---------------------------------------------------------------------------
// Hyperliquid.
// POST /info {"type":"metaAndAssetCtxs"} — asset ctx field "funding".
// POST /info {"type":"predictedFundings"} — per venue: fundingRate,
// nextFundingTime, and fundingIntervalHours, i.e. the venue DECLARES its own
// interval (measured 2026-09-03: HlPerp fundingIntervalHours=1).
// https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/info-endpoint/perpetuals
// Funding doc: "The funding rate formula applies to 8 hour funding rate.
// However, funding is paid every hour at one eighth of the computed rate" —
// so the API's "funding" field is the per-interval (hourly, already ÷8)
// value; the ÷8-vs-not distinction is guarded by the cross-venue magnitude
// check, which flags any ≥5× outlier.
// https://hyperliquid.gitbook.io/hyperliquid-docs/trading/funding
func fetchHyperliquid(ctx context.Context) (row, []verdict, error) {
	var resp []json.RawMessage
	if err := postJSON(ctx, "https://api.hyperliquid.xyz/info", `{"type":"metaAndAssetCtxs"}`, &resp); err != nil {
		return row{}, nil, err
	}
	if len(resp) != 2 {
		return row{}, nil, fmt.Errorf("hyperliquid: expected [meta, ctxs], got %d elements", len(resp))
	}
	var meta struct {
		Universe []struct {
			Name string `json:"name"`
		} `json:"universe"`
	}
	if err := json.Unmarshal(resp[0], &meta); err != nil {
		return row{}, nil, err
	}
	var ctxs []struct {
		Funding string `json:"funding"`
	}
	if err := json.Unmarshal(resp[1], &ctxs); err != nil {
		return row{}, nil, err
	}
	idx := -1
	for i, u := range meta.Universe {
		if u.Name == "BTC" {
			idx = i
			break
		}
	}
	if idx < 0 || idx >= len(ctxs) {
		return row{}, nil, fmt.Errorf("hyperliquid: BTC not found in universe")
	}
	fundingPerIntervalFrac, err := parseFloatFrac("hyperliquid", "funding", ctxs[idx].Funding)
	if err != nil {
		return row{}, nil, err
	}

	// predictedFundings: [[coin, [[venueName, {fundingRate, nextFundingTime,
	// fundingIntervalHours}], ...]], ...] — the HlPerp entry is Hyperliquid
	// describing itself.
	var predicted []json.RawMessage
	if err := postJSON(ctx, "https://api.hyperliquid.xyz/info", `{"type":"predictedFundings"}`, &predicted); err != nil {
		return row{}, nil, err
	}
	intervalHours := int64(0)
	nextFundingAtMs := int64(0)
	for _, entry := range predicted {
		var pair []json.RawMessage
		if err := json.Unmarshal(entry, &pair); err != nil || len(pair) != 2 {
			continue
		}
		var coin string
		if err := json.Unmarshal(pair[0], &coin); err != nil || coin != "BTC" {
			continue
		}
		var venues []json.RawMessage
		if err := json.Unmarshal(pair[1], &venues); err != nil {
			continue
		}
		for _, ve := range venues {
			var vp []json.RawMessage
			if err := json.Unmarshal(ve, &vp); err != nil || len(vp) != 2 {
				continue
			}
			var venueName string
			if err := json.Unmarshal(vp[0], &venueName); err != nil || venueName != "HlPerp" {
				continue
			}
			var pf struct {
				FundingIntervalHours int64 `json:"fundingIntervalHours"`
				NextFundingAtMs      int64 `json:"nextFundingTime"`
			}
			if err := json.Unmarshal(vp[1], &pf); err == nil {
				intervalHours = pf.FundingIntervalHours
				nextFundingAtMs = pf.NextFundingAtMs
			}
		}
	}
	if intervalHours <= 0 {
		return row{}, nil, fmt.Errorf("hyperliquid: predictedFundings carries no HlPerp fundingIntervalHours for BTC")
	}

	r := row{
		Venue:               "hyperliquid",
		NativeSymbol:        "BTC",
		RawRateField:        "funding",
		RawRate:             ctxs[idx].Funding,
		IntervalAsQuoted:    fmt.Sprintf("%dh (self-declared via predictedFundings; docs: 8h-formula rate paid hourly at 1/8)", intervalHours),
		IntervalSec:         intervalHours * 3600,
		RatePerIntervalFrac: fundingPerIntervalFrac,
		NextFundingAtMs:     nextFundingAtMs,
		Model:               "discrete",
	}
	v := []verdict{{
		Name:   "Hyperliquid: venue self-declares its funding interval (trap ④)",
		Pass:   intervalHours > 0 && nextFundingAtMs > 0,
		Detail: fmt.Sprintf("predictedFundings HlPerp: fundingIntervalHours=%d, nextFundingTime=%d", intervalHours, nextFundingAtMs),
	}}
	return r, v, nil
}

// ---------------------------------------------------------------------------
// Paradex.
// GET /v1/funding/data — funding_index (continuous accrual), funding_rate,
// funding_period_hours, created_at. Measured 2026-09-03: points every ~5s,
// funding_index monotone, funding_period_hours=8 — Funding V2 accrues
// continuously and quotes its rate for a self-declared window (trap ④/🟡).
// https://docs.paradex.trade/api-reference/prod/funding/list-funding-data
// The verdict asserts the continuous MODEL — points arrive far more often
// than once per quoted period — not today's cadence or window values.
func fetchParadex(ctx context.Context) (row, []verdict, error) {
	var fd struct {
		Results []struct {
			FundingIndex       string `json:"funding_index"`
			FundingRate        string `json:"funding_rate"`
			FundingPeriodHours int64  `json:"funding_period_hours"`
			CreatedAtMs        int64  `json:"created_at"`
		} `json:"results"`
	}
	if err := getJSON(ctx, "https://api.prod.paradex.trade/v1/funding/data?market=BTC-USD-PERP&page_size=8", &fd); err != nil {
		return row{}, nil, err
	}
	if len(fd.Results) < 2 {
		return row{}, nil, fmt.Errorf("paradex: need ≥2 funding data points, got %d", len(fd.Results))
	}
	// Order-agnostic: find the newest point and the largest gap between
	// adjacent timestamps after sorting, so a change in the endpoint's sort
	// order cannot flip the verdict or pick a stale rate.
	atMs := make([]int64, len(fd.Results))
	newest := 0
	for i, p := range fd.Results {
		atMs[i] = p.CreatedAtMs
		if p.CreatedAtMs > fd.Results[newest].CreatedAtMs {
			newest = i
		}
	}
	for i := 1; i < len(atMs); i++ { // insertion sort; n ≤ 8
		for j := i; j > 0 && atMs[j] < atMs[j-1]; j-- {
			atMs[j], atMs[j-1] = atMs[j-1], atMs[j]
		}
	}
	maxGapMs := int64(0)
	for i := 0; i+1 < len(atMs); i++ {
		if gap := atMs[i+1] - atMs[i]; gap > maxGapMs {
			maxGapMs = gap
		}
	}
	p := fd.Results[newest]
	rateFrac, err := parseFloatFrac("paradex", "funding_rate", p.FundingRate)
	if err != nil {
		return row{}, nil, err
	}
	if p.FundingPeriodHours <= 0 {
		return row{}, nil, fmt.Errorf("paradex: funding_period_hours = %d is not a positive window", p.FundingPeriodHours)
	}
	intervalSec := p.FundingPeriodHours * 3600

	r := row{
		Venue:               "paradex",
		NativeSymbol:        "BTC-USD-PERP",
		RawRateField:        "funding_rate",
		RawRate:             p.FundingRate,
		IntervalAsQuoted:    fmt.Sprintf("continuous; rate quoted per funding_period_hours=%dh", p.FundingPeriodHours),
		IntervalSec:         intervalSec,
		RatePerIntervalFrac: rateFrac,
		NextFundingAtMs:     0, // no settlement timestamp exists
		Model:               "continuous",
		Note:                fmt.Sprintf("quote=USD; funding_index=%s", p.FundingIndex),
	}
	periodMs := intervalSec * 1000
	v := []verdict{{
		Name: "Paradex: funding accrues continuously via funding_index (🟡 Funding V2)",
		// Continuous ⇔ index points arrive far more often than once per
		// quoted period (here: at least 100× as often).
		Pass:   maxGapMs > 0 && maxGapMs < periodMs/100,
		Detail: fmt.Sprintf("largest gap %dms across %d points, quoted period %dh", maxGapMs, len(fd.Results), p.FundingPeriodHours),
	}}
	return r, v, nil
}

// ---------------------------------------------------------------------------

// coherenceVerdict checks that every venue's per-8h magnitude sits within
// coherenceFactor× of the cross-venue median. Wrong-field and wrong-unit bugs
// are multiplicative — ÷8 or ×8 (interval), 60× (minutes as seconds), ~8×10³
// (Kraken absolute as relative) — so they throw a venue far outside any
// legitimate dispersion. Signs are ignored: negative and mixed-sign funding
// are routine market states, not unit bugs. When the whole cluster sits near
// zero the ratio cannot discriminate anything, so the check abstains (Pass
// with a note) rather than fail healthy data.
const (
	coherenceFactor        = 5.0  // legitimate cross-venue dispersion stays well under this
	coherenceFloorPer8hAbs = 1e-6 // |median| below this ⇒ regime too flat to judge
)

func coherenceVerdict(venues []string, per8hFracs []float64) verdict {
	v := verdict{Name: fmt.Sprintf("Cross-venue magnitude of rate/8h within %.0f× of the median", coherenceFactor)}
	if len(venues) < 3 {
		v.Pass = true
		v.Detail = fmt.Sprintf("only %d venue(s) fetched — too few to form a cluster", len(venues))
		return v
	}
	abs := make([]float64, 0, len(per8hFracs))
	for i, x := range per8hFracs {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			v.Pass = false
			v.Detail = fmt.Sprintf("%s normalized to %v — a parse or interval bug upstream", venues[i], x)
			return v
		}
		abs = append(abs, math.Abs(x))
	}
	sorted := append([]float64(nil), abs...)
	for i := 1; i < len(sorted); i++ { // insertion sort; n ≤ 7
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	median := sorted[len(sorted)/2]
	if median < coherenceFloorPer8hAbs {
		v.Pass = true
		v.Detail = fmt.Sprintf("median |rate/8h| = %.2g%% — too near zero to discriminate units; nothing checked", median*100)
		return v
	}
	for i, a := range abs {
		if a < coherenceFloorPer8hAbs {
			continue // one venue legitimately near zero says nothing about its units
		}
		if ratio := a / median; ratio > coherenceFactor || ratio < 1/coherenceFactor {
			v.Pass = false
			v.Detail = fmt.Sprintf("%s at %.6f%%/8h is %.1f× the median %.6f%%/8h — smells like ÷8/×8, 60×, or absolute-vs-relative",
				venues[i], per8hFracs[i]*100, ratio, median*100)
			return v
		}
	}
	v.Pass = true
	v.Detail = fmt.Sprintf("median %.6f%%/8h across %d venues, all magnitudes within %.0f×", median*100, len(venues), coherenceFactor)
	return v
}

func main() {
	fetchers := []func(context.Context) (row, []verdict, error){
		fetchBinance, fetchBybit, fetchOKX, fetchGate, fetchKraken, fetchHyperliquid, fetchParadex,
	}

	// Venues are sampled concurrently so the coherence check compares rates
	// from (nearly) the same moment; funding drifts within a window, and
	// minutes of skew would be attributed to the venues instead of the clock.
	ctx := context.Background()
	rows := make([]*row, len(fetchers))
	verdictsByVenue := make([][]verdict, len(fetchers))
	errs := make([]error, len(fetchers))
	var wg sync.WaitGroup
	for i, fetch := range fetchers {
		wg.Add(1)
		go func(i int, fetch func(context.Context) (row, []verdict, error)) {
			defer wg.Done()
			r, v, err := fetch(ctx)
			if err != nil {
				errs[i] = err
				return
			}
			rows[i] = &r
			verdictsByVenue[i] = v
		}(i, fetch)
	}
	wg.Wait()

	var verdicts []verdict
	var venues []string
	var per8hFracs []float64
	failed := 0
	for i := range fetchers {
		if errs[i] != nil {
			fmt.Fprintf(os.Stderr, "FETCH FAILED: %v\n", errs[i])
			failed++
			continue
		}
		verdicts = append(verdicts, verdictsByVenue[i]...)
		venues = append(venues, rows[i].Venue)
		per8hFracs = append(per8hFracs, ratePer8hFrac(rows[i].RatePerIntervalFrac, rows[i].IntervalSec))
	}

	now := time.Now().UTC()
	fmt.Printf("BTC funding across venues — %s\n\n", now.Format(time.RFC3339))
	fmt.Printf("%-12s %-14s %-22s %-15s %9s %14s %14s %11s %-6s %s\n",
		"venue", "native symbol", "raw field", "raw value", "int(sec)", "rate/interval", "rate/8h", "APR", "model", "next funding (UTC)")
	for _, r := range rows {
		if r == nil {
			continue
		}
		next := "—"
		if r.NextFundingAtMs > 0 {
			next = time.UnixMilli(r.NextFundingAtMs).UTC().Format("15:04:05")
		}
		fmt.Printf("%-12s %-14s %-22s %-15s %9d %13.6f%% %13.6f%% %10.2f%% %-6s %s\n",
			r.Venue, r.NativeSymbol, r.RawRateField, r.RawRate, r.IntervalSec,
			r.RatePerIntervalFrac*100,
			ratePer8hFrac(r.RatePerIntervalFrac, r.IntervalSec)*100,
			aprFrac(r.RatePerIntervalFrac, r.IntervalSec)*100,
			r.Model, next)
		if r.IntervalAsQuoted != "" {
			fmt.Printf("%-12s   interval as quoted: %s\n", "", r.IntervalAsQuoted)
		}
		if r.Note != "" {
			fmt.Printf("%-12s   note: %s\n", "", r.Note)
		}
	}

	verdicts = append(verdicts, coherenceVerdict(venues, per8hFracs))

	fmt.Println("\nVerification verdicts (DATA-REQUIREMENTS §3 appendix):")
	allPass := failed == 0
	for _, v := range verdicts {
		mark := "PASS"
		if !v.Pass {
			mark = "FAIL"
			allPass = false
		}
		fmt.Printf("  [%s] %s\n         %s\n", mark, v.Name, v.Detail)
	}
	if failed > 0 {
		fmt.Printf("\n%d venue(s) failed to fetch.\n", failed)
	}
	if !allPass {
		os.Exit(1)
	}
}
