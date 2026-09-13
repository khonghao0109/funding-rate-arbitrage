package execution

import (
	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/risk"
)

// The venue rules below are the REAL ones, read from Binance testnet's own
// exchangeInfo on 2026-09-13 (the same figures internal/broker/binance's
// rounding tests use). They matter here because the two legs' grids DIFFER by
// two decimal places, which is the whole reason the invariant has a tolerance:
//
//	spot BTCUSDT  tick 0.01  step 0.00001  minQty 0.00001  minNotional  5
//	perp BTCUSDT  tick 0.10  step 0.0001   minQty 0.0001   minNotional 50
//
// so the coarser step is the PERP's 0.0001, and no pair of orders can be closer
// in size than that.
func spotRules() exchanges.Instrument {
	return exchanges.Instrument{
		Symbol: "BTCUSDT", Source: "binance_spot", MarketType: "spot",
		Status: exchanges.StatusTrading, TickSizeQuote: 0.01,
		StepSizeCoin: 0.00001, MinQtyCoin: 0.00001, MaxQtyCoin: 9000,
		MinNotionalQuote: 5, ContractSizeCoin: 1,
		BaseAsset: "BTC", QuoteAsset: "USDT",
	}
}

func perpRules() exchanges.Instrument {
	return exchanges.Instrument{
		Symbol: "BTCUSDT", Source: "binance_futures", MarketType: "perp",
		Status: exchanges.StatusTrading, TickSizeQuote: 0.10,
		StepSizeCoin: 0.0001, MinQtyCoin: 0.0001, MaxQtyCoin: 1000,
		MinNotionalQuote: 50, ContractSizeCoin: 1,
		BaseAsset: "BTC", QuoteAsset: "USDT",
	}
}

// deepBook is a book that can absorb the test sizes comfortably. The spans
// exceed the wide window, so EstimateFill prices rather than refusing and does
// not mark the estimate a lower bound.
func deepBook(source string, mid float64, sampledAtMs int64) depth.Summary {
	return depth.Summary{
		Source: source, Symbol: "BTCUSDT",
		SampledAtMs:   sampledAtMs,
		MidPriceQuote: mid,
		BestBidQuote:  mid * 0.99995, BestAskQuote: mid * 1.00005,
		SpreadPct:                0.01,
		BidDepthWithinTightQuote: 400_000, AskDepthWithinTightQuote: 400_000,
		BidDepthWithinWideQuote: 3_000_000, AskDepthWithinWideQuote: 3_000_000,
		BidLevels: 100, AskLevels: 100,
		BidSpanPct: 0.8, AskSpanPct: 0.8,
	}
}

// thinBook holds almost nothing: EstimateFill refuses a real size against it.
func thinBook(source string, mid float64, sampledAtMs int64) depth.Summary {
	b := deepBook(source, mid, sampledAtMs)
	b.BidDepthWithinTightQuote, b.AskDepthWithinTightQuote = 200, 200
	b.BidDepthWithinWideQuote, b.AskDepthWithinWideQuote = 900, 900
	return b
}

// verifiedBracket is a maintenance schedule somebody actually looked up.
func verifiedBracket() risk.Bracket {
	return risk.Bracket{
		Source: "binance_futures", MaintenanceMarginFrac: 0.004,
		TierCeilingQuote: 300_000, MaxLeverage: 20, Verified: true,
	}
}

// testIntent is a well-formed intent both legs can satisfy.
func testIntent(nowMs int64) Intent {
	const mid = 60_000.0
	return Intent{
		ID:             "intent-test-0001",
		Symbol:         "BTCUSDT",
		SpotInstrument: spotRules(),
		PerpInstrument: perpRules(),
		SpotBook:       deepBook("binance_spot", mid, nowMs),
		PerpBook:       deepBook("binance_futures", mid, nowMs),
		SpotPriceQuote: mid,
		PerpPriceQuote: mid,
		NotionalQuote:  20_000,
		// Generous: the deep book above prices well inside this, so the
		// widening check passes unless a test deliberately narrows it.
		SignalEntryCostPct: 1.0,
		PerpMarginFrac:     0.10,
		PerpBracket:        verifiedBracket(),
	}
}
