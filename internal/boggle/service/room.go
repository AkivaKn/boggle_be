package service

import (
	"context"
	"encoding/json"
	"math/rand"
	"time"

	"boggle-api/internal/boggle/models"
	"boggle-api/internal/boggle/repository"
	"boggle-api/internal/codes"
	"boggle-api/internal/dberr"

	"github.com/google/uuid"
)

var boggleDice = []string{
	"AAEEGN", "ABBJOO", "ACHOPS", "AFFKPS",
	"AOOTTW", "CIMOTU", "DEILRX", "DELRVY",
	"DISTTY", "EEGHNW", "EEINSU", "EHRTVW",
	"EIOSST", "ELRTTY", "HIMNQU", "HLNNRZ",
}

type RoomService interface {
	CreateRoom(ctx context.Context) (*models.Room, error)
	GetRoom(ctx context.Context, id string) (*models.Room, error)
	GenerateBoard(ctx context.Context, roomID string) (*models.Board, error)
	GetActiveGame(ctx context.Context, roomID string) (*models.Room, *models.Board, error)
	// M-A2: address a room by its surfaced CODE (what a friend types). The
	// WS join uses this exclusively; the REST resolver falls back to the
	// uuid so a creator's existing link keeps working.
	GetActiveGameByCode(ctx context.Context, code string) (*models.Room, *models.Board, error)
	// ResolveRef: CODE-FIRST with a UUID FALLBACK, exactly like Millionaire's
	// M7 resolver — the code is what humans share, but a client that only
	// ever learned the uuid must not be stranded.
	ResolveRef(ctx context.Context, ref string) (*models.Room, *models.Board, error)
}

type roomService struct {
	repo repository.RoomRepository
}

func NewRoomService(repo repository.RoomRepository) RoomService {
	return &roomService{repo: repo}
}

// CreateRoom persists a room with its WORD-WORD-DD code already assigned
// (M-A2). Generation is shared with Millionaire (internal/codes) but the
// uniqueness check is Boggle's own partial index, so a collision is retried
// here with a fresh candidate rather than failing the create.
func (s *roomService) CreateRoom(ctx context.Context) (*models.Room, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	room := &models.Room{ID: id.String(), CreatedAt: time.Now().UTC()}
	for attempt := 0; ; attempt++ {
		room.Code = codes.NewRoomCode()
		err = s.repo.CreateRoom(ctx, room)
		if err == nil {
			return room, nil
		}
		if !dberr.IsUniqueViolation(err) || attempt >= codes.MaxPick {
			return nil, err
		}
		// Code collision — regenerate over the loop above.
	}
}

func (s *roomService) GetRoom(ctx context.Context, id string) (*models.Room, error) {
	return s.repo.GetRoom(ctx, id)
}

func (s *roomService) GenerateBoard(ctx context.Context, roomID string) (*models.Board, error) {
	id, _ := uuid.NewV7()
	board := &models.Board{
		ID:        id.String(),
		RoomID:    roomID,
		Cells:     generateBoggleBoard(),
		EndsAt:    time.Now().UTC().Add(3 * time.Minute),
		CreatedAt: time.Now().UTC(),
	}
	err := s.repo.CreateBoard(ctx, board)
	return board, err
}

func (s *roomService) GetActiveGame(ctx context.Context, roomID string) (*models.Room, *models.Board, error) {
	room, err := s.repo.GetRoom(ctx, roomID)
	if err != nil {
		return nil, nil, err
	}
	board, _ := s.repo.GetLatestBoard(ctx, roomID) // Ignore error if no board exists yet
	return room, board, nil
}

// GetActiveGameByCode is the strictly code-keyed resolve used by the WS
// join: a friend types the code, so there is no uuid to fall back to and a
// typo must fail loudly rather than resolving to some other room.
func (s *roomService) GetActiveGameByCode(ctx context.Context, code string) (*models.Room, *models.Board, error) {
	room, err := s.repo.GetRoomByCode(ctx, code)
	if err != nil {
		return nil, nil, err
	}
	board, _ := s.repo.GetLatestBoard(ctx, room.ID)
	return room, board, nil
}

// ResolveRef is CODE-FIRST with a UUID FALLBACK (M-A2). It reports which
// form matched so the caller can log/answer precisely.
func (s *roomService) ResolveRef(ctx context.Context, ref string) (*models.Room, *models.Board, error) {
	if room, board, err := s.GetActiveGameByCode(ctx, ref); err == nil {
		return room, board, nil
	}
	return s.GetActiveGame(ctx, ref)
}

func generateBoggleBoard() string {
	var faces []string
	for _, die := range boggleDice {
		faces = append(faces, string(die[rand.Intn(len(die))]))
	}
	rand.Shuffle(len(faces), func(i, j int) {
		faces[i], faces[j] = faces[j], faces[i]
	})

	var board []models.BoardCell
	for _, letter := range faces {
		orientation := []int{0, 90, 180, 270}[rand.Intn(4)]
		board = append(board, models.BoardCell{
			Letter:      letter,
			Orientation: orientation,
		})
	}

	data, _ := json.Marshal(board)
	return string(data)
}
