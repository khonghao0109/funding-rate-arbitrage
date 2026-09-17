package main

import (
	"context"
	"fmt"
	"os"

	"futures-arbitrage-scanner/internal/broker"
	binancebroker "futures-arbitrage-scanner/internal/broker/binance"
	bybitbroker "futures-arbitrage-scanner/internal/broker/bybit"
)

// Strategy 1 on Bybit (PLAN 4.5j): spot and linear on ONE Unified Trading
// Account, behind the same portal, the same execution machine and the same
// bot. Only the reads whose answer SHAPE differs are adapted here; every order
// method, and the optional capabilities execution looks for by type assertion
// (broker.TradeReader, FundingReader, MarkPriceReader), are the Bybit client's
// own, promoted through the embedded pointer — so what execution asserts on is
// the real implementation, not a wrapper that could drop one.

// bybitCredentialEnv names the one key pair. On a Unified account one key
// trades both markets (bybitcheck reads ContractTrade and Spot on the same key),
// unlike the two Binance testnets.
var bybitCredentialEnv = []broker.EnvPair{{KeyVar: "BYBIT_API_KEY", SecretVar: "BYBIT_API_SECRET"}}

// bybitVenue is one Bybit market as the portal's venue / perpVenue.
type bybitVenue struct {
	*bybitbroker.Client
}

var (
	_ venue                  = bybitVenue{}
	_ perpVenue              = bybitVenue{}
	_ broker.TradeReader     = bybitVenue{}
	_ broker.FundingReader   = bybitVenue{}
	_ broker.MarkPriceReader = bybitVenue{}
)

// dialBybitMarkets builds both markets over ONE signed transport: one host, one
// IP limit, one clock, one cool-down (bybit.Client.WithMarket). Two transports
// would each believe they owned the venue's whole request budget.
func dialBybitMarkets() markets {
	m := markets{profile: profileFor(venueBybit)}
	c, source, err := dialBybit()
	if err != nil {
		m.spotErr, m.perpErr = err, err
		return m
	}
	spot, err := c.WithMarket(broker.MarketSpot)
	if err != nil {
		m.spotErr, m.perpErr = err, err
		return m
	}
	m.perp, m.perpSourceVI = bybitVenue{c}, source
	m.spot, m.spotSourceVI = bybitVenue{spot}, source
	return m
}

func dialBybit() (*bybitbroker.Client, string, error) {
	creds, err := broker.CredentialsFromEnvAny(bybitCredentialEnv...)
	if err != nil {
		return nil, "", err
	}
	mode, err := bybitbroker.ResolveMode(os.Getenv("BYBIT_MODE"), os.Getenv("BYBIT_TESTNET"))
	if err != nil {
		return nil, "", err
	}
	cfg, err := bybitbroker.DefaultConfig(mode, creds)
	if err != nil {
		return nil, "", err
	}
	cfg.UserAgentVI = "funding-rate-arbitrage/execportal"
	// broker.NewClient refuses any host outside the Bybit scheme's allow-list
	// (api-testnet, api-demo). The portal takes no flag that could move it.
	c, err := bybitbroker.New(mode, broker.MarketFuturesUSDM, cfg)
	if err != nil {
		return nil, "", err
	}
	return c, fmt.Sprintf("%s (BYBIT_MODE=%s, một key cho cả spot và linear)", creds.SourceVI, mode), nil
}

// FetchInstrument carries Bybit's rules in the portal's rules value. There is no
// Bybit equivalent of BuyPriceFloorFrac read here, so it is 0 — "this market
// publishes no such limit" in that field's own words — and every order the
// portal sends is MARKET, which the band would not bind anyway.
func (v bybitVenue) FetchInstrument(ctx context.Context, symbol string) (binancebroker.MarketRules, error) {
	r, err := v.Client.FetchInstrument(ctx, symbol)
	if err != nil {
		return binancebroker.MarketRules{}, err
	}
	return binancebroker.MarketRules{Instrument: r.Instrument}, nil
}

// CommissionRates is this account's taker fee on the client's market.
func (v bybitVenue) CommissionRates(ctx context.Context, symbol string) (binancebroker.CommissionRates, error) {
	r, err := v.Client.CommissionRates(ctx, symbol)
	if err != nil {
		return binancebroker.CommissionRates{}, err
	}
	return binancebroker.CommissionRates{Market: r.Market, Symbol: r.Symbol,
		TakerBuyFrac: r.TakerBuyFrac, TakerSellFrac: r.TakerSellFrac, SourceVI: r.SourceVI}, nil
}

// FundingRateHistory answers the portal's [startMs, endMs] window. Bybit's
// endpoint is anchored on its END, so the range is walked backwards
// (FundingRateHistoryRange); a settlement's mark price and rate type are not
// published there and stay 0 and "".
func (v bybitVenue) FundingRateHistory(ctx context.Context, symbol string, startMs, endMs int64) ([]binancebroker.FundingRate, error) {
	rows, err := v.Client.FundingRateHistoryRange(ctx, symbol, startMs, endMs)
	if err != nil {
		return nil, err
	}
	out := make([]binancebroker.FundingRate, 0, len(rows))
	for _, r := range rows {
		out = append(out, binancebroker.FundingRate{Symbol: r.Symbol, SettledAtMs: r.SettledAtMs, RatePerIntervalFrac: r.RatePerIntervalFrac})
	}
	return out, nil
}

// FetchMaintenanceBracket is the risk-limit tier covering the notional.
func (v bybitVenue) FetchMaintenanceBracket(ctx context.Context, symbol string, notionalQuote float64) (binancebroker.MaintenanceBracket, error) {
	b, err := v.Client.FetchMaintenanceBracket(ctx, symbol, notionalQuote)
	if err != nil {
		return binancebroker.MaintenanceBracket{}, err
	}
	return binancebroker.MaintenanceBracket{Symbol: b.Symbol, Tier: b.Tier,
		NotionalFloorQuote: b.NotionalFloorQuote, NotionalCapQuote: b.NotionalCapQuote,
		MaintMarginFrac: b.MaintMarginFrac, MaxLeverage: b.MaxLeverage}, nil
}
