package binance

import (
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// One case per interval Binance actually runs (measured 2026-09-03: 4h is the
// majority, 8h next, 1h exists) — the conversion must scale by the SYMBOL's
// interval, never an assumed 8h.
func TestFundingUnitConversion(t *testing.T) {
	recvAt := time.Date(2026, 9, 3, 8, 36, 0, 0, time.UTC)
	for _, c := range []struct {
		name          string
		intervalHours int64
		reading       exchanges.FundingReading
		want          exchanges.FundingData
	}{
		{
			name: "8h: hours to seconds, rate unchanged", intervalHours: 8,
			reading: exchanges.FundingReading{Symbol: "BTCUSDT", Source: "binance_futures", RecvAt: recvAt, VenueTimeMs: 1788424561000, RateFrac: 0.00006206},
			want: exchanges.FundingData{
				Symbol: "BTCUSDT", Source: "binance_futures", VenueTimeMs: 1788424561000,
				Model: exchanges.FundingDiscrete, RawRate: 0.00006206, RawRateField: "lastFundingRate",
				RatePerIntervalFrac: 0.00006206, IntervalSec: 28800,
				RatePer8hFrac: 0.00006206, APRFrac: 0.00006206 * 1095,
				NextFundingAtMs: 1788451200000,
			},
		},
		{
			name: "4h override: rate per interval doubles per 8h", intervalHours: 4,
			reading: exchanges.FundingReading{Symbol: "ETHUSDT", Source: "binance_futures", RecvAt: recvAt, RateFrac: 0.0001},
			want: exchanges.FundingData{
				Symbol: "ETHUSDT", Source: "binance_futures",
				Model: exchanges.FundingDiscrete, RawRate: 0.0001, RawRateField: "lastFundingRate",
				RatePerIntervalFrac: 0.0001, IntervalSec: 14400,
				RatePer8hFrac: 0.0002, APRFrac: 0.0001 * 2190,
				NextFundingAtMs: 1788451200000,
			},
		},
		{
			name: "1h symbol (TUSDT class exists since 2026-09-03)", intervalHours: 1,
			reading: exchanges.FundingReading{Symbol: "TUSDT", Source: "binance_futures", RecvAt: recvAt, RateFrac: 0.0000125},
			want: exchanges.FundingData{
				Symbol: "TUSDT", Source: "binance_futures",
				Model: exchanges.FundingDiscrete, RawRate: 0.0000125, RawRateField: "lastFundingRate",
				RatePerIntervalFrac: 0.0000125, IntervalSec: 3600,
				RatePer8hFrac: 0.0001, APRFrac: 0.0000125 * 8760,
				NextFundingAtMs: 1788451200000,
			},
		},
	} {
		got, err := normalizeBinanceFunding(binanceFundingInput{
			FundingReading: c.reading, IntervalHours: c.intervalHours, NextFundingAtMs: 1788451200000,
		})
		exchangestest.CheckNormalizedFunding(t, c.name, got, err, recvAt, c.want)
	}
}

// A non-positive interval cannot be normalized — dividing by it would turn one
// bad message into an Inf/NaN APR that poisons every consumer downstream.
func TestFundingBuilder_RejectsNonPositiveIntervals(t *testing.T) {
	at := time.Now()
	for _, hours := range []int64{0, -8} {
		if _, err := normalizeBinanceFunding(binanceFundingInput{
			FundingReading: exchanges.FundingReading{Symbol: "X", Source: "s", RecvAt: at, RateFrac: 0.0001},
			IntervalHours:  hours, NextFundingAtMs: 1,
		}); err == nil {
			t.Errorf("interval %dh: want error, got nil", hours)
		}
	}
}
