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

**Phase 1 — Hardening.** 7 of 7 steps done; the 72h unattended run is still owed. Step 1.0 froze the WebSocket JSON
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

**The remaining phase-1 work is the 72h unattended run — now in progress**
(started 2026-09-03 14:03, deadline 2026-09-06; PID in `.soak/scanner.pid`,
port 8082 — do not touch it, and run anything else on another port). It was
started with the step-1.6 defect still open: a subscription the venue silently
drops is detected but never re-established, because the read deadline is
refreshed by any frame and three venues answer keepalives with data frames.
The run may therefore mostly re-demonstrate that hole; a separate session
delivers the soak verdict and updates phase-1 status — no other session marks
phase 1 done.

Phase 2 (Funding Rate Monitor) has started in parallel without touching the
soak process: step 2.1 (`cmd/fundingcheck`) verified the funding fields of all
7 venues against live APIs on 2026-09-03 and corrected the survey — see
[docs/DATA-REQUIREMENTS.md §3](docs/DATA-REQUIREMENTS.md).

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
skew, not age. `RecvAt` is stamped **at the socket read** — in exactly two places,
one per transport: `runSession` in
[exchanges/stream.go](exchanges/stream.go) for the nine WebSocket connectors, and
the SSE read loop in [exchanges/pyth.go](exchanges/pyth.go). Never add a third.
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
| **Kraken** | `funding_rate` is an absolute price amount, not a rate — verified live: absolute ÷ relative ≈ index price. Use `relative_funding_rate`. Settles hourly and the relative rate is **per 1h, used as-is** — ×8 for the 8h comparison, never ÷8 (correction history: DATA-REQUIREMENTS §3.2②). Its WS `next_funding_rate_time` is an **absolute epoch-ms stamp** even though the doc prose says "time until" — probed live twice; see §3.3⑥. |
| **Bybit** | Ticker pushes snapshot AND delta. A field absent from a message means unchanged, not zero. Merge into cached state; never overwrite. |
| **Binance** | `fundingInfo` documents itself as returning ONLY symbols whose config differs from default — as of 2026-09-03 it happens to cover every TRADING perpetual (777 symbols, BTCUSDT included via its adjusted ±0.3% cap), but the docs promise no such coverage. Default to 8h and override; do not read it as the source of truth for all symbols. Intervals seen: 4h (majority), 8h, and 1h. Also filter `rateType: "Special"` in backtests. |
| **Hyperliquid** | Funding is hourly, not 8-hourly. Annualizing as 8h is wrong by 8x. |
| **Paradex** | Funding V2 accrues continuously via a funding index. There is no settlement timestamp. |
| **Binance** | The aggTrade payload carries both `m` (buyer is maker) and `M` (deprecated, always true). Go's `encoding/json` prefers an exact tag match but **falls back to a case-insensitive one**, so declaring only `m` let `M` overwrite it and every trade came out a sell. Declare BOTH members of every case-colliding key pair, including the one you do not use — leaving it out is not "ignore it", it is "let it overwrite the other". |
| **Units** | Funding interval arrives as hours (Binance), minutes (Bybit), and seconds (Gate) for the same concept. Normalize to seconds in the connector. |
| **Contracts** | OKX, Gate and Kraken denominate orders in contracts, not coins (`ctVal`×`ctMult`, `quanto_multiplier`). Binance, Bybit, Hyperliquid use coins. ⚠️ Step 1.2 measured Kraken's *book* quantity looking coin-denominated (PF_XBTUSD 0.0929 with BTC near $77.5k), which contradicts this row. Unresolved — the instrument registry settles it; until then Kraken reports no quantity. |

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
exchanges/           WebSocket connectors — PUBLIC DATA ONLY, no credentials
  testdata/          one real recording per venue, a frame per line
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
go run ./cmd/scanner  # reads ./config.yaml, serves http://localhost:8082
                      # (run from repo root; -config picks another file)
go build ./...
gofmt -l .            # must print nothing
go vet ./...
go test ./...         # offline — no test opens a network socket
go run ./cmd/fundingcheck  # live re-check of the funding-field survey (network)
go test -race ./...   # required for any goroutine change

# Re-record exchanges/testdata/ from the live venues. Opens real sockets, so it
# is skipped by default; run it when a venue changes its payloads.
CAPTURE_TESTDATA=1 go test -run TestCaptureTestdata -timeout 5m ./exchanges/
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
- ~~Top-of-book size is parsed and thrown away.~~ Collected in step 1.2 for the
  five sources that publish it in **coin**: Binance futures/spot (`B`/`A`), Bybit
  futures/spot (level index 1), Hyperliquid (`sz`). OKX, Gate and Kraken publish
  **contract counts** and Paradex publishes no size at all, so those four stay 0.
  **`0` means "not known", never "no liquidity"** — converting needs
  `ctVal`×`ctMult` / `quanto_multiplier` from an instrument registry that arrives
  in phase 2. The units were settled by measurement, not documentation; the
  numbers are on `exchanges.OrderbookData`.
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
  slippage needs book depth and funding is phase 2. Only **5 of 9** venues have a
  verified schedule; the rest carry `fee_verified: false`, which means *not
  looked up*, not *free* (Paradex really does charge retail 0%), and any pair
  touching one publishes `spread_after_fees_pct: null`.
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
- `broadcastSpreads` recomputes an O(n²) matrix and writes to every client on
  every single price tick.
- 150 test functions (`grep -r '^func Test' --include='*_test.go'`, most
  table-driven so the case count is far higher; earlier docs quoted a "211
  tests" figure whose counting method did not survive — this one is stated so
  it can be re-measured): `exchanges` 32 (63.6% of statements),
  `internal/scanner` 89 (89.7%), `internal/config` 19 (78.2%),
  `internal/fees` 5 (100%), `cmd/scanner` 2, `cmd/fundingcheck` 3 (the pure
  normalization/coherence functions; the fetchers run only against live
  venues).
  `exchanges/testdata/` holds a real recording per venue; re-record with
  `CAPTURE_TESTDATA=1 go test -run TestCaptureTestdata ./exchanges/`. **Pyth has
  no recording** - hermes.pyth.network answers 401 - so its fixture is synthetic
  and labelled as such; it proves the arithmetic, not that Pyth's current format
  still matches what the connector decodes.
- A subscription the venue silently drops is never re-established. The read
  deadline is refreshed by ANY frame, and Bybit, OKX and Hyperliquid answer
  keepalives with ordinary data frames, so a socket that stays open with a dead
  subscription looks healthy to the connector forever. The scanner notices
  (silence downgrades the state) but cannot act. Found in review at step 1.6.
- Bybit's `orderbook.1` pushes snapshot **and** delta and the connector does not
  distinguish them, so a delta deleting the top level (size `"0"`) is taken at
  face value. This predates step 1.2 and affects the price as well as the new
  quantity. Fixing it means merging deltas into cached state — trap 3 below.

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
