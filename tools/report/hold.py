#!/usr/bin/env python3
"""Measure the HOLD side of the strategy: how long a trade needs to break even,
and which hold-side parameters (holding_days, the decay window N, the
min-hold floor M, the sign-flip gates) keep a trade open until it has.

Three sources, all read-only (CLAUDE.md rule 8):

  1. The funding corpus in SQLite — for every settlement taken as an ENTRY,
     the days until the funding collected since then reaches the round-trip
     cost Go priced for that series. This is a MEASUREMENT of the corpus, not
     a rule: it says what any hold rule is up against.
  2. cmd/backtest's runs CSV — one row per (series, parameter set), with the
     return Go computed. Nothing here recomputes a rule.
  3. cmd/backtest's trades CSV — one row per trade, so a set can be described
     by what its trades did: how many broke even, why each one closed, how
     long it was held.

Output: ONE JSON, rendered by hold_build.py.
"""
import argparse
import collections
import csv
import json
import re
import sqlite3
import statistics
import sys
import time

AXES = ["min_rate_per_8h_bps", "persistence_periods", "min_net_apr_frac", "exit_net_apr_frac",
        "exit_persistence_periods", "exit_negative_min_bps", "exit_negative_periods",
        "exit_negative_cum_cost_frac", "min_hold_recovered_cost_frac", "notional_quote", "holding_days",
        # Written by cmd/backtest since 2026-09-09: the basis limits (until
        # then hardcoded, so every earlier CSV ran at 1.0 / 0.5) and the
        # series-selection pair (off before it existed).
        "max_basis_pct", "max_basis_widen_pct", "min_trailing_mean_bps", "trailing_mean_days",
        # Written since 2026-09-10: the cost-crossing selection (off before it existed).
        "trailing_mean_min_cost_frac"]
# Columns a CSV written before an axis existed may lack: the value the engine
# used then, never 0 when 0 would mean something else (1 negative period is
# the 3.2 rule; 0 is not a rule).
AXIS_DEFAULT = {"exit_negative_min_bps": 0.0, "exit_negative_periods": 1, "exit_negative_cum_cost_frac": 0.0,
                "min_hold_recovered_cost_frac": 0.0,
                "max_basis_pct": 1.0, "max_basis_widen_pct": 0.5, "min_trailing_mean_bps": 0.0, "trailing_mean_days": 0.0,
                "trailing_mean_min_cost_frac": 0.0}
CONFIG_KEYS = {"min_rate_per_8h_bps", "persistence_periods", "min_net_apr_frac", "exit_net_apr_frac",
               "exit_persistence_periods", "exit_negative_min_bps", "exit_negative_periods",
               "exit_negative_cum_cost_frac", "min_hold_recovered_cost_frac", "notional_quote", "holding_days",
               "max_basis_pct", "max_basis_widen_pct", "min_trailing_mean_bps", "trailing_mean_days",
               "trailing_mean_min_cost_frac"}
REASONS = ["window_end", "basis", "sign_flip", "decay", "liquidation", "unpriceable", "hedge_gone", "other"]
HORIZONS = (7, 14, 30, 60, 90, 180)


def classify(s):
    """The exit-reason class prep.py writes; a raw sentence is classified the same way."""
    if s in REASONS:
        return s
    if s.startswith("THOÁT: funding đã đảo dấu"):
        return "sign_flip"
    if s.startswith("THOÁT: cả"):
        return "decay"
    if s.startswith("THOÁT: basis"):
        return "basis"
    if s.startswith("THANH LÝ"):
        return "liquidation"
    if s.startswith("Đóng ở cuối cửa sổ"):
        return "window_end"
    if "không còn tính được APR ròng" in s:
        return "unpriceable"
    if "chân spot" in s:
        return "hedge_gone"
    return "other"


def shipped_set(config_path):
    """The strategy: block of config.yaml, line-based (no PyYAML on this machine)."""
    out, inside = {}, False
    for line in open(config_path, encoding="utf-8"):
        if re.match(r"^strategy:\s*(#.*)?$", line):
            inside = True
            continue
        if inside and re.match(r"^\S", line):
            break
        if not inside:
            continue
        m = re.match(r"^\s{2}([a-z_0-9]+):\s*([-0-9.]+)", line)
        if m and m.group(1) in CONFIG_KEYS:
            out[m.group(1)] = float(m.group(2))
    missing = CONFIG_KEYS - set(out)
    if missing:
        sys.exit(f"config.yaml strategy block lacks {sorted(missing)}")
    return out


def key_of(r):
    return tuple(float(r.get(a) if r.get(a) not in (None, "") else AXIS_DEFAULT.get(a, 0)) for a in AXES)


def same(k, s):
    return all(abs(k[i] - s[a]) < 1e-9 for i, a in enumerate(AXES))


def pctl(xs, q):
    if not xs:
        return None
    xs = sorted(xs)
    i = (len(xs) - 1) * q
    lo, hi = int(i), min(int(i) + 1, len(xs) - 1)
    return xs[lo] + (xs[hi] - xs[lo]) * (i - lo)


def mean(xs):
    return statistics.mean(xs) if xs else None


def med(xs):
    return statistics.median(xs) if xs else None


# --- 1. the corpus ---------------------------------------------------------

def horizon(ts, rates, cost, entries):
    """Days from entry k to the first settlement at which funding collected
    since k reaches cost; None when the window ends first. Collected means
    strictly after k — the position exists at stamp k and is paid from k+1,
    which is the arithmetic internal/backtest's equity curve uses."""
    n = len(ts)
    P = [0.0] * (n + 1)
    for i, r in enumerate(rates):
        P[i + 1] = P[i] + r
    out = []
    for k in entries:
        need, j, found = P[k + 1] + cost, k + 1, None
        while j < n:
            if P[j + 1] >= need:
                found = j
                break
            j += 1
        out.append((ts[found] - ts[k]) / 86400000 if found is not None else None)
    return out


def summarise(h, entries, ts, to_ms):
    ok = [x for x in h if x is not None]
    left = [(to_ms - ts[k]) / 86400000 for k in entries]
    return {
        "entries": len(h), "reached": len(ok), "censored": len(h) - len(ok),
        "p10": pctl(ok, .1), "p25": pctl(ok, .25), "median": pctl(ok, .5), "p75": pctl(ok, .75), "p90": pctl(ok, .9),
        "mean": mean(ok),
        # Share reached within D days, among entries that still had D days of
        # window left — numerator AND denominator, or a short corpus reports
        # more than 100% (it did, on the first run).
        "within": {str(d): (sum(1 for x, l in zip(h, left) if x is not None and x <= d and l >= d) /
                            max(1, sum(1 for l in left if l >= d))) for d in HORIZONS},
        "eligible": {str(d): sum(1 for l in left if l >= d) for d in HORIZONS},
    }


def corpus(db, series_cost, from_ms, to_ms, entry_bps, entry_persist, recorded_by_ms=None):
    """recorded_by_ms: only rows recorded by this stamp count. The default,
    the window's end, is the replay's own view — a row recorded after the
    window closed did not exist for it. A slice of a BACKFILLED corpus (rows
    stamped the day of the backfill, settlements years earlier) must pass the
    backfill time instead, or the filter silently empties every past year —
    the same trap that dropped the hyperliquid alts from the pair screen."""
    if recorded_by_ms is None:
        recorded_by_ms = to_ms
    out = []
    for (sym, src), cost in sorted(series_cost.items()):
        rows = db.execute("""SELECT funding_at_ms, rate_per_interval_frac, rate_per_8h_frac, interval_sec
                             FROM funding_history WHERE source=? AND symbol=? AND model='discrete'
                               AND rate_type<>'Special' AND funding_at_ms>=? AND funding_at_ms<?
                               AND recorded_at_ms<=? ORDER BY funding_at_ms""",
                          (src, sym, from_ms, to_ms, recorded_by_ms)).fetchall()
        # recorded_at_ms <= window end (by default): only rows the replay could have seen.
        if len(rows) < 2:
            continue
        ts = [r[0] for r in rows]
        rates = [r[1] for r in rows]
        r8 = [r[2] for r in rows]
        iv = rows[-1][3]
        n = len(ts)
        any_entries = list(range(n - 1))
        rule_entries = [k for k in range(entry_persist - 1, n - 1)
                        if all(r8[i] * 1e4 >= entry_bps for i in range(k - entry_persist + 1, k + 1))]
        mean_rate = statistics.mean(rates)
        # The hold-through equity curve as internal/backtest would draw it:
        # paid from the second settlement on, the round trip charged at the
        # close, and the forced close counted in the drawdown.
        eq, peak, dd = 0.0, 0.0, 0.0
        for x in rates[1:]:
            eq += x
            peak = max(peak, eq)
            dd = max(dd, peak - eq)
        eq -= cost
        dd = max(dd, peak - eq)
        out.append({
            "hold_through_dd_frac": dd,
            "symbol": sym, "perp": src, "interval_sec": iv, "settlements": n, "cost_frac": cost,
            "covered_days": (ts[-1] - ts[0]) / 86400000 + iv / 86400,
            "mean_rate_per_8h_bps": statistics.mean(r8) * 1e4,
            "positive_share": sum(1 for r in rates if r > 0) / n,
            # Enter at the first settlement, leave at the last: one round trip.
            "hold_through_net_frac": sum(rates[1:]) - cost,
            "naive_break_even_days": (cost / mean_rate) * iv / 86400 if mean_rate > 0 else None,
            "any_entry": summarise(horizon(ts, rates, cost, any_entries), any_entries, ts, to_ms),
            "rule_entry": summarise(horizon(ts, rates, cost, rule_entries), rule_entries, ts, to_ms),
        })
    return out


# --- 2. the runs ------------------------------------------------------------

def load_runs(path):
    runs, refused = [], collections.OrderedDict()
    for r in csv.DictReader(open(path, encoding="utf-8", newline="")):
        if r["ok"] != "true":
            refused.setdefault((r["symbol"], r["perp_source"]), r["reason_vi"])
            continue
        runs.append(r)
    return runs, refused


def aggregate_runs(runs):
    sets, cap = {}, None
    for r in runs:
        k = key_of(r)
        s = sets.setdefault(k, {"series": 0, "sum_cap": 0.0, "sum_notional": 0.0, "positive": 0, "trades": 0,
                                "sum_dd": 0.0, "liq": 0, "per_series": {}})
        v = float(r["total_return_on_capital_frac"])
        s["series"] += 1
        s["sum_cap"] += v
        s["sum_notional"] += float(r["total_return_frac"])
        s["sum_dd"] += float(r["max_drawdown_frac"])
        s["liq"] += int(r.get("liquidations") or 0)
        s["positive"] += 1 if v > 0 else 0
        s["trades"] += int(r["trades"])
        s["per_series"][f"{r['symbol']}|{r['perp_source']}"] = {
            "cap": v, "trades": int(r["trades"]),
            # The equity curve's peak-to-trough, which Go measured per run; on
            # capital like the return, so the two divide into one ratio.
            "dd": float(r["max_drawdown_frac"]) / float(r["capital_per_notional_frac"]),
            "pip": int(r["periods_in_position"]), "st": int(r["settlements"])}
        cap = float(r["capital_per_notional_frac"])
    return sets, cap


# --- 3. the trades ----------------------------------------------------------

def new_acc():
    return {"n": 0, "be": 0, "sum_net": 0.0, "sum_fund": 0.0, "sum_held": 0.0,
            "reasons": collections.Counter(), "be_by_reason": collections.Counter(),
            "held_by_reason": collections.defaultdict(float), "net_by_reason": collections.defaultdict(float),
            "neg_net": 0.0}


def stream_trades(path, ledger_keys):
    """One pass: per-set accumulators for every set, full rows for ledger_keys."""
    acc = collections.defaultdict(new_acc)
    ledgers = {k: [] for k in ledger_keys}
    n, t0 = 0, time.time()
    for r in csv.DictReader(open(path, encoding="utf-8", newline="")):
        n += 1
        k = key_of(r)
        a = acc[k]
        net, held, c = float(r["net_frac"]), float(r["held_days"]), classify(r["exit_reason_vi"])
        a["n"] += 1
        a["sum_net"] += net
        a["sum_fund"] += float(r["funding_frac"])
        a["sum_held"] += held
        a["reasons"][c] += 1
        a["held_by_reason"][c] += held
        a["net_by_reason"][c] += net
        if net >= 0:
            a["be"] += 1
            a["be_by_reason"][c] += 1
        else:
            a["neg_net"] += net
        if k in ledgers:
            ledgers[k].append({"symbol": r["symbol"], "perp": r["perp_source"], "open_ms": int(r["open_at_ms"]),
                               "close_ms": int(r["close_at_ms"]), "held_days": held, "settlements": int(r["settlements"]),
                               "funding_frac": float(r["funding_frac"]), "cost_frac": float(r["cost_frac"]),
                               "net_frac": net, "reason": c})
        if n % 1000000 == 0:
            print(f"  {n:,} trades, {time.time() - t0:.0f}s", file=sys.stderr)
    return acc, ledgers, n


def risk_stats(per_series):
    """Return over drawdown for one set (or the hold-through benchmark) across
    its series. The ratio is MEAN return over MEAN drawdown, never a mean of
    per-series ratios: a series with a tiny drawdown and a tiny return would
    otherwise dominate. None when nothing drew down (nothing traded)."""
    rets = [v["cap"] for v in per_series.values()]
    dds = [v["dd"] for v in per_series.values()]
    if not rets:
        return {}
    mean_ret, mean_dd, worst_dd = statistics.mean(rets), statistics.mean(dds), max(dds)
    return {"mean_dd_cap_pct": mean_dd * 100, "worst_dd_cap_pct": worst_dd * 100,
            "worst_series_cap_pct": min(rets) * 100, "std_cap_pct": statistics.pstdev(rets) * 100,
            "calmar": mean_ret / mean_dd if mean_dd > 0 else None,
            "worst_calmar": mean_ret / worst_dd if worst_dd > 0 else None,
            "exposure": statistics.mean(v["pip"] / v["st"] for v in per_series.values() if v.get("st")) if any(v.get("st") for v in per_series.values()) else None}


def set_row(k, s, a):
    n = a["n"] if a else 0
    row = dict(zip(AXES, k))
    row.update(risk_stats(s["per_series"]))
    row.update({"series": s["series"], "mean_cap_pct": s["sum_cap"] * 100 / s["series"],
                "sum_notional_pct": s["sum_notional"] * 100, "positive": s["positive"],
                "trades_per_series": s["trades"] / s["series"], "mean_dd_pct": s["sum_dd"] * 100 / s["series"],
                "liquidations": s["liq"], "trades": n,
                "be_share": a["be"] / n if n else None,
                "mean_net_pct": a["sum_net"] * 100 / n if n else None,
                "mean_fund_pct": a["sum_fund"] * 100 / n if n else None,
                "mean_held_days": a["sum_held"] / n if n else None,
                "loss_sum_pct": a["neg_net"] * 100 if n else None,
                "reasons": {c: a["reasons"].get(c, 0) for c in REASONS} if n else {},
                "yield_exit_share": (a["reasons"].get("sign_flip", 0) + a["reasons"].get("decay", 0)) / n if n else None,
                "be_by_reason": {c: a["be_by_reason"].get(c, 0) for c in REASONS} if n else {},
                "held_by_reason": {c: a["held_by_reason"][c] / a["reasons"][c] for c in a["reasons"]} if n else {},
                "net_by_reason": {c: a["net_by_reason"][c] * 100 / a["reasons"][c] for c in a["reasons"]} if n else {}})
    return row


def marginals(rows):
    out = {}
    for a in AXES:
        vals = sorted({r[a] for r in rows})
        if len(vals) < 2:
            continue
        out[a] = []
        for v in vals:
            g = [r for r in rows if abs(r[a] - v) < 1e-9]
            gt = [r for r in g if r["trades"] > 0]
            out[a].append({
                "value": v, "n": len(g), "traded": len(gt),
                "mean_cap_pct": mean([r["mean_cap_pct"] for r in g]),
                "median_cap_pct": med([r["mean_cap_pct"] for r in g]),
                "best_cap_pct": max(r["mean_cap_pct"] for r in g) if g else None,
                "be_share": mean([r["be_share"] for r in gt]),
                "flip_share": mean([r["reasons"]["sign_flip"] / r["trades"] for r in gt]),
                "decay_share": mean([r["reasons"]["decay"] / r["trades"] for r in gt]),
                "basis_share": mean([r["reasons"]["basis"] / r["trades"] for r in gt]),
                "trades_per_series": mean([r["trades_per_series"] for r in g]),
                "mean_held_days": mean([r["mean_held_days"] for r in gt]),
                "positive": mean([r["positive"] for r in g]),
            })
    return out


def heat(rows, xa, ya, stat="best"):
    """A 2-D slice: for each (x, y) the BEST (or mean) mean_cap_pct over the other axes."""
    xs, ys = sorted({r[xa] for r in rows}), sorted({r[ya] for r in rows})
    cells = []
    for y in ys:
        line = []
        for x in xs:
            g = [r for r in rows if abs(r[xa] - x) < 1e-9 and abs(r[ya] - y) < 1e-9]
            if not g:
                line.append(None)
                continue
            b = max(g, key=lambda r: r["mean_cap_pct"])
            line.append({"best": b["mean_cap_pct"], "mean": mean([r["mean_cap_pct"] for r in g]),
                         "be_share": b["be_share"], "trades_per_series": b["trades_per_series"],
                         "yield_exit_share": b["yield_exit_share"], "n": len(g)})
        cells.append(line)
    return {"x_axis": xa, "y_axis": ya, "x": xs, "y": ys, "cells": cells}


def window(name, runs_path, trades_path, db, ship, entry_bps, entry_persist):
    print(f"window {name}: runs", file=sys.stderr)
    runs, refused = load_runs(runs_path)
    if not runs:
        sys.exit(f"{runs_path}: no successful run")
    from_ms, to_ms = int(runs[0]["window_from_ms"]), int(runs[0]["window_to_ms"])
    sets, cap = aggregate_runs(runs)
    full = max(s["series"] for s in sets.values())
    keys = [k for k, s in sets.items() if s["series"] == full]
    dropped = len(sets) - len(keys)
    series_cost = {}
    for r in runs:
        if abs(float(r["notional_quote"]) - 50000) < 1:
            series_cost.setdefault((r["symbol"], r["perp_source"]), float(r["round_trip_cost_pct"]) / 100)
    print(f"window {name}: corpus horizon for {len(series_cost)} series", file=sys.stderr)
    corp = corpus(db, series_cost, from_ms, to_ms, entry_bps, entry_persist)
    ht_by = {f"{c['symbol']}|{c['perp']}": c["hold_through_net_frac"] for c in corp}
    ht_mean_cap = statistics.mean(ht_by.values()) * 100 / cap if ht_by else None
    ht_ps = {f"{c['symbol']}|{c['perp']}": {"cap": c["hold_through_net_frac"] / cap, "dd": c["hold_through_dd_frac"] / cap,
                                            "trades": 1, "pip": c["settlements"] - 1, "st": c["settlements"]} for c in corp}
    ht_risk = risk_stats(ht_ps)

    ship_key = next((k for k in keys if same(k, ship)), None)
    best_key = max(keys, key=lambda k: sets[k]["sum_cap"])
    # The shipped set with the min-hold floor at 1.0 (or the smallest value
    # >= 1 the grid has): the page diffs its ledger against the shipped one.
    m_on_key = None
    if ship_key is not None:
        cands = [k for k in keys if all(abs(k[i] - ship[a]) < 1e-9 for i, a in enumerate(AXES) if a != "min_hold_recovered_cost_frac")
                 and k[AXES.index("min_hold_recovered_cost_frac")] >= 1]
        if cands:
            m_on_key = min(cands, key=lambda k: k[AXES.index("min_hold_recovered_cost_frac")])
    ledger_keys = {k for k in (ship_key, best_key, m_on_key) if k is not None}
    print(f"window {name}: trades", file=sys.stderr)
    acc, ledgers, n_trades = stream_trades(trades_path, ledger_keys)
    rows = [set_row(k, sets[k], acc.get(k)) for k in keys]
    rows.sort(key=lambda r: -r["mean_cap_pct"])
    for i, r in enumerate(rows):
        r["rank"] = i + 1
    by_key = {key_of(r): r for r in rows}
    traded = [r for r in rows if r["trades"] > 0]

    # The set the user's question describes: the fewest yield exits, then the
    # most trades at or above break-even, then the return — the three ranked
    # in the order the question puts them.
    clean = sorted([r for r in traded if r["yield_exit_share"] <= 0.10],
                   key=lambda r: (-r["mean_cap_pct"]))
    best_be = sorted([r for r in traded if r["trades_per_series"] >= 1],
                     key=lambda r: (-r["be_share"], -r["mean_cap_pct"]))

    def series_table(k):
        s = sets.get(k)
        return {sk: v for sk, v in s["per_series"].items()} if s else {}

    ship_row = by_key.get(ship_key) if ship_key else None
    best_row = by_key[best_key]

    # Return over risk. Ranked among sets that trade on every series, so a
    # set that never entered (no drawdown, no return) cannot top the list.
    ranked_calmar = sorted([r for r in rows if r.get("calmar") is not None and r["trades_per_series"] >= 1],
                           key=lambda r: -r["calmar"])
    best_calmar_row = ranked_calmar[0] if ranked_calmar else None

    # Every set as one point: return, drawdown, trades per series, and the
    # sign-flip cost gate — the axis that splits the cloud in two.
    scatter = [[round(r["mean_cap_pct"], 4), round(r.get("mean_dd_cap_pct", 0), 4), round(r["trades_per_series"], 2),
                r["exit_negative_cum_cost_frac"], r["min_hold_recovered_cost_frac"]] for r in rows]

    # The risk that is NOT on any hold axis: which series are traded. The
    # same set and the same benchmark on four ex-ante subsets and one
    # hindsight subset (labelled so), each measured on its own series only.
    # "Quote-bridged" follows the report convention: the USD-quoted perps.
    BRIDGED = {"hyperliquid_futures", "kraken_futures", "paradex_futures"}
    majors = {"BTCUSDT", "ETHUSDT"}
    all_sk = sorted(ht_ps)
    subsets = [("all", all_sk, False),
               ("usdt_quoted", [k for k in all_sk if k.split("|")[1] not in BRIDGED], False),
               ("majors", [k for k in all_sk if k.split("|")[0] in majors], False),
               ("majors_usdt", [k for k in all_sk if k.split("|")[0] in majors and k.split("|")[1] not in BRIDGED], False),
               ("ht_positive", [k for k in all_sk if ht_ps[k]["cap"] > 0], True)]

    def on_subset(per_series, sks):
        sub = {k: per_series[k] for k in sks if k in per_series}
        if not sub:
            return None
        st = risk_stats(sub)
        st["mean_cap_pct"] = statistics.mean(v["cap"] for v in sub.values()) * 100
        st["positive"] = sum(1 for v in sub.values() if v["cap"] > 0)
        st["n"] = len(sub)
        return st
    subset_rows = []
    for name, sks, hindsight in subsets:
        subset_rows.append({"name": name, "n": len(sks), "hindsight": hindsight,
                            "ship": on_subset(sets[ship_key]["per_series"], sks) if ship_key else None,
                            "best": on_subset(sets[best_key]["per_series"], sks),
                            "ht": on_subset(ht_ps, sks)})

    # What the floor DID, trade by trade: the series whose trade list differs
    # between the shipped set and the same set with the floor on, with the
    # trades on each side and their net. A share of trades at or above break
    # even can rise because losing trades were merged, not because any trade
    # was saved — this is what lets the page say which.
    m_diff = None
    if m_on_key is not None and ship_key is not None:
        def by_series(led):
            out = collections.defaultdict(list)
            for x in led:
                out[f"{x['symbol']}|{x['perp']}"].append(x)
            return out
        a, b = by_series(ledgers[ship_key]), by_series(ledgers[m_on_key])
        sig = lambda xs: sorted((x["open_ms"], x["close_ms"]) for x in xs)
        changed = []
        for sk in sorted(set(a) | set(b)):
            if sig(a.get(sk, [])) != sig(b.get(sk, [])):
                slim = lambda xs: [{"net_frac": x["net_frac"], "reason": x["reason"], "held_days": x["held_days"],
                                    "open_ms": x["open_ms"], "close_ms": x["close_ms"]} for x in xs]
                changed.append({"series": sk, "off": slim(a.get(sk, [])), "on": slim(b.get(sk, []))})
        m_diff = {"m": m_on_key[AXES.index("min_hold_recovered_cost_frac")], "row": by_key.get(m_on_key), "changed": changed}

    # The question as asked: from the shipped set, move ONE axis and read what
    # happens to the return, the break-even share and the yield exits. A grid
    # marginal averages over every other axis; this is the row the operator
    # would actually change.
    one_axis = {}
    if ship_row:
        for ax in ("holding_days", "exit_persistence_periods", "min_hold_recovered_cost_frac",
                   "exit_negative_cum_cost_frac", "exit_negative_min_bps", "exit_negative_periods",
                   "exit_net_apr_frac", "min_rate_per_8h_bps", "persistence_periods"):
            rs_ = sorted([r for r in rows if all(abs(r[a] - ship[a]) < 1e-9 for a in AXES if a != ax)], key=lambda r: r[ax])
            if len(rs_) > 1:
                one_axis[ax] = rs_
    return {
        "window_from_ms": from_ms, "window_to_ms": to_ms, "window_days": float(runs[0]["window_days"]),
        "capital_per_notional": cap, "series": full, "sets": len(keys), "sets_incomplete": dropped,
        "runs": len(runs), "trades_total": n_trades,
        "refusals": [{"symbol": k[0], "perp": k[1], "reason_vi": v} for k, v in refused.items()],
        "hold_through": {"mean_cap_pct": ht_mean_cap, "positive": sum(1 for v in ht_by.values() if v > 0),
                         "by_series": {k: v * 100 / cap for k, v in ht_by.items()},
                         "dd_by_series": {k: v["dd"] * 100 for k, v in ht_ps.items()},
                         **ht_risk,
                         "sets_at_or_above": sum(1 for r in rows if ht_mean_cap is not None and r["mean_cap_pct"] >= ht_mean_cap)},
        "corpus": corp,
        "shipped": {"params": ship, "row": ship_row, "ledger": sorted(ledgers.get(ship_key, []), key=lambda t: (t["symbol"], t["perp"], t["open_ms"])),
                    "per_series": series_table(ship_key)} if ship_row else None,
        "best": {"row": best_row, "ledger": sorted(ledgers.get(best_key, []), key=lambda t: (t["symbol"], t["perp"], t["open_ms"])),
                 "per_series": series_table(best_key)},
        "one_axis": one_axis, "m_diff": m_diff,
        "risk": {"best_calmar": best_calmar_row, "ranked_calmar": ranked_calmar[:10], "scatter": scatter, "subsets": subset_rows,
                 "sets_at_or_above_ht_calmar": sum(1 for r in ranked_calmar if ht_risk.get("calmar") is not None and r["calmar"] >= ht_risk["calmar"])},
        "clean_best": clean[:10], "clean_count": len(clean),
        "be_best": best_be[:10],
        "top": rows[:25],
        "marginals": marginals(rows),
        "heat_hold_N": heat(rows, "holding_days", "exit_persistence_periods"),
        "heat_hold_M": heat(rows, "holding_days", "min_hold_recovered_cost_frac"),
        "heat_M_C": heat(rows, "min_hold_recovered_cost_frac", "exit_negative_cum_cost_frac"),
        "stats": {"positive_sets": sum(1 for r in rows if r["mean_cap_pct"] > 0),
                  "median_cap_pct": med([r["mean_cap_pct"] for r in rows]),
                  "traded_sets": len(traded)},
        # The axis values this window swept; the page's grid line is built
        # from these, so the full set list need not travel with it.
        "grid_values": {a: sorted({r[a] for r in rows}) for a in AXES},
    }


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", required=True)
    ap.add_argument("--config", required=True, help="config.yaml; its strategy block is the shipped set")
    ap.add_argument("--window", action="append", required=True, help="NAME=runs.csv:trades.csv (repeatable)")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    ship = shipped_set(args.config)
    db = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    # JS orders integer-like object keys ascending, so "6" would come before
    # "12" whatever the file says; the page reads this list instead.
    out = {"shipped": ship, "windows": {}, "window_order": [], "axes": AXES}
    for w in args.window:
        name, paths = w.split("=", 1)
        runs_path, trades_path = paths.split(":", 1)
        out["windows"][name] = window(name, runs_path, trades_path, db, ship,
                                      ship["min_rate_per_8h_bps"], int(ship["persistence_periods"]))
        out["window_order"].append(name)
    grid = {}
    for name, W in out["windows"].items():
        for a in AXES:
            grid.setdefault(a, set()).update(W["grid_values"][a])
    out["grid"] = {a: sorted(v) for a, v in grid.items()}
    json.dump(out, open(args.out, "w", encoding="utf-8"), ensure_ascii=False)
    print(f"wrote {args.out}", file=sys.stderr)


if __name__ == "__main__":
    main()
