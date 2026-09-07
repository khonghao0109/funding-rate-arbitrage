package gate

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// Gate's `t` is in SECONDS. AssertCandlesSane rejects a stamp that is not a
// plausible epoch-MS value, so this recording is what proves the ×1000 happens.
func TestGateCandlesGolden(t *testing.T) {
	symbol := exchangestest.BTCSymbol(t, "gate_futures")
	var raw []gateCandle
	exchangestest.LoadJSON(t, "price_history_gate.json", &raw)
	if len(raw) < 2 {
		t.Fatalf("the recording has %d candles", len(raw))
	}
	if raw[0].T > 100_000_000_000 {
		t.Fatalf("the recording's t = %d looks like milliseconds; this venue stamps in seconds "+
			"and the parser multiplies — re-record before trusting this test", raw[0].T)
	}

	rows, err := parseGateCandles(raw, "gate_futures", exchangestest.WidePriceWindow)
	if err != nil {
		t.Fatal(err)
	}
	candles, err := exchanges.FinishPriceHistory("gate_futures", symbol, rows)
	if err != nil {
		t.Fatal(err)
	}
	exchangestest.AssertCandlesSane(t, candles, "gate_futures")

	// `v` is a contract count and the registry is not available here.
	for i, candle := range candles {
		if candle.BaseVolumeCoin != 0 {
			t.Errorf("candle %d carries volume %g; Gate quotes it in contracts", i, candle.BaseVolumeCoin)
		}
	}
}
