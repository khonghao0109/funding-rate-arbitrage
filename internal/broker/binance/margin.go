package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"futures-arbitrage-scanner/internal/broker"
)

// AccountMargin is GET /fapi/v3/account's account-wide margin totals, as the
// venue states them, for the cross-venue margin guard (internal/risk).
//
// The unit is the account's asset mode — "USDT only in single-asset mode;
// USD-denominated in multi-assets mode" (Account Information V3,
// https://developers.binance.com/docs/derivatives/usds-margined-futures/account/rest-api/Account-Information-V3,
// read 2026-09-17) — and every total comes from ONE answer, so they share it.
//
// V3 publishes no margin RATIO (PLAN 4.5i spec correction 3). A caller derives
// maintenance ÷ margin balance and says it derived it.
type AccountMargin struct {
	TotalMaintMarginQuote   float64
	TotalMarginBalanceQuote float64
	TotalInitialMarginQuote float64
	TotalWalletBalanceQuote float64
	AvailableBalanceQuote   float64

	// IsolatedPositions counts positions carrying their own isolated wallet (a
	// non-zero isolatedWallet — one of the V3 positions fields parsePosition's
	// comment lists). Such a position is liquidated against that wallet, not the
	// account, so an account-wide ratio says nothing about it.
	IsolatedPositions int
}

// FuturesAccountMargin reads the account's margin totals. Futures only.
func (c *Client) FuturesAccountMargin(ctx context.Context) (AccountMargin, error) {
	if c.market != broker.MarketFuturesUSDM {
		return AccountMargin{}, fmt.Errorf("%w: %q has no futures margin account", broker.ErrNotSupported, c.market)
	}
	var raw json.RawMessage
	if err := c.http.GetSigned(ctx, broker.FuturesAccount, nil, &raw); err != nil {
		return AccountMargin{}, classify(err)
	}
	return parseAccountMargin(raw)
}

// parseAccountMargin is strict where parseFloat is lenient, on purpose: an
// absent or unparseable maintenance margin read as 0 is an account reported
// HEALTHY, which is the most dangerous number this field can carry.
func parseAccountMargin(raw json.RawMessage) (AccountMargin, error) {
	var account struct {
		TotalInitialMargin *string `json:"totalInitialMargin"`
		TotalMaintMargin   *string `json:"totalMaintMargin"`
		TotalWalletBalance *string `json:"totalWalletBalance"`
		TotalMarginBalance *string `json:"totalMarginBalance"`
		AvailableBalance   *string `json:"availableBalance"`
		Positions          []struct {
			Symbol         string `json:"symbol"`
			IsolatedWallet string `json:"isolatedWallet"`
		} `json:"positions"`
	}
	if err := decode(raw, &account); err != nil {
		return AccountMargin{}, err
	}
	var out AccountMargin
	for _, f := range []struct {
		name string
		raw  *string
		into *float64
	}{
		{"totalMaintMargin", account.TotalMaintMargin, &out.TotalMaintMarginQuote},
		{"totalMarginBalance", account.TotalMarginBalance, &out.TotalMarginBalanceQuote},
		{"totalInitialMargin", account.TotalInitialMargin, &out.TotalInitialMarginQuote},
		{"totalWalletBalance", account.TotalWalletBalance, &out.TotalWalletBalanceQuote},
		{"availableBalance", account.AvailableBalance, &out.AvailableBalanceQuote},
	} {
		if f.raw == nil {
			return AccountMargin{}, fmt.Errorf("binance: /fapi/v3/account answered no %s — refused rather than read as 0", f.name)
		}
		v, err := strconv.ParseFloat(*f.raw, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return AccountMargin{}, fmt.Errorf("binance: /fapi/v3/account %s is %q, not a number", f.name, *f.raw)
		}
		*f.into = v
	}
	for _, p := range account.Positions {
		if p.IsolatedWallet == "" {
			continue
		}
		v, err := strconv.ParseFloat(p.IsolatedWallet, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return AccountMargin{}, fmt.Errorf("binance: /fapi/v3/account %s isolatedWallet is %q, not a number", p.Symbol, p.IsolatedWallet)
		}
		if v != 0 {
			out.IsolatedPositions++
		}
	}
	return out, nil
}
