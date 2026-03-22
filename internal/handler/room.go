package handler

import (
	"log"
	"net/http"

	"boggle-api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type RoomHandler struct {
	service service.RoomService
	ws      *wsManager
}

func NewRoomHandler(service service.RoomService) *RoomHandler {
	return &RoomHandler{
		service: service,
		ws:      newWSManager(),
	}
}

func (h *RoomHandler) GenerateBoard(c *gin.Context) {
	roomID := c.Param("id")

	board, err := h.service.GenerateBoard(c.Request.Context(), roomID)
	if err != nil {
		log.Printf("Error generating board for room %s: %v", roomID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate board"})
		return
	}

	h.ws.Broadcast(roomID, gin.H{
		"event":   "new_board",
		"board":   board.Letters,
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

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("WS Upgrade Error:", err)
		return
	}
	defer conn.Close()

	h.ws.AddClient(room.ID, conn)
	defer h.ws.RemoveClient(room.ID, conn)

	conn.WriteJSON(gin.H{
		"event":   "room_created",
		"room_id": room.ID,
	})

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}

func (h *RoomHandler) JoinRoomWS(c *gin.Context) {
	roomID := c.Param("id")

	room, board, err := h.service.GetActiveGame(c.Request.Context(), roomID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
		return
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("WS Upgrade Error:", err)
		return
	}
	defer conn.Close()

	h.ws.AddClient(roomID, conn)
	defer h.ws.RemoveClient(roomID, conn)

	state := gin.H{
		"event":   "room_state",
		"room_id": room.ID,
	}

	if board != nil {
		state["board"] = board.Letters
		state["ends_at"] = board.EndsAt
	}

	conn.WriteJSON(state)

	h.ws.Broadcast(roomID, gin.H{
		"event":   "player_joined",
		"message": "A player entered the lobby.",
	})

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}