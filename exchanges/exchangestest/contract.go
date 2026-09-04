package exchangestest

import (
	"math"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
)

// BookExpectation records what one venue's payloads are known to contain; the
// shared assertions in CheckBookContract run against it.
type BookExpectation struct {
	// QuantityInCoin says the venue publishes a top-of-book size this connector
	// can put in a ...Coin field. False means the venue denominates its book in
	// CONTRACTS (OKX, Gate, Kraken) or publishes no size at all (Paradex), and
	// the connector must leave the field at 0 - "not known", never "no
	// liquidity"; the contract venues fill Best*QtyContracts instead.
	QuantityInCoin bool
	// VenueClock says the payload this connector reads carries the venue's own
	// timestamp. False means it must stay 0: filling it with our clock would
	// report our time as theirs and make a dead feed look current forever
	// (CLAUDE.md rule 13).
	VenueClock bool
	// Trades says the RECORDING contains trades, which is a weaker claim than
	// "the venue has a trade feed" and is deliberately the one asserted: it can
	// be checked.
	Trades bool
}

// CheckBookContract runs the one set of data-contract assertions every venue's
// recording must pass: the symbol on the wire is one we asked for, the source
// name is configuration rather than a literal, the receive stamp survives, a
// venue clock is never our clock, and a quantity is in coin or is zero — never
// a contract count wearing a ...Coin name.
func CheckBookContract(t *testing.T, source string, r *Recorder, recvAt time.Time, expect BookExpectation) {
	t.Helper()

	books := r.Orderbooks()
	trades := r.Trades()
	if len(books) == 0 {
		t.Fatalf("%s produced no top of book from its recording", source)
	}

	subscribed := map[string]bool{}
	for _, symbol := range Symbols(source) {
		subscribed[symbol.Standard] = true
	}

	var sawCoinQuantity bool
	for i, book := range books {
		// A market we never asked for must never reach the scanner. This is
		// not theoretical: Paradex publishes a summary for every market it
		// lists, options included.
		if !subscribed[book.Symbol] {
			t.Fatalf("book %d is for %q, which was never subscribed", i, book.Symbol)
		}
		// The source name is configuration. Hardcoding it inside a connector is
		// what made two config entries sharing a connector report under one
		// name at step 1.4.
		if book.Source != source {
			t.Errorf("book %d reports source %q, want %q", i, book.Source, source)
		}
		if !book.RecvAt.Equal(recvAt) {
			t.Errorf("book %d lost the receive stamp: %s", i, book.RecvAt)
		}
		if !isUsable(book.BestBid) || !isUsable(book.BestAsk) {
			t.Errorf("book %d has unusable prices %g/%g", i, book.BestBid, book.BestAsk)
		}
		if book.BestBid > book.BestAsk {
			t.Errorf("book %d is crossed: bid %g above ask %g", i, book.BestBid, book.BestAsk)
		}

		if expect.VenueClock {
			if book.VenueTimeMs <= 0 {
				t.Errorf("book %d has no venue timestamp, but this venue publishes one", i)
			}
		} else if book.VenueTimeMs != 0 {
			t.Errorf("book %d carries venue_time_ms %d from a venue that publishes none; only our own clock could have produced it",
				i, book.VenueTimeMs)
		}

		switch {
		case !expect.QuantityInCoin:
			if book.BestBidQtyCoin != 0 || book.BestAskQtyCoin != 0 {
				t.Errorf("book %d published %g/%g in a ...Coin field, but this venue denominates in contracts or publishes no size",
					i, book.BestBidQtyCoin, book.BestAskQtyCoin)
			}
		case book.BestBidQtyCoin > 0 && book.BestAskQtyCoin > 0:
			sawCoinQuantity = true
		}
	}

	if expect.QuantityInCoin && !sawCoinQuantity {
		t.Errorf("%s publishes book sizes in coin, but not one was collected", source)
	}

	if expect.Trades && len(trades) == 0 {
		t.Errorf("%s subscribes to a trade feed but the recording produced none", source)
	}
	for i, trade := range trades {
		if !subscribed[trade.Symbol] {
			t.Fatalf("trade %d is for %q, which was never subscribed", i, trade.Symbol)
		}
		if trade.Source != source {
			t.Errorf("trade %d reports source %q, want %q", i, trade.Source, source)
		}
		if !trade.RecvAt.Equal(recvAt) {
			t.Errorf("trade %d lost the receive stamp", i)
		}
		// The scanner and everything downstream branch on exactly these two
		// spellings. A venue's own casing reaching them is a silent
		// mis-classification.
		if trade.Side != "buy" && trade.Side != "sell" {
			t.Errorf("trade %d has side %q, want the normalized buy or sell", i, trade.Side)
		}
		if !isUsable(trade.Price) {
			t.Errorf("trade %d has unusable price %g", i, trade.Price)
		}
	}
}

// FundingExpectation is what one venue's funding recording is known to contain.
type FundingExpectation struct {
	// IntervalSec is the venue's real settlement (or quote-window) length.
	// Pinned per venue because assuming 8h everywhere is the single error this
	// whole phase exists to prevent: Kraken and Hyperliquid settle hourly.
	IntervalSec int64
	Model       exchanges.FundingModel
	// VenueClock says the funding payload carries the venue's own timestamp.
	VenueClock bool
	// NextFundingStamp says the payload states when the next settlement is. A
	// continuous venue never does.
	NextFundingStamp bool
	// RawRateField is the venue field the number was read from, which is how a
	// dashboard number is traced back to a message.
	RawRateField string
	// Estimated says the venue's published rate is still forming. Kraken is
	// the one venue whose WS field is a SETTLED figure (its docs put the
	// forming estimate in a separate relative_funding_rate_prediction field);
	// Gate's was probed drifting mid-period 2026-09-04. Pinned because
	// mislabeling either direction misleads a phase-3 consumer that filters
	// on finality.
	Estimated bool
}

// CheckFundingContract runs the shared funding assertions: identity, stamps,
// per-venue interval, and the comparison arithmetic re-derived — scaling by
// anything but the real interval is how a 1h venue is annualized as 8h.
func CheckFundingContract(t *testing.T, source string, r *Recorder, recvAt time.Time, want FundingExpectation) {
	t.Helper()

	readings := r.Fundings()
	if len(readings) == 0 {
		t.Fatalf("%s published no funding from its recording — re-record with %s=1", source, CaptureEnv)
	}

	configured := map[string]bool{}
	for _, s := range Symbols(source) {
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
		// A venue clock must never be our clock: that is what makes a dead
		// feed look current forever.
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
		wantPer8h := got.RatePerIntervalFrac * 28800 / float64(got.IntervalSec)
		wantAPR := got.RatePerIntervalFrac * 31_536_000 / float64(got.IntervalSec)
		if math.Abs(got.RatePer8hFrac-wantPer8h) > 1e-15 {
			t.Errorf("RatePer8hFrac = %v, want %v", got.RatePer8hFrac, wantPer8h)
		}
		if math.Abs(got.APRFrac-wantAPR) > 1e-12 {
			t.Errorf("APRFrac = %v, want %v", got.APRFrac, wantAPR)
		}
	}
}

func isUsable(value float64) bool {
	return value > 0 && !math.IsInf(value, 0) && !math.IsNaN(value)
}

// AssertDepthBookSane is what every venue's REST book must satisfy after
// parsing, whatever order or shape it arrived in: identity, both sides, bids
// descending, asks ascending, not crossed, no zero levels.
func AssertDepthBookSane(t *testing.T, book exchanges.DepthBook, wantSource, wantSymbol string) {
	t.Helper()
	if book.Source != wantSource || book.Symbol != wantSymbol {
		t.Errorf("identity = %s/%s, want %s/%s", book.Source, book.Symbol, wantSource, wantSymbol)
	}
	if len(book.Bids) == 0 || len(book.Asks) == 0 {
		t.Fatalf("book has %d bids and %d asks", len(book.Bids), len(book.Asks))
	}
	for i := 1; i < len(book.Bids); i++ {
		if book.Bids[i].PriceQuote > book.Bids[i-1].PriceQuote {
			t.Fatalf("bids are not descending at %d: %g then %g",
				i, book.Bids[i-1].PriceQuote, book.Bids[i].PriceQuote)
		}
	}
	for i := 1; i < len(book.Asks); i++ {
		if book.Asks[i].PriceQuote < book.Asks[i-1].PriceQuote {
			t.Fatalf("asks are not ascending at %d: %g then %g",
				i, book.Asks[i-1].PriceQuote, book.Asks[i].PriceQuote)
		}
	}
	if book.Bids[0].PriceQuote >= book.Asks[0].PriceQuote {
		t.Fatalf("book is crossed: best bid %g, best ask %g", book.Bids[0].PriceQuote, book.Asks[0].PriceQuote)
	}
	for _, side := range [][]exchanges.DepthLevel{book.Bids, book.Asks} {
		for _, level := range side {
			if level.PriceQuote <= 0 || level.QtyNative <= 0 {
				t.Fatalf("level %+v is not usable liquidity", level)
			}
		}
	}
}

// CheckNormalizedFunding compares one builder's output against the expected
// normalized block, so every venue's unit-conversion test fails with the same
// field-naming message. The expected values come from the live readings of
// 2026-09-03 recorded by cmd/fundingcheck and docs/DATA-REQUIREMENTS.md §3/§4.3
// — one case per distinct interval unit, so a wrong conversion factor cannot
// cancel out.
func CheckNormalizedFunding(t *testing.T, name string, got exchanges.FundingData, err error, recvAt time.Time, want exchanges.FundingData) {
	t.Helper()
	const (
		frac = 1e-15 // rate comparisons
		apr  = 1e-12 // APR carries the ×8760 factor, so a hair more slack
	)
	if err != nil {
		t.Errorf("%s: unexpected error: %v", name, err)
		return
	}
	if got.Symbol != want.Symbol || got.Source != want.Source {
		t.Errorf("%s: identity = %s/%s, want %s/%s", name, got.Symbol, got.Source, want.Symbol, want.Source)
	}
	if !got.RecvAt.Equal(recvAt) {
		t.Errorf("%s: RecvAt = %v, want %v", name, got.RecvAt, recvAt)
	}
	if got.VenueTimeMs != want.VenueTimeMs {
		t.Errorf("%s: VenueTimeMs = %d, want %d", name, got.VenueTimeMs, want.VenueTimeMs)
	}
	if got.Model != want.Model {
		t.Errorf("%s: Model = %q, want %q", name, got.Model, want.Model)
	}
	if got.RawRateField != want.RawRateField {
		t.Errorf("%s: RawRateField = %q, want %q", name, got.RawRateField, want.RawRateField)
	}
	if got.RawRate != want.RawRate {
		t.Errorf("%s: RawRate = %v, want %v", name, got.RawRate, want.RawRate)
	}
	if got.IntervalSec != want.IntervalSec {
		t.Errorf("%s: IntervalSec = %d, want %d", name, got.IntervalSec, want.IntervalSec)
	}
	if math.Abs(got.RatePerIntervalFrac-want.RatePerIntervalFrac) > frac {
		t.Errorf("%s: RatePerIntervalFrac = %v, want %v", name, got.RatePerIntervalFrac, want.RatePerIntervalFrac)
	}
	if math.Abs(got.RatePer8hFrac-want.RatePer8hFrac) > frac {
		t.Errorf("%s: RatePer8hFrac = %v, want %v", name, got.RatePer8hFrac, want.RatePer8hFrac)
	}
	if math.Abs(got.APRFrac-want.APRFrac) > apr {
		t.Errorf("%s: APRFrac = %v, want %v", name, got.APRFrac, want.APRFrac)
	}
	if got.NextFundingAtMs != want.NextFundingAtMs {
		t.Errorf("%s: NextFundingAtMs = %d, want %d", name, got.NextFundingAtMs, want.NextFundingAtMs)
	}
}
