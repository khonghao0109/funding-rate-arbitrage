# CLAUDE.md

Context for AI agents working in this repository. Read this before touching code.

---

## What this project is

A Go service that connects to 9 crypto venues over WebSocket, normalizes their
order book data, and surfaces price dislocations in real time through a browser
dashboard.

**It is a read-only scanner today.** It holds no credentials, places no orders,
and persists nothing. Every number it displays is derived from public market
data held in memory.

## Where it is going

The target is a **Funding Rate Arbitrage bot**: hold spot long and perpetual
short of equal notional, collect the funding payment each settlement period,
stay delta-neutral throughout. After that, Basis Trade. The full roadmap is 9
phases and 40 steps in [docs/PLAN.md](docs/PLAN.md).

Expected return for the funding strategy is **5–15% APR**. Anyone or anything
proposing a design that implies far more than that has misunderstood the
strategy, not discovered an edge.

## Current phase

**Phase 1 — Hardening.** 2 of 7 steps done. Step 1.0 froze the WebSocket JSON
contract for the whole phase — it is specified in
[docs/WS-CONTRACT.md](docs/WS-CONTRACT.md) and **must not be reshaped** before
phase 2: steps 1.1–1.3 fill data into fields that already exist. Step 1.1 added the staleness filter. The next task is
step 1.2, separating spot from perpetual from oracle; the contract already has
`cross_venue_groups[]`, `basis[]`, `oracle_deviation[]` and the
`market_type`/`quote_asset`/`tradable` flags, and the dashboard already renders
all three blocks, so 1.2 only changes how the backend groups sources.

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
maker/taker fees, funding cost, and estimated slippage. No fee calculation
exists yet, so every number the UI shows today is gross. Label it as such
wherever it surfaces.

**3. Never hardcode a funding interval.** Not 8h, not anything. Hyperliquid
settles hourly, Kraken settles hourly but quotes for an 8h window, Paradex
accrues continuously with no discrete settlement, Binance runs 8h or 4h. See
rule 5.

**4. Name the unit in the identifier.** `IntervalSec`, `RatePer8hFrac`,
`NotionalUSD`, `TakerFeeBps`, `QtyContracts`. A bare `rate`, `size`, or
`interval` crossing a function boundary is a defect, not a style preference.
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
skew, not age. The scanner stamps `RecvAt` in exactly one place
([main.go](main.go) `updatePrice`) — never add a second. Staleness thresholds are
per venue and measured; the numbers and how they were obtained are on
`sourceRegistry` in [wire.go](wire.go).

---

## Domain traps already discovered

Surveyed 2026-08-28 across all 7 futures venues. Full detail and confidence
levels in [docs/DATA-REQUIREMENTS.md §3](docs/DATA-REQUIREMENTS.md). Do not
re-research these; do verify before writing the integration.

| Venue | Trap |
|---|---|
| **OKX** | `fundingTime` is the NEXT settlement; `nextFundingTime` is the one AFTER that. Mapping it like Binance's `T` is off by one period. |
| **Kraken** | `funding_rate` is an absolute price amount (`-6.26e-11`), not a rate. Use `relative_funding_rate`. Settles hourly but quotes for an 8h realization window. |
| **Bybit** | Ticker pushes snapshot AND delta. A field absent from a message means unchanged, not zero. Merge into cached state; never overwrite. |
| **Binance** | `fundingInfo` returns ONLY symbols whose config differs from default. Default to 8h and override; do not read it as the source of truth for all symbols. Also filter `rateType: "Special"` in backtests. |
| **Hyperliquid** | Funding is hourly, not 8-hourly. Annualizing as 8h is wrong by 8x. |
| **Paradex** | Funding V2 accrues continuously via a funding index. There is no settlement timestamp. |
| **Units** | Funding interval arrives as hours (Binance), minutes (Bybit), and seconds (Gate) for the same concept. Normalize to seconds in the connector. |
| **Contracts** | OKX, Gate and Kraken denominate orders in contracts, not coins (`ctVal`×`ctMult`, `quanto_multiplier`). Binance, Bybit, Hyperliquid use coins. |

**Known bug:** [exchanges/hyperliquid.go:58](exchanges/hyperliquid.go#L58) uses
`coin := symbol[:3]`. It works only because all four current symbols have
3-character bases. `DOGEUSDT` would silently subscribe to `DOG`. Fix scheduled
for step 2.4.

---

## Layout

```
cmd/scanner/         entrypoint — wires connectors into the engine, serves HTTP
exchanges/           WebSocket connectors — PUBLIC DATA ONLY, no credentials
internal/
  scanner/           the engine: price state, staleness, the wire contract
  instruments/       trading rules, spot<->perp mapping, delta-neutral sizing
  fees/              fee table, net profit
  store/             SQLite persistence
  strategy/          APR, entry and exit signals
  backtest/          historical replay
  notify/            Telegram and Discord alerts
  broker/            ⚠️ THE ONLY PACKAGE HOLDING CREDENTIALS
  execution/         delta-neutral position open and close
  risk/              margin, kill switch, capital limits
static/              vanilla JS dashboard
docs/                PLAN.md, DATA-REQUIREMENTS.md, CONVENTIONS.md
```

Packages under `internal/` currently contain only `doc.go` stating their
responsibility and boundaries. Read the relevant `doc.go` before adding code to
one.

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
go run ./cmd/scanner  # starts on http://localhost:8082 (run from repo root)
go build ./...
gofmt -l .            # must print nothing
go vet ./...
go test ./...         # no tests exist yet — phase 1 step 1.6 adds the first
go test -race ./...   # required for any goroutine change
```

Stack: Go 1.23.5, `gorilla/websocket`, `joho/godotenv`. Frontend is vanilla JS
with TradingView Lightweight Charts — keep it that way, no framework migration.

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
- Top-of-book size is parsed and thrown away. `OrderbookData` carries only
  prices, while the connectors already decode quantities (Binance `B`/`A`,
  Bybit `v`, OKX `sz`). That is a free first-order liquidity filter going
  unused.
- Pyth is treated as a tradeable venue. `checkArbitrage` walks every entry in
  the price map, so the scanner can report "buy on Pyth, sell on Binance",
  which is meaningless — Pyth is an oracle.
- `checkArbitrage` takes min/max across *all* sources, mixing spot and perp into
  one comparison. That produces "opportunities" that cannot be executed — what
  it is actually measuring, spot vs perp, is basis. The spread matrix now
  declares itself `tradable: false` for this reason, but the **alerts table is
  still built from the mixed comparison**; step 1.2 fixes the calculation.
- No fee model anywhere.
- Reconnect uses a fixed sleep with no backoff, no ping/keepalive, no read
  deadline.
- `broadcastSpreads` recomputes an O(n²) matrix and writes to every client on
  every single price tick.
- No `exchanges/testdata/` and no connector tests — golden tests need real
  payloads captured from a running scanner first. `main` has 39 tests covering
  the wire contract and the staleness filter (52.5% of statements); `exchanges/` is still at zero.

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
