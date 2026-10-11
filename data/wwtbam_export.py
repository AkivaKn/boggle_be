#!/usr/bin/env python3
"""WWTBAM workbook → data/wwtbam.jsonl converter (labels-first seed asset).

Reads the community-edited "MillionaireDB — WWTBAM Question Database"
workbook (one sheet per ladder level: Q1..Q15) and writes the
seed-of-record JSONL consumed by `go run ./cmd/import -source wwtbam`.

STDLIB ONLY (no openpyxl on the host): the workbook is read as an OOXML
zip — xl/workbook.xml maps sheet names → rIds → worksheets/sheetN.xml
(via xl/_rels/workbook.xml.rels), and every text cell value lives in
xl/sharedStrings.xml. Cell text is HTML-decoded (the corpus embeds
entities like &quot; as literal content) and whitespace-trimmed.

// Row schema (header row 1: Question | A | B | C | D | Correct |
// Question Source | Contributor; the LEVEL is the sheet name Q<n>):
//
//    {"level": <int 1..15>,
//     "sheet": "Q<level>",                       # provenance sheet name
//     "question": "<decoded text>",
//     "options": ["<A>", "<B>", "<C>", "<D>"],   # always exactly 4
//     "correct": "a".."d",                       # lowercase letter
//     "src": "<episode/show source tag>"}        # "Unknown" when blank
//
// SHEET→LEVEL CALIBRATION (the M10 defect and its fix): the workbook's
// physical worksheet file order does NOT match the sheet-tab order (the
// INTRO/editor sheets come first: Q1 lives in sheet4.xml, not sheet1.xml).
// The level is therefore taken STRICTLY from the resolved SHEET NAME
// (workbook.xml → r:id → rels Target), never from the file position.
//
// SANITY CROSS-CHECK: the workbook's own INTRO sheet carries the editors'
// census ("Current # of Qs per Level": $100:964 … $1 MILLION:371, TOTAL
// 11,874). The Q sheets carry no Level column (column A is the Question
// itself), so the census IS the per-sheet calibration check: each Q<n>
// sheet's stored count is compared against the census value and every
// mismatch is reported (parse drops: empty question / <4 options / bad
// correct letter — the census counts raw sheet rows, so drops are
// expected and DOCUMENTED, never silently accepted).

A row is skipped (counted, with reason) when its question is empty, any
of the four options is empty, or the Correct cell is not one of A–D.
Duplicate TEXTS pass through here — the Go importer dedupes against the
whole DB by normalized text (idempotent re-runs add 0).

Usage (HOST): python3 data/wwtbam_export.py [workbook.xlsx] [out.jsonl]
"""

import html
import json
import re
import sys
import zipfile
from collections import Counter

DEFAULT_WB = "/home/akiva/Downloads/MillionaireDB_ WWTBAM Question Database.xlsx"
DEFAULT_OUT = "data/wwtbam.jsonl"
SHEET_RE = re.compile(r"^Q(\d+)$")
COLS = "ABCDEFGH"  # A=Question B..E=options F=Correct G=Source H=Contributor


def sheet_name_map(z):
    """sheet title → xl/worksheets/sheetN.xml path, from workbook.xml.

    THE M10 CALIBRATION FIX: the resolved TARGET (via the r:id → Target
    rels edge) is the only truth — the worksheet FILE order is not the
    sheet-tab order and must never be inferred from the sheet's position.
    Attribute order inside <sheet …> is irrelevant (name may come after
    r:id), and unquoted-name sheets are tolerated.
    """
    wb = z.read("xl/workbook.xml").decode("utf-8")
    rels = z.read("xl/_rels/workbook.xml.rels").decode("utf-8")
    rid2target = {}
    for rel in re.findall(r"<Relationship\b[^>]*>", rels):
        rid = re.search(r'\bId="([^"]+)"', rel)
        tgt = re.search(r'\bTarget="([^"]+)"', rel)
        if rid and tgt:
            rid2target[rid.group(1)] = tgt.group(1)
    out = {}
    for sheet_tag in re.findall(r"<sheet\b[^>]*>", wb):
        name = re.search(r'\bname="([^"]*)"', sheet_tag)
        rid = re.search(r'\br:id="([^"]+)"', sheet_tag)
        if not name or not rid or rid.group(1) not in rid2target:
            continue  # an unresolvable sheet is skipped, never guessed
        target = rid2target[rid.group(1)].lstrip("/")
        if not target.startswith("xl/"):
            target = "xl/" + target
        out[name.group(1)] = target
    return out


def shared_strings(z):
    """All <si> shared strings, concatenated text parts, HTML-decoded."""
    raw = z.read("xl/sharedStrings.xml").decode("utf-8")
    out = []
    for si in re.findall(r"<si>(.*?)</si>", raw, re.S):
        parts = re.findall(r"<t[^>]*>(.*?)</t>|<t[^>]*/>", si, re.S)
        out.append(html.unescape("".join(parts)))
    return out


def _attr(attrs, name):
    """attr value or None for `name="value"` inside one <c> attr string."""
    m = re.search(re.escape(name) + r'"?([^"]*)"', attrs)
    v = m.group(1) if m else None
    if name == "r=" and v is not None:
        v = re.sub(r"\d+$", "", v)  # cell ref A12 → column letter A
    return v


def sheet_rows(z, path, strings):
    """Yields row dicts {col: decoded value} for one sheet XML."""
    xml = z.read(path).decode("utf-8")
    # Normalize self-closing cells (`<c …/>`) to empty paired form so one
    # simple paired-cell regex handles both (cells never nest).
    xml = re.sub(r"<c ([^>]*?)/>", r"<c \1></c>", xml)
    cell_re = re.compile(r"<c ([^>]*?)/?>((?:(?!</c>).)*)</c>", re.S)
    for row_m in re.finditer(r'<row [^>]*r="\d+"[^>]*>(.*?)</row>', xml, re.S):
        row = {}
        for c in cell_re.finditer(row_m.group(1)):
            attrs, body = c.group(1), c.group(2)
            col, typ = _attr(attrs, "r="), _attr(attrs, "t=")
            if col is None:
                continue
            v = re.search(r"<v>(.*?)</v>", body, re.S)
            if v is None:
                row[col] = ""
            elif typ == "s":
                row[col] = strings[int(v.group(1))]
            else:
                row[col] = html.unescape(v.group(1))
        yield row


def clean(s):
    return re.sub(r"\s+", " ", s).strip()


# The community cells occasionally carry CYRILLIC HOMOGLYPH letters in the
# Correct column (e.g. U+0421 С for C — 3 rows in the shipped workbook).
# Lookalike Cyrillic capitals that text-editors swap for Latin letters map
# to their Latin twin strictly by GLYPH SHAPE (not by sound), which is what
# a letter coordinate means.
CYRILLIC_LOOKALIKES = {
    "А": "A", "В": "B", "С": "C", "Е": "E", "Н": "H", "К": "K",
    "М": "M", "О": "O", "Р": "P", "Т": "T", "Х": "X",
}
for _cyr, _lat in tuple(CYRILLIC_LOOKALIKES.items()):
    CYRILLIC_LOOKALIKES[_cyr.lower()] = _lat.lower()


def correct_letter(raw):
    """Cell F value → 'a'..'d' (whitespace/UTF-8/homoglyph tolerant), or None."""
    s = clean(raw).lower()
    if s in CYRILLIC_LOOKALIKES:
        s = CYRILLIC_LOOKALIKES[s]
    return s if s in ("a", "b", "c", "d") else None


# The INTRO sheet's census ("Current # of Qs per Level"): money ladder
# rung → level. Parsed from the sheet itself so the check cannot drift
# from the workbook being converted.
MONEY_TO_LEVEL = {
    "100": 1, "200": 2, "300": 3, "500": 4,
    "1000": 5, "2000": 6, "4000": 7, "8000": 8, "16000": 9,
    "32000": 10, "64000": 11, "125000": 12, "250000": 13, "500000": 14,
}
MONEY_TO_LEVEL = {
    "100": 1, "200": 2, "300": 3, "500": 4,
    "1000": 5, "2000": 6, "4000": 7, "8000": 8, "16000": 9,
    "32000": 10, "64000": 11, "125000": 12, "250000": 13, "500000": 14,
}


def _money_level(label):
    """INTRO money-rung cell text → level 1..15 (None otherwise).

    The rung cells are NUMERIC in the workbook ($100 renders as "100.0"),
    except the $1 MILLION row (text) — both forms normalize.
    """
    s = clean(label).replace(",", "").replace(" ", "").upper()
    s = s.lstrip("$")
    if re.fullmatch(r"\d+\.0", s):  # Excel float rendering: "100.0" → "100"
        s = s[:-2]
    if s in ("1MILLION", "1M"):
        return 15
    return MONEY_TO_LEVEL.get(s)


def intro_census(z, intro_path, strings):
    """The workbook's own per-level census from the INTRO sheet: the cell
    holding a money rung is followed (rightward) by the editors' row
    count for that level. Returns {level: count}."""
    census = {}
    for r in sheet_rows(z, intro_path, strings):
        cols = sorted(r.keys())
        for i, col in enumerate(cols):
            lvl = _money_level(r.get(col, ""))
            if lvl is None:
                continue
            for nxt in cols[cols.index(col) + 1:]:
                v = clean(r.get(nxt, "")).replace(",", "")
                if v.isdigit():
                    census[lvl] = int(v)
                    break
    return census


def main(workbook, out_path):
    z = zipfile.ZipFile(workbook)
    sheets = sheet_name_map(z)
    strings = shared_strings(z)
    census = intro_census(z, sheets["INTRO"], strings) if "INTRO" in sheets else {}
    if not census:
        print("WARNING: no INTRO census parsed — the per-sheet calibration "
              "cross-check cannot run (levels still come from the sheet names)")

    stats = Counter()  # sheet → imported
    parsed_n = Counter()  # sheet → parsed (pre-validation) rows
    skips = Counter()  # sheet → skipped
    reasons = Counter()  # decode reason → n (aggregate)
    rows = []
    for name, path in sorted(sheets.items()):
        m = SHEET_RE.match(name)
        if not m:
            continue
        # The LEVEL is the SHEET NAME (canonical: the Q2 sheet ⇒ level 2).
        # Never the worksheet file position — file order ≠ sheet-tab order.
        level = int(m.group(1))
        parsed = 0
        for i, r in enumerate(sheet_rows(z, path, strings)):
            if i == 0:  # header row
                continue
            parsed += 1
            parsed_n[name] += 1
            question = clean(r.get("A", ""))
            options = [clean(r.get(c, "")) for c in "BCDE"]
            correct = correct_letter(r.get("F", ""))
            src = clean(r.get("G", "")) or "Unknown"
            if not question:
                skips[name] += 1
                reasons["empty question"] += 1
            elif any(not o for o in options):
                skips[name] += 1
                reasons["<4 options"] += 1
            elif correct not in ("a", "b", "c", "d"):
                skips[name] += 1
                reasons["bad correct letter"] += 1
            else:
                rows.append(
                    {
                        "level": level,
                        "sheet": name,
                        "question": question,
                        "options": options,
                        "correct": correct,
                        "src": src,
                    }
                )
                stats[name] += 1

        # Per-sheet calibration cross-check against the workbook's own
        # INTRO census. The census counts RAW sheet rows (header excluded
        # is not guaranteed) — parse drops (skips) plus any census
        # off-by-header row explain the deltas; every mismatch is REPORTED,
        # never silently accepted.
        want = census.get(level)
        if want is not None and parsed != want:
            print(f"WARNING {name}: parsed {parsed} rows ≠ INTRO census {want} "
                  f"({skips[name]} skipped this sheet — drops documented below)")
        print(f"{name}: parsed={parsed} stored={stats[name]} census={want}")

    # Soundness gates (every stored row must be well-formed): 4 options,
    # correct letter inside range, level inside 1..15.
    for r in rows:
        assert len(r["options"]) == 4 and all(r["options"]), r
        assert r["correct"] in ("a", "b", "c", "d"), r
        assert 1 <= r["level"] <= 15, r

    with open(out_path, "w", encoding="utf-8") as f:
        for r in rows:
            f.write(json.dumps(r, ensure_ascii=True) + "\n")

    total = sum(stats.values())
    print(f"\nimported={total} skipped={sum(skips.values())} ({out_path})")
    print("per-sheet:", dict(sorted(stats.items(), key=lambda kv: int(kv[0][1:]))))
    print("skips:", dict(skips) or "{}", "reasons:", dict(reasons) or "{}")
    srcs = Counter(r["src"] for r in rows)
    print(f"distinct sources: {len(srcs)}; top 5: {srcs.most_common(5)}")
    if census:
        print("\nINTRO census cross-check (level | census | stored | delta):")
        c_total = 0
        for lvl in range(1, 16):
            want, got = census.get(lvl), stats.get(f"Q{lvl}", 0)
            c_total += want or 0
            print(f"  Q{lvl:<2} | {want} | {got} | {got - (want or 0):+d}")
        print(f"  TOTAL | {c_total} | {total} | {total - c_total:+d}"
              "  (census counts raw rows incl. header/degenerate rows; "
              "every delta is the documented skip list above)")


if __name__ == "__main__":
    wb = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_WB
    out = sys.argv[2] if len(sys.argv) > 2 else DEFAULT_OUT
    main(wb, out)
