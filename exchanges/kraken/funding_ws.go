package kraken

import "futures-arbitrage-scanner/exchanges"

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

// handleKrakenFunding publishes one reading per ticker frame.
//
// It reports two things: whether the frame belonged to this channel at all —
// which tells the caller to stop trying other shapes — and whether it PRODUCED
// a reading on the feed. The stream lifecycle needs the second answer to tell a
// live subscription from a socket that only answers keepalives
// (exchanges.StreamConfig.Handle, 2026-09-12).
func handleKrakenFunding(source string, symbols []exchanges.Symbol, f exchanges.Feeds, raw []byte, recvAt time.Time) (handled, produced bool) {
	var message krakenTickerMessage
	if !exchanges.Decode(raw, &message) || message.Feed != "ticker" {
		return false, false
	}

	standard := exchanges.StandardOf(symbols, message.ProductID)
	if standard == "" {
		return true, false // a product this connector never subscribed to
	}
	// Kraken sends a ticker for its dated futures too, and those carry no
	// relative funding rate at all. Absent means absent.
	if message.RelativeFundingRate == nil {
		return true, false
	}

	var nextFundingAtMs int64
	if message.NextFundingRateTime != nil {
		nextFundingAtMs = *message.NextFundingRateTime
	}

	data, err := normalizeKrakenFunding(krakenFundingInput{
		FundingReading: exchanges.FundingReading{
			Symbol:      standard,
			Source:      source,
			RecvAt:      recvAt,
			VenueTimeMs: message.Time,
			RateFrac:    *message.RelativeFundingRate,
		},
		NextFundingAtMs: nextFundingAtMs,
	})
	if err != nil {
		return true, false
	}
	data.MarkPrice = message.MarkPrice
	data.IndexPrice = message.Index
	// relative_funding_rate is a SETTLED figure, not a forming one. The docs
	// define it as "the absolute funding rate relative to the spot price at
	// the time of funding rate calculation" — a calculation that already
	// happened — and put the forming estimate in a SEPARATE field,
	// relative_funding_rate_prediction ("the estimated next..."), which this
	// connector does not read. The project's own probe agrees: the WS value
	// matched the already-settled hour of /v4/historicalfundingrates
	// (funding_test.go, DATA-REQUIREMENTS §3.3⑥). The earlier hard-coded
	// `true` here contradicted both, and a phase-3 consumer filtering for
	// final rates would have discarded the venue entirely.
	// https://docs.kraken.com/api/docs/futures-api/websocket/ticker
	//
	// Nuance a consumer must know: final means final for the LAST COMPLETED
	// hourly calculation. Paired with next_funding_rate_time it reads "the
	// most recent settled rate, next settlement at T" — it does not predict
	// what settles at T.
	data.IsEstimated = false

	return true, f.SendFunding(data)
}
