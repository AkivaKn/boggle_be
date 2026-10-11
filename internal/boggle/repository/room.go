package repository

import (
	"context"

	"boggle-api/internal/boggle/models"

	"github.com/vingarcia/ksql"
)

type RoomRepository interface {
	CreateRoom(ctx context.Context, room *models.Room) error
	GetRoom(ctx context.Context, id string) (*models.Room, error)
	// M-A2: resolve by the surfaced room CODE (what a friend types). Kept
	// separate from GetRoom so the WS join can be strictly code-keyed while
	// the REST resolver still has a uuid fallback.
	GetRoomByCode(ctx context.Context, code string) (*models.Room, error)

	CreateBoard(ctx context.Context, board *models.Board) error
	GetLatestBoard(ctx context.Context, roomID string) (*models.Board, error)
}

type roomRepo struct {
	db ksql.Provider
}

// M-A1: the shared games database namespaces each game's tables, so these
// are `boggle_`-prefixed (Millionaire owns `millionaire_rooms`, etc.). Both
// the ksql table objects AND the raw SQL below must name the prefixed table.
var RoomsTable = ksql.NewTable("boggle_rooms")
var BoardsTable = ksql.NewTable("boggle_boards")

func NewRoomRepository(db ksql.Provider) RoomRepository {
	return &roomRepo{db: db}
}

func (r *roomRepo) CreateRoom(ctx context.Context, room *models.Room) error {
	return r.db.Insert(ctx, RoomsTable, room)
}

func (r *roomRepo) GetRoom(ctx context.Context, id string) (*models.Room, error) {
	var room models.Room
	err := r.db.QueryOne(ctx, &room, "SELECT * FROM boggle_rooms WHERE id = $1", id)
	return &room, err
}

func (r *roomRepo) GetRoomByCode(ctx context.Context, code string) (*models.Room, error) {
	var room models.Room
	err := r.db.QueryOne(ctx, &room, "SELECT * FROM boggle_rooms WHERE code = $1", code)
	return &room, err
}

func (r *roomRepo) CreateBoard(ctx context.Context, board *models.Board) error {
	return r.db.Insert(ctx, BoardsTable, board)
}

func (r *roomRepo) GetLatestBoard(ctx context.Context, roomID string) (*models.Board, error) {
	var board models.Board
	// Get the board with the newest creation date for this room
	err := r.db.QueryOne(ctx, &board, "SELECT * FROM boggle_boards WHERE room_id = $1 ORDER BY created_at DESC LIMIT 1", roomID)
	if err != nil {
		return nil, err
	}
	return &board, nil
}
