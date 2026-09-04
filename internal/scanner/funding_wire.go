package scanner

import (
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/fees"
)

// The funding half of the contract (step 2.7). docs/WS-CONTRACT.md §9 is
// authoritative; this file must not reshape it.
//
// Two conversions happen here and nowhere else. Go and SQLite carry funding as
// FRACTIONS (RatePer8hFrac, APRFrac) because that is what the arithmetic wants;
// the wire carries bps and percent because that is the vocabulary the rest of
// the contract already speaks (taker_fee_bps, spread_gross_pct) and what the
// dashboard renders. Doing it in one place is what keeps a second, drifting
// definition of "per 8h" out of the codebase.

// fundingModelGross is the only cost model a funding figure can have before
// step 3.1: nothing is deducted. CLAUDE.md rule 2.
const fundingModelGross = "gross"

// Why a funding reading stopped being trustworthy. Empty while it is live.
const (
	// staleReasonAge: nothing has arrived for longer than this source's
	// measured funding threshold. The feed is the problem.
	staleReasonAge = "age"
	// staleReasonSettled: the settlement this reading names has already
	// happened, so the rate describes a period that is over. The reading is
	// the problem - and this is the check that catches a dead funding
	// subscription on a venue that publishes only on change, where age proves
	// nothing (config.yaml, funding_publish_mode).
	staleReasonSettled = "settled"
)

// fundingSettledGrace is how long after a settlement a reading may still name
// it before being called stale.
//
// A venue republishes the next period within seconds of settling, but it does
// not do so at the same instant, and the reading crossing the boundary is not
// wrong - it is one message old. Too small a grace makes every venue flap at
// every settlement; too large delays the one check that catches a silently
// dropped funding subscription. Two minutes is under 4% of the shortest
// interval in the system (Kraken and Hyperliquid settle hourly).
const fundingSettledGrace = 2 * time.Minute

const (
	bpsPerUnit = 10000 // a fraction to basis points
	pctPerUnit = 100   // a fraction to percent
	secPerDay  = 86400
)

// HedgeLeg is where one perpetual's spot hedge lives, or why it has none.
//
// The scanner is told this rather than working it out: the hedge mapping is
// built from the instrument registry (step 2.4), and pulling that dependency
// into the wire layer would put venue trading rules inside the package whose
// job is to describe data to a browser. cmd/scanner owns the translation.
type HedgeLeg struct {
	Symbol     string
	PerpSource string

	// SpotSource is the spot market this perp can be hedged against, already
	// chosen among the valid candidates; "" means there is none and NoteVI
	// says why, in the venue's own terms.
	SpotSource string
	NoteVI     string
}

// wireFundingPoint is one venue's latest funding reading for one pair, with
// everything needed to judge whether it can be trusted and what it would cost
// to act on.
//
// It carries the raw venue value beside the normalized one for the same reason
// FundingData does: a number on screen has to be traceable back to the message
// it came from. RawRate is NEVER the figure to compute with - venues quote it
// per 1h, per 8h and as an absolute price amount.
type wireFundingPoint struct {
	Model string `json:"model"` // discrete | continuous

	// The comparison figures. RatePer8hBps is the cross-venue one: the same
	// bps at 1h and at 8h are eight different annual returns, so a table that
	// compares per-interval rates compares nothing.
	RatePer8hBps       float64 `json:"rate_per_8h_bps"`
	RatePerIntervalBps float64 `json:"rate_per_interval_bps"`
	IntervalSec        int64   `json:"interval_sec"`
	APRGrossPct        float64 `json:"apr_gross_pct"`

	// NextFundingAtMs is absolute epoch ms; 0 means no settlement instant
	// exists (continuous) or the venue published none. The dashboard counts
	// down from ServerTimeMs, never from the browser clock.
	NextFundingAtMs int64 `json:"next_funding_at_ms"`
	IsEstimated     bool  `json:"is_estimated"`

	// Freshness, decided by the backend exactly as it is for prices, and
	// measured from RecvAtMs alone. AgeMs is -1 when there is nothing to
	// measure from.
	RecvAtMs    int64  `json:"recv_at_ms"`
	AgeMs       int64  `json:"age_ms"`
	Status      string `json:"status"`
	StaleReason string `json:"stale_reason"` // "" | age | settled

	MarkPrice  float64 `json:"mark_price"`
	IndexPrice float64 `json:"index_price"`

	// Venue-published bounds on ONE settlement's rate. The two flags are
	// separate because Bybit publishes a cap and no floor, and one flag would
	// turn its unset floor into "funding can never be negative".
	RateCapPerIntervalBps   float64 `json:"rate_cap_per_interval_bps"`
	RateFloorPerIntervalBps float64 `json:"rate_floor_per_interval_bps"`
	HasCap                  bool    `json:"has_cap"`
	HasFloor                bool    `json:"has_floor"`

	// Diagnostics: what the venue actually sent, and which field it came from.
	RateType     string  `json:"rate_type"`
	RawRate      float64 `json:"raw_rate"`
	RawRateField string  `json:"raw_rate_field"`

	// The hedge. An attractive rate on a perp with no spot leg is not an
	// opportunity, and three of the seven venues here quote USD against
	// USDT-only spot markets - so this is the difference between a number
	// worth reading and a number that cannot be acted on at all.
	HedgeSpotSource string `json:"hedge_spot_source"`
	HedgeNoteVI     string `json:"hedge_note_vi"`

	// BreakevenDaysFeesOnly is how long the position must be held for the
	// funding collected to cover the commission of opening and closing BOTH
	// legs, and nothing else. null when there is no hedge, when either venue's
	// fee schedule is unverified, or when the rate does not pay this side of
	// the trade at all. It is not a profit figure and not net of slippage.
	BreakevenDaysFeesOnly *float64 `json:"breakeven_days_fees_only"`
}

type wireFunding struct {
	Type         string                                 `json:"type"`
	V            int                                    `json:"v"`
	ServerTimeMs int64                                  `json:"server_time_ms"`
	Funding      map[string]map[string]wireFundingPoint `json:"funding"`
}

// newWireFunding builds the funding message from the readings the scanner holds.
//
// Every reading is published, including a stale one: dropping it would make a
// venue whose funding subscription died disappear from the table, which reads
// as "this venue has no funding" instead of "this feed stopped". Same rule as
// prices.
func newWireFunding(readings []exchanges.FundingData, hedges map[string]HedgeLeg, now time.Time) wireFunding {
	out := wireFunding{
		Type:         "funding",
		V:            wireVersion,
		ServerTimeMs: now.UnixMilli(),
		Funding:      make(map[string]map[string]wireFundingPoint),
	}

	for _, data := range readings {
		if out.Funding[data.Symbol] == nil {
			out.Funding[data.Symbol] = make(map[string]wireFundingPoint)
		}
		out.Funding[data.Symbol][data.Source] = newWireFundingPoint(data, hedges[hedgeKey(data.Symbol, data.Source)], now)
	}
	return out
}

// hedgeKey addresses one perpetual's hedge: a spot leg is valid for a pair on a
// venue, not for the venue as a whole.
func hedgeKey(symbol, perpSource string) string { return symbol + "|" + perpSource }

func newWireFundingPoint(data exchanges.FundingData, hedge HedgeLeg, now time.Time) wireFundingPoint {
	status, reason := fundingStatus(data, fundingStaleAfter(data.Source), now)

	point := wireFundingPoint{
		Model:              string(data.Model),
		RatePer8hBps:       data.RatePer8hFrac * bpsPerUnit,
		RatePerIntervalBps: data.RatePerIntervalFrac * bpsPerUnit,
		IntervalSec:        data.IntervalSec,
		APRGrossPct:        data.APRFrac * pctPerUnit,
		NextFundingAtMs:    data.NextFundingAtMs,
		IsEstimated:        data.IsEstimated,
		RecvAtMs:           recvAtMs(data.RecvAt),
		AgeMs:              ageMs(data.RecvAt, now),
		Status:             status,
		StaleReason:        reason,
		MarkPrice:          data.MarkPrice,
		IndexPrice:         data.IndexPrice,
		HasCap:             data.HasCap,
		HasFloor:           data.HasFloor,
		RateType:           data.RateType,
		RawRate:            data.RawRate,
		RawRateField:       data.RawRateField,
		HedgeSpotSource:    hedge.SpotSource,
		HedgeNoteVI:        hedgeNote(hedge),
	}
	// Converted only where the venue stated one: an unset bound must stay 0
	// beside its false flag rather than become a bound of exactly zero.
	if data.HasCap {
		point.RateCapPerIntervalBps = data.RateCapFrac * bpsPerUnit
	}
	if data.HasFloor {
		point.RateFloorPerIntervalBps = data.RateFloorFrac * bpsPerUnit
	}
	point.BreakevenDaysFeesOnly = breakevenDaysFeesOnly(data, hedge.SpotSource)
	return point
}

// hedgeNote explains a missing spot leg.
//
// A leg the mapping never reported is NOT the same as one it refused, and
// saying "no hedge" for both would turn "the registry has not refreshed yet"
// and "this venue quotes USD against USDT-only spot markets" into the same
// sentence. The first is temporary and the second is permanent.
func hedgeNote(hedge HedgeLeg) string {
	if hedge.SpotSource != "" || hedge.NoteVI != "" {
		return hedge.NoteVI
	}
	return "Chưa biết: bảng ghép spot↔perp chưa dựng xong (registry chưa refresh)."
}

// recvAtMs is 0 for a reading that never carried a receive stamp, matching the
// price contract's documented default.
func recvAtMs(recvAt time.Time) int64 {
	if recvAt.IsZero() {
		return 0
	}
	return recvAt.UnixMilli()
}

// ageMs is -1 when there is nothing to measure from, exactly as for prices.
func ageMs(recvAt, now time.Time) int64 {
	if recvAt.IsZero() {
		return -1
	}
	return now.Sub(recvAt).Milliseconds()
}

// fundingStaleAfter is how long this source may go without republishing funding
// before its last reading stops being trusted.
//
// It is a per-source measurement and much larger than the price threshold; see
// config.yaml for the numbers and for why a venue that publishes only on change
// gets a threshold derived from its settlement interval instead of from
// observation. A source with no configured value gets the price threshold as a
// floor rather than zero, which would mark every reading stale on arrival.
func fundingStaleAfter(source string) time.Duration {
	if index, ok := sourceOrder[source]; ok {
		if sec := sourceRegistry[index].FundingStaleAfterSec; sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	return staleAfter(source)
}

// fundingStatus decides whether one funding reading can still be trusted, and
// says which of the two independent things went wrong.
//
// Age is checked first: when a feed has gone silent AND the settlement it named
// has passed, the silence is the cause and the expired stamp is its symptom.
//
// The settlement check exists because age alone cannot do this job. Bybit
// republishes only when a funding field moves - measured 2026-09-04, it went 19
// minutes without a message while its socket carried book updates the whole
// time - so its threshold has to be the settlement interval, and a subscription
// that dies would otherwise look live for eight hours. Once the settlement it
// names is behind us, the reading describes a period that is over no matter how
// recently it arrived.
func fundingStatus(data exchanges.FundingData, threshold time.Duration, now time.Time) (status, reason string) {
	if data.RecvAt.IsZero() {
		return statusUnknown, ""
	}
	if now.Sub(data.RecvAt) > threshold {
		return statusStale, staleReasonAge
	}
	// Continuous accrual has no settlement instant to expire (Paradex), and a
	// discrete reading whose venue publishes no stamp leaves this at 0.
	if data.Model == exchanges.FundingDiscrete && data.NextFundingAtMs > 0 {
		settledAt := time.UnixMilli(data.NextFundingAtMs)
		if now.After(settledAt.Add(fundingSettledGrace)) {
			return statusStale, staleReasonSettled
		}
	}
	return statusLive, ""
}

// breakevenDaysFeesOnly is how many days of funding pay for the commission of
// opening and closing both legs.
//
// FEES ONLY. Slippage needs order book depth and is not deducted anywhere yet,
// the spot leg's borrow cost is not in it, and the rate is assumed to hold -
// which it will not. It is an ordering aid for a table, not a forecast, and it
// is null far more often than it is a number.
//
// The income term counts SETTLEMENTS, never an APR multiplied by a holding
// time: a position pays only if it is open at the settlement instant
// (CLAUDE.md rule 6). Days rather than settlements is what the reader wants,
// and 86400/IntervalSec settlements land in every day for every interval any
// venue here uses. For the continuous model the same arithmetic is exact for a
// different reason - accrual never stops - so the branch is on Model only in
// what it means, not in what it computes.
//
// null when: there is no spot leg to hedge against, either venue's fee schedule
// is unverified (an unknown fee is not a free one), the venue published no
// usable interval, or the rate does not pay the side of the trade this project
// takes. Spot long plus perp short receives funding when the rate is POSITIVE;
// a negative rate is a cost, and a cost has no breakeven.
func breakevenDaysFeesOnly(data exchanges.FundingData, spotSource string) *float64 {
	if spotSource == "" || data.IntervalSec <= 0 || data.RatePerIntervalFrac <= 0 {
		return nil
	}
	costPct, ok := fees.RoundTripTakerPct(scheduleFor(spotSource), scheduleFor(data.Source))
	if !ok {
		return nil
	}
	// A verified zero-fee pair really exists (Paradex charges retail nothing),
	// and it breaks even immediately rather than after an infinite wait.
	if costPct <= 0 {
		zero := 0.0
		return &zero
	}
	settlementsPerDay := float64(secPerDay) / float64(data.IntervalSec)
	incomePctPerDay := data.RatePerIntervalFrac * pctPerUnit * settlementsPerDay
	days := costPct / incomePctPerDay
	return &days
}
