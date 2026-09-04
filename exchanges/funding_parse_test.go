package exchanges

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Golden tests for funding collection (step 2.5): every venue's REAL handler
// replayed against payloads the venue actually sent, recorded 2026-09-04.
//
// What they assert is what the rest of the system cannot check for itself:
// the reading is identified (symbol from the configured set, source from
// configuration), it is stamped with OUR receive time and never our clock in
// the venue's field, its interval is the venue's own rather than an assumed
// 8h, and the comparison figures are the per-interval rate scaled by exactly
// the interval — the arithmetic that makes seven venues comparable.

func (r *recorder) fundings() []FundingData {
	var out []FundingData
	for {
		select {
		case data := <-r.fundingChan:
			out = append(out, data)
		default:
			return out
		}
	}
}

// fundingExpectation is what one venue's recording is known to contain.
type fundingExpectation struct {
	// IntervalSec is the venue's real settlement (or quote-window) length.
	// Pinned per venue because assuming 8h everywhere is the single error this
	// whole phase exists to prevent: Kraken and Hyperliquid settle hourly.
	IntervalSec int64
	Model       FundingModel
	// VenueClock says the funding payload carries the venue's own timestamp.
	// False means VenueTimeMs must stay 0 — filling it with our clock would
	// report our time as theirs (CLAUDE.md rule 13).
	VenueClock bool
	// NextFundingStamp says the payload states when the next settlement is.
	// A continuous venue never does; Hyperliquid's comes from REST, which is
	// out of this replay's reach.
	NextFundingStamp bool
	// RawRateField is the venue field the number was read from, which is how a
	// dashboard number is traced back to a message.
	RawRateField string
	// Estimated says the venue's published rate is still forming. Kraken is
	// the one venue whose WS field is a SETTLED figure (its docs put the
	// forming estimate in a separate relative_funding_rate_prediction field,
	// and the §3.3⑥ probe saw the WS value match the already-settled hour);
	// Gate's was probed drifting mid-period 2026-09-04. Pinned here because
	// mislabeling either direction misleads a phase-3 consumer that filters
	// on finality.
	Estimated bool
}

var fundingExpectations = map[string]fundingExpectation{
	"bybit_futures": {
		IntervalSec: 8 * 3600, Model: FundingDiscrete, VenueClock: true,
		NextFundingStamp: true, RawRateField: "fundingRate", Estimated: true,
	},
	"okx_futures": {
		IntervalSec: 8 * 3600, Model: FundingDiscrete, VenueClock: true,
		NextFundingStamp: true, RawRateField: "fundingRate", Estimated: true,
	},
	"gate_futures": {
		IntervalSec: 8 * 3600, Model: FundingDiscrete, VenueClock: true,
		NextFundingStamp: true, RawRateField: "funding_rate", Estimated: true,
	},
	// Hourly, not 8-hourly. Annualizing this venue as 8h is wrong by 8×.
	// And its WS rate is the settled figure of the last completed hour —
	// see the Estimated field's comment.
	"kraken_futures": {
		IntervalSec: 3600, Model: FundingDiscrete, VenueClock: true,
		NextFundingStamp: true, RawRateField: "relative_funding_rate", Estimated: false,
	},
	// Continuous accrual: there is no settlement instant to publish.
	"paradex_futures": {
		IntervalSec: 8 * 3600, Model: FundingContinuous, VenueClock: true,
		NextFundingStamp: false, RawRateField: "funding_rate", Estimated: true,
	},
	// Hourly, and its WS payload carries neither a venue clock nor a
	// settlement stamp — both come from predictedFundings over REST, so the
	// replay supplies them the way production's meta cache does.
	"hyperliquid_futures": {
		IntervalSec: 3600, Model: FundingDiscrete, VenueClock: false,
		NextFundingStamp: true, RawRateField: "funding", Estimated: true,
	},
}

// replayFunding pushes a golden file through the venue's production handler,
// with the funding metadata production would have fetched over REST.
func replayFunding(t *testing.T, source string) (*recorder, time.Time) {
	t.Helper()

	r := newRecorder(t)
	recvAt := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)

	streams := captureStreams(r.feeds)
	if source == "hyperliquid_futures" {
		// Production fills this from predictedFundings; the recording holds
		// only the WebSocket side. The value is the one the live endpoint
		// returned on 2026-09-04 (HlPerp: fundingIntervalHours 1), asserted
		// against the real response by TestParseHyperliquidPredictedFundings.
		// The stamp is the CURRENT period's boundary, as the venue publishes
		// it: the connector adds one interval to reach the upcoming one. Half
		// an interval back, so the corrected value lands half an interval
		// ahead of the receive time — the ordinary case in production.
		currentPeriodMs := recvAt.Add(-30 * time.Minute).UnixMilli()
		meta := newFundingMetaCache()
		meta.put(map[string]fundingMetaEntry{
			"BTCUSDT": {IntervalHours: 1, NextFundingAtMs: currentPeriodMs},
			"ETHUSDT": {IntervalHours: 1, NextFundingAtMs: currentPeriodMs},
		})
		streams[source] = hyperliquidStream(source, captureSymbols[source], r.feeds, meta)
	}
	handle := streams[source].Handle
	if handle == nil {
		t.Fatalf("no stream config for %s", source)
	}
	for _, frame := range readFrames(t, source) {
		handle(frame, recvAt)
	}
	return r, recvAt
}

// Every WebSocket-sourced venue publishes funding that honours the contract.
func TestGoldenFunding_EveryVenueHonoursTheContract(t *testing.T) {
	for source, want := range fundingExpectations {
		t.Run(source, func(t *testing.T) {
			r, recvAt := replayFunding(t, source)
			readings := r.fundings()
			if len(readings) == 0 {
				t.Fatalf("%s published no funding from its recording — re-record with %s=1", source, captureEnv)
			}

			configured := map[string]bool{}
			for _, s := range captureSymbols[source] {
				configured[s.Standard] = true
			}

			for _, got := range readings {
				if !configured[got.Symbol] {
					t.Errorf("symbol %q is not one this connector subscribed to", got.Symbol)
				}
				if got.Source != source {
					t.Errorf("Source = %q, want the configured %q", got.Source, source)
				}
				if !got.RecvAt.Equal(recvAt) {
					t.Errorf("RecvAt = %v, want the stamp taken at the socket read %v", got.RecvAt, recvAt)
				}
				if got.Model != want.Model {
					t.Errorf("Model = %q, want %q", got.Model, want.Model)
				}
				if got.RawRateField != want.RawRateField {
					t.Errorf("RawRateField = %q, want %q", got.RawRateField, want.RawRateField)
				}
				if got.IntervalSec != want.IntervalSec {
					t.Errorf("IntervalSec = %d, want the venue's real %d", got.IntervalSec, want.IntervalSec)
				}
				if got.IsEstimated != want.Estimated {
					t.Errorf("IsEstimated = %v, want %v — mislabeled finality misleads a consumer filtering on it", got.IsEstimated, want.Estimated)
				}
				if want.VenueClock == (got.VenueTimeMs == 0) {
					t.Errorf("VenueTimeMs = %d but VenueClock = %v", got.VenueTimeMs, want.VenueClock)
				}
				// A venue clock must never be our clock: that is what makes a
				// dead feed look current forever.
				if got.VenueTimeMs == recvAt.UnixMilli() {
					t.Error("VenueTimeMs equals our receive stamp — the local clock leaked into the venue's field")
				}
				if want.NextFundingStamp && got.NextFundingAtMs == 0 {
					t.Error("NextFundingAtMs = 0 but this venue publishes a settlement stamp")
				}
				if !want.NextFundingStamp && got.NextFundingAtMs != 0 {
					t.Errorf("NextFundingAtMs = %d but this venue publishes none", got.NextFundingAtMs)
				}
				if got.RawRate != got.RatePerIntervalFrac {
					t.Errorf("RawRate %v and RatePerIntervalFrac %v differ; no venue here needs a rate conversion",
						got.RawRate, got.RatePerIntervalFrac)
				}
				// The comparison arithmetic, re-derived: scaling by anything
				// but the real interval is how a 1h venue is annualized as 8h.
				wantPer8h := got.RatePerIntervalFrac * 28800 / float64(got.IntervalSec)
				wantAPR := got.RatePerIntervalFrac * 31_536_000 / float64(got.IntervalSec)
				if math.Abs(got.RatePer8hFrac-wantPer8h) > 1e-15 {
					t.Errorf("RatePer8hFrac = %v, want %v", got.RatePer8hFrac, wantPer8h)
				}
				if math.Abs(got.APRFrac-wantAPR) > 1e-12 {
					t.Errorf("APRFrac = %v, want %v", got.APRFrac, wantAPR)
				}
			}
		})
	}
}

// Bybit's ticker is snapshot+delta and an absent field means UNCHANGED. This
// is the trap of the step, so it is asserted directly on the recording: the
// deltas that follow a snapshot must never blank the funding rate.
func TestGoldenFunding_BybitDeltaNeverBlanksTheRate(t *testing.T) {
	r, _ := replayFunding(t, "bybit_futures")
	readings := r.fundings()
	if len(readings) < 2 {
		t.Fatalf("recording produced %d readings; the snapshot+delta merge needs several", len(readings))
	}
	for _, got := range readings {
		if got.RawRate == 0 {
			t.Fatal("a funding rate came out 0 — a delta overwrote the merged state instead of updating it")
		}
		if got.IntervalSec != 8*3600 {
			t.Fatalf("IntervalSec = %d; fundingIntervalHour arrives only on the SNAPSHOT, so a delta lost it",
				got.IntervalSec)
		}
	}
}

// The merge itself, exercised on the two message shapes the venue sends: only
// fields a message CONTAINS may change the cached state.
func TestBybitTickerMerge_AbsentMeansUnchanged(t *testing.T) {
	snapshot := []byte(`{"topic":"tickers.BTCUSDT","type":"snapshot","ts":1788487585984,"data":{` +
		`"symbol":"BTCUSDT","fundingRate":"0.00005908","nextFundingTime":"1788508800000",` +
		`"fundingIntervalHour":"8","fundingCap":"0.00333","markPrice":"80808.79","indexPrice":"80843.27"}}`)
	// A real delta, measured 2026-09-04: no funding fields at all.
	delta := []byte(`{"topic":"tickers.BTCUSDT","type":"delta","ts":1788487586083,"data":{` +
		`"symbol":"BTCUSDT","markPrice":"80808.82","ask1Price":"80807.00"}}`)

	// A delta that DOES move the rate, which must publish the merged view:
	// the new rate, with the interval and settlement stamp only the snapshot
	// ever carried.
	rateDelta := []byte(`{"topic":"tickers.BTCUSDT","type":"delta","ts":1788487587000,"data":{` +
		`"symbol":"BTCUSDT","fundingRate":"0.00006100"}}`)

	r := newRecorder(t)
	tickers := map[string]*bybitTicker{}
	symbols := []Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}}
	recvAt := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)

	handleBybitTicker("bybit_futures", symbols, tickers, r.feeds, snapshot, recvAt)
	handleBybitTicker("bybit_futures", symbols, tickers, r.feeds, delta, recvAt)

	// The price-only delta must publish NOTHING. Republishing on it would
	// refresh RecvAt roughly ten times a second per market, which is what
	// makes a dead funding subscription look permanently fresh.
	readings := r.fundings()
	if len(readings) != 1 {
		t.Fatalf("got %d readings from a snapshot plus a price-only delta, want only the snapshot's", len(readings))
	}
	if readings[0].RawRate != 0.00005908 {
		t.Errorf("snapshot RawRate = %v, want 0.00005908", readings[0].RawRate)
	}

	handleBybitTicker("bybit_futures", symbols, tickers, r.feeds, rateDelta, recvAt)
	readings = r.fundings()
	if len(readings) != 1 {
		t.Fatalf("a delta carrying a NEW rate produced %d readings, want 1", len(readings))
	}
	after := readings[0]
	if after.RawRate != 0.00006100 {
		t.Errorf("RawRate = %v, want the delta's 0.00006100", after.RawRate)
	}
	// These came from the snapshot and must survive into a reading published
	// by a delta that did not repeat them — the whole point of merging.
	if after.IntervalSec != 8*3600 {
		t.Errorf("IntervalSec = %d, want the snapshot's 28800", after.IntervalSec)
	}
	if after.NextFundingAtMs != 1788508800000 {
		t.Errorf("NextFundingAtMs = %d, want the snapshot's", after.NextFundingAtMs)
	}
	if after.RateCapFrac != 0.00333 || !after.HasCap {
		t.Errorf("cap = %v (has %v), want the snapshot's 0.00333", after.RateCapFrac, after.HasCap)
	}
	// Bybit sends no floor at all, so claiming one would say funding here can
	// never go negative.
	if after.HasFloor {
		t.Errorf("HasFloor = true, but Bybit publishes no floor (RateFloorFrac %v)", after.RateFloorFrac)
	}
	// The price-only delta's mark price was merged even though it published
	// nothing, so it shows up here.
	if after.MarkPrice != 80808.82 {
		t.Errorf("MarkPrice = %v, want the price-only delta's 80808.82", after.MarkPrice)
	}
	if after.VenueTimeMs != 1788487587000 {
		t.Errorf("VenueTimeMs = %d, want the publishing frame's own stamp", after.VenueTimeMs)
	}
}

// A rate with no interval is not a reading: publishing one would mean dividing
// by an interval nobody supplied. Hyperliquid is the venue where this happens
// in production, while its REST metadata is still being fetched.
func TestHyperliquidFunding_NoMetaNoReading(t *testing.T) {
	frame := []byte(`{"channel":"activeAssetCtx","data":{"coin":"BTC","ctx":{` +
		`"funding":"0.0000105002","markPx":"80808.0","oraclePx":"80842.7"}}}`)
	symbols := []Symbol{{Standard: "BTCUSDT", Venue: "BTC"}}
	recvAt := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)

	r := newRecorder(t)
	empty := newFundingMetaCache()
	if !handleHyperliquidFunding("hyperliquid_futures", symbols, empty, r.feeds, frame, recvAt) {
		t.Fatal("the frame should be recognised as activeAssetCtx even with no metadata")
	}
	if got := r.fundings(); len(got) != 0 {
		t.Fatalf("published %d readings with no interval known: %+v", len(got), got)
	}

	// With the metadata, the same frame becomes a reading — and the settlement
	// stamp is the venue's field PLUS one interval, because predictedFundings
	// publishes the period already running (measured across an hour boundary
	// 2026-09-04, see hyperliquid_funding_ws.go).
	currentPeriodMs := recvAt.Add(-30 * time.Minute).UnixMilli()
	meta := newFundingMetaCache()
	meta.put(map[string]fundingMetaEntry{"BTCUSDT": {IntervalHours: 1, NextFundingAtMs: currentPeriodMs}})
	handleHyperliquidFunding("hyperliquid_futures", symbols, meta, r.feeds, frame, recvAt)
	got := r.fundings()
	if len(got) != 1 || got[0].IntervalSec != 3600 {
		t.Fatalf("with metadata, got %+v; want one hourly reading", got)
	}
	wantStamp := currentPeriodMs + 3600*1000
	if got[0].NextFundingAtMs != wantStamp {
		t.Errorf("NextFundingAtMs = %d, want the venue's stamp plus one interval %d — taking it at face value publishes a settlement that already happened",
			got[0].NextFundingAtMs, wantStamp)
	}

	// And a stamp so old that even the correction leaves it in the past must
	// be dropped rather than published.
	stale := newFundingMetaCache()
	stale.put(map[string]fundingMetaEntry{"BTCUSDT": {IntervalHours: 1, NextFundingAtMs: recvAt.Add(-3 * time.Hour).UnixMilli()}})
	handleHyperliquidFunding("hyperliquid_futures", symbols, stale, r.feeds, frame, recvAt)
	got = r.fundings()
	if len(got) != 1 || got[0].NextFundingAtMs != 0 {
		t.Fatalf("a stamp still in the past after correction must be dropped, got %+v", got)
	}
}

// A settlement stamp that has already passed is dropped rather than published:
// Hyperliquid settles hourly while its metadata refreshes every few minutes,
// so a cached stamp goes stale between refreshes.
func TestFutureStampMs_DropsThePast(t *testing.T) {
	now := time.Date(2026, 9, 4, 9, 30, 0, 0, time.UTC)
	future := now.Add(30 * time.Minute).UnixMilli()
	past := now.Add(-1 * time.Minute).UnixMilli()

	if got := futureStampMs(future, now); got != future {
		t.Errorf("a stamp still ahead must survive: got %d, want %d", got, future)
	}
	if got := futureStampMs(past, now); got != 0 {
		t.Errorf("a stamp already passed must become 0 (not supplied), got %d", got)
	}
	if got := futureStampMs(0, now); got != 0 {
		t.Errorf("an absent stamp stays absent, got %d", got)
	}
}

// Binance funding comes from REST, so its golden fixture is a REST response.
func TestParseBinanceFunding_Golden(t *testing.T) {
	var premium []binancePremiumIndex
	loadInstrumentTestdata(t, "funding_binance_premium_index.json", &premium)
	var infos []binanceFundingInfo
	loadInstrumentTestdata(t, "funding_binance_info.json", &infos)

	symbols := []Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}, {Standard: "ETHUSDT", Venue: "ETHUSDT"}}
	meta := newFundingMetaCache()
	meta.put(binanceFundingMeta(infos, symbols))

	r := newRecorder(t)
	recvAt := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	if err := publishBinanceFunding(r.feeds, "binance_futures", symbols, meta, premium, recvAt); err != nil {
		t.Fatal(err)
	}

	readings := r.fundings()
	if len(readings) != 2 {
		t.Fatalf("got %d readings, want one per configured symbol", len(readings))
	}
	for _, got := range readings {
		if got.Source != "binance_futures" || got.RawRateField != "lastFundingRate" {
			t.Errorf("identity = %+v", got)
		}
		if got.IntervalSec != 8*3600 && got.IntervalSec != 4*3600 && got.IntervalSec != 3600 {
			t.Errorf("%s: IntervalSec = %d, want one of the intervals Binance actually runs (8h/4h/1h)",
				got.Symbol, got.IntervalSec)
		}
		if got.NextFundingAtMs == 0 {
			t.Errorf("%s: premiumIndex always carries nextFundingTime", got.Symbol)
		}
		if got.MarkPrice <= 0 || got.IndexPrice <= 0 {
			t.Errorf("%s: mark %v index %v, both are in this payload", got.Symbol, got.MarkPrice, got.IndexPrice)
		}
		if !got.HasCap {
			t.Errorf("%s: fundingInfo carries the cap and floor", got.Symbol)
		}
		if got.VenueTimeMs == 0 {
			t.Errorf("%s: premiumIndex carries the venue's own time", got.Symbol)
		}
	}
}

// A symbol fundingInfo omits must default to 8h, never inherit another
// symbol's interval or come out zero — the endpoint documents itself as
// listing only symbols that DIFFER from the default.
func TestBinanceFundingMeta_DefaultsTheOmitted(t *testing.T) {
	symbols := []Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}, {Standard: "XRPUSDT", Venue: "XRPUSDT"}}
	meta := binanceFundingMeta([]binanceFundingInfo{
		{Symbol: "BTCUSDT", FundingIntervalHours: 4,
			AdjustedFundingRateCap: "0.00300", AdjustedFundingRateFloor: "-0.00300"},
	}, symbols)

	if got := meta["BTCUSDT"]; got.IntervalHours != 4 || !got.HasCap || got.CapFrac != 0.003 {
		t.Errorf("BTCUSDT = %+v, want the endpoint's 4h override with its cap", got)
	}
	got, ok := meta["XRPUSDT"]
	if !ok {
		t.Fatal("a symbol fundingInfo omits must still get an entry — otherwise it never publishes")
	}
	if got.IntervalHours != binanceDefaultFundingIntervalHours {
		t.Errorf("omitted symbol: IntervalHours = %d, want the documented default %d",
			got.IntervalHours, binanceDefaultFundingIntervalHours)
	}
	if got.HasCap {
		t.Error("a symbol with no fundingInfo entry has no published cap")
	}
}

// Hyperliquid's predictedFundings lists competitors beside itself. Reading the
// wrong row would annualize an 8h venue's cadence onto an hourly one.
func TestParseHyperliquidPredictedFundings(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "funding_hyperliquid_predicted.json"))
	if err != nil {
		t.Fatalf("missing recording: %v (re-record with %s=1)", err, captureEnv)
	}
	var rows [][]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("recording does not decode: %v", err)
	}

	symbols := []Symbol{{Standard: "BTCUSDT", Venue: "BTC"}, {Standard: "ETHUSDT", Venue: "ETH"}}
	meta, err := parseHyperliquidPredictedFundings(rows, symbols)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range symbols {
		entry, ok := meta[s.Standard]
		if !ok {
			t.Fatalf("%s missing from the parsed metadata", s.Standard)
		}
		// HlPerp settles hourly; BinPerp and BybitPerp in the same rows are 8h.
		if entry.IntervalHours != 1 {
			t.Errorf("%s: IntervalHours = %d, want Hyperliquid's own 1 — an 8h value means a competitor's row was read",
				s.Standard, entry.IntervalHours)
		}
		if entry.NextFundingAtMs == 0 {
			t.Errorf("%s: predictedFundings carries nextFundingTime", s.Standard)
		}
	}

	// A payload with only competitors must be an error, not a silent empty map
	// that leaves every symbol without an interval forever.
	onlyOthers := [][]any{{"BTC", []any{[]any{"BinPerp", map[string]any{"fundingIntervalHours": float64(8)}}}}}
	if _, err := parseHyperliquidPredictedFundings(onlyOthers, symbols); err == nil {
		t.Error("a response with no HlPerp row must be reported, not accepted as empty")
	}
}

// OKX's nextFundingRate arrives EMPTY under method=current_period (measured
// 2026-09-04, and the recording carries two such frames). Parsed as 0 it would
// publish "the next period pays nothing" with HasFollowingRate=true — this
// pins the ""-is-absent rule, which no other test asserted.
func TestOKXEmptyNextFundingRateIsAbsentNotZero(t *testing.T) {
	symbols := []Symbol{{Standard: "BTCUSDT", Venue: "BTC-USDT-SWAP"}}

	frame := func(nextFundingRate string) []byte {
		return []byte(`{"arg":{"channel":"funding-rate","instId":"BTC-USDT-SWAP"},"data":[{` +
			`"instId":"BTC-USDT-SWAP","fundingRate":"0.0000663322867090",` +
			`"fundingTime":"1788508800000","nextFundingTime":"1788537600000",` +
			`"nextFundingRate":"` + nextFundingRate + `",` +
			`"minFundingRate":"-0.00375","maxFundingRate":"0.00375",` +
			`"settState":"settled","ts":"1788487507788"}]}`)
	}

	r := newRecorder(t)
	handleOKXFunding("okx_futures", symbols, r.feeds, frame(""), time.Now())
	readings := r.fundings()
	if len(readings) != 1 {
		t.Fatalf("published %d readings, want 1", len(readings))
	}
	if readings[0].HasFollowingRate {
		t.Fatal(`nextFundingRate "" published HasFollowingRate=true — an absent forecast became "pays nothing"`)
	}
	if readings[0].FollowingRateFrac != 0 {
		t.Fatalf("FollowingRateFrac = %g behind a false flag, want the zero value", readings[0].FollowingRateFrac)
	}

	handleOKXFunding("okx_futures", symbols, r.feeds, frame("0.0000512345678901"), time.Now())
	readings = r.fundings()
	if len(readings) != 1 {
		t.Fatalf("published %d readings, want 1", len(readings))
	}
	if !readings[0].HasFollowingRate || readings[0].FollowingRateFrac != 0.0000512345678901 {
		t.Fatalf("real forecast: HasFollowingRate=%v FollowingRateFrac=%g, want true and the parsed value",
			readings[0].HasFollowingRate, readings[0].FollowingRateFrac)
	}
}
