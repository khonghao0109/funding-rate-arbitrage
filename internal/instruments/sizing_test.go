package instruments

import (
	"math"
	"strings"
	"testing"

	"futures-arbitrage-scanner/exchanges"
)

// Real instrument rules as the venues published them on 2026-09-03 (recorded
// in exchanges/testdata/instruments_*.json and docs/DATA-REQUIREMENTS.md §5).
// Quantities are already normalized to coin by the exchanges layer; the
// contract venues carry their measured contract size.
func realInstruments() map[string]exchanges.Instrument {
	return map[string]exchanges.Instrument{
		"binance_spot": {
			Symbol: "BTCUSDT", Source: "binance_spot", MarketType: "spot", Status: "trading",
			TickSizeQuote: 0.01, StepSizeCoin: 0.00001, MinQtyCoin: 0.00001, MaxQtyCoin: 9000,
			MinNotionalQuote: 5, ContractSizeCoin: 1,
		},
		"bybit_spot": {
			Symbol: "BTCUSDT", Source: "bybit_spot", MarketType: "spot", Status: "trading",
			TickSizeQuote: 0.1, StepSizeCoin: 0.000001, MinQtyCoin: 0.000001, MaxQtyCoin: 230,
			MinNotionalQuote: 5, ContractSizeCoin: 1,
		},
		"binance_futures": {
			Symbol: "BTCUSDT", Source: "binance_futures", MarketType: "perp", Status: "trading",
			TickSizeQuote: 0.1, StepSizeCoin: 0.001, MinQtyCoin: 0.001, MaxQtyCoin: 1000,
			MinNotionalQuote: 50, ContractSizeCoin: 1,
		},
		"bybit_futures": {
			Symbol: "BTCUSDT", Source: "bybit_futures", MarketType: "perp", Status: "trading",
			TickSizeQuote: 0.1, StepSizeCoin: 0.001, MinQtyCoin: 0.001, MaxQtyCoin: 1500,
			MinNotionalQuote: 5, ContractSizeCoin: 1, MaxLeverageX: 150,
		},
		"okx_futures": {
			Symbol: "BTCUSDT", Source: "okx_futures", MarketType: "perp", Status: "trading",
			// lotSz 0.01 contracts × ctVal 0.01 BTC = 0.0001 BTC per step.
			TickSizeQuote: 0.1, StepSizeCoin: 0.0001, MinQtyCoin: 0.0001,
			IsContract: true, ContractSizeCoin: 0.01, MaxLeverageX: 100,
		},
		"gate_futures": {
			Symbol: "BTCUSDT", Source: "gate_futures", MarketType: "perp", Status: "trading",
			// order sizes are WHOLE contracts of quanto_multiplier 0.0001 BTC.
			TickSizeQuote: 0.1, StepSizeCoin: 0.0001, MinQtyCoin: 0.0001, MaxQtyCoin: 1200,
			IsContract: true, ContractSizeCoin: 0.0001, MaxLeverageX: 200,
		},
		"kraken_futures": {
			Symbol: "BTCUSDT", Source: "kraken_futures", MarketType: "perp", Status: "trading",
			// contractSize 1 BTC, precision 4 → 0.0001-BTC steps; Kraken
			// publishes no minimum order size, so MinQtyCoin is 0 = "not
			// stated" and the sizing floor comes from the step itself.
			TickSizeQuote: 1, StepSizeCoin: 0.0001,
			IsContract: true, ContractSizeCoin: 1, MaxLeverageX: 100,
		},
		"hyperliquid_futures": {
			Symbol: "BTCUSDT", Source: "hyperliquid_futures", MarketType: "perp", Status: "trading",
			// szDecimals 5; tick size is a 5-significant-figure RULE, hence 0.
			TickSizeQuote: 0, StepSizeCoin: 0.00001, MinQtyCoin: 0.00001,
			MinNotionalQuote: 10, ContractSizeCoin: 1, MaxLeverageX: 40,
		},
		"paradex_futures": {
			Symbol: "BTCUSDT", Source: "paradex_futures", MarketType: "perp", Status: "trading",
			TickSizeQuote: 0.1, StepSizeCoin: 0.00001, MaxQtyCoin: 100,
			MinNotionalQuote: 10, ContractSizeCoin: 1,
		},
	}
}

func req(spotPrice, perpPrice, notional float64) SizingRequest {
	return SizingRequest{SpotPriceQuote: spotPrice, PerpPriceQuote: perpPrice, NotionalQuote: notional}
}

// The acceptance criterion of step 2.3: any notional in, a valid two-leg size
// out on every venue — or a reasoned refusal. Spot leg is Binance spot; the
// perp leg walks all seven venues.
func TestSizeDeltaNeutral_AllPerpVenues(t *testing.T) {
	inst := realInstruments()
	const (
		spotPrice = 78000.0
		perpPrice = 78010.0
		notional  = 1000.0
	)
	// 1000/78000 = 0.0128205... coin before rounding.
	cases := []struct {
		perp         string
		wantQtyCoin  float64
		wantPerpUnit float64
	}{
		// Coarser step 0.001 (perp): floor(0.0128205/0.001) → 0.012.
		{"binance_futures", 0.012, 0.012},
		{"bybit_futures", 0.012, 0.012},
		// Coarser 0.0001 → 0.0128; OKX orders in 0.01-BTC contracts → 1.28.
		{"okx_futures", 0.0128, 1.28},
		// Gate orders in 0.0001-BTC contracts → 128 whole contracts.
		{"gate_futures", 0.0128, 128},
		// Kraken contracts are 1 BTC each → 0.0128 contracts.
		{"kraken_futures", 0.0128, 0.0128},
		// Coarser 0.00001 → 0.01282.
		{"hyperliquid_futures", 0.01282, 0.01282},
		{"paradex_futures", 0.01282, 0.01282},
	}
	for _, c := range cases {
		got, err := SizeDeltaNeutral(inst["binance_spot"], inst[c.perp], req(spotPrice, perpPrice, notional))
		if err != nil {
			t.Errorf("%s: unexpected refusal: %v", c.perp, err)
			continue
		}
		if math.Abs(got.QtyCoin-c.wantQtyCoin) > 1e-12 {
			t.Errorf("%s: QtyCoin = %v, want %v", c.perp, got.QtyCoin, c.wantQtyCoin)
		}
		if math.Abs(got.SpotQtyUnits-c.wantQtyCoin) > 1e-12 {
			t.Errorf("%s: SpotQtyUnits = %v, want %v (spot is always coin)", c.perp, got.SpotQtyUnits, c.wantQtyCoin)
		}
		if math.Abs(got.PerpQtyUnits-c.wantPerpUnit) > 1e-9 {
			t.Errorf("%s: PerpQtyUnits = %v, want %v", c.perp, got.PerpQtyUnits, c.wantPerpUnit)
		}
		if math.Abs(got.SpotNotionalQuote-got.QtyCoin*spotPrice) > 1e-9 {
			t.Errorf("%s: SpotNotionalQuote = %v, want qty×spotPrice", c.perp, got.SpotNotionalQuote)
		}
		if math.Abs(got.PerpNotionalQuote-got.QtyCoin*perpPrice) > 1e-9 {
			t.Errorf("%s: PerpNotionalQuote = %v, want qty×perpPrice", c.perp, got.PerpNotionalQuote)
		}
	}
}

// The other spot venue: Bybit spot's finer step must not matter — the perp's
// coarser step decides.
func TestSizeDeltaNeutral_BybitSpotLeg(t *testing.T) {
	inst := realInstruments()
	got, err := SizeDeltaNeutral(inst["bybit_spot"], inst["gate_futures"], req(78000, 78010, 500))
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	// 500/78000 = 0.0064102...; coarser 0.0001 → 0.0064 → 64 Gate contracts.
	if math.Abs(got.QtyCoin-0.0064) > 1e-12 || math.Abs(got.PerpQtyUnits-64) > 1e-9 {
		t.Fatalf("got QtyCoin=%v PerpQtyUnits=%v, want 0.0064 and 64", got.QtyCoin, got.PerpQtyUnits)
	}
}

func TestSizeDeltaNeutral_Refusals(t *testing.T) {
	inst := realInstruments()
	tradable := func(mut func(i *exchanges.Instrument)) exchanges.Instrument {
		i := inst["binance_futures"]
		mut(&i)
		return i
	}
	cases := []struct {
		name       string
		spot, perp exchanges.Instrument
		spotPrice  float64
		perpPrice  float64
		notional   float64
		wantInWhy  string
	}{
		{
			// 60/78000 = 0.00077 coin floors to zero 0.001-steps — refused by
			// the below-one-step guard, which cannot lean on MinQtyCoin
			// because two venues publish none.
			name: "notional too small for the coarser step",
			spot: inst["binance_spot"], perp: inst["binance_futures"],
			spotPrice: 78000, perpPrice: 78010, notional: 60,
			wantInWhy: "below one step",
		},
		{
			name:      "contract-denominated spot leg refused loudly",
			spot:      exchanges.Instrument{Source: "spot_x", Symbol: "BTCUSDT", Status: "trading", StepSizeCoin: 0.0001, IsContract: true, ContractSizeCoin: 0.01},
			perp:      inst["binance_futures"],
			spotPrice: 78000, perpPrice: 78010, notional: 1000,
			wantInWhy: "contract-denominated",
		},
		{
			name:      "perp min notional not cleared",
			spot:      exchanges.Instrument{Source: "spot_x", Status: "trading", StepSizeCoin: 0.01, MinQtyCoin: 0.01, MinNotionalQuote: 5, ContractSizeCoin: 1},
			perp:      exchanges.Instrument{Source: "perp_x", Status: "trading", StepSizeCoin: 0.01, MinQtyCoin: 0.01, MinNotionalQuote: 50, ContractSizeCoin: 1},
			spotPrice: 1, perpPrice: 1, notional: 20,
			wantInWhy: "notional",
		},
		{
			name:      "market not trading",
			spot:      inst["binance_spot"],
			perp:      tradable(func(i *exchanges.Instrument) { i.Status = "break" }),
			spotPrice: 78000, perpPrice: 78010, notional: 1000,
			wantInWhy: "break",
		},
		{
			name:      "contract venue without a known contract size",
			spot:      inst["binance_spot"],
			perp:      tradable(func(i *exchanges.Instrument) { i.IsContract = true; i.ContractSizeCoin = 0 }),
			spotPrice: 78000, perpPrice: 78010, notional: 1000,
			wantInWhy: "contract size",
		},
		{
			// 20_000_000/78000 = 256.4 BTC > Bybit spot's maxOrderQty 230.
			name: "quantity above the venue's max order size",
			spot: inst["bybit_spot"], perp: inst["kraken_futures"],
			spotPrice: 78000, perpPrice: 78010, notional: 20_000_000,
			wantInWhy: "max",
		},
		{
			// floor(0.005/0.003) → 0.003 coin = 1.5 spot steps of 0.002:
			// on the coarser grid but NOT on the finer one.
			name:      "incommensurable steps refuse instead of shipping a half-valid size",
			spot:      exchanges.Instrument{Source: "spot_x", Status: "trading", StepSizeCoin: 0.002, MinQtyCoin: 0.002, ContractSizeCoin: 1},
			perp:      exchanges.Instrument{Source: "perp_x", Status: "trading", StepSizeCoin: 0.003, MinQtyCoin: 0.003, ContractSizeCoin: 1},
			spotPrice: 1, perpPrice: 1, notional: 0.005,
			wantInWhy: "step",
		},
		{
			name: "non-positive notional",
			spot: inst["binance_spot"], perp: inst["binance_futures"],
			spotPrice: 78000, perpPrice: 78010, notional: 0,
			wantInWhy: "notional",
		},
		{
			name: "non-positive price",
			spot: inst["binance_spot"], perp: inst["binance_futures"],
			spotPrice: 0, perpPrice: 78010, notional: 1000,
			wantInWhy: "price",
		},
		{
			name:      "unknown step size",
			spot:      inst["binance_spot"],
			perp:      tradable(func(i *exchanges.Instrument) { i.StepSizeCoin = 0 }),
			spotPrice: 78000, perpPrice: 78010, notional: 1000,
			wantInWhy: "step",
		},
	}
	for _, c := range cases {
		_, err := SizeDeltaNeutral(c.spot, c.perp, req(c.spotPrice, c.perpPrice, c.notional))
		if err == nil {
			t.Errorf("%s: want refusal, got a size", c.name)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), c.wantInWhy) {
			t.Errorf("%s: refusal %q does not mention %q", c.name, err, c.wantInWhy)
		}
	}
}

// Floating-point regression: 128 × 0.0001 is not exactly 0.0128 in float64;
// the grid validation must tolerate representation error, not refuse it.
func TestSizeDeltaNeutral_FloatRepresentationOnGrids(t *testing.T) {
	inst := realInstruments()
	for notional := 100.0; notional <= 5000; notional += 137.31 {
		if _, err := SizeDeltaNeutral(inst["binance_spot"], inst["gate_futures"], req(78000, 78010, notional)); err != nil {
			t.Fatalf("notional %.2f: unexpected refusal: %v", notional, err)
		}
	}
}
