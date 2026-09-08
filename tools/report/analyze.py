#!/usr/bin/env python3
"""Aggregate cmd/backtest sweep CSVs (+ trades CSVs) into one JSON for the HTML
report. Read-only: it reads the CSVs the Go tool wrote and the SQLite corpus
(mode=ro) for the funding statistics behind the break-even figures."""
import argparse, array, collections, csv, json, math, re, sqlite3, statistics, sys, time

NUM = {"window_from_ms", "window_to_ms", "window_days", "covered_days", "min_rate_per_8h_bps",
       "persistence_periods", "min_net_apr_frac", "exit_net_apr_frac", "exit_persistence_periods",
       "notional_quote", "holding_days", "round_trip_cost_pct", "cost_book_sampled_at_ms",
       "settlements", "trades", "periods_in_position", "total_return_frac", "realized_apr_frac",
       "max_drawdown_frac", "funding_reversals", "positive_funding_period_share", "dropped_special",
       "basis_not_evaluable", "basis_evaluable", "entered_without_basis", "liquidations",
       "capital_per_notional_frac", "total_return_on_capital_frac", "realized_apr_on_capital_frac",
       "perp_margin_frac", "min_liquidation_buffer_pct", "min_hold_recovered_cost_frac"}
TNUM = {"min_rate_per_8h_bps", "persistence_periods", "min_net_apr_frac", "exit_net_apr_frac",
        "exit_persistence_periods", "exit_negative_min_bps", "exit_negative_periods", "exit_negative_cum_cost_frac",
        "notional_quote", "holding_days", "open_at_ms", "close_at_ms",
        "held_days", "settlements", "funding_frac", "cost_frac", "net_frac"}
AXES = ["min_rate_per_8h_bps", "persistence_periods", "min_net_apr_frac", "exit_net_apr_frac",
        "exit_persistence_periods", "exit_negative_min_bps", "exit_negative_periods", "exit_negative_cum_cost_frac",
        "notional_quote", "holding_days"]
GATES = ["exit_negative_min_bps", "exit_negative_periods", "exit_negative_cum_cost_frac"]
NUM |= set(GATES)
TNUM |= set(GATES)
# BASE is the set config.yaml ships as of 2026-09-07 afternoon: the gate grid's
# best set with holding_days 30->90, exit_persistence_periods 12->48 and
# exit_negative_cum_cost_frac 0.25->1.0 applied on top.
# PREV is the step-3.3 set, which is ALSO what the running 3.5 journal process
# is producing — it loaded ba5ee31's config once at start-up and config edits
# do not reach it. Keeping both lets the report show what the change bought
# instead of asserting it, and lets the 3.5 gate compare like with like.
BASE = {"min_rate_per_8h_bps": 0.3, "persistence_periods": 6, "min_net_apr_frac": 0.02,
        "exit_net_apr_frac": 0.0, "exit_persistence_periods": 48,
        "exit_negative_min_bps": 2.0, "exit_negative_periods": 2, "exit_negative_cum_cost_frac": 1.0,
        "notional_quote": 50000, "holding_days": 90}
PREV = {"min_rate_per_8h_bps": 0.5, "persistence_periods": 3, "min_net_apr_frac": 0.02,
        "exit_net_apr_frac": 0.005, "exit_persistence_periods": 3,
        "exit_negative_min_bps": 0, "exit_negative_periods": 1, "exit_negative_cum_cost_frac": 0,
        "notional_quote": 50000, "holding_days": 30}
# MORNING is the set that shipped between the two, so a reader can see the two
# steps separately rather than one merged jump.
MORNING = {"min_rate_per_8h_bps": 0.3, "persistence_periods": 6, "min_net_apr_frac": 0.02,
           "exit_net_apr_frac": 0.0, "exit_persistence_periods": 12,
           "exit_negative_min_bps": 2.0, "exit_negative_periods": 2, "exit_negative_cum_cost_frac": 0.25,
           "notional_quote": 50000, "holding_days": 30}
BASE_NOGATE = {a: v for a, v in BASE.items() if a not in GATES}
HOLD_BINS = [0, 1, 3, 7, 14, 30, 60, math.inf]


GATE_DEFAULT = {"exit_negative_min_bps": "0", "exit_negative_periods": "1", "exit_negative_cum_cost_frac": "0"}


def load(path, numeric):
    out = []
    for r in csv.DictReader(open(path, encoding="utf-8")):
        for g, d in GATE_DEFAULT.items():
            r.setdefault(g, d)
        for k in numeric:
            # Columns added after an older CSV was written are absent, not
            # zero-valued; defaulting keeps this able to read a previous run's
            # output, which is what the two are compared on.
            v = r.get(k)
            r[k] = float(v) if v not in (None, "") else 0.0
        out.append(r)
    return out


class TradeAcc:
    """Accumulates exactly what trade_block reports, without holding the rows.

    The 12,288-set grid writes 4.6 million trades for the 12-month window
    alone. Materialising those as dicts reached 8.2 GB and had to be killed, so
    the loader streams and keeps six flat arrays instead — array('d') stores a
    raw double per value rather than a boxed float plus a list slot, which is
    the difference between ~150 MB and several GB. The figures are identical:
    the same values, summarised at the end rather than at the start.
    """

    def __init__(self):
        self.net = array.array("d")
        self.held = array.array("d")
        self.settle = array.array("d")
        self.funding = array.array("d")
        self.cost = array.array("d")
        self.reasons = collections.Counter()

    def add(self, net, held, settle, funding, cost, reason):
        self.net.append(net)
        self.held.append(held)
        self.settle.append(settle)
        self.funding.append(funding)
        self.cost.append(cost)
        self.reasons[classify_exit(reason)] += 1

    def block(self):
        n = len(self.net)
        if not n:
            return {"n": 0}
        return {"n": n, "share_net_positive": sum(1 for v in self.net if v > 0) / n,
                "median_held_days": med(self.held), "mean_held_days": mean(self.held),
                "median_settlements": med(self.settle),
                "mean_funding_pct": mean([v * 100 for v in self.funding]),
                "mean_cost_pct": mean([v * 100 for v in self.cost]),
                "mean_net_pct": mean([v * 100 for v in self.net]),
                "hold_hist": hist(self.held, HOLD_BINS),
                "exit_reasons": dict(self.reasons),
                "net_hist": hist([v * 100 for v in self.net], auto_edges([v * 100 for v in self.net], 20))}


def stream_trades(path, groups):
    """One pass over the trades CSV, feeding each row to whichever accumulators
    claim it. groups is a list of (predicate over the row dict, TradeAcc)."""
    if not path:
        return
    with open(path, encoding="utf-8", newline="") as f:
        rd = csv.reader(f)
        cols = {name: i for i, name in enumerate(next(rd))}
        need = ("net_frac", "held_days", "settlements", "funding_frac", "cost_frac", "exit_reason_vi")
        idx = [cols[c] for c in need]
        axis_idx = {a: cols[a] for a in AXES if a in cols}
        sym_i, perp_i = cols["symbol"], cols["perp_source"]
        for row in rd:
            # A dict of just the fields a predicate can ask about, built once
            # per row; the six reported values are read positionally.
            view = {a: float(row[i]) for a, i in axis_idx.items()}
            for g in GATE_DEFAULT:
                view.setdefault(g, float(GATE_DEFAULT[g]))
            view["symbol"], view["perp_source"] = row[sym_i], row[perp_i]
            net, held, settle, funding, cost, reason = (row[i] for i in idx)
            for pred, acc in groups:
                if pred(view):
                    acc.add(float(net), float(held), float(settle), float(funding), float(cost), reason)


def med(xs):
    return statistics.median(xs) if xs else None


def mean(xs):
    return statistics.fmean(xs) if xs else None


# The classes prep.py emits, so a pre-classified token passes straight through.
EXIT_CLASSES = {"sign_flip", "decay", "basis", "liquidation", "window_end",
                "unpriceable", "hedge_gone", "other"}


def classify_exit(s):
    if s in EXIT_CLASSES:
        return s
    if s.startswith("THOÁT: funding đã đảo dấu"):
        return "sign_flip"
    # New at 3.3b/3.3c and both were falling into "other": the basis exit could
    # not fire at all before price_history, and liquidation did not exist.
    if s.startswith("THOÁT: basis"):
        return "basis"
    if s.startswith("THANH LÝ"):
        return "liquidation"
    if s.startswith("THOÁT: cả"):
        return "decay"
    if s.startswith("Đóng ở cuối cửa sổ"):
        return "window_end"
    if "không còn tính được APR ròng" in s:
        return "unpriceable"
    if "chân spot" in s:
        return "hedge_gone"
    return "other"


def hist(values, edges):
    counts = [0] * (len(edges) - 1)
    for v in values:
        for i in range(len(edges) - 1):
            if edges[i] <= v < edges[i + 1]:
                counts[i] += 1
                break
    return [{"x0": None if math.isinf(edges[i]) else edges[i],
             "x1": None if math.isinf(edges[i + 1]) else edges[i + 1], "n": counts[i]}
            for i in range(len(edges) - 1)]


def auto_edges(values, bins=24):
    lo, hi = min(values), max(values)
    if lo == hi:
        return [lo - 0.5, hi + 0.5]
    step = (hi - lo) / bins
    mag = 10 ** math.floor(math.log10(step))
    for m in (1, 2, 2.5, 5, 10):
        if m * mag >= step:
            step = m * mag
            break
    start = math.floor(lo / step) * step
    edges = [start]
    while edges[-1] < hi:
        edges.append(round(edges[-1] + step, 10))
    return edges


def params_of(r):
    return {a: r[a] for a in AXES}


def is_base(r):
    return all(abs(r[a] - BASE[a]) < 1e-12 for a in AXES)


def is_prev(r):
    return all(abs(r[a] - PREV[a]) < 1e-12 for a in AXES)


def run_row(r):
    return {"symbol": r["symbol"], "perp": r["perp_source"], "spot": r["spot_source"],
            "params": params_of(r), "covered_days": r["covered_days"], "settlements": int(r["settlements"]),
            "trades": int(r["trades"]), "periods_in_position": int(r["periods_in_position"]),
            "total_return_pct": r["total_return_frac"] * 100, "realized_apr_pct": r["realized_apr_frac"] * 100,
            "max_drawdown_pct": r["max_drawdown_frac"] * 100, "cost_pct": r["round_trip_cost_pct"],
            "positive_share": r["positive_funding_period_share"], "reversals": int(r["funding_reversals"]),
            "basis_not_evaluable": int(r["basis_not_evaluable"]),
            # New since the previous report: price_history made the basis exit
            # evaluable, so "0 basis exits" can finally be told apart from "the
            # rule was never tested". And the capital denominator, which is the
            # one the leverage question needs.
            "basis_evaluable": int(r.get("basis_evaluable", 0)),
            "entered_without_basis": int(r.get("entered_without_basis", 0)),
            "capital_per_notional": r.get("capital_per_notional_frac", 2.0),
            "total_return_on_capital_pct": r.get("total_return_on_capital_frac", 0.0) * 100,
            "realized_apr_on_capital_pct": r.get("realized_apr_on_capital_frac", 0.0) * 100}


def corpus_stats(db, from_ms, to_ms):
    out = {}
    q = """SELECT source, symbol, interval_sec, count(*), avg(rate_per_interval_frac), avg(rate_per_8h_frac),
                  avg(rate_per_interval_frac > 0), sum(rate_per_interval_frac), min(funding_at_ms), max(funding_at_ms)
           FROM funding_history WHERE model='discrete' AND rate_type<>'Special'
             AND funding_at_ms >= ? AND funding_at_ms < ?
             AND recorded_at_ms <= ? GROUP BY 1,2,3"""
    # recorded_at_ms <= window end: only rows the replay could have seen. The
    # scanner tops the corpus up hourly, so a settlement inside the window but
    # written after the run would otherwise enter the hold-through benchmark
    # while the replay never saw it.
    for src, sym, iv, n, mi, m8, pos, tot, lo, hi in db.execute(q, (from_ms, to_ms, to_ms)):
        k = (sym, src)
        if k in out and out[k]["n"] >= n:
            continue  # keep the modal cadence's row
        out[k] = {"interval_sec": iv, "n": n, "mean_rate_per_interval_frac": mi, "mean_rate_per_8h_frac": m8,
                  "positive_share": pos, "sum_rate_frac": tot, "first_ms": lo, "last_ms": hi}
    return out


def window_summary(runs, trades_path, db):
    ok = [r for r in runs if r["ok"] == "true"]
    refused = [r for r in runs if r["ok"] != "true"]
    traded = [r for r in ok if r["trades"] > 0]
    profitable = [r for r in traded if r["total_return_frac"] > 0]
    from_ms, to_ms = runs[0]["window_from_ms"], runs[0]["window_to_ms"]
    corpus = corpus_stats(db, from_ms, to_ms)

    refusals = collections.OrderedDict()
    for r in refused:
        refusals.setdefault((r["symbol"], r["perp_source"]), r["reason_vi"])

    series = collections.OrderedDict()
    for r in ok:
        k = (r["symbol"], r["perp_source"], r["spot_source"])
        s = series.setdefault(k, {"symbol": k[0], "perp": k[1], "spot": k[2], "covered_days": r["covered_days"],
                                  "settlements": int(r["settlements"]), "cost_pct_by_notional": {}, "runs": 0,
                                  "traded": 0, "profitable": 0, "returns": [], "best": None, "worst": None,
                                  "trades_total": 0, "base": None})
        s["runs"] += 1
        s["cost_pct_by_notional"][str(int(r["notional_quote"]))] = r["round_trip_cost_pct"]
        if r["trades"] > 0:
            s["traded"] += 1
            s["trades_total"] += int(r["trades"])
            s["returns"].append(r["total_return_frac"] * 100)
            if r["total_return_frac"] > 0:
                s["profitable"] += 1
            if s["best"] is None or r["total_return_frac"] > s["best"]["total_return_frac"]:
                s["best"] = r
            if s["worst"] is None or r["total_return_frac"] < s["worst"]["total_return_frac"]:
                s["worst"] = r
        if is_base(r):
            s["base"] = r
    series_out = []
    for k, s in series.items():
        c = corpus.get((s["symbol"], s["perp"]), {})
        cost50 = s["cost_pct_by_notional"].get("50000") or next(iter(s["cost_pct_by_notional"].values()))
        mi = c.get("mean_rate_per_interval_frac")
        be_settle = (cost50 / 100) / mi if mi and mi > 0 else None
        series_out.append({
            "symbol": s["symbol"], "perp": s["perp"], "spot": s["spot"], "covered_days": s["covered_days"],
            "settlements": s["settlements"], "cost_pct_by_notional": s["cost_pct_by_notional"],
            "runs": s["runs"], "traded": s["traded"], "profitable": s["profitable"],
            "median_return_pct": med(s["returns"]), "mean_trades": s["trades_total"] / s["traded"] if s["traded"] else 0,
            "best": run_row(s["best"]) if s["best"] else None,
            "worst": run_row(s["worst"]) if s["worst"] else None,
            "base": run_row(s["base"]) if s["base"] else None,
            "corpus": {"interval_sec": c.get("interval_sec"), "n": c.get("n"),
                       "mean_rate_per_8h_bps": (c.get("mean_rate_per_8h_frac") or 0) * 1e4,
                       "mean_rate_per_interval_bps": (mi or 0) * 1e4,
                       "positive_share": c.get("positive_share"),
                       "hold_through_gross_pct": (c.get("sum_rate_frac") or 0) * 100,
                       "hold_through_net_pct": (c.get("sum_rate_frac") or 0) * 100 - cost50},
            "sets_beating_hold_through": sum(1 for r in ok if r["symbol"] == s["symbol"] and r["perp_source"] == s["perp"]
                                             and r["total_return_frac"] * 100 > (c.get("sum_rate_frac") or 0) * 100 - cost50),
            "break_even_settlements": be_settle,
            "break_even_days": be_settle * c["interval_sec"] / 86400 if be_settle else None,
        })

    axes = {}
    for a in AXES:
        vals = sorted({r[a] for r in ok})
        axes[a] = []
        for v in vals:
            rs = [r for r in traded if r[a] == v]
            axes[a].append({"value": v, "n": len(rs),
                            "median_return_pct": med([r["total_return_frac"] * 100 for r in rs]),
                            "mean_return_pct": mean([r["total_return_frac"] * 100 for r in rs]),
                            "max_return_pct": max([r["total_return_frac"] * 100 for r in rs]) if rs else None,
                            "share_profitable": (sum(1 for r in rs if r["total_return_frac"] > 0) / len(rs)) if rs else None,
                            "mean_trades": mean([r["trades"] for r in rs])})

    by_perp = []
    for perp in sorted({r["perp_source"] for r in ok}):
        rs = [r for r in ok if r["perp_source"] == perp]
        tr = [r for r in rs if r["trades"] > 0]
        pr = [r for r in tr if r["total_return_frac"] > 0]
        best = max(tr, key=lambda r: r["total_return_frac"]) if tr else None
        by_perp.append({"perp": perp, "series": len({r["symbol"] for r in rs}), "runs": len(rs), "traded": len(tr),
                        "profitable": len(pr), "median_return_pct": med([r["total_return_frac"] * 100 for r in tr]),
                        "best_return_pct": best["total_return_frac"] * 100 if best else None,
                        "best_symbol": best["symbol"] if best else None, "best_trades": int(best["trades"]) if best else None,
                        "covered_days": med([r["covered_days"] for r in rs]),
                        "cost_pct_50k": med([r["round_trip_cost_pct"] for r in rs if r["notional_quote"] == 50000]),
                        "mean_trades": mean([r["trades"] for r in tr])})
    signflip = []
    for v in sorted({tuple(r[g] for g in GATES) for r in ok}):
        rs = [r for r in ok if all(r[g] == v[i] for i, g in enumerate(GATES))]
        tr = [r for r in rs if r["trades"] > 0]
        pr = [r for r in tr if r["total_return_frac"] > 0]
        at_base = [r for r in rs if all(abs(r[a] - BASE_NOGATE[a]) < 1e-12 for a in BASE_NOGATE)]
        base_rets = [r["total_return_frac"] * 100 for r in at_base]
        signflip.append({"gates": dict(zip(GATES, v)), "runs": len(rs), "traded": len(tr), "profitable": len(pr),
                         "median_return_pct": med([r["total_return_frac"] * 100 for r in tr]),
                         "best_return_pct": max([r["total_return_frac"] * 100 for r in tr]) if tr else None,
                         "mean_trades": mean([r["trades"] for r in tr]),
                         "base_series": {f"{r['symbol']}|{r['perp_source']}": {"return_pct": r["total_return_frac"] * 100, "trades": int(r["trades"]),
                                                                          "periods": int(r["periods_in_position"])} for r in at_base},
                         "base_median_return_pct": med(base_rets), "base_sum_return_pct": sum(base_rets) if base_rets else None,
                         "base_profitable": sum(1 for x in base_rets if x > 0), "base_n": len(base_rets),
                         "base_mean_trades": mean([r["trades"] for r in at_base])})
    by_set = collections.defaultdict(list)
    for r in ok:
        by_set[tuple(r[a] for a in AXES)].append(r)
    n_series = len({(r["symbol"], r["perp_source"]) for r in ok})
    best_sets = []
    for k, rs in by_set.items():
        if len(rs) != n_series:
            continue
        rets = [r["total_return_frac"] * 100 for r in rs]
        best_sets.append({"params": dict(zip(AXES, k)), "sum_return_pct": sum(rets), "median_return_pct": med(rets),
                          "profitable": sum(1 for x in rets if x > 0), "n": len(rets), "mean_trades": mean([r["trades"] for r in rs]),
                          "series": {f"{r['symbol']}|{r['perp_source']}": {"return_pct": r["total_return_frac"] * 100, "trades": int(r["trades"])} for r in rs}})
    best_sets.sort(key=lambda b: -b["sum_return_pct"])
    current_set = next((b for b in best_sets if all(abs(b["params"][a] - BASE[a]) < 1e-12 for a in AXES)), None)
    prev_set = next((b for b in best_sets if all(abs(b["params"][a] - PREV[a]) < 1e-12 for a in AXES)), None)
    morning_set = next((b for b in best_sets if all(abs(b["params"][a] - MORNING[a]) < 1e-12 for a in AXES)), None)
    # How much of the corpus the basis exit could actually be judged on. Every
    # run through step 3.3 reported "not evaluable" at every settlement, so a
    # report that does not carry this number cannot say whether the rule was
    # tested or merely silent.
    # Counted over the SHIPPED SET only — one run per series. Summing across the
    # whole grid would multiply every settlement by the 12,288 parameter sets
    # that replayed it and report hundreds of millions of settlements for a
    # corpus that holds tens of thousands.
    basis_runs = [r for r in ok if is_base(r)] or ok
    basis = {"runs": len(basis_runs),
             "over_shipped_set": bool([r for r in ok if is_base(r)]),
             "runs_fully_evaluable": sum(1 for r in basis_runs if r["basis_not_evaluable"] == 0),
             "settlements_evaluable": sum(int(r.get("basis_evaluable", 0)) for r in basis_runs),
             "evaluations_not_evaluable": sum(int(r["basis_not_evaluable"]) for r in basis_runs),
             "entered_without_basis": sum(int(r.get("entered_without_basis", 0)) for r in basis_runs),
             "series_fully_evaluable": len({(r["symbol"], r["perp_source"]) for r in basis_runs
                                            if r["basis_not_evaluable"] == 0}),
             "series": len({(r["symbol"], r["perp_source"]) for r in basis_runs})}
    # The denominator every summed figure has to be divided by before it can be
    # read as a return. A set's "sum" adds 24 series each measured on its OWN
    # full notional, so it is a ranking device and nothing else; the mean is the
    # same number for ranking (a monotone transform, since every complete set
    # covers the same series) and is the one that can be compared against a
    # target APR. Capital is what both legs tie up — the spot leg cannot be
    # levered — so it is N*(1+f), reported here so the page never guesses it.
    caps = collections.Counter(r.get("capital_per_notional_frac", 2.0) for r in ok)
    capital = {"per_notional": caps.most_common(1)[0][0] if caps else 2.0,
               "unique": len(caps) == 1, "series": len(series_out)}
    ht_sum = sum(x["corpus"]["hold_through_net_pct"] for x in series_out)
    set_stats = {"n": len(best_sets), "positive": sum(1 for b in best_sets if b["sum_return_pct"] > 0),
                 "at_or_above_hold_through": sum(1 for b in best_sets if b["sum_return_pct"] >= ht_sum),
                 "max_sum_return_pct": best_sets[0]["sum_return_pct"] if best_sets else None,
                 "hold_through_sum_pct": ht_sum,
                 "hold_through_profitable": sum(1 for x in series_out if x["corpus"]["hold_through_net_pct"] > 0)}
    best_sets = best_sets[:8]
    apr_vals = [r["realized_apr_frac"] * 100 for r in traded]
    ret_vals = [r["total_return_frac"] * 100 for r in traded]
    top = sorted(traded, key=lambda r: -r["realized_apr_frac"])[:20]
    worst = sorted(traded, key=lambda r: r["realized_apr_frac"])[:5]

    # The three trade populations the report shows, filled in ONE streaming pass
    # so the rows are never all in memory at once. best_run is only known here,
    # after the runs have been ranked — which is why the pass happens now and
    # not at load time.
    best_run = top[0] if top else None
    acc_all, acc_base, acc_best = TradeAcc(), TradeAcc(), TradeAcc()
    groups = [(lambda t: True, acc_all), (is_base, acc_base)]
    if best_run:
        groups.append((lambda t: all(abs(t[a] - best_run[a]) < 1e-12 for a in AXES)
                       and t["symbol"] == best_run["symbol"] and t["perp_source"] == best_run["perp_source"],
                       acc_best))
    stream_trades(trades_path, groups)

    return {
        "window_from_ms": from_ms, "window_to_ms": to_ms, "window_days": runs[0]["window_days"],
        "book_sampled_at_ms": {"min": min(r["cost_book_sampled_at_ms"] for r in ok), "max": max(r["cost_book_sampled_at_ms"] for r in ok)} if ok else None,
        "counts": {"total": len(runs), "refused": len(refused), "ran": len(ok), "traded": len(traded),
                   "profitable": len(profitable), "untraded": len(ok) - len(traded)},
        "refusals": [{"symbol": k[0], "perp": k[1], "reason_vi": v} for k, v in refusals.items()],
        "series": series_out,
        "by_perp": by_perp,
        "axes": axes,
        "signflip": signflip,
        "best_sets": best_sets, "current_set": current_set, "prev_set": prev_set,
        "morning_set": morning_set, "set_stats": set_stats, "basis": basis, "capital": capital,
        "apr_stats": {"min": min(apr_vals) if apr_vals else None, "max": max(apr_vals) if apr_vals else None,
                      "median": med(apr_vals)},
        "return_stats": {"min": min(ret_vals) if ret_vals else None, "max": max(ret_vals) if ret_vals else None,
                         "median": med(ret_vals)},
        "top": [run_row(r) for r in top], "worst": [run_row(r) for r in worst],
        "trades_all": acc_all.block(), "trades_base": acc_base.block(), "trades_best": acc_best.block(),
        "assumptions_vi": next((r["assumptions_vi"].split(" | ") for r in ok if r["assumptions_vi"]), []),
    }


def fee_schedule(path):
    out, cur = [], None
    for line in open(path, encoding="utf-8"):
        s = line.strip()
        m = re.match(r"- source:\s*(\S+)", s)
        if m:
            cur = {"source": m.group(1)}
            out.append(cur)
            continue
        if cur is None:
            continue
        for key in ("market_type", "quote_asset", "maker_bps", "taker_bps", "verified", "tradable"):
            m = re.match(rf"{key}:\s*([^\s#]+)", s)
            if m and key not in cur:
                cur[key] = m.group(1).strip('"')
    return out


def skipped_from_log(path):
    out = []
    for line in open(path, encoding="utf-8"):
        m = re.search(r"bỏ qua (\S+)/(\S+): (.*)$", line.strip())
        if m:
            out.append({"symbol": m.group(1), "perp": m.group(2), "reason_vi": m.group(3)})
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", default="data/scanner.db")
    ap.add_argument("--config", default="config.yaml")
    ap.add_argument("--window", action="append", required=True, help="label=runs.csv:trades.csv[:stderr.log]")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    db = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    windows, skipped = collections.OrderedDict(), []
    grid = None
    for spec in args.window:
        label, paths = spec.split("=", 1)
        parts = paths.split(":")
        runs = load(parts[0], NUM)
        trades_path = parts[1] if len(parts) > 1 and parts[1] else None
        if len(parts) > 2:
            skipped = skipped_from_log(parts[2]) or skipped
        windows[label] = window_summary(runs, trades_path, db)
        if grid is None:
            grid = {a: sorted({r[a] for r in runs}) for a in AXES}
            grid["param_sets"] = len({tuple(r[a] for a in AXES) for r in runs})
            grid["series"] = len({(r["symbol"], r["perp_source"]) for r in runs})
    json.dump({"generated_at_ms": int(time.time() * 1000), "grid": grid, "windows": windows,
               "skipped_by_mapping": skipped, "fees": fee_schedule(args.config)},
              open(args.out, "w", encoding="utf-8"), ensure_ascii=False, indent=1)
    for label, w in windows.items():
        print(label, w["counts"], "apr", w["apr_stats"], "trades", w["trades_all"].get("n"))


if __name__ == "__main__":
    main()
