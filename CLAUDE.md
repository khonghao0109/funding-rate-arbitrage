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

**Phase 1 — Hardening.** Not started. The immediate next task is step 1.1:
add a staleness filter by replacing `map[string]float64` with a price type that
carries a receive timestamp, in [main.go](main.go).

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
main.go              entrypoint and wiring
exchanges/           WebSocket connectors — PUBLIC DATA ONLY, no credentials
internal/
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
go run main.go        # starts on http://localhost:8082
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

- `Timestamp` is discarded when order book data becomes a price
  ([main.go](main.go)), so a disconnected venue keeps producing signals from a
  frozen price.
- `checkArbitrage` takes min/max across *all* sources, mixing spot and perp into
  one comparison. That produces "opportunities" that cannot be executed — what
  it is actually measuring, spot vs perp, is basis.
- No fee model anywhere.
- Reconnect uses a fixed sleep with no backoff, no ping/keepalive, no read
  deadline.
- `broadcastSpreads` recomputes an O(n²) matrix and writes to every client on
  every single price tick.
- Zero test coverage.

---

## Document map

| Document | Contents |
|---|---|
| [docs/PLAN.md](docs/PLAN.md) | 9 phases, 40 steps, acceptance criteria, risk register, open decisions |
| [docs/WORKFLOW.md](docs/WORKFLOW.md) | The 9-phase loop every change follows, review checklist, commit rules |
| [docs/DATA-REQUIREMENTS.md](docs/DATA-REQUIREMENTS.md) | What data is needed from a venue, 7-venue funding survey, `FundingData` design, data traps |
| [docs/CONVENTIONS.md](docs/CONVENTIONS.md) | Naming, structure, errors, concurrency, testing, dependency rules |
| [README.md](README.md) | Project introduction, current capabilities and gaps, setup, risk notice |
