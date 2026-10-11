package handler

import (
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// closeReplaced is the close code used when a rejoin kicks the player's
// still-connected old socket (M2 review finding 2: one live socket per
// seat — the new connection replaces the old one).
const closeReplaced = 4500

// writeTimeout bounds every WriteJSON under the manager-wide lock
// (M3 carry-over from the M2 round-2 review): a stuck peer gets at most
// 1s of the lock, never indefinite.
const writeTimeout = time.Second

// wsManager tracks active websocket connections per room (Boggle pattern,
// extended for per-player capable delivery): Broadcast reaches every
// connection in a room, SendTo reaches one player's socket — needed by
// SPEC §2 for private events like answer_result. Each conn is recorded
// with the seat of the player it speaks for so a rejoin can find and kick
// a replaced socket (O(n) scan over the room's conns is fine for ≤2).
type wsManager struct {
	mu    sync.RWMutex
	rooms map[string]map[*websocket.Conn]int // conn → player seat
}

func newWSManager() *wsManager {
	return &wsManager{
		rooms: make(map[string]map[*websocket.Conn]int),
	}
}

func (m *wsManager) AddClient(roomID string, conn *websocket.Conn, seat int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rooms[roomID] == nil {
		m.rooms[roomID] = make(map[*websocket.Conn]int)
	}
	m.rooms[roomID][conn] = seat
}

func (m *wsManager) RemoveClient(roomID string, conn *websocket.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if clients, ok := m.rooms[roomID]; ok {
		delete(clients, conn)
		if len(clients) == 0 {
			delete(m.rooms, roomID) // empty-room cleanup
		}
	}
}

// KickSeat closes every live socket of the room that carries the given seat
// with close code 4500 ("replaced by reconnect") and deregisters it — the
// rejoin path calls this before attaching the fresh socket, so a player
// never ends up with two live connections (M2 review finding 2 decision).
func (m *wsManager) KickSeat(roomID string, seat int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for conn, s := range m.rooms[roomID] {
		if s != seat {
			continue
		}
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(closeReplaced, "replaced by reconnect"),
			time.Now().Add(time.Second))
		delete(m.rooms[roomID], conn)
		conn.Close()
	}
	if len(m.rooms[roomID]) == 0 {
		delete(m.rooms, roomID)
	}
}

// Broadcast writes message to every connection in the room. A conn whose
// write fails is removed from the registry and closed here (its read loop
// ends on the closed socket anyway); dead conns must not accumulate and
// spam later broadcasts (M2 review finding 1). Every write runs under a
// 1s deadline (M3 carry-over): a stuck TCP peer must not freeze the
// manager-wide lock — and with it Add/Remove/Broadcast for ALL rooms.
func (m *wsManager) Broadcast(roomID string, message interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for conn, seat := range m.rooms[roomID] {
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err := conn.WriteJSON(message); err != nil {
			log.Printf("Broadcast Error: room %s conn (seat %d): %v", roomID, seat, err)
			delete(m.rooms[roomID], conn)
			conn.Close()
		}
	}
	if len(m.rooms[roomID]) == 0 {
		delete(m.rooms, roomID)
	}
}

// SendTo writes message to one specific connection in the room. A failed
// write removes the conn from the registry and closes it, same as
// Broadcast (M2 review finding 1). Writes under the same 1s deadline.
func (m *wsManager) SendTo(roomID string, conn *websocket.Conn, message interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rooms[roomID][conn]; !ok {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := conn.WriteJSON(message); err != nil {
		log.Printf("SendTo Error: room %s: %v", roomID, err)
		delete(m.rooms[roomID], conn)
		conn.Close()
	}
	if len(m.rooms[roomID]) == 0 {
		delete(m.rooms, roomID)
	}
}

// SendToSeat writes message to the live socket(s) of one player's seat —
// the per-player delivery needed by SPEC §2 private events
// (answer_result, error replies to a failed REST command). Unknown seat
// (socket already gone) is a silent no-op: REST still answers the caller.
// Writes under the same 1s deadline as Broadcast/SendTo.
func (m *wsManager) SendToSeat(roomID string, seat int, message interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for conn, s := range m.rooms[roomID] {
		if s != seat {
			continue
		}
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err := conn.WriteJSON(message); err != nil {
			log.Printf("SendToSeat Error: room %s seat %d: %v", roomID, seat, err)
			delete(m.rooms[roomID], conn)
			conn.Close()
		}
	}
	if len(m.rooms[roomID]) == 0 {
		delete(m.rooms, roomID)
	}
}
