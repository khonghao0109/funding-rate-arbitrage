package crossperp

import (
	"context"
	"fmt"
	"time"

	"futures-arbitrage-scanner/internal/broker/binance"
	"futures-arbitrage-scanner/internal/broker/bybit"
	"futures-arbitrage-scanner/internal/risk"
)

// The margin guard's venue readers. They live here, beside the credentials,
// because internal/risk must not import a broker: internal/strategy imports
// risk, and the step-3.5 gate's binary must not link a credential.

// BinanceAccountMargin is the one call the Binance reader needs;
// *binance.Client satisfies it.
type BinanceAccountMargin interface {
	FuturesAccountMargin(ctx context.Context) (binance.AccountMargin, error)
}

// BinanceMarginReader DERIVES the ratio from totalMaintMargin ÷
// totalMarginBalance: V3 publishes none (PLAN 4.5i correction 3).
type BinanceMarginReader struct {
	Venue   string
	Account BinanceAccountMargin
	Now     func() time.Time
}

func (r BinanceMarginReader) VenueName() string { return r.Venue }

// ReadMargin implements risk.MarginReader.
func (r BinanceMarginReader) ReadMargin(ctx context.Context) (risk.MarginReading, error) {
	m, err := r.Account.FuturesAccountMargin(ctx)
	if err != nil {
		return risk.MarginReading{}, err
	}
	if m.IsolatedPositions > 0 {
		return risk.MarginReading{}, fmt.Errorf("%w: %d vị thế trên %s dùng ký quỹ isolated — chúng bị thanh lý theo ví riêng, tỷ lệ tài khoản không mô tả chúng",
			risk.ErrMarginNotApplicable, m.IsolatedPositions, r.Venue)
	}
	return risk.DerivedMarginReading(r.Venue, "USDT (một tài sản) hoặc USD (đa tài sản) — theo chế độ tài khoản",
		m.TotalMaintMarginQuote, m.TotalMarginBalanceQuote,
		"SUY RA: totalMaintMargin ÷ totalMarginBalance (GET /fapi/v3/account)", nowMs(r.Now))
}

// BybitAccount is the two calls the Bybit reader needs; *bybit.Client
// satisfies it.
type BybitAccount interface {
	FetchAccountInfo(ctx context.Context) (bybit.AccountInfo, error)
	FetchWallet(ctx context.Context, coins ...string) (bybit.Wallet, error)
}

// BybitMarginReader reads the PUBLISHED accountMMRate, refuses an isolated
// account, and cross-checks the rate against the wallet's own two totals
// (risk.PublishedMarginReading).
type BybitMarginReader struct {
	Venue   string
	Account BybitAccount
	Now     func() time.Time
}

func (r BybitMarginReader) VenueName() string { return r.Venue }

// ReadMargin implements risk.MarginReader.
func (r BybitMarginReader) ReadMargin(ctx context.Context) (risk.MarginReading, error) {
	info, err := r.Account.FetchAccountInfo(ctx)
	if err != nil {
		return risk.MarginReading{}, err
	}
	if info.MarginMode == bybit.MarginModeIsolated {
		return risk.MarginReading{}, fmt.Errorf("%w: tài khoản %s ở %s — \"All account wide fields are not applicable to isolated margin\"",
			risk.ErrMarginNotApplicable, r.Venue, info.MarginMode)
	}
	w, err := r.Account.FetchWallet(ctx)
	if err != nil {
		return risk.MarginReading{}, err
	}
	return risk.PublishedMarginReading(r.Venue, "USD", w.AccountMMRateFrac, w.RatesPublished,
		w.TotalMaintenanceMarginUSD, w.TotalMarginBalanceUSD,
		"SÀN CÔNG BỐ: accountMMRate (GET /v5/account/wallet-balance, "+info.MarginMode+")", nowMs(r.Now))
}

func nowMs(now func() time.Time) int64 {
	if now == nil {
		return time.Now().UnixMilli()
	}
	return now().UnixMilli()
}
