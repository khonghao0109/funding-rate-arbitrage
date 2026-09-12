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
stay delta-neutral throughout. After that, a second strategy — **Crowding
Reversal**, which is DIRECTIONAL (long/short BTC/ETH perps against the crowd's
long/short account ratio), ported to Go from a Python research package and
gated on parity with its fixture. It replaced Basis Trade on 2026-09-11
(decision Q11 in PLAN §7.1 — a roadmap decision, reversible, not a verdict on
basis trading). The full roadmap is 9 phases and 42 steps in
[docs/PLAN.md](docs/PLAN.md).

Expected return for the funding strategy is **5–15% APR**. Anyone or anything
proposing a design that implies far more than that has misunderstood the
strategy, not discovered an edge. That band is the FUNDING strategy's, stated
as simple APR on one leg's notional. The phase-6 crowding branch is a different
strategy with different numbers: its research package reports 27–37% CAGR after
an ASSUMED 5 bps/side and historical funding, before measured slippage, on one
venue over 4.5 years, with a validation segment its authors say was inspected
repeatedly. Read PLAN.md phase 6 before repeating or contesting either figure;
never present the two as comparable, additive, or "net", and never call the
system as a whole delta-neutral once phase 6 runs.

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
on port 8082 and **died with the machine reboot of 2026-09-10 01:13 +07**, the
same reboot that killed step-3.5 run 1; `.soak/` keeps its record (last log
line 01:10) and **port 8082 is free** — re-measured 2026-09-12.

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

**Re-measured on 2026-09-07 evening from `906ac42`, after 3.3b/3.3c** — the
same grid plus the two axes the shipped set had moved to (`exit-persist 48`,
`hold-days 90`), so the live configuration sits INSIDE the grid: 12,288 sets ×
24 series × 3 windows.

**READ THE DENOMINATOR FIRST.** A set's headline figure is a SUM over 24
series, each measured on its own full notional. It ranks parameter sets and is
not a return — the user caught this being quoted as one. Divide by 24 and then
by the capital both legs tie up (2× notional, because the spot leg cannot be
levered) to get a number comparable to the project's 5–15% target. Over 12
months: shipped sums +27.56% = **+0.57%/yr per series on capital** (17/24
positive, 1.7 trades each); hold-through sums +29.73% = **+0.62%**; the grid's
best sums +27.81% = +0.58%; the retired 3.3 set sums −526% = −10.96% at 73
trades each. **0 of 12,288 sets reach hold-through.** Two verdicts, not one:
the rule does not beat buying and holding, AND everything on the page is an
order of magnitude below the target band.

**No backtest this project has ever run reached 5–15% on capital.** Swept every
stored result — 36 files, ~2.1M traded replays: the maximum is **+3.45% on
capital** (BTC/hyperliquid, 1 trade, the 3-month window, and a quote-bridged
pair); over 12 months the maximum is +2.98%. Earlier claims that the band had
been reached ("hyperliquid BTC +5.44%", step 3.1's "7.40% net") were quoted on
NOTIONAL and are half that on capital; 3.1's figure was additionally a
projection from an assumed funding rate, not a replay. The band is for SELECTED
opportunities, not majors held mechanically: BTC/binance averaged 0.306 bps/8h
for the year, which is ~0.55%/yr on capital before a 0.30% round trip. Reaching
the band needs a different lever — higher-funding pairs, maker fills (20 of the
30 bps), or both legs on one venue so capital is N rather than 2N — not another
parameter grid. Two things it did change. The basis exit is
EVALUABLE for the first time (20/24 series report 0 unpriced settlements over
67,121 of them; the residue is entirely Hyperliquid, whose candles stop at
~208 days) and it **costs 2.94 points and 10 round trips a year, all of it on
kraken** — measured by replaying the shipped set twice with only the basis
thresholds changed, which is the isolation `cmd/backtest` cannot do in a sweep
because `baseParams` hardcodes `MaxBasisPct`/`MaxBasisWidenPct`. And the
leverage table gained the capital denominator (above). Report:
`docs/reports/backtest-3.3b-3.3c-2026-09-07.html`; the generator and the three
traps it records are in `tools/report/`.

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
reconstruct the old set from `git show 2328307:config.yaml` exactly as it
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

**The hold side is saturated (measured 2026-09-09, 3,200 sets × 24 series ×
12 and 6 months, `tools/report/hold.py`).** The question was how long a trade
must be held to break even and which of `holding_days`, the decay window N,
the min-hold floor M and the sign-flip gates keep it open until it has. Read
straight from the corpus with no rule in between — every settlement as an
entry, days until the funding collected since it reaches the priced round
trip — BTC/ETH at the 8h venues break even in a median **21–52 days** (P75
26–91), hyperliquid in 15–17, and the shipped entry rule's own entries in
12–50; on 6 series (SOL at binance/bybit/gate/kraken, XRP at binance/kraken)
**40% of entries never break even inside the window** because mean funding is
below cost, so no hold rule can save a trade there. On the grid: the shipped
set ranks 31/3,200 at +0.562%/yr on capital and nothing beats it by more than
0.013 points; moving `holding_days` 30→365 shifts it 0.010 and N 12→192 shifts
0.007 — both already flat at the shipped values, because N=48 leaves 0 decay
exits and C=1 leaves 6 sign-flip exits, all on SOL. **M=1 (do not take a yield
exit before the trade has earned its round trip back) is the literal answer to
the question and it makes things worse**: break-even share 56%→62% and yield
exits 6→2, but return +0.562%→+0.542%, because the 4 exits it blocks were
losing SOL trades that then hold to the window end and lose more (SOL/binance
−1.12% → −2.15% on notional). The share rises because the DENOMINATOR falls,
not because any trade was saved — a count-based target is the wrong target on
a series whose break-even horizon is infinite. Hold-through stays unreached
(+0.613%, 0 of 3,200 on 12 months; on 6 months 1,002 sets reach it by entering
later than the window start). **Ranking by return over max drawdown (both on
capital, mean over mean) gives the same order** — shipped 1.30, best 1.35,
hold-through 1.49, 0 sets reach it — because every good set draws down 0.42–
0.44% on the SAME series. The risk lever is not on any hold axis, it is which
series are traded: the shipped set on BTC+ETH only (12 series) makes +1.19%
on capital at 0.18% drawdown, ratio 6.55, 12/12 positive, worst series +0.22%;
on BTC+ETH with USDT-quoted perps (8 series, no USD/USDT bridging) +0.76% at
ratio 4.26. Twice the return at half the drawdown comes from NOT entering
SOL/XRP, and no rule in `strategy` decides that yet. Report:
`docs/reports/backtest-hold-2026-09-09.html`.

**The pair list was screened and expanded on 2026-09-09** (`cmd/pairscreen`
+ `tools/report/pairscreen.py`, report
`docs/reports/pairscreen-2026-09-09.html`). 32 candidates × 7 perps were put
through six criteria, all evaluated: listed → hedge leg by the config's own
declarations → round trip priced at 50k → book fits 50k inside ±0.5% on all
four sides → corpus ≥ 150 days → mean funding ≥ 0.94 bps/8h (5%/yr on capital
at K=2 after a 0.30% trip). **0 of 252 pass all six.** The one combination
above 0.94 (HYPE·hyperliquid 0.946) has no book for 50k. The finding that
matters: **Hyperliquid pays 2–3× the USDT venues on the same coin** — NEAR
0.930, LINK 0.925, UNI 0.916, AAVE 0.861 bps/8h over 365 days at 91% positive,
hold-through **+4.4…+4.8%/yr on capital**, the highest this project has
measured — and all four are quote-bridged (USD perp / USDT spot) at K=2 across
venues, with no spot leg on Hyperliquid to bring K down. Best USDT venues:
HYPE·binance 0.649, LINK·binance 0.425, SUI·bybit 0.412, UNI·binance 0.404.
`config.yaml` now carries 13 pairs: the 4 shipped plus 9 admitted by the
weaker "worth a backtest" verdict (every venue-side and corpus criterion, mean
≥ 0.40 bps/8h), each with its measurements in a comment and the bridged-only
ones (NEAR, AAVE, LTC, BNB, DOGE) saying so. The running 3.5 process loaded
its config once and is unaffected; `cmd/backtest` now replays 13 pairs. Two
traps met: the corpus for a candidate is backfilled AFTER the screen's
snapshot, so a `recorded_at_ms <= snapshot` filter (right for a replay)
silently dropped every Hyperliquid alt on the first run; and OKX's 96-day
mean is not comparable with a 365-day one, so a pair's "best venue" prefers a
deep corpus before a higher number.

**The shipped threshold set was re-run on the expanded universe (2026-09-09)
and it does NOT work there.** Replaying it over 13 pairs — 74 series, 11
refused by name (5 paradex `continuous`, 4 hyperliquid alts whose 20-level
book does not cover 50k inside 0.5%) — gives **−0.061%/yr on capital per
series against hold-through's +0.924%**, beating hold-through on 4/74 series
at 4.6 trades each; the old 24 series are unchanged (+0.568% at 1.7 trades).
**One exit on one venue is the whole difference, and a sweep cannot see it**:
`cmd/backtest` hardcodes the basis thresholds in `baseParams` and the run CSV
carries no basis column, so the measurement is two plain runs side by side
differing only in `max_basis_pct`/`max_basis_widen_pct` (a config copy). With
the basis exit off: **+0.940 points (−0.061% → +0.878%) and 343 → 85 trades**,
all of it in the quote-bridged cohort (+3.16 points; the USDT-quoted cohort
moves 0.000). HYPE·kraken alone goes −28.82% → +3.50% as its trades fall 143
→ 2, and kraken as a venue is −5.06% at 20.8 trades/series against its own
hold-through of +0.27% — the 1.0%/0.5-point thresholds were calibrated on
BTC/ETH, and on kraken's USD alt perps the measured basis carries the
USDT/USD bridge, so it fires about twenty times a year. Fixing that is still
not enough: +0.878% is under hold-through, and a 768-set grid on the same 74
series (every axis around the shipped set) tops out at +0.165%, ranks the
shipped set 183/768, and puts **0 of 768 above hold-through** — every set in
it carries the same basis thresholds. Where the money is: the 5 added
hyperliquid series make **+3.713% on capital at 0.268% drawdown** (ratio 13.8)
against hold-through's +3.876%, and **HYPE·hyperliquid +4.99%** with
LINK·hyperliquid +4.80% are the highest figures this project has measured on
capital — both exactly equal to their own hold-through on ONE trade, so the
rule contributed nothing, and both quote-bridged. Shorter windows say the
same (6 months +0.314% vs +0.469%, 3 months +0.227% vs +0.316%). `config.yaml`
was NOT changed: this is a measurement. Report:
`docs/reports/backtest-expansion-2026-09-09.html`; the analysis is
`tools/report/expand.py`, and replaying new pairs needs a depth snapshot and
an instrument snapshot first — a separate 5-minute scanner with
`strategy.enabled: false` on another port writes both and never touches the
3.5 process.

**The capital-and-risk grid (2026-09-09) found the set, and it is mostly
"stop leaving".** Two axes were added to the code first: a SERIES-SELECTION
entry condition — `strategy.Params.MinTrailingMeanBps` over `TrailingMeanDays`
(config `min_trailing_mean_bps` / `trailing_mean_days`, 0 = off = the old rule,
pinned by test; DAYS not settlements, rule 3, and a history that does not
reach the window start REFUSES rather than averaging less) — and the basis
limits as sweep axes (`-max-basis`, `-max-basis-widen`; four new columns at
the END of both CSVs). 624 sets × 74 series × 12 and 6 months, ranked on the
UNIVERSE's capital (74 slots × 2 notional, an unselected slot earns 0) and on
the PORTFOLIO drawdown — a daily equity curve rebuilt from Go's own trades for
every set (`tools/report/capital.py`). Twelve months: hold-through +0.924% at
0.231% portfolio drawdown; the shipped set ranks **624/624** (−0.056%, 0.551%
drawdown, 259 basis exits); the best set is +0.888% with the basis exit OFF
and M=1, and **0/624 beat hold-through**. `max_basis_widen_pct` is the axis
that matters (grid mean 0.121% at 0.5 → 0.304% at off); `max_basis_pct` barely
does. The recommended set keeps a finite guard — **basis 2.0 / widen 2.0,
M=1, selection off, everything else as shipped → +0.872% (rank 5), portfolio
drawdown 0.231%, 62/74 positive, 1.12 trades/series, and rank 14/624 in the
6-month window**; the guard costs 0.016 points against fully off. Series
selection does NOT raise return on the universe's capital (grid mean falls
monotonically 0.595 → 0.139% as the floor rises to 0.9 bps) because unselected
slots idle; it buys drawdown: the frontier is ≤0.05% → +0.269% on 16 series
(0.9 bps/30d, ratio 5.7), ≤0.10% → +0.365% on 31, ≤0.20% → +0.548% on 60, and
from 0.30% up the unselected set wins. The tightest selection (0.9/90d) puts
+2.625% on DEPLOYED capital across 5 series but +0.177% on the universe and
ranks 403rd in the 6-month window — and concentrating capital there is at a
size the book was never priced for (hyperliquid refused 4 alts at 50k). So
selection is the DRAWDOWN-BUDGET lever, meaningful only when idle capital has
another use; the absolute return lives in the basis guard and in not leaving.
The proposal is in `docs/reports/backtest-capital-2026-09-09.html`, and
**the user applied it on 2026-09-09**: `config.yaml` now ships
`max_basis_pct: 2.0`, `max_basis_widen_pct: 2.0`,
`min_hold_recovered_cost_frac: 1.0` (the two selection keys stay at 0), and
`cmd/backtest`'s `baseParams` moved with it — every sweep axis overrides those
fields, so the default grid is unchanged, pinned by test. Where an earlier
paragraph says "the shipped set", it means the block as it stood before this
change (basis 1.0/0.5, M=0). The measurement behind the decision, on the old
set's 342 trades over 74 series × 50k, 12 months: 259 basis exits held a
median 0.9 days and paid $71,954 in round trips for $11,566 of funding; the
new set makes 83 trades and pays $19,709 for the same funding stream. **The
running 3.5 process is unaffected** — it loaded its config once — so its
journal is still the `2328307` block and restarting would reset the 14 days.

**Re-measured on the applied set the same day** (`0773a73`, report
`docs/reports/backtest-applied-2026-09-09.html`, tool `tools/report/applied.py`
+ `applied.template.html`): 12 months, 74 series → **+0.871%/yr on capital per
series**, 62/74 positive, 1.12 trades/series, hold-through +0.924%; 6 months
+0.443% vs +0.469%; 3 months +0.277% vs +0.315%. Three plain runs of the same
universe differing in ONE named thing, diffed as applied − other: the block
it replaced (basis 1.0/0.5, M 0) made −0.065% at 344 trades → **+0.937
points**, 19 series moved, HYPE·kraken alone +31.86; the basis exit fully OFF
(100/100) makes +0.888% at 78 trades → the finite guard costs 0.016; M 0
makes +0.862% at 90 trades → M=1 adds 0.009. A 324-set grid around the
applied set (rate 0.3/0.5/0.8 × persist 3/6 × N 24/48/96 × M 0/1 × basis
1/2/100 × widen 1/2/100): all 324 positive, median +0.789%, the applied set
ranks **93/324**, the best (0.3/3, N 24, M 1, basis off) is +0.917%, 0.046
above it, and **0/324 beat hold-through**. Per axis: persistence 3 over 6 is
worth 0.053 grid-mean points and is the largest axis left; basis 1.0 → 2.0
is +0.075 and 2.0 → off only +0.004; widen 1.0 → 2.0 is +0.094 and 2.0 → off
+0.005; N and M are flat. `config.yaml` was not moved on this grid.

**The corpus was extended to three years on 2026-09-09 (on a scratchpad
COPY of the DB — the live file is what the 3.5 process writes), and the
applied set was re-measured on it** (report
`docs/reports/backtest-3y-2026-09-09.html`; recipe, venue reach and the
`recorded_at_ms` trap in `tools/report/README.md`). Reach with `-months 36`:
binance, bybit and hyperliquid answer the full 3 years (HYPE from listing),
**kraken has nothing before 2025-09-03** (its endpoint takes no time
parameter and returns all it keeps — an absolute anchor that will grow, not
a one-year cap), gate 180 days, okx ~3 months, paradex nothing; 3-year candles exist for binance/bybit both legs,
hyperliquid candles still stop at ~208 days. Only **32 of the 74 series cover
the whole 3 years**, so the 36-month universe mean mixes corpus lengths and
the per-year figures are read on those 32. Applied set over 36 months:
+6.49% on capital per series (74), +13.66% = **+4.55%/yr on the 32**, 66/74
positive, 1.24 trades/series, hold-through +6.49%; 24 months +3.10% vs
+3.16% (+2.91%/yr on the 32); 12 months +0.88% vs +0.92%. Per-year
hold-through read straight from the corpus (`applied.py --slices`): **2023-24
+7.49%** (1.41 bps/8h; USDT venues +5.94%, hyperliquid +12.16%), 2024-25
+4.20% (0.81 bps; +3.28% / +6.95%), 2025-26 +1.23% over 48 series and
**+1.42% on the SAME 32** (the third slice adds 13 kraken + 3 HYPE series
that did not exist earlier) — funding fell ~5× in three years. "The
universe" of 2023-24 is 32 series on 3 venues and 12 pairs (28/32 inside
5–15%; USDT-quoted mean +5.94% at the band's floor, the 8 bridged
hyperliquid series +12.16%); 7/32 in band in 2024-25, 0/32 in 2025-26.
Hold-through is a Python benchmark from the corpus, not a Go replay, and
the pair list is in-sample (screened 2026-09-09 on the last 12 months). Grids on the 3-year corpus: 324 sets → the
applied set ranks 105/324, best +6.561% (0.3/3, N 24, M 0, basis off) is
0.068 above it, 108 beat hold-through; 1,296 sets (adds C 0.25/1.0 and
holding_days 30/90) → rank **156/1,296**, best still +6.561%, 164 beat
hold-through by ≤0.07, the top 20 span 0.02 points, every axis mean within
0.11 (C 0.25 → 1.0 +0.11, widen 1 → 2 +0.09, basis 1 → 2 +0.07, persist 3 vs
6 +0.015). The old block (1.0/0.5, M 0) makes +5.574% at 4.97 trades on the
universe but the SAME +13.67% on the 32 full-coverage series — its whole
deficit is the short-corpus kraken/alt series. The adversarial review of that
sweep corrected five readings: re-ranked on the 32 full-coverage series the
applied set is **365/1,296** (gap 0.085) and 392/1,296 on the USDT-quoted
subset — mid-grid, though still only 0.03–0.04 points/yr behind; the
flatness is ONE-SIDED (no set beats hold-through by more than +0.07, but
805/1,296 trail it by more than 0.1 and the worst by 0.51); the 164
"beat hold-through" sets all owe their margin to ONE episode — BNB·binance
flat for 71 days in Dec 2023–Mar 2024, +2.6% on capital — and without that
series 55 sets beat it by ≤0.017 while the applied set trails by 0.028;
the two basis axes are worth exactly 0.000 on the 32 three-year series (the
basis exit never fired there at any threshold, and the whole +0.919 over
the old block is kraken's one-year cohort, +0.921), the decay exit never
fires (N 24/48/96 bit-identical), and hyperliquid is basis-blind for 79.9%
of its 36-month settlements; and 12 of the 36 months are the very window
the set was chosen on — on the same 32 series the rule tracks hold-through
year by year (7.85/4.43/1.67 vs 7.72/4.42/1.65) because 26/32 positions
opened Sep–Oct 2023 and held to the end, and a perfect-foresight exit would
have added at most +0.16%/yr in 2023-24. The grid also omits every value
already shown to hurt (widen 0.5, C 0, N 1, rate ≥1.2), so "flat" describes
the neighbourhood of a set chosen for being flat. **Verdict: nothing in that
neighbourhood is meaningfully better over 3 years (0.02–0.03 points/yr),
the upside over hold-through is capped at +0.07 while the downside reaches
−0.51, and the yearly regime swing (6.07 points/yr on the same 32 series)
is 31× the grid's whole span and 260× the best-minus-applied gap.** The
lever left is WHEN and WHERE to be in (the trailing-mean selection key,
still 0), to be measured on this corpus. `config.yaml` unchanged.

**Six regularities were then distilled from the 3-year corpus (2026-09-09,
report `docs/reports/regime-3y-2026-09-09.html`; tools `tools/report/regime.py`
and `forward.py`; `cmd/backtest` gained `-from/-to`, a 200-day funding
LOOKBACK before the window so trailing checks are not blind at the start, and
prices pinned windows on the NEWEST book — looking for a book at a past
window's end had refused every series).** On the 32 series covering all
three years, as the adversarial review of 2026-09-09 left them: (1) what is
predictable is the RANKING across series at a date, not a series' own
level — cross-sectional Spearman ≈ 0.55–0.63 at every horizon and in every
year, within-series ≈ 0.45 at 30 → 30 but ≈ 0 at 30 → 90 in 2024-25 and
+0.40 / −0.47 / −0.03 at 90 → 90 by year; the pooled +0.63 mixes the two on
overlapping weekly samples that start 180 days into the corpus; (2) the
threshold that pays a round trip is the series' OWN cost ÷ settlements in
the hold, not a universal number — ≈ 0.5 bps/8h at the 0.45% mean cost
(0.33 at BTC/ETH's 0.30%, 0.8 at NEAR's 0.74%), and it moved from ≈ 0.25 in
2024-25 to ≈ 0.8 in 2025-26; above it the forward 90-day net is positive
91–98% against an 80% base rate — a SELECTION rule, not timing; (3) what
persists across years is the VENUE (hyperliquid 2.0 / 2.0 / 2.3× the USDT
venues, quote-bridged) and BNB at the bottom: year-to-year Spearman +0.75 /
+0.40 on 32 series but only +0.48 / +0.18 inside the 24 USDT series (top-6
overlap 3/6 then 1/6), so coin ranking inside a venue is not something to
size on; (4) the LEVEL does not persist (1.41 → 0.81 → 0.30 bps/8h;
hold-through 7.49 → 4.20 → 1.42% on the same 32; the 5–15% band existed only
in 2023-24 — one bull phase and its decay, no information about recurrence);
(5) timing and exits are bounded: 0/54 regime in/out benchmarks beat
hold-through, and the **oracle ceiling** — a two-state DP with perfect
foresight and half a round trip per switch, verified by brute force — adds
only **+0.83% on capital over THREE years** (+0.74% with weekly decisions),
53% of it on the three BNB series (the one coin where holding loses), +0.43
without BNB, +0.07 on BTC/ETH; (6) the lever is ALLOCATION, and most of it
is the venue: the same capital rebalanced quarterly ∝ trailing 90-day
funding (cap 2× equal, equal weights until the horizon is covered) makes
+15.55% vs +13.56% equal-weight over 3 years (+1.99; null +12.86 ± 0.25), of
which ≈ +1.4 is the static hyperliquid tilt and **+0.46 (≈ 0.15%/yr) is
within-venue selection** (no-hyperliquid +11.15 vs +10.69; null +10.07 ±
0.12) — a Python benchmark; an allocation rule belongs to phases 4–5, and
for a 1–2 position account it collapses to "100% in last quarter's best
hyperliquid series". **Walk-forward on the Go grid** (208 sets × 5 calendar
windows): the best set of EVERY window has selection off, entry 0.3 bps,
persistence 3 or 6, C 1.0; the applied set ranks 5–11/208 everywhere; a set
chosen on any train window lands within 0.05 of the test window's best and
of hold-through — but the 16 selection-off sets span only 0.13–0.23 points
per window and only C moves them (~0.1), so the walk-forward validates "do
not leave", not the applied set over its siblings; selection ON lowers the
universe mean on every window by the idle-capital denominator, not by the
signal. So the applied block stays; the stronger thing is not a threshold
but two rules outside `strategy`: enter only series whose trailing 30–90d
funding clears that series' own cost-crossing, and move capital between
series by trailing funding — knowing the second is mostly "hold more
hyperliquid".

**Rule 1 was implemented and measured on 2026-09-10 (report
`docs/reports/costcross-2026-09-10.html`, tool `tools/report/costcross.py`),
and it ships OFF.** `strategy.Params.TrailingMeanMinCostFrac` (config
`trailing_mean_min_cost_frac`, sweep `-trail-cost`, a column in both CSVs
and a key in the journal's `params_json`): the trailing mean over
`trailing_mean_days`, held for `holding_days` at the venue's cadence, must
pay that fraction of the series' OWN priced round trip. The crossing is read
off `NetAPR` with a unit rate and converted per 8h by
`exchanges.DeriveFundingRates`, so 1.0 is exactly "NetAPR of the trailing
mean ≥ 0" and the per-8h crossing does not depend on the cadence (pinned
8h/1h); 0 reproduces the applied set bit for bit (HEAD vs new binary, 85
series, 83 trades). Measured on the rebuilt 3-year copy, five pinned windows,
k 0.5–2.0 × D 30/60/90 beside the absolute floor, ranked on UNIVERSE capital:
on 2024-25 (32 series) no cost set excludes a single series — every series
clears its own crossing at some point in the year, so the rule only enters
LATER — and the best is +0.003 over the applied set; on 2025-26 (48 series)
1.5 × 90d makes +1.241 vs +1.204 (+0.037) by excluding exactly 4 series, all
losers (+0.13 on the universe), minus −0.10 per kept series of late entries;
1.0 × 90d is −0.022; windows starting at the corpus start are blind for D
days and read −1.6 at 90d, all of it late entry. Walk-forward picks the OFF
set on two of three pairs and the one it picks (0.5 × 30d) ranks 13/19 on
its test window. Structural reason, not a bug: the pair list was screened on
2026-09-09 at ≥ 0.40 bps/8h over 12 months, itself a static cost-crossing
screen, so what is left for the rule is entry TIMING, which regularities 1
and 5 already bounded. Turn it on only for an unscreened list or a year with
systematic losers (2025-26's kraken alts: 1.5 × 90d kept out 4 losers and 0
winners). Rule 2 (moving capital between series) is recorded under PLAN step
5.4 with its three prerequisites — a portfolio replay mode in
`internal/backtest`, a capital/position object in Go, and the operator's
decision on how much may sit on one quote-bridged venue.

**Step 3.4 (alerts) is deferred by the user's decision, and step 3.5 is
RUNNING AGAIN — run 2, launched 2026-09-11 14:15:50 +07 from a clean tree
at `59d3707`** (port **8085**, PID in `.paper/scanner.pid`, log
`.paper/scanner.log`, launch record `.paper/launch-state.txt`, verdict no
earlier than 2026-09-25 14:15:50 +07; `caffeinate -i -s` holds the machine
awake and the machine must stay on AC power with the lid open). Run 1 ran
from 2026-09-07 09:39:52 +07 until the machine rebooted at about 2026-09-10
01:13 +07 (its record is archived in `.paper/run1-2026-09-07/`; 4,004 rows,
2 days 15.5 hours of the 14 needed, and the phase-1 soak on 8082 died with
it). PLAN 3.5 ③ step 1 says what the relaunch means — the window restarts
from the launch, 14 unbroken days, no stitching — and run 2 runs the
verified fees and every key added since `2328307`, so its journal compares
against `cmd/backtest` on the CURRENT block with `-compare-journal -from
"2026-09-11 14:15:50 +0700"`; the fee and key state are in the launch log,
in `.paper/launch-state.txt`, and in every journal row. Since 2026-09-10 every journal row also carries the fee
state of both legs it was priced with (`params_json.fees`, PLAN 3.5 ④), and
the launch log prints every source's taker bps and verified flag — the
first run's rows lack the key and mean "the fees of `2328307`". The first
run had a worse hole than its death: the machine slept on lid-close many
times a day (`pmset -g log`), so its journal holds ~55 rows a day per
market instead of 144 and 16 of 32 settlements have no row within two
hours — a relaunch needs AC power, an open lid and `caffeinate -i -s`
(the relaunch script does the last). The
"alert-only mode" is a **journal-only mode**: `cmd/scanner`'s `startSignals` evaluates
every hedge leg every 10 minutes with the SAME `strategy.Candidate` the
backtest builds — settled history from the store, fees from config (loaded ONCE at start-up: the 3.5 process still holds the pre-verification fees of `2328307`, see PLAN 3.5 ③), the newest
measured books — plus live spot/perp prices, which is why the basis exit is
evaluable here and nowhere else. Every decision lands in `signal_journal`
(schema v4) with all its checks as JSON; paper positions reseed from it on
restart. The `strategy:` block in `config.yaml` is the one parameter set both
the live path and `cmd/backtest` run, and the comparison protocol for the gate
is written in PLAN 3.5 — read it before delivering the verdict. Once it is
running again, **do not restart it casually**: the gate needs 14 UNBROKEN
days.

**Two 3.5 debts paid 2026-09-12, for run 3 only (run 2's binary predates
them):** `cmd/scanner`'s `tickLoop` now measures each tick's lateness on the
WALL clock from arming to RECEIPT — not on `time.Time.Sub` (monotonic, and
Darwin's monotonic clock stops during sleep) and not on the value a timer
channel delivers (backdated to the schedule since Go 1.23) — logs `tick trễ
…` past one minute, hands `fn` the receipt instant, and counts on the wire
in `prices.tick_status` beside `source_status` (WS-CONTRACT §4.3; one count
per job, the funding top-up's own ticker uncounted). That `time.Now()` is a
scheduling stamp, not a fourth `RecvAt` site. And the live path's settled
lookback is `max(30, trailing_mean_days + 7)` days with the config capped at
199 so the replay's 200-day lookback always covers what the live path
judges. Run 1 lost 16 of 32 settlements to a sleeping machine with no line
saying so; run 3 will say so.

**Step 4.3 (paper ledger) shipped 2026-09-11, beside the running gate (Q12).**
`internal/paper` is the ledger arithmetic — fills through
`strategy.EstimateFill` on a book sampled AT OR BEFORE the decision, fees
from the journal row's own `params_json.fees`, funding credited only at a
settlement strictly after the open, the pair marked to mid, both legs or
neither — and `cmd/paperledger` is a SEPARATE process that opens the store
READ-ONLY (`store.OpenReadOnly`, SQLite `mode=ro`), rebuilds the account from
scratch every 5 minutes and serves it on loopback on its own port (default
8086; it refuses 8082 and 8085) with **PAPER** beside every figure and no
go-live control. Exit legs are sized at the coins' CURRENT value (qty × mid),
never at the entry notional — the adversarial review caught that as blocking. It decides nothing. Measured on a `.backup` copy of the run-2
database: 18/18 journal enters filled on the sweep before the tick, the
paper entry cost equals half the journal's round trip on every position, and
the one refusal is an exit the hyperliquid book could not absorb — the
position stays open and flagged rather than filled at an imagined price.
The scanner's wire got no new message (the operator's instruction); the
broker interface belongs to 4.2, behind the gate. A finding from that
reconstruction, recorded in PLAN 4.3: **bybit_spot has produced zero price
samples in run 2** (connected at launch, REST depth fine), so the live basis
exit on HYPE·kraken is "not evaluable" every tick. **Diagnosed 2026-09-12** —
see the next paragraph — which means HYPE·kraken's run-2 journal rows say
nothing about the basis rule and must not be read as if they did.

**The step-1.6 silent-subscription debt reproduced, was diagnosed and is
fixed (2026-09-12, for the next run's binary only).** `bybit_spot` delivered
not one frame in 19h45 because Bybit's public SPOT socket accepts at most 10
`args` per subscribe request and the 13-pair list sends 26 — the venue refuses
the whole request, says so, and goes quiet; nothing read the answer, and the
keepalive pongs kept every health signal green. The connector now batches the
spot subscribe under the documented ceiling and logs a refusal, and the shared
lifecycle gained a second clock: `StreamConfig.Handle` reports whether a frame
became a message, a session that answers keepalives while producing no data
ends with `exchanges.ErrDataSilence` and re-subscribes on the next dial, and
the per-source threshold lives in `config.yaml` beside `stale_after_sec`
(`data_silence_sec`, shipped 600s, absent = off). Measured with the production
connectors on the shipped config, same tool before and after: bybit_spot went
from **0 messages in 20 minutes** to **9,592 in 6**. The running gate was not
touched.

**Step 6.1 (crowding core) shipped 2026-09-12.** `internal/crowding` ports
the research package's whole nine-definition path (not four functions) with
the pandas semantics written in its doc.go first, and its parity test
gunzips the package's golden fixture, checks its SHA-256 against the
constant PLAN names, then compares 21,914 asset-bars: max |target error|
1.55e-14 (threshold 1e-10), max |score error| 2.51e-13 (stated tolerance
1e-10), the signal exactly equal everywhere, and the manifest's seven
prefix-causality cut-offs reproduced with error 0. It proves Go = Python on
the already-aggregated 4h path and nothing about the aggregation layer or
an edge. One semantic the fixture could not see and the review caught: a
window of identical ratios must give z = NaN (pandas' variance is exactly
0 there) so a stuck feed goes flat rather than being held on a rounding
error. Steps 6.2 onward wait for the 3.5 verdict and 3.4.

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
| **Bybit** | The public **SPOT** WebSocket takes at most **10 `args` per subscribe request** ("Spot can input up to 10 args for each subscription request sent to one connection"; "No args limit for Futures and Spread for now"). Over the limit it REFUSES THE WHOLE REQUEST — `{"success":false,"ret_msg":"args size >10"}` — and then delivers nothing, rather than truncating. Measured live 2026-09-12: 26 args → 0 data frames in 12s, 8 args → 99. This is what silenced `bybit_spot` for 19 hours of step-3.5 run 2 when the pair list grew from 4 to 13. Batch the topics, and READ the reply: the venue says exactly what is wrong. |
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
cmd/backtest/        step-3.3 replay + parameter sweep: reads the corpus,
                     calls internal/strategy's production rules (never its own
                     copy — two AST tests enforce it), writes CSVs for
                     tools/report/. -compare-journal is the 3.5 gate by machine
cmd/pairscreen/      candidate-pair screen (2026-09-09): live registry, hedge
                     mapping, 9 order books and the round trip strategy prices
                     at 50k, one JSON row per pair × perp; the corpus half and
                     the six-criterion verdict are tools/report/pairscreen.py
cmd/paperledger/     step-4.3 paper ledger: a separate process that reads the
                     journal and the store READ-ONLY, prices every journalled
                     decision with internal/paper, serves PAPER-labelled JSON
                     and a UI on its own port; never touches cmd/scanner
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
  config/            config.yaml -> typed config; the ONE mapping from the
                     strategy: block to strategy.Params, shared by cmd/scanner
                     and cmd/backtest so the 3.5 gate compares one parameter set
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
  crowding/          the Crowding Reversal core (phase 6, step 6.1): a Go port
                     of the frozen Python research package, DIRECTIONAL, proven
                     equal to the package's fixture (target ≤ 1e-10, signal
                     exact); it outputs a fraction of equity before any cost
                     and never says "net"; backtest/ingestion are 6.2–6.3
  paper/             the paper ledger (step 4.3): fills from EstimateFill on a
                     book at or before the decision, funding at settlements
                     only, mark to mid, equity — it PRICES decisions, never
                     makes one; rule 7's one exception, bounded in
                     execution/doc.go
  notify/            Telegram and Discord alerts — step 3.4, DEFERRED behind 3.5
  broker/            ⚠️ THE ONLY PACKAGE HOLDING CREDENTIALS
  execution/         delta-neutral position open and close
  risk/              margin, kill switch, capital limits — since 2026-09-07 it
                     holds the perp liquidation model strategy calls
static/              vanilla JS dashboard
docs/                PLAN.md, DATA-REQUIREMENTS.md, CONVENTIONS.md
  reports/           built HTML reports, one per measurement run — a new file
                     each time, never an overwrite: an older one is the record
                     of what was known that day
  research/          READ-ONLY reference material received from outside — the
                     crowding-reversal Python package (phase 6 source to port,
                     its parity fixture, and the evidence Q11 cites). Never
                     imported or run by any process (rule 8)
tools/report/        READ-ONLY Python that turns cmd/backtest's CSVs into those
                     reports (rule 8's "Python reads SQLite to plot"): stdlib
                     only, mode=ro, and it never recomputes a rule — every
                     profit figure on a page traces to a column Go wrote
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

**Three packages under `internal/` are still empty** — `broker`, `execution`
and `notify` contain only `doc.go` stating their responsibility and boundaries.
`backtest` has been real code since step 3.3 (2026-09-04) and `risk` since the
liquidation model of 2026-09-07; `crowding` (6.1) and `paper` (4.3) arrived on
2026-09-11/12. Read the relevant `doc.go` before adding code to any of them,
empty or not — the boundaries written there are the contract, not a suggestion,
and `paper/doc.go` plus `execution/doc.go` are where rule 7's one exception is
bounded.

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
# config.yaml. PORT picks the port. Run 2 holds 8085; 8082 is free since the
# phase-1 soak died on 2026-09-10, but nothing may be started on either while
# the gate runs.
PORT=8085 go run ./cmd/scanner
sqlite3 data/scanner.db "SELECT datetime(evaluated_at_ms/1000,'unixepoch'), symbol, perp_source, action FROM signal_journal ORDER BY 1 DESC LIMIT 28"

# The 3.5 verdict by machine (PLAN 3.5 ③ steps 2, 4, 5): the journal in the
# run's window against a replay of config.yaml's block, decision by decision.
# Exit 0 passed, 1 failed, 2 the journal was not written with this block.
go run ./cmd/backtest -compare-journal -from 2026-09-10T07:40:00 -to 2026-09-24T07:40:00 -csv /tmp/compare.csv


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
- 650 test functions (`grep -r '^func Test' --include='*_test.go'`, re-measured
  2026-09-12 with `go test -cover ./...`; most are table-driven so the case
  count is far higher; earlier docs quoted 211 and then 483 under counting
  methods that did not survive — the command is stated so the number can always
  be re-measured): `internal/scanner` 118 (89.1% of statements),
  `internal/strategy` 94 (95.9%), `internal/backtest` 50 (94.3%),
  `internal/config` 47 (83.0%), `internal/instruments` 36 (96.1%),
  `cmd/scanner` 32 (40.3%), `internal/store` 31 (80.6%), `exchanges` 29
  (67.6%), `cmd/backtest` 28 (43.3%), `internal/crowding` 13 (94.3%),
  `internal/depth` 12 (95.4%), `internal/paper` 10 (90.7%),
  `cmd/paperledger` 9 (54.0%), `internal/risk` 6 (100%),
  `internal/history` 6 (51.4%), `internal/fees` 5 (100%), plus 88 across the
  eight per-venue packages (47.1–63.5%). The root `exchanges` figure ROSE from
  58.8% as step 3.3b's shared candle helpers gained tests; the per-venue
  packages sit lower because step 2.6's history fetchers and step 3.3b's candle
  pagers have loops that only run against live venues. Their parsers and the
  cadence arithmetic are golden-tested against recorded payloads; the loops are
  not, and pretending otherwise with a mock HTTP server would test the mock.
  Each `exchanges/<venue>/testdata/` holds that venue's real recordings;
  re-record with `CAPTURE_TESTDATA=1 go test -run TestCapture ./exchanges/...`. **Pyth has
  no recording** - hermes.pyth.network answers 401 - so its fixture is synthetic
  and labelled as such; it proves the arithmetic, not that Pyth's current format
  still matches what the connector decodes.
- ~~A subscription the venue silently drops is never re-established.~~ **Fixed
  2026-09-12, after it happened**: `bybit_spot` delivered NOT ONE frame in 19h45
  of step-3.5 run 2 (`last_msg_at_ms: 0` on the live wire, 0 price samples
  against 7,904 per other source) because a 26-arg subscribe exceeded Bybit
  spot's documented 10-arg ceiling and was refused whole — see the trap table.
  The connector never read the refusal and the pongs kept the read deadline
  fresh, which is exactly the shape this entry described, reached by a different
  road: the venue did not go quiet, it ANSWERED and nobody listened. Both halves
  are now closed. `StreamConfig.Handle` returns whether the frame produced a
  message on a feed, so `runSession` runs two clocks — one for silence of any
  kind, one for silence of DATA — and a session that keeps answering while
  producing nothing ends with `exchanges.ErrDataSilence`, backs off and
  re-subscribes; `shouldResetBackoff` counts data frames, so a permanently
  refused subscription escalates to the 60s ceiling instead of cycling at the
  floor. The threshold is per source in `config.yaml`
  (`default_data_silence_sec`, shipped 600s, floored at 60s by
  `config.MinDataSilenceSec` because below the socket's own 60s read deadline
  the data clock would always fire first on a merely quiet feed). The number is
  chosen on ASYMMETRIC COST — a wasted reconnect against a dead-feed window
  that just cost 19 hours — not on a ratio to the worst measured gap, which is
  not stable: bybit_futures measured 0.69s in one window and 18.38s in another
  minutes later. Absent means OFF, so the config the 3.5 process loaded still
  means what it meant, and the oracle is skipped because Pyth is SSE with its
  own read loop. The fix is in the NEXT run's binary — run 2 was not
  restarted.
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
  **Measured on the same 16 series over 12 months, and READ WITH THE RIGHT
  DENOMINATOR** — the first reading of this table got it wrong and the user
  caught it. Per NOTIONAL: no leverage +9.49% / 0 liquidations, 2× +9.16% / 2,
  3× +8.13% / 2, 5× +6.83% / 4, 10× −2.83% / **18**, 20× −19.35% / 21. That
  looks monotone, but **notional does not change when leverage is switched on**,
  so those figures measure only what leverage COSTS (the round trips its
  liquidations force) and structurally cannot measure what it buys. The benefit
  is in the CAPITAL, and **the spot leg cannot be levered** — hedging N still
  costs N — so capital is N·(1+f) and **the whole ceiling is 2.00× as f→0, not
  10× at 10×**. Per capital, after also charging the venue liquidation fee
  (≈ the maintenance margin left, 0.30–0.50% of notional): off +4.74%, 2×
  **+5.57%**, 3× +5.51%, 5× +4.44%, 10× −8.60%, 20× −25.28%. So the entire
  prize is **+0.83pp at 2× (1.17× against a 1.17-to-1.33× ceiling)** and it is
  negative by 5×, bought with 0 → 2 → 4 → 18 → 21 liquidations.
  The lost margin is deliberately NOT charged, and the earlier claim that
  omitting it "FLATTERS leverage by 180% of a notional" was **wrong**: a short
  is only liquidated when the price RISES, so at that instant the spot leg holds
  an unrealized gain of N(f−m)/(1+m) against a margin of f·N — the combined
  position is still flat, and deducting the margin without crediting the spot
  side counts one move twice. What is really lost is the venue's liquidation fee
  and **the hedge itself**: naked long spot until it can be sold, which nothing
  here prices. Ships at 0 — the reason is the unpriced window, not the table.
  Real capital efficiency comes from putting BOTH legs on ONE venue under
  unified/portfolio margin (the spot gain offsets the perp loss inside one
  account, reaching near the 2× ceiling with no added liquidation risk); that is
  a phase-4 EXECUTION decision, and only `binance_futures ← binance_spot` is
  same-venue today. `backtest.Result` now carries `CapitalPerNotional`,
  `TotalReturnOnCapitalFrac` and `RealizedAPROnCapitalFrac`, reported BESIDE the
  notional figures and never instead of them.
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
- **`depth_snapshots` is thin and starts where the scanner did**, not where the
  corpus does. Re-measured 2026-09-12 on the live file: **1,579 rows over 116
  source|pair series in 31 sweeps** — the step-2.7b acceptance sweep plus what
  run 2 has written since 2026-09-11 14:15 — against 12 months of funding and
  candles. So there is still **no historical depth**, a backtest cannot model
  slippage from the book as it was, and it has to take slippage as a stated
  parameter and say so. Every hour the scanner is not running is an hour of
  depth nobody can recover. One more limitation the paper ledger met (PLAN 4.3):
  `sampled_at_ms` stamps the START of a whole sweep (~117 fetches, 30–90s), so
  "the book at or before a decision" resolves to a sweep, not to a fetch; the
  `fetched_at_ms` column that would fix it is schema v6, deliberately deferred
  until nothing is writing the file.

---

## Document map

| Document | Contents |
|---|---|
| [docs/PLAN.md](docs/PLAN.md) | 9 phases, 42 steps, acceptance criteria, risk register, open decisions |
| [docs/WORKFLOW.md](docs/WORKFLOW.md) | The 9-phase loop every change follows, review checklist, commit rules |
| [docs/DATA-REQUIREMENTS.md](docs/DATA-REQUIREMENTS.md) | What data is needed from a venue, 7-venue funding survey, `FundingData` design, data traps |
| [docs/CONVENTIONS.md](docs/CONVENTIONS.md) | Naming, structure, errors, concurrency, testing, dependency rules |
| [docs/WS-CONTRACT.md](docs/WS-CONTRACT.md) | The frozen backend↔dashboard JSON contract, and which field carries real data at which step |
| [README.md](README.md) | Project introduction, current capabilities and gaps, setup, risk notice |
