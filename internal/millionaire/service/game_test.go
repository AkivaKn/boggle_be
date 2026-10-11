package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"boggle-api/internal/millionaire/models"
	"boggle-api/internal/millionaire/repository"

	"github.com/google/uuid"
	"github.com/vingarcia/ksql"
	"github.com/vingarcia/ksql/adapters/kpgx"
)

// ---------------------------------------------------------------------------
// Pure helpers (no DB needed).
// ---------------------------------------------------------------------------

// TestAmountAtLadderAndWalks pins the ladder and the walk rule: a walk
// locks in the amount of the last ANSWERED level (SPEC §1).
func TestAmountAtLadderAndWalks(t *testing.T) {
	cases := []struct {
		level, amount int
	}{
		{0, 0}, {1, 100}, {2, 200}, {3, 300}, {4, 500}, {5, 1000},
		{6, 2000}, {10, 32000}, {11, 64000}, {14, 500000}, {15, 1000000},
		{16, 0}, {-1, 0},
	}
	for _, c := range cases {
		if got := AmountAt(c.level); got != c.amount {
			t.Errorf("AmountAt(%d) = %d, want %d", c.level, got, c.amount)
		}
		if got := WalkAmount(c.level); got != c.amount {
			t.Errorf("WalkAmount(%d) = %d, want %d", c.level, got, c.amount)
		}
	}
}

// TestFallAmountSafeHavenDrop pins the fall rule (SPEC §1): below the first
// safe haven $0, otherwise the last BANKED haven. A haven is banked only by
// ANSWERING the haven level correctly, so level_reached is the last
// correctly-answered level: falling AT level 5 leaves $0, falling at
// 6..10 leaves $1,000, falling at 11..15 leaves $32,000.
func TestFallAmountSafeHavenDrop(t *testing.T) {
	// fallAt N means the player was ANSWERING level N and got it wrong, so
	// level_reached = N-1.
	cases := []struct {
		fallAt, reached, amount int
	}{
		{1, 0, 0}, {3, 2, 0}, {4, 3, 0}, {5, 4, 0}, // still below haven 5
		{6, 5, 1000}, {10, 9, 1000}, // haven 5 banked
		{11, 10, 32000}, {12, 11, 32000}, {15, 14, 32000}, // haven 10 banked
	}
	for _, c := range cases {
		if got := FallAmount(c.reached); got != c.amount {
			t.Errorf("FallAmount(reached %d after falling at level %d) = %d, want %d",
				c.reached, c.fallAt, got, c.amount)
		}
	}
}

func TestTierForAndBudgets(t *testing.T) {
	for _, c := range []struct {
		level int
		tier  string
		bud   time.Duration
	}{
		{1, "easy", 30 * time.Second}, {5, "easy", 30 * time.Second},
		{6, "medium", 20 * time.Second}, {10, "medium", 20 * time.Second},
		{11, "hard", 15 * time.Second}, {15, "hard", 15 * time.Second},
	} {
		if got := TierFor(c.level); got != c.tier {
			t.Errorf("TierFor(%d) = %q, want %q", c.level, got, c.tier)
		}
		if got := BudgetFor(DefaultBudgets, c.level); got != c.bud {
			t.Errorf("BudgetFor(level %d) = %v, want %v", c.level, got, c.bud)
		}
	}
}

// TestShuffleQuestionRemaps pins the room-side shuffle: a permutation of
// the original options whose remapped index still points at the correct
// option (SPEC §2 invariant 4). Fifty runs must move the index at least
// once (a deterministic no-op shuffle would be caught here).
func TestShuffleQuestionRemaps(t *testing.T) {
	q := models.Question{
		ID:           "q-test",
		Question:     "which one is it?",
		Options:      `["alpha","beta","gamma","delta"]`,
		CorrectIndex: 2, // "gamma"
	}
	changed := 0
	for i := 0; i < 50; i++ {
		lq, err := shuffleQuestion(q)
		if err != nil {
			t.Fatal(err)
		}
		if len(lq.options) != 4 {
			t.Fatalf("shuffle kept %d options", len(lq.options))
		}
		seen := map[string]bool{}
		for _, o := range lq.options {
			seen[o] = true
		}
		if len(seen) != 4 {
			t.Fatalf("shuffled options are not a permutation: %v", lq.options)
		}
		if lq.correctIndex < 0 || lq.correctIndex > 3 || lq.options[lq.correctIndex] != "gamma" {
			t.Fatalf("remapped index %d does not point at the correct option %v",
				lq.correctIndex, lq.options)
		}
		if lq.text != q.Question || lq.questionID != q.ID {
			t.Fatalf("shuffle mutated %q / %q", lq.text, lq.questionID)
		}
		if lq.correctIndex != q.CorrectIndex {
			changed++
		}
	}
	if changed == 0 {
		t.Fatal("shuffled index never differed in 50 runs — shuffle looks inert")
	}
}

// ---------------------------------------------------------------------------
// Service-level integration against the dev DB (skip-if-unreachable).
// ---------------------------------------------------------------------------

// fakeNotifier records the events a test game emitted, ordered by send.
type fakeNotifier struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	roomID string
	seat   int // 0 = broadcast
	msg    map[string]interface{}
}

func (f *fakeNotifier) Broadcast(roomID string, msg interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recordedEvent{roomID: roomID, msg: msg.(map[string]interface{})})
}

func (f *fakeNotifier) SendToSeat(roomID string, seat int, msg interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recordedEvent{roomID: roomID, seat: seat, msg: msg.(map[string]interface{})})
}

// eventsOf returns the events matching a name, optionally private to one
// seat (seat -1 = any).
func (f *fakeNotifier) eventsOf(event string, seat int) []recordedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []recordedEvent{}
	for _, e := range f.events {
		if e.msg["event"] != event || (seat != -1 && e.seat != seat) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// eventData is the payload of the last matching event.
func (f *fakeNotifier) eventData(event string, seat int) map[string]interface{} {
	found := f.eventsOf(event, seat)
	if len(found) == 0 {
		return nil
	}
	return found[len(found)-1].msg
}

// waitEvent polls until the given event is recorded or the deadline
// passes (only the real-timer fallback path dispatches asynchronously).
func (f *fakeNotifier) waitEvent(event string, seat int, timeout time.Duration) map[string]interface{} {
	deadline := time.Now().Add(timeout)
	for {
		if data := f.eventData(event, seat); data != nil {
			return data
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func newServiceTestDB(t *testing.T) ksql.DB {
	t.Helper()
	dsn := "postgres://devuser:devpassword@localhost:5436/millionaire?sslmode=disable"
	if os.Getenv("DATABASE_URL") != "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	db, err := kpgx.New(context.Background(), dsn, ksql.Config{})
	if err != nil {
		t.Skipf("dev DB unreachable (%v) — skipping integration test", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestGame(t *testing.T, db ksql.DB) (*GameService, *fakeNotifier, RoomService, repository.PlayerRepository) {
	t.Helper()
	n := &fakeNotifier{}
	rooms := NewRoomService(repository.NewRoomRepository(db))
	g := NewGameService(
		db,
		rooms,
		repository.NewPlayerRepository(db),
		repository.NewQuestionRepository(db),
		repository.NewRoomQuestionRepository(db),
		n,
	)
	// M7 harness default: legacy SYNCHRONOUS advance (reveal beat 0) so
	// the table-driven tests keep their pre-M7 timing; the beat and the
	// solo Continue floor get their own dedicated tests (which inject a
	// small beat via SetRevealBeat).
	g.SetRevealBeat(0)
	t.Cleanup(func() {
		g.mu.Lock()
		for id, game := range g.games {
			if game.timer != nil {
				game.timer.Stop()
			}
			if game.beat != nil {
				game.beat.Stop()
			}
			delete(g.games, id)
		}
		g.mu.Unlock()
	})
	return g, n, rooms, repository.NewPlayerRepository(db)
}

// createTestRoom inserts a room + active players and registers cleanup.
func createTestRoom(t *testing.T, ctx context.Context, db ksql.DB, rooms RoomService, players repository.PlayerRepository, mode string, nSeats int) (*models.Room, []models.Player) {
	t.Helper()
	room, err := rooms.CreateRoom(ctx, mode)
	if err != nil {
		t.Fatalf("creating room: %v", err)
	}
	t.Cleanup(func() {
		dctx := context.Background()
		_, _ = db.Exec(dctx, "DELETE FROM millionaire_room_questions WHERE room_id = $1", room.ID)
		_, _ = db.Exec(dctx, "DELETE FROM millionaire_players WHERE room_id = $1", room.ID)
		_, _ = db.Exec(dctx, "DELETE FROM millionaire_rooms WHERE id = $1", room.ID)
	})

	ps := []models.Player{}
	for seat := 1; seat <= nSeats; seat++ {
		p := &models.Player{
			ID:            uuidString(t),
			RoomID:        room.ID,
			Seat:          seat,
			Token:         uuidString(t),
			Name:          fmt.Sprintf("Seat%d", seat),
			Status:        "active",
			LifelinesLeft: models.DefaultLifelinesLeft,
			Amount:        0,
			LevelReached:  0,
			CreatedAt:     time.Now().UTC(),
		}
		if err := players.InsertPlayer(ctx, p); err != nil {
			t.Fatalf("inserting player: %v", err)
		}
		ps = append(ps, *p)
	}
	gotRoom, err := rooms.GetRoom(ctx, room.ID)
	if err != nil {
		t.Fatalf("reading room back: %v", err)
	}
	return gotRoom, ps
}

func uuidString(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return id.String()
}

// reloadPlayer re-reads the freshest row for a player (rows change on
// every resolution).
func reloadPlayer(t *testing.T, ctx context.Context, players repository.PlayerRepository, p models.Player) models.Player {
	t.Helper()
	fresh, err := players.GetPlayerByToken(ctx, p.RoomID, p.Token)
	if err != nil {
		t.Fatalf("reloading player: %v", err)
	}
	return *fresh
}

// currentLevel returns the game's current level from the (same-package)
// in-memory state.
func currentLevel(t *testing.T, g *GameService, roomID string) int {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	game := g.games[roomID]
	if game == nil {
		t.Fatal("no game running for the room")
	}
	return game.level
}

// shuffledCorrectIndex reads the correct answer of the ACTIVE level's
// SHARED PRIMARY question from the same-package in-memory game state
// (variant 1 at issue time and forever after — wave 5 removed the
// room-wide swap that used to move it to variant 2). Tests that need the
// SWAPPED question's index read seatCorrectIndex instead.
func shuffledCorrectIndex(t *testing.T, g *GameService, roomID string, level int) int {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	game := g.games[roomID]
	if game == nil || game.level != level {
		t.Fatalf("level %d is not the active question (current %v)", level, game == nil)
	}
	return game.current.correctIndex
}

// seatQuestion reads a seat's OWN active question at the active level
// (wave 5 per-seat swap state; the shared primary when it never swapped).
func seatQuestion(t *testing.T, g *GameService, roomID string, level, seat int) levelQuestion {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	game := g.games[roomID]
	if game == nil || game.level != level {
		t.Fatalf("level %d is not the active question (current %v)", level, game == nil)
	}
	return activeQuestionLocked(game, seat)
}

// seatCorrectIndex is seatQuestion's correct index — the score oracle for a
// SEAT (a swapped seat must be answered against ITS question, not the
// shared primary's).
func seatCorrectIndex(t *testing.T, g *GameService, roomID string, level, seat int) int {
	t.Helper()
	return seatQuestion(t, g, roomID, level, seat).correctIndex
}

// answerCorrectly answers the given players' CURRENT level correctly,
// re-reading levels between answers (they advance on resolution). M7
// pacing awareness: in SOLO rooms a correct answer only settles the level
// (the room holds) — the helper then deals the next level through the
// real Continue path (reveal-beat 0 in the harness). Mid-level duel
// rounds leave the level gated; the synchronous advance at beat 0 fires
// when the LAST player's answer resolves the level.
func answerCorrectly(t *testing.T, ctx context.Context, g *GameService, players repository.PlayerRepository, roomID string, ps ...models.Player) {
	t.Helper()
	for _, p := range ps {
		fresh := reloadPlayer(t, ctx, players, p)
		if fresh.Status != "active" {
			t.Fatalf("player %s is not active, cannot answer", fresh.Token)
		}
		level := currentLevel(t, g, roomID)
		if _, err := g.Answer(ctx, roomID, &fresh, level, shuffledCorrectIndex(t, g, roomID, level)); err != nil {
			t.Fatalf("answering level %d correctly: %v", level, err)
		}
		fresh = reloadPlayer(t, ctx, players, p)
		if fresh.Status != "active" {
			continue // run ended (fall can't happen on a correct answer; won at 15)
		}
		g.mu.Lock()
		mode := ""
		if game := g.games[roomID]; game != nil {
			mode = game.mode
		}
		g.mu.Unlock()
		if mode != "solo" {
			continue // duels advance through the beat, not the continue endpoint
		}
		if _, err := g.Continue(ctx, roomID, &fresh); err != nil {
			t.Fatalf("continue after level %d (solo hold): %v", level, err)
		}
	}
}

// answerWrong answers the current level deliberately wrongly and returns
// the level that was failed.
func answerWrong(t *testing.T, ctx context.Context, g *GameService, players repository.PlayerRepository, roomID string, p models.Player) int {
	t.Helper()
	fresh := reloadPlayer(t, ctx, players, p)
	level := currentLevel(t, g, roomID)
	correct := shuffledCorrectIndex(t, g, roomID, level)
	if _, err := g.Answer(ctx, roomID, &fresh, level, (correct+1)%4); err != nil {
		t.Fatalf("answering level %d wrongly: %v", level, err)
	}
	return level
}

// walkNow walks the given player at their current level.
func walkNow(t *testing.T, ctx context.Context, g *GameService, players repository.PlayerRepository, roomID string, p models.Player) int {
	t.Helper()
	fresh := reloadPlayer(t, ctx, players, p)
	amount, err := g.Walk(ctx, roomID, &fresh)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return amount
}

// TestStartGamePicksAndPersists verifies selection + persistence + the
// level-1 issue: 30 room_questions rows (2 distict per level, tier per
// SPEC §1), layout persisted, room active, game_started with uniform
// payload (never a bare event, never a correct_index).
func TestStartGamePicksAndPersists(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "solo", 1)

	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatalf("StartGame: %v", err)
	}

	gs := n.eventData("game_started", 0)
	if gs == nil {
		t.Fatal("no game_started event")
	}
	q, ok := gs["question"].(QuestionView)
	if !ok {
		t.Fatalf("game_started question = %T, want QuestionView", gs["question"])
	}
	if q.Level != 1 || q.Text == "" || len(q.Options) != 4 {
		t.Fatalf("level-1 payload = %+v", q)
	}
	if _, has := gs["ends_at"]; !has {
		t.Fatal("game_started without ends_at")
	}
	for _, c := range leakScanAll(n.eventsOf("game_started", -1)) {
		if c {
			t.Fatalf("game_started leaks a forbidden field: %v", gs)
		}
	}

	var rqs []models.RoomQuestion
	if err := db.Query(ctx, &rqs, "SELECT * FROM millionaire_room_questions WHERE room_id = $1 ORDER BY level, variant", room.ID); err != nil {
		t.Fatalf("reading room_questions: %v", err)
	}
	if len(rqs) != 30 {
		t.Fatalf("room_questions = %d rows, want 30", len(rqs))
	}
	perLevel := map[int]int{}
	for _, rq := range rqs {
		perLevel[rq.Level]++
		if rq.ShuffledCorrectIndex < 0 || rq.ShuffledCorrectIndex > 3 {
			t.Errorf("level %d variant %d: bad shuffled_correct_index %d", rq.Level, rq.Variant, rq.ShuffledCorrectIndex)
		}
		var shuffled []string
		if err := json.Unmarshal([]byte(rq.ShuffledOptions), &shuffled); err != nil || len(shuffled) != 4 {
			t.Errorf("level %d variant %d: bad shuffled_options %q", rq.Level, rq.Variant, rq.ShuffledOptions)
		}
		// Tier check via the questions table. WWTBAM rows are exempt from
		// the LEGACY tier contract (M10): their difficulty is a
		// display-only synthetic (wwtbamDifficultyTier — 1–2 easy,
		// 3–6 medium, 7–15 hard, ≠ TierFor); the governing axis is
		// strength == the level (pinned here per pick).
		var row struct {
			Difficulty string `ksql:"difficulty"`
			Source     string `ksql:"source"`
			Strength   *int   `ksql:"strength"`
		}
		if err := db.QueryOne(ctx, &row, "SELECT difficulty, source, strength FROM millionaire_questions WHERE id = $1", rq.QuestionID); err != nil {
			t.Fatalf("reading question %s: %v", rq.QuestionID, err)
		}
		if row.Source == WWTBAMSource {
			if row.Strength == nil || *row.Strength != rq.Level {
				t.Errorf("level %d served a wwtbam row with strength %v — calibration broken", rq.Level, row.Strength)
			}
		} else if row.Difficulty != TierFor(rq.Level) {
			t.Errorf("level %d uses tier %q, want %q", rq.Level, row.Difficulty, TierFor(rq.Level))
		}
	}
	for lvl, count := range perLevel {
		if count != 2 {
			t.Errorf("level %d got %d variants, want 2", lvl, count)
		}
	}

	var dupes []struct {
		QID string `ksql:"qid"`
		N   int    `ksql:"n"`
	}
	if err := db.Query(ctx, &dupes, "SELECT question_id AS qid, count(*) AS n FROM millionaire_room_questions WHERE room_id = $1 GROUP BY question_id HAVING count(*) > 1", room.ID); err != nil {
		t.Fatalf("dup query: %v", err)
	}
	if len(dupes) != 0 {
		t.Fatalf("repeated questions in one room: %v", dupes)
	}

	fresh, err := rooms.GetRoom(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != "active" {
		t.Fatalf("room status = %q, want active", fresh.Status)
	}
}

// leakScanAll walks every recorded event and reports forbidden fields:
// correct_index is only allowed in answer_result, in run_ended with
// status=fallen, and in opponent_reveal (M4: the question is settled for
// the answerer by then — SPEC §2 invariant 1 exception).
func leakScanAll(events []recordedEvent) []bool {
	out := make([]bool, len(events))
	for i, e := range events {
		out[i] = scanLeak(e.msg, "", false)
	}
	return out
}

// scanLeak recursively looks for correct_index keys, returning true when
// found outside the allowed event types.
func scanLeak(v interface{}, parentEvent string, inSettled bool) bool {
	switch val := v.(type) {
	case map[string]interface{}:
		event, _ := val["event"].(string)
		if event == "" {
			event = parentEvent
		}
		allowed := event == "answer_result" ||
			(event == "run_ended" && val["status"] == "fallen") ||
			event == "opponent_reveal"
		if _, has := val["correct_index"]; has && !allowed {
			return true
		}
		for k, sub := range val {
			if k == "correct_index" {
				continue
			}
			if scanLeak(sub, event, allowed || inSettled) {
				return true
			}
		}
	case []interface{}:
		for _, sub := range val {
			if scanLeak(sub, parentEvent, inSettled) {
				return true
			}
		}
	}
	return false
}

// TestAnswerScoringAdvances verifies a correct answer: level_reached and
// amount persist, answer_result is PRIVATE to the answerer with the
// SPEC-allowed correct_index, and the next level issues only after ALL
// active players resolved (invariant 5).
func TestAnswerScoringAdvances(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "duel", 2)

	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	// Seat 1 answers correctly: seat 2 is still pending, so the level must
	// NOT advance yet.
	if _, err := g.Answer(ctx, room.ID, &ps[0], 1, shuffledCorrectIndex(t, g, room.ID, 1)); err != nil {
		t.Fatal(err)
	}
	if n.eventData("question_next", 0) != nil {
		t.Fatal("question_next issued before all active players resolved")
	}
	if lvl := currentLevel(t, g, room.ID); lvl != 1 {
		t.Fatalf("level advanced to %d while seat 2 was pending", lvl)
	}

	// Seat 2 resolves → level 2 for the room.
	if _, err := g.Answer(ctx, room.ID, &ps[1], 1, shuffledCorrectIndex(t, g, room.ID, 1)); err != nil {
		t.Fatal(err)
	}
	qn := n.eventData("question_next", 0)
	if qn == nil {
		t.Fatal("no question_next after both players resolved")
	}
	q, ok := qn["question"].(QuestionView)
	if !ok || q.Level != 2 {
		t.Fatalf("question_next = %+v, want level 2", qn)
	}

	// Persistence of seat 1: level_reached 1, amount 100, still active.
	p1 := reloadPlayer(t, ctx, players, ps[0])
	if p1.LevelReached != 1 || p1.Amount != 100 || p1.Status != "active" {
		t.Fatalf("seat 1 row = %d/%d/%s, want 1/100/active",
			p1.LevelReached, p1.Amount, p1.Status)
	}

	// answer_result is private: only the answerer's seat got it.
	ar := n.eventsOf("answer_result", -1)
	if len(ar) != 2 {
		t.Fatalf("got %d answer_result events, want exactly one per answer", len(ar))
	}
	if ar[0].seat != 1 || ar[1].seat != 2 {
		t.Fatalf("answer_result seats = [%d %d], want each private to the answerer",
			ar[0].seat, ar[1].seat)
	}
	first := ar[0].msg
	if first["correct"] != true || fmt.Sprint(first["amount"]) != "100" ||
		first["correct_index"] == nil || first["status"] != "active" {
		t.Fatalf("answer_result = %v, want correct/100/correct_index set/active", first)
	}
}

// TestSafeHavenDropOnFall is the table-driven safe-haven/scoring gate:
// fall at level 4 → $0; at 5 → $0 (attempting the haven banks nothing);
// at 6 → $1,000; at 10 → $1,000; at 11 → $32,000; at 12 → $32,000;
// at 15 → $32,000. Every solo case reaches game_over.
func TestSafeHavenDropOnFall(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)

	cases := []struct {
		name         string
		rightsBefore int // correct answers before the fatal miss
		fallAt       int // the level whose question is missed
		amount       int
	}{
		{"fall-at-4", 3, 4, 0},
		{"fall-at-5", 4, 5, 0},
		{"fall-at-6", 5, 6, 1000},
		{"fall-at-10", 9, 10, 1000},
		{"fall-at-11", 10, 11, 32000},
		{"fall-at-12", 11, 12, 32000},
		{"fall-at-15", 14, 15, 32000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, n, rooms, players := newTestGame(t, db)
			room, ps := createTestRoom(t, ctx, db, rooms, players, "solo", 1)
			if err := g.StartGame(ctx, room, ps); err != nil {
				t.Fatal(err)
			}

			fallen := answerWrongLevel(t, ctx, g, players, room.ID, ps[0], c.rightsBefore)
			if fallen != c.fallAt {
				t.Fatalf("fell at level %d, want %d", fallen, c.fallAt)
			}

			// Solo room: the run ends immediately.
			re := n.eventData("run_ended", -1)
			if re == nil {
				t.Fatal("no run_ended after the fall")
			}
			if fmt.Sprint(re["status"]) != "fallen" || fmt.Sprint(re["amount"]) != strconv.Itoa(c.amount) {
				t.Fatalf("run_ended = %v, want fallen/%d", re, c.amount)
			}
			// The fallen run_ended MAY carry correct_index (SPEC allowance).
			if _, has := re["correct_index"]; !has {
				t.Errorf("run_ended(fallen) should carry correct_index: %v", re)
			}
			over := n.eventData("game_over", -1)
			if over == nil {
				t.Fatal("no game_over after a solo fall")
			}
			gotRoom, err := rooms.GetRoom(ctx, room.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotRoom.Status != "finished" {
				t.Fatalf("room status = %q, want finished", gotRoom.Status)
			}
			fresh := reloadPlayer(t, ctx, players, ps[0])
			if fresh.Amount != c.amount {
				t.Fatalf("player amount = %d, want %d", fresh.Amount, c.amount)
			}
			if fresh.LevelReached != c.rightsBefore || fresh.Status != "fallen" {
				t.Fatalf("player level_reached/status = %d/%q, want %d/fallen (highest kept)",
					fresh.LevelReached, fresh.Status, c.rightsBefore)
			}
		})
	}
}

// answerWrongLevel answers `rightsBefore` levels correctly, then misses
// the next one, returning that level. Used by the fall table so each case
// states its intent explicitly.
func answerWrongLevel(t *testing.T, ctx context.Context, g *GameService, players repository.PlayerRepository, roomID string, p models.Player, rightsBefore int) int {
	t.Helper()
	for i := 0; i < rightsBefore; i++ {
		answerCorrectly(t, ctx, g, players, roomID, p)
	}
	return answerWrong(t, ctx, g, players, roomID, p)
}

// TestWalkLocksLastAnswered covers the walk table: walk with nothing
// answered → $0, at 4 answered → $300, at 5 answered → $500, after 6
// answered → $1,000, at 15 (after answering 14) → $500,000. Per SPEC §1 a
// walk locks the last ANSWERED level — $1,000,000 comes only from
// answering 15 (win path, covered in TestWinAt15).
func TestWalkLocksLastAnswered(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)

	cases := []struct {
		name         string
		rightsBefore int // correctly answered levels, then walk
		walkAt       int
		amount       int
	}{
		{"walk-at-0", 0, 1, 0},
		{"walk-at-4-answered-3", 3, 4, 300},
		{"walk-at-5-answered-4", 4, 5, 500},
		{"walk-at-6-answered-5", 5, 6, 1000},
		{"walk-at-7-answered-6", 6, 7, 2000},
		{"walk-at-15-answered-14", 14, 15, 500000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, n, rooms, players := newTestGame(t, db)
			room, ps := createTestRoom(t, ctx, db, rooms, players, "solo", 1)
			if err := g.StartGame(ctx, room, ps); err != nil {
				t.Fatal(err)
			}

			for i := 0; i < c.rightsBefore; i++ {
				answerCorrectly(t, ctx, g, players, room.ID, ps[0])
			}
			if got := currentLevel(t, g, room.ID); got != c.walkAt {
				t.Fatalf("current level = %d, want %d", got, c.walkAt)
			}

			amount := walkNow(t, ctx, g, players, room.ID, ps[0])
			if amount != c.amount {
				t.Fatalf("walk amount = %d, want %d", amount, c.amount)
			}

			re := n.eventData("run_ended", -1)
			if re == nil {
				t.Fatal("no run_ended after the walk")
			}
			if fmt.Sprint(re["status"]) != "walked" || fmt.Sprint(re["amount"]) != strconv.Itoa(c.amount) {
				t.Fatalf("run_ended = %v, want walked/%d", re, c.amount)
			}
			if _, has := re["correct_index"]; has {
				t.Fatalf("walk run_ended must NOT carry correct_index: %v", re)
			}

			fresh := reloadPlayer(t, ctx, players, ps[0])
			if fresh.Status != "walked" || fresh.Amount != c.amount || fresh.LevelReached != c.rightsBefore {
				t.Fatalf("player row = %d/%d/%s, want level %d amount %d walked",
					fresh.LevelReached, fresh.Amount, fresh.Status, c.rightsBefore, c.amount)
			}

			// Solo walk is the only player → immediate game_over.
			over := n.waitEvent("game_over", -1, time.Second)
			if over == nil {
				t.Fatal("no game_over after the solo walk")
			}
		})
	}
}

// TestDoubleAnswerRejected pins invariant 3: the second answer for the
// same (player, level) is rejected with 409 — the "already answered"
// window exists in a duel while the other player is still pending — and
// a stale level number is rejected as a level mismatch.
func TestDoubleAnswerRejected(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, _, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "duel", 2)

	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	if _, err := g.Answer(ctx, room.ID, &ps[0], 1, shuffledCorrectIndex(t, g, room.ID, 1)); err != nil {
		t.Fatalf("first answer: %v", err)
	}

	// Same seat, same level, again → 409 already answered.
	again := ps[0]
	if _, err := g.Answer(ctx, room.ID, &again, 1, 0); err == nil {
		t.Fatal("duplicate answer accepted")
	} else if ge, ok := err.(*GameError); !ok || ge.Status != 409 {
		t.Fatalf("duplicate answer error = %v, want GameError 409", err)
	}

	// A stale level number gets a level-mismatch 409.
	stale := ps[1]
	if _, err := g.Answer(ctx, room.ID, &stale, 15, 0); err == nil {
		t.Fatal("answer with a non-active level accepted")
	} else if ge, ok := err.(*GameError); !ok || ge.Status != 409 {
		t.Fatalf("stale-level error = %v, want GameError 409", err)
	}

	// Exactly one score: seat 1's row was left untouched by the rejects.
	p1 := reloadPlayer(t, ctx, players, ps[0])
	if p1.LevelReached != 1 || p1.Amount != 100 || p1.Status != "active" {
		t.Fatalf("seat 1 row = %d/%d/%s, want 1/100/active",
			p1.LevelReached, p1.Amount, p1.Status)
	}
}

// TestTimeoutFallsAndFinishes is the timer-fallback gate: an active player
// who never answers falls at ends_at (run_ended only — NO answer_result,
// they never answered), the timer stops, the solo room finishes.
func TestTimeoutFallsAndFinishes(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	g.SetBudgetForTier("easy", 300*time.Millisecond)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "solo", 1)

	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	re := n.waitEvent("run_ended", -1, 10*time.Second)
	if re == nil {
		t.Fatal("timeout produced no run_ended within 10s")
	}
	if fmt.Sprint(re["status"]) != "fallen" || fmt.Sprint(re["amount"]) != "0" {
		t.Fatalf("run_ended = %v, want fallen/0", re)
	}

	if n.eventData("answer_result", -1) != nil {
		t.Fatal("timeout must NOT produce answer_result — the player never answered")
	}

	over := n.waitEvent("game_over", -1, 5*time.Second)
	if over == nil {
		t.Fatal("no game_over after the timeout fall")
	}
	gotRoom, err := rooms.GetRoom(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRoom.Status != "finished" {
		t.Fatalf("room status = %q, want finished", gotRoom.Status)
	}

	// A late POST after the timeout is rejected (mutex-serialized paths).
	fresh := reloadPlayer(t, ctx, players, ps[0])
	if _, err := g.Answer(ctx, room.ID, &fresh, 1, 0); err == nil {
		t.Fatal("late POST after timeout accepted")
	}
}

// TestDuelSurvivorContinues is the full duel flow at service level: both
// answer level 1, one falls at the next level, the survivor keeps
// climbing solo, and the game only ends when the survivor's run ends.
func TestDuelSurvivorContinues(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "duel", 2)

	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	// Level 1: both correct → level 2.
	answerCorrectly(t, ctx, g, players, room.ID, ps[0])
	answerCorrectly(t, ctx, g, players, room.ID, ps[1])
	if got := currentLevel(t, g, room.ID); got != 2 {
		t.Fatalf("level = %d, want 2", got)
	}

	// Level 2: seat 1 falls; the level stays open for seat 2.
	ans := answerWrong(t, ctx, g, players, room.ID, ps[0])
	if ans != 2 {
		t.Fatalf("seat 1 fell at %d, want 2", ans)
	}
	if got := currentLevel(t, g, room.ID); got != 2 {
		t.Fatalf("game advanced after only one player fell (level %d)", got)
	}

	// run_ended broadcast to the room for seat 1.
	re := n.eventData("run_ended", -1)
	if re == nil || fmt.Sprint(re["seat"]) != "1" || fmt.Sprint(re["status"]) != "fallen" {
		t.Fatalf("run_ended after the fall = %v, want seat1/fallen", re)
	}

	// Survivor answers level 2 → the level advances for the room.
	answerCorrectly(t, ctx, g, players, room.ID, ps[1])
	if got := currentLevel(t, g, room.ID); got != 3 {
		t.Fatalf("survivor fell through — level = %d, want 3", got)
	}

	// The fallen player cannot answer or walk anymore (invariant 2).
	fallen := reloadPlayer(t, ctx, players, ps[0])
	if _, aerr := g.Answer(ctx, room.ID, &fallen, currentLevel(t, g, room.ID), 0); aerr == nil {
		t.Fatal("fallen player could answer")
	} else if ge, ok := aerr.(*GameError); !ok || ge.Status != 409 {
		t.Fatalf("fallen answer error = %v, want GameError 409", aerr)
	}
	if _, werr := g.Walk(ctx, room.ID, &fallen); werr == nil {
		t.Fatal("fallen player could walk")
	} else if ge, ok := werr.(*GameError); !ok || ge.Status != 409 {
		t.Fatalf("fallen walk error = %v, want GameError 409", werr)
	}

	// Survivor climbs 3..6 then walks at 7: game_over with sorted
	// standings (survivor's amount above seat 1's $0).
	survivor := reloadPlayer(t, ctx, players, ps[1])
	for i := 0; i < 4; i++ {
		answerCorrectly(t, ctx, g, players, room.ID, survivor)
	}
	amount := walkNow(t, ctx, g, players, room.ID, survivor)

	over := n.waitEvent("game_over", -1, time.Second)
	if over == nil {
		t.Fatal("no game_over after the survivor's walk")
	}
	standings, ok := over["standings"].([]map[string]interface{})
	if !ok || len(standings) != 2 {
		t.Fatalf("standings type = %T (%#v), want 2 maps", over["standings"], over["standings"])
	}
	if fmt.Sprint(standings[0]["amount"]) != strconv.Itoa(amount) {
		t.Fatalf("standings not sorted by amount: %#v", standings)
	}
	if fmt.Sprint(standings[1]["amount"]) != "0" {
		t.Fatalf("seat 1 amount wrong in standings: %#v", standings)
	}

	gotRoom, err := rooms.GetRoom(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRoom.Status != "finished" {
		t.Fatalf("room status = %q, want finished", gotRoom.Status)
	}
}

// TestWinAt15 covers the win path: answering level 15 correctly ends the
// run with status won, amount $1,000,000, run_ended {won} broadcast
// (without correct_index — fell nothing) and game_over.
func TestWinAt15(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "solo", 1)

	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	for i := 1; i < 15; i++ {
		answerCorrectly(t, ctx, g, players, room.ID, ps[0])
	}
	final := reloadPlayer(t, ctx, players, ps[0])
	outcome, err := g.Answer(ctx, room.ID, &final, 15, shuffledCorrectIndex(t, g, room.ID, 15))
	if err != nil {
		t.Fatalf("answering 15: %v", err)
	}
	if outcome.Status != "won" || outcome.Amount != 1000000 || !outcome.Correct {
		t.Fatalf("outcome = %+v, want won/1000000/correct", outcome)
	}

	re := n.eventData("run_ended", -1)
	if re == nil || fmt.Sprint(re["status"]) != "won" || fmt.Sprint(re["amount"]) != "1000000" {
		t.Fatalf("run_ended = %v, want won/1000000", re)
	}
	if _, has := re["correct_index"]; has {
		t.Fatalf("won run_ended must not carry correct_index: %v", re)
	}

	over := n.waitEvent("game_over", -1, time.Second)
	if over == nil {
		t.Fatal("no game_over after the win")
	}
	fresh := reloadPlayer(t, ctx, players, ps[0])
	if fresh.Status != "won" || fresh.Amount != 1000000 || fresh.LevelReached != 15 {
		t.Fatalf("player row = %d/%d/%s, want 15/1000000/won",
			fresh.LevelReached, fresh.Amount, fresh.Status)
	}
}

// TestDuelDrawBothFallEqualStandings is the M4 draw case: both players
// fall at the SAME level (level 1) with $0, so the standings carry EQUAL
// top amounts. The server emits them sorted (tie broken by seat →
// deterministic) exactly ONCE, and the draw itself is decided CLIENT-side
// by comparing the top amounts — there is no server "draw" flag (SPEC §1
// "Tie = draw", see the game_over row annotation).
func TestDuelDrawBothFallEqualStandings(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "duel", 2)

	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	// Seat 1 falls at level 1 (the level stays open for seat 2).
	if lvl := answerWrong(t, ctx, g, players, room.ID, ps[0]); lvl != 1 {
		t.Fatalf("seat 1 fell at %d, want 1", lvl)
	}
	// Seat 2 falls at the same level → nobody left climbing → finish.
	if lvl := answerWrong(t, ctx, g, players, room.ID, ps[1]); lvl != 1 {
		t.Fatalf("seat 2 fell at %d, want 1", lvl)
	}

	over := n.waitEvent("game_over", -1, time.Second)
	if over == nil {
		t.Fatal("no game_over when the last active player fell")
	}
	standings, ok := over["standings"].([]map[string]interface{})
	if !ok || len(standings) != 2 {
		t.Fatalf("standings = %T (%#v), want 2 entries", over["standings"], over["standings"])
	}
	for i, s := range standings {
		if s["amount"] != 0 {
			t.Fatalf("standings[%d] amount = %v, want 0 (both fell at level 1)", i, s["amount"])
		}
		// Exact shape per the SPEC game_over row: {seat, name, status,
		// amount} — nothing else.
		if len(s) != 4 || s["seat"] == nil || s["name"] == nil || s["status"] == nil {
			t.Fatalf("standings[%d] = %v, want exactly {seat,name,status,amount}", i, s)
		}
		if s["status"] != "fallen" {
			t.Fatalf("standings[%d] status = %v, want fallen", i, s["status"])
		}
	}
	if standings[0]["seat"] != 1 || standings[1]["seat"] != 2 {
		t.Fatalf("tie order = %v/%v, want seat 1 before seat 2 (deterministic)",
			standings[0]["seat"], standings[1]["seat"])
	}
	// Equal top amounts IS the draw condition (client-side).
	if standings[0]["amount"] != standings[1]["amount"] {
		t.Fatalf("expected a draw: %v vs %v", standings[0]["amount"], standings[1]["amount"])
	}
	// game_over is a one-shot broadcast.
	if c := len(n.eventsOf("game_over", -1)); c != 1 {
		t.Fatalf("game_over emitted %d times, want exactly 1", c)
	}
}

// TestTimeoutRevealToOpponent is the M4 TIMEOUT branch of the reveal rule:
// a duel player who never answers falls at ends_at (run_ended only — NO
// answer_result, nothing was answered) and the still-active survivor
// receives opponent_reveal with chosen_index = -1 (nothing was chosen).
// The reveal goes ONLY to the other player, never back to the fallen one.
func TestTimeoutRevealToOpponent(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	g.SetBudgetForTier("easy", 400*time.Millisecond)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "duel", 2)

	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	// Seat 1 answers level 1 correctly; seat 2 never answers.
	if _, err := g.Answer(ctx, room.ID, &ps[0], 1, shuffledCorrectIndex(t, g, room.ID, 1)); err != nil {
		t.Fatal(err)
	}

	re := n.waitEvent("run_ended", -1, 10*time.Second)
	if re == nil || fmt.Sprint(re["seat"]) != "2" || fmt.Sprint(re["status"]) != "fallen" {
		t.Fatalf("run_ended = %v, want seat 2 fallen (timeout)", re)
	}
	// The timed-out player gets no answer_result (they never answered).
	if seat2 := n.eventsOf("answer_result", 2); len(seat2) != 0 {
		t.Fatalf("timed-out seat 2 got answer_result: %v", seat2)
	}
	// The survivor's reveal: chosen_index -1, correct_index present.
	reveal := n.waitEvent("opponent_reveal", 1, 5*time.Second)
	if reveal == nil {
		t.Fatal("seat 1 got no opponent_reveal for the timeout fall")
	}
	if ci := fmt.Sprint(reveal["chosen_index"]); ci != "-1" {
		t.Fatalf("opponent_reveal chosen_index = %s, want -1 (nothing chosen)", ci)
	}
	if _, has := reveal["correct_index"]; !has {
		t.Fatal("opponent_reveal without correct_index")
	}
	// The fallen player himself never receives the reveal.
	if leaked := n.eventsOf("opponent_reveal", 2); len(leaked) != 0 {
		t.Fatalf("the fallen player received opponent_reveal: %v", leaked)
	}
	// The survivor continues: level 2 issued right after the fall.
	if lvl := currentLevel(t, g, room.ID); lvl != 2 {
		t.Fatalf("level = %d, want 2 after seat 2's timeout fall", lvl)
	}

	for _, c := range leakScanAll(n.events) {
		if c {
			t.Fatalf("leaked correct_index outside the allowed events: %v", n.events)
		}
	}
}

// flakyPlayers fails the first N GetPlayersByRoom calls (simulated
// transient DB outage) and delegates everything else to the real repo.
type flakyPlayers struct {
	repository.PlayerRepository
	mu    sync.Mutex
	fails int
}

func (f *flakyPlayers) GetPlayersByRoom(ctx context.Context, roomID string) ([]models.Player, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return nil, fmt.Errorf("simulated transient roster failure")
	}
	return f.PlayerRepository.GetPlayersByRoom(ctx, roomID)
}

// TestTimeoutRetryReArm proves the M3-review re-arm (M4 gate item): when
// the timer path's roster read fails transiently, the branch re-arms a
// ~1s retry timer instead of stranding the level. Here BOTH active
// players are resolved-but-unprocessed (the timeout is the only thing
// that can ever resolve them), so without the re-arm the game would hang
// forever; with it the third attempt succeeds and the game finishes.
func TestTimeoutRetryReArm(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	n := &fakeNotifier{}
	realPlayers := repository.NewPlayerRepository(db)
	flaky := &flakyPlayers{PlayerRepository: realPlayers, fails: 2}
	rooms := NewRoomService(repository.NewRoomRepository(db))
	g := NewGameService(
		db,
		rooms,
		flaky,
		repository.NewQuestionRepository(db),
		repository.NewRoomQuestionRepository(db),
		n,
	)
	g.SetRevealBeat(0) // harness default: legacy synchronous advance (see newTestGame)
	t.Cleanup(func() {
		g.mu.Lock()
		for id, game := range g.games {
			if game.timer != nil {
				game.timer.Stop()
			}
			if game.beat != nil {
				game.beat.Stop()
			}
			delete(g.games, id)
		}
		g.mu.Unlock()
	})
	g.SetBudgetForTier("easy", 300*time.Millisecond)
	room, ps := createTestRoom(t, ctx, db, rooms, realPlayers, "duel", 2)

	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	// Timer fires at 300ms → attempt 0 fails → retry 1s later → attempt 1
	// fails → retry → attempt 2 succeeds → both players fall → game_over.
	over := n.waitEvent("game_over", -1, 15*time.Second)
	if over == nil {
		t.Fatal("no game_over — a transient roster failure stranded the level (re-arm missing?)")
	}
	flaky.mu.Lock()
	exercised := 3 - flaky.fails
	flaky.mu.Unlock()
	if exercised < 2 {
		t.Fatalf("failure branch exercised %d times, want >= 2", exercised)
	}

	// Both timed-out players are fallen with $0; both runs got run_ended.
	for _, p := range ps {
		fresh := reloadPlayer(t, ctx, realPlayers, p)
		if fresh.Status != "fallen" || fresh.Amount != 0 {
			t.Fatalf("seat %d row = %d/%s, want 0/fallen", fresh.Seat, fresh.Amount, fresh.Status)
		}
	}
	if c := len(n.eventsOf("run_ended", -1)); c != 2 {
		t.Fatalf("run_ended emitted %d times, want 2 (one per timed-out player)", c)
	}
	if c := len(n.eventsOf("game_over", -1)); c != 1 {
		t.Fatalf("game_over emitted %d times, want exactly 1", c)
	}
}

// TestWalkAfterCorrectAnswerBanks (adversarial review finding 2): the WALK
// AWAY affordance showed the player's current stake, but the server refused
// the walk for the rest of any level they had already answered — so after a
// CORRECT answer (the only case where the seat is still active) the button
// was a dead end. Walking now banks what the correct answer secured and ends
// the run, exactly as walking before answering would.
func TestWalkAfterCorrectAnswerBanks(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "solo", 1)
	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	answerCorrectly(t, ctx, g, players, room.ID, ps[0])
	if st := reloadPlayer(t, ctx, players, ps[0]); st.Status != "active" {
		t.Fatalf("after a correct answer the seat must still be active, got %q", st.Status)
	}

	amount := walkNow(t, ctx, g, players, room.ID, ps[0])
	if want := WalkAmount(1); amount != want {
		t.Fatalf("walk-after-correct amount = %d, want the secured %d", amount, want)
	}

	re := n.eventData("run_ended", -1)
	if re == nil || fmt.Sprint(re["status"]) != "walked" {
		t.Fatalf("run_ended = %v, want walked", re)
	}
	if _, has := re["correct_index"]; has {
		t.Fatalf("walk run_ended must NOT carry correct_index: %v", re)
	}
	fresh := reloadPlayer(t, ctx, players, ps[0])
	if fresh.Status != "walked" || fresh.Amount != amount {
		t.Fatalf("player row = %d/%s, want %d/walked", fresh.Amount, fresh.Status, amount)
	}
}
