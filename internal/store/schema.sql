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
    -- interval_sec is the series' modal spacing (the cadence) while this is
    -- what happened at this row. They differ exactly where a settlement was
    -- missed or the cadence changed.
    gap_prev_sec           INTEGER NOT NULL,
    rate_per_8h_frac       REAL NOT NULL,
    apr_frac               REAL NOT NULL,

    raw_rate        REAL NOT NULL,
    raw_rate_field  TEXT NOT NULL,  -- the payload field raw_rate came from
    -- Binance only: 'Regular' | 'Special'. A Special rate is dividend-driven
    -- and must be filtered out of a backtest. Empty everywhere else.
    rate_type       TEXT NOT NULL DEFAULT '',
    mark_price      REAL NOT NULL DEFAULT 0,

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
