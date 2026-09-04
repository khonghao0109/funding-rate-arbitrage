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
