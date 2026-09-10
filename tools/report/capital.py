#!/usr/bin/env python3
"""The capital-and-risk grid: which series get capital, and which risk exit is
worth its round trips — ranked on return over drawdown, both on CAPITAL.

Two axes the strategy gained on 2026-09-09 are what this page is about:

  * series selection (min_trailing_mean_bps over trailing_mean_days) — the
    capital-allocation rule, deciding WHICH series a position is opened on;
  * the basis exit's two limits (max_basis_pct / max_basis_widen_pct) — until
    that day the one parameter a sweep could not vary, and the one an
    isolation run had priced at 0.94 points on 13 pairs.

Beside them the min-hold floor and the entry level, everything else at the
shipped value. Every set ran on the SAME 74 series, so a set that selects
fewer series is measured on the whole universe's capital — idle capital earns
0 — and separately on the capital it actually deployed. The two numbers
answer different questions and the page keeps them apart.

Read-only, like everything in tools/report (CLAUDE.md rule 8): the corpus,
run and trade readers are hold.py's, and the portfolio curve drawn here is
the engine's own trades replayed over the engine's own settlement arithmetic
(paid from the settlement after entry, the round trip charged at the close) —
a measurement of what Go decided, never a second decision.
"""
import argparse
import collections
import csv
import json
import os
import sqlite3
import statistics
import sys
import tempfile

import expand
import hold

CAP_AXES = ["min_trailing_mean_bps", "trailing_mean_days", "trailing_mean_min_cost_frac", "max_basis_pct", "max_basis_widen_pct",
            "min_hold_recovered_cost_frac", "min_rate_per_8h_bps"]


def concat_csv(paths, out_path):
    """Several sweep CSVs with the same header into one file, header once."""
    header = None
    with open(out_path, "w", encoding="utf-8", newline="") as out:
        w = csv.writer(out)
        for p in paths:
            with open(p, encoding="utf-8", newline="") as f:
                r = csv.reader(f)
                h = next(r)
                if header is None:
                    header = h
                    w.writerow(h)
                elif h != header:
                    sys.exit(f"{p}: header differs from {paths[0]}")
                for row in r:
                    w.writerow(row)
    return out_path


def mean(xs):
    return statistics.mean(xs) if xs else None


# --- the portfolio curve ------------------------------------------------------

DAY_MS = 86400000


def series_rates(db, sym, src, from_ms, to_ms):
    """One series' settled rates inside the window, oldest first — the rows
    the replay could have seen (recorded before the window's end)."""
    return db.execute("""SELECT funding_at_ms, rate_per_interval_frac FROM funding_history
                         WHERE source=? AND symbol=? AND model='discrete' AND rate_type<>'Special'
                           AND funding_at_ms>=? AND funding_at_ms<? AND recorded_at_ms<=? ORDER BY funding_at_ms""",
                      (src, sym, from_ms, to_ms, to_ms)).fetchall()


class SeriesRates:
    """One series' settled rates in the window, indexed for range sums.

    stamps/rates oldest first; prefix[i] is the sum of rates[:i]; day_first[d]
    is the index of the first stamp on or after day d (days counted from the
    window start), so a trade's take on one day is one prefix difference."""

    def __init__(self, rows, from_ms, n_days):
        self.stamps = [r[0] for r in rows]
        self.rates = [r[1] for r in rows]
        self.prefix = [0.0]
        for r in self.rates:
            self.prefix.append(self.prefix[-1] + r)
        self.day_first = [len(self.stamps)] * (n_days + 2)
        d = 0
        for i, ms in enumerate(self.stamps):
            while d <= (ms - from_ms) // DAY_MS and d < n_days + 1:
                self.day_first[d] = i
                d += 1
        for k in range(d, n_days + 2):
            self.day_first[k] = len(self.stamps)

    def take(self, open_ms, close_ms, from_ms, out, weight):
        """Add this trade's funding, day by day, into out[day]. Paid on every
        stamp strictly after the open and up to the close — the engine's
        arithmetic (rule 6: the position must exist at the stamp)."""
        import bisect
        lo = bisect.bisect_right(self.stamps, open_ms)   # first index paid
        hi = bisect.bisect_right(self.stamps, close_ms)  # one past the last
        if hi <= lo:
            return
        d0 = max(0, (self.stamps[lo] - from_ms) // DAY_MS)
        d1 = (self.stamps[hi - 1] - from_ms) // DAY_MS
        for d in range(d0, d1 + 1):
            i0 = max(lo, self.day_first[d])
            i1 = min(hi, self.day_first[d + 1])
            if i1 > i0:
                out[d] += (self.prefix[i1] - self.prefix[i0]) * weight


def daily_portfolio(trades, rates_by, from_ms, n_days, n_series, cap_per_notional):
    """Equal weight over n_series slots of one notional each, on a DAILY grid:
    the mean per-slot equity on capital. A slot with no trade is a flat 0 —
    capital committed to the universe and never deployed. The round trip is
    charged on the close day. Daily rather than per settlement so the same
    curve can be drawn for every set in the grid; the grid's drawdown is
    therefore measured between daily closes, which understates an intraday
    dip and never overstates it."""
    out = [0.0] * (n_days + 2)
    w = 1.0 / n_series / cap_per_notional
    for t in trades:
        sr = rates_by.get(f"{t['symbol']}|{t['perp']}")
        if sr is None:
            continue
        sr.take(t["open_ms"], t["close_ms"], from_ms, out, w)
        d = min(n_days + 1, max(0, (t["close_ms"] - from_ms) // DAY_MS))
        out[d] -= t["cost_frac"] * w
    eq, peak, dd, curve = 0.0, 0.0, 0.0, []
    for d, delta in enumerate(out):
        eq += delta
        peak = max(peak, eq)
        dd = max(dd, peak - eq)
        curve.append((from_ms + d * DAY_MS, eq))
    return {"final_cap_pct": eq * 100, "max_dd_cap_pct": dd * 100, "curve": curve}


def deployment(trades):
    """How many slots were open at once, and when the first one opened."""
    ev = sorted([(t["open_ms"], 1) for t in trades] + [(t["close_ms"], -1) for t in trades])
    cur = peak = 0
    for _, d in ev:
        cur += d
        peak = max(peak, cur)
    return {"max_concurrent": peak, "first_entry_ms": min((t["open_ms"] for t in trades), default=None),
            "series_traded": sorted({f"{t['symbol']}|{t['perp']}" for t in trades})}


def downsample(curve, n=400):
    if len(curve) <= n:
        return [[ms, round(v * 100, 5)] for ms, v in curve]
    step = len(curve) / n
    out = [curve[int(i * step)] for i in range(n)]
    out.append(curve[-1])
    return [[ms, round(v * 100, 5)] for ms, v in out]


# The drawdown budgets the frontier answers for: "the best set that never
# drew the portfolio down more than X on capital". A ratio alone would crown
# a set that parks 97% of the universe in cash and picks one lucky series.
BUDGETS_PCT = (0.05, 0.1, 0.15, 0.2, 0.3, 0.5, 1.0)


# --- one window ----------------------------------------------------------------

def window(name, runs_paths, trades_paths, db, ship, facts, tmpdir):
    runs_path = concat_csv(runs_paths, os.path.join(tmpdir, f"runs{name}.csv"))
    trades_path = concat_csv(trades_paths, os.path.join(tmpdir, f"trades{name}.csv"))
    print(f"window {name}: runs", file=sys.stderr)
    runs, refused = hold.load_runs(runs_path)
    sets, cap = hold.aggregate_runs(runs)
    full = max(s["series"] for s in sets.values())
    keys = [k for k, s in sets.items() if s["series"] == full]
    from_ms, to_ms = int(runs[0]["window_from_ms"]), int(runs[0]["window_to_ms"])
    series_keys = sorted({f"{r['symbol']}|{r['perp_source']}" for r in runs})
    series_cost = {(r["symbol"], r["perp_source"]): float(r["round_trip_cost_pct"]) / 100 for r in runs}

    print(f"window {name}: corpus for {len(series_cost)} series", file=sys.stderr)
    corp = hold.corpus(db, series_cost, from_ms, to_ms, ship["min_rate_per_8h_bps"], int(ship["persistence_periods"]))
    corp_by = {f"{c['symbol']}|{c['perp']}": c for c in corp}
    ht_cap = {k: c["hold_through_net_frac"] / cap for k, c in corp_by.items()}
    ht_ps = {k: {"cap": v, "dd": corp_by[k]["hold_through_dd_frac"] / cap, "trades": 1,
                 "pip": corp_by[k]["settlements"] - 1, "st": corp_by[k]["settlements"]} for k, v in ht_cap.items()}
    ht_risk = hold.risk_stats(ht_ps)
    ht_mean = statistics.mean(ht_cap.values()) * 100

    ship_key = next((k for k in keys if hold.same(k, ship)), None)
    print(f"window {name}: trades", file=sys.stderr)
    acc, ledgers, n_trades = hold.stream_trades(trades_path, set(keys))

    print(f"window {name}: daily portfolio curve for {len(keys)} sets", file=sys.stderr)
    n_series = len(series_keys)
    n_days = int((to_ms - from_ms) // DAY_MS) + 1
    rates_by = {k: SeriesRates(series_rates(db, *k.split("|"), from_ms, to_ms), from_ms, n_days) for k in series_keys}
    pf = {k: daily_portfolio(ledgers.get(k, []), rates_by, from_ms, n_days, n_series, cap) for k in keys}

    rows = [hold.set_row(k, sets[k], acc.get(k)) for k in keys]
    for r in rows:
        k = hold.key_of(r)
        s = sets[k]
        traded = [v for v in s["per_series"].values() if v["trades"] > 0]
        r["traded_series"] = len(traded)
        r["deployed_cap_pct"] = mean([v["cap"] for v in traded]) * 100 if traded else None
        r["beat_ht"] = sum(1 for sk, v in s["per_series"].items() if sk in ht_cap and v["cap"] > ht_cap[sk])
        # Hold-through of the SAME series this set deployed on: separates
        # picking the series from timing the trades.
        hton = mean([ht_cap[sk] for sk, v in s["per_series"].items() if v["trades"] > 0 and sk in ht_cap])
        r["ht_on_traded_cap_pct"] = hton * 100 if hton is not None else None
        r["pf_final_cap_pct"] = pf[k]["final_cap_pct"]
        r["pf_dd_cap_pct"] = pf[k]["max_dd_cap_pct"]
        r["pf_calmar"] = pf[k]["final_cap_pct"] / pf[k]["max_dd_cap_pct"] if pf[k]["max_dd_cap_pct"] > 0 else None
        # Either selection floor (the absolute one, or since 2026-09-10 the
        # series' own cost-crossing) makes a set a selecting set.
        r["selection_on"] = r["min_trailing_mean_bps"] > 0 or r["trailing_mean_min_cost_frac"] > 0
    rows.sort(key=lambda r: -r["mean_cap_pct"])
    for i, r in enumerate(rows):
        r["rank"] = i + 1
    by_key = {hold.key_of(r): r for r in rows}
    best_key = hold.key_of(rows[0])

    # The frontier: for each drawdown budget, the best return among sets whose
    # PORTFOLIO drawdown stayed inside it. This is the capital/risk answer —
    # a ratio alone crowns whoever deployed least.
    frontier = []
    for b in BUDGETS_PCT:
        inside = [r for r in rows if r["pf_dd_cap_pct"] <= b]
        if inside:
            top = max(inside, key=lambda r: r["mean_cap_pct"])
            frontier.append({"budget_pct": b, "sets_inside": len(inside), "row": top,
                             "params": dict(zip(hold.AXES, hold.key_of(top)))})
    # The set the frontier itself points at: the smallest budget whose best
    # return is within 0.05 points of the unconstrained best.
    pick_key = best_key
    for f in frontier:
        if f["row"]["mean_cap_pct"] >= rows[0]["mean_cap_pct"] - 0.05:
            pick_key = hold.key_of(f["row"])
            break
    scatter = [[round(r["mean_cap_pct"], 4), round(r["pf_dd_cap_pct"], 4), r["traded_series"], 1 if r["selection_on"] else 0,
                round(r["max_basis_widen_pct"], 2)] for r in rows]
    # The recommended set keeps a basis GUARD: the best return among sets
    # whose two basis limits are both finite (a limit of 100 points is the
    # exit switched off, and a risk exit that can never fire is not risk
    # management). Its distance to the unconstrained best is the guard's price.
    guarded_rows = [r for r in rows if r["max_basis_pct"] < 50 and r["max_basis_widen_pct"] < 50]
    guarded_key = hold.key_of(guarded_rows[0]) if guarded_rows else None

    def curve_for(key):
        d = deployment(ledgers.get(key, []))
        return {"final_cap_pct": pf[key]["final_cap_pct"], "max_dd_cap_pct": pf[key]["max_dd_cap_pct"],
                "curve": downsample(pf[key]["curve"]), **d}

    ht_trades = [{"symbol": k.split("|")[0], "perp": k.split("|")[1], "open_ms": rates_by[k].stamps[0],
                  "close_ms": rates_by[k].stamps[-1], "cost_frac": series_cost[tuple(k.split("|"))]}
                 for k in series_keys if len(rates_by[k].stamps) > 1]
    ht_pc = daily_portfolio(ht_trades, rates_by, from_ms, n_days, n_series, cap)

    def cohorts_for(key):
        s = sets[key]
        rws = []
        for sk, v in s["per_series"].items():
            sym, perp = sk.split("|")
            rws.append({"key": sk, "symbol": sym, "perp": perp, "cap_pct": v["cap"] * 100, "dd_cap_pct": v["dd"] * 100,
                        "trades": v["trades"], "covered_days": corp_by[sk]["covered_days"] if sk in corp_by else None,
                        "ht_cap_pct": ht_cap[sk] * 100 if sk in ht_cap else None,
                        "mean_bps": corp_by[sk]["mean_rate_per_8h_bps"] if sk in corp_by else None,
                        "quote_bridged": facts.get(perp, {}).get("quote_asset") == "USD"})
        out = collections.OrderedDict()
        for cname, sub in expand.cohorts(rws, facts).items():
            summ = expand.summarise(sub, "cap_pct", "dd_cap_pct", "trades")
            summ["ht_mean_cap_pct"] = mean([r["ht_cap_pct"] for r in sub if r["ht_cap_pct"] is not None])
            summ["traded"] = sum(1 for r in sub if r["trades"] > 0)
            out[cname] = summ
        return out, sorted(rws, key=lambda r: -r["cap_pct"])

    def block(key):
        if key is None:
            return None
        coh, per_series = cohorts_for(key)
        return {"row": by_key[key], "params": dict(zip(hold.AXES, key)), "portfolio": curve_for(key),
                "cohorts": coh, "per_series": per_series}

    marg = hold.marginals(rows)
    # Selection sets on their own: the best by return on DEPLOYED capital, so
    # the page can say what concentrating capital would have earned — and on
    # how few series, at a size the book was never priced for.
    sel_rows = sorted([r for r in rows if r["selection_on"] and r["traded_series"] > 0],
                      key=lambda r: -(r["deployed_cap_pct"] or -99))
    ht_pf_calmar = ht_pc["final_cap_pct"] / ht_pc["max_dd_cap_pct"] if ht_pc["max_dd_cap_pct"] > 0 else None
    return {
        "from_ms": from_ms, "to_ms": to_ms, "days": (to_ms - from_ms) / 86400000, "capital_per_notional": cap,
        "sets": len(rows), "dropped_partial": len(sets) - len(keys), "series": n_series, "trades_read": n_trades,
        "refused": len(refused),
        "hold_through": {"mean_cap_pct": ht_mean, "positive": sum(1 for v in ht_cap.values() if v > 0),
                         "portfolio": {"final_cap_pct": ht_pc["final_cap_pct"], "max_dd_cap_pct": ht_pc["max_dd_cap_pct"],
                                       "curve": downsample(ht_pc["curve"]), "pf_calmar": ht_pf_calmar},
                         **ht_risk},
        "shipped": block(ship_key),
        "best": block(best_key),
        "pick": block(pick_key) if pick_key != best_key else None,
        "pick_is_best": pick_key == best_key,
        "guarded": block(guarded_key) if guarded_key is not None and guarded_key != best_key else None,
        "guarded_is_best": guarded_key == best_key,
        "frontier": frontier,
        "scatter": scatter,
        "selection_best_deployed": sel_rows[:10],
        "selection_best_block": block(hold.key_of(sel_rows[0])) if sel_rows else None,
        "top": rows[:25],
        "marginals": {a: marg[a] for a in CAP_AXES if a in marg},
        "grid_values": {a: sorted({r[a] for r in rows}) for a in CAP_AXES},
        "stats": {"positive_sets": sum(1 for r in rows if r["mean_cap_pct"] > 0),
                  "beat_ht": sum(1 for r in rows if r["mean_cap_pct"] > ht_mean),
                  "inside_ht_dd": sum(1 for r in rows if r["pf_dd_cap_pct"] <= ht_pc["max_dd_cap_pct"]),
                  "median_cap_pct": statistics.median(r["mean_cap_pct"] for r in rows),
                  "selection_sets": sum(1 for r in rows if r["selection_on"])},
        "rows_index": {"|".join(str(v) for v in hold.key_of(r)): r["rank"] for r in rows},
    }


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", required=True)
    ap.add_argument("--config", required=True)
    ap.add_argument("--window", action="append", required=True,
                    help="NAME=runs1.csv,runs2.csv:trades1.csv,trades2.csv (repeatable; several CSVs are one grid)")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    ship = hold.shipped_set(args.config)
    facts = expand.source_facts(args.config)
    db = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    out = {"shipped": ship, "axes": hold.AXES, "cap_axes": CAP_AXES, "windows": {}, "window_order": []}
    with tempfile.TemporaryDirectory() as tmp:
        for w in args.window:
            name, paths = w.split("=", 1)
            runs, trades = paths.split(":", 1)
            out["windows"][name] = window(name, runs.split(","), trades.split(","), db, ship, facts, tmp)
            out["window_order"].append(name)
    # Robustness: where does each window's best land in the other window?
    for name, W in out["windows"].items():
        W["cross_rank"] = {}
        for other, O in out["windows"].items():
            if other == name:
                continue
            for which in ("best", "guarded", "selection_best_block"):
                b = W.get(which)
                if b:
                    k = "|".join(str(v) for v in hold.key_of(b["row"]))
                    W["cross_rank"][f"{which}_in_{other}"] = O["rows_index"].get(k)
    for W in out["windows"].values():
        del W["rows_index"]
    json.dump(out, open(args.out, "w", encoding="utf-8"), ensure_ascii=False)
    print(f"wrote {args.out}", file=sys.stderr)


if __name__ == "__main__":
    main()
