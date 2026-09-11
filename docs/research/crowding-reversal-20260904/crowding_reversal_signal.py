"""Print the frozen crowding-reversal portfolio target from local data.

This module intentionally does not connect to an exchange or place orders.  It
turns the research rule into an auditable decision artifact suitable for paper
trading.  Feed it only completed Binance metric/price bars.
"""
from __future__ import annotations

import argparse
import json
from dataclasses import replace

import pandas as pd

from crowding_reversal_research import (
    ENSEMBLE_MEMBERS,
    SYMBOLS,
    StrategyConfig,
    apply_trend_confirmation,
    crowding_scores,
    ensemble_targets,
    hysteresis_signal,
    load_market_panel,
    trend_confirmation_votes,
)


def latest_decision(
    as_of: str | pd.Timestamp | None = None,
    target_vol: float = 0.24,
) -> dict[str, object]:
    panel = load_market_panel(0)
    if as_of is not None:
        timestamp = pd.Timestamp(as_of)
        timestamp = timestamp.tz_localize("UTC") if timestamp.tz is None else timestamp.tz_convert("UTC")
        panel = panel.loc[:timestamp]
    if len(panel) < max(member[0] for member in ENSEMBLE_MEMBERS):
        raise ValueError("not enough completed history for the frozen ensemble")

    config = StrategyConfig(target_vol=target_vol)
    raw_config = replace(config, use_trend_filter=False)
    raw_target, average_signal, average_score = ensemble_targets(panel, raw_config)
    target = apply_trend_confirmation(
        pd.DataFrame(
            {symbol: panel[(symbol, "close")] for symbol in SYMBOLS},
            index=panel.index,
        ),
        raw_target,
        config.trend_horizon_days,
        config.trend_required,
    )
    trend_votes = trend_confirmation_votes(
        pd.DataFrame(
            {symbol: panel[(symbol, "close")] for symbol in SYMBOLS},
            index=panel.index,
        ),
        raw_target,
        config.trend_horizon_days,
    )
    decision_time = panel.index[-1]
    member_votes: dict[str, dict[str, int]] = {
        symbol: {"long": 0, "flat": 0, "short": 0} for symbol in SYMBOLS
    }
    score_cache = {
        lookback: crowding_scores(panel, lookback)
        for lookback in sorted({member[0] for member in ENSEMBLE_MEMBERS})
    }
    for lookback, entry_z, exit_z in ENSEMBLE_MEMBERS:
        signal = hysteresis_signal(score_cache[lookback], entry_z, exit_z)
        for symbol in SYMBOLS:
            value = float(signal.loc[decision_time, symbol])
            label = "long" if value > 0 else "short" if value < 0 else "flat"
            member_votes[symbol][label] += 1

    targets = {symbol: float(target.loc[decision_time, symbol]) for symbol in SYMBOLS}
    return {
        "decision_time_utc": decision_time.isoformat(),
        "intended_holding_interval_utc": {
            "from": decision_time.isoformat(),
            "to": (decision_time + pd.Timedelta(hours=4)).isoformat(),
        },
        "targets_fraction_of_equity": targets,
        "gross_target": sum(abs(value) for value in targets.values()),
        "average_member_signal": {
            symbol: float(average_signal.loc[decision_time, symbol])
            for symbol in SYMBOLS
        },
        "average_zscore": {
            symbol: float(average_score.loc[decision_time, symbol])
            for symbol in SYMBOLS
        },
        "trend_confirmation": {
            "horizon_days": list(config.trend_horizon_days),
            "required_votes": config.trend_required,
            "votes": {
                symbol: int(trend_votes.loc[decision_time, symbol])
                for symbol in SYMBOLS
            },
        },
        "member_votes": member_votes,
        "portfolio_config": {
            "target_vol": config.target_vol,
            "max_gross": config.max_gross,
            "research_cost_per_one_way_turnover": config.fee,
            "decision_lag_bars": config.lag_bars,
            "bar_hours": 4,
            "side": config.side,
            "include_funding": config.include_funding,
        },
        "members": [
            {"lookback_bars": lb, "entry_z": entry, "exit_z": exit_}
            for lb, entry, exit_ in ENSEMBLE_MEMBERS
        ],
        "warning": (
            "paper-trading decision only; verify source freshness and completed bars, "
            "then apply exchange precision and independent risk controls"
        ),
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--as-of", help="optional UTC cutoff, e.g. 2025-12-31T20:00:00Z")
    parser.add_argument("--target-vol", type=float, default=0.24)
    args = parser.parse_args()
    print(json.dumps(latest_decision(args.as_of, args.target_vol), indent=2, ensure_ascii=False))


if __name__ == "__main__":
    main()
