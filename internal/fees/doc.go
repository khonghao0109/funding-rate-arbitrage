// Package fees models the real cost of a round trip: maker/taker commission
// per venue and market type, withdrawal cost, and an estimated slippage term
// derived from order book depth.
//
// Every figure surfaced to a user or fed into a signal must say exactly which
// costs have been taken out of it. A gross spread is never presented as though
// it were what a trade keeps.
//
// What step 1.3 actually built is COMMISSION ONLY: a taker fill on all four
// legs of opening and closing a two-venue position. The result is "after
// trading fees" and must never be called net profit — slippage needs order book
// depth (phase 2) and funding is what phase 2 exists to collect. Withdrawal and
// transfer costs are not modelled either.
//
// A venue whose published schedule could not be read is marked unverified and
// produces NO figure at all. Zero is a real fee on some venues, so an unfilled
// entry must never be mistaken for a free one, and nothing downstream may
// compute a cost from an unverified schedule.
//
// Account-specific fee tiers (VIP level, 30-day volume) override the static
// table once credentials exist; until then the static table is a documented
// upper bound on cost for a fresh account, not a guess. Step 1.4 moves it to
// config.yaml so an operator can enter the rates their own account pays.
//
// Introduced in: PLAN.md phase 1, step 1.3.
package fees
