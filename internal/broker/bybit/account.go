package bybit

import (
	"context"
	"fmt"
	"strings"

	"futures-arbitrage-scanner/internal/broker"
)

// GetPosition implements broker.Broker, reading FROM THE VENUE (rule 7).
//
// GET /v5/position/list?category=linear&symbol=… — "If symbol passed, it
// returns data regardless of having position or not", so an EMPTY list is not
// "flat": it is an answer the documentation says does not happen, and it is an
// error. A zero Position would read as flat.
//
// Only one-way mode is accepted (positionIdx 0, the "System default"). A
// hedge-mode account lists a Buy side and a Sell side separately, and summing
// them into one signed quantity would hide a real long beside a real short.
func (c *Client) GetPosition(ctx context.Context, market broker.Market, symbol string) (broker.Position, error) {
	if err := c.checkMarket(market); err != nil {
		return broker.Position{}, err
	}
	if symbol == "" {
		return broker.Position{}, fmt.Errorf("%w: GetPosition needs a symbol", broker.ErrInvalidOrder)
	}
	var res struct {
		List []struct {
			PositionIdx   int    `json:"positionIdx"`
			Symbol        string `json:"symbol"`
			Side          string `json:"side"`
			Size          string `json:"size"`
			AvgPrice      string `json:"avgPrice"`
			MarkPrice     string `json:"markPrice"`
			UnrealisedPnl string `json:"unrealisedPnl"`
			LiqPrice      string `json:"liqPrice"`
			Leverage      string `json:"leverage"`
			UpdatedTime   string `json:"updatedTime"`
		} `json:"list"`
	}
	if err := c.getSigned(ctx, epPositionList, []broker.Param{
		{Key: "category", Value: categoryLinear}, {Key: "symbol", Value: symbol},
	}, &res); err != nil {
		return broker.Position{}, err
	}
	var rows int
	var out broker.Position
	for _, p := range res.List {
		if p.Symbol != symbol {
			continue
		}
		rows++
		if p.PositionIdx != 0 {
			return broker.Position{}, fmt.Errorf("bybit: %s is in HEDGE mode (positionIdx %d); this package reads one-way positions only and will not net two sides into one number", symbol, p.PositionIdx)
		}
		// "size: Position size, always positive"; "side: Buy: long, Sell:
		// short return an empty string "" for an empty position".
		size, err := parseRequiredNumber("size", p.Size)
		if err != nil {
			return broker.Position{}, err
		}
		switch {
		case p.Side == "Buy":
			out.QtyCoin = size
		case p.Side == "Sell":
			out.QtyCoin = -size
		case p.Side == "" && size == 0:
			out.QtyCoin = 0
		default:
			return broker.Position{}, fmt.Errorf("bybit: %s position has side %q with size %v — the two disagree, refused rather than guessed", symbol, p.Side, size)
		}
		fields := []struct {
			name string
			raw  string
			into *float64
		}{
			{"avgPrice", p.AvgPrice, &out.EntryPriceQuote},
			{"markPrice", p.MarkPrice, &out.MarkPriceQuote},
			{"unrealisedPnl", p.UnrealisedPnl, &out.UnrealizedPnLQuote},
			// "keeps "" when liqPrice <= minPrice or liqPrice >= maxPrice", and
			// for cross margin it "is an estimated price": 0 means NOT
			// PUBLISHED, never "cannot be liquidated".
			{"liqPrice", p.LiqPrice, &out.LiquidationPriceQuote},
			{"leverage", p.Leverage, &out.LeverageX},
		}
		for _, f := range fields {
			if *f.into, err = parseNumber(f.name, f.raw); err != nil {
				return broker.Position{}, err
			}
		}
		if out.UpdatedAtMs, err = parseMs("updatedTime", p.UpdatedTime); err != nil {
			return broker.Position{}, err
		}
	}
	switch rows {
	case 0:
		return broker.Position{}, fmt.Errorf("bybit: /v5/position/list returned no row for %s, which the documentation says it always does — refused rather than read as flat", symbol)
	case 1:
		out.Market, out.Symbol = market, symbol
		return out, nil
	}
	return broker.Position{}, fmt.Errorf("bybit: /v5/position/list returned %d rows for %s in one-way mode", rows, symbol)
}

// Wallet is the unified account as GET /v5/account/wallet-balance states it.
//
// Account-wide figures are in USD, not USDT — "totalEquity: Account total
// equity (USD)" — and the identifiers say so. "All account wide fields are not
// applicable to isolated margin", so on an isolated-margin account the two
// rates below say nothing about liquidation risk.
type Wallet struct {
	TotalEquityUSD            float64
	TotalWalletBalanceUSD     float64
	TotalMarginBalanceUSD     float64
	TotalAvailableBalanceUSD  float64
	TotalInitialMarginUSD     float64
	TotalMaintenanceMarginUSD float64

	// AccountIMRateFrac and AccountMMRateFrac are the venue's own ratios as
	// fractions (0.05 = 5%). They are READ, not derived. RatesPublished is
	// false when the venue sent either as "" — then both are 0 and mean NOT
	// STATED, which a margin guard must treat as unknown, never as healthy.
	AccountIMRateFrac float64
	AccountMMRateFrac float64
	RatesPublished    bool

	Coins []WalletCoin
}

// WalletCoin is one coin of the unified wallet, in that coin's units.
type WalletCoin struct {
	Coin                string
	EquityCoin          float64
	WalletBalanceCoin   float64
	LockedCoin          float64 // "Locked balance due to the Spot open order"
	TotalOrderIMCoin    float64 // "Pre-occupied margin for order"
	TotalPositionIMCoin float64
	TotalPositionMMCoin float64
	UnrealisedPnLCoin   float64
	USDValue            float64
}

// FetchWallet reads the unified wallet. availableToWithdraw and free are not
// read: both are marked Deprecated for accountType=UNIFIED.
//
// coins names coins to include even at zero. Without it the venue lists only
// "non-zero asset info", so an empty account answers an empty list — which a
// caller cannot tell apart from a decode that found nothing. Measured on the
// testnet 2026-09-17: an unfunded account answered 0 coins.
func (c *Client) FetchWallet(ctx context.Context, coins ...string) (Wallet, error) {
	type coinRow struct {
		Coin            string `json:"coin"`
		Equity          string `json:"equity"`
		WalletBalance   string `json:"walletBalance"`
		Locked          string `json:"locked"`
		TotalOrderIM    string `json:"totalOrderIM"`
		TotalPositionIM string `json:"totalPositionIM"`
		TotalPositionMM string `json:"totalPositionMM"`
		UnrealisedPnl   string `json:"unrealisedPnl"`
		UsdValue        string `json:"usdValue"`
	}
	var res struct {
		List []struct {
			AccountType            string    `json:"accountType"`
			AccountIMRate          string    `json:"accountIMRate"`
			AccountMMRate          string    `json:"accountMMRate"`
			TotalEquity            string    `json:"totalEquity"`
			TotalWalletBalance     string    `json:"totalWalletBalance"`
			TotalMarginBalance     string    `json:"totalMarginBalance"`
			TotalAvailableBalance  string    `json:"totalAvailableBalance"`
			TotalInitialMargin     string    `json:"totalInitialMargin"`
			TotalMaintenanceMargin string    `json:"totalMaintenanceMargin"`
			Coin                   []coinRow `json:"coin"`
		} `json:"list"`
	}
	params := []broker.Param{{Key: "accountType", Value: "UNIFIED"}}
	if len(coins) > 0 {
		params = append(params, broker.Param{Key: "coin", Value: strings.Join(coins, ",")})
	}
	if err := c.getSigned(ctx, epWalletBalance, params, &res); err != nil {
		return Wallet{}, err
	}
	if len(res.List) != 1 {
		return Wallet{}, fmt.Errorf("bybit: wallet-balance answered %d accounts for accountType=UNIFIED, expected 1", len(res.List))
	}
	a := res.List[0]
	if a.AccountType != "UNIFIED" {
		return Wallet{}, fmt.Errorf("bybit: wallet-balance answered accountType %q for a UNIFIED request", a.AccountType)
	}
	var w Wallet
	w.RatesPublished = strings.TrimSpace(a.AccountIMRate) != "" && strings.TrimSpace(a.AccountMMRate) != ""
	for _, f := range []struct {
		name, raw string
		into      *float64
	}{
		{"totalEquity", a.TotalEquity, &w.TotalEquityUSD},
		{"totalWalletBalance", a.TotalWalletBalance, &w.TotalWalletBalanceUSD},
		{"totalMarginBalance", a.TotalMarginBalance, &w.TotalMarginBalanceUSD},
		{"totalAvailableBalance", a.TotalAvailableBalance, &w.TotalAvailableBalanceUSD},
		{"totalInitialMargin", a.TotalInitialMargin, &w.TotalInitialMarginUSD},
		{"totalMaintenanceMargin", a.TotalMaintenanceMargin, &w.TotalMaintenanceMarginUSD},
		{"accountIMRate", a.AccountIMRate, &w.AccountIMRateFrac},
		{"accountMMRate", a.AccountMMRate, &w.AccountMMRateFrac},
	} {
		v, err := parseNumber(f.name, f.raw)
		if err != nil {
			return Wallet{}, err
		}
		*f.into = v
	}
	for _, row := range a.Coin {
		wc := WalletCoin{Coin: row.Coin}
		for _, f := range []struct {
			name, raw string
			into      *float64
		}{
			{"equity", row.Equity, &wc.EquityCoin},
			{"walletBalance", row.WalletBalance, &wc.WalletBalanceCoin},
			{"locked", row.Locked, &wc.LockedCoin},
			{"totalOrderIM", row.TotalOrderIM, &wc.TotalOrderIMCoin},
			{"totalPositionIM", row.TotalPositionIM, &wc.TotalPositionIMCoin},
			{"totalPositionMM", row.TotalPositionMM, &wc.TotalPositionMMCoin},
			{"unrealisedPnl", row.UnrealisedPnl, &wc.UnrealisedPnLCoin},
			{"usdValue", row.UsdValue, &wc.USDValue},
		} {
			v, err := parseNumber(f.name, f.raw)
			if err != nil {
				return Wallet{}, err
			}
			*f.into = v
		}
		w.Coins = append(w.Coins, wc)
	}
	return w, nil
}

// GetBalance implements broker.Broker from the unified wallet.
//
// broker.Balance has two halves and a unified coin row has no field named
// either — "free" is Deprecated. So they are DERIVED, and the derivation is
// stated here rather than hidden:
//
//	Locked = locked (spot orders) + totalOrderIM + totalPositionIM
//	Free   = walletBalance − Locked
//
// Free can be NEGATIVE (a coin that is borrowed against, or margin exceeding the
// coin's own balance under cross collateral) and is not floored: a clamped zero
// would read as "empty" where the account is in fact short of that coin. For
// the account's real spending power read FetchWallet's
// TotalAvailableBalanceUSD, which is what the venue itself suggests in place of
// the deprecated availableToWithdraw.
func (c *Client) GetBalance(ctx context.Context, market broker.Market) ([]broker.Balance, error) {
	if err := c.checkMarket(market); err != nil {
		return nil, err
	}
	w, err := c.FetchWallet(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]broker.Balance, 0, len(w.Coins))
	for _, coin := range w.Coins {
		locked := coin.LockedCoin + coin.TotalOrderIMCoin + coin.TotalPositionIMCoin
		out = append(out, broker.Balance{
			Market: market, Asset: coin.Coin,
			FreeQtyCoin:   coin.WalletBalanceCoin - locked,
			LockedQtyCoin: locked,
		})
	}
	return out, nil
}

// APIKeyInfo is what GET /v5/user/query-api says about the key in use. The key
// itself (`apiKey`) is deliberately NOT decoded, and `secret` is "Always """.
type APIKeyInfo struct {
	ReadOnly    bool                // readOnly "1: Read only"
	Permissions map[string][]string // e.g. ContractTrade: [Order Position]
	IPsBound    []string
	ExpiredAt   string
	UTA         bool // uta "1: unified trade account"
}

// CanTradeContracts reports whether the key may place and manage linear
// orders: read-write, with ContractTrade holding both Order and Position.
func (k APIKeyInfo) CanTradeContracts() bool {
	if k.ReadOnly {
		return false
	}
	has := map[string]bool{}
	for _, p := range k.Permissions["ContractTrade"] {
		has[p] = true
	}
	return has["Order"] && has["Position"]
}

// CanWithdraw reports "Withdraw" inside the Wallet permission array — "Permission
// of wallet AccountTransfer, SubMemberTransfer(master account), … Withdraw(master
// account)". A key that can withdraw is refused by this project's rule
// (broker/doc.go: "Keys are provisioned with trading enabled and WITHDRAWAL
// DISABLED").
func (k APIKeyInfo) CanWithdraw() bool {
	for _, p := range k.Permissions["Wallet"] {
		if p == "Withdraw" {
			return true
		}
	}
	return false
}

// FetchAPIKeyInfo reads the key's own permissions.
func (c *Client) FetchAPIKeyInfo(ctx context.Context) (APIKeyInfo, error) {
	var res struct {
		ReadOnly    int                 `json:"readOnly"`
		Permissions map[string][]string `json:"permissions"`
		IPs         []string            `json:"ips"`
		ExpiredAt   string              `json:"expiredAt"`
		UTA         int                 `json:"uta"`
	}
	if err := c.getSigned(ctx, epQueryAPI, nil, &res); err != nil {
		return APIKeyInfo{}, err
	}
	if res.Permissions == nil {
		return APIKeyInfo{}, fmt.Errorf("bybit: query-api answered no permissions object — refused rather than read as 'no permissions'")
	}
	return APIKeyInfo{
		ReadOnly: res.ReadOnly == 1, Permissions: res.Permissions,
		IPsBound: res.IPs, ExpiredAt: res.ExpiredAt, UTA: res.UTA == 1,
	}, nil
}
