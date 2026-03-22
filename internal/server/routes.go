package server

import "github.com/gin-gonic/gin"

func (s *Server) RegisterRoutes() {
	s.engine.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	api := s.engine.Group("/api")
	{
		api.POST("/rooms/:id/boards", s.strictLimit, s.roomHandler.GenerateBoard)
	}

	ws := s.engine.Group("/ws")
	{
		ws.GET("/rooms", s.roomHandler.CreateAndJoinRoomWS)
		ws.GET("/rooms/:id", s.roomHandler.JoinRoomWS)
	}
}
