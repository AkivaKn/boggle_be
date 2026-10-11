package handler

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"boggle-api/internal/boggle/models"
	"boggle-api/internal/boggle/service"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type RoomHandler struct {
	service        service.RoomService
	ws             *wsManager
	allowedOrigins []string
	upgrader       websocket.Upgrader
}

func NewRoomHandler(service service.RoomService, origins []string) *RoomHandler {
	h := &RoomHandler{
		service:        service,
		ws:             newWSManager(),
		allowedOrigins: origins,
	}

	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     h.checkOrigin,
	}

	return h
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

func (h *RoomHandler) GenerateBoard(c *gin.Context) {
	// M-A2: `:code` is the surfaced room code; the resolver is CODE-FIRST
	// with a uuid fallback so an existing creator link (which only ever knew
	// the uuid) keeps working.
	ref := c.Param("code")
	room, _, err := h.service.ResolveRef(c.Request.Context(), ref)
	if err != nil {
		log.Printf("Error resolving room %s: %v", ref, err)
		c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
		return
	}
	roomID := room.ID

	board, err := h.service.GenerateBoard(c.Request.Context(), roomID)
	if err != nil {
		log.Printf("Error generating board for room %s: %v", roomID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate board"})
		return
	}
	var cells []models.BoardCell
	if err := json.Unmarshal([]byte(board.Cells), &cells); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse board cells"})
		return
	}
	h.ws.Broadcast(roomID, gin.H{
		"event":   "new_board",
		"board":   cells,
		"ends_at": board.EndsAt,
	})

	c.JSON(http.StatusOK, board)
}

func (h *RoomHandler) CreateAndJoinRoomWS(c *gin.Context) {
	room, err := h.service.CreateRoom(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create room"})
		return
	}

	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("WS Upgrade Error:", err)
		return
	}
	defer func() {
		log.Println("JoinRoomWS: Closing connection for room", room.ID)
		conn.Close()
	}()

	h.ws.AddClient(room.ID, conn)
	defer h.ws.RemoveClient(room.ID, conn)

	conn.WriteJSON(gin.H{
		"event":   "room_created",
		"room_id": room.ID,
		// M-A2: the code is the shareable surface — the host reads it out
		// and the friend types it. The uuid stays internal.
		"code": room.Code,
	})

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			log.Println("JoinRoomWS: ReadMessage error:", err)
			break
		}
	}
}

func (h *RoomHandler) JoinRoomWS(c *gin.Context) {
	// M-A2: the WS join is strictly CODE-keyed (the route param is the code
	// a friend typed). No uuid fallback here on purpose — a mistyped code
	// must fail rather than silently resolving to a different room.
	code := c.Param("code")

	room, board, err := h.service.GetActiveGameByCode(c.Request.Context(), code)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
		return
	}
	roomID := room.ID

	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("WS Upgrade Error:", err)
		return
	}
	defer func() {
		log.Println("JoinRoomWS: Closing connection for room", roomID)
		conn.Close()
	}()

	h.ws.AddClient(roomID, conn)
	defer h.ws.RemoveClient(roomID, conn)

	state := gin.H{
		"event":   "room_state",
		"room_id": room.ID,
		// M-A2: echoed so a rejoin can confirm it is in the room it thinks.
		"code": room.Code,
	}

	if board != nil {
		var cells []models.BoardCell
		if err := json.Unmarshal([]byte(board.Cells), &cells); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse board cells"})
			return
		}
		state["board"] = cells
		state["ends_at"] = board.EndsAt
	}
	conn.SetReadLimit(4096)
	conn.WriteJSON(state)

	h.ws.Broadcast(roomID, gin.H{
		"event":   "player_joined",
		"message": "A player entered the lobby.",
	})

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			log.Println("JoinRoomWS: ReadMessage error:", err)
			break
		}
	}
}
