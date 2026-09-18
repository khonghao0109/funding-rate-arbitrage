package crossperp

import (
	"context"
	"errors"
	"math"
	"testing"

	"futures-arbitrage-scanner/internal/broker/binance"
	"futures-arbitrage-scanner/internal/broker/bybit"
	"futures-arbitrage-scanner/internal/risk"
)

type fakeBinanceAccount struct {
	m   binance.AccountMargin
	err error
}

func (f fakeBinanceAccount) FuturesAccountMargin(context.Context) (binance.AccountMargin, error) {
	return f.m, f.err
}

type fakeBybitAccount struct {
	mode string
	w    bybit.Wallet
}

func (f fakeBybitAccount) FetchAccountInfo(context.Context) (bybit.AccountInfo, error) {
	return bybit.AccountInfo{MarginMode: f.mode}, nil
}

func (f fakeBybitAccount) FetchWallet(context.Context, ...string) (bybit.Wallet, error) {
	return f.w, nil
}

func TestBinanceMarginReader_DerivesTheRatioAndRefusesIsolatedPositions(t *testing.T) {
	r := BinanceMarginReader{Venue: venueBinance, Account: fakeBinanceAccount{m: binance.AccountMargin{TotalMaintMarginQuote: 660, TotalMarginBalanceQuote: 1000}}}
	got, err := r.ReadMargin(context.Background())
	if err != nil || math.Abs(got.MaintenanceMarginRatioFrac-0.66) > 1e-12 || risk.DefaultMarginThresholds().TierOf(got.MaintenanceMarginRatioFrac) != risk.MarginTierRed {
		t.Fatalf("%+v, %v", got, err)
	}
	r.Account = fakeBinanceAccount{m: binance.AccountMargin{TotalMaintMarginQuote: 10, TotalMarginBalanceQuote: 1000, IsolatedPositions: 1}}
	if _, err := r.ReadMargin(context.Background()); !errors.Is(err, risk.ErrMarginNotApplicable) {
		t.Errorf("isolated: %v", err)
	}
	r.Account = fakeBinanceAccount{err: errTransport}
	if _, err := r.ReadMargin(context.Background()); !errors.Is(err, errTransport) {
		t.Errorf("a failed read: %v", err)
	}
}

func TestBybitMarginReader_ReadsThePublishedRateCrossCheckedAndRefusesIsolated(t *testing.T) {
	wallet := bybit.Wallet{RatesPublished: true, AccountMMRateFrac: 0.67, TotalMaintenanceMarginUSD: 6700, TotalMarginBalanceUSD: 10_000}
	r := BybitMarginReader{Venue: venueBybit, Account: fakeBybitAccount{mode: bybit.MarginModeRegular, w: wallet}}
	if got, err := r.ReadMargin(context.Background()); err != nil || got.MaintenanceMarginRatioFrac != 0.67 {
		t.Fatalf("%+v, %v", got, err)
	}
	r.Account = fakeBybitAccount{mode: bybit.MarginModeIsolated, w: wallet}
	if _, err := r.ReadMargin(context.Background()); !errors.Is(err, risk.ErrMarginNotApplicable) {
		t.Errorf("isolated: %v", err)
	}
	// The synthetic wallet internal/broker/bybit's own test uses publishes
	// accountMMRate 0.0045 beside totals giving 45 ÷ 1000.5 = 0.045: the two
	// disagree tenfold, which is the unit question nobody has measured live.
	inconsistent := bybit.Wallet{RatesPublished: true, AccountMMRateFrac: 0.0045, TotalMaintenanceMarginUSD: 45, TotalMarginBalanceUSD: 1000.5}
	r.Account = fakeBybitAccount{mode: bybit.MarginModeRegular, w: inconsistent}
	if _, err := r.ReadMargin(context.Background()); !errors.Is(err, risk.ErrMarginEvidenceConflict) {
		t.Errorf("a tenfold disagreement: %v, want ErrMarginEvidenceConflict", err)
	}
	r.Account = fakeBybitAccount{mode: bybit.MarginModeRegular, w: bybit.Wallet{RatesPublished: false}}
	if _, err := r.ReadMargin(context.Background()); !errors.Is(err, risk.ErrMarginNotPublished) {
		t.Errorf("a blank rate: %v", err)
	}
}
