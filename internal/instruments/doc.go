// Package instruments holds the instrument registry: trading rules fetched
// from each venue once per day and cached in memory.
//
// It answers three questions the rest of the system must never guess at:
//
//   - How do I round an order? (tickSize, stepSize, minNotional)
//   - How much is one unit? (contract size / multiplier — OKX, Gate and Kraken
//     denominate orders in contracts, not coins)
//   - Does this perp have a matching spot market? (spot <-> perp mapping)
//
// The delta-neutral sizing helper lives here because it needs both legs' rules
// at once: round DOWN to the coarser stepSize of the two legs, then verify the
// result still clears minNotional on BOTH sides before any order is placed.
//
// Introduced in: PLAN.md phase 2, step 2.3-2.4.
package instruments
