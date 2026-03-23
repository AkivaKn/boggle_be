package service

import (
	"context"
	"encoding/json"
	"math/rand"
	"time"

	"boggle-api/internal/models"
	"boggle-api/internal/repository"

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
}

type roomService struct {
	repo repository.RoomRepository
}

func NewRoomService(repo repository.RoomRepository) RoomService {
	return &roomService{repo: repo}
}

func (s *roomService) CreateRoom(ctx context.Context) (*models.Room, error) {
	id, _ := uuid.NewV7()
	room := &models.Room{ID: id.String(), CreatedAt: time.Now().UTC()}
	err := s.repo.CreateRoom(ctx, room)
	return room, err
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
