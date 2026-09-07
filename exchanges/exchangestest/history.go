package exchangestest

import (
	"math"
	"testing"

	"futures-arbitrage-scanner/exchanges"
)

// WideWindow accepts every row in a fixture. Window filtering is tested
// separately, on synthetic stamps, so the fixtures do not have to be re-recorded
// when they age past a fixed bound.
var WideWindow = exchanges.FundingWindow{StartMs: 0, EndMs: math.MaxInt64}

func BTCSymbol(t *testing.T, source string) exchanges.Symbol {
	t.Helper()
	for _, s := range Symbols(source) {
		if s.Standard == "BTCUSDT" {
			return s
		}
	}
	t.Fatalf("%s has no BTCUSDT in the symbol table", source)
	return exchanges.Symbol{}
}

// AssertHistorySane checks what must hold for every venue, so a per-venue test
// can concentrate on that venue's trap.
func AssertHistorySane(t *testing.T, entries []exchanges.FundingHistoryEntry, source string, model exchanges.FundingModel) {
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
		if want := entry.RatePerIntervalFrac * 28800 / float64(entry.IntervalSec); !closeEnough(entry.RatePer8hFrac, want) {
			t.Errorf("%s[%d]: RatePer8hFrac = %g, want %g", source, i, entry.RatePer8hFrac, want)
		}
		if want := entry.RatePerIntervalFrac * 31_536_000 / float64(entry.IntervalSec); !closeEnough(entry.APRFrac, want) {
			t.Errorf("%s[%d]: APRFrac = %g, want %g", source, i, entry.APRFrac, want)
		}
		prevMs = entry.SettledAtMs
	}
}

func closeEnough(got, want float64) bool {
	return math.Abs(got-want) <= 1e-12+math.Abs(want)*1e-9
}

// WidePriceWindow accepts every candle in a fixture. Window filtering is tested
// separately, per venue, with stamps chosen for the test — a fixture recorded
// last week would otherwise start failing the moment it aged out of a window
// expressed in real time.
var WidePriceWindow = exchanges.PriceWindow{StartMs: 0, EndMs: math.MaxInt64}

// AssertCandlesSane is the contract every venue's candle parser must satisfy,
// whatever shape the venue sent.
//
// It exists because these eight payloads disagree about everything a parser can
// get plausibly wrong and still produce numbers: field ORDER (Binance and Bybit
// are both positional arrays and both different), row ORDER (Bybit and OKX are
// newest-first), the time UNIT (Gate stamps in seconds), and, on Hyperliquid,
// two keys that differ only in case.
func AssertCandlesSane(t *testing.T, candles []exchanges.PriceCandle, source string) {
	t.Helper()
	if len(candles) == 0 {
		t.Fatalf("%s: the recording produced no candles at all", source)
	}
	for i, candle := range candles {
		switch {
		case candle.Source != source:
			t.Errorf("%s candle %d: Source = %q", source, i, candle.Source)
		case candle.Symbol == "":
			t.Errorf("%s candle %d: no symbol", source, i)
		case candle.IntervalSec != exchanges.PriceCandleIntervalSec:
			t.Errorf("%s candle %d: IntervalSec = %d, want %d",
				source, i, candle.IntervalSec, exchanges.PriceCandleIntervalSec)
		}
		// A millisecond stamp of a plausible recent instant. A venue whose
		// stamps are in SECONDS lands four orders of magnitude below this, and
		// that is the whole of Gate's trap.
		if candle.OpenTimeMs < 1_500_000_000_000 || candle.OpenTimeMs > 4_000_000_000_000 {
			t.Errorf("%s candle %d: OpenTimeMs = %d is not a plausible epoch-ms stamp — a seconds/ms mix-up",
				source, i, candle.OpenTimeMs)
		}
		// Candles open on the hour at every venue here. Hyperliquid publishes
		// both "t" (open) and "T" (close, = open + interval - 1), and Go's
		// case-insensitive JSON fallback let the second overwrite the first —
		// which showed up as every stamp landing one millisecond BEFORE the
		// hour and every cross-venue join finding nothing.
		if off := candle.OpenTimeMs % (exchanges.PriceCandleIntervalSec * exchanges.MsPerSecond); off != 0 {
			t.Errorf("%s candle %d: OpenTimeMs = %d is %d ms off the hour — the open stamp is not the open stamp",
				source, i, candle.OpenTimeMs, off)
		}
		if i > 0 && candle.OpenTimeMs <= candles[i-1].OpenTimeMs {
			t.Errorf("%s candle %d: stamp %d does not follow %d — the parser must deliver oldest first",
				source, i, candle.OpenTimeMs, candles[i-1].OpenTimeMs)
		}
		switch {
		case candle.ClosePriceQuote <= 0:
			t.Errorf("%s candle %d: close = %g", source, i, candle.ClosePriceQuote)
		case candle.OpenPriceQuote <= 0:
			t.Errorf("%s candle %d: open = %g", source, i, candle.OpenPriceQuote)
		case candle.HighPriceQuote < candle.LowPriceQuote:
			t.Errorf("%s candle %d: high %g below low %g — the field ORDER is wrong",
				source, i, candle.HighPriceQuote, candle.LowPriceQuote)
		case candle.ClosePriceQuote > candle.HighPriceQuote || candle.ClosePriceQuote < candle.LowPriceQuote:
			t.Errorf("%s candle %d: close %g outside [%g, %g] — the field ORDER is wrong",
				source, i, candle.ClosePriceQuote, candle.LowPriceQuote, candle.HighPriceQuote)
		case candle.OpenPriceQuote > candle.HighPriceQuote || candle.OpenPriceQuote < candle.LowPriceQuote:
			t.Errorf("%s candle %d: open %g outside [%g, %g] — the field ORDER is wrong",
				source, i, candle.OpenPriceQuote, candle.LowPriceQuote, candle.HighPriceQuote)
		case candle.BaseVolumeCoin < 0:
			t.Errorf("%s candle %d: volume = %g", source, i, candle.BaseVolumeCoin)
		}
	}
}
