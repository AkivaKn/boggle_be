// Command backfill assigns room codes to rooms created before migration
// 005_rooms_code.sql (M7 deploy safety): every room still carrying NULL
// or an empty code gets one — a real generated WORD-WORD-DD code for
// joinable duel rooms, empty for solo rooms and finished rooms (codes are a
// duel-only surface, SPEC v1.1). Fresh devcontainers never need it
// (001..005 auto-run and CreateRoom always sets a code); the command is
// for older volumes, run as `go run ./cmd/backfill` inside the container.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"boggle-api/internal/codes"
	"boggle-api/internal/dberr"

	"github.com/vingarcia/ksql"
	"github.com/vingarcia/ksql/adapters/kpgx"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://devuser:devpassword@db:5432/millionaire?sslmode=disable"
	}
	ctx := context.Background()
	db, err := kpgx.New(ctx, dsn, ksql.Config{})
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer db.Close()

	var rows []struct {
		ID     string `ksql:"id"`
		Mode   string `ksql:"mode"`
		Status string `ksql:"status"`
	}
	if err := db.Query(ctx, &rows,
		"SELECT id, mode, status FROM millionaire_rooms WHERE code IS NULL OR code = ''"); err != nil {
		log.Fatalf("listing codeless rooms: %v", err)
	}
	if len(rows) == 0 {
		fmt.Println("no rooms missing codes — nothing to do")
		return
	}

	// Only unfinished duel rooms really need a real code (a finished room
	// can never be joined again); everything else gets '' so no row leaves
	// the loop without a definitive value. Collisions retry with a fresh
	// candidate (the rooms.code unique index decides; same contract as
	// CreateRoom).
	var failed int
	for _, row := range rows {
		code := ""
		if row.Mode == "duel" && row.Status != "finished" {
			c, ok := generateFreeCode(ctx, db, row.ID)
			if !ok {
				log.Printf("room %s: could not find a free code, leaving blank", row.ID)
				failed++
				continue
			}
			code = c
		}
		if _, err := db.Exec(ctx,
			"UPDATE millionaire_rooms SET code = $1 WHERE id = $2 AND (code IS NULL OR code = '')",
			code, row.ID); err != nil {
			log.Printf("room %s: updating code: %v", row.ID, err)
			failed++
			continue
		}
		fmt.Printf("room %s (mode %s): code %q\n", row.ID, row.Mode, code)
	}
	if failed > 0 {
		log.Fatalf("backfill incomplete: %d rooms left without codes", failed)
	}
}

// generateFreeCode retries NewRoomCode updates for ONE room until a
// candidate survives the rooms.code unique index. ok = false only when
// the collision retry budget is exhausted (a fraction of the room-table
// name space — negligible) or the DB errors.
func generateFreeCode(ctx context.Context, db ksql.Provider, roomID string) (string, bool) {
	const maxAttempts = 10
	for attempt := 0; attempt < maxAttempts; attempt++ {
		code := codes.NewRoomCode()
		if _, err := db.Exec(ctx,
			"UPDATE millionaire_rooms SET code = $1 WHERE id = $2 AND (code IS NULL OR code = '')",
			code, roomID); err != nil {
			if dberr.IsUniqueViolation(err) {
				continue
			}
			log.Printf("room %s: code claim: %v", roomID, err)
			return "", false
		}
		return code, true
	}
	return "", false
}
