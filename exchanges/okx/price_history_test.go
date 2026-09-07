package okx

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestOKXCandlesGolden(t *testing.T) {
	symbol := exchangestest.BTCSymbol(t, "okx_futures")
	var resp okxCandleResponse
	exchangestest.LoadJSON(t, "price_history_okx.json", &resp)
	if resp.Code != "0" {
		t.Fatalf("the recording carries code %s %q", resp.Code, resp.Msg)
	}
	if len(resp.Data) < 2 {
		t.Fatalf("the recording has %d candles", len(resp.Data))
	}
	// The premise of the backwards pagination: newest first, so the OLDEST is
	// the last element and that is where the next `after` comes from.
	if resp.Data[0][0] <= resp.Data[len(resp.Data)-1][0] {
		t.Error("this recording is not newest-first; the fetcher's paging assumes it is")
	}

	rows, err := parseOKXCandles(resp.Data, "okx_futures", exchangestest.WidePriceWindow)
	if err != nil {
		t.Fatal(err)
	}
	candles, err := exchanges.FinishPriceHistory("okx_futures", symbol, rows)
	if err != nil {
		t.Fatal(err)
	}
	exchangestest.AssertCandlesSane(t, candles, "okx_futures")

	// vol is in CONTRACTS here and the conversion needs the registry, so the
	// parser must leave it at 0 — NOT KNOWN — rather than publish a contract
	// count under a name that says coins.
	for i, candle := range candles {
		if candle.BaseVolumeCoin != 0 {
			t.Errorf("candle %d carries volume %g; OKX quotes it in contracts and this package cannot convert",
				i, candle.BaseVolumeCoin)
		}
	}
}
