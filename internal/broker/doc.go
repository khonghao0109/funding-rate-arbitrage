// Package broker is the ONLY package that holds exchange credentials.
//
// It provides signed REST access: HMAC signing, recvWindow handling, server
// clock skew correction, and per-venue weight-aware rate limiting.
//
// Boundary rule, enforced by review: package exchanges (public, read-only data)
// must never import this package, and this package must never be reachable from
// the data ingestion path. Public market data and credentials live on opposite
// sides of that line.
//
// Operational rules:
//   - API keys are loaded from the environment or a secret store, never from
//     source, and are never written to a log — not even redacted.
//   - Keys are provisioned with trading enabled and WITHDRAWAL DISABLED.
//
// Introduced in: PLAN.md phase 4, step 4.1-4.2.
package broker
