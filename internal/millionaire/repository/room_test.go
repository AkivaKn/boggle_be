package repository

import (
	"context"
	"os"
	"testing"
	"time"

	"boggle-api/internal/millionaire/models"

	"github.com/google/uuid"
	"github.com/vingarcia/ksql"
	"github.com/vingarcia/ksql/adapters/kpgx"
)

// connectTestDB connects to the dev DB (app container env) or skips the test
// when DATABASE_URL is unset/unreachable, e.g. when running tests on the host.
func connectTestDB(t *testing.T) ksql.Provider {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://devuser:devpassword@localhost:5436/millionaire?sslmode=disable"
	}
	db, err := kpgx.New(context.Background(), dsn, ksql.Config{})
	if err != nil {
		t.Skipf("dev DB unreachable (%v) — skipping integration test", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestRoomID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("generating room id: %v", err)
	}
	return id.String()
}

// TestRoomInsertExplicitFieldsPassesChecks proves the M0-review standing
// invariant the happy way: with Mode/Status/CreatedAt set explicitly, the
// rooms CHECK constraints don't trip on insert.
func TestRoomInsertExplicitFieldsPassesChecks(t *testing.T) {
	db := connectTestDB(t)
	repo := NewRoomRepository(db)
	ctx := context.Background()

	room := &models.Room{
		ID:        newTestRoomID(t),
		Mode:      "duel",
		Status:    "lobby",
		CreatedAt: time.Now().UTC(),
	}
	if err := repo.CreateRoom(ctx, room); err != nil {
		t.Fatalf("insert with explicit Mode/Status/CreatedAt failed: %v", err)
	}
	defer func() {
		_, _ = db.Exec(ctx, "DELETE FROM rooms WHERE id = $1", room.ID)
	}()

	got, err := repo.GetRoom(ctx, room.ID)
	if err != nil {
		t.Fatalf("reading room back: %v", err)
	}
	if got.Mode != "duel" || got.Status != "lobby" {
		t.Errorf("round-trip mismatch: got mode=%q status=%q, want duel/lobby", got.Mode, got.Status)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt must round-trip non-zero")
	}
}

// TestRoomInsertZeroStatusTripsCheckConstraint is the trip-wire half of the
// invariant: ksql INSERT sends every field, so a zero-value Status is sent as
// ” and violates CHECK (status IN ('lobby','active','finished')) — call sites
// must always set Status/Mode/CreatedAt explicitly.
func TestRoomInsertZeroStatusTripsCheckConstraint(t *testing.T) {
	db := connectTestDB(t)
	repo := NewRoomRepository(db)
	ctx := context.Background()

	room := &models.Room{
		ID:        newTestRoomID(t),
		Mode:      "duel",
		CreatedAt: time.Now().UTC(),
		// Status deliberately zero — the DB has a DEFAULT but ksql sends ''.
	}
	err := repo.CreateRoom(ctx, room)
	if err == nil {
		_, _ = db.Exec(ctx, "DELETE FROM rooms WHERE id = $1", room.ID)
		t.Fatal("insert with zero-value Status unexpectedly succeeded; expected CHECK violation")
	}
	t.Logf("zero-value Status rejected as expected: %v", err)
}
