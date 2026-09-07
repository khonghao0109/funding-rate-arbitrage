#!/usr/bin/env python3
"""Measure what the basis exit costs, by replaying the shipped set twice.

Why two runs and not a sweep axis: cmd/backtest hardcodes MaxBasisPct and
MaxBasisWidenPct in baseParams, so -config does not reach a -sweep run. Only
the plain path (plainParams) reads them from the config's strategy block. So
the measurement is two PLAIN runs — one with config.yaml as it ships, one with
a copy whose two basis thresholds are raised until the rule can never fire —
and the difference between them is the rule's price.

The two runs must be adjacent in time. Round-trip cost is priced on the newest
measured order book, and the live scanner keeps writing new depth snapshots, so
runs hours apart differ for a reason that has nothing to do with the basis.
"""
import argparse, csv, json


def load(path):
    return [r for r in csv.DictReader(open(path, encoding="utf-8")) if r["ok"] == "true"]


def agg(rows):
    return {"sum_pct": sum(float(r["total_return_frac"]) for r in rows) * 100,
            "trades": sum(int(r["trades"]) for r in rows),
            "positive": sum(1 for r in rows if float(r["total_return_frac"]) > 0),
            "n": len(rows)}


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--on", required=True, help="CSV of the run with the shipped basis thresholds")
    ap.add_argument("--off", required=True, help="CSV of the run with the thresholds raised out of reach")
    ap.add_argument("--out", required=True)
    a = ap.parse_args()

    on, off = load(a.on), load(a.off)
    key = lambda r: (r["symbol"], r["perp_source"])
    ion = {key(r): (float(r["total_return_frac"]) * 100, int(r["trades"])) for r in on}
    ioff = {key(r): (float(r["total_return_frac"]) * 100, int(r["trades"])) for r in off}
    if set(ion) != set(ioff):
        raise SystemExit("the two runs replayed different series; they are not comparable")

    rows = [{"symbol": k[0], "perp": k[1],
             "off_pct": ioff[k][0], "off_trades": ioff[k][1],
             "on_pct": ion[k][0], "on_trades": ion[k][1],
             "delta_pp": ion[k][0] - ioff[k][0]}
            for k in sorted(ion) if abs(ion[k][0] - ioff[k][0]) > 1e-9]

    out = {"on": agg(on), "off": agg(off), "rows": rows,
           "cost_pp": agg(on)["sum_pct"] - agg(off)["sum_pct"],
           "extra_trades": agg(on)["trades"] - agg(off)["trades"]}
    json.dump(out, open(a.out, "w", encoding="utf-8"), ensure_ascii=False, indent=1)
    print(f"basis exit costs {out['cost_pp']:+.2f}pp and {out['extra_trades']:+d} trades "
          f"across {len(rows)} affected series")


if __name__ == "__main__":
    main()
