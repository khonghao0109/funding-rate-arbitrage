// Package history collects settled funding rates from the venues into the
// store (PLAN.md step 2.6).
//
// It is the layer between two packages that must not know about each other:
// exchanges owns the venue REST idioms and normalization, store owns SQLite and
// nothing else. Putting the orchestration in either would give one of them a
// responsibility it should not have — a persistence package that opens sockets,
// or a connector package that writes to a database.
//
// Two callers share it. cmd/backfill runs it once over a long window to build
// the phase-3 corpus; cmd/scanner runs it on a slow schedule so the corpus stays
// current without a cron job. They differ only in the window they ask for.
//
// Every result says how far the venue ACTUALLY reached, because three of the
// seven cannot answer a twelve-month request: OKX keeps about three months,
// Gate refuses a `from` older than 180 days, and Paradex has no settlements at
// all. A collector that reported "done" for all seven would leave phase 3
// backtesting a corpus weighted by retention policy.
//
// Introduced in: PLAN.md phase 2, step 2.6.
package history
