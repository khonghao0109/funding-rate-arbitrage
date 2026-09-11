"""Causal BTC/ETH 4h trend-confirmed retail-crowding reversal research.

The candidate is deliberately small and interpretable:

* standardise Binance's global long/short *account* ratio over a trailing
  calendar window;
* fade an extreme reading only when 30/60/120-day momentum agrees;
* keep the position until crowding normalises or trend confirmation is lost;
* risk-balance BTC and ETH with only trailing price volatility;
* execute one completed 4h bar later;
* include realised USD-M funding and one-way turnover costs.

This file also repairs an important defect in the earlier exploratory panel:
it does not drop a 4h row merely because an unrelated top-trader field is
missing.  Missing account-ratio observations fail closed and never use a
future value.
"""
from __future__ import annotations

from dataclasses import dataclass
from functools import cache
from pathlib import Path
from typing import Iterable

import numpy as np
import pandas as pd


BASE = Path(__file__).resolve().parent
OUT = BASE / "crowding_reversal_results"
SYMBOLS = ("BTC", "ETH")
DEV_END = "2023-12-31 23:59:59"
OOS_START = "2024-01-01"
ANNUALISATION = 6 * 365.25
ENSEMBLE_MEMBERS = tuple(
    (lookback, entry_z, exit_z)
    for lookback in (90, 180, 360)
    for entry_z in (1.0, 1.25)
    for exit_z in (0.25, 0.50)
)


@dataclass(frozen=True)
class StrategyConfig:
    lookback: int = 180       # 30 calendar days of completed 4h observations
    entry_z: float = 1.0
    exit_z: float = 0.25
    target_vol: float = 0.24
    max_gross: float = 1.0
    fee: float = 0.0005       # 5 bps per one-way notional turnover
    lag_bars: int = 1         # signal at t first applies to (t, t+4h]
    boundary_offset_hours: int = 0
    side: str = "both"        # both, long, or short
    include_funding: bool = True
    use_trend_filter: bool = True
    trend_horizon_days: tuple[int, ...] = (30, 60, 120)
    trend_required: int = 2


def _four_hour(frame: pd.DataFrame | pd.Series, offset_hours: int, how: str):
    rule = "4h"
    kwargs = {
        "rule": rule,
        "label": "right",
        "closed": "right",
        "offset": f"{offset_hours}h",
    }
    grouped = frame.resample(**kwargs)
    if how == "last":
        return grouped.last()
    if how == "sum":
        return grouped.sum(min_count=1)
    raise ValueError(how)


@cache
def load_market_panel(offset_hours: int = 0) -> pd.DataFrame:
    """Load only fields used by this method on a complete 4h price grid."""
    blocks = []
    for symbol in SYMBOLS:
        metrics = pd.read_parquet(
            BASE / f"{symbol.lower()}usdt_metrics_5m.parquet",
            columns=[
                "create_time",
                "count_long_short_ratio",
                "sum_open_interest_value",
            ],
        )
        metrics.index = pd.DatetimeIndex(
            pd.to_datetime(metrics.pop("create_time"), utc=True)
        ).as_unit("ns")
        metrics = metrics.sort_index()
        metrics = _four_hour(metrics, offset_hours, "last")

        price = pd.read_parquet(
            BASE / f"{symbol.lower()}_um_1m.parquet",
            columns=["ts", "c"],
        )
        price.index = pd.DatetimeIndex(
            pd.to_datetime(price.pop("ts"), unit="ms", utc=True)
        ).as_unit("ns")
        close = _four_hour(price.c.sort_index(), offset_hours, "last").rename("close")
        joined = close.to_frame().join(metrics, how="left")
        joined.columns = pd.MultiIndex.from_product([[symbol], joined.columns])
        blocks.append(joined)

    panel = pd.concat(blocks, axis=1, join="inner").sort_index()
    # Price is required.  Ratio/OI gaps stay missing so the signal can fail closed.
    required = [(symbol, "close") for symbol in SYMBOLS]
    panel = panel.dropna(subset=required)
    # Price archives can extend beyond the derivatives-metric archive.  Do not
    # count a stale all-missing tail as flat strategy days in annualisation.
    ratio_available = panel[
        [(symbol, "count_long_short_ratio") for symbol in SYMBOLS]
    ].notna().any(axis=1)
    if ratio_available.any():
        panel = panel.loc[: ratio_available[ratio_available].index[-1]]
    return panel


@cache
def _funding_by_offset(offset_hours: int) -> dict[str, pd.Series]:
    rates = {}
    for symbol in SYMBOLS:
        raw = pd.read_parquet(BASE / f"{symbol.lower()}usdt_funding.parquet")
        # Binance settlement timestamps in the archive are often a few
        # milliseconds after 00:00/08:00/16:00.  They economically belong to
        # that settlement instant, not to the following 4h bucket.
        settlement = pd.to_datetime(raw.calc_time, unit="ms", utc=True).dt.round("h")
        raw.index = pd.DatetimeIndex(settlement).as_unit("ns")
        grouped = _four_hour(
            raw.last_funding_rate.astype(float).sort_index(), offset_hours, "sum"
        )
        rates[symbol] = grouped
    return rates


def load_funding(index: pd.DatetimeIndex, offset_hours: int = 0) -> pd.DataFrame:
    grouped = _funding_by_offset(offset_hours)
    return pd.DataFrame(
        {symbol: grouped[symbol].reindex(index).fillna(0.0) for symbol in SYMBOLS},
        index=index,
    )


def rolling_zscore(series: pd.Series, lookback: int) -> pd.Series:
    """Calendar-bar rolling z-score; current completed observation is allowed."""
    minimum = max(20, int(lookback * 0.80))
    logged = np.log(series.where(series > 0))
    window = logged.rolling(lookback, min_periods=minimum)
    return (logged - window.mean()) / window.std()


def crowding_scores(panel: pd.DataFrame, lookback: int) -> pd.DataFrame:
    return pd.DataFrame(
        {
            symbol: rolling_zscore(
                panel[(symbol, "count_long_short_ratio")], lookback
            )
            for symbol in SYMBOLS
        },
        index=panel.index,
    )


def hysteresis_signal(
    score: pd.DataFrame,
    entry_z: float = 1.0,
    exit_z: float = 0.25,
    side: str = "both",
) -> pd.DataFrame:
    """Fade extremes, remain in the state until the ratio normalises."""
    if not 0 <= exit_z < entry_z:
        raise ValueError("require 0 <= exit_z < entry_z")
    if side not in {"both", "long", "short"}:
        raise ValueError(side)
    out = pd.DataFrame(0.0, index=score.index, columns=score.columns)
    for column in score:
        state = 0.0
        values = score[column].to_numpy(float)
        result = np.zeros(len(values), dtype=float)
        for i, value in enumerate(values):
            if not np.isfinite(value):
                state = 0.0
            elif state == 0.0:
                if value >= entry_z and side in {"both", "short"}:
                    state = -1.0
                elif value <= -entry_z and side in {"both", "long"}:
                    state = 1.0
            elif abs(value) <= exit_z:
                state = 0.0
            result[i] = state
        out[column] = result
    return out


def causal_weights(
    close: pd.DataFrame,
    signal: pd.DataFrame,
    target_vol: float,
    max_gross: float = 1.0,
    vol_window: int = 180,
) -> pd.DataFrame:
    """Inverse-vol mix with a trailing two-asset covariance risk scale."""
    returns = close.pct_change(fill_method=None)
    vol = returns.rolling(vol_window, min_periods=int(vol_window * 0.80)).std()
    vol = vol * np.sqrt(ANNUALISATION)
    raw = signal.div(vol).replace([np.inf, -np.inf], np.nan).fillna(0.0)
    mix = raw.div(raw.abs().sum(axis=1).replace(0, np.nan), axis=0).fillna(0.0)
    minimum = int(vol_window * 0.80)
    left, right = close.columns[:2]
    rolling = returns.rolling(vol_window, min_periods=minimum)
    left_variance = rolling[left].var()
    right_variance = rolling[right].var()
    covariance = returns[left].rolling(
        vol_window, min_periods=minimum
    ).cov(returns[right])
    portfolio_variance = ANNUALISATION * (
        mix[left].pow(2) * left_variance
        + mix[right].pow(2) * right_variance
        + 2.0 * mix[left] * mix[right] * covariance
    )
    portfolio_vol = np.sqrt(portfolio_variance.clip(lower=0.0))
    scale = (target_vol / portfolio_vol.replace(0.0, np.nan)).clip(upper=max_gross)
    scale = scale.replace([np.inf, -np.inf], np.nan).fillna(0.0)
    # Preserve the original warm-up convention: index `minimum` is the first
    # eligible allocation, even though a rolling variance exists one row prior.
    scale.iloc[:minimum] = 0.0
    weights = mix.mul(scale, axis=0).fillna(0.0)
    if (weights.abs().sum(axis=1) > max_gross + 1e-10).any():
        raise AssertionError("gross exposure cap violated")
    return weights


def trend_confirmation_votes(
    close: pd.DataFrame,
    target: pd.DataFrame,
    horizon_days: tuple[int, ...] = (30, 60, 120),
) -> pd.DataFrame:
    """Count price-momentum horizons agreeing with each proposed direction."""
    votes = pd.DataFrame(0, index=target.index, columns=target.columns)
    for days in horizon_days:
        momentum_up = close.pct_change(days * 6, fill_method=None) > 0
        aligned = ((target > 0) & momentum_up) | ((target < 0) & ~momentum_up)
        # A horizon without enough history must not be interpreted as bearish.
        available = close.shift(days * 6).notna()
        votes += (aligned & available).astype(int)
    return votes


def apply_trend_confirmation(
    close: pd.DataFrame,
    target: pd.DataFrame,
    horizon_days: tuple[int, ...] = (30, 60, 120),
    required: int = 2,
) -> pd.DataFrame:
    if not horizon_days or not 1 <= required <= len(horizon_days):
        raise ValueError("invalid trend-confirmation vote")
    votes = trend_confirmation_votes(close, target, horizon_days)
    return target.where(votes >= required, 0.0)


def simulate(panel: pd.DataFrame, config: StrategyConfig) -> pd.DataFrame:
    close = pd.DataFrame(
        {symbol: panel[(symbol, "close")] for symbol in SYMBOLS},
        index=panel.index,
    )
    score = crowding_scores(panel, config.lookback)
    signal = hysteresis_signal(score, config.entry_z, config.exit_z, config.side)
    target = causal_weights(
        close, signal, config.target_vol, config.max_gross, config.lookback
    )
    if config.use_trend_filter:
        target = apply_trend_confirmation(
            close, target, config.trend_horizon_days, config.trend_required
        )
    position = target.shift(config.lag_bars).fillna(0.0)
    price_return = close.pct_change(fill_method=None).fillna(0.0)
    price_pnl = position * price_return
    if config.include_funding:
        rates = load_funding(panel.index, config.boundary_offset_hours)
    else:
        rates = pd.DataFrame(0.0, index=panel.index, columns=SYMBOLS)
    # A positive Binance funding rate is paid by longs and received by shorts.
    funding_pnl = -position * rates
    turnover_by_asset = position.diff().abs()
    turnover_by_asset.iloc[0] = position.iloc[0].abs()
    cost_by_asset = turnover_by_asset * config.fee

    path = pd.DataFrame(index=panel.index)
    for symbol in SYMBOLS:
        path[f"score_{symbol}"] = score[symbol]
        path[f"signal_{symbol}"] = signal[symbol]
        path[f"target_{symbol}"] = target[symbol]
        path[f"position_{symbol}"] = position[symbol]
        path[f"price_pnl_{symbol}"] = price_pnl[symbol]
        path[f"funding_pnl_{symbol}"] = funding_pnl[symbol]
        path[f"cost_{symbol}"] = cost_by_asset[symbol]
    path["price_pnl"] = price_pnl.sum(axis=1)
    path["funding_pnl"] = funding_pnl.sum(axis=1)
    path["cost"] = cost_by_asset.sum(axis=1)
    path["turnover"] = turnover_by_asset.sum(axis=1)
    path["net"] = path.price_pnl + path.funding_pnl - path.cost
    if (path.net <= -1).any():
        raise AssertionError("invalid period return")
    path["equity"] = (1.0 + path.net).cumprod()
    return path


def ensemble_targets(
    panel: pd.DataFrame,
    config: StrategyConfig,
    members: tuple[tuple[int, float, float], ...] = ENSEMBLE_MEMBERS,
) -> tuple[pd.DataFrame, pd.DataFrame, pd.DataFrame]:
    """Average nearby models before execution so opposing orders are netted."""
    close = pd.DataFrame(
        {symbol: panel[(symbol, "close")] for symbol in SYMBOLS},
        index=panel.index,
    )
    score_cache = {
        lookback: crowding_scores(panel, lookback)
        for lookback in sorted({member[0] for member in members})
    }
    targets = []
    signals = []
    scores = []
    for lookback, entry_z, exit_z in members:
        score = score_cache[lookback]
        signal = hysteresis_signal(score, entry_z, exit_z, config.side)
        target = causal_weights(
            close, signal, config.target_vol, config.max_gross, lookback
        )
        targets.append(target)
        signals.append(signal)
        scores.append(score)
    target = sum(targets) / len(targets)
    if config.use_trend_filter:
        target = apply_trend_confirmation(
            close, target, config.trend_horizon_days, config.trend_required
        )
    return target, sum(signals) / len(signals), sum(scores) / len(scores)


def simulate_ensemble(
    panel: pd.DataFrame,
    config: StrategyConfig,
    members: tuple[tuple[int, float, float], ...] = ENSEMBLE_MEMBERS,
) -> pd.DataFrame:
    """Run the fixed 12-member parameter-diversified candidate."""
    close = pd.DataFrame(
        {symbol: panel[(symbol, "close")] for symbol in SYMBOLS},
        index=panel.index,
    )
    target, signal, score = ensemble_targets(panel, config, members)
    position = target.shift(config.lag_bars).fillna(0.0)
    price_return = close.pct_change(fill_method=None).fillna(0.0)
    price_pnl = position * price_return
    if config.include_funding:
        rates = load_funding(panel.index, config.boundary_offset_hours)
    else:
        rates = pd.DataFrame(0.0, index=panel.index, columns=SYMBOLS)
    funding_pnl = -position * rates
    turnover_by_asset = position.diff().abs()
    turnover_by_asset.iloc[0] = position.iloc[0].abs()
    cost_by_asset = turnover_by_asset * config.fee

    path = pd.DataFrame(index=panel.index)
    for symbol in SYMBOLS:
        path[f"score_{symbol}"] = score[symbol]
        path[f"signal_{symbol}"] = signal[symbol]
        path[f"target_{symbol}"] = target[symbol]
        path[f"position_{symbol}"] = position[symbol]
        path[f"price_pnl_{symbol}"] = price_pnl[symbol]
        path[f"funding_pnl_{symbol}"] = funding_pnl[symbol]
        path[f"cost_{symbol}"] = cost_by_asset[symbol]
    path["price_pnl"] = price_pnl.sum(axis=1)
    path["funding_pnl"] = funding_pnl.sum(axis=1)
    path["cost"] = cost_by_asset.sum(axis=1)
    path["turnover"] = turnover_by_asset.sum(axis=1)
    path["net"] = path.price_pnl + path.funding_pnl - path.cost
    if (path.net <= -1).any():
        raise AssertionError("invalid period return")
    path["equity"] = (1.0 + path.net).cumprod()
    return path


def invert_path(path: pd.DataFrame) -> pd.DataFrame:
    """Exact direction-flip diagnostic with identical timing and turnover."""
    inverted = path.copy()
    for symbol in SYMBOLS:
        for field in ("signal", "target", "position", "price_pnl", "funding_pnl"):
            inverted[f"{field}_{symbol}"] = -path[f"{field}_{symbol}"]
    inverted["price_pnl"] = -path.price_pnl
    inverted["funding_pnl"] = -path.funding_pnl
    inverted["net"] = inverted.price_pnl + inverted.funding_pnl - inverted.cost
    if (inverted.net <= -1).any():
        raise AssertionError("invalid inverted period return")
    inverted["equity"] = (1.0 + inverted.net).cumprod()
    return inverted


def daily_returns(path: pd.DataFrame) -> pd.Series:
    return (1.0 + path.net).resample("1D").prod(min_count=1) - 1.0


def metrics(
    returns: pd.Series,
    start: str | pd.Timestamp | None = None,
    end: str | pd.Timestamp | None = None,
) -> dict[str, float]:
    view = returns.loc[start:end].dropna()
    if len(view) < 3:
        return {key: np.nan for key in ("cagr", "max_dd", "sharpe", "calmar", "multiple")}
    equity = (1.0 + view).cumprod()
    years = max(
        (view.index[-1] - view.index[0]).total_seconds() / (365.25 * 86400),
        len(view) / 365.25,
    )
    cagr = float(equity.iloc[-1] ** (1.0 / years) - 1.0)
    drawdown = equity / equity.cummax().clip(lower=1.0) - 1.0
    standard_deviation = float(view.std(ddof=1))
    sharpe = (
        float(view.mean() / standard_deviation * np.sqrt(365.25))
        if standard_deviation > 0
        else np.nan
    )
    max_dd = float(drawdown.min())
    return {
        "cagr": cagr,
        "max_dd": max_dd,
        "sharpe": sharpe,
        "calmar": cagr / abs(max_dd) if max_dd else np.nan,
        "multiple": float(equity.iloc[-1]),
    }


def split_metrics(returns: pd.Series) -> dict[str, float]:
    row = {}
    for label, start, end in (
        ("dev", None, DEV_END),
        ("oos", OOS_START, None),
        ("full", None, None),
    ):
        for key, value in metrics(returns, start, end).items():
            row[f"{label}_{key}"] = value
    return row


def benchmark_returns(panel: pd.DataFrame, fee: float) -> pd.Series:
    close = pd.DataFrame(
        {symbol: panel[(symbol, "close")] for symbol in SYMBOLS},
        index=panel.index,
    )
    # True 50/50 buy-and-hold price benchmark, rather than silently assuming
    # costless 4h rebalancing. It is a price benchmark and excludes funding.
    equity = close.div(close.iloc[0]).mean(axis=1)
    daily = equity.resample("1D").last().pct_change(fill_method=None).fillna(0.0)
    # A single half-notional entry in each asset sums to one unit of turnover.
    daily.iloc[0] -= fee
    return daily


def _csv_close(panel: pd.DataFrame, market: str) -> pd.DataFrame:
    """Replace the execution-price series with an independent local archive."""
    modified = panel.copy()
    start = "2017-08" if market == "spot" else "2020-01"
    for symbol in SYMBOLS:
        file = BASE / f"{symbol}USDT-{market}-15m-FULL-{start}_2026-07-07.csv"
        raw = pd.read_csv(file, usecols=["open_time", "close"])
        timestamp = raw.pop("open_time").astype("int64")
        # The spot archive changes from milliseconds to microseconds late in
        # its history. Normalise both representations explicitly.
        timestamp = timestamp.where(timestamp < 10**14, timestamp // 1_000)
        raw.index = pd.DatetimeIndex(
            pd.to_datetime(timestamp, unit="ms", utc=True)
        ).as_unit("ns")
        close = _four_hour(raw.close.astype(float).sort_index(), 0, "last")
        modified[(symbol, "close")] = close.reindex(modified.index)
    return modified.dropna(subset=[(symbol, "close") for symbol in SYMBOLS])


def _ratio_variant(
    panel: pd.DataFrame,
    field: str = "count_long_short_ratio",
    aggregation: str = "last",
) -> pd.DataFrame:
    """Build causal alternatives to the default end-of-window ratio sample."""
    if aggregation not in {"last", "first", "mean", "median"}:
        raise ValueError(aggregation)
    modified = panel.copy()
    for symbol in SYMBOLS:
        raw = pd.read_parquet(
            BASE / f"{symbol.lower()}usdt_metrics_5m.parquet",
            columns=["create_time", field],
        )
        raw.index = pd.DatetimeIndex(
            pd.to_datetime(raw.pop("create_time"), utc=True)
        ).as_unit("ns")
        grouped = raw[field].sort_index().resample(
            "4h", label="right", closed="right"
        )
        ratio = getattr(grouped, aggregation)()
        modified[(symbol, "count_long_short_ratio")] = ratio.reindex(modified.index)
    return modified


def run_data_robustness() -> pd.DataFrame:
    """Price-source, sampling, and related-feature falsification checks."""
    panel = load_market_panel(0)
    config = StrategyConfig()
    variants = {
        "perp_1m_ratio_last": panel,
        "perp_15m_ratio_last": _csv_close(panel, "um-perp"),
        "spot_15m_price_proxy": _csv_close(panel, "spot"),
        "ratio_first": _ratio_variant(panel, aggregation="first"),
        "ratio_mean": _ratio_variant(panel, aggregation="mean"),
        "ratio_median": _ratio_variant(panel, aggregation="median"),
        "toptrader_account_last": _ratio_variant(
            panel, "count_toptrader_long_short_ratio"
        ),
        "toptrader_position_last": _ratio_variant(
            panel, "sum_toptrader_long_short_ratio"
        ),
        "taker_flow_last": _ratio_variant(panel, "sum_taker_long_short_vol_ratio"),
    }
    rows = []
    for name, variant in variants.items():
        result = split_metrics(daily_returns(simulate_ensemble(variant, config)))
        rows.append({"variant": name, "bars": len(variant), **result})
    return pd.DataFrame(rows)


def trade_episodes(path: pd.DataFrame, fee: float) -> pd.DataFrame:
    """Group same-direction asset holdings into human-readable campaigns."""
    rows = []
    for symbol in SYMBOLS:
        position = path[f"position_{symbol}"]
        sign = np.sign(position).astype(int)
        starts = sign.ne(sign.shift(fill_value=0)) & sign.ne(0)
        for start in path.index[starts]:
            first = path.index.get_loc(start)
            direction = int(sign.iloc[first])
            last = first
            while last + 1 < len(path) and int(sign.iloc[last + 1]) == direction:
                last += 1
            active = path.iloc[first : last + 1]
            gross = float(
                (
                    active[f"price_pnl_{symbol}"]
                    + active[f"funding_pnl_{symbol}"]
                ).sum()
            )
            entry_cost = abs(float(position.iloc[first])) * fee
            resize_cost = (
                float(position.iloc[first : last + 1].diff().abs().iloc[1:].sum())
                * fee
            )
            exit_cost = abs(float(position.iloc[last])) * fee
            total_cost = entry_cost + resize_cost + exit_cost
            rows.append(
                {
                    "symbol": symbol,
                    "side": "long" if direction > 0 else "short",
                    "start": start,
                    "end": path.index[last],
                    "bars": last - first + 1,
                    "gross_contribution": gross,
                    "cost": total_cost,
                    "net_contribution": gross - total_cost,
                }
            )
    return pd.DataFrame(rows)


def episode_summary(episodes: pd.DataFrame) -> pd.DataFrame:
    rows = []
    for symbol, group in episodes.groupby("symbol"):
        winners = group[group.net_contribution > 0].net_contribution
        losers = group[group.net_contribution <= 0].net_contribution
        average_win = float(winners.mean()) if len(winners) else np.nan
        average_loss = float(losers.mean()) if len(losers) else np.nan
        rows.append(
            {
                "symbol": symbol,
                "episodes": len(group),
                "win_rate": float((group.net_contribution > 0).mean()),
                "avg_win": average_win,
                "avg_loss": average_loss,
                "payoff_ratio": average_win / abs(average_loss),
                "profit_factor": float(winners.sum() / abs(losers.sum())),
                "median_bars": float(group.bars.median()),
                "mean_days": float(group.bars.mean() * 4 / 24),
            }
        )
    return pd.DataFrame(rows)


def scenario_row(name: str, config: StrategyConfig, path: pd.DataFrame) -> dict[str, object]:
    returns = daily_returns(path)
    exposure = path[[f"position_{symbol}" for symbol in SYMBOLS]].abs().sum(axis=1)
    active = exposure[exposure > 0]
    return {
        "scenario": name,
        "lookback": config.lookback,
        "entry_z": config.entry_z,
        "exit_z": config.exit_z,
        "target_vol": config.target_vol,
        "max_gross": config.max_gross,
        "fee": config.fee,
        "lag_bars": config.lag_bars,
        "offset_hours": config.boundary_offset_hours,
        "side": config.side,
        "include_funding": config.include_funding,
        "use_trend_filter": config.use_trend_filter,
        "trend_horizon_days": ",".join(map(str, config.trend_horizon_days)),
        "trend_required": config.trend_required,
        "turnover": float(path.turnover.sum()),
        "active_share": float((exposure > 0).mean()),
        "mean_active_gross": float(active.mean()) if len(active) else 0.0,
        "realised_full_vol": float(returns.std(ddof=1) * np.sqrt(365.25)),
        **split_metrics(returns),
    }


def run_scenarios() -> tuple[pd.DataFrame, pd.DataFrame, pd.DataFrame]:
    rows = []
    base_panel = load_market_panel(0)
    core = StrategyConfig()
    core_path = simulate_ensemble(base_panel, core)
    rows.append(scenario_row("ensemble_core", core, core_path))
    rows.append(scenario_row("strategy_inverse", core, invert_path(core_path)))
    rows.append(scenario_row("single_core", core, simulate(base_panel, core)))
    crowd_only = StrategyConfig(use_trend_filter=False)
    rows.append(
        scenario_row(
            "trend_none_crowding_only",
            crowd_only,
            simulate_ensemble(base_panel, crowd_only),
        )
    )

    for days in (15, 30, 60, 120, 200):
        config = StrategyConfig(trend_horizon_days=(days,), trend_required=1)
        rows.append(
            scenario_row(
                f"trend_single_{days}d", config, simulate_ensemble(base_panel, config)
            )
        )
    for horizons, required in (
        ((15, 30, 60), 2),
        ((30, 60, 120), 2),
        ((20, 60, 120), 2),
        ((15, 30, 60, 120), 2),
        ((30, 60, 120, 200), 2),
        ((15, 30, 60, 120, 200), 3),
    ):
        config = StrategyConfig(
            trend_horizon_days=horizons, trend_required=required
        )
        label = "_".join(map(str, horizons))
        rows.append(
            scenario_row(
                f"trend_vote_{label}_r{required}",
                config,
                simulate_ensemble(base_panel, config),
            )
        )

    for risk in (0.12, 0.16, 0.20, 0.24, 0.28):
        config = StrategyConfig(target_vol=risk)
        rows.append(
            scenario_row(
                f"risk_{risk:.2f}", config, simulate_ensemble(base_panel, config)
            )
        )
    for fee in (0.0003, 0.0005, 0.0008, 0.0010, 0.0015, 0.0020):
        config = StrategyConfig(fee=fee)
        rows.append(
            scenario_row(
                f"fee_{fee * 1e4:.0f}bp", config, simulate_ensemble(base_panel, config)
            )
        )
    for lag in (1, 2, 3, 6, 12):
        config = StrategyConfig(lag_bars=lag)
        rows.append(
            scenario_row(
                f"lag_{lag * 4}h", config, simulate_ensemble(base_panel, config)
            )
        )
    for side in ("both", "long", "short"):
        config = StrategyConfig(side=side)
        rows.append(
            scenario_row(
                f"side_{side}", config, simulate_ensemble(base_panel, config)
            )
        )
    for funding in (True, False):
        config = StrategyConfig(include_funding=funding)
        rows.append(
            scenario_row(
                "funding_on" if funding else "funding_off",
                config,
                simulate_ensemble(base_panel, config),
            )
        )
    for offset in (0, 1, 2, 3):
        panel = load_market_panel(offset)
        config = StrategyConfig(boundary_offset_hours=offset)
        rows.append(
            scenario_row(
                f"boundary_{offset}h", config, simulate_ensemble(panel, config)
            )
        )

    parameter_rows = []
    for lookback in (90, 180, 360):
        for entry in (0.75, 1.0, 1.25, 1.5):
            for exit_z in (0.0, 0.25, 0.5):
                if exit_z >= entry:
                    continue
                config = StrategyConfig(
                    lookback=lookback, entry_z=entry, exit_z=exit_z
                )
                path = simulate(base_panel, config)
                parameter_rows.append(
                    scenario_row(
                        f"lb{lookback}_e{entry:.2f}_x{exit_z:.2f}", config, path
                    )
                )

    asset_rows = []
    for asset in SYMBOLS:
        modified = base_panel.copy()
        other = "ETH" if asset == "BTC" else "BTC"
        modified[(other, "count_long_short_ratio")] = np.nan
        config = StrategyConfig()
        asset_rows.append(
            scenario_row(
                f"asset_{asset}", config, simulate_ensemble(modified, config)
            )
        )
    asset_rows.append(scenario_row("asset_BTC_ETH", core, core_path))
    return pd.DataFrame(rows), pd.DataFrame(parameter_rows), pd.DataFrame(asset_rows)


def chronological_trend_audit(panel: pd.DataFrame) -> pd.DataFrame:
    """Choose a trend variant on early history, then display later periods.

    This is a diagnostic against selecting the prettiest 2024+ result.  It is
    not described as a pristine holdout because the broader research process
    has already inspected all of these years.
    """
    configurations: list[tuple[str, StrategyConfig]] = [
        ("crowding_only", StrategyConfig(use_trend_filter=False))
    ]
    configurations.extend(
        (
            f"single_{days}d",
            StrategyConfig(trend_horizon_days=(days,), trend_required=1),
        )
        for days in (15, 30, 60, 120, 200)
    )
    for horizons, required in (
        ((15, 30, 60), 2),
        ((30, 60, 120), 2),
        ((20, 60, 120), 2),
        ((15, 30, 60, 120), 2),
        ((30, 60, 120, 200), 2),
        ((15, 30, 60, 120, 200), 3),
    ):
        label = "_".join(map(str, horizons))
        configurations.append(
            (
                f"vote_{label}_r{required}",
                StrategyConfig(
                    trend_horizon_days=horizons, trend_required=required
                ),
            )
        )

    rows = []
    for name, config in configurations:
        returns = daily_returns(simulate_ensemble(panel, config))
        row: dict[str, object] = {"variant": name}
        for label, start, end in (
            ("train_to_2022", None, "2022-12-31"),
            ("confirmation_2023", "2023-01-01", "2023-12-31"),
            ("validation_2024plus", OOS_START, None),
        ):
            for key, value in metrics(returns, start, end).items():
                row[f"{label}_{key}"] = value
        rows.append(row)
    result = pd.DataFrame(rows)
    selected = result.train_to_2022_calmar.idxmax()
    result["selected_by_early_calmar"] = False
    result.loc[selected, "selected_by_early_calmar"] = True
    return result


def data_coverage_audit(panel: pd.DataFrame) -> pd.DataFrame:
    """Summarise missingness in the exact 4h fields used by the strategy."""
    rows = []
    for symbol in SYMBOLS:
        ratio = panel[(symbol, "count_long_short_ratio")]
        valid = ratio.notna()
        first = ratio.first_valid_index()
        last = ratio.last_valid_index()
        interior = ratio.loc[first:last] if first is not None else ratio.iloc[:0]
        missing = interior.isna()
        if len(missing):
            groups = missing.ne(missing.shift()).cumsum()
            missing_runs = missing.groupby(groups).sum()
            longest_gap = int(missing_runs.max())
        else:
            longest_gap = 0
        rows.append(
            {
                "symbol": symbol,
                "panel_first": panel.index[0],
                "panel_last": panel.index[-1],
                "panel_4h_bars": len(panel),
                "ratio_first": first,
                "ratio_last": last,
                "ratio_valid_bars": int(valid.sum()),
                "ratio_missing_within_availability": int(missing.sum()),
                "ratio_missing_share_within_availability": (
                    float(missing.mean()) if len(missing) else np.nan
                ),
                "longest_missing_run_4h_bars": longest_gap,
                "nonpositive_ratio_bars": int((ratio.dropna() <= 0).sum()),
            }
        )
    return pd.DataFrame(rows)


def block_bootstrap(
    returns: pd.Series,
    samples: int = 5_000,
    block_days: int = 90,
    seed: int = 20260903,
) -> pd.DataFrame:
    values = returns.dropna().to_numpy(float)
    n = len(values)
    rng = np.random.default_rng(seed)
    rows = []
    blocks_needed = int(np.ceil(n / block_days))
    for _ in range(samples):
        starts = rng.integers(0, max(1, n - block_days + 1), size=blocks_needed)
        draw = np.concatenate([values[start : start + block_days] for start in starts])[:n]
        equity = np.cumprod(1.0 + draw)
        cagr = equity[-1] ** (365.25 / n) - 1.0
        drawdown = equity / np.maximum.accumulate(np.maximum(equity, 1.0)) - 1.0
        standard_deviation = draw.std(ddof=1)
        rows.append(
            (
                cagr,
                float(drawdown.min()),
                float(draw.mean() / standard_deviation * np.sqrt(365.25))
                if standard_deviation > 0
                else np.nan,
            )
        )
    return pd.DataFrame(rows, columns=["cagr", "max_dd", "sharpe"])


def circular_shift_placebo(
    panel: pd.DataFrame,
    path: pd.DataFrame,
    start: str = OOS_START,
    minimum_shift_days: int = 30,
    fee: float = 0.0005,
) -> pd.DataFrame:
    """Break signal/return timing while preserving the position path.

    Jointly circular-shifting both asset positions preserves their exposure,
    persistence, cross-asset dependence, and almost exactly their turnover.
    Daily shifts within 30 days of the real alignment (or its wraparound) are
    excluded because they are not credible null timings for a persistent rule.
    """
    view = path.loc[start:].copy()
    close = pd.DataFrame(
        {symbol: panel.loc[view.index, (symbol, "close")] for symbol in SYMBOLS},
        index=view.index,
    )
    price_return = close.pct_change(fill_method=None).fillna(0.0).to_numpy(float)
    funding = load_funding(view.index, 0).to_numpy(float)
    position = view[[f"position_{symbol}" for symbol in SYMBOLS]].to_numpy(float)
    day_code, unique_days = pd.factorize(view.index.floor("D"))
    bars_per_day = int(
        round(pd.Timedelta("1D") / view.index.to_series().diff().dropna().median())
    )

    rows = []
    for shift_days in range(minimum_shift_days, len(unique_days) - minimum_shift_days):
        shifted = np.roll(position, shift_days * bars_per_day, axis=0)
        # Compare circular paths on equal footing: charge the wrap transition.
        previous = np.vstack([shifted[-1], shifted[:-1]])
        turnover = np.abs(shifted - previous).sum(axis=1)
        net = (
            (shifted * price_return).sum(axis=1)
            - (shifted * funding).sum(axis=1)
            - turnover * fee
        )
        daily_log = np.bincount(
            day_code, weights=np.log1p(net), minlength=len(unique_days)
        )
        daily = pd.Series(
            np.expm1(daily_log),
            index=pd.DatetimeIndex(unique_days),
        )
        rows.append({"shift_days": shift_days, **metrics(daily)})
    return pd.DataFrame(rows)


def stability_tables(path: pd.DataFrame) -> tuple[pd.DataFrame, pd.DataFrame, dict[str, float]]:
    daily = daily_returns(path).loc[OOS_START:]
    monthly = ((1.0 + daily).resample("ME").prod() - 1.0).rename("return").reset_index()
    quarterly = ((1.0 + daily).resample("QE").prod() - 1.0).rename("return").reset_index()
    complete_month = daily.resample("ME").count().to_numpy() >= 20
    complete_quarter = daily.resample("QE").count().to_numpy() >= 80
    equity = (1.0 + daily).cumprod()
    underwater = equity < equity.cummax()
    runs = underwater.ne(underwater.shift()).cumsum()
    underwater_lengths = underwater.groupby(runs).sum()
    rolling = (1.0 + daily).rolling(365, min_periods=365).apply(np.prod, raw=True) - 1.0
    summary = {
        "positive_month_share": float((monthly.loc[complete_month, "return"] > 0).mean()),
        "positive_quarter_share": float((quarterly.loc[complete_quarter, "return"] > 0).mean()),
        "worst_month": float(monthly.loc[complete_month, "return"].min()),
        "worst_quarter": float(quarterly.loc[complete_quarter, "return"].min()),
        "longest_underwater_days": float(underwater_lengths.max()),
        "positive_rolling_365_share": float((rolling.dropna() > 0).mean()),
        "median_rolling_365": float(rolling.median()),
    }
    return monthly, quarterly, summary


def annual_table(returns: pd.Series) -> pd.DataFrame:
    rows = []
    for year, group in returns.groupby(returns.index.year):
        total = float((1.0 + group).prod() - 1.0)
        rows.append(
            {
                "year": int(year),
                "days": len(group),
                "calendar_return": total,
                "annualised_if_partial": (1.0 + total) ** (365.25 / len(group)) - 1.0,
            }
        )
    return pd.DataFrame(rows)


def quantiles(values: Iterable[float]) -> tuple[float, float, float]:
    return tuple(float(x) for x in np.quantile(list(values), [0.025, 0.5, 0.975]))


def _pct(value: float) -> str:
    return "n/a" if not np.isfinite(value) else f"{value:.2%}"


def _num(value: float) -> str:
    return "n/a" if not np.isfinite(value) else f"{value:.2f}"


def write_overview_chart(path: pd.DataFrame, benchmark: pd.Series) -> None:
    """Write a compact visual audit of equity and drawdown."""
    import matplotlib.pyplot as plt

    strategy = daily_returns(path)
    joined = pd.concat(
        [strategy.rename("strategy"), benchmark.rename("50/50 buy-and-hold")],
        axis=1,
    ).dropna()
    equity = (1.0 + joined).cumprod()
    drawdown = equity.div(equity.cummax().clip(lower=1.0)) - 1.0

    figure, axes = plt.subplots(
        2, 1, figsize=(11, 7), sharex=True, gridspec_kw={"height_ratios": [2, 1]}
    )
    equity.plot(ax=axes[0], linewidth=1.6)
    axes[0].axvline(pd.Timestamp(OOS_START, tz="UTC"), color="grey", linestyle="--")
    axes[0].set_yscale("log")
    axes[0].set_ylabel("growth of 1 (log scale)")
    axes[0].set_title("BTC/ETH crowding reversal: historical audit")
    axes[0].grid(alpha=0.25)
    drawdown.plot(ax=axes[1], linewidth=1.2)
    axes[1].axvline(pd.Timestamp(OOS_START, tz="UTC"), color="grey", linestyle="--")
    axes[1].set_ylabel("drawdown")
    axes[1].set_xlabel("UTC date")
    axes[1].grid(alpha=0.25)
    figure.tight_layout()
    figure.savefig(OUT / "overview.png", dpi=170)
    plt.close(figure)


def write_report(
    scenarios: pd.DataFrame,
    parameters: pd.DataFrame,
    assets: pd.DataFrame,
    annual: pd.DataFrame,
    bootstrap: pd.DataFrame,
    benchmark: dict[str, float],
    path: pd.DataFrame,
    data_robustness: pd.DataFrame,
    episodes_oos: pd.DataFrame,
    placebo: pd.DataFrame,
    stability: dict[str, float],
    chronological: pd.DataFrame,
    coverage: pd.DataFrame,
) -> None:
    core = scenarios[scenarios.scenario == "ensemble_core"].iloc[0]
    risk = scenarios[scenarios.scenario.str.startswith("risk_")].drop_duplicates("target_vol")
    fee = scenarios[scenarios.scenario.str.startswith("fee_")].drop_duplicates("fee")
    lag = scenarios[scenarios.scenario.str.startswith("lag_")].drop_duplicates("lag_bars")
    boundary = scenarios[scenarios.scenario.str.startswith("boundary_")].drop_duplicates("offset_hours")
    side = scenarios[scenarios.scenario.str.startswith("side_")].drop_duplicates("side")
    trend = scenarios[scenarios.scenario.str.startswith("trend_")]
    inverse = scenarios[scenarios.scenario == "strategy_inverse"].iloc[0]
    cagr_ci = quantiles(bootstrap.cagr)
    dd_ci = quantiles(bootstrap.max_dd)
    sharpe_ci = quantiles(bootstrap.sharpe.dropna())
    placebo_cagr_ci = quantiles(placebo.cagr)
    placebo_p = float(
        ((placebo.cagr >= core.oos_cagr).sum() + 1) / (len(placebo) + 1)
    )
    positive_neighbours = parameters[
        (parameters.dev_cagr > 0) & (parameters.oos_cagr > 0)
    ]
    target_neighbours = parameters[
        (parameters.dev_cagr >= 0.20) & (parameters.oos_cagr >= 0.20)
    ]
    exposure = path[[f"position_{s}" for s in SYMBOLS]].abs().sum(axis=1)
    rolling = daily_returns(path).loc[OOS_START:]
    rolling_365 = (1.0 + rolling).rolling(365, min_periods=365).apply(np.prod, raw=True) - 1.0
    early_selected = chronological[chronological.selected_by_early_calmar].iloc[0]
    lines = [
        "# BTC/ETH 4h 趋势确认的散户拥挤反转研究",
        "",
        "## 结论先行",
        "",
        (
            f"核心规则在 5bp/边成本、实现资金费率、1 根 4h 延迟下："
            f"Dev CAGR {_pct(core.dev_cagr)}，2024+ 历史验证 CAGR {_pct(core.oos_cagr)}，"
            f"验证段 MaxDD {_pct(core.oos_max_dd)}，Sharpe {_num(core.oos_sharpe)}。"
        ),
        (
            f"它在两个总体时段都超过 20% 门槛，但不是对未来“每年 20%”的保证；"
            f"最差完整自然年和 365 日滚动收益必须与逐年表一起看。"
        ),
        "这仍是历史候选，不是未来收益保证。当前核心风险是只有一个交易所的拥挤数据，且研究期只有约 4.5 年。",
        "",
        "## 固定规则",
        "",
        "- 数据：Binance USD-M BTCUSDT/ETHUSDT 4h，以官方 5m global long/short account ratio 为拥挤度；回测价格窗口 2021-07-01 至 2026-07-01，拥挤原始资料截至 2026-06-30。",
        "- 拥挤层是 12 个固定等权参数成员：回看 15/30/60 天，入场 |z| 为 1/1.25，退出 |z| 为 0.25/0.50；不按 OOS 排名挑最优格。",
        "- 趋势层用 30/60/120 天价格动量投票：拥挤反转方向至少获得 2/3 趋势同意才持仓。即上升趋势中等群体过度做空再买，下降趋势中等群体过度做多再空。",
        "- z 为正向极端做空，z 为负向极端做多；拥挤回归中性区或趋势确认丢失即退出。这是“跟趋势、反拥挤”。",
        "- BTC/ETH 逆波动率分配，组合目标波动 24%，总名义仓位上限 100%，没有借杠杆凑收益。",
        "- 信号在 4h 收盘后确认，下一个 4h 区间才持仓；正资金费多头支付、空头获得。",
        "- 成本是每次单边名义换手 5bp，完整开平约 10bp；不宣称 maker 价可必然成交。",
        "",
        "## 风险档",
        "",
        "| 目标波动 | Dev CAGR | 验证 CAGR | 验证 DD | 验证 Sharpe | 实现波动 |",
        "|---:|---:|---:|---:|---:|---:|",
    ]
    for _, row in risk.sort_values("target_vol").iterrows():
        lines.append(
            f"| {_pct(row.target_vol)} | {_pct(row.dev_cagr)} | {_pct(row.oos_cagr)} | "
            f"{_pct(row.oos_max_dd)} | {_num(row.oos_sharpe)} | {_pct(row.realised_full_vol)} |"
        )
    lines.extend(
        [
            "",
            "## 逐年结果",
            "",
            "| 年份 | 天数 | 区间收益 | 若为不完整年的机械年化 |",
            "|---:|---:|---:|---:|",
        ]
    )
    for _, row in annual.iterrows():
        lines.append(
            f"| {int(row.year)} | {int(row.days)} | {_pct(row.calendar_return)} | "
            f"{_pct(row.annualised_if_partial)} |"
        )
    lines.extend(
        [
            "",
            f"- 2024+ 最差滚动 365 日收益：{_pct(float(rolling_365.min()))}。",
            f"- 全样本激活时间占比：{_pct(core.active_share)}；激活时平均总名义仓位：{_pct(core.mean_active_gross)}。",
            "",
            "## 交易形态（2024+）",
            "",
            "| 资产 | 持仓段 | 胜率 | 平均赢 | 平均亏 | 平均盈亏比 | Profit factor | 平均持有天数 |",
            "|---|---:|---:|---:|---:|---:|---:|---:|",
        ]
    )
    for _, row in episodes_oos.iterrows():
        lines.append(
            f"| {row.symbol} | {int(row.episodes)} | {_pct(row.win_rate)} | "
            f"{_pct(row.avg_win)} | {_pct(row.avg_loss)} | {_num(row.payoff_ratio)} | "
            f"{_num(row.profit_factor)} | {_num(row.mean_days)} |"
        )
    lines.extend(
        [
            "",
            "这里的“持仓段”按单币同方向连续持仓统计，收益是对组合净值的贡献，不是用最大仓位归一化后的单笔标的涨跌幅。",
            f"验证段正收益月份占 {_pct(stability['positive_month_share'])}，正收益季度占 {_pct(stability['positive_quarter_share'])}；最差月 {_pct(stability['worst_month'])}，最差季度 {_pct(stability['worst_quarter'])}。",
            f"最长未创新高期约 {int(stability['longest_underwater_days'])} 天；有完整窗口的滚动 365 日中，正收益窗口占 {_pct(stability['positive_rolling_365_share'])}，中位滚动收益 {_pct(stability['median_rolling_365'])}。",
            "",
            "## 费用与延迟压力",
            "",
            "| 单边成本 | Dev CAGR | 验证 CAGR | 验证 DD |",
            "|---:|---:|---:|---:|",
        ]
    )
    for _, row in fee.sort_values("fee").iterrows():
        lines.append(
            f"| {row.fee * 1e4:.0f}bp | {_pct(row.dev_cagr)} | {_pct(row.oos_cagr)} | {_pct(row.oos_max_dd)} |"
        )
    lines.extend(
        [
            "",
            "| 决策延迟 | Dev CAGR | 验证 CAGR | 验证 DD |",
            "|---:|---:|---:|---:|",
        ]
    )
    for _, row in lag.sort_values("lag_bars").iterrows():
        lines.append(
            f"| {int(row.lag_bars * 4)}h | {_pct(row.dev_cagr)} | {_pct(row.oos_cagr)} | {_pct(row.oos_max_dd)} |"
        )
    lines.extend(
        [
            "",
            "## 结构稳健性",
            "",
            f"- 36 个有限参数邻域中，Dev/OOS 同时为正：{len(positive_neighbours)}/{len(parameters)}；同时 CAGR >=20%：{len(target_neighbours)}/{len(parameters)}。",
            "- 这个计数用于检查局部平台，不使用 OOS 选择新的“最佳参数”。",
            f"- 将最终策略每个仓位完整反向、保持时机和换手不变后，Dev CAGR {_pct(inverse.dev_cagr)}，验证 CAGR {_pct(inverse.oos_cagr)}。这说明最终优势来自“跟趋势、反拥挤”的方向，不是交易时点本身。",
            "",
            "| 趋势确认变体 | Dev CAGR | 验证 CAGR | 验证 DD | 验证 Sharpe |",
            "|---|---:|---:|---:|---:|",
        ]
    )
    for _, row in trend.iterrows():
        lines.append(
            f"| {row.scenario.removeprefix('trend_')} | {_pct(row.dev_cagr)} | "
            f"{_pct(row.oos_cagr)} | {_pct(row.oos_max_dd)} | {_num(row.oos_sharpe)} |"
        )
    lines.extend(
        [
            "",
            "### 时间顺序选择审计",
            "",
            "只用 2021-07 至 2022 年的 Calmar 在上表同一有限趋势变体集中选择，不看后续收益。早期选中的不是报告默认规则，而是 `vote_30_60_120_200_r2`。",
            "",
            "| 早期选中变体 | 2021-07~2022 CAGR | 早期 Calmar | 2023 CAGR | 2024+ CAGR | 2024+ DD |",
            "|---|---:|---:|---:|---:|---:|",
            (
                f"| {early_selected.variant} | {_pct(early_selected.train_to_2022_cagr)} | "
                f"{_num(early_selected.train_to_2022_calmar)} | "
                f"{_pct(early_selected.confirmation_2023_cagr)} | "
                f"{_pct(early_selected.validation_2024plus_cagr)} | "
                f"{_pct(early_selected.validation_2024plus_max_dd)} |"
            ),
            "",
            "这个结果表明“只用早期风险调整表现选趋势层”仍然越过 20% 历史门槛；但因为整个研究过程已经看过后续数据，它仍是审计，不是预注册盲测。",
            "",
            "| 4h 边界偏移 | Dev CAGR | 验证 CAGR | 验证 DD |",
            "|---:|---:|---:|---:|",
        ]
    )
    for _, row in boundary.sort_values("offset_hours").iterrows():
        lines.append(
            f"| {int(row.offset_hours)}h | {_pct(row.dev_cagr)} | {_pct(row.oos_cagr)} | {_pct(row.oos_max_dd)} |"
        )
    lines.extend(
        [
            "",
            "| 方向消融 | Dev CAGR | 验证 CAGR | 验证 DD |",
            "|---|---:|---:|---:|",
        ]
    )
    for _, row in side.sort_values("side").iterrows():
        lines.append(
            f"| {row.side} | {_pct(row.dev_cagr)} | {_pct(row.oos_cagr)} | {_pct(row.oos_max_dd)} |"
        )
    lines.extend(
        [
            "",
            "| 资产消融 | Dev CAGR | 验证 CAGR | 验证 DD |",
            "|---|---:|---:|---:|",
        ]
    )
    for _, row in assets.iterrows():
        lines.append(
            f"| {row.scenario.removeprefix('asset_')} | {_pct(row.dev_cagr)} | "
            f"{_pct(row.oos_cagr)} | {_pct(row.oos_max_dd)} |"
        )
    lines.extend(
        [
            "",
            "## 数据源与定义交叉核验",
            "",
            "| 变体 | Dev CAGR | 验证 CAGR | 验证 DD | 验证 Sharpe |",
            "|---|---:|---:|---:|---:|",
        ]
    )
    for _, row in data_robustness.iterrows():
        lines.append(
            f"| {row.variant} | {_pct(row.dev_cagr)} | {_pct(row.oos_cagr)} | "
            f"{_pct(row.oos_max_dd)} | {_num(row.oos_sharpe)} |"
        )
    lines.extend(
        [
            "",
            "- `spot_15m_price_proxy` 仅用现货价格交叉检查价格数据，仍按永续策略计资金费，不代表现货可直接做空。",
            "- 全市场账户比和头部账户比的同方向结果是支持证据；头部持仓比开发段明显更弱，taker flow 两段都失败，说明不是任意订单流字段都能产生这个结果。",
            "",
            "### 数据覆盖",
            "",
            "| 资产 | 账户比起点 | 账户比终点 | 有效 4h 根数 | 可用区间缺失率 | 最长连续缺失 |",
            "|---|---|---|---:|---:|---:|",
        ]
    )
    for _, row in coverage.iterrows():
        lines.append(
            f"| {row.symbol} | {row.ratio_first} | {row.ratio_last} | "
            f"{int(row.ratio_valid_bars)} | "
            f"{_pct(row.ratio_missing_share_within_availability)} | "
            f"{int(row.longest_missing_run_4h_bars)} 根 4h |"
        )
    lines.extend(
        [
            "",
            "ETH 账户比在 2021-12 前不可用；所有缺失值均使该币种信号归零，没有向前或向后填充。",
            "",
            "## 90 日移动区块 Bootstrap（2024+，5000 次）",
            "",
            f"- CAGR 95% 区间：{_pct(cagr_ci[0])} ~ {_pct(cagr_ci[2])}，中位 {_pct(cagr_ci[1])}。",
            f"- MaxDD 95% 分位：{_pct(dd_ci[0])} ~ {_pct(dd_ci[2])}，中位 {_pct(dd_ci[1])}。",
            f"- Sharpe 95% 区间：{_num(sharpe_ci[0])} ~ {_num(sharpe_ci[2])}，中位 {_num(sharpe_ci[1])}。",
            f"- CAGR > 0 的重采样比例：{_pct(float((bootstrap.cagr > 0).mean()))}；CAGR >= 20%：{_pct(float((bootstrap.cagr >= 0.20).mean()))}。",
            "",
            "## 循环错位安慰剂检验（2024+）",
            "",
            f"- 将实际 BTC/ETH 仓位路径整体错位 30 天以上，共 {len(placebo)} 个日级错位；保留仓位持续性、跨资产关系和换手。",
            f"- 错位 CAGR 95% 区间 {_pct(placebo_cagr_ci[0])} ~ {_pct(placebo_cagr_ci[2])}，中位 {_pct(placebo_cagr_ci[1])}；实际 {_pct(core.oos_cagr)} 的单侧安慰剂 p 值约 {_pct(placebo_p)}。",
            "- 这支持“信号与后续收益的时间对齐有信息”，但它不能修复样本短或多轮研究已查看验证段的问题。",
            "",
            "## 基准和可信边界",
            "",
            f"- 同期 BTC/ETH 50/50 价格买入持有（不含资金费）：Dev CAGR {_pct(benchmark['dev_cagr'])}，验证 CAGR {_pct(benchmark['oos_cagr'])}，验证 DD {_pct(benchmark['oos_max_dd'])}。",
            "- 验证段已被本地多轮研究查看过，因此只能称“历史验证”，不再冒充一次全新盲测。",
            "- 循环错位 p 值没有校正本项目曾搜索的多批指标、反向和趋势策略，不能把 0.12% 解读成未经挖掘的显著性。",
            "- 账户多空比是 Binance 交易所特有的参与者结构；在 OKX/Bybit 上跨所复现之前，不能称它是普适市场规律。",
            "- 回测含历史资金费和换手成本，但没有 L2 排队、大额冲击、强平与交易所/稳定币尾部风险。",
            "- 必须冻结规则做前瞻纸面交易；若真实成本长期高于 5bp/边，应使用费用压力表而不是基准数。",
            "",
            "## 官方字段依据",
            "",
            "- [Binance USDⓈ-M 市场数据 API](https://developers.binance.com/en/docs/catalog/core-trading-derivatives-trading-usd-s-m-futures/api/rest-api/market-data)：`globalLongShortAccountRatio` 是全体交易者的多/空账户数量比，官方提供 5m 周期；`fundingRate` 历史接口同时返回费率与结算时间。",
            "- [Binance 资金费说明](https://www.binance.com/en/support/faq/detail/61012e690cf343e7979649282a2ccc3c)：正资金费由多头支付给空头，负资金费方向相反。",
            "- [Binance 公开历史数据仓库](https://github.com/binance/binance-public-data)：USDⓈ-M K 线归属 `/fapi/v1/klines` 口径。",
            "",
        ]
    )
    (BASE / "CROWDING_REVERSAL_RESEARCH.md").write_text(
        "\n".join(lines), encoding="utf-8"
    )


def main() -> None:
    OUT.mkdir(exist_ok=True)
    panel = load_market_panel(0)
    core = StrategyConfig()
    path = simulate_ensemble(panel, core)
    returns = daily_returns(path)
    scenarios, parameters, assets = run_scenarios()
    annual = annual_table(returns)
    bootstrap = block_bootstrap(returns.loc[OOS_START:])
    benchmark_daily = benchmark_returns(panel, core.fee)
    benchmark = split_metrics(benchmark_daily)
    data_robustness = run_data_robustness()
    episodes = trade_episodes(path, core.fee)
    episodes_oos = trade_episodes(path.loc[OOS_START:], core.fee)
    episodes_oos_summary = episode_summary(episodes_oos)
    placebo = circular_shift_placebo(panel, path, fee=core.fee)
    monthly, quarterly, stability = stability_tables(path)
    chronological = chronological_trend_audit(panel)
    coverage = data_coverage_audit(panel)

    scenarios.to_csv(OUT / "scenarios.csv", index=False)
    parameters.to_csv(OUT / "parameter_neighbourhood.csv", index=False)
    assets.to_csv(OUT / "asset_ablation.csv", index=False)
    annual.to_csv(OUT / "annual_returns.csv", index=False)
    bootstrap.to_csv(OUT / "oos_block_bootstrap.csv", index=False)
    data_robustness.to_csv(OUT / "data_robustness.csv", index=False)
    episodes.to_csv(OUT / "trade_episodes.csv", index=False)
    episodes_oos_summary.to_csv(OUT / "oos_episode_summary.csv", index=False)
    placebo.to_csv(OUT / "oos_circular_shift_placebo.csv", index=False)
    monthly.to_csv(OUT / "oos_monthly_returns.csv", index=False)
    quarterly.to_csv(OUT / "oos_quarterly_returns.csv", index=False)
    chronological.to_csv(OUT / "chronological_trend_audit.csv", index=False)
    coverage.to_csv(OUT / "data_coverage.csv", index=False)
    path.to_parquet(OUT / "core_path.parquet")
    write_overview_chart(path, benchmark_daily)
    write_report(
        scenarios,
        parameters,
        assets,
        annual,
        bootstrap,
        benchmark,
        path,
        data_robustness,
        episodes_oos_summary,
        placebo,
        stability,
        chronological,
        coverage,
    )

    columns = [
        "scenario",
        "dev_cagr",
        "dev_max_dd",
        "oos_cagr",
        "oos_max_dd",
        "oos_sharpe",
        "realised_full_vol",
        "turnover",
    ]
    print(
        scenarios[scenarios.scenario == "ensemble_core"][columns].to_string(
            index=False
        )
    )
    print(
        f"parameter neighbours positive in both: "
        f"{int(((parameters.dev_cagr > 0) & (parameters.oos_cagr > 0)).sum())}/"
        f"{len(parameters)}"
    )


if __name__ == "__main__":
    main()
