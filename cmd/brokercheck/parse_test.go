package main

import (
	"encoding/json"
	"testing"
)

// The two account payloads, in the shapes the documentation describes. They are
// hand-built from the field lists on those pages rather than captured, because
// capturing one needs a credential — so this pins the FIELD NAMES the parser
// depends on, and nothing about a live account.
//
// Getting a name wrong here does not fail loudly: encoding/json leaves the
// field zero, the command prints "0 tài sản", and the acceptance looks like a
// venue problem instead of a typo.
//
// https://developers.binance.com/docs/derivatives/usds-margined-futures/account/rest-api/Futures-Account-Balance-V3
// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/account-endpoints

func TestParseFuturesBalance_ReadsTheDocumentedFields(t *testing.T) {
	raw := json.RawMessage(`[
	  {"accountAlias":"SgsR","asset":"USDT","balance":"122607.35137903","crossWalletBalance":"23.72469206",
	   "crossUnPnl":"0.00000000","availableBalance":"23.72469206","maxWithdrawAmount":"23.72469206",
	   "marginAvailable":true,"updateTime":1617939110373},
	  {"accountAlias":"SgsR","asset":"BNB","balance":"0.00000000","crossWalletBalance":"0.00000000",
	   "crossUnPnl":"0.00000000","availableBalance":"0.00000000","maxWithdrawAmount":"0.00000000",
	   "marginAvailable":true,"updateTime":1617939110373}
	]`)
	assets, nonZero, err := parseFuturesBalance(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(assets) != 2 || assets[0] != "USDT" || assets[1] != "BNB" {
		t.Errorf("assets = %v, want [USDT BNB] read from the `asset` field", assets)
	}
	if nonZero != 1 {
		t.Errorf("non-zero = %d, want 1 — only USDT carries a balance", nonZero)
	}
}

func TestParseSpotAccount_ReadsBalancesFreeAndLocked(t *testing.T) {
	raw := json.RawMessage(`{
	  "makerCommission":15,"takerCommission":15,"canTrade":true,"canWithdraw":false,"canDeposit":true,
	  "accountType":"SPOT","uid":354937868,"updateTime":123456789,
	  "balances":[
	    {"asset":"BTC","free":"4723846.89208129","locked":"0.00000000"},
	    {"asset":"LTC","free":"0.00000000","locked":"0.00000000"},
	    {"asset":"ETH","free":"0.00000000","locked":"2.50000000"}
	  ]}`)
	assets, nonZero, err := parseSpotAccount(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(assets) != 3 {
		t.Fatalf("assets = %v, want three read from `balances`", assets)
	}
	// LOCKED counts: a balance sitting in an open order is still balance, and
	// reading only `free` would report an account with everything on the book
	// as empty.
	if nonZero != 2 {
		t.Errorf("non-zero = %d, want 2 (BTC free, ETH locked)", nonZero)
	}
}

// An empty envelope must not read as a healthy account: the acceptance asks
// whether the venue answered with real state.
func TestParse_AnEmptyAnswerIsZeroAssetsNotAnError(t *testing.T) {
	assets, nonZero, err := parseFuturesBalance(json.RawMessage(`[]`))
	if err != nil || len(assets) != 0 || nonZero != 0 {
		t.Errorf("empty futures balance = %v, %d, %v; want no assets and no error", assets, nonZero, err)
	}
	assets, nonZero, err = parseSpotAccount(json.RawMessage(`{"balances":[]}`))
	if err != nil || len(assets) != 0 || nonZero != 0 {
		t.Errorf("empty spot account = %v, %d, %v; want no assets and no error", assets, nonZero, err)
	}
}

// A malformed body must not travel into the message this command prints.
func TestDecode_DoesNotEchoTheVenuesBody(t *testing.T) {
	const secretish = "THIS-LOOKS-LIKE-A-CREDENTIAL"
	err := decode(json.RawMessage(`{"balances": "`+secretish+`"}`), &struct {
		Balances []string `json:"balances"`
	}{})
	if err == nil {
		t.Fatal("a type mismatch must be an error")
	}
	if containsStr(err.Error(), secretish) {
		t.Errorf("the decode error echoed the venue's body: %q", err)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
