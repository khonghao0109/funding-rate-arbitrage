// Package risk keeps positions survivable.
//
// Responsibilities: margin and liquidation distance monitoring with tiered
// alerts, delta drift detection and rebalancing, hard capital limits per symbol
// and per venue, the emergency kill switch, and state recovery after a crash.
//
// Recovery rule: on startup, positions are read back from the VENUE, never
// restored from local state. Local bookkeeping is a cache and is assumed stale
// or wrong until reconciled.
//
// Liquidation price is driven by the maintenance margin rate for the current
// notional bracket, not by the leverage setting a user picked. Sizing up can
// move a position into a stricter bracket without any other change.
//
// Introduced in: PLAN.md phase 5.
package risk
