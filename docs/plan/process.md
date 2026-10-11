# Millionaire — Working Process (implementation loop)

Applies to both plans. Roles are OpenCode subagents; the orchestrator (main session)
never writes game code itself.

## Loop per milestone
1. **Implement** — spawn a `general` subagent with:
   - the plan file path + the one milestone in `"IN PROGRESS"` status
   - instruction to read `SPEC.md` (source of truth) and the Boggle reference repos
   - requirement to run the milestone's gate commands itself and paste real output
   - requirement to update the plan file's checklist as it completes items
2. **Gate check** — orchestrator re-runs gate commands independently (never trust
   pasted output alone).
3. **Adversarial review** — spawn a fresh `general` subagent with NO knowledge of the
   implementer's claims. Give it only: repo paths, `SPEC.md` path, the milestone's
   gate list, and these standing hunt-directives:
   - Verify every SPEC invariant (backend §2: answer-leak, idempotency, option
     shuffling, survivor-continues, lifeline trust) by reading code and running tests/API calls.
   - Try to break it: double answers, replayed tokens, wrong-tier questions, timer
     bypass, forgotten rule, leaked `correct_index` in any event/REST payload.
   - For FE: event-table drift vs SPEC, answer leakage via React devtools-visible
     state, broken localStorage rejoin.
   - Verdict format: `APPROVE` or `REVISE` + numbered findings (severity, file:line).
4. **Fix loop** — `REVISE` findings go back to an implementer subagent (new session)
   with the findings list; repeat review. Max 2 retries, then escalate to the user.
5. **Mark complete** — on APPROVE: set milestone status to complete and paste the
   reviewer's APPROVE under the milestone's **Verification** line in the plan file.
6. **Proceed** to the next milestone.

## Parallelism
- Backend and frontend track their own milestone chains and may run concurrently
  (they only share the SPEC contract — mismatches surface at FE verification time).
- Adversarial reviews always run on a fresh session (never a continue of the
  implementer's session).
- **Orchestrator commits ONLY bookkeeping paths** (`docs/plan/**`, `PLAN.md`,
  README, compose/devcontainer) — never `git add -A` on a repo while an implement
  agent may have uncommitted WIP code there; sweeping a implementer's in-progress
  files into a plan commit confuses attribution (observed in M1). Implementers
  commit their own code.

## Wave ordering (locked 2026-10-11 — user)
1. **Per-seat swap engine** (BE) — swap swaps ONLY your own question.
2. **FE wave 6** — swap-power UI + the wave-4B session-total lines on the
   rematch card + the contrast/affordance audit.
3. **Games merge** (M-A → M-D, per `games-merge-plan.md`).
4. **Bulk labelling LAST** — the OpenTDB/OTQA labelling batches run only AFTER
   the merge lands, so the labelling tooling is written once against the merged
   repo layout instead of being migrated afterwards.

## Manual verification tooling
- `agent-browser` CLI is installed globally (v0.27, Chrome bundled). Core workflow:
  `agent-browser open <url>` → `snapshot -i` → `click/fill @ref` → re-snapshot.
  Skill file lives at `~/.config/opencode/skills/agent-browser/SKILL.md`.
- ⚠️ **`agent-browser record start` RELOADS THE PAGE** (it opens a new target),
  silently killing any in-flight game and invalidating "video frame" evidence.
  For animation evidence use an in-page rAF sampler (`getComputedStyle` per frame
  into an array, read back after) + `screenshot` at leisure — DOM/sampler numbers
  AND pixels, from the SAME run.
- Duo runs: two agent-browser tabs in one session.

## Core file conventions
- Plan of record lives in `/home/akiva/.opencode/plan/millionaire/` (this directory):
  `SPEC.md`, `backend-plan.md`, `frontend-plan.md`, `process.md`, `sync.sh`.
- **Repos carry full readable copies** at `<repo>/docs/plan/` (SPEC + repo plan +
  process). After ANY plan edit, run `sh ~/.opencode/plan/millionaire/sync.sh` and
  commit the updated `docs/plan/` in the repos — in-repo copies must never go stale.
- Each repo root also has a thin `PLAN.md` status index pointing at `docs/plan/`.

## Current status
- SPEC v1 finalized (phone-a-friend cut). Files written.
- Next: backend M0 and frontend M0 via implementer subagents.
