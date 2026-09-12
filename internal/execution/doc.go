// Package execution opens and closes delta-neutral positions.
//
// A position is two legs placed together: spot long and perpetual short of
// equal notional. The hard part is not placing them — it is what happens when
// one leg fills and the other does not.
//
// Partial fill is the single most expensive failure mode in this system: the
// bot is left directionally exposed while believing it is hedged. Every code
// path here must resolve to either both legs open, or both legs closed. There
// is no third state, and "retry later" is not a resolution.
//
// Paper mode runs identical logic against a simulated ledger, so the only
// difference between paper and live is where orders are sent.
//
// # Paper mode and CLAUDE.md rule 7 (added at step 4.3)
//
// Rule 7 says a position is read from the VENUE and local bookkeeping is a
// cache assumed stale until reconciled. Paper mode is the ONE exception, and
// it is an exception by necessity, not by convenience: there is no venue
// holding a paper position, so internal/paper's ledger IS the source of truth
// for it. That exception ends exactly where paper mode ends. Nothing written
// for the paper ledger — reseeding a book from a journal, trusting a stored
// fill, summing a local balance — may be carried into live mode as a habit:
// the first thing a live path does with a position is ask the venue what it
// holds, and disagree with itself rather than with the venue.
//
// Introduced in: PLAN.md phase 4, step 4.3-4.5.
package execution
