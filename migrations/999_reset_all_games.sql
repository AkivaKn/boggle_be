-- 999_reset_all_games.sql — ONE-SHOT FULL RESET of the games schema.
--
--   USE THIS WHEN:  the database still carries the OLD, unprefixed schema
--                   (rooms / boards / questions / …) and you do not care
--                   about its contents — every game is being re-created from
--                   scratch anyway.
--   NOT FOR:        a database you want to KEEP. If you have live rooms you
--                   care about, run 000_prefix_existing_tables.sql instead:
--                   it renames in place and preserves everything.
--
-- ⚠️  DESTRUCTIVE. Every table below is dropped without confirmation. Rooms,
--     boards and any Millionaire run history are GONE. The question corpus is
--     gone too — re-seed after running this with:
--         go run ./cmd/import -source wwtbam -file data/wwtbam.jsonl
--
-- Idempotent: safe to run repeatedly (it drops before it creates).
--
-- What this produces — ONE database, two games, separated by table prefix so
-- a query can never cross between them:
--     boggle_*        Boggle (rooms, boards, the WORD-WORD-DD code)
--     millionaire_*   Millionaire (questions, rooms, players, selections, totals)

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. Drop everything this script owns — both the old unprefixed names and the
--    current prefixed ones — so the result never depends on what was there.
-- ─────────────────────────────────────────────────────────────────────────────
DROP TABLE IF EXISTS boggle_boards CASCADE;
DROP TABLE IF EXISTS boggle_rooms CASCADE;

DROP TABLE IF EXISTS millionaire_room_totals CASCADE;
DROP TABLE IF EXISTS millionaire_room_questions CASCADE;
DROP TABLE IF EXISTS millionaire_players CASCADE;
DROP TABLE IF EXISTS millionaire_questions CASCADE;
DROP TABLE IF EXISTS millionaire_rooms CASCADE;

-- The pre-merge, unprefixed tables (what a database created before the games
-- were folded together still has). Safe if they were never created.
DROP TABLE IF EXISTS boards CASCADE;
DROP TABLE IF EXISTS rooms CASCADE;
DROP TABLE IF EXISTS room_totals CASCADE;
DROP TABLE IF EXISTS room_questions CASCADE;
DROP TABLE IF EXISTS players CASCADE;
DROP TABLE IF EXISTS questions CASCADE;

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. Boggle
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE boggle_rooms (
    id         UUID PRIMARY KEY,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    -- The human-friendly room code (WORD-WORD-DD) a friend types to join.
    -- Every Boggle room gets one (Boggle has no solo mode).
    code       text
);
-- Real codes are unique; pre-code rows may be NULL, which never collides.
CREATE UNIQUE INDEX idx_boggle_rooms_code_unique
    ON boggle_rooms (code) WHERE code IS NOT NULL AND code <> '';

CREATE TABLE boggle_boards (
    id         UUID PRIMARY KEY,
    room_id    UUID REFERENCES boggle_rooms(id) ON DELETE CASCADE,
    cells      JSONB NOT NULL,
    ends_at    TIMESTAMP NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. Millionaire — the question corpus
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE millionaire_questions (
    id            uuid PRIMARY KEY,
    question      text NOT NULL,
    options       jsonb NOT NULL CHECK (jsonb_array_length(options) = 4),
    correct_index int  NOT NULL CHECK (correct_index BETWEEN 0 AND 3),
    difficulty    text NOT NULL CHECK (difficulty IN ('easy', 'medium', 'hard')),
    category      text NOT NULL DEFAULT '',
    source        text NOT NULL DEFAULT 'opentdb',
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- Question engine v2: per-question strength (1–15). NULL = unrated.
    strength      int CHECK (strength BETWEEN 1 AND 15)
);
CREATE INDEX idx_millionaire_questions_difficulty
    ON millionaire_questions (difficulty);
CREATE INDEX idx_millionaire_questions_tier
    ON millionaire_questions (difficulty, correct_index);
-- Partial: the band selection only ever reads rated rows.
CREATE INDEX idx_millionaire_questions_strength_band
    ON millionaire_questions (strength) WHERE strength IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. Millionaire — rooms, players, per-room question selection, session totals
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE millionaire_rooms (
    id         uuid PRIMARY KEY,
    mode       text NOT NULL CHECK (mode IN ('solo', 'duel')),
    status     text NOT NULL DEFAULT 'lobby'
                   CHECK (status IN ('lobby', 'active', 'finished')),
    created_at timestamptz NOT NULL DEFAULT now(),
    -- Room codes (WORD-WORD-DD) are a DUEL-only surface; solo rooms keep ''
    -- and the partial index below excludes it.
    code       text
);
CREATE UNIQUE INDEX idx_millionaire_rooms_code_unique
    ON millionaire_rooms (code) WHERE code <> '';

CREATE TABLE millionaire_players (
    id             uuid PRIMARY KEY,
    room_id        uuid NOT NULL REFERENCES millionaire_rooms(id) ON DELETE CASCADE,
    seat           int  NOT NULL CHECK (seat IN (1, 2)),
    token          uuid NOT NULL,
    name           text NOT NULL,
    status         text NOT NULL DEFAULT 'active'
                       CHECK (status IN ('active', 'fallen', 'walked', 'won')),
    -- Each lifeline is one use per run (SPEC §1).
    lifelines_left jsonb NOT NULL
        DEFAULT '["fifty_fifty", "audience", "switch"]'::jsonb,
    amount         integer NOT NULL DEFAULT 0,
    level_reached  integer NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (room_id, seat)
);
CREATE INDEX idx_millionaire_players_room ON millionaire_players (room_id);

-- The server-shuffled option order + remapped correct index per room question,
-- so the correct index is not uniform across rooms (SPEC §2 invariant 4).
-- `variant` is the per-level alternative the SWAP lifeline draws from.
CREATE TABLE millionaire_room_questions (
    room_id                uuid NOT NULL
        REFERENCES millionaire_rooms(id) ON DELETE CASCADE,
    level                  int  NOT NULL CHECK (level BETWEEN 1 AND 15),
    question_id            uuid NOT NULL REFERENCES millionaire_questions(id),
    variant                int  NOT NULL CHECK (variant IN (1, 2)),
    shuffled_options       jsonb NOT NULL DEFAULT '[]'::jsonb,
    shuffled_correct_index int  NOT NULL DEFAULT 0
        CHECK (shuffled_correct_index BETWEEN 0 AND 3),
    created_at             timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (room_id, level, variant)
);
CREATE INDEX idx_millionaire_room_questions_room
    ON millionaire_room_questions (room_id);

-- Per-room SESSION census: accumulated at every game_over, never reset by a
-- rematch, dying only with its room.
CREATE TABLE millionaire_room_totals (
    id           uuid PRIMARY KEY,
    room_id      uuid NOT NULL
        REFERENCES millionaire_rooms(id) ON DELETE CASCADE,
    seat         int  NOT NULL CHECK (seat IN (1, 2)),
    games_played int  NOT NULL DEFAULT 0,
    games_won    int  NOT NULL DEFAULT 0,
    total_money  bigint NOT NULL DEFAULT 0,
    best_level   int  NOT NULL DEFAULT 0 CHECK (best_level BETWEEN 0 AND 15),
    best_amount  int  NOT NULL DEFAULT 0,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (room_id, seat)
);

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. Verify (optional): everything below should list 7 tables.
-- ─────────────────────────────────────────────────────────────────────────────
-- SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY 1;