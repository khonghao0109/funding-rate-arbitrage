#!/usr/bin/env python3
"""Screen candidate pairs: the corpus half joined to cmd/pairscreen's live half.

For every (symbol, perp source) cmd/pairscreen reported, read the funding the
pair actually paid over the window from funding_history (mode=ro) and measure
what a hold-through position would have made of it: mean rate, share of
positive settlements, sign flips a year, the worst negative episode, the
net of one round trip, and the break-even horizon (every settlement as an
entry, days until the funding collected since it reaches the round trip the
live books price). Then a VERDICT per row, each criterion named with the
number that decided it — a pair is never dropped silently.

The verdict is a report's, not a rule's: nothing here is a strategy entry
condition. It says which candidates are worth a backtest and why the rest
are not.
"""
import argparse
import collections
import datetime as dt
import json
import sqlite3
import statistics

HORIZONS = (30, 60, 90, 180)
DAY_MS = 86400000


def pctl(xs, q):
    if not xs:
        return None
    xs = sorted(xs)
    i = (len(xs) - 1) * q
    lo, hi = int(i), min(int(i) + 1, len(xs) - 1)
    return xs[lo] + (xs[hi] - xs[lo]) * (i - lo)


def horizon(ts, rates, cost, to_ms):
    """Median days to break even over every entry, and the share of entries
    with >= D days of window left that reached it within D days."""
    n = len(ts)
    P = [0.0] * (n + 1)
    for i, r in enumerate(rates):
        P[i + 1] = P[i] + r
    days, left = [], []
    for k in range(n - 1):
        need, j, found = P[k + 1] + cost, k + 1, None
        while j < n:
            if P[j + 1] >= need:
                found = j
                break
            j += 1
        days.append((ts[found] - ts[k]) / DAY_MS if found is not None else None)
        left.append((to_ms - ts[k]) / DAY_MS)
    ok = [d for d in days if d is not None]
    return {
        "entries": len(days), "reached": len(ok), "median": pctl(ok, .5), "p75": pctl(ok, .75),
        "within": {str(d): (sum(1 for x, l in zip(days, left) if x is not None and x <= d and l >= d) /
                            max(1, sum(1 for l in left if l >= d))) for d in HORIZONS},
    }


def episodes(rates, per8h):
    """Negative runs: how many, the worst one's cumulative cost, and the
    number of sign changes — what the sign-flip exit would have faced."""
    flips, worst, run = 0, 0.0, 0.0
    prev = None
    for r, r8 in zip(rates, per8h):
        neg = r8 < 0
        if prev is not None and neg != prev:
            flips += 1
        prev = neg
        if neg:
            run += -r
            worst = max(worst, run)
        else:
            run = 0.0
    return flips, worst


def corpus_row(db, symbol, source, from_ms, to_ms, cost_frac):
    # No recorded_at_ms filter here, unlike hold.py: a candidate's corpus is
    # backfilled AFTER cmd/pairscreen took its snapshot, by construction, and
    # the filter silently dropped every Hyperliquid alt on the first run —
    # LINK read 0.425 bps/8h (binance) instead of 0.925 (hyperliquid). The
    # window is bounded by funding_at_ms, which is what matters for a mean.
    rows = db.execute("""SELECT funding_at_ms, rate_per_interval_frac, rate_per_8h_frac, interval_sec, model
                         FROM funding_history WHERE source=? AND symbol=? AND rate_type<>'Special'
                           AND funding_at_ms>=? AND funding_at_ms<? ORDER BY funding_at_ms""",
                      (source, symbol, from_ms, to_ms)).fetchall()
    if not rows:
        return None
    if rows[-1][4] != "discrete":
        return {"model": rows[-1][4], "settlements": len(rows)}
    ts = [r[0] for r in rows]
    rates = [r[1] for r in rows]
    r8 = [r[2] for r in rows]
    iv = rows[-1][3]
    covered = (ts[-1] - ts[0]) / DAY_MS + iv / 86400
    flips, worst = episodes(rates, r8)
    mean8 = statistics.mean(r8)
    out = {
        "model": "discrete", "settlements": len(rows), "interval_sec": iv, "covered_days": covered,
        "first_ms": ts[0], "last_ms": ts[-1],
        "mean_rate_per_8h_bps": mean8 * 1e4,
        "gross_apr_pct": mean8 * 3 * 365 * 100,
        "positive_share": sum(1 for r in rates if r > 0) / len(rates),
        "sign_flips_per_year": flips * 365 / covered if covered > 0 else None,
        "worst_negative_episode_pct": worst * 100,
        "hold_through_gross_pct": sum(rates[1:]) * 100,
        "p10_rate_per_8h_bps": pctl(r8, .1) * 1e4, "p90_rate_per_8h_bps": pctl(r8, .9) * 1e4,
    }
    if cost_frac is not None:
        out["hold_through_net_pct"] = (sum(rates[1:]) - cost_frac) * 100
        out["horizon"] = horizon(ts, rates, cost_frac, to_ms)
    return out


def worth_backtest(checks, corp, backtest_bps):
    """The weaker verdict, the one that admits a pair to config.yaml: every
    venue-side criterion and the corpus depth pass, and the pair pays at least
    backtest_bps — a bar set at 'more than every shipped major at a USDT
    venue', not at the 5% target. It says 'worth replaying with the shipped
    rules', nothing more."""
    by = {c["name"]: c["passed"] for c in checks}
    return all(by.get(k) for k in ("listed", "hedge_leg", "cost_priced", "book_fits", "corpus")) \
        and corp is not None and corp.get("model") == "discrete" and corp["mean_rate_per_8h_bps"] >= backtest_bps


def verdict(row, corp, notional, min_bps, min_cover_days, cap):
    """Named criteria, all evaluated, in the order they would stop a trade:
    listed → hedge leg → priced cost → book fits → corpus deep enough →
    funding pays. Returns (pass, [criterion: verdict + number])."""
    checks = []
    ok = True

    def check(name, passed, detail):
        nonlocal ok
        checks.append({"name": name, "passed": passed, "detail": detail})
        ok = ok and passed

    check("listed", row["Listed"], row["RefusalVI"] if not row["Listed"] else row["Perp"]["NativeSymbol"])
    if not row["Listed"]:
        return False, checks
    check("hedge_leg", bool(row["SpotSource"]), row["SpotSource"] or row["RefusalVI"])
    if not row["SpotSource"]:
        return False, checks
    cost = row["Cost"]
    check("cost_priced", cost["OK"], f"{cost['TotalPct']:.3f}% /notional" if cost["OK"] else cost["ReasonVI"])
    thin = row["MinDepthWithinWideQuote"]
    check("book_fits", thin >= notional, f"mỏng nhất {thin:,.0f} trong ±0,5% so với {notional:,.0f}")
    if corp is None:
        check("corpus", False, "chưa có dòng funding nào trong cửa sổ — backfill chưa chạy hoặc sàn không có lịch sử")
        return False, checks
    if corp.get("model") != "discrete":
        check("corpus", False, f"model {corp.get('model')}: mẫu chỉ số, không phải mốc settle — không phát lại được")
        return False, checks
    check("corpus", corp["covered_days"] >= min_cover_days,
          f"{corp['covered_days']:.0f} ngày, {corp['settlements']} mốc (cần ≥ {min_cover_days})")
    ht_cap = corp.get("hold_through_net_pct")
    ht_cap = ht_cap / cap if ht_cap is not None else None
    check("funding_pays", corp["mean_rate_per_8h_bps"] >= min_bps,
          f"{corp['mean_rate_per_8h_bps']:.3f} bps/8h TB (cần ≥ {min_bps}); giữ suốt {ht_cap:+.2f}% /vốn" if ht_cap is not None
          else f"{corp['mean_rate_per_8h_bps']:.3f} bps/8h TB (cần ≥ {min_bps})")
    return ok, checks


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", required=True)
    ap.add_argument("--screen", required=True, help="cmd/pairscreen JSON")
    ap.add_argument("--months", type=int, default=12)
    ap.add_argument("--min-bps", type=float, default=0.94,
                    help="mean funding, bps per 8h, a pair needs for 5%% a year on capital at 2x notional after a 0.30%% round trip")
    ap.add_argument("--min-cover-days", type=float, default=150)
    ap.add_argument("--backtest-bps", type=float, default=0.4,
                    help="mean funding, bps per 8h, above which a pair that passes every venue-side criterion is worth a backtest — above every shipped major at a USDT venue (BTC·binance 0.31)")
    ap.add_argument("--capital-per-notional", type=float, default=2.0)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    screen = json.load(open(args.screen, encoding="utf-8"))
    to_ms = screen["GeneratedAtMs"]
    from_ms = int((dt.datetime.fromtimestamp(to_ms / 1000, dt.timezone.utc) - dt.timedelta(days=365 * args.months / 12)).timestamp() * 1000)
    db = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    notional = screen["NotionalQuote"]

    rows = []
    for r in screen["Rows"]:
        cost_frac = r["Cost"]["TotalPct"] / 100 if r["Listed"] and r["SpotSource"] and r["Cost"]["OK"] else None
        corp = corpus_row(db, r["Symbol"], r["PerpSource"], from_ms, to_ms, cost_frac) if r["Listed"] else None
        passed, checks = verdict(r, corp, notional, args.min_bps, args.min_cover_days, args.capital_per_notional)
        worth = worth_backtest(checks, corp, args.backtest_bps)
        rows.append({"worth_backtest": worth,"symbol": r["Symbol"], "perp": r["PerpSource"], "spot": r["SpotSource"], "bridged": r["QuoteBridged"],
                     "listed": r["Listed"], "refusal_vi": r["RefusalVI"],
                     "perp_native": r["Perp"]["NativeSymbol"], "perp_status": r["Perp"]["Status"],
                     "is_contract": r["Perp"]["IsContract"], "contract_size_coin": r["Perp"]["ContractSizeCoin"],
                     "min_notional_quote": r["Perp"]["MinNotionalQuote"], "max_qty_coin": r["Perp"]["MaxQtyCoin"],
                     "max_leverage_x": r["Perp"]["MaxLeverageX"],
                     "cost": r["Cost"], "min_depth_wide_quote": r["MinDepthWithinWideQuote"],
                     "perp_book": {k: r["PerpBook"].get(k) for k in ("MidPriceQuote", "SpreadPct", "BidDepthWithinWideQuote", "AskDepthWithinWideQuote", "BidDepthWithinTightQuote", "AskDepthWithinTightQuote", "BidLevels", "AskLevels", "BidSpanPct", "AskSpanPct", "ErrVI")} if r["Listed"] else None,
                     "spot_book": {k: r["SpotBook"].get(k) for k in ("MidPriceQuote", "SpreadPct", "BidDepthWithinWideQuote", "AskDepthWithinWideQuote", "BidDepthWithinTightQuote", "AskDepthWithinTightQuote", "BidLevels", "AskLevels", "BidSpanPct", "AskSpanPct", "ErrVI")} if r["SpotSource"] else None,
                     "corpus": corp, "passed": passed, "checks": checks})

    # Per symbol: the best venue by mean funding among rows that passed, and
    # how many venues list / pair / pass — the summary a reader scans first.
    by_symbol = collections.OrderedDict()
    for r in rows:
        s = by_symbol.setdefault(r["symbol"], {"symbol": r["symbol"], "venues_listed": 0, "venues_paired": 0, "venues_passed": 0,
                                                "venues_worth": 0, "worth_unbridged": 0, "best": None, "best_unbridged": None, "rows": []})
        s["rows"].append(r)
        s["venues_listed"] += r["listed"]
        s["venues_paired"] += bool(r["spot"])
        s["venues_passed"] += r["passed"]
        s["venues_worth"] += r["worth_backtest"]
        s["worth_unbridged"] += r["worth_backtest"] and not r["bridged"]
        c = r.get("corpus") or {}
        if r["listed"] and r["spot"] and c.get("model") == "discrete":
            # A mean over 96 days of OKX is not comparable with a mean over
            # 365 days elsewhere, so a venue with a deep-enough corpus beats
            # one without whatever its number; among comparable venues the
            # higher mean wins. hold-through is None, not 0, when the trip
            # could not be priced.
            deep = c["covered_days"] >= args.min_cover_days
            ht = c.get("hold_through_net_pct")
            cand = {"perp": r["perp"], "mean_bps": c["mean_rate_per_8h_bps"], "covered_days": c["covered_days"], "deep": deep,
                    "ht_net_cap_pct": ht / args.capital_per_notional if ht is not None else None,
                    "positive_share": c["positive_share"],
                    "passed": r["passed"], "worth": r["worth_backtest"], "bridged": r["bridged"]}
            better = lambda a, b: b is None or (a["deep"], a["mean_bps"]) > (b["deep"], b["mean_bps"])
            if better(cand, s["best"]):
                s["best"] = cand
            if not r["bridged"] and better(cand, s["best_unbridged"]):
                s["best_unbridged"] = cand
    summary = sorted(by_symbol.values(), key=lambda s: -(s["best"]["mean_bps"] if s["best"] else -1e9))
    for s in summary:
        del s["rows"]

    json.dump({"generated_at_ms": to_ms, "window_from_ms": from_ms, "window_to_ms": to_ms, "notional": notional,
               "criteria": {"min_bps": args.min_bps, "min_cover_days": args.min_cover_days, "capital_per_notional": args.capital_per_notional,
                            "backtest_bps": args.backtest_bps},
               "rows": rows, "symbols": summary, "rejections": screen.get("Rejections", [])},
              open(args.out, "w", encoding="utf-8"), ensure_ascii=False)
    passed = [r for r in rows if r["passed"]]
    print(f"{len(rows)} rows, {sum(1 for r in rows if r['listed'])} listed, {sum(1 for r in rows if r['spot'])} paired, "
          f"{len(passed)} pass, {sum(1 for r in rows if r['worth_backtest'])} worth a backtest")
    for s in summary:
        b = s["best"]
        print(f"  {s['symbol']:9s} listed {s['venues_listed']}/7 paired {s['venues_paired']} pass {s['venues_passed']} worth {s['venues_worth']}({s['worth_unbridged']} unbridged)  best: "
              + (f"{b['perp']:20s} {b['mean_bps']:6.3f} bps/8h over {b['covered_days']:.0f}d  HT {('%+.2f%%' % b['ht_net_cap_pct']) if b['ht_net_cap_pct'] is not None else '—':>7s} /vốn {'(USD-bridged)' if b['bridged'] else ''}" if b else "—"))


if __name__ == "__main__":
    main()
