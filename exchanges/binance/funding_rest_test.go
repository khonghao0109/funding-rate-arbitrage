package binance

import (
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

// Binance funding comes from REST, so its golden fixture is a REST response.
func TestParseBinanceFunding_Golden(t *testing.T) {
	var premium []binancePremiumIndex
	exchangestest.LoadJSON(t, "funding_binance_premium_index.json", &premium)
	var infos []binanceFundingInfo
	exchangestest.LoadJSON(t, "funding_binance_info.json", &infos)

	symbols := []exchanges.Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}, {Standard: "ETHUSDT", Venue: "ETHUSDT"}}
	meta := exchanges.NewFundingMetaCache()
	meta.Put(binanceFundingMeta(infos, symbols))

	r := exchangestest.NewRecorder(t)
	recvAt := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	if err := publishBinanceFunding(r.Feeds, "binance_futures", symbols, meta, premium, recvAt); err != nil {
		t.Fatal(err)
	}

	readings := r.Fundings()
	if len(readings) != 2 {
		t.Fatalf("got %d readings, want one per configured symbol", len(readings))
	}
	for _, got := range readings {
		if got.Source != "binance_futures" || got.RawRateField != "lastFundingRate" {
			t.Errorf("identity = %+v", got)
		}
		if got.IntervalSec != 8*3600 && got.IntervalSec != 4*3600 && got.IntervalSec != 3600 {
			t.Errorf("%s: IntervalSec = %d, want one of the intervals Binance actually runs (8h/4h/1h)",
				got.Symbol, got.IntervalSec)
		}
		if got.NextFundingAtMs == 0 {
			t.Errorf("%s: premiumIndex always carries nextFundingTime", got.Symbol)
		}
		if got.MarkPrice <= 0 || got.IndexPrice <= 0 {
			t.Errorf("%s: mark %v index %v, both are in this payload", got.Symbol, got.MarkPrice, got.IndexPrice)
		}
		if !got.HasCap {
			t.Errorf("%s: fundingInfo carries the cap and floor", got.Symbol)
		}
		if got.VenueTimeMs == 0 {
			t.Errorf("%s: premiumIndex carries the venue's own time", got.Symbol)
		}
	}
}

// A symbol fundingInfo omits must default to 8h, never inherit another
// symbol's interval or come out zero — the endpoint documents itself as
// listing only symbols that DIFFER from the default.
func TestBinanceFundingMeta_DefaultsTheOmitted(t *testing.T) {
	symbols := []exchanges.Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}, {Standard: "XRPUSDT", Venue: "XRPUSDT"}}
	meta := binanceFundingMeta([]binanceFundingInfo{
		{Symbol: "BTCUSDT", FundingIntervalHours: 4,
			AdjustedFundingRateCap: "0.00300", AdjustedFundingRateFloor: "-0.00300"},
	}, symbols)

	if got := meta["BTCUSDT"]; got.IntervalHours != 4 || !got.HasCap || got.CapFrac != 0.003 {
		t.Errorf("BTCUSDT = %+v, want the endpoint's 4h override with its cap", got)
	}
	got, ok := meta["XRPUSDT"]
	if !ok {
		t.Fatal("a symbol fundingInfo omits must still get an entry — otherwise it never publishes")
	}
	if got.IntervalHours != binanceDefaultFundingIntervalHours {
		t.Errorf("omitted symbol: IntervalHours = %d, want the documented default %d",
			got.IntervalHours, binanceDefaultFundingIntervalHours)
	}
	if got.HasCap {
		t.Error("a symbol with no fundingInfo entry has no published cap")
	}
}
