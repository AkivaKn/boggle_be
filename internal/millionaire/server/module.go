package server

import (
	"github.com/gin-gonic/gin"
	"github.com/vingarcia/ksql"

	"boggle-api/internal/app"
	"boggle-api/internal/millionaire/handler"
	"boggle-api/internal/millionaire/repository"
	"boggle-api/internal/millionaire/service"
)

// RoutePrefix is where this game mounts on the shared engine. One binary
// hosts both games; the prefix is what keeps their REST + WS trees apart.
const RoutePrefix = "millionaire"

// Module is Millionaire's contribution to the shared process (the M-A1 fold
// of the standalone `millionaire-api` binary). It builds its repositories /
// services / handler on the SHARED db and registers its routes on the SHARED
// engine — it never dials, never listens, never owns /health, and its tables
// are `millionaire_`-prefixed so it can never collide with Boggle's.
type Module struct {
	db           ksql.DB
	roomHandler  *handler.RoomHandler
	strictLimit  gin.HandlerFunc
	generalLimit gin.HandlerFunc
}

// NewModule wires Millionaire onto the shared database and origin allowlist.
func NewModule(db ksql.DB, origins []string, limiters app.Limiters) *Module {
	roomRepo := repository.NewRoomRepository(db)
	roomService := service.NewRoomService(roomRepo)
	playerRepo := repository.NewPlayerRepository(db)
	questionRepo := repository.NewQuestionRepository(db)
	roomQuestionRepo := repository.NewRoomQuestionRepository(db)
	return &Module{
		db:           db,
		roomHandler:  handler.NewRoomHandler(db, roomService, playerRepo, questionRepo, roomQuestionRepo, origins),
		strictLimit:  limiters.Strict,
		generalLimit: limiters.General,
	}
}

// RegisterRoutes mounts Millionaire's route tree under its prefix.
func (m *Module) RegisterRoutes(engine *gin.Engine) {
	api := engine.Group("/api/" + RoutePrefix)
	{
		// Rate limiter stays on the REST group only — WS connections must
		// not be throttled by the 100/min general limit at handshake.
		api.Use(m.generalLimit)

		// Gameplay command subgroup (SPEC §2 routes). M8 wiring: the
		// rate-sensitive WRITE endpoints (answer/walk/lifeline/continue
		// — and since wave 4 the room-lifecycle writes start/replay)
		// each run behind the strict 10/min limiter — Boggle's pattern
		// (the strict limiter guards the per-body route). The strict
		// budget is PER CLIENT IP, held in a process-local
		// IN-MEMORY store (lost on restart, not shared at scale-out —
		// single-process deployment assumption, same as Boggle), and it
		// is SHARED across these SIX routes: one limiter, one store.
		// Everything else on /api keeps only the general 100/min; the WS
		// handshake stays unthrottled. Routes key on the SURFACED
		// room CODE since M7 (:code — the uuid stays the internal PK;
		// the resolver does CODE-FIRST with a UUID FALLBACK so clients
		// that only know the internal uuid (solo instant-play) keep
		// working — WS JOIN remains strictly code-keyed).
		cmds := api.Group("/rooms/:code")
		{
			cmds.POST("/answer", m.strictLimit, m.roomHandler.PostAnswer)
			cmds.POST("/walk", m.strictLimit, m.roomHandler.PostWalk)
			cmds.POST("/lifeline", m.strictLimit, m.roomHandler.PostLifeline)
			cmds.POST("/continue", m.strictLimit, m.roomHandler.PostContinue)
			cmds.POST("/start", m.strictLimit, m.roomHandler.PostStart)
			cmds.POST("/replay", m.strictLimit, m.roomHandler.PostReplay)
		}
	}

	ws := engine.Group("/ws/" + RoutePrefix)
	{
		ws.GET("/rooms", m.roomHandler.CreateRoomWS)
		ws.GET("/rooms/:code", m.roomHandler.JoinRoomWS)
	}
}