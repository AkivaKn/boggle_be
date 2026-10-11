package server

import (
	"github.com/gin-gonic/gin"
	"github.com/vingarcia/ksql"

	"boggle-api/internal/app"
	"boggle-api/internal/boggle/handler"
	"boggle-api/internal/boggle/repository"
	"boggle-api/internal/boggle/service"
)

// RoutePrefix is where this game mounts on the shared engine. One binary
// hosts both games; the prefix is what keeps their REST + WS trees apart.
const RoutePrefix = "boggle"

// Module is Boggle's contribution to the shared process. It builds its
// repositories/services/handler on the SHARED db and registers its routes on
// the SHARED engine — it never dials, never listens, never owns /health.
type Module struct {
	db          ksql.DB
	roomHandler *handler.RoomHandler
	strictLimit gin.HandlerFunc
}

// NewModule wires Boggle onto the shared database and origin allowlist.
func NewModule(db ksql.DB, origins []string, limiters app.Limiters) *Module {
	roomRepo := repository.NewRoomRepository(db)
	roomService := service.NewRoomService(roomRepo)
	return &Module{
		db:          db,
		roomHandler: handler.NewRoomHandler(roomService, origins),
		strictLimit: limiters.Strict,
	}
}

// RegisterRoutes mounts Boggle's route tree under its prefix.
func (m *Module) RegisterRoutes(engine *gin.Engine) {
	api := engine.Group("/api/" + RoutePrefix)
	{
		api.POST("/rooms/:id/boards", m.strictLimit, m.roomHandler.GenerateBoard)
	}

	ws := engine.Group("/ws/" + RoutePrefix)
	{
		ws.GET("/rooms", m.roomHandler.CreateAndJoinRoomWS)
		ws.GET("/rooms/:id", m.roomHandler.JoinRoomWS)
	}
}