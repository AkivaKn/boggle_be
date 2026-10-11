package handler

import (
	"github.com/gorilla/websocket"
	"log"
	"sync"
)

// wsManager handles active websocket connections
type wsManager struct {
	mu    sync.RWMutex
	rooms map[string]map[*websocket.Conn]bool
}

func newWSManager() *wsManager {
	return &wsManager{
		rooms: make(map[string]map[*websocket.Conn]bool),
	}
}

func (m *wsManager) AddClient(roomID string, conn *websocket.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rooms[roomID] == nil {
		m.rooms[roomID] = make(map[*websocket.Conn]bool)
	}
	m.rooms[roomID][conn] = true
}

func (m *wsManager) RemoveClient(roomID string, conn *websocket.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if clients, ok := m.rooms[roomID]; ok {
		delete(clients, conn)
		if len(clients) == 0 {
			delete(m.rooms, roomID)
		}
	}
}

func (m *wsManager) Broadcast(roomID string, message interface{}) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for conn := range m.rooms[roomID] {
		err := conn.WriteJSON(message)
		if err != nil {
			log.Println("Broadcast Error:", err)
			conn.Close()
		}
	}
}
