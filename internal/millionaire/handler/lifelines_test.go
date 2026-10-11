package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/vingarcia/ksql"
)

// TestDuelLifelineIntegration is the M5 scripted duel over the REAL
// handler chain (WS + REST): mid-question at level 1 the ACTIVE players
// use their lifelines — seat 1 fifty_fifty (removed_indices exclude the
// correct option, broadcast to BOTH sockets), seat 2 audience (4
// percentages summing to 100, the easy-tier correct weight 70) — reuse is
// rejected 409 + error event, and the WAVE-5 SWAP is PER-SEAT: seat 1
// swapping hands ONLY seat 1 a private `question_swapped` (seat 2's
// question object stays byte-identical, and the room `lifeline_used`
// broadcast carries no question at all). Each seat then answers ITS OWN
// question — seat 1 against variant 2, seat 2 against the shared variant-1
// primary — via the REST oracle. The two seats' lifeline sets consume
// independently, asserted against the players rows (lifelines_left jsonb
// round-trip).
func TestDuelLifelineIntegration(t *testing.T) {
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
	if !ok || tokenB == "" {
		t.Fatalf("room_state token = %v", state["token"])
	}
	// Wave-4: the duel starts on the HOST's start endpoint (deferred start).
	startDuelHost(t, base, roomID, tokenA, a, b)

	lifeline := func(token, name string) (int, map[string]interface{}) {
		return postJSON(t, base, "/api/rooms/"+roomCode+"/lifeline", map[string]interface{}{
			"token": token, "lifeline": name,
		})
	}

	// --- seat 1: fifty_fifty at level 1 (mid-question) -------------------
	code, body := lifeline(tokenA, "fifty_fifty")
	if code != 200 {
		t.Fatalf("A fifty_fifty: http %d (%v)", code, body)
	}
	removed, ok := body["data"].(map[string]interface{})["removed_indices"].([]interface{})
	if !ok || len(removed) != 2 {
		t.Fatalf("fifty_fifty REST data = %v", body)
	}
	correct1 := correctIndexFromDB(t, db, roomID, 1)
	for _, r := range removed {
		if int(r.(float64)) == correct1 {
			t.Fatalf("removed_indices %v include the correct index %d", removed, correct1)
		}
	}
	for _, st := range []*wsStream{a, b} {
		ev := st.waitFor("lifeline_used", 5*time.Second)
		if ev == nil || ev["seat"] != float64(1) || ev["lifeline"] != "fifty_fifty" {
			t.Fatalf("lifeline_used on a peer = %v", ev)
		}
		if d, ok := ev["data"].(map[string]interface{}); !ok || d["removed_indices"] == nil {
			t.Fatalf("lifeline_used data = %v", ev["data"])
		}
	}

	// --- seat 2: audience on the same level (independent sets) -----------
	code, body = lifeline(tokenB, "audience")
	if code != 200 {
		t.Fatalf("B audience: http %d (%v)", code, body)
	}
	percentages, ok := body["data"].(map[string]interface{})["percentages"].([]interface{})
	if !ok || len(percentages) != 4 {
		t.Fatalf("audience REST data = %v", body)
	}
	sum := 0
	for _, v := range percentages {
		sum += int(v.(float64))
	}
	if sum != 100 {
		t.Fatalf("percentages sum = %d (%v), want 100", sum, percentages)
	}
	if percentages[correct1] != float64(70) {
		t.Fatalf("easy-tier correct weight = %v, want 70 (%v)", percentages[correct1], percentages)
	}
	for _, st := range []*wsStream{a, b} {
		ev := st.waitFor("lifeline_used", 5*time.Second)
		if ev == nil || ev["seat"] != float64(2) || ev["lifeline"] != "audience" {
			t.Fatalf("lifeline_used on a peer = %v", ev)
		}
	}

	// --- reuse: seat 2 audience again → 409 + error event to that socket -
	code, body = lifeline(tokenB, "audience")
	if code != http.StatusConflict {
		t.Fatalf("audience reuse: http %d (%v), want 409", code, body)
	}
	errEv := b.waitFor("error", 5*time.Second)
	if errEv == nil || errEv["message"] != "audience already used for this run" {
		t.Fatalf("reuse error event = %v", errEv)
	}

	// --- seat 1: swap — ONLY seat 1's own question changes ---------------
	// Wave 5: the replacement is per-seat. Seat 2's question object must be
	// byte-identical before and after, and the room broadcast must carry no
	// question at all.
	seat2Before := seatQuestionJSON(t, b, "game_started")
	variant2Text, variant2Options := variantRow(t, db, roomID, 1, 2)
	code, body = lifeline(tokenA, "switch")
	if code != 200 {
		t.Fatalf("A switch: http %d (%v)", code, body)
	}
	switchQ, ok := body["data"].(map[string]interface{})["question"].(map[string]interface{})
	if !ok {
		t.Fatalf("switch REST data = %v", body)
	}
	assertSwitchQuestion(t, switchQ, variant2Text, variant2Options, 1)

	// Seat 1's own socket gets the private per-seat event.
	swapped := a.waitFor("question_swapped", 5*time.Second)
	if swapped == nil || swapped["seat"] != float64(1) {
		t.Fatalf("A got no question_swapped: %v", swapped)
	}
	if _, leaked := swapped["correct_index"]; leaked {
		t.Fatalf("question_swapped leaked correct_index: %v", swapped)
	}
	swQ, ok := swapped["question"].(map[string]interface{})
	if !ok {
		t.Fatalf("question_swapped question = %T", swapped["question"])
	}
	assertQuestionPayload(t, swQ, 1)
	assertSwitchQuestion(t, swQ, variant2Text, variant2Options, 1)

	// The room broadcast still announces the lifeline use — with NO question.
	a.waitFor("lifeline_used", 5*time.Second)
	usedOnB := b.waitFor("lifeline_used", 5*time.Second)
	if usedOnB == nil || usedOnB["lifeline"] != "switch" {
		t.Fatalf("B never heard the switch lifeline was used: %v", usedOnB)
	}
	if d, isMap := usedOnB["data"].(map[string]interface{}); !isMap || len(d) != 0 {
		t.Fatalf("the room lifeline_used leaked the per-seat swap question: %v", usedOnB["data"])
	}

	// Seat 2's socket received NO per-seat event at all, and a token rejoin
	// reproduces its question object BYTE-IDENTICALLY: seat 1's swap never
	// touched seat 2's question.
	b.drainUntil(200 * time.Millisecond)
	for _, ev := range b.all {
		if ev["event"] == "question_swapped" {
			t.Fatalf("seat 2 received seat 1's private swap event: %v", ev)
		}
	}
	reB := dialStream(t, wsBase+"/ws/rooms/"+roomCode+"?token="+tokenB)
	if stateB := reB.waitFor("room_state", 5*time.Second); stateB == nil {
		t.Fatal("no room_state on seat 2's rejoin after seat 1's swap")
	} else if got := questionJSON(t, stateB); got != seat2Before {
		t.Fatalf("seat 2's question changed under seat 1's swap:\n before %s\n after  %s", seat2Before, got)
	}
	b = reB // the rejoin replaced seat 2's socket; carry on with the live one

	// Second switch attempt → 409 ("already used for this run").
	if code, body = lifeline(tokenA, "switch"); code != http.StatusConflict {
		t.Fatalf("switch reuse: http %d (%v), want 409", code, body)
	}

	// --- persistence: each seat consumed only its OWN lifelines ----------
	// seat 1 used fifty_fifty + switch → ["audience"]; seat 2 used
	// audience → ["fifty_fifty","switch"].
	assertLifelinesLeft(t, db, roomID, 1, []string{"audience"})
	assertLifelinesLeft(t, db, roomID, 2, []string{"fifty_fifty", "switch"})

	// --- each seat answers ITS OWN question ------------------------------
	// seat 1 is on the swapped variant-2 question, seat 2 is still on the
	// shared variant-1 primary: each scores against the layout ITS card
	// shows, and the level advances.
	variant2Correct := variantCorrect(t, db, roomID, 1, 2)
	variant1Correct := variantCorrect(t, db, roomID, 1, 1)
	for _, tt := range []struct {
		stream *wsStream
		token  string
		choice int
	}{
		{a, tokenA, variant2Correct},
		{b, tokenB, variant1Correct},
	} {
		code, errPost := postJSON(t, base, "/api/rooms/"+roomCode+"/answer", map[string]interface{}{
			"token": tt.token, "level": 1, "choice_index": tt.choice,
		})
		if code != 200 {
			t.Fatalf("answer: http %d (%v)", code, errPost)
		}
		if ar := tt.stream.waitFor("answer_result", 5*time.Second); ar == nil || ar["correct"] != true {
			t.Fatalf("answer_result = %v", ar)
		}
	}
	for _, st := range []*wsStream{a, b} {
		if qn := st.waitFor("question_next", 10*time.Second); qn == nil {
			t.Fatal("no question_next after both answered the switched question")
		}
	}

	// Leak discipline over both sockets' full history: fifty_fifty and the
	// switch payload must not carry a correct_index anywhere.
	a.drainUntil(200 * time.Millisecond)
	b.drainUntil(200 * time.Millisecond)
	a.leakScan(t)
	b.leakScan(t)
}

// questionJSON canonicalizes a question-bearing event's `question` object
// (encoding/json emits map keys sorted, so two canonicalizations of equal
// objects are byte-identical). Wave-5 per-seat swap evidence: the SAME
// seat's question must serialize identically before and after a PEER's swap.
func questionJSON(t *testing.T, ev map[string]interface{}) string {
	t.Helper()
	q, ok := ev["question"].(map[string]interface{})
	if !ok {
		t.Fatalf("event %v carries no question object (got %T)", ev["event"], ev["question"])
	}
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("marshaling the question object: %v", err)
	}
	return string(b)
}

// seatQuestionJSON returns the canonicalized question object of the LAST
// `event` of that kind this socket recorded (e.g. the game_started deal).
func seatQuestionJSON(t *testing.T, s *wsStream, event string) string {
	t.Helper()
	for i := len(s.all) - 1; i >= 0; i-- {
		if s.all[i]["event"] == event {
			return questionJSON(t, s.all[i])
		}
	}
	t.Fatalf("socket never recorded a %s event", event)
	return ""
}

// variantRow reads one persisted variant's question text + shuffled
// option layout (the M3 layout oracle — same realization the switch uses).
func variantRow(t *testing.T, db ksql.DB, roomID string, level, variant int) (string, []string) {
	t.Helper()
	var row struct {
		Question string `ksql:"question"`
		Options  string `ksql:"shuffled_options"`
	}
	if err := db.QueryOne(context.Background(), &row,
		"SELECT millionaire_questions.question AS question, millionaire_room_questions.shuffled_options AS shuffled_options FROM millionaire_room_questions JOIN millionaire_questions ON millionaire_questions.id = millionaire_room_questions.question_id WHERE millionaire_room_questions.room_id = $1 AND millionaire_room_questions.level = $2 AND millionaire_room_questions.variant = $3",
		roomID, level, variant); err != nil {
		t.Fatalf("reading variant row: %v", err)
	}
	var opts []string
	if err := json.Unmarshal([]byte(row.Options), &opts); err != nil {
		t.Fatal(err)
	}
	return row.Question, opts
}

// variantCorrect reads a persisted variant's shuffled correct index.
func variantCorrect(t *testing.T, db ksql.DB, roomID string, level, variant int) int {
	t.Helper()
	var row struct {
		Idx int `ksql:"idx"`
	}
	if err := db.QueryOne(context.Background(), &row,
		"SELECT shuffled_correct_index AS idx FROM millionaire_room_questions WHERE room_id = $1 AND level = $2 AND variant = $3",
		roomID, level, variant); err != nil {
		t.Fatalf("reading variant correct index: %v", err)
	}
	return row.Idx
}

// assertSwitchQuestion pins a switch payload's question: the requested
// level, exactly the variant layout read from the DB, no correct_index
// (assertQuestionPayload covers the latter at the WS call sites).
func assertSwitchQuestion(t *testing.T, q map[string]interface{}, wantText string, wantOptions []string, wantLevel int) {
	t.Helper()
	if fmt.Sprint(q["level"]) != fmt.Sprint(wantLevel) {
		t.Fatalf("switch question level = %v, want %d", q["level"], wantLevel)
	}
	if q["text"] != wantText {
		t.Fatalf("switch question text = %q, want the variant-2 question %q", q["text"], wantText)
	}
	if fmt.Sprint(q["options"]) != fmt.Sprint(wantOptions) {
		t.Fatalf("switch question options = %v, want the persisted layout %v", q["options"], wantOptions)
	}
}

// assertLifelinesLeft asserts a player's persisted jsonb lifelines_left
// (UpdateLifelinesLeft round-trip through the same-column table).
func assertLifelinesLeft(t *testing.T, db ksql.DB, roomID string, seat int, want []string) {
	t.Helper()
	var row struct {
		LL string `ksql:"ll"`
	}
	if err := db.QueryOne(context.Background(), &row,
		"SELECT lifelines_left AS ll FROM millionaire_players WHERE room_id = $1 AND seat = $2", roomID, seat); err != nil {
		t.Fatalf("reading lifelines_left (seat %d): %v", seat, err)
	}
	var got []string
	if err := json.Unmarshal([]byte(row.LL), &got); err != nil {
		t.Fatalf("lifelines_left (seat %d) = %q: %v", seat, row.LL, err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("seat %d lifelines_left = %v, want %v", seat, got, want)
	}
}

// TestLifelineSpendAtomicConcurrent pins the M5-review finding 2 fix:
// the lifeline consumption is a single guarded SQL spend, so N parallel
// identical POSTs yield EXACTLY ONE winner (one 200), 409s for the rest,
// exactly ONE lifeline_used broadcast per socket (the pre-M6 race
// broadcast 6 copies with per-copy recomputed data), and a persistently
// updated lifelines_left row.
func TestLifelineSpendAtomicConcurrent(t *testing.T) {
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
	if b.waitFor("room_state", 5*time.Second) == nil {
		t.Fatal("no room_state for seat 2")
	}
	// Wave-4: the duel starts on the HOST's start endpoint (deferred start).
	startDuelHost(t, base, roomID, tokenA, a, b)

	const racers = 8
	type outcome struct {
		code int
		body map[string]interface{}
	}
	codes := make([]outcome, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // fire truly in parallel against one fresh token row
			code, body := postJSON(t, base, "/api/rooms/"+roomCode+"/lifeline", map[string]interface{}{
				"token": tokenA, "lifeline": "fifty_fifty",
			})
			codes[i] = outcome{code, body}
		}(i)
	}
	close(start)
	wg.Wait()

	wins, conflicts := 0, 0
	for _, o := range codes {
		switch o.code {
		case http.StatusOK:
			wins++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected status %d in the race (%v)", o.code, o.body)
		}
	}
	if wins != 1 {
		t.Fatalf("atomic spend accepted %d of %d parallel POSTs, want exactly 1", wins, racers)
	}
	if conflicts != racers-1 {
		t.Fatalf("atomic spend rejected %d, want %d", conflicts, racers-1)
	}

	// Exactly ONE lifeline_used broadcast across each socket (drain the
	// trailing error events first — every loser got an error event on the
	// seat-1 socket ONLY, the seat-2 socket got none of those).
	a.drainUntil(300 * time.Millisecond)
	b.drainUntil(300 * time.Millisecond)
	usedOnA, usedOnB := 0, 0
	for _, ev := range a.all {
		if ev["event"] == "lifeline_used" {
			usedOnA++
		}
	}
	for _, ev := range b.all {
		if ev["event"] == "lifeline_used" {
			usedOnB++
		}
	}
	if usedOnA != 1 || usedOnB != 1 {
		t.Fatalf("lifeline_used broadcasts: seat1 socket %d, seat2 socket %d, want 1/1", usedOnA, usedOnB)
	}

	// The winner's data is server-computed and consistent (2 distinct
	// wrong indices — and both sockets saw the SAME pair, since the
	// broadcast was a single room message).
	pairA := lifelineUsedRemovals(a.all)
	pairB := lifelineUsedRemovals(b.all)
	if len(pairA) != 2 || len(pairB) != 2 {
		t.Fatalf("the winner/broadcast pair malformed: %v vs %v", pairA, pairB)
	}
	for i := range pairA {
		if pairA[i] != pairB[i] {
			t.Fatalf("the room broadcast data and the winner's data diverge: %v vs %v", pairA, pairB)
		}
	}

	// Persisted: fifty_fifty is gone from seat 1's row (atomic spend).
	assertLifelinesLeft(t, db, roomID, 1, []string{"audience", "switch"})
	a.leakScan(t)
	b.leakScan(t)
}

// lifelineUsedRemovals extracts the removals of the single fifty_fifty
// lifeline_used event recorded in a socket history.
func lifelineUsedRemovals(events []map[string]interface{}) []float64 {
	for _, ev := range events {
		if ev["event"] == "lifeline_used" && ev["lifeline"] == "fifty_fifty" {
			data, ok := ev["data"].(map[string]interface{})
			if !ok {
				continue
			}
			removed, ok := data["removed_indices"].([]interface{})
			if !ok {
				continue
			}
			var out []float64
			for _, r := range removed {
				out = append(out, r.(float64))
			}
			return out
		}
	}
	return nil
}

// TestFiftyFiftyRemovalsEchoInRoomState pins the M5-review finding 1 fix:
// the per-seat 50:50 removals live on the game and are echoed in the
// room_state SELF-VIEW (own removed_indices) — a mid-question rejoin must
// not resurrect removed options; the OTHER seat's self-view carries no
// one else's removals; a switch (or a new level) dies with the replaced
// question's removals.
func TestFiftyFiftyRemovalsEchoInRoomState(t *testing.T) {
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
	if b.waitFor("room_state", 5*time.Second) == nil {
		t.Fatal("no room_state for seat 2")
	}
	// Wave-4: the duel starts on the HOST's start endpoint (deferred start).
	startDuelHost(t, base, roomID, tokenA, a, b)

	// Apply seat 1's fifty_fifty.
	code, body := postJSON(t, base, "/api/rooms/"+roomCode+"/lifeline", map[string]interface{}{
		"token": tokenA, "lifeline": "fifty_fifty",
	})
	if code != http.StatusOK {
		t.Fatalf("A fifty_fifty: http %d (%v)", code, body)
	}
	a.waitFor("lifeline_used", 5*time.Second) // consume the broadcast
	b.waitFor("lifeline_used", 5*time.Second)
	restRemoved := body["data"].(map[string]interface{})["removed_indices"]
	if restRemoved == nil {
		t.Fatalf("fifty_fifty data = %v", body)
	}

	// MID-QUESTION rejoin of seat 1: the self-view must echo the same pair.
	re := dialStream(t, wsBase+"/ws/rooms/"+roomCode+"?token="+tokenA)
	if re.waitFor("room_created", 1*time.Second) != nil {
		// (rejoins never get room_created — defensive no-op)
		_ = re
	}
	state := re.waitFor("room_state", 5*time.Second)
	if state == nil {
		t.Fatal("no room_state on rejoin")
	}
	if fmt.Sprint(state["removed_indices"]) != fmt.Sprint(restRemoved) {
		t.Fatalf("rejoin removed_indices = %v, want %v (removed options resurrected?)",
			state["removed_indices"], restRemoved)
	}

	// Seat 2's SELF-VIEW must NOT carry seat 1's removals.
	reB := dialStream(t, wsBase+"/ws/rooms/"+roomCode+"?token="+roomStateToken(t, base, roomID, b))
	_ = reB // replaced below: B rejoins by its own token from the initial state
	// (b's token was consumed from the very first room_state — re-dial by it)
	if reB != nil {
		stateB := reB.waitFor("room_state", 5*time.Second)
		if stateB == nil {
			t.Fatal("no room_state on seat 2 rejoin")
		}
		if _, has := stateB["removed_indices"]; has {
			t.Fatalf("seat 2 self-view leaked seat 1's removals: %v", stateB["removed_indices"])
		}
	}

	// The switch replaces the ACTIVE question wholesale: removals die with
	// it — seat 1's NEXT self-view must be eviction-free.
	variant2Text, variant2Options := variantRow(t, db, roomID, 1, 2)
	code, body = postJSON(t, base, "/api/rooms/"+roomCode+"/lifeline", map[string]interface{}{
		"token": tokenA, "lifeline": "switch",
	})
	if code != http.StatusOK {
		t.Fatalf("A switch: http %d (%v)", code, body)
	}
	a.waitFor("lifeline_used", 5*time.Second)
	b.waitFor("lifeline_used", 5*time.Second)
	if sq := body["data"].(map[string]interface{})["question"].(map[string]interface{}); sq == nil {
		t.Fatalf("switch data = %v", body)
	} else {
		assertSwitchQuestion(t, sq, variant2Text, variant2Options, 1)
	}

	reA2 := dialStream(t, wsBase+"/ws/rooms/"+roomCode+"?token="+tokenA)
	stateA2 := reA2.waitFor("room_state", 5*time.Second)
	if stateA2 == nil {
		t.Fatal("no room_state after switch rejoin")
	}
	if _, has := stateA2["removed_indices"]; has {
		t.Fatalf("removals survived the switch in the self-view: %v", stateA2["removed_indices"])
	}
	a.leakScan(t)
	b.leakScan(t)
	re.leakScan(t)
	reA2.leakScan(t)
}

// roomStateToken re-dials seat 2 by its ORIGINAL token; the token was
// only ever delivered on the first room_state of the seat-2 stream.
func roomStateToken(t *testing.T, base, roomID string, s *wsStream) string {
	t.Helper()
	for _, ev := range s.all {
		if ev["event"] == "room_state" {
			if token, ok := ev["token"].(string); ok && token != "" {
				return token
			}
		}
	}
	t.Fatal("seat 2 token not found in its history")
	return ""
}
