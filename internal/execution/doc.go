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
// Introduced in: PLAN.md phase 4, step 4.3-4.5.
package execution
