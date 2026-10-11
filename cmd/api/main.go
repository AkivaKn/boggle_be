package main

import (
	"log"

	"boggle-api/internal/app"
	boggleserver "boggle-api/internal/boggle/server"
	millionaireserver "boggle-api/internal/millionaire/server"
)

// ONE binary, BOTH games (the M-A1 fold). This entrypoint is deliberately
// thin: everything shared — the DB pool, the gin engine, /health, CORS, the
// rate-limit policy, the origin allowlist — lives in internal/app; each game
// contributes only a module that mounts its own route tree under its prefix.
//
// The build path stays `./cmd/api/main.go`, which is what both the Dockerfile
// and the deploy hook already use — folding the games in must not require
// touching CI.
func main() {
	origins := app.AllowedOrigins()
	db := app.Connect()
	defer db.Close()

	engine := app.NewEngine(origins)
	limiters := app.NewLimiters()

	boggleserver.NewModule(db, origins, limiters).RegisterRoutes(engine)
	millionaireserver.NewModule(db, origins, limiters).RegisterRoutes(engine)

	addr := app.ListenAddr()
	log.Printf("Gin server starting on port %s (routes: /api/%s, /api/%s)",
		addr, boggleserver.RoutePrefix, millionaireserver.RoutePrefix)
	if err := engine.Run(addr); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}
}