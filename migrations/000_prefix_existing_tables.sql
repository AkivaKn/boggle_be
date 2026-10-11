-- 000_prefix_existing_tables.sql — DATA-PRESERVING path for a database that
-- ALREADY runs one (or both) of the games under the OLD, unprefixed table
-- names. Run this INSTEAD of the DROP+CREATE files when the database holds
-- data you care about (a deployed Boggle instance, or a dev Millionaire volume
-- with its seeded question corpus).
--
-- Fully idempotent: every step is guarded by an existence check, so running
-- it on a fresh database is a no-op and running it twice is harmless. After it
-- completes, 001/002 find their tables already present and re-create them
-- only if they are missing — so the normal fresh-install order still works:
--
--   000_prefix_existing_tables.sql   (rename in place; no-op when absent)
--   001_boggle_schema.sql            (fresh Boggle)
--   002_millionaire_schema.sql       (fresh Millionaire)
--
-- NOTE on constraint/index names: renaming a table does NOT rename its
-- constraints or indexes, and constraint names are global per schema. The
-- renames below therefore only move TABLES — the existing constraint and index
-- names (e.g. `rooms_pkey`) are left alone deliberately, since the new files
-- declare their own names for freshly created tables and a name clash on an
-- already-renamed table would abort the transaction.

DO $$
DECLARE
    legacy_rooms_is_millionaire boolean;
BEGIN
    -- Identify the legacy unprefixed `rooms` table. Millionaire's has a
    -- `mode` column (solo|duel); Boggle's does not. This test is the ONLY
    -- thing standing between the two games' data, so an ambiguous result
    -- RAISES instead of guessing — a silent mis-rename would repoint
    -- Millionaire's foreign keys at Boggle's table.
    IF to_regclass('public.rooms') IS NOT NULL THEN
        SELECT EXISTS (
            SELECT 1 FROM information_schema.columns
            WHERE table_schema = 'public'
              AND table_name = 'rooms'
              AND column_name = 'mode'
        ) INTO legacy_rooms_is_millionaire;

        IF to_regclass('public.boggle_rooms') IS NOT NULL
           AND to_regclass('public.millionaire_rooms') IS NOT NULL THEN
            RAISE EXCEPTION
                'ambiguous: both boggle_rooms and millionaire_rooms already exist';
        END IF;

        IF legacy_rooms_is_millionaire THEN
            IF to_regclass('public.boggle_rooms') IS NULL THEN
                ALTER TABLE rooms RENAME TO millionaire_rooms;
            END IF;
        ELSE
            IF to_regclass('public.millionaire_rooms') IS NULL THEN
                ALTER TABLE rooms RENAME TO boggle_rooms;
            END IF;
        END IF;
    END IF;

    IF to_regclass('public.boards') IS NOT NULL
       AND to_regclass('public.boggle_boards') IS NULL THEN
        ALTER TABLE boards RENAME TO boggle_boards;
    END IF;

    IF to_regclass('public.players') IS NOT NULL
       AND to_regclass('public.millionaire_players') IS NULL THEN
        ALTER TABLE players RENAME TO millionaire_players;
    END IF;

    IF to_regclass('public.questions') IS NOT NULL
       AND to_regclass('public.millionaire_questions') IS NULL THEN
        ALTER TABLE questions RENAME TO millionaire_questions;
    END IF;

    IF to_regclass('public.room_questions') IS NOT NULL
       AND to_regclass('public.millionaire_room_questions') IS NULL THEN
        ALTER TABLE room_questions RENAME TO millionaire_room_questions;
    END IF;

    IF to_regclass('public.room_totals') IS NOT NULL
       AND to_regclass('public.millionaire_room_totals') IS NULL THEN
        ALTER TABLE room_totals RENAME TO millionaire_room_totals;
    END IF;
END $$;