package exchanges

import "testing"

func TestStandardOf_AnUnknownVenueSymbolResolvesToNothing(t *testing.T) {
	symbols := []Symbol{
		{Standard: "BTCUSDT", Venue: "PF_XBTUSD"},
		{Standard: "ETHUSDT", Venue: "PF_ETHUSD"},
	}

	if got := StandardOf(symbols, "PF_XBTUSD"); got != "BTCUSDT" {
		t.Errorf("StandardOf(PF_XBTUSD) = %q, want BTCUSDT", got)
	}
	// Empty, never a guess: a connector uses this to drop a market it never
	// subscribed to, and any non-empty fallback would file that market's price
	// under some other symbol.
	if got := StandardOf(symbols, "PF_SOLUSD"); got != "" {
		t.Errorf("StandardOf(PF_SOLUSD) = %q, want the empty string", got)
	}
	if got := StandardOf(nil, "PF_XBTUSD"); got != "" {
		t.Errorf("StandardOf on no symbols = %q, want the empty string", got)
	}
}

func TestVenueSymbols_IsTheSubscriptionList(t *testing.T) {
	got := VenueSymbols([]Symbol{
		{Standard: "BTCUSDT", Venue: "BTC-USDT-SWAP"},
		{Standard: "ETHUSDT", Venue: "ETH-USDT-SWAP"},
	})

	want := []string{"BTC-USDT-SWAP", "ETH-USDT-SWAP"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d = %q, want %q", i, got[i], want[i])
		}
	}
}
