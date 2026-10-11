package service

import (
	"context"
	"regexp"
	"testing"

	"boggle-api/internal/millionaire/repository"
)

// roomCodeFormatRe is the DB-facing code format (same contract the
// handler tests pin via handler.roomCodeRe).
var roomCodeFormatRe = regexp.MustCompile(`^[A-Z]{3,}-[A-Z]{3,}-[2-9][2-9]$`)

// TestCreateRoomCodesDuelsDistinctSoloEmpty runs the REAL CreateRoom and
// checks the M7 contract against the DB-backed unique index: every duel
// room gets a fresh distinct WORD-WORD-DD code (the generation + retry
// loop actually exercised), solo rooms carry an EMPTY code (codes are duel-only,
// SPEC v1.1) and multiple solo rooms never collide on the partial unique
// index.
func TestCreateRoomCodesDuelsDistinctSoloEmpty(t *testing.T) {
	ctx := context.Background()
	db := newServiceTestDB(t)
	rooms := NewRoomService(repository.NewRoomRepository(db))

	seen := map[string]bool{}
	const duels = 5
	for i := 0; i < duels; i++ {
		room, err := rooms.CreateRoom(ctx, "duel")
		if err != nil {
			t.Fatalf("creating duel room %d: %v", i, err)
		}
		t.Cleanup(func() {
			dctx := context.Background()
			_, _ = db.Exec(dctx, "DELETE FROM millionaire_rooms WHERE id = $1", room.ID)
		})
		if !roomCodeFormatRe.MatchString(room.Code) {
			t.Fatalf("duel room %d code = %q, want WORD-WORD-DD", i, room.Code)
		}
		if seen[room.Code] {
			t.Fatalf("duplicate room code %q across %d duel rooms", room.Code, duels)
		}
		seen[room.Code] = true
	}

	// Solo rooms: code '', and two of them coexist (the partial unique
	// index excludes ''); the DB row really scans back as ''.
	for i := 0; i < 2; i++ {
		room, err := rooms.CreateRoom(ctx, "solo")
		if err != nil {
			t.Fatalf("creating solo room %d: %v", i, err)
		}
		t.Cleanup(func() {
			dctx := context.Background()
			_, _ = db.Exec(dctx, "DELETE FROM millionaire_rooms WHERE id = $1", room.ID)
		})
		if room.Code != "" {
			t.Fatalf("solo room %d code = %q, want ''", i, room.Code)
		}
		got, err := rooms.GetRoom(ctx, room.ID)
		if err != nil {
			t.Fatalf("reading solo room back: %v", err)
		}
		if got.Code != "" {
			t.Fatalf("solo room DB code = %q, want ''", got.Code)
		}
	}

	// A misscode resolves nothing; only a real code comes back by code.
	if _, err := rooms.GetRoomByCode(ctx, "MISSING-CODE-42"); err == nil {
		t.Fatal("GetRoomByCode resolved an unknown code")
	}
	for code := range seen {
		got, err := rooms.GetRoomByCode(ctx, code)
		if err != nil || got == nil {
			t.Fatalf("GetRoomByCode(%q) failed: %v", code, err)
		}
	}
}
