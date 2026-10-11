package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vingarcia/ksql"
)

// This file covers the wave-4 IN-ROOM REMATCH over the REAL chain: a
// finished duel replays IN THE SAME ROOM (same code — no new room is
// minted), every per-seat row resets (psql-verified), the room returns to
// the deferred lobby and a full second pass completes with FRESH
// standings (no stale carryover); mid-run and lobby replays 409; solo
// rooms replay straight into a fresh Q1.

// countEvent counts an event name across the stream's FULL history.
func countEvent(st *wsStream, event string) int {
	n := 0
	for _, m := range st.all {
		if m["event"] == event {
			n++
		}
	}
	return n
}

// playerRowsOf loads the roster straight from the DB (rematch-reset
// oracle).
func playerRowsOf(t *testing.T, db ksql.DB, roomID string) []struct {
	Seat         int    `ksql:"seat"`
	Status       string `ksql:"status"`
	Amount       int    `ksql:"amount"`
	LevelReached int    `ksql:"level_reached"`
	Lifelines    string `ksql:"lifelines_left"`
} {
	t.Helper()
	var rows []struct {
		Seat         int    `ksql:"seat"`
		Status       string `ksql:"status"`
		Amount       int    `ksql:"amount"`
		LevelReached int    `ksql:"level_reached"`
		Lifelines    string `ksql:"lifelines_left"`
	}
	if err := db.Query(context.Background(), &rows,
		"SELECT seat, status, amount, level_reached, lifelines_left FROM millionaire_players WHERE room_id = $1 ORDER BY seat",
		roomID); err != nil {
		t.Fatalf("loading players: %v", err)
	}
	return rows
}

// assertFreshRoster asserts the wave-4 reset on every seat.
func assertFreshRoster(t *testing.T, db ksql.DB, roomID string, wantSeats int) {
	t.Helper()
	rows := playerRowsOf(t, db, roomID)
	if len(rows) != wantSeats {
		t.Fatalf("roster = %d rows, want %d", len(rows), wantSeats)
	}
	for _, r := range rows {
		if r.Status != "active" || r.Amount != 0 || r.LevelReached != 0 {
			t.Fatalf("seat %d not reset: %d/%d/%s, want 0/0/active", r.Seat, r.LevelReached, r.Amount, r.Status)
		}
		var left []string
		if err := json.Unmarshal([]byte(r.Lifelines), &left); err != nil {
			t.Fatalf("seat %d lifelines_left = %q: %v", r.Seat, r.Lifelines, err)
		}
		// jsonb normalizes spacing — compare the parsed default set.
		if fmt.Sprint(left) != fmt.Sprint([]string{"fifty_fifty", "audience", "switch"}) {
			t.Fatalf("seat %d lifelines_left = %v, want the default set", r.Seat, left)
		}
	}
}

// TestDuelReplaySameRoomFullPass is the full 2-pass scripted duel in ONE
// room: pass 1 ends (B walks with £300, A falls £0); a MID-RUN replay is
// rejected 409; the rematch resets both seats (psql), re-shows the lobby
// (rematch_ready, same code) and waits; the host starts; pass 2 ends with
// BOTH falling at L1 → standings must be FRESH 0/0 (no £300 carryover);
// a second rematch right after works too, initiated by SEAT 2.
func TestDuelReplaySameRoomFullPass(t *testing.T) {
	base, wsBase, db, _, registerRooms := fullTestServer(t, nil)
	a, b, roomID, roomCode, tokenA, tokenB := duelLobby(t, base, wsBase, registerRooms)
	startDuelHost(t, base, roomID, tokenA, a, b)

	answer := func(st *wsStream, token string, level, choice int) (int, map[string]interface{}) {
		return postJSON(t, base, fmt.Sprintf("/api/rooms/%s/answer", roomCode),
			map[string]interface{}{"token": token, "level": level, "choice_index": choice})
	}

	// --- Pass 1: both correct through L1–2 ------------------------------
	for level := 1; level <= 2; level++ {
		c := correctIndexFromDB(t, db, roomID, level)
		for _, pair := range []struct {
			st    *wsStream
			token string
		}{{a, tokenA}, {b, tokenB}} {
			if rc, rbody := answer(pair.st, pair.token, level, c); rc != 200 {
				t.Fatalf("pass1 L%d: http %d (%v)", level, rc, rbody)
			}
		}
		for _, st := range []*wsStream{a, b} {
			if qn := st.waitFor("question_next", 5*time.Second); qn == nil {
				t.Fatalf("pass1: no question_next after L%d", level)
			}
		}
	}

	// A falls at L3 (B still active) → MID-RUN replay is rejected.
	aChoice := (correctIndexFromDB(t, db, roomID, 3) + 1) % 4
	if rc, rbody := answer(a, tokenA, 3, aChoice); rc != 200 {
		t.Fatalf("pass1 L3 wrong: http %d (%v)", rc, rbody)
	}
	if a.waitFor("run_ended", 5*time.Second) == nil {
		t.Fatal("pass1: no run_ended for seat 1's fall")
	}
	for _, tok := range []string{tokenA, tokenB} {
		code, body := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/replay", roomCode),
			map[string]interface{}{"token": tok})
		if code != 409 || !strings.Contains(fmt.Sprint(body["error"]), "still in progress") {
			t.Fatalf("mid-run replay: http %d body %v, want 409 in-progress", code, body)
		}
	}

	// B answers L3 and walks at L4 → £300 secured, game_over.
	if rc, rbody := answer(b, tokenB, 3, correctIndexFromDB(t, db, roomID, 3)); rc != 200 {
		t.Fatalf("pass1 B L3: http %d (%v)", rc, rbody)
	}
	if qn := b.waitFor("question_next", 5*time.Second); qn == nil ||
		fmt.Sprint(qn["question"].(map[string]interface{})["level"]) != "4" {
		t.Fatalf("pass1: survivor did not get L4: %v", qn)
	}
	walkCode, walkBody := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/walk", roomCode),
		map[string]interface{}{"token": tokenB})
	if walkCode != 200 || walkBody["amount"] != float64(300) {
		t.Fatalf("pass1 B walk: http %d body %v, want 200/300", walkCode, walkBody)
	}
	over := a.waitFor("game_over", 5*time.Second)
	if over == nil {
		t.Fatal("pass1: no game_over")
	}
	if standings, ok := over["standings"].([]interface{}); !ok ||
		fmt.Sprint(standings[0].(map[string]interface{})["amount"]) != "300" {
		t.Fatalf("pass1 standings = %v, want B's 300 on top", over["standings"])
	}
	if room, _ := roomStatusOf(t, db, roomID); room != "finished" {
		t.Fatalf("pass1 room status = %q, want finished", room)
	}

	// --- Rematch in the SAME room (host initiates) ----------------------
	code, body := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/replay", roomCode),
		map[string]interface{}{"token": tokenA})
	if code != 200 || body["replayed"] != true || body["started"] != false {
		t.Fatalf("duel replay: http %d body %v, want 200 {replayed:true, started:false}", code, body)
	}
	for _, st := range []*wsStream{a, b} {
		rr := st.waitFor("rematch_ready", 5*time.Second)
		if rr == nil {
			t.Fatal("no rematch_ready on the lobby re-show")
		}
		seats, ok := rr["seats"].([]interface{})
		if !ok || len(seats) != 2 || fmt.Sprint(seats[0]) != "1" || fmt.Sprint(seats[1]) != "2" {
			t.Fatalf("rematch_ready seats = %v, want [1 2]", rr["seats"])
		}
	}

	// The SAME room keeps the SAME code — nothing new was minted.
	if room, _ := roomStatusOf(t, db, roomID); room != "lobby" {
		t.Fatalf("room status after replay = %q, want lobby", room)
	}
	if n := roomQuestionCount(t, db, roomID); n != 30 {
		t.Fatalf("rematch layout = %d rows, want a fresh 30", n)
	}
	assertFreshRoster(t, db, roomID, 2)

	// Still deferred: no new game is live until the host starts.
	beforeA, beforeB := countEvent(a, "game_started"), countEvent(b, "game_started")
	if beforeA != 1 || beforeB != 1 {
		t.Fatalf("game_started counts after pass 1 = %d/%d, want 1/1", beforeA, beforeB)
	}
	// A replay while the rematch is already waiting → 409 (lobby state).
	code, body = postJSON(t, base, fmt.Sprintf("/api/rooms/%s/replay", roomCode),
		map[string]interface{}{"token": tokenA})
	if code != 409 || !strings.Contains(fmt.Sprint(body["error"]), "lobby") {
		t.Fatalf("double rematch: http %d body %v, want 409 (already waiting in the lobby)", code, body)
	}

	// Host starts the rematch.
	startDuelHost(t, base, roomID, tokenA, a, b)
	if gotA, gotB := countEvent(a, "game_started"), countEvent(b, "game_started"); gotA != 2 || gotB != 2 {
		t.Fatalf("game_started counts into pass 2 = %d/%d, want 2/2", gotA, gotB)
	}

	// --- Pass 2: BOTH fall at L1 → fresh standings 0/0 ------------------
	for _, pair := range []struct {
		st    *wsStream
		token string
	}{{a, tokenA}, {b, tokenB}} {
		c := correctIndexFromDB(t, db, roomID, 1)
		if rc, rbody := answer(pair.st, pair.token, 1, (c+1)%4); rc != 200 {
			t.Fatalf("pass2 fall: http %d (%v)", rc, rbody)
		}
	}
	over2 := a.waitFor("game_over", 5*time.Second)
	if over2 == nil {
		t.Fatal("pass2: no game_over")
	}
	standings2, ok := over2["standings"].([]interface{})
	if !ok || len(standings2) != 2 {
		t.Fatalf("pass2 standings = %v", over2["standings"])
	}
	for i, s := range standings2 {
		if fmt.Sprint(s.(map[string]interface{})["amount"]) != "0" {
			t.Fatalf("pass2 standings[%d] = %v, want a FRESH 0 (no stale carryover)", i, s)
		}
	}

	// --- Second rematch hands-on: SEAT 2 initiates, works again ---------
	code, body = postJSON(t, base, fmt.Sprintf("/api/rooms/%s/replay", roomCode),
		map[string]interface{}{"token": tokenB})
	if code != 200 || body["replayed"] != true || body["started"] != false {
		t.Fatalf("second rematch (seat 2): http %d body %v, want 200 {replayed:true}", code, body)
	}
	assertFreshRoster(t, db, roomID, 2)
	if room, _ := roomStatusOf(t, db, roomID); room != "lobby" {
		t.Fatalf("room after the second rematch = %q, want lobby", room)
	}
	a.waitFor("rematch_ready", 5*time.Second)
	b.waitFor("rematch_ready", 5*time.Second)

	// Leak discipline across the whole two-pass history.
	a.drainUntil(200 * time.Millisecond)
	b.drainUntil(200 * time.Millisecond)
	a.leakScan(t)
	b.leakScan(t)
}

// TestSoloReplaySameRoom pins the solo rematch: the room is private (no
// code), so the replay resets in place and INSTANTLY issues a fresh Q1.
func TestSoloReplaySameRoom(t *testing.T) {
	base, wsBase, db, _, registerRooms := fullTestServer(t, nil)

	s := dialStream(t, wsBase+"/ws/rooms?mode=solo&name=Replayer")
	created := s.waitFor("room_created", 5*time.Second)
	if created == nil {
		t.Fatal("no room_created")
	}
	roomID := created["room_id"].(string)
	registerCleanup(t, registerRooms, roomID)
	if code, _ := created["code"].(string); code != "" {
		t.Fatalf("solo code = %q, want ''", code)
	}
	token := created["token"].(string)
	if s.waitFor("game_started", 10*time.Second) == nil {
		t.Fatal("no instant game_started")
	}

	// L1 correct, hold-continue, fall at L2 → game_over.
	c := correctIndexFromDB(t, db, roomID, 1)
	if rc, _ := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/answer", roomID),
		map[string]interface{}{"token": token, "level": 1, "choice_index": c}); rc != 200 {
		t.Fatal("L1 answer failed")
	}
	s.waitFor("answer_result", 5*time.Second)
	if cc, cb := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/continue", roomID),
		map[string]interface{}{"token": token}); cc != 200 || cb["continued"] != true {
		t.Fatalf("continue: http %d body %v", cc, cb)
	}
	s.waitFor("question_next", 5*time.Second)
	c2 := correctIndexFromDB(t, db, roomID, 2)
	if rc, _ := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/answer", roomID),
		map[string]interface{}{"token": token, "level": 2, "choice_index": (c2 + 1) % 4}); rc != 200 {
		t.Fatal("L2 wrong answer failed")
	}
	if s.waitFor("game_over", 5*time.Second) == nil {
		t.Fatal("no game_over after the solo fall")
	}
	if room, _ := roomStatusOf(t, db, roomID); room != "finished" {
		t.Fatalf("solo room status = %q, want finished", room)
	}

	// Replay: same room, instant fresh Q1 (no lobby step exists in solo).
	code, body := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/replay", roomID),
		map[string]interface{}{"token": token})
	if code != 200 || body["replayed"] != true || body["started"] != true {
		t.Fatalf("solo replay: http %d body %v, want 200 {replayed:true, started:true}", code, body)
	}
	gs := s.waitFor("game_started", 5*time.Second)
	if gs == nil {
		t.Fatal("no fresh game_started after the solo replay")
	}
	if q, ok := gs["question"].(map[string]interface{}); !ok || fmt.Sprint(q["level"]) != "1" {
		t.Fatalf("rematch question = %v, want a fresh level 1", gs["question"])
	}
	if room, _ := roomStatusOf(t, db, roomID); room != "active" {
		t.Fatalf("solo room after replay = %q, want active", room)
	}
	if n := roomQuestionCount(t, db, roomID); n != 30 {
		t.Fatalf("rematch layout = %d rows, want 30", n)
	}
	assertFreshRoster(t, db, roomID, 1)

	// The replayed game is REAL: a correct L1 scores again on this run.
	c = correctIndexFromDB(t, db, roomID, 1)
	if rc, rbody := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/answer", roomID),
		map[string]interface{}{"token": token, "level": 1, "choice_index": c}); rc != 200 || rbody["correct"] != true {
		t.Fatalf("rematch L1: http %d body %v, want a scoring 200", rc, rbody)
	}

	s.drainUntil(200 * time.Millisecond)
	s.leakScan(t)
}

// TestReplayValidation covers the replay endpoint's rule table on
// rejections: unknown code → 404; bad token → 403; a lobby duel (never
// started) → 409.
func TestReplayValidation(t *testing.T) {
	base, wsBase, _, _, registerRooms := fullTestServer(t, nil)

	code, body := postJSON(t, base, "/api/rooms/MISSING-CODE-42/replay",
		map[string]interface{}{"token": "whatever"})
	if code != 404 || !strings.Contains(fmt.Sprint(body["error"]), "room not found") {
		t.Fatalf("replay on unknown code: http %d body %v, want 404", code, body)
	}

	// A lobby duel (both seated, host start NOT issued) → 409.
	_, _, roomID, roomCode, _, tokenB := duelLobby(t, base, wsBase, registerRooms)
	code, body = postJSON(t, base, fmt.Sprintf("/api/rooms/%s/replay", roomCode),
		map[string]interface{}{"token": tokenB})
	if code != 409 || !strings.Contains(fmt.Sprint(body["error"]), "lobby") {
		t.Fatalf("lobby replay: http %d body %v, want 409 nothing-to-replay", code, body)
	}

	// Bad token → 403 (before any rule check).
	code, body = postJSON(t, base, fmt.Sprintf("/api/rooms/%s/replay", roomCode),
		map[string]interface{}{"token": "bogus-token"})
	if code != 403 {
		t.Fatalf("bad-token replay: http %d body %v, want 403", code, body)
	}
	_ = roomID // the uuid path is covered by the solo/duel flows above
}
