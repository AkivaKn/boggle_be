// Package app is the ONE process that hosts every game.
//
// It owns the three things that must be shared by construction, because
// splitting them is exactly how a two-app split breaks:
//
//   - the DATABASE CONNECTION (one pool; the games differ by TABLE PREFIX,
//     not by database — see migrations/), so a game's SQL can never
//     accidentally read another game's tables;
//   - the gin ENGINE (one listener, one /health, one CORS + rate-limit
//     policy) — each game only contributes its own route TREE under its
//     prefix;
//   - the ORIGIN ALLOWLIST, which feeds both the REST CORS middleware here
//     and each game's WS upgrader CheckOrigin (one env var, one list).
//
// A game module is `NewModule(db, origins)` + `RegisterRoutes(engine, prefix)`
// — it never builds an engine, never listens, never dials. That is what lets
// Boggle and Millionaire ship in one binary without either one owning the port.
package app

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/ulule/limiter/v3"
	mgin "github.com/ulule/limiter/v3/drivers/middleware/gin"
	"github.com/ulule/limiter/v3/drivers/store/memory"
	"github.com/vingarcia/ksql"
	"github.com/vingarcia/ksql/adapters/kpgx"
)

// Rate limits shared by both games (the values each game already used —
// this is a move, not a policy change):
//   - general: 100/min on each game's REST group;
//   - strict: 10/min on the rate-sensitive gameplay WRITES (Millionaire's
//     answer/walk/lifeline/continue/start/replay; Boggle's board generate).
// Both are process-local in-memory stores: counters reset on restart and are
// not shared across replicas (the single-process deployment assumption both
// games already documented).
const (
	generalLimitPerMinute = 100
	strictLimitPerMinute  = 10
)

// Limiters is the pair of gin middlewares a game module mounts on its own
// route groups. Built ONCE here so both games share the policy; each game
// still gets its OWN store (separate budgets — a Boggle client cannot spend
// a Millionaire player's answer budget).
type Limiters struct {
	General gin.HandlerFunc
	Strict  gin.HandlerFunc
}

// NewLimiters builds the two middlewares.
func NewLimiters() Limiters {
	return Limiters{
		General: mgin.NewMiddleware(limiter.New(memory.NewStore(),
			limiter.Rate{Limit: generalLimitPerMinute, Period: time.Minute})),
		Strict: mgin.NewMiddleware(limiter.New(memory.NewStore(),
			limiter.Rate{Limit: strictLimitPerMinute, Period: time.Minute})),
	}
}

// Connect opens the shared database pool, retrying while the DB boots (the
// devcontainer starts the app alongside Postgres).
func Connect() ksql.DB {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = "postgres://devuser:devpassword@localhost:5435/boggle?sslmode=disable"
	}

	ctx := context.Background()
	var (
		db  ksql.DB
		err error
	)
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
	return db
}

// AllowedOrigins reads the ALLOWED_ORIGINS allowlist. The default covers the
// Vite dev-server variants for both games' published dev ports.
func AllowedOrigins() []string {
	origins := []string{
		"http://localhost:5177", // Millionaire FE dev server
		"http://127.0.0.1:5177",
		"http://host.docker.internal:5177",
		"http://localhost:5173", // Boggle FE dev server
		"http://127.0.0.1:5173",
		"http://host.docker.internal:5173",
	}
	if env := os.Getenv("ALLOWED_ORIGINS"); env != "" {
		origins = strings.Split(env, ",")
	}
	return origins
}

// NewEngine builds the ONE engine: default logging/recovery + the shared CORS
// middleware (engine-level on purpose — see cors.go for why group-level would
// 404 every preflight).
func NewEngine(origins []string) *gin.Engine {
	engine := gin.Default()
	engine.Use(corsMiddleware(origins))
	engine.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})
	return engine
}

// ListenAddr resolves the bind address from PORT (Azure Container Apps passes
// PORT; locally it defaults to :8080).
func ListenAddr() string {
	port := os.Getenv("PORT")
	if port == "" {
		return ":8080"
	}
	return "0.0.0.0:" + port
}