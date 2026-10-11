package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"math/rand/v2"
)

// This file covers the M5 lifelines: the deterministic (rng-injected)
// audience weights, the fifty_fifty wrong-index exclusion, and the
// service-level integration flow (use → event → persistence → reuse
// rejection), including the one-use-each-per-run contract, the
// resolved-but-not-yet-advanced window, and the switch payload/ends_at
// contract. DB-backed tests skip only when the dev DB is unreachable
// (same pattern as game_test.go).

// seededRNG returns a deterministic RNG (each call site uses a fresh
// seeded instance; a shared instance would make the assertions depend on
// call order).
func seededRNG(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, seed))
}

// TestAudiencePercentagesByTier verifies the SPEC §1 weight table: the
// CORRECT option's share is ~70% on levels 1–5, ~55% on 6–10, ~40% on
// 11–15; the four integers always sum to 100 and every entry is ≥ 0 —
// and on the easy tier the correct option is never below a wrong one
// (structurally guaranteed: the wrong shares split at most 30).
func TestAudiencePercentagesByTier(t *testing.T) {
	cases := []struct {
		name  string
		level int
		want  int
	}{
		{"easy-1", 1, 70}, {"easy-5", 5, 70},
		{"medium-6", 6, 55}, {"medium-10", 10, 55},
		{"hard-11", 11, 40}, {"hard-15", 15, 40},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for seed := uint64(1); seed <= 50; seed++ {
				p := audiencePercentages(c.level, 2, seededRNG(seed))
				if len(p) != 4 {
					t.Fatalf("percentages length %d, want 4", len(p))
				}
				sum := 0
				for i, v := range p {
					if v < 0 {
						t.Fatalf("seed %d: entries must be >= 0, got %d at index %d: %v", seed, v, i, p)
					}
					sum += v
				}
				if sum != 100 {
					t.Fatalf("seed %d: sum = %d, want 100 (%v)", seed, sum, p)
				}
				if p[2] != c.want {
					t.Fatalf("seed %d: correct-option weight = %d, want %d (%v)", seed, p[2], c.want, p)
				}
				if c.level <= 5 {
					for i, v := range p {
						if i != 2 && v > p[2] {
							t.Fatalf("seed %d: easy tier wrong option %d (%d) above the correct option (%d): %v",
								seed, i, v, p[2], p)
						}
					}
				}
			}
		})
	}
}

// TestFiftyFiftyExcludesCorrect pins the fifty_fifty contract: exactly
// two distinct indices, neither is the correct option's index, regardless
// of which index is correct (deterministic via seeded RNG).
func TestFiftyFiftyExcludesCorrect(t *testing.T) {
	for correct := 0; correct < 4; correct++ {
		q := levelQuestion{correctIndex: correct}
		for i := 0; i < 40; i++ {
			removed := removeTwoWrongLocked(q, seededRNG(uint64(1000*i+correct)))
			if len(removed) != 2 {
				t.Fatalf("correct %d: removed %v, want exactly 2 indices", correct, removed)
			}
			seen := map[int]bool{}
			for _, idx := range removed {
				if idx == correct {
					t.Fatalf("correct %d: removed_indices include the correct index: %v", correct, removed)
				}
				if seen[idx] {
					t.Fatalf("correct %d: removed_indices duplicated: %v", correct, removed)
				}
				seen[idx] = true
			}
			// Sorted ascending → stable presentation.
			if removed[0] > removed[1] {
				t.Fatalf("correct %d: removed_indices not sorted: %v", correct, removed)
			}
		}
	}
}

// TestLifelinesFiftyFiftyFlow is the fifty_fifty service flow: use →
// lifeline_used broadcast to the ROOM with two wrong indices →
// lifelines_left persisted without the spent lifeline → reuse 409 with an
// error event on that socket → unknown lifeline 400.
func TestLifelinesFiftyFiftyFlow(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "solo", 1)

	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}
	player := reloadPlayer(t, ctx, players, ps[0])

	data, err := g.Lifeline(ctx, room.ID, &player, "fifty_fifty")
	if err != nil {
		t.Fatalf("fifty_fifty: %v", err)
	}
	removed, ok := data["removed_indices"].([]int)
	if !ok || len(removed) != 2 {
		t.Fatalf("fifty_fifty data = %v", data)
	}
	correct := shuffledCorrectIndex(t, g, room.ID, 1)
	for _, idx := range removed {
		if idx == correct {
			t.Fatalf("removed_indices %v include the correct index %d", removed, correct)
		}
	}

	ev := n.eventData("lifeline_used", 0)
	if ev == nil {
		t.Fatal("no lifeline_used broadcast")
	}
	if ev["seat"] != 1 || ev["lifeline"] != "fifty_fifty" {
		t.Fatalf("lifeline_used = %v", ev)
	}
	if r, ok := ev["data"].(map[string]interface{}); !ok {
		t.Fatalf("lifeline_used data = %T", ev["data"])
	} else if _, has := r["removed_indices"]; !has {
		t.Fatalf("lifeline_used data missing removed_indices: %v", r)
	}

	// Persisted: the spent lifeline is gone from the jsonb array.
	p1 := reloadPlayer(t, ctx, players, ps[0])
	var left []string
	if err := json.Unmarshal([]byte(p1.LifelinesLeft), &left); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(left) != "[audience switch]" {
		t.Fatalf("lifelines_left after fifty_fifty = %v", left)
	}

	// Second use → 409 + error event to that socket.
	fresh := reloadPlayer(t, ctx, players, ps[0])
	if _, uerr := g.Lifeline(ctx, room.ID, &fresh, "fifty_fifty"); uerr == nil {
		t.Fatal("second fifty_fifty accepted")
	} else if ge, ok := uerr.(*GameError); !ok || ge.Status != 409 {
		t.Fatalf("second fifty_fifty error = %v, want GameError 409", uerr)
	}
	if evErr := n.eventData("error", 1); evErr == nil {
		t.Fatal("no error event for the reuse rejection")
	}

	// Unknown lifeline → 400 (+ error event).
	if _, uerr := g.Lifeline(ctx, room.ID, &fresh, "phone_a_friend"); uerr == nil {
		t.Fatal("unknown lifeline accepted")
	} else if ge, ok := uerr.(*GameError); !ok || ge.Status != 400 {
		t.Fatalf("unknown lifeline error = %v, want GameError 400", uerr)
	}

	// Leak scan: no correct_index may ever appear in lifeline events.
	for _, e := range n.eventsOf("lifeline_used", -1) {
		if scanLeak(e.msg, "", false) {
			t.Fatalf("lifeline_used leaks correct_index: %v", e.msg)
		}
	}
}

// TestLifelinesAudienceFlow uses the audience poll on a level-1 easy
// question: percentages in option order, 4 entries summing to 100, the
// correct option at the easy weight 70 (seeded RNG service).
func TestLifelinesAudienceFlow(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	g.SetRNG(seededRNG(42))
	room, ps := createTestRoom(t, ctx, db, rooms, players, "solo", 1)

	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}
	player := reloadPlayer(t, ctx, players, ps[0])

	data, err := g.Lifeline(ctx, room.ID, &player, "audience")
	if err != nil {
		t.Fatalf("audience: %v", err)
	}
	percentages, ok := data["percentages"].([]int)
	if !ok || len(percentages) != 4 {
		t.Fatalf("audience data = %v", data)
	}
	sum := 0
	for _, v := range percentages {
		sum += v
	}
	if sum != 100 {
		t.Fatalf("percentages sum = %d (%v), want 100", sum, percentages)
	}
	if correct := shuffledCorrectIndex(t, g, room.ID, 1); percentages[correct] != 70 {
		t.Fatalf("easy-tier correct weight = %d, want 70 (%v)", percentages[correct], percentages)
	}

	if ev := n.eventData("lifeline_used", 0); ev == nil || ev["lifeline"] != "audience" {
		t.Fatalf("lifeline_used = %v", ev)
	}

	// Persisted.
	p1 := reloadPlayer(t, ctx, players, ps[0])
	var left []string
	if err := json.Unmarshal([]byte(p1.LifelinesLeft), &left); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(left) != "[fifty_fifty switch]" {
		t.Fatalf("lifelines_left after audience = %v", left)
	}
}

// TestLifelinesSwitchFlow covers the switch contract (wave 5 per-seat swap
// engine): the CALLING SEAT's active question becomes the persisted
// variant-2 alternate of the same level (text and shuffled options), ends_at
// is NOT touched, the timer is unaffected, a `question_swapped` event goes to
// that seat ONLY (and the room `lifeline_used` carries NO question), the
// second switch is rejected, and scoring follows the NEW correct index.
func TestLifelinesSwitchFlow(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "solo", 1)

	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	_, before, _ := g.Snapshot(room.ID, ps[0].Seat)
	player := reloadPlayer(t, ctx, players, ps[0])

	data, err := g.Lifeline(ctx, room.ID, &player, "switch")
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	q, ok := data["question"].(QuestionView)
	if !ok || q.Level != 1 || len(q.Options) != 4 {
		t.Fatalf("switch data = %v", data)
	}

	// The replacement IS the persisted variant-2 realization.
	var row struct {
		Question string `ksql:"question"`
		Options  string `ksql:"shuffled_options"`
	}
	if err := db.QueryOne(ctx, &row,
		"SELECT questions.question AS question, rq.shuffled_options AS shuffled_options"+
			" FROM room_questions rq JOIN questions ON questions.id = rq.question_id"+
			" WHERE rq.room_id = $1 AND rq.level = 1 AND rq.variant = 2",
		room.ID); err != nil {
		t.Fatalf("reading variant-2 row: %v", err)
	}
	var rowOptions []string
	if err := json.Unmarshal([]byte(row.Options), &rowOptions); err != nil {
		t.Fatal(err)
	}
	if q.Text != row.Question {
		t.Fatalf("switch question text %q, want the variant-2 question %q", q.Text, row.Question)
	}
	if fmt.Sprint(q.Options) != fmt.Sprint(rowOptions) {
		t.Fatalf("switch options %v, want the persisted variant-2 layout %v", q.Options, rowOptions)
	}

	// ends_at unchanged (no timer reset — SPEC §1).
	_, after, _ := g.Snapshot(room.ID, ps[0].Seat)
	if !before.Equal(*after) {
		t.Fatalf("switch changed ends_at: %v → %v", before, after)
	}
	if lvl := currentLevel(t, g, room.ID); lvl != 1 {
		t.Fatalf("switch changed the level: %d", lvl)
	}

	if ev := n.eventData("lifeline_used", 0); ev == nil || ev["lifeline"] != "switch" {
		t.Fatalf("lifeline_used = %v", ev)
	} else if d, ok := ev["data"].(map[string]interface{}); !ok || len(d) != 0 {
		t.Fatalf("the room lifeline_used must NOT carry the per-seat swap question: %v", ev["data"])
	}

	// Wave 5: the replacement reaches the caller through a PRIVATE
	// question_swapped event on its OWN seat — never a room broadcast.
	sw := n.eventData("question_swapped", ps[0].Seat)
	if sw == nil {
		t.Fatal("no question_swapped event on the swapping seat")
	}
	if _, leaked := sw["correct_index"]; leaked {
		t.Fatalf("question_swapped leaked correct_index: %v", sw)
	}
	if fmt.Sprint(sw["seat"]) != fmt.Sprint(ps[0].Seat) || fmt.Sprint(sw["level"]) != "1" {
		t.Fatalf("question_swapped seat/level = %v/%v", sw["seat"], sw["level"])
	}
	swq, ok := sw["question"].(QuestionView)
	if !ok || swq.Text != q.Text || fmt.Sprint(swq.Options) != fmt.Sprint(q.Options) {
		t.Fatalf("question_swapped payload = %v, want the same question the REST body returned", sw["question"])
	}

	// Second switch attempt → 409.
	fresh := reloadPlayer(t, ctx, players, ps[0])
	if _, uerr := g.Lifeline(ctx, room.ID, &fresh, "switch"); uerr == nil {
		t.Fatal("second switch accepted")
	} else if ge, ok := uerr.(*GameError); !ok || ge.Status != 409 {
		t.Fatalf("second switch error = %v, want GameError 409", uerr)
	}

	// Scoring follows the NEW correct index: answering the variant-2
	// correct index scores level 1 correctly.
	variant2Correct := -1
	var cidx struct {
		Idx int `ksql:"idx"`
	}
	if err := db.QueryOne(ctx, &cidx,
		"SELECT shuffled_correct_index AS idx FROM room_questions WHERE room_id = $1 AND level = 1 AND variant = 2", room.ID); err != nil {
		t.Fatal(err)
	}
	variant2Correct = cidx.Idx
	if variant2Correct != seatCorrectIndex(t, g, room.ID, 1, ps[0].Seat) {
		t.Fatalf("in-memory seat question disagrees with the persisted variant-2 layout (%d vs %d)",
			variant2Correct, seatCorrectIndex(t, g, room.ID, 1, ps[0].Seat))
	}
	// The shared primary is untouched by the swap (wave 5 per-seat): it
	// still matches the persisted variant-1 layout.
	var p1idx struct {
		Idx int `ksql:"idx"`
	}
	if err := db.QueryOne(ctx, &p1idx,
		"SELECT shuffled_correct_index AS idx FROM room_questions WHERE room_id = $1 AND level = 1 AND variant = 1", room.ID); err != nil {
		t.Fatal(err)
	}
	if p1idx.Idx != shuffledCorrectIndex(t, g, room.ID, 1) {
		t.Fatalf("the swap moved the shared primary (%d vs persisted variant-1 %d)",
			shuffledCorrectIndex(t, g, room.ID, 1), p1idx.Idx)
	}
	fresh = reloadPlayer(t, ctx, players, ps[0])
	outcome, aerr := g.Answer(ctx, room.ID, &fresh, 1, variant2Correct)
	if aerr != nil {
		t.Fatalf("answering after switch: %v", aerr)
	}
	if !outcome.Correct || outcome.Status != "active" {
		t.Fatalf("outcome after switch = %+v, want correct/active on the variant-2 index", outcome)
	}

	// The spent switch is gone from persistence; the other two remain.
	p1 := reloadPlayer(t, ctx, players, ps[0])
	var left []string
	if err := json.Unmarshal([]byte(p1.LifelinesLeft), &left); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(left) != "[fifty_fifty audience]" {
		t.Fatalf("lifelines_left after switch = %v", left)
	}
}

// TestLifelinesIndependentPerSeatAndRunEnd is the duel-side contract:
// each seat holds its OWN lifeline set (independent uses), the
// resolved-but-not-yet-advanced window rejects lifeline calls (M7
// reveal-beat prep), and a fallen player's run rejects every late call.
func TestLifelinesIndependentPerSeatAndRunEnd(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	g, n, rooms, players := newTestGame(t, db)
	room, ps := createTestRoom(t, ctx, db, rooms, players, "duel", 2)

	if err := g.StartGame(ctx, room, ps); err != nil {
		t.Fatal(err)
	}

	p1 := reloadPlayer(t, ctx, players, ps[0])
	p2 := reloadPlayer(t, ctx, players, ps[1])

	// Seat 1 uses fifty_fifty — seat 2's set is untouched (independent).
	if _, err := g.Lifeline(ctx, room.ID, &p1, "fifty_fifty"); err != nil {
		t.Fatalf("seat 1 fifty_fifty: %v", err)
	}
	if _, err := g.Lifeline(ctx, room.ID, &p2, "fifty_fifty"); err != nil {
		t.Fatalf("seat 2 fifty_fifty (independent sets): %v", err)
	}

	// Resolved-but-not-yet-advanced window: seat 1 answers, seat 2 is
	// still pending → seat 1's lifeline call lands in the window → 409.
	if _, err := g.Answer(ctx, room.ID, &p1, 1, shuffledCorrectIndex(t, g, room.ID, 1)); err != nil {
		t.Fatal(err)
	}
	if got := currentLevel(t, g, room.ID); got != 1 {
		t.Fatalf("level advanced while seat 2 was pending: %d", got)
	}
	fresh1 := reloadPlayer(t, ctx, players, ps[0])
	if _, werr := g.Lifeline(ctx, room.ID, &fresh1, "audience"); werr == nil {
		t.Fatal("lifeline accepted during the resolved-but-not-advanced window")
	} else if ge, ok := werr.(*GameError); !ok || ge.Status != 409 {
		t.Fatalf("window lifeline error = %v, want GameError 409", werr)
	}

	// Seat 2 answers level 1 → the room advances to level 2. Seat 2 then
	// falls there: their run ends and every later lifeline call on that
	// seat is rejected.
	p2 = reloadPlayer(t, ctx, players, ps[1])
	answerCorrectly(t, ctx, g, players, room.ID, p2)
	if got := currentLevel(t, g, room.ID); got != 2 {
		t.Fatalf("level = %d, want 2 after both resolved", got)
	}
	if lvl := answerWrong(t, ctx, g, players, room.ID, p2); lvl != 2 {
		t.Fatalf("seat 2 fell at %d, want 2", lvl)
	}

	// Seat 1 keeps climbing solo; the fallen seat 2 cannot use lifelines
	// (invariant 2).
	fallen, err := g.players.GetPlayerByToken(ctx, room.ID, ps[1].Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := g.Lifeline(ctx, room.ID, fallen, "switch"); werr == nil {
		t.Fatal("fallen player could use a lifeline")
	} else if ge, ok := werr.(*GameError); !ok || ge.Status != 409 || ge.Message != "your run has already ended" {
		t.Fatalf("fallen lifeline error = %v, want GameError 409 'run ended'", werr)
	}
	if evErr := n.eventData("error", 2); evErr == nil {
		t.Fatal("fallen player's socket got no error event")
	}

	// And the survivor (still active, unresolved at level 2) can still use
	// their remaining lifelines.
	p1 = reloadPlayer(t, ctx, players, ps[0])
	if _, err := g.Lifeline(ctx, room.ID, &p1, "audience"); err != nil {
		t.Fatalf("survivor audience: %v", err)
	}
}
