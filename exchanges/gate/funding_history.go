package gate

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Gate settled funding rates (step 2.6).
//
//	GET /api/v4/futures/usdt/funding_rate?contract=&limit=1000&from=&to=
//	[{"r":"0.000052","t":1788480003}]
//
// ⚠️ Two things about this endpoint are not what they look like.
//
// `t` is in SECONDS while every other venue here stamps milliseconds, and it is
// not on the hour: the settlement above landed three seconds past 00:00:00.
// Those three seconds are the venue's own record of the event and are kept
// verbatim — rounding them to a boundary would invent a timestamp the venue
// never published, and a re-fetch returns the same value, which is what makes
// it usable as a primary key.
//
// `from` is refused beyond 180 days: {"label":"INVALID_PARAM_VALUE","message":
// "from time exceeds 180-day limit"} (measured 2026-09-04). A 12-month request
// is therefore clamped rather than sent and rejected — the venue's retention is
// a fact about the corpus, not an error. Without from/to the endpoint ignores
// limit and answers with about 30 days, so the window parameters are required
// even for a short fetch.
// https://www.gate.com/docs/developers/apiv4/en/#funding-rate-history
const (
	gateFundingHistoryLimit = 1000
	// gateFundingHistoryMaxAgeSec stops one day short of the documented 180 so
	// the request cannot be rejected for aging past the limit in flight.
	gateFundingHistoryMaxAgeSec = 179 * 24 * exchanges.SecPerHour
	// gateFundingSettle is the settlement currency in the path. Hardcoded the
	// same way the instrument fetcher hardcodes it: every Gate source in this
	// scanner is a USDT-margined perpetual, and deriving it by slicing the
	// contract name is the class of bug docs/CLAUDE.md bans.
	gateFundingSettle = "usdt"
)

type gateFundingHistoryRow struct {
	Rate string `json:"r"`
	Time int64  `json:"t"` // SECONDS
}

// FetchFundingHistory walks backwards from the end of the window, with the
// start clamped to the venue's 180-day retention.
func FetchFundingHistory(ctx context.Context, source string, symbol exchanges.Symbol, window exchanges.FundingWindow) ([]exchanges.FundingHistoryEntry, error) {
	startSec := window.StartMs / exchanges.MsPerSecond
	if oldest := time.Now().Unix() - gateFundingHistoryMaxAgeSec; startSec < oldest {
		startSec = oldest
	}
	toSec := window.EndMs / exchanges.MsPerSecond

	var rows []exchanges.FundingHistoryRow
	for page := 0; page < exchanges.MaxFundingHistoryPages; page++ {
		if toSec <= startSec {
			break
		}
		url := fmt.Sprintf("https://api.gateio.ws/api/v4/futures/%s/funding_rate?contract=%s&limit=%d&from=%d&to=%d",
			gateFundingSettle, symbol.Venue, gateFundingHistoryLimit, startSec, toSec)
		var raw []gateFundingHistoryRow
		if err := exchanges.FetchFundingHistoryPage(ctx, exchanges.FundingHistoryPageDelay, func() error {
			raw = nil
			return exchanges.FetchJSON(ctx, url, &raw)
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
		parsed, oldestSec, err := parseGateFundingHistory(raw, symbol, window)
		if err != nil {
			return nil, err
		}
		rows = append(rows, parsed...)
		if len(raw) < gateFundingHistoryLimit || oldestSec <= startSec || oldestSec-1 >= toSec {
			break
		}
		toSec = oldestSec - 1
		if err := exchanges.FundingHistoryPause(ctx, exchanges.FundingHistoryPageDelay); err != nil {
			return nil, err
		}
	}
	return exchanges.FinishFundingHistory(source, symbol, exchanges.FundingDiscrete, rows)
}

// parseGateFundingHistory also reports the oldest stamp seen, in seconds.
func parseGateFundingHistory(raw []gateFundingHistoryRow, symbol exchanges.Symbol, window exchanges.FundingWindow) ([]exchanges.FundingHistoryRow, int64, error) {
	out := make([]exchanges.FundingHistoryRow, 0, len(raw))
	var oldestSec int64
	for _, entry := range raw {
		if oldestSec == 0 || entry.Time < oldestSec {
			oldestSec = entry.Time
		}
		stampMs := entry.Time * exchanges.MsPerSecond
		if !window.Contains(stampMs) {
			continue
		}
		rateFrac, err := strconv.ParseFloat(entry.Rate, 64)
		if err != nil {
			return nil, 0, fmt.Errorf("gate funding history %s: r %q does not parse", symbol.Venue, entry.Rate)
		}
		out = append(out, exchanges.FundingHistoryRow{
			SettledAtMs:  stampMs,
			RateFrac:     rateFrac,
			RawRate:      rateFrac,
			RawRateField: "r",
		})
	}
	return out, oldestSec, nil
}
