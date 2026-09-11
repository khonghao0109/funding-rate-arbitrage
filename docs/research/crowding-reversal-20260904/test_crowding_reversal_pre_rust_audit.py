import numpy as np
import pandas as pd

from crowding_reversal_pre_rust_audit import PREFIX_TIMES
from crowding_reversal_research import (
    StrategyConfig,
    ensemble_targets,
    load_market_panel,
    simulate_ensemble,
)
from crowding_reversal_signal import latest_decision


def test_paper_signal_matches_frozen_research_target_at_historical_cutoffs():
    panel = load_market_panel(0)
    config = StrategyConfig()
    for raw_timestamp in PREFIX_TIMES[-3:]:
        timestamp = pd.Timestamp(raw_timestamp)
        prefix = panel.loc[:timestamp]
        expected = ensemble_targets(prefix, config)[0].iloc[-1]
        actual = latest_decision(timestamp)["targets_fraction_of_equity"]
        for symbol in ("BTC", "ETH"):
            assert np.isclose(actual[symbol], expected[symbol], atol=1e-14)


def test_frozen_path_has_exact_one_bar_execution_lag_and_accounting_identity():
    panel = load_market_panel(0)
    path = simulate_ensemble(panel, StrategyConfig())
    for symbol in ("BTC", "ETH"):
        expected = path[f"target_{symbol}"].shift(1).fillna(0.0)
        pd.testing.assert_series_equal(
            path[f"position_{symbol}"], expected, check_names=False
        )
    expected_net = path.price_pnl + path.funding_pnl - path.cost
    pd.testing.assert_series_equal(path.net, expected_net, check_names=False)
    gross = path[["position_BTC", "position_ETH"]].abs().sum(axis=1)
    assert gross.max() <= 1.0 + 1e-12
