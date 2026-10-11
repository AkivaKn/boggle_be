package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"boggle-api/internal/millionaire/repository"
	"boggle-api/internal/millionaire/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/vingarcia/ksql"
	"github.com/vingarcia/ksql/adapters/kpgx"
)

// newWSTestServer boots a gin engine wired to the dev DB with the real WS
// + REST handlers (room_test.go skip pattern): tests skip when
// DATABASE_URL is unset/unreachable, e.g. on the host without the
// devcontainer up. The returned cleanup func deletes the rooms (and
// players) a test created.
func newWSTestServer(t *testing.T) (baseURL string, rooms repository.RoomRepository, game *service.GameService, db ksql.DB, cleanupRooms func(ids ...string)) {
	t.Helper()
	db, game, r, rooms, cleanupRooms := newGameTestServer(t, nil)
	gin.SetMode(gin.TestMode)
	srv := httptest.NewServer(r)
	t.Cleanup(func() { srv.Close(); db.Close() })
	return "ws" + strings.TrimPrefix(srv.URL, "http"), rooms, game, db, cleanupRooms
}

// newGameTestServer builds the full handler chain (POST answer/walk/
// lifeline/continue included) against the dev DB. budgets maps tier →
// duration overrides (used by timer-fallback tests); nil keeps the SPEC §1
// defaults. Returns the game service too so beat-focused tests can inject
// their own reveal beat (the harness default is beat 0 = the legacy
// synchronous advance — M7 pacing tests override it explicitly).
func newGameTestServer(t *testing.T, budgets map[string]time.Duration) (db ksql.DB, game *service.GameService, engine *gin.Engine, rooms repository.RoomRepository, cleanupRooms func(ids ...string)) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://devuser:devpassword@localhost:5436/millionaire?sslmode=disable"
	}
	db, err := kpgx.New(context.Background(), dsn, ksql.Config{})
	if err != nil {
		t.Skipf("dev DB unreachable (%v) — skipping integration test", err)
	}
	roomsRepo := repository.NewRoomRepository(db)
	h := NewRoomHandler(
		db,
		service.NewRoomService(roomsRepo),
		repository.NewPlayerRepository(db),
		repository.NewQuestionRepository(db),
		repository.NewRoomQuestionRepository(db),
		nil,
	)
	if budgets != nil {
		for tier, d := range budgets {
			h.game.SetBudgetForTier(tier, d)
		}
	}
	// Harness default: legacy synchronous advance — see the M7 harness note
	// on the func doc above.
	h.game.SetRevealBeat(0)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ws/rooms", h.CreateRoomWS)
	r.GET("/ws/rooms/:code", h.JoinRoomWS)
	r.POST("/api/rooms/:code/answer", h.PostAnswer)
	r.POST("/api/rooms/:code/walk", h.PostWalk)
	r.POST("/api/rooms/:code/lifeline", h.PostLifeline)
	r.POST("/api/rooms/:code/continue", h.PostContinue)
	r.POST("/api/rooms/:code/start", h.PostStart)
	r.POST("/api/rooms/:code/replay", h.PostReplay)

	cleanupRooms = func(ids ...string) {
		ctx := context.Background()
		for _, id := range ids {
			if _, err := db.Exec(ctx, "DELETE FROM millionaire_room_questions WHERE room_id = $1", id); err != nil {
				t.Logf("cleanup: deleting room_questions of %s: %v", id, err)
			}
			if _, err := db.Exec(ctx, "DELETE FROM millionaire_players WHERE room_id = $1", id); err != nil {
				t.Logf("cleanup: deleting players of %s: %v", id, err)
			}
			if _, err := db.Exec(ctx, "DELETE FROM millionaire_rooms WHERE id = $1", id); err != nil {
				t.Logf("cleanup: deleting room %s: %v", id, err)
			}
		}
	}
	return db, h.game, r, roomsRepo, cleanupRooms
}

// registerRoomCleanup deletes the room (and its players) when the test
// ends, so the dev DB stays clean across runs.
func registerRoomCleanup(t *testing.T, cleanupRooms func(ids ...string), roomIDs ...string) {
	t.Helper()
	ids := append([]string(nil), roomIDs...)
	t.Cleanup(func() { cleanupRooms(ids...) })
}

func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v (http %v)", url, err, resp)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// readEvent reads the next WS message within a deadline as a JSON object.
func readEvent(t *testing.T, conn *websocket.Conn) map[string]interface{} {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("reading event: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("event is not JSON: %v (%s)", err, data)
	}
	return m
}

func wantEvent(t *testing.T, m map[string]interface{}, event string) {
	t.Helper()
	if m["event"] != event {
		t.Fatalf("event = %v, want %q (payload %v)", m["event"], event, m)
	}
}

// assertQuestionPayload pins the UPDATED SPEC §2 uniform question shape:
// {level, text, options} with exactly 4 shuffled options and no leak of
// the correct index.
func assertQuestionPayload(t *testing.T, q map[string]interface{}, wantLevel int) {
	t.Helper()
	if q["level"] != float64(wantLevel) {
		t.Fatalf("question level = %v, want %d", q["level"], wantLevel)
	}
	text, ok := q["text"].(string)
	if !ok || text == "" {
		t.Fatalf("question text missing: %v", q)
	}
	opts, ok := q["options"].([]interface{})
	if !ok || len(opts) != 4 {
		t.Fatalf("question options = %v, want 4 strings", q["options"])
	}
	for _, o := range opts {
		if _, ok := o.(string); !ok {
			t.Fatalf("question options must be strings: %v", opts)
		}
	}
	unique := map[interface{}]bool{}
	for _, o := range opts {
		unique[o] = true
	}
	if len(unique) != 4 {
		t.Fatalf("question options must be distinct: %v", opts)
	}
	if _, has := q["correct_index"]; has {
		t.Fatalf("question payload leaks correct_index: %v", q)
	}
}

// roomCodeRe pins the surfaced duel code format (M7): WORD-WORD-DD with
// uppercase words and a 2-digit suffix from the unambiguous digits 2..9.
var roomCodeRe = regexp.MustCompile(`^[A-Z]{3,}-[A-Z]{3,}-[2-9][2-9]$`)

// TestJoinRoomWSContract reproduces the SPEC §2 join contract end to end:
// create (duel) → seat-2 join by the surfaced CODE (M7) → start; solo
// third-join rejection (close 4400 after an error event; solo rooms carry
// code is empty — so no code can name one — a uuid is equally unknown); token
// rejoin of the creator's seat by code.
func TestJoinRoomWSContract(t *testing.T) {
	base, rooms, _, _, cleanupRooms := newWSTestServer(t)
	httpBase := "http" + strings.TrimPrefix(base, "ws") // postJSON needs http(s)

	// --- 1. Create a duel room. ------------------------------------------
	creator := dialWS(t, base+"/ws/rooms?mode=duel&name=Alice")
	created := readEvent(t, creator)
	wantEvent(t, created, "room_created")
	roomID, ok := created["room_id"].(string)
	if !ok || roomID == "" {
		t.Fatalf("room_created has no room_id: %v", created)
	}
	registerRoomCleanup(t, cleanupRooms, roomID)
	if created["mode"] != "duel" {
		t.Fatalf("room_created mode = %v, want duel", created["mode"])
	}
	// M7: the surfaced code is present, in format WORD-WORD-DD, and is
	// NOT the internal uuid.
	code, ok := created["code"].(string)
	if !ok || !roomCodeRe.MatchString(code) {
		t.Fatalf("room_created code = %v, want WORD-WORD-DD format", created["code"])
	}
	if code == roomID {
		t.Fatalf("room_created code must not be the internal uuid: %q", code)
	}
	if created["seat"] != float64(1) {
		t.Fatalf("creator seat = %v, want 1", created["seat"])
	}
	token1, ok := created["token"].(string)
	if !ok || token1 == "" {
		t.Fatalf("room_created has no creator token: %v", created)
	}
	if _, err := uuid.Parse(token1); err != nil {
		t.Fatalf("token %q is not a uuid: %v", token1, err)
	}

	// --- 2. Second client joins token-less BY CODE: seat 2, no start. ----
	// Wave-4 deferred duel start: the join lands the room in the WAITING
	// lobby — player_joined + room_state, and NO game_started until the
	// host's POST /start.
	joiner := dialWS(t, base+"/ws/rooms/"+code+"?name=Bob")
	state := readEvent(t, joiner)
	wantEvent(t, state, "room_state")
	if state["seat"] != float64(2) {
		t.Fatalf("joiner seat = %v, want 2", state["seat"])
	}
	if state["mode"] != "duel" {
		t.Fatalf("room_state mode = %v, want duel", state["mode"])
	}
	if state["code"] != code {
		t.Fatalf("room_state code = %v, want the room's %q", state["code"], code)
	}
	// Self-view: the joiner's OWN token, echoed back.
	token2, ok := state["token"].(string)
	if !ok || token2 == "" || token2 == token1 {
		t.Fatalf("room_state token must be the joiner's own fresh token, got %v", state["token"])
	}
	// The lobby snapshot carries NO question (nothing has been dealt).
	if state["question"] != nil {
		t.Fatalf("lobby room_state must not carry a question: %v", state["question"])
	}

	// player_joined arrives, but game_started must NOT (deferred start).
	pj := readEvent(t, joiner)
	wantEvent(t, pj, "player_joined")
	if pj["name"] != "Bob" || pj["seat"] != float64(2) {
		t.Fatalf("player_joined = %v, want {Bob seat 2}", pj)
	}
	// The wave-4 QUIETNESS of this join (no game_started, nothing dealt)
	// is pinned stream-based in TestDuelDeferredStart; this test stays on
	// plain sequential reads (a read-timeout leaves a raw-deadline socket
	// unable to read again), so it goes straight to the host start.

	// The creator heard the same broadcast.
	pjCreator := readEvent(t, creator)
	wantEvent(t, pjCreator, "player_joined")

	// Room STILL in lobby in the DB.
	ctx := context.Background()
	gotRoom, err := rooms.GetRoom(ctx, roomID)
	if err != nil {
		t.Fatalf("reading room back: %v", err)
	}
	if gotRoom.Status != "lobby" {
		t.Fatalf("room status = %q, want 'lobby' after the seat-2 join (deferred duel start)", gotRoom.Status)
	}

	// The HOST flips the room: POST /start → game_started on BOTH
	// sockets and rooms.status → active (same M7 issue path as before).
	startCode, startBody := postJSON(t, httpBase, "/api/rooms/"+code+"/start", map[string]interface{}{"token": token1})
	if startCode != 200 || startBody["started"] != true {
		t.Fatalf("host start: http %d body %v, want 200 {started:true}", startCode, startBody)
	}
	gs := readEvent(t, joiner)
	wantEvent(t, gs, "game_started")
	// M3: game_started now carries the uniform question payload + ends_at.
	gsQ, ok := gs["question"].(map[string]interface{})
	if !ok {
		t.Fatalf("game_started must carry the question payload: %v", gs)
	}
	assertQuestionPayload(t, gsQ, 1)
	if _, has := gs["ends_at"]; !has {
		t.Errorf("game_started must carry ends_at: %v", gs)
	}
	gsCreator := readEvent(t, creator)
	wantEvent(t, gsCreator, "game_started")

	// Room flipped to active in the DB.
	gotRoom, err = rooms.GetRoom(ctx, roomID)
	if err != nil {
		t.Fatalf("reading room back: %v", err)
	}
	if gotRoom.Status != "active" {
		t.Fatalf("room status = %q, want active after the host start", gotRoom.Status)
	}

	// --- 3. Full room rejected: token-less third join on the duel room. --
	full := dialWS(t, base+"/ws/rooms/"+code+"?name=Late")
	errFull := readEvent(t, full)
	wantEvent(t, errFull, "error")
	if msg, _ := errFull["message"].(string); msg == "" {
		t.Fatalf("error event has no message: %v", errFull)
	}
	assertCloseCode(t, full, closeRejected)

	// --- 4. Fresh SOLO room: creator auto-starts; join-by-code rejected. -
	solo := dialWS(t, base+"/ws/rooms?mode=solo&name=Solo")
	sc := readEvent(t, solo)
	wantEvent(t, sc, "room_created")
	soloRoomID, ok := sc["room_id"].(string)
	if !ok || soloRoomID == "" {
		t.Fatalf("solo room_created has no room_id: %v", sc)
	}
	registerRoomCleanup(t, cleanupRooms, soloRoomID)
	// M7: solo rooms carry code '' — the SPEC surfaces codes for duels
	// only, so no segment can name a solo room.
	if scode, _ := sc["code"].(string); scode != "" {
		t.Fatalf("solo room_created code = %q, want ''", scode)
	}
	sgs := readEvent(t, solo)
	wantEvent(t, sgs, "game_started") // solo starts on its seat-1 join

	// The solo room is unreachable on the code route — both when naming it
	// by its internal uuid and with a made-up code; the uuid resolution
	// would weaken the "code is the only surfaced key" contract.
	intruder := dialWS(t, base+"/ws/rooms/"+soloRoomID+"?name=Intruder")
	errSolo := readEvent(t, intruder)
	wantEvent(t, errSolo, "error")
	if !strings.Contains(fmt.Sprint(errSolo["message"]), "not found") {
		t.Fatalf("solo join by uuid = %v, want a room-not-found error", errSolo)
	}
	assertCloseCode(t, intruder, closeRejected)
	missCode := dialWS(t, base+"/ws/rooms/MISSING-CODE-42?name=Late")
	errMiss := readEvent(t, missCode)
	wantEvent(t, errMiss, "error")
	if !strings.Contains(fmt.Sprint(errMiss["message"]), "not found") {
		t.Fatalf("miss-code join = %v, want a room-not-found error", errMiss)
	}
	assertCloseCode(t, missCode, closeRejected)

	// --- 5. Invalid token rejected too. ---------------------------------
	bad := dialWS(t, base+"/ws/rooms/"+code+"?token="+token1[1:])
	errBad := readEvent(t, bad)
	wantEvent(t, errBad, "error")
	assertCloseCode(t, bad, closeRejected)

	// --- 6. Rejoin with the creator's token → own snapshot again. --------
	// M2 fix: the rejoin REPLACES the still-open creator socket — the old
	// one is closed with close code 4500 ("replaced by reconnect"), the
	// fresh one receives the room_state snapshot.
	re := dialWS(t, base+"/ws/rooms/"+code+"?token="+token1)
	rstate := readEvent(t, re)
	wantEvent(t, rstate, "room_state")
	if rstate["seat"] != float64(1) {
		t.Fatalf("rejoin seat = %v, want 1", rstate["seat"])
	}
	if rstate["token"] != token1 {
		t.Fatalf("rejoin token = %v, want the creator's own %q", rstate["token"], token1)
	}
	// M3: the mid-question rejoin snapshot carries the active question in
	// the uniform shape + ends_at.
	rsQ, ok := rstate["question"].(map[string]interface{})
	if !ok {
		t.Fatalf("rejoin room_state mid-question must carry the question: %v", rstate)
	}
	assertQuestionPayload(t, rsQ, 1)
	if _, has := rstate["ends_at"]; !has {
		t.Errorf("rejoin room_state must carry ends_at mid-question: %v", rstate)
	}
	assertCloseCodeReason(t, creator, closeReplaced, "replaced by reconnect")
	// Public player view only: seat/name/status/amount/level_reached, no
	// tokens, no lifelines internals.
	playersRaw, ok := rstate["players"].([]interface{})
	if !ok || len(playersRaw) != 2 {
		t.Fatalf("room_state players = %v, want 2 entries", rstate["players"])
	}
	for i, raw := range playersRaw {
		p, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("players[%d] not an object: %v", i, raw)
		}
		for _, forbidden := range []string{"token", "id", "room_id", "lifelines_left"} {
			if _, has := p[forbidden]; has {
				t.Errorf("players[%d] leaks %q in the public view: %v", i, forbidden, p)
			}
		}
		if p["seat"] == nil || p["name"] == nil || p["status"] != "active" {
			t.Errorf("players[%d] missing public fields or wrong status: %v", i, p)
		}
	}
}

// assertCloseCode reads the next frame expecting a close with the given code.
func assertCloseCode(t *testing.T, conn *websocket.Conn, code int) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatalf("expected close with code %d, got a normal message", code)
	}
	if !websocket.IsCloseError(err, code) {
		t.Fatalf("close error = %v, want close code %d", err, code)
	}
}

// assertCloseCodeReason additionally pins the close frame's reason phrase.
func assertCloseCodeReason(t *testing.T, conn *websocket.Conn, code int, reason string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err := conn.ReadMessage()
	var ce *websocket.CloseError
	if err == nil || !errors.As(err, &ce) || ce.Code != code || ce.Text != reason {
		t.Fatalf("close error = %v, want close %d with reason %q", err, code, reason)
	}
}

// TestCreateRoomWSRejectsUnknownMode: mode is required and explicit.
func TestCreateRoomWSRejectsUnknownMode(t *testing.T) {
	base, _, _, _, _ := newWSTestServer(t)

	conn := dialWS(t, base+"/ws/rooms?mode=party&name=X")
	ev := readEvent(t, conn)
	wantEvent(t, ev, "error")
	if !strings.Contains(fmt.Sprint(ev["message"]), "mode") {
		t.Fatalf("error message should mention mode: %v", ev)
	}
	assertCloseCode(t, conn, closeRejected)
}

// TestRejoinKicksReplacedSocket pins the M2 rejoin-multiplexing decision:
// one live socket per seat. A token rejoin while the player's old socket is
// still open must kick the old one with close 4500 ("replaced by
// reconnect") and serve the fresh one a room_state snapshot.
func TestRejoinKicksReplacedSocket(t *testing.T) {
	base, _, _, _, cleanupRooms := newWSTestServer(t)
	httpBase := "http" + strings.TrimPrefix(base, "ws")

	creator := dialWS(t, base+"/ws/rooms?mode=duel&name=Alice")
	created := readEvent(t, creator)
	wantEvent(t, created, "room_created")
	roomID, ok := created["room_id"].(string)
	if !ok || roomID == "" {
		t.Fatalf("room_created has no room_id: %v", created)
	}
	registerRoomCleanup(t, cleanupRooms, roomID)
	code, _ := created["code"].(string)
	if !roomCodeRe.MatchString(code) {
		t.Fatalf("room_created code = %v, want WORD-WORD-DD", created["code"])
	}

	// Seat 2 joins normally by code and goes into the read loop.
	joiner := dialWS(t, base+"/ws/rooms/"+code+"?name=Bob")
	state := readEvent(t, joiner)
	wantEvent(t, state, "room_state")
	token2, ok := state["token"].(string)
	if !ok || token2 == "" {
		t.Fatalf("room_state has no token: %v", state)
	}
	readEvent(t, joiner) // player_joined
	// Wave-4: the seat-2 join stays quiet (deferred start is pinned in
	// TestDuelDeferredStart); the HOST's /start deals Q1 so the pre-rejoin
	// world holds a live game exactly like the pre-wave-4 flow.
	startCode, startBody := postJSON(t, httpBase, "/api/rooms/"+code+"/start",
		map[string]interface{}{"token": created["token"].(string)})
	if startCode != 200 || startBody["started"] != true {
		t.Fatalf("host start: http %d body %v, want 200 {started:true}", startCode, startBody)
	}
	readEvent(t, joiner) // game_started

	// Rejoin seat 2 with its token BY CODE while the original socket is
	// STILL open.
	re := dialWS(t, base+"/ws/rooms/"+code+"?token="+token2)
	rstate := readEvent(t, re)
	wantEvent(t, rstate, "room_state")
	if rstate["seat"] != float64(2) {
		t.Fatalf("rejoin seat = %v, want 2", rstate["seat"])
	}
	if rstate["token"] != token2 {
		t.Fatalf("rejoin token = %v, want the same player's %q", rstate["token"], token2)
	}
	// The replaced socket gets the specific close, not a network break.
	assertCloseCodeReason(t, joiner, closeReplaced, "replaced by reconnect")
}

// TestCreateRoomWSCleansUpOrphanRoom covers the M2 finding that CreateRoomWS
// persisted the room before the player insert: a player-insert failure used
// to leave an orphan room row behind. The failure is induced for real by
// wiring the handler with a player repo whose DB pool is already closed (the
// concrete repos have no error-injection seam); the room repository stays
// live, so the handler's cleanup DeleteRoom path is exercised end to end and
// the zero-player room count must return to its before value.
func TestCreateRoomWSCleansUpOrphanRoom(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://devuser:devpassword@localhost:5436/millionaire?sslmode=disable"
	}
	db, err := kpgx.New(context.Background(), dsn, ksql.Config{})
	if err != nil {
		t.Skipf("dev DB unreachable (%v) — skipping integration test", err)
	}
	defer db.Close()
	roomsRepo := repository.NewRoomRepository(db)

	broken, err := kpgx.New(context.Background(), dsn, ksql.Config{})
	if err != nil {
		t.Skipf("dev DB unreachable (%v) — skipping integration test", err)
	}
	broken.Close() // every subsequent player query now fails

	h := NewRoomHandler(
		broken,
		service.NewRoomService(roomsRepo),
		repository.NewPlayerRepository(broken),
		repository.NewQuestionRepository(broken),
		repository.NewRoomQuestionRepository(broken),
		nil,
	)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ws/rooms", h.CreateRoomWS)
	srv := httptest.NewServer(r)
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	// Baseline: how many rooms currently have zero players (the stray row
	// from the pre-fix code counts here if it were still present).
	zeroPlayerRooms := func() int {
		t.Helper()
		var counts []struct {
			N int `ksql:"count"`
		}
		if err := db.Query(context.Background(), &counts, "SELECT count(*) FROM millionaire_rooms r WHERE NOT EXISTS (SELECT 1 FROM millionaire_players p WHERE p.room_id = r.id)"); err != nil {
			t.Fatalf("counting zero-player rooms: %v", err)
		}
		if len(counts) != 1 {
			t.Fatalf("count query returned %d rows", len(counts))
		}
		return counts[0].N
	}
	before := zeroPlayerRooms()

	conn := dialWS(t, base+"/ws/rooms?mode=solo&name=Ghost")
	ev := readEvent(t, conn)
	wantEvent(t, ev, "error")
	assertCloseCode(t, conn, closeRejected)

	// The room row must be gone: zero-player count back to baseline.
	for i := 0; i < 20; i++ {
		if after := zeroPlayerRooms(); after == before {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("orphan room survived: zero-player rooms before=%d after=%d", before, zeroPlayerRooms())
}
