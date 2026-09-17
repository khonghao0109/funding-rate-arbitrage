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
// "Holds" means what the account holds, not what the orders filled. A venue
// that keeps a spot BUY's fee in the base coin (Bybit always; Binance unless
// fees are paid in BNB) puts Q × (1 − fee) in the wallet for an order of Q, so
// Open buys the spot leg GROSSED UP — Q ÷ (1 − fee) rounded up onto the spot
// grid — and judges it on the fee its FILLS state and the base balance's
// measured gain (Result.SpotHeldQtyCoin): within the tolerance the smaller one,
// past it ErrSpotEvidenceConflict with nothing more sent. A wallet that
// received less than the perp needs is cut to match or unwound, the
// unwind sells what the wallet received, and Close sells what the wallet holds
// within Config.MaxSpotBaseFeeFrac of the original buy (PLAN 4.5j, parts 1 and
// 2, 2026-09-17). A fee above that ceiling is refused before sizing
// (ErrSpotFeeUnhedged, from Intent.SpotBuyFeeInBaseFrac).
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
// fill. The cap is the BOOK'S OWN BEST PRICE on the side being taken, moved by
// Config.MaxSlippageBps — a buy caps at bestAsk x (1 + bps/10000), a sell
// floors at bestBid x (1 - bps/10000) — and the rounding is passive, so
// rounding can only tighten it. This is also why entry is not a plain MARKET
// order: a market order accepts any price, and the entire cost model
// (strategy.RoundTripCost, four taker fills) assumes a bounded one.
//
// The cap was derived from strategy.EstimateFill's ReachedOffsetPct in the
// first draft of 4.4a, and that was a DIRECTION ERROR, found while writing up
// what the design was unsure of rather than by a test. EstimateFill
// reconstructs a curve from two aggregate points and is deliberately
// PESSIMISTIC — the safe direction for a COST, because you plan for a worse
// fill than you get, and the UNSAFE direction for a CAP, because an estimate
// that believes the fill reaches far into the book writes a cap far from the
// touch and authorises a price nobody chose. EstimateFill still decides
// whether the trade is worth doing; it no longer decides what may be paid.
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
// ## Which order the legs go in
//
// Config.LegOrder is sequential-spot-first by default and can be parallel.
//
// Sequential spot-first is the default for a liquidation argument: if the
// second leg fails we hold the first one naked until the unwind completes, and
// a naked SPOT LONG cannot be liquidated while a naked PERP SHORT can. Its cost
// is that the naked window is as long as leg 2's whole timeout — ten seconds by
// default — and on an alt the basis moves inside ten seconds.
//
// Parallel shortens that window to the difference between two fills, and pays
// for it by having both legs live at once: a refusal on one arrives while the
// other is still working. Neither is obviously right, so both are measurable
// and Result.UnhedgedWindow is reported on every run. 4.4b measures both on a
// real venue; the default does not move before that measurement exists.
//
// Config.LegTimeout stays at ten seconds and stays a GUESS, written down as
// one: nothing has yet measured how long a leg takes to fill on the venue this
// will run against. An unmeasured constant that nobody can see becomes a fact
// by default, so it is a named parameter with a named default constant.
//
// ## Reducing to match, before unwinding
//
// When one leg stops short, the pair is first SHRUNK to what the short leg
// really holds — the larger leg sold back down to it — and only unwound if that
// cannot be done. 4.4a unwound unconditionally, and its own report called that
// the design it was least sure of: a 60% fill is a hedged position, merely
// smaller than asked for, and closing it pays a round trip to destroy
// something that works.
//
// Shrinking needs three things, all checked BEFORE anything is sent:
//
//  1. the KEPT size is still a legal, CLOSEABLE position on both venues — a
//     pair too small for a venue to trade is a pair we cannot get out of,
//     which is worse than not having it;
//  2. the REDUCTION order is placeable on the larger leg's venue;
//  3. the book absorbs that reduction inside MaxSlippageBps.
//
// Any of them failing falls back to unwinding to flat. The book check belongs
// to the reduction and NOT to the unwind, and the asymmetry is deliberate:
// reducing is optional, so it may refuse on a bad price; unwinding is
// mandatory, so it may not.
//
// A leg that fills BELOW the venue's own minimum notional cannot be KEPT, and
// it unwinds. It CAN be closed, and that difference is a rule of the venue
// stated in the text of its own error:
//
//	-4164 MIN_NOTIONAL: "Order's notional must be no smaller than 5.0
//	(unless you choose reduce only)"
//	https://developers.binance.com/docs/derivatives/usds-margined-futures/error-code
//
// Every closing order on USDⓈ-M is therefore sent reduceOnly, and
// broker.RoundRequest.ReduceOnly skips the minimum for it. This paragraph first
// said the opposite — that such a leg was stuck, un-closeable by any order —
// because the MIN_NOTIONAL filter's own description page states no exemption
// and was read on its own. The exemption lives on the error-code page, in a
// parenthesis, and the parenthesis is the rule. Worth recording as the shape of
// the mistake: a venue's rules are not all on the page named after them.
//
// Spot publishes no such exemption, so a spot remainder under its NOTIONAL
// filter genuinely is unreachable, and that case is still reported loudly with
// the quantity and the venue's own refusal.
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
//     the asset for reasons that have nothing to do with this intent, so an
//     absolute balance assertion would be meaningless.
//
// 4.4a therefore proved spot flatness from this process's OWN arithmetic, which
// is precisely what rule 7 warns against, and its report listed that as an open
// hole. It is closed with a SECOND, INDEPENDENT piece of evidence rather than a
// better single one:
//
//	A (the venue)  the base-asset balance, read before anything was placed and
//	               again after everything was closed, is back where it started
//	               within one step size
//	B (our books)  the closing order filled exactly what the opening order did
//
// Neither is sufficient alone. A moves for reasons that have nothing to do with
// us — another process, a deposit, a fee taken in the base asset. B is our own
// belief about our own orders, which is the thing under test. They are worth
// having together because they FAIL DIFFERENTLY.
//
// When they disagree, NOTHING is reconciled: both numbers are reported and the
// caller gets ErrFlatEvidenceConflict, which is a different sentinel from
// ErrUnwindIncomplete because it means a different thing — not "the unwind
// failed" but "we cannot tell whether it succeeded". Picking the reading that
// matches what we expected is how a wrong belief survives contact with
// evidence, and picking the venue's silently would hide a real bug in here.
//
// ## What is still deliberately not built
//
// When leg 1 falls short in sequential mode, leg 2 is never placed and the pair
// unwinds. It would also be possible to place leg 2 at the SMALLER size instead
// — the mirror of reducing to match — and that is genuinely better economics
// for the same reason. It is not done because it opens a NEW order after a
// failure rather than closing one, which is a different risk, and because it
// needs the whole plan re-made at the new size (a new cap, new rounding, new
// minimum checks) at a moment when one leg is already naked.
//
// The book is still a REST snapshot. Incremental WebSocket depth during entry
// is PLAN 4.4b/4.4c and CLAUDE.md rule 10's one exception; whether it is needed
// at all is a MEASUREMENT — if the worst slippage over a run of real fills
// sits inside MaxSlippageBps, the snapshot is enough at that size and the debt
// is named with its number rather than assumed.
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
// # Step 4.4b and 4.5 — measured on the venue, 2026-09-13
//
// Everything above was proven against a fake. PLAN's Q15 then allowed the same
// machine to run on Binance TESTNET while the step-3.5 gate continued, through
// cmd/execcheck, with every position typed by a person — there is no path from
// a live signal to an order here and there will not be before both 3.5 and 3.4.
// Closing is in close.go, which carries its own contract; the figures it
// reports are in realized.go.
//
// What the venue said, and what it is worth:
//
//   - 10 opens, 10/10 hedged, residual 0 coin on every one.
//   - The unhedged window is 464-628 ms placing SEQUENTIALLY and 313-387 ms in
//     PARALLEL. This is the number this file could previously only reason
//     about. Parallel is ~151 ms shorter and the default did not move: 151 ms
//     has to be weighed against both legs being live when one fails, and
//     nothing at a $65 size forces the choice.
//   - With leg 2 refused BY THE VENUE for real, leg 1 was flat again in
//     225-284 ms, the closing order itself taking 120-171 ms. PLAN 4.4's
//     acceptance says "within a few seconds".
//   - Worst slippage over 20 fills: 0.00 bps against a 10 bps cap, one fill
//     1.18 bps BETTER than the touch. So the marketable-limit design does what
//     it says. It does NOT establish that a REST snapshot is enough at a real
//     size: testnet's spot book returned the same best ask four opens running,
//     and $65 is swallowed by one level. 4.4c is a named debt with a trigger.
//
// And four things the venue taught that no fake could have:
//
//  1. A futures MARKET order's answer is an ACKNOWLEDGEMENT, not a result —
//     see settleOrder. Believing it left a naked leg.
//  2. MIN_NOTIONAL exempts reduceOnly, which this file had backwards.
//  3. A venue rejection was reaching here as ambiguous, because VenueError hid
//     the HTTP status that definiteRejection tests.
//  4. The closing fills' prices were not being recorded, so half the slippage
//     arithmetic was quietly missing and the pair's drift read as exactly 0.
//
// Introduced in: PLAN.md phase 4, step 4.3-4.5.
package execution
