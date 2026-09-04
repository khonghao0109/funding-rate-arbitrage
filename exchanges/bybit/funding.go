package bybit

import "futures-arbitrage-scanner/exchanges"

import "fmt"

// bybitFundingInput is one Bybit v5 linear ticker reading in Bybit's units.
//
// IntervalHours is the WS ticker's own fundingIntervalHour ("8"), measured
// 2026-09-04 in the tickers.BTCUSDT snapshot. The SAME venue quotes this in
// MINUTES on REST instruments-info (480) — trap ③ — so the field name carries
// the unit and a REST caller must convert before filling it.
// https://bybit-exchange.github.io/docs/v5/websocket/public/ticker
//
// ⚠️ The v5 ticker is snapshot+delta: an absent field means UNCHANGED, not
// zero. The connector merges into cached state and calls this with the MERGED
// values — never with a delta's partial view.
type bybitFundingInput struct {
	exchanges.FundingReading
	IntervalHours   int64
	NextFundingAtMs int64
}

func normalizeBybitFunding(in bybitFundingInput) (exchanges.FundingData, error) {
	if in.IntervalHours <= 0 {
		return exchanges.FundingData{}, fmt.Errorf("bybit funding %s: non-positive interval %dh", in.Symbol, in.IntervalHours)
	}
	return exchanges.DeriveFundingRates(exchanges.FundingData{
		Symbol: in.Symbol, Source: in.Source, RecvAt: in.RecvAt, VenueTimeMs: in.VenueTimeMs,
		Model:               exchanges.FundingDiscrete,
		RawRate:             in.RateFrac,
		RawRateField:        "fundingRate",
		RatePerIntervalFrac: in.RateFrac,
		IntervalSec:         in.IntervalHours * exchanges.SecPerHour,
		NextFundingAtMs:     in.NextFundingAtMs,
	})
}
