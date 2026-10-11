-- M-A1 — Boggle's schema inside the shared games database.
--
-- Table PREFIXING is the whole point of this file: both games want a `rooms`
-- table, so the shared database namespaces them (`boggle_rooms` vs
-- `millionaire_rooms`) rather than relying on schema/search_path games. That
-- makes a cross-game query impossible by construction instead of by review.

DROP TABLE IF EXISTS boggle_boards;
DROP TABLE IF EXISTS boggle_rooms;

CREATE TABLE boggle_rooms (
    id UUID PRIMARY KEY,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE boggle_boards (
    id UUID PRIMARY KEY,
    room_id UUID REFERENCES boggle_rooms(id) ON DELETE CASCADE,
    cells JSONB NOT NULL,
    ends_at TIMESTAMP NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);