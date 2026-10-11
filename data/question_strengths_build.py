#!/usr/bin/env python3
"""Regenerate data/question_strengths.csv and
migrations/1001_apply_question_strengths.sql from the live database.

Both files are GENERATED (the migration header says so). Re-run after any
rating work:

    psql "$DATABASE_URL" -At -F'|' -c \
      "SELECT question, strength FROM millionaire_questions WHERE source = 'wwtbam' ORDER BY question" \
    | python3 data/question_strengths_build.py

Ratings are keyed by QUESTION TEXT, not id: every import run mints fresh
UUIDs, so the same question has a different id in every database.
"""
import csv
import subprocess
import sys

HEADER = """-- 1001_apply_question_strengths.sql — apply the curated difficulty ratings.
-- GENERATED FILE — do not hand-edit. Source of truth: data/question_strengths.csv
-- (regenerate with: psql "$DATABASE_URL" -At -F'|' -c "SELECT question, strength
--  FROM millionaire_questions WHERE source = 'wwtbam' ORDER BY question" |
--  python3 data/question_strengths_build.py)
--
-- WHY
--   The game serves WWTBAM questions strictly at `strength == level`, so what
--   sits at strength 1-3 IS the player's easy tier. These ratings were produced
--   by rating the corpus against a pub-quiz rubric (label/rubric.md). Two
--   sweeps shaped it:
--     1. a downward re-rating of 1,800 questions — the original levels were a
--        near-uniform forced spread, with genuinely easy questions stranded in
--        the high bands ("What are monotremes?" was filed at strength 9);
--     2. an UPWARD quality sweep of the whole easy band, which moved 460
--        niche/cultural rows out of levels 1-3 (franchise lore, sport-by-sport,
--        celebrity minutiae, film/TV trivia) down to a floor of 1,800 rows.
--
-- WHY KEYED BY TEXT
--   Every import run mints fresh UUIDs, so the same question has a different id
--   in every database. Keying on text is what makes this portable.
--
-- USAGE
--   psql "$DATABASE_URL" -f migrations/1001_apply_question_strengths.sql
--   (or paste it into any SQL console). Idempotent: re-running re-applies the
--   same values. The verification block at the end prints what changed and
--   flags any corpus drift.
"""

TAIL = """-- Apply.
WITH applied AS (
    UPDATE millionaire_questions q
       SET strength = i.strength
      FROM strength_import i
     WHERE q.question = i.question
    RETURNING 1
)
SELECT count(*) AS ratings_applied FROM applied;

-- Drift check: any mismatch means the corpus and the ratings have diverged.
\\echo '--- coverage (non-zero = corpus drift, not a failure) ---'
SELECT count(*) AS corpus_questions_missing_from_ratings
  FROM millionaire_questions q
 WHERE NOT EXISTS (SELECT 1 FROM strength_import i WHERE i.question = q.question);
SELECT count(*) AS ratings_with_no_question
  FROM strength_import i
 WHERE NOT EXISTS (SELECT 1 FROM millionaire_questions q WHERE q.question = i.question);

\\echo '--- after ---'
SELECT count(*) FILTER (WHERE strength IS NULL)    AS unrated,
       count(*) FILTER (WHERE strength BETWEEN 1 AND 3) AS easy_pool
  FROM millionaire_questions;
SELECT strength, count(*) FROM millionaire_questions GROUP BY strength ORDER BY strength;

COMMIT;
"""


def sql_literal(s):
    return "'" + s.replace("'", "''") + "'"


def main():
    dsn = subprocess.run(
        ["bash", "-lc", 'psql "$DATABASE_URL" -At -F"|" '
         "-c \"SELECT question, strength FROM millionaire_questions "
         "WHERE source = 'wwtbam' ORDER BY question\""],
        capture_output=True, text=True, check=True).stdout

    rows = []
    for line in dsn.splitlines():
        if not line.strip():
            continue
        q, _, s = line.rpartition("|")
        rows.append((q, int(s)))
    if not rows:
        sys.exit("no wwtbam ratings found — is DATABASE_URL set?")

    with open("data/question_strengths.csv", "w", newline="", encoding="utf-8") as fh:
        w = csv.writer(fh, quoting=csv.QUOTE_ALL)
        w.writerow(["question", "strength"])
        for q, s in rows:
            w.writerow([q, s])

    with open("migrations/1001_apply_question_strengths.sql", "w", encoding="utf-8") as fh:
        fh.write(HEADER)
        fh.write("BEGIN;\n")
        fh.write("CREATE TEMP TABLE strength_import (question text PRIMARY KEY, strength int NOT NULL) ON COMMIT DROP;\n")
        fh.write("INSERT INTO strength_import (question, strength) VALUES\n")
        fh.write(",\n".join("  (%s, %d)" % (sql_literal(q), s) for q, s in rows))
        fh.write(";\n\n")
        fh.write("\\echo '--- before ---'\n")
        fh.write("SELECT count(*) FILTER (WHERE strength IS NULL)    AS unrated,\n")
        fh.write("       count(*) FILTER (WHERE strength BETWEEN 1 AND 3) AS easy_pool,\n")
        fh.write("       max(strength)                               AS hardest\n")
        fh.write("  FROM millionaire_questions;\n\n")
        fh.write(TAIL)

    print("regenerated %d ratings" % len(rows))


if __name__ == "__main__":
    main()