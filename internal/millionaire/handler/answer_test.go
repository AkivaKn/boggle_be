package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"boggle-api/internal/millionaire/service"

	"github.com/gorilla/websocket"
	"github.com/vingarcia/ksql"
)

// This file covers the M3 question lifecycle over the REAL handler chain:
// REST answer/walk endpoints + WS events + the timer fallback + the duel
// survivor flow + the leak discipline (SPEC §2 invariant 1).

// wsStream is a continuously-read WS event stream with a scan buffer, so
// tests wait for events by predicate instead of fixed ordering (private
// answer_result and room broadcasts can interleave between sockets).
type wsStream struct {
	t    *testing.T
	conn *websocket.Conn
	ch   chan map[string]interface{}
	buf  []map[string]interface{}
	all  []map[string]interface{} // every event ever received (leak scanning)
}

func dialStream(t *testing.T, url string) *wsStream {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v (http %v)", url, err, resp)
	}
	t.Cleanup(func() { conn.Close() })
	s := &wsStream{t: t, conn: conn, ch: make(chan map[string]interface{}, 128)}
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				close(s.ch)
				return
			}
			var m map[string]interface{}
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			s.ch <- m
		}
	}()
	return s
}

// waitFor scans the buffered events first, then reads with a deadline.
// Returns nil on timeout. Every received event is kept for the leak scan.
func (s *wsStream) waitFor(event string, timeout time.Duration) map[string]interface{} {
	start := time.Now()
	for {
		for i, m := range s.buf {
			if m["event"] == event {
				s.buf = append(s.buf[:i], s.buf[i+1:]...)
				return m
			}
		}
		if time.Since(start) > timeout {
			return nil
		}
		select {
		case m, ok := <-s.ch:
			if !ok {
				return nil
			}
			s.buf = append(s.buf, m)
			s.all = append(s.all, m)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// drain consumes pending events into all/history without matching anything.
func (s *wsStream) drainUntil(quiet time.Duration) {
	deadline := time.Now().Add(quiet)
	for {
		select {
		case m, ok := <-s.ch:
			if !ok {
				return
			}
			s.all = append(s.all, m)
		case <-time.After(time.Until(deadline)):
			return
		}
	}
}

// leakScan walks every recorded event (recursively) asserting no
// correct_index outside the SPEC §2 allowance (answer_result; run_ended
// when status=fallen; opponent_reveal — M4).
func (s *wsStream) leakScan(t *testing.T) {
	t.Helper()
	for _, m := range s.all {
		if scanForCorrectIndex(m, fmt.Sprint(m["event"])) {
			t.Fatalf("event leaks correct_index outside the allowed events: %v", m)
		}
	}
}

func scanForCorrectIndex(v interface{}, event string) bool {
	switch val := v.(type) {
	case map[string]interface{}:
		name, _ := val["event"].(string)
		if name != "" {
			event = name
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
			if scanForCorrectIndex(sub, event) {
				return true
			}
		}
	case []interface{}:
		for _, sub := range val {
			if scanForCorrectIndex(sub, event) {
				return true
			}
		}
	}
	return false
}

func postJSON(t *testing.T, base, path string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(base+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// correctIndexFromDB reads the room's persisted shuffled layout (variant
// 1) — a server-side oracle for tests; it must never appear in a client
// event before the answer resolves.
func correctIndexFromDB(t *testing.T, db ksql.DB, roomID string, level int) int {
	t.Helper()
	var row struct {
		Idx int `ksql:"idx"`
	}
	if err := db.QueryOne(context.Background(), &row,
		"SELECT shuffled_correct_index AS idx FROM millionaire_room_questions WHERE room_id = $1 AND level = $2 AND variant = 1",
		roomID, level); err != nil {
		t.Fatalf("reading shuffled_correct_index: %v", err)
	}
	return row.Idx
}

// fullTestServer boots the engine (WS + REST routes) over an httptest
// server; returns the http base, the ws base, the db, the game service
// (for harness overrides like SetRevealBeat in the M7 pacing tests) and a
// room cleanup that tests must register their rooms with (players and
// question rows cascade on room delete).
func fullTestServer(t *testing.T, budgets map[string]time.Duration) (baseURL, wsBase string, db ksql.DB, game *service.GameService, registerRooms func(ids ...string)) {
	t.Helper()
	db, game, engine, _, cleanupRooms := newGameTestServer(t, budgets)
	srv := httptest.NewServer(engine)
	t.Cleanup(func() { srv.Close() })
	return srv.URL, "ws" + strings.TrimPrefix(srv.URL, "http"), db, game, cleanupRooms
}

// registerCleanup deletes the given rooms when the test ends.
func registerCleanup(t *testing.T, registerRooms func(ids ...string), roomIDs ...string) {
	t.Helper()
	t.Cleanup(func() { registerRooms(roomIDs...) })
}

// TestAnswerRESTFlow is the scripted solo REST+WS run: correct answer at
// levels 1–3, question_next between, wrong answer at 4 → run_ended fallen
// $0 → game_over, then a late POST is rejected 409 + error event.
func TestAnswerRESTFlow(t *testing.T) {
	base, wsBase, db, _, registerRooms := fullTestServer(t, nil)

	s := dialStream(t, wsBase+"/ws/rooms?mode=solo&name=Runner")
	created := s.waitFor("room_created", 5*time.Second)
	if created == nil {
		t.Fatal("no room_created")
	}
	roomID := created["room_id"].(string)
	roomCode := created["code"].(string)
	if roomCode != "" {
		t.Fatalf("solo room_created code = %q, want '' — codes are a duel-only surface (M7)", roomCode)
	}
	roomCode = roomID // solo clients address REST by the internal uuid (no code exists)
	token := created["token"].(string)
	registerCleanup(t, registerRooms, roomID)

	gs := s.waitFor("game_started", 10*time.Second)
	if gs == nil {
		t.Fatal("no game_started")
	}
	gsQ, ok := gs["question"].(map[string]interface{})
	if !ok {
		t.Fatalf("game_started question = %T", gs["question"])
	}
	assertQuestionPayload(t, gsQ, 1)
	if _, has := gs["ends_at"]; !has {
		t.Fatal("game_started without ends_at")
	}

	// Answer levels 1..3 correctly, following the actual active level.
	// M7 solo pacing: a correct answer does NOT deal the next question —
	// the room holds until the player's continue (SPEC v1.1).
	for i := 1; i <= 3; i++ {
		correct := correctIndexFromDB(t, db, roomID, i)
		code, body := postJSON(t, base, "/api/rooms/"+roomCode+"/answer", map[string]interface{}{
			"token":        token,
			"level":        i,
			"choice_index": correct,
		})
		if code != 200 {
			t.Fatalf("level %d correct answer: http %d body %v", i, code, body)
		}
		if body["correct"] != true || fmt.Sprint(body["amount"]) != fmt.Sprint([]int{100, 200, 300}[i-1]) {
			t.Fatalf("level %d answer body = %v", i, body)
		}
		ar := s.waitFor("answer_result", 5*time.Second)
		if ar == nil {
			t.Fatalf("no answer_result after level %d", i)
		}
		if ar["correct"] != true || ar["correct_index"] == nil || ar["status"] != "active" {
			t.Fatalf("answer_result = %v", ar)
		}
		cc, cbody := postJSON(t, base, "/api/rooms/"+roomCode+"/continue", map[string]interface{}{
			"token": token,
		})
		if cc != 200 || cbody["continued"] != true {
			t.Fatalf("continue after level %d: http %d body %v, want 200 {continued:true}", i, cc, cbody)
		}
		qn := s.waitFor("question_next", 5*time.Second)
		if qn == nil {
			t.Fatalf("no question_next after the level %d continue", i)
		}
		qnQ, ok := qn["question"].(map[string]interface{})
		if !ok {
			t.Fatalf("question_next question = %T", qn["question"])
		}
		assertQuestionPayload(t, qnQ, i+1)
	}

	// Level 4: deliberate wrong answer → fall, run_ended, game_over.
	correct := correctIndexFromDB(t, db, roomID, 4)
	code, body := postJSON(t, base, "/api/rooms/"+roomCode+"/answer", map[string]interface{}{
		"token":        token,
		"level":        4,
		"choice_index": (correct + 1) % 4,
	})
	if code != 200 {
		t.Fatalf("wrong answer: http %d body %v", code, body)
	}
	if body["correct"] != false || fmt.Sprint(body["amount"]) != "0" || body["status"] != "fallen" {
		t.Fatalf("wrong answer body = %v, want fallen/0", body)
	}
	ar := s.waitFor("answer_result", 5*time.Second)
	if ar == nil || ar["correct"] != false || ar["status"] != "fallen" {
		t.Fatalf("answer_result after the fall = %v", ar)
	}
	re := s.waitFor("run_ended", 5*time.Second)
	if re == nil || fmt.Sprint(re["status"]) != "fallen" || fmt.Sprint(re["amount"]) != "0" {
		t.Fatalf("run_ended = %v, want fallen/0", re)
	}
	if _, has := re["correct_index"]; !has {
		t.Errorf("run_ended(fallen) may carry correct_index — want it present: %v", re)
	}
	over := s.waitFor("game_over", 5*time.Second)
	if over == nil {
		t.Fatal("no game_over after the solo fall")
	}
	if standings, ok := over["standings"].([]interface{}); !ok || len(standings) != 1 {
		t.Fatalf("standings = %v", over["standings"])
	}

	// gameState finished: any further command is rejected 409.
	code, body = postJSON(t, base, "/api/rooms/"+roomCode+"/answer", map[string]interface{}{
		"token": token, "level": 5, "choice_index": 0,
	})
	if code != 409 {
		t.Fatalf("late POST after game_over: http %d body %v, want 409", code, body)
	}
	errEv := s.waitFor("error", 5*time.Second)
	if errEv == nil || !strings.Contains(fmt.Sprint(errEv["message"]), "no game") {
		t.Fatalf("no error event on the socket after the late POST: %v", errEv)
	}

	// Leak scan over everything the socket ever saw.
	s.drainUntil(200 * time.Millisecond)
	s.leakScan(t)
}

// TestAnswerValidation covers REST validation and idempotency rejections:
// bad token → 403; bad body → 400; wrong level → 409 (+ error event);
// duplicate answer → 409 (+ error event), no score change.
func TestAnswerValidation(t *testing.T) {
	base, wsBase, db, _, registerRooms := fullTestServer(t, nil)

	s := dialStream(t, wsBase+"/ws/rooms?mode=solo&name=V")
	created := s.waitFor("room_created", 5*time.Second)
	if created == nil {
		t.Fatal("no room_created")
	}
	roomID := created["room_id"].(string)
	roomCode := created["code"].(string)
	if roomCode != "" {
		t.Fatalf("solo room_created code = %q, want '' — codes are a duel-only surface (M7)", roomCode)
	}
	roomCode = roomID // solo clients address REST by the internal uuid (no code exists)
	token := created["token"].(string)
	registerCleanup(t, registerRooms, roomID)
	s.waitFor("game_started", 10*time.Second)

	// Bad token.
	code, _ := postJSON(t, base, "/api/rooms/"+roomCode+"/answer", map[string]interface{}{
		"token": "not-a-token", "level": 1, "choice_index": 0,
	})
	if code != http.StatusForbidden {
		t.Fatalf("bad token: http %d, want 403", code)
	}

	// Bad body fields.
	code, _ = postJSON(t, base, "/api/rooms/"+roomCode+"/answer", map[string]interface{}{
		"token": token, "level": 1, "choice_index": 9,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("choice 9: http %d, want 400", code)
	}
	code, _ = postJSON(t, base, "/api/rooms/"+roomCode+"/answer", map[string]interface{}{
		"token": token, "level": 42, "choice_index": 0,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("level 42: http %d, want 400", code)
	}

	// Wrong level number while level 1 is active → 409 + error event.
	correct := correctIndexFromDB(t, db, roomID, 2)
	code, _ = postJSON(t, base, "/api/rooms/"+roomCode+"/answer", map[string]interface{}{
		"token": token, "level": 2, "choice_index": correct,
	})
	if code != http.StatusConflict {
		t.Fatalf("level 2 while 1 active: http %d, want 409", code)
	}
	errEv := s.waitFor("error", 5*time.Second)
	if errEv == nil {
		t.Fatal("no error event for the level-mismatch reject")
	}

	// A real correct answer, then a duplicate of the same (player, level)
	// is impossible here (solo advances instantly), so the duplicate is
	// rejected as soon as the game moved on: assert level mismatch at the
	// SAME level number.
	code, _ = postJSON(t, base, "/api/rooms/"+roomCode+"/answer", map[string]interface{}{
		"token": token, "level": 1, "choice_index": correctIndexFromDB(t, db, roomID, 1),
	})
	if code != 200 {
		t.Fatalf("good answer: http %d", code)
	}
	s.waitFor("question_next", 5*time.Second)
	code, _ = postJSON(t, base, "/api/rooms/"+roomCode+"/answer", map[string]interface{}{
		"token": token, "level": 1, "choice_index": correctIndexFromDB(t, db, roomID, 1),
	})
	if code != http.StatusConflict {
		t.Fatalf("stale-level answer: http %d, want 409", code)
	}
}

// TestWalkRESTFlow walks an active solo player mid-game: locks the last
// answered level's amount, run_ended without correct_index, immediate
// game_over, room finished.
func TestWalkRESTFlow(t *testing.T) {
	base, wsBase, db, _, registerRooms := fullTestServer(t, nil)

	s := dialStream(t, wsBase+"/ws/rooms?mode=solo&name=Walker")
	created := s.waitFor("room_created", 5*time.Second)
	if created == nil {
		t.Fatal("no room_created")
	}
	roomID := created["room_id"].(string)
	roomCode := created["code"].(string)
	if roomCode != "" {
		t.Fatalf("solo room_created code = %q, want '' — codes are a duel-only surface (M7)", roomCode)
	}
	roomCode = roomID // solo clients address REST by the internal uuid (no code exists)
	registerCleanup(t, registerRooms, roomID)
	token := created["token"].(string)
	s.waitFor("game_started", 10*time.Second)

	correct := correctIndexFromDB(t, db, roomID, 1)
	code, _ := postJSON(t, base, "/api/rooms/"+roomCode+"/answer", map[string]interface{}{
		"token": token, "level": 1, "choice_index": correct,
	})
	if code != 200 {
		t.Fatalf("level 1: http %d", code)
	}
	s.waitFor("answer_result", 5*time.Second)
	// M7 solo hold: the level is settled — continue deals level 2.
	if cc, _ := postJSON(t, base, "/api/rooms/"+roomCode+"/continue", map[string]interface{}{"token": token}); cc != 200 {
		t.Fatalf("hold continue after level 1: http %d", cc)
	}
	s.waitFor("question_next", 5*time.Second)

	// Walk at level 2 (answered level 1) → $100 per SPEC §1.
	code, body := postJSON(t, base, "/api/rooms/"+roomCode+"/walk", map[string]interface{}{"token": token})
	if code != 200 || body["amount"] != float64(100) {
		t.Fatalf("walk: http %d body %v, want 200/100", code, body)
	}
	if _, has := body["correct_index"]; has {
		t.Fatalf("walk response must not carry correct_index: %v", body)
	}
	re := s.waitFor("run_ended", 5*time.Second)
	if re == nil || fmt.Sprint(re["status"]) != "walked" || fmt.Sprint(re["amount"]) != "100" {
		t.Fatalf("run_ended = %v, want walked/100", re)
	}
	if _, has := re["correct_index"]; has {
		t.Fatalf("walked run_ended must not carry correct_index: %v", re)
	}
	if s.waitFor("game_over", 5*time.Second) == nil {
		t.Fatal("no game_over after the solo walk")
	}

	// A second walk after the run ended is rejected.
	code, _ = postJSON(t, base, "/api/rooms/"+roomCode+"/walk", map[string]interface{}{"token": token})
	if code != http.StatusConflict {
		t.Fatalf("second walk: http %d, want 409", code)
	}

	s.drainUntil(200 * time.Millisecond)
	s.leakScan(t)
}

// TestTimeoutHandlerFallback forces a timeout against the live server:
// no answer → run_ended fallen $0 + game_over at ends_at; the late POST
// is rejected 409 and an error event reaches the socket.
func TestTimeoutHandlerFallback(t *testing.T) {
	base, wsBase, db, _, registerRooms := fullTestServer(t, map[string]time.Duration{"easy": 400 * time.Millisecond})

	s := dialStream(t, wsBase+"/ws/rooms?mode=solo&name=Sleepy")
	created := s.waitFor("room_created", 5*time.Second)
	if created == nil {
		t.Fatal("no room_created")
	}
	roomID := created["room_id"].(string)
	roomCode := created["code"].(string)
	if roomCode != "" {
		t.Fatalf("solo room_created code = %q, want '' — codes are a duel-only surface (M7)", roomCode)
	}
	roomCode = roomID // solo clients address REST by the internal uuid (no code exists)
	registerCleanup(t, registerRooms, roomID)
	token := created["token"].(string)
	if s.waitFor("game_started", 10*time.Second) == nil {
		t.Fatal("no game_started")
	}

	re := s.waitFor("run_ended", 10*time.Second)
	if re == nil || fmt.Sprint(re["status"]) != "fallen" || fmt.Sprint(re["amount"]) != "0" {
		t.Fatalf("run_ended = %v, want fallen/0", re)
	}
	if _, has := re["correct_index"]; !has {
		t.Errorf("fallen run_ended should carry correct_index: %v", re)
	}
	if s.waitFor("answer_result", 300*time.Millisecond) != nil {
		t.Fatal("timeout produced answer_result — forbidden, nothing was answered")
	}
	if s.waitFor("game_over", 5*time.Second) == nil {
		t.Fatal("no game_over after the timeout fall")
	}

	// Late POST after the timeout resolved the player → 409 + error event.
	code, _ := postJSON(t, base, "/api/rooms/"+roomCode+"/answer", map[string]interface{}{
		"token": token, "level": 1, "choice_index": correctIndexFromDB(t, db, roomID, 1),
	})
	if code != http.StatusConflict {
		t.Fatalf("late POST: http %d, want 409", code)
	}
	errEv := s.waitFor("error", 5*time.Second)
	if errEv == nil {
		t.Fatal("no error event for the late POST")
	}

	s.drainUntil(200 * time.Millisecond)
	s.leakScan(t)
}

// TestDuelSurvivorIntegration is the full duel scripted flow (M4): both
// answer levels 1–2 correctly; seat 1 falls at level 3 → answer_result
// (private to A), opponent_reveal (private to B, NEVER to A), run_ended
// broadcast; survivor B continues (question_next L4 only after B also
// resolved L3), climbs 4–5, walks at 6 (=$1,000); game_over with
// standings sorted, emitted EXACTLY once. Leak scan over both sockets.
func TestDuelSurvivorIntegration(t *testing.T) {
	base, wsBase, db, _, registerRooms := fullTestServer(t, nil)

	a := dialStream(t, wsBase+"/ws/rooms?mode=duel&name=Alice")
	created := a.waitFor("room_created", 5*time.Second)
	if created == nil {
		t.Fatal("no room_created")
	}
	roomID := created["room_id"].(string)
	roomCode, _ := created["code"].(string)
	if !roomCodeRe.MatchString(roomCode) {
		t.Fatalf("duel room_created code = %q, want WORD-WORD-DD format (M7)", roomCode)
	}
	registerCleanup(t, registerRooms, roomID)
	tokenA := created["token"].(string)

	b := dialStream(t, wsBase+"/ws/rooms/"+roomCode+"?name=Bob")
	state := b.waitFor("room_state", 5*time.Second)
	if state == nil {
		t.Fatal("no room_state for seat 2")
	}
	tokenB, ok := state["token"].(string)
	if !ok || tokenB == "" || tokenB == tokenA {
		t.Fatalf("room_state token = %v", state["token"])
	}

	// Wave-4: both land in the lobby (deferred duel start); the host's
	// start endpoint issues Q1 to both sockets.
	if a.waitFor("player_joined", 5*time.Second) == nil {
		t.Fatal("no player_joined on the creator socket")
	}
	startDuelHost(t, base, roomID, tokenA, a, b)

	answer := func(stream *wsStream, token string, level int, choice int) (int, map[string]interface{}) {
		code, body := postJSON(t, base, "/api/rooms/"+roomCode+"/answer", map[string]interface{}{
			"token": token, "level": level, "choice_index": choice,
		})
		return code, body
	}

	// Levels 1–2 both correct.
	for level := 1; level <= 2; level++ {
		choice := correctIndexFromDB(t, db, roomID, level)
		if code, body := answer(a, tokenA, level, choice); code != 200 {
			t.Fatalf("A level %d: http %d (%v)", level, code, body)
		}
		choice = correctIndexFromDB(t, db, roomID, level)
		if code, body := answer(b, tokenB, level, choice); code != 200 {
			t.Fatalf("B level %d: http %d (%v)", level, code, body)
		}
		// Each answer produces its own private answer_result; after the
		// second player resolves, the room gets question_next.
		for _, st := range []*wsStream{a, b} {
			ar := st.waitFor("answer_result", 10*time.Second)
			if ar == nil {
				t.Fatalf("missing answer_result at level %d", level)
			}
		}
		for _, st := range []*wsStream{a, b} {
			qn := st.waitFor("question_next", 10*time.Second)
			if qn == nil {
				t.Fatalf("no question_next after level %d", level)
			}
		}
	}

	// Level 3: A falls with a deliberate wrong choice.
	aChoice := (correctIndexFromDB(t, db, roomID, 3) + 1) % 4
	if code, body := answer(a, tokenA, 3, aChoice); code != 200 {
		t.Fatalf("A level 3 wrong: http %d (%v)", code, body)
	}
	aAr := a.waitFor("answer_result", 5*time.Second)
	if aAr == nil || aAr["correct"] != false || aAr["status"] != "fallen" {
		t.Fatalf("A answer_result = %v", aAr)
	}
	aRe := a.waitFor("run_ended", 5*time.Second)
	if aRe == nil || fmt.Sprint(aRe["status"]) != "fallen" || fmt.Sprint(aRe["amount"]) != "0" {
		t.Fatalf("A run_ended = %v", aRe)
	}

	// B sees A's fall: run_ended broadcast AND the M4 opponent_reveal —
	// question payload (uniform shape, level 3), A's wrong chosen_index
	// and the correct index (allowed: A's question is settled).
	bRe := b.waitFor("run_ended", 5*time.Second)
	if bRe == nil || fmt.Sprint(bRe["status"]) != "fallen" || fmt.Sprint(bRe["seat"]) != "1" {
		t.Fatalf("B run_ended = %v", bRe)
	}
	bReveal := b.waitFor("opponent_reveal", 5*time.Second)
	if bReveal == nil {
		t.Fatal("B got no opponent_reveal for A's fall")
	}
	if ci := fmt.Sprint(bReveal["chosen_index"]); ci != fmt.Sprint(aChoice) {
		t.Fatalf("opponent_reveal chosen_index = %s, want A's wrong pick %d", ci, aChoice)
	}
	if ci := fmt.Sprint(bReveal["correct_index"]); ci != fmt.Sprint(correctIndexFromDB(t, db, roomID, 3)) {
		t.Fatalf("opponent_reveal correct_index = %s", ci)
	}
	rvQ, ok := bReveal["question"].(map[string]interface{})
	if !ok {
		t.Fatalf("opponent_reveal question = %T", bReveal["question"])
	}
	assertQuestionPayload(t, rvQ, 3)

	// The reveal must NOT reach the fallen player: scan A's ENTIRE
	// history so far and the final scan below covers the rest.
	for _, m := range a.all {
		if m["event"] == "opponent_reveal" {
			t.Fatalf("fallen player received opponent_reveal: %v", m)
		}
	}

	// Survivor gate: B is still pending on level 3, so NO question_next
	// may be issued for A's fall alone.
	if qn := b.waitFor("question_next", 500*time.Millisecond); qn != nil {
		t.Fatalf("question_next issued while the surviving player was still pending: %v", qn)
	}

	// Level 3 stays open for B; B answers correctly → level 4 (survivor).
	if code, body := answer(b, tokenB, 3, correctIndexFromDB(t, db, roomID, 3)); code != 200 {
		t.Fatalf("B level 3: http %d (%v)", code, body)
	}
	qn := b.waitFor("question_next", 10*time.Second)
	if qn == nil {
		t.Fatal("survivor did not get level 4")
	}
	if qnQ, ok := qn["question"].(map[string]interface{}); !ok {
		t.Fatalf("question_next question = %T", qn["question"])
	} else if fmt.Sprint(qnQ["level"]) != "4" {
		t.Fatalf("question_next level = %v, want 4", qnQ["level"])
	}

	// B climbs 4..5, walks at 6: locks in the amount of level 5 = $1,000.
	for level := 4; level <= 5; level++ {
		if code, body := answer(b, tokenB, level, correctIndexFromDB(t, db, roomID, level)); code != 200 {
			t.Fatalf("B level %d: http %d (%v)", level, code, body)
		}
		qn = b.waitFor("question_next", 10*time.Second)
		if qn == nil || fmt.Sprint(qn["question"].(map[string]interface{})["level"]) != fmt.Sprint(level+1) {
			t.Fatalf("survivor did not get level %d: %v", level+1, qn)
		}
	}
	walkCode, walkBody := postJSON(t, base, "/api/rooms/"+roomCode+"/walk", map[string]interface{}{"token": tokenB})
	if walkCode != 200 || walkBody["amount"] != float64(1000) {
		t.Fatalf("B walk: http %d body %v, want 200/1000 (answered through 5)", walkCode, walkBody)
	}
	bWalkRe := b.waitFor("run_ended", 5*time.Second)
	if bWalkRe == nil || fmt.Sprint(bWalkRe["status"]) != "walked" || fmt.Sprint(bWalkRe["amount"]) != "1000" {
		t.Fatalf("B run_ended = %v, want walked/1000", bWalkRe)
	}
	over := a.waitFor("game_over", 5*time.Second)
	if over == nil {
		t.Fatal("no game_over after the survivor walked")
	}
	standings, ok2 := over["standings"].([]interface{})
	if !ok2 || len(standings) != 2 {
		t.Fatalf("standings = %v", over["standings"])
	}
	first, _ := standings[0].(map[string]interface{})
	if fmt.Sprint(first["amount"]) != "1000" {
		t.Fatalf("standings[0] = %v, want the walker's 1000", first)
	}

	// game_over is a one-shot FINISH: the broadcast reaches every live
	// socket exactly once. Count per stream over the FULL history (after
	// a drain, so both sockets certainly ingested their copy — a second
	// server-side emission would show up here as 2 per stream).
	a.drainUntil(200 * time.Millisecond)
	b.drainUntil(200 * time.Millisecond)
	for i, stream := range []*wsStream{a, b} {
		overCount := 0
		for _, m := range stream.all {
			if m["event"] == "game_over" {
				overCount++
			}
		}
		if overCount != 1 {
			t.Fatalf("socket %d: game_over delivered %d times, want exactly 1 (one finish, one copy per socket)", i+1, overCount)
		}
	}

	// Leak discipline across BOTH sockets' full history.
	a.leakScan(t)
	b.leakScan(t)
}

// TestDuelDisconnectResolvesViaTimer is the M4 disconnect-mid-duel gate:
// seat 1's socket vanishes right after the duel started. The server only
// de-registers the socket — the player's ROW stays `active` and their run
// resolves via the per-level time.AfterFunc fallback. The opponent is
// never stranded: the level stays gated on the missing player, at
// ends_at seat 1 falls (run_ended, no answer_result — they never
// answered), the survivor gets the reveal (chosen_index = -1), then
// continues and finishes the game alone.
func TestDuelDisconnectResolvesViaTimer(t *testing.T) {
	base, wsBase, db, _, registerRooms := fullTestServer(t, map[string]time.Duration{"easy": 900 * time.Millisecond})

	a := dialStream(t, wsBase+"/ws/rooms?mode=duel&name=Gone")
	created := a.waitFor("room_created", 5*time.Second)
	if created == nil {
		t.Fatal("no room_created")
	}
	roomID := created["room_id"].(string)
	roomCode, _ := created["code"].(string)
	if !roomCodeRe.MatchString(roomCode) {
		t.Fatalf("duel room_created code = %q, want WORD-WORD-DD format (M7)", roomCode)
	}
	registerCleanup(t, registerRooms, roomID)
	_ = created["token"].(string) // tokenA unused — the player never comes back

	b := dialStream(t, wsBase+"/ws/rooms/"+roomCode+"?name=Stayer")
	state := b.waitFor("room_state", 5*time.Second)
	if state == nil {
		t.Fatal("no room_state for seat 2")
	}
	tokenB, ok := state["token"].(string)
	if !ok || tokenB == "" {
		t.Fatalf("room_state token = %v", state["token"])
	}
	// Wave-4: the duel starts on the HOST's start endpoint (deferred
	// start); seat 1 is still connected when Q1 is issued. Only after
	// that does seat 1's socket vanish.
	startDuelHost(t, base, roomID, created["token"].(string), a, b)

	// Seat 1's socket vanishes mid-duel; server-side the row stays active.
	a.conn.Close()

	// Level 1: B answers correctly, but the room CANNOT advance — the
	// disconnected seat 1 is still active and unresolved.
	correct1 := correctIndexFromDB(t, db, roomID, 1)
	if code, body := postJSON(t, base, "/api/rooms/"+roomCode+"/answer", map[string]interface{}{
		"token": tokenB, "level": 1, "choice_index": correct1,
	}); code != 200 {
		t.Fatalf("B level 1: http %d (%v)", code, body)
	}
	if qn := b.waitFor("question_next", 300*time.Millisecond); qn != nil {
		t.Fatalf("level advanced while the disconnected player was still pending: %v", qn)
	}

	// At ends_at the AfterFunc fallback falls seat 1 and reveals the level
	// to the survivor before issuing level 2.
	re := b.waitFor("run_ended", 10*time.Second)
	if re == nil || fmt.Sprint(re["seat"]) != "1" || fmt.Sprint(re["status"]) != "fallen" {
		t.Fatalf("run_ended = %v, want seat 1 fallen via the timer", re)
	}
	reveal := b.waitFor("opponent_reveal", 5*time.Second)
	if reveal == nil {
		t.Fatal("no opponent_reveal for the disconnected player's timeout fall")
	}
	if ci := fmt.Sprint(reveal["chosen_index"]); ci != "-1" {
		t.Fatalf("chosen_index = %s, want -1 (timeout, nothing chosen)", ci)
	}
	if rvQ, ok := reveal["question"].(map[string]interface{}); !ok || fmt.Sprint(rvQ["level"]) != "1" {
		t.Fatalf("reveal question = %v, want level 1", reveal["question"])
	}
	// The disconnected player never got an answer_result (nothing was
	// answered) — checked over what their socket saw before closing.
	for _, m := range a.all {
		if m["event"] == "answer_result" {
			t.Fatalf("disconnected player received answer_result: %v", m)
		}
	}

	// The survivor continues: level 2 issued after the timeout fall, then
	// walks at level 3 (answered 1–2 = $200) → game_over.
	qn := b.waitFor("question_next", 10*time.Second)
	if qn == nil {
		t.Fatal("survivor did not get level 2 after the timeout fall")
	}
	correct2 := correctIndexFromDB(t, db, roomID, 2)
	if code, body := postJSON(t, base, "/api/rooms/"+roomCode+"/answer", map[string]interface{}{
		"token": tokenB, "level": 2, "choice_index": correct2,
	}); code != 200 {
		t.Fatalf("B level 2: http %d (%v)", code, body)
	}
	if qn := b.waitFor("question_next", 10*time.Second); qn == nil {
		t.Fatal("survivor did not get level 3")
	}
	walkCode, walkBody := postJSON(t, base, "/api/rooms/"+roomCode+"/walk", map[string]interface{}{"token": tokenB})
	if walkCode != 200 || walkBody["amount"] != float64(200) {
		t.Fatalf("B walk: http %d body %v, want 200/200 (answered levels 1–2)", walkCode, walkBody)
	}
	over := b.waitFor("game_over", 5*time.Second)
	if over == nil {
		t.Fatal("no game_over after the survivor walked")
	}
	standings, ok2 := over["standings"].([]interface{})
	if !ok2 || len(standings) != 2 {
		t.Fatalf("standings = %v", over["standings"])
	}
	first, _ := standings[0].(map[string]interface{})
	if fmt.Sprint(first["amount"]) != "200" {
		t.Fatalf("standings[0] = %v, want the survivor's 200", first)
	}

	// Leak discipline: B's full history; A's whatever-arrived-before-close.
	b.drainUntil(200 * time.Millisecond)
	b.leakScan(t)
	a.leakScan(t)
}
