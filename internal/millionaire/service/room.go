package service

import (
	"context"
	"fmt"
	"time"

	"boggle-api/internal/millionaire/models"
	"boggle-api/internal/millionaire/repository"

	"github.com/google/uuid"
)

type RoomService interface {
	CreateRoom(ctx context.Context, mode string) (*models.Room, error)
	GetRoom(ctx context.Context, id string) (*models.Room, error)
	GetRoomByCode(ctx context.Context, code string) (*models.Room, error)
	SetRoomStatus(ctx context.Context, id string, status string) error
	DeleteRoom(ctx context.Context, id string) error
}

type roomService struct {
	repo repository.RoomRepository
}

func NewRoomService(repo repository.RoomRepository) RoomService {
	return &roomService{repo: repo}
}

// CreateRoom persists a fresh room with the code already assigned
// (M7, SPEC v1.1): duel rooms get a generated WORD-WORD-DD code from the
// curated vocabulary — the partial unique index on rooms.code rejects a
// collision, and the insert retries with a new candidate (see
// generateRoomCode); solo rooms carry an EMPTY code (codes are surfaced for duels
// only, §1 solo-instant-play decision), which the same index excludes.
func (s *roomService) CreateRoom(ctx context.Context, mode string) (*models.Room, error) {
	// M0 review follow-up, retired: NewV7's error is handled, not discarded.
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generating room id: %w", err)
	}
	room := &models.Room{
		ID:        id.String(),
		Mode:      mode,
		Status:    "lobby",
		CreatedAt: time.Now().UTC(),
	}
	if mode != "solo" {
		for attempt := 0; ; attempt++ {
			room.Code = generateRoomCode()
			err = s.repo.CreateRoom(ctx, room)
			if err == nil {
				return room, nil
			}
			if !repository.IsUniqueViolation(err) || attempt >= codeWordsMaxPick {
				return nil, fmt.Errorf("creating room: %w", err)
			}
			// Code collision — regenerated over the loop above.
		}
	}
	if err := s.repo.CreateRoom(ctx, room); err != nil {
		return nil, err
	}
	return room, nil
}

func (s *roomService) GetRoom(ctx context.Context, id string) (*models.Room, error) {
	return s.repo.GetRoom(ctx, id)
}

// GetRoomByCode is the M7 join resolution: clients only ever know the
// surfaced code; the handler maps it to the internal room once and every
// downstream path keeps using the uuid.
func (s *roomService) GetRoomByCode(ctx context.Context, code string) (*models.Room, error) {
	return s.repo.GetRoomByCode(ctx, code)
}

func (s *roomService) SetRoomStatus(ctx context.Context, id string, status string) error {
	return s.repo.UpdateStatus(ctx, id, status)
}

// DeleteRoom removes the room row entirely — used by CreateRoomWS when the
// creator's player insert fails after the room was persisted (no orphan
// rows, M2 review finding 3).
func (s *roomService) DeleteRoom(ctx context.Context, id string) error {
	return s.repo.DeleteRoom(ctx, id)
}
