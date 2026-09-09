#!/usr/bin/env python3
"""Does the shipped threshold set still work now that the pair list is 13?

The 2026-09-09 screen admitted 9 pairs beside the 4 the project had replayed
all along, so every earlier verdict was measured on a universe a third this
size. This script reads what cmd/backtest wrote for the SHIPPED set on the
new universe and answers three questions, in this order:

  1. Does the set make money per series, on CAPITAL (2x notional — the spot
     leg cannot be levered), against the hold-through benchmark the corpus
     itself defines?
  2. WHERE does it make it: the four old pairs, the nine new ones, the
     USD-quoted (quote-bridged) venues, one venue at a time?
  3. Is it still the right set on this universe, or does the wider grid now
     point somewhere else?

Read-only, like every other tool in here (CLAUDE.md rule 8): the corpus and
the run/trade readers are hold.py's, so nothing recomputes a rule Go ran.
"""
import argparse
import collections
import json
import re
import sqlite3
import statistics
import sys

import hold

# The pairs config.yaml carried before the 2026-09-09 screen. Every earlier
# report in docs/reports/ measured these and nothing else.
BASELINE = ("BTCUSDT", "ETHUSDT", "SOLUSDT", "XRPUSDT")


def source_facts(config_path):
    """market_type and quote_asset per source, line-based (no PyYAML here)."""
    out, cur = {}, None
    for line in open(config_path, encoding="utf-8"):
        m = re.match(r"^\s{2}- source:\s*(\S+)", line)
        if m:
            cur = m.group(1)
            out[cur] = {}
            continue
        if cur is None:
            continue
        if re.match(r"^\S", line):
            cur = None
            continue
        m = re.match(r"^\s{4}(market_type|quote_asset):\s*(\S+)", line)
        if m:
            out[cur][m.group(1)] = m.group(2)
    return out


def mean(xs):
    return statistics.mean(xs) if xs else None


def med(xs):
    return statistics.median(xs) if xs else None


def summarise(rows, ret_key, dd_key, trades_key):
    """One cohort as a line: mean return on capital, the drawdown it was
    bought with, and the ratio of the two — MEAN over MEAN, never a mean of
    per-series ratios (a series with a tiny drawdown would otherwise own the
    number). n is series, not trades."""
    rets = [r[ret_key] for r in rows if r.get(ret_key) is not None]
    dds = [r[dd_key] for r in rows if r.get(dd_key) is not None]
    if not rets:
        return {"series": 0}
    m_ret, m_dd = statistics.mean(rets), statistics.mean(dds) if dds else 0.0
    return {
        "series": len(rets),
        "mean_cap_pct": m_ret, "median_cap_pct": med(rets),
        "worst_cap_pct": min(rets), "best_cap_pct": max(rets),
        "positive": sum(1 for x in rets if x > 0),
        "mean_dd_cap_pct": m_dd, "worst_dd_cap_pct": max(dds) if dds else None,
        "calmar": m_ret / m_dd if m_dd > 0 else None,
        "trades_per_series": mean([r[trades_key] for r in rows if r.get(trades_key) is not None]),
        "std_cap_pct": statistics.pstdev(rets) if len(rets) > 1 else 0.0,
    }


def cohorts(rows, facts):
    """The cuts the question asks for. Each is a NAMED subset of series, not a
    rule: nothing in internal/strategy selects a series yet, so every line
    below is what the shipped set would have made had it been pointed at that
    subset, and the page says so."""
    def sel(pred):
        return [r for r in rows if pred(r)]

    def bridged(r):
        return facts.get(r["perp"], {}).get("quote_asset") == "USD"

    out = collections.OrderedDict()
    out["all"] = rows
    out["baseline"] = sel(lambda r: r["symbol"] in BASELINE)
    out["added"] = sel(lambda r: r["symbol"] not in BASELINE)
    out["usdt_quoted"] = sel(lambda r: not bridged(r))
    out["bridged"] = sel(bridged)
    out["hyperliquid"] = sel(lambda r: r["perp"] == "hyperliquid_futures")
    out["added_hyperliquid"] = sel(lambda r: r["symbol"] not in BASELINE and r["perp"] == "hyperliquid_futures")
    out["added_usdt"] = sel(lambda r: r["symbol"] not in BASELINE and not bridged(r))
    out["deep_corpus"] = sel(lambda r: (r.get("covered_days") or 0) >= 300)
    return out


def venue_rollup(rows):
    by = collections.defaultdict(list)
    for r in rows:
        by[r["perp"]].append(r)
    return [dict(name=k, **summarise(v, "cap_pct", "dd_cap_pct", "trades"),
                 ht_mean_cap_pct=mean([x["ht_cap_pct"] for x in v if x.get("ht_cap_pct") is not None]),
                 beat_ht=sum(1 for x in v if x.get("ht_cap_pct") is not None and x["cap_pct"] > x["ht_cap_pct"]))
            for k, v in sorted(by.items())]


def symbol_rollup(rows):
    by = collections.defaultdict(list)
    for r in rows:
        by[r["symbol"]].append(r)
    out = []
    for k, v in by.items():
        s = summarise(v, "cap_pct", "dd_cap_pct", "trades")
        s.update(name=k, added=k not in BASELINE,
                 mean_funding_bps=mean([x["mean_bps"] for x in v if x.get("mean_bps") is not None]),
                 ht_mean_cap_pct=mean([x["ht_cap_pct"] for x in v if x.get("ht_cap_pct") is not None]),
                 beat_ht=sum(1 for x in v if x.get("ht_cap_pct") is not None and x["cap_pct"] > x["ht_cap_pct"]))
        out.append(s)
    out.sort(key=lambda r: -(r["mean_cap_pct"] if r["mean_cap_pct"] is not None else -99))
    return out


def trade_stats(ledger):
    """What the shipped set's trades did. classify() is hold.py's, so an exit
    reason is bucketed exactly as every earlier report bucketed it."""
    if not ledger:
        return {"n": 0}
    n = len(ledger)
    reasons = collections.Counter(t["reason"] for t in ledger)
    be = sum(1 for t in ledger if t["net_frac"] >= 0)
    return {
        "n": n, "be": be, "be_share": be / n,
        "mean_net_pct": statistics.mean(t["net_frac"] for t in ledger) * 100,
        "mean_held_days": statistics.mean(t["held_days"] for t in ledger),
        "median_held_days": statistics.median(t["held_days"] for t in ledger),
        "mean_funding_pct": statistics.mean(t["funding_frac"] for t in ledger) * 100,
        "mean_cost_pct": statistics.mean(t["cost_frac"] for t in ledger) * 100,
        "reasons": {c: reasons.get(c, 0) for c in hold.REASONS},
        "worst_pct": min(t["net_frac"] for t in ledger) * 100,
        "best_pct": max(t["net_frac"] for t in ledger) * 100,
    }


def window(name, runs_path, trades_path, db, ship, facts):
    print(f"window {name}: runs", file=sys.stderr)
    runs, refused = hold.load_runs(runs_path)
    if not runs:
        sys.exit(f"{runs_path}: no successful run")
    keys = {hold.key_of(r) for r in runs}
    if len(keys) != 1:
        sys.exit(f"{runs_path}: {len(keys)} parameter sets — this page reads ONE (a plain run)")
    the_key = next(iter(keys))
    if not hold.same(the_key, ship):
        sys.exit(f"{runs_path}: the run's parameters are not config.yaml's strategy block")
    from_ms, to_ms = int(runs[0]["window_from_ms"]), int(runs[0]["window_to_ms"])
    cap_per_notional = float(runs[0]["capital_per_notional_frac"])

    series_cost = {(r["symbol"], r["perp_source"]): float(r["round_trip_cost_pct"]) / 100 for r in runs}
    print(f"window {name}: corpus for {len(series_cost)} series", file=sys.stderr)
    corp = hold.corpus(db, series_cost, from_ms, to_ms, ship["min_rate_per_8h_bps"],
                       int(ship["persistence_periods"]))
    corp_by = {f"{c['symbol']}|{c['perp']}": c for c in corp}

    print(f"window {name}: trades", file=sys.stderr)
    _, ledgers, n_trades = hold.stream_trades(trades_path, {the_key})
    ledger = ledgers[the_key]
    by_series = collections.defaultdict(list)
    for t in ledger:
        by_series[f"{t['symbol']}|{t['perp']}"].append(t)

    rows = []
    for r in runs:
        k = f"{r['symbol']}|{r['perp_source']}"
        c = corp_by.get(k)
        rows.append({
            "key": k, "symbol": r["symbol"], "perp": r["perp_source"], "spot": r["spot_source"],
            "cap_pct": float(r["total_return_on_capital_frac"]) * 100,
            "apr_cap_pct": float(r["realized_apr_on_capital_frac"]) * 100,
            "notional_pct": float(r["total_return_frac"]) * 100,
            # On capital, like the return, so the two divide into one ratio.
            "dd_cap_pct": float(r["max_drawdown_frac"]) * 100 / cap_per_notional,
            "trades": int(r["trades"]), "settlements": int(r["settlements"]),
            "exposure": int(r["periods_in_position"]) / max(1, int(r["settlements"])),
            "cost_pct": float(r["round_trip_cost_pct"]),
            "covered_days": float(r["covered_days"]),
            "coverage_short": r["coverage_short"] == "true",
            "basis_not_evaluable": int(r["basis_not_evaluable"]),
            "liquidations": int(r["liquidations"]),
            "quote_bridged": facts.get(r["perp_source"], {}).get("quote_asset") == "USD",
            "added": r["symbol"] not in BASELINE,
            "mean_bps": c["mean_rate_per_8h_bps"] if c else None,
            "positive_share": c["positive_share"] if c else None,
            # The benchmark every earlier report is judged against: in at the
            # first settlement, out at the last, ONE round trip.
            "ht_cap_pct": c["hold_through_net_frac"] * 100 / cap_per_notional if c else None,
            "ht_dd_cap_pct": c["hold_through_dd_frac"] * 100 / cap_per_notional if c else None,
            "be_median_days": c["rule_entry"]["median"] if c else None,
            "be_reached": c["rule_entry"]["reached"] if c else None,
            "be_entries": c["rule_entry"]["entries"] if c else None,
            "trade_list": by_series.get(k, []),
        })
    rows.sort(key=lambda r: -r["cap_pct"])

    coh = cohorts(rows, facts)
    cohort_rows = collections.OrderedDict()
    for cname, sub in coh.items():
        s = summarise(sub, "cap_pct", "dd_cap_pct", "trades")
        ht = summarise([r for r in sub if r.get("ht_cap_pct") is not None], "ht_cap_pct", "ht_dd_cap_pct", None)
        s["ht"] = ht
        s["beat_ht"] = sum(1 for r in sub if r.get("ht_cap_pct") is not None and r["cap_pct"] > r["ht_cap_pct"])
        s["members"] = [r["key"] for r in sub]
        cohort_rows[cname] = s

    return {
        "from_ms": from_ms, "to_ms": to_ms,
        "days": (to_ms - from_ms) / 86400000,
        "capital_per_notional": cap_per_notional,
        "series": rows,
        "refused": [{"symbol": s, "perp": p, "reason_vi": why} for (s, p), why in refused.items()],
        "cohorts": cohort_rows,
        "by_venue": venue_rollup(rows),
        "by_symbol": symbol_rollup(rows),
        "trades": trade_stats(ledger),
        "trade_count_all_sets": n_trades,
        "assumptions_vi": runs[0]["assumptions_vi"],
    }


def isolation(path, trades_path, w, label):
    """The SAME set with one exit turned off, run separately and diffed here.

    cmd/backtest cannot vary the basis thresholds inside a sweep (baseParams
    hardcodes them), and the run CSV carries no basis column, so the two runs
    are indistinguishable by their parameters — the caller NAMES what it
    changed and this function only measures the difference. Same universe,
    same window, same everything else, or the diff is not the exit's price.
    """
    runs, refused = hold.load_runs(path)
    off = {f"{r['symbol']}|{r['perp_source']}": r for r in runs}
    cap = w["capital_per_notional"]
    rows = []
    for s in w["series"]:
        o = off.get(s["key"])
        if o is None:
            continue
        cap_off = float(o["total_return_on_capital_frac"]) * 100
        rows.append({
            "key": s["key"], "symbol": s["symbol"], "perp": s["perp"],
            "cap_pct": s["cap_pct"], "cap_off_pct": cap_off, "delta_pct": cap_off - s["cap_pct"],
            "trades": s["trades"], "trades_off": int(o["trades"]),
            "dd_cap_pct": s["dd_cap_pct"], "dd_off_cap_pct": float(o["max_drawdown_frac"]) * 100 / cap,
        })
    rows.sort(key=lambda r: -abs(r["delta_pct"]))
    by = {r["key"]: r for r in rows}
    coh = {}
    for cname, c in w["cohorts"].items():
        members = [by[k] for k in c["members"] if k in by]
        if not members:
            continue
        coh[cname] = {
            "series": len(members),
            "mean_cap_pct": statistics.mean(m["cap_pct"] for m in members),
            "mean_off_pct": statistics.mean(m["cap_off_pct"] for m in members),
            "delta_pct": statistics.mean(m["delta_pct"] for m in members),
            "trades": sum(m["trades"] for m in members),
            "trades_off": sum(m["trades_off"] for m in members),
        }
    return {
        "label_vi": label, "series": rows[:20], "cohorts": coh,
        "trades": sum(r["trades"] for r in rows), "trades_off": sum(r["trades_off"] for r in rows),
        "moved": sum(1 for r in rows if abs(r["delta_pct"]) > 1e-9),
    }


def sweep(path, trades_path, ship, ht_mean_cap_pct):
    """The wider grid on the SAME series: is the shipped set still the answer?
    Only sets that ran on every series are ranked — a set missing a series is
    a different universe, not a better one."""
    print("sweep: runs", file=sys.stderr)
    runs, refused = hold.load_runs(path)
    sets, cap = hold.aggregate_runs(runs)
    full = max(s["series"] for s in sets.values())
    keys = [k for k, s in sets.items() if s["series"] == full]
    ledger_keys = set()
    ship_key = next((k for k in keys if hold.same(k, ship)), None)
    best_key = max(keys, key=lambda k: sets[k]["sum_cap"])
    for k in (ship_key, best_key):
        if k is not None:
            ledger_keys.add(k)
    print("sweep: trades", file=sys.stderr)
    acc, _, n_trades = hold.stream_trades(trades_path, ledger_keys) if trades_path else ({}, {}, 0)
    rows = [hold.set_row(k, sets[k], acc.get(k)) for k in keys]
    rows.sort(key=lambda r: -r["mean_cap_pct"])
    for i, r in enumerate(rows):
        r["rank"] = i + 1
    by_key = {hold.key_of(r): r for r in rows}
    ship_row = by_key.get(ship_key) if ship_key else None
    return {
        "sets": len(rows), "dropped_partial": len(sets) - len(keys), "series": full,
        "shipped": ship_row, "best": rows[0], "top": rows[:20],
        "marginals": hold.marginals(rows),
        "grid_values": {a: sorted({r[a] for r in rows}) for a in hold.AXES},
        "positive_sets": sum(1 for r in rows if r["mean_cap_pct"] > 0),
        "median_cap_pct": med([r["mean_cap_pct"] for r in rows]),
        "beat_ht": sum(1 for r in rows if ht_mean_cap_pct is not None and r["mean_cap_pct"] > ht_mean_cap_pct),
        "refused_series": len(refused),
        "trades_read": n_trades,
    }


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", required=True)
    ap.add_argument("--config", required=True, help="config.yaml; its strategy block IS the set under test")
    ap.add_argument("--window", action="append", required=True, help="NAME=runs.csv:trades.csv (repeatable)")
    ap.add_argument("--iso", action="append", default=[],
                    help="NAME=runs.csv:trades.csv — the same set with ONE exit turned off, for the diff")
    ap.add_argument("--iso-label", default="lối thoát basis TẮT (max_basis_pct = max_basis_widen_pct = 100)",
                    help="what the --iso run changed; the CSV cannot say, so the caller must")
    ap.add_argument("--sweep", help="runs.csv:trades.csv of the grid run on the same universe")
    ap.add_argument("--sweep-window", default="12", help="which window the sweep covers")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    ship = hold.shipped_set(args.config)
    facts = source_facts(args.config)
    db = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    out = {"shipped": ship, "sources": facts, "baseline_symbols": list(BASELINE),
           "windows": {}, "window_order": [], "axes": hold.AXES}
    for w in args.window:
        name, paths = w.split("=", 1)
        runs_path, trades_path = paths.split(":", 1)
        out["windows"][name] = window(name, runs_path, trades_path, db, ship, facts)
        # JS orders integer-like object keys ascending, so "12" would follow
        # "3" and "6" whatever the file says; the page reads this list.
        out["window_order"].append(name)
    for w in args.iso:
        name, paths = w.split("=", 1)
        runs_path, trades_path = paths.split(":", 1)
        if name not in out["windows"]:
            sys.exit(f"--iso {name}: no such window")
        out["windows"][name]["iso"] = isolation(runs_path, trades_path, out["windows"][name], args.iso_label)
    if args.sweep:
        runs_path, trades_path = args.sweep.split(":", 1)
        W = out["windows"].get(args.sweep_window)
        ht = W["cohorts"]["all"]["ht"]["mean_cap_pct"] if W else None
        out["sweep"] = sweep(runs_path, trades_path, ship, ht)
        out["sweep"]["window"] = args.sweep_window
        out["sweep"]["ht_mean_cap_pct"] = ht
    json.dump(out, open(args.out, "w", encoding="utf-8"), ensure_ascii=False)
    print(f"wrote {args.out}", file=sys.stderr)


if __name__ == "__main__":
    main()
