#!/usr/bin/env python3
"""Turn a -perp-margin sweep CSV into the report's leverage block.

Two things this has to get right, both of which an earlier reading got wrong:

 1. Compare LIKE WITH LIKE. Switching the margin model on makes strategy refuse
    entry on any venue whose maintenance bracket is unverified (binance,
    hyperliquid), so those series trade at f=0 and not at all elsewhere.
    Averaging over "whatever traded" then compares 24 series against 16 and
    calls the difference leverage. Only series that trade at EVERY level count.

 2. Use the CAPITAL denominator. The CSV's total_return_frac is a fraction of
    notional, and notional does not move when leverage does — so that column
    measures leverage's cost and can never measure its benefit. Capital is
    N*(1+f) because the spot leg cannot be levered, which caps the whole
    benefit at 2/(1+f).

The venue liquidation fee is charged here rather than in the engine: what the
venue keeps on a liquidation is roughly the maintenance margin left on the
position, and that IS a real loss, unlike the posted margin (which the spot
leg's gain offsets — see the report's own note).
"""
import argparse, collections, csv, datetime, json, re, sys


def maintenance(config_path):
    """Read each source's VERIFIED maintenance rate out of config.yaml.

    Line-based like analyze.py's fee_schedule: PyYAML is not installed here and
    a report generator is not a reason to add a dependency to the machine that
    also runs the live process. An unverified bracket is deliberately absent
    rather than 0 — the same refusal internal/risk makes.
    """
    out, cur, inm = {}, None, False
    for line in open(config_path, encoding="utf-8"):
        m = re.match(r"\s*- source:\s*(\S+)", line)
        if m:
            cur, inm = {"source": m.group(1)}, False
            continue
        if cur is None:
            continue
        if re.match(r"\s{4}margin:\s*$", line):
            inm = True
            continue
        if re.match(r"\s{0,4}\S", line) and not re.match(r"\s{6}", line):
            inm = False
        if not inm:
            continue
        m = re.match(r"\s+maintenance_frac:\s*([0-9.]+)", line)
        if m:
            cur["frac"] = float(m.group(1))
        if re.match(r"\s+verified:\s*true", line) and cur.get("frac"):
            out[cur["source"]] = cur["frac"]
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--csv", required=True)
    ap.add_argument("--config", default="config.yaml")
    ap.add_argument("--out", required=True)
    a = ap.parse_args()

    mm = maintenance(a.config)
    rows = [r for r in csv.DictReader(open(a.csv, encoding="utf-8")) if r["ok"] == "true"]
    by_f = collections.defaultdict(dict)
    for r in rows:
        by_f[float(r["perp_margin_frac"])][(r["symbol"], r["perp_source"])] = r
    levels = sorted(by_f, reverse=True)
    if 0.0 not in by_f:
        sys.exit("the sweep has no f=0 row to compare against")
    common = [k for k in by_f[0.0] if all(int(by_f[f][k]["trades"]) > 0 for f in levels)]
    if not common:
        sys.exit("no series trades at every leverage level")

    out_rows, off_after = [], None
    for f in levels:
        rs = [by_f[f][k] for k in common]
        notional = sum(float(r["total_return_frac"]) for r in rs) * 100
        cap = float(rs[0]["capital_per_notional_frac"])
        oncap = sum(float(r["total_return_on_capital_frac"]) for r in rs) * 100
        fee = sum(int(r["liquidations"]) * mm[r["perp_source"]] for r in rs) * 100
        after = (notional - fee) / cap
        liq = sum(int(r["liquidations"]) for r in rs)
        if f == 0.0:
            off_after = after
        out_rows.append({
            "off": f == 0.0, "f": f, "label": "—" if f == 0 else f"{1/f:.0f}x",
            # A short opened at P0 is closed at P0*(1+f)/(1+m); reported against
            # the strictest verified bracket in play so the figure is not
            # quoted from the friendliest venue.
            "liq_at_pct": None if f == 0 else ((1 + f) / (1 + max(mm[r["perp_source"]] for r in rs)) - 1) * 100,
            "notional_pct": notional, "capital": cap, "oncap_pct": oncap,
            "fee_pp": fee, "after_pct": after, "liquidations": liq,
            "ceiling": 2 / cap,
        })
    for r in out_rows:
        r["vs"] = None if r["off"] else (r["after_pct"] / off_after if off_after else None)

    levered = [r for r in out_rows if not r["off"]]
    best = max(levered, key=lambda r: r["after_pct"])
    book = max(int(float(r["cost_book_sampled_at_ms"])) for r in rows)
    import datetime
    json.dump({
        "series": len(common), "rows": out_rows,
        "book": datetime.datetime.fromtimestamp(book / 1000, datetime.timezone.utc).strftime("%Y-%m-%d %H:%M UTC"),
        "best_label": best["label"], "best_gain_pp": best["after_pct"] - off_after,
        # vs is what leverage ACHIEVED against no leverage; ceiling is what it
        # could have achieved at zero liquidations. Keeping them apart is the
        # whole point of the section — collapsing them was the original error.
        "best_vs": best["vs"], "best_ceiling": best["ceiling"],
        "best_liquidations": best["liquidations"],
        "sources": sorted({k[1] for k in common}),
    }, open(a.out, "w", encoding="utf-8"), ensure_ascii=False, indent=1)
    print(f"{len(common)} series, best {best['label']} {best['after_pct']:.2f}% vs off {off_after:.2f}%")


if __name__ == "__main__":
    main()
