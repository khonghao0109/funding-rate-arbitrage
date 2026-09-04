package exchanges

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

// Golden tests for the settled-funding parsers (step 2.6), against the real
// responses recorded 2026-09-04 into testdata/funding_history_*.json.
//
// What these are actually guarding is not "does JSON decode". It is the four
// ways a funding history parser produces numbers that are wrong but not
// obviously wrong: reading Kraken's absolute price amount as a rate, reading
// Gate's seconds as milliseconds, measuring Paradex's five-second sampling
// interval as its funding period, and annotating every row with today's
// interval when the venue's cadence changed mid-corpus.

// wideWindow accepts every row in a fixture. Window filtering is tested
// separately, on synthetic stamps, so the fixtures do not have to be re-recorded
// when they age past a fixed bound.
var wideWindow = FundingWindow{StartMs: 0, EndMs: math.MaxInt64}

func btcSymbol(t *testing.T, source string) Symbol {
	t.Helper()
	for _, s := range captureSymbols[source] {
		if s.Standard == "BTCUSDT" {
			return s
		}
	}
	t.Fatalf("%s has no BTCUSDT in captureSymbols", source)
	return Symbol{}
}

// assertHistorySane checks what must hold for every venue, so a per-venue test
// can concentrate on that venue's trap.
func assertHistorySane(t *testing.T, entries []FundingHistoryEntry, source string, model FundingModel) {
	t.Helper()

	if len(entries) == 0 {
		t.Fatalf("%s: no entries parsed from the recording", source)
	}
	var prevMs int64
	for i, entry := range entries {
		switch {
		case entry.Source != source:
			t.Errorf("%s[%d]: Source = %q", source, i, entry.Source)
		case entry.Symbol != "BTCUSDT":
			t.Errorf("%s[%d]: Symbol = %q", source, i, entry.Symbol)
		case entry.Model != model:
			t.Errorf("%s[%d]: Model = %q, want %q", source, i, entry.Model, model)
		case entry.SettledAtMs <= prevMs:
			t.Errorf("%s[%d]: SettledAtMs %d not after %d — entries must be oldest first",
				source, i, entry.SettledAtMs, prevMs)
		case entry.IntervalSec <= 0:
			t.Errorf("%s[%d]: IntervalSec = %d", source, i, entry.IntervalSec)
		case entry.RawRateField == "":
			t.Errorf("%s[%d]: RawRateField is empty — a number nobody can trace back to a payload field",
				source, i)
		}
		// A funding rate outside ±1% for one interval is not a rate: it is a
		// price, an absolute amount, or a unit conversion that went the wrong
		// way. Real caps are an order of magnitude tighter than this.
		if math.Abs(entry.RatePerIntervalFrac) > 0.01 {
			t.Errorf("%s[%d]: RatePerIntervalFrac = %g — implausible for one interval",
				source, i, entry.RatePerIntervalFrac)
		}
		// The comparison figures must be derived from the pair above, not
		// carried over from whatever the venue happened to publish.
		if want := entry.RatePerIntervalFrac * secPer8h / float64(entry.IntervalSec); !closeEnough(entry.RatePer8hFrac, want) {
			t.Errorf("%s[%d]: RatePer8hFrac = %g, want %g", source, i, entry.RatePer8hFrac, want)
		}
		if want := entry.RatePerIntervalFrac * secPerYear / float64(entry.IntervalSec); !closeEnough(entry.APRFrac, want) {
			t.Errorf("%s[%d]: APRFrac = %g, want %g", source, i, entry.APRFrac, want)
		}
		prevMs = entry.SettledAtMs
	}
}

func closeEnough(got, want float64) bool {
	return math.Abs(got-want) <= 1e-12+math.Abs(want)*1e-9
}

func TestBinanceFundingHistoryGolden(t *testing.T) {
	symbol := btcSymbol(t, "binance_futures")
	var raw []binanceFundingHistoryRow
	loadInstrumentTestdata(t, "funding_history_binance.json", &raw)

	rows, err := parseBinanceFundingHistory(raw, symbol, wideWindow)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := finishFundingHistory("binance_futures", symbol, FundingDiscrete, rows)
	if err != nil {
		t.Fatal(err)
	}
	assertHistorySane(t, entries, "binance_futures", FundingDiscrete)

	// rateType is published on this endpoint and nowhere else in the codebase.
	// It is what a phase-3 backtest filters "Special" on, so a parser that
	// dropped it would leave that filter with nothing to read.
	for i, entry := range entries {
		if entry.RateType == "" {
			t.Fatalf("entry %d carries no RateType; the recording has one", i)
		}
		if entry.RawRateField != "fundingRate" {
			t.Errorf("entry %d: RawRateField = %q", i, entry.RawRateField)
		}
		if entry.MarkPrice <= 0 {
			t.Errorf("entry %d: MarkPrice = %g, but this venue publishes one", i, entry.MarkPrice)
		}
	}
}

func TestBybitFundingHistoryGolden(t *testing.T) {
	symbol := btcSymbol(t, "bybit_futures")
	var resp bybitFundingHistoryResponse
	loadInstrumentTestdata(t, "funding_history_bybit.json", &resp)

	rows, oldestMs, err := parseBybitFundingHistory(resp, symbol, wideWindow)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := finishFundingHistory("bybit_futures", symbol, FundingDiscrete, rows)
	if err != nil {
		t.Fatal(err)
	}
	assertHistorySane(t, entries, "bybit_futures", FundingDiscrete)

	// The venue answers newest first; the entries must come back oldest first
	// (assertHistorySane checks the order) and the paging cursor must be the
	// OLDEST stamp seen, or the next request re-reads the page just read.
	if oldestMs != entries[0].SettledAtMs {
		t.Errorf("paging cursor = %d, want the oldest stamp %d", oldestMs, entries[0].SettledAtMs)
	}
}

func TestBybitFundingHistorySeparatesFailureFromAbsence(t *testing.T) {
	symbol := Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"}

	// Bybit reports both failure and absence inside HTTP 200, and they must not
	// be read the same way. A rate-limit or server error read as an empty list
	// would truncate the corpus silently...
	loud := bybitFundingHistoryResponse{RetCode: 10002, RetMsg: "request not supported"}
	if _, _, err := parseBybitFundingHistory(loud, symbol, wideWindow); err == nil {
		t.Error("a venue error was read as an empty history")
	}

	// ...while an unlisted market read as an error would put a permanent hourly
	// failure in the log for a pair that will never exist (step 2.4, XLMUSDT).
	absent := bybitFundingHistoryResponse{RetCode: 10001, RetMsg: "params error: symbol invalid"}
	rows, _, err := parseBybitFundingHistory(absent, symbol, wideWindow)
	if err != nil {
		t.Errorf("an unlisted market was reported as a failure: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows for an unlisted market", len(rows))
	}
}

func TestOKXFundingHistorySeparatesFailureFromAbsence(t *testing.T) {
	symbol := Symbol{Standard: "BTCUSDT", Venue: "BTC-USDT-SWAP"}

	// 51001 is the one OKX code that means "instrument does not exist".
	rows, _, err := parseOKXFundingHistory(okxFundingHistoryResponse{
		Code: "51001", Msg: "Instrument ID does not exist",
	}, symbol, wideWindow)
	if err != nil || len(rows) != 0 {
		t.Errorf("51001 = %d rows, %v; want absent and no error", len(rows), err)
	}
	// Everything else stays loud: "system busy" read as absence would truncate
	// the corpus without a word.
	if _, _, err := parseOKXFundingHistory(okxFundingHistoryResponse{
		Code: "50011", Msg: "Requests too frequent",
	}, symbol, wideWindow); err == nil {
		t.Error("a rate-limit response was read as an empty history")
	}
}

func TestOKXFundingHistoryGolden(t *testing.T) {
	symbol := btcSymbol(t, "okx_futures")
	var resp okxFundingHistoryResponse
	loadInstrumentTestdata(t, "funding_history_okx.json", &resp)

	rows, oldestMs, err := parseOKXFundingHistory(resp, symbol, wideWindow)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := finishFundingHistory("okx_futures", symbol, FundingDiscrete, rows)
	if err != nil {
		t.Fatal(err)
	}
	assertHistorySane(t, entries, "okx_futures", FundingDiscrete)

	if oldestMs != entries[0].SettledAtMs {
		t.Errorf("paging cursor = %d, want the oldest stamp %d", oldestMs, entries[0].SettledAtMs)
	}
	// realizedRate is the rate actually charged and is what the recording
	// carries; falling back to fundingRate is for rows that state no realized
	// one, not the normal path.
	for i, entry := range entries {
		if entry.RawRateField != "realizedRate" {
			t.Errorf("entry %d: RawRateField = %q, want realizedRate", i, entry.RawRateField)
		}
	}
}

func TestOKXFundingHistoryFallsBackToFundingRate(t *testing.T) {
	// An empty realizedRate must never parse as 0 — that would record a
	// settlement that paid nothing.
	var resp okxFundingHistoryResponse
	resp.Code = "0"
	resp.Data = append(resp.Data, struct {
		InstID       string `json:"instId"`
		FundingRate  string `json:"fundingRate"`
		RealizedRate string `json:"realizedRate"`
		FundingTime  string `json:"fundingTime"`
		Method       string `json:"method"`
	}{InstID: "BTC-USDT-SWAP", FundingRate: "0.0001", RealizedRate: "", FundingTime: "1788480000000"})

	rows, _, err := parseOKXFundingHistory(resp, Symbol{Standard: "BTCUSDT", Venue: "BTC-USDT-SWAP"}, wideWindow)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RawRateField != "fundingRate" || rows[0].RateFrac != 0.0001 {
		t.Fatalf("got %+v, want one row from fundingRate = 0.0001", rows)
	}
}

func TestGateFundingHistoryGolden(t *testing.T) {
	symbol := btcSymbol(t, "gate_futures")
	var raw []gateFundingHistoryRow
	loadInstrumentTestdata(t, "funding_history_gate.json", &raw)

	rows, oldestSec, err := parseGateFundingHistory(raw, symbol, wideWindow)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := finishFundingHistory("gate_futures", symbol, FundingDiscrete, rows)
	if err != nil {
		t.Fatal(err)
	}
	assertHistorySane(t, entries, "gate_futures", FundingDiscrete)

	if oldestSec*msPerSecond != entries[0].SettledAtMs {
		t.Errorf("paging cursor = %ds, want the oldest stamp %dms", oldestSec, entries[0].SettledAtMs)
	}
	// This venue stamps SECONDS. A parser that stored them as milliseconds
	// would date every settlement to January 1970 and the corpus would look
	// empty for the last fifty years rather than wrong.
	newest := entries[len(entries)-1]
	if newest.SettledAtMs < 1_500_000_000_000 {
		t.Fatalf("newest SettledAtMs = %d — seconds were not converted to milliseconds", newest.SettledAtMs)
	}
	// And they are not on the hour: the recording has settlements one to three
	// seconds past the boundary. Rounding them would invent a stamp the venue
	// never published, and the primary key would stop matching on a re-fetch.
	if newest.SettledAtMs%(secPerHour*msPerSecond) == 0 {
		t.Errorf("SettledAtMs %d landed exactly on the hour; the recording's stamps do not", newest.SettledAtMs)
	}
}

func TestKrakenFundingHistoryGolden(t *testing.T) {
	symbol := btcSymbol(t, "kraken_futures")
	var resp krakenFundingHistoryResponse
	loadInstrumentTestdata(t, "funding_history_kraken.json", &resp)

	rows, err := parseKrakenFundingHistory(resp, symbol, wideWindow)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := finishFundingHistory("kraken_futures", symbol, FundingDiscrete, rows)
	if err != nil {
		t.Fatal(err)
	}
	assertHistorySane(t, entries, "kraken_futures", FundingDiscrete)

	for i, entry := range entries {
		if entry.RawRateField != "relativeFundingRate" {
			t.Fatalf("entry %d reads %q; fundingRate on this venue is an absolute price amount",
				i, entry.RawRateField)
		}
	}

	// THE step-2.3 obligation. kraken_funding.go pins the hourly cadence as a
	// constant because no Kraken funding message carries an interval, and the
	// standing condition on that constant was that every backfill re-measure
	// it. This is the measurement: re-record the fixture and this test says
	// whether the constant is still true.
	report := FundingGaps(entries)
	if report.ModalGapSec != secPerHour {
		t.Fatalf("Kraken settles every %ds in the recording, but kraken_funding.go pins %ds — "+
			"the venue changed cadence; gaps observed: %v",
			report.ModalGapSec, secPerHour, report.Counts)
	}
	// The rate itself must be the relative one. The absolute field in the same
	// rows is order 1 (a price amount), so this bound separates them by five
	// orders of magnitude rather than by a field name alone.
	for i, entry := range entries {
		if entry.RatePerIntervalFrac != 0 && math.Abs(entry.RatePerIntervalFrac) > 0.001 {
			t.Errorf("entry %d: rate %g looks like the absolute funding amount, not the relative rate",
				i, entry.RatePerIntervalFrac)
		}
	}
}

func TestKrakenFundingHistoryRefusesMissingRelativeRate(t *testing.T) {
	var resp krakenFundingHistoryResponse
	resp.Result = "success"
	resp.Rates = append(resp.Rates, struct {
		Timestamp           string   `json:"timestamp"`
		FundingRate         float64  `json:"fundingRate"`
		RelativeFundingRate *float64 `json:"relativeFundingRate"`
	}{Timestamp: "2026-09-04T03:00:00Z", FundingRate: 1.81, RelativeFundingRate: nil})

	if _, err := parseKrakenFundingHistory(resp, Symbol{Standard: "BTCUSDT", Venue: "PF_XBTUSD"}, wideWindow); err == nil {
		t.Fatal("a row with no relativeFundingRate was accepted; fundingRate would have been used as a rate")
	}
}

func TestHyperliquidFundingHistoryGolden(t *testing.T) {
	symbol := btcSymbol(t, "hyperliquid_futures")
	var raw []hyperliquidFundingHistoryRow
	loadInstrumentTestdata(t, "funding_history_hyperliquid.json", &raw)

	rows, err := parseHyperliquidFundingHistory(raw, symbol, wideWindow)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := finishFundingHistory("hyperliquid_futures", symbol, FundingDiscrete, rows)
	if err != nil {
		t.Fatal(err)
	}
	assertHistorySane(t, entries, "hyperliquid_futures", FundingDiscrete)

	// Hourly, measured — not assumed. The stamps carry millisecond jitter, so
	// this also proves roundedGapSec absorbs it: truncation would produce a
	// mix of 3599 and 3600 and no single cadence.
	report := FundingGaps(entries)
	if report.ModalGapSec != secPerHour {
		t.Fatalf("modal gap = %ds, want %ds (hourly); gaps: %v", report.ModalGapSec, secPerHour, report.Counts)
	}
	for i, entry := range entries {
		if entry.IntervalSec != secPerHour {
			t.Errorf("entry %d: IntervalSec = %d, want %d", i, entry.IntervalSec, secPerHour)
		}
	}
	// The jitter itself must survive into the stored stamp: rounding it to the
	// hour would be inventing a timestamp, and a re-fetch would then miss the
	// primary key and insert a duplicate settlement.
	jittered := false
	for _, entry := range entries {
		if entry.SettledAtMs%(secPerHour*msPerSecond) != 0 {
			jittered = true
		}
	}
	if !jittered {
		t.Error("every stamp landed exactly on the hour; the recording's do not")
	}
}

func TestParadexFundingHistoryGolden(t *testing.T) {
	symbol := btcSymbol(t, "paradex_futures")
	var resp paradexFundingHistoryResponse
	loadInstrumentTestdata(t, "funding_history_paradex.json", &resp)

	rows, err := parseParadexFundingHistory(resp, symbol, wideWindow)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := finishFundingHistory("paradex_futures", symbol, FundingContinuous, rows)
	if err != nil {
		t.Fatal(err)
	}
	assertHistorySane(t, entries, "paradex_futures", FundingContinuous)

	// The venue declares the funding period per row, and that declaration is
	// the only thing standing between this and a catastrophic measurement: the
	// samples are five seconds apart, so measuring the interval from their
	// spacing would report a 5-second funding period and an APR 5,760× too
	// large.
	for i, entry := range entries {
		if entry.IntervalSec != 8*secPerHour {
			t.Errorf("entry %d: IntervalSec = %d, want %d from funding_period_hours",
				i, entry.IntervalSec, 8*secPerHour)
		}
		if entry.Model != FundingContinuous {
			t.Errorf("entry %d: Model = %q — this venue has no settlements", i, entry.Model)
		}
	}
}

func TestParadexFundingHistoryRefusesMissingPeriod(t *testing.T) {
	var resp paradexFundingHistoryResponse
	resp.Results = append(resp.Results, struct {
		Market             string `json:"market"`
		FundingRate        string `json:"funding_rate"`
		FundingRate8h      string `json:"funding_rate_8h"`
		FundingPeriodHours int64  `json:"funding_period_hours"`
		FundingIndex       string `json:"funding_index"`
		CreatedAt          int64  `json:"created_at"`
	}{Market: "BTC-USD-PERP", FundingRate: "0.00008", FundingPeriodHours: 0, CreatedAt: 1788492012060})

	if _, err := parseParadexFundingHistory(resp, Symbol{Standard: "BTCUSDT", Venue: "BTC-USD-PERP"}, wideWindow); err == nil {
		t.Fatal("a row with no funding_period_hours was accepted; the 5s sample spacing would become the interval")
	}
}

func TestFundingWindowFiltersRows(t *testing.T) {
	symbol := Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"}
	raw := []binanceFundingHistoryRow{
		{Symbol: "BTCUSDT", FundingTime: 1000, FundingRate: "0.0001", MarkPrice: "1", RateType: "Regular"},
		{Symbol: "BTCUSDT", FundingTime: 2000, FundingRate: "0.0001", MarkPrice: "1", RateType: "Regular"},
		{Symbol: "BTCUSDT", FundingTime: 3000, FundingRate: "0.0001", MarkPrice: "1", RateType: "Regular"},
		{Symbol: "ETHUSDT", FundingTime: 2500, FundingRate: "0.0001", MarkPrice: "1", RateType: "Regular"},
	}
	rows, err := parseBinanceFundingHistory(raw, symbol, FundingWindow{StartMs: 2000, EndMs: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SettledAtMs != 2000 {
		t.Fatalf("got %+v, want the single row at 2000 — the window is closed-open and another symbol's row is not ours", rows)
	}
}

func TestModalGapSec(t *testing.T) {
	for _, tc := range []struct {
		name string
		gaps []int64
		want int64
	}{
		{"hourly with two outages", []int64{3600, 3600, 7200, 3600, 3600, 10800}, 3600},
		{"eight hourly", []int64{28800, 28800, 28800}, 28800},
		{"a tie goes to the smaller gap, which an outage cannot manufacture",
			[]int64{3600, 7200}, 3600},
		{"zeros are not a cadence", []int64{0, 0}, 0},
		{"nothing to measure", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := modalGapSec(tc.gaps); got != tc.want {
				t.Errorf("modalGapSec(%v) = %d, want %d", tc.gaps, got, tc.want)
			}
		})
	}
}

func TestRoundedGapSecAbsorbsVenueJitter(t *testing.T) {
	// The two consecutive Hyperliquid stamps that make truncation fail.
	if got := roundedGapSec(1788480000030, 1788483600062); got != 3600 {
		t.Errorf("gap = %d, want 3600", got)
	}
	if got := roundedGapSec(1788483600062, 1788487200002); got != 3600 {
		t.Errorf("gap = %d, want 3600", got)
	}
}

func TestFinishFundingHistoryDeduplicatesAndOrders(t *testing.T) {
	symbol := Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"}
	// Overlapping pages: the same settlement arrives twice, out of order.
	rows := []fundingHistoryRow{
		{SettledAtMs: 3000 * msPerSecond, RateFrac: 0.0003, RawRateField: "r"},
		{SettledAtMs: 1000 * msPerSecond, RateFrac: 0.0001, RawRateField: "r"},
		{SettledAtMs: 2000 * msPerSecond, RateFrac: 0.0002, RawRateField: "r"},
		{SettledAtMs: 2000 * msPerSecond, RateFrac: 0.0002, RawRateField: "r"},
	}
	entries, err := finishFundingHistory("test", symbol, FundingDiscrete, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3 — the duplicate settlement was kept", len(entries))
	}
	for i, want := range []int64{1000, 2000, 3000} {
		if entries[i].SettledAtMs != want*msPerSecond {
			t.Errorf("entry %d at %d, want %d", i, entries[i].SettledAtMs, want*msPerSecond)
		}
	}
	// A duplicate kept would have shown up here as a 0-second gap winning the
	// mode, and every APR would be an infinity.
	if entries[0].IntervalSec != 1000 {
		t.Errorf("IntervalSec = %d, want 1000", entries[0].IntervalSec)
	}
	// The first row has no predecessor and borrows its successor's spacing;
	// leaving it 0 would make its own APR undefined.
	if entries[0].GapPrevSec != 1000 {
		t.Errorf("first GapPrevSec = %d, want 1000 borrowed from the next row", entries[0].GapPrevSec)
	}
}

func TestFinishFundingHistoryRefusesUnmeasurableCadence(t *testing.T) {
	symbol := Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"}
	rows := []fundingHistoryRow{{SettledAtMs: 1000, RateFrac: 0.0001, RawRateField: "r"}}
	if _, err := finishFundingHistory("test", symbol, FundingDiscrete, rows); err == nil {
		t.Fatal("one row with no declared interval was accepted; every consumer divides by IntervalSec")
	}
}

func TestFinishFundingHistoryKeepsMeasuredGapBesideCadence(t *testing.T) {
	// An hourly series with one settlement missed. The cadence stays hourly for
	// every row — that is what the comparison figures must use — while the row
	// after the gap records what actually happened.
	symbol := Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"}
	var rows []fundingHistoryRow
	for _, atSec := range []int64{3600, 7200, 10800, 18000, 21600} {
		rows = append(rows, fundingHistoryRow{
			SettledAtMs: atSec * msPerSecond, RateFrac: 0.0001, RawRateField: "r",
		})
	}
	entries, err := finishFundingHistory("test", symbol, FundingDiscrete, rows)
	if err != nil {
		t.Fatal(err)
	}
	for i, entry := range entries {
		if entry.IntervalSec != 3600 {
			t.Errorf("entry %d: IntervalSec = %d, want the modal 3600", i, entry.IntervalSec)
		}
	}
	if entries[3].GapPrevSec != 7200 {
		t.Errorf("the row after the missed settlement records GapPrevSec = %d, want 7200",
			entries[3].GapPrevSec)
	}
	report := FundingGaps(entries)
	if report.ModalGapSec != 3600 || report.Counts[7200] != 1 {
		t.Errorf("gap report = %+v, want modal 3600 with one 7200", report)
	}
}

func TestCadenceLooksMixedSeparatesOutagesFromACadenceChange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts map[int64]int
		modal  int64
		want   bool
	}{
		{
			// Kraken's real year: six double-gaps and one triple in 8,771.
			name:   "a handful of missed settlements is not a second cadence",
			counts: map[int64]int{3600: 8764, 7200: 6, 10800: 1},
			modal:  3600,
			want:   false,
		},
		{
			// A symbol Binance moved from 8h to 4h part-way through the year.
			name:   "two eras with real weight are",
			counts: map[int64]int{14400: 1200, 28800: 400},
			modal:  14400,
			want:   true,
		},
		{
			name:   "one spacing is never mixed",
			counts: map[int64]int{28800: 1095},
			modal:  28800,
			want:   false,
		},
		{
			name:   "an empty series says nothing",
			counts: map[int64]int{},
			modal:  0,
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := FundingGapReport{ModalGapSec: tc.modal, Counts: tc.counts}
			if got := report.CadenceLooksMixed(); got != tc.want {
				t.Errorf("CadenceLooksMixed() = %v, want %v for %v", got, tc.want, tc.counts)
			}
		})
	}
}

func TestFetchFundingHistoryPageRetriesOnlyWhatIsWorthRetrying(t *testing.T) {
	transient := errors.New("HTTP 502")

	t.Run("a blip is retried and the page is kept", func(t *testing.T) {
		// The alternative is losing a series: a Paradex fetch is 2,000 requests
		// and nine minutes, and aborting it over one 502 discards all of it.
		calls := 0
		err := fetchFundingHistoryPage(context.Background(), 0, func() error {
			calls++
			if calls < 3 {
				return transient
			}
			return nil
		})
		if err != nil || calls != 3 {
			t.Fatalf("err = %v after %d calls; want success on the third", err, calls)
		}
	})

	t.Run("a persistent failure gives up and reports the last error", func(t *testing.T) {
		calls := 0
		err := fetchFundingHistoryPage(context.Background(), 0, func() error {
			calls++
			return transient
		})
		if !errors.Is(err, transient) || calls != fundingHistoryPageAttempts {
			t.Fatalf("err = %v after %d calls; want the venue's error after %d",
				err, calls, fundingHistoryPageAttempts)
		}
	})

	t.Run("a market the venue does not list is not retried", func(t *testing.T) {
		// Not transient: retrying it spends two more requests and one more
		// second per page to be told the same thing.
		calls := 0
		err := fetchFundingHistoryPage(context.Background(), 0, func() error {
			calls++
			return fmt.Errorf("wrapped: %w", errInstrumentNotListed)
		})
		if !errors.Is(err, errInstrumentNotListed) || calls != 1 {
			t.Fatalf("err = %v after %d calls; want one call", err, calls)
		}
	})

	t.Run("a cancelled fetch stops immediately", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		if err := fetchFundingHistoryPage(ctx, 0, func() error {
			calls++
			return transient
		}); err == nil {
			t.Fatal("a cancelled fetch reported success")
		}
		if calls != 1 {
			t.Fatalf("%d calls after cancellation; want one", calls)
		}
	})

	t.Run("a rate limit is retried like any other transient failure", func(t *testing.T) {
		// It is transient by definition - the budget refills - and the venue
		// that produced it (Hyperliquid, 2026-09-04) cost two whole series.
		calls := 0
		err := fetchFundingHistoryPage(context.Background(), 0, func() error {
			calls++
			if calls < 2 {
				return &rateLimitError{URL: "https://api.hyperliquid.xyz/info", Body: "null"}
			}
			return nil
		})
		if err != nil || calls != 2 {
			t.Fatalf("err = %v after %d calls; want success on the second", err, calls)
		}
	})
}

func TestRetryDelayWaitsLongerForARateLimitThanForABlip(t *testing.T) {
	const pageDelay = 200 * time.Millisecond

	t.Run("an ordinary failure waits one page delay", func(t *testing.T) {
		if got := retryDelay(errors.New("HTTP 502"), pageDelay, 1); got != pageDelay {
			t.Fatalf("retryDelay = %s, want the page delay %s", got, pageDelay)
		}
	})

	t.Run("a rate limit waits seconds, and longer the second time", func(t *testing.T) {
		// The whole point: a budget measured over a minute is not cleared by
		// coming back 200ms later, which is how three attempts were spent in
		// 600ms and a 12-month series was lost.
		limited := fmt.Errorf("wrapped: %w", &rateLimitError{URL: "u", Body: "null"})
		first, second := retryDelay(limited, pageDelay, 1), retryDelay(limited, pageDelay, 2)
		if first < time.Second {
			t.Fatalf("first rate-limit retry waits %s; that is inside the same exhausted window", first)
		}
		if second <= first {
			t.Fatalf("second retry waits %s, not more than the first %s", second, first)
		}
	})

	t.Run("the venue's own Retry-After wins when it is longer", func(t *testing.T) {
		// It knows its window; our backoff is a guess at it.
		limited := &rateLimitError{URL: "u", RetryAfter: 90 * time.Second}
		if got := retryDelay(limited, pageDelay, 1); got != 90*time.Second {
			t.Fatalf("retryDelay = %s, want the venue's 90s", got)
		}
	})

	t.Run("a Retry-After shorter than the backoff does not shorten it", func(t *testing.T) {
		limited := &rateLimitError{URL: "u", RetryAfter: time.Second}
		if got := retryDelay(limited, pageDelay, 2); got != rateLimitBackoff(2) {
			t.Fatalf("retryDelay = %s, want the backoff %s", got, rateLimitBackoff(2))
		}
	})
}

func TestRateLimitErrorIsRecognisedThroughWrapping(t *testing.T) {
	err := fmt.Errorf("hyperliquid funding history BTC: %w",
		&rateLimitError{URL: "https://api.hyperliquid.xyz/info", Body: "null"})
	if !errors.Is(err, errRateLimited) {
		t.Fatal("a wrapped rate limit is not recognised; every retry decision reads it through wrapping")
	}
	if errors.Is(err, errInstrumentNotListed) {
		t.Fatal("a rate limit read as 'market not listed' would end the series silently and report no rows")
	}
}

func TestParseRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"30", 30 * time.Second},
		{" 5 ", 5 * time.Second},
		{"", 0},
		{"0", 0},
		{"-1", 0},
		// The HTTP-date form is deliberately not parsed: comparing it against
		// our clock measures venue clock skew, which this project has already
		// measured at 80ms on Binance (CLAUDE.md rule 13).
		{"Fri, 04 Sep 2026 12:00:00 GMT", 0},
	} {
		if got := parseRetryAfter(tc.header); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", tc.header, got, tc.want)
		}
	}
}
