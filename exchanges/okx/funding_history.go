package okx

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"errors"
	"fmt"
	"strconv"
)

// OKX settled funding rates (step 2.6).
//
//	GET /api/v5/public/funding-rate-history?instId=&limit=100&after=<fundingTime ms>
//	{"code":"0","data":[{"fundingRate":"0.0000418741079605","fundingTime":"1788480000000",
//	  "realizedRate":"0.0000418741079605","method":"current_period",
//	  "formulaType":"withRate","instId":"BTC-USDT-SWAP","instType":"SWAP"}]}
//
// ⚠️ RETENTION, measured 2026-09-04 by bisection on BTC-USDT-SWAP: `after` 60
// and 90 days back return rows; 120, 150, 180 and 270 days back all return an
// EMPTY data array with code "0". OKX keeps roughly three months, so a 12-month
// backfill request comes back three months deep and that is the venue's answer,
// not a failure. The caller reports coverage from the rows.
//
// `after` means "records earlier than this fundingTime" — the opposite reading
// of the word from what a forward cursor would suggest, and getting it backwards
// re-requests the newest page forever.
// https://www.okx.com/docs-v5/en/#public-data-rest-api-get-funding-rate-history
const okxFundingHistoryLimit = 100

type okxFundingHistoryResponse struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []struct {
		InstID      string `json:"instId"`
		FundingRate string `json:"fundingRate"`
		// RealizedRate is what was actually charged. It equals fundingRate for
		// method "current_period" (the only method seen live, 2026-09-04) but
		// is the authoritative one of the pair, so it is preferred and the
		// choice is recorded in RawRateField rather than assumed.
		RealizedRate string `json:"realizedRate"`
		FundingTime  string `json:"fundingTime"`
		Method       string `json:"method"`
	} `json:"data"`
}

// FetchFundingHistory walks backwards from the end of the window.
func FetchFundingHistory(ctx context.Context, source string, symbol exchanges.Symbol, window exchanges.FundingWindow) ([]exchanges.FundingHistoryEntry, error) {
	var rows []exchanges.FundingHistoryRow
	afterMs := window.EndMs

	for page := 0; page < exchanges.MaxFundingHistoryPages; page++ {
		url := fmt.Sprintf("https://www.okx.com/api/v5/public/funding-rate-history?instId=%s&limit=%d&after=%d",
			symbol.Venue, okxFundingHistoryLimit, afterMs)
		var resp okxFundingHistoryResponse
		if err := exchanges.FetchFundingHistoryPage(ctx, exchanges.FundingHistoryPageDelay, func() error {
			resp = okxFundingHistoryResponse{}
			return exchanges.FetchJSON(ctx, url, &resp)
		}); err != nil {
			// A market this venue does not list is ABSENT, not an error:
			// step 2.4 found live that one unsupported pair (XLMUSDT on
			// Paradex) otherwise blanks a whole source. Here it would fill
			// the log with an hourly failure for a pair that will never
			// exist.
			if errors.Is(err, exchanges.ErrNotListed) {
				break
			}
			return nil, err
		}
		parsed, oldestMs, err := parseOKXFundingHistory(resp, symbol, window)
		if err != nil {
			return nil, err
		}
		rows = append(rows, parsed...)
		// An empty page is how OKX says "that is all the history there is" —
		// see the retention note above.
		if len(resp.Data) == 0 || oldestMs <= window.StartMs || oldestMs >= afterMs {
			break
		}
		afterMs = oldestMs
		if err := exchanges.FundingHistoryPause(ctx, exchanges.FundingHistoryPageDelay); err != nil {
			return nil, err
		}
	}
	return exchanges.FinishFundingHistory(source, symbol, exchanges.FundingDiscrete, rows)
}

func parseOKXFundingHistory(resp okxFundingHistoryResponse, symbol exchanges.Symbol, window exchanges.FundingWindow) ([]exchanges.FundingHistoryRow, int64, error) {
	// OKX wraps its errors in HTTP 200, so the body's code is the only signal.
	// The shared recognizer owns which code means "not listed" (absent, not a
	// failure). Every other non-zero code stays loud — reading a "system busy"
	// as an empty history would silently truncate the corpus.
	if okxCodeMeansNotListed(resp.Code) {
		return nil, 0, nil
	}
	if resp.Code != "0" {
		return nil, 0, fmt.Errorf("okx funding history %s: code %s %s", symbol.Venue, resp.Code, resp.Msg)
	}

	out := make([]exchanges.FundingHistoryRow, 0, len(resp.Data))
	var oldestMs int64
	for _, entry := range resp.Data {
		stampMs, err := strconv.ParseInt(entry.FundingTime, 10, 64)
		if err != nil {
			return nil, 0, fmt.Errorf("okx funding history %s: fundingTime %q does not parse", entry.InstID, entry.FundingTime)
		}
		if oldestMs == 0 || stampMs < oldestMs {
			oldestMs = stampMs
		}
		if entry.InstID != symbol.Venue || !window.Contains(stampMs) {
			continue
		}
		field, value := "realizedRate", entry.RealizedRate
		rateFrac, err := strconv.ParseFloat(value, 64)
		if err != nil {
			// Never parse "" as 0: an absent realizedRate means the venue did
			// not state one, and fundingRate is then the only figure it has.
			field, value = "fundingRate", entry.FundingRate
			rateFrac, err = strconv.ParseFloat(value, 64)
			if err != nil {
				return nil, 0, fmt.Errorf("okx funding history %s: neither realizedRate nor fundingRate parses (%q, %q)",
					entry.InstID, entry.RealizedRate, entry.FundingRate)
			}
		}
		out = append(out, exchanges.FundingHistoryRow{
			SettledAtMs:  stampMs,
			RateFrac:     rateFrac,
			RawRate:      rateFrac,
			RawRateField: field,
		})
	}
	return out, oldestMs, nil
}
