#!/usr/bin/env python3
"""Generate migrations/1002_import_opentdb_corpus.sql from the live database.

The OpenTDB rows are the one part of the corpus that CANNOT be reconstructed
from a file in the repo alone: upstream OpenTDB ships a question and an answer
but no options, and millionaire_questions.options requires exactly four. The
three distractors are synthesised at import time (see
internal/millionaire/service/opentdb.go), so a database that was seeded any
other way has different options for the same question.

That makes this a different kind of file from 1001. 1001 restores a LABEL that
the repo already knows the true value of; 1002 restores CONTENT. So it is
generated from the live database — after `go run ./cmd/import -source opentdb`
has run — and it carries the whole row, not just a strength.

Reads $DATABASE_URL from the environment (never a literal connection string).

Usage: python3 data/opentdb_insert_build.py
"""
import os
import subprocess
import sys

QUERY = """
SELECT id::text, question, options::text, correct_index, difficulty, category, strength
  FROM millionaire_questions
 WHERE source = 'opentdb'
 ORDER BY question
"""


def esc(value: str) -> str:
    """Quote as a SQL literal. Doubling the quote is the only escape needed."""
    return "'" + str(value).replace("'", "''") + "'"


def main() -> int:
    dsn = os.environ.get("DATABASE_URL", "")
    if not dsn:
        sys.exit("DATABASE_URL is not set")

    raw = subprocess.run(
        ["psql", dsn, "-At", "-F", "\x1f", "-c", QUERY],
        capture_output=True, text=True, check=True,
    ).stdout

    rows = []
    for line in raw.splitlines():
        if not line.strip():
            continue
        parts = line.split("\x1f")
        if len(parts) != 7:
            sys.exit(f"malformed row ({len(parts)} fields): {line[:80]}")
        rows.append(parts)

    if not rows:
        sys.exit("no opentdb rows found — has the importer run?")

    out = [
        "-- 1002_import_opentdb_corpus.sql — insert the OpenTDB easy-band corpus.\n",
        "--\n",
        "-- GENERATED FILE — do not hand-edit. Rebuild with:\n",
        "--   go run ./cmd/import -source opentdb && python3 data/opentdb_insert_build.py\n",
        "--\n",
        "-- WHY THIS IS DATA AND NOT JUST A LABEL\n",
        "--   1001 restores difficulty ratings, whose true values the repo knows.\n",
        "--   This file restores CONTENT. Upstream OpenTDB carries a question and\n",
        "--   an answer but no options, and millionaire_questions.options requires\n",
        "--   exactly four; the three distractors are synthesised from the same\n",
        "--   category's other answers at import time (service/opentdb.go). A\n",
        "--   database seeded any other way therefore has different options for the\n",
        "--   same question, and this file is what makes them reproducible.\n",
        "--\n",
        "-- WHY IT MATTERS\n",
        "--   Levels 1-3 draw from drawCasualBlend, which flips a coin between\n",
        "--   WWTBAM@level and a casual pool that is source <> 'wwtbam'. With WWTBAM\n",
        "--   as the only source the casual side is always empty and the blend\n",
        "--   silently runs at 100/0 — every easy question from one source.\n",
        "--\n",
        "--   OpenTDB — https://opentdb.com — is CC BY-SA 4.0; attribution is in the\n",
        "--   README. The categories vendored are those the casual blacklist allows:\n",
        "--   no Entertainment:*, no Celebrities, no Science: Gadgets, no Mythology.\n",
        "--   The distractors are SYNTHETIC — this is a known quality compromise,\n",
        "--   not an oversight.\n",
        "--\n",
        "-- USAGE\n",
        "--   psql \"$DATABASE_URL\" -f migrations/1002_import_opentdb_corpus.sql\n",
        "--\n",
        "-- Run AFTER 1001 (the ratings file covers WWTBAM only). Idempotent: rows\n",
        "-- are skipped when a question with that exact text already exists, so\n",
        "-- re-running imports 0. IDs are minted here with gen_random_uuid() rather\n",
        "-- than baked in — every import run mints fresh UUIDs anyway, so a fixed ID\n",
        "-- would be a lie about where the row came from.\n",
        "\nBEGIN;\n\n",
    ]

    # The table has NO unique constraint on question, so idempotency has to be an
    # explicit NOT EXISTS guard rather than ON CONFLICT. The whole batch goes in
    # as one INSERT ... SELECT from a VALUES list so the guard can see every
    # incoming question at once (a row-by-row loop would be 1,900 statements).
    values = [
        "  (%s, %s::jsonb, %s, %s, %s, %s)"
        % (esc(question), esc(options), correct, esc(difficulty), esc(category), strength)
        for _, question, options, correct, difficulty, category, strength in rows
    ]

    out.append("INSERT INTO millionaire_questions\n")
    out.append("    (id, question, options, correct_index, difficulty, category, source, strength)\n")
    out.append("SELECT gen_random_uuid(), v.question, v.options, v.correct_index,\n")
    out.append("       v.difficulty, v.category, 'opentdb', v.strength\n")
    out.append("  FROM (VALUES\n")
    out.append(",\n".join(values))
    out.append("\n  ) AS v(question, options, correct_index, difficulty, category, strength)\n")
    out.append(
        " WHERE NOT EXISTS (\n"
        "         SELECT 1 FROM millionaire_questions q\n"
        "          WHERE q.source = 'opentdb' AND q.question = v.question);\n\n"
    )

    out.append("\\echo '--- result ---'\n")
    out.append(
        "SELECT source, count(*) AS rows,\n"
        "       count(*) FILTER (WHERE strength BETWEEN 1 AND 3) AS easy_band\n"
        "  FROM millionaire_questions GROUP BY source ORDER BY source;\n"
    )
    out.append(
        "-- The blend is live only when easy_band > 0 for opentdb. Zero here means\n"
        "-- levels 1-3 are still running at 100/0 WWTBAM.\n\nCOMMIT;\n"
    )

    path = "migrations/1002_import_opentdb_corpus.sql"
    with open(path, "w", encoding="utf-8") as f:
        f.write("".join(out))

    print(f"wrote {path}: {len(rows)} rows")
    return 0


if __name__ == "__main__":
    sys.exit(main())