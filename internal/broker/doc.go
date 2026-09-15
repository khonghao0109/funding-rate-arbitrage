// Package broker is the ONLY package that holds exchange credentials.
//
// It provides signed REST access: HMAC-SHA256 signing, recvWindow handling,
// server clock skew correction, and weight-aware rate limiting — step 4.1,
// accepted 2026-09-13 against both Binance testnets.
//
// Step 4.2 added the ORDER INTERFACE in order.go: Broker, with PlaceOrder,
// CancelOrder, GetOrder, OpenOrders, GetPosition and GetBalance. Defining it
// sends nothing; the implementations are internal/broker/binance (testnet) and
// internal/broker/brokertest (in memory, for step 4.4's unit tests).
//
// Two rules the interface carries, both there to be read before adding to it:
// every filled figure is GROSS, and the word "net" belongs to internal/strategy
// alone (CLAUDE.md rule 2); and every quantity is BASE COIN with the unit in
// the identifier (rule 4), because three of the nine venues denominate orders
// in contracts and the conversion belongs on the far side of the interface.
//
// # One credential pair per venue
//
// Measured 2026-09-13: Binance's USDⓈ-M futures testnet and its spot testnet
// are separate registrations, and a futures key sent to the spot host is
// refused -2015. CredentialsFromEnvAny reads each venue's own variables, with
// the original BINANCE_TESTNET_API_* names kept as the futures fallback.
//
// # Testnet only, and there is no switch
//
// NewClient refuses any base URL whose host is not in the documented testnet
// allow-list in hosts.go, and refuses plaintext. This is the step-4.1 limit the
// operator set on 2026-09-12 (PLAN §7.1 Q14): 4.1 may be built while the
// step-3.5 gate runs, on TESTNET credentials only. A mainnet host arrives at
// step 4.6, with the operator's decision and real capital behind it — and it
// arrives by adding an entry with the documentation URL that names it, never by
// widening the check to a prefix or suffix match. Two tests enforce this: one
// puts real hosts through the guard in both directions, and one reads this
// package's own source for a field or flag that would bypass it.
//
// # And no redirect, and no other host per request
//
// Choosing the host once is not enough: http.Client follows a 3xx by itself
// and re-sends a 307/308 with its method, its body and the X-MBX-APIKEY header
// to wherever Location points. Every client this package builds refuses every
// 3xx (ErrRedirectAttempted, reporting only its Location's scheme and host, and
// not its body) and refuses any request that is not https to its own base host
// (ErrHostNotPinned). A refused redirect says nothing about whether an order
// arrived, so it is ambiguous, never a refusal. See redirect.go; paid
// 2026-09-15, the debt PLAN 4.5b recorded as blocking 4.6.
//
// Nobody hands this package an *http.Client, a transport or a redirect policy:
// Config takes none, every client is built here, and the transport under the
// pin is each client's own (newVenueTransport) — never the process-wide
// http.DefaultTransport, which anything linked into a binary can rewire. The
// one transport hook,
// Config.TestTransport, runs BELOW the host pin — it receives the signed
// request — so NewClient refuses it outside a `go test` binary
// (ErrTestTransportOutsideTest) and boundary_test.go fails if a non-test file
// in the module names it. Production code that wants to see answers uses
// Config.ObserveResponse, which is handed method, path, status and body after
// the read, and nothing it could send with.
//
// # The boundary
//
// CLAUDE.md states it twice — package exchanges (public, read-only market data)
// must never import this package, and this package must never be reachable from
// the data ingestion path. Both are checked by machine in boundary_test.go: an
// AST walk over exchanges/, and `go list -deps` over every command in cmd/,
// which is the only check that sees an import three packages deep. The gate
// process of step 3.5 runs unattended for a fortnight; the binary it runs must
// not contain this code.
//
// cmd/brokercheck is the one command that links this package. It is a
// diagnostic, in the manner of cmd/fundingcheck: it prints what it found and
// sits on no data path.
//
// # Operational rules
//
//   - API keys are loaded from the environment (never from source, never from
//     config.yaml) and are never written to a log. Not redacted-in-a-log:
//     absent. Secret makes that structural rather than a habit — see its type
//     comment for the four doors it closes and the one Go leaves open.
//   - A credential's signature rides on the QUERY STRING, so every error in
//     this package names scheme, host and path only, and venue-supplied text is
//     scrubbed before it is stored in one. leak_test.go is the proof.
//   - Keys are provisioned with trading enabled and WITHDRAWAL DISABLED.
//
// Introduced in: PLAN.md phase 4, step 4.1-4.2.
package broker
