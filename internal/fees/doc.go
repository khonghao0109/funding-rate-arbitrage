// Package fees models the real cost of a round trip: maker/taker commission
// per venue and market type, withdrawal cost, and an estimated slippage term
// derived from order book depth.
//
// Every profit figure surfaced to a user or fed into a signal must be NET.
// Gross spread is never displayed on its own — it is not a number anyone can
// act on.
//
// Account-specific fee tiers (VIP level, 30-day volume) override the static
// table once credentials exist; until then the static table is a documented
// upper bound on cost, not a guess.
//
// Introduced in: PLAN.md phase 1, step 1.3.
package fees
