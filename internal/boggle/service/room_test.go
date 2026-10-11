package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"boggle-api/internal/boggle/models"
	"boggle-api/internal/dberr"
)

// fakeRepo is an in-memory RoomRepository. M-A2 needs it for the code
// COLLISION path specifically: that branch only runs when Postgres rejects a
// duplicate, which a real database will not produce on demand, so the retry
// loop would otherwise ship untested.
type fakeRepo struct {
	rooms       map[string]*models.Room
	byCode      map[string]*models.Room
	boards      map[string]*models.Board
	createCalls int
	// collideFirst makes the first N CreateRoom calls return a unique
	// violation, simulating a code collision.
	collideFirst int
	lastErr      error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		rooms:  map[string]*models.Room{},
		byCode: map[string]*models.Room{},
		boards: map[string]*models.Board{},
	}
}

func (f *fakeRepo) CreateRoom(_ context.Context, room *models.Room) error {
	f.createCalls++
	if f.createCalls <= f.collideFirst {
		f.lastErr = errors.New(`ERROR: duplicate key value violates unique constraint "idx_boggle_rooms_code_unique" (SQLSTATE 23505)`)
		return f.lastErr
	}
	if _, taken := f.byCode[room.Code]; taken {
		f.lastErr = fmt.Errorf(`duplicate key (SQLSTATE 23505)`)
		return f.lastErr
	}
	cp := *room
	f.rooms[room.ID] = &cp
	f.byCode[room.Code] = &cp
	return nil
}

func (f *fakeRepo) GetRoom(_ context.Context, id string) (*models.Room, error) {
	r, ok := f.rooms[id]
	if !ok {
		return nil, errors.New("no such room")
	}
	return r, nil
}

func (f *fakeRepo) GetRoomByCode(_ context.Context, code string) (*models.Room, error) {
	r, ok := f.byCode[code]
	if !ok {
		return nil, errors.New("no such code")
	}
	return r, nil
}

func (f *fakeRepo) CreateBoard(_ context.Context, b *models.Board) error {
	cp := *b
	f.boards[b.ID] = &cp
	return nil
}

func (f *fakeRepo) GetLatestBoard(_ context.Context, roomID string) (*models.Board, error) {
	for _, b := range f.boards {
		if b.RoomID == roomID {
			return b, nil
		}
	}
	return nil, errors.New("no board")
}

var codeRe = regexp.MustCompile(`^[A-Z]{3,}-[A-Z]{3,}-[2-9][2-9]$`)

// TestCreateRoomAssignsShareableCode: every Boggle room carries a code in the
// shared WORD-WORD-DD format (Boggle has no solo mode, so there is no '' case).
func TestCreateRoomAssignsShareableCode(t *testing.T) {
	svc := NewRoomService(newFakeRepo())
	room, err := svc.CreateRoom(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !codeRe.MatchString(room.Code) {
		t.Fatalf("room code = %q, want WORD-WORD-DD", room.Code)
	}
	if room.ID == "" {
		t.Fatal("room has no id")
	}
}

// TestCreateRoomRetriesCodeCollision is the regression test for the retry
// loop: a duplicate must REGENERATE, not surface as a failed create.
func TestCreateRoomRetriesCodeCollision(t *testing.T) {
	repo := newFakeRepo()
	repo.collideFirst = 2 // two collisions in a row
	svc := NewRoomService(repo)

	room, err := svc.CreateRoom(context.Background())
	if err != nil {
		t.Fatalf("create failed after collisions instead of retrying: %v", err)
	}
	if repo.createCalls != 3 {
		t.Fatalf("CreateRoom called %d times, want 3 (2 collisions + 1 success)", repo.createCalls)
	}
	if !dberr.IsUniqueViolation(repo.lastErr) && repo.createCalls == 1 {
		t.Fatal("collision was not classified as a unique violation")
	}
	if !codeRe.MatchString(room.Code) {
		t.Fatalf("retried room code = %q, want WORD-WORD-DD", room.Code)
	}
}

// TestCreateRoomGivesUpOnNonCollisionErrors: an unrelated failure (a down
// database) must NOT be retried as if it were a code collision.
func TestCreateRoomGivesUpOnNonCollisionErrors(t *testing.T) {
	repo := &alwaysFailRepo{}
	svc := NewRoomService(repo)
	if _, err := svc.CreateRoom(context.Background()); err == nil {
		t.Fatal("CreateRoom swallowed a non-collision error")
	}
	if repo.calls != 1 {
		t.Fatalf("CreateRoom retried %d times on a non-collision error, want 1", repo.calls)
	}
}

type alwaysFailRepo struct {
	*fakeRepo
	calls int
}

func (a *alwaysFailRepo) CreateRoom(_ context.Context, _ *models.Room) error {
	a.calls++
	return errors.New("connection refused")
}

// TestResolveRefIsCodeFirstWithUUIDFallback pins the REST resolution order:
// the code is what humans share, but a client that only ever learned the
// uuid must still resolve.
func TestResolveRefIsCodeFirstWithUUIDFallback(t *testing.T) {
	repo := newFakeRepo()
	svc := NewRoomService(repo)
	room, err := svc.CreateRoom(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	byCode, _, err := svc.ResolveRef(context.Background(), room.Code)
	if err != nil || byCode.ID != room.ID {
		t.Fatalf("ResolveRef(code) = %v, %v", byCode, err)
	}
	byUUID, _, err := svc.ResolveRef(context.Background(), room.ID)
	if err != nil || byUUID.ID != room.ID {
		t.Fatalf("ResolveRef(uuid fallback) = %v, %v", byUUID, err)
	}
	if _, _, err := svc.ResolveRef(context.Background(), "NOPE-NOPE-42"); err == nil {
		t.Fatal("ResolveRef resolved a reference that is neither a code nor a uuid")
	}
}

// TestGetActiveGameByCodeHasNoFallback: the WS join is strictly code-keyed,
// so a uuid typed into the join URL must NOT resolve.
func TestGetActiveGameByCodeHasNoFallback(t *testing.T) {
	repo := newFakeRepo()
	svc := NewRoomService(repo)
	room, err := svc.CreateRoom(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.GetActiveGameByCode(context.Background(), room.ID); err == nil {
		t.Fatal("the code-keyed join resolved a uuid — the WS join must be strict")
	}
}