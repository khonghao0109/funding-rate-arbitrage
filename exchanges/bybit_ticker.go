package exchanges

import (
	"log"
	"strconv"
	"strings"
	"time"
)

// Bybit's v5 linear ticker, which is where its funding rate lives (step 2.5).
//
// ⚠️ THE trap of this venue: the channel pushes a full snapshot once and then
// DELTAS, and a field absent from a delta means UNCHANGED — not zero. Measured
// 2026-09-04, the snapshot carries fundingRate, nextFundingTime,
// fundingIntervalHour, fundingCap, markPrice and indexPrice, while a typical
// delta carries only {symbol, markPrice, ask1Price, ask1Size,
// openInterestValue}. Overwriting cached state with a delta would blank the
// funding rate to 0 every few hundred milliseconds and publish it as real.
//
// So this file keeps one merged ticker per market, updated field by field, and
// only fields the message actually contained are touched. json.RawMessage is
// what makes "contained" decidable: a *string that stays nil was absent, and a
// present-but-empty string is a different thing the venue does send
// (preOpenPrice is "" on a live market).
// https://bybit-exchange.github.io/docs/v5/websocket/public/ticker

// bybitTickerMessage is the envelope. Type is "snapshot" or "delta".
type bybitTickerMessage struct {
	Topic string            `json:"topic"`
	Type  string            `json:"type"`
	TS    int64             `json:"ts"`
	Data  bybitTickerFields `json:"data"`
}

// bybitTickerFields uses pointers so an absent field is distinguishable from a
// zero one. Only the fields this scanner consumes are declared; the rest of the
// payload (open interest, 24h stats, the pre-listing block) is ignored.
type bybitTickerFields struct {
	Symbol          string  `json:"symbol"`
	FundingRate     *string `json:"fundingRate"`
	NextFundingTime *string `json:"nextFundingTime"`
	// A STRING in the payload ("8"), like every other number Bybit sends —
	// declared as int64 it silently fails the whole decode and the frame
	// falls through to the orderbook branch, which is how the first version
	// of this file published no Bybit funding at all.
	FundingIntervalHour *string `json:"fundingIntervalHour"`
	FundingCap          *string `json:"fundingCap"`
	FundingFloor        *string `json:"fundingFloor"`
	MarkPrice           *string `json:"markPrice"`
	IndexPrice          *string `json:"indexPrice"`
}

// bybitTicker is the merged state for one market: the last value seen for each
// field, whichever message carried it.
type bybitTicker struct {
	fundingRate         string
	nextFundingTime     string
	fundingIntervalHour string
	fundingCap          string
	fundingFloor        string
	markPrice           string
	indexPrice          string
	haveRate            bool
	haveInterval        bool
}

// merge folds one message's PRESENT fields into the cached state and reports
// whether any FUNDING field actually changed value.
//
// Mark and index prices are merged too but deliberately do not count as a
// change: they move on nearly every delta, and treating them as funding news
// would publish a reading — and refresh its receive stamp — ten times a second.
func (t *bybitTicker) merge(fields bybitTickerFields) bool {
	changed := false
	if fields.FundingRate != nil {
		changed = changed || *fields.FundingRate != t.fundingRate
		t.fundingRate = *fields.FundingRate
		t.haveRate = true
	}
	if fields.NextFundingTime != nil {
		changed = changed || *fields.NextFundingTime != t.nextFundingTime
		t.nextFundingTime = *fields.NextFundingTime
	}
	if fields.FundingIntervalHour != nil {
		changed = changed || *fields.FundingIntervalHour != t.fundingIntervalHour
		t.fundingIntervalHour = *fields.FundingIntervalHour
		t.haveInterval = true
	}
	if fields.FundingCap != nil {
		changed = changed || *fields.FundingCap != t.fundingCap
		t.fundingCap = *fields.FundingCap
	}
	if fields.FundingFloor != nil {
		changed = changed || *fields.FundingFloor != t.fundingFloor
		t.fundingFloor = *fields.FundingFloor
	}
	if fields.MarkPrice != nil {
		t.markPrice = *fields.MarkPrice
	}
	if fields.IndexPrice != nil {
		t.indexPrice = *fields.IndexPrice
	}
	return changed
}

// handleBybitTicker merges one ticker frame and publishes the funding reading
// the merged state describes. It reports whether the frame was a ticker at all.
//
// tickers is per-connection state, cleared on every (re)subscribe: Bybit
// resends a snapshot then, and state assembled over a socket that no longer
// exists could otherwise supply a field the new session never confirmed.
func handleBybitTicker(source string, symbols []Symbol, tickers map[string]*bybitTicker,
	f Feeds, raw []byte, recvAt time.Time) bool {

	var message bybitTickerMessage
	if !decode(raw, &message) || message.Data.Symbol == "" {
		return false
	}
	if message.Type != "snapshot" && message.Type != "delta" {
		return false
	}
	if !strings.HasPrefix(message.Topic, "tickers.") {
		return false
	}

	standard := StandardOf(symbols, message.Data.Symbol)
	if standard == "" {
		return true // a market this connector never subscribed to
	}

	ticker := tickers[message.Data.Symbol]
	if ticker == nil {
		ticker = &bybitTicker{}
		tickers[message.Data.Symbol] = ticker
	}
	changed := ticker.merge(message.Data)

	// The snapshot that carries the funding fields may not have arrived yet
	// after a (re)subscribe.
	if !ticker.haveRate || !ticker.haveInterval {
		return true
	}
	// A delta that changed no FUNDING field says nothing new about funding, and
	// republishing on it would be actively harmful: this channel pushes every
	// ~100ms (measured 2026-09-04), so four pairs would emit ~40 readings a
	// second, and each one would refresh RecvAt. Freshness is judged from
	// RecvAt — by the staleness filter now and by the dashboard at 2.7 — so a
	// price-only delta refreshing it would make a funding subscription that
	// died silently look permanently current, which is the step-1.6 defect
	// re-created one layer up.
	if !changed {
		return true
	}
	rateFrac, err := strconv.ParseFloat(ticker.fundingRate, 64)
	if err != nil {
		return true
	}
	// An unparseable settlement stamp stays 0 ("not supplied") rather than
	// discarding a good rate.
	nextFundingAtMs, _ := strconv.ParseInt(ticker.nextFundingTime, 10, 64)
	intervalHours, err := strconv.ParseInt(ticker.fundingIntervalHour, 10, 64)
	if err != nil {
		// Logged, not just skipped: this exact field already killed Bybit
		// funding once, silently, when its string type poisoned an int64
		// decode (trap ⑨). A future format change ("0.5", "8h") stops this
		// symbol's funding — the log line is the only difference between
		// "venue changed the format" and "subscription died".
		log.Printf("%s: fundingIntervalHour %q for %s does not parse as whole hours; funding for this symbol is NOT published",
			source, ticker.fundingIntervalHour, standard)
		return true
	}

	data, err := normalizeBybitFunding(bybitFundingInput{
		fundingReading: fundingReading{
			Symbol:      standard,
			Source:      source,
			RecvAt:      recvAt,
			VenueTimeMs: message.TS,
			RateFrac:    rateFrac,
		},
		IntervalHours:   intervalHours,
		NextFundingAtMs: nextFundingAtMs,
	})
	if err != nil {
		return true
	}
	data.MarkPrice, _ = strconv.ParseFloat(ticker.markPrice, 64)
	data.IndexPrice, _ = strconv.ParseFloat(ticker.indexPrice, 64)
	// Bybit publishes fundingCap on the snapshot and NO floor at all (measured
	// 2026-09-04, and the field stays absent across every recorded frame), so
	// the two bounds are flagged independently. Reporting an unset floor as 0
	// would claim this venue never pays negative funding.
	if capFrac, err := strconv.ParseFloat(ticker.fundingCap, 64); err == nil {
		data.RateCapFrac, data.HasCap = capFrac, true
	}
	if floorFrac, err := strconv.ParseFloat(ticker.fundingFloor, 64); err == nil {
		data.RateFloorFrac, data.HasFloor = floorFrac, true
	}
	data.IsEstimated = true // the rate for the period now running

	f.SendFunding(data)
	return true
}
