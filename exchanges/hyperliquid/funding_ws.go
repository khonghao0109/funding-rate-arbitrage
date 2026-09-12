package hyperliquid

import (
	"futures-arbitrage-scanner/exchanges"

	"context"
	"fmt"
	"strconv"
	"time"
)

// Hyperliquid funding (step 2.5): the rate arrives over WebSocket, the cadence
// and the settlement stamp over REST.
//
// activeAssetCtx, measured 2026-09-04:
//
//	{"channel":"activeAssetCtx","data":{"coin":"BTC","ctx":{
//	  "funding":"0.0000105002","openInterest":"35986.71058",
//	  "premium":"-0.0001818346","oraclePx":"80842.7","markPx":"80808.0",...}}}
//
// That payload carries neither an interval nor a next-settlement stamp — and
// no venue clock either. predictedFundings carries both, per venue, and this
// is the endpoint that makes the venue DECLARE its cadence instead of the code
// assuming one (CLAUDE.md rule 3):
//
//	[["BTC", [["BinPerp", {...,"fundingIntervalHours":8}],
//	          ["HlPerp", {"fundingRate":"0.0000103378",
//	                      "nextFundingTime":1788487200000,
//	                      "fundingIntervalHours":1}], ...]], ...]
//
// Only the HlPerp entry describes Hyperliquid itself; the others are its view
// of Binance and Bybit and must never be read as its own.
// https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/info-endpoint/perpetuals

// hyperliquidSelfVenue is the name Hyperliquid gives its own perpetuals inside
// predictedFundings, alongside the competitors it also lists.
const hyperliquidSelfVenue = "HlPerp"

type hyperliquidAssetCtxMessage struct {
	Channel string `json:"channel"`
	Data    struct {
		Coin string `json:"coin"`
		Ctx  struct {
			Funding  string `json:"funding"`
			MarkPx   string `json:"markPx"`
			OraclePx string `json:"oraclePx"`
		} `json:"ctx"`
	} `json:"data"`
}

// handleHyperliquidFunding publishes one reading per activeAssetCtx frame.
//
// It reports two things: whether the frame belonged to this channel at all —
// which tells the caller to stop trying other shapes — and whether it PRODUCED
// a reading on the feed. The stream lifecycle needs the second answer to tell a
// live subscription from a socket that only answers keepalives
// (exchanges.StreamConfig.Handle, 2026-09-12).
func handleHyperliquidFunding(source string, symbols []exchanges.Symbol, meta *exchanges.FundingMetaCache,
	f exchanges.Feeds, raw []byte, recvAt time.Time) (handled, produced bool) {

	var message hyperliquidAssetCtxMessage
	if !exchanges.Decode(raw, &message) || message.Channel != "activeAssetCtx" {
		return false, false
	}

	standard := exchanges.StandardOf(symbols, message.Data.Coin)
	if standard == "" {
		return true, false // a coin this connector never subscribed to
	}
	rateFrac, err := strconv.ParseFloat(message.Data.Ctx.Funding, 64)
	if err != nil {
		return true, false
	}
	// No interval means no reading: RatePer8hFrac and APRFrac both divide by
	// it, and this venue's whole trap is that assuming 8h is wrong by 8×.
	entry, ok := meta.Get(standard)
	if !ok {
		return true, false
	}

	data, err := normalizeHyperliquidFunding(hyperliquidFundingInput{
		FundingReading: exchanges.FundingReading{
			Symbol: standard,
			Source: source,
			RecvAt: recvAt,
			// This payload carries no venue clock. Writing the local one here
			// would report our time as the venue's; RecvAt is ours and says so.
			VenueTimeMs: 0,
			RateFrac:    rateFrac,
		},
		IntervalHours: entry.IntervalHours,
		// ⚠️ Hyperliquid's "nextFundingTime" is the settlement of the period
		// ALREADY RUNNING, not the next one — the same off-by-one shape as
		// OKX's fundingTime, in the other direction. Measured across an hour
		// boundary on 2026-09-04: at 02:47:49Z HlPerp returned 02:00:00Z (47
		// minutes past) and at 03:01:30Z it returned 03:00:00Z (90 seconds
		// past), while the BinPerp and BybitPerp rows of the same responses
		// both returned a future 08:00:00Z. So the upcoming settlement is one
		// interval later, and taking the field at face value would publish a
		// moment that has already gone.
		//
		// FutureStampMs still guards the result: the metadata is refreshed
		// every few minutes against an hourly cadence, so a stamp fetched just
		// before a boundary can be stale by the time it is used, and a
		// countdown to the past is worse than none.
		NextFundingAtMs: exchanges.FutureStampMs(
			entry.NextFundingAtMs+entry.IntervalHours*exchanges.SecPerHour*exchanges.MsPerSecond, recvAt),
	})
	if err != nil {
		return true, false
	}
	data.MarkPrice, _ = strconv.ParseFloat(message.Data.Ctx.MarkPx, 64)
	data.IndexPrice, _ = strconv.ParseFloat(message.Data.Ctx.OraclePx, 64)
	data.IsEstimated = true // the rate for the hour now running

	return true, f.SendFunding(data)
}

// refreshHyperliquidFundingMeta reads predictedFundings and keeps the HlPerp
// entry for every configured symbol.
func refreshHyperliquidFundingMeta(ctx context.Context, meta *exchanges.FundingMetaCache, symbols []exchanges.Symbol) error {
	// [[coin, [[venueName, {fundingRate, nextFundingTime, fundingIntervalHours}]]]]
	var raw [][]any
	if err := exchanges.PostJSON(ctx, "https://api.hyperliquid.xyz/info", `{"type":"predictedFundings"}`, &raw); err != nil {
		return err
	}
	parsed, err := parseHyperliquidPredictedFundings(raw, symbols)
	if err != nil {
		return err
	}
	meta.Put(parsed)
	return nil
}

// parseHyperliquidPredictedFundings walks the venue's nested-array shape. It
// is decoded as []any deliberately: the payload is a heterogeneous array whose
// first element is a string and whose second is a list of pairs, which no
// struct can describe.
func parseHyperliquidPredictedFundings(raw [][]any, symbols []exchanges.Symbol) (map[string]exchanges.FundingMetaEntry, error) {
	wanted := make(map[string]string, len(symbols)) // venue coin → standard
	for _, s := range symbols {
		wanted[s.Venue] = s.Standard
	}

	out := make(map[string]exchanges.FundingMetaEntry, len(symbols))
	for _, row := range raw {
		if len(row) != 2 {
			continue
		}
		coin, ok := row[0].(string)
		if !ok {
			continue
		}
		standard, wantedCoin := wanted[coin]
		if !wantedCoin {
			continue
		}
		venues, ok := row[1].([]any)
		if !ok {
			continue
		}
		for _, venueRow := range venues {
			pair, ok := venueRow.([]any)
			if !ok || len(pair) != 2 {
				continue
			}
			// Only Hyperliquid's own row describes Hyperliquid: the same list
			// carries BinPerp and BybitPerp, whose 8h cadence would be wrong
			// by 8× if read as this venue's.
			if name, ok := pair[0].(string); !ok || name != hyperliquidSelfVenue {
				continue
			}
			fields, ok := pair[1].(map[string]any)
			if !ok {
				continue
			}
			hours, ok := fields["fundingIntervalHours"].(float64)
			if !ok || hours <= 0 {
				continue
			}
			entry := exchanges.FundingMetaEntry{IntervalHours: int64(hours)}
			if next, ok := fields["nextFundingTime"].(float64); ok {
				entry.NextFundingAtMs = int64(next)
			}
			out[standard] = entry
		}
	}
	if len(out) == 0 && len(symbols) > 0 {
		return nil, fmt.Errorf("predictedFundings carried no %s entry for any configured coin", hyperliquidSelfVenue)
	}
	return out, nil
}
