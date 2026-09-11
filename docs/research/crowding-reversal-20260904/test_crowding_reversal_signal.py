import pandas as pd

from crowding_reversal_signal import latest_decision


def test_latest_decision_is_bounded_and_reproducible_as_of():
    result = latest_decision("2024-06-30T20:00:00Z", target_vol=0.24)
    targets = result["targets_fraction_of_equity"]
    assert result["decision_time_utc"] == "2024-06-30T20:00:00+00:00"
    assert result["gross_target"] <= 1.0 + 1e-12
    assert set(targets) == {"BTC", "ETH"}
    assert sum(result["member_votes"]["BTC"].values()) == 12
    assert 0 <= result["trend_confirmation"]["votes"]["BTC"] <= 3
    assert pd.Timestamp(result["intended_holding_interval_utc"]["to"]) > pd.Timestamp(
        result["intended_holding_interval_utc"]["from"]
    )
