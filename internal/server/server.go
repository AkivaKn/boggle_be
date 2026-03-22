package server

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"boggle-api/internal/handler"
	"boggle-api/internal/repository"
	"boggle-api/internal/service"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/vingarcia/ksql"
	"github.com/vingarcia/ksql/adapters/kpgx"
)

type Server struct {
	port        string
	engine      *gin.Engine
	db          ksql.DB
	roomHandler *handler.RoomHandler
}

func NewServer() *Server {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = "postgres://devuser:devpassword@localhost:5435/boggle?sslmode=disable"
	}

	var db ksql.DB
	var err error
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		db, err = kpgx.New(ctx, connStr, ksql.Config{})
		if err == nil {
			break
		}
		log.Printf("Waiting for DB (attempt %d/5)...", i+1)
		time.Sleep(2 * time.Second)
	}

	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}

	router := gin.Default()

	allowedOriginsEnv := os.Getenv("ALLOWED_ORIGINS")
	origins := []string{"http://localhost:5173"} // Default fallback
	if allowedOriginsEnv != "" {
		origins = strings.Split(allowedOriginsEnv, ",")
	}

	router.Use(cors.New(cors.Config{
		AllowOrigins:     origins,
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	roomRepo := repository.NewRoomRepository(db)
	roomService := service.NewRoomService(roomRepo)
	roomHandler := handler.NewRoomHandler(roomService)
	port := os.Getenv("PORT")
	if port == "" {
		port = ":8080"
	} else {
		port = "0.0.0.0:" + port
	}
	srv := &Server{
		port:        port,
		engine:      router,
		db:          db,
		roomHandler: roomHandler,
	}

	srv.RegisterRoutes()

	return srv
}

func (s *Server) Start() error {
	log.Printf("Gin server starting on port %s", s.port)
	return s.engine.Run(s.port)
}

func (s *Server) Close() {
	log.Println("Closing database connection...")
	s.db.Close()
}
