package okx

import (
	"futures-arbitrage-scanner/exchanges"

	"strconv"
	"time"
)

// OKX's funding-rate channel (step 2.5). Measured payload, 2026-09-04:
//
//	{"arg":{"channel":"funding-rate","instId":"BTC-USDT-SWAP"},"data":[{
//	  "formulaType":"withRate","fundingRate":"0.0000663322867090",
//	  "fundingTime":"1788508800000","impactValue":"20000",
//	  "instId":"BTC-USDT-SWAP","instType":"SWAP","interestRate":"0.0001",
//	  "maxFundingRate":"0.00375","method":"current_period",
//	  "minFundingRate":"-0.00375","nextFundingRate":"",
//	  "nextFundingTime":"1788537600000","premium":"-0.0004527652078389",
//	  "prevFundingTime":"1788480000000","settFundingRate":"0.0000418741079605",
//	  "settState":"settled","ts":"1788487507788"}]}
//
// Two traps are visible in that one frame and both are handled below:
// fundingTime is the UPCOMING settlement while nextFundingTime is the one
// after it (trap ①), and nextFundingRate is EMPTY under method=current_period
// — parsed as 0 it would publish "the next period pays nothing".
// https://www.okx.com/docs-v5/en/#public-data-websocket-funding-rate-channel

type okxFundingMessage struct {
	Arg struct {
		Channel string `json:"channel"`
		InstID  string `json:"instId"`
	} `json:"arg"`
	Data []struct {
		InstID          string `json:"instId"`
		FundingRate     string `json:"fundingRate"`
		FundingTime     string `json:"fundingTime"`
		NextFundingRate string `json:"nextFundingRate"`
		NextFundingTime string `json:"nextFundingTime"`
		MinFundingRate  string `json:"minFundingRate"`
		MaxFundingRate  string `json:"maxFundingRate"`
		SettState       string `json:"settState"`
		TS              string `json:"ts"`
	} `json:"data"`
}

// handleOKXFunding publishes one reading per funding-rate entry and reports
// whether the frame belonged to that channel.
func handleOKXFunding(source string, symbols []exchanges.Symbol, f exchanges.Feeds, raw []byte, recvAt time.Time) bool {
	var message okxFundingMessage
	if !exchanges.Decode(raw, &message) || message.Arg.Channel != "funding-rate" {
		return false
	}

	for _, entry := range message.Data {
		standard := exchanges.StandardOf(symbols, entry.InstID)
		if standard == "" {
			continue // an instrument this connector never subscribed to
		}
		rateFrac, err := strconv.ParseFloat(entry.FundingRate, 64)
		if err != nil {
			continue
		}
		fundingAtMs, err1 := strconv.ParseInt(entry.FundingTime, 10, 64)
		nextFundingAtMs, err2 := strconv.ParseInt(entry.NextFundingTime, 10, 64)
		if err1 != nil || err2 != nil {
			// The interval is DERIVED from these two, so without both there is
			// no interval, and without an interval there is no reading — every
			// consumer divides by it.
			continue
		}
		venueTimeMs, _ := strconv.ParseInt(entry.TS, 10, 64)

		data, err := normalizeOKXFunding(okxFundingInput{
			FundingReading: exchanges.FundingReading{
				Symbol:      standard,
				Source:      source,
				RecvAt:      recvAt,
				VenueTimeMs: venueTimeMs,
				RateFrac:    rateFrac,
			},
			FundingAtMs:     fundingAtMs,
			NextFundingAtMs: nextFundingAtMs,
		})
		if err != nil {
			continue
		}

		// nextFundingRate is "" under method=current_period: absent, not zero.
		// HasFollowingRate stays false and the field keeps its zero value —
		// which is exactly why consumers must read the flag, not the number.
		if followingFrac, err := strconv.ParseFloat(entry.NextFundingRate, 64); err == nil {
			data.FollowingRateFrac = followingFrac
			data.FollowingAtMs = nextFundingAtMs
			data.HasFollowingRate = true
		}
		if capFrac, err := strconv.ParseFloat(entry.MaxFundingRate, 64); err == nil {
			data.RateCapFrac, data.HasCap = capFrac, true
		}
		if floorFrac, err := strconv.ParseFloat(entry.MinFundingRate, 64); err == nil {
			data.RateFloorFrac, data.HasFloor = floorFrac, true
		}
		// settState "settled" means the rate for the period now running is
		// still moving; "processing" is the brief settlement window.
		data.IsEstimated = entry.SettState != "processing"

		if !f.SendFunding(data) {
			return true // shutting down
		}
	}
	return true
}
