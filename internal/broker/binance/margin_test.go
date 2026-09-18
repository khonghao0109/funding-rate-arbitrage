package binance

import (
	"strings"
	"testing"
)

// v3AccountExample is the Account Information V3 page's OWN example answer
// (read 2026-09-17), with one position row added in the shape parsePosition's
// comment lists. It is NOT a capture: no testnet answer of this endpoint's
// totals has been recorded, because the testnet account has never been read
// while holding a position. Replace it with a sanitized capture when one exists.
const v3AccountExample = `{
  "totalInitialMargin": "0.00000000",
  "totalMaintMargin": "0.00000000",
  "totalWalletBalance": "126.72469206",
  "totalUnrealizedProfit": "0.00000000",
  "totalMarginBalance": "126.72469206",
  "totalPositionInitialMargin": "0.00000000",
  "totalOpenOrderInitialMargin": "0.00000000",
  "totalCrossWalletBalance": "126.72469206",
  "totalCrossUnPnl": "0.00000000",
  "availableBalance": "126.72469206",
  "maxWithdrawAmount": "126.72469206",
  "assets": [],
  "positions": [
    {"symbol": "BTCUSDT", "positionSide": "BOTH", "positionAmt": "-0.010", "unrealizedProfit": "0", "isolatedMargin": "0", "notional": "-600", "isolatedWallet": "0", "initialMargin": "300", "maintMargin": "2.4", "updateTime": 1}
  ]
}`

func TestParseAccountMargin_ReadsTheTotalsTheGuardDerivesFrom(t *testing.T) {
	m, err := parseAccountMargin([]byte(strings.Replace(v3AccountExample,
		`"totalMaintMargin": "0.00000000"`, `"totalMaintMargin": "83.64"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if m.TotalMaintMarginQuote != 83.64 || m.TotalMarginBalanceQuote != 126.72469206 || m.AvailableBalanceQuote != 126.72469206 {
		t.Fatalf("%+v", m)
	}
	if m.IsolatedPositions != 0 {
		t.Errorf("a cross position (isolatedWallet 0) counted as isolated: %+v", m)
	}
}

func TestParseAccountMargin_AnIsolatedPositionIsCounted(t *testing.T) {
	m, err := parseAccountMargin([]byte(strings.Replace(v3AccountExample, `"isolatedWallet": "0"`, `"isolatedWallet": "150.5"`, 1)))
	if err != nil || m.IsolatedPositions != 1 {
		t.Fatalf("%+v, %v — want one isolated position", m, err)
	}
}

// Absent or unparseable is an error, never 0: a maintenance margin of 0 is an
// account reported healthy.
func TestParseAccountMargin_RefusesWhatWouldReadAsHealthy(t *testing.T) {
	for name, body := range map[string]string{
		"no totalMaintMargin":      strings.Replace(v3AccountExample, `"totalMaintMargin": "0.00000000",`, ``, 1),
		"blank totalMaintMargin":   strings.Replace(v3AccountExample, `"totalMaintMargin": "0.00000000"`, `"totalMaintMargin": ""`, 1),
		"no totalMarginBalance":    strings.Replace(v3AccountExample, `"totalMarginBalance": "126.72469206",`, ``, 1),
		"garbage margin balance":   strings.Replace(v3AccountExample, `"totalMarginBalance": "126.72469206"`, `"totalMarginBalance": "NaN"`, 1),
		"garbage isolated wallet":  strings.Replace(v3AccountExample, `"isolatedWallet": "0"`, `"isolatedWallet": "x"`, 1),
		"not the documented shape": `[]`,
	} {
		if m, err := parseAccountMargin([]byte(body)); err == nil {
			t.Errorf("%s: accepted %+v", name, m)
		}
	}
}
