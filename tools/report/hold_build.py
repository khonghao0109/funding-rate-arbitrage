#!/usr/bin/env python3
"""Render ONE analysis JSON (hold.py, pairscreen.py) through its template into
a self-contained bilingual page. Every sentence on the page is written in the template from the
JSON's numbers; this script only injects the data and the run's provenance."""
import argparse
import datetime
import json
import pathlib
import re

HERE = pathlib.Path(__file__).resolve().parent
ap = argparse.ArgumentParser(description=__doc__)
ap.add_argument("--json", required=True, help="hold.py output")
ap.add_argument("--template", default=str(HERE / "hold.template.html"))
ap.add_argument("--out", action="append", required=True, help="output path (repeatable)")
ap.add_argument("--commit", required=True, help="the commit the sweep binary was built from")
ap.add_argument("--title", help="static <title>; defaults to the template's")
args = ap.parse_args()

data = json.load(open(args.json, encoding="utf-8"))
meta = {"commit": args.commit, "generated": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d %H:%M UTC")}
tpl = open(args.template, encoding="utf-8").read()
out = (tpl.replace("/*__DATA__*/", json.dumps(data, ensure_ascii=False, separators=(",", ":")))
          .replace("/*__META__*/", json.dumps(meta, ensure_ascii=False)))
if args.title:
    # Whatever <title> the template carries; the same builder serves every
    # template that takes ONE JSON (hold.template.html, pairscreen.template.html).
    out = re.sub(r"<title>.*?</title>", f"<title>{args.title}</title>", out, count=1)
for path in args.out:
    # A new file each run, never an overwrite: an older report is the record of
    # what was known that day.
    if pathlib.Path(path).exists():
        raise SystemExit(f"{path} exists — reports are never overwritten; pick a new name")
    open(path, "w", encoding="utf-8").write(out)
    print(f"{path}: {len(out):,} bytes")
