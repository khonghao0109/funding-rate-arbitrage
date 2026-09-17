package broker

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// The only base URLs this package will talk to, and there is no flag to add
// one. Phase 4.6 is where a mainnet host is introduced, behind the operator's
// own decision and real capital; until then a typo, a copied snippet or a
// helpful "just point it at prod to check" cannot reach an account with money
// in it, because the host is not in this table.
//
// Each entry is quoted from Binance's own documentation:
//
//   - USDⓈ-M futures: "The REST base url for testnet is
//     https://demo-fapi.binance.com"
//     https://developers.binance.com/docs/derivatives/usds-margined-futures/general-info
//   - Spot: "The base endpoint is https://testnet.binance.vision/api"
//     https://developers.binance.com/docs/binance-spot-api-docs/testnet/rest-api/general-api-information
//
// UNVERIFIED, deliberately absent: `testnet.binancefuture.com` appears in
// search results and in older third-party material as a Binance futures testnet
// host, but no official documentation page reachable from this environment on
// 2026-09-12 states it, so it is not in the table (CLAUDE.md rule 5). If a key
// is issued against that host, add it here WITH the doc URL that names it —
// do not widen the check to a suffix match to make it fit.
const (
	BinanceFuturesTestnetHost = "demo-fapi.binance.com"
	BinanceSpotTestnetHost    = "testnet.binance.vision"

	BinanceFuturesTestnetBaseURL = "https://" + BinanceFuturesTestnetHost
	BinanceSpotTestnetBaseURL    = "https://" + BinanceSpotTestnetHost
)

// SigningScheme is how a venue authenticates a signed request. It is part of
// the host guard: each scheme has its own allow-list, so a Bybit client cannot
// be pointed at a Binance host or the reverse, and a host added for one venue
// widens nothing for the other.
type SigningScheme string

const (
	// SchemeBinanceQuery signs the query string (or form body) and appends a
	// `signature` parameter; the key rides in X-MBX-APIKEY. The zero value.
	SchemeBinanceQuery SigningScheme = ""
	// SchemeBybitV5Header signs timestamp + key + recv_window + payload and
	// sends everything in X-BAPI-* headers (bybit_sign.go).
	SchemeBybitV5Header SigningScheme = "bybit_v5_header"
)

func (s SigningScheme) name() string {
	if s == SchemeBinanceQuery {
		return "binance_query"
	}
	return string(s)
}

// Bybit's non-production hosts (PLAN 4.5i). Quoted in bybit_hosts.go with the
// documentation that names them.
//
// The production hosts — api.bybit.com, api.bytick.com and the regional ones —
// are absent by construction, and a test names them in the refusal list.

// testnetHosts is the allow-list, per scheme. A map so the check is exact: a
// host is in the set or it is not, with no prefix, suffix or "contains"
// matching — those are how `demo-fapi.binance.com.attacker.example` or
// `nottestnet.binance.vision` get through a guard that looked right.
var testnetHosts = map[SigningScheme]map[string]bool{
	SchemeBinanceQuery: {
		BinanceFuturesTestnetHost: true,
		BinanceSpotTestnetHost:    true,
	},
	SchemeBybitV5Header: {
		BybitTestnetHost: true,
		BybitDemoHost:    true,
	},
}

// TestnetHosts lists the BINANCE allowed hosts, sorted — what it meant before
// Bybit arrived, and what every existing caller (the Binance binaries' banners,
// the portal's status, and the guard tests asserting a Binance market resolves
// to an allowed host) still means. A union of both venues would let those guards
// pass for a Binance market wired to a Bybit host. Use TestnetHostsFor for any
// other scheme.
func TestnetHosts() []string { return TestnetHostsFor(SchemeBinanceQuery) }

// TestnetHostsFor lists one scheme's allowed hosts, sorted.
func TestnetHostsFor(scheme SigningScheme) []string {
	out := make([]string, 0, len(testnetHosts[scheme]))
	for h := range testnetHosts[scheme] {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// checkTestnetBaseURL is the guard NewClient runs before anything else.
//
// It requires HTTPS: a credential that signs over plaintext is a credential
// posted publicly, and every host here serves HTTPS.
func checkTestnetBaseURL(raw string, scheme SigningScheme) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		// Not quoted, and neither is url.Parse's error, which repeats the URL:
		// an unparseable URL may still carry userinfo.
		return errors.New("broker: base URL does not parse — not quoted, since it may carry userinfo")
	}
	// Checked first, and never echoed: userinfo is a credential in a URL, and
	// the messages below quote the URL. The client pins every request to this
	// URL's host, so a query, a fragment or another port would either be
	// dropped silently or make every request fail the pin.
	if u.User != nil {
		return fmt.Errorf("broker: base URL %s carries userinfo — refused, a base URL holds scheme and host only", redactURL(strings.TrimSpace(raw)))
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("broker: base URL %s carries a query or a fragment — refused, a base URL holds scheme and host only", redactURL(strings.TrimSpace(raw)))
	}
	if port := u.Port(); port != "" && port != "443" {
		return fmt.Errorf("broker: base URL %s names port %s — the testnet hosts serve https on 443 only", redactURL(strings.TrimSpace(raw)), port)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("broker: base URL %q must be https (got scheme %q) — a signed request over plaintext publishes the credential", raw, u.Scheme)
	}
	allowed, known := testnetHosts[scheme]
	if !known {
		return fmt.Errorf("broker: signing scheme %q is not one this package knows", scheme.name())
	}
	if !allowed[u.Hostname()] {
		return fmt.Errorf(
			"broker: base URL host %q is not a TESTNET or DEMO host for scheme %s; this package talks to non-production hosts only and there is no flag to change that (allowed: %s). A mainnet host arrives at step 4.6",
			u.Hostname(), scheme.name(), strings.Join(TestnetHostsFor(scheme), ", "))
	}
	return nil
}
