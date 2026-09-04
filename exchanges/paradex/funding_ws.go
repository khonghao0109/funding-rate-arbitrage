package paradex

import (
	"futures-arbitrage-scanner/exchanges"

	"strconv"
	"strings"
	"time"
)

// Paradex's funding_data channel (step 2.5). Measured payload, 2026-09-04:
//
//	{"jsonrpc":"2.0","method":"subscription","params":{
//	  "channel":"funding_data.BTC-USD-PERP","data":{"market":"BTC-USD-PERP",
//	  "funding_index":"23783.1345938951196",
//	  "funding_premium":"5.80239907283936344",
//	  "funding_rate":"0.00007172918481","funding_rate_8h":"0.00007172",
//	  "funding_period_hours":8,"created_at":1788487597060,
//	  "impact_premium_rate":"-0.0002813396290686"}}}
//
// This channel is used rather than markets_summary, which also carries a
// funding_rate: only funding_data states the QUOTE WINDOW the rate belongs to
// (funding_period_hours), and a rate without its window is a number that
// cannot be compared with any other venue. The payload confirms the window is
// what it says: funding_rate 0.00007172918481 sits beside funding_rate_8h
// 0.00007172, so the rate IS the 8h figure and needs no scaling.
//
// Funding V2 accrues continuously through funding_index — there is no
// settlement instant, which is why Model is continuous and NextFundingAtMs
// stays 0.
// https://docs.paradex.trade/ws/channels/funding-data

// paradexFundingChannelPrefix is what the per-market channel name starts with:
// the subscription is funding_data.{market}, so the channel identifies the
// market as well as the feed.
const paradexFundingChannelPrefix = "funding_data."

type paradexFundingEvent struct {
	Method string `json:"method"`
	Params struct {
		Channel string `json:"channel"`
		Data    struct {
			Market             string `json:"market"`
			FundingRate        string `json:"funding_rate"`
			FundingRate8h      string `json:"funding_rate_8h"`
			FundingIndex       string `json:"funding_index"`
			FundingPeriodHours int64  `json:"funding_period_hours"`
			CreatedAt          int64  `json:"created_at"`
		} `json:"data"`
	} `json:"params"`
}

// handleParadexFunding publishes one reading per funding_data event and
// reports whether the frame belonged to that channel.
func handleParadexFunding(source string, symbols []exchanges.Symbol, f exchanges.Feeds, raw []byte, recvAt time.Time) bool {
	var event paradexFundingEvent
	if !exchanges.Decode(raw, &event) || !strings.HasPrefix(event.Params.Channel, paradexFundingChannelPrefix) {
		return false
	}
	if event.Method != "subscription" {
		return true // the subscription acknowledgement names the same channel
	}

	standard := exchanges.StandardOf(symbols, event.Params.Data.Market)
	if standard == "" {
		return true // a market this connector never subscribed to
	}
	rateFrac, err := strconv.ParseFloat(event.Params.Data.FundingRate, 64)
	if err != nil {
		return true
	}

	data, err := normalizeParadexFunding(paradexFundingInput{
		FundingReading: exchanges.FundingReading{
			Symbol:      standard,
			Source:      source,
			RecvAt:      recvAt,
			VenueTimeMs: event.Params.Data.CreatedAt,
			RateFrac:    rateFrac,
		},
		PeriodHours: event.Params.Data.FundingPeriodHours,
	})
	if err != nil {
		return true
	}
	// A continuously accruing rate is always "still moving": there is no
	// moment at which this venue's figure is final for a period.
	data.IsEstimated = true

	f.SendFunding(data)
	return true
}
