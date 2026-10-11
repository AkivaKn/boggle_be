package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"boggle-api/internal/millionaire/models"
	"boggle-api/internal/millionaire/repository"

	"github.com/google/uuid"
	"github.com/vingarcia/ksql"
)

// This file implements the M3 question lifecycle: selection at game start,
// per-level issue/answer/score, walk-away, timer fallback and standings.
//
// STATE MODEL (documented non-resilience): the live game state — current
// level, ends_at, per-seat resolutions and the in-flight timer — lives in
// the in-memory `games` map below, mirroring Boggle's pragmatism of a
// stateless single-process game server. A process restart kills any
// mid-game: the room's DB rows stay behind (status `active`, stale
// amounts) and clients cannot resume; only the canonical question layout
// (shuffled options + remapped correct index) is persisted, in
// room_questions, alongside the selection. Everything else is deliberately
// NOT recovered on restart — re-creating rooms is the user's path.
//
// LEAK DISCIPLINE (SPEC §2 invariant 1): the remapped correct index lives
// in roomGame memory and in room_questions (server-side DB) ONLY. It may
// leave the server exclusively via answer_result (the player answered),
// and run_ended when status=fallen (the run is over either way). No other
// event or REST response ever includes it — Snapshot, question payloads
// and game_over standings are all built without it.

// ladder is the 15-level money ladder (SPEC §1), index 0 = level 1.
var ladder = []int{
	100, 200, 300, 500, 1000, // 1-5 (safe haven at 5)
	2000, 4000, 8000, 16000, 32000, // 6-10 (safe haven at 10)
	64000, 125000, 250000, 500000, 1000000, // 11-15
}

// AmountAt returns the ladder amount of a 1-based level ($0 outside 1..15).
func AmountAt(level int) int {
	if level < 1 || level > len(ladder) {
		return 0
	}
	return ladder[level-1]
}

// WalkAmount is what a walk locks in: the amount of the last ANSWERED
// level (SPEC §1) — level_reached is exactly that.
func WalkAmount(levelReached int) int {
	return AmountAt(levelReached)
}

// FallAmount is what a fall leaves behind: below the first safe haven $0,
// otherwise the last safe-haven amount BANKED. A haven is banked only by
// ANSWERING the haven level correctly (level_reached >= 5 means level 5
// was answered correctly). Falling ON a haven question (attempting level
// 5 or 10 without having answered it) keeps the PREVIOUS haven: attempting
// level 5 → $0, levels 6..10 → $1,000, levels 11..15 → $32,000.
func FallAmount(levelReached int) int {
	switch {
	case levelReached >= 10:
		return 32000
	case levelReached >= 5:
		return 1000
	default:
		return 0
	}
}

// TierFor maps a level to its question difficulty (SPEC §1: 1-5 easy,
// 6-10 medium, 11-15 hard).
func TierFor(level int) string {
	switch {
	case level <= 5:
		return "easy"
	case level <= 10:
		return "medium"
	default:
		return "hard"
	}
}

// BudgetFor returns the per-question timer budget of a level's tier
// (SPEC §1: 30s / 20s / 15s).
func BudgetFor(budgets map[string]time.Duration, level int) time.Duration {
	return budgets[TierFor(level)]
}

// DefaultBudgets are the SPEC §1 tier budgets.
var DefaultBudgets = map[string]time.Duration{
	"easy":   30 * time.Second,
	"medium": 20 * time.Second,
	"hard":   15 * time.Second,
}

// advanceRetryBackoff is the delay before re-attempting the roster read
// after a transient failure (M3 review finding 1: without a re-arm, one
// failed GetPlayersByRoom inside the advance/timeout paths would strand
// the level forever — nothing else would ever re-trigger it once every
// party had resolved).
const advanceRetryBackoff = time.Second

// advanceRetryAttempts caps the re-arms per level so a persistently
// failing roster read gives up instead of spinning timers forever (the
// level stays stranded either way — capped, and logged every time).
const advanceRetryAttempts = 3

// duelRevealBeat is the SPEC v1.1 resolve pacing: after every active
// player resolved a level, duels issue the next question on a fixed
// 2.5s reveal beat (server-scheduled so both players stay in sync).
// Solo rooms NEVER auto-advance — they hold for the Continue endpoint,
// whose accept floor is the same beat since the last resolution event
// (enforced server-side so a solo client cannot hammer past the reveal).
const duelRevealBeat = 2500 * time.Millisecond

// Notifier delivers game events to connected sockets (implemented by the
// handler's ws manager). seat 0 means "the whole room".
type Notifier interface {
	Broadcast(roomID string, msg interface{})
	SendToSeat(roomID string, seat int, msg interface{})
}

// QuestionView is the uniform question payload (SPEC §2): every
// question-bearing event and the room_state question field use this exact
// shape — level, text and the room-wide SHUFFLED options. Never carries
// the correct index.
type QuestionView struct {
	Level   int      `json:"level"`
	Text    string   `json:"text"`
	Options []string `json:"options"`
}

// GameError is a game-rule rejection with the REST status to answer with
// (the handler additionally relays the message as a WS error event).
type GameError struct {
	Status  int
	Message string
}

func (e *GameError) Error() string { return e.Message }

// AnswerOutcome is the private answer_result payload: handed to the
// answerer only, after they answered (so carrying correct_index is the
// SPEC-allowed exception, not a leak).
type AnswerOutcome struct {
	Correct      bool   `json:"correct"`
	CorrectIndex int    `json:"correct_index"`
	Amount       int    `json:"amount"`
	Status       string `json:"status"` // active | fallen | won
}

type levelQuestion struct {
	questionID   string
	text         string
	options      []string // room-wide shuffled order
	correctIndex int
}

type seatResolution struct {
	choiceIndex int    // -1 when the run ended without an answer (walk/timeout)
	questionID  string // the question this seat RESOLVED at this level
	// questionID is the wave-5 duel-fairness ledger: a swap may never move a
	// seat onto a question another seat has already ANSWERED at this level
	// (see swapTargetLocked).
}

// roomGame is one room's live game state (see the STATE MODEL note above).
type roomGame struct {
	level  int // current level, 1..15
	endsAt time.Time
	timer  *time.Timer // fallback: unresolved actives fall at ends_at

	// current is the level's PRIMARY question — variant 1 of the pre-selected
	// batch, issued to EVERY seat at level issue. Wave-5 SWAP ENGINE: it is
	// NOT mutated by a swap any more (the room-wide `game.current = alt` of
	// the old M5 switch is gone — see `seatQuestions`). It stays the default
	// a seat reads when it has no entry in `seatQuestions`, i.e. the shared
	// selection head every seat starts the level on.
	current levelQuestion

	// seatQuestions is the PER-SEAT ACTIVE question of game.level (wave 5 —
	// the locked product decision "SWAP replaces ONLY THE SEAT'S OWN
	// QUESTION"). A seat with no entry is on `current` (the shared primary).
	// A swap writes ONLY the swapping seat's entry: the peer's entry (and
	// therefore the peer's question object) is never touched.
	//
	// Every per-question READ goes through activeQuestionLocked(game, seat):
	//   - Answer            → scores against the ANSWERER's own question
	//   - onTimeout         → falls on each seat's own question
	//   - opponent_reveal   → reveals the FALLEN seat's own question
	//   - Snapshot          → the REJOINING seat's own question
	//   - fifty_fifty/audience → computed against the CALLER's own question
	// A swap does NOT touch ends_at, the in-flight timer, the level, or the
	// pre-selected batch (`variants`) — same level, same deadline, new text.
	seatQuestions map[int]levelQuestion

	// swapped marks the seats that already spent a swap at the CURRENT
	// level ("one swap per seat per level"). Reset by issueLevelLocked.
	// Belt-and-braces on top of the DB one-use-per-run spend: it keeps the
	// per-level rule explicit and independent of the lifeline budget.
	swapped map[int]bool

	variants map[int][]levelQuestion // level → 2 questions; [0] primary, [1] swap alternate
	// The full pre-selected layout (the SWAP PATH's question pool): it is
	// persisted identically in room_questions and is never re-selected or
	// mutated by a swap — a swap only re-points a SEAT at another variant
	// of the SAME level (SPEC §2 `room_questions`).

	resolved map[int]*seatResolution // seat → resolution of the CURRENT level

	// removals is the server-authoritative state of the fifty_fifty
	// lifeline for the CURRENT question (M5-review finding 1): seat → the
	// two removed wrong-option indices. One entry per seat is set exactly
	// ONCE, atomically with the guarded DB spend (SpendLifeline); the
	// room_state snapshot echoes a seat's own entry so a mid-question
	// rejoin never resurrects removed options and parallel recomputes can
	// never broadcast DIFFERENT pairs. Cleared per level (issueLevelLocked)
	// and for the SWAPPING SEAT on a swap — the replacement question arrives
	// untouched (removals are per-question; documented M5 semantics). Wave
	// 5 scopes that to the owner: a peer's removals belong to the peer's
	// UNTOUCHED question and therefore survive.
	removals map[int][]int

	// Retry budget (M4 review finding 2): the two retry paths hold SEPARATE
	// per-level counters so they can never eat each other's budget — a
	// per-path cap of advanceRetryAttempts (3) re-arms per level, not a
	// combined one. Both reset on every issueLevelLocked.
	//
	// retries        — advance path (scheduleAdvanceRetryLocked after a
	//                  failing roster read in advanceIfNeededLocked)
	// timeoutRetries — timeout path (onTimeout's own re-arms)
	retries        int
	timeoutRetries int

	// M7 game identity & pacing:
	//
	// mode pins the room's mode at StartGame ("solo" | "duel") — the
	// advance policy is mode-keyed: duels issue the next level after the
	// reveal beat, solo holds for the Continue endpoint (invariant:
	// exactly one auto-advance or hold decision per level; issueLevelLocked
	// / finishLocked clear the beat, so a scheduled one can never leak
	// into the next level).
	//
	// lastResolvedAt is the most recent resolution instant of the CURRENT
	// level (answer, walk, or timeout — the single player in solo is the
	// only writer). It is the Continue floor: a continue accepted earlier
	// than the beat since this instant is rejected as "too soon".
	//
	// beat is the scheduled duel advance timer; non-nil from the moment
	// all-resolved is reached (duels) until the level issues or the game
	// finishes. Exactly-once per level: it is set under g.mu only when
	// nil and re-cleared by issueLevelLocked.
	mode           string
	lastResolvedAt time.Time
	beat           *time.Timer
}

// GameService owns the question lifecycle for all rooms. Concurrency: one
// mutex guards the games map AND each room's resolution flow — the REST
// answer path and the time.AfterFunc fallback both resolve through it, so
// a late answer can never double-score against a timeout (SPEC §2
// invariant 3).
type GameService struct {
	mu    sync.Mutex
	games map[string]*roomGame

	// Wave-4 in-flight guards for the two room-lifecycle commands: a
	// start/replay POST that is mid-selection (a slow DB round trip)
	// must make a CONCURRENT duplicate POST an idempotent no-op, not a
	// second selection racing the first. Cleared by deferred cleanup on
	// every exit path (success, rule rejection, or selection failure —
	// a failed attempt stays retryable).
	starting  map[string]bool
	replaying map[string]bool

	db            ksql.Provider
	rooms         RoomService
	players       repository.PlayerRepository
	questions     repository.QuestionRepository
	roomQuestions repository.RoomQuestionRepository
	totals        repository.RoomTotalsRepository // wave-4B per-room session totals

	notify Notifier

	budgets map[string]time.Duration // tier → budget; test hook overrides

	// revealBeat is the resolve-pacing beat: duels delay the next level by
	// it after all players resolved; the solo Continue endpoint rejects a
	// continue less than it after the last resolution. Tests may inject a
	// smaller value (or 0 for synchronous advance) via SetRevealBeat.
	revealBeat time.Duration

	// rng is the randomness seam for the lifelines (M5): the audience
	// remainder split and the fifty_fifty wrong-index pick. Default is the
	// math/rand/v2 global (auto-seeded, concurrency-safe). Tests inject a
	// fixed-seed value for determinism. Every call happens while g.mu is
	// held, so even a non-thread-safe rand would be safe here.
	rng RNG
}

// RNG is the injectable randomness seam (see GameService.rng).
type RNG interface {
	IntN(n int) int
}

// globalRNG adapts the auto-seeded math/rand/v2 package-level functions
// (safe for concurrent use) to the RNG seam.
type globalRNG struct{}

func (globalRNG) IntN(n int) int { return rand.IntN(n) }

func NewGameService(
	db ksql.Provider,
	rooms RoomService,
	players repository.PlayerRepository,
	questions repository.QuestionRepository,
	roomQuestions repository.RoomQuestionRepository,
	notify Notifier,
) *GameService {
	// budgets must be a COPY of DefaultBudgets: SetBudgetForTier mutates
	// the map, and sharing the package-level map would leak one test's
	// overrides into every later GameService (found by the M4 duel
	// integration test running after a budget-override test).
	budgets := make(map[string]time.Duration, len(DefaultBudgets))
	for tier, d := range DefaultBudgets {
		budgets[tier] = d
	}
	return &GameService{
		games:         map[string]*roomGame{},
		starting:      map[string]bool{},
		replaying:     map[string]bool{},
		db:            db,
		rooms:         rooms,
		players:       players,
		questions:     questions,
		roomQuestions: roomQuestions,
		totals:        repository.NewRoomTotalsRepository(db),
		notify:        notify,
		budgets:       budgets,
		revealBeat:    duelRevealBeat,
		rng:           globalRNG{},
	}
}

// SetRevealBeat overrides the resolve-pacing beat (tests only): duels
// issue the next level this long after the level fully resolves, and the
// solo Continue endpoint requires that much time since the last
// resolution. 0 = synchronous advance (legacy pre-M7 semantics used by
// the service-level tests' helpers; production keeps duelRevealBeat).
func (g *GameService) SetRevealBeat(d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.revealBeat = d
}

// SetBudgetForTier overrides one tier's timer budget (tests only).
func (g *GameService) SetBudgetForTier(tier string, d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.budgets[tier] = d
}

// SetRNG injects a deterministic randomness source (tests only).
func (g *GameService) SetRNG(r RNG) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rng = r
}

// StartGame picks the room's 15×2 questions (per-level same-tier random,
// options shuffled server-side, correct index remapped), persists the
// selection + layout and the room flip to `active` in one transaction
// (M1 review finding 5), then issues level 1 with the game_started event.
// Errors leave the room in `lobby` (no partial broadcast) — the caller
// logs; the connected players simply wait in the lobby.
//
// Since wave 4 this is the SOLO instant-play start (SPEC v1.1) and the
// internal engine primitive the service tests drive directly. DUELS no
// longer auto-start through it at seat-2 join: the first duel question
// is issued only by the host's StartDuel (POST /api/rooms/:code/start).
func (g *GameService) StartGame(ctx context.Context, room *models.Room, players []models.Player) error {
	g.mu.Lock()
	if g.games[room.ID] != nil {
		g.mu.Unlock() // already started (defensive; handlers call this once)
		return nil
	}
	g.mu.Unlock()

	list, err := g.beginRound(ctx, room, "active")
	if err != nil {
		return err
	}

	question, endsAt := g.issueFirstLevel(room, list)

	g.notify.Broadcast(room.ID, map[string]interface{}{
		"event":    "game_started",
		"question": question,
		"ends_at":  endsAt,
	})
	return nil
}

// beginRound is the shared start/replay pipeline: a FRESH 15×2 selection
// through the CURRENT selection service (S2/S1 blend rules — unchanged
// for a rematch), then ONE transaction persisting the layout together
// with the room status flip. The room's previous layout is DELETED first
// in the same transaction: a replay reuses the room, and room_questions
// is composite-keyed (room_id, level, variant) — a second run must
// replace, never duplicate, the 30 rows. For a fresh room the delete is
// a no-op. Selection errors return before any write (the room keeps its
// previous state — retryable).
func (g *GameService) beginRound(ctx context.Context, room *models.Room, status string) (map[int][]levelQuestion, error) {
	list, err := g.selectQuestions(ctx, room.ID)
	if err != nil {
		return nil, err
	}

	err = g.db.Transaction(ctx, func(tx ksql.Provider) error {
		if err := repository.NewRoomQuestionRepository(tx).DeleteByRoom(ctx, room.ID); err != nil {
			return err
		}
		batch := make([]models.RoomQuestion, 0, 30)
		now := time.Now().UTC()
		for level := 1; level <= 15; level++ {
			for v := 0; v < 2; v++ {
				batch = append(batch, models.RoomQuestion{
					RoomID:               room.ID,
					Level:                level,
					QuestionID:           list[level][v].questionID,
					Variant:              v + 1,
					ShuffledOptions:      optionsJSON(list[level][v].options),
					ShuffledCorrectIndex: list[level][v].correctIndex,
					CreatedAt:            now,
				})
			}
		}
		if err := repository.NewRoomQuestionRepository(tx).InsertBatch(ctx, batch); err != nil {
			return err
		}
		return repository.NewRoomRepository(tx).UpdateStatus(ctx, room.ID, status)
	})
	if err != nil {
		return nil, fmt.Errorf("persisting room questions: %w", err)
	}
	return list, nil
}

// issueFirstLevel registers the in-memory game for the room and issues
// level 1 (timeout fallback armed), exactly like the pre-wave-4 StartGame
// body did. The caller broadcasts game_started AFTER it returns. Shared
// by StartGame and the solo branch of Replay. Concurrent-start safe: a
// game registered meanwhile wins and its live question is returned.
func (g *GameService) issueFirstLevel(room *models.Room, list map[int][]levelQuestion) (QuestionView, time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if game := g.games[room.ID]; game != nil { // started concurrently meanwhile
		q := activeQuestionLocked(game, 1)
		return QuestionView{Level: game.level, Text: q.text, Options: q.options}, game.endsAt
	}
	game := &roomGame{
		variants:      list,
		resolved:      map[int]*seatResolution{},
		removals:      map[int][]int{},
		seatQuestions: map[int]levelQuestion{},
		swapped:       map[int]bool{},
		mode:          room.Mode,
	}
	g.games[room.ID] = game
	return g.issueLevelLocked(room.ID, game, 1)
}

// StartDuel implements the wave-4 DEFERRED DUEL START: a seat-2 join no
// longer deals anything — rooms.status stays 'lobby' (that status IS the
// in-memory-free waiting flag) and the joiner's room_state snapshot
// already shows the lobby. The duel becomes live only when the HOST calls
// POST /api/rooms/:code/start, which lands here.
//
// Contract:
//   - already running (or a start already in flight) → started=false,
//     nil error — the idempotent double-start no-op;
//   - solo room → 409 (solo starts instantly on creation; unchanged);
//   - non-host (seat != 1) → 403 (the handler relays an error event to
//     that seat's socket);
//   - seat 2 not seated yet → 409 (a duel must not start half-empty);
//   - finished room → 409 (replay is the path back to a lobby);
//   - success: exactly the M7 start path — selection, one persistence
//     transaction, game_started broadcast carrying Q1 + ends_at,
//     rooms.status → active.
func (g *GameService) StartDuel(ctx context.Context, room *models.Room, player *models.Player) (bool, error) {
	// Surface checks come first: a SOLO room (instant-play, already
	// running or not) has NO start endpoint, and a non-host is refused
	// regardless of the room's state.
	if room.Mode != "duel" {
		return false, &GameError{Status: 409, Message: "solo rooms start instantly on creation — nothing to start"}
	}
	if player.Seat != 1 {
		return false, &GameError{Status: 403, Message: "only the host (seat 1) can start the duel"}
	}

	g.mu.Lock()
	if g.games[room.ID] != nil || g.starting[room.ID] {
		g.mu.Unlock()
		return false, nil // already started / start already in flight — no-op
	}
	g.starting[room.ID] = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.starting, room.ID)
		g.mu.Unlock()
	}()

	players, err := g.players.GetPlayersByRoom(ctx, room.ID)
	if err != nil {
		log.Printf("StartDuel: room %s: loading players: %v", room.ID, err)
		return false, &GameError{Status: 500, Message: "start could not read the room"}
	}
	if len(players) < 2 {
		return false, &GameError{Status: 409, Message: "waiting for the second player before the duel can start"}
	}
	// The caller's room row may be stale (resolved before a concurrent
	// state flip); the DB is the waiting-flag authority for the start.
	current, err := g.rooms.GetRoom(ctx, room.ID)
	if err != nil {
		log.Printf("StartDuel: room %s: re-reading the room: %v", room.ID, err)
		return false, &GameError{Status: 500, Message: "start could not read the room"}
	}
	if current.Status == "active" {
		return false, &GameError{Status: 409, Message: "the duel is already marked active (stale run) — replay or create a new room"}
	}
	if current.Status == "finished" {
		return false, &GameError{Status: 409, Message: "the game is over — use replay to run it again"}
	}

	list, err := g.beginRound(ctx, room, "active")
	if err != nil {
		return false, err
	}
	question, endsAt := g.issueFirstLevel(room, list)
	g.notify.Broadcast(room.ID, map[string]interface{}{
		"event":    "game_started",
		"question": question,
		"ends_at":  endsAt,
	})
	return true, nil
}

// Replay resets a FINISHED room for a fresh run IN THE SAME ROOM (wave-4
// user decision: play again must NOT mint a new room/code). Either lapsed
// seat may initiate; both get reset symmetrically.
//
// Rejections:
//   - a live in-memory game, or any seat whose run is still `active`
//     (mid-run) → 409 "a run is still in progress";
//   - a lobby-state room (a duel that never started, or a rematch
//     already reset and waiting) → 409;
//   - unknown token → 403 at the handler.
//
// A concurrent double-replay is an idempotent no-op (replayed=false).
//
// Effects: a brand-new 15×2 selection through the CURRENT selection
// service (same S2/S1 blend rules — casual L1–3, wwtbam@level), the old
// room_questions layout replaced in one transaction, every player row
// reset (status `active`, amount 0, level_reached 0, lifelines default),
// and a `rematch_ready` broadcast listing the seats (the FE re-shows the
// lobby). Then:
//   - duel: rooms.status stays `lobby` — the rematch waits for the
//     host's start endpoint (replayed=true, started=false);
//   - solo: the reset immediately issues level 1 (solo instant-play has
//     no host/lobby step) — game_started broadcast (replayed=true,
//     started=true).
func (g *GameService) Replay(ctx context.Context, room *models.Room, player *models.Player) (replayed, started bool, err error) {
	g.mu.Lock()
	if game := g.games[room.ID]; game != nil {
		g.mu.Unlock()
		return false, false, &GameError{Status: 409, Message: "a run is still in progress"}
	}
	if g.replaying[room.ID] {
		g.mu.Unlock()
		return false, false, nil // replay already in flight — idempotent no-op
	}
	g.replaying[room.ID] = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.replaying, room.ID)
		g.mu.Unlock()
	}()

	// The caller's room row may be stale (resolved before a concurrent
	// state flip); re-read. A lobby room has nothing to replay (never
	// started, or a rematch already waiting for the host's start).
	current, err := g.rooms.GetRoom(ctx, room.ID)
	if err != nil {
		log.Printf("Replay: room %s: re-reading the room: %v", room.ID, err)
		return false, false, &GameError{Status: 500, Message: "replay could not read the room"}
	}
	if current.Status == "lobby" {
		return false, false, &GameError{Status: 409, Message: "nothing to replay — the room is waiting in the lobby"}
	}

	players, err := g.players.GetPlayersByRoom(ctx, room.ID)
	if err != nil {
		log.Printf("Replay: room %s: loading players: %v", room.ID, err)
		return false, false, &GameError{Status: 500, Message: "replay could not read the room"}
	}
	for _, p := range players {
		if p.Status == "active" {
			return false, false, &GameError{Status: 409, Message: "a run is still in progress"}
		}
	}

	if room.Mode == "solo" {
		log.Printf("Replay: room %s: solo replay — reset in place (private room, no code surface)", room.ID)
	} else {
		log.Printf("Replay: room %s: duel replay — both seats reset, room back to lobby (same room, same code)", room.ID)
	}

	if err := g.players.ResetRoomPlayers(ctx, room.ID, models.DefaultLifelinesLeft); err != nil {
		return false, false, fmt.Errorf("resetting players for replay: %w", err)
	}

	// Solo instant-play re-arms with the first question right away; a
	// duel waits for the host's start (rooms.status → lobby).
	status := "lobby"
	if room.Mode == "solo" {
		status = "active"
	}
	list, err := g.beginRound(ctx, room, status)
	if err != nil {
		return false, false, err
	}

	seats := make([]int, 0, len(players))
	for _, p := range players {
		seats = append(seats, p.Seat)
	}
	sort.Ints(seats)
	g.notify.Broadcast(room.ID, map[string]interface{}{
		"event": "rematch_ready",
		"seats": seats,
	})

	if room.Mode != "solo" {
		return true, false, nil // duel: the rematch now waits for the host's start
	}
	question, endsAt := g.issueFirstLevel(room, list)
	g.notify.Broadcast(room.ID, map[string]interface{}{
		"event":    "game_started",
		"question": question,
		"ends_at":  endsAt,
	})
	return true, true, nil
}

// selectQuestions samples two DISTINCT questions per level under the v2
// rules (SPEC §2 question engine v2): per-level strength BAND via
// quantile rank windows, the per-room CULTURAL CAP everywhere except
// levels 1–3 (always non-cultural there), with widening fallback and,
// finally, the legacy tier sampling when the rated corpus cannot yield
// candidates at all (e.g. before the labeling pipeline rated anything).
// Exclusions grow as questions are picked, so variants never duplicate
// each other or another level.
func (g *GameService) selectQuestions(ctx context.Context, roomID string) (map[int][]levelQuestion, error) {
	list := make(map[int][]levelQuestion, 15)
	draw := &roomDraw{excluded: map[string]bool{}}

	ratedN, err := g.questions.CountRated(ctx)
	if err != nil {
		return nil, fmt.Errorf("counting rated questions for band selection: %w", err)
	}
	if ratedN < minRatedForBands {
		log.Printf("selectQuestions: room %s: only %d rated questions (< %d) — using the legacy tier fallback for every level",
			roomID, ratedN, minRatedForBands)
	}

	for level := 1; level <= 15; level++ {
		list[level] = make([]levelQuestion, 0, 2)
		for v := 0; v < 2; v++ {
			q, err := g.drawOne(ctx, roomID, level, draw, ratedN)
			if err != nil {
				return nil, err
			}
			draw.excluded[q.ID] = true
			pq, err := shuffleQuestion(q)
			if err != nil {
				return nil, err
			}
			list[level] = append(list[level], pq)
		}
	}
	return list, nil
}

// roomDraw accumulates what a single room's selection has consumed (the
// exclusion set and the running cultural-pick count — the room-lifetime
// taste state; SPEC §2: ≤30% cultural ≈ max 9 of the 30 picks).
type roomDraw struct {
	excluded  map[string]bool
	cultural  int
	orderedEx []string // stable slice for the SQL exclude list
}

func (d *roomDraw) isExcluded(id string) bool { return d.excluded[id] }

func (d *roomDraw) exclude(id string) {
	if d.excluded[id] {
		return
	}
	d.excluded[id] = true
	d.orderedEx = append(d.orderedEx, id)
}

// minRatedForBands is the smallest rated corpus worth querying per band:
// below it every band query would still empty out, so selection skips
// straight to the legacy tier fallback (log above).
const minRatedForBands = 15

// maxCulturalPerRoom caps CULTURAL picks per room (15 levels × 2 variants
// = 30 picks): ~30% ≈ 9 cultural questions across the whole run.
const maxCulturalPerRoom = 9

// levelsForcedNonCultural: levels 1..3 are pub-quiz round 1 — cultural
// share must be near-zero there, so they NEVER draw cultural questions
// (band widening also keeps their filter).
const levelsForcedNonCultural = 3

// S2 CASUAL-POOL RULE + S3 WWTBAM BLEND (v3-ux-plan "Question quality",
// labels-first pivot):
//
// Levels 1–3 NEVER draw from the raw tier pool. Their draw is the
// WWTBAM ∪ CASUAL blend (M10 S3): a per-pick 50/50 coin over the rng seam
// decides which pool is tried first —
//
//   - pool A (the coin's first): WWTBAM@level (source='wwtbam',
//     strength == the level — the real show calibration) OR the S2
//     casual pool (strength 1..casualMaxStrength=3, non-blacklisted,
//     wwtbam rows EXCLUDED — the blend may only mix OTHER sources'
//     rows at L1–3, so a wwtbam row is never served off-level);
//   - pool A yields nothing → pool B (the other one);
//   - BOTH pools dry → the EXISTING S2 degradation chain (widened casual
//     strength 1..5, then the fail-safe opentdb easy pool) — logged;
//   - honest error — the raw tier pool is still NOT an L1–3 option.
//
// Levels 4–15: PRIMARILY WWTBAM@level; the existing band rules (quantile
// rank windows → widening → legacy tier) take over only when the wwtbam
// pool for that level holds fewer than wwtbamMinPool=2 candidates (the
// degradation is logged).
const (
	casualMaxStrength     = 3
	casualWideMaxStrength = 5

	// wwtbamSource matches models Question.source for the WWTBAM ingest
	// (service.WWTBAMSource value duplicated here to avoid an import
	// cycle through models; pin-tested in wwtbam_test.go).
	wwtbamSource = "wwtbam"

	// wwtbamMinPool: both variants of a level draw from the SAME pool
	// rules, so a wwtbam level pool below 2 candidates cannot serve the
	// level — selection degrades to the band rules.
	wwtbamMinPool = 2
)

// failSafeCasualCategories is the S2 fail-safe category set for levels
// 1–3. "Food & Drink" is kept for corpus parity with the approved plan
// rule even though the current seeder imports no such opentdb rows yet
// (the extra filter value is then simply never matched).
var failSafeCasualCategories = []string{
	"General Knowledge", "Geography", "Sports", "History", "Food & Drink",
}

// allowCulturalLocked is the v2 taste decision for one pick: cultural
// candidates are allowed only past level 3 and only as long as the room
// is under its 9-pick cap. Pure + deterministic (unit-tested).
func allowCulturalLocked(level, culturalPicks int) bool {
	return level > levelsForcedNonCultural && culturalPicks < maxCulturalPerRoom
}

// bandBounds maps a level to its 15-band quantile rank window over a rated
// corpus of n questions (1-based ranks, ordered by strength): band L owns
// ranks (L-1)*n/15 + 1 .. L*n/15. Pure + deterministic (unit-tested).
func bandBounds(level, n int) (lo, hi int) {
	lo = (level-1)*n/15 + 1
	hi = level * n / 15
	if hi < lo {
		hi = lo // degenerate tiny corpora: a one-rank window
	}
	return lo, hi
}

// bandSize is one ranking step when widening a band window: the mean
// band occupancy (floor(n/15), ≥1) — used ±1 / ±2 ranks per widening
// step so the window grows to precisely the adjacent bands.
func bandSize(n int) int {
	if b := n / 15; b > 0 {
		return b
	}
	return 1
}

// drawOne picks ONE question for a level under the v2 rules + the S3
// WWTBAM blend:
//
//  1. levels 1–3: the WWTBAM ∪ CASUAL blend (drawCasualBlend — the coin
//     picks the first pool; both dry → the S2 degradation chain; never
//     the bands, never the raw tier pool);
//  2. levels 4–15: WWTBAM@level first (strength == the level; degraded
//     to the band rules when the pool has < 2 candidates or empties
//     under the room's exclusions — logged);
//  3. band query for the level's quantile window with the taste decision
//     (unchanged M6 machinery);
//  4. widening: on an empty window the adjacent bands blend in, ±1 then
//     ±2 (each step logged; documented corner case for small-rated
//     corpora);
//  5. last resort: the legacy M3 tier sampling (unchanged behavior; also
//     the whole path pre-labeling — logged; it may return a cultural
//     question since the tier pool predates the taste layer).
//
// Both variants of a level go through the SAME rules; exclusions (this
// room's picks so far) narrow every query.
func (g *GameService) drawOne(ctx context.Context, roomID string, level int, draw *roomDraw, ratedN int) (models.Question, error) {
	if level <= levelsForcedNonCultural {
		return g.drawCasualBlend(ctx, roomID, level, draw)
	}

	// S3: WWTBAM@level first for 4..15 (count-gated pool; degradation
	// logged inside).
	picked, served, err := g.drawWWTBAMLevel(ctx, roomID, level, draw)
	if err != nil {
		return models.Question{}, err
	}
	if served {
		return picked, nil
	}

	if ratedN < minRatedForBands {
		log.Printf("selectQuestions: room %s: level %d: rated corpus too small (%d) — falling back to tier sampling",
			roomID, level, ratedN)
		return g.drawLegacyTier(ctx, level, draw)
	}

	culturalAllowed := allowCulturalLocked(level, draw.cultural)
	lo, hi := bandBounds(level, ratedN)
	for widen := 0; widen <= 2; widen++ {
		wlo, whi := lo-widen*bandSize(ratedN), hi+widen*bandSize(ratedN)
		if wlo < 1 {
			wlo = 1
		}
		if whi > ratedN {
			whi = ratedN
		}
		picked, err := g.questions.RandomByRankBand(ctx, wlo, whi, !culturalAllowed, 1, draw.orderedEx)
		if err != nil {
			return models.Question{}, fmt.Errorf("sampling band questions for level %d: %w", level, err)
		}
		if len(picked) > 0 && !draw.isExcluded(picked[0].ID) {
			cand := picked[0]
			draw.exclude(cand.ID)
			if culturalAllowed && models.IsCultural(cand.Category, cand.Source) {
				draw.cultural++
			}
			return cand, nil
		}
		if widen == 0 {
			log.Printf("selectQuestions: room %s: level %d: empty band window [%d..%d] (rated=%d) — widening to adjacent bands",
				roomID, level, lo, hi, ratedN)
		}
	}
	log.Printf("selectQuestions: room %s: level %d: band pool exhausted (rated=%d) — falling back to tier sampling",
		roomID, level, ratedN)
	return g.drawLegacyTier(ctx, level, draw)
}

// drawLegacyTier is the M3-era last-resort sampling: one random question
// of the level's tier from any source (strength ignored — it is what
// games played before the band corpus existed).
//
// S2 NOTE: this path is UNREACHABLE for levels 1–3 — drawOne routes those
// levels to drawCasual, and no branch of the casual order falls back here
// (the raw tier pool may serve Entertainment/Video Games/Gadgets rows a
// level-1 player must never face; see TestCasualSelection* in
// casual_test.go).
func (g *GameService) drawLegacyTier(ctx context.Context, level int, draw *roomDraw) (models.Question, error) {
	tier := TierFor(level)
	picked, err := g.questions.RandomByTier(ctx, tier, 1, draw.orderedEx)
	if err != nil {
		return models.Question{}, fmt.Errorf("sampling %s questions for level %d: %w", tier, level, err)
	}
	if len(picked) == 0 {
		return models.Question{}, fmt.Errorf("not enough %s questions in the pool for level %d", tier, level)
	}
	draw.exclude(picked[0].ID)
	return picked[0], nil
}

// drawCasual implements the S2 L1–3 guarantee in code (see the rule block
// above levelsForcedNonCultural):
//
//  1. casual pool: strength 1..3, casual-blacklist-filtered,
//     wwtbam rows EXCLUDED (they are strictly strength == level),
//     room-exclusions applied;
//  2. widened window: strength 1..5 (same blacklist) — logged;
//  3. fail-safe: opentdb easy rows over failSafeCasualCategories —
//     logged as the DEGRADED state (the pool is not strength-verified,
//     but every category in it is vetted round-1 material);
//  4. honest error when even the fail-safe pool is exhausted — the raw
//     tier pool is intentionally NOT a fallback for levels 1–3.
func (g *GameService) drawCasual(ctx context.Context, roomID string, level int, draw *roomDraw) (models.Question, error) {
	picked, err := g.questions.RandomByCasual(ctx, casualMaxStrength, 1, draw.orderedEx)
	if err != nil {
		return models.Question{}, fmt.Errorf("sampling casual pool for level %d: %w", level, err)
	}
	if len(picked) > 0 {
		draw.exclude(picked[0].ID)
		return picked[0], nil
	}

	log.Printf("selectQuestions: room %s: level %d: casual pool (strength 1..%d) empty (%d excluded so far) — widening to strength 1..%d",
		roomID, level, casualMaxStrength, len(draw.orderedEx), casualWideMaxStrength)
	picked, err = g.questions.RandomByCasual(ctx, casualWideMaxStrength, 1, draw.orderedEx)
	if err != nil {
		return models.Question{}, fmt.Errorf("sampling widened casual pool for level %d: %w", level, err)
	}
	if len(picked) > 0 {
		draw.exclude(picked[0].ID)
		return picked[0], nil
	}

	log.Printf("selectQuestions: room %s: level %d: casual pools both dry — DEGRADED to the fail-safe opentdb easy pool %v (S2)",
		roomID, level, failSafeCasualCategories)
	return g.drawFailSafeCasual(ctx, level, draw)
}

// drawFailSafeCasual serves the last resort of the casual order (see
// drawCasual): one random opentdb easy question from the fail-safe
// categories. Errors (never falls through to tier sampling).
func (g *GameService) drawFailSafeCasual(ctx context.Context, level int, draw *roomDraw) (models.Question, error) {
	picked, err := g.questions.RandomByFailSafeCasual(ctx, failSafeCasualCategories, 1, draw.orderedEx)
	if err != nil {
		return models.Question{}, fmt.Errorf("sampling fail-safe casual pool for level %d: %w", level, err)
	}
	if len(picked) == 0 {
		return models.Question{}, fmt.Errorf("casual pools and the fail-safe opentdb pool both exhausted (level %d) — no S2-compliant question left", level)
	}
	draw.exclude(picked[0].ID)
	return picked[0], nil
}

// ---------------------------------------------------------------------------
// S3 WWTBAM BLEND (M10): per-level pools from the real show corpus
// ---------------------------------------------------------------------------

// drawCasualBlend implements the levels 1–3 policy: WWTBAM@level ∪ casual
// (see the S2/S3 rule block above casualMaxStrength for the exact order).
// A 50/50 coin over the rng seam picks which pool is tried first; the
// other pool covers the first one yielding nothing; BOTH dry → the
// existing degradation chain (drawCasual: widened casual → fail-safe,
// each step logged there).
func (g *GameService) drawCasualBlend(ctx context.Context, roomID string, level int, draw *roomDraw) (models.Question, error) {
	wwtbamFirst := g.rng.IntN(2) == 0

	if wwtbamFirst {
		q, served, err := g.tryWWTBAMLevel(ctx, roomID, level, draw)
		if err != nil {
			return models.Question{}, err
		}
		if served {
			return q, nil
		}
		// wwtbam pool dry → the S2 casual chain resolves (its own pool,
		// then widening, then the fail-safe — logged inside).
		return g.drawCasual(ctx, roomID, level, draw)
	}

	// Casual first (the strict 1..3 window, no widening yet — widening is
	// degradation, not the blend).
	picked, err := g.questions.RandomByCasual(ctx, casualMaxStrength, 1, draw.orderedEx)
	if err != nil {
		return models.Question{}, fmt.Errorf("sampling casual pool for level %d: %w", level, err)
	}
	if len(picked) > 0 {
		draw.exclude(picked[0].ID)
		return picked[0], nil
	}
	// Casual window dry → the other blend pool (WWTBAM@level).
	q, served, err := g.tryWWTBAMLevel(ctx, roomID, level, draw)
	if err != nil {
		return models.Question{}, err
	}
	if served {
		return q, nil
	}
	// Both blend pools dry → the existing degradation chain (widened
	// casual window + the fail-safe pool; logged there).
	log.Printf("selectQuestions: room %s: level %d: blend pools BOTH dry (casual 1..%d and wwtbam@%d) — DEGRADING to the S2 chain (widened casual → fail-safe)",
		roomID, level, casualMaxStrength, level)
	return g.drawCasual(ctx, roomID, level, draw)
}

// tryWWTBAMLevel attempts ONE draw from the wwtbam pool at exactly this
// level (source == 'wwtbam', strength == level, room-exclusions applied).
// served=false means the pool yielded nothing — the caller's documented
// fallback takes over.
func (g *GameService) tryWWTBAMLevel(ctx context.Context, roomID string, level int, draw *roomDraw) (models.Question, bool, error) {
	picked, err := g.questions.RandomBySourceLevel(ctx, wwtbamSource, level, 1, draw.orderedEx)
	if err != nil {
		return models.Question{}, false, fmt.Errorf("sampling wwtbam pool for level %d: %w", level, err)
	}
	if len(picked) == 0 {
		return models.Question{}, false, nil
	}
	draw.exclude(picked[0].ID)
	return picked[0], true, nil
}

// drawWWTBAMLevel is the levels 4–15 primary pool: it requires the level
// pool to hold at least wwtbamMinPool=2 candidates BEFORE exclusions (both
// variants must be servable from the same pool) and draws one random row
// minus the room's exclusions. served=false carries the logged degradation
// trigger (small pool < 2, or the exclusions emptied the remaining pool).
func (g *GameService) drawWWTBAMLevel(ctx context.Context, roomID string, level int, draw *roomDraw) (models.Question, bool, error) {
	count, err := g.questions.CountBySourceLevel(ctx, wwtbamSource, level)
	if err != nil {
		return models.Question{}, false, fmt.Errorf("counting wwtbam pool for level %d: %w", level, err)
	}
	if count < wwtbamMinPool {
		log.Printf("selectQuestions: room %s: level %d: wwtbam pool has %d candidates (< %d) — DEGRADED to the band rules",
			roomID, level, count, wwtbamMinPool)
		return models.Question{}, false, nil
	}
	q, served, err := g.tryWWTBAMLevel(ctx, roomID, level, draw)
	if err != nil || served {
		return q, served, err
	}
	log.Printf("selectQuestions: room %s: level %d: wwtbam pool (%d rows, %d excluded this room) exhausted — DEGRADED to the band rules",
		roomID, level, count, len(draw.orderedEx))
	return models.Question{}, false, nil
}

// shuffleQuestion parses a stored question and produces the room-specific
// layout: options in a random server-chosen order, correct index remapped
// into that order (SPEC §2 invariant 4).
func shuffleQuestion(q models.Question) (levelQuestion, error) {
	var options []string
	if err := json.Unmarshal([]byte(q.Options), &options); err != nil {
		return levelQuestion{}, fmt.Errorf("question %s options: %w", q.ID, err)
	}
	if len(options) != 4 {
		return levelQuestion{}, fmt.Errorf("question %s has %d options, want 4", q.ID, len(options))
	}
	shuffled := append([]string(nil), options...)
	// math/rand/v2 top-level functions are safe for concurrent use and
	// auto-seeded; the shuffle needs no reproducibility.
	rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	correct := options[q.CorrectIndex]
	correctIndex := -1
	for i, o := range shuffled {
		if o == correct {
			correctIndex = i
			break
		}
	}
	if correctIndex == -1 { // cannot happen for a permutation, but never guess
		return levelQuestion{}, fmt.Errorf("question %s: correct option lost in shuffle", q.ID)
	}
	return levelQuestion{questionID: q.ID, text: q.Question, options: shuffled, correctIndex: correctIndex}, nil
}

func optionsJSON(options []string) string {
	b, err := json.Marshal(options)
	if err != nil { // []string always marshals; belt and suspenders
		return "[]"
	}
	return string(b)
}

// activeQuestionLocked returns the question SEAT is currently answering at
// game.level — the wave-5 per-seat active question. A seat with no entry in
// seatQuestions (nobody has swapped it, or the level was just issued) reads
// the shared primary `current`. Every per-question reader goes through here;
// reading `current` directly would re-introduce the room-wide swap the
// product decision forbids. Caller holds g.mu.
func activeQuestionLocked(game *roomGame, seat int) levelQuestion {
	if q, ok := game.seatQuestions[seat]; ok && q.questionID != "" {
		return q
	}
	return game.current
}

// issueLevelLocked sets the game to `level` and arms the timeout fallback.
// Caller holds g.mu. Timers are recreated per level; the previous one is
// stopped (and a fired-but-blocked callback is neutralized by the level
// guard in onTimeout). Per-level state resets here too: resolutions, the
// fifty_fifty removals, the wave-5 PER-SEAT active questions and the
// per-level swap ledger (both seats start the level on the shared primary).
// The M7 beat timer is also cleared here: once the next level is dealt
// the previous level's advance decision is over — a beat can never leak
// a second question_next into a level (exactly-once per level).
func (g *GameService) issueLevelLocked(roomID string, game *roomGame, level int) (QuestionView, time.Time) {
	q := game.variants[level][0]
	game.current = q
	budget := BudgetFor(g.budgets, level)
	endsAt := time.Now().Add(budget)
	game.level = level
	game.endsAt = endsAt
	game.resolved = map[int]*seatResolution{}
	game.removals = map[int][]int{}
	game.seatQuestions = map[int]levelQuestion{}
	game.swapped = map[int]bool{}
	game.retries = 0
	game.timeoutRetries = 0
	game.lastResolvedAt = time.Time{} // no resolution this level yet
	if game.beat != nil {
		game.beat.Stop()
		game.beat = nil
	}
	if game.timer != nil {
		game.timer.Stop()
	}
	game.timer = time.AfterFunc(budget, func() { g.onTimeout(roomID, level, 0) })
	return QuestionView{Level: level, Text: q.text, Options: q.options}, endsAt
}

// Snapshot serves the room_state question field on (re)join: the SEAT'S OWN
// active question at the current level (wave 5 — a seat that swapped gets
// its swapped question back on rejoin; the uniform shape, no correct index),
// ends_at, and the CALLER's own fifty_fifty removals when they applied one
// to that question (M5-review finding 1: a mid-question rejoin must not
// resurrect removed options — per-seat because each player may have their own
// 50:50 on the same question). nil removals = no own 50:50 on the active
// question.
func (g *GameService) Snapshot(roomID string, seat int) (*QuestionView, *time.Time, []int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	game := g.games[roomID]
	if game == nil {
		return nil, nil, nil
	}
	q := activeQuestionLocked(game, seat)
	view := QuestionView{Level: game.level, Text: q.text, Options: q.options}
	endsAt := game.endsAt
	var removed []int
	if rs := game.removals[seat]; rs != nil {
		removed = append([]int(nil), rs...)
	}
	return &view, &endsAt, removed
}

// Answer scores one answer (SPEC §2 invariant 3: exactly one score per
// (player, level) — the second one is rejected). The caller passes the
// player row it resolved by token; status must be `active` (invariant 2).
// Returns the private answer_result payload.
func (g *GameService) Answer(ctx context.Context, roomID string, player *models.Player, level, choiceIndex int) (AnswerOutcome, error) {
	outcome, sends, gerr := func() (AnswerOutcome, []outbound, *GameError) {
		g.mu.Lock()
		defer g.mu.Unlock()

		game := g.games[roomID]
		if game == nil {
			sends, gerr := reject(player, 409, "no game is running for this room")
			return AnswerOutcome{}, sends, gerr
		}
		if game.level != level {
			sends, gerr := reject(player, 409, fmt.Sprintf("level %d is not the active question", level))
			return AnswerOutcome{}, sends, gerr
		}
		if game.resolved[player.Seat] != nil {
			sends, gerr := reject(player, 409, "already answered this level")
			return AnswerOutcome{}, sends, gerr
		}
		if player.Status != "active" {
			sends, gerr := reject(player, 409, "your run has already ended")
			return AnswerOutcome{}, sends, gerr
		}

		q := activeQuestionLocked(game, player.Seat)
		correct := choiceIndex == q.correctIndex
		amount, status := 0, "active"
		if correct {
			amount = AmountAt(level)
			if level == 15 {
				status = "won"
			}
		} else {
			amount = FallAmount(player.LevelReached)
			status = "fallen"
		}

		g.persistResolution(ctx, player, status, amount, level, correct)
		game.resolved[player.Seat] = &seatResolution{choiceIndex: choiceIndex, questionID: q.questionID}
		// M7: the reveal-beat / Continue floor runs from the LAST
		// resolution event of the level.
		game.lastResolvedAt = time.Now()

		sends := []outbound{{seat: player.Seat, msg: map[string]interface{}{
			"event":         "answer_result",
			"correct":       correct,
			"correct_index": q.correctIndex,
			"amount":        amount,
			"status":        status,
		}}}
		if status == "fallen" {
			// Duel improvement (SPEC §1): the OTHER, still-active players
			// see what the fallen player picked (their run ends either
			// way, so carrying correct_index is the allowed exception).
			sends = append(sends, g.revealsForFallenLocked(ctx, roomID, player.Seat, level, q, choiceIndex)...)
		}
		if status != "active" { // fallen (or won at 15): run_ended to the room
			sends = append(sends, outbound{msg: runEndedPayload(player.Seat, status, amount, &q.correctIndex)})
		}
		if status == "active" {
			// Wave-4 spectate: the SURVIVOR's run just advanced — every
			// lapsed seat (fallen/walked) receives the public-fact
			// opponent_progress. Fires after the result is recorded and
			// BEFORE the hold/beat logic below, so the fallen player's
			// spectate ladder updates immediately.
			sends = append(sends, g.progressForLapsedLocked(ctx, roomID, player.Seat, level)...)
		}
		// Advance / finish check runs for EVERY resolution: a correct
		// answer by the last pending active player also unblocks the next
		// level (SPEC §2 invariant 5).
		sends = append(sends, g.advanceIfNeededLocked(ctx, roomID, game)...)
		return AnswerOutcome{Correct: correct, CorrectIndex: q.correctIndex, Amount: amount, Status: status}, sends, nil
	}()
	if gerr != nil {
		g.dispatch(roomID, sends) // the reject() error event for the offender's socket
		return AnswerOutcome{}, gerr
	}
	g.dispatch(roomID, sends)
	return outcome, nil
}

// Walk ends an active player's run before answering the current question
// (SPEC §1): locks in the amount of the last answered level and
// broadcasts run_ended {status: walked}. If no active player remains the
// game finishes immediately.
func (g *GameService) Walk(ctx context.Context, roomID string, player *models.Player) (int, error) {
	amount, sends, gerr := func() (int, []outbound, *GameError) {
		g.mu.Lock()
		defer g.mu.Unlock()

		game := g.games[roomID]
		if game == nil {
			sends, gerr := reject(player, 409, "no game is running for this room")
			return 0, sends, gerr
		}
		if player.Status != "active" {
			sends, gerr := reject(player, 409, "your run has already ended")
			return 0, sends, gerr
		}
		if game.resolved[player.Seat] != nil {
			// They already answered the current question; the level result
			// decides their run — walking is no longer allowed.
			sends, gerr := reject(player, 409, "already answered this level")
			return 0, sends, gerr
		}

		amount := WalkAmount(player.LevelReached)
		g.persistResolution(ctx, player, "walked", amount, player.LevelReached, false)
		game.resolved[player.Seat] = &seatResolution{choiceIndex: -1, questionID: activeQuestionLocked(game, player.Seat).questionID}
		game.lastResolvedAt = time.Now() // M7: resolution event for the beat floor

		sends := []outbound{{
			msg: runEndedPayload(player.Seat, "walked", amount, nil),
		}}
		sends = append(sends, g.advanceIfNeededLocked(ctx, roomID, game)...)
		return amount, sends, nil
	}()
	if gerr != nil {
		g.dispatch(roomID, sends)
		return 0, gerr
	}
	g.dispatch(roomID, sends)
	return amount, nil
}

// allActiveResolvedLocked reports whether every still-active player has
// resolved the game's CURRENT level (SPEC §2 invariant 5's gating
// condition, shared by the beat scheduler and the Continue floor).
// Caller holds g.mu.
func allActiveResolvedLocked(game *roomGame, players []models.Player) bool {
	for _, p := range players {
		if p.Status != "active" {
			continue
		}
		if game.resolved[p.Seat] == nil {
			return false
		}
	}
	return true
}

// Continue implements the solo hold-until-continue pacing (M7, SPEC §1
// resolve pacing): the next question is dealt only when the player asks
// for it — POST /api/rooms/:code/continue (the handler enforces the
// solo-mode-only surface; duels 409 there). Accepted ONLY when every
// active player resolved the current level AND the reveal beat
// (~2.5s = duelRevealBeat) elapsed since the LAST resolution event —
// enforced server-side so a solo client cannot hammer past the reveal
// beat. Effects: the next level is dealt through the standard engine
// path (issueLevelLocked → question_next broadcast; the timer budget
// starts at issue, exactly like the pre-M7 auto-flip).
//
// Idempotency contract:
//   - double continue (the level already advanced → nobody resolved the
//     NEW level) → 200 {"continued": false} no-op;
//   - continue while a question is still open (nothing resolved) → 200
//     no-op — indistinguishable by design, so repeated POSTs are safe;
//   - premature continue (beat not elapsed) → 409 {"error": "too soon"};
//   - continue after the run ended (game finished and deleted) → 200
//     no-op (the game_over path owns that state).
//
// Returns advanced=true only when a question_next was actually issued.
func (g *GameService) Continue(ctx context.Context, roomID string, player *models.Player) (bool, error) {
	sends, advanced, gerr := func() ([]outbound, bool, *GameError) {
		g.mu.Lock()
		defer g.mu.Unlock()

		game := g.games[roomID]
		if game == nil {
			return nil, false, nil // finished/never started → idempotent no-op
		}
		if game.mode != "solo" {
			// Dual enforcement with the handler's mode 409: the service
			// refuses too, so the endpoint's contract holds regardless of
			// who calls it (duels auto-advance on the beat instead).
			return nil, false, &GameError{Status: 409, Message: "continue is a solo-only endpoint (duels auto-advance on the reveal beat)"}
		}
		if player.Status != "active" {
			// Unreachable in practice (an ended run deletes the solo game
			// — the game_over path ran first); defensive mirror of the
			// Answer/Walk guards.
			return nil, false, &GameError{Status: 409, Message: "your run has already ended"}
		}
		players, err := g.players.GetPlayersByRoom(ctx, roomID)
		if err != nil {
			return nil, false, &GameError{Status: 500, Message: "continue could not read the room"}
		}
		if !allActiveResolvedLocked(game, players) {
			return nil, false, nil // a question is still open — nothing to continue
		}
		if time.Since(game.lastResolvedAt) < g.revealBeat {
			return nil, false, &GameError{Status: 409, Message: "too soon"}
		}
		next := game.level + 1
		if next > 15 {
			// A winning 15-correct resolution marks the player non-active,
			// so game_over already finished the room; the guard is paranoia.
			return nil, false, nil
		}
		question, endsAt := g.issueLevelLocked(roomID, game, next)
		return []outbound{{msg: map[string]interface{}{
			"event":    "question_next",
			"question": question,
			"ends_at":  endsAt,
		}}}, true, nil
	}()
	if gerr != nil {
		g.dispatch(roomID, sends) // so far always nil — the reject() error event
		return false, gerr
	}
	g.dispatch(roomID, sends)
	return advanced, nil
}

// onTimeout is the time.AfterFunc fallback armed by issueLevelLocked:
// every still-unresolved ACTIVE player falls exactly as with a wrong
// answer (SPEC §1 timeout = wrong answer; run_ended only — they never
// answered, so no answer_result). Both this path and a late POST resolve
// under g.mu, so the two can never double-score.
//
// M4 carry-over (M3 review finding 1): if the roster read fails here the
// nobody-can-resolve situation would strand the level — the timer already
// fired and nothing else would ever re-trigger it. The branch therefore
// re-arms itself via a short retry timer.
//
// Retry budget (M4 review finding 2): this path counts against its OWN
// per-level budget (game.timeoutRetries, capped by advanceRetryAttempts) —
// NOT the shared counter the advance path consumes, so the two paths can
// no longer eat each other's re-arms (the per-level cap is per path).
func (g *GameService) onTimeout(roomID string, level int, attempt int) {
	sends := func() []outbound {
		g.mu.Lock()
		defer g.mu.Unlock()

		game := g.games[roomID]
		if game == nil || game.level != level {
			return nil // game over / level already advanced — stale callback
		}

		ctx := context.Background()
		players, err := g.players.GetPlayersByRoom(ctx, roomID)
		if err != nil {
			log.Printf("onTimeout: room %s: loading players (attempt %d): %v", roomID, attempt, err)
			if game.timeoutRetries < advanceRetryAttempts {
				game.timeoutRetries++
				time.AfterFunc(advanceRetryBackoff, func() { g.onTimeout(roomID, level, attempt+1) })
			}
			return nil
		}

		// Wave 5: each seat is resolved against ITS OWN active question —
		// a swapped seat falls on the question it was actually looking at,
		// so its run_ended correct_index and the opponent_reveal question
		// describe that seat's question, not the shared primary.
		sends := []outbound{}
		fallen := []models.Player{}
		for _, p := range players {
			if p.Status != "active" || game.resolved[p.Seat] != nil {
				continue
			}
			q := activeQuestionLocked(game, p.Seat)
			amount := FallAmount(p.LevelReached)
			g.persistResolution(ctx, &p, "fallen", amount, p.LevelReached, false)
			game.resolved[p.Seat] = &seatResolution{choiceIndex: -1, questionID: q.questionID}
			game.lastResolvedAt = time.Now() // M7: resolution event for the beat floor
			fallen = append(fallen, p)
			// No answer_result — they did not answer. run_ended carries
			// correct_index (SPEC allowance, status=fallen).
			sends = append(sends, outbound{msg: runEndedPayload(p.Seat, "fallen", amount, &q.correctIndex)})
		}
		// Reveals for this pass's falls: still-active players see WHAT the
		// timed-out player would have faced with chosen_index = -1 (they
		// never answered; documented on the SPEC event row). The question
		// shown is the FALLEN seat's own (wave 5) question.
		for _, f := range fallen {
			sends = append(sends, g.revealsForFallenLocked(ctx, roomID, f.Seat, level, activeQuestionLocked(game, f.Seat), -1)...)
		}
		sends = append(sends, g.advanceIfNeededLocked(ctx, roomID, game)...)
		return sends
	}()
	g.dispatch(roomID, sends)
}

// revealsForFallenLocked builds the opponent_reveal events per SPEC §2:
// every player OTHER than the fallen one whose run is still `active`
// receives the question (uniform shape) + what the fallen player picked
// + the correct index — only because the question is now resolved for the
// answerer (SPEC §2 invariant 1 exception). Solo rooms have no other
// players, so the duel-only rule falls out naturally without a mode flag.
// A fallen/walked/won opponent gets nothing — the event must not reach
// the fallen player itself. Caller holds g.mu; failures are best-effort
// (the run_ended broadcast still reaches everyone).
func (g *GameService) revealsForFallenLocked(ctx context.Context, roomID string, fallenSeat int, level int, q levelQuestion, chosenIndex int) []outbound {
	players, err := g.players.GetPlayersByRoom(ctx, roomID)
	if err != nil {
		log.Printf("revealsForFallenLocked: room %s: loading players: %v", roomID, err)
		return nil
	}
	var sends []outbound
	for _, p := range players {
		if p.Seat == fallenSeat || p.Status != "active" {
			continue
		}
		sends = append(sends, outbound{seat: p.Seat, msg: map[string]interface{}{
			"event": "opponent_reveal",
			"question": QuestionView{
				Level:   level,
				Text:    q.text,
				Options: q.options,
			},
			"chosen_index":  chosenIndex,
			"correct_index": q.correctIndex,
		}})
	}
	return sends
}

// progressForLapsedLocked builds the wave-4 spectator progress events:
// whenever the SURVIVOR's run visibly moves (a correct answer, or any
// lifeline usage on the current question), every LAPSED seat —
// fallen/walked, now spectating — receives
//
//	opponent_progress {seat, level, amount, status: "active"}
//
// PRIVATE to that seat's socket. The payload is public-fact only: the
// survivor's seat, the level IN PLAY, their current amount (read back
// from the persisted roster — the same value persistResolution just
// wrote, authoritative for both the answer and the lifeline path) and
// their status — never correct_index, never anyone's private data (the
// survivor's own answer_result stays private; lifeline_used remains the
// room broadcast the FE already renders). Solo rooms have no lapsed
// seats → nothing. Caller holds g.mu; best-effort on a roster-read
// failure (logged; the primary events of the triggering action are
// unaffected).
func (g *GameService) progressForLapsedLocked(ctx context.Context, roomID string, survivorSeat, level int) []outbound {
	players, err := g.players.GetPlayersByRoom(ctx, roomID)
	if err != nil {
		log.Printf("progressForLapsedLocked: room %s: loading players: %v", roomID, err)
		return nil
	}
	// The SURVIVOR's current amount (persisted already on the answer
	// path; untouched on the lifeline path) is the event's amount.
	survivorAmount := 0
	found := false
	for _, p := range players {
		if p.Seat == survivorSeat {
			survivorAmount, found = p.Amount, true
			break
		}
	}
	if !found {
		return nil
	}
	var sends []outbound
	for _, p := range players {
		if p.Seat == survivorSeat || p.Status == "active" {
			continue // the survivor's own socket; and only lapses spectate
		}
		sends = append(sends, outbound{seat: p.Seat, msg: map[string]interface{}{
			"event":  "opponent_progress",
			"seat":   survivorSeat,
			"level":  level,
			"amount": survivorAmount,
			"status": "active",
		}})
	}
	return sends
}

// scheduleAdvanceRetryLocked re-arms the advance/timeout machinery ~1s
// after a transient roster-read failure (M3 review finding 1). The
// callback is a DIRECT, self-contained consequence of the failure branch:
// under the lock it re-checks the game/level guards (a stale retry after
// the level advanced or the game finished is a no-op) and simply retries
// advanceIfNeededLocked. Cap (M4 review finding 2): per-path per-level —
// the advance path has its own budget (game.retries), the timeout path
// its own (game.timeoutRetries), each capped by advanceRetryAttempts and
// reset by issueLevelLocked; neither consumes the other's. Caller holds
// g.mu.
func (g *GameService) scheduleAdvanceRetryLocked(roomID string, game *roomGame) {
	if game.retries >= advanceRetryAttempts {
		log.Printf("scheduleAdvanceRetryLocked: room %s: giving up after %d retries — level %d stays open until the next resolution", roomID, game.retries, game.level)
		return
	}
	game.retries++
	level := game.level
	time.AfterFunc(advanceRetryBackoff, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		cur := g.games[roomID]
		if cur == nil || cur != game || cur.level != level {
			return // finished, replaced, or already advanced — stale retry
		}
		g.advanceIfNeededLocked(context.Background(), roomID, cur)
	})
}

// advanceIfNeededLocked issues the next level when every ACTIVE player
// resolved the current one, or finishes the game when no active player
// remains. Caller holds g.mu; the returned events are sent by the caller
// after the lock is released.
func (g *GameService) advanceIfNeededLocked(ctx context.Context, roomID string, game *roomGame) []outbound {
	players, err := g.players.GetPlayersByRoom(ctx, roomID)
	if err != nil {
		// The room cannot advance safely without the roster. Every other
		// path that could unblock (further answers/walks) runs through
		// this same call — if all parties already resolved, nothing else
		// would ever re-trigger it, so the branch re-arms a short retry
		// timer instead of stranding the level (M3 review finding 1).
		log.Printf("advanceIfNeededLocked: room %s: loading players: %v", roomID, err)
		g.scheduleAdvanceRetryLocked(roomID, game)
		return nil
	}

	active := map[int]models.Player{}
	for _, p := range players {
		if p.Status == "active" {
			active[p.Seat] = p
		}
	}
	if len(active) == 0 {
		return g.finishLocked(ctx, roomID, game, players) // nobody left climbing
	}
	for seat := range active {
		if game.resolved[seat] == nil {
			return nil // still waiting on an active player
		}
	}

	next := game.level + 1
	if next > 15 {
		return g.finishLocked(ctx, roomID, game, players)
	}
	// M7 resolve pacing (SPEC v1.1 decision log): the next level is NOT
	// dealt the instant everyone resolved.
	//   - solo: HOLD — the level stays settled until the Continue endpoint
	//     fires question_next (the expired ends_at is safe: the timeout
	//     fallback falls only still-unresolved actives, and there are
	//     none).
	//   - duel: schedule exactly ONE advance timer for the reveal beat
	//     (server-scheduled so both players stay in sync); `game.beat`
	//     being non-nil guards re-entry (a timeout callback or a retry
	//     firing while the beat is pending is a no-op), and
	//     issueLevelLocked/finishLocked always clear it.
	//
	// revealBeat == 0 (tests, SetRevealBeat(0)) keeps the legacy
	// synchronous advance so table-driven service tests behave as before.
	if game.mode == "solo" {
		return nil
	}
	if g.revealBeat <= 0 {
		question, endsAt := g.issueLevelLocked(roomID, game, next)
		return []outbound{{msg: map[string]interface{}{
			"event":    "question_next",
			"question": question,
			"ends_at":  endsAt,
		}}}
	}
	if game.beat != nil {
		return nil // already scheduled this level's advance — exactly once
	}
	level, self := game.level, game
	game.beat = time.AfterFunc(g.revealBeat, func() { g.onRevealBeat(roomID, self, level) })
	return nil
}

// onRevealBeat is the duel 2.5s reveal-beat callback: under the game
// mutex it re-validates that this game is still live and still on the
// level whose advance was scheduled (a finished/replaced/advanced game
// was moved by an earlier path — stale callback no-op, the same guard
// pattern as onTimeout), then deals the next level and dispatches the
// question_next broadcast after releasing the lock.
func (g *GameService) onRevealBeat(roomID string, expected *roomGame, level int) {
	sends := func() []outbound {
		g.mu.Lock()
		defer g.mu.Unlock()

		cur := g.games[roomID]
		if cur == nil || cur != expected || cur.level != level {
			return nil // finished, or the level already moved — stale callback
		}
		if cur.beat != nil { // self: the level is advancing now
			cur.beat.Stop()
			cur.beat = nil
		}
		next := cur.level + 1
		if next > 15 {
			// An advance could only be BEAT-scheduled while an active
			// player still existed; reaching 15 with the beat pending
			// means the win resolution and the schedule raced — the win
			// path (status != "active") already finished the game, so
			// only unknown corruption can land here. Nothing to issue.
			return nil
		}
		question, endsAt := g.issueLevelLocked(roomID, cur, next)
		return []outbound{{msg: map[string]interface{}{
			"event":    "question_next",
			"question": question,
			"ends_at":  endsAt,
		}}}
	}()
	g.dispatch(roomID, sends)
}

// finishLocked ends the room: stops the timer, flips rooms.status to
// finished, broadcasts game_over {standings} (sorted by amount desc, ties
// broken by seat so the order is deterministic) and drops the in-memory
// game. Standings carry exactly {seat, name, status, amount} (SPEC §2);
// the run-ends reasons themselves are all broadcast as run_ended, so the
// amount suffices for the client — a DRAW (equal top amounts, e.g. both
// fallen at the same level) is decided client-side by comparing the top
// standings entries, not by a server flag.
// Exactly-once: the game is deleted from the map here and no path
// re-enters finishLocked afterwards (every later resolution 409s), so
// game_over is emitted exactly once per room.
func (g *GameService) finishLocked(ctx context.Context, roomID string, game *roomGame, players []models.Player) []outbound {
	if game.timer != nil {
		game.timer.Stop()
		game.timer = nil
	}
	if game.beat != nil {
		game.beat.Stop()
		game.beat = nil
	}
	sort.Slice(players, func(i, j int) bool {
		if players[i].Amount != players[j].Amount {
			return players[i].Amount > players[j].Amount
		}
		return players[i].Seat < players[j].Seat // stable tie order
	})
	standings := make([]map[string]interface{}, 0, len(players))
	for _, p := range players {
		standings = append(standings, map[string]interface{}{
			"seat":   p.Seat,
			"name":   p.Name,
			"status": p.Status,
			"amount": p.Amount,
		})
	}
	if err := g.rooms.SetRoomStatus(ctx, roomID, "finished"); err != nil {
		log.Printf("finishLocked: room %s: marking finished: %v", roomID, err)
	}
	// Wave-4B: the standings just finalized — the room's per-seat SESSION
	// totals take their census of this completed run NOW, before the
	// game_over dispatch. The single accumulation site in the pipeline
	// (see AccumulateRoomTotals: exactly-once per run, and necessarily
	// BEFORE any later Replay's ResetRoomPlayers — which touches the
	// players rows only; totals never reset on a rematch).
	g.AccumulateRoomTotals(ctx, roomID, players)
	delete(g.games, roomID)

	// Wave 6: carry the census we JUST took in the game_over payload itself.
	// Without it the session block only ever reached a client on a rejoin
	// (room_state is broadcast at join/replay), so a player who pressed PLAY
	// AGAIN saw their session lines only after a manual page reload — the
	// feature read as broken exactly where it matters (the rematch card).
	// Public facts only — the same six fields room_state already carries.
	msg := map[string]interface{}{"event": "game_over", "standings": standings}
	if rows, err := g.totals.GetRoomTotals(ctx, roomID); err != nil {
		log.Printf("finishLocked: room %s: reading session totals: %v", roomID, err)
	} else if len(rows) > 0 {
		totals := make([]map[string]interface{}, 0, len(rows))
		for _, r := range rows {
			totals = append(totals, map[string]interface{}{
				"seat":         r.Seat,
				"games_played": r.GamesPlayed,
				"games_won":    r.GamesWon,
				"total_money":  r.TotalMoney,
				"best_level":   r.BestLevel,
				"best_amount":  r.BestAmount,
			})
		}
		msg["totals"] = totals
	}
	return []outbound{{msg: msg}}
}

// AccumulateRoomTotals upserts ONE completed run into the room's per-seat
// SESSION totals (wave-4B, "in one room session we should persist player
// totals"), reading from the FINAL players-table rows the caller already
// resolved — persistResolution wrote every amount/status/level_reached
// before the advance decision that led to the finish, so the rows are the
// run's authoritative final census.
//
// EXACTLY-ONCE / DOUBLE-COUNT GUARD: this is invoked ONLY from
// finishLocked, which runs exactly once per completed run (the in-memory
// game is deleted from the map right here and every later resolution
// 409s — the finishLocked header note). The replay flow cannot
// double-count either: game_over accumulates FIRST (synchronously, still
// under g.mu, from the freshly-resolved roster), and a later Replay's
// ResetRoomPlayers resets the PLAYERS rows only — room_totals is
// deliberately untouched, so a REPLAYED room's game_over is a SECOND
// accumulation of the NEW run (games_played → 2, money sums both runs,
// bests are greater-of) computed from the post-reset rows' OWN final
// values. A stale pass-1 roster is never re-read here.
//
// Math per seat (the final players rows):
//   - games_played +1 when the seat's run is RESOLVED (status ≠ active —
//     defensive: game_over implies every seat lapsed; a hypothetically
//     still-active seat is not a completed run and is skipped);
//   - games_won +1 only when that seat's status = won (nobody won in a
//     run → the run adds 0 for every seat);
//   - total_money += the run's amount (walk/fall bank, or a win's final
//     amount);
//   - best_level / best_amount = greater-of (GREATEST inside the upsert).
//
// Solo rooms accumulate the same way (seat 1 only — the roster is the
// roster). Best-effort like the rest of the finish path (SetRoomStatus):
// failures are logged and never block the game_over broadcast.
func (g *GameService) AccumulateRoomTotals(ctx context.Context, roomID string, players []models.Player) {
	for _, p := range players {
		if p.Status == "active" {
			continue // not a completed run — no games_played, no money
		}
		won := 0
		if p.Status == "won" {
			won = 1
		}
		id, err := uuid.NewV7()
		if err != nil {
			log.Printf("AccumulateRoomTotals: room %s: seat %d: generating id: %v", roomID, p.Seat, err)
			continue
		}
		run := &models.RoomTotal{
			ID:          id.String(),
			RoomID:      roomID,
			Seat:        p.Seat,
			GamesPlayed: 1,
			GamesWon:    won,
			TotalMoney:  int64(p.Amount),
			BestLevel:   p.LevelReached,
			BestAmount:  p.Amount,
		}
		if err := g.totals.AccumulateSeat(ctx, run); err != nil {
			log.Printf("AccumulateRoomTotals: room %s: seat %d: %v", roomID, p.Seat, err)
		}
	}
}

// persistResolution writes the run-level outcome to the players row
// (amount / level_reached / status). ksql updates send only named columns
// via Exec here, so no ksql all-fields hazard applies. Caller holds g.mu.
func (g *GameService) persistResolution(ctx context.Context, player *models.Player, status string, amount, level int, correct bool) {
	p := player
	if err := g.players.UpdateStatus(ctx, p.ID, status); err != nil {
		log.Printf("persistResolution: player %s: status: %v", p.ID, err)
	}
	if err := g.players.UpdateAmount(ctx, p.ID, amount); err != nil {
		log.Printf("persistResolution: player %s: amount: %v", p.ID, err)
	}
	if correct {
		// Only a correct answer advances the ladder; on fall/walk the
		// highest reached stays as-is.
		if err := g.players.UpdateLevelReached(ctx, p.ID, level); err != nil {
			log.Printf("persistResolution: player %s: level_reached: %v", p.ID, err)
		}
	}
}

// outbound is one queued event: err != nil → error event to the
// offender's socket (and a GameError back to the REST caller); seat != 0
// → private to that seat; otherwise a room broadcast.
type outbound struct {
	seat int
	msg  map[string]interface{}
	err  *GameError
}

// reject queues an error event to the player's socket and carries the
// GameError back to the REST caller.
func reject(player *models.Player, status int, message string) ([]outbound, *GameError) {
	return []outbound{{
		seat: player.Seat,
		msg:  map[string]interface{}{"event": "error", "message": message},
		err:  &GameError{Status: status, Message: message},
	}}, &GameError{Status: status, Message: message}
}

// runEndedPayload builds the run_ended event. correct_index is included
// ONLY when status=fallen (SPEC-allowed exception: the question is fully
// resolved for that player once their run ends by fall; the FE uses it to
// show the missed question).
func runEndedPayload(seat int, status string, amount int, correctIndex *int) map[string]interface{} {
	payload := map[string]interface{}{
		"event":  "run_ended",
		"seat":   seat,
		"status": status,
		"amount": amount,
	}
	if correctIndex != nil && status == "fallen" {
		payload["correct_index"] = *correctIndex
	}
	return payload
}

// dispatch sends queued events after the game mutex is released (network
// writes never hold it; the wsManager has its own lock + 1s deadlines).
func (g *GameService) dispatch(roomID string, sends []outbound) {
	for _, s := range sends {
		if s.seat != 0 {
			g.notify.SendToSeat(roomID, s.seat, s.msg)
			continue
		}
		g.notify.Broadcast(roomID, s.msg)
	}
}

// ---------------------------------------------------------------------------
// Lifelines (M5 — SPEC §1: 50:50, Ask the Audience, Switch; one use EACH
// per run, server-applied; nothing works after a player's run has ended).
// ---------------------------------------------------------------------------

// validLifelines is the SPEC §1 lifeline set; anything else is a 400.
var validLifelines = map[string]bool{"fifty_fifty": true, "audience": true, "switch": true}

// Lifeline applies one lifeline for the caller's CURRENT question:
//   - fifty_fifty: two of the three WRONG options are removed (the correct
//     index is excluded); `removed_indices` broadcast to the ROOM;
//   - audience:     fake poll (see audiencePercentages); `percentages[4]`
//     in option order, broadcast to the ROOM;
//   - switch:       the ACTIVE question is replaced by the variant-2
//     alternate of the same level (the layout M3 already persisted);
//     ends_at and the in-flight timer are NOT touched; `data.question`
//     (uniform shape) is broadcast to the ROOM so both players see the
//     replacement.
//
// One use EACH per run: the caller's `lifelines_left` jsonb (persisted via
// UpdateLifelinesLeft, models.Player.LifelinesLeft JSON array) is the
// source of truth and the spent lifeline is removed from it on every use —
// both seats hold independent sets (enforcement keys on the seated player,
// not the game). A second use of any lifeline → 409 ("... already used for
// this run"; SPEC's one-per-run rule makes the per-level case unreachable).
// Call after the run ended (player status not active) → 409; during the
// resolved-but-not-yet-advanced window → 409 (M7 reveal-beat prep: when
// the hold-continue beat arrives the same guard already 409s lifeline
// calls). Unknown lifeline string → 400. Every rejection is both a REST
// GameError and an error event to that player's socket (relayed by the
// handler's gameReject helper).
//
// Lifeline applies one lifeline for the caller's CURRENT question:
//   - fifty_fifty: two of the three WRONG options are removed (the correct
//     index is excluded); `removed_indices` broadcast to the ROOM. The
//     computed pair is persisted on the game (roomGame.removals) and
//     becomes the single authoritative source — the room_state snapshot
//     echoes the applier's own pair (M5-review finding 1);
//   - audience:     fake poll (see audiencePercentages); `percentages[4]`
//     in option order, broadcast to the ROOM;
//   - switch:       the ACTIVE question is replaced by the variant-2
//     alternate of the same level (the layout M3 already persisted);
//     ends_at and the in-flight timer are NOT touched; `data.question`
//     (uniform shape) is broadcast to the ROOM so both players see the
//     replacement. All per-question fifty_fifty removals die with the
//     replaced question (documented M5 semantics).
//
// ONE-USE-ENFORCEMENT — ATOMIC (M5-review finding 2): the consumption is a
// single guarded SQL update through repository.SpendLifeline
//
//	UPDATE players SET lifelines_left = lifelines_left - '"x"'::jsonb
//	WHERE id = $1 AND lifelines_left @> '"x"'::jsonb
//
// whose rows-affected decide the outcome: 0 rows → 409 ("... already used
// for this run"). The DB row is the source of truth, so two parallel
// identical POSTs deterministically yield exactly ONE winner, ONE
// lifeline_used broadcast, and 409s for the losers (the previously
// measured 6×200 / 6-broadcast race is dead). A failed spend broadcasts
// nothing (the data is computed only after the spend is committed).
// Both seats hold independent sets (enforcement keys on the seated
// player). Call after the run ended (player status not active) → 409;
// during the resolved-but-not-yet-advanced window → 409 (M7 reveal-beat
// prep: when the hold-continue beat arrives the same guard already 409s
// lifeline calls). Unknown lifeline string → 400. Every rejection is both
// a REST GameError and an error event to that player's socket (relayed by
// the handler's gameReject helper).
//
// SWITCH-AFTER-50:50 semantics (documented): lifelines are one use each
// per run, so a fifty_fifty already applied to a question can never
// re-strike after a swap. If fifty_fifty was used on an EARLIER level
// and switch is used now, nothing re-applies — removals are per-question
// and the swapped question arrives with all four options. The replaced
// question is gone ONLY for the swapping seat: a swap clears THAT seat's
// own game.removals entry (its indices described the question it just
// left), while the peer's removals belong to the peer's UNTOUCHED
// question and survive. Covered by test flow ordering.
//
// WAVE-5 PER-SEAT SWAP ENGINE (locked product decision: "SWAP replaces ONLY
// THE SEAT'S OWN QUESTION"): the old room-wide `game.current = alt` +
// room broadcast is gone. A swap now
//   - writes ONLY game.seatQuestions[caller] (the peer's entry, and hence
//     the peer's whole question object, is untouched — byte-identical),
//   - emits a NEW per-seat event `question_swapped` to the CALLER'S SOCKET
//     ONLY (never a room broadcast, never the peer),
//   - still broadcasts the generic `lifeline_used` {seat, lifeline, data}
//     to the room as the "a lifeline was used" signal, but with an EMPTY
//     data for the swap — the replacement question never rides the room
//     broadcast, so no peer can learn it,
//   - preserves the LEVEL and ends_at (the in-flight timer is NOT reset)
//     and answers still validate `level` exactly as before.
//
// Guards (all evaluated BEFORE the atomic DB spend, so a refused swap does
// not burn the lifeline): one swap per seat per level (game.swapped), the
// question must not have expired (ends_at), the level must still have an
// unused variant, and the target variant must not be a question another
// seat has ALREADY ANSWERED at this level (duel fairness — see
// swapTargetLocked). Each refusal is a 409 GameError + an `error` event to
// the caller's socket, identical in shape to every other lifeline refusal.
func (g *GameService) Lifeline(ctx context.Context, roomID string, player *models.Player, name string) (map[string]interface{}, error) {
	data, sends, gerr := func() (map[string]interface{}, []outbound, *GameError) {
		g.mu.Lock()
		defer g.mu.Unlock()

		game := g.games[roomID]
		if game == nil {
			sends, gerr := reject(player, 409, "no game is running for this room")
			return nil, sends, gerr
		}
		if !validLifelines[name] {
			sends, gerr := reject(player, 400, "unknown lifeline (use fifty_fifty, audience or switch)")
			return nil, sends, gerr
		}
		if player.Status != "active" {
			sends, gerr := reject(player, 409, "your run has already ended")
			return nil, sends, gerr
		}
		if game.resolved[player.Seat] != nil {
			// Resolved-but-not-yet-advanced window: the level is settled
			// for this seat (answer or timeout) — no lifeline can affect
			// anything anymore (also the future M7 hold-continue beat).
			sends, gerr := reject(player, 409, "already resolved the current level")
			return nil, sends, gerr
		}

		// WAVE-5 SWAP PRE-CHECKS — deliberately BEFORE the atomic spend so a
		// refused swap leaves the lifeline unspent (and therefore retryable
		// in spirit; the one-use-per-run rule still caps it at one accept).
		// The swap target is resolved here so the accepted path below cannot
		// pick anything else.
		var swapTo *levelQuestion
		if name == "switch" {
			target, rejects, gerr := swapTargetLocked(game, player)
			if gerr != nil {
				return nil, rejects, gerr
			}
			swapTo = &target
		}

		// ATOMIC SPEND (M5-review finding 2): the DB guards the one-use
		// rule. 0 rows affected = the lifeline was already spent → 409.
		// (The freshly-fetched player row is only a courtesy fast path
		// for a friendlier already-common case; the DB decides.)
		rows, err := g.players.SpendLifeline(ctx, player.ID, name)
		if err != nil {
			log.Printf("Lifeline %s: player %s: atomic spend failed: %v", name, player.ID, err)
			sends, gerr := reject(player, 500, "lifelines could not be spent, try again")
			return nil, sends, gerr
		}
		if rows == 0 {
			sends, gerr := reject(player, 409, fmt.Sprintf("%s already used for this run", name))
			return nil, sends, gerr
		}

		var data map[string]interface{}
		// broadcastData is what the room-wide `lifeline_used` carries. It
		// equals `data` for fifty_fifty/audience and is deliberately EMPTY
		// for the per-seat swap (never leak a peer's replacement question).
		var broadcastData map[string]interface{}
		var swapSends []outbound
		switch name {
		case "fifty_fifty":
			removed, seen := game.removals[player.Seat]
			if !seen {
				// First application for this seat on THIS seat's question —
				// compute once and persist as the authoritative pair;
				// later re-entries (impossible with the one-use rule)
				// would re-serve the SAME pair. Wave 5: the wrong options
				// come from the CALLER's own active question, so a swapped
				// seat's 50:50 strikes ITS option order, never the shared
				// primary's.
				removed = removeTwoWrongLocked(activeQuestionLocked(game, player.Seat), g.rng)
				game.removals[player.Seat] = removed
			}
			data = map[string]interface{}{"removed_indices": removed}
			broadcastData = data
		case "audience":
			data = map[string]interface{}{"percentages": audiencePercentages(game.level, activeQuestionLocked(game, player.Seat).correctIndex, g.rng)}
			broadcastData = data
		case "switch":
			alt := *swapTo                        // pre-checked + validated by swapTargetLocked
			game.seatQuestions[player.Seat] = alt // THIS SEAT ONLY
			game.swapped[player.Seat] = true
			delete(game.removals, player.Seat) // its indices described the replaced question
			data = map[string]interface{}{
				"question": QuestionView{Level: game.level, Text: alt.text, Options: alt.options},
				"ends_at":  game.endsAt, // echoed UNCHANGED — the timer never resets
			}
			// The room broadcast carries an EMPTY data for a swap: the
			// replacement question is per-seat and must never reach the
			// peer. The caller learns it from the private event below and
			// from its own REST body.
			broadcastData = map[string]interface{}{}
			swapSends = append(swapSends, outbound{seat: player.Seat, msg: map[string]interface{}{
				"event":    "question_swapped",
				"seat":     player.Seat,
				"level":    game.level,
				"question": QuestionView{Level: game.level, Text: alt.text, Options: alt.options},
				"ends_at":  game.endsAt,
			}})
		}

		// Mirror the idempotent outcome into the local copy of the row =
		// the spent set. The DB row stays the source of truth; a parse
		// hiccup here is logged and harmless (fresh rows come per REST
		// call via GetPlayerByToken).
		left, lerr := lifelinesLeftOf(player)
		if lerr != nil {
			log.Printf("Lifeline %s: player %s: re-parsing lifelines_left %q: %v", name, player.ID, player.LifelinesLeft, lerr)
		}
		player.LifelinesLeft = lifelinesLeftJSON(removeString(left, name))

		// The per-seat `question_swapped` goes out FIRST (the caller's
		// question changes), then the generic room `lifeline_used`
		// signal — which for a swap deliberately carries no question.
		sends := append(swapSends, outbound{msg: map[string]interface{}{
			"event":    "lifeline_used",
			"seat":     player.Seat,
			"lifeline": name,
			"data":     broadcastData,
		}})
		// Wave-4 spectate: the survivor just used a lifeline on the level
		// in play — lapsed seats get the public-fact progress update too
		// (lifeline_used itself stays the room broadcast the FE renders).
		sends = append(sends, g.progressForLapsedLocked(ctx, roomID, player.Seat, game.level)...)
		return data, sends, nil
	}()
	if gerr != nil {
		g.dispatch(roomID, sends) // the reject() error event for the offender's socket
		return nil, gerr
	}
	g.dispatch(roomID, sends)
	return data, nil
}

// swapTargetLocked is the wave-5 SWAP rule engine: it picks the variant the
// calling seat would move to and refuses the swap (409 + an `error` event to
// the caller's socket, exactly like every other lifeline refusal) when any
// rule is violated. It runs BEFORE the atomic lifeline spend so a refusal
// costs the player nothing. Caller holds g.mu.
//
// RULES, in order:
//  1. ONE SWAP PER SEAT PER LEVEL — game.swapped[seat]. (The DB's
//     one-use-per-run spend is a second, independent gate.)
//  2. NOT EXPIRED — now >= ends_at. The timeout callback may not have run
//     yet, but the seat's clock is up: swapping after the buzzer would
//     hand a fresh question to a seat that is about to fall.
//  3. THE POOL STILL HAS AN UNUSED VARIANT — variants[level] must carry a
//     second (variant-2) realization. StartGame guarantees two; the guard
//     exists so a malformed layout degrades to a clean refusal instead of
//     corrupting the live question.
//  4. DUEL FAIRNESS — the target must not be a question another seat has
//     ALREADY ANSWERED at this level. The per-level batch holds exactly two
//     variants, so this is reachable: seat 1 swaps, answers the variant-2
//     question, and seat 2 then tries to swap onto the very same one. A
//     seat may only ever MOVE ONTO an unanswered question; it is never
//     moved onto one the opponent has already scored.
//
// Exhaustion fallback (documented decision): every refusal above is a 409
// GameError PLUS an `error` WS event to the offender's socket — the same
// contract as every other lifeline rejection, never a silent no-op, so a
// client can always tell "refused" from "applied".
func swapTargetLocked(game *roomGame, player *models.Player) (levelQuestion, []outbound, *GameError) {
	if game.swapped[player.Seat] {
		rejects, gerr := reject(player, 409, "you already switched the question at this level")
		return levelQuestion{}, rejects, gerr
	}
	if !time.Now().Before(game.endsAt) {
		rejects, gerr := reject(player, 409, "the question has already expired")
		return levelQuestion{}, rejects, gerr
	}
	pool := game.variants[game.level]
	if len(pool) < 2 || pool[1].questionID == "" {
		// Defensive: the selection always persists two variants per level.
		rejects, gerr := reject(player, 409, "no swap question available for this level")
		return levelQuestion{}, rejects, gerr
	}
	target := pool[1]
	if target.questionID == activeQuestionLocked(game, player.Seat).questionID {
		rejects, gerr := reject(player, 409, "no unused swap question available for this level")
		return levelQuestion{}, rejects, gerr
	}
	for seat, res := range game.resolved {
		if seat == player.Seat || res == nil {
			continue
		}
		if res.questionID == target.questionID {
			rejects, gerr := reject(player, 409, "your opponent already answered that question at this level")
			return levelQuestion{}, rejects, gerr
		}
	}
	return target, nil, nil
}

// removeString copies `left` without `name`, tolerating nil.
func removeString(left []string, name string) []string {
	out := make([]string, 0, len(left))
	for _, l := range left {
		if l != name {
			out = append(out, l)
		}
	}
	return out
}

// lifelinesLeftOf parses the player's jsonb lifelines_left (a JSON array
// of lifeline names, models.DefaultLifelinesLeft at insert).
func lifelinesLeftOf(p *models.Player) ([]string, error) {
	var left []string
	if err := json.Unmarshal([]byte(p.LifelinesLeft), &left); err != nil {
		return nil, err
	}
	return left, nil
}

// lifelinesLeftJSON marshals the remaining set back into the jsonb string
// column (an empty slice marshals to "[]" — valid jsonb).
func lifelinesLeftJSON(left []string) string {
	b, err := json.Marshal(left)
	if err != nil { // []string always marshals; belt and suspenders
		return "[]"
	}
	return string(b)
}

// removeTwoWrongLocked picks the two indices fifty_fifty removes: both are
// WRONG options of the question the seat is being shown (`q` — wave 5: the
// seat's OWN active question, so a swapped seat's 50:50 strikes its option
// order). The correct index is excluded.
// Deterministic under an injected RNG. The result is sorted ascending so
// the presentation is stable regardless of pick order.
func removeTwoWrongLocked(q levelQuestion, r RNG) []int {
	wrong := make([]int, 0, 3)
	for i := 0; i < 4; i++ {
		if i != q.correctIndex {
			wrong = append(wrong, i)
		}
	}
	a := r.IntN(3)
	swap := r.IntN(2) // 0: keep adjacent pick, 1: take the next one
	b := (a + 1 + swap) % 3
	removed := []int{wrong[a], wrong[b]}
	sort.Ints(removed)
	return removed
}

// audiencePercentages builds the SPEC §1 fake poll: the CORRECT option's
// weight degrades by tier band (~70% for levels 1–5, ~55% for 6–10, ~40%
// for 11–15 — these are FLOOR-weight reads, always the highest share on
// easy because the remainder can never exceed 30). The remainder is split
// RANDOMLY (integers ≥ 0) among the three wrong options so the four
// integers sum to exactly 100. Result is in option order; the shape never
// says WHICH entry is the correct one — finding the heavy entry is the
// intended playable signal.
//
// Determinism seam: the remainder split uses the injected RNG; the last
// (fourth) wrong option receives whatever the earlier shares left — a
// documented, structurally-caused skew that tests keep deterministic.
func audiencePercentages(level, correctIndex int, r RNG) []int {
	weight := 70
	switch {
	case level >= 11:
		weight = 40
	case level >= 6:
		weight = 55
	}
	out := []int{0, 0, 0, 0}
	out[correctIndex] = weight
	remaining := 100 - weight

	wrong := make([]int, 0, 3)
	for i := 0; i < 4; i++ {
		if i != correctIndex {
			wrong = append(wrong, i)
		}
	}
	for i, idx := range wrong {
		if i == len(wrong)-1 {
			out[idx] = remaining // the split remainder; whatever is left
			break
		}
		share := r.IntN(remaining + 1)
		out[idx] = share
		remaining -= share
	}
	return out
}
