// Package coordinator is the Exclusive Symbol Lock: at any instant a symbol
// belongs to at most ONE engine (docs/designs/cross-perp-engine-design.md §2).
//
// # Why a lock at all
//
// Binance USDⓈ-M and Bybit's linear market run this account in ONE-WAY mode.
// Engine 1 (cash & carry) holds a SHORT perp; Engine 2 (cross-venue perp–perp)
// holds a LONG perp on one venue and a SHORT on the other. Were both engines on
// one symbol at one venue, the venue would NET the two into one position: Engine
// 2's long would shrink Engine 1's short, both hedges would break, and either
// engine's close would trade the other's position. So a symbol is idle (either
// engine may take it), occupied by exactly one engine, or in conflict.
//
// # The three states
//
//	idle      no lock and the venues were read flat — either engine may ask
//	occupied  one engine holds it; the other is refused AT THE GATE, before it
//	          may even form an intent
//	conflict  the evidence disagrees — a lock file naming an owner the venues'
//	          positions contradict, or a position no engine's shape explains.
//	          Nobody opens and nobody releases; an operator clears it once the
//	          venues read flat. Never reconciled by picking one side
//
// # Contention, and why a mutex alone is not the rule
//
// Every piece of state sits behind one mutex, so two requests in "the same
// microsecond" are serialized and exactly one lock can ever be granted. But a
// mutex alone grants whoever wins the scheduler race, and the design asks for
// the engine with the higher expected return. So the first request for an idle
// symbol opens a short CONTEST (Config.ContestWindow, 100 ms by default), every
// request that arrives inside it competes, and the highest
// PriorityAPROnCapitalFrac wins — ties go to the earliest arrival. A request
// after the grant is refused however high its figure: the winner may already
// have orders on the wire, and pre-empting it would be exactly the netting the
// lock exists to prevent.
//
// The figure is compared, never computed, and never displayed as a return. It
// must be ON CAPITAL — the two engines tie up capital differently (Engine 1 buys
// spot outright, Engine 2 posts margin on two venues, PLAN 4.5i correction 1),
// so a per-notional figure would rank them wrongly — and it must carry the words
// for what it has deducted (PriorityAPRBasisVI). Only internal/strategy may call
// a figure net (CLAUDE.md rule 2), and no strategy function prices Engine 2 yet.
//
// # Release is proven on the venues, not on a label
//
// Release does not take the caller's word that the position is flat. It reads
// every configured venue's perp position AND resting orders for the symbol (rule
// 7) and keeps the lock unless every position reads EXACTLY zero with no order
// working; an unreadable venue keeps it too. The one lock given back without a
// venue proof is Withdraw's: a lock granted in this process under which nothing
// was ever sent — and "nothing was sent" is not the caller's word but the lock's
// own record: an owner calls MarkOrdersSent, which reaches the disk BEFORE its
// first order, and a marked lock is never withdrawn. A lock held after the
// position is gone costs an opportunity; a lock released with a leg still open
// costs a netted hedge. Engine 1's spot leg has no
// position to read, so its flatness is internal/execution.Close's proof, not
// this package's.
//
// # Persistence, and what happens when the file is lost
//
// Every grant and release is written to Config.Path
// (.paper/coordinator-locks.json by default) atomically — temp file, fsync,
// rename — BEFORE it is reported, so a crash can never leave an engine holding
// a lock the disk does not know. The write happens under the mutex: it is a
// local file of a few hundred bytes, never the network, and it is what keeps
// memory and disk in the same order.
//
// The file is a CACHE (rule 7). A missing file starts empty; a corrupt one is
// moved aside — never overwritten, it is evidence — and the coordinator starts
// empty. Either way NOTHING is granted until ReconcileActivePositions has read
// every venue, and any symbol with a live position comes back occupied,
// attributed by its shape: SHORT only → Engine 1 (whose perp leg is always
// short), a LONG on one venue and a SHORT on another → Engine 2, whatever their
// sizes — anything else (a naked long, two longs, three legs) → conflict. The
// sizes decide nothing here: whether a long and a short are a HEDGE depends on
// the two venues' step sizes and minimum notionals, which this package does not
// hold, so the imbalance is written into the lock and Engine 2 judges it when it
// adopts the pair (review 4.5k, N4). That is the answer to "Engine 2 must never
// open against Engine 1 if the lock file is lost": the venues remember what the
// file forgot. A lock from the file is never re-granted to its own intent either;
// it is taken back with Adopt.
//
// What the lock CANNOT do is bind an engine that does not ask it. Today's
// cmd/execportal (Engine 1) does not. The execution machine reads both venues'
// positions and resting orders immediately before it places anything
// (internal/execution/crossperp), which catches every position and order that
// exists at that instant — and nothing an Engine 1 sends a moment later. So it
// is a DEPLOYMENT PRECONDITION, not a guarantee of this package, that Engine 2
// never runs beside an Engine 1 that bypasses the coordinator on the same
// symbols (review 4.5k, M9).
//
// Two limits of the evidence itself, recorded rather than hidden: a SHORT-only
// shape is attributed to Engine 1 without reading Engine 1's spot leg, so a naked
// Engine-2 short whose lock record was ALSO lost reads as Engine 1's (a double
// fault); and every flatness proof trusts GetPosition, whose Binance adapter sums
// hedge-mode LONG and SHORT sides into one number (internal/broker/binance
// parsePosition) — an account must run one-way mode.
//
// # Reconcile does not flip a live owner
//
// An engine that acquired a lock in THIS process may be mid-open or mid-close,
// and its legs pass through shapes that are not its final one (one leg filled,
// the other still working). Reconcile reports such a lock as inconsistent and
// leaves it with its owner. Only a lock from the FILE or from an earlier
// inference — nobody alive is operating it — is turned into a conflict when the
// venues contradict it: that is the shape a crash between two legs leaves.
//
// Introduced 2026-09-17, PLAN step 4.5k. It places no order and holds no
// credential: it reads positions through whatever broker.Broker it is given.
package coordinator
