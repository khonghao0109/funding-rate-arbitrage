#!/usr/bin/env python3
"""Walk-forward over cmd/backtest sweeps: choose on one window, report on another.

Every earlier grid in docs/reports/ ranked parameter sets on the window they
were chosen on, which is how a set gets called "best" for being lucky. With
-from/-to (2026-09-09) the same grid can be replayed on calendar windows,
and this script does the one thing a grid page cannot: it takes the set the
TRAIN window would have picked and reads its result on the TEST window, next
to hold-through and next to the test window's own in-sample best — the
in-sample bound the reader should not expect to reach.

Inputs are pairs of sweep CSVs per window (selection off + selection on,
the same 15-axis grid). Ranking is on the mean total return on CAPITAL per
series, over a NAMED universe: every replayed series, or only those whose
corpus covers the window (covered_days >= 95% of window_days), because a
mean over 93-day and 730-day corpora ranks noise. Nothing here recomputes a
rule: every number is a column Go wrote.
"""
import argparse
import collections
import json
import statistics
import sys

import hold

SHOW = ("min_rate_per_8h_bps", "persistence_periods", "exit_negative_cum_cost_frac",
        "min_hold_recovered_cost_frac", "min_trailing_mean_bps", "trailing_mean_days", "trailing_mean_min_cost_frac")


def load_window(paths, full_only):
    """runs rows of one window, keyed by parameter set → {series key: row}."""
    runs = []
    for p in paths:
        r, _ = hold.load_runs(p)
        runs.extend(r)
    if not runs:
        sys.exit(f"{paths}: no successful run")
    win_days = float(runs[0]["window_days"])
    by_set = collections.defaultdict(dict)
    for r in runs:
        if full_only and float(r["covered_days"]) < 0.95 * win_days:
            continue
        by_set[hold.key_of(r)][f"{r['symbol']}|{r['perp_source']}"] = r
    # every set must cover the same series, or the means are not comparable
    full = max(len(v) for v in by_set.values())
    by_set = {k: v for k, v in by_set.items() if len(v) == full}
    return {"from_ms": int(runs[0]["window_from_ms"]), "to_ms": int(runs[0]["window_to_ms"]),
            "window_days": win_days, "capital_per_notional": float(runs[0]["capital_per_notional_frac"]),
            "series": full, "sets": by_set}


def set_stats(rows):
    caps = [float(r["total_return_on_capital_frac"]) * 100 for r in rows.values()]
    dds = [float(r["max_drawdown_frac"]) * 100 / float(r["capital_per_notional_frac"]) for r in rows.values()]
    return {"mean_cap_pct": statistics.mean(caps), "positive": sum(1 for c in caps if c > 0), "series": len(caps),
            "trades_per_series": statistics.mean(int(r["trades"]) for r in rows.values()),
            "mean_dd_cap_pct": statistics.mean(dds), "worst_cap_pct": min(caps),
            "mean_apr_cap_pct": statistics.mean(float(r["realized_apr_on_capital_frac"]) * 100 for r in rows.values())}


def ranked(win):
    rows = []
    for k, v in win["sets"].items():
        s = set_stats(v)
        s.update(dict(zip(hold.AXES, k)))
        s["key"] = k
        rows.append(s)
    rows.sort(key=lambda r: -r["mean_cap_pct"])
    for i, r in enumerate(rows):
        r["rank"] = i + 1
    return rows


def hold_through_of(db, win, ship):
    """hold-through per series of this window, from the corpus (hold.corpus),
    read with today's stamp because the corpus was backfilled."""
    import time
    any_set = next(iter(win["sets"].values()))
    series_cost = {(r["symbol"], r["perp_source"]): float(r["round_trip_cost_pct"]) / 100 for r in any_set.values()}
    corp = hold.corpus(db, series_cost, win["from_ms"], win["to_ms"], ship["min_rate_per_8h_bps"],
                       int(ship["persistence_periods"]), recorded_by_ms=int(time.time() * 1000))
    cap = win["capital_per_notional"]
    ht = {f"{c['symbol']}|{c['perp']}": c["hold_through_net_frac"] * 100 / cap for c in corp}
    keys = [k for k in any_set if k in ht]
    return {"mean_cap_pct": statistics.mean(ht[k] for k in keys) if keys else None, "series": len(keys),
            "positive": sum(1 for k in keys if ht[k] > 0), "per_series": {k: ht[k] for k in keys}}


def describe(r):
    return {a: r[a] for a in SHOW}


def marginals(rows):
    """Per axis value: mean / best / positive-share of the sets carrying it.
    hold.marginals wants the per-trade columns a full ledger provides; this
    page ranks runs only, so it carries its own lighter version."""
    out = {}
    for a in hold.AXES:
        vals = sorted({r[a] for r in rows})
        if len(vals) < 2:
            continue
        out[a] = []
        for v in vals:
            g = [r for r in rows if abs(r[a] - v) < 1e-9]
            out[a].append({"value": v, "n": len(g), "mean_cap_pct": statistics.mean(r["mean_cap_pct"] for r in g),
                           "best_cap_pct": max(r["mean_cap_pct"] for r in g),
                           "trades_per_series": statistics.mean(r["trades_per_series"] for r in g),
                           "positive": statistics.mean(r["positive"] for r in g)})
    return out


def walk(train, test, ht_test, ship, top_n=5):
    tr, te = ranked(train), ranked(test)
    te_by = {r["key"]: r for r in te}
    chosen = tr[0]
    on_test = te_by.get(chosen["key"])
    ship_key = next((k for k in test["sets"] if hold.same(k, ship)), None)
    ship_test = te_by.get(ship_key) if ship_key else None
    ship_train = next((r for r in tr if hold.same(r["key"], ship)), None)
    top_on_test = [te_by[r["key"]] for r in tr[:top_n] if r["key"] in te_by]
    return {
        "train_sets": len(tr), "test_sets": len(te), "train_series": train["series"], "test_series": test["series"],
        "chosen": {**describe(chosen), "train": {k: chosen[k] for k in ("rank", "mean_cap_pct", "positive", "series", "trades_per_series", "mean_dd_cap_pct")}},
        "chosen_on_test": None if on_test is None else {k: on_test[k] for k in ("rank", "mean_cap_pct", "positive", "series", "trades_per_series", "mean_dd_cap_pct", "mean_apr_cap_pct")},
        "top5_train_on_test_mean": statistics.mean(r["mean_cap_pct"] for r in top_on_test) if top_on_test else None,
        "test_best": {**describe(te[0]), "mean_cap_pct": te[0]["mean_cap_pct"], "trades_per_series": te[0]["trades_per_series"]},
        "test_median_cap_pct": statistics.median(r["mean_cap_pct"] for r in te),
        "test_hold_through": ht_test,
        "test_sets_beating_hold_through": sum(1 for r in te if ht_test["mean_cap_pct"] is not None and r["mean_cap_pct"] > ht_test["mean_cap_pct"]),
        "applied_on_train": None if ship_train is None else {k: ship_train[k] for k in ("rank", "mean_cap_pct")},
        "applied_on_test": None if ship_test is None else {k: ship_test[k] for k in ("rank", "mean_cap_pct", "positive", "series", "trades_per_series", "mean_apr_cap_pct")},
        "train_top": [{**describe(r), "mean_cap_pct": r["mean_cap_pct"], "on_test": te_by[r["key"]]["mean_cap_pct"] if r["key"] in te_by else None,
                       "test_rank": te_by[r["key"]]["rank"] if r["key"] in te_by else None} for r in tr[:10]],
        "axes_train": marginals(tr),
        "axes_test": marginals(te),
    }


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", required=True)
    ap.add_argument("--config", required=True)
    ap.add_argument("--window", action="append", required=True, help="NAME=runs1.csv,runs2.csv (the same grid, e.g. selection off + on)")
    ap.add_argument("--pair", action="append", required=True, help="TRAIN:TEST window names (repeatable)")
    ap.add_argument("--universe", choices=("all", "full"), default="full", help="rank on every replayed series, or only those whose corpus covers the window")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    import sqlite3
    ship = hold.shipped_set(args.config)
    db = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    wins = {}
    for w in args.window:
        name, paths = w.split("=", 1)
        wins[name] = load_window(paths.split(","), args.universe == "full")
        print(f"window {name}: {len(wins[name]['sets'])} sets × {wins[name]['series']} series", file=sys.stderr)
    hts = {name: hold_through_of(db, win, ship) for name, win in wins.items()}
    out = {"universe": args.universe, "shipped": ship, "windows": {}, "pairs": []}
    for name, win in wins.items():
        rows = ranked(win)
        out["windows"][name] = {"from_ms": win["from_ms"], "to_ms": win["to_ms"], "window_days": win["window_days"],
                                "series": win["series"], "sets": len(rows), "hold_through": hts[name],
                                "best": {**describe(rows[0]), "mean_cap_pct": rows[0]["mean_cap_pct"], "trades_per_series": rows[0]["trades_per_series"]},
                                "median_cap_pct": statistics.median(r["mean_cap_pct"] for r in rows),
                                "applied": next(({"rank": r["rank"], "mean_cap_pct": r["mean_cap_pct"], "trades_per_series": r["trades_per_series"], "positive": r["positive"]} for r in rows if hold.same(r["key"], ship)), None),
                                "beating_hold_through": sum(1 for r in rows if hts[name]["mean_cap_pct"] is not None and r["mean_cap_pct"] > hts[name]["mean_cap_pct"]),
                                "axes": marginals(rows)}
    for p in args.pair:
        a, b = p.split(":", 1)
        print(f"walk-forward {a} → {b}", file=sys.stderr)
        out["pairs"].append({"train": a, "test": b, **walk(wins[a], wins[b], hts[b], ship)})
    json.dump(out, open(args.out, "w", encoding="utf-8"), ensure_ascii=False)
    print(f"wrote {args.out}", file=sys.stderr)


if __name__ == "__main__":
    main()
