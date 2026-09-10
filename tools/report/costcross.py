#!/usr/bin/env python3
"""Rule 1 — series selection on the series' OWN cost-crossing (2026-09-10).

Regularity 2 of the three-year study (regime-3y-2026-09-09) said the level
at which a series pays for its round trip is that series' own priced cost
divided by the settlements a hold crosses — 0.33 bps/8h at BTC/ETH, 0.8 at
NEAR — not one number for every series. strategy.Params gained
TrailingMeanMinCostFrac for it (config trailing_mean_min_cost_frac, sweep
axis -trail-cost), and this page reads what cmd/backtest wrote for it: the
applied set with the selection OFF, with the cost-crossing at 0.5 / 1.0 /
1.5 / 2.0 round trips over 30 / 60 / 90 days, and beside it the ABSOLUTE
floor (min_trailing_mean_bps 0.3 / 0.5 / 0.8 over 30 / 90 days) — on pinned
calendar windows of the three-year corpus copy (cmd/backtest -from/-to).

Two denominators, kept apart on purpose: the UNIVERSE (every replayed
series; a slot the rule refused earns 0) and DEPLOYED capital (only the
series the set entered). A selection rule can only ever lower the first and
raise the second; what it buys is which series are NOT traded, so every set
also lists the series it kept out and what the unselected run made on them.
The walk-forward half chooses a set on one window and reads it on another
(forward.walk). Read-only, like everything here (CLAUDE.md rule 8): every
profit figure is a column Go wrote, the corpus benchmark is hold.corpus, and
the daily portfolio curve is capital.daily_portfolio over Go's own trades.
"""
import argparse
import json
import os
import sqlite3
import statistics
import sys
import tempfile
import time

import capital
import expand
import forward
import hold

COST_AXIS, DAYS_AXIS, ABS_AXIS = "trailing_mean_min_cost_frac", "trailing_mean_days", "min_trailing_mean_bps"


def series_rates(db, sym, src, from_ms, to_ms, recorded_by_ms):
    """capital.series_rates with the recording bound as a parameter: the corpus
    was backfilled in 2026, so a 2024 window read with its own end as the
    bound is empty (the recorded_at_ms trap, tools/report/README.md)."""
    return db.execute("""SELECT funding_at_ms, rate_per_interval_frac FROM funding_history
                         WHERE source=? AND symbol=? AND model='discrete' AND rate_type<>'Special'
                           AND funding_at_ms>=? AND funding_at_ms<? AND recorded_at_ms<=? ORDER BY funding_at_ms""",
                      (src, sym, from_ms, to_ms, recorded_by_ms)).fetchall()


def kind_of(params):
    if params[COST_AXIS] > 0:
        return "cost"
    if params[ABS_AXIS] > 0:
        return "abs"
    return "off"


def window(name, runs_paths, trades_paths, db, ship, facts, full_only, tmpdir, now_ms):
    win = forward.load_window(runs_paths, full_only)
    ht = forward.hold_through_of(db, win, ship)
    cap = win["capital_per_notional"]
    from_ms, to_ms = win["from_ms"], win["to_ms"]
    keys = list(win["sets"])
    series_keys = sorted(next(iter(win["sets"].values())))
    n_series, n_days = len(series_keys), int((to_ms - from_ms) // capital.DAY_MS) + 1

    print(f"window {name}: {len(keys)} sets × {n_series} series; trades", file=sys.stderr)
    trades_path = capital.concat_csv(trades_paths, os.path.join(tmpdir, f"trades-{name}.csv"))
    _, ledgers, _ = hold.stream_trades(trades_path, set(keys))
    rates_by = {k: capital.SeriesRates(series_rates(db, *k.split("|"), from_ms, to_ms, now_ms), from_ms, n_days)
                for k in series_keys}
    pf = {k: capital.daily_portfolio(ledgers.get(k, []), rates_by, from_ms, n_days, n_series, cap) for k in keys}

    # The baseline is THE applied set (config.yaml's block, selection off),
    # found by its full parameter tuple, and there must be exactly one
    # selection-off set: a second one (a grid on another axis, a re-run
    # concatenated in) would make every delta below relative to an unnamed
    # baseline.
    off_keys = [k for k in keys if kind_of(dict(zip(hold.AXES, k))) == "off"]
    if len(off_keys) != 1:
        sys.exit(f"window {name}: {len(off_keys)} selection-off sets — the page needs exactly one, the applied set")
    off_key = off_keys[0]
    if not hold.same(off_key, ship):
        sys.exit(f"window {name}: the selection-off set is not config.yaml's strategy block: {dict(zip(hold.AXES, off_key))}")
    off_rows = win["sets"][off_key]
    off_cap = {sk: float(r["total_return_on_capital_frac"]) * 100 for sk, r in off_rows.items()}

    # The corpus view of every series, for the per-series table.
    corp_by = {}
    series_cost = {tuple(sk.split("|")): float(r["round_trip_cost_pct"]) / 100 for sk, r in off_rows.items()}
    for c in hold.corpus(db, series_cost, from_ms, to_ms, ship["min_rate_per_8h_bps"], int(ship["persistence_periods"]),
                         recorded_by_ms=now_ms):
        corp_by[f"{c['symbol']}|{c['perp']}"] = c

    sets = []
    for k in keys:
        rows = win["sets"][k]
        params = dict(zip(hold.AXES, k))
        s = forward.set_stats(rows)
        traded = {sk for sk, r in rows.items() if int(r["trades"]) > 0}
        excluded = sorted(sk for sk in off_rows if int(off_rows[sk]["trades"]) > 0 and sk not in traded)
        deployed = [float(rows[sk]["total_return_on_capital_frac"]) * 100 for sk in traded]
        hton = [ht["per_series"][sk] for sk in traded if sk in ht["per_series"]]
        s.update({
            "key": "|".join(str(v) for v in k), "params": params, "kind": kind_of(params),
            "trail_cost": params[COST_AXIS], "trail_days": params[DAYS_AXIS], "trail_bps": params[ABS_AXIS],
            "traded_series": len(traded),
            "deployed_cap_pct": statistics.mean(deployed) if deployed else None,
            "ht_on_traded_cap_pct": statistics.mean(hton) if hton else None,
            # The unselected run on the SAME series this set entered: the gap
            # between this and deployed_cap_pct is what the rule cost by
            # entering LATER (it waits for the trailing window), separated
            # from what it bought by not entering the excluded ones.
            "off_on_traded_cap_pct": statistics.mean(off_cap[sk] for sk in traded) if traded else None,
            "pf_final_cap_pct": pf[k]["final_cap_pct"], "pf_dd_cap_pct": pf[k]["max_dd_cap_pct"],
            "delta_vs_off_cap_pct": s["mean_cap_pct"] - forward.set_stats(off_rows)["mean_cap_pct"],
            "excluded": excluded,
            "excluded_off_sum_cap_pct": sum(off_cap[sk] for sk in excluded),
            "excluded_off_mean_cap_pct": statistics.mean(off_cap[sk] for sk in excluded) if excluded else None,
            "excluded_negative": sum(1 for sk in excluded if off_cap[sk] < 0),
            "excluded_positive": sum(1 for sk in excluded if off_cap[sk] > 0),
            # What the selection changed on the series it KEPT: the same
            # series may still trade differently (a later entry), so the
            # rule's effect is not only the excluded list.
            "kept_delta_cap_pct": sum(float(rows[sk]["total_return_on_capital_frac"]) * 100 - off_cap[sk] for sk in traded),
        })
        sets.append(s)
    sets.sort(key=lambda r: -r["mean_cap_pct"])
    for i, r in enumerate(sets):
        r["rank"] = i + 1
    ht_pf = capital.daily_portfolio(
        [{"symbol": sk.split("|")[0], "perp": sk.split("|")[1], "open_ms": rates_by[sk].stamps[0],
          "close_ms": rates_by[sk].stamps[-1], "cost_frac": series_cost[tuple(sk.split("|"))]}
         for sk in series_keys if len(rates_by[sk].stamps) > 1], rates_by, from_ms, n_days, n_series, cap)

    series = []
    for sk in series_keys:
        sym, perp = sk.split("|")
        r, c = off_rows[sk], corp_by.get(sk)
        series.append({"key": sk, "symbol": sym, "perp": perp,
                       "quote_bridged": facts.get(perp, {}).get("quote_asset") == "USD",
                       "covered_days": float(r["covered_days"]), "cost_pct": float(r["round_trip_cost_pct"]),
                       "mean_bps": c["mean_rate_per_8h_bps"] if c else None,
                       "positive_share": c["positive_share"] if c else None,
                       "ht_cap_pct": ht["per_series"].get(sk),
                       "off_cap_pct": off_cap[sk], "off_trades": int(r["trades"]),
                       "off_dd_cap_pct": float(r["max_drawdown_frac"]) * 100 / cap})
    series.sort(key=lambda r: -r["off_cap_pct"])
    return {"from_ms": from_ms, "to_ms": to_ms, "window_days": win["window_days"], "capital_per_notional": cap,
            "series": n_series, "sets": sets, "off_key": "|".join(str(v) for v in off_key),
            "hold_through": {"mean_cap_pct": ht["mean_cap_pct"], "positive": ht["positive"], "series": ht["series"],
                             "pf_dd_cap_pct": ht_pf["max_dd_cap_pct"], "pf_final_cap_pct": ht_pf["final_cap_pct"]},
            "per_series": series, "win": win, "ht": ht}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", required=True)
    ap.add_argument("--config", required=True, help="config.yaml; its strategy block is the applied (selection-off) set")
    ap.add_argument("--window", action="append", required=True,
                    help="NAME=runs1.csv,runs2.csv:trades1.csv,trades2.csv (repeatable; the off, cost and abs runs of one window)")
    ap.add_argument("--pair", action="append", default=[], help="TRAIN:TEST window names (repeatable)")
    ap.add_argument("--universe", choices=("all", "full"), default="full",
                    help="rank on every replayed series, or only those whose corpus covers the window")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    ship = hold.shipped_set(args.config)
    facts = expand.source_facts(args.config)
    db = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    now_ms = int(time.time() * 1000)
    out = {"shipped": ship, "sources": facts, "universe": args.universe, "axes": hold.AXES,
           "window_order": [], "windows": {}, "pairs": []}
    wins = {}
    with tempfile.TemporaryDirectory() as tmp:
        for w in args.window:
            name, paths = w.split("=", 1)
            runs_paths, trades_paths = paths.split(":", 1)
            wins[name] = window(name, runs_paths.split(","), trades_paths.split(","), db, ship, facts,
                                args.universe == "full", tmp, now_ms)
            out["window_order"].append(name)
            out["windows"][name] = {k: v for k, v in wins[name].items() if k not in ("win", "ht")}
    for p in args.pair:
        a, b = p.split(":", 1)
        print(f"walk-forward {a} → {b}", file=sys.stderr)
        out["pairs"].append({"train": a, "test": b, **forward.walk(wins[a]["win"], wins[b]["win"], wins[b]["ht"], ship)})
    json.dump(out, open(args.out, "w", encoding="utf-8"), ensure_ascii=False)
    print(f"wrote {args.out}", file=sys.stderr)


if __name__ == "__main__":
    main()
