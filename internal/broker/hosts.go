package broker

import (
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

// testnetHosts is the allow-list. A map so the check is exact: a host is in the
// set or it is not, with no prefix, suffix or "contains" matching — those are
// how `demo-fapi.binance.com.attacker.example` or `nottestnet.binance.vision`
// get through a guard that looked right.
var testnetHosts = map[string]bool{
	BinanceFuturesTestnetHost: true,
	BinanceSpotTestnetHost:    true,
}

// TestnetHosts lists the allowed hosts, sorted, for an error message and for
// the diagnostic command's banner.
func TestnetHosts() []string {
	out := make([]string, 0, len(testnetHosts))
	for h := range testnetHosts {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// checkTestnetBaseURL is the guard NewClient runs before anything else.
//
// It requires HTTPS: a credential that signs over plaintext is a credential
// posted publicly, and every host here serves HTTPS.
func checkTestnetBaseURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("broker: base URL %q does not parse: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("broker: base URL %q must be https (got scheme %q) — a signed request over plaintext publishes the credential", raw, u.Scheme)
	}
	if !testnetHosts[u.Hostname()] {
		return fmt.Errorf(
			"broker: base URL host %q is not a Binance TESTNET host; step 4.1 talks to testnet only and there is no flag to change that (allowed: %s). A mainnet host arrives at step 4.6",
			u.Hostname(), strings.Join(TestnetHosts(), ", "))
	}
	return nil
}
