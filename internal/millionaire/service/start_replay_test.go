package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"boggle-api/internal/millionaire/models"
)

// This file covers the wave-4 room-lifecycle commands at service level
// (table-driven): StartDuel (the deferred duel start behind POST
// /api/rooms/:code/start) and Replay (the in-room rematch), plus the
// opponent_progress spectator broadcasts. The handler integration twins
// live in internal/handler/{start,replay,spectate}_test.go.

// assertGameStartedIs pins the game_started broadcast count.
func assertGameStartedIs(t *testing.T, n *fakeNotifier, want int) {
	t.Helper()
	if got := len(n.eventsOf("game_started", -1)); got != want {
		t.Fatalf("game_started broadcasts = %d, want %d", got, want)
	}
}

// assertCommandRefusal pins a GameError's status and (optionally) a
// message fragment, for rejections that must leave the room untouched.
func assertCommandRefusal(t *testing.T, err error, wantStatus int, msgFragment string) {
	t.Helper()
	ge, ok := err.(*GameError)
	if !ok {
		t.Fatalf("error = %v (%T), want *GameError %d", err, err, wantStatus)
	}
	if ge.Status != wantStatus {
		t.Fatalf("GameError status = %d (%s), want %d", ge.Status, ge.Message, wantStatus)
	}
	if msgFragment != "" && !strings.Contains(strings.ToLower(ge.Message), strings.ToLower(msgFragment)) {
		t.Fatalf("GameError message = %q, want it to contain %q", ge.Message, msgFragment)
	}
}

// fallSeat answers the seat's CURRENT level deliberately wrongly.
func fallSeat(t *testing.T, ctx context.Context, g *GameService, players repositoryPlayerRepo, roomID string, p models.Player) int {
	t.Helper()
	fresh, err := players.GetPlayerByToken(ctx, roomID, p.Token)
	if err != nil {
		t.Fatalf("reloading player: %v", err)
	}
	level := currentLevel(t, g, roomID)
	correct := shuffledCorrectIndex(t, g, roomID, level)
	if _, err := g.Answer(ctx, roomID, fresh, level, (correct+1)%4); err != nil {
		t.Fatalf("wrong answer at level %d: %v", level, err)
	}
	return level
}

// repositoryPlayerRepo narrows the repo surface the helpers need.
type repositoryPlayerRepo interface {
	GetPlayerByToken(ctx context.Context, roomID, token string) (*models.Player, error)
}

// finishDuelMutualFall drives a started duel to game_over quickly: both
// seats fall at whichever level is in play.
func finishDuelMutualFall(t *testing.T, ctx context.Context, g *GameService, players repositoryPlayerRepo, roomID string, ps []models.Player) {
	t.Helper()
	for _, p := range ps {
		fresh, err := players.GetPlayerByToken(ctx, roomID, p.Token)
		if err != nil {
			t.Fatalf("reloading player: %v", err)
		}
		if fresh.Status != "active" {
			continue // already lapsed
		}
		fallSeat(t, ctx, g, players, roomID, *fresh)
	}
}

// TestStartDuelCommand is the table-driven gate for the deferred duel
// start: only the host can flip a FULL lobby duel into a live game; every
// other shape is refused without side effects; a double start is an
// idempotent no-op.
func TestStartDuelCommand(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		seats    int
		actAt    int // index into ps the command runs for
		kind     string
		wantStat int
	}{
		{"host opens the waiting duel", "duel", 2, 0, "started", 0},
		{"joiner cannot start", "duel", 2, 1, "refused", 403},
		{"solo has nothing to start", "solo", 1, 0, "refused", 409},
		{"half-filled duel waits", "duel", 1, 0, "refused", 409},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			db := newServiceTestDB(t)
			g, n, rooms, players := newTestGame(t, db)
			room, ps := createTestRoom(t, ctx, db, rooms, players, c.mode, c.seats)

			started, err := g.StartDuel(ctx, room, &ps[c.actAt])
			switch c.kind {
			case "started":
				if err != nil || !started {
					t.Fatalf("StartDuel = (%v, %v), want started", started, err)
				}
				assertGameStartedIs(t, n, 1)
				fresh, rerr := rooms.GetRoom(ctx, room.ID)
				if rerr != nil {
					t.Fatal(rerr)
				}
				if fresh.Status != "active" {
					t.Fatalf("room status = %q, want active", fresh.Status)
				}
				if lvl := currentLevel(t, g, room.ID); lvl != 1 {
					t.Fatalf("level = %d, want 1 issued", lvl)
				}
				// Idempotent double start: (false, nil), ONE broadcast.
				again, aerr := g.StartDuel(ctx, room, &ps[0])
				if aerr != nil || again {
					t.Fatalf("double start = (%v, %v), want (false, nil)", again, aerr)
				}
				assertGameStartedIs(t, n, 1)
			case "refused":
				assertCommandRefusal(t, err, c.wantStat, "")
				assertGameStartedIs(t, n, 0)
				fresh, rerr := rooms.GetRoom(ctx, room.ID)
				if rerr != nil {
					t.Fatal(rerr)
				}
				if fresh.Status != "lobby" {
					t.Fatalf("refused start flipped the room: %q", fresh.Status)
				}
			}
		})
	}

	// A FINISHED duel refuses start (replay is the path back).
	t.Run("finished duel refuses start", func(t *testing.T) {
		ctx := context.Background()
		db := newServiceTestDB(t)
		g, n, rooms, players := newTestGame(t, db)
		room, ps := createTestRoom(t, ctx, db, rooms, players, "duel", 2)
		if _, err := g.StartDuel(ctx, room, &ps[0]); err != nil {
			t.Fatal(err)
		}
		finishDuelMutualFall(t, ctx, g, players, room.ID, ps)
		if n.waitEvent("game_over", -1, time.Second) == nil {
			t.Fatal("expected a finished duel in this cell")
		}
		_, err := g.StartDuel(ctx, room, &ps[0])
		assertCommandRefusal(t, err, 409, "replay")
		assertGameStartedIs(t, n, 1)
	})
}

// TestReplayCommand is the table-driven gate for the in-room rematch.
func TestReplayCommand(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		seats   int
		initAt  int // index into ps that initiates the rematch
		wantRe  bool
		wantStr bool // solo instant re-issue
	}{
		{"finished duel resets to the lobby (host initiates)", "duel", 2, 0, true, false},
		{"finished duel resets to the lobby (seat 2 initiates)", "duel", 2, 1, true, false},
		{"finished solo resets and instantly re-issues Q1", "solo", 1, 0, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			db := newServiceTestDB(t)
			g, n, rooms, players := newTestGame(t, db)
			room, ps := createTestRoom(t, ctx, db, rooms, players, c.mode, c.seats)

			// Finish the run: solo → start (instant-play) then the seat
			// falls at L1; duel → host starts, both fall at L1.
			if c.mode == "solo" {
				if err := g.StartGame(ctx, room, ps); err != nil {
					t.Fatal(err)
				}
				fallSeat(t, ctx, g, players, room.ID, ps[0])
			} else {
				if _, err := g.StartDuel(ctx, room, &ps[0]); err != nil {
					t.Fatal(err)
				}
				finishDuelMutualFall(t, ctx, g, players, room.ID, ps)
			}
			if n.waitEvent("game_over", -1, time.Second) == nil {
				t.Fatal("expected a finished run in this cell")
			}
			finished, err := rooms.GetRoom(ctx, room.ID)
			if err != nil {
				t.Fatal(err)
			}
			if finished.Status != "finished" {
				t.Fatalf("room status = %q, want finished", finished.Status)
			}

			replayed, started, err := g.Replay(ctx, room, &ps[c.initAt])
			if err != nil || replayed != c.wantRe || started != c.wantStr {
				t.Fatalf("Replay = (%v, %v, %v), want (%v, %v, nil)", replayed, started, err, c.wantRe, c.wantStr)
			}

			// rematch_ready lists the seats ascending, and the solo case
			// carries a FRESH game_started right after it.
			rr := n.eventData("rematch_ready", -1)
			if rr == nil {
				t.Fatal("no rematch_ready broadcast")
			}
			seats := rr["seats"].([]int)
			if len(seats) != c.seats {
				t.Fatalf("rematch_ready seats = %v, want %d entries", seats, c.seats)
			}
			for i, s := range seats {
				if s != i+1 {
					t.Fatalf("rematch_ready seats = %v, want ascending 1..%d", seats, c.seats)
				}
			}
			if c.wantStr {
				assertGameStartedIs(t, n, 2) // the run + the instant rematch
			} else {
				assertGameStartedIs(t, n, 1) // duel: still deferred, lobby only
			}

			// Every seat is reset (status/amount/level/lifelines) and the
			// room landed back in its wave-4 state.
			for _, p := range ps {
				fresh, err := players.GetPlayerByToken(ctx, room.ID, p.Token)
				if err != nil {
					t.Fatal(err)
				}
				var left []string
				if err := json.Unmarshal([]byte(fresh.LifelinesLeft), &left); err != nil {
					t.Fatalf("seat %d lifelines_left = %q: %v", fresh.Seat, fresh.LifelinesLeft, err)
				}
				if fresh.Status != "active" || fresh.Amount != 0 || fresh.LevelReached != 0 ||
					fmt.Sprint(left) != fmt.Sprint([]string{"fifty_fifty", "audience", "switch"}) {
					t.Fatalf("seat %d row = %d/%d/%s/%s, want reset 0/0/active/default",
						fresh.Seat, fresh.LevelReached, fresh.Amount, fresh.Status, left)
				}
			}
			after, err := rooms.GetRoom(ctx, room.ID)
			if err != nil {
				t.Fatal(err)
			}
			if c.wantStr && after.Status != "active" {
				t.Fatalf("solo rematch room status = %q, want active", after.Status)
			}
			if !c.wantStr && after.Status != "lobby" {
				t.Fatalf("duel rematch room status = %q, want lobby", after.Status)
			}
		})
	}

	// MID-RUN replay refuses while any seat is still climbing, and a
	// rematch TWICE in a row works with FRESH standings (no carryover).
	t.Run("mid-run refused and back-to-back rematches stay fresh", func(t *testing.T) {
		ctx := context.Background()
		db := newServiceTestDB(t)
		g, n, rooms, players := newTestGame(t, db)
		room, ps := createTestRoom(t, ctx, db, rooms, players, "duel", 2)

		if _, err := g.StartDuel(ctx, room, &ps[0]); err != nil {
			t.Fatal(err)
		}
		// Seat 1 answers L1 correctly; seat 2 is pending → mid-run.
		fresh1, err := players.GetPlayerByToken(ctx, room.ID, ps[0].Token)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := g.Answer(ctx, room.ID, fresh1, 1, shuffledCorrectIndex(t, g, room.ID, 1)); err != nil {
			t.Fatal(err)
		}
		_, _, rerr := g.Replay(ctx, room, &ps[0])
		assertCommandRefusal(t, rerr, 409, "still in progress")
		// And the room must be UNTOUCHED: still active, still on L1.
		if lvl := currentLevel(t, g, room.ID); lvl != 1 {
			t.Fatalf("mid-run replay disturbed the level: %d", lvl)
		}

		// Seat 2 falls at L1 → the level advances to L2 for the survivor;
		// seat 1 answers L2 and WALKS at L3 → standings [seat1 £200, seat2 £0].
		fresh2, err := players.GetPlayerByToken(ctx, room.ID, ps[1].Token)
		if err != nil {
			t.Fatal(err)
		}
		fallSeat(t, ctx, g, players, room.ID, *fresh2)
		if lvl := currentLevel(t, g, room.ID); lvl != 2 {
			t.Fatalf("level = %d, want 2 after seat 2's fall", lvl)
		}
		walker, _ := players.GetPlayerByToken(ctx, room.ID, ps[0].Token)
		if _, err := g.Answer(ctx, room.ID, walker, 2, shuffledCorrectIndex(t, g, room.ID, 2)); err != nil {
			t.Fatal(err)
		}
		if lvl := currentLevel(t, g, room.ID); lvl != 3 {
			t.Fatalf("level = %d, want 3 for the walk", lvl)
		}
		fresh1, _ = players.GetPlayerByToken(ctx, room.ID, ps[0].Token)
		walkAmount, err := g.Walk(ctx, room.ID, fresh1)
		if err != nil {
			t.Fatal(err)
		}
		if walkAmount != 200 {
			t.Fatalf("walk amount = %d, want 200", walkAmount)
		}
		if n.waitEvent("game_over", -1, time.Second) == nil {
			t.Fatal("no game_over for pass 1")
		}

		// Rematch #1 (host): both rows reset — no stale £100 anywhere.
		replayed, started, err := g.Replay(ctx, room, &ps[0])
		if err != nil || !replayed || started {
			t.Fatalf("rematch #1 = (%v, %v, %v), want (true, false, nil)", replayed, started, err)
		}
		for _, p := range ps {
			fresh, err := players.GetPlayerByToken(ctx, room.ID, p.Token)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.Amount != 0 || fresh.LevelReached != 0 || fresh.Status != "active" {
				t.Fatalf("seat %d not reset for the rematch: %d/%d/%s",
					fresh.Seat, fresh.LevelReached, fresh.Amount, fresh.Status)
			}
		}

		// Pass 2: host starts, both fall at L1 → standings 0/0 fresh.
		if _, err := g.StartDuel(ctx, room, &ps[0]); err != nil {
			t.Fatal(err)
		}
		finishDuelMutualFall(t, ctx, g, players, room.ID, ps)
		over := n.waitEvent("game_over", -1, time.Second)
		if over == nil {
			t.Fatal("no game_over for pass 2")
		}
		standings := over["standings"].([]map[string]interface{})
		if len(standings) != 2 {
			t.Fatalf("pass2 standings = %#v", over["standings"])
		}
		for i, s := range standings {
			if s["amount"] != 0 {
				t.Fatalf("pass2 standings[%d] = %#v, want a FRESH 0 (no carryover)", i, s)
			}
		}

		// Rematch #2 right after: works again.
		replayed, started, err = g.Replay(ctx, room, &ps[1])
		if err != nil || !replayed || started {
			t.Fatalf("rematch #2 = (%v, %v, %v), want (true, false, nil)", replayed, started, err)
		}
		if after, _ := rooms.GetRoom(ctx, room.ID); after.Status != "lobby" {
			t.Fatalf("room after rematch #2 = %q, want lobby", after.Status)
		}

		for _, c := range leakScanAll(n.events) {
			if c {
				t.Fatalf("leaked correct_index outside the allowed events: %v", n.events)
			}
		}
	})
}

// TestOpponentProgressSpectateEvents pins the spectator broadcasts at
// service level: the survivor's correct answers and lifeline usage emit
// opponent_progress PRIVATELY to the lapsed seat (never a broadcast,
// never to the survivor, never a correct_index).
func TestOpponentProgressSpectateEvents(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "duel", 2)

	if _, err := g.StartDuel(ctx, room, &ps[0]); err != nil {
		t.Fatal(err)
	}

	// Seat 1 falls at L1.
	if lvl := fallSeat(t, ctx, g, players, room.ID, ps[0]); lvl != 1 {
		t.Fatalf("seat 1 fell at %d, want 1", lvl)
	}

	// Survivor (seat 2) answers L1 → progress to seat 1 only.
	fresh2, _ := players.GetPlayerByToken(ctx, room.ID, ps[1].Token)
	if _, err := g.Answer(ctx, room.ID, fresh2, 1, shuffledCorrectIndex(t, g, room.ID, 1)); err != nil {
		t.Fatal(err)
	}
	progress := n.eventsOf("opponent_progress", 1)
	if len(progress) != 1 {
		t.Fatalf("lapsed seat got %d opponent_progress events, want 1", len(progress))
	}
	msg := progress[0].msg
	if msg["seat"] != 2 || msg["status"] != "active" {
		t.Fatalf("opponent_progress = %v, want seat2/active", msg)
	}
	if lvl, _ := msg["level"].(int); lvl != 1 {
		t.Fatalf("opponent_progress level = %v, want 1", msg["level"])
	}
	if amt, _ := msg["amount"].(int); amt != 100 {
		t.Fatalf("opponent_progress amount = %v, want 100", msg["amount"])
	}
	if got := n.eventsOf("opponent_progress", 2); len(got) != 0 {
		t.Fatalf("the survivor received own progress: %v", got)
	}
	if got := n.eventsOf("opponent_progress", 0); len(got) != 0 {
		t.Fatalf("opponent_progress leaked as a room broadcast: %v", got)
	}

	// Survivor answers L2 → progress {level:2, amount:200}.
	fresh2, _ = players.GetPlayerByToken(ctx, room.ID, ps[1].Token)
	if _, err := g.Answer(ctx, room.ID, fresh2, 2, shuffledCorrectIndex(t, g, room.ID, 2)); err != nil {
		t.Fatal(err)
	}
	msg = n.eventData("opponent_progress", 1)
	if msg == nil {
		t.Fatal("no second opponent_progress")
	}
	if lvl, _ := msg["level"].(int); lvl != 2 {
		t.Fatalf("second progress level = %v, want 2", msg["level"])
	}
	if amt, _ := msg["amount"].(int); amt != 200 {
		t.Fatalf("second progress amount = %v, want 200", msg["amount"])
	}

	// A lifeline by the survivor on the NEXT level in play → progress
	// {level:3, amount:200} to the lapsed seat only.
	if _, err := g.Lifeline(ctx, room.ID, fresh2, "fifty_fifty"); err != nil {
		t.Fatal(err)
	}
	msg = n.eventData("opponent_progress", 1)
	if msg == nil {
		t.Fatal("no progress after the lifeline")
	}
	if lvl, _ := msg["level"].(int); lvl != 3 {
		t.Fatalf("lifeline progress level = %v, want 3 (the level in play)", msg["level"])
	}
	if amt, _ := msg["amount"].(int); amt != 200 {
		t.Fatalf("lifeline progress amount = %v, want the secured 200", msg["amount"])
	}
	if total := len(n.eventsOf("opponent_progress", 1)); total != 3 {
		t.Fatalf("lapsed seat %d totals %d progress events, want 3", 1, total)
	}

	for _, c := range leakScanAll(n.events) {
		if c {
			t.Fatalf("leaked correct_index outside the allowed events: %v", n.events)
		}
	}
}
