// Package crossperp opens and closes Engine 2's position: a LONG perpetual on
// one venue and a SHORT perpetual of the same coin quantity on another
// (docs/designs/cross-perp-engine-design.md §3). Written 2026-09-17, PLAN step
// 4.5k, and proven against internal/broker/brokertest ONLY — no order from this
// package has reached any venue, testnet included.
//
// # The invariant is internal/execution's, with two perps
//
// When Open or Close returns, the account is in one of two states:
//
//	BOTH OPEN  the long venue holds +q and the short venue −q', with |q − q'| at
//	           most the COARSER of the two venues' steps and that residual worth
//	           less than either venue's minimum notional
//	BOTH FLAT  neither venue holds anything of this pair
//
// or the call says, LOUDLY, that it cannot prove either: every such error wraps
// execution.ErrUnwindIncomplete (or execution.ErrFlatEvidenceConflict), so a
// caller that alarms on Engine 1's failures alarms on these, and the Engine
// keeps the symbol's coordinator lock so nothing opens beside a state nobody can
// name. There is no "one leg open, fix it next tick".
//
// # Opening: both legs at once
//
// Engine 1 places spot first because a naked spot long cannot be liquidated.
// Here BOTH legs are liquidatable perps on separate margin accounts, so neither
// order is safer, and the lever left is the length of the unhedged window: the
// two opening legs go from two goroutines at the same moment, and
// Result.UnhedgedWindow reports the gap between the two fills on this process's
// clock. A leg the venue refuses outright, or whose send loses its answer, stops
// the other at once — its marketable limit is cancelled and read back instead of
// working the rest of LegTimeout while nothing hedges it — and a leg whose
// partner already stopped is not sent at all. Binance-only parallel placement
// measured 313–387 ms on testnet (execution/doc.go); cross-venue adds a second
// host and Bybit's asynchronous acknowledgement, and nothing has measured it yet.
//
// Before the first order, the executor marks the coordinator lock as "orders
// sent", on disk (coordinator.MarkOrdersSent): from then on the lock can only be
// released by reading the venues.
//
// # Sizing on the coarser grid, and float dust
//
// Step_common = max(Step_A, Step_B), REFUSED when the coarser step is not a
// whole multiple of the finer (a quantity on one grid would then not be on the
// other). Q = broker.FloorToStep(NotionalQuote ÷ P_mid, Step_common), with P_mid
// the mean of the two books' mids, and Q × P_mid must reach the larger of the
// two venues' minimum notionals — the design's three formulas. Each leg then
// goes through broker.RoundOrder on its OWN venue's rules, which must return Q
// unchanged. FloorToStep snaps onto the grid's decimals, so the float the venue
// clients render with strconv.FormatFloat(v, 'f', -1, 64) has no IEEE-754 tail
// (0.3, never 0.30000000000000004) — the lesson of bbfe12f, tested here.
//
// # Marketable limits, not market orders, to open
//
// Long caps at bestAsk × (1 + MaxSlippageBps), short floors at bestBid ×
// (1 − MaxSlippageBps), rounded passively — Engine 1's accepted design and its
// reason: the cost model assumes a bounded fill. A leg works until LegTimeout,
// then its remainder is cancelled and the order is READ BACK until the venue
// says it can no longer change (Status.Done). Bybit cancels asynchronously, so a
// read-back that merely returns is not a final fill — PLAN 4.5i correction 9,
// which this package pays for itself rather than inheriting.
//
// # A timeout is not a refusal, an opening order is never resent, and a lost one is never called absent
//
// The design says "leg B rejected OR timed out → unwind leg A". A refusal and a
// timeout are different facts. A dropped connection, Bybit's ErrOrderNotVisible,
// Binance's HTTP 408 ("a timeout has occurred while waiting for a response from
// the backend server"), -1000, -1006 and -1007 ("execution status unknown")
// leave leg B possibly FILLED. A duplicate client id — Binance -4116, Bybit
// 110072 — is proof an order EXISTS.
//
// What they share is the operator's rule for Engine 2: a leg whose partner was
// refused or cut off by the network is taken back to flat within 500 ms. So a
// lost send is not asked about first. Leg A stops at once — and returns at once,
// its own order still unconfirmed, rather than waiting for its cancel to be read
// back (review 4.5k round 3, M3) — and settleAbortedOpen drives BOTH legs to zero
// immediately while, in parallel, it cancels and asks the venue about every
// unconfirmed order by its deterministic ClientOrderID, for AmbiguousSendQuiet
// (recvWindow plus a second) counted from when B's send RETURNED — not from when
// it left, which a slow send would have used up (review 4.5k, N2). A leg whose
// order is still being asked about is re-read every PollEvery and driven to zero
// whenever it holds something, so a lost order that reaches the venue and fills
// during the asking is taken back at once (round 3, M2); an order seen resting is
// cancelled again (round 3, B1). When the asking ends, both legs are driven to
// zero once more.
// Leg B is NEVER resent (review 4.5k, B3): Binance can answer -2013 before its
// backend has processed a request, and its newClientOrderId is unique only "among
// open orders", so a resend of an original that has since filled is accepted as a
// SECOND order.
//
// And B is never concluded absent. "No such order" at any time proves nothing —
// Binance documents no bound on when an "execution status unknown" request is
// processed, and Bybit never answers "not found" at all — which is how the
// execution portal already reads an OPENING order it cannot see. A B that is
// still unseen leaves the flat result LOUD (ErrLegAmbiguous): the Engine keeps the
// lock and records the intent as an unresolved pair whose opening orders are
// unproven, the margin guard sees whatever the venues hold for it, every Close
// sends cancels for both opening ids before it reduces, reads them back before its
// verdict and closes whatever they filled (round 3, M5), and only the venue
// reading them back finished, or a person (Engine.ConfirmOrdersFinished), lets a
// close release the lock. The cost is written down: a send that never reached the
// venue needs a person — the broker hands a network failure over as text, so even
// a dial that provably sent nothing is ambiguous here — and a pair whose answer
// was merely lost is unwound rather than kept.
//
// The 500 ms is this machine's own work, asserted on in-memory venues
// (TestOpen_ShortLegLostOnTheNetwork_LongFlatWithin500msAndResultLoud and the
// round-3 tests beside it). What bounds it on a real venue, besides the venues'
// round trips: a connection that hangs is noticed only when the broker's HTTP
// timeout fires; a position that trails a KNOWN fill is waited for up to
// PositionSettleTimeout; a lost order's late fill is seen at the next watch read,
// PollEvery later. Binance's same-venue unwind measured 225–284 ms on testnet
// (execution/doc.go); nothing cross-venue has been measured.
//
// definiteRejection here differs from internal/execution's on purpose; that
// package still reads every 4xx — 408 included — as a refusal and resends after
// an immediate -2013. Both are latent defects for Engine 1, recorded in PLAN
// 4.5k and not fixed here: it is accepted code on a running portal.
//
// # Shrinking a leg: reduce-only MARKET, sized from the VENUE
//
// Every order that shrinks a leg — a cut, an unwind, a close — is a MARKET order
// with reduceOnly, sized from the venue's own position read that round (rule 7),
// never from this process's memory of what filled, and named by the CALL's
// attempt nonce plus its round: a second Close must not reuse the first one's ids
// (review 4.5k, B1 — Bybit refuses a reused orderLinkId and reads back the old
// order). Those orders never rest, so no restarted process needs their ids.
//
// Toward ZERO, an ambiguous round is followed by a new one: reduce-only cannot
// open or flip a position — "true means your position can only reduce in size"
// (Bybit create-order); Binance refuses one that cannot with -2022
// REDUCE_ONLY_REJECT — so a late duplicate at zero is refused. Toward a size
// ABOVE zero that is not true, two reductions of the same size can both
// execute, so a cut whose order is not proven finished stops and the pair unwinds
// to zero instead (review 4.5k, B2). An IP cool-down stops a leg at once:
// retrying inside a ban lengthens it.
//
// # Closing and unwinding: one leg after the other
//
// Two legs closed at once leave ONE naked leg whenever either venue refuses — a
// cool-down, a key error, an IOC that expires on a thin book (review 4.5k, M2).
// So a close, and the unwind of an open whose orders are all proven finished,
// take the FIRST leg to zero and then the SECOND down to what the first really
// holds afterwards, each on a budget of its own. That protects against the FIRST
// venue failing — the pair stays whole — and not against the SECOND failing after
// the first closed, which leaves one leg, loud, exactly as two at once would. What
// the ordering costs is time: the whole first close is unhedged, and
// CloseResult.UnhedgedWindow measures it. The design asked for a simultaneous
// close; its own emergency rule — close on the venue with the higher maintenance
// ratio first — is this ordering, and CloseRequest.FirstVenue is how the margin
// guard names that venue. An unwind closes the larger leg first. An open with an
// unproven order flattens both legs at once instead (above): reduce-only toward
// zero can neither open nor flip anything, and there is no hedge left to protect.
//
// A first leg whose position cannot be read leaves the second untouched. A first
// leg with a reduce-only order that may still execute is taken as it READS: that
// order can only shrink it, so the second leg cut to the reading never undershoots
// it, where leaving the second leg whole would leave the whole difference naked
// (review 4.5k, N5).
//
// A close's verdict is loud — unresolved, ErrLegAmbiguous — whenever something
// that may still execute stands beside the positions it read: a reduce-only order
// not proven finished next to legs still open (two equal legs are not a hedge if
// one may still shrink, review 4.5k, N1), an opening order not proven finished, or
// any order resting on the symbol (N3).
//
// Flat legs beside a pending reduce-only order are not a pair to release either:
// the order cannot open anything, but it reduces WHATEVER the venue holds when it
// executes — the next engine's position once the lock is gone (round 3, M1). So a
// flat verdict, of a close or of an open's unwind, first reads every such order
// back: finished is proven; still working is cancelled and waited for; not shown
// at all counts as absent only once AmbiguousSendQuiet has passed since its send
// returned — the execution portal's reading of an order that is not an OPENING
// order, and a recvWindow bound. An order the venue SHOWED, or whose send it
// answered "execution status unknown", is never taken as absent: the result is
// loud, the pair keeps it in PendingOrders and the lock stays. The residual risk
// is written down, not hidden: a request that reached a venue's backend with no
// answer at all and executes later than that quiet period reduces whatever is
// there by then.
//
// MARKET rather than a capped limit because the thing being avoided is a pair
// left half-closed in the very market that moved against it; at 65% maintenance
// the venue's liquidation fee and the loss of the hedge both cost more than a few
// basis points of slippage.
//
// Reducing to match — shrinking the larger leg to the smaller — is tried before
// unwinding, as in internal/execution: a pair filled to 60% on one venue is a
// hedged position, merely smaller. The kept size must be closeable on both
// venues WITHOUT any reduce-only minimum exemption (Binance documents one, Bybit
// linear's has not been read), and the book must absorb the cut inside
// MaxSlippageBps. A pair next to an order that may still fill is never reduced.
//
// Close never re-hedges, and when the orders and the venues' positions disagree
// NOTHING more is sent: a close sized from a position that may be the wrong one
// can itself open a naked leg. Both are loud and left to a person.
//
// # The Engine: the lock, the pairs, and a restart
//
// Engine takes the coordinator lock before opening and gives it back only when
// the venues read flat and quiet, every opening order of the intent is proven
// finished and every reduce-only order is proven finished or unseen past its
// quiet period. Every result that is not that — a hedge, a naked leg, conflicting
// evidence, a flat open beside an unprovable order — is recorded as a Pair, so
// the margin guard's OpenPairs sees it, read again from the venues when it is
// unresolved; conflicting evidence is listed and blocked from an automatic close.
// After a restart, Adopt records what the venues hold for a lock Engine 2 owns
// rather than refusing what is not a clean hedge: a pair within the executor's own
// criterion is a pair, anything else is unresolved at its real sizes, and flat
// venues are a pair of size zero whose Close releases the lock — its opening
// orders unproven when orders were ever sent under that lock. Adopt refuses a lock
// granted in this process: its owner is live (round 3, minor 4). Two operations
// are a person's alone and send nothing: UnblockClose and ConfirmOrdersFinished.
//
// # What the package does not do
//
// It decides nothing: no signal, no funding figure, no exit rule — the
// cross-venue radar ranks and logs only (PLAN 4.5i direction 1). It does not
// model either leg's liquidation price (internal/risk models a SHORT perp at
// isolated margin; Engine 2's long leg is not modelled at all); the only margin
// protection is the account-level MarginGuard. It sets no leverage and verifies
// no position mode — every flatness proof trusts GetPosition, which on Binance
// sums hedge-mode sides. Every figure is GROSS (rule 2). And it is linked by no
// command: wiring it into a binary is a separate step, behind the operator's
// decision — as is running it beside an Engine 1 that bypasses the coordinator,
// which no check here can make safe (coordinator/doc.go).
package crossperp
