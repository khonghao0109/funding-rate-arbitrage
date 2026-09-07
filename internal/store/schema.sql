-- Schema for the scanner's SQLite store (PLAN.md step 2.6).
--
-- Column names carry their unit, exactly like the Go identifiers do
-- (docs/CONVENTIONS.md §1). These names are not internal: phase 8 reads this
-- file from Python, and `rate_per_8h` next to `interval_sec` is how a reader
-- ends up dividing a figure that was already divided.
--
-- Every table is WITHOUT ROWID: each one has a natural composite key that IS
-- the access path, so the extra rowid index would be a second copy of the data
-- earning nothing.

-- funding_history is one funding rate as a venue reported it IN HINDSIGHT.
--
-- The primary key is what makes the whole design restartable: a backfill and an
-- hourly top-up can re-fetch overlapping windows freely, and INSERT OR IGNORE
-- turns the overlap into a no-op instead of a duplicate settlement.
CREATE TABLE IF NOT EXISTS funding_history (
    source          TEXT    NOT NULL,  -- wire id: binance_futures, ...
    symbol          TEXT    NOT NULL,  -- normalized: BTCUSDT
    -- The settlement this rate was paid at, as the VENUE stamps it: Gate's land
    -- seconds past the hour and Hyperliquid's carry millisecond jitter, and
    -- both are kept verbatim. Rounding them to a boundary would invent a
    -- timestamp and break the key on the next re-fetch.
    funding_at_ms   INTEGER NOT NULL,

    -- 'discrete' | 'continuous'. Paradex accrues continuously and has no
    -- settlements at all, so its rows are hourly SAMPLES of a funding index and
    -- funding_at_ms is the sample instant. Counting them as payments would
    -- credit 8,760 settlements a year on a venue that makes none — anything
    -- that counts settlements must filter on this column first.
    model           TEXT    NOT NULL,

    rate_per_interval_frac REAL NOT NULL,
    interval_sec           INTEGER NOT NULL,
    -- The measured distance to the previous settlement, which is NOT
    -- interval_sec: no venue publishes an interval beside a historical rate, so
    -- for a DISCRETE series interval_sec is the modal spacing (the cadence)
    -- while this is what happened at this row. They differ exactly where a
    -- settlement was missed or the cadence changed. For a CONTINUOUS series
    -- (model='continuous', Paradex) interval_sec is instead the rate's QUOTE
    -- window — 28800 for its per-8h rate — while the rows are hourly SAMPLES,
    -- so gap_prev_sec (~3600) and interval_sec legitimately disagree there.
    gap_prev_sec           INTEGER NOT NULL,
    rate_per_8h_frac       REAL NOT NULL,
    apr_frac               REAL NOT NULL,

    -- raw_rate is the venue's number VERBATIM, in whatever unit the named
    -- field carries — it is the debug trail back to the payload, never an
    -- input to arithmetic. Today it equals rate_per_interval_frac on every
    -- venue, but that is an observation, not a contract: compute from the
    -- *_frac columns only.
    raw_rate        REAL NOT NULL,
    raw_rate_field  TEXT NOT NULL,  -- the payload field raw_rate came from
    -- Binance only: 'Regular' | 'Special'. A Special rate is dividend-driven
    -- and must be filtered out of a backtest. Empty everywhere else.
    rate_type       TEXT NOT NULL DEFAULT '',
    -- The price funding was charged on, in the market's QUOTE asset
    -- (Binance only; 0 = not supplied). Renamed from mark_price at v3.
    mark_price_quote REAL NOT NULL DEFAULT 0,

    -- When this process wrote the row. NOT a receive time: a rate that settled
    -- last March never came off a socket, and RecvAt is stamped in exactly
    -- three places, one per transport (CLAUDE.md rule 13).
    recorded_at_ms  INTEGER NOT NULL,

    PRIMARY KEY (source, symbol, funding_at_ms)
) WITHOUT ROWID;

-- The primary key answers "one venue's history"; this answers "every venue's
-- history for one pair over a window", which is the shape both the 30-day
-- query and the phase-3 backtest read in.
CREATE INDEX IF NOT EXISTS funding_history_by_symbol
    ON funding_history (symbol, funding_at_ms);

-- price_snapshots is a periodic sample of the live top of book.
--
-- Sampled, not streamed: the scanner sees thousands of ticks a second and a
-- funding position is held for days. The sampling period is configuration
-- (storage.price_sample_every_sec) because the row count is linear in it —
-- 4 pairs × 9 sources at 5s is ~622k rows a day.
CREATE TABLE IF NOT EXISTS price_snapshots (
    source          TEXT    NOT NULL,
    symbol          TEXT    NOT NULL,
    sampled_at_ms   INTEGER NOT NULL,  -- when the scanner took the sample

    -- Prices are in the SOURCE's own quote asset, which is why a USD-quoted
    -- perp and a USDT-quoted spot are never compared without saying so.
    mid_price_quote REAL NOT NULL,
    best_bid_quote  REAL NOT NULL,
    best_ask_quote  REAL NOT NULL,
    -- 0 means NOT KNOWN, never "no liquidity": four venues publish their book
    -- in contracts and this pipeline carries coins.
    best_bid_qty_coin REAL NOT NULL,
    best_ask_qty_coin REAL NOT NULL,

    -- When the underlying message came off the socket. Kept beside the sample
    -- instant so staleness stays reconstructible after the fact: a sample whose
    -- recv_at_ms is minutes behind sampled_at_ms recorded a dead feed, and
    -- without this column it would look like a live price.
    recv_at_ms      INTEGER NOT NULL,

    PRIMARY KEY (source, symbol, sampled_at_ms)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS price_snapshots_by_symbol
    ON price_snapshots (symbol, sampled_at_ms);

-- instrument_snapshots is one day's trading rules per market.
--
-- Versioned by day on purpose: when a venue changes a stepSize or a contract
-- size, a backtest must be able to see WHEN, or it silently reinterprets old
-- data under new rules. Kept forever — 36 markets a day is 13k rows a year, and
-- the whole point is hindsight.
CREATE TABLE IF NOT EXISTS instrument_snapshots (
    snapshot_day    TEXT    NOT NULL,  -- 'YYYY-MM-DD', UTC
    source          TEXT    NOT NULL,
    symbol          TEXT    NOT NULL,

    native_symbol   TEXT    NOT NULL,
    market_type     TEXT    NOT NULL,
    -- What the VENUE declares, verbatim, case included. Never sliced out of the
    -- symbol string, and never upper-cased: Hyperliquid's kPEPE means 1000
    -- PEPE and "KPEPE" is an asset nobody lists.
    base_asset      TEXT    NOT NULL,
    quote_asset     TEXT    NOT NULL,
    status          TEXT    NOT NULL,

    -- 0 in any of these means the venue publishes no such rule, never that the
    -- rule is zero. tick_size_quote 0 means the venue defines price granularity
    -- by a RULE instead (Hyperliquid: five significant figures).
    tick_size_quote    REAL NOT NULL,
    step_size_coin     REAL NOT NULL,
    min_qty_coin       REAL NOT NULL,
    max_qty_coin       REAL NOT NULL,
    min_notional_quote REAL NOT NULL,

    is_contract        INTEGER NOT NULL,  -- the venue's order unit is contracts
    contract_size_coin REAL NOT NULL,     -- base coins in one contract
    max_leverage_x     REAL NOT NULL,     -- 0 = not published publicly

    recorded_at_ms  INTEGER NOT NULL,

    PRIMARY KEY (snapshot_day, source, symbol)
) WITHOUT ROWID;

-- depth_snapshots is one periodic order book measurement per market
-- (step 2.7b).
--
-- It exists because depth CANNOT be backfilled. Funding history is published by
-- the venues months after the fact; an order book is gone the instant it
-- changes, so every hour not recorded here is an hour phase 3 can never model
-- slippage for. That is the whole justification for the table — the live
-- dashboard would be happy with memory.
--
-- The window percentages are in the COLUMN NAMES, which is why they are Go
-- constants and not configuration: a YAML edit must not be able to redefine
-- what a stored column means for a reader six months later.
--
-- Every depth figure is in the market's QUOTE asset, not USD. Kraken,
-- Hyperliquid and Paradex quote USD while the rest quote USDT, and summing them
-- as one currency is the mix docs/WS-CONTRACT.md §5.2 refuses elsewhere. Join
-- instrument_snapshots for the quote if a reader needs to convert.
CREATE TABLE IF NOT EXISTS depth_snapshots (
    source          TEXT    NOT NULL,
    symbol          TEXT    NOT NULL,
    sampled_at_ms   INTEGER NOT NULL,  -- OUR clock, one stamp per sweep
    venue_time_ms   INTEGER NOT NULL,  -- the venue's own, 0 when it sends none

    mid_price_quote REAL    NOT NULL,
    best_bid_quote  REAL    NOT NULL,
    best_ask_quote  REAL    NOT NULL,
    -- Already converted from contracts where the venue quoted them; the flag
    -- below says whether a conversion was involved.
    best_bid_qty_coin REAL  NOT NULL,
    best_ask_qty_coin REAL  NOT NULL,
    spread_pct      REAL    NOT NULL,

    bid_depth_within_0_1pct_quote REAL NOT NULL,
    ask_depth_within_0_1pct_quote REAL NOT NULL,
    bid_depth_within_0_5pct_quote REAL NOT NULL,
    ask_depth_within_0_5pct_quote REAL NOT NULL,

    bid_levels      INTEGER NOT NULL,
    ask_levels      INTEGER NOT NULL,
    -- How far from the mid the farthest level returned sits. When a span is
    -- BELOW a window, that window's figure is a lower bound and not a
    -- measurement: Hyperliquid returns 20 levels spanning 0.025% on BTC.
    -- A reader that ignores these two columns will call that venue illiquid.
    bid_span_pct    REAL    NOT NULL,
    ask_span_pct    REAL    NOT NULL,

    is_contract_book INTEGER NOT NULL,  -- the numbers above came from a conversion

    PRIMARY KEY (source, symbol, sampled_at_ms)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS depth_snapshots_by_symbol
    ON depth_snapshots (symbol, sampled_at_ms);

-- signal_journal is step 3.5's paper-trading record: one row per decision the
-- LIVE signal path made, with the full reasoning it made it on.
--
-- It exists for one comparison. PLAN step 3.5 gates the project on the live
-- path and the backtest agreeing over the same window, and that comparison
-- needs both sides' decisions at the same instants with the same inputs
-- visible. A journal of verdicts without reasons cannot say WHY two sides
-- disagreed, which is the only thing the gate is for.
--
-- REPLACE on the key, unlike funding_history: re-evaluating one instant is a
-- corrected reading of a moment, and a restart mid-window must not double
-- count it.
CREATE TABLE IF NOT EXISTS signal_journal (
    evaluated_at_ms INTEGER NOT NULL,  -- the instant passed to EvaluateEntry/Exit
    symbol          TEXT    NOT NULL,
    perp_source     TEXT    NOT NULL,
    spot_source     TEXT    NOT NULL,  -- '' when the perp has no hedge leg

    action          TEXT    NOT NULL,  -- enter | skip | hold | exit
    -- The figure the decision was made on. net_apr_ok 0 means NO number:
    -- net_apr_frac is then 0 and must not be read as a rate of zero.
    net_apr_frac    REAL    NOT NULL,
    net_apr_ok      INTEGER NOT NULL,
    cost_total_pct  REAL    NOT NULL,  -- the priced round trip, 0 when not OK

    -- Every Check as JSON [{name, passed, detail_vi}], and the Params the
    -- decision ran under. JSON rather than columns because the check list is
    -- the strategy's to change, and a schema that mirrored it would need a
    -- migration each time a condition is added.
    checks_json     TEXT    NOT NULL,
    params_json     TEXT    NOT NULL,

    recorded_at_ms  INTEGER NOT NULL,  -- when this process wrote the row

    PRIMARY KEY (evaluated_at_ms, symbol, perp_source)
) WITHOUT ROWID;
