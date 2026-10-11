package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"boggle-api/internal/millionaire/models"
)

// This file is the wave-5 PER-SEAT SWAP ENGINE suite (locked product
// decision: "SWAP replaces ONLY THE SEAT'S OWN QUESTION"). It pins:
//
//   - per-seat isolation      — a swap rewrites the caller's question and
//     NOTHING of the peer's (same level, same ends_at);
//   - the one-per-level ledger and every refusal (answered/locked, expired,
//     exhausted pool) — each a 409 + an `error` event on the caller's socket
//     AND a lifeline that stays UNSPENT;
//   - duel fairness           — a swap may never move a seat onto a question
//     the opponent already answered this level;
//   - the leak discipline     — the `question_swapped` payload carries no
//     correct_index anywhere, and the room `lifeline_used` broadcast carries
//     no question at all;
//   - concurrency             — N parallel swaps from one seat yield exactly
//     one winner.
//
// DB-backed like the rest of the service suite: it skips only when the dev
// DB is unreachable (same pattern as game_test.go).

// swapOf runs a swap for `seat` and returns the REST-side data map, failing
// the test on any non-200 outcome.
func swapOf(t *testing.T, ctx context.Context, g *GameService, players interface {
	GetPlayerByToken(context.Context, string, string) (*models.Player, error)
}, roomID string, ps []models.Player, seat int) map[string]interface{} {
	t.Helper()
	p, err := players.GetPlayerByToken(ctx, roomID, ps[seat-1].Token)
	if err != nil {
		t.Fatalf("reloading seat %d: %v", seat, err)
	}
	data, err := g.Lifeline(ctx, roomID, p, "switch")
	if err != nil {
		t.Fatalf("seat %d swap: %v", seat, err)
	}
	return data
}

// lifelinesLeftNames returns a player's persisted jsonb lifeline set.
func lifelinesLeftNames(t *testing.T, ctx context.Context, players interface {
	GetPlayerByToken(context.Context, string, string) (*models.Player, error)
}, roomID string, ps []models.Player, seat int) []string {
	t.Helper()
	p, err := players.GetPlayerByToken(ctx, roomID, ps[seat-1].Token)
	if err != nil {
		t.Fatalf("reloading seat %d: %v", seat, err)
	}
	left, err := lifelinesLeftOf(p)
	if err != nil {
		t.Fatalf("seat %d lifelines_left %q: %v", seat, p.LifelinesLeft, err)
	}
	return left
}

// sameQuestion compares two levelQuestions field-by-field (the struct holds
// a slice, so it is not directly comparable).
func sameQuestion(a, b levelQuestion) bool {
	return a.questionID == b.questionID && a.text == b.text &&
		a.correctIndex == b.correctIndex && fmt.Sprint(a.options) == fmt.Sprint(b.options)
}

// hasCorrectIndex walks an event payload for the `correct_index` key at any
// depth — the wave-5 leak guard for the swap event.
func hasCorrectIndex(v interface{}) bool {
	switch val := v.(type) {
	case map[string]interface{}:
		for k, sub := range val {
			if k == "correct_index" {
				return true
			}
			if hasCorrectIndex(sub) {
				return true
			}
		}
	case []interface{}:
		for _, sub := range val {
			if hasCorrectIndex(sub) {
				return true
			}
		}
	}
	return false
}

// TestSwapIsPerSeatOnly is the headline wave-5 contract: seat 2 swaps and
// ONLY seat 2's question changes. Seat 1's active question is identical
// before and after (question id, text, options AND correct index), the level
// and ends_at are preserved, and seat 2's replacement is the variant-2
// realization of the SAME level drawn from the pre-selected batch.
func TestSwapIsPerSeatOnly(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "duel", 2)
	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	seat1Before := seatQuestion(t, g, room.ID, 1, 1)
	seat2Before := seatQuestion(t, g, room.ID, 1, 2)
	if seat1Before.questionID != seat2Before.questionID {
		t.Fatalf("a fresh level must start both seats on the shared primary (%q vs %q)",
			seat1Before.questionID, seat2Before.questionID)
	}
	_, endsBefore, _ := g.Snapshot(room.ID, 1)

	data := swapOf(t, ctx, g, players, room.ID, ps, 2)
	swapped, ok := data["question"].(QuestionView)
	if !ok {
		t.Fatalf("swap REST data carries no question: %v", data)
	}

	// Seat 2 got a DIFFERENT question of the SAME level; seat 1 is
	// byte-for-byte untouched.
	seat2After := seatQuestion(t, g, room.ID, 1, 2)
	seat1After := seatQuestion(t, g, room.ID, 1, 1)
	if seat2After.questionID == seat2Before.questionID {
		t.Fatal("seat 2's swap did not change its own question")
	}
	if swapped.Text != seat2After.text || fmt.Sprint(swapped.Options) != fmt.Sprint(seat2After.options) {
		t.Fatalf("swap payload %q/%v disagrees with the seat's active question %q/%v",
			swapped.Text, swapped.Options, seat2After.text, seat2After.options)
	}
	if !sameQuestion(seat1After, seat1Before) {
		t.Fatalf("seat 1's question changed under seat 2's swap:\n before %+v\n after  %+v", seat1Before, seat1After)
	}
	if lvl := currentLevel(t, g, room.ID); lvl != 1 {
		t.Fatalf("swap changed the level: %d", lvl)
	}
	_, endsAfter, _ := g.Snapshot(room.ID, 1)
	if !endsBefore.Equal(*endsAfter) {
		t.Fatalf("swap reset the timer: %v → %v", endsBefore, endsAfter)
	}

	// PIN THE EMITTED DEADLINE, not just the internal field (adversarial
	// review finding 1): the FE adopts the echoed `ends_at` VERBATIM and
	// re-derives its whole countdown from it, so if the swap ever emitted a
	// fresh `now+budget` while leaving game.endsAt alone, every other test
	// here would still pass while the swap became a free re-read — the exact
	// invariant SPEC §1 forbids. Both emissions (the REST `data.ends_at` and
	// the private `question_swapped.ends_at`) are asserted below.
	wantEnds := endsBefore.UTC()
	restEnds, ok := data["ends_at"].(time.Time)
	if !ok {
		t.Fatalf("swap REST data.ends_at is %T, want a deadline the FE can adopt", data["ends_at"])
	}
	if !restEnds.UTC().Equal(wantEnds) {
		t.Fatalf("swap REST data.ends_at = %v, want the unchanged deadline %v "+
			"(a swap must never re-arm the clock)", restEnds.UTC(), wantEnds)
	}

	// The replacement is the persisted variant-2 realization of level 1.
	var row struct {
		QID string `ksql:"qid"`
	}
	if err := db.QueryOne(ctx, &row,
		"SELECT question_id AS qid FROM millionaire_room_questions WHERE room_id = $1 AND level = 1 AND variant = 2",
		room.ID); err != nil {
		t.Fatal(err)
	}
	if row.QID != seat2After.questionID {
		t.Fatalf("swapped question %q is not the pre-selected variant-2 (%q)", seat2After.questionID, row.QID)
	}

	// Delivery: private to seat 2, never a room broadcast, and the generic
	// lifeline_used carries no question.
	events := n.eventsOf("question_swapped", -1)
	if len(events) != 1 {
		t.Fatalf("question_swapped events = %d, want exactly 1", len(events))
	}
	if events[0].seat != 2 {
		t.Fatalf("question_swapped went to seat %d, want seat 2 only", events[0].seat)
	}
	if hasCorrectIndex(events[0].msg) {
		t.Fatalf("question_swapped payload leaks correct_index: %v", events[0].msg)
	}
	// The WS frame's echoed deadline is pinned too (see the REST assertion
	// above) — this is the value the FE's Timer renders from verbatim. The
	// notifier captures the payload pre-marshal, so accept either the
	// time.Time or its marshalled string form.
	var wsEnds time.Time
	switch v := events[0].msg["ends_at"].(type) {
	case time.Time:
		wsEnds = v
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			t.Fatalf("question_swapped.ends_at is not a deadline string: %v", err)
		}
		wsEnds = parsed
	default:
		t.Fatalf("question_swapped.ends_at is %T, want a deadline", events[0].msg["ends_at"])
	}
	if !wsEnds.UTC().Equal(wantEnds) {
		t.Fatalf("question_swapped.ends_at = %v, want the unchanged deadline %v", wsEnds.UTC(), wantEnds)
	}
	used := n.eventData("lifeline_used", 0)
	if used == nil {
		t.Fatal("no room lifeline_used broadcast for the swap")
	}
	if d, isMap := used["data"].(map[string]interface{}); !isMap || len(d) != 0 {
		t.Fatalf("room lifeline_used must carry no question for a per-seat swap: %v", used["data"])
	}

	// Seat 2's lifeline was consumed; seat 1's set is untouched.
	if left := strings.Join(lifelinesLeftNames(t, ctx, players, room.ID, ps, 2), ","); left != "fifty_fifty,audience" {
		t.Fatalf("seat 2 lifelines_left = %q", left)
	}
	if left := strings.Join(lifelinesLeftNames(t, ctx, players, room.ID, ps, 1), ","); left != "fifty_fifty,audience,switch" {
		t.Fatalf("seat 1 lifelines_left = %q — a peer's swap must not spend it", left)
	}
}

// TestSwapScoringIsPerSeat pins that each seat is scored against ITS OWN
// active question: seat 2's variant-2 index scores, the shared primary's
// index does not (whenever the two layouts disagree), and seat 1 — still on
// the primary — scores on the primary's own index.
func TestSwapScoringIsPerSeat(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, _, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "duel", 2)
	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}
	swapOf(t, ctx, g, players, room.ID, ps, 2)

	seat2Idx := seatCorrectIndex(t, g, room.ID, 1, 2)
	seat1Idx := seatCorrectIndex(t, g, room.ID, 1, 1)

	p2, _ := players.GetPlayerByToken(ctx, room.ID, ps[1].Token)
	outcome, err := g.Answer(ctx, room.ID, p2, 1, seat2Idx)
	if err != nil {
		t.Fatalf("seat 2 answering its OWN question: %v", err)
	}
	if !outcome.Correct || outcome.CorrectIndex != seat2Idx {
		t.Fatalf("seat 2 outcome = %+v, want correct at its own index %d", outcome, seat2Idx)
	}

	// Seat 1 still climbs on the shared primary.
	if lvl := currentLevel(t, g, room.ID); lvl != 1 {
		t.Fatalf("level advanced before seat 1 resolved: %d", lvl)
	}
	p1, _ := players.GetPlayerByToken(ctx, room.ID, ps[0].Token)
	outcome, err = g.Answer(ctx, room.ID, p1, 1, seat1Idx)
	if err != nil {
		t.Fatalf("seat 1 answering the shared primary: %v", err)
	}
	if !outcome.Correct || outcome.CorrectIndex != seat1Idx {
		t.Fatalf("seat 1 outcome = %+v, want correct at the primary index %d", outcome, seat1Idx)
	}
}

// TestSwapFiftyFiftyTargetsOwnQuestion: after a swap, 50:50 and the audience
// poll are computed against the CALLER's own question (they strike/weigh
// the seat's option order), never the peer's or the shared primary's.
func TestSwapFiftyFiftyTargetsOwnQuestion(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, _, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "duel", 2)
	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}
	swapOf(t, ctx, g, players, room.ID, ps, 2)
	seat2Idx := seatCorrectIndex(t, g, room.ID, 1, 2)

	p2, _ := players.GetPlayerByToken(ctx, room.ID, ps[1].Token)
	data, err := g.Lifeline(ctx, room.ID, p2, "fifty_fifty")
	if err != nil {
		t.Fatalf("seat 2 fifty_fifty after a swap: %v", err)
	}
	removed, _ := data["removed_indices"].([]int)
	if len(removed) != 2 {
		t.Fatalf("removed_indices = %v", data["removed_indices"])
	}
	for _, idx := range removed {
		if idx == seat2Idx {
			t.Fatalf("50:50 struck the swapped seat's CORRECT option %d: %v", seat2Idx, removed)
		}
	}

	p1, _ := players.GetPlayerByToken(ctx, room.ID, ps[0].Token)
	data, err = g.Lifeline(ctx, room.ID, p1, "audience")
	if err != nil {
		t.Fatalf("seat 1 audience: %v", err)
	}
	pcts, _ := data["percentages"].([]int)
	if len(pcts) != 4 || pcts[seatCorrectIndex(t, g, room.ID, 1, 1)] != 70 {
		t.Fatalf("seat 1 audience poll = %v, want the easy-tier 70 on the PRIMARY's index", pcts)
	}
}

// TestSwapRefusedOnceSeatResolved is the "locked answer" guard: a seat that
// has already resolved the level (answered, timed out or walked) cannot
// swap — and the refusal costs the seat nothing.
func TestSwapRefusedOnceSeatResolved(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "duel", 2)
	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	p2, _ := players.GetPlayerByToken(ctx, room.ID, ps[1].Token)
	if _, err := g.Answer(ctx, room.ID, p2, 1, seatCorrectIndex(t, g, room.ID, 1, 2)); err != nil {
		t.Fatalf("seat 2 answering: %v", err)
	}

	fresh, _ := players.GetPlayerByToken(ctx, room.ID, ps[1].Token)
	_, err := g.Lifeline(ctx, room.ID, fresh, "switch")
	if err == nil {
		t.Fatal("a resolved seat swapped its question")
	}
	ge, ok := err.(*GameError)
	if !ok || ge.Status != 409 {
		t.Fatalf("swap-after-answer error = %v, want GameError 409", err)
	}
	if ev := n.eventData("error", 2); ev == nil {
		t.Fatal("no error event on the refused seat's socket")
	}
	if len(n.eventsOf("question_swapped", -1)) != 0 {
		t.Fatal("a refused swap still emitted question_swapped")
	}
	// The lifeline was NOT burned by the refusal.
	if left := strings.Join(lifelinesLeftNames(t, ctx, players, room.ID, ps, 2), ","); left != "fifty_fifty,audience,switch" {
		t.Fatalf("a refused swap spent the lifeline: %q", left)
	}
}

// TestSwapRefusedAfterExpiry: past ends_at the seat's clock is up — the
// swap is refused even when the timeout callback has not run yet, and the
// lifeline survives.
func TestSwapRefusedAfterExpiry(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "solo", 1)
	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	// Expire the level without resolving it: stop the fallback timer and
	// move ends_at into the past (same-package white box).
	g.mu.Lock()
	game := g.games[room.ID]
	game.timer.Stop()
	game.endsAt = time.Now().Add(-time.Second)
	g.mu.Unlock()

	p, _ := players.GetPlayerByToken(ctx, room.ID, ps[0].Token)
	_, err := g.Lifeline(ctx, room.ID, p, "switch")
	if err == nil {
		t.Fatal("a swap was accepted after the question expired")
	}
	ge, ok := err.(*GameError)
	if !ok || ge.Status != 409 || ge.Message != "the question has already expired" {
		t.Fatalf("expired swap error = %v, want 409 'the question has already expired'", err)
	}
	if ev := n.eventData("error", 1); ev == nil {
		t.Fatal("no error event for the expired swap")
	}
	if left := strings.Join(lifelinesLeftNames(t, ctx, players, room.ID, ps, 1), ","); left != "fifty_fifty,audience,switch" {
		t.Fatalf("an expired swap spent the lifeline: %q", left)
	}
}

// TestSwapRefusedWhenPoolExhausted: a level whose pre-selected batch has no
// spare variant degrades to a clean refusal instead of corrupting the live
// question. The seat keeps its question and its lifeline.
func TestSwapRefusedWhenPoolExhausted(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "solo", 1)
	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}
	before := seatQuestion(t, g, room.ID, 1, 1)

	g.mu.Lock()
	g.games[room.ID].variants[1] = g.games[room.ID].variants[1][:1] // drop the alternate
	g.mu.Unlock()

	p, _ := players.GetPlayerByToken(ctx, room.ID, ps[0].Token)
	_, err := g.Lifeline(ctx, room.ID, p, "switch")
	if err == nil {
		t.Fatal("a swap was accepted with no unused variant left")
	}
	ge, ok := err.(*GameError)
	if !ok || ge.Status != 409 || ge.Message != "no swap question available for this level" {
		t.Fatalf("exhausted swap error = %v, want 409 'no swap question available for this level'", err)
	}
	if after := seatQuestion(t, g, room.ID, 1, 1); !sameQuestion(after, before) {
		t.Fatalf("a refused swap mutated the live question: %+v → %+v", before, after)
	}
	if len(n.eventsOf("question_swapped", -1)) != 0 {
		t.Fatal("an exhausted swap emitted question_swapped")
	}
	if left := strings.Join(lifelinesLeftNames(t, ctx, players, room.ID, ps, 1), ","); left != "fifty_fifty,audience,switch" {
		t.Fatalf("an exhausted swap spent the lifeline: %q", left)
	}
}

// TestSwapOnePerSeatPerLevel pins the per-LEVEL ledger independently of the
// one-use-per-run DB spend: even with a restored lifeline set, the second
// swap at the same level is refused.
func TestSwapOnePerSeatPerLevel(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, _, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "solo", 1)
	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}
	swapOf(t, ctx, g, players, room.ID, ps, 1)
	first := seatQuestion(t, g, room.ID, 1, 1)

	// Hand the lifeline back so only the per-level ledger can refuse.
	if _, err := db.Exec(ctx, "UPDATE millionaire_players SET lifelines_left = $1 WHERE room_id = $2 AND seat = 1",
		models.DefaultLifelinesLeft, room.ID); err != nil {
		t.Fatalf("restoring lifelines_left: %v", err)
	}
	p, _ := players.GetPlayerByToken(ctx, room.ID, ps[0].Token)
	_, err := g.Lifeline(ctx, room.ID, p, "switch")
	if err == nil {
		t.Fatal("a second swap at the same level was accepted")
	}
	ge, ok := err.(*GameError)
	if !ok || ge.Status != 409 {
		t.Fatalf("second-same-level error = %v, want GameError 409", err)
	}
	if after := seatQuestion(t, g, room.ID, 1, 1); !sameQuestion(after, first) {
		t.Fatalf("a refused second swap moved the question: %+v → %+v", first, after)
	}
}

// TestSwapFairnessNeverShowsOpponentAnsweredQuestion is the duel fairness
// invariant: with a two-variant pool the swap target can genuinely be a
// question the opponent already answered. That swap is REFUSED — a seat is
// only ever moved ONTO an unanswered question — and the level stays intact
// for the refusing seat.
func TestSwapFairnessNeverShowsOpponentAnsweredQuestion(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "duel", 2)
	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	// Seat 1 swaps onto variant 2 and answers it.
	swapOf(t, ctx, g, players, room.ID, ps, 1)
	variant2 := seatCorrectIndex(t, g, room.ID, 1, 1)
	p1, _ := players.GetPlayerByToken(ctx, room.ID, ps[0].Token)
	if _, err := g.Answer(ctx, room.ID, p1, 1, variant2); err != nil {
		t.Fatalf("seat 1 answering its swapped question: %v", err)
	}
	// The level is NOT advanced — seat 2 is still pending.
	if lvl := currentLevel(t, g, room.ID); lvl != 1 {
		t.Fatalf("level advanced while seat 2 was pending: %d", lvl)
	}

	seat2Before := seatQuestion(t, g, room.ID, 1, 2)
	p2, _ := players.GetPlayerByToken(ctx, room.ID, ps[1].Token)
	_, err := g.Lifeline(ctx, room.ID, p2, "switch")
	if err == nil {
		t.Fatal("seat 2 swapped onto the question seat 1 already answered")
	}
	ge, ok := err.(*GameError)
	if !ok || ge.Status != 409 {
		t.Fatalf("unfair swap error = %v, want GameError 409", err)
	}
	if ev := n.eventData("error", 2); ev == nil {
		t.Fatal("no error event for the unfair swap")
	}
	if after := seatQuestion(t, g, room.ID, 1, 2); !sameQuestion(after, seat2Before) {
		t.Fatalf("the refused swap moved seat 2's question: %+v → %+v", seat2Before, after)
	}
	if len(n.eventsOf("question_swapped", -1)) != 1 {
		t.Fatalf("question_swapped count = %d, want exactly seat 1's", len(n.eventsOf("question_swapped", -1)))
	}
	if left := strings.Join(lifelinesLeftNames(t, ctx, players, room.ID, ps, 2), ","); left != "fifty_fifty,audience,switch" {
		t.Fatalf("the unfair refusal spent seat 2's lifeline: %q", left)
	}

	// Control: the swap IS allowed while the target is still unanswered (the
	// same sequence without seat 1's answer) — see TestSwapBothSeatsSwap.
}

// TestSwapBothSeatsSwapIndependently: both seats may swap at the same level
// (each at most once). Neither one's swap disturbs the other's event or
// question, and each seat is scored against its own variant-2 layout.
func TestSwapBothSeatsSwapIndependently(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "duel", 2)
	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	swapOf(t, ctx, g, players, room.ID, ps, 1)
	seat1Swapped := seatQuestion(t, g, room.ID, 1, 1)
	swapOf(t, ctx, g, players, room.ID, ps, 2)
	seat2Swapped := seatQuestion(t, g, room.ID, 1, 2)

	if !sameQuestion(seat1Swapped, seat2Swapped) {
		t.Fatalf("both seats swapped to the same level-2 variant: %q vs %q",
			seat1Swapped.questionID, seat2Swapped.questionID)
	}
	if !sameQuestion(seatQuestion(t, g, room.ID, 1, 1), seat1Swapped) {
		t.Fatal("seat 2's swap moved seat 1's question")
	}
	for _, seat := range []int{1, 2} {
		if ev := n.eventData("question_swapped", seat); ev == nil {
			t.Fatalf("seat %d never received its own question_swapped", seat)
		}
	}
}

// TestSwapConcurrentSameSeatExactlyOneWinner: N parallel swap POSTs from one
// seat yield exactly one accept — the per-level ledger and the atomic DB
// spend hold under a true race, and exactly one event is emitted.
func TestSwapConcurrentSameSeatExactlyOneWinner(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "solo", 1)
	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	const racers = 8
	codes := make([]int, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := players.GetPlayerByToken(ctx, room.ID, ps[0].Token)
			if err != nil {
				codes[i] = -1
				return
			}
			<-start
			if _, err := g.Lifeline(ctx, room.ID, p, "switch"); err != nil {
				if _, ok := err.(*GameError); ok {
					codes[i] = 409
					return
				}
				codes[i] = -1
				return
			}
			codes[i] = 200
		}(i)
	}
	close(start)
	wg.Wait()

	wins, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case 200:
			wins++
		case 409:
			conflicts++
		default:
			t.Fatalf("unexpected outcome %d in the swap race", c)
		}
	}
	if wins != 1 {
		t.Fatalf("%d of %d parallel swaps were accepted, want exactly 1", wins, racers)
	}
	if conflicts != racers-1 {
		t.Fatalf("%d conflicts, want %d", conflicts, racers-1)
	}
	if got := len(n.eventsOf("question_swapped", -1)); got != 1 {
		t.Fatalf("question_swapped events = %d, want exactly 1", got)
	}
}
