package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"boggle-api/internal/millionaire/models"
	"boggle-api/internal/millionaire/repository"
	"boggle-api/internal/millionaire/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/vingarcia/ksql"
)

// closeRejected distinguishes an application-level WS rejection (bad mode,
// unknown room, invalid token, room full) from a plain network failure
// (gorilla's synthetic 1006) — SPEC §2 join contract.
const closeRejected = 4400

// maxSeatsPerMode caps room occupancy: solo rooms hold exactly one player,
// duels two (SPEC §2). A zero lookup means "unknown mode".
var maxSeatsPerMode = map[string]int{"solo": 1, "duel": 2}

type RoomHandler struct {
	rooms          service.RoomService
	players        repository.PlayerRepository
	totals         repository.RoomTotalsRepository // wave-4B: session totals for the room_state snapshot
	game           *service.GameService
	ws             *wsManager
	allowedOrigins []string
	upgrader       websocket.Upgrader

	// joinMu serializes seat claiming: two concurrent token-less joins must
	// never grab the same seat, and two concurrent token-rejoins of the
	// same seat must never register two live sockets (M3 carry-over: the
	// rejoin's KickSeat+attach run as one critical section here). One
	// process-wide mutex around the (fast) claim critical section is
	// deliberately simple for a lobby-scale game; the DB player rows stay
	// the source of truth.
	joinMu sync.Mutex
}

func NewRoomHandler(
	db ksql.Provider,
	rooms service.RoomService,
	players repository.PlayerRepository,
	questions repository.QuestionRepository,
	roomQuestions repository.RoomQuestionRepository,
	origins []string,
) *RoomHandler {
	h := &RoomHandler{
		rooms:          rooms,
		players:        players,
		totals:         repository.NewRoomTotalsRepository(db),
		ws:             newWSManager(),
		allowedOrigins: origins,
	}
	h.game = service.NewGameService(db, rooms, players, questions, roomQuestions, h)

	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     h.checkOrigin,
	}

	return h
}

// Broadcast + SendToSeat make the handler the Notifier the GameService
// emits events through — events originate in the service, delivery stays
// in the handler's ws registry.
func (h *RoomHandler) Broadcast(roomID string, msg interface{}) { h.ws.Broadcast(roomID, msg) }
func (h *RoomHandler) SendToSeat(roomID string, seat int, msg interface{}) {
	h.ws.SendToSeat(roomID, seat, msg)
}

func (h *RoomHandler) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")

	if origin == "" {
		return true
	}

	for _, allowed := range h.allowedOrigins {
		if strings.TrimSuffix(origin, "/") == strings.TrimSuffix(allowed, "/") {
			return true
		}
	}

	log.Printf("WebSocket Rejected: Unauthorized Origin: %s", origin)
	return false
}

// CreateRoomWS handles GET /ws/rooms?mode=solo|duel&name=<n>. Mode is
// required and explicit — unknown modes are rejected with an error event +
// close 4400 (never defaulted). Creation always makes a FRESH room with the
// creator as seat 1; joining existing rooms is JoinRoomWS's job (by CODE
// since M7 — the joiner only ever learns the surfaced value).
func (h *RoomHandler) CreateRoomWS(c *gin.Context) {
	mode := c.Query("mode")
	if maxSeatsPerMode[mode] == 0 {
		h.upgradeAndReject(c, "unknown or missing mode (use mode=solo or mode=duel)")
		return
	}

	room, err := h.rooms.CreateRoom(c.Request.Context(), mode)
	if err != nil {
		log.Printf("CreateRoomWS: %v", err)
		h.upgradeAndReject(c, "failed to create room")
		return
	}

	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("CreateRoomWS: WS Upgrade Error:", err)
		return
	}
	defer conn.Close()

	player, err := h.insertPlayer(c.Request.Context(), room.ID, 1, playerName(c.Query("name"), 1))
	if err != nil {
		log.Printf("CreateRoomWS: %v", err)
		// The room row already exists — without a player it is an orphan
		// (M2 review finding 3): remove it before rejecting.
		if derr := h.rooms.DeleteRoom(c.Request.Context(), room.ID); derr != nil {
			log.Printf("CreateRoomWS: cleaning up orphan room %s: %v", room.ID, derr)
		}
		h.reject(conn, "failed to create player")
		return
	}

	h.attach(room.ID, conn, player.Seat)
	defer h.ws.RemoveClient(room.ID, conn)

	// M7: room_created carries the surfaced code — the generated
	// WORD-WORD-DD value for duels, '' for solo (codes are surfaced for
	// duels only; solo never shows room mechanics, SPEC v1.1).
	h.ws.SendTo(room.ID, conn, gin.H{
		"event":   "room_created",
		"room_id": room.ID,
		"code":    room.Code,
		"mode":    room.Mode,
		"token":   player.Token,
		"seat":    player.Seat,
	})

	// Solo rooms are ready the moment their seat-1 player exists (SPEC
	// v1.1 instant play). DUEL rooms no longer start here at all (wave-4
	// deferred start): JoinRoomWS never calls this helper — the duel
	// waits in the lobby for the host's POST /start, and startGameIfReady
	// only ever fires for solo. The duel branch is kept so the helper's
	// contract (start when the full complement is present) stays honest.
	h.startGameIfReady(c.Request.Context(), room, []models.Player{*player})

	h.readLoop(room.ID, conn)
}

// JoinRoomWS handles GET /ws/rooms/:code?token=&name= per SPEC §2 (M7:
// the route key is the SURFACED code, not the uuid — the uuid stays the
// internal PK; a uuid in the URL resolves nothing, by design). Resolution,
// then the join contract:
//   - a valid token belonging to a player of the room → rejoin that seat
//     (room_state only; no new roster row, no player_joined — the player
//     never left the roster, only their socket did). If a live socket for
//     that seat still exists it is KICKED first with close code 4500
//     ("replaced by reconnect") so a player never has two live sockets;
//   - no token → claim the next free seat (solo: seat 1 only; duel: 1→2;
//     full rooms are rejected);
//   - invalid token / unknown code / full room → error event then close
//     4400 so clients can tell rejection apart from network failure.
//
// Solo rooms are NOT joinable by code: their code is empty (duel-only
// surface), so no route segment can name one — the uuid in the URL also
// fails the code lookup (close 4400).
func (h *RoomHandler) JoinRoomWS(c *gin.Context) {
	code := c.Param("code")

	room, err := h.rooms.GetRoomByCode(c.Request.Context(), code)
	if err != nil {
		h.upgradeAndReject(c, "room not found")
		return
	}
	roomID := room.ID // internal PK; ws/game paths stay keyed on it

	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("JoinRoomWS: WS Upgrade Error:", err)
		return
	}
	defer conn.Close()

	token := c.Query("token")
	if token != "" {
		player, err := h.players.GetPlayerByToken(c.Request.Context(), roomID, token)
		if err != nil {
			h.reject(conn, "invalid token for room")
			return
		}
		// Rejoin decision (M2 review finding 2): one live socket per seat.
		// If the old socket is still connected, it is kicked with close
		// 4500 ("replaced by reconnect") before the new one is attached.
		// M3 carry-over (M2 round-2 review finding 1): kick+attach happen
		// as ONE joinMu critical section, so two concurrent token-rejoins
		// of the same seat cannot interleave "kick / attach" steps and
		// momentarily register two live sockets for one player.
		h.joinMu.Lock()
		h.ws.KickSeat(roomID, player.Seat)
		h.attach(roomID, conn, player.Seat)
		h.joinMu.Unlock()
		defer h.ws.RemoveClient(roomID, conn)
		h.sendRoomState(roomID, room, *player, conn)
		h.readLoop(roomID, conn)
		return
	}

	// Token-less join: claim the next free seat under the guard.
	h.joinMu.Lock()
	players, err := h.players.GetPlayersByRoom(c.Request.Context(), roomID)
	if err != nil {
		h.joinMu.Unlock()
		log.Printf("JoinRoomWS: %v", err)
		h.reject(conn, "failed to load room players")
		return
	}
	seat := nextFreeSeat(room.Mode, players)
	if seat == 0 {
		h.joinMu.Unlock()
		h.reject(conn, "room full")
		return
	}
	player, err := h.insertPlayer(c.Request.Context(), roomID, seat, playerName(c.Query("name"), seat))
	if err != nil {
		h.joinMu.Unlock()
		log.Printf("JoinRoomWS: %v", err)
		h.reject(conn, "failed to create player")
		return
	}
	h.joinMu.Unlock()

	h.attach(roomID, conn, player.Seat)
	defer h.ws.RemoveClient(roomID, conn)

	// The joiner's own snapshot is written BEFORE any room broadcast
	// (SPEC §2 event order for join).
	h.sendRoomState(roomID, room, *player, conn)

	h.ws.Broadcast(roomID, gin.H{
		"event": "player_joined",
		"name":  player.Name,
		"seat":  player.Seat,
	})

	// Wave-4 DEFERRED DUEL START: a seat-2 join does NOT deal anything —
	// no question is issued, no room_questions rows exist, and the room's
	// status stays `lobby` (that status is the waiting flag). The joiner
	// already received their room_state lobby snapshot above and the room
	// heard the player_joined. The first question (game_started with Q1 +
	// ends_at) is issued ONLY by the host's POST /api/rooms/:code/start →
	// GameService.StartDuel.

	h.readLoop(roomID, conn)
}

// attach registers the socket with its room (recording the player seat it
// speaks for) and applies inbound limits.
func (h *RoomHandler) attach(roomID string, conn *websocket.Conn, seat int) {
	h.ws.AddClient(roomID, conn, seat)
	conn.SetReadLimit(4096)
}

// readLoop parks the handler goroutine on the socket until the client
// disconnects (Boggle pattern). Inbound is unmarshaled lightly: malformed
// payloads are ignored silently, ping is the keepalive and ignored, and
// any other event is ignored — gameplay input is REST-only per SPEC §2
// (answer / walk / lifeline commands; decided at M3).
//
// DISCONNECT MID-DUEL (M4): a vanished socket is ONLY removed from the
// room registry here — the player's DB row stays `active` and their run
// keeps going. The per-level time.AfterFunc fallback in the GameService
// resolves their run on timeout (run_ended + opponent_reveal to the
// survivor), so the opponent is never stranded: the level advances when
// every active player resolved, and the disconnected player simply plays
// on a zero clock until the timer ends their run. A reconnecting player
// rejoins via token and gets room_state (kick-on-rejoin, M2).
func (h *RoomHandler) readLoop(roomID string, conn *websocket.Conn) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("readLoop: room %s: %v", roomID, err)
			}
			return // removal only — no broadcast storm on disconnect
		}

		var inbound struct {
			Event string `json:"event"`
		}
		if json.Unmarshal(data, &inbound) != nil {
			continue
		}
	}
}

// insertPlayer persists a new player row (uuid v7 token, active, default
// lifelines). Standing invariant (M0 review): every field is set explicitly
// because ksql INSERT sends them all.
func (h *RoomHandler) insertPlayer(ctx context.Context, roomID string, seat int, name string) (*models.Player, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generating player id: %w", err)
	}
	token, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generating player token: %w", err)
	}
	p := &models.Player{
		ID:            id.String(),
		RoomID:        roomID,
		Seat:          seat,
		Token:         token.String(),
		Name:          name,
		Status:        "active",
		LifelinesLeft: models.DefaultLifelinesLeft,
		Amount:        0,
		LevelReached:  0,
		CreatedAt:     time.Now().UTC(),
	}
	if err := h.players.InsertPlayer(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// startGameIfReady flips the room to active and broadcasts game_started
// when the room has its full complement of players. Since wave 4 this is
// reached ONLY by solo rooms (their single seat-1 player exists right
// after creation — SPEC v1.1 instant play): JoinRoomWS no longer calls it
// for a filled duel, because duels wait in the lobby for the host's
// start endpoint. The GameService does the selection, persistence (one
// tx: 30 room_questions rows + rooms.status) and the broadcast.
func (h *RoomHandler) startGameIfReady(ctx context.Context, room *models.Room, players []models.Player) {
	if len(players) < maxSeatsPerMode[room.Mode] {
		return
	}
	if err := h.game.StartGame(ctx, room, players); err != nil {
		log.Printf("startGameIfReady: room %s: %v", room.ID, err)
		return
	}
}

// sendRoomState writes the joiner's SELF-VIEW snapshot: their own token and
// seat (always present, including on rejoin), the room's public player list,
// and — when a question is active — the question with its ends_at plus the
// joiner's OWN fifty_fifty removals for the current question (removed_indices,
// M6: per-seat own view, SPEC §2 room_state row; absent when the seat has
// no 50:50 applied).
func (h *RoomHandler) sendRoomState(roomID string, room *models.Room, self models.Player, conn *websocket.Conn) {
	players := h.roomPlayers(roomID)
	views := make([]publicPlayerView, 0, len(players))
	for _, p := range players {
		views = append(views, publicPlayerView{
			Seat: p.Seat, Name: p.Name, Status: p.Status,
			Amount: p.Amount, LevelReached: p.LevelReached,
		})
	}
	question, endsAt, removedIndices := h.game.Snapshot(roomID, self.Seat)
	state := gin.H{
		"event":    "room_state",
		"room_id":  roomID,
		"code":     room.Code, // M7: the surfaced duel code ('' for solo)
		"mode":     room.Mode,
		"token":    self.Token, // the joiner's OWN token — never anyone else's
		"seat":     self.Seat,
		"players":  views,
		"question": question, // nil when no question is active
		"ends_at":  endsAt,
	}
	if len(removedIndices) > 0 {
		// Self-view only: this seat's own 50:50 removals (never anyone
		// else's; never the lifeline state of other seats).
		state["removed_indices"] = removedIndices
	}
	if totals := h.roomTotals(roomID); len(totals) > 0 {
		// Wave-4B: the room's persisted per-seat SESSION totals (games
		// played / won, total money, per-seat bests) — PUBLIC facts only,
		// attached ONLY when the room has history (any totals row; the
		// key is omitted entirely for a history-less room). They persist
		// across rematches, so this covers every snapshot moment: a duel
		// LOBBY (pre-start and post-rematch-waiting) carries them too so
		// the FE rematch card can render before the host starts the
		// rematch (FE consumer contract: wave-5c).
		state["totals"] = totals
	}
	h.ws.SendTo(roomID, conn, state)
}

// roomTotalsView is one `totals` entry of the room_state snapshot (wave
// 4B): the per-seat session census — public facts only, never tokens.
type roomTotalsView struct {
	Seat        int   `json:"seat"`
	GamesPlayed int   `json:"games_played"`
	GamesWon    int   `json:"games_won"`
	TotalMoney  int64 `json:"total_money"`
	BestLevel   int   `json:"best_level"`
	BestAmount  int   `json:"best_amount"`
}

// roomTotals loads the room's session totals for the room_state snapshot.
// An EMPTY result (a room with no completed runs yet) returns nil — the
// caller omits the `totals` key entirely rather than showing a blank
// array. Read errors degrade the same way (logged; no key) — a snapshot
// must never fail over an optional enrichment.
func (h *RoomHandler) roomTotals(roomID string) []roomTotalsView {
	rows, err := h.totals.GetRoomTotals(context.Background(), roomID)
	if err != nil {
		log.Printf("roomTotals: %v", err)
		return nil
	}
	views := make([]roomTotalsView, 0, len(rows))
	for _, r := range rows {
		views = append(views, roomTotalsView{
			Seat:        r.Seat,
			GamesPlayed: r.GamesPlayed,
			GamesWon:    r.GamesWon,
			TotalMoney:  r.TotalMoney,
			BestLevel:   r.BestLevel,
			BestAmount:  r.BestAmount,
		})
	}
	return views
}

// roomPlayers loads the roster for snapshots.
func (h *RoomHandler) roomPlayers(roomID string) []models.Player {
	players, err := h.players.GetPlayersByRoom(context.Background(), roomID)
	if err != nil {
		log.Printf("roomPlayers: %v", err)
		return nil
	}
	return players
}

// publicPlayerView is one players[] entry of room_state: seat/name/status/
// amount/level_reached only — no tokens, no lifelines internals (SPEC §2
// self-view contract).
type publicPlayerView struct {
	Seat         int    `json:"seat"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	Amount       int    `json:"amount"`
	LevelReached int    `json:"level_reached"`
}

// nextFreeSeat returns the smallest unclaimed seat (1..cap) or 0 when the
// room is full. Solo rooms cap at 1, duels at 2 (SPEC §2).
func nextFreeSeat(mode string, players []models.Player) int {
	claimed := map[int]bool{}
	for _, p := range players {
		claimed[p.Seat] = true
	}
	for seat := 1; seat <= maxSeatsPerMode[mode]; seat++ {
		if !claimed[seat] {
			return seat
		}
	}
	return 0
}

// playerName defaults an empty ?name= to "Player <seat>" so the lobby shows
// something usable for everyone.
func playerName(name string, seat int) string {
	if strings.TrimSpace(name) == "" {
		return fmt.Sprintf("Player %d", seat)
	}
	return name
}

// answerRequest is the POST answer body (SPEC §2): token is the REST
// identity, level must name the ACTIVE question (client-inferred levels
// are rejected), choice_index is 0..3.
type answerRequest struct {
	Token       string `json:"token"`
	Level       int    `json:"level"`
	ChoiceIndex int    `json:"choice_index"`
}

// walkRequest is the POST walk body (SPEC §2).
type walkRequest struct {
	Token string `json:"token"`
}

// lifelineRequest is the POST lifeline body (SPEC §2): token is the REST
// identity, lifeline one of the SPEC §1 names (fifty_fifty | audience |
// switch — anything else is a 400 with an error event to the socket).
type lifelineRequest struct {
	Token    string `json:"token"`
	Lifeline string `json:"lifeline"`
}

// continueRequest is the POST continue body (M7, SPEC §1 solo pacing):
// token is the REST identity.
type continueRequest struct {
	Token string `json:"token"`
}

// resolveRoomPath maps a REST :code route param to the internal room
// (M7). The surfaced code is the PRIMARY value on these routes (duels);
// but solo rooms carry an EMPTY code (codes are a duel-only surface,
// SPEC v1.1) and their client's only room identity is the internal uuid
// it received in room_created — so a failed code lookup falls back to
// the uuid. Lookup order cannot alias: real codes never collide with a
// uuid; unknown values answer a clean 404 JSON.
func (h *RoomHandler) resolveRoomPath(c *gin.Context) (*models.Room, bool) {
	param := c.Param("code")
	room, err := h.rooms.GetRoomByCode(c.Request.Context(), param)
	if err == nil {
		return room, true
	}
	room, err = h.rooms.GetRoom(c.Request.Context(), param)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "room not found"})
		return nil, false
	}
	return room, true
}

// PostAnswer handles POST /api/rooms/:code/answer — the scoring command.
// Validation → game-rule rejections → the answer_result payload (which MAY
// carry correct_index: the caller just answered — SPEC §2 invariant 1
// exception). Game-rule rejections answer REST with their GameError status
// AND relay an error event to the player's socket.
func (h *RoomHandler) PostAnswer(c *gin.Context) {
	room, ok := h.resolveRoomPath(c)
	if !ok {
		return
	}
	roomID := room.ID

	var req answerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	if req.Token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token is required"})
		return
	}
	if req.Level < 1 || req.Level > 15 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "level must be 1..15"})
		return
	}
	if req.ChoiceIndex < 0 || req.ChoiceIndex > 3 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "choice_index must be 0..3"})
		return
	}

	player, err := h.players.GetPlayerByToken(c.Request.Context(), roomID, req.Token)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid token for room"})
		return
	}

	outcome, err := h.game.Answer(c.Request.Context(), roomID, player, req.Level, req.ChoiceIndex)
	if gerr, ok := err.(*service.GameError); ok {
		h.gameReject(c, roomID, player.Seat, gerr)
		return
	}
	if err != nil {
		log.Printf("PostAnswer: room %s: %v", roomID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "scoring the answer failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"correct":       outcome.Correct,
		"correct_index": outcome.CorrectIndex, // SPEC-allowed: they just answered
		"amount":        outcome.Amount,
		"status":        outcome.Status,
	})
}

// PostWalk handles POST /api/rooms/:code/walk — walk away before answering
// the current question, locking in the last answered level's amount.
func (h *RoomHandler) PostWalk(c *gin.Context) {
	room, ok := h.resolveRoomPath(c)
	if !ok {
		return
	}
	roomID := room.ID

	var req walkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	if req.Token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token is required"})
		return
	}

	player, err := h.players.GetPlayerByToken(c.Request.Context(), roomID, req.Token)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid token for room"})
		return
	}

	amount, err := h.game.Walk(c.Request.Context(), roomID, player)
	if gerr, ok := err.(*service.GameError); ok {
		h.gameReject(c, roomID, player.Seat, gerr)
		return
	}
	if err != nil {
		log.Printf("PostWalk: room %s: %v", roomID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "walking away failed"})
		return
	}
	// No correct_index here — walk answers nothing (SPEC §2 invariant 1).
	c.JSON(http.StatusOK, gin.H{"status": "walked", "amount": amount})
}

// PostLifeline handles POST /api/rooms/:code/lifeline {token, lifeline}
// (M5, SPEC §2). The GameService validates and applies the lifeline
// server-side; all game-rule rejections (second use 409, after-run-end
// 409, unknown lifeline 400…) answer REST with their GameError status AND
// relay an error event to the player's socket. On success the REST body
// mirrors the lifeline_used data the room receives over WS, but the WS
// broadcast stays the sole source clients may trust (invariant 6: server-
// computed result data; there is no correct_index in it either way).
func (h *RoomHandler) PostLifeline(c *gin.Context) {
	room, ok := h.resolveRoomPath(c)
	if !ok {
		return
	}
	roomID := room.ID

	var req lifelineRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	if req.Token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token is required"})
		return
	}

	player, err := h.players.GetPlayerByToken(c.Request.Context(), roomID, req.Token)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid token for room"})
		return
	}

	data, err := h.game.Lifeline(c.Request.Context(), roomID, player, req.Lifeline)
	if gerr, ok := err.(*service.GameError); ok {
		h.gameReject(c, roomID, player.Seat, gerr)
		return
	}
	if err != nil {
		log.Printf("PostLifeline: room %s: %v", roomID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "using the lifeline failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":   "used",
		"lifeline": req.Lifeline,
		"data":     data,
	})
}

// PostContinue handles POST /api/rooms/:code/continue {token} (M7, SPEC §1
// solo hold-until-continue): the SOLO reveal beat ends only on the
// player's explicit continue. Enforcements here:
//   - unknown code → 404 {"error":"room not found"} (route param is the
//     surfaced code, never the uuid);
//   - invalid token → 403 (same contract as answer/walk/lifeline);
//   - anything but a solo room → 409 (duels auto-advance on the
//     server-scheduled 2.5s beat instead);
//   - premature continue inside the beat → 409 {"error":"too soon"}
//     (GameService.Continue's floor); repeated, already-continued and
//     post-game continues are idempotent 200 no-ops.
//
// On success the room's WS stream carries the question_next; the REST
// body mirrors it for convenience (uniform shape, never a correct_index).
func (h *RoomHandler) PostContinue(c *gin.Context) {
	room, ok := h.resolveRoomPath(c)
	if !ok {
		return
	}
	roomID := room.ID

	var req continueRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	if req.Token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token is required"})
		return
	}

	player, err := h.players.GetPlayerByToken(c.Request.Context(), roomID, req.Token)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid token for room"})
		return
	}

	if room.Mode != "solo" {
		c.JSON(http.StatusConflict, gin.H{"error": "continue is a solo-only endpoint (duels auto-advance on the reveal beat)"})
		return
	}

	advanced, err := h.game.Continue(c.Request.Context(), roomID, player)
	if gerr, ok := err.(*service.GameError); ok {
		h.gameReject(c, roomID, player.Seat, gerr)
		return
	}
	if err != nil {
		log.Printf("PostContinue: room %s: %v", roomID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "continuing failed"})
		return
	}
	body := gin.H{"continued": advanced}
	if advanced {
		question, endsAt, _ := h.game.Snapshot(roomID, player.Seat)
		body["question"] = question
		body["ends_at"] = endsAt
	}
	c.JSON(http.StatusOK, body)
}

// tokenRequest is the POST start/replay body: token is the REST identity.
type tokenRequest struct {
	Token string `json:"token"`
}

// PostStart handles POST /api/rooms/:code/start {token} (wave-4 deferred
// duel start). Enforcements:
//   - unknown code → 404; empty token → 400; invalid token → 403;
//   - solo rooms → 409 (solo starts instantly on creation — unchanged);
//   - non-host (seat 2) → 403 AND an error event relayed to that seat's
//     socket (both a joiner and an attacker learn their place politely);
//   - an incomplete duel (seat 2 not seated yet) → 409;
//   - a finished room → 409 (replay is the path back to a lobby);
//   - an already-running duel → 200 {"started":false} (idempotent
//     double-start no-op; no second game_started is ever broadcast).
//
// On success the REST body mirrors the game_started payload (uniform
// question shape + ends_at, never a correct_index) — the WS broadcast
// stays the clients' trustworthy source, exactly like continue/lifeline.
func (h *RoomHandler) PostStart(c *gin.Context) {
	room, ok := h.resolveRoomPath(c)
	if !ok {
		return
	}
	roomID := room.ID

	var req tokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	if req.Token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token is required"})
		return
	}

	player, err := h.players.GetPlayerByToken(c.Request.Context(), roomID, req.Token)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid token for room"})
		return
	}

	started, err := h.game.StartDuel(c.Request.Context(), room, player)
	if gerr, ok := err.(*service.GameError); ok {
		h.gameReject(c, roomID, player.Seat, gerr)
		return
	}
	if err != nil {
		log.Printf("PostStart: room %s: %v", roomID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "starting the duel failed"})
		return
	}
	body := gin.H{"started": started}
	if started {
		question, endsAt, _ := h.game.Snapshot(roomID, player.Seat)
		body["question"] = question
		body["ends_at"] = endsAt
	}
	c.JSON(http.StatusOK, body)
}

// PostReplay handles POST /api/rooms/:code/replay {token} (wave-4
// in-room rematch): a LAPSED seat re-arms the SAME room for a fresh run —
// no new room, no new code. Enforcements:
//   - unknown code → 404; empty token → 400; invalid token → 403;
//   - a live game or any seat still `active` (mid-run) → 409 AND an
//     error event to that seat's socket;
//   - a lobby-state room (never started, or a rematch already waiting
//     for the host's start) → 409;
//   - a concurrent double-replay → 200 {"replayed":false,"started":false}
//     no-op.
//
// Effects (GameService.Replay): fresh 15×2 selection via the current
// selection service, old room_questions layout replaced in ONE
// transaction, every player row reset (active/0/0/default lifelines),
// rematch_ready broadcast listing the seats. A DUEL then waits in the
// lobby for the host's start ({"replayed":true,"started":false}); a SOLO
// room instantly issues Q1 again ({"replayed":true,"started":true,
// question+ends_at}).
func (h *RoomHandler) PostReplay(c *gin.Context) {
	room, ok := h.resolveRoomPath(c)
	if !ok {
		return
	}
	roomID := room.ID

	var req tokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	if req.Token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token is required"})
		return
	}

	player, err := h.players.GetPlayerByToken(c.Request.Context(), roomID, req.Token)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid token for room"})
		return
	}

	replayed, started, err := h.game.Replay(c.Request.Context(), room, player)
	if gerr, ok := err.(*service.GameError); ok {
		h.gameReject(c, roomID, player.Seat, gerr)
		return
	}
	if err != nil {
		log.Printf("PostReplay: room %s: %v", roomID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "replaying the room failed"})
		return
	}
	body := gin.H{"replayed": replayed, "started": started}
	if started { // solo replay: the instant Q1 mirrors game_started
		question, endsAt, _ := h.game.Snapshot(roomID, player.Seat)
		body["question"] = question
		body["ends_at"] = endsAt
	}
	c.JSON(http.StatusOK, body)
}

// gameReject answers a game-rule rejection on the REST call AND relays the
// same message to the player's live socket as an error event (per the M3
// plan: REST 409 + error event for idempotency/late-answer conflicts).
func (h *RoomHandler) gameReject(c *gin.Context, roomID string, seat int, gerr *service.GameError) {
	if gerr == nil {
		return
	}
	c.JSON(gerr.Status, gin.H{"error": gerr.Message})
	h.ws.SendToSeat(roomID, seat, gin.H{"event": "error", "message": gerr.Message})
}

// upgradeAndReject upgrades only so the client can receive a structured
// error event, then closes with the rejection code (an un-upgraded HTTP
// fetch would see neither).
func (h *RoomHandler) upgradeAndReject(c *gin.Context, message string) {
	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("WS Upgrade Error:", err)
		return
	}
	defer conn.Close()
	h.reject(conn, message)
}

// reject sends the first-class error event (SPEC §2) followed by a close
// frame with a non-1006 code carrying the same message.
func (h *RoomHandler) reject(conn *websocket.Conn, message string) {
	h.sendError(conn, message)
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(closeRejected, message),
		time.Now().Add(time.Second))
}

// sendError writes {"event":"error","message":...} — the SPEC's first-class
// error event (bad request, room full, invalid token…).
func (h *RoomHandler) sendError(conn *websocket.Conn, message string) {
	_ = conn.WriteJSON(gin.H{"event": "error", "message": message})
}
