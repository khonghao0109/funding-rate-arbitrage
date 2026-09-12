package okx

import (
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// OKX's nextFundingRate arrives EMPTY under method=current_period (measured
// 2026-09-04, and the recording carries two such frames). Parsed as 0 it would
// publish "the next period pays nothing" with HasFollowingRate=true — this
// pins the ""-is-absent rule, which no other test asserted.
func TestOKXEmptyNextFundingRateIsAbsentNotZero(t *testing.T) {
	symbols := []exchanges.Symbol{{Standard: "BTCUSDT", Venue: "BTC-USDT-SWAP"}}

	frame := func(nextFundingRate string) []byte {
		return []byte(`{"arg":{"channel":"funding-rate","instId":"BTC-USDT-SWAP"},"data":[{` +
			`"instId":"BTC-USDT-SWAP","fundingRate":"0.0000663322867090",` +
			`"fundingTime":"1788508800000","nextFundingTime":"1788537600000",` +
			`"nextFundingRate":"` + nextFundingRate + `",` +
			`"minFundingRate":"-0.00375","maxFundingRate":"0.00375",` +
			`"settState":"settled","ts":"1788487507788"}]}`)
	}

	r := exchangestest.NewRecorder(t)
	_, _ = handleOKXFunding("okx_futures", symbols, r.Feeds, frame(""), time.Now())
	readings := r.Fundings()
	if len(readings) != 1 {
		t.Fatalf("published %d readings, want 1", len(readings))
	}
	if readings[0].HasFollowingRate {
		t.Fatal(`nextFundingRate "" published HasFollowingRate=true — an absent forecast became "pays nothing"`)
	}
	if readings[0].FollowingRateFrac != 0 {
		t.Fatalf("FollowingRateFrac = %g behind a false flag, want the zero value", readings[0].FollowingRateFrac)
	}

	_, _ = handleOKXFunding("okx_futures", symbols, r.Feeds, frame("0.0000512345678901"), time.Now())
	readings = r.Fundings()
	if len(readings) != 1 {
		t.Fatalf("published %d readings, want 1", len(readings))
	}
	if !readings[0].HasFollowingRate || readings[0].FollowingRateFrac != 0.0000512345678901 {
		t.Fatalf("real forecast: HasFollowingRate=%v FollowingRateFrac=%g, want true and the parsed value",
			readings[0].HasFollowingRate, readings[0].FollowingRateFrac)
	}
}
