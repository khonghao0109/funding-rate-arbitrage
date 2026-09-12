package broker

import (
	"strings"
	"testing"
)

// Binance publishes worked signature examples with SAMPLE keys of its own —
// these are the venue's documentation, not anyone's credentials. Both are
// reproduced here because they come from the two different references this
// package targets and they pin two different things: that the key is the
// secret, and that the string signed is the parameter list IN THE ORDER SENT
// rather than sorted. url.Values.Encode would sort these and produce a
// different digest, which is why Param is an ordered slice.
//
// Spot:
// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/endpoint-security-type
// Futures:
// https://developers.binance.com/docs/derivatives/usds-margined-futures/general-info
func TestSign_ReproducesBinancesOwnDocumentedExamples(t *testing.T) {
	cases := []struct {
		name        string
		secret      string
		totalParams string
		want        string
	}{
		{
			name:   "spot reference example",
			secret: "NhqPtmdSJYdKjVHjA7PZj4Mge3R5YNiP1e3UZjInClVN65XAbvqqM6A7H5fATj0j",
			totalParams: "symbol=LTCBTC&side=BUY&type=LIMIT&timeInForce=GTC&quantity=1&price=0.1" +
				"&recvWindow=5000&timestamp=1499827319559",
			want: "c8db56825ae71d6d79447849e617115f4a920fa2acdcab2b053c4b2838bd6b71",
		},
		{
			name:   "usds-m futures reference example",
			secret: "2b5eb11e18796d12d88f13dc27dbbd02c2cc51ff7059765ed9821957d82bb4d9",
			totalParams: "symbol=BTCUSDT&side=BUY&type=LIMIT&quantity=1&price=9000&timeInForce=GTC" +
				"&recvWindow=5000&timestamp=1591702613943",
			want: "3c661234138461fcc7a7d8746c6558c9842d4e10870d2ecbedf7777cad694af9",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Sign(NewSecret(tc.secret), tc.totalParams); got != tc.want {
				t.Errorf("signature = %s, want %s (the venue's own documented result)", got, tc.want)
			}
		})
	}
}

// The same example again, this time built through Param/QueryString rather than
// handed in as a finished string: it proves the renderer produces the exact
// bytes the documented digest was taken over, which is the whole contract
// between QueryString and Sign.
func TestQueryString_RendersTheExactBytesTheDocumentedDigestCovers(t *testing.T) {
	params := []Param{
		{"symbol", "LTCBTC"}, {"side", "BUY"}, {"type", "LIMIT"}, {"timeInForce", "GTC"},
		{"quantity", "1"}, {"price", "0.1"}, {"recvWindow", "5000"}, {"timestamp", "1499827319559"},
	}
	secret := NewSecret("NhqPtmdSJYdKjVHjA7PZj4Mge3R5YNiP1e3UZjInClVN65XAbvqqM6A7H5fATj0j")
	const want = "c8db56825ae71d6d79447849e617115f4a920fa2acdcab2b053c4b2838bd6b71"

	if got := Sign(secret, QueryString(params)); got != want {
		t.Errorf("Sign(QueryString(params)) = %s, want %s — the renderer and the signer disagree about the bytes", got, want)
	}
	query := SignedQuery(secret, params)
	if !strings.HasSuffix(query, "&signature="+want) {
		t.Errorf("SignedQuery must end with the signature over everything before it; got %q", scrubHexSignature(query))
	}
}

// A value carrying & or = would otherwise be read by the venue as two
// parameters: a request that verifies correctly and means something else.
func TestQueryString_EncodesValuesThatWouldSplitTheQuery(t *testing.T) {
	got := QueryString([]Param{{"a", "1&b=2"}, {"c", "x y"}})
	if want := "a=1%26b%3D2&c=x+y"; got != want {
		t.Errorf("QueryString = %q, want %q", got, want)
	}
}

// redactURL is the second door on the signature. If it ever starts returning
// the query, every error in this package leaks.
func TestRedactURL_KeepsThePathAndDropsEverythingIdentifying(t *testing.T) {
	cases := map[string]string{
		"https://demo-fapi.binance.com/fapi/v3/balance?timestamp=1&signature=deadbeef": "https://demo-fapi.binance.com/fapi/v3/balance",
		"https://user:pw@testnet.binance.vision/api/v3/account?x=1#frag":               "https://testnet.binance.vision/api/v3/account",
		"https://demo-fapi.binance.com/fapi/v1/time":                                   "https://demo-fapi.binance.com/fapi/v1/time",
	}
	for raw, want := range cases {
		if got := redactURL(raw); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestScrubHexSignature_RemovesTheDigestWhereverItSurvived(t *testing.T) {
	in := "GET /fapi/v3/balance?timestamp=1&signature=c8db56825ae71d6d79447849e617115f4a920fa2acdcab2b053c4b2838bd6b71 failed"
	got := scrubHexSignature(in)
	if strings.Contains(got, "c8db5682") {
		t.Errorf("the digest survived scrubbing: %q", got)
	}
	if !strings.Contains(got, "signature="+Redacted) {
		t.Errorf("scrubbed form = %q, want the parameter kept and its value replaced", got)
	}
}
