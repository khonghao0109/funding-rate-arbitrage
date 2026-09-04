// Package store is the persistence layer (SQLite).
//
// It owns funding rate history, daily instrument snapshots, sampled price
// series, and — once trading begins — the position and trade ledger. It opens
// no sockets: collecting funding history from the venues is internal/history's
// job, and keeping the two apart is what stops a persistence package from
// growing a network dependency.
//
// Three tables, described in schema.sql, which is embedded and is the single
// description of the shape:
//
//   - funding_history — one SETTLED rate per (source, symbol, funding_at_ms).
//     That key is what makes the whole design restartable: a backfill and an
//     hourly top-up may re-fetch overlapping windows freely, and the overlap
//     costs an ignored insert instead of a duplicate settlement.
//   - price_snapshots — a periodic cross-section of the top of book, every row
//     of a round sharing one sampled_at_ms so a cross-venue spread stays a
//     lookup rather than a join on approximate times.
//   - instrument_snapshots — one day's trading rules per market.
//
// Instrument snapshots are versioned by day on purpose: when a venue changes a
// stepSize or a funding interval, the change must be visible in hindsight,
// otherwise a backtest silently reinterprets old data under new rules.
//
// Every column name carries its unit, exactly like the Go identifiers
// (docs/CONVENTIONS.md §1). These are not internal names — phase 8 reads this
// file from Python, and `rate_per_8h` next to `interval_sec` is how a reader
// ends up dividing a figure that was already divided.
//
// Retention: 12 months of funding, 3 months of price samples, instrument
// snapshots forever. A retention of 0 means KEEP EVERYTHING, never "delete
// everything" — an unset field in a YAML file must not be the instruction that
// empties a corpus three of the seven venues cannot refill.
//
// Introduced in: PLAN.md phase 2, step 2.6.
package store
