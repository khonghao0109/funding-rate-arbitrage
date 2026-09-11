"""Build a self-contained HTML dashboard for the 20% and 24% risk profiles.

The two profiles share exactly the same alpha and entry/exit decisions.  Only
the volatility target differs.  The dashboard deliberately exposes weekly and
monthly lumpiness instead of showing only CAGR and maximum drawdown.
"""
from __future__ import annotations

import base64
from io import BytesIO
from pathlib import Path

import matplotlib.pyplot as plt
import numpy as np
import pandas as pd

from crowding_reversal_research import (
    BASE,
    DEV_END,
    OOS_START,
    OUT,
    SYMBOLS,
    StrategyConfig,
    annual_table,
    benchmark_returns,
    daily_returns,
    load_market_panel,
    metrics,
    simulate_ensemble,
    trade_episodes,
)


DATA_END = "2026-06-30 23:59:59"
PROFILES = {
    "risk20": ("20% target-vol", StrategyConfig(target_vol=0.20)),
    "risk24": ("24% target-vol", StrategyConfig(target_vol=0.24)),
}
COLORS = {"risk20": "#2c7be5", "risk24": "#e8590c", "benchmark": "#8290a3"}


def _period_returns(
    daily: pd.Series, rule: str, minimum_days: int
) -> pd.DataFrame:
    returns = ((1.0 + daily).resample(rule).prod() - 1.0).rename("return")
    days = daily.resample(rule).count().rename("observations")
    frame = pd.concat([returns, days], axis=1).reset_index(names="period_end")
    frame["complete"] = frame.observations >= minimum_days
    frame["sample"] = np.where(
        frame.period_end < pd.Timestamp(OOS_START, tz="UTC"),
        "development",
        "historical_validation",
    )
    return frame


def _drawdown(returns: pd.Series) -> pd.Series:
    equity = (1.0 + returns).cumprod()
    return equity / equity.cummax().clip(lower=1.0) - 1.0


def _longest_underwater_days(returns: pd.Series) -> int:
    underwater = _drawdown(returns) < 0
    groups = underwater.ne(underwater.shift()).cumsum()
    lengths = underwater.groupby(groups).sum()
    return int(lengths.max()) if len(lengths) else 0


def _figure_uri(figure: plt.Figure) -> str:
    stream = BytesIO()
    figure.savefig(stream, format="png", dpi=165, bbox_inches="tight")
    plt.close(figure)
    encoded = base64.b64encode(stream.getvalue()).decode("ascii")
    return f"data:image/png;base64,{encoded}"


def _equity_figure(
    daily: dict[str, pd.Series], benchmark: pd.Series
) -> str:
    figure, axis = plt.subplots(figsize=(12, 5.2))
    for key, series in daily.items():
        equity = 10_000 * (1.0 + series).cumprod()
        axis.plot(equity.index, equity, label=PROFILES[key][0], color=COLORS[key], lw=1.7)
    benchmark_equity = 10_000 * (1.0 + benchmark).cumprod()
    axis.plot(
        benchmark_equity.index,
        benchmark_equity,
        label="BTC/ETH 50/50 buy-and-hold",
        color=COLORS["benchmark"],
        lw=1.2,
        alpha=0.85,
    )
    axis.axvline(pd.Timestamp(OOS_START, tz="UTC"), color="#111827", ls="--", lw=1)
    axis.text(
        pd.Timestamp(OOS_START, tz="UTC"),
        axis.get_ylim()[1],
        "  historical validation starts",
        ha="left",
        va="top",
        fontsize=8,
    )
    axis.set_yscale("log")
    axis.set_title("Growth of 10,000 USDT (log scale)")
    axis.set_ylabel("USDT")
    axis.grid(alpha=0.22)
    axis.legend(loc="upper left", ncol=3)
    return _figure_uri(figure)


def _drawdown_figure(daily: dict[str, pd.Series]) -> str:
    figure, axis = plt.subplots(figsize=(12, 4.2))
    for key, series in daily.items():
        drawdown = _drawdown(series.loc[OOS_START:DATA_END]) * 100
        axis.fill_between(
            drawdown.index,
            drawdown.to_numpy(),
            0,
            alpha=0.13,
            color=COLORS[key],
        )
        axis.plot(drawdown.index, drawdown, label=PROFILES[key][0], color=COLORS[key])
    axis.axhline(0, color="#111827", lw=0.8)
    axis.set_title("Historical-validation drawdown (daily close)")
    axis.set_ylabel("drawdown %")
    axis.grid(alpha=0.22)
    axis.legend(loc="lower left")
    return _figure_uri(figure)


def _monthly_figure(monthly: pd.DataFrame) -> str:
    view = monthly[
        monthly.complete & (monthly["sample"] == "historical_validation")
    ].copy()
    wide = view.pivot(index="period_end", columns="profile", values="return")
    x = np.arange(len(wide))
    figure, axis = plt.subplots(figsize=(13, 4.8))
    width = 0.42
    axis.bar(
        x - width / 2,
        wide.risk20 * 100,
        width,
        label=PROFILES["risk20"][0],
        color=COLORS["risk20"],
    )
    axis.bar(
        x + width / 2,
        wide.risk24 * 100,
        width,
        label=PROFILES["risk24"][0],
        color=COLORS["risk24"],
    )
    labels = [timestamp.strftime("%Y-%m") for timestamp in wide.index]
    axis.set_xticks(x)
    axis.set_xticklabels(labels, rotation=60, ha="right", fontsize=7)
    axis.axhline(0, color="#111827", lw=0.8)
    axis.set_title("Monthly returns: every complete month")
    axis.set_ylabel("return %")
    axis.grid(axis="y", alpha=0.22)
    axis.legend()
    return _figure_uri(figure)


def _weekly_distribution_figure(weekly: pd.DataFrame) -> str:
    view = weekly[
        weekly.complete & (weekly["sample"] == "historical_validation")
    ]
    figure, axis = plt.subplots(figsize=(8, 4.5))
    bins = np.linspace(-0.06, 0.14, 36)
    for key in PROFILES:
        values = view.loc[view.profile == key, "return"] * 100
        axis.hist(
            values,
            bins=bins * 100,
            alpha=0.48,
            label=PROFILES[key][0],
            color=COLORS[key],
        )
    axis.axvline(0, color="#111827", lw=0.8)
    axis.set_title("Weekly return distribution")
    axis.set_xlabel("weekly return %")
    axis.set_ylabel("number of weeks")
    axis.grid(axis="y", alpha=0.2)
    axis.legend()
    return _figure_uri(figure)


def _rolling_figure(daily: dict[str, pd.Series]) -> str:
    figure, axis = plt.subplots(figsize=(8, 4.5))
    for key, series in daily.items():
        rolling = (
            (1.0 + series.loc[OOS_START:DATA_END])
            .rolling(365, min_periods=365)
            .apply(np.prod, raw=True)
            - 1.0
        )
        axis.plot(rolling.index, rolling * 100, label=PROFILES[key][0], color=COLORS[key])
    axis.axhline(20, color="#16a34a", ls="--", lw=1, label="20% reference")
    axis.axhline(0, color="#111827", lw=0.8)
    axis.set_title("Trailing 365-day return")
    axis.set_ylabel("return %")
    axis.grid(alpha=0.22)
    axis.legend()
    return _figure_uri(figure)


def _position_figure(paths: dict[str, pd.DataFrame]) -> str:
    figure, axes = plt.subplots(2, 1, figsize=(12, 6.5), sharex=True)
    for axis, key in zip(axes, PROFILES):
        view = paths[key].loc[OOS_START:DATA_END]
        axis.plot(
            view.index,
            view.position_BTC * 100,
            label="BTC",
            color="#2c7be5",
            lw=0.8,
        )
        axis.plot(
            view.index,
            view.position_ETH * 100,
            label="ETH",
            color="#e8590c",
            lw=0.8,
        )
        axis.axhline(0, color="#111827", lw=0.7)
        axis.set_ylabel("% of equity")
        axis.set_title(PROFILES[key][0])
        axis.grid(alpha=0.18)
        axis.legend(loc="upper right", ncol=2)
    axes[-1].set_xlabel("UTC date")
    figure.suptitle("Actual 4h holdings (positive = long, negative = short)", y=1.01)
    figure.tight_layout()
    return _figure_uri(figure)


def _holding_figure(trades: pd.DataFrame) -> str:
    view = trades[trades["sample"] == "historical_validation"]
    figure, axes = plt.subplots(1, 2, figsize=(11, 4.4))
    for symbol, color in (("BTC", "#2c7be5"), ("ETH", "#e8590c")):
        values = view.loc[
            (view.profile == "risk24") & (view.symbol == symbol), "holding_days"
        ]
        axes[0].hist(values.clip(upper=15), bins=30, alpha=0.55, label=symbol, color=color)
    axes[0].set_title("Holding time (values above 15d clipped)")
    axes[0].set_xlabel("days")
    axes[0].set_ylabel("campaigns")
    axes[0].legend()
    summary = (
        view[view.profile == "risk24"]
        .groupby(["symbol", "side"])
        .holding_days.agg(["count", "median", "mean"])
        .reset_index()
    )
    x = np.arange(len(summary))
    axes[1].bar(x - 0.18, summary["median"], 0.36, label="median", color="#2c7be5")
    axes[1].bar(x + 0.18, summary["mean"], 0.36, label="mean", color="#f59f00")
    axes[1].set_xticks(x)
    axes[1].set_xticklabels(
        summary.symbol + " " + summary.side, rotation=25, ha="right"
    )
    axes[1].set_ylabel("days")
    axes[1].set_title("Holding time by asset and side")
    axes[1].legend()
    for axis in axes:
        axis.grid(axis="y", alpha=0.2)
    figure.tight_layout()
    return _figure_uri(figure)


def _monthly_attribution_figure(paths: dict[str, pd.DataFrame]) -> str:
    path = paths["risk24"].loc[OOS_START:DATA_END]
    parts = path[["price_pnl", "funding_pnl", "cost"]].copy()
    parts["cost"] *= -1
    monthly = parts.resample("ME").sum()
    figure, axis = plt.subplots(figsize=(12, 4.8))
    x = np.arange(len(monthly))
    axis.bar(x, monthly.price_pnl * 100, label="price PnL", color="#2c7be5")
    axis.bar(
        x,
        monthly.funding_pnl * 100,
        bottom=monthly.price_pnl * 100,
        label="funding PnL",
        color="#16a34a",
    )
    axis.bar(x, monthly.cost * 100, label="turnover cost", color="#dc2626")
    axis.axhline(0, color="#111827", lw=0.8)
    axis.set_xticks(x)
    axis.set_xticklabels(
        [timestamp.strftime("%Y-%m") for timestamp in monthly.index],
        rotation=60,
        ha="right",
        fontsize=7,
    )
    axis.set_title("24% profile: monthly additive PnL attribution")
    axis.set_ylabel("portfolio contribution %")
    axis.grid(axis="y", alpha=0.2)
    axis.legend(ncol=3)
    return _figure_uri(figure)


def _enriched_trades(
    paths: dict[str, pd.DataFrame], configs: dict[str, StrategyConfig]
) -> pd.DataFrame:
    blocks = []
    for key, path in paths.items():
        episodes = trade_episodes(path, configs[key].fee).copy()
        episodes.insert(0, "profile", key)
        episodes["profile_name"] = PROFILES[key][0]
        episodes["sample"] = np.where(
            episodes.start < pd.Timestamp(OOS_START, tz="UTC"),
            "development",
            "historical_validation",
        )
        episodes["holding_days"] = episodes.bars * 4 / 24
        entry_weight = []
        average_weight = []
        maximum_weight = []
        status = []
        for row in episodes.itertuples():
            position = path.loc[row.start : row.end, f"position_{row.symbol}"]
            entry_weight.append(float(position.iloc[0]))
            average_weight.append(float(position.abs().mean()))
            maximum_weight.append(float(position.abs().max()))
            status.append("open_at_data_end" if row.end == path.index[-1] else "closed")
        episodes["entry_weight"] = entry_weight
        episodes["average_abs_weight"] = average_weight
        episodes["maximum_abs_weight"] = maximum_weight
        episodes["status"] = status
        episodes["result"] = np.where(episodes.net_contribution > 0, "win", "loss")
        blocks.append(episodes)
    return pd.concat(blocks, ignore_index=True).sort_values(
        ["start", "profile", "symbol"]
    )


def _monthly_order_activity(
    paths: dict[str, pd.DataFrame], trades: pd.DataFrame
) -> tuple[pd.DataFrame, pd.DataFrame]:
    """Separate directional campaigns from 4h position-resize orders."""
    months = pd.period_range("2024-01", "2026-06", freq="M").astype(str)
    monthly_blocks = []
    summary_rows = []
    for key, path in paths.items():
        positions = path[[f"position_{symbol}" for symbol in SYMBOLS]].loc[:DATA_END]
        previous = positions.shift(1).fillna(0.0)
        events = []
        for symbol in SYMBOLS:
            current = positions[f"position_{symbol}"]
            prior = previous[f"position_{symbol}"]
            delta = current - prior
            changed = delta.abs() > 1e-12
            for timestamp in current.index[changed]:
                old = float(prior.loc[timestamp])
                new = float(current.loc[timestamp])
                if old == 0.0 and new != 0.0:
                    event_type = "open"
                elif old != 0.0 and new == 0.0:
                    event_type = "close"
                elif np.sign(old) != np.sign(new):
                    event_type = "flip"
                elif abs(new) > abs(old):
                    event_type = "resize_up"
                else:
                    event_type = "resize_down"
                events.append(
                    {
                        "timestamp": timestamp,
                        "symbol": symbol,
                        "event_type": event_type,
                        "one_way_turnover": abs(new - old),
                    }
                )
        events = pd.DataFrame(events)
        events = events[
            (events.timestamp >= pd.Timestamp(OOS_START, tz="UTC"))
            & (events.timestamp <= pd.Timestamp(DATA_END, tz="UTC"))
        ].copy()
        events["month"] = events.timestamp.dt.tz_localize(None).dt.to_period("M").astype(str)

        oos_positions = positions.loc[OOS_START:DATA_END]
        gross = oos_positions.abs().sum(axis=1)
        gross_frame = pd.DataFrame(
            {
                "month": gross.index.tz_localize(None).to_period("M").astype(str),
                "gross": gross.to_numpy(),
            }
        )
        rows = []
        for month in months:
            month_events = events[events.month == month]
            month_gross = gross_frame.loc[gross_frame.month == month, "gross"]
            counts = month_events.event_type.value_counts()
            flips = int(counts.get("flip", 0))
            rows.append(
                {
                    "profile": key,
                    "profile_name": PROFILES[key][0],
                    "month": month,
                    "campaign_entries": int(counts.get("open", 0)) + flips,
                    "campaign_exits": int(counts.get("close", 0)) + flips,
                    "direction_flips": flips,
                    "resize_orders": int(counts.get("resize_up", 0))
                    + int(counts.get("resize_down", 0)),
                    "all_order_events": int(len(month_events)),
                    "one_way_turnover": float(month_events.one_way_turnover.sum()),
                    "mean_gross_exposure": float(month_gross.mean()),
                    "max_gross_exposure": float(month_gross.max()),
                    "active_bar_share": float((month_gross > 1e-12).mean()),
                }
            )
        monthly = pd.DataFrame(rows)
        monthly_blocks.append(monthly)

        profile_trades = trades[
            (trades.profile == key)
            & (trades.start >= pd.Timestamp(OOS_START, tz="UTC"))
            & (trades.start <= pd.Timestamp(DATA_END, tz="UTC"))
        ]
        active_gross = gross[gross > 1e-12]
        summary_rows.append(
            {
                "profile": key,
                "profile_name": PROFILES[key][0],
                "entries_per_month": float(monthly.campaign_entries.mean()),
                "exits_per_month": float(monthly.campaign_exits.mean()),
                "entry_exit_orders_per_month": float(
                    (monthly.campaign_entries + monthly.campaign_exits).mean()
                ),
                "all_order_events_per_month": float(monthly.all_order_events.mean()),
                "one_way_turnover_per_month": float(monthly.one_way_turnover.mean()),
                "mean_gross_exposure": float(gross.mean()),
                "mean_active_gross_exposure": float(active_gross.mean()),
                "max_gross_exposure": float(gross.max()),
                "median_holding_days": float(profile_trades.holding_days.median()),
                "mean_holding_days": float(profile_trades.holding_days.mean()),
                "p90_holding_days": float(profile_trades.holding_days.quantile(0.90)),
                "max_holding_days": float(profile_trades.holding_days.max()),
            }
        )
    return pd.concat(monthly_blocks, ignore_index=True), pd.DataFrame(summary_rows)


def _format_percent(value: object) -> str:
    if pd.isna(value):
        return "—"
    return f"{float(value):.2%}"


def _html_table(
    frame: pd.DataFrame,
    table_id: str,
    percent_columns: tuple[str, ...] = (),
    decimal_columns: tuple[str, ...] = (),
) -> str:
    display = frame.copy()
    for column in display.columns:
        if pd.api.types.is_datetime64_any_dtype(display[column]):
            display[column] = display[column].dt.strftime("%Y-%m-%d %H:%M UTC")
    for column in percent_columns:
        if column in display:
            display[column] = display[column].map(_format_percent)
    for column in decimal_columns:
        if column in display:
            display[column] = display[column].map(
                lambda value: "—" if pd.isna(value) else f"{float(value):.2f}"
            )
    return display.to_html(index=False, table_id=table_id, classes="data-table", escape=True)


def build_dashboard() -> Path:
    OUT.mkdir(exist_ok=True)
    panel = load_market_panel(0)
    configs = {key: config for key, (_, config) in PROFILES.items()}
    paths = {key: simulate_ensemble(panel, config) for key, config in configs.items()}
    daily = {key: daily_returns(path).loc[:DATA_END] for key, path in paths.items()}
    benchmark = benchmark_returns(panel, StrategyConfig().fee).loc[:DATA_END]

    weekly_blocks = []
    monthly_blocks = []
    annual_blocks = []
    summary_rows = []
    for key in PROFILES:
        weekly = _period_returns(daily[key], "W-SUN", 7)
        weekly.insert(0, "profile", key)
        weekly.insert(1, "profile_name", PROFILES[key][0])
        weekly_blocks.append(weekly)
        monthly = _period_returns(daily[key], "ME", 20)
        monthly.insert(0, "profile", key)
        monthly.insert(1, "profile_name", PROFILES[key][0])
        monthly_blocks.append(monthly)
        annual = annual_table(daily[key])
        annual.insert(0, "profile", key)
        annual.insert(1, "profile_name", PROFILES[key][0])
        annual_blocks.append(annual)

        oos_daily = daily[key].loc[OOS_START:DATA_END]
        oos_weekly = weekly[
            weekly.complete & (weekly["sample"] == "historical_validation")
        ].set_index("period_end")["return"]
        oos_monthly = monthly[
            monthly.complete & (monthly["sample"] == "historical_validation")
        ].set_index("period_end")["return"]
        dev_stats = metrics(daily[key], None, DEV_END)
        oos_stats = metrics(daily[key], OOS_START, DATA_END)
        four_hour = paths[key].loc[OOS_START:DATA_END, "net"]
        four_hour_dd = _drawdown(four_hour)
        weekly_log = np.log1p(oos_weekly).sort_values(ascending=False)
        summary_rows.append(
            {
                "profile": key,
                "profile_name": PROFILES[key][0],
                "dev_cagr": dev_stats["cagr"],
                "oos_cagr": oos_stats["cagr"],
                "oos_daily_max_dd": oos_stats["max_dd"],
                "oos_4h_max_dd": float(four_hour_dd.min()),
                "oos_sharpe": oos_stats["sharpe"],
                "positive_weeks": float((oos_weekly > 0).mean()),
                "median_week": float(oos_weekly.median()),
                "worst_week": float(oos_weekly.min()),
                "best_week": float(oos_weekly.max()),
                "positive_months": float((oos_monthly > 0).mean()),
                "median_month": float(oos_monthly.median()),
                "worst_month": float(oos_monthly.min()),
                "best_month": float(oos_monthly.max()),
                "longest_underwater_days": _longest_underwater_days(oos_daily),
                "top_5_week_log_growth_share": float(
                    weekly_log.head(5).sum() / weekly_log.sum()
                ),
            }
        )

    weekly_all = pd.concat(weekly_blocks, ignore_index=True)
    monthly_all = pd.concat(monthly_blocks, ignore_index=True)
    annual_all = pd.concat(annual_blocks, ignore_index=True)
    summary = pd.DataFrame(summary_rows)
    trades = _enriched_trades(paths, configs)
    monthly_orders, order_summary = _monthly_order_activity(paths, trades)

    daily_export = pd.DataFrame(index=daily["risk20"].index)
    daily_export.index.name = "date_utc"
    for key in PROFILES:
        series = daily[key].reindex(daily_export.index).fillna(0.0)
        daily_export[f"{key}_return"] = series
        daily_export[f"{key}_equity_growth_of_1"] = (1.0 + series).cumprod()
        daily_export[f"{key}_drawdown"] = _drawdown(series)
        positions = paths[key][[f"position_{symbol}" for symbol in SYMBOLS]]
        for symbol in SYMBOLS:
            daily_export[f"{key}_{symbol.lower()}_end_position"] = (
                positions[f"position_{symbol}"].resample("1D").last().reindex(daily_export.index)
            )
        daily_export[f"{key}_mean_gross_exposure"] = (
            positions.abs().sum(axis=1).resample("1D").mean().reindex(daily_export.index)
        )
    daily_export["benchmark_return"] = benchmark.reindex(daily_export.index).fillna(0.0)
    daily_export["benchmark_equity_growth_of_1"] = (
        1.0 + daily_export.benchmark_return
    ).cumprod()

    weekly_all.to_csv(OUT / "weekly_returns_comparison.csv", index=False)
    monthly_all.to_csv(OUT / "monthly_returns_comparison.csv", index=False)
    annual_all.to_csv(OUT / "annual_returns_comparison.csv", index=False)
    summary.to_csv(OUT / "risk_profile_summary.csv", index=False)
    trades.to_csv(OUT / "trade_log_comparison.csv", index=False)
    monthly_orders.to_csv(OUT / "monthly_order_activity.csv", index=False)
    daily_export.to_csv(OUT / "daily_equity_comparison.csv")

    summary_display = summary[
        [
            "profile_name",
            "dev_cagr",
            "oos_cagr",
            "oos_daily_max_dd",
            "oos_4h_max_dd",
            "oos_sharpe",
            "positive_weeks",
            "median_week",
            "positive_months",
            "median_month",
            "longest_underwater_days",
            "top_5_week_log_growth_share",
        ]
    ].rename(
        columns={
            "profile_name": "risk profile",
            "dev_cagr": "dev CAGR",
            "oos_cagr": "2024+ CAGR",
            "oos_daily_max_dd": "daily max DD",
            "oos_4h_max_dd": "4h max DD",
            "oos_sharpe": "Sharpe",
            "positive_weeks": "positive weeks",
            "median_week": "median week",
            "positive_months": "positive months",
            "median_month": "median month",
            "longest_underwater_days": "longest underwater days",
            "top_5_week_log_growth_share": "top-5-week growth share",
        }
    )
    summary_html = _html_table(
        summary_display,
        "summary-table",
        percent_columns=(
            "dev CAGR",
            "2024+ CAGR",
            "daily max DD",
            "4h max DD",
            "positive weeks",
            "median week",
            "positive months",
            "median month",
            "top-5-week growth share",
        ),
        decimal_columns=("Sharpe",),
    )

    monthly_display = monthly_all.sort_values(["period_end", "profile"])[
        ["period_end", "profile_name", "sample", "observations", "complete", "return"]
    ].rename(columns={"profile_name": "risk profile", "return": "return"})
    weekly_display = weekly_all.sort_values(["period_end", "profile"])[
        ["period_end", "profile_name", "sample", "observations", "complete", "return"]
    ].rename(columns={"profile_name": "risk profile", "return": "return"})
    annual_display = annual_all.sort_values(["year", "profile"])[
        ["year", "profile_name", "days", "calendar_return", "annualised_if_partial"]
    ].rename(columns={"profile_name": "risk profile"})
    trade_display = trades.sort_values("start", ascending=False)[
        [
            "profile_name",
            "sample",
            "symbol",
            "side",
            "start",
            "end",
            "holding_days",
            "entry_weight",
            "average_abs_weight",
            "maximum_abs_weight",
            "gross_contribution",
            "cost",
            "net_contribution",
            "result",
            "status",
        ]
    ].rename(columns={"profile_name": "risk profile"})
    order_summary_display = order_summary[
        [
            "profile_name",
            "entries_per_month",
            "exits_per_month",
            "entry_exit_orders_per_month",
            "all_order_events_per_month",
            "one_way_turnover_per_month",
            "mean_gross_exposure",
            "mean_active_gross_exposure",
            "max_gross_exposure",
            "median_holding_days",
            "mean_holding_days",
            "p90_holding_days",
            "max_holding_days",
        ]
    ].rename(columns={"profile_name": "risk profile"})
    monthly_order_display = monthly_orders[
        [
            "month",
            "profile_name",
            "campaign_entries",
            "campaign_exits",
            "direction_flips",
            "resize_orders",
            "all_order_events",
            "one_way_turnover",
            "mean_gross_exposure",
            "max_gross_exposure",
            "active_bar_share",
        ]
    ].rename(columns={"profile_name": "risk profile"})

    chart_equity = _equity_figure(daily, benchmark)
    chart_drawdown = _drawdown_figure(daily)
    chart_monthly = _monthly_figure(monthly_all)
    chart_weekly = _weekly_distribution_figure(weekly_all)
    chart_rolling = _rolling_figure(daily)
    chart_positions = _position_figure(paths)
    chart_holding = _holding_figure(trades)
    chart_attribution = _monthly_attribution_figure(paths)

    html = f"""<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>BTC/ETH 4h 拥挤反转回测仪表板</title>
<style>
:root{{--bg:#f4f7fb;--card:#fff;--ink:#172033;--muted:#657188;--line:#dbe2ea;--blue:#2c7be5;--orange:#e8590c;}}
*{{box-sizing:border-box}} body{{margin:0;background:var(--bg);color:var(--ink);font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;line-height:1.55}}
.wrap{{max-width:1460px;margin:auto;padding:28px}} h1{{margin:0 0 8px;font-size:28px}} h2{{margin:0 0 14px;font-size:20px}} p{{margin:6px 0}} .muted{{color:var(--muted)}}
.notice{{background:#fff8e6;border:1px solid #f7d58b;border-radius:10px;padding:13px 16px;margin:18px 0}}
.card{{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:18px;margin:16px 0;box-shadow:0 2px 8px rgba(25,42,70,.04)}}
.grid{{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:16px}} .chart img{{display:block;width:100%;height:auto}}
.scroll{{max-height:560px;overflow:auto;border:1px solid var(--line);border-radius:8px}} .data-table{{border-collapse:collapse;width:100%;font-size:12px;background:white}}
.data-table th,.data-table td{{padding:7px 9px;border-bottom:1px solid #e7ebf0;white-space:nowrap;text-align:right}} .data-table th{{position:sticky;top:0;background:#edf2f8;z-index:1}} .data-table th:first-child,.data-table td:first-child{{text-align:left}}
input[type=search]{{width:min(420px,100%);padding:9px 11px;border:1px solid #cbd5e1;border-radius:7px;margin:0 0 10px}}
details summary{{cursor:pointer;font-weight:650;font-size:17px;margin-bottom:10px}} code{{background:#eef2f7;padding:2px 5px;border-radius:4px}} .legend{{display:flex;gap:18px;flex-wrap:wrap}} .dot{{width:10px;height:10px;border-radius:50%;display:inline-block;margin-right:6px}}
@media(max-width:900px){{.grid{{grid-template-columns:1fr}}.wrap{{padding:14px}}}}
</style>
</head>
<body><main class="wrap">
<h1>BTC/ETH 4h 趋势确认 × 散户拥挤反转</h1>
<p class="muted">数据 2021-07-01 至 2026-06-30；2024-01-01 后标记为历史验证。成本 5bp/单边换手，含历史资金费，信号延迟 4h，总名义仓位不超过 100%。</p>
<div class="notice"><strong>请先看：</strong>20% 与 24% 是同一个信号的两个风险档，不是两个可以分散风险的独立策略。图表是历史模拟，不是未来收益保证。</div>

<section class="card"><h2>一眼看懂</h2>{summary_html}</section>
<section class="card"><h2>仓位、杠杆与订单口径</h2>
<p>回测资金净值从 <code>1.0</code> 起步，资金曲线只是按 <strong>10,000 USDT</strong> 等比展示。BTC 和 ETH 的绝对仓位权重之和不超过 100% 净值，因此回测的<strong>最大有效杠杆是 1.0倍</strong>；20%/24% 是目标年化波动率，不是 20倍/24倍杠杆。</p>
<p class="muted"><code>campaign</code> 是一段连续同方向持仓。回测每 4h 重算目标权重；仓位数量变动也计一个调仓订单，所以“实际订单数”远高于“方向性开平仓”。一次反手同时计一次旧仓退出和新仓进入，但交易所可用一张净额订单完成。</p>
{_html_table(order_summary_display, 'order-summary-table', ('one_way_turnover_per_month','mean_gross_exposure','mean_active_gross_exposure','max_gross_exposure'), ('entries_per_month','exits_per_month','entry_exit_orders_per_month','all_order_events_per_month','median_holding_days','mean_holding_days','p90_holding_days','max_holding_days'))}
</section>
<section class="card chart"><h2>资金曲线</h2><img src="{chart_equity}" alt="equity curve"><p class="muted">虚线是 2024-01-01。对数坐标防止后期金额大时视觉失真。</p></section>
<section class="card chart"><h2>回撤曲线</h2><img src="{chart_drawdown}" alt="drawdown curve"><p class="muted">这是每日收盘回撤；4h 收盘最大回撤见顶部汇总表。真实盘中和成交冲击可能更差。</p></section>

<div class="grid">
  <section class="card chart"><h2>月收益</h2><img src="{chart_monthly}" alt="monthly returns"></section>
  <section class="card chart"><h2>周收益分布</h2><img src="{chart_weekly}" alt="weekly distribution"></section>
  <section class="card chart"><h2>滚动一年收益</h2><img src="{chart_rolling}" alt="rolling return"></section>
  <section class="card chart"><h2>持仓时长</h2><img src="{chart_holding}" alt="holding duration"></section>
</div>
<section class="card chart"><h2>4h 实际持仓</h2><img src="{chart_positions}" alt="positions"><p class="muted">正值为多，负值为空；两档的交易方向相同，仓位幅度不同。</p></section>
<section class="card chart"><h2>月度 PnL 来源</h2><img src="{chart_attribution}" alt="monthly pnl attribution"><p class="muted">价格、资金费和换手成本是可加的组合贡献，与月度复利收益会有小差异。</p></section>

<section class="card"><details open><summary>自然年收益表</summary><div class="scroll">{_html_table(annual_display, 'annual-table', ('calendar_return','annualised_if_partial'))}</div></details></section>
<section class="card"><details open><summary>逐月收益表</summary><input type="search" id="monthly-search" placeholder="搜索年月、风险档或阶段" oninput="filterTable('monthly-search','monthly-table')"><div class="scroll">{_html_table(monthly_display, 'monthly-table', ('return',))}</div></details></section>
<section class="card"><details open><summary>逐月开平仓与调仓订单</summary><input type="search" id="order-search" placeholder="搜索年月或风险档" oninput="filterTable('order-search','monthly-order-table')"><div class="scroll">{_html_table(monthly_order_display, 'monthly-order-table', ('one_way_turnover','mean_gross_exposure','max_gross_exposure','active_bar_share'))}</div></details></section>
<section class="card"><details><summary>逐周收益表</summary><input type="search" id="weekly-search" placeholder="搜索日期、风险档或阶段" oninput="filterTable('weekly-search','weekly-table')"><div class="scroll">{_html_table(weekly_display, 'weekly-table', ('return',))}</div></details></section>
<section class="card"><details><summary>完整持仓段/交易表</summary><p class="muted"><code>net_contribution</code> 是对组合净值的贡献，不是按单笔最大仓位归一化的币价涨跌。数据末尚未退出的持仓会标记 <code>open_at_data_end</code>。</p><input type="search" id="trade-search" placeholder="搜索 BTC、ETH、long、short、win、loss 或日期" oninput="filterTable('trade-search','trade-table')"><div class="scroll">{_html_table(trade_display, 'trade-table', ('entry_weight','average_abs_weight','maximum_abs_weight','gross_contribution','cost','net_contribution'), ('holding_days',))}</div></details></section>

<section class="card"><h2>CSV 原始数据</h2><ul>
<li><code>weekly_returns_comparison.csv</code>：逐周收益和完整周标记。</li>
<li><code>monthly_returns_comparison.csv</code>：逐月收益和完整月标记。</li>
<li><code>annual_returns_comparison.csv</code>：自然年收益。</li>
<li><code>daily_equity_comparison.csv</code>：日收益、净值、回撤、日末仓位和平均暴露。</li>
<li><code>trade_log_comparison.csv</code>：全部连续同方向持仓段。</li>
<li><code>monthly_order_activity.csv</code>：逐月方向性开平仓、调仓订单、换手和暴露。</li>
</ul></section>
</main>
<script>
function filterTable(inputId, tableId){{
  const query=document.getElementById(inputId).value.toLowerCase();
  const rows=document.querySelectorAll('#'+tableId+' tbody tr');
  rows.forEach(row=>{{row.style.display=row.innerText.toLowerCase().includes(query)?'':'none';}});
}}
</script>
</body></html>"""
    output = OUT / "backtest_dashboard.html"
    output.write_text(html, encoding="utf-8")
    return output


def main() -> None:
    output = build_dashboard()
    print(output)


if __name__ == "__main__":
    main()
