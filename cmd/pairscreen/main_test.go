package main

import (
	"strings"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/depth"
	"futures-arbitrage-scanner/internal/instruments"
)

// A config with one candidate pair, one USDT spot and two perps: a USDT one
// and a USD one bridged by the same declaration config.yaml ships.
func screenFixtureConfig(spotFeeVerified bool) config.Config {
	return config.Config{
		Symbols: []config.Symbol{{Symbol: "DOGEUSDT", Base: "DOGE", Quote: "USDT"}},
		Sources: []config.Source{
			{Source: "binance_spot", Connector: "binance_spot", MarketType: "spot", QuoteAsset: "USDT", Tradable: true,
				Fee: config.Fee{MakerBps: 10, TakerBps: 10, Verified: spotFeeVerified}},
			{Source: "binance_futures", Connector: "binance_futures", MarketType: "perp", QuoteAsset: "USDT", Tradable: true,
				Fee: config.Fee{MakerBps: 2, TakerBps: 5, Verified: true}},
			{Source: "kraken_futures", Connector: "kraken_futures", MarketType: "perp", QuoteAsset: "USD", Tradable: true,
				SymbolFormat: "PF_{base}USD", Fee: config.Fee{MakerBps: 2, TakerBps: 5, Verified: true}},
		},
		Hedge: config.Hedge{QuoteEquivalents: [][]string{{"USD", "USDT"}}},
	}
}

func inst(symbol, source, marketType, quote string) exchanges.Instrument {
	return exchanges.Instrument{Symbol: symbol, NativeSymbol: symbol, Source: source, MarketType: marketType,
		BaseAsset: "DOGE", QuoteAsset: quote, Status: exchanges.StatusTrading, ContractSizeCoin: 1,
		StepSizeCoin: 1, MinQtyCoin: 1}
}

// A book deep enough that a 50k trip fits inside 0.5% on both sides: ten
// levels of 100,000 coins at ~0.20, i.e. ~$20k a level and $200k a side.
func book(symbol, source string) depth.Summary {
	var bids, asks []exchanges.DepthLevel
	for i := 0; i < 10; i++ {
		bids = append(bids, exchanges.DepthLevel{PriceQuote: 0.2000 - 0.0001*float64(i+1), QtyNative: 100000})
		asks = append(asks, exchanges.DepthLevel{PriceQuote: 0.2000 + 0.0001*float64(i+1), QtyNative: 100000})
	}
	return depth.Summarize(exchanges.DepthBook{Source: source, Symbol: symbol, Bids: bids, Asks: asks}, 1, nil)
}

func TestScreenRows_AnUnlistedPerpIsNamedNotDropped(t *testing.T) {
	cfg := screenFixtureConfig(true)
	insts := []exchanges.Instrument{inst("DOGEUSDT", "binance_spot", "spot", "USDT"), inst("DOGEUSDT", "binance_futures", "perp", "USDT")}
	rows := screenRows(cfg, insts, hedgeMapping(cfg, insts), nil, 50000, time.Unix(0, 0), "")
	if len(rows) != 2 {
		t.Fatalf("want one row per perp source, got %d", len(rows))
	}
	kraken := rows[1]
	if kraken.PerpSource != "kraken_futures" || kraken.Listed || kraken.RefusalVI == "" {
		t.Fatalf("kraken has no instrument and must be reported unlisted with a reason: %+v", kraken)
	}
	if rows[0].RefusalVI != "" || !rows[0].Listed {
		t.Fatalf("binance is listed and must not carry a refusal: %+v", rows[0])
	}
}

func TestScreenRows_BridgedLegIsFlaggedAndTheTripIsPriced(t *testing.T) {
	cfg := screenFixtureConfig(true)
	insts := []exchanges.Instrument{
		inst("DOGEUSDT", "binance_spot", "spot", "USDT"),
		inst("DOGEUSDT", "binance_futures", "perp", "USDT"),
		inst("DOGEUSDT", "kraken_futures", "perp", "USD"),
	}
	books := []depth.Summary{book("DOGEUSDT", "binance_spot"), book("DOGEUSDT", "binance_futures"), book("DOGEUSDT", "kraken_futures")}
	rows := screenRows(cfg, insts, hedgeMapping(cfg, insts), books, 50000, time.Unix(0, 0), "")
	byPerp := map[string]Row{}
	for _, r := range rows {
		byPerp[r.PerpSource] = r
	}
	b, k := byPerp["binance_futures"], byPerp["kraken_futures"]
	if b.QuoteBridged || k.SpotSource != "binance_spot" || !k.QuoteBridged {
		t.Fatalf("USDT perp must pair plainly and the USD perp must pair BRIDGED: binance=%+v kraken=%+v", b.QuoteBridged, k.QuoteBridged)
	}
	for _, r := range []Row{b, k} {
		if !r.Cost.OK || r.Cost.TotalPct <= 0 {
			t.Fatalf("%s: a priced trip must carry a positive cost: %+v", r.PerpSource, r.Cost)
		}
		if r.MinDepthWithinWideQuote <= 0 {
			t.Fatalf("%s: the thinnest side must be reported from the books: %+v", r.PerpSource, r.MinDepthWithinWideQuote)
		}
	}
}

func TestScreenRows_AnUnverifiedFeeRefusesTheCostRatherThanCostingZero(t *testing.T) {
	cfg := screenFixtureConfig(false)
	insts := []exchanges.Instrument{inst("DOGEUSDT", "binance_spot", "spot", "USDT"), inst("DOGEUSDT", "binance_futures", "perp", "USDT")}
	books := []depth.Summary{book("DOGEUSDT", "binance_spot"), book("DOGEUSDT", "binance_futures")}
	rows := screenRows(cfg, insts, hedgeMapping(cfg, insts), books, 50000, time.Unix(0, 0), "")
	r := rows[0]
	if r.Cost.OK || r.Cost.TotalPct != 0 || r.Cost.ReasonVI == "" {
		t.Fatalf("an unverified spot fee must refuse with a reason, never price 0: %+v", r.Cost)
	}
}

func TestScreenRows_OnlyNarrowsToOnePair(t *testing.T) {
	cfg := screenFixtureConfig(true)
	cfg.Symbols = append(cfg.Symbols, config.Symbol{Symbol: "ADAUSDT", Base: "ADA", Quote: "USDT"})
	rows := screenRows(cfg, nil, instruments.HedgeMapping{}, nil, 50000, time.Unix(0, 0), "ADAUSDT")
	for _, r := range rows {
		if r.Symbol != "ADAUSDT" {
			t.Fatalf("-symbol must drop every other pair, got %s", r.Symbol)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("want ADAUSDT × 2 perps, got %d rows", len(rows))
	}
	if !strings.Contains(rows[0].RefusalVI, "niêm yết") {
		t.Fatalf("no instruments at all must read as unlisted: %q", rows[0].RefusalVI)
	}
}
