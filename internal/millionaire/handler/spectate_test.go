package handler

import (
	"fmt"
	"testing"
	"time"
)

// This file covers the wave-4 SPECTATOR PROGRESS broadcasts over the REAL
// chain: once a seat lapses (fallen), the SURVIVOR's visible moves — each
// correct answer and each lifeline usage — emit
//
//	opponent_progress {seat, level, amount, status: "active"}
//
// PRIVATELY to the lapsed seat's socket only. The survivor's own socket
// never sees the event, the data is public-fact only (never
// correct_index), and it lands after the resolution but BEFORE the
// advance's question_next.

// TestOpponentProgressToLapsedSeat: seat 1 falls at L1; seat 2 (the
// survivor) answers L1, answers L2 and uses fifty_fifty on L3 — every one
// of those moves emits exactly one opponent_progress to seat ONE's socket
// and NONE to seat 2's; ordering places it before each question_next.
func TestOpponentProgressToLapsedSeat(t *testing.T) {
	base, wsBase, db, _, registerRooms := fullTestServer(t, nil)
	a, b, roomID, roomCode, tokenA, tokenB := duelLobby(t, base, wsBase, registerRooms)
	startDuelHost(t, base, roomID, tokenA, a, b)

	answer := func(st *wsStream, token string, level, choice int) (int, map[string]interface{}) {
		return postJSON(t, base, fmt.Sprintf("/api/rooms/%s/answer", roomCode),
			map[string]interface{}{"token": token, "level": level, "choice_index": choice})
	}

	// Seat 1 falls at L1 (deliberate wrong pick).
	c1 := correctIndexFromDB(t, db, roomID, 1)
	if rc, rbody := answer(a, tokenA, 1, (c1+1)%4); rc != 200 || rbody["status"] != "fallen" {
		t.Fatalf("A L1 wrong: http %d body %v", rc, rbody)
	}
	a.waitFor("run_ended", 5*time.Second)
	b.waitFor("run_ended", 5*time.Second) // broadcast copy on the survivor

	// Survivor answers L1 correctly → the lapsed seat gets the progress
	// update {seat:2, level:1, amount:100, status:active}.
	c1 = correctIndexFromDB(t, db, roomID, 1)
	if rc, rbody := answer(b, tokenB, 1, c1); rc != 200 || rbody["status"] != "active" {
		t.Fatalf("B L1: http %d body %v", rc, rbody)
	}
	prog := a.waitFor("opponent_progress", 5*time.Second)
	if prog == nil {
		t.Fatal("fallen socket got no opponent_progress after the survivor's answer")
	}
	if fmt.Sprint(prog["seat"]) != "2" || fmt.Sprint(prog["level"]) != "1" ||
		fmt.Sprint(prog["amount"]) != "100" || fmt.Sprint(prog["status"]) != "active" {
		t.Fatalf("opponent_progress = %v, want {seat:2, level:1, amount:100, status:active}", prog)
	}
	if _, has := prog["correct_index"]; has {
		t.Fatalf("opponent_progress leaks correct_index: %v", prog)
	}

	// Ordering: the progress lands BEFORE the advance's question_next.
	// (Resolution and advance are two dispatches — wait for the advance
	// to arrive before comparing indices.)
	qn := a.waitFor("question_next", 5*time.Second)
	if qn == nil {
		t.Fatal("survivor's L1 advance never dealt the next question")
	}
	progIdx, qnIdx := -1, -1
	for i, m := range a.all {
		switch m["event"] {
		case "opponent_progress":
			progIdx = i
		case "question_next":
			if qnIdx == -1 {
				qnIdx = i
			}
		}
	}
	if progIdx > qnIdx {
		t.Fatalf("opponent_progress (idx %d) arrived after question_next (idx %d)", progIdx, qnIdx)
	}

	// The survivor's own socket never receives the event.
	b.drainUntil(150 * time.Millisecond)
	if n := countEvent(b, "opponent_progress"); n != 0 {
		t.Fatalf("survivor socket received %d opponent_progress events, want 0", n)
	}

	// Survivor answers L2 → another progress update {level:2, amount:200}
	// (the amount is the ladder rung of the answered level, not a sum).
	c2 := correctIndexFromDB(t, db, roomID, 2)
	if rc, rbody := answer(b, tokenB, 2, c2); rc != 200 {
		t.Fatalf("B L2: http %d (%v)", rc, rbody)
	}
	prog = a.waitFor("opponent_progress", 5*time.Second)
	if prog == nil || fmt.Sprint(prog["seat"]) != "2" || fmt.Sprint(prog["level"]) != "2" ||
		fmt.Sprint(prog["amount"]) != "200" {
		t.Fatalf("second opponent_progress = %v, want {seat:2, level:2, amount:200, active}", prog)
	}

	// Lifeline usage by the survivor on L4 (in play) → the lifeline's own
	// progress update. NOTE: B's L3 CORRECT ANSWER also emits a progress
	// event ({level:3, amount:300}) — consume that one FIRST, then the
	// lifeline's {level:4, amount:300} (the secured rung on the level in
	// play).
	c3 := correctIndexFromDB(t, db, roomID, 3)
	if rc, rbody := answer(b, tokenB, 3, c3); rc != 200 {
		t.Fatalf("B L3: http %d (%v)", rc, rbody)
	}
	a.waitFor("question_next", 5*time.Second) // L4 issued before the lifeline
	prog = a.waitFor("opponent_progress", 5*time.Second)
	if prog == nil || fmt.Sprint(prog["seat"]) != "2" || fmt.Sprint(prog["level"]) != "3" ||
		fmt.Sprint(prog["amount"]) != "300" {
		t.Fatalf("L3-answer opponent_progress = %v, want {seat:2, level:3, amount:300, active}", prog)
	}
	if rc, rbody := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/lifeline", roomCode),
		map[string]interface{}{"token": tokenB, "lifeline": "fifty_fifty"}); rc != 200 {
		t.Fatalf("B fifty_fifty: http %d (%v)", rc, rbody)
	}
	if ev := a.waitFor("lifeline_used", 5*time.Second); ev == nil {
		t.Fatal("lifeline_used broadcast missing for the fallen spectator")
	}
	prog = a.waitFor("opponent_progress", 5*time.Second)
	if prog == nil || fmt.Sprint(prog["seat"]) != "2" || fmt.Sprint(prog["level"]) != "4" ||
		fmt.Sprint(prog["amount"]) != "300" {
		t.Fatalf("lifeline opponent_progress = %v, want {seat:2, level:4, amount:300, active}", prog)
	}

	// Everything the fallen spectator ever saw stays leak-free, and the
	// survivor's socket still carries zero copies of the event.
	a.drainUntil(200 * time.Millisecond)
	b.drainUntil(200 * time.Millisecond)
	a.leakScan(t)
	b.leakScan(t)
	if n := countEvent(b, "opponent_progress"); n != 0 {
		t.Fatalf("survivor socket received %d opponent_progress events in total, want 0", n)
	}
	if n := countEvent(a, "opponent_progress"); n != 4 {
		t.Fatalf("fallen socket received %d opponent_progress events, want 4 (answer L1, answer L2, answer L3, lifeline L4)", n)
	}
}
