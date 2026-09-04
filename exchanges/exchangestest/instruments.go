package exchangestest

import (
	"math"
	"testing"

	"futures-arbitrage-scanner/exchanges"
)

// AssertInstrument compares a parsed instrument against the survey's expected
// numbers field by field, so a failure names the exact rule that drifted
// instead of dumping two structs.
func AssertInstrument(t *testing.T, got, want exchanges.Instrument) {
	t.Helper()
	if got.Symbol != want.Symbol || got.NativeSymbol != want.NativeSymbol ||
		got.Source != want.Source || got.MarketType != want.MarketType || got.Status != want.Status ||
		got.BaseAsset != want.BaseAsset || got.QuoteAsset != want.QuoteAsset {
		t.Errorf("identity = %+v,\nwant %+v", got, want)
		return
	}
	fields := []struct {
		name      string
		got, want float64
	}{
		{"TickSizeQuote", got.TickSizeQuote, want.TickSizeQuote},
		{"StepSizeCoin", got.StepSizeCoin, want.StepSizeCoin},
		{"MinQtyCoin", got.MinQtyCoin, want.MinQtyCoin},
		{"MaxQtyCoin", got.MaxQtyCoin, want.MaxQtyCoin},
		{"MinNotionalQuote", got.MinNotionalQuote, want.MinNotionalQuote},
		{"ContractSizeCoin", got.ContractSizeCoin, want.ContractSizeCoin},
		{"MaxLeverageX", got.MaxLeverageX, want.MaxLeverageX},
	}
	for _, f := range fields {
		if math.Abs(f.got-f.want) > 1e-12 {
			t.Errorf("%s %s: %s = %v, want %v", got.Source, got.NativeSymbol, f.name, f.got, f.want)
		}
	}
	if got.IsContract != want.IsContract {
		t.Errorf("%s %s: IsContract = %v, want %v", got.Source, got.NativeSymbol, got.IsContract, want.IsContract)
	}
}
