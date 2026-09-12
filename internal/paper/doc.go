// Package paper is the paper-trading ledger (PLAN.md step 4.3, Q12): it
// PRICES decisions that have already been made and never makes one.
//
// The step-3.5 signal path writes every decision to signal_journal; the
// backtest replays the same rules; both compare BY DECISION. Neither says
// what the decisions were worth in money. This package answers that with a
// virtual account: two legs filled on the measured order book, taker fees at
// the schedule the journal row recorded, funding credited at each settlement
// the position was open across, the pair marked to mid so a basis move shows
// as a loss before funding has earned it back, and an equity curve over all of
// it. It reads normalized values only — books, settlements, fee states — and
// holds no credential, opens no socket, places nothing.
//
// # What is REAL and what is FAKE here
//
// Real: the prices, the books, the funding rates, the decisions (production
// EvaluateEntry / EvaluateExit, as journalled). Fake: the fills, the
// positions, the balance, the funding credit. Every figure this package
// produces is a PAPER figure and its consumers label it so.
//
// # Rules the ledger enforces, and why
//
//   - Both legs or neither. A position is spot long + perp short of the same
//     coin quantity; an opening where one leg cannot be priced opens NOTHING,
//     and a closing where one leg cannot be priced closes nothing and says so.
//     There is no one-legged state — the invariant internal/execution/doc.go
//     names as the most expensive failure mode of the real thing.
//   - Fills come from strategy.EstimateFill on a book sampled AT OR BEFORE the
//     decision (never after: that is a price the decision could not have
//     seen), and no older than the decision's own book-age budget. Buy at the
//     ask side, sell at the bid side; the fill price is the mid moved by the
//     estimate's slippage, which includes crossing half the spread. A size the
//     measured book cannot absorb inside 0.5% is REFUSED, exactly as
//     production refuses it — never filled at an imagined price.
//   - Fees are the taker schedule the journal row was PRICED with
//     (params_json.fees), never the config file of the day the ledger runs. A
//     row without a fee state (the first run's rows) cannot be priced and is
//     refused by name.
//   - Funding is a discrete event (CLAUDE.md rule 6): credited only at a
//     settlement stamp strictly after the open and at or before the close,
//     amount = mark price × coin quantity × per-interval rate, positive to the
//     short when the rate is positive. Never pro-rated by holding time. A
//     continuous-model series (Paradex) has samples, not settlements, and is
//     counted as "not creditable" rather than credited 8,760 times a year.
//   - A Binance "Special" (dividend-driven) settlement IS credited: an
//     account receives it. The decision rule and the backtest DROP such rows
//     (strategy.UsableSettled), so the ledger's funding can exceed the
//     backtest's by exactly those payments; the count travels as
//     FundingSpecial so the difference is attributable.
//   - Rule 7 does not apply: there is no venue to read a paper position from,
//     so the ledger IS the source of truth for paper positions. That exception
//     is written in internal/execution/doc.go so nobody carries the habit of
//     trusting local state into live mode.
//
// # What it does not see
//
// Partial fills, queue position, latency, rejected orders, liquidation,
// margin calls, API failures, and the difference between a stored 0.5%
// aggregate and a real level book. Those are what steps 4.4 and 4.6 exist to
// measure; the ledger's summary says so.
//
// Two limits of the stored inputs, stated rather than hidden: a depth row's
// sampled_at_ms is the START of its collection round (internal/depth stamps
// one instant per round, and a round of ~117 fetches with 150 ms pauses
// runs 30–90 s), so "at or before the decision" is enforced at the
// resolution of a round, not of a fetch — a book fetched late in a round
// that started before the decision passes; a per-fetch stamp is scheduled
// for after the 3.5 gate (PLAN 4.3). And the JSON this package feeds is not
// the dashboard wire: it carries fractions under *_frac names beside quote
// amounts, and its consumer converts to percent for display, which
// CONVENTIONS §1.5 allows because every name says what it holds.
//
// Introduced in: PLAN.md phase 4, step 4.3.
package paper
