package scanner

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/depth"
)

func depthFixture(sampledAt time.Time) depth.Summary {
	return depth.Summary{
		Source: "gate_futures", Symbol: "BTCUSDT",
		SampledAtMs: sampledAt.UnixMilli(), VenueTimeMs: sampledAt.UnixMilli() - 30,
		MidPriceQuote: 81000, BestBidQuote: 80999, BestAskQuote: 81001,
		BestBidQtyCoin: 1.3614, BestAskQtyCoin: 1.9466, SpreadPct: 0.0025,
		BidDepthWithinTightQuote: 250000, AskDepthWithinTightQuote: 310000,
		BidDepthWithinWideQuote: 1250000, AskDepthWithinWideQuote: 1410000,
		BidLevels: 100, AskLevels: 100,
		BidSpanPct: 0.62, AskSpanPct: 0.58,
		IsContractBook: true,
	}
}

func TestNewWireDepth_CarriesTheMeasurementAndItsIdentity(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	message := newWireDepth([]depth.Summary{depthFixture(now.Add(-5 * time.Minute))}, now)

	point, ok := message.Depth["BTCUSDT"]["gate_futures"]
	if !ok {
		t.Fatal("the measurement is missing from the message")
	}
	if point.Status != statusLive {
		t.Errorf("status = %q for a 5-minute-old sweep on an hourly cadence", point.Status)
	}
	if point.AgeMs != 300000 {
		t.Errorf("age_ms = %d, want 300000", point.AgeMs)
	}
	if !point.IsContractIn {
		t.Error("a converted book must say so; it marks which numbers depend on the registry")
	}
	if point.BestBidQtyCoin != 1.3614 {
		t.Errorf("best_bid_qty_coin = %g; the wire carries COIN, already converted", point.BestBidQtyCoin)
	}
}

// A sweep that was missed is old data, and the dashboard has to say so rather
// than show an hours-old book as current.
func TestNewWireDepth_AMissedSweepGoesStale(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	message := newWireDepth([]depth.Summary{depthFixture(now.Add(-4 * time.Hour))}, now)

	if got := message.Depth["BTCUSDT"]["gate_futures"].Status; got != statusStale {
		t.Errorf("status = %q for a 4-hour-old sweep against a %ds cadence", got, depthRefreshEverySec)
	}
}

// "Could not be read" is not "old". A failed measurement is unknown however
// recent the attempt, because there is no measurement here to age.
func TestNewWireDepth_AFailedMeasurementIsUnknownNotStale(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	failed := depth.Summary{
		Source: "okx_futures", Symbol: "BTCUSDT",
		SampledAtMs: now.Add(-time.Minute).UnixMilli(),
		ErrVI:       "Không đọc được sổ lệnh: HTTP 503",
	}
	message := newWireDepth([]depth.Summary{failed}, now)

	point := message.Depth["BTCUSDT"]["okx_futures"]
	if point.Status != statusUnknown {
		t.Errorf("status = %q, want unknown", point.Status)
	}
	if point.ErrorVI == "" {
		t.Error("the row lost its reason; a blank cell reads as an empty book")
	}
}

// The truncation flags are computed by the backend, so the dashboard renders a
// decision rather than re-deriving one (contract rule 3).
func TestNewWireDepth_ReportsWhetherTheBookReachesEachWindow(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	truncated := depthFixture(now)
	truncated.Source = "hyperliquid_futures"
	truncated.BidSpanPct, truncated.AskSpanPct = 0.025, 0.025 // measured on BTC

	message := newWireDepth([]depth.Summary{depthFixture(now), truncated}, now)

	if full := message.Depth["BTCUSDT"]["gate_futures"]; !full.CoversTight || !full.CoversWide {
		t.Errorf("a book spanning 0.6%% reports covers = %v/%v", full.CoversTight, full.CoversWide)
	}
	short := message.Depth["BTCUSDT"]["hyperliquid_futures"]
	if short.CoversTight || short.CoversWide {
		t.Error("a book spanning 0.025% claims to cover 0.1% or 0.5%")
	}
	if short.BidDepthWithinWideQuote <= 0 {
		t.Error("the figure is still published as a lower bound")
	}
}

// Every key static/app.js reads has to be present with its documented name.
func TestNewWireDepth_SerializesEveryContractKey(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	raw, err := json.Marshal(newWireDepth([]depth.Summary{depthFixture(now)}, now))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded struct {
		Type    string `json:"type"`
		V       int    `json:"v"`
		Now     int64  `json:"server_time_ms"`
		Depth   map[string]map[string]map[string]any
		Verbose map[string]any
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != "depth" || decoded.V != wireVersion || decoded.Now != now.UnixMilli() {
		t.Fatalf("envelope = %+v", decoded)
	}

	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal generic: %v", err)
	}
	point := generic["depth"].(map[string]any)["BTCUSDT"].(map[string]any)["gate_futures"].(map[string]any)
	for _, key := range []string{
		"sampled_at_ms", "age_ms", "status", "venue_time_ms",
		"mid_price_quote", "best_bid_quote", "best_ask_quote",
		"best_bid_qty_coin", "best_ask_qty_coin", "spread_pct",
		"bid_depth_within_0_1pct_quote", "ask_depth_within_0_1pct_quote",
		"bid_depth_within_0_5pct_quote", "ask_depth_within_0_5pct_quote",
		"bid_levels", "ask_levels", "bid_span_pct", "ask_span_pct",
		"covers_0_1pct", "covers_0_5pct", "is_contract_book", "error_vi",
	} {
		if _, ok := point[key]; !ok {
			t.Errorf("contract field %q is missing from the depth message", key)
		}
	}
}

// The windows on the wire must be the ones the store's column names carry, or
// the dashboard's legend describes a different measurement from the corpus.
func TestNewWireMeta_DepthWindowsMatchTheStoredColumns(t *testing.T) {
	meta := newWireMeta([]string{"BTCUSDT"}, 1)

	if len(meta.Depth.WindowsPct) != 2 ||
		meta.Depth.WindowsPct[0] != depth.WindowTightPct ||
		meta.Depth.WindowsPct[1] != depth.WindowWidePct {
		t.Errorf("meta windows = %v, want %g/%g", meta.Depth.WindowsPct, depth.WindowTightPct, depth.WindowWidePct)
	}
	if meta.Depth.RefreshEverySec <= 0 {
		t.Error("meta carries no depth cadence; the dashboard cannot explain the age it renders")
	}
	if meta.Depth.NoteVI == "" {
		t.Error("meta carries no depth note")
	}
}

// The step-1.2 debt this closes: OKX, Gate and Kraken publish top-of-book size
// in CONTRACTS, and the wire has shown 0 for them ever since.
func TestQtyInCoin_ConvertsAContractVenueAndRefusesToGuess(t *testing.T) {
	s := New([]string{"BTCUSDT"})
	s.SetContractSizes(map[string]float64{"BTCUSDT|gate_futures": 0.0001})

	book := exchanges.OrderbookData{
		Symbol: "BTCUSDT", Source: "gate_futures",
		BestBidQtyContracts: 13614, BestAskQtyContracts: 19466, // as Gate sends them
	}
	bid, ask := s.qtyInCoin(book)
	if math.Abs(bid-1.3614) > 1e-9 || math.Abs(ask-1.9466) > 1e-9 {
		t.Errorf("converted to %g/%g coin, want 1.3614/1.9466", bid, ask)
	}

	// A market the registry does not know must stay 0 — "not known". Applying
	// a multiplier of 1 here would report Gate as ten thousand times deeper.
	unknown := exchanges.OrderbookData{
		Symbol: "BTCUSDT", Source: "okx_futures", BestBidQtyContracts: 243.73,
	}
	if bid, ask := s.qtyInCoin(unknown); bid != 0 || ask != 0 {
		t.Errorf("an unknown multiplier produced %g/%g; unknown must stay 0", bid, ask)
	}

	// A venue that publishes no size at all stays 0 without consulting anything.
	none := exchanges.OrderbookData{Symbol: "BTCUSDT", Source: "paradex_futures"}
	if bid, ask := s.qtyInCoin(none); bid != 0 || ask != 0 {
		t.Errorf("a venue publishing no size produced %g/%g", bid, ask)
	}

	// A COIN venue must pass through untouched, and must not wait for the
	// registry: Binance, Bybit and Hyperliquid have published real quantities
	// since step 1.2 and a restart must not blank them for the first sweep.
	coin := exchanges.OrderbookData{
		Symbol: "BTCUSDT", Source: "binance_futures",
		BestBidQtyCoin: 3.5, BestAskQtyCoin: 0.25,
	}
	if bid, ask := s.qtyInCoin(coin); bid != 3.5 || ask != 0.25 {
		t.Errorf("a coin venue was altered to %g/%g, want 3.5/0.25", bid, ask)
	}
}

func TestSetContractSizes_ReplacesRatherThanMerges(t *testing.T) {
	// A market a venue has delisted must disappear, and merging would keep
	// yesterday's multiplier alive for a market that no longer exists.
	s := New([]string{"BTCUSDT"})
	s.SetContractSizes(map[string]float64{"BTCUSDT|gate_futures": 0.0001})
	s.SetContractSizes(map[string]float64{"BTCUSDT|okx_futures": 0.01})

	if _, ok := s.contractSizeCoin("BTCUSDT", "gate_futures"); ok {
		t.Error("a refresh that dropped a market left its multiplier behind")
	}
	if size, ok := s.contractSizeCoin("BTCUSDT", "okx_futures"); !ok || size != 0.01 {
		t.Errorf("okx multiplier = %g (ok %v), want 0.01", size, ok)
	}
}
