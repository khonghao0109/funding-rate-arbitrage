// Package strategy turns normalized market data into trade decisions.
//
// It computes net APR, scores how durable a funding rate has been across past
// periods, and emits entry and exit signals with the reasoning attached — a
// signal without a logged reason cannot be debugged after the fact.
//
// Named "strategy" rather than "signal" to avoid colliding with the standard
// library's os/signal, which the entrypoint needs for graceful shutdown.
//
// This package has exactly two callers: the live signal path and package
// backtest. Both must reach the same decision from the same inputs, so nothing
// here may depend on wall-clock time, network access, or process state — pass
// the evaluation time in rather than reading the clock.
//
// This package consumes only normalized values. It must never see a raw
// exchange payload, a rate whose interval is unknown, or a duration whose unit
// is ambiguous — normalization is the connector's job, not the caller's.
//
// Introduced in: PLAN.md phase 3, step 3.1-3.2.
package strategy
