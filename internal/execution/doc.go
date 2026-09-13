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
// # Step 4.4a — the two-leg open, designed against a fake broker
//
// This is the design, written before the code, as PLAN's Q12 ordering note
// permits: the partial-fill state machine may be designed and unit-tested
// against a FAKE broker while the step-3.5 gate runs, and may not be marked
// done before the gate's verdict. Nothing here has placed a two-leg position
// on a real venue. What is proven is the state machine, against
// internal/broker/brokertest, with no network and no credential.
//
// ## The invariant, and it is the only one
//
// When Open returns, for any reason whatsoever — success, refusal, venue
// error, cancelled context, panic-free early exit — the account is in exactly
// one of two states:
//
//	BOTH OPEN   both legs hold quantity, and |qtySpot - qtyPerp| is at most
//	            the COARSER of the two venues' step sizes, with that residual
//	            worth less than the minimum notional on both venues — i.e.
//	            small enough that no venue would let us trade it away even if
//	            we wanted to.
//	BOTH FLAT   neither leg holds anything this call opened.
//
// There is no third state. In particular there is no "one leg open, will fix
// it on the next tick": an unhedged leg is a directional bet the strategy
// never authorised, and the time between ticks is exactly when the price
// moves. Every return path in Open is enumerated in openOutcome and every one
// of them is covered by a test that asserts the invariant rather than
// asserting the path.
//
// The residual tolerance is not slack for sloppiness. Two venues publish
// different step sizes (DATA-REQUIREMENTS §5), so a quantity valid on one is
// not always valid on the other, and the best achievable equality is bounded
// below by the coarser grid. Requiring exact equality would make some pairs
// impossible to hedge at all.
//
// ## Sizing, before anything is sent
//
// Target notional -> instruments.SizeDeltaNeutral (the step-2.3 rules, not a
// second copy of them) -> broker.RoundOrder on EACH venue's own grid -> take
// the SMALLER of the two rounded quantities as the common quantity -> re-round
// that on the other venue -> check the invariant would hold -> only then place
// anything.
//
// Taking the smaller and re-rounding matters: rounding each leg independently
// is exactly how a "delta-neutral" position starts life unbalanced, and the
// second rounding can move the number again, so the check has to come after
// it and not before. If either leg lands under its venue's minimum notional,
// the whole intent is REFUSED by name. It is never topped up — raising the
// size trades more than the strategy asked for, and raising only one leg
// breaks the hedge while looking like a fix.
//
// ## The book is re-read immediately before placing
//
// A signal is generated against a book that is already history by the time an
// order could reach the venue. So the entry cost is re-estimated with
// strategy.EstimateFill at the REAL rounded quantity, against a book sampled
// now, and compared with the cost that was current when the signal was made.
// If it has widened by more than MaxEntryCostWidenBps, the intent is abandoned
// and NOTHING is placed. Entering at a worse price than the signal assumed is
// how a backtested edge becomes a live loss, and this is the one moment where
// refusing costs nothing at all.
//
// This is the REST-snapshot version. PLAN 4.4 calls for incremental WebSocket
// depth during entry and exit — the only place in the roadmap that needs a
// realtime book (§7.4, and CLAUDE.md rule 10) — and that is 4.4b, not here.
// The snapshot's staleness is therefore a real limitation of 4.4a and is
// reported on the result rather than hidden: BookAgeMs says how old the book
// that authorised the entry was.
//
// ## Intents, and why the order ids are derived rather than generated
//
// An Intent carries an id. Each leg's ClientOrderID is derived
// DETERMINISTICALLY from that id and the leg's name, so the same intent always
// produces the same two ids and two different intents never collide.
//
// That is not tidiness, it is the foundation of step 5.3. A process that is
// killed between placing a leg and recording that it did so comes back knowing
// only the intent id — and from the intent id alone it can reconstruct both
// ClientOrderIDs and ask the venue what happened to them. An id generated at
// random, or handed out by the venue, is an id a restarted process cannot ask
// about, and the position it names becomes invisible.
//
// ## Placing, and the ambiguity that has to be resolved rather than guessed
//
// The legs are placed SEQUENTIALLY, spot first, and the order is deliberate.
// If the first leg opens and the second fails, we hold the first leg naked
// until the unwind completes. A naked SPOT LONG cannot be liquidated — we own
// the coin outright. A naked PERP SHORT can. So the leg whose failure leaves
// the safer exposure goes first, and the window where we are unhedged is a
// window in which nothing can be forcibly closed against us.
//
// Both legs are MARKETABLE LIMIT orders: priced through the touch so they take
// liquidity like a market order, but with a cap beyond which they will not
// fill. The cap comes from the entry-cost estimate that just authorised the
// trade, so the order cannot execute at a price the decision did not allow.
// The rounding is passive — a buy cap rounds DOWN, a sell floor rounds UP —
// which is the direction that keeps the fill inside the authorised cost. This
// is also why entry is not a plain MARKET order: a market order accepts any
// price, and the entire cost model (strategy.RoundTripCost, four taker fills)
// assumes a bounded one.
//
// Each leg has its own timeout, a parameter. When a leg has not filled enough
// by its deadline, the remainder is cancelled and then — always, without
// exception — the order is READ BACK FROM THE VENUE with GetOrder before any
// decision is taken. A cancel can race a fill: the venue may have filled the
// order in the microseconds before the cancel arrived, and a process that
// believes its own cancel succeeded will unwind a position it still holds, or
// fail to unwind one it does. This re-read is the single line whose removal
// the property test is designed to catch.
//
// An ambiguous place — a timeout, a dropped connection — is resolved exactly
// as the step-4.2 contract says, and never by guessing:
//
//	GetOrder by the derived ClientOrderID
//	  err == nil                       it arrived; act on its status
//	  errors.Is(err, ErrOrderNotFound)  it never arrived; resending is safe
//	  any other error                   STILL AMBIGUOUS: do not resend, keep
//	                                    asking until the answer is one of the
//	                                    first two, or the deadline passes and
//	                                    the intent unwinds
//
// A resend happens at most MaxResendPerLeg times (1 by default) and only down
// the ErrOrderNotFound branch. The third branch is the one that gets deleted
// by somebody tidying up, and it is the only branch that is always right to be
// careful in.
//
// ## Unwinding
//
// When an intent cannot reach BOTH OPEN, whatever is filled is closed at once
// with a MARKET order, sized to the quantity that ACTUALLY FILLED — read from
// the venue, not the quantity that was intended. Sizing an unwind from the
// intended notional is how a partial fill becomes an opposite position.
//
// Confirming flatness is not symmetric between the legs, and pretending it is
// would be a lie worth avoiding:
//
//   - The PERP leg has a position at the venue, so GetPosition answers the
//     question directly and the invariant is asserted on the venue's number.
//   - The SPOT leg has no position, only a balance — and the account may hold
//     the asset for reasons that have nothing to do with this intent. An
//     absolute balance assertion would be meaningless. So the proof there is
//     that the closing order filled the SAME quantity the opening order did,
//     read back from the venue by its own derived id. The balance is fetched
//     and reported, but it is a REPORT, not the proof.
//
// ## What is deliberately not built in 4.4a
//
// When one leg reaches the target and the other stops short, this unwinds to
// flat. It does NOT reduce the larger leg to match the smaller one, which
// would keep a smaller hedged position alive and is genuinely the better
// economics. It is left out because it needs a second minimum-notional check,
// a second book check at the new size, and a second chance to fail halfway —
// and the invariant is far easier to prove when the only outcomes are
// full size or flat. Revisit it in 4.4b with the incremental book, where the
// information needed to re-size is actually available.
//
// ## Recording
//
// Every state transition goes through Recorder. In 4.4a the only
// implementation is in memory, and the tests assert on the sequence it
// captures. Writing transitions to the store is 4.4b: the step-3.5 gate is
// writing data/scanner.db right now and no schema may move under it.
//
// ## Figures
//
// Everything reported is GROSS (CLAUDE.md rule 2): quantity, average fill
// price, and the venue's own commission where the venue states one. Nothing in
// this package says "net" — only internal/strategy may, and only about a
// figure that has had all four fills and the measured book deducted.
//
// Before any of this, risk.Evaluate prices the perp leg against that venue's
// maintenance bracket. An unverified bracket is REFUSED, exactly as
// internal/strategy refuses an unverified fee schedule: a maintenance rate
// nobody looked up is not zero, and treating it as zero puts the liquidation
// price further away than the venue would ever allow.
//
// Introduced in: PLAN.md phase 4, step 4.3-4.5.
package execution
