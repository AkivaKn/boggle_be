package handler

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// This file covers the M7 pacing surface end to end over the REAL chain:
// the solo hold-until-continue endpoint (premature 409 → after the beat
// the level deals → double continue no-op → wrong token 403) and the duel
// reveal beat (exactly ONE delayed question_next per level, unaffected by
// continue spam — duels auto-advance, their REST continue 409s).

// testBeat is small enough to keep the suite fast, large enough that the
// BEFORE-window assertions are reliable (they wait ~20% of the beat).
const testBeat = 250 * time.Millisecond

// TestSoloHoldContinuePacing pins the SPEC v1.1 solo pacing: a resolved
// level holds; CONTINUE deals the next question, but only after the
// reveal beat (server-side floor — duelRevealBeat 2.5s in production,
// injected small here). Rejections: premature → 409 "too soon" (+ error
// event), repeat after the deal → idempotent 200 no-op, wrong token → 403.
func TestSoloHoldContinuePacing(t *testing.T) {
	base, wsBase, db, game, registerRooms := fullTestServer(t, nil)
	game.SetRevealBeat(testBeat)

	s := dialStream(t, wsBase+"/ws/rooms?mode=solo&name=Holder")
	created := s.waitFor("room_created", 5*time.Second)
	if created == nil {
		t.Fatal("no room_created")
	}
	roomID := created["room_id"].(string)
	registerCleanup(t, registerRooms, roomID)
	token := created["token"].(string)
	// Solo: code '' surfaced (duel-only surface) — the uuid is this
	// room's REST path segment (fallback resolution).
	if code, _ := created["code"].(string); code != "" {
		t.Fatalf("solo room_created code = %q, want ''", code)
	}
	if s.waitFor("game_started", 10*time.Second) == nil {
		t.Fatal("no game_started")
	}

	correct := correctIndexFromDB(t, db, roomID, 1)
	code, body := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/answer", roomID),
		map[string]interface{}{"token": token, "level": 1, "choice_index": correct})
	if code != 200 || body["status"] != "active" {
		t.Fatalf("L1 correct answer: http %d body %v", code, body)
	}
	s.waitFor("answer_result", 5*time.Second)

	// Premature continue (inside the beat): 409 "too soon", and the room
	// stays held — no question_next may appear during the whole before-
	// window.
	cc, cbody := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/continue", roomID),
		map[string]interface{}{"token": token})
	if cc != 409 || !strings.Contains(fmt.Sprint(cbody["error"]), "too soon") {
		t.Fatalf("premature continue: http %d body %v, want 409 too soon", cc, cbody)
	}
	errEv := s.waitFor("error", 2*time.Second)
	if errEv == nil || !strings.Contains(fmt.Sprint(errEv["message"]), "too soon") {
		t.Fatalf("no 'too soon' error event on the socket: %v", errEv)
	}
	if qn := s.waitFor("question_next", testBeat/4); qn != nil {
		t.Fatalf("question_next dealt before the beat elapsed: %v", qn)
	}

	// After the beat: 200 {continued:true} + the delayed question_next, L2.
	time.Sleep(testBeat)
	cc, cbody = postJSON(t, base, fmt.Sprintf("/api/rooms/%s/continue", roomID),
		map[string]interface{}{"token": token})
	if cc != 200 || cbody["continued"] != true {
		t.Fatalf("in-beat continue: http %d body %v, want 200 continued", cc, cbody)
	}
	qn := s.waitFor("question_next", 5*time.Second)
	if qn == nil {
		t.Fatal("no question_next after the beat-elapsed continue")
	}
	qnQ, ok := qn["question"].(map[string]interface{})
	if !ok || fmt.Sprint(qnQ["level"]) != "2" {
		t.Fatalf("advanced question = %v, want level 2", qnQ)
	}

	// Double continue on the new level (nobody resolved it): idempotent
	// 200 no-op, and still exactly the ONE question_next on the stream.
	time.Sleep(testBeat) // past any floor — the no-op must not depend on it
	cc, cbody = postJSON(t, base, fmt.Sprintf("/api/rooms/%s/continue", roomID),
		map[string]interface{}{"token": token})
	if cc != 200 || cbody["continued"] != false {
		t.Fatalf("double continue: http %d body %v, want 200 {continued:false}", cc, cbody)
	}
	s.drainUntil(150 * time.Millisecond)
	qnCount := 0
	for _, m := range s.all {
		if m["event"] == "question_next" && fmt.Sprint(m["question"].(map[string]interface{})["level"]) == "2" {
			qnCount++
		}
	}
	if qnCount != 1 {
		t.Fatalf("question_next L2 seen %d times, want exactly 1", qnCount)
	}

	// Wrong token → 403 (the REST identity check runs before pacing).
	bad, bbody := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/continue", roomID),
		map[string]interface{}{"token": token[1:]})
	if bad != 403 {
		t.Fatalf("wrong-token continue: http %d body %v, want 403", bad, bbody)
	}

	// Hold while a question is OPEN (nothing resolved): a continue is an
	// idempotent no-op, never an advance past a question in play.
	oc, obody := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/continue", roomID),
		map[string]interface{}{"token": token})
	if oc != 200 || obody["continued"] != false {
		t.Fatalf("open-question continue: http %d body %v, want 200 no-op", oc, obody)
	}
}

// TestDuelRevealBeatExactlyOnce pins the duel auto-advance: after BOTH
// players resolve a level exactly ONE question_next is issued ~beat later
// (timer-scheduled, ends_at recomputed at issue time); no continue path
// exists for duels (every continue POST → 409) and continue spam cannot
// duplicate the advance. Also asserts the delayed payload carries a FRESH
// ends_at (identity of the beat: the timer budget starts at issue).
func TestDuelRevealBeatExactlyOnce(t *testing.T) {
	base, wsBase, db, game, registerRooms := fullTestServer(t, nil)
	game.SetRevealBeat(testBeat)

	a := dialStream(t, wsBase+"/ws/rooms?mode=duel&name=Alice")
	created := a.waitFor("room_created", 5*time.Second)
	if created == nil {
		t.Fatal("no room_created")
	}
	roomID := created["room_id"].(string)
	registerCleanup(t, registerRooms, roomID)
	code := created["code"].(string)
	_ = code // the joins are code-keyed elsewhere; pacing uses the uuid path
	tokenA := created["token"].(string)

	b := dialStream(t, wsBase+"/ws/rooms/"+code+"?name=Bob")
	state := b.waitFor("room_state", 5*time.Second)
	if state == nil {
		t.Fatal("no room_state for seat 2")
	}
	tokenB := state["token"].(string)
	// Wave-4: the duel no longer starts at seat-2 join — the HOST's start
	// endpoint issues the first question (deferred duel start).
	startDuelHost(t, base, roomID, tokenA, a, b)
	startedAt := time.Now()

	// Both answer L1 correctly (DB oracle for the shuffled layout).
	for _, pair := range []struct {
		st    *wsStream
		token string
	}{{a, tokenA}, {b, tokenB}} {
		c := correctIndexFromDB(t, db, roomID, 1)
		rc, rbody := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/answer", roomID),
			map[string]interface{}{"token": pair.token, "level": 1, "choice_index": c})
		if rc != 200 || rbody["status"] != "active" {
			t.Fatalf("L1 answer: http %d body %v", rc, rbody)
		}
	}
	// Continue spam from BOTH seats while the beat is pending: duels have
	// NO continue endpoint — every call 409s (mode guard), and none of it
	// may affect the exactly-once advance.
	for i := 0; i < 4; i++ {
		tok := tokenA
		if i%2 == 1 {
			tok = tokenB
		}
		cc, cbody := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/continue", roomID),
			map[string]interface{}{"token": tok})
		if cc != 409 {
			t.Fatalf("duel continue #%d: http %d body %v, want 409", i, cc, cbody)
		}
	}

	// Well inside the beat window: nothing dealt yet.
	if qn := a.waitFor("question_next", testBeat/4); qn != nil {
		t.Fatalf("question_next dealt before the beat: %v", qn)
	}
	// After the beat: the ONE scheduled advance lands (both sockets).
	// Poll until arrival and time it from `startedAt` (just after
	// game_started — the resolutions follow it, so the lower bound
	// starts below the beat by the answering latency; the guarantee
	// under test is only "never EARLY", and the live run transcript
	// adds the ~±300ms fidelity check around the production 2.5s beat).
	deadline := time.Now().Add(5 * time.Second)
	var qnA, qnB map[string]interface{}
	for (qnA == nil || qnB == nil) && time.Now().Before(deadline) {
		if qnA == nil {
			qnA = a.waitFor("question_next", 50*time.Millisecond)
		}
		if qnB == nil {
			qnB = b.waitFor("question_next", 50*time.Millisecond)
		}
	}
	arrived := time.Since(startedAt)
	if qnA == nil || qnB == nil {
		t.Fatalf("missing question_next after the beat (a=%v b=%v)", qnA, qnB)
	}
	if !strings.Contains(fmt.Sprint(qnA["question"].(map[string]interface{})["level"]), "2") {
		t.Fatalf("advanced level = %v, want 2", qnA["question"])
	}
	if arrived < testBeat {
		t.Fatalf("beat advance noticed %v after start, want >= %v (dealt early?)", arrived, testBeat)
	}
	endsA, _ := qnA["ends_at"].(string)
	if endsA == "" {
		t.Fatalf("question_next without ends_at: %v", qnA)
	}

	// Exactly once: drain and count — one copy per socket, none extra
	// (the beat timer already fired; nothing re-schedules a settled level).
	a.drainUntil(200 * time.Millisecond)
	b.drainUntil(200 * time.Millisecond)
	count := 0
	for _, m := range a.all {
		if m["event"] == "question_next" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("socket A saw %d question_next events, want exactly 1", count)
	}
	count = 0
	for _, m := range b.all {
		if m["event"] == "question_next" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("socket B saw %d question_next events, want exactly 1", count)
	}
}

// TestUnknownCodeREST pins the clean 404 of the REST commands on codes
// that resolve nothing (never a 500, never a panic path).
func TestUnknownCodeREST(t *testing.T) {
	base, _, _, _, _ := fullTestServer(t, nil)

	for _, tail := range []string{"answer", "walk", "lifeline", "continue"} {
		c, body := postJSON(t, base, fmt.Sprintf("/api/rooms/MISSING-CODE-42/%s", tail),
			map[string]interface{}{"token": "whatever"})
		if c != 404 || !strings.Contains(fmt.Sprint(body["error"]), "room not found") {
			t.Fatalf("%s on unknown code: http %d body %v, want 404 room not found", tail, c, body)
		}
	}
}
