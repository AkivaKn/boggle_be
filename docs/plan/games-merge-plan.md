# }. Games merge plan — fold Millionaire into Boggle (v1.0 draft for review)

> Approved direction: ONE binary, per-game Go packages (boggle & millionaire each
> with handler/service/repository/models), no lazy route loading (FE small), fold
> INTO `/home/akiva/repositories/boggle` (its GitHub deployment is already connected)
> and INTO `/repositories/boggle_fe` (same for the FE deploy).

## User decisions (2026-10-11)
1. **Repo/CI naming STAYS**: the Go module path remains `boggle-api`, repo/model
   names unchanged, so the GitHub build path needs no touching. The repo name will
   read "boggle" while hosting both games; internal packages keep their per-game
   namespaces (`internal/boggle`, `internal/millionaire`) — the naming assumption
   is documented in the README (a games room that started as Boggle).
2. **FE deploy is Netlify on the boggle_fe repo**: ONE endpoint SPA. Its build
   stays the same root (src/ replaced in place with the hub-routing structure) so
   the Netlify build command and publish dir (`dist/`) need no changes. The
   hub routes `/` (game selection), `/boggle`, `/millionaire`.
3. **Boggle room codes**: Boggle adopts millionaire's human-friendly codes
   (`WORD-WORD-DD`, strict code join, uuid fallback for REST) using a SHARED
   generator extracted during the fold (`internal/codes`, consumed by both games;
   zero vocabulary collision — one vocab, both games).
4. The 5436/8085/5177 port experiment retires; the merged devcontainer is the
   boggle repo's (5435 DB, 8080 BE, 5173 FE).


## Sequencing in the overall program
1. Current waves land: FE 5b (deferred start/rematch) + BE 4B (session totals) — in flight.
2. Per-seat swap engine (swap only swaps your own — user decision) — the last millionaire
   feature wave; landing first avoids stacking a big refactor inside the merge.
3. **Then the merge** (M-A → M-D below) as a standalone program (~4–6 implementer waves
   + reviews; fewer code-risk rebases since both games will be feature-complete).
4. Final merged verification: both games' test suites + one manual gate per game +
   labeling/attraction carry-over (data assets move with the fold).

## M-A/M-B STATUS — COMPLETE (backend `3cae3cc`, `9e9bdb1`, `c80d61a`, `1cc75e9`; FE `6188a70`)
Both folds landed in the Boggle repos and are live-verified end to end.

**Backend shape (as built).**
- `internal/app` owns everything shared: the DB pool, the gin engine,
  `/health`, CORS, the rate-limit policy, the origin allowlist.
- Each game is a `Module`: `NewModule(db, origins, limiters)` +
  `RegisterRoutes(engine)` → `internal/boggle/server`, `internal/millionaire/server`.
  Neither game dials, listens, or owns the port. Build path stays
  `./cmd/api/main.go` (Dockerfile + deploy hook untouched).
- Routes: `/api/boggle/*` + `/ws/boggle/*`, `/api/millionaire/*` + `/ws/millionaire/*`.
- One database, `boggle_*` / `millionaire_*` table prefixes — a cross-game
  query is impossible by construction, not by review.
- Migrations: `000_prefix_existing_tables.sql` (data-preserving rename,
  guarded, RAISES on ambiguous input), `001_boggle_schema.sql`,
  `002_millionaire_schema.sql`, `003_boggle_room_codes.sql`.
- Shared packages: `internal/codes` (the WORD-WORD-DD generator — one
  vocabulary for both games) and `internal/dberr` (unique-violation
  classification — both games retry code collisions identically).

**Deliberate deviation from this plan: no shared `internal/wshub`.** Each game
keeps its own websocket hub. The games are fully namespaced (separate route
trees, separate tables, separate clients), so a shared hub would couple two
systems that never talk, for no functional gain — and the fold is the moment
to be deleting coupling, not adding it. Revisit only if a third game appears.

**Frontend shape (as built).** `src/App.tsx` is the hub; `src/router.ts` holds
the three routes (hand-rolled History API + popstate — no router dependency
for three routes); games live under `src/games/{boggle,millionaire}/`.
`public/_redirects` gives Netlify the SPA history fallback so deep links and
in-game refreshes work with the build command and publish dir unchanged.

**Verified:** build/vet/gofmt clean; the full Go suite green twice; 12/12 live
API checks (both games create/join/play through the merged binary, prefixed
tables isolated, session census written by the upsert); FE build + lint clean
at its pre-existing 4 problems; browser pass through the hub — lobby → both
games, deep links, back-navigation, Boggle board generation with the code chip.

**Incident worth remembering:** the first schema application renamed tables in
the DEV database the user's running backend (old binary, unprefixed SQL) was
using, which broke it until reverted. The merged binary now runs against its
own `games` database so the two stacks cannot collide again.

## M-A — Backend fold (2–3 waves)
- New layout in `/repositories/boggle`:
  ```
  cmd/api/                     ONE binary; registers /boggle/* and /millionaire/*
                               route trees (their own rate-limit groups: boggle
                               general-only; millionaire general + strict per cmd)
  internal/boggle/{handler,service,repository,models}
  internal/millionaire/{handler,service,repository,models}
  internal/codes/              SHARED room-code generator (extracted from M7's
  internal/wshub/              unified wsManager (take millionaire's: SendToSeat,
                               close codes 4400/4500; key rooms as "boggle/<id>" vs
                               "millionaire/<id>")
                               service/codes.go) — both games use the same
                               WORD-WORD-DD vocabulary, zero collisions
  migrations/                  one sequence: 001_boggle_*.sql + 002_millionaire_*.sql…
                               (fresh single DB; no data carryover)
  Dockerfile                   ONE image: builds the single binary (the GitHub build
                               path needs no touching: decisions #1/#2 above; keep
                               module boggle-api)
  ```
- **Boggle room-code parity (new, decision #3)**: boggle adopts the human-friendly
  codes — rooms gain `code`, joins become code-keyed (uuid REST fallback for
  creator-only IDs as millionaire has), the FE join flow switches from pasting the
  uuid to codes; FE shows the code chip + copy affordance identically to
  millionaire's duel lobby (a fold-time backport using the SHARED codes lib).
- DB: ONE database, tables renamed per game (`boggle_rooms`/`boggle_boards`;
  `millionaire_*` prefix for rooms/players/questions/room_questions/room_totals) —
  honest "one DB"; repository edits are mechanical (ksql table constants + SQL
  strings).
- Millionaire-only assets move: `data/wwtbam.jsonl`, `data/wwtbam_export.py`,
  `label/` tooling, `cmd/label`, `cmd/seed`, `cmd/import`, `cmd/backfill`
  (namespaced or moved to `cmd/millionaire-*` where ambiguous).
- Millionaire's old repo archived (kept, renamed `millionaire.archived-YYYYMMDD`);

## M-B — Frontend fold (2–3 waves)
- `/repositories/boggle_fe` becomes one SPA:
  ```
  src/hub/                     the games-room hub (cards for both games)
  src/boggle/                  Boggle app (components/state; a router arrives:
  src/millionaire/             both are small — NO lazy loading per user decision)
  src/shared/{tokens,components}   the v3 design language becomes the shared base
  ```
- Vite: one dev server/devport 5173; the 5177 port-split disappears.
- Boggle's FE gains the v3 tokens (align its styling; its old look modernized
  opportunistically but do NOT redesign the game itself in the merge).
- `millionaire_fe` archived same as above; the FE deploy hook pointed at the
  boggle_fe repo (its build command may need the new entry path — flag to user).

## M-C — Devcontainer/deploy/docs (1 wave)
- One devcontainer: Go toolchain + Postgres (single port; FE dev 5173; BE 8080),
  ALLOWED_ORIGINS unified, the old port split (5436/5435/8085) retired.
- README/deploy docs; the CI/Dockerfile/hook updates flagged for the user above.

## M-D — Verification
- Full backend test suites per game inside one module + a live two-game session
  (Boggle room + Millionaire solo + duel) on one binary at ONE port;
- FE: hub router + both games manual + gates per游戏的 existing plan structure;
- Rollback safety: both original repos remain archived until you're happy.

## Out of scope (parked)
Migrating the old dev DB data (re-seed only), Boggle redesign (only token alignment),
any third game (the hub anticipates it).
