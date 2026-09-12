// Package broker is the ONLY package that holds exchange credentials.
//
// It provides signed REST access: HMAC-SHA256 signing, recvWindow handling,
// server clock skew correction, and weight-aware rate limiting. At step 4.1 it
// reads and nothing else — every method is a GET. The order interface
// (PlaceOrder, CancelOrder, GetPosition, GetBalance) is step 4.2 and is not
// here.
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
