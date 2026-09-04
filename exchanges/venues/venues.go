// Package venues is the registry of every venue integration: the four tables
// that map a config.yaml connector name to the venue package that implements
// it. It exists as its own package because the tables must import all eight
// venue packages while every venue package imports the exchanges kernel — put
// the tables in the kernel and the import cycle is immediate.
//
// This is the ONE place that knows the full venue list. The kernel knows only
// the shared types and lifecycle; a venue package knows only itself; and
// config.yaml supplies everything else about a source. Adding a venue is a new
// package under exchanges/ plus one entry per table here — cmd/scanner tests
// that these tables and config.yaml name the same set in both directions.
package venues

import (
	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/binance"
	"futures-arbitrage-scanner/exchanges/bybit"
	"futures-arbitrage-scanner/exchanges/gate"
	"futures-arbitrage-scanner/exchanges/hyperliquid"
	"futures-arbitrage-scanner/exchanges/kraken"
	"futures-arbitrage-scanner/exchanges/okx"
	"futures-arbitrage-scanner/exchanges/paradex"
	"futures-arbitrage-scanner/exchanges/pyth"
)

// Connectors is every connector that exists, by the name config.yaml uses in a
// source's `connector` field.
//
// This is the one thing configuration cannot supply: a venue nobody has written
// a connector for needs Go. Everything else about a source - its labels, its
// colours, its thresholds, its fees, and which symbols it serves - is data.
func Connectors() map[string]exchanges.ConnectFunc {
	return map[string]exchanges.ConnectFunc{
		"binance_futures":     binance.ConnectFutures,
		"binance_spot":        binance.ConnectSpot,
		"bybit_futures":       bybit.ConnectFutures,
		"bybit_spot":          bybit.ConnectSpot,
		"hyperliquid_futures": hyperliquid.ConnectFutures,
		"kraken_futures":      kraken.ConnectFutures,
		"okx_futures":         okx.ConnectFutures,
		"gate_futures":        gate.ConnectFutures,
		"paradex_futures":     paradex.ConnectFutures,
		"pyth":                pyth.ConnectPrices,
	}
}

// DepthFetchers maps connector names to their depth fetcher.
//
// Nine entries: every tradable source, spot as well as perp. The spot side is
// not optional — the exit of a funding position sells the spot leg, so the
// depth that matters most is the BID side of a market the perp-only view never
// looks at (docs/PLAN.md §7.4). Pyth has no entry: an oracle has no book.
func DepthFetchers() map[string]exchanges.DepthFetchFunc {
	return map[string]exchanges.DepthFetchFunc{
		"binance_futures":     binance.FetchFuturesDepth,
		"binance_spot":        binance.FetchSpotDepth,
		"bybit_futures":       bybit.FetchFuturesDepth,
		"bybit_spot":          bybit.FetchSpotDepth,
		"okx_futures":         okx.FetchDepth,
		"gate_futures":        gate.FetchDepth,
		"kraken_futures":      kraken.FetchDepth,
		"hyperliquid_futures": hyperliquid.FetchDepth,
		"paradex_futures":     paradex.FetchDepth,
	}
}

// InstrumentFetchers maps connector names (the same names Connectors uses) to
// their instrument fetcher. Pyth has no entry: an oracle has no instruments.
func InstrumentFetchers() map[string]exchanges.InstrumentFetchFunc {
	return map[string]exchanges.InstrumentFetchFunc{
		"binance_futures":     binance.FetchFuturesInstruments,
		"binance_spot":        binance.FetchSpotInstruments,
		"bybit_futures":       bybit.FetchFuturesInstruments,
		"bybit_spot":          bybit.FetchSpotInstruments,
		"okx_futures":         okx.FetchInstruments,
		"gate_futures":        gate.FetchInstruments,
		"kraken_futures":      kraken.FetchInstruments,
		"hyperliquid_futures": hyperliquid.FetchInstruments,
		"paradex_futures":     paradex.FetchInstruments,
	}
}

// FundingHistoryFetchers maps connector names to their history fetcher. Only
// perpetual sources appear: a spot market has no funding, and an oracle has
// neither.
func FundingHistoryFetchers() map[string]exchanges.FundingHistoryFetchFunc {
	return map[string]exchanges.FundingHistoryFetchFunc{
		"binance_futures":     binance.FetchFundingHistory,
		"bybit_futures":       bybit.FetchFundingHistory,
		"okx_futures":         okx.FetchFundingHistory,
		"gate_futures":        gate.FetchFundingHistory,
		"kraken_futures":      kraken.FetchFundingHistory,
		"hyperliquid_futures": hyperliquid.FetchFundingHistory,
		"paradex_futures":     paradex.FetchFundingHistory,
	}
}
