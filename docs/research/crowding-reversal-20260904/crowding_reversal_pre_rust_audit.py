"""Freeze the Python reference before a Rust crowding-reversal port.

This is an implementation-readiness audit, not a new parameter search.  It
replays the already frozen 24% target-volatility strategy, verifies accounting
and causality identities, records exact source hashes, and emits a full 4h
golden fixture for cross-language parity tests.
"""
from __future__ import annotations

import hashlib
import json
import platform
from dataclasses import asdict
from pathlib import Path

import numpy as np
import pandas as pd

from crowding_reversal_research import (
    BASE,
    DEV_END,
    ENSEMBLE_MEMBERS,
    OOS_START,
    SYMBOLS,
    StrategyConfig,
    annual_table,
    daily_returns,
    ensemble_targets,
    load_funding,
    load_market_panel,
    metrics,
    simulate_ensemble,
)


OUT = BASE / "crowding_reversal_results"
MANIFEST = OUT / "rust_reference_manifest.json"
FIXTURE = OUT / "rust_parity_fixture.csv"
SOURCE_FILES = (
    "btc_um_1m.parquet",
    "eth_um_1m.parquet",
    "btcusdt_metrics_5m.parquet",
    "ethusdt_metrics_5m.parquet",
    "btcusdt_funding.parquet",
    "ethusdt_funding.parquet",
)
PREFIX_TIMES = (
    "2022-06-30T20:00:00Z",
    "2023-06-30T20:00:00Z",
    "2023-12-31T20:00:00Z",
    "2024-06-30T20:00:00Z",
    "2025-06-30T20:00:00Z",
    "2025-12-31T20:00:00Z",
    "2026-06-30T20:00:00Z",
)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        while chunk := stream.read(8 * 1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def strict_completed_panel() -> pd.DataFrame:
    """Alternative standard [start,end) aggregation for boundary robustness."""
    blocks: list[pd.DataFrame] = []
    for symbol in SYMBOLS:
        metric = pd.read_parquet(
            BASE / f"{symbol.lower()}usdt_metrics_5m.parquet",
            columns=[
                "create_time",
                "count_long_short_ratio",
                "sum_open_interest_value",
            ],
        )
        metric.index = pd.DatetimeIndex(
            pd.to_datetime(metric.pop("create_time"), utc=True)
        ).as_unit("ns")
        metric = metric.sort_index().resample(
            "4h", label="right", closed="left"
        ).last()

        price = pd.read_parquet(
            BASE / f"{symbol.lower()}_um_1m.parquet", columns=["ts", "c"]
        )
        price.index = pd.DatetimeIndex(
            pd.to_datetime(price.pop("ts"), unit="ms", utc=True)
        ).as_unit("ns")
        close = price.c.sort_index().resample(
            "4h", label="right", closed="left"
        ).last().rename("close")
        joined = close.to_frame().join(metric, how="left")
        joined.columns = pd.MultiIndex.from_product([[symbol], joined.columns])
        blocks.append(joined)

    panel = pd.concat(blocks, axis=1, join="inner").sort_index()
    panel = panel.dropna(subset=[(symbol, "close") for symbol in SYMBOLS])
    available = panel[
        [(symbol, "count_long_short_ratio") for symbol in SYMBOLS]
    ].notna().any(axis=1)
    return panel.loc[: available[available].index[-1]]


def _rolling_365(daily: pd.Series, start: str | None = None) -> dict[str, object]:
    rolling = (
        (1.0 + daily).rolling(365, min_periods=365).apply(np.prod, raw=True) - 1.0
    )
    if start is not None:
        rolling = rolling.loc[start:]
    rolling = rolling.dropna()
    return {
        "windows": int(len(rolling)),
        "minimum": float(rolling.min()),
        "median": float(rolling.median()),
        "positive_share": float(rolling.gt(0.0).mean()),
        "ge_20_percent_share": float(rolling.ge(0.20).mean()),
        "minimum_window_end": rolling.idxmin().isoformat(),
    }


def _prefix_causality_error(
    panel: pd.DataFrame, full_target: pd.DataFrame, config: StrategyConfig
) -> tuple[float, list[dict[str, object]]]:
    rows = []
    maximum = 0.0
    for raw_time in PREFIX_TIMES:
        timestamp = pd.Timestamp(raw_time)
        prefix = panel.loc[:timestamp]
        actual = ensemble_targets(prefix, config)[0].iloc[-1]
        expected = full_target.loc[prefix.index[-1]]
        error = float((actual - expected).abs().max())
        maximum = max(maximum, error)
        rows.append(
            {
                "timestamp": prefix.index[-1].isoformat(),
                "max_absolute_target_error": error,
            }
        )
    return maximum, rows


def _source_manifest() -> dict[str, dict[str, object]]:
    result = {}
    for name in SOURCE_FILES:
        path = BASE / name
        frame = pd.read_parquet(path)
        time_column = next(
            column for column in ("ts", "create_time", "calc_time")
            if column in frame.columns
        )
        unit = "ms" if time_column in {"ts", "calc_time"} else None
        timestamp = pd.to_datetime(frame[time_column], unit=unit, utc=True)
        result[name] = {
            "bytes": path.stat().st_size,
            "rows": len(frame),
            "first_timestamp": timestamp.min().isoformat(),
            "last_timestamp": timestamp.max().isoformat(),
            "duplicate_timestamps": int(timestamp.duplicated().sum()),
            "sha256": sha256_file(path),
        }
    return result


def write_fixture(panel: pd.DataFrame, path: pd.DataFrame) -> dict[str, object]:
    funding = load_funding(panel.index)
    fixture = pd.DataFrame(index=panel.index)
    fixture.index.name = "timestamp_utc"
    for symbol in SYMBOLS:
        fixture[f"input_{symbol}_close"] = panel[(symbol, "close")]
        fixture[f"input_{symbol}_global_account_ratio"] = panel[
            (symbol, "count_long_short_ratio")
        ]
        fixture[f"input_{symbol}_funding_rate"] = funding[symbol]
        for field in (
            "score", "signal", "target", "position", "price_pnl",
            "funding_pnl", "cost",
        ):
            fixture[f"expected_{field}_{symbol}"] = path[f"{field}_{symbol}"]
    for field in ("price_pnl", "funding_pnl", "cost", "turnover", "net", "equity"):
        fixture[f"expected_{field}"] = path[field]
    fixture.to_csv(FIXTURE, float_format="%.17g")
    return {
        "path": str(FIXTURE),
        "rows": len(fixture),
        "columns": fixture.columns.tolist(),
        "sha256": sha256_file(FIXTURE),
    }


def run_audit() -> dict[str, object]:
    OUT.mkdir(exist_ok=True)
    config = StrategyConfig()
    panel = load_market_panel(0)
    target, _, _ = ensemble_targets(panel, config)
    path = simulate_ensemble(panel, config)
    daily = daily_returns(path)
    strict_panel = strict_completed_panel()
    strict_daily = daily_returns(simulate_ensemble(strict_panel, config))

    position_columns = [f"position_{symbol}" for symbol in SYMBOLS]
    lag_error = max(
        float(
            (
                path[f"position_{symbol}"]
                - path[f"target_{symbol}"].shift(1).fillna(0.0)
            ).abs().max()
        )
        for symbol in SYMBOLS
    )
    pnl_error = float(
        (path.net - (path.price_pnl + path.funding_pnl - path.cost)).abs().max()
    )
    prefix_error, prefix_rows = _prefix_causality_error(panel, target, config)
    index_steps = panel.index.to_series().diff().dropna()
    integrity = {
        "index_monotonic": bool(panel.index.is_monotonic_increasing),
        "index_unique": bool(panel.index.is_unique),
        "non_4h_index_steps": int(index_steps.ne(pd.Timedelta(hours=4)).sum()),
        "maximum_gross_position": float(path[position_columns].abs().sum(axis=1).max()),
        "maximum_position_lag_error": lag_error,
        "maximum_pnl_identity_error": pnl_error,
        "maximum_prefix_causality_error": prefix_error,
        "prefix_checks": prefix_rows,
    }
    hard_integrity_pass = bool(
        integrity["index_monotonic"]
        and integrity["index_unique"]
        and integrity["non_4h_index_steps"] == 0
        and integrity["maximum_gross_position"] <= config.max_gross + 1e-12
        and lag_error <= 1e-15
        and pnl_error <= 1e-15
        and prefix_error <= 1e-12
    )

    fee_stress = {}
    for basis_points in (5, 10, 15, 20):
        stressed = daily_returns(
            simulate_ensemble(
                panel, StrategyConfig(fee=basis_points / 10_000.0)
            )
        )
        fee_stress[str(basis_points)] = {
            "development": metrics(stressed, end=DEV_END),
            "historical_validation": metrics(stressed, start=OOS_START),
        }

    annual = annual_table(daily)
    complete_years = annual.loc[annual.days.ge(365)]
    core = {
        "full": metrics(daily),
        "development": metrics(daily, end=DEV_END),
        "historical_validation": metrics(daily, start=OOS_START),
        "from_2022": metrics(daily.loc["2022-01-01":]),
        "annual_returns": annual.to_dict(orient="records"),
        "complete_years": int(len(complete_years)),
        "complete_years_ge_20_percent": int(
            complete_years.calendar_return.ge(0.20).sum()
        ),
        "rolling_365_full": _rolling_365(daily),
        "rolling_365_validation": _rolling_365(daily, OOS_START),
        "strict_completed_interval_development": metrics(
            strict_daily, end=DEV_END
        ),
        "strict_completed_interval_validation": metrics(
            strict_daily, start=OOS_START
        ),
        "fee_stress": fee_stress,
    }
    backtest_gate = bool(
        hard_integrity_pass
        and core["development"]["cagr"] >= 0.20
        and core["historical_validation"]["cagr"] >= 0.20
        and fee_stress["10"]["development"]["cagr"] >= 0.20
        and fee_stress["10"]["historical_validation"]["cagr"] >= 0.20
        and core["complete_years"] >= 4
        and core["complete_years_ge_20_percent"] == core["complete_years"]
    )

    result: dict[str, object] = {
        "schema_version": 1,
        "decision": (
            "GO_FOR_RUST_REFERENCE_ENGINE_AND_FORWARD_PAPER_TRADING"
            if backtest_gate
            else "NO_GO_FOR_RUST_PORT"
        ),
        "explicitly_not_approved": [
            "full-capital live trading",
            "monthly income guarantee",
            "parameter changes selected on 2024+ results",
        ],
        "python": platform.python_version(),
        "pandas": pd.__version__,
        "panel": {
            "first": panel.index.min().isoformat(),
            "last": panel.index.max().isoformat(),
            "rows_4h": len(panel),
        },
        "configuration": asdict(config),
        "ensemble_members": [list(member) for member in ENSEMBLE_MEMBERS],
        "source_files": _source_manifest(),
        "integrity": integrity,
        "hard_integrity_pass": hard_integrity_pass,
        "core_statistics": core,
        "rust_parity_fixture": write_fixture(panel, path),
        "known_limits": [
            "The 2024+ segment has been inspected repeatedly and is not a fresh blind test.",
            "There are four complete natural years, not enough to guarantee future 20% CAGR.",
            "The full-sample worst rolling 365-day return is below 20%.",
            "The bootstrap 95% CAGR interval extends below 20%.",
            "Crowding data is Binance-specific and has not been replicated cross-exchange.",
            "Backtest costs omit order-book queueing, outages, liquidation and venue tail risk.",
        ],
    }
    # Fixture hash is computed after writing; now freeze the complete manifest.
    MANIFEST.write_text(
        json.dumps(result, indent=2, ensure_ascii=False, default=str),
        encoding="utf-8",
    )
    return result


def main() -> None:
    result = run_audit()
    core = result["core_statistics"]
    print("decision", result["decision"])
    print("hard_integrity_pass", result["hard_integrity_pass"])
    print("full", core["full"])
    print("development", core["development"])
    print("historical_validation", core["historical_validation"])
    print("rolling_365_full", core["rolling_365_full"])
    print("strict_boundary_dev", core["strict_completed_interval_development"])
    print("strict_boundary_validation", core["strict_completed_interval_validation"])
    print("manifest", MANIFEST)
    print("fixture", FIXTURE)


if __name__ == "__main__":
    main()
