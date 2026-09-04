package exchanges

import "time"

// Kraken's ticker feed (step 2.5). Measured payload, 2026-09-04:
//
//	{"time":1788487590000,"product_id":"PF_XBTUSD",
//	 "funding_rate":-1.198397121121223,
//	 "funding_rate_prediction":-1.3488802755275,
//	 "relative_funding_rate":-0.000014799208333333,
//	 "relative_funding_rate_prediction":-0.00001668025,
//	 "next_funding_rate_time":1788490800000,"feed":"ticker",
//	 "markPrice":80865.23511128523,"index":80873.57,...}
//
// That single frame shows both Kraken traps at once. funding_rate is an
// ABSOLUTE price amount (-1.198 next to a relative -0.0000148, a ratio of
// ~80,900 — the index price), so the relative field is the rate and the
// obvious one is not. And next_funding_rate_time is an ABSOLUTE epoch-ms
// stamp: 1788490800000 is the next round hour, not "ms remaining", whatever
// the doc prose says.
//
// Kraken publishes no interval field anywhere in this message; the hourly
// cadence lives in normalizeKrakenFunding with its evidence.
// https://docs.kraken.com/api/docs/futures-api/websocket/ticker/

type krakenTickerMessage struct {
	Feed      string  `json:"feed"`
	ProductID string  `json:"product_id"`
	Time      int64   `json:"time"`
	MarkPrice float64 `json:"markPrice"`
	Index     float64 `json:"index"`
	// Pointers: this feed sends real negative rates, so a missing field and a
	// legitimately zero one must stay distinguishable.
	RelativeFundingRate *float64 `json:"relative_funding_rate"`
	NextFundingRateTime *int64   `json:"next_funding_rate_time"`
}

// handleKrakenFunding publishes one reading per ticker frame and reports
// whether the frame belonged to that feed.
func handleKrakenFunding(source string, symbols []Symbol, f Feeds, raw []byte, recvAt time.Time) bool {
	var message krakenTickerMessage
	if !decode(raw, &message) || message.Feed != "ticker" {
		return false
	}

	standard := StandardOf(symbols, message.ProductID)
	if standard == "" {
		return true // a product this connector never subscribed to
	}
	// Kraken sends a ticker for its dated futures too, and those carry no
	// relative funding rate at all. Absent means absent.
	if message.RelativeFundingRate == nil {
		return true
	}

	var nextFundingAtMs int64
	if message.NextFundingRateTime != nil {
		nextFundingAtMs = *message.NextFundingRateTime
	}

	data, err := normalizeKrakenFunding(krakenFundingInput{
		fundingReading: fundingReading{
			Symbol:      standard,
			Source:      source,
			RecvAt:      recvAt,
			VenueTimeMs: message.Time,
			RateFrac:    *message.RelativeFundingRate,
		},
		NextFundingAtMs: nextFundingAtMs,
	})
	if err != nil {
		return true
	}
	data.MarkPrice = message.MarkPrice
	data.IndexPrice = message.Index
	// relative_funding_rate is the rate accrued so far in the hour now
	// running; relative_funding_rate_prediction is the venue's estimate of
	// where it lands. The accrued figure is the one that settles, and it keeps
	// moving until it does.
	data.IsEstimated = true

	f.SendFunding(data)
	return true
}
