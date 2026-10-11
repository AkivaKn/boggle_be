#!/usr/bin/env python3
"""Build data/opentdb.csv from the OpenTDB mirror (QuartzWarrior/OTDB-Source).

The mirror ships one URL-encoded, two-column CSV per OpenTDB category
(`<question>,<answer>`); the category is the FILE NAME. Internal commas inside
a question are percent-encoded there, so splitting on the first comma is safe.

We keep only the categories the casual blacklist allows (see
models.CasualBlacklistOpentdb*): every `Entertainment:*` category, `Celebrities`
and `Science: Gadgets` are dropped, plus `Mythology` (an explicit product
decision for the Millionaire easy band).

Usage: python3 data/opentdb_build.py <mirror-dir> <out.csv>
"""
import csv
import os
import sys
import urllib.parse

# Categories kept. Everything in the mirror that is NOT listed here is
# blacklisted (Entertainment:*, Celebrities, Science: Gadgets) or Mythology.
KEEP = [
    "Animals",
    "Art",
    "General Knowledge",
    "Geography",
    "History",
    "Politics",
    "Science & Nature",
    "Science: Computers",
    "Science: Mathematics",
    "Sports",
    "Vehicles",
]


def main() -> int:
    mirror, out_path = sys.argv[1], sys.argv[2]
    rows = []
    for category in KEEP:
        path = os.path.join(mirror, category + ".csv")
        with open(path, encoding="utf-8") as fh:
            for line in fh:
                line = line.rstrip("\n").rstrip("\r")
                if not line.strip():
                    continue
                question, _, answer = line.partition(",")
                q = urllib.parse.unquote(question).strip()
                a = urllib.parse.unquote(answer).strip()
                if not q or not a:
                    continue
                # The mirror keeps only question+answer, so a True/False row
                # has no distractor pool of its own and cannot be rebuilt as
                # a four-option question (the schema requires exactly 4).
                if a.lower() in ("true", "false", "yes", "no"):
                    continue
                rows.append((q, a, category))
    # Dedupe on the question text (the DB does the same, normalized).
    seen, uniq = set(), []
    for q, a, c in rows:
        if q in seen:
            continue
        seen.add(q)
        uniq.append((q, a, c))
    with open(out_path, "w", encoding="utf-8", newline="") as fh:
        w = csv.writer(fh, quoting=csv.QUOTE_ALL)
        w.writerow(["question", "answer", "category"])
        w.writerows(uniq)
    print(f"wrote {len(uniq)} rows to {out_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())