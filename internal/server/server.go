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
	"github.com/gin-gonic/contrib/secure"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/ulule/limiter/v3"
	mgin "github.com/ulule/limiter/v3/drivers/middleware/gin"
	"github.com/ulule/limiter/v3/drivers/store/memory"
	"github.com/vingarcia/ksql"
	"github.com/vingarcia/ksql/adapters/kpgx"
)

type Server struct {
	port        string
	engine      *gin.Engine
	db          ksql.DB
	roomHandler *handler.RoomHandler
	strictLimit gin.HandlerFunc
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
	generalRate := limiter.Rate{Limit: 100, Period: time.Minute}
	generalStore := memory.NewStore()
	generalMiddleware := mgin.NewMiddleware(limiter.New(generalStore, generalRate))

	strictRate := limiter.Rate{Limit: 10, Period: time.Minute}
	strictStore := memory.NewStore()
	strictMiddleware := mgin.NewMiddleware(limiter.New(strictStore, strictRate))

	router := gin.Default()
	router.Use(generalMiddleware)
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
	router.Use(secure.Secure(secure.Options{
		AllowedHosts:       []string{os.Getenv("BACKEND_URL"), "localhost:8080"},
		STSSeconds:         315360000,
		FrameDeny:          true,
		ContentTypeNosniff: true,
		BrowserXssFilter:   true,
	}))
	roomRepo := repository.NewRoomRepository(db)
	roomService := service.NewRoomService(roomRepo)
	roomHandler := handler.NewRoomHandler(roomService, origins)
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
		strictLimit: strictMiddleware,
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
