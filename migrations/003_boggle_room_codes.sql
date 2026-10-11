-- 003_boggle_room_codes.sql — M-A2: Boggle adopts the same human-friendly
-- room codes Millionaire has used since M7 (WORD-WORD-DD, e.g. BLUE-TIGER-73).
--
-- Both games now generate codes from the SAME shared vocabulary
-- (internal/codes), so a code means the same thing everywhere in the app and
-- the two games can never disagree about the format. The generator is shared;
-- the tables are not — each game's `code` column carries its own partial
-- unique index, so uniqueness is per-game and a Boggle code never has to be
-- distinct from a Millionaire one.
--
-- Every Boggle room gets a code (Boggle has no solo mode: every room is a
-- lobby somebody must share), so the column is NOT NULL with no ''-exclusion
-- like Millionaire's solo rows.

ALTER TABLE boggle_rooms ADD COLUMN IF NOT EXISTS code text;

-- Existing rooms (if any) get a code backfilled by cmd/backfill; until then
-- the column stays NULL and those rooms remain reachable by uuid only.
CREATE UNIQUE INDEX IF NOT EXISTS idx_boggle_rooms_code_unique
    ON boggle_rooms (code) WHERE code IS NOT NULL AND code <> '';