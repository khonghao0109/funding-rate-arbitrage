// Package store is the persistence layer (SQLite).
//
// It owns funding rate history, daily instrument snapshots, sampled price
// series, and — once trading begins — the position and trade ledger.
//
// Instrument snapshots are versioned by day on purpose: when a venue changes a
// stepSize or a funding interval, the change must be visible in hindsight,
// otherwise a backtest silently reinterprets old data under new rules.
//
// Retention: 12 months of funding, 3 months of price samples.
//
// Introduced in: PLAN.md phase 2, step 2.6.
package store
