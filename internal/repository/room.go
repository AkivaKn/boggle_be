package repository

import (
	"context"

	"boggle-api/internal/models"

	"github.com/vingarcia/ksql"
)

type RoomRepository interface {
	CreateRoom(ctx context.Context, room *models.Room) error
	GetRoom(ctx context.Context, id string) (*models.Room, error)
	
	CreateBoard(ctx context.Context, board *models.Board) error
	GetLatestBoard(ctx context.Context, roomID string) (*models.Board, error)
}

type roomRepo struct {
	db ksql.Provider
}

var RoomsTable = ksql.NewTable("rooms")
var BoardsTable = ksql.NewTable("boards")

func NewRoomRepository(db ksql.Provider) RoomRepository {
	return &roomRepo{db: db}
}

func (r *roomRepo) CreateRoom(ctx context.Context, room *models.Room) error {
	return r.db.Insert(ctx, RoomsTable, room)
}

func (r *roomRepo) GetRoom(ctx context.Context, id string) (*models.Room, error) {
	var room models.Room
	err := r.db.QueryOne(ctx, &room, "SELECT * FROM rooms WHERE id = $1", id)
	return &room, err
}

func (r *roomRepo) CreateBoard(ctx context.Context, board *models.Board) error {
	return r.db.Insert(ctx, BoardsTable, board)
}

func (r *roomRepo) GetLatestBoard(ctx context.Context, roomID string) (*models.Board, error) {
	var board models.Board
	// Get the board with the newest creation date for this room
	err := r.db.QueryOne(ctx, &board, "SELECT * FROM boards WHERE room_id = $1 ORDER BY created_at DESC LIMIT 1", roomID)
	if err != nil {
		return nil, err
	}
	return &board, nil
}