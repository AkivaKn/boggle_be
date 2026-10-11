package service

import (
	"context"
	"testing"
	"time"

	"boggle-api/internal/millionaire/models"
	"boggle-api/internal/millionaire/repository"
)

// This file covers the wave-4B PER-ROOM SESSION TOTALS at service level:
// every completed run (game_over) upserts each resolved seat's census
// into room_totals — games_played/games_won/total_money sum across runs,
// best_level/best_amount take the greater-of — and a rematch NEVER resets
// them (they accrete; only the room row's deletion cascade removes them).
// Hermetic against the dev DB: rooms are cleaned up in createTestRoom's
// teardown (and the totals rows cascade with them).

// assertTotalsAre pins the full per-seat census for one seat.
func assertTotalsAre(t *testing.T, ctx context.Context, totals repository.RoomTotalsRepository, roomID string, seat int, want models.RoomTotal) {
	t.Helper()
	rows, err := totals.GetRoomTotals(ctx, roomID)
	if err != nil {
		t.Fatalf("loading room_totals (room %s): %v", roomID, err)
	}
	var got *models.RoomTotal
	for i := range rows {
		if rows[i].Seat == seat {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatalf("room_totals has no seat %d row (room %s): %v", seat, roomID, rows)
	}
	if got.GamesPlayed != want.GamesPlayed || got.GamesWon != want.GamesWon ||
		got.TotalMoney != want.TotalMoney || got.BestLevel != want.BestLevel ||
		got.BestAmount != want.BestAmount {
		t.Fatalf("seat %d totals = {played %d, won %d, money %d, bestLevel %d, bestAmount %d}, want {played %d, won %d, money %d, bestLevel %d, bestAmount %d}",
			seat, got.GamesPlayed, got.GamesWon, got.TotalMoney, got.BestLevel, got.BestAmount,
			want.GamesPlayed, want.GamesWon, want.TotalMoney, want.BestLevel, want.BestAmount)
	}
}

// TestRoomTotalsAccumulateAcrossRematches drives ONE duel room through two
// completions (two full plays):
//
//	pass 1 — seat 1 answers L1 correctly, climbs and WALKS at L2 (£100);
//	         seat 2 falls at L1 (£0) → game_over
//	replay — players reset (active/0/0) but room_totals UNTOUCHED
//	pass 2 — seat 2 answers L1 correctly and WALKS at L2 (£100);
//	         seat 1 falls at L1 (£0) → game_over
//
// Math pinned (DB blocks, seat-ordered): games_played 2 per seat,
// games_won 0 (nobody won), total_money 100 for BOTH seats (sum of the
// two runs: seat 1 £100+£0, seat 2 £0+£100), best_amount 100 and
// best_level 1 for both (greater-of across runs).
func TestRoomTotalsAccumulateAcrossRematches(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, _, rooms, players := newTestGame(t, db)
	totals := repository.NewRoomTotalsRepository(db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "duel", 2)
	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatalf("StartGame: %v", err)
	}

	// --- Pass 1 -------------------------------------------------------
	// Seat 2 falls at L1 (£0, level_reached stays 0).
	if lvl := fallSeat(t, ctx, g, players, room.ID, ps[1]); lvl != 1 {
		t.Fatalf("pass1: seat 2 fell at level %d, want 1", lvl)
	}
	// Seat 1 answers L1 correctly, then walks at L2 (£100 locked).
	fresh1 := reloadPlayer(t, ctx, players, ps[0])
	correct := shuffledCorrectIndex(t, g, room.ID, 1)
	if _, err := g.Answer(ctx, room.ID, &fresh1, 1, correct); err != nil {
		t.Fatalf("pass1: seat 1 correct L1: %v", err)
	}
	if got := walkNow(t, ctx, g, players, room.ID, ps[0]); got != 100 {
		t.Fatalf("pass1: seat 1 walk locked %d, want 100 (L1 answered)", got)
	}
	// One game_over → exactly one accumulation pass: the census exists
	// and matches pass 1 exactly.
	assertTotalsAre(t, ctx, totals, room.ID, 1,
		models.RoomTotal{GamesPlayed: 1, GamesWon: 0, TotalMoney: 100, BestLevel: 1, BestAmount: 100})
	assertTotalsAre(t, ctx, totals, room.ID, 2,
		models.RoomTotal{GamesPlayed: 1, GamesWon: 0, TotalMoney: 0, BestLevel: 0, BestAmount: 0})
	if rows, err := totals.GetRoomTotals(ctx, room.ID); err != nil || len(rows) != 2 {
		t.Fatalf("room_totals rows = %v (%v), want exactly 2 (per-seat)", rows, err)
	}

	// --- Replay -------------------------------------------------------
	// The rematch resets the PLAYERS (wave-4) but must NOT touch
	// room_totals: same census before and after the reset.
	if replayed, started, err := g.Replay(ctx, room, &ps[0]); err != nil || !replayed || started {
		t.Fatalf("Replay = (%v, %v, %v), want (true, false, nil)", replayed, started, err)
	}
	fresh := reloadPlayer(t, ctx, players, ps[0])
	if fresh.Status != "active" || fresh.Amount != 0 || fresh.LevelReached != 0 {
		t.Fatalf("replay did not reset seat 1: %d/%d/%s", fresh.LevelReached, fresh.Amount, fresh.Status)
	}
	assertTotalsAre(t, ctx, totals, room.ID, 1,
		models.RoomTotal{GamesPlayed: 1, GamesWon: 0, TotalMoney: 100, BestLevel: 1, BestAmount: 100})
	assertTotalsAre(t, ctx, totals, room.ID, 2,
		models.RoomTotal{GamesPlayed: 1, GamesWon: 0, TotalMoney: 0, BestLevel: 0, BestAmount: 0})

	// --- Pass 2 (roles flipped: seats must accrete independently) -----
	// The duel rematch waits in the lobby (deferred start, wave 4): the
	// HOST re-opens it (StartDuel) before the second run can play.
	if started, err := g.StartDuel(ctx, room, &ps[0]); err != nil || !started {
		t.Fatalf("rematch StartDuel = (%v, %v), want (true, nil)", started, err)
	}
	if lvl := fallSeat(t, ctx, g, players, room.ID, ps[0]); lvl != 1 {
		t.Fatalf("pass2: seat 1 fell at level %d, want 1", lvl)
	}
	fresh2 := reloadPlayer(t, ctx, players, ps[1])
	correct2 := shuffledCorrectIndex(t, g, room.ID, 1)
	if _, err := g.Answer(ctx, room.ID, &fresh2, 1, correct2); err != nil {
		t.Fatalf("pass2: seat 2 correct L1: %v", err)
	}
	if got := walkNow(t, ctx, g, players, room.ID, ps[1]); got != 100 {
		t.Fatalf("pass2: seat 2 walk locked %d, want 100", got)
	}
	// BOTH completions × BOTH seats, summed / greater-of:
	assertTotalsAre(t, ctx, totals, room.ID, 1,
		models.RoomTotal{GamesPlayed: 2, GamesWon: 0, TotalMoney: 100, BestLevel: 1, BestAmount: 100})
	assertTotalsAre(t, ctx, totals, room.ID, 2,
		models.RoomTotal{GamesPlayed: 2, GamesWon: 0, TotalMoney: 100, BestLevel: 1, BestAmount: 100})
}

// TestRoomTotalsSoloAccretes is the solo variant: seat 1 only, session
// totals across a solo rematch (walk pass + fall pass).
func TestRoomTotalsSoloAccretes(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, _, rooms, players := newTestGame(t, db)
	totals := repository.NewRoomTotalsRepository(db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "solo", 1)
	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatalf("StartGame: %v", err)
	}

	// Pass 1: answer L1 correctly (solo holds), CONTINUE to L2 (the beat
	// floor is 0 in this harness), then walk at L2 → £100, game over.
	fresh := reloadPlayer(t, ctx, players, ps[0])
	correct := shuffledCorrectIndex(t, g, room.ID, 1)
	if _, err := g.Answer(ctx, room.ID, &fresh, 1, correct); err != nil {
		t.Fatalf("pass1: correct L1: %v", err)
	}
	if advanced, err := g.Continue(ctx, room.ID, &fresh); err != nil || !advanced {
		t.Fatalf("pass1: Continue = (%v, %v), want (true, nil)", advanced, err)
	}
	if got := walkNow(t, ctx, g, players, room.ID, ps[0]); got != 100 {
		t.Fatalf("pass1: walk locked %d, want 100", got)
	}
	assertTotalsAre(t, ctx, totals, room.ID, 1,
		models.RoomTotal{GamesPlayed: 1, GamesWon: 0, TotalMoney: 100, BestLevel: 1, BestAmount: 100})

	// Solo rematch re-issues Q1 instantly.
	replayed, started, err := g.Replay(ctx, room, &ps[0])
	if err != nil || !replayed || !started {
		t.Fatalf("solo Replay = (%v, %v, %v), want (true, true, nil)", replayed, started, err)
	}
	// Totals PERSIST through the rematch reset.
	assertTotalsAre(t, ctx, totals, room.ID, 1,
		models.RoomTotal{GamesPlayed: 1, GamesWon: 0, TotalMoney: 100, BestLevel: 1, BestAmount: 100})

	// Pass 2: fall at L1 → £0, game over. Accretes on top (played 2,
	// money stays 100 — the fall added £0, bests keep the walk).
	if lvl := fallSeat(t, ctx, g, players, room.ID, ps[0]); lvl != 1 {
		t.Fatalf("pass2: fell at level %d, want 1", lvl)
	}
	assertTotalsAre(t, ctx, totals, room.ID, 1,
		models.RoomTotal{GamesPlayed: 2, GamesWon: 0, TotalMoney: 100, BestLevel: 1, BestAmount: 100})
}

// TestAccumulateRoomTotalsMath is the direct helper unit over the DB
// upsert: the won math (needs no 15-level run), the run sums across
// calls, the greater-of bests, and the ACTIVE-seat guard — a seat whose
// run is not resolved contributes NOTHING (no games_played, no money).
func TestAccumulateRoomTotalsMath(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, _, rooms, players := newTestGame(t, db)
	totals := repository.NewRoomTotalsRepository(db)
	// The room is never started — AccumulateRoomTotals is the ONLY
	// writer here, isolating the pure math.
	room, _ := createTestRoom(t, ctx, db, rooms, players, "duel", 2)

	// Run 1: seat 1 WON at 15 (£1,000,000, level 15); seat 2 walked £500
	// at level 4. Seat 1 active would be skipped — deliberately not
	// present instead; every call is "the final rows of one run".
	g.AccumulateRoomTotals(ctx, room.ID, []models.Player{
		{RoomID: room.ID, Seat: 1, Status: "won", Amount: 1000000, LevelReached: 15},
		{RoomID: room.ID, Seat: 2, Status: "walked", Amount: 500, LevelReached: 4},
	})
	assertTotalsAre(t, ctx, totals, room.ID, 1,
		models.RoomTotal{GamesPlayed: 1, GamesWon: 1, TotalMoney: 1000000, BestLevel: 15, BestAmount: 1000000})
	assertTotalsAre(t, ctx, totals, room.ID, 2,
		models.RoomTotal{GamesPlayed: 1, GamesWon: 0, TotalMoney: 500, BestLevel: 4, BestAmount: 500})

	// Run 2 (a rematch's second completion): seat 1 STILL "active" in the
	// final rows (defensive guard) → contributes nothing; seat 2 fell £0
	// but reached level 7 → best_level improves to 7, everything else
	// sums. Nobody won either run per seat beyond run 1.
	g.AccumulateRoomTotals(ctx, room.ID, []models.Player{
		{RoomID: room.ID, Seat: 1, Status: "active", Amount: 900, LevelReached: 9},
		{RoomID: room.ID, Seat: 2, Status: "fallen", Amount: 0, LevelReached: 7},
	})
	// Seat 1 untouched by the skipped seat AND (nothing added).
	assertTotalsAre(t, ctx, totals, room.ID, 1,
		models.RoomTotal{GamesPlayed: 1, GamesWon: 1, TotalMoney: 1000000, BestLevel: 15, BestAmount: 1000000})
	assertTotalsAre(t, ctx, totals, room.ID, 2,
		models.RoomTotal{GamesPlayed: 2, GamesWon: 0, TotalMoney: 500, BestLevel: 7, BestAmount: 500})

	// Run 3: a lower walk must never LOWER the bests (greater-of).
	g.AccumulateRoomTotals(ctx, room.ID, []models.Player{
		{RoomID: room.ID, Seat: 1, Status: "walked", Amount: 200, LevelReached: 2},
	})
	assertTotalsAre(t, ctx, totals, room.ID, 1,
		models.RoomTotal{GamesPlayed: 2, GamesWon: 1, TotalMoney: 1000200, BestLevel: 15, BestAmount: 1000000})
}

// TestRoomTotalsCascadeAndEmptyRoom pins the lifecycle edges:
//   - a room with ZERO completed runs has NO totals rows (the read is
//     empty, never a crash — the clients see the key omitted);
//   - deleting the ROOM removes its totals rows with it (FK
//     ON DELETE CASCADE — the session's totals live and die WITH the room;
//     a rematch NEVER deletes anything).
func TestRoomTotalsCascadeAndEmptyRoom(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, _, rooms, players := newTestGame(t, db)
	totals := repository.NewRoomTotalsRepository(db)
	// 0-runs rooms.
	empty, _ := createTestRoom(t, ctx, db, rooms, players, "duel", 2)
	rows, err := totals.GetRoomTotals(ctx, empty.ID)
	if err != nil {
		t.Fatalf("reading totals of a room with no history: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("0-runs room has %d totals rows, want none", len(rows))
	}
	// The helper over an EMPTY roster is a no-op too.
	g.AccumulateRoomTotals(ctx, empty.ID, nil)
	if rows, _ := totals.GetRoomTotals(ctx, empty.ID); len(rows) != 0 {
		t.Fatalf("empty-roster accumulation created rows: %v", rows)
	}

	// Cascade: a room WITH history loses its totals rows when the room
	// row is deleted.
	cascaded, _ := createTestRoom(t, ctx, db, rooms, players, "solo", 1)
	g.AccumulateRoomTotals(ctx, cascaded.ID, []models.Player{
		{RoomID: cascaded.ID, Seat: 1, Status: "won", Amount: 100000, LevelReached: 12},
	})
	if rows, _ := totals.GetRoomTotals(ctx, cascaded.ID); len(rows) != 1 {
		t.Fatalf("seeded totals missing before the delete: %v", rows)
	}
	if err := rooms.DeleteRoom(ctx, cascaded.ID); err != nil {
		t.Fatalf("deleting the room: %v", err)
	}
	rows, err = totals.GetRoomTotals(ctx, cascaded.ID)
	if err != nil {
		t.Fatalf("reading totals after the room delete: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("room_totals survived the room delete: %v (FK ON DELETE CASCADE must remove them)", rows)
	}
	// updated_at is freshly stamped by the upsert (sanity, not zero-time).
	full, _ := createTestRoom(t, ctx, db, rooms, players, "solo", 1)
	g.AccumulateRoomTotals(ctx, full.ID, []models.Player{
		{RoomID: full.ID, Seat: 1, Status: "walked", Amount: 300, LevelReached: 3},
	})
	if rows, _ := totals.GetRoomTotals(ctx, full.ID); len(rows) != 1 || rows[0].UpdatedAt.IsZero() ||
		time.Since(rows[0].UpdatedAt) > 5*time.Second {
		t.Fatalf("upsert did not stamp updated_at: %v", rows)
	}
}
