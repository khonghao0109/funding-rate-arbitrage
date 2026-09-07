# CLAUDE.md

Context for AI agents working in this repository. Read this before touching code.

---

## What this project is

A Go service that connects to 9 crypto venues over WebSocket, normalizes their
order book data, and surfaces price dislocations in real time through a browser
dashboard.

**It is a read-only scanner today.** It holds no credentials and places no
orders. Every number it displays is derived from public market data. Since step
2.6 it also WRITES that data to a local SQLite file — funding history, sampled
prices, daily instrument rules — to build the phase-3 backtest corpus; reading a
venue and recording what it said is still read-only with respect to the venue.

## Where it is going

The target is a **Funding Rate Arbitrage bot**: hold spot long and perpetual
short of equal notional, collect the funding payment each settlement period,
stay delta-neutral throughout. After that, Basis Trade. The full roadmap is 9
phases and 40 steps in [docs/PLAN.md](docs/PLAN.md).

Expected return for the funding strategy is **5–15% APR**. Anyone or anything
proposing a design that implies far more than that has misunderstood the
strategy, not discovered an edge.

## Current phase

**Phase 1 — Hardening: closed 2026-09-07.** 7 of 7 steps done and the 72h
unattended run passed (measurements below and in PLAN step 1.5). Step 1.0 froze the WebSocket JSON
contract for the whole phase — it is specified in
[docs/WS-CONTRACT.md](docs/WS-CONTRACT.md) and **must not be reshaped** before
phase 2: steps 1.1–1.3 fill data into fields that already exist. Step 1.1 added
the staleness filter; step 1.2 split the comparison into `perp_usdt`, `perp_usd`
and `spot_usdt` groups, filled `basis[]` and `oracle_deviation[]`, and started
collecting top-of-book quantities.

Step 1.3 added `internal/fees/` and the after-fee figure. Step 1.4 moved every
operational fact into **`config.yaml`** — pairs, venues, thresholds, fees, and
the per-venue symbol mapping — so adding a pair or a venue is a YAML edit.

Step 1.5 replaced nine copies of the reconnect loop with one shared lifecycle in
[exchanges/stream.go](exchanges/stream.go): `Feeds` carries `ctx` and the
channels, backoff runs 2s→60s, every venue gets the keepalive its own
documentation specifies, and cancelling `ctx` stops every connector in
milliseconds. `RecvAt` is now stamped at the socket read (debt from 1.1), and
`source_status` carries `reconnect_count` and `uptime_sec` the connectors
actually report.

Step 1.6 recorded real payloads from every venue into `exchanges/testdata/` and
golden-tested each parser against them, taking `exchanges/` from 20.8% to 63.8%.
It found that **every Binance trade was labelled a sell** — see the trap table.

**The 72h unattended run passed (verdict 2026-09-07).** It started 2026-09-03
14:03 on commit `4feea94` — the step-1.6 code, built before any phase-2 commit,
so it exercised phase-1 code only — and was still the same PID at 91h. Measured:
RSS 28.9 → 29.4 MB at the 72h mark (min 25.5, max 35.4), 27 fds; 273 connection
drops inside the window, every one reconnected in 2–4s except four 60s waits
where the backoff had reached its cap after runs of dial failures, which is the
design; two whole-network outages on 2026-09-06 (all nine sources within 5s)
recovered in ≤4s; Hyperliquid expires every session after ~2h50 and Paradex
deployed 21 times, both absorbed by the shared lifecycle; 36/36 series live at
91h. Two caveats: Pyth answered 401 on every attempt, so the oracle was absent
the whole run, and the step-1.6 silent-subscription defect simply did not occur
— it stays open, unfixed. The soak process was left running past its deadline
on port 8082 (`.soak/scanner.pid`); stop it with SIGINT when it is no longer
wanted, and keep using another port while it lives.

Phase 2 (Funding Rate Monitor) has started in parallel without touching the
soak process: step 2.1 (`cmd/fundingcheck`) verified the funding fields of all
7 venues against live APIs on 2026-09-03 and corrected the survey — see
[docs/DATA-REQUIREMENTS.md §3](docs/DATA-REQUIREMENTS.md). Step 2.2 added
`FundingData` and per-venue funding normalization; step 2.3 built the
instrument registry (`internal/instruments/`) with delta-neutral sizing;
step 2.4 added the spot↔perp hedge mapping (`BuildHedgeMapping` in
[internal/instruments/mapping.go](internal/instruments/mapping.go)):
`Instrument` now carries VENUE-DECLARED `BaseAsset`/`QuoteAsset`, pairing is
validated both ways against config's declarations, and anything unpairable is
a named rejection — never a guess. USD-quoted perps (Kraken, Hyperliquid,
Paradex) were refused against USDT spots until 2026-09-07, when
`hedge.quote_equivalents` in `config.yaml` made the equivalence DECLARABLE:
the shipped file declares `[USD, USDT]`, which pairs those perps with a USDT
spot and marks every such pair `QuoteBridged`. That is a RISK decision, not a
venue fact — the position is delta-neutral in the coin and OPEN in USDT/USD,
which nothing here deducts — so the label travels to the dashboard leg note,
to each backtest series line and to its assumptions block. Declaring nothing
restores the older, stricter behaviour, and a refusal now says WHICH case it
is ("no quote equivalence declared" vs "these quotes are in no group").

Step 2.5 collects funding from all 7 venues in real time: six over WebSocket
(Bybit `tickers`, OKX `funding-rate`, Gate `futures.tickers`, Kraken `ticker`,
Hyperliquid `activeAssetCtx`, Paradex `funding_data.{market}`) and Binance over
REST `premiumIndex`, because its mark-price stream delivers nothing here — the
measurement and the two other design-changing findings are in
[docs/DATA-REQUIREMENTS.md §3.4](docs/DATA-REQUIREMENTS.md). Readings land in
the scanner's funding map and are printed once a minute as a GROSS table; the
dashboard is step 2.7.

Step 2.7a put funding on the dashboard: a new `funding` message (pushed after
`meta` on connect, then every 5s), `meta.funding_basis`, a REST
`GET /api/funding/history` served by `cmd/scanner` because the history lives in
SQLite and `internal/scanner` must not learn about the store, and a Funding tab
with the venue × pair matrix in **bps per 8h**, a per-pair detail table and a
settled-history chart. The contract grew by the rules of
[WS-CONTRACT §8](docs/WS-CONTRACT.md) — new type, new fields with defaults, `v`
still `1`.

Step 2.7b added REST order book depth for all nine tradable sources
(`exchanges/*_depth.go` → `internal/depth` → `depth_snapshots`, schema v2), the
`depth` message, and the liquidity columns — including the BID side of the spot
leg, which is where a funding position actually gets stuck on the way out. It
also closed the step-1.2 debt: OKX, Gate and Kraken now publish their top-of-book
size as CONTRACTS and the scanner converts through the registry, so 8 of 9
sources carry a real coin quantity where 3 of them showed 0 since step 1.2.
**Phase 2 is complete.** An independent review of the whole phase
(2026-09-04) found no blocking defect and eight commits of hardening
followed it — Kraken WS snapshot ordering, OKX/Binance max order caps,
whole-contract sizing, fetcher-declared book denomination, schema v3
(`mark_price_quote`), measured `is_estimated` semantics (Gate forming /
Kraken settled), and dashboard freshness retraction on socket loss. The
remaining review debts are recorded at the end of the phase-2 section in
docs/PLAN.md.

**Phase 3 has started.** Step 3.1 built `internal/strategy` — `EstimateFill`
(slippage for one fill from the measured book), `RoundTripCost` (the four taker
fills of opening and closing a delta-neutral position: buy spot + sell perp in,
sell spot + buy perp out) and `NetAPR`. Measured on the real `depth_snapshots`
with every venue given the SAME funding rate, so cost is the only variable: at
$20k hyperliquid ranks first at 7.40% net and paradex last at 5.10%; at $60k
paradex is REFUSED (its book holds $21.5k inside 0.5%); at $12M hyperliquid is
refused and binance leads at 3.48%. **The ranking changes with size** — the case
PLAN §7.4 says a screener without depth gets backwards. Venues whose fee
schedule is `verified: false` refuse to produce a number rather than costing 0.

Step 3.2 added `EvaluateEntry`/`EvaluateExit` — six entry conditions, four exit
conditions, each returning a `Check` with the numbers behind it, and **every
check runs even after one fails** so the log names all the problems at once.
Three rules it fixes in place: the decision is made on **settled history, never
on a forming rate** (only settled rates exist on both sides of the step-3.5
gate, and `IsEstimated` means something different at every venue — see the trap
table); a check whose dependency failed reports "not evaluated" instead of
inventing a second cause; and the decay exit requires the net APR to stay under
the floor for N consecutive settlements, because a single-print rule closes on a
one-period dip and pays a round trip in each direction to do it (measured on
binance BTCUSDT Aug 2026: 0.79 → 0.51 → 0.23 → 0.20 → 0.83 → 1.00 bps/8h). A
funding SIGN FLIP exits immediately by default — that is money leaving every
settlement — and since 2026-09-07 can be gated (`ExitNegative*`, 0/1/0 = the
default; negative prints are that exit's alone, the decay exit counts only
non-negative settlements). Measured on the real corpus: strict thresholds give 0 entries / 28
skips; loose ones give 4 entries at 5.71–6.97% net APR, inside the 5–15% band.

Step 3.3 built `internal/backtest` + `cmd/backtest`, which call the step-3.2
functions rather than reimplementing them — two AST tests enforce that, one
requiring the four calls and one forbidding any locally declared rule name.
**Its verdict is that the strategy as parameterized LOSES money**: 6 months, 16
series × 24 parameter sets, of which 288 are REFUSED BY NAME (unverified fee
schedules — no cost, no replay, never "0 trades at 0 cost"), 96 run, 72 trade,
and **0 are profitable**. The signal
picks the right regime — 98.7% of held periods had positive funding — but the
0.3010% taker round trip exceeds what funding pays over the holds the exit rule
produces. Measured on the corpus: BTC funding averaged 0.002573%/8h, so break
even is 117 settlements = **39 days**, while the losing trades held 1.7–12 days.
PLAN §7.4's worked example assumed 0.01%/8h — 3.9× optimistic — which is where
its "~10 days" came from. This is not a bug: the 5–15% band is for SELECTED
opportunities, not for BTC/ETH held mechanically on taker fees. A backtest
returning 5–15% at this configuration would be the suspicious result.

A wider sweep on 2026-09-07 (6,048 parameter sets × 16 series × 12/6/3-month
windows, through the list-valued grid flags `cmd/backtest` now takes — the
defaults reproduce the step-3.3 grid exactly, pinned by test) confirmed the
verdict and sharpened it. Run first with the fees of that morning (12 series
refused by name, 4 binance series replayed: 12 months → 0 of 8,928 traded
runs profitable), then again after the user verified bybit/okx/gate fees the
same afternoon (0 refused, all 16 replayed): 12 months → **0 profitable runs
on 15 of 16 series**, and the 78 "profitable" runs left are one XRP/okx trade
of +0.028% on a corpus that covers 96 days. The 6- and 3-month windows show a
positive region (BTC/binance, 2–5 trades, best +0.72%) and every set in it
loses over 12 months — the Jul–Aug 2026 regime, not a parameter. Facts to
carry forward: entry thresholds ≥1.2 bps/8h never trigger on BTC/ETH/SOL
because the corpus maximum there is exactly 1.0 bps/8h (XRP reaches 3.0, and
loses); holding through the window on one round trip beats every rule on
BTC/ETH at all four venues (binance +3.05% / +2.18% net over 12 months,
bybit +2.54% / +2.24%, against −3.71% / −6.00% and −8.83% / −7.44% for the
3.3 set) — the exit-and-re-enter rule pays 0.30% per sign flip (BTC flips
200 times a year at binance, 288 at bybit), and two spot fills are 20 of
those 30 bps; the three newly priced venues are not cheaper (0.316–0.326%
per round trip at 50k). Measured side effect: re-sampling the book two hours
later moved XRP's cost from 0.377% to 0.358% and its trade counts by up to
±50 per run, so "one book held fixed" is a real sensitivity on thin pairs.
The 3.3 parameter set stays as it is until the 3.5 gate has judged it.
Bilingual VI/ZH report: `docs/reports/backtest-3.3-wide-2026-09-07.html`.

The same afternoon the sign-flip exit gained three GATES as parameters
(`strategy.Params.ExitNegativeMinBps/Periods/CumCostFrac`, config keys
`exit_negative_*`; zero values are the 3.2 rule exactly, pinned by test, so
the live 3.5 set did not move). Measured: BTC flips sign 200 times a year and
the median negative episode costs 0.3 bps against a 30 bps round trip, yet
the 3.2 rule leaves on ANY negative print. A 64 base-set × 125-gate ×
16-series sweep (v5, run from the committed `5510ee6` after the jurisdiction
fix below; the earlier 24 × 64 run said the same) found the gates halve the
loss at the 3.3 setting (−88% → −43% summed over 16 series, still 1/16
positive) and, with the decay exit relaxed (floor 0, 12 periods), converge on
hold-through: 3,398 of 8,000 sets sum positive over 12 months but NONE
reaches hold-through's +10.23% (12/16); the best sums +9.72% with 12/16
positive at 1.7 trades each, and it beats hold-through only on series where
hold-through is weak or negative (SOL, the short okx/gate corpora) — never on
BTC/ETH at binance or bybit. The decisive axes are the decay exit's length
and the cumulative-cost gate C; X and N barely move the result. The 6- and
3-month windows let 50 and 1,096 sets beat hold-through by entering later
than the window start, and none keeps that over 12 months — because on this
corpus no negative episode in a year costs as much as one round trip, so the
best gate is "do not leave on a sign flip" and the edge is in entry and cost,
not exit. An exit rule still worth writing compares the EXPECTED cost of holding
through a negative run with the round trip, and must beat hold-through, not
match it; that is 3.2 work after the gate. The adversarial
review of that sweep found the decay exit counting negative prints too, which
made `ExitPersistencePeriods` a hard ceiling on every gate (half the grid was
degenerate, exits merely relabelled); the two exits now have disjoint
jurisdictions and the sweep was re-run. The same review made
`config.Strategy.StrategyParams()` the ONE config→Params mapping for both
`cmd/scanner` and `cmd/backtest`, with a test pinning the sweep base to the
shipped block.

Verifying bybit_spot at the same 10 bps as binance_spot exposed a latent
defect the same day: `config.CheapestVerifiedSpot` kept the FIRST candidate on
a fee tie, and the candidate order came from whoever called it — the hedge
mapping in `cmd/scanner`, a loop in `cmd/backtest` — so the two commands the
3.5 gate compares could have chosen different spot legs for the same perp. A
tie now resolves by CONFIG order (binance_spot is listed before bybit_spot),
whatever order the candidates arrive in, and the note says a tie was broken.
Tests that had borrowed "bybit is unverified" from the shipped config.yaml
now state that scenario themselves (`markFeeUnverified` in
`internal/scanner`, an explicit override in `cmd/scanner/hedges_test.go`).

**On 2026-09-07 the user replaced the shipped `strategy:` block with the gate
grid's best set** — entry 0.3 bps/8h held 6 settlements, hold floor 0 over 12
periods, sign-flip gates X 2.0 / N 2 / C 0.25 — and `cmd/backtest`'s
`baseParams` was moved with it (the sweep overrides every one of those axes,
so the default grid is unchanged, pinned by test). The old set is
`0.5 / 3 / 0.005 / 3` with gates `0/1/0`. **This does not move the running 3.5
process**, which loaded its config once at start-up: the file no longer
describes what that journal is producing, so the gate comparison must
reconstruct the old set from `git show ba5ee31:config.yaml` exactly as it
already does for the pre-verification fees. Restarting to pick the new set up
would restart the 14-day clock. Measured with the new set over 12 months on
24 series: hyperliquid BTC +5.44% and ETH +5.01% realized APR, the first
figures this project has produced inside the 5–15% band — both on a
quote-bridged pair, so both carry undeducted USDT/USD exposure.

**The 24-series wide sweep (2026-09-07, 4,096 sets × 3 windows, one grid now
that config carries the gates) says the same thing louder.** The shipped set
sums +14.86% over 24 series (14/24 positive, 4.0 trades each) where the
retired 3.3 set sums **−533%** at **73 trades** each — the old rule left on any
negative print, and at an hourly cadence there are 8× as many, so it paid 73
round trips a year per series. But hold-through grew too: **+29.63%** (18/24),
and **0 of 4,096 sets reach it**; the grid's best is +19.27%. Per series, the
count of sets beating hold-through is 0 at BTC/ETH on hyperliquid and bybit,
2 at BTC/binance, and 2,342 at SOL/binance where hold-through is itself
negative. The rule wins only where holding loses. Anything proposing to trade
this must beat hold-through on the 365-day series, not merely turn positive.

**Step 3.4 (alerts) is deferred by the user's decision, and step 3.5 is
RUNNING** (started 2026-09-07 09:39:52, port **8085**, PID in
`.paper/scanner.pid`, verdict no earlier than 2026-09-21). The "alert-only
mode" is a **journal-only mode**: `cmd/scanner`'s `startSignals` evaluates
every hedge leg every 10 minutes with the SAME `strategy.Candidate` the
backtest builds — settled history from the store, fees from config (loaded ONCE at start-up: the 3.5 process still holds the pre-verification fees of `ba5ee31`, see PLAN 3.5 ③), the newest
measured books — plus live spot/perp prices, which is why the basis exit is
evaluable here and nowhere else. Every decision lands in `signal_journal`
(schema v4) with all its checks as JSON; paper positions reseed from it on
restart. The `strategy:` block in `config.yaml` is the one parameter set both
the live path and `cmd/backtest` run, and the comparison protocol for the gate
is written in PLAN 3.5 — read it before delivering the verdict. **Do not
restart that process casually**: the gate needs 14 UNBROKEN days.

Step 2.6 added persistence: `internal/store/` (SQLite through the pure-Go
`modernc.org/sqlite`, so `CGO_ENABLED=0` builds keep working), `internal/history/`
(venue REST → store, shared by the scanner's hourly top-up and `cmd/backfill`),
and seven `exchanges/<venue>_funding_history.go` fetchers for the SETTLED rates
the phase-3 backtest replays. Three tables — `funding_history`,
`price_snapshots`, `instrument_snapshots` — every column carrying its unit,
because phase 8 reads them from Python. **The corpus is not uniformly deep and
must never be assumed to be**: OKX publishes ~3 months, Gate refuses a `from`
older than 180 days, and Paradex has no settlements at all (measurements in
[docs/DATA-REQUIREMENTS.md §9](docs/DATA-REQUIREMENTS.md)).

---

## How work is done here

Every change follows the 9-phase loop in [docs/WORKFLOW.md](docs/WORKFLOW.md):
select step -> reconcile against real code -> design -> test first -> implement
-> review -> acceptance -> update docs -> `codegraph sync` + ONE commit.

Four gates block progress: reality must match the plan's assumption (P1), review
must pass (P5), the step's acceptance criteria must actually be exercised (P6),
and documentation must be in sync (P7). Read WORKFLOW.md before starting a step.

Use `codegraph explore|context|node|impact` to learn what the code really does
before trusting what a document says it does. Run `codegraph sync` before every
commit.

## Rules that override default behavior

These exist because violating them produces expensive, silent failures.

**1. Do not skip phases.** Execution and credentials (phase 4) come after
monitoring, signals, and backtesting are proven (phases 2–3). If asked to "just
add order placement", say what is missing first. Phases 2 and 3 need no API key
at all — that is deliberate.

**2. Never present gross profit as profit.** Every profit figure must be net of
maker/taker fees, funding cost, and estimated slippage. Step 1.3 added the fee
model, so a figure can now be "after trading fees". **Step 3.1 opened the word
"net", and only inside `internal/strategy`**: `NetAPR` deducts taker commission
on all four fills AND slippage estimated from the measured book for the intended
size, and it carries `AppliedVI`/`ExcludedVI` so no caller can display the
number without being able to say what it covers. Five costs are still excluded
by name — spot borrow/margin, basis drift between entry and exit, the book AT
EXIT, transfer fees, liquidation risk. Everything outside that package is still
gross or after-fees-only: the wire's `funding_basis.model` is still `"gross"`,
and the stored funding corpus is gross. Label every figure with what has
actually been taken off it, wherever it surfaces.

**3. Never hardcode a funding interval.** Not 8h, not anything. Hyperliquid
settles hourly, Kraken settles hourly with a per-1h rate, Paradex accrues
continuously with no discrete settlement, Binance runs 8h, 4h or even 1h per
symbol (measured 2026-09-03: 4h is now the majority). See rule 5.

**4. Name the unit in the identifier.** `IntervalSec`, `RatePer8hFrac`,
`NotionalUSD`, `TakerFeeBps`, `QtyContracts`. A bare `rate`, `size`, or
`interval` crossing a function boundary is a defect, not a style preference.
`Bps` is fractional, not integral: real schedules include 1.5 bps and 0.3 bps
legs, and rounding them away misstates a leg by a third or makes it free.
This is the single most important convention in the codebase — see
[docs/CONVENTIONS.md §1](docs/CONVENTIONS.md).

**5. Do not invent exchange API fields.** Venue APIs disagree in ways that look
like they should agree. When adding or changing an integration, read the current
official documentation and cite it in a comment. If documentation cannot be
reached, say so rather than guessing — a plausible wrong field name produces
numbers that are wrong but not obviously wrong.

**6. Funding is a discrete event, not a continuous yield.** A position earns
nothing unless it is open at the settlement timestamp. Holding 7h59m of an 8h
period pays zero. Count settlements crossed; never multiply an APR by a holding
duration. (Paradex is the exception — it accrues continuously.)

**7. Positions are read from the venue, never from local state.** Local
bookkeeping is a cache and is assumed stale until reconciled.

**8. Go owns everything that decides or executes a trade.** Ingestion, REST,
instrument registry, strategy, backtest, execution and risk are Go, in one
process. Do not propose splitting any of them into another language: REST and
WebSocket share the same types and the same unit-normalization layer, and a
second implementation of "normalize Kraken's relative funding rate" will drift
from the first.

The backtest engine specifically must import `internal/strategy` and call the
production entry/exit functions — never a reimplementation, never a port. Phase
3 step 3.5 gates the project on backtest results matching paper trading; that
gate is meaningless if the two sides run different code.

Python is read-only and lives outside the process: it reads SQLite to plot and
explore. It becomes necessary at phase 8 (cointegration, VECM/GARCH, ML), where
Go has no equivalent — not before, and never inside the trading loop.

**9. Do not introduce CCXT.** It normalizes away exactly the venue differences
this project has found to be decisive (see the trap table below). Seven
hand-written connectors already work.

**10. Do not stream order book depth continuously.** Funding positions are held
for days to weeks, so incremental `depth` — with its sequence numbers, gap
detection and resync — is only justified while an order is actually being
placed (phase 4.4). Periodic REST snapshots (`depth?limit=100`, every few
minutes, candidate pairs only) are what screening needs. Depth also gates
opportunity ranking: without it a 200% APR pair with a $2k book outranks a 15%
pair with a $500k book. See docs/PLAN.md §7.4.

**11. The frontend is not the bottleneck — do not propose a framework.** The
vanilla JS already batches messages at 50ms, keeps only the newest spread
snapshot, throttles the matrix rebuild to 300ms, and updates charts
incrementally. The waste is server-side: `broadcastSpreads` fires on every tick
and ships every symbol to every client. Fix the broadcast layer first. See
docs/PLAN.md §7.3 for scale thresholds.

**12. The WebSocket contract is frozen for phase 1.**
[docs/WS-CONTRACT.md](docs/WS-CONTRACT.md) is authoritative. Add a field with a
documented default; never rename one, change its type, or remove it. Every field
carrying a unit says so in its name, on the wire as well as in Go. No field is
ever named `profit_*`: a figure with nothing deducted is `*_gross_pct`. The
frontend builds its source list, symbol selector and cost disclaimer from the
`meta` message — do not reintroduce a hardcoded source list.

**13. Freshness is measured from the receive time only.** `VenueTimeMs` is the
venue's own clock and is 0 for the venues that publish none; a real measurement
showed Binance's running 80ms *ahead* of ours, so differencing the two measures
skew, not age. `RecvAt` is stamped **as close to the read as the transport
allows — one place per transport, and there are exactly three**:
`runSession` in [exchanges/stream.go](exchanges/stream.go) for the nine
WebSocket connectors, the SSE read loop in [exchanges/pyth/pyth.go](exchanges/pyth/pyth.go),
and `pollBinancePremiumIndex` in
[exchanges/binance/funding_rest.go](exchanges/binance/funding_rest.go) for the
REST funding poller, which step 2.5 added because Binance's mark-price stream
delivers nothing to this environment (measured — DATA-REQUIREMENTS §3.4). Never
add a fourth, and never add a second site for a transport that already has one.
Step 1.5 moved it there from the scanner's dequeue: the ingestion channels hold
1000 messages, and stamping at the far end restarted each message's clock after
it had already waited, so a backed-up scanner reported prices seconds old as
freshly received. Stamping at the read folds the queue delay into the age, which
means a real backlog now shows up as staleness instead of hiding. The scanner falls
back to its own clock only for data that never crossed a socket (`receivedAt` in
[internal/scanner/scanner.go](internal/scanner/scanner.go)). Staleness thresholds
are per venue and measured; the numbers and the reasoning are in `config.yaml`.

---

## Domain traps already discovered

Surveyed 2026-08-28 across all 7 futures venues. Full detail and confidence
levels in [docs/DATA-REQUIREMENTS.md §3](docs/DATA-REQUIREMENTS.md). Do not
re-research these; do verify before writing the integration.

| Venue | Trap |
|---|---|
| **OKX** | `fundingTime` is the NEXT settlement; `nextFundingTime` is the one AFTER that. Mapping it like Binance's `T` is off by one period. |
| **Kraken** | `funding_rate` is an absolute price amount, not a rate — verified live: absolute ÷ relative ≈ index price. Use `relative_funding_rate`. Settles hourly and the relative rate is **per 1h, used as-is** — ×8 for the 8h comparison, never ÷8 (correction history: DATA-REQUIREMENTS §3.2②). Its WS `next_funding_rate_time` is an **absolute epoch-ms stamp** even though the doc prose says "time until" — probed live twice; see §3.3⑥. And its WS `relative_funding_rate` is the **already-settled** figure of the last completed hour (the forming estimate lives in `relative_funding_rate_prediction`), so `IsEstimated=false` there and the rate does NOT forecast the next stamp. |
| **Bybit** | Ticker pushes snapshot AND delta. A field absent from a message means unchanged, not zero. Merge into cached state; never overwrite — and publish only when a FUNDING field actually changed, or the ~100ms delta stream refreshes `RecvAt` ten times a second and a dead subscription looks permanently fresh. Its `fundingIntervalHour` is the string `"8"`, not a number: declared as `int64` the whole frame fails to decode and the venue silently produces no funding at all. It publishes `fundingCap` and **no floor**, so cap and floor need separate flags. |
| **Binance** | `fundingInfo` documents itself as returning ONLY symbols whose config differs from default — as of 2026-09-03 it happens to cover every TRADING perpetual (777 symbols, BTCUSDT included via its adjusted ±0.3% cap), but the docs promise no such coverage. Default to 8h and override; do not read it as the source of truth for all symbols. Intervals seen: 4h (majority), 8h, and 1h. Also filter `rateType: "Special"` in backtests. |
| **Hyperliquid** | Funding is hourly, not 8-hourly. Annualizing as 8h is wrong by 8x. Its `predictedFundings` also lists BinPerp and BybitPerp beside its own **HlPerp** row — read the wrong row and an 8h cadence lands on an hourly venue. And `nextFundingTime` there is the settlement of the period ALREADY RUNNING (measured across an hour boundary 2026-09-04: 02:47→02:00, 03:01→03:00), so the upcoming one is that stamp plus one interval. |
| **Paradex** | Funding V2 accrues continuously via a funding index. There is no settlement timestamp. |
| **Binance** | The `@markPrice@1s` stream delivers NOTHING to this environment — measured 2026-09-04, one socket carried 4,782 bookTicker frames and zero markPriceUpdate frames in 45s after the server acknowledged both subscriptions. Funding comes from REST `premiumIndex`, queried per symbol (the unfiltered form is 199 KB for ~780 entries and costs request weight 10 against 1). See DATA-REQUIREMENTS §3.4⑦. |
| **Binance** | The aggTrade payload carries both `m` (buyer is maker) and `M` (deprecated, always true). Go's `encoding/json` prefers an exact tag match but **falls back to a case-insensitive one**, so declaring only `m` let `M` overwrite it and every trade came out a sell. Declare BOTH members of every case-colliding key pair, including the one you do not use — leaving it out is not "ignore it", it is "let it overwrite the other". |
| **History depth** | The three venues that cannot answer a 12-month request, measured 2026-09-04: **OKX** keeps ~3 months and answers beyond it with an EMPTY array and `code "0"` (not an error); **Gate** refuses outright — `from time exceeds 180-day limit` — so the fetcher clamps to 179 days rather than sending a request it knows will fail; **Paradex** has no settlements at all, only a 5-second sample of a cumulative funding index. Never assume the corpus is as deep as it was asked for; read the coverage. |
| **Candle depth** | Hourly candles are NOT uniformly deep either, measured 2026-09-07: Binance, Bybit, OKX, Gate and Kraken all answer a full year, **Hyperliquid reaches ~208 days and signals the boundary with an EMPTY ARRAY**, not an error — the same shape as OKX's funding retention. Read `store.PriceCoverage` before comparing two sources' basis. |
| **Candle paging** | Six venues, six idioms, and three of them are traps. **Bybit anchors its page on `end`, not `start`**, so a window wider than one page must be walked BACKWARDS — walking `start` forward re-requests the newest page forever and reports 42 days of a 365-day request as a venue retention limit (measured here on the first run). **OKX's `after` is an EXCLUSIVE UPPER bound**, and its plain `candles` endpoint returns nothing past ~300 days while `history-candles` reaches 400+. **Gate stamps candles in SECONDS** — request and response both — while every other venue uses ms, and **Kraken takes seconds in and answers in milliseconds**. Page caps measured: 1500 / 1000 / 1000 / 300 / 2000 / 2000 / ~5000. |
| **Hyperliquid `t`/`T`** | The candle payload carries BOTH `t` (open ms) and `T` (close ms), so Go's case-insensitive JSON fallback let `T` overwrite `t` and every candle came out stamped 1 ms **before** the hour — every cross-venue join found nothing and the venue's basis was silently empty. Same defect as the Binance aggTrade `m`/`M` row below, hit a second time in a second package. Declare BOTH members of a case-colliding pair, including the one you do not use. |
| **History interval** | No venue publishes an interval beside a historical rate — Paradex is the lone exception. Annotating a 12-month backfill with today's interval is a **2× error over months** on symbols Binance moved from 8h to 4h. `interval_sec` is the series' MEASURED modal spacing and every row also keeps `gap_prev_sec`, the real distance to the previous settlement. Two cadences with real weight is a different thing from a few missed settlements and is reported separately (`CadenceLooksMixed`, 10% threshold — Kraken's year has 6 outages in 8,771 gaps = 0.07%). |
| **History stamps** | Settlement stamps are stored VERBATIM. Gate's land 1–3 seconds past the hour, Hyperliquid's carry tens of milliseconds of jitter. Rounding them to a boundary invents a timestamp the venue never published, and the next fetch then misses the primary key and inserts the same settlement again. |
| **Funding cadence** | How often a venue REPUBLISHES funding is not how often it settles, and it is not the price cadence either. Measured over 44 minutes (DATA-REQUIREMENTS §10): kraken/hyperliquid 1s, gate 4s, binance 16s, okx 67s, paradex 71s — and **Bybit 2,639s and still climbing**, because step 2.5 made it publish only when a funding field really changes. So funding freshness needs its OWN per-venue threshold, and for a venue in that mode age proves nothing: the detectors are `source_status` plus "the settlement this reading names has already passed". Paradex is the warning about measurement windows — 18s after four minutes, 71s after forty-four. |
| **Hyperliquid rate limit** | `fundingHistory` costs weight 20 **plus 1 per 20 items returned**, against an aggregated 1200/minute per IP — a 500-row page is 45, so the budget is one page every 2.3s. The shared 200ms page delay is 11× over it and cost two whole series to HTTP 429 during the 2.6 acceptance run. Pace per venue, and back off in SECONDS for a 429: retrying inside the same exhausted minute just spends the attempts. |
| **Units** | Funding interval arrives as hours (Binance), minutes (Bybit), and seconds (Gate) for the same concept. Normalize to seconds in the connector. |
| **"Not listed"** | Per-symbol instrument endpoints answer "market not listed" in THREE shapes (measured 2026-09-03): Paradex → HTTP **404**; OKX → HTTP 200 + `code 51001`; Bybit linear → HTTP 200 + `retCode 10001` "symbol invalid" while Bybit **spot** → `retCode 0` + empty list. All must read as "absent" — treating any as an error lets one unsupported pair blank a venue's whole rule set (found live in step 2.4 when XLMUSDT killed the Paradex source). Every OTHER non-zero code stays a loud error. |
| **Quote bridging** | Pairing a USD-quoted perp with a USDT spot is delta-neutral in the coin and OPEN in USDT/USD — a depeg moves the legs apart and NO figure in this project deducts it. It happens only when `hedge.quote_equivalents` declares it, and every pair it creates carries `QuoteBridged` so the label reaches the reader. Never quietly fold USD into USDT in a comparison, a fee, or a chart. |
| **Assets** | Base/quote must come from what the venue DECLARES, never from slicing the symbol string. OKX swaps leave `baseCcy`/`quoteCcy` empty (spot-only fields — use `ctValCcy`/`settleCcy` for linear); Kraken names BTC "XBT" in symbols but declares `base: "BTC"`, so no alias table exists anywhere; Hyperliquid declares no quote (venue-wide documented "USD"). |
| **Depth: bid order** | Kraken's REST order book returns its BIDS ASCENDING — `bids[0]` is a resting order at a price of **1**, and the best bid is the LAST element. Every other venue puts the best price first. It also has no limit parameter and returns the whole book (~40 KB). Sort unconditionally; never trust a documented order. |
| **Depth: level ceilings** | Exceeding a venue's level limit LOSES THE WHOLE BOOK rather than shortening it: Gate answers HTTP 400 above 300, Paradex says `"Depth: must be no greater than 100."` above 100. And 100 levels is not enough — measured 2026-09-04, 7 of 9 venues do not reach even 0.1% of mid at 100 levels, so depth figures become a ranking of *who returns the most levels*. Ask each venue for its own maximum, and read `covers_0_1pct`/`covers_0_5pct` before comparing two venues' depth. |
| **Contracts** | OKX, Gate and Kraken denominate orders in contracts, not coins (`ctVal`×`ctMult`, `quanto_multiplier`, `contractSize`). Binance, Bybit, Hyperliquid use coins. ✅ The step-1.2 Kraken contradiction was settled at 2.3: PF_ contracts ARE contract-denominated but `contractSize` is **1 base unit** (with `contractValueTradePrecision` decimals), so contract counts are numerically coin — the survey and the 1.2 measurement were both right. Measured sizes live in the registry; note PF_XRPUSD and Hyperliquid XRP trade in WHOLE XRP (precision/szDecimals 0). |

~~**Known bug:** `coin := symbol[:3]` in the Hyperliquid connector.~~ Fixed in
step 1.4: the venue identifier comes from `config.yaml`
(`symbol_format: "{base}"`), and the acceptance run added a real `DOGEUSDT` —
a four-character base — across every venue.

---

## Layout

```
cmd/scanner/         entrypoint — wires connectors into the engine, serves HTTP
cmd/fundingcheck/    step-2.1 diagnostic: reads BTC funding from all 7 venues
                     over REST and verdicts the survey's traps against live
                     data — deliberately shares NO code with the connectors,
                     so a connector bug cannot confirm itself
cmd/backfill/        step-2.6 one-off: fills funding_history from the venues'
                     history endpoints and reports how deep each series really
                     reached. Safe to re-run — every row is keyed by settlement
exchanges/           the venue-integration tree — PUBLIC DATA ONLY, no credentials
                     the root package is the shared KERNEL: types, Feeds, the
                     RunStream lifecycle, FetchJSON, and the normalization
                     helpers (DeriveFundingRates, FinishDepthBook,
                     FinishFundingHistory) every venue shares
  <venue>/           one package per venue (binance, bybit, okx, gate, kraken,
                     hyperliquid, paradex, pyth): connector, funding, depth,
                     instruments, funding history — and its own testdata/ with
                     the real recordings its golden tests replay
  venues/            the five registry tables (Connectors, DepthFetchers,
                     InstrumentFetchers, FundingHistoryFetchers,
                     PriceHistoryFetchers) — the ONE package that imports every
                     venue, and where the cross-venue REST capture tests live
  exchangestest/     shared test harness: recorder, capture tool, and the
                     contract checkers every venue's recording must pass
internal/
  scanner/           the engine: price state, staleness, the wire contract
  depth/             order book -> liquidity figures; contract->coin conversion
  instruments/       trading rules, spot<->perp mapping, delta-neutral sizing
  fees/              fee table, net profit
  history/           venue REST -> store; the only place that knows both
  store/             SQLite persistence
  strategy/          net APR, slippage from depth, entry and exit signals
                     ⚠️ the ONLY package allowed to say "net" (step 3.1)
  backtest/          historical replay — MUST call strategy, never re-grow a
                     rule (two AST tests enforce it)
  notify/            Telegram and Discord alerts — step 3.4, DEFERRED behind 3.5
  broker/            ⚠️ THE ONLY PACKAGE HOLDING CREDENTIALS
  execution/         delta-neutral position open and close
  risk/              margin, kill switch, capital limits — since 2026-09-07 it
                     holds the perp liquidation model strategy calls
static/              vanilla JS dashboard
docs/                PLAN.md, DATA-REQUIREMENTS.md, CONVENTIONS.md
config.yaml          pairs, venues, thresholds, fees, symbol mapping
```

**`config.yaml` is the single source of truth** for anything operational. Adding
a pair or a venue is a change to that file and nothing else — the dashboard
builds its whole source list from the `meta` message, which is built from the
config. The one thing it cannot supply is a connector for a venue nobody has
written one for; `connector:` picks from `exchanges.Connectors()`, and
`cmd/scanner` has a test that the two lists agree in both directions.

Per-venue symbol naming lives there too (`symbol_format`, `symbol_map`), which is
what removed six hardcoded translation tables from `exchanges/`.

The remaining empty packages under `internal/` (`backtest`, `notify`, `broker`,
`execution`, `risk`) contain only `doc.go` stating their responsibility and
boundaries. Read the relevant `doc.go` before adding code to one — the
boundaries written there are the contract, not a suggestion.

### Dependency rules — blocking, not advisory

```
exchanges/        must NOT import any internal/ package
internal/broker/  must NOT be reachable from the data ingestion path
strategy, backtest  receive NORMALIZED values only, never raw venue payloads
```

Public market data and credentials live on opposite sides of that line.

---

## Working in this repo

```bash
go run ./cmd/scanner  # reads ./config.yaml, serves http://localhost:8082
                      # (run from repo root; -config picks another file)
go build ./...
gofmt -l .            # must print nothing
go vet ./...
go test ./...         # offline — no test opens a network socket
go run ./cmd/fundingcheck  # live re-check of the funding-field survey (network)

# Fill the phase-3 funding corpus from the venues' history endpoints (network,
# minutes). Safe to re-run: a second pass over the same window inserts nothing.
go run ./cmd/backfill                 # 12 months, every configured pair
go run ./cmd/backfill -months 6 -symbol BTCUSDT

# Fill price_history — hourly candles for both legs, which is what makes the
# basis exit evaluable in a replay. 8 sources x 4 pairs x 8,760 hours; ~5 min.
go run ./cmd/backfill -prices -months 12

# Replay the production entry/exit rules over the stored corpus (offline: reads
# SQLite, writes nothing back). Prints its assumptions with every report.
go run ./cmd/backtest                        # 6 months, every hedgeable pair
go run ./cmd/backtest -sweep -csv out.csv    # parameter sweep, parallel
go run ./cmd/backtest -sweep -months 12 -min-rate-bps 0.3,0.5,0.8,1.2,2,3,5 \
  -persist 1,2,3,6 -min-net-apr 0.02,0.05 -exit-net-apr 0,0.0025,0.005 \
  -exit-persist 1,3,6,12 -notional 20000,50000,200000 -hold-days 14,30,60 \
  -csv wide.csv -trades-csv trades.csv -top 40   # the 6,048-set grid of 2026-09-07

# Step 3.5's journal-only run: same binary, strategy: block enabled in
# config.yaml. PORT picks the port (8082 belongs to the phase-1 soak).
PORT=8085 go run ./cmd/scanner
sqlite3 data/scanner.db "SELECT datetime(evaluated_at_ms/1000,'unixepoch'), symbol, perp_source, action FROM signal_journal ORDER BY 1 DESC LIMIT 28"


# Re-measure what a stored price sample costs on disk before changing
# storage.price_sample_every_sec — the row count is linear in it.
MEASURE_STORE=1 go test -run TestPriceSnapshotRowCost -v ./internal/store/
go test -race ./...   # required for any goroutine change

# Re-record each venue's testdata/ from the live venues. Opens real sockets, so
# it is skipped by default; run it when a venue changes its payloads.
CAPTURE_TESTDATA=1 go test -run TestCapture -timeout 10m ./exchanges/...
go test -race ./...   # required for any goroutine change
```

Stack: Go 1.23.5, `gorilla/websocket`, `joho/godotenv`, `gopkg.in/yaml.v3`, and
`modernc.org/sqlite` — the PURE-GO SQLite driver, pinned at v1.38.2 because it
is the newest release still targeting go1.23. It was chosen over the cgo driver
so `CGO_ENABLED=0` static builds keep working and the deploy target needs no C
toolchain; the cost is a larger module graph and slower bulk writes, which at 36
series is not a constraint. Frontend is vanilla JS with TradingView Lightweight
Charts — keep it that way, no framework migration.

Identifiers, comments and commit messages are **English**. Documents in `docs/`
and user-facing UI strings are Vietnamese. Full conventions in
[docs/CONVENTIONS.md](docs/CONVENTIONS.md).

---

## Known weaknesses in current code

Do not build on top of these without addressing them; they are scheduled in
phase 1.

- ~~`Timestamp` is discarded, and means two different things depending on the
  venue.~~ Fixed in step 1.1: the field is `VenueTimeMs`, no venue fills it with
  the local clock, and `RecvAt` is stamped by the scanner in exactly one place
  (`updatePrice`). Staleness is measured only from `RecvAt`.
- ~~Top-of-book size is parsed and thrown away.~~ Collected in step 1.2 for the
  five coin-denominated sources, and finished in step 2.7b: OKX, Gate and Kraken
  now send their size as `Best*QtyContracts` and the scanner multiplies by the
  registry's `ContractSizeCoin`. Measured live 2026-09-04 — gate 1.3384, okx
  1.8104, kraken 0.0369 BTC, all three previously 0. **8 of 9 sources** carry a
  real quantity; Paradex publishes none and stays 0. **`0` still means "not
  known", never "no liquidity"** — and a market the registry does not know stays
  0 rather than being published unconverted, because Gate's 0.0001 BTC contract
  would otherwise report ten thousand times the real size.
- ~~Pyth is treated as a tradeable venue.~~ Fixed in step 1.2: an oracle never
  enters a comparison group and can no longer appear at either end of an alert.
- ~~`checkArbitrage` takes min/max across *all* sources, mixing spot and perp.~~
  Fixed in step 1.2: two sources are compared only when they share a market type
  **and** a quote asset. Alerts are raised inside one tradable group, so the
  alerts table can no longer name an unexecutable pair. Spot vs perp on one venue
  is reported separately as `basis[]`. The rules live in
  [internal/scanner/grouping.go](internal/scanner/grouping.go).
- ~~No fee model anywhere.~~ Step 1.3 added `internal/fees/`. It is **commission
  only**: a taker fill on all four legs of opening and closing a two-venue
  position. Call the output **"after trading fees", never "net profit"** —
  slippage needs book depth and funding is phase 2. As of 2026-09-07 **8 of 9** sources carry a
  verified schedule — bybit, okx, gate and bybit_spot were read from the
  venues' own pages by the user that day (the fetches time out or demand a
  login from this environment); only the oracle stays unverified, because it
  has no fee. Every verified figure is the venue's PUBLIC DEFAULT tier
  (Binance "Regular User", Kraken tier 1, Hyperliquid tier 0, Bybit/OKX/Gate
  VIP 0), never an account's real tier. `fee_verified: false` still means
  *not looked up*, not *free* (Paradex really does charge retail 0%), and any
  pair touching one publishes `spread_after_fees_pct: null`.
- Measured on live data: a round trip costs about **0.19%** while cross-venue
  spreads on the majors run a few thousandths of a percent, so **every alert the
  scanner currently raises is negative after fees**. The alert threshold still
  fires on the gross spread by design — deciding what is worth acting on is
  phase 3 — but nothing displays a gross figure without labelling it.
- ~~Reconnect uses a fixed sleep with no backoff, no ping/keepalive, no read
  deadline.~~ Fixed in step 1.5. All nine WebSocket connectors share one
  lifecycle (`runStream` in [exchanges/stream.go](exchanges/stream.go)):
  exponential backoff 2s→60s, a read deadline that also counts pongs and server
  pings as activity, a keepalive per the venue's own documentation, and a stop
  through `ctx` that closes the socket underneath a blocked read. Pyth is SSE and
  keeps its own loop, sharing the backoff and the cancellation.
- ~~`broadcastSpreads` recomputes an O(n²) matrix and writes to every client on
  every single price tick.~~ Throttled in step 2.7a, after measuring what it
  actually cost: **101,627 `spreads` frames in 30 seconds to ONE client —
  3,387/s, 11.3 MB/s, 99.85% of every frame on the wire.** The newest matrix per
  symbol is now queued and flushed on a 200ms ticker, so the rate is bounded at
  `symbols × 5/s` — measured 20.0/s and 65 KB/s afterwards. Alerts are not
  queued. Still open, and now the dominant cost: the server ships every symbol
  to every client (PLAN §7.3 item 2), so 50 symbols would be 250 msg/s.
- 483 test functions (`grep -r '^func Test' --include='*_test.go'`, re-measured 2026-09-07; most
  table-driven so the case count is far higher; earlier docs quoted a "211
  tests" figure whose counting method did not survive — this one is stated so
  it can be re-measured): `internal/strategy` 35 (96.7%),
  `exchanges` 90 (58.8% of statements),
  `internal/scanner` 114 (86.5%), `internal/instruments` 27 (96.9%),
  `internal/store` 18 (84.2%), `internal/config` 34 (84.0%),
  `internal/depth` 10 (95.3%), `internal/history` 6 (76.5%),
  `internal/fees` 5 (100%), `cmd/scanner` 19, `cmd/backfill` 6,
  `cmd/fundingcheck` 3. The `exchanges` percentage FELL from
  61.8% at step 1.6 while the test count rose: step 2.6 added seven history
  fetchers whose pagination loops only run against live venues. Their parsers
  and the cadence arithmetic are golden-tested against recorded payloads; the
  loops are not, and pretending otherwise with a mock HTTP server would test
  the mock.
  Each `exchanges/<venue>/testdata/` holds that venue's real recordings;
  re-record with `CAPTURE_TESTDATA=1 go test -run TestCapture ./exchanges/...`. **Pyth has
  no recording** - hermes.pyth.network answers 401 - so its fixture is synthetic
  and labelled as such; it proves the arithmetic, not that Pyth's current format
  still matches what the connector decodes.
- A subscription the venue silently drops is never re-established. The read
  deadline is refreshed by ANY frame, and Bybit, OKX and Hyperliquid answer
  keepalives with ordinary data frames, so a socket that stays open with a dead
  subscription looks healthy to the connector forever. The scanner notices
  (silence downgrades the state) but cannot act. Found in review at step 1.6.
  The 72h soak did not trigger it — all 36 series were live at 91h — which is
  absence over one run, not a fix.
- Bybit's `orderbook.1` pushes snapshot **and** delta and the connector does not
  distinguish them, so a delta deleting the top level (size `"0"`) is taken at
  face value. This predates step 1.2 and affects the price as well as the new
  quantity. Fixing it means merging deltas into cached state — trap 3 below.
- ~~`fetchInstrumentJSON`'s name lied about three of its four jobs.~~ Paid in
  the exchanges/ reorganization (2026-09-04): it is `exchanges.FetchJSON` now —
  the shared venue-REST GET with the not-listed sentinel and rate-limit
  handling, named for what it does.
- The funding corpus is **not uniformly deep and never will be** — see the
  History depth trap row. Anything that ranks or backtests across venues has to
  read `store.FundingCoverage` first, or it is comparing a year of one venue
  against three months of another and calling the difference a signal.
- Paradex's history reach is capped at ~83 days by `maxFundingHistoryPages`,
  because one hour of its corpus costs one HTTP request. That is this tool's
  limit, not the venue's; a deeper corpus means more runs or a higher cap.
- **Depth is stored and published as AGGREGATES, never as levels** — two
  cumulative figures a side (within 0.1% and 0.5% of mid), the best price, the
  spread, and how far the response reached. There is no level list anywhere, in
  memory or in the corpus. So `strategy.EstimateFill` reconstructs a piecewise
  linear cumulative curve through the points that exist and integrates along it.
  That assumes liquidity is spread evenly inside a window; real books are denser
  near the touch, so the estimate is **too expensive**, which is the safe
  direction. Anything wanting a sharper fill model has to store levels first —
  and depth cannot be backfilled, so that decision only ever applies forward.
- ~~Nothing models the perp leg's margin.~~ Added 2026-09-07: `internal/risk`
  prices the short perp leg (liquidation price = entry × (1+margin)/(1+mm)),
  `strategy` gained a `margin_known` ENTRY refusal and a `margin_thin` RISK
  exit, and the backtest detects liquidation from the candle **HIGH** — a short
  dies on a spike and an hourly close steps over it. Maintenance brackets are in
  `config.yaml` per source, read from the venues' PUBLIC endpoints on
  2026-09-07: bybit 0.33% (tier ≤$300k), okx 0.4%, gate 0.3% (≤500k), kraken
  0.5% (≤$1M). **Binance is `verified: false` because its `leverageBracket`
  needs an API key**, and Hyperliquid's is derived per coin from `maxLeverage`
  rather than published — both are refused at entry rather than assumed free.
  **Measured, and the result is one-way**: on the same 16 series over 12 months,
  no leverage sums +9.54% with 0 liquidations, 3× +8.18% with 2, 10× −2.76%
  with **18**, 20× −19.28% with 21. Monotone — there is no optimum in the
  middle. And it FLATTERS leverage: the equity curve charges only the round trip
  on a liquidation, not the posted margin (18 × 10% = 180% of a notional,
  unrecorded). Using leverage on the perp leg to free capital is arithmetically
  wrong on this corpus. Ships at 0.
- ~~The basis exit cannot be evaluated in a backtest.~~ Fixed 2026-09-07 by
  `price_history`: hourly candles for all 8 tradable sources, backfilled 12
  months (`go run ./cmd/backfill -prices`). Unlike depth, candles CAN be
  refetched, which is why this one was fixable and slippage is not. The price
  used at a settlement is the newest candle to have **fully closed** at or
  before it — never the candle containing it, whose close is stamped up to an
  hour in the future — so it is at most one interval stale, and a leg with no
  candle within two intervals still reports `NotEvaluated`. Measured: 18 of 24
  series now report `basis_not_evaluable: 0`; the six hyperliquid/kraken ones
  keep a residue where funding reaches 365 days and candles 208. **On the 8
  quote-bridged series the measured basis is the coin basis PLUS the USD/USDT
  spread**, which is stated in the assumptions block — the exit there is partly
  watching the bridging risk nothing deducts.
- **`depth_snapshots` holds one sample per source/pair** (2026-09-04), left over
  from the step-2.7b acceptance sweep. The live scanner fills it going forward,
  but there is **no historical depth**, so a backtest cannot model slippage from
  the book as it was — it has to take slippage as a stated parameter and say so.
  Every hour the scanner is not running is an hour of depth nobody can recover.

---

## Document map

| Document | Contents |
|---|---|
| [docs/PLAN.md](docs/PLAN.md) | 9 phases, 40 steps, acceptance criteria, risk register, open decisions |
| [docs/WORKFLOW.md](docs/WORKFLOW.md) | The 9-phase loop every change follows, review checklist, commit rules |
| [docs/DATA-REQUIREMENTS.md](docs/DATA-REQUIREMENTS.md) | What data is needed from a venue, 7-venue funding survey, `FundingData` design, data traps |
| [docs/CONVENTIONS.md](docs/CONVENTIONS.md) | Naming, structure, errors, concurrency, testing, dependency rules |
| [docs/WS-CONTRACT.md](docs/WS-CONTRACT.md) | The frozen backend↔dashboard JSON contract, and which field carries real data at which step |
| [README.md](README.md) | Project introduction, current capabilities and gaps, setup, risk notice |
