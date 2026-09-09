#!/usr/bin/env python3
"""Regularities in the funding corpus, read straight from funding_history.

Three years of settled funding let three questions be asked that a threshold
grid cannot answer:

  1. PREDICTABILITY — does the trailing mean funding of a series (30 / 90 /
     180 days) say anything about what the same series pays over the NEXT
     30 / 90 days? Pooled over series and sample dates, per year, and as a
     bucket table: "when the trailing mean was in this range, forward
     funding averaged that, and the forward hold-through net was positive
     this often."
  2. PERSISTENCE — does the cross-section persist: are the series that paid
     most in one year the ones that pay most in the next (rank correlation,
     top-quarter overlap), and is the venue premium stable?
  3. A REGIME BENCHMARK — be in a series only while its trailing D-day mean
     is at or above X (enter) and leave when it falls below X_out, paying a
     round trip per entry and per exit. Ranked walk-forward: the (X, D, X_out)
     chosen on one span is REPORTED on the other, beside the in-sample best
     of that other span, so the reader sees how much of the in-sample edge
     survives.

Everything here is a BENCHMARK computed in Python from the corpus, like
hold.py's hold-through: it is not a Go replay and it is not a rule the
scanner runs (rule 8). A regime rule this page makes a case for must be
written in internal/strategy and replayed by cmd/backtest before its number
is called a result. Costs are the ONE measured round trip per series that
cmd/backtest wrote into the runs CSV (today's book, applied to every entry),
on capital = capital_per_notional_frac × notional, exactly as the engine does.

The corpus is read WITHOUT the recorded_at_ms filter: it was backfilled on
one day, so every settlement of 2023 carries a 2026 recording stamp.
"""
import argparse
import bisect
import collections
import datetime
import json
import math
import statistics
import sqlite3
import sys

import hold

DAY = 86400000


def day(ms):
    return datetime.datetime.utcfromtimestamp(ms / 1000).strftime("%Y-%m-%d")


def load_series(db, src, sym, from_ms, to_ms):
    return db.execute("""SELECT funding_at_ms, rate_per_interval_frac, rate_per_8h_frac FROM funding_history
                         WHERE source=? AND symbol=? AND model='discrete' AND rate_type<>'Special'
                           AND funding_at_ms>=? AND funding_at_ms<? ORDER BY funding_at_ms""",
                      (src, sym, from_ms, to_ms)).fetchall()


class Series:
    """One series' settlements with prefix sums, so a trailing or forward mean
    over any span is two bisects and a subtraction."""

    def __init__(self, key, symbol, perp, rows, cost_frac, cap):
        self.key, self.symbol, self.perp, self.cost, self.cap = key, symbol, perp, cost_frac, cap
        self.ts = [r[0] for r in rows]
        self.rate = [r[1] for r in rows]          # per interval, fraction of notional
        self.r8 = [r[2] * 1e4 for r in rows]      # bps per 8h, for comparison across cadences
        self.cum_rate = [0.0]
        self.cum_r8 = [0.0]
        for a, b in zip(self.rate, self.r8):
            self.cum_rate.append(self.cum_rate[-1] + a)
            self.cum_r8.append(self.cum_r8[-1] + b)

    def idx(self, t_ms):
        """number of settlements with stamp < t_ms"""
        return bisect.bisect_left(self.ts, t_ms)

    def mean_r8(self, lo, hi):
        """mean bps/8h over settlements with lo <= stamp < hi, or None"""
        i, j = self.idx(lo), self.idx(hi)
        return (self.cum_r8[j] - self.cum_r8[i]) / (j - i) if j > i else None

    def sum_rate(self, lo, hi):
        i, j = self.idx(lo), self.idx(hi)
        return self.cum_rate[j] - self.cum_rate[i], j - i


# --- 1. predictability ---------------------------------------------------------

def spearman(xs, ys):
    def ranks(v):
        order = sorted(range(len(v)), key=lambda i: v[i])
        r = [0.0] * len(v)
        i = 0
        while i < len(order):
            j = i
            while j + 1 < len(order) and v[order[j + 1]] == v[order[i]]:
                j += 1
            for k in range(i, j + 1):
                r[order[k]] = (i + j) / 2 + 1
            i = j + 1
        return r
    return pearson(ranks(xs), ranks(ys))


def pearson(xs, ys):
    n = len(xs)
    if n < 3:
        return None
    mx, my = statistics.mean(xs), statistics.mean(ys)
    sxx = sum((x - mx) ** 2 for x in xs)
    syy = sum((y - my) ** 2 for y in ys)
    if sxx == 0 or syy == 0:
        return None
    return sum((x - mx) * (y - my) for x, y in zip(xs, ys)) / math.sqrt(sxx * syy)


BUCKETS = [(-99, 0), (0, 0.3), (0.3, 0.5), (0.5, 1.0), (1.0, 2.0), (2.0, 99)]


def predictability(series, from_ms, to_ms, trailing_days=(30, 90, 180), forward_days=(30, 90), step_days=7):
    """Sample every step_days from the first date with `max trailing` of history
    to the last date with `forward` of future. One pooled point = (series,
    date). Reports correlations and the bucket table, pooled and per year."""
    out = {}
    for D in trailing_days:
        for F in forward_days:
            pts = []
            t = from_ms + max(trailing_days) * DAY
            while t + F * DAY <= to_ms:
                for s in series:
                    tr = s.mean_r8(t - D * DAY, t)
                    fw = s.mean_r8(t, t + F * DAY)
                    if tr is None or fw is None:
                        continue
                    paid, n = s.sum_rate(t, t + F * DAY)
                    pts.append({"t": t, "key": s.key, "trail": tr, "fwd": fw,
                                "fwd_net_cap_pct": (paid - s.cost) * 100 / s.cap})
                t += step_days * DAY
            def summarise(sub):
                if len(sub) < 3:
                    return {"n": len(sub)}
                xs, ys = [p["trail"] for p in sub], [p["fwd"] for p in sub]
                buckets = []
                for lo, hi in BUCKETS:
                    b = [p for p in sub if lo <= p["trail"] < hi]
                    if not b:
                        continue
                    buckets.append({"lo": lo, "hi": hi, "n": len(b),
                                    "mean_trail_bps": statistics.mean(p["trail"] for p in b),
                                    "mean_fwd_bps": statistics.mean(p["fwd"] for p in b),
                                    "fwd_net_cap_pct": statistics.mean(p["fwd_net_cap_pct"] for p in b),
                                    "fwd_net_positive_share": sum(1 for p in b if p["fwd_net_cap_pct"] > 0) / len(b)})
                # The pooled rank correlation mixes two regularities: at one
                # date, which SERIES pays more (the cross-section), and for one
                # series, whether its own level persists (timing). They are
                # separated here because a bot uses them differently.
                by_date, by_key = collections.defaultdict(list), collections.defaultdict(list)
                for q in sub:
                    by_date[q["t"]].append(q)
                    by_key[q["key"]].append(q)
                cs = [spearman([q["trail"] for q in g], [q["fwd"] for q in g]) for g in by_date.values() if len(g) >= 8]
                ws = [spearman([q["trail"] for q in g], [q["fwd"] for q in g]) for g in by_key.values() if len(g) >= 8]
                cs = [x for x in cs if x is not None]
                ws = [x for x in ws if x is not None]
                return {"n": len(sub), "pearson": pearson(xs, ys), "spearman": spearman(xs, ys),
                        "cross_sectional_spearman": statistics.mean(cs) if cs else None, "dates": len(cs),
                        "within_series_spearman": statistics.mean(ws) if ws else None, "series": len(ws),
                        "mean_trail_bps": statistics.mean(xs), "mean_fwd_bps": statistics.mean(ys),
                        "buckets": buckets}
            by_year = collections.OrderedDict()
            for p in pts:
                y = int((to_ms - p["t"] - 1) // (365 * DAY))   # 0 = the last 365 days, 1 = the year before…
                by_year.setdefault(y, []).append(p)
            out[f"{D}/{F}"] = {"trailing_days": D, "forward_days": F, "pooled": summarise(pts),
                               "by_slice": {f"{day(to_ms - (y + 1) * 365 * DAY)} → {day(to_ms - y * 365 * DAY)}": summarise(sub)
                                            for y, sub in sorted(by_year.items(), reverse=True)}}
    return out


# --- 2. persistence -----------------------------------------------------------

def persistence(series, from_ms, to_ms):
    slices = []
    n = int((to_ms - from_ms) // (365 * DAY))
    for i in range(n, 0, -1):
        a, b = to_ms - i * 365 * DAY, to_ms - (i - 1) * 365 * DAY
        per = {}
        for s in series:
            m = s.mean_r8(a, b)
            paid, cnt = s.sum_rate(a, b)
            if m is not None and cnt > 0:
                per[s.key] = {"mean_bps": m, "ht_cap_pct": (paid - s.cost) * 100 / s.cap}
        slices.append({"from": day(a), "to": day(b), "series": per})
    pairs = []
    for x, y in zip(slices, slices[1:]):
        common = sorted(set(x["series"]) & set(y["series"]))
        if len(common) < 4:
            continue
        usdt = [k for k in common if not k.endswith("hyperliquid_futures")]
        within_usdt = None
        if len(usdt) >= 4:
            qx = max(1, len(usdt) // 4)
            tx = set(sorted(usdt, key=lambda k: -x["series"][k]["mean_bps"])[:qx])
            ty = set(sorted(usdt, key=lambda k: -y["series"][k]["mean_bps"])[:qx])
            within_usdt = {"n": len(usdt), "spearman": spearman([x["series"][k]["mean_bps"] for k in usdt], [y["series"][k]["mean_bps"] for k in usdt]),
                           "top_quarter": qx, "top_overlap": len(tx & ty)}
        xs = [x["series"][k]["mean_bps"] for k in common]
        ys = [y["series"][k]["mean_bps"] for k in common]
        q = max(1, len(common) // 4)
        top_x = set(sorted(common, key=lambda k: -x["series"][k]["mean_bps"])[:q])
        top_y = set(sorted(common, key=lambda k: -y["series"][k]["mean_bps"])[:q])
        bot_x = set(sorted(common, key=lambda k: x["series"][k]["mean_bps"])[:q])
        bot_y = set(sorted(common, key=lambda k: y["series"][k]["mean_bps"])[:q])
        pairs.append({"from_slice": f"{x['from']} → {x['to']}", "to_slice": f"{y['from']} → {y['to']}",
                      "n": len(common), "spearman": spearman(xs, ys), "pearson": pearson(xs, ys),
                      "top_quarter": q, "top_overlap": len(top_x & top_y), "bottom_overlap": len(bot_x & bot_y),
                      "top_x": sorted(top_x), "top_y": sorted(top_y), "within_usdt": within_usdt})
    # venue premium per slice: mean bps per perp source
    venues = []
    for sl in slices:
        by = collections.defaultdict(list)
        for k, v in sl["series"].items():
            by[k.split("|")[1]].append(v["mean_bps"])
        venues.append({"slice": f"{sl['from']} → {sl['to']}", "venues": {p: statistics.mean(v) for p, v in by.items()}})
    return {"slices": slices, "pairs": pairs, "venues": venues}


# --- 3. the regime benchmark ------------------------------------------------------

def regime_run(s, from_ms, to_ms, x_in, d_days, x_out, year_edges):
    """Be in while the trailing d-day mean is >= x_in, leave when it is < x_out.
    Funding is collected from the settlement AFTER the entry through the exit
    settlement, a round trip is paid at each exit, and a position still open
    at the end is closed there (like the engine's window-end close).
    year_edges: stamps that split the window into yearly slices, oldest first."""
    i0, i1 = s.idx(from_ms), s.idx(to_ms)
    paid_by_year = [0.0] * (len(year_edges) - 1)
    trips_by_year = [0] * (len(year_edges) - 1)
    in_count = 0
    in_pos = False
    trips = 0
    total = 0.0

    def year_of(t):
        k = bisect.bisect_right(year_edges, t) - 1
        return min(max(k, 0), len(year_edges) - 2)

    for i in range(i0, i1):
        t = s.ts[i]
        if in_pos:
            total += s.rate[i]
            paid_by_year[year_of(t)] += s.rate[i]
            in_count += 1
        # history must cover the trailing window before the mean means anything
        if t - s.ts[0] < d_days * DAY:
            continue
        tr = s.mean_r8(t - d_days * DAY, t + 1)
        if tr is None:
            continue
        if not in_pos and tr >= x_in:
            in_pos = True
        elif in_pos and tr < x_out:
            in_pos = False
            trips += 1
            trips_by_year[year_of(t)] += 1
    if in_pos:
        trips += 1
        trips_by_year[-1] += 1
    cost = trips * s.cost
    return {"cap_pct": (total - cost) * 100 / s.cap, "trips": trips,
            "in_share": in_count / max(1, i1 - i0),
            "by_year_cap_pct": [(p - tr * s.cost) * 100 / s.cap for p, tr in zip(paid_by_year, trips_by_year)],
            "trips_by_year": trips_by_year}


def hold_through(s, from_ms, to_ms, year_edges):
    i0, i1 = s.idx(from_ms), s.idx(to_ms)
    if i1 - i0 < 2:
        return None
    paid_by_year = [0.0] * (len(year_edges) - 1)
    for i in range(i0 + 1, i1):
        k = min(max(bisect.bisect_right(year_edges, s.ts[i]) - 1, 0), len(year_edges) - 2)
        paid_by_year[k] += s.rate[i]
    by = [p * 100 / s.cap for p in paid_by_year]
    by[-1] -= s.cost * 100 / s.cap
    return {"cap_pct": (sum(paid_by_year) - s.cost) * 100 / s.cap, "by_year_cap_pct": by}


def cohort_of(s, facts):
    return "bridged" if facts.get(s.perp, {}).get("quote_asset") == "USD" else "usdt"


def regime_grid(series, from_ms, to_ms, facts, x_ins=(0.2, 0.3, 0.5, 0.7, 1.0, 1.5), d_list=(30, 90, 180), out_modes=("same", "half", "zero")):
    n_years = int(round((to_ms - from_ms) / (365 * DAY)))
    year_edges = [to_ms - (n_years - k) * 365 * DAY for k in range(n_years + 1)]
    year_edges[0] = min(year_edges[0], from_ms)
    labels = [f"{day(a)} → {day(b)}" for a, b in zip(year_edges, year_edges[1:])]
    ht = {s.key: hold_through(s, from_ms, to_ms, year_edges) for s in series}
    ht = {k: v for k, v in ht.items() if v}
    keys = sorted(ht)
    def agg(vals, label):
        rows = [vals[k] for k in keys if k in vals]
        return {"series": len(rows), "mean_cap_pct": statistics.mean(r["cap_pct"] for r in rows),
                "positive": sum(1 for r in rows if r["cap_pct"] > 0),
                "by_year_cap_pct": [statistics.mean(r["by_year_cap_pct"][y] for r in rows) for y in range(n_years)],
                "trips_per_series": statistics.mean(r.get("trips", 1) for r in rows),
                "in_share": statistics.mean(r.get("in_share", 1.0) for r in rows)}
    configs = []
    for x_in in x_ins:
        for d in d_list:
            for mode in out_modes:
                x_out = {"same": x_in, "half": x_in / 2, "zero": 0.0}[mode]
                res = {s.key: regime_run(s, from_ms, to_ms, x_in, d, x_out, year_edges) for s in series if s.key in ht}
                row = {"x_in": x_in, "days": d, "x_out_mode": mode, "x_out": x_out}
                row.update(agg(res, "all"))
                for c in ("usdt", "bridged"):
                    sub = {k: v for k, v in res.items() if cohort_of(next(s for s in series if s.key == k), facts) == c}
                    if sub:
                        row[c] = {"series": len(sub), "mean_cap_pct": statistics.mean(v["cap_pct"] for v in sub.values()),
                                  "by_year_cap_pct": [statistics.mean(v["by_year_cap_pct"][y] for v in sub.values()) for y in range(n_years)]}
                row["per_series"] = {k: round(v["cap_pct"], 4) for k, v in res.items()}
                configs.append(row)
    hold = {"by_year_labels": labels}
    hold.update(agg(ht, "all"))
    for c in ("usdt", "bridged"):
        sub = {k: v for k, v in ht.items() if cohort_of(next(s for s in series if s.key == k), facts) == c}
        if sub:
            hold[c] = {"series": len(sub), "mean_cap_pct": statistics.mean(v["cap_pct"] for v in sub.values()),
                       "by_year_cap_pct": [statistics.mean(v["by_year_cap_pct"][y] for v in sub.values()) for y in range(n_years)]}
    hold["per_series"] = {k: round(v["cap_pct"], 4) for k, v in ht.items()}
    configs.sort(key=lambda r: -r["mean_cap_pct"])
    for i, r in enumerate(configs):
        r["rank"] = i + 1
    return {"year_labels": labels, "hold_through": hold, "configs": configs}


def walk_forward(series, facts, spans):
    """spans: list of (name, train_from, train_to, test_from, test_to). The
    config is chosen on the train span (best mean on capital over all series)
    and reported on the test span next to hold-through and the test span's
    own in-sample best."""
    out = []
    for name, a, b, c, d in spans:
        train = regime_grid(series, a, b, facts)
        test = regime_grid(series, c, d, facts)
        chosen = train["configs"][0]
        same = next(r for r in test["configs"] if r["x_in"] == chosen["x_in"] and r["days"] == chosen["days"] and r["x_out_mode"] == chosen["x_out_mode"])
        out.append({"name": name, "train": f"{day(a)} → {day(b)}", "test": f"{day(c)} → {day(d)}",
                    "chosen": {k: chosen[k] for k in ("x_in", "days", "x_out_mode", "mean_cap_pct", "trips_per_series", "in_share", "positive", "series")},
                    "train_hold_through": train["hold_through"]["mean_cap_pct"],
                    "test_chosen": {k: same[k] for k in ("rank", "mean_cap_pct", "trips_per_series", "in_share", "positive", "series", "by_year_cap_pct")},
                    "test_hold_through": {k: test["hold_through"][k] for k in ("mean_cap_pct", "positive", "series", "by_year_cap_pct")},
                    "test_best": {k: test["configs"][0][k] for k in ("x_in", "days", "x_out_mode", "mean_cap_pct", "trips_per_series")},
                    "test_sets": len(test["configs"]),
                    "test_sets_beating_hold_through": sum(1 for r in test["configs"] if r["mean_cap_pct"] > test["hold_through"]["mean_cap_pct"])})
    return out


# --- 4. the oracle ceiling ---------------------------------------------------------

def oracle(series, from_ms, to_ms, year_edges, lazy_days=None):
    """Runs the DP twice per series (free and lazy) and reports, on capital,
    ORACLE − HOLD-THROUGH per year, the switches the oracle used, and how many
    series clear one round trip a year of extra edge."""
    out = []
    for s in series:
        i0, i1 = s.idx(from_ms), s.idx(to_ms)
        n = i1 - i0
        if n < 2:
            continue
        allowed = None
        if lazy_days:
            allowed = [False] * len(s.ts)
            t = from_ms
            j = i0
            while t < to_ms and j < i1:
                while j < i1 and s.ts[j] < t:
                    j += 1
                if j < i1:
                    allowed[j] = True
                t += lazy_days * DAY
        res = _oracle_dp(s, i0, i1, allowed, year_edges)
        ht = hold_through(s, from_ms, to_ms, year_edges)
        res["key"] = s.key
        res["edge_cap_pct"] = res["cap_pct"] - ht["cap_pct"]
        res["edge_by_year_cap_pct"] = [a - b for a, b in zip(res["by_year_cap_pct"], ht["by_year_cap_pct"])]
        out.append(res)
    n_years = len(year_edges) - 1
    def mean_edge(pred):
        sub = [r for r in out if pred(r["key"])]
        return {"series": len(sub), "mean_edge_cap_pct": statistics.mean(r["edge_cap_pct"] for r in sub)} if sub else None
    return {"series": len(out),
            "excluding_bnb": mean_edge(lambda k: not k.startswith("BNBUSDT|")),
            "btc_eth_only": mean_edge(lambda k: k.split("|")[0] in ("BTCUSDT", "ETHUSDT")),
            "mean_edge_cap_pct": statistics.mean(r["edge_cap_pct"] for r in out),
            "mean_edge_by_year_cap_pct": [statistics.mean(r["edge_by_year_cap_pct"][y] for r in out) for y in range(n_years)],
            "mean_switches": statistics.mean(r["switches"] for r in out),
            "series_clearing_one_trip_per_year": [sum(1 for r in out if r["edge_by_year_cap_pct"][y] > r["cost_cap_pct"]) for y in range(n_years)],
            "per_series": {r["key"]: {"edge_cap_pct": round(r["edge_cap_pct"], 4), "switches": r["switches"]} for r in out}}


def _oracle_dp(s, i0, i1, allowed, year_edges):
    n = i1 - i0
    c = s.cost / 2
    rates = s.rate[i0:i1]
    ok = [True] * n if allowed is None else allowed[i0:i1]
    NEG = -1e18
    # v_in[k], v_out[k]: best total collected from k..end given state at k
    v_in = [0.0] * (n + 1)
    v_out = [0.0] * (n + 1)
    v_in[n], v_out[n] = -c, 0.0          # after the last settlement the position must be closed
    for k in range(n - 1, -1, -1):
        leave = v_out[k + 1] - c if ok[k] else NEG
        enter = v_in[k + 1] - c if ok[k] else NEG
        v_in[k] = rates[k] + max(v_in[k + 1], leave)
        v_out[k] = max(v_out[k + 1], enter)
    # start closed before settlement 0: entering for k=0 costs c
    state = 1 if (ok[0] and v_in[0] - c > v_out[0]) else 0
    total = -c if state == 1 else 0.0
    switches = 1 if state == 1 else 0
    paid_by_year = [0.0] * (len(year_edges) - 1)
    trips_by_year = [0.0] * (len(year_edges) - 1)
    def year_of(t):
        k = bisect.bisect_right(year_edges, t) - 1
        return min(max(k, 0), len(year_edges) - 2)
    if state == 1:
        trips_by_year[year_of(s.ts[i0])] += c
    for k in range(n):
        y = year_of(s.ts[i0 + k])
        if state == 1:
            total += rates[k]
            paid_by_year[y] += rates[k]
        # decide the state for k+1
        if k == n - 1:
            if state == 1:
                total -= c
                trips_by_year[y] += c
                switches += 1
            break
        if state == 1:
            nxt = 1 if v_in[k + 1] >= (v_out[k + 1] - c if ok[k] else NEG) else 0
        else:
            nxt = 1 if (ok[k] and v_in[k + 1] - c > v_out[k + 1]) else 0
        if nxt != state:
            total -= c
            trips_by_year[y] += c
            switches += 1
        state = nxt
    return {"cap_pct": total * 100 / s.cap, "switches": switches, "cost_cap_pct": s.cost * 100 / s.cap,
            "by_year_cap_pct": [(p - t) * 100 / s.cap for p, t in zip(paid_by_year, trips_by_year)]}


# --- 5. allocation: move capital instead of idling it ------------------------------

def allocation(series, from_ms, to_ms, year_edges, facts, rebalance_days=90, trail_days=90, cap_mult=2.0, seed=7):
    """Rebalance the SAME capital across the series every rebalance_days:
    weight ∝ max(0, trailing mean funding over trail_days), capped at cap_mult
    × equal weight (excess redistributed pro rata). While the corpus does not
    yet cover the trailing horizon the weights stay equal — the same refusal
    checkTrailingMean makes — so the first rebalance is not decided by one
    print. Funding
    between rebalances is Σ w_i × Σ rate_i; moving weight costs each series
    half its round trip on the notional moved (closing part of one position
    and opening part of another is two fills each). Equal weight held for the
    whole window is exactly the hold-through mean. Reported beside a
    permutation null (the same weights handed to randomly relabelled series)
    and a hyperliquid-excluded variant, because the venue premium alone would
    make any funding-weighted scheme look clever."""
    import random
    keys = [s.key for s in series]
    by = {s.key: s for s in series}
    n_years = len(year_edges) - 1
    def year_of(t):
        k = bisect.bisect_right(year_edges, t) - 1
        return min(max(k, 0), len(year_edges) - 2)

    def run(scheme, pool, rnd=None):
        n = len(pool)
        eq = 1.0 / n
        w = {k: eq for k in pool}
        total = 0.0
        by_year = [0.0] * n_years
        turnover = 0.0
        t = from_ms
        first = True
        venue_w = collections.defaultdict(float)
        while t < to_ms:
            t2 = min(t + rebalance_days * DAY, to_ms)
            if scheme != "equal" and t - from_ms >= trail_days * DAY:
                tr = {k: by[k].mean_r8(t - trail_days * DAY, t) for k in pool}
                vals = [max(0.0, v) if v is not None else 0.0 for v in (tr[k] for k in pool)]
                if rnd is not None:
                    rnd.shuffle(vals)
                if scheme == "topq":
                    q = max(1, n // 4)
                    top = sorted(range(n), key=lambda i: -vals[i])[:q]
                    raw = [1.0 if i in top else 0.0 for i in range(n)]
                else:
                    raw = vals
                tot = sum(raw)
                target = {k: (raw[i] / tot if tot > 0 else eq) for i, k in enumerate(pool)}
                # cap at cap_mult × equal weight, redistribute the excess pro rata to the uncapped
                capv = cap_mult * eq
                for _ in range(10):
                    excess = sum(max(0.0, v - capv) for v in target.values())
                    if excess <= 1e-12:
                        break
                    under = [k for k in pool if target[k] < capv - 1e-12]
                    room = sum(capv - target[k] for k in under)
                    for k in pool:
                        target[k] = min(target[k], capv)
                    if not under or room <= 0:
                        break
                    for k in under:
                        target[k] += excess * (capv - target[k]) / room
                s_ = sum(target.values())
                target = {k: v / s_ for k, v in target.items()}
            else:
                target = {k: eq for k in pool} if (first or scheme == "equal") else dict(w)
            for k in pool:
                moved_cost = abs(target[k] - (0.0 if first else w[k])) * by[k].cost / 2
                turnover += moved_cost
                by_year[year_of(t)] -= moved_cost
            w = target
            first = False
            # funding from the settlement after the rebalance, attributed to the
            # year each settlement falls in (a 90-day period can straddle an edge)
            cuts = [t + 1] + [e for e in year_edges if t + 1 < e < t2] + [t2]
            for k in pool:
                for a, b in zip(cuts, cuts[1:]):
                    paid, _ = by[k].sum_rate(a, b)
                    total += w[k] * paid
                    by_year[year_of(a)] += w[k] * paid
                venue_w[k.split("|")[1]] += w[k] * (t2 - t)
            t = t2
        # close everything at the end, charged to the last year
        closing = sum(w[k] * by[k].cost / 2 for k in pool)
        turnover += closing
        by_year[-1] -= closing
        span = to_ms - from_ms
        return {"cap_pct": (total - turnover) * 100 / series[0].cap, "turnover_cost_cap_pct": turnover * 100 / series[0].cap,
                "by_year_cap_pct": [x * 100 / series[0].cap for x in by_year],
                "venue_weight": {v: x / span for v, x in venue_w.items()}}

    pools = {"all": keys, "no_hyperliquid": [k for k in keys if not k.endswith("hyperliquid_futures")]}
    out = {"rebalance_days": rebalance_days, "trail_days": trail_days, "cap_mult": cap_mult, "schemes": {}}
    for pname, pool in pools.items():
        res = {"series": len(pool)}
        for scheme in ("equal", "weighted", "topq"):
            res[scheme] = run(scheme, pool)
        rnd = random.Random(seed)
        nulls = [run("weighted", pool, rnd)["cap_pct"] for _ in range(100)]
        res["null_weighted_mean_cap_pct"] = statistics.mean(nulls)
        res["null_weighted_max_cap_pct"] = max(nulls)
        res["null_weighted_sd_cap_pct"] = statistics.pstdev(nulls)
        res["null_draws"] = len(nulls)
        out["schemes"][pname] = res
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", required=True, help="the corpus (a backfilled copy is fine: no recorded_at_ms filter here)")
    ap.add_argument("--config", required=True, help="config.yaml, for each source's quote asset")
    ap.add_argument("--runs", required=True, help="a cmd/backtest runs CSV: series list, round-trip cost, capital per notional, window")
    ap.add_argument("--min-covered-days", type=float, default=1000, help="keep series whose corpus covers at least this (default: the 3-year set)")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    import expand
    facts = expand.source_facts(args.config)
    runs, _ = hold.load_runs(args.runs)
    runs = [r for r in runs if float(r["covered_days"]) >= args.min_covered_days]
    if not runs:
        sys.exit("no series covers the requested span")
    from_ms, to_ms = int(runs[0]["window_from_ms"]), int(runs[0]["window_to_ms"])
    cap = float(runs[0]["capital_per_notional_frac"])
    db = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    series = []
    for r in runs:
        rows = load_series(db, r["perp_source"], r["symbol"], from_ms - 200 * DAY, to_ms)
        if len(rows) < 100:
            continue
        series.append(Series(f"{r['symbol']}|{r['perp_source']}", r["symbol"], r["perp_source"], rows,
                             float(r["round_trip_cost_pct"]) / 100, cap))
    print(f"{len(series)} series, {day(from_ms)} → {day(to_ms)}", file=sys.stderr)

    n_years = int(round((to_ms - from_ms) / (365 * DAY)))
    edges = [to_ms - (n_years - k) * 365 * DAY for k in range(n_years + 1)]
    spans = []
    if n_years >= 3:
        spans.append(("first two years → last year", edges[0], edges[2], edges[2], edges[3]))
        spans.append(("last two years → first year", edges[1], edges[3], edges[0], edges[1]))
        spans.append(("first year → last two years", edges[0], edges[1], edges[1], edges[3]))
    elif n_years == 2:
        spans.append(("first year → second year", edges[0], edges[1], edges[1], edges[2]))

    print("predictability", file=sys.stderr)
    pred = predictability(series, from_ms, to_ms)
    print("persistence", file=sys.stderr)
    pers = persistence(series, from_ms, to_ms)
    print("regime grid", file=sys.stderr)
    grid = regime_grid(series, from_ms, to_ms, facts)
    print("walk-forward", file=sys.stderr)
    wf = walk_forward(series, facts, spans)
    print("oracle", file=sys.stderr)
    orc = {"free": oracle(series, from_ms, to_ms, edges), "lazy7": oracle(series, from_ms, to_ms, edges, lazy_days=7),
           "year_labels": [f"{day(a)} → {day(b)}" for a, b in zip(edges, edges[1:])]}
    print("allocation", file=sys.stderr)
    alloc = allocation(series, from_ms, to_ms, edges, facts)
    out = {"from_ms": from_ms, "to_ms": to_ms, "capital_per_notional": cap, "oracle": orc, "allocation": alloc,
           "series": [{"key": s.key, "symbol": s.symbol, "perp": s.perp, "cost_pct": s.cost * 100,
                       "settlements": len(s.ts), "cohort": cohort_of(s, facts)} for s in series],
           "predictability": pred, "persistence": pers, "regime": grid, "walk_forward": wf}
    json.dump(out, open(args.out, "w", encoding="utf-8"), ensure_ascii=False)
    print(f"wrote {args.out}", file=sys.stderr)


if __name__ == "__main__":
    main()
