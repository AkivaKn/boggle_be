package handler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vingarcia/ksql"
)

// This file covers the wave-4 DEFERRED DUEL START end to end over the
// REAL chain: a seat-2 join no longer deals anything (no room_questions,
// rooms.status stays 'lobby'), the host's POST /start issues Q1 exactly
// like the pre-wave-4 auto-start did, a joiner's start is a 403 with an
// error event, a double start is an idempotent no-op, solo rooms 409.

// startDuelHost is the shared wave-4 test helper: the host (seat 1, from
// room_created) POSTs /start and both sockets wait for game_started.
// Fails the test on anything but a fresh 200 {started:true}.
func startDuelHost(t *testing.T, base string, roomID, tokenA string, a, b *wsStream) {
	t.Helper()
	code, body := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/start", roomID),
		map[string]interface{}{"token": tokenA})
	if code != 200 || body["started"] != true {
		t.Fatalf("host start: http %d body %v, want 200 {started:true}", code, body)
	}
	q, ok := body["question"].(map[string]interface{})
	if !ok {
		t.Fatalf("start response without the question mirror: %v", body)
	}
	assertQuestionPayload(t, q, 1)
	if _, has := body["ends_at"]; !has {
		t.Fatalf("start response without ends_at: %v", body)
	}
	for i, st := range []*wsStream{a, b} {
		if gs := st.waitFor("game_started", 10*time.Second); gs == nil {
			t.Fatalf("socket %d: no game_started after the host start", i+1)
		}
	}
}

// duelLobby dials a fresh duel room, joins seat 2 by code, and returns
// both streams and identities with the room left strictly in the lobby.
func duelLobby(t *testing.T, base, wsBase string, registerRooms func(ids ...string)) (a, b *wsStream, roomID, roomCode, tokenA, tokenB string) {
	t.Helper()
	a = dialStream(t, wsBase+"/ws/rooms?mode=duel&name=Alice")
	created := a.waitFor("room_created", 5*time.Second)
	if created == nil {
		t.Fatal("no room_created")
	}
	roomID = created["room_id"].(string)
	registerCleanup(t, registerRooms, roomID)
	roomCode, _ = created["code"].(string)
	if !roomCodeRe.MatchString(roomCode) {
		t.Fatalf("duel room_created code = %q, want WORD-WORD-DD format (M7)", roomCode)
	}
	tokenA = created["token"].(string)

	b = dialStream(t, wsBase+"/ws/rooms/"+roomCode+"?name=Bob")
	state := b.waitFor("room_state", 5*time.Second)
	if state == nil {
		t.Fatal("no room_state for seat 2")
	}
	tokenB, ok := state["token"].(string)
	if !ok || tokenB == "" || tokenB == tokenA {
		t.Fatalf("room_state token = %v", state["token"])
	}
	if b.waitFor("player_joined", 5*time.Second) == nil {
		t.Fatal("no player_joined on the joiner socket")
	}
	if a.waitFor("player_joined", 5*time.Second) == nil {
		t.Fatal("no player_joined on the creator socket")
	}
	return a, b, roomID, roomCode, tokenA, tokenB
}

// expectNothingFor is a negative window: no game_started may arrive and
// none may sit in the drained history (pins the DEFERRED start).
func (s *wsStream) expectNothingFor(t *testing.T, within time.Duration, why string) {
	t.Helper()
	if ev := s.waitFor("game_started", within); ev != nil {
		t.Fatalf("%s: game_started arrived: %v", why, ev)
	}
	s.drainUntil(50 * time.Millisecond)
	for _, m := range s.all {
		if m["event"] == "game_started" || m["event"] == "question_next" {
			t.Fatalf("%s: unexpected %v in the stream", why, m)
		}
	}
}

// roomStatusOf reads the room's status straight from the DB.
func roomStatusOf(t *testing.T, db ksql.DB, roomID string) (string, error) {
	t.Helper()
	var row struct {
		Status string `ksql:"status"`
	}
	if err := db.QueryOne(context.Background(), &row,
		"SELECT status FROM millionaire_rooms WHERE id = $1", roomID); err != nil {
		return "", err
	}
	return row.Status, nil
}

// roomQuestionCount counts the room's persisted layout rows.
func roomQuestionCount(t *testing.T, db ksql.DB, roomID string) int {
	t.Helper()
	var row struct {
		N int `ksql:"n"`
	}
	if err := db.QueryOne(context.Background(), &row,
		"SELECT count(*) AS n FROM millionaire_room_questions WHERE room_id = $1", roomID); err != nil {
		t.Fatalf("counting room_questions: %v", err)
	}
	return row.N
}

// TestDuelDeferredStart pins the wave-4 lobby semantics: seat-2 join →
// player_joined + room_state, NO game_started, room stays 'lobby' with
// zero room_questions rows; joiner start → 403 + error event to that
// socket; host start → 200 {started:true, Q1+ends_at} + game_started on
// both sockets + room 'active'; double start → 200 {started:false} with
// still exactly ONE game_started per socket.
func TestDuelDeferredStart(t *testing.T) {
	base, wsBase, db, _, registerRooms := fullTestServer(t, nil)
	a, b, roomID, roomCode, tokenA, tokenB := duelLobby(t, base, wsBase, registerRooms)

	// The waiting flag: rooms.status stays 'lobby' and NOTHING was dealt.
	if room, err := roomStatusOf(t, db, roomID); err != nil || room != "lobby" {
		t.Fatalf("room status after seat-2 join = %q (%v), want 'lobby' (deferred start)", room, err)
	}
	if n := roomQuestionCount(t, db, roomID); n != 0 {
		t.Fatalf("seat-2 join dealt %d room_questions rows, want 0", n)
	}
	a.expectNothingFor(t, 400*time.Millisecond, "deferred start (creator socket)")
	b.expectNothingFor(t, 400*time.Millisecond, "deferred start (joiner socket)")

	// Joiner (seat 2) start → 403 + error event to seat 2's socket only.
	code, body := postJSON(t, base, fmt.Sprintf("/api/rooms/%s/start", roomCode),
		map[string]interface{}{"token": tokenB})
	if code != 403 || !strings.Contains(fmt.Sprint(body["error"]), "host") {
		t.Fatalf("joiner start: http %d body %v, want 403 host-only", code, body)
	}
	errEv := b.waitFor("error", 5*time.Second)
	if errEv == nil || !strings.Contains(fmt.Sprint(errEv["message"]), "host") {
		t.Fatalf("no host-only error event on the joiner socket: %v", errEv)
	}
	for _, m := range a.all {
		if m["event"] == "error" {
			t.Fatalf("the joiner's 403 leaked an error event to seat 1: %v", m)
		}
	}
	if room, _ := roomStatusOf(t, db, roomID); room != "lobby" {
		t.Fatalf("rejected start must not flip the room: %q", room)
	}

	// Host start → live game.
	startDuelHost(t, base, roomID, tokenA, a, b)
	if room, _ := roomStatusOf(t, db, roomID); room != "active" {
		t.Fatalf("room status after host start = %q, want 'active'", room)
	}
	if n := roomQuestionCount(t, db, roomID); n != 30 {
		t.Fatalf("room_questions after start = %d rows, want 30", n)
	}

	// Double start → idempotent 200 no-op, one game_started per socket.
	code, body = postJSON(t, base, fmt.Sprintf("/api/rooms/%s/start", roomCode),
		map[string]interface{}{"token": tokenA})
	if code != 200 || body["started"] != false {
		t.Fatalf("double start: http %d body %v, want 200 {started:false}", code, body)
	}
	a.drainUntil(200 * time.Millisecond)
	b.drainUntil(200 * time.Millisecond)
	for i, st := range []*wsStream{a, b} {
		count := 0
		for _, m := range st.all {
			if m["event"] == "game_started" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("socket %d saw %d game_started events, want exactly 1", i+1, count)
		}
	}
}

// TestStartEndpointValidation covers the start endpoint's rule table:
// unknown code → 404; bad token → 403; solo room → 409 (solo is
// instant-play and auto-starts on creation, unchanged); a duel whose
// seat 2 has not joined → 409 (never half-full).
func TestStartEndpointValidation(t *testing.T) {
	base, wsBase, _, _, registerRooms := fullTestServer(t, nil)

	// Unknown code.
	code, body := postJSON(t, base, "/api/rooms/MISSING-CODE-42/start",
		map[string]interface{}{"token": "whatever"})
	if code != 404 || !strings.Contains(fmt.Sprint(body["error"]), "room not found") {
		t.Fatalf("start on unknown code: http %d body %v, want 404", code, body)
	}

	// Solo room: instant-play already started it — start → 409.
	s := dialStream(t, wsBase+"/ws/rooms?mode=solo&name=SoloStarter")
	created := s.waitFor("room_created", 5*time.Second)
	if created == nil {
		t.Fatal("no room_created")
	}
	roomID := created["room_id"].(string)
	registerCleanup(t, registerRooms, roomID)
	token := created["token"].(string)
	if s.waitFor("game_started", 10*time.Second) == nil {
		t.Fatal("solo should still auto-start on creation")
	}
	code, body = postJSON(t, base, fmt.Sprintf("/api/rooms/%s/start", roomID),
		map[string]interface{}{"token": token})
	if code != 409 || !strings.Contains(fmt.Sprint(body["error"]), "solo") {
		t.Fatalf("solo start: http %d body %v, want 409 (instant-play, nothing to start)", code, body)
	}
	if errEv := s.waitFor("error", 5*time.Second); errEv == nil {
		t.Fatal("no error event on the solo-start rejection")
	}

	// Duel with only the host seated → 409 waiting-for-seat-2.
	d := dialStream(t, wsBase+"/ws/rooms?mode=duel&name=Lonely")
	dcreated := d.waitFor("room_created", 5*time.Second)
	if dcreated == nil {
		t.Fatal("no room_created")
	}
	dRoomID := dcreated["room_id"].(string)
	registerCleanup(t, registerRooms, dRoomID)
	dToken := dcreated["token"].(string)
	code, body = postJSON(t, base, fmt.Sprintf("/api/rooms/%s/start", dRoomID),
		map[string]interface{}{"token": dToken})
	if code != 409 || !strings.Contains(fmt.Sprint(body["error"]), "second player") {
		t.Fatalf("half-full duel start: http %d body %v, want 409 waiting for seat 2", code, body)
	}
	d.expectNothingFor(t, 300*time.Millisecond, "rejected half-full start")

	// Bad token → 403 (before any game-rule evaluation).
	code, body = postJSON(t, base, fmt.Sprintf("/api/rooms/%s/start", dRoomID),
		map[string]interface{}{"token": "bogus-token"})
	if code != 403 {
		t.Fatalf("bad-token start: http %d body %v, want 403", code, body)
	}
}
