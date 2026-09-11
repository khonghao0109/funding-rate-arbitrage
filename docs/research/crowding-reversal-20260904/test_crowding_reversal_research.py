import numpy as np
import pandas as pd

from crowding_reversal_research import (
    _funding_by_offset,
    StrategyConfig,
    apply_trend_confirmation,
    benchmark_returns,
    causal_weights,
    crowding_scores,
    ensemble_targets,
    hysteresis_signal,
    invert_path,
    trade_episodes,
)


def synthetic_panel(n=500):
    index = pd.date_range("2022-01-01", periods=n, freq="4h", tz="UTC")
    data = {}
    for symbol, shift in (("BTC", 0.0), ("ETH", 0.2)):
        data[(symbol, "close")] = 100 * np.exp(
            np.cumsum(0.001 * np.sin(np.arange(n) / 11 + shift))
        )
        data[(symbol, "count_long_short_ratio")] = np.exp(
            0.2 * np.sin(np.arange(n) / 17 + shift)
        )
        data[(symbol, "sum_open_interest_value")] = 1e9
    return pd.DataFrame(data, index=index)


def test_scores_and_signal_are_prefix_causal():
    panel = synthetic_panel()
    cut = 400
    first = crowding_scores(panel.iloc[:cut], 90)
    changed = panel.copy()
    for symbol in ("BTC", "ETH"):
        changed.loc[changed.index[cut]:, (symbol, "count_long_short_ratio")] *= 9
    second = crowding_scores(changed, 90).iloc[:cut]
    pd.testing.assert_frame_equal(first, second)
    pd.testing.assert_frame_equal(
        hysteresis_signal(first, 1.0, 0.25),
        hysteresis_signal(second, 1.0, 0.25),
    )


def test_weights_are_prefix_causal_and_bounded():
    panel = synthetic_panel()
    close = pd.DataFrame({s: panel[(s, "close")] for s in ("BTC", "ETH")})
    signal = hysteresis_signal(crowding_scores(panel, 90), 1.0, 0.25)
    first = causal_weights(close.iloc[:400], signal.iloc[:400], 0.24, 1.0, 90)
    changed = close.copy()
    changed.iloc[400:] *= 3
    second = causal_weights(changed, signal, 0.24, 1.0, 90).iloc[:400]
    pd.testing.assert_frame_equal(first, second)
    assert (first.abs().sum(axis=1) <= 1.0 + 1e-12).all()


def test_missing_ratio_fails_closed_without_future_fill():
    panel = synthetic_panel(250)
    score = crowding_scores(panel, 90)
    score.loc[score.index[200], "BTC"] = np.nan
    signal = hysteresis_signal(score, 1.0, 0.25)
    assert signal.loc[score.index[200], "BTC"] == 0.0


def test_invalid_hysteresis_rejected():
    score = pd.DataFrame(
        {"BTC": [0.0, 2.0], "ETH": [0.0, -2.0]},
        index=pd.date_range("2024-01-01", periods=2, freq="4h", tz="UTC"),
    )
    try:
        hysteresis_signal(score, 1.0, 1.0)
    except ValueError:
        pass
    else:
        raise AssertionError("invalid exit threshold must fail")


def test_ensemble_is_prefix_causal_and_bounded():
    panel = synthetic_panel(500)
    config = StrategyConfig(target_vol=0.24)
    members = ((90, 1.0, 0.25), (90, 1.25, 0.50))
    first = ensemble_targets(panel.iloc[:400], config, members)[0]
    changed = panel.copy()
    for symbol in ("BTC", "ETH"):
        changed.loc[changed.index[400]:, (symbol, "close")] *= 2
        changed.loc[
            changed.index[400]:, (symbol, "count_long_short_ratio")
        ] *= 5
    second = ensemble_targets(changed, config, members)[0].iloc[:400]
    pd.testing.assert_frame_equal(first, second)
    assert (first.abs().sum(axis=1) <= 1.0 + 1e-12).all()


def test_benchmark_is_true_buy_and_hold_not_periodically_rebalanced():
    panel = synthetic_panel(20)
    result = benchmark_returns(panel, fee=0.0)
    close = pd.DataFrame({s: panel[(s, "close")] for s in ("BTC", "ETH")})
    expected_equity = close.div(close.iloc[0]).mean(axis=1).resample("1D").last()
    expected = expected_equity.pct_change(fill_method=None).fillna(0.0)
    pd.testing.assert_series_equal(result, expected)


def test_episode_costs_include_entry_resize_and_exit():
    index = pd.date_range("2024-01-01", periods=5, freq="4h", tz="UTC")
    path = pd.DataFrame(index=index)
    path["position_BTC"] = [0.0, 0.2, 0.3, 0.0, 0.0]
    path["position_ETH"] = 0.0
    for symbol in ("BTC", "ETH"):
        path[f"price_pnl_{symbol}"] = 0.0
        path[f"funding_pnl_{symbol}"] = 0.0
    episodes = trade_episodes(path, fee=0.001)
    assert len(episodes) == 1
    # 0.2 entry + 0.1 resize + 0.3 exit, all at 10bp.
    assert np.isclose(episodes.iloc[0].cost, 0.0006)


def test_funding_millisecond_timestamp_belongs_to_settlement_bucket():
    grouped = _funding_by_offset(0)["BTC"]
    raw = pd.read_parquet("btcusdt_funding.parquet").iloc[1]
    settlement = pd.to_datetime(raw.calc_time, unit="ms", utc=True).round("h")
    assert settlement == pd.Timestamp("2021-07-01T08:00:00Z")
    assert np.isclose(grouped.loc[settlement], raw.last_funding_rate)


def test_trend_confirmation_is_causal_and_fails_closed_during_warmup():
    index = pd.date_range("2024-01-01", periods=1_000, freq="4h", tz="UTC")
    close = pd.DataFrame(
        {"BTC": np.arange(1.0, 1_001.0), "ETH": np.arange(1_000.0, 0.0, -1.0)},
        index=index,
    )
    target = pd.DataFrame({"BTC": 0.5, "ETH": -0.5}, index=index)
    confirmed = apply_trend_confirmation(close, target, (30, 60, 120), 2)
    # Two votes are required, so 30d alone is insufficient; 30d+60d can act.
    assert (confirmed.iloc[: 60 * 6] == 0.0).all().all()
    assert confirmed.iloc[-1].equals(target.iloc[-1])


def test_direction_flip_keeps_cost_and_negates_directional_pnl():
    index = pd.date_range("2024-01-01", periods=3, freq="4h", tz="UTC")
    path = pd.DataFrame(index=index)
    for symbol in ("BTC", "ETH"):
        path[f"score_{symbol}"] = 1.0
        path[f"signal_{symbol}"] = 1.0
        path[f"target_{symbol}"] = 0.1
        path[f"position_{symbol}"] = 0.1
        path[f"price_pnl_{symbol}"] = 0.002
        path[f"funding_pnl_{symbol}"] = -0.0001
        path[f"cost_{symbol}"] = 0.0002
    path["price_pnl"] = 0.004
    path["funding_pnl"] = -0.0002
    path["cost"] = 0.0004
    path["turnover"] = 0.4
    path["net"] = path.price_pnl + path.funding_pnl - path.cost
    path["equity"] = (1.0 + path.net).cumprod()
    inverse = invert_path(path)
    assert inverse.cost.equals(path.cost)
    assert inverse.turnover.equals(path.turnover)
    assert inverse.price_pnl.equals(-path.price_pnl)
    assert np.allclose(inverse.net, -path.price_pnl - path.funding_pnl - path.cost)
