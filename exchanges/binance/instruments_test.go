package binance

import (
	"testing"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/exchangestest"
)

func TestParseBinanceInstruments_Golden(t *testing.T) {
	var futures binanceExchangeInfo
	exchangestest.LoadJSON(t, "instruments_binance_futures.json", &futures)
	got, err := parseBinanceInstruments(futures, "binance_futures", "perp",
		[]exchanges.Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}, {Standard: "XRPUSDT", Venue: "XRPUSDT"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d instruments, want 2", len(got))
	}
	// MaxQtyCoin is the SMALLER of LOT_SIZE.maxQty (limit cap) and
	// MARKET_LOT_SIZE.maxQty (market cap) — in the recording BTCUSDT is
	// 1000 vs 120 and XRPUSDT 10,000,000 vs 2,000,000: the market cap wins,
	// and it is the one the taker fills of this strategy are governed by.
	exchangestest.AssertInstrument(t, got[0], exchanges.Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTCUSDT", Source: "binance_futures",
		MarketType: "perp", Status: exchanges.StatusTrading, BaseAsset: "BTC", QuoteAsset: "USDT",
		TickSizeQuote: 0.10, StepSizeCoin: 0.001, MinQtyCoin: 0.001, MaxQtyCoin: 120,
		MinNotionalQuote: 50, ContractSizeCoin: 1,
	})
	exchangestest.AssertInstrument(t, got[1], exchanges.Instrument{
		Symbol: "XRPUSDT", NativeSymbol: "XRPUSDT", Source: "binance_futures",
		MarketType: "perp", Status: exchanges.StatusTrading, BaseAsset: "XRP", QuoteAsset: "USDT",
		TickSizeQuote: 0.0001, StepSizeCoin: 0.1, MinQtyCoin: 0.1, MaxQtyCoin: 2_000_000,
		MinNotionalQuote: 5, ContractSizeCoin: 1,
	})

	var spot binanceExchangeInfo
	exchangestest.LoadJSON(t, "instruments_binance_spot.json", &spot)
	gotSpot, err := parseBinanceInstruments(spot, "binance_spot", "spot",
		[]exchanges.Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotSpot) != 1 {
		t.Fatalf("parsed %d spot instruments, want 1", len(gotSpot))
	}
	// Spot's MARKET_LOT_SIZE cap is dynamic (a rolling average the venue
	// updates); the value here is whatever the recording froze, and far
	// tighter than LOT_SIZE's 9000.
	exchangestest.AssertInstrument(t, gotSpot[0], exchanges.Instrument{
		Symbol: "BTCUSDT", NativeSymbol: "BTCUSDT", Source: "binance_spot",
		MarketType: "spot", Status: exchanges.StatusTrading, BaseAsset: "BTC", QuoteAsset: "USDT",
		TickSizeQuote: 0.01, StepSizeCoin: 0.00001, MinQtyCoin: 0.00001, MaxQtyCoin: 149.14262137,
		MinNotionalQuote: 5, ContractSizeCoin: 1,
	})
}
