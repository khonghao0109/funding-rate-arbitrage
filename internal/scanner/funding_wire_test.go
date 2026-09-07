package scanner

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
)

// A reading whose venue publishes on a fixed cadence goes stale by AGE, which
// is the ordinary case and the one the price path already handles.
func TestFundingStatus_SilenceMakesAReadingStale(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	data := exchanges.FundingData{
		Model:           exchanges.FundingDiscrete,
		RecvAt:          now.Add(-90 * time.Second),
		NextFundingAtMs: now.Add(2 * time.Hour).UnixMilli(),
	}

	if status, reason := fundingStatus(data, 60*time.Second, now); status != statusStale || reason != staleReasonAge {
		t.Fatalf("90s of silence against a 60s threshold = %q/%q, want stale/age", status, reason)
	}
	if status, _ := fundingStatus(data, 5*time.Minute, now); status != statusLive {
		t.Fatalf("90s of silence against a 5m threshold = %q, want live", status)
	}
}

// The check age cannot do. Bybit republishes only when a funding field moves —
// measured 2026-09-04, nineteen minutes of silence with the socket healthy — so
// its threshold has to be a settlement interval. Without this, a funding
// subscription that died would look live for eight hours.
func TestFundingStatus_APassedSettlementIsStaleHoweverFreshTheMessage(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	data := exchanges.FundingData{
		Model:  exchanges.FundingDiscrete,
		RecvAt: now.Add(-time.Second), // arrived a second ago
		// ...but names a settlement that happened ten minutes ago.
		NextFundingAtMs: now.Add(-10 * time.Minute).UnixMilli(),
	}

	status, reason := fundingStatus(data, time.Hour, now)
	if status != statusStale || reason != staleReasonSettled {
		t.Fatalf("a reading naming a past settlement = %q/%q, want stale/settled", status, reason)
	}
}

// A venue republishes the next period within seconds of settling, not at the
// same instant. Without a grace every venue would flap stale at every boundary.
func TestFundingStatus_TheSettlementBoundaryDoesNotFlap(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	data := exchanges.FundingData{
		Model:           exchanges.FundingDiscrete,
		RecvAt:          now.Add(-time.Second),
		NextFundingAtMs: now.Add(-30 * time.Second).UnixMilli(),
	}

	if status, _ := fundingStatus(data, time.Hour, now); status != statusLive {
		t.Fatalf("30s past settlement = %q, want live; the grace is %s", status, fundingSettledGrace)
	}
}

// Continuous accrual has no settlement instant, so there is nothing to expire.
// Reading Paradex's zero as "settled in 1970" would mark it permanently stale.
func TestFundingStatus_ContinuousAccrualNeverExpires(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	data := exchanges.FundingData{
		Model:           exchanges.FundingContinuous,
		RecvAt:          now.Add(-time.Second),
		NextFundingAtMs: 0,
	}

	if status, reason := fundingStatus(data, time.Minute, now); status != statusLive || reason != "" {
		t.Fatalf("continuous reading = %q/%q, want live", status, reason)
	}
}

// A discrete venue that publishes no settlement stamp leaves the field at 0.
// That is "not supplied", not "settled at the epoch".
func TestFundingStatus_AMissingStampIsNotAPassedSettlement(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	data := exchanges.FundingData{
		Model:           exchanges.FundingDiscrete,
		RecvAt:          now.Add(-time.Second),
		NextFundingAtMs: 0,
	}

	if status, _ := fundingStatus(data, time.Minute, now); status != statusLive {
		t.Fatalf("a discrete reading with no stamp = %q, want live", status)
	}
}

func TestFundingStatus_NothingReceivedIsUnknownNotStale(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	status, reason := fundingStatus(exchanges.FundingData{Model: exchanges.FundingDiscrete}, time.Minute, now)
	if status != statusUnknown || reason != "" {
		t.Fatalf("a reading with no RecvAt = %q/%q, want unknown", status, reason)
	}
}

// The funding threshold is its own measurement and must not be the price one:
// Bybit's is 29,100s against a 10s price threshold, and using the price value
// would mark every one of its funding rows stale within seconds.
func TestFundingStaleAfter_IsTheFundingThresholdNotThePriceOne(t *testing.T) {
	for _, meta := range sourceRegistry {
		if meta.FundingStaleAfterSec == 0 {
			continue // spot or oracle: no funding at all
		}
		want := time.Duration(meta.FundingStaleAfterSec) * time.Second
		if got := fundingStaleAfter(meta.Source); got != want {
			t.Errorf("%s: fundingStaleAfter = %s, want %s", meta.Source, got, want)
		}
		if got := fundingStaleAfter(meta.Source); got == staleAfter(meta.Source) && want != staleAfter(meta.Source) {
			t.Errorf("%s: funding threshold collapsed onto the price threshold", meta.Source)
		}
	}
	// An unknown source must not get zero, which would mark it stale on arrival.
	if got := fundingStaleAfter("nobody_configured_this"); got <= 0 {
		t.Errorf("an unconfigured source got a funding threshold of %s", got)
	}
}

// Every configured perp must carry both funding settings, because the funding
// table renders a row for each of them and a missing threshold is a cell whose
// freshness nothing decides.
func TestSourceRegistry_EveryPerpDeclaresItsFundingFreshness(t *testing.T) {
	perps := 0
	for _, meta := range sourceRegistry {
		if meta.MarketType != "perp" {
			if meta.FundingStaleAfterSec != 0 || meta.FundingPublishMode != "" {
				t.Errorf("%s is %s but carries funding settings", meta.Source, meta.MarketType)
			}
			continue
		}
		perps++
		if meta.FundingStaleAfterSec <= 0 {
			t.Errorf("perp %s has no funding_stale_after_sec", meta.Source)
		}
		if meta.FundingPublishMode != "periodic" && meta.FundingPublishMode != "on_change" {
			t.Errorf("perp %s has funding_publish_mode %q", meta.Source, meta.FundingPublishMode)
		}
	}
	if perps == 0 {
		t.Fatal("no perp sources configured; this test would pass vacuously")
	}
}

func fundingFixture(now time.Time) exchanges.FundingData {
	return exchanges.FundingData{
		Symbol:              "BTCUSDT",
		Source:              "binance_futures",
		Model:               exchanges.FundingDiscrete,
		RawRate:             0.0000123,
		RawRateField:        "lastFundingRate",
		RatePerIntervalFrac: 0.0000123,
		IntervalSec:         28800,
		RatePer8hFrac:       0.0000123,
		APRFrac:             0.0134685,
		NextFundingAtMs:     now.Add(3 * time.Hour).UnixMilli(),
		IsEstimated:         true,
		MarkPrice:           81124.3,
		RecvAt:              now.Add(-2 * time.Second),
	}
}

// The wire speaks bps and percent while Go and SQLite speak fractions. The
// conversion happens in one place; this is the test that pins it, because a
// factor of 100 here is the difference between 13% APR and 0.13%.
func TestNewWireFundingPoint_ConvertsFractionsToTheWireUnits(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	point := newWireFundingPoint(fundingFixture(now), HedgeLeg{}, now)

	if math.Abs(point.RatePer8hBps-0.123) > 1e-9 {
		t.Errorf("rate_per_8h_bps = %g, want 0.123 (0.0000123 as bps)", point.RatePer8hBps)
	}
	if math.Abs(point.APRGrossPct-1.34685) > 1e-9 {
		t.Errorf("apr_gross_pct = %g, want 1.34685", point.APRGrossPct)
	}
	if point.RawRate != 0.0000123 || point.RawRateField != "lastFundingRate" {
		t.Errorf("the raw venue value must travel unconverted, got %g from %q",
			point.RawRate, point.RawRateField)
	}
	if point.AgeMs != 2000 {
		t.Errorf("age_ms = %d, want 2000", point.AgeMs)
	}
}

// An unset bound must stay 0 beside its false flag. Bybit publishes a cap and
// no floor, and a floor of "exactly 0" reads as "funding can never be negative".
func TestNewWireFundingPoint_AnUnsetBoundStaysZero(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	data := fundingFixture(now)
	data.HasCap, data.RateCapFrac = true, 0.005
	data.HasFloor, data.RateFloorFrac = false, -0.005 // a value nobody stated

	point := newWireFundingPoint(data, HedgeLeg{}, now)
	if !point.HasCap || math.Abs(point.RateCapPerIntervalBps-50) > 1e-9 {
		t.Errorf("cap = %g bps (has=%v), want 50", point.RateCapPerIntervalBps, point.HasCap)
	}
	if point.HasFloor || point.RateFloorPerIntervalBps != 0 {
		t.Errorf("floor = %g bps (has=%v), want 0/false", point.RateFloorPerIntervalBps, point.HasFloor)
	}
}

// "The mapping has not been built yet" and "this venue cannot be hedged" are
// different facts. Collapsing them would tell the reader a temporary state is
// permanent.
func TestNewWireFundingPoint_AnUnknownHedgeIsNotARefusedOne(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	unknown := newWireFundingPoint(fundingFixture(now), HedgeLeg{}, now)
	if unknown.HedgeSpotSource != "" || unknown.HedgeNoteVI == "" {
		t.Errorf("an absent mapping entry must say it is unknown, got %+v", unknown.HedgeNoteVI)
	}

	refused := newWireFundingPoint(fundingFixture(now),
		HedgeLeg{NoteVI: "Không ghép được chân spot: quote USD"}, now)
	if refused.HedgeNoteVI != "Không ghép được chân spot: quote USD" {
		t.Errorf("a refusal must keep the venue's own words, got %q", refused.HedgeNoteVI)
	}
}

func TestBreakevenDaysFeesOnly(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	t.Run("no hedge means no number", func(t *testing.T) {
		if got := breakevenDaysFeesOnly(fundingFixture(now), ""); got != nil {
			t.Errorf("breakeven = %v with no spot leg; a trade that cannot be opened has no breakeven", *got)
		}
	})

	t.Run("a negative rate has no breakeven", func(t *testing.T) {
		// Spot long plus perp short RECEIVES when funding is positive. A
		// negative rate is a cost, and a cost never pays itself back.
		data := fundingFixture(now)
		data.RatePerIntervalFrac = -0.0001
		if got := breakevenDaysFeesOnly(data, "binance_spot"); got != nil {
			t.Errorf("breakeven = %v on a rate that costs us money", *got)
		}
	})

	t.Run("an unverified schedule produces no number", func(t *testing.T) {
		markFeeUnverified(t, "bybit_futures")
		data := fundingFixture(now)
		data.Source = "bybit_futures"
		if got := breakevenDaysFeesOnly(data, "binance_spot"); got != nil {
			t.Errorf("breakeven = %v against an unverified fee; unknown is not free", *got)
		}
	})

	t.Run("a real pair produces the fee-only arithmetic", func(t *testing.T) {
		// binance_spot taker 10 bps + binance_futures taker 5 bps, four fills
		// = 0.30% round trip. At 1 bps per 8h the venue settles three times a
		// day, so 0.03%/day and the fees take ten days.
		data := fundingFixture(now)
		data.RatePerIntervalFrac = 0.0001
		data.IntervalSec = 28800

		got := breakevenDaysFeesOnly(data, "binance_spot")
		if got == nil {
			t.Fatal("two verified schedules produced no breakeven")
		}
		if math.Abs(*got-10) > 0.001 {
			t.Errorf("breakeven = %g days, want 10", *got)
		}
	})

	t.Run("the interval is respected, never assumed to be 8h", func(t *testing.T) {
		// The same per-interval rate on an hourly venue pays eight times as
		// often, so it breaks even eight times sooner. Hardcoding 8h here is
		// exactly CLAUDE.md rule 3.
		hourly := fundingFixture(now)
		hourly.RatePerIntervalFrac, hourly.IntervalSec = 0.0001, 3600
		eightly := fundingFixture(now)
		eightly.RatePerIntervalFrac, eightly.IntervalSec = 0.0001, 28800

		fast := breakevenDaysFeesOnly(hourly, "binance_spot")
		slow := breakevenDaysFeesOnly(eightly, "binance_spot")
		if fast == nil || slow == nil {
			t.Fatal("expected a number for both intervals")
		}
		if math.Abs(*slow/(*fast)-8) > 1e-6 {
			t.Errorf("8h breakeven %g is not 8x the 1h breakeven %g", *slow, *fast)
		}
	})
}

// The message keeps stale rows. Dropping them would make a venue whose funding
// subscription died look like a venue that has no funding — the same misreading
// the price contract avoids by keeping stale prices on screen.
func TestNewWireFunding_KeepsStaleReadingsSoTheTableCanShowThem(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	fresh := fundingFixture(now)
	dead := fundingFixture(now)
	dead.Source = "okx_futures"
	dead.RecvAt = now.Add(-2 * time.Hour)

	message := newWireFunding([]exchanges.FundingData{fresh, dead}, nil, now)

	row := message.Funding["BTCUSDT"]
	if len(row) != 2 {
		t.Fatalf("got %d sources, want both the live and the dead one", len(row))
	}
	if row["okx_futures"].Status != statusStale {
		t.Errorf("the silent source is %q, want stale", row["okx_futures"].Status)
	}
	if row["binance_futures"].Status != statusLive {
		t.Errorf("the fresh source is %q, want live", row["binance_futures"].Status)
	}
}

// The contract is what static/app.js reads. Every key it renders has to be
// present with its documented name, including the ones carrying a default.
func TestNewWireFunding_SerializesEveryContractKey(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	message := newWireFunding([]exchanges.FundingData{fundingFixture(now)}, nil, now)

	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Type    string `json:"type"`
		V       int    `json:"v"`
		Now     int64  `json:"server_time_ms"`
		Funding map[string]map[string]struct {
			Model                 string   `json:"model"`
			RatePer8hBps          *float64 `json:"rate_per_8h_bps"`
			RatePerIntervalBps    *float64 `json:"rate_per_interval_bps"`
			IntervalSec           *int64   `json:"interval_sec"`
			APRGrossPct           *float64 `json:"apr_gross_pct"`
			NextFundingAtMs       *int64   `json:"next_funding_at_ms"`
			IsEstimated           *bool    `json:"is_estimated"`
			RecvAtMs              *int64   `json:"recv_at_ms"`
			AgeMs                 *int64   `json:"age_ms"`
			Status                *string  `json:"status"`
			StaleReason           *string  `json:"stale_reason"`
			MarkPrice             *float64 `json:"mark_price"`
			IndexPrice            *float64 `json:"index_price"`
			RateCapPerInterval    *float64 `json:"rate_cap_per_interval_bps"`
			RateFloorPerInterval  *float64 `json:"rate_floor_per_interval_bps"`
			HasCap                *bool    `json:"has_cap"`
			HasFloor              *bool    `json:"has_floor"`
			RateType              *string  `json:"rate_type"`
			RawRate               *float64 `json:"raw_rate"`
			RawRateField          *string  `json:"raw_rate_field"`
			HedgeSpotSource       *string  `json:"hedge_spot_source"`
			HedgeNoteVI           *string  `json:"hedge_note_vi"`
			BreakevenDaysFeesOnly *float64 `json:"breakeven_days_fees_only"` // nullable by design
		} `json:"funding"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != "funding" || decoded.V != wireVersion || decoded.Now != now.UnixMilli() {
		t.Fatalf("envelope = %+v", decoded)
	}

	point, ok := decoded.Funding["BTCUSDT"]["binance_futures"]
	if !ok {
		t.Fatal("the reading is missing from the message")
	}
	for name, present := range map[string]bool{
		"rate_per_8h_bps":             point.RatePer8hBps != nil,
		"rate_per_interval_bps":       point.RatePerIntervalBps != nil,
		"interval_sec":                point.IntervalSec != nil,
		"apr_gross_pct":               point.APRGrossPct != nil,
		"next_funding_at_ms":          point.NextFundingAtMs != nil,
		"is_estimated":                point.IsEstimated != nil,
		"recv_at_ms":                  point.RecvAtMs != nil,
		"age_ms":                      point.AgeMs != nil,
		"status":                      point.Status != nil,
		"stale_reason":                point.StaleReason != nil,
		"mark_price":                  point.MarkPrice != nil,
		"index_price":                 point.IndexPrice != nil,
		"rate_cap_per_interval_bps":   point.RateCapPerInterval != nil,
		"rate_floor_per_interval_bps": point.RateFloorPerInterval != nil,
		"has_cap":                     point.HasCap != nil,
		"has_floor":                   point.HasFloor != nil,
		"rate_type":                   point.RateType != nil,
		"raw_rate":                    point.RawRate != nil,
		"raw_rate_field":              point.RawRateField != nil,
		"hedge_spot_source":           point.HedgeSpotSource != nil,
		"hedge_note_vi":               point.HedgeNoteVI != nil,
	} {
		if !present {
			t.Errorf("contract field %q is missing from the funding message", name)
		}
	}
	if point.Model != "discrete" {
		t.Errorf("model = %q, want discrete", point.Model)
	}
}

// No funding field may ever be called profit, and the basis block has to name
// what has not been taken off. CLAUDE.md rule 2, enforced where it is rendered.
func TestNewWireMeta_FundingIsLabelledGrossAndNamesItsExclusions(t *testing.T) {
	meta := newWireMeta([]string{"BTCUSDT"}, 1)

	if meta.FundingBasis.Model != fundingModelGross {
		t.Errorf("funding_basis.model = %q, want %q", meta.FundingBasis.Model, fundingModelGross)
	}
	if len(meta.FundingBasis.Applied) != 0 {
		t.Errorf("funding_basis.applied = %v; nothing is deducted from a funding figure before step 3.1",
			meta.FundingBasis.Applied)
	}
	for _, want := range []string{"taker_fee", "slippage", "spot_borrow"} {
		found := false
		for _, excluded := range meta.FundingBasis.Excluded {
			if excluded == want {
				found = true
			}
		}
		if !found {
			t.Errorf("funding_basis.excluded does not name %q: %v", want, meta.FundingBasis.Excluded)
		}
	}

	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := generic["funding_basis"]; !ok {
		t.Error("meta carries no funding_basis; the dashboard would render an unlabelled gross figure")
	}
}

// The matrix used to go out on every price tick from every venue: 58 of 60
// frames on a live socket were `spreads` (measured 2026-09-04). Queueing keeps
// the newest picture per symbol and sends it once a tick instead.
func TestQueueSpreads_KeepsOneMatrixPerSymbolAndPrefersTheNewestSnapshot(t *testing.T) {
	scanner := New([]string{"BTCUSDT", "ETHUSDT"})

	scanner.queueSpreads(wireSpreads{Symbol: "BTCUSDT", ServerTimeMs: 1000})
	scanner.queueSpreads(wireSpreads{Symbol: "BTCUSDT", ServerTimeMs: 3000})
	// Delivered last but computed FIRST: two producers publish per symbol from
	// different goroutines, so arrival order is not snapshot order.
	scanner.queueSpreads(wireSpreads{Symbol: "BTCUSDT", ServerTimeMs: 2000})
	scanner.queueSpreads(wireSpreads{Symbol: "ETHUSDT", ServerTimeMs: 1500})

	scanner.pendingMutex.Lock()
	defer scanner.pendingMutex.Unlock()

	if len(scanner.pendingSpreads) != 2 {
		t.Fatalf("queued %d messages, want one per symbol", len(scanner.pendingSpreads))
	}
	if got := scanner.pendingSpreads["BTCUSDT"].ServerTimeMs; got != 3000 {
		t.Errorf("BTCUSDT kept the snapshot at %d, want the newest at 3000", got)
	}
	if got := scanner.pendingSpreads["ETHUSDT"].ServerTimeMs; got != 1500 {
		t.Errorf("ETHUSDT kept %d, want 1500", got)
	}
}
