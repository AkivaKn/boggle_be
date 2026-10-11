-- 002_millionaire_schema.sql — Millionaire's schema inside the shared games
-- database (the cumulative end state of the former 001–006 migration chain,
-- rewritten with prefixed table names).
--
-- WHY PREFIXED: Boggle's schema already owns `rooms`/`boards` (001_boggle_*).
-- Rather than separating the games by schema or search_path — which a single
-- unqualified query can silently cross — every Millionaire table is namespaced
-- `millionaire_*`. A cross-game query is then impossible by construction.
--
-- Idempotent for re-apply (DROP + CREATE, exactly like the old 001 chain did):
-- this file is the FRESH-database path. An EXISTING Millionaire deployment
-- keeps its data via 000_prefix_existing_tables.sql, which renames in place
-- instead of dropping.

DROP TABLE IF EXISTS millionaire_room_totals;
DROP TABLE IF EXISTS millionaire_room_questions;
DROP TABLE IF EXISTS millionaire_players;
DROP TABLE IF EXISTS millionaire_questions;
DROP TABLE IF EXISTS millionaire_rooms;

CREATE TABLE millionaire_questions (
    id            uuid PRIMARY KEY,
    question      text NOT NULL,
    options       jsonb NOT NULL CHECK (jsonb_array_length(options) = 4),
    correct_index int  NOT NULL CHECK (correct_index BETWEEN 0 AND 3),
    difficulty    text NOT NULL CHECK (difficulty IN ('easy', 'medium', 'hard')),
    category      text NOT NULL DEFAULT '',
    source        text NOT NULL DEFAULT 'opentdb',
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- M6 question engine v2: per-question strength (sheet level 1–15).
    strength      int CHECK (strength BETWEEN 1 AND 15)
);
CREATE INDEX idx_millionaire_questions_difficulty
    ON millionaire_questions (difficulty);
CREATE INDEX idx_millionaire_questions_tier
    ON millionaire_questions (difficulty, correct_index);
-- Partial: band selection (v2) only ever reads rated rows.
CREATE INDEX idx_millionaire_questions_strength_band
    ON millionaire_questions (strength) WHERE strength IS NOT NULL;

CREATE TABLE millionaire_rooms (
    id         uuid PRIMARY KEY,
    mode       text NOT NULL CHECK (mode IN ('solo', 'duel')),
    status     text NOT NULL DEFAULT 'lobby'
                   CHECK (status IN ('lobby', 'active', 'finished')),
    created_at timestamptz NOT NULL DEFAULT now(),
    -- M7: human-friendly room codes (WORD-WORD-DD). Solo rooms keep
    -- code = '' (codes are surfaced for duels only — SPEC v1.1 decision log).
    code       text
);
-- Real codes (duels) are unique; the empty string (solo) is excluded.
CREATE UNIQUE INDEX idx_millionaire_rooms_code_unique
    ON millionaire_rooms (code) WHERE code <> '';

CREATE TABLE millionaire_players (
    id            uuid PRIMARY KEY,
    room_id       uuid NOT NULL REFERENCES millionaire_rooms(id) ON DELETE CASCADE,
    seat          int  NOT NULL CHECK (seat IN (1, 2)),
    token         uuid NOT NULL,
    name          text NOT NULL,
    status        text NOT NULL DEFAULT 'active'
                      CHECK (status IN ('active', 'fallen', 'walked', 'won')),
    -- SPEC §1: each lifeline is one use per run.
    lifelines_left jsonb NOT NULL
        DEFAULT '["fifty_fifty", "audience", "switch"]'::jsonb,
    amount        integer NOT NULL DEFAULT 0,
    level_reached integer NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (room_id, seat)
);
CREATE INDEX idx_millionaire_players_room ON millionaire_players (room_id);

-- M3: the server-shuffled option order + remapped correct index per room
-- question (SPEC §2 invariant 4 — the correct index must not be uniform
-- across rooms). `variant` is the per-level alternative the SWAP lifeline
-- draws from.
CREATE TABLE millionaire_room_questions (
    room_id                  uuid NOT NULL
        REFERENCES millionaire_rooms(id) ON DELETE CASCADE,
    level                    int  NOT NULL CHECK (level BETWEEN 1 AND 15),
    question_id              uuid NOT NULL REFERENCES millionaire_questions(id),
    variant                  int  NOT NULL CHECK (variant IN (1, 2)),
    shuffled_options         jsonb NOT NULL DEFAULT '[]'::jsonb,
    shuffled_correct_index   int  NOT NULL DEFAULT 0
        CHECK (shuffled_correct_index BETWEEN 0 AND 3),
    created_at               timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (room_id, level, variant)
);
CREATE INDEX idx_millionaire_room_questions_room
    ON millionaire_room_questions (room_id);

-- Wave-4B: ONE per-room SESSION census row per seat — accumulated at every
-- game_over, never reset by a rematch, dying only with its room.
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