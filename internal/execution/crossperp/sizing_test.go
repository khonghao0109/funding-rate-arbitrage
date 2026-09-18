package crossperp

import (
	"errors"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/internal/depth"
)

func TestCommonStepCoin_IsTheCoarserStepAndRefusesIncommensurableGrids(t *testing.T) {
	for _, c := range []struct {
		a, b float64
		want float64
		ok   bool
	}{
		{0.0001, 0.001, 0.001, true}, // Binance testnet BTC vs Bybit linear BTC
		{0.001, 0.0001, 0.001, true},
		{0.001, 0.001, 0.001, true},
		{1, 0.1, 1, true},
		{0.002, 0.005, 0, false},
		{0.1, 0.25, 0, false},
		{0, 0.001, 0, false},
		{math.NaN(), 0.001, 0, false},
	} {
		got, err := CommonStepCoin(c.a, c.b)
		if c.ok != (err == nil) || c.ok && got != c.want {
			t.Errorf("CommonStepCoin(%v, %v) = %v, %v", c.a, c.b, got, err)
		}
		if !c.ok && !errors.Is(err, ErrIncommensurateSteps) {
			t.Errorf("CommonStepCoin(%v, %v) refused with %v, want ErrIncommensurateSteps", c.a, c.b, err)
		}
	}
}

// The design's three formulas, on the real rules of both venues.
func TestPlanOpen_TheDesignsThreeFormulas(t *testing.T) {
	h := newHarness(t)
	plan, err := planOpen(h.intent(), h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Q = FloorToStep(20000 / 60000, max(0.0001, 0.001)) = 0.333
	if plan.CommonStepCoin != 0.001 || plan.QtyCoin != 0.333 || plan.LongOrder.QtyCoin != 0.333 || plan.ShortOrder.QtyCoin != 0.333 {
		t.Fatalf("%+v", plan)
	}

	// Q × P_mid under the LARGER minimum is refused before anything is sent:
	// 90 quote → Q = 0.001 → 60 quote, under a 100-quote minimum.
	intent := h.intent()
	intent.NotionalQuote = 90
	intent.Short.Rules.MinNotionalQuote = 100
	if _, err := planOpen(intent, h.cfg); !errors.Is(err, ErrSizeBelowMinimum) {
		t.Errorf("under the larger minimum: %v", err)
	}
	intent.NotionalQuote = 50 // below one common step
	if _, err := planOpen(intent, h.cfg); !errors.Is(err, ErrSizeBelowMinimum) {
		t.Errorf("under one step: %v", err)
	}
}

func TestPlanOpen_RefusesWhatIsNotOnePairOnTwoVenues(t *testing.T) {
	h := newHarness(t)
	cases := map[string]func(i *Intent){
		"one venue twice":        func(i *Intent) { i.Short.Venue = i.Long.Venue },
		"a stale book":           func(i *Intent) { i.Short.Book.SampledAtMs = time.Now().Add(-time.Hour).UnixMilli() },
		"a quote bridge":         func(i *Intent) { i.Short.Rules.QuoteAsset = "USD" },
		"two coins":              func(i *Intent) { i.Short.Rules.BaseAsset = "ETH" },
		"a spot market":          func(i *Intent) { i.Long.Rules.MarketType = "spot" },
		"incommensurable grids":  func(i *Intent) { i.Long.Rules.StepSizeCoin, i.Short.Rules.StepSizeCoin = 0.002, 0.005 },
		"a book that widened":    func(i *Intent) { i.SignalEntryCostPct = -1 },
		"no best ask":            func(i *Intent) { i.Long.Book.BestAskQuote = 0 },
		"a contract of 10 coins": func(i *Intent) { i.Long.Rules.IsContract, i.Long.Rules.ContractSizeCoin = true, 10 },
	}
	for name, mutate := range cases {
		intent := h.intent()
		mutate(&intent)
		if _, err := planOpen(intent, h.cfg); !errors.Is(err, ErrRefusedBeforePlacing) {
			t.Errorf("%s: %v, want a refusal before placing", name, err)
		}
	}
}

// Float dust: across thousands of sizes and prices, the quantity and price a
// venue client renders with strconv.FormatFloat(v, 'f', -1, 64) carry no more
// decimals than their grid, and parse back to exactly the value sent.
func TestPlanOpen_NoFloatDustReachesTheWire(t *testing.T) {
	h := newHarness(t)
	rnd := rand.New(rand.NewSource(7))
	checked := 0
	for i := 0; i < 5000; i++ {
		intent := h.intent()
		mid := math.Round((100+rnd.Float64()*99_900)*10) / 10
		intent.Long.Book = deepBook(venueBinance, mid, time.Now().UnixMilli())
		intent.Short.Book = deepBook(venueBybit, mid, time.Now().UnixMilli())
		intent.NotionalQuote = 60 + rnd.Float64()*40_000
		plan, err := planOpen(intent, h.cfg)
		if err != nil {
			continue
		}
		checked++
		for _, o := range []struct {
			name             string
			qty, price       float64
			qtyDec, priceDec int
		}{
			{"long", plan.LongOrder.QtyCoin, plan.LongOrder.PriceQuote, 3, 1},
			{"short", plan.ShortOrder.QtyCoin, plan.ShortOrder.PriceQuote, 3, 1},
		} {
			for _, v := range []struct {
				what string
				val  float64
				dec  int
			}{{"qty", o.qty, o.qtyDec}, {"price", o.price, o.priceDec}} {
				s := strconv.FormatFloat(v.val, 'f', -1, 64)
				if dot := strings.IndexByte(s, '.'); dot >= 0 && len(s)-dot-1 > v.dec {
					t.Fatalf("%s %s %s on the wire has %d decimals, the grid has %d (notional %v, mid %v)", o.name, v.what, s, len(s)-dot-1, v.dec, intent.NotionalQuote, mid)
				}
				if back, _ := strconv.ParseFloat(s, 64); back != v.val {
					t.Fatalf("%s %s %s does not round-trip to %v", o.name, v.what, s, v.val)
				}
			}
		}
	}
	if checked < 4000 {
		t.Fatalf("only %d of 5000 random intents were sized — the sweep proves little", checked)
	}
}

// A manual open has no earlier signal to widen from, so its tolerance is its
// own. The shipped tolerance must still bind on an intent that does not ask for
// the exemption — otherwise one caller's convenience would disarm the check for
// the signal path too.
func TestPlanOpen_ThePerIntentWidenToleranceOverridesTheConfigsAndOnlyForThatIntent(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxEntryCostWidenBps = 5

	// A book thin enough that the entry really costs more than five basis
	// points, which is the state every manual press met on the Bybit testnet's
	// ADAUSDT (12.1 bps measured 2026-09-18).
	thin := func(source string, mid float64) depth.Summary {
		b := deepBook(source, mid, h.cfg.Now().UnixMilli())
		b.BidDepthWithinTightQuote, b.AskDepthWithinTightQuote = 500, 500
		b.BidDepthWithinWideQuote, b.AskDepthWithinWideQuote = 30_000, 30_000
		return b
	}
	base := func() Intent {
		in := h.intent()
		// A button press states no signal cost, which is exactly why the
		// shipped tolerance refuses it: every real book widens past zero.
		in.SignalEntryCostPct = 0
		in.Long.Book, in.Short.Book = thin(venueBybit, 60_000), thin(venueBinance, 60_000)
		return in
	}
	infinity, nan, tight := math.Inf(1), math.NaN(), 0.0

	t.Run("without the override the shipped tolerance refuses a widened book", func(t *testing.T) {
		if _, err := planOpen(base(), h.cfg); !errors.Is(err, ErrBookWidened) {
			t.Fatalf("err = %v, want ErrBookWidened — the shipped tolerance must still bind", err)
		}
	})
	t.Run("+Inf means the tolerance is not checked, and the cost is still measured", func(t *testing.T) {
		in := base()
		in.MaxEntryCostWidenBpsOverride = &infinity
		plan, err := planOpen(in, h.cfg)
		if errors.Is(err, ErrBookWidened) {
			t.Fatalf("a manual open was refused for widening: %v", err)
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("entry cost on the thin book = %.4f%% (%.2f bps)", plan.EntryCostPct, plan.WidenBps)
		if plan.EntryCostPct <= 0 {
			t.Errorf("EntryCostPct = %v — the cost must still be MEASURED and reported, only not acted on", plan.EntryCostPct)
		}
	})
	t.Run("an override can TIGHTEN where the config would not", func(t *testing.T) {
		loose := h.cfg
		loose.MaxEntryCostWidenBps = 10_000
		in := base()
		in.MaxEntryCostWidenBpsOverride = &tight
		if _, err := planOpen(in, loose); !errors.Is(err, ErrBookWidened) {
			t.Fatalf("err = %v, want ErrBookWidened — an override must bind in both directions", err)
		}
	})
	t.Run("NaN is refused rather than accepting every book", func(t *testing.T) {
		in := base()
		in.MaxEntryCostWidenBpsOverride = &nan
		if err := validateIntent(in); !errors.Is(err, ErrIntentInvalid) {
			t.Fatalf("err = %v, want ErrIntentInvalid — NaN compares false against everything", err)
		}
	})
}
