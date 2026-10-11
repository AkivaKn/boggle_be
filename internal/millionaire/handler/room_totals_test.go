package handler

import (
	"context"
	"testing"
	"time"
)

// This file covers the wave-4B read API: the room_state snapshot carries
// the room's per-seat SESSION totals (`totals[]` — public facts) ONLY
// when the room has history (any room_totals row), and the duel LOBBY
// (pre-start) carries them too so the FE rematch card can render before
// the host starts the rematch (FE consumer contract: wave-5c).

// TestRoomStateCarriesSessionTotals drives the REAL join/rejoin chain: a
// 0-runs lobby room_state has NO totals key; once a totals row exists
// (seeded here straight into the DB — the accumulation path itself is the
// service suites' job) a token rejoin's room_state carries the exact
// per-seat block.
func TestRoomStateCarriesSessionTotals(t *testing.T) {
	base, wsBase, db, _, registerRooms := fullTestServer(t, nil)
	a, b, roomID, roomCode, tokenA, _ := duelLobby(t, base, wsBase, registerRooms)

	// 0-runs room: seat 2's lobby room_state must NOT carry `totals`
	// (omitted entirely — never a blank array).
	for _, m := range b.all {
		if m["event"] == "room_state" {
			if _, has := m["totals"]; has {
				t.Fatalf("0-runs lobby room_state carried totals: %v", m)
			}
		}
	}

	// Seed one seat's census straight into the DB (three completed runs,
	// one won, £1,500 total, best £500 at level 9).
	if _, err := db.Exec(context.Background(),
		"INSERT INTO room_totals (id, room_id, seat, games_played, games_won, total_money, best_level, best_amount) "+
			"VALUES ('11111111-1111-4111-8111-111111111111', $1, 1, 3, 1, 1500, 9, 500)",
		roomID); err != nil {
		t.Fatalf("seeding a room_totals row: %v", err)
	}

	// Rejoin seat 1 by token → the snapshot carries the totals block.
	// (The rejoin replaces seat 1's live socket — kick-on-rejoin, M2.)
	_ = a
	re := dialStream(t, wsBase+"/ws/rooms/"+roomCode+"?token="+tokenA)
	state := re.waitFor("room_state", 5*time.Second)
	if state == nil {
		t.Fatal("no room_state on the rejoin")
	}
	totals, ok := state["totals"].([]interface{})
	if !ok {
		t.Fatalf("rejoin room_state totals = %T (%v), want an array", state["totals"], state["totals"])
	}
	if len(totals) != 1 {
		t.Fatalf("totals = %d blocks, want 1 (only the seeded seat 1)", len(totals))
	}
	block, ok := totals[0].(map[string]interface{})
	if !ok {
		t.Fatalf("totals[0] = %T, want an object", totals[0])
	}
	if block["seat"] != float64(1) {
		t.Fatalf("totals seat = %v, want 1", block["seat"])
	}
	if block["games_played"] != float64(3) {
		t.Fatalf("totals games_played = %v, want 3", block["games_played"])
	}
	if block["games_won"] != float64(1) {
		t.Fatalf("totals games_won = %v, want 1", block["games_won"])
	}
	if block["total_money"] != float64(1500) {
		t.Fatalf("totals total_money = %v, want 1500", block["total_money"])
	}
	if block["best_level"] != float64(9) {
		t.Fatalf("totals best_level = %v, want 9", block["best_level"])
	}
	if block["best_amount"] != float64(500) {
		t.Fatalf("totals best_amount = %v, want 500", block["best_amount"])
	}
}
