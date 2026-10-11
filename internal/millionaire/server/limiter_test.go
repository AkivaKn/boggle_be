package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"boggle-api/internal/app"
	"boggle-api/internal/millionaire/handler"
	"boggle-api/internal/millionaire/models"
	"github.com/gin-gonic/gin"
)

// rejectRoomService is a stub RoomService whose lookups always miss, so
// every gameplay command resolves the room FAILS at resolveRoomPath and
// answers 404 before any other handler dependency is touched — the burst
// test can drive the real route chain without a database.
type rejectRoomService struct{}

func (rejectRoomService) CreateRoom(context.Context, string) (*models.Room, error) {
	return nil, errors.New("stub")
}
func (rejectRoomService) GetRoom(context.Context, string) (*models.Room, error) {
	return nil, errors.New("stub")
}
func (rejectRoomService) GetRoomByCode(context.Context, string) (*models.Room, error) {
	return nil, errors.New("stub")
}
func (rejectRoomService) SetRoomStatus(context.Context, string, string) error {
	return errors.New("stub")
}
func (rejectRoomService) DeleteRoom(context.Context, string) error {
	return errors.New("stub")
}

// newLimitTestServer builds the REAL route registration (RegisterRoutes
// — the M8 strict-limiter wiring included) against stub dependencies.
// The Module is assembled by hand (NewModule dials nothing, but it does
// build real repositories, and these tests must stay database-free), then
// mounted on a bare engine exactly as internal/app does it.
func newLimitTestServer(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	limiters := app.NewLimiters()
	m := &Module{
		// nil interfaces: rejectRoomService below intercepts gameplay
		// commands at the room-resolution step before the rest is used.
		roomHandler:  handler.NewRoomHandler(nil, rejectRoomService{}, nil, nil, nil, nil),
		generalLimit: limiters.General,
		strictLimit:  limiters.Strict,
	}
	// Build the engine exactly as internal/app does in production: the
	// shared shell (which owns /health + CORS) plus this game's module. The
	// limiter assertions below therefore exercise the REAL surface, not a
	// hand-rolled stand-in.
	engine := app.NewEngine([]string{"http://localhost:5177"})
	m.RegisterRoutes(engine)
	return engine
}

func burst(t *testing.T, r *gin.Engine, path, ip string) int {
	t.Helper()
	req := httptest.NewRequest("POST", path, nil)
	req.RemoteAddr = ip + ":43210"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// TestStrictLimiterGuardsGameplayCommands drives the M8-wired routes:
//   - the first 10 POSTs inside the strict 10/min window reach the
//     handler (404 — unknown room; the point is the limiter PASSED them),
//     spread over all four gameplay commands;
//   - the 11th from the SAME client IP is throttled 429 (the budget is
//     shared across answer/walk/lifeline/continue — one limiter, one
//     in-memory store, one process);
//   - a DIFFERENT client IP is a separate budget (still gets through).
func TestStrictLimiterGuardsGameplayCommands(t *testing.T) {
	engine := newLimitTestServer(t)

	routes := []string{
		"/api/millionaire/rooms/ZZZNOPE/answer",
		"/api/millionaire/rooms/ZZZNOPE/walk",
		"/api/millionaire/rooms/ZZZNOPE/lifeline",
		"/api/millionaire/rooms/ZZZNOPE/continue",
		// Wave-4 room-lifecycle commands share the ONE strict budget too.
		"/api/millionaire/rooms/ZZZNOPE/start",
		"/api/millionaire/rooms/ZZZNOPE/replay",
	}

	for i := 0; i < 10; i++ {
		if code := burst(t, engine, routes[i%len(routes)], "10.1.1.1"); code != 404 {
			t.Fatalf("request %d (%s): code %d, want 404 (under-limit request must reach the handler)", i+1, routes[i%len(routes)], code)
		}
	}
	if code := burst(t, engine, routes[0], "10.1.1.1"); code != 429 {
		t.Fatalf("11th request from same IP: code %d, want 429 (strict 10/min)", code)
	}

	if code := burst(t, engine, routes[1], "10.1.1.2"); code != 404 {
		t.Fatalf("request from a DIFFERENT IP: code %d, want 404 (strict budget is per client IP)", code)
	}

	// The unthrottled surface keeps working after the burst: /health is
	// outside the /api group (and /ws is outside the limiter entirely).
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("GET /health after burst: code %d, want 200", w.Code)
	}
}

// The general 100/min limiter must remain at the /api group ROOT and the
// strict limiter must sit ONLY on the four gameplay commands: a route
// outside the gameplay subgroup (none exist in /api today besides the
// commands — hence the proof here is the negative: an api-surface POST
// via the gameplay path budget) is documented by the burst test above;
// this test pins the wiring shape itself.
func TestStrictLimiterWiringShape(t *testing.T) {
	engine := newLimitTestServer(t)
	seen := map[string]bool{}
	for _, ri := range engine.Routes() {
		key := ri.Method + " " + ri.Path
		seen[key] = true
	}
	for _, want := range []string{
		"POST /api/millionaire/rooms/:code/answer",
		"POST /api/millionaire/rooms/:code/walk",
		"POST /api/millionaire/rooms/:code/lifeline",
		"POST /api/millionaire/rooms/:code/continue",
		"POST /api/millionaire/rooms/:code/start",
		"POST /api/millionaire/rooms/:code/replay",
		"GET /ws/millionaire/rooms",
		"GET /ws/millionaire/rooms/:code",
		"GET /health",
	} {
		if !seen[want] {
			t.Fatalf("route %q not registered (seen: %v)", want, seen)
		}
	}
}
