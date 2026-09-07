#!/usr/bin/env python3
"""Shrink the sweep CSVs before analyze.py loads them.

The 12,288-set grid writes 5.5 GB across six files, and analyze.py holds every
row as a dict — the first attempt reached 8.2 GB RSS on wide12.csv alone and
had to be killed. Almost all of that bulk is two text columns repeated on every
row: assumptions_vi (~1.5 KB, identical for every run of a window) and
exit_reason_vi (a full sentence per trade).

So: keep assumptions_vi on the first OK row only, which is the one analyze.py
reads it from, and replace exit_reason_vi with the class analyze.py would have
derived from it. Streaming, one row at a time, nothing else touched — the
numbers analyze.py sees are byte-identical to the originals.
"""
import csv, sys


def classify(s):
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


def slim(src, dst, kind):
    kept = False
    with open(src, encoding="utf-8", newline="") as fi, open(dst, "w", encoding="utf-8", newline="") as fo:
        rd = csv.DictReader(fi)
        wr = csv.DictWriter(fo, fieldnames=rd.fieldnames)
        wr.writeheader()
        n = 0
        for r in rd:
            if kind == "runs":
                if r.get("assumptions_vi"):
                    if kept:
                        r["assumptions_vi"] = ""
                    elif r["ok"] == "true":
                        kept = True
            else:
                r["exit_reason_vi"] = classify(r.get("exit_reason_vi", ""))
            wr.writerow(r)
            n += 1
    return n


if __name__ == "__main__":
    src, dst, kind = sys.argv[1], sys.argv[2], sys.argv[3]
    print(f"{dst}: {slim(src, dst, kind):,} rows")
