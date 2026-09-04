package exchanges

import (
	"context"
	"math"
	"testing"
	"time"
)

// The expected values come from the live readings of 2026-09-03 recorded by
// cmd/fundingcheck and docs/DATA-REQUIREMENTS.md §3/§4.3. One case per
// distinct interval unit, so a wrong conversion factor cannot cancel out:
// hours (Binance, Hyperliquid), minutes (Bybit), seconds (Gate), derived from
// timestamps (OKX), fixed hourly (Kraken), quoted window only (Paradex).
func TestFundingUnitConversion_AllVenues(t *testing.T) {
	const (
		frac = 1e-15 // rate comparisons
		apr  = 1e-12 // APR carries the ×8760 factor, so a hair more slack
	)
	recvAt := time.Date(2026, 9, 3, 8, 36, 0, 0, time.UTC)
	cases := []struct {
		name  string
		build func() (FundingData, error)
		want  FundingData // identity, the normalized block and timing fields
	}{
		{
			name: "binance 8h: hours to seconds, rate unchanged",
			build: func() (FundingData, error) {
				return normalizeBinanceFunding(binanceFundingInput{
					fundingReading: fundingReading{Symbol: "BTCUSDT", Source: "binance_futures", RecvAt: recvAt, VenueTimeMs: 1788424561000, RateFrac: 0.00006206},
					IntervalHours:  8, NextFundingAtMs: 1788451200000,
				})
			},
			want: FundingData{
				Symbol: "BTCUSDT", Source: "binance_futures", VenueTimeMs: 1788424561000,
				Model: FundingDiscrete, RawRate: 0.00006206, RawRateField: "lastFundingRate",
				RatePerIntervalFrac: 0.00006206, IntervalSec: 28800,
				RatePer8hFrac: 0.00006206, APRFrac: 0.00006206 * 1095,
				NextFundingAtMs: 1788451200000,
			},
		},
		{
			name: "binance 4h override: rate per interval doubles per 8h",
			build: func() (FundingData, error) {
				return normalizeBinanceFunding(binanceFundingInput{
					fundingReading: fundingReading{Symbol: "ETHUSDT", Source: "binance_futures", RecvAt: recvAt, VenueTimeMs: 0, RateFrac: 0.0001},
					IntervalHours:  4, NextFundingAtMs: 1788451200000,
				})
			},
			want: FundingData{
				Symbol: "ETHUSDT", Source: "binance_futures",
				Model: FundingDiscrete, RawRate: 0.0001, RawRateField: "lastFundingRate",
				RatePerIntervalFrac: 0.0001, IntervalSec: 14400,
				RatePer8hFrac: 0.0002, APRFrac: 0.0001 * 2190,
				NextFundingAtMs: 1788451200000,
			},
		},
		{
			name: "binance 1h symbol (TUSDT class exists since 2026-09-03)",
			build: func() (FundingData, error) {
				return normalizeBinanceFunding(binanceFundingInput{
					fundingReading: fundingReading{Symbol: "TUSDT", Source: "binance_futures", RecvAt: recvAt, VenueTimeMs: 0, RateFrac: 0.0000125},
					IntervalHours:  1, NextFundingAtMs: 1788451200000,
				})
			},
			want: FundingData{
				Symbol: "TUSDT", Source: "binance_futures",
				Model: FundingDiscrete, RawRate: 0.0000125, RawRateField: "lastFundingRate",
				RatePerIntervalFrac: 0.0000125, IntervalSec: 3600,
				RatePer8hFrac: 0.0001, APRFrac: 0.0000125 * 8760,
				NextFundingAtMs: 1788451200000,
			},
		},
		{
			name: "bybit: HOURS from the WS ticker (the same venue quotes MINUTES on REST — trap ③)",
			build: func() (FundingData, error) {
				return normalizeBybitFunding(bybitFundingInput{
					fundingReading: fundingReading{Symbol: "BTCUSDT", Source: "bybit_futures", RecvAt: recvAt, VenueTimeMs: 0, RateFrac: 0.0001},
					IntervalHours:  8, NextFundingAtMs: 1788451200000,
				})
			},
			want: FundingData{
				Symbol: "BTCUSDT", Source: "bybit_futures",
				Model: FundingDiscrete, RawRate: 0.0001, RawRateField: "fundingRate",
				RatePerIntervalFrac: 0.0001, IntervalSec: 28800,
				RatePer8hFrac: 0.0001, APRFrac: 0.0001 * 1095,
				NextFundingAtMs: 1788451200000,
			},
		},
		{
			name: "okx: fundingTime IS the next settlement (trap ①), interval derived",
			build: func() (FundingData, error) {
				return normalizeOKXFunding(okxFundingInput{
					fundingReading: fundingReading{Symbol: "BTCUSDT", Source: "okx_futures", RecvAt: recvAt, VenueTimeMs: 1788424561000, RateFrac: 0.0000475156961351},
					FundingAtMs:    1788451200000, NextFundingAtMs: 1788480000000,
				})
			},
			want: FundingData{
				Symbol: "BTCUSDT", Source: "okx_futures", VenueTimeMs: 1788424561000,
				Model: FundingDiscrete, RawRate: 0.0000475156961351, RawRateField: "fundingRate",
				RatePerIntervalFrac: 0.0000475156961351, IntervalSec: 28800,
				RatePer8hFrac: 0.0000475156961351, APRFrac: 0.0000475156961351 * 1095,
				// fundingTime, never nextFundingTime — mapping the latter is
				// off by one full period.
				NextFundingAtMs: 1788451200000,
			},
		},
		{
			name: "gate: interval already SECONDS, next_apply epoch seconds to ms (trap ③)",
			build: func() (FundingData, error) {
				return normalizeGateFunding(gateFundingInput{
					fundingReading: fundingReading{Symbol: "BTCUSDT", Source: "gate_futures", RecvAt: recvAt, VenueTimeMs: 0, RateFrac: 0.000038},
					IntervalSec:    28800, FundingNextApplySec: 1788451200,
				})
			},
			want: FundingData{
				Symbol: "BTCUSDT", Source: "gate_futures",
				Model: FundingDiscrete, RawRate: 0.000038, RawRateField: "funding_rate",
				RatePerIntervalFrac: 0.000038, IntervalSec: 28800,
				RatePer8hFrac: 0.000038, APRFrac: 0.000038 * 1095,
				NextFundingAtMs: 1788451200000,
			},
		},
		{
			// Every number in this case is from the live WS frame probed
			// 2026-09-03T08:36Z: relative_funding_rate matched the settled
			// 08:00 hour of /v4/historicalfundingrates, and
			// next_funding_rate_time was the ABSOLUTE stamp of the next round
			// hour — not a remaining-ms countdown (DATA-REQUIREMENTS §3.3⑥).
			name: "kraken: per-1h rate AS-IS ×8 (§3.2②), next_funding_rate_time absolute (§3.3⑥)",
			build: func() (FundingData, error) {
				return normalizeKrakenFunding(krakenFundingInput{
					fundingReading:  fundingReading{Symbol: "BTCUSDT", Source: "kraken_futures", RecvAt: recvAt, VenueTimeMs: 1788424576257, RateFrac: 1.625386637931e-05},
					NextFundingAtMs: 1788426000000,
				})
			},
			want: FundingData{
				Symbol: "BTCUSDT", Source: "kraken_futures", VenueTimeMs: 1788424576257,
				Model: FundingDiscrete, RawRate: 1.625386637931e-05, RawRateField: "relative_funding_rate",
				RatePerIntervalFrac: 1.625386637931e-05, IntervalSec: 3600,
				RatePer8hFrac: 1.625386637931e-05 * 8, APRFrac: 1.625386637931e-05 * 8760,
				NextFundingAtMs: 1788426000000,
			},
		},
		{
			name: "hyperliquid: per-1h rate (already ÷8 by the venue), ×8 for the 8h window",
			build: func() (FundingData, error) {
				return normalizeHyperliquidFunding(hyperliquidFundingInput{
					fundingReading: fundingReading{Symbol: "BTCUSDT", Source: "hyperliquid_futures", RecvAt: recvAt, VenueTimeMs: 0, RateFrac: 0.0000125},
					IntervalHours:  1, NextFundingAtMs: 1788422400000,
				})
			},
			want: FundingData{
				Symbol: "BTCUSDT", Source: "hyperliquid_futures",
				Model: FundingDiscrete, RawRate: 0.0000125, RawRateField: "funding",
				RatePerIntervalFrac: 0.0000125, IntervalSec: 3600,
				RatePer8hFrac: 0.0001, APRFrac: 0.0000125 * 8760,
				NextFundingAtMs: 1788422400000,
			},
		},
		{
			name: "paradex: continuous — no settlement timestamp, rate quoted per 8h window",
			build: func() (FundingData, error) {
				return normalizeParadexFunding(paradexFundingInput{
					fundingReading: fundingReading{Symbol: "BTCUSDT", Source: "paradex_futures", RecvAt: recvAt, VenueTimeMs: 1788421452060, RateFrac: 0.00009228931609},
					PeriodHours:    8,
				})
			},
			want: FundingData{
				Symbol: "BTCUSDT", Source: "paradex_futures", VenueTimeMs: 1788421452060,
				Model: FundingContinuous, RawRate: 0.00009228931609, RawRateField: "funding_rate",
				RatePerIntervalFrac: 0.00009228931609, IntervalSec: 28800,
				RatePer8hFrac: 0.00009228931609, APRFrac: 0.00009228931609 * 1095,
				NextFundingAtMs: 0,
			},
		},
	}

	for _, c := range cases {
		got, err := c.build()
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if got.Symbol != c.want.Symbol || got.Source != c.want.Source {
			t.Errorf("%s: identity = %s/%s, want %s/%s", c.name, got.Symbol, got.Source, c.want.Symbol, c.want.Source)
		}
		if !got.RecvAt.Equal(recvAt) {
			t.Errorf("%s: RecvAt = %v, want %v", c.name, got.RecvAt, recvAt)
		}
		if got.VenueTimeMs != c.want.VenueTimeMs {
			t.Errorf("%s: VenueTimeMs = %d, want %d", c.name, got.VenueTimeMs, c.want.VenueTimeMs)
		}
		if got.Model != c.want.Model {
			t.Errorf("%s: Model = %q, want %q", c.name, got.Model, c.want.Model)
		}
		if got.RawRateField != c.want.RawRateField {
			t.Errorf("%s: RawRateField = %q, want %q", c.name, got.RawRateField, c.want.RawRateField)
		}
		if got.RawRate != c.want.RawRate {
			t.Errorf("%s: RawRate = %v, want %v", c.name, got.RawRate, c.want.RawRate)
		}
		if got.IntervalSec != c.want.IntervalSec {
			t.Errorf("%s: IntervalSec = %d, want %d", c.name, got.IntervalSec, c.want.IntervalSec)
		}
		if math.Abs(got.RatePerIntervalFrac-c.want.RatePerIntervalFrac) > frac {
			t.Errorf("%s: RatePerIntervalFrac = %v, want %v", c.name, got.RatePerIntervalFrac, c.want.RatePerIntervalFrac)
		}
		if math.Abs(got.RatePer8hFrac-c.want.RatePer8hFrac) > frac {
			t.Errorf("%s: RatePer8hFrac = %v, want %v", c.name, got.RatePer8hFrac, c.want.RatePer8hFrac)
		}
		if math.Abs(got.APRFrac-c.want.APRFrac) > apr {
			t.Errorf("%s: APRFrac = %v, want %v", c.name, got.APRFrac, c.want.APRFrac)
		}
		if got.NextFundingAtMs != c.want.NextFundingAtMs {
			t.Errorf("%s: NextFundingAtMs = %d, want %d", c.name, got.NextFundingAtMs, c.want.NextFundingAtMs)
		}
	}
}

// A non-positive interval cannot be normalized — dividing by it would turn one
// bad message into an Inf/NaN APR that poisons every consumer downstream. Each
// builder must refuse instead of producing a number that looks plausible.
func TestFundingBuilders_RejectNonPositiveIntervals(t *testing.T) {
	at := time.Now()
	cases := []struct {
		name  string
		build func() (FundingData, error)
	}{
		{"binance zero hours", func() (FundingData, error) {
			return normalizeBinanceFunding(binanceFundingInput{
				fundingReading: fundingReading{Symbol: "X", Source: "s", RecvAt: at, VenueTimeMs: 0, RateFrac: 0.0001},
				IntervalHours:  0, NextFundingAtMs: 1,
			})
		}},
		{"binance negative hours", func() (FundingData, error) {
			return normalizeBinanceFunding(binanceFundingInput{
				fundingReading: fundingReading{Symbol: "X", Source: "s", RecvAt: at, VenueTimeMs: 0, RateFrac: 0.0001},
				IntervalHours:  -8, NextFundingAtMs: 1,
			})
		}},
		{"bybit zero minutes", func() (FundingData, error) {
			return normalizeBybitFunding(bybitFundingInput{
				fundingReading: fundingReading{Symbol: "X", Source: "s", RecvAt: at, VenueTimeMs: 0, RateFrac: 0.0001},
				IntervalHours:  0, NextFundingAtMs: 1,
			})
		}},
		{"okx next not after funding time", func() (FundingData, error) {
			return normalizeOKXFunding(okxFundingInput{
				fundingReading: fundingReading{Symbol: "X", Source: "s", RecvAt: at, VenueTimeMs: 0, RateFrac: 0.0001},
				FundingAtMs:    5, NextFundingAtMs: 5,
			})
		}},
		{"okx next before funding time", func() (FundingData, error) {
			return normalizeOKXFunding(okxFundingInput{
				fundingReading: fundingReading{Symbol: "X", Source: "s", RecvAt: at, VenueTimeMs: 0, RateFrac: 0.0001},
				FundingAtMs:    5, NextFundingAtMs: 4,
			})
		}},
		{"okx sub-second spacing truncates to zero", func() (FundingData, error) {
			return normalizeOKXFunding(okxFundingInput{
				fundingReading: fundingReading{Symbol: "X", Source: "s", RecvAt: at, VenueTimeMs: 0, RateFrac: 0.0001},
				FundingAtMs:    5, NextFundingAtMs: 900,
			})
		}},
		{"gate zero seconds", func() (FundingData, error) {
			return normalizeGateFunding(gateFundingInput{
				fundingReading: fundingReading{Symbol: "X", Source: "s", RecvAt: at, VenueTimeMs: 0, RateFrac: 0.0001},
				IntervalSec:    0, FundingNextApplySec: 1,
			})
		}},
		{"hyperliquid zero hours", func() (FundingData, error) {
			return normalizeHyperliquidFunding(hyperliquidFundingInput{
				fundingReading: fundingReading{Symbol: "X", Source: "s", RecvAt: at, VenueTimeMs: 0, RateFrac: 0.0001},
				IntervalHours:  0, NextFundingAtMs: 1,
			})
		}},
		{"paradex zero window", func() (FundingData, error) {
			return normalizeParadexFunding(paradexFundingInput{
				fundingReading: fundingReading{Symbol: "X", Source: "s", RecvAt: at, VenueTimeMs: 0, RateFrac: 0.0001},
				PeriodHours:    0,
			})
		}},
	}
	for _, c := range cases {
		if _, err := c.build(); err == nil {
			t.Errorf("%s: want error, got nil", c.name)
		}
	}
}

// SendFunding must obey the same contract as SendPrice: deliver, or give up
// when the context is cancelled so a connector can never block on shutdown.
func TestSendFunding_GivesUpOnCancelledContext(t *testing.T) {
	ch := make(chan FundingData, 1)
	ctx, cancel := context.WithCancel(context.Background())
	f := Feeds{Ctx: ctx, Funding: ch}

	if ok := f.SendFunding(FundingData{Symbol: "BTCUSDT"}); !ok {
		t.Fatal("SendFunding with buffer space should deliver")
	}
	// Buffer now full and nobody draining: only the cancellation can free it.
	cancel()
	if ok := f.SendFunding(FundingData{Symbol: "ETHUSDT"}); ok {
		t.Fatal("SendFunding after cancel with a full buffer should give up")
	}
}
