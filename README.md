# Games API — Boggle + Millionaire

One Go binary serving **both games** behind one API. The repo/module name stays
`boggle-api` on purpose: the deployment, CI path and Dockerfile were built
around it, and folding a second game in is not a reason to churn them. The
internal packages are what names the games honestly.

## Project structure

```
cmd/api/                       ONE binary — registers both games' route trees
cmd/{seed,import,label,backfill}/   one-shot question-corpus tooling (Millionaire)
internal/app/                  everything shared: DB pool, gin engine, /health,
                               CORS, rate limits, origin allowlist
internal/boggle/               Boggle: handler, models, repository, server, service
internal/millionaire/          Millionaire: handler, models, repository, server, service
internal/codes/                SHARED WORD-WORD-DD room-code generator (both games)
internal/dberr/                SHARED unique-violation classification (both games)
migrations/                    one sequence, table-prefixed per game
data/                          the WWTBAM question corpus (import seed)
```

A game is a **module**: `NewModule(db, origins, limiters)` builds its
dependencies on the shared DB pool, and `RegisterRoutes(engine)` mounts its own
route tree. Neither game dials the database, listens on a port, or owns
`/health` — which is exactly why they can share a process.

## Routes

| Game | REST | WebSocket |
|---|---|---|
| Boggle | `/api/boggle/*` | `/ws/boggle/rooms`, `/ws/boggle/rooms/:code` |
| Millionaire | `/api/millionaire/*` | `/ws/millionaire/rooms`, `/ws/millionaire/rooms/:code` |
| both | `GET /health` | |

**Room codes.** Both games use the same `WORD-WORD-DD` vocabulary (e.g.
`BLUE-TIGER-73`) from `internal/codes`. A code is what you share; the uuid
stays the internal key. WebSocket joins are strictly code-keyed (a mistyped
code fails rather than resolving somewhere else); the REST resolver is
code-first with a uuid fallback so existing links keep working.

## The database

One database hosts both games, namespaced by **table prefix** — `boggle_*` and
`millionaire_*`. This is deliberate: separating by schema or `search_path`
means one unqualified query can silently cross games, while prefixes make that
impossible by construction.

| File | What it does |
|---|---|
| `000_prefix_existing_tables.sql` | Data-preserving rename for a database that already ran a game under the old unprefixed names. Guarded; **raises** rather than guessing when the input is ambiguous. |
| `001_boggle_schema.sql` | Boggle's schema. |
| `002_millionaire_schema.sql` | Millionaire's cumulative schema. |
| `003_boggle_room_codes.sql` | Boggle's `code` column + partial unique index. |

Fresh database: apply `000` → `001` → `002` → `003` in order (`000` is a no-op
when there is nothing to rename). Existing database with data: run `000` once
and stop — it renames in place; **do not** then run `001`/`002`, which DROP and
recreate.

The devcontainer mounts `migrations/` at `/docker-entrypoint-initdb.d`, so all
four run automatically **on first boot only**. For an existing volume, apply new
files by hand:

```sh
psql "$DATABASE_URL" -f migrations/00X_new_thing.sql
```

## First boot — seeding the question corpus

Millionaire needs questions; Boggle does not. The WWTBAM corpus ships in
`data/wwtbam.jsonl` (11.8k real show questions) and is imported once:

```sh
go run ./cmd/import -source wwtbam -file data/wwtbam.jsonl
```

The importer dedupes against the live corpus and reports what it added, so
re-running it is a no-op. OpenTDB/OTQA corpora and the rating/label pipeline use
`cmd/seed`, `cmd/label` the same way (see `cmd/*/main.go` flags).

## Setup

1. Open the repo in VS Code and reopen in the dev container.
2. `go mod tidy`
3. `go run ./cmd/api` → API on <http://localhost:8080>.
4. Frontend lives in the sibling `boggle_fe` repo: `npm run dev` → hub on
   <http://localhost:5173> (`/` picks a game).

## Environment variables

- `DATABASE_URL` — defaults to the devcontainer's `db` service.
- `ALLOWED_ORIGINS` — comma-separated; feeds both the REST CORS middleware and
  each game's WebSocket origin check.
- `PORT` — bind port (default `:8080`; Azure Container Apps passes `PORT`).

## Testing

```sh
go test ./...
```

Millionaire's suites run against a real Postgres: point `DATABASE_URL` at a
database whose `millionaire_*` schema is migrated (a disposable one is fine —
they create and delete their own rooms).