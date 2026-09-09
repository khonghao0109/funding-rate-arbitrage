#!/usr/bin/env python3
"""The threshold set the operator APPLIED on 2026-09-09, replayed as shipped.

The capital-and-risk grid of that day proposed basis 2.0 / 2.0 with the
minimum-hold floor at 1, and the operator put it in config.yaml (commit
0773a73). This page reads what cmd/backtest wrote for that block — the plain
path, so the set under test IS the file — and answers, in order:

  1. What it makes per series on CAPITAL against the hold-through benchmark,
     on 12 / 6 / 3 months.
  2. What the change BOUGHT: the same universe replayed with the block it
     replaced, and with each of the two changed exits undone one at a time,
     every comparison a separate plain run diffed here (rule 8: nothing in
     Python recomputes a rule Go ran).
  3. Whether the grid around the applied set — now including the basis and
     minimum-hold axes — still points at it.

Everything a window needs is expand.py's; this module adds the comparison
list and the sign convention the page reads ("applied minus other").
"""
import argparse
import json
import sqlite3
import sys

import expand
import hold


def compare(key, labels, runs_path, trades_path, w):
    """One plain run of the same universe with ONE named difference, diffed
    against the window's applied run. expand.isolation reports other − applied
    ("off − on"); this page asks what the applied set has OVER the other, so
    the sign is flipped once, here, and the JSON says so in the field name."""
    iso = expand.isolation(runs_path, trades_path, w, labels.get("vi", key))
    for r in iso["series"]:
        r["delta_pct"] = -r["delta_pct"]
    for c in iso["cohorts"].values():
        c["delta_pct"] = -c["delta_pct"]
    runs, refused = hold.load_runs(runs_path)
    if not runs:
        sys.exit(f"{runs_path}: no successful run")
    keys = {hold.key_of(r) for r in runs}
    if len(keys) != 1:
        sys.exit(f"{runs_path}: {len(keys)} parameter sets — a comparison run is ONE plain run")
    params = dict(zip(hold.AXES, next(iter(keys))))
    cap = w["capital_per_notional"]
    other = [float(r["total_return_on_capital_frac"]) * 100 for r in runs]
    iso.update({
        "key": key,
        "label_vi": labels.get("vi", key), "label_zh": labels.get("zh", labels.get("vi", key)),
        "params": params,
        "delta_sign": "applied_minus_other",
        "series_other": len(runs), "positive_other": sum(1 for v in other if v > 0),
        "mean_other_cap_pct": sum(other) / len(other),
        "trades_per_series_other": sum(int(r["trades"]) for r in runs) / len(runs),
        "mean_dd_other_cap_pct": sum(float(r["max_drawdown_frac"]) * 100 / cap for r in runs) / len(runs),
        "refused_other": len(refused),
    })
    return iso


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", required=True)
    ap.add_argument("--config", required=True, help="config.yaml; its strategy block IS the applied set")
    ap.add_argument("--window", action="append", required=True, help="NAME=runs.csv:trades.csv (repeatable)")
    ap.add_argument("--compare", action="append", default=[],
                    help="WINDOW:KEY=runs.csv:trades.csv — a plain run of the same universe with ONE named difference")
    ap.add_argument("--label", action="append", default=[],
                    help="KEY=vi text|zh text — what the --compare run changed; the CSV's parameter columns say the set, not the reason")
    ap.add_argument("--sweep", help="runs.csv:trades.csv of the grid run on the same universe")
    ap.add_argument("--sweep-window", default="12", help="which window the sweep covers")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    labels = {}
    for item in args.label:
        key, text = item.split("=", 1)
        vi, _, zh = text.partition("|")
        labels[key] = {"vi": vi.strip(), "zh": zh.strip() or vi.strip()}

    ship = hold.shipped_set(args.config)
    facts = expand.source_facts(args.config)
    db = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    out = {"shipped": ship, "sources": facts, "baseline_symbols": list(expand.BASELINE),
           "windows": {}, "window_order": [], "axes": hold.AXES}
    for w in args.window:
        name, paths = w.split("=", 1)
        runs_path, trades_path = paths.split(":", 1)
        out["windows"][name] = expand.window(name, runs_path, trades_path, db, ship, facts)
        out["windows"][name]["compares"] = []
        out["window_order"].append(name)
    for c in args.compare:
        target, paths = c.split("=", 1)
        wname, key = target.split(":", 1)
        runs_path, trades_path = paths.split(":", 1)
        if wname not in out["windows"]:
            sys.exit(f"--compare {target}: no such window")
        if key not in labels:
            sys.exit(f"--compare {target}: no --label {key}=... says what that run changed")
        print(f"compare {wname}:{key}", file=sys.stderr)
        out["windows"][wname]["compares"].append(compare(key, labels[key], runs_path, trades_path, out["windows"][wname]))
    if args.sweep:
        runs_path, trades_path = args.sweep.split(":", 1)
        W = out["windows"].get(args.sweep_window)
        ht = W["cohorts"]["all"]["ht"]["mean_cap_pct"] if W else None
        out["sweep"] = expand.sweep(runs_path, trades_path, ship, ht)
        out["sweep"]["window"] = args.sweep_window
        out["sweep"]["ht_mean_cap_pct"] = ht
    json.dump(out, open(args.out, "w", encoding="utf-8"), ensure_ascii=False)
    print(f"wrote {args.out}", file=sys.stderr)


if __name__ == "__main__":
    main()
