-- Full reset of the games schema. DESTRUCTIVE: drops every table below and
-- recreates them empty. Safe to run more than once.
-- After running, seed the question corpus:
--   go run ./cmd/import -source wwtbam -file data/wwtbam.jsonl

DROP TABLE IF EXISTS boggle_boards CASCADE;
DROP TABLE IF EXISTS boggle_rooms CASCADE;
DROP TABLE IF EXISTS millionaire_room_totals CASCADE;
DROP TABLE IF EXISTS millionaire_room_questions CASCADE;
DROP TABLE IF EXISTS millionaire_players CASCADE;
DROP TABLE IF EXISTS millionaire_questions CASCADE;
DROP TABLE IF EXISTS millionaire_rooms CASCADE;
DROP TABLE IF EXISTS boards CASCADE;
DROP TABLE IF EXISTS rooms CASCADE;
DROP TABLE IF EXISTS room_totals CASCADE;
DROP TABLE IF EXISTS room_questions CASCADE;
DROP TABLE IF EXISTS players CASCADE;
DROP TABLE IF EXISTS questions CASCADE;

CREATE TABLE boggle_rooms (
    id uuid PRIMARY KEY,
    created_at timestamp DEFAULT CURRENT_TIMESTAMP,
    code text
);

CREATE UNIQUE INDEX idx_boggle_rooms_code_unique
    ON boggle_rooms (code) WHERE code IS NOT NULL AND code <> '';

CREATE TABLE boggle_boards (
    id uuid PRIMARY KEY,
    room_id uuid REFERENCES boggle_rooms(id) ON DELETE CASCADE,
    cells jsonb NOT NULL,
    ends_at timestamp NOT NULL,
    created_at timestamp DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE millionaire_questions (
    id uuid PRIMARY KEY,
    question text NOT NULL,
    options jsonb NOT NULL CHECK (jsonb_array_length(options) = 4),
    correct_index int NOT NULL CHECK (correct_index BETWEEN 0 AND 3),
    difficulty text NOT NULL CHECK (difficulty IN ('easy', 'medium', 'hard')),
    category text NOT NULL DEFAULT '',
    source text NOT NULL DEFAULT 'opentdb',
    created_at timestamptz NOT NULL DEFAULT now(),
    strength int CHECK (strength BETWEEN 1 AND 15)
);

CREATE INDEX idx_millionaire_questions_difficulty
    ON millionaire_questions (difficulty);

CREATE INDEX idx_millionaire_questions_tier
    ON millionaire_questions (difficulty, correct_index);

CREATE INDEX idx_millionaire_questions_strength_band
    ON millionaire_questions (strength) WHERE strength IS NOT NULL;

CREATE TABLE millionaire_rooms (
    id uuid PRIMARY KEY,
    mode text NOT NULL CHECK (mode IN ('solo', 'duel')),
    status text NOT NULL DEFAULT 'lobby' CHECK (status IN ('lobby', 'active', 'finished')),
    created_at timestamptz NOT NULL DEFAULT now(),
    code text
);

CREATE UNIQUE INDEX idx_millionaire_rooms_code_unique
    ON millionaire_rooms (code) WHERE code <> '';

CREATE TABLE millionaire_players (
    id uuid PRIMARY KEY,
    room_id uuid NOT NULL REFERENCES millionaire_rooms(id) ON DELETE CASCADE,
    seat int NOT NULL CHECK (seat IN (1, 2)),
    token uuid NOT NULL,
    name text NOT NULL,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'fallen', 'walked', 'won')),
    lifelines_left jsonb NOT NULL DEFAULT '["fifty_fifty", "audience", "switch"]'::jsonb,
    amount integer NOT NULL DEFAULT 0,
    level_reached integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (room_id, seat)
);

CREATE INDEX idx_millionaire_players_room
    ON millionaire_players (room_id);

CREATE TABLE millionaire_room_questions (
    room_id uuid NOT NULL REFERENCES millionaire_rooms(id) ON DELETE CASCADE,
    level int NOT NULL CHECK (level BETWEEN 1 AND 15),
    question_id uuid NOT NULL REFERENCES millionaire_questions(id),
    variant int NOT NULL CHECK (variant IN (1, 2)),
    shuffled_options jsonb NOT NULL DEFAULT '[]'::jsonb,
    shuffled_correct_index int NOT NULL DEFAULT 0 CHECK (shuffled_correct_index BETWEEN 0 AND 3),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (room_id, level, variant)
);

CREATE INDEX idx_millionaire_room_questions_room
    ON millionaire_room_questions (room_id);

CREATE TABLE millionaire_room_totals (
    id uuid PRIMARY KEY,
    room_id uuid NOT NULL REFERENCES millionaire_rooms(id) ON DELETE CASCADE,
    seat int NOT NULL CHECK (seat IN (1, 2)),
    games_played int NOT NULL DEFAULT 0,
    games_won int NOT NULL DEFAULT 0,
    total_money bigint NOT NULL DEFAULT 0,
    best_level int NOT NULL DEFAULT 0 CHECK (best_level BETWEEN 0 AND 15),
    best_amount int NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (room_id, seat)
);