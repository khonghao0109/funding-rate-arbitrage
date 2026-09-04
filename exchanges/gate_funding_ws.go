package exchanges

import (
	"strconv"
	"time"
)

// Gate's futures.tickers channel (step 2.5). Measured payload, 2026-09-04:
//
//	{"time":1788487588,"time_ms":1788487588269,"channel":"futures.tickers",
//	 "event":"update","result":[{"contract":"BTC_USDT","last":"80825.0",
//	 "mark_price":"80825.0","funding_rate":"0.000048",
//	 "funding_rate_indicative":"0.000048","funding_interval":28800,
//	 "funding_offset":0,"funding_next_apply":1788508800,
//	 "index_price":"80841.2",...,"t":1788487588240}]}
//
// Gate is the only venue whose funding message needs no second endpoint: the
// rate, the interval (SECONDS — the unit trap that makes 28800 look like a
// timestamp elsewhere) and the next settlement (epoch SECONDS) all ride here.
// https://www.gate.com/docs/developers/futures/ws/en/#tickers-api

type gateTickerMessage struct {
	Time    int64  `json:"time"`
	Channel string `json:"channel"`
	Event   string `json:"event"`
	Result  []struct {
		Contract              string `json:"contract"`
		MarkPrice             string `json:"mark_price"`
		IndexPrice            string `json:"index_price"`
		FundingRate           string `json:"funding_rate"`
		FundingRateIndicative string `json:"funding_rate_indicative"`
		FundingIntervalSec    int64  `json:"funding_interval"`
		FundingNextApplySec   int64  `json:"funding_next_apply"`
		TimeMs                int64  `json:"t"`
	} `json:"result"`
}

// handleGateFunding publishes one reading per ticker entry and reports whether
// the frame belonged to that channel.
func handleGateFunding(source string, symbols []Symbol, f Feeds, raw []byte, recvAt time.Time) bool {
	var message gateTickerMessage
	if !decode(raw, &message) || message.Channel != "futures.tickers" {
		return false
	}
	if message.Event != "update" {
		return true // the subscription acknowledgement rides the same channel
	}

	for _, entry := range message.Result {
		standard := StandardOf(symbols, entry.Contract)
		if standard == "" {
			continue // a contract this connector never subscribed to
		}
		rateFrac, err := strconv.ParseFloat(entry.FundingRate, 64)
		if err != nil {
			continue
		}

		data, err := normalizeGateFunding(gateFundingInput{
			fundingReading: fundingReading{
				Symbol:      standard,
				Source:      source,
				RecvAt:      recvAt,
				VenueTimeMs: entry.TimeMs,
				RateFrac:    rateFrac,
			},
			IntervalSec:         entry.FundingIntervalSec,
			FundingNextApplySec: entry.FundingNextApplySec,
		})
		if err != nil {
			continue
		}
		data.MarkPrice, _ = strconv.ParseFloat(entry.MarkPrice, 64)
		data.IndexPrice, _ = strconv.ParseFloat(entry.IndexPrice, 64)
		// funding_rate is the settled figure for the period now running and
		// funding_rate_indicative the one still forming; when they differ the
		// reading is still moving. Measured 2026-09-04 they were equal.
		//
		// Compared as NUMBERS: "0.000048" and "4.8e-05" are the same rate, and
		// a venue that reformats one of the two fields would otherwise mark
		// every reading estimated.
		if indicativeFrac, err := strconv.ParseFloat(entry.FundingRateIndicative, 64); err == nil {
			data.IsEstimated = indicativeFrac != rateFrac
		}

		if !f.SendFunding(data) {
			return true // shutting down
		}
	}
	return true
}
