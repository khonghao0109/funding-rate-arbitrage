package execution

import (
	"errors"
	"math"
	"testing"
	"time"
)

// Sizing happens entirely before anything is sent, and every refusal here
// costs nothing — which is why the refusals are generous and named.

func TestPlanEntry_SizesBothLegsOntoTheirOwnGridsAndHoldsTheInvariant(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	intent := testIntent(nowMs)
	cfg := DefaultConfig()
	cfg.Now = func() time.Time { return time.UnixMilli(nowMs) }

	plan, err := planEntry(intent, cfg)
	if err != nil {
		t.Fatalf("planEntry: %v", err)
	}

	// The common quantity must sit on BOTH grids, not just on the coarser one.
	for _, leg := range []struct {
		name string
		step float64
		qty  float64
	}{
		{"spot", spotRules().StepSizeCoin, plan.SpotOrder.QtyCoin},
		{"perp", perpRules().StepSizeCoin, plan.PerpOrder.QtyCoin},
	} {
		steps := leg.qty / leg.step
		if math.Abs(steps-math.Round(steps)) > 1e-6 {
			t.Errorf("%s quantity %v is not a whole multiple of its %v step", leg.name, leg.qty, leg.step)
		}
	}

	// The invariant, checked before a single order exists.
	residual := math.Abs(plan.SpotOrder.QtyCoin - plan.PerpOrder.QtyCoin)
	coarser := math.Max(spotRules().StepSizeCoin, perpRules().StepSizeCoin)
	if residual > coarser+1e-12 {
		t.Errorf("the two legs differ by %v, more than the coarser step %v — this position would open unhedged", residual, coarser)
	}
	if plan.ResidualToleranceQtyCoin != coarser {
		t.Errorf("ResidualToleranceQtyCoin = %v, want the coarser step %v", plan.ResidualToleranceQtyCoin, coarser)
	}

	// Both legs must clear their own minimum notional, or nothing may be sent.
	if plan.SpotOrder.NotionalQuote < spotRules().MinNotionalQuote {
		t.Errorf("spot notional %v is under its minimum", plan.SpotOrder.NotionalQuote)
	}
	if plan.PerpOrder.NotionalQuote < perpRules().MinNotionalQuote {
		t.Errorf("perp notional %v is under its minimum", plan.PerpOrder.NotionalQuote)
	}

	// The marketable-limit prices must be on the right side of the mid: a BUY
	// cap ABOVE the mid so it can take liquidity, a SELL floor BELOW it.
	if plan.SpotOrder.PriceQuote <= intent.SpotBook.MidPriceQuote {
		t.Errorf("the spot BUY cap %v is not above the mid %v, so it would rest instead of taking",
			plan.SpotOrder.PriceQuote, intent.SpotBook.MidPriceQuote)
	}
	if plan.PerpOrder.PriceQuote >= intent.PerpBook.MidPriceQuote {
		t.Errorf("the perp SELL floor %v is not below the mid %v", plan.PerpOrder.PriceQuote, intent.PerpBook.MidPriceQuote)
	}
}

// The book check is the last thing before placing, and it refuses rather than
// entering at a price the signal never authorised.
func TestPlanEntry_RefusesWhenTheBookWidenedPastTheAuthorisedCost(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	intent := testIntent(nowMs)
	// The signal was made when entry cost almost nothing. The book now prices
	// the same size far worse than that.
	intent.SignalEntryCostPct = 0.0001
	cfg := DefaultConfig()
	cfg.Now = func() time.Time { return time.UnixMilli(nowMs) }
	cfg.MaxEntryCostWidenBps = 1

	_, err := planEntry(intent, cfg)
	if !errors.Is(err, ErrBookWidened) {
		t.Fatalf("err = %v, want ErrBookWidened", err)
	}
	if !errors.Is(err, ErrRefusedBeforePlacing) {
		t.Error("a widened book must be refused BEFORE anything is placed")
	}
}

// A book too thin to absorb the size prices nothing, and a refused fill must
// never become a zero cost.
func TestPlanEntry_RefusesAThinBookRatherThanPricingItAtZero(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	cfg := DefaultConfig()
	cfg.Now = func() time.Time { return time.UnixMilli(nowMs) }

	for _, which := range []string{"spot", "perp"} {
		t.Run(which+" side is thin", func(t *testing.T) {
			intent := testIntent(nowMs)
			if which == "spot" {
				intent.SpotBook = thinBook("binance_spot", 60_000, nowMs)
			} else {
				intent.PerpBook = thinBook("binance_futures", 60_000, nowMs)
			}
			_, err := planEntry(intent, cfg)
			if !errors.Is(err, ErrBookWidened) {
				t.Fatalf("err = %v, want ErrBookWidened for a book that cannot absorb the size", err)
			}
		})
	}
}

// A book old enough to be history is not evidence about the current market.
func TestPlanEntry_RefusesAStaleBook(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	intent := testIntent(nowMs - 10*60*1000) // books sampled ten minutes ago
	cfg := DefaultConfig()
	cfg.Now = func() time.Time { return time.UnixMilli(nowMs) }

	_, err := planEntry(intent, cfg)
	if !errors.Is(err, ErrBookStale) {
		t.Fatalf("err = %v, want ErrBookStale", err)
	}
}

// Under a venue minimum is a named refusal, and the name says which venue.
func TestPlanEntry_RefusesWhenEitherLegLandsUnderItsMinimum(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	cfg := DefaultConfig()
	cfg.Now = func() time.Time { return time.UnixMilli(nowMs) }

	// $30 clears spot's minNotional of 5 but not the perp's 50 — exactly the
	// asymmetry that makes rounding each leg separately dangerous.
	intent := testIntent(nowMs)
	intent.NotionalQuote = 30

	_, err := planEntry(intent, cfg)
	if !errors.Is(err, ErrSizeBelowMinimum) {
		t.Fatalf("err = %v, want ErrSizeBelowMinimum", err)
	}
	if !errors.Is(err, ErrRefusedBeforePlacing) {
		t.Error("a size refusal must be a before-placing refusal")
	}
}

// An unverified maintenance bracket is refused, exactly as internal/strategy
// refuses an unverified fee schedule. A rate nobody looked up is not zero.
func TestPlanEntry_RefusesAnUnverifiedMaintenanceBracket(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	intent := testIntent(nowMs)
	intent.PerpBracket.Verified = false
	cfg := DefaultConfig()
	cfg.Now = func() time.Time { return time.UnixMilli(nowMs) }

	_, err := planEntry(intent, cfg)
	if !errors.Is(err, ErrMarginUnverified) {
		t.Fatalf("err = %v, want ErrMarginUnverified — an unlooked-up maintenance rate is not zero", err)
	}
}

// An intent that does not describe a position is refused before any venue rule
// is even consulted.
func TestPlanEntry_RefusesAMalformedIntent(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	cfg := DefaultConfig()
	cfg.Now = func() time.Time { return time.UnixMilli(nowMs) }

	bad := map[string]func(Intent) Intent{
		"no id":              func(i Intent) Intent { i.ID = ""; return i },
		"no symbol":          func(i Intent) Intent { i.Symbol = ""; return i },
		"no notional":        func(i Intent) Intent { i.NotionalQuote = 0; return i },
		"negative notional":  func(i Intent) Intent { i.NotionalQuote = -1; return i },
		"no spot price":      func(i Intent) Intent { i.SpotPriceQuote = 0; return i },
		"no perp price":      func(i Intent) Intent { i.PerpPriceQuote = 0; return i },
		"spot not trading":   func(i Intent) Intent { i.SpotInstrument.Status = "halt"; return i },
		"perp not trading":   func(i Intent) Intent { i.PerpInstrument.Status = "break"; return i },
		"symbols disagree":   func(i Intent) Intent { i.PerpInstrument.Symbol = "ETHUSDT"; return i },
		"no margin declared": func(i Intent) Intent { i.PerpMarginFrac = 0; return i },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := planEntry(mutate(testIntent(nowMs)), cfg); !errors.Is(err, ErrRefusedBeforePlacing) {
				t.Fatalf("err = %v, want a before-placing refusal", err)
			}
		})
	}
}

// The plan reports how old the evidence was. 4.4a is a REST snapshot, so this
// is a real limitation and is measured rather than hidden.
func TestPlanEntry_ReportsTheAgeOfTheAuthorisingBook(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	intent := testIntent(nowMs - 3_000)
	cfg := DefaultConfig()
	cfg.Now = func() time.Time { return time.UnixMilli(nowMs) }

	plan, err := planEntry(intent, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if plan.BookAgeMs < 3_000 || plan.BookAgeMs > 3_100 {
		t.Errorf("BookAgeMs = %d, want about 3000", plan.BookAgeMs)
	}
}

// The invariant itself, tested directly.
//
// planEntry cannot reach a mismatched pair today — instruments.SizeDeltaNeutral
// refuses incommensurable grids, so both RoundOrder calls floor the same number
// to itself and the residual is exactly zero. The taking-the-smaller-and-
// re-rounding step in planEntry is therefore defence in depth against a future
// change to either function, and this test is what proves the defence works,
// since no input to planEntry can currently exercise it.
func TestPairInvariant_RejectsWhatIsNeitherHedgedNorFlat(t *testing.T) {
	inv := pairInvariant{
		ToleranceQtyCoin:     0.0001, // the coarser (perp) step
		SpotPriceQuote:       60_000,
		PerpPriceQuote:       60_000,
		SpotMinNotionalQuote: 5,
		PerpMinNotionalQuote: 50,
	}

	t.Run("equal quantities are hedged", func(t *testing.T) {
		if err := inv.check(0.5, 0.5); err != nil {
			t.Errorf("equal legs were rejected: %v", err)
		}
	})

	t.Run("a difference beyond the coarser step is not hedged", func(t *testing.T) {
		if err := inv.check(0.5, 0.4); err == nil {
			t.Error("legs differing by 0.1 coin were accepted as hedged")
		}
	})

	t.Run("a residual inside the step can still be a real position", func(t *testing.T) {
		// One perp step, 0.0001 BTC, is $6 at this price: under the perp's own
		// $50 minimum but OVER the spot venue's $5 one. So it is a position
		// somebody could close, which means it is a position that is there.
		// The two conditions are independent and this is the case that proves
		// it.
		err := inv.check(0.5001, 0.5)
		if err == nil {
			t.Fatal("a $6 residual was accepted, although the spot venue would let us trade it away — it is a real unhedged position")
		}
		if !contains(err.Error(), "spot") {
			t.Errorf("the refusal does not say which venue could trade the residual: %v", err)
		}
	})

	t.Run("a residual too small for either venue is acceptable", func(t *testing.T) {
		// 0.00005 BTC is $3, under both minimums.
		if err := inv.check(0.50005, 0.5); err != nil {
			t.Errorf("a residual neither venue would trade was rejected: %v", err)
		}
	})

	t.Run("classify names the two states and refuses the third", func(t *testing.T) {
		if got, err := inv.classify(0, 0); err != nil || got != OutcomeBothFlat {
			t.Errorf("classify(0,0) = %q, %v; want both_flat", got, err)
		}
		if got, err := inv.classify(0.5, 0.5); err != nil || got != OutcomeBothOpen {
			t.Errorf("classify(0.5,0.5) = %q, %v; want both_open", got, err)
		}
		// One leg open and the other flat is the state the package exists to
		// prevent, and it must be loud rather than silently classified.
		got, err := inv.classify(0.5, 0)
		if err == nil {
			t.Fatalf("classify(0.5,0) returned %q with no error — a naked leg was classified as a valid outcome", got)
		}
		if !errors.Is(err, ErrUnwindIncomplete) {
			t.Errorf("err = %v, want ErrUnwindIncomplete", err)
		}
	})
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
