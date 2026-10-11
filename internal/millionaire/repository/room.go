package repository

import (
	"context"
	"errors"
	"strings"

	"boggle-api/internal/millionaire/models"

	"github.com/jackc/pgconn"
	"github.com/lib/pq"
	"github.com/vingarcia/ksql"
)

type RoomRepository interface {
	CreateRoom(ctx context.Context, room *models.Room) error
	GetRoom(ctx context.Context, id string) (*models.Room, error)
	GetRoomByCode(ctx context.Context, code string) (*models.Room, error)
	UpdateStatus(ctx context.Context, id string, status string) error
	DeleteRoom(ctx context.Context, id string) error
}

type roomRepo struct {
	db ksql.Provider
}

var QuestionsTable = ksql.NewTable("millionaire_questions")
var RoomsTable = ksql.NewTable("millionaire_rooms")
var PlayersTable = ksql.NewTable("millionaire_players")

// room_questions has a composite key (room_id, level, variant) and no id
// column — the default "id" would be inserted and fail (M3 finding).
var RoomQuestionsTable = ksql.NewTable("millionaire_room_questions", "room_id", "level", "variant")

func NewRoomRepository(db ksql.Provider) RoomRepository {
	return &roomRepo{db: db}
}

func (r *roomRepo) CreateRoom(ctx context.Context, room *models.Room) error {
	return r.db.Insert(ctx, RoomsTable, room)
}

func (r *roomRepo) GetRoom(ctx context.Context, id string) (*models.Room, error) {
	var room models.Room
	err := r.db.QueryOne(ctx, &room, "SELECT * FROM millionaire_rooms WHERE id = $1", id)
	return &room, err
}

// GetRoomByCode resolves a surfaced duel code to its room (M7): the
// code is the client identity, the uuid stays the internal PK. A code
// never collides across rooms (partial unique index on the column), and
// the empty string only ever matches nothing real (solo rooms are
// unjoinable by code — callers pass non-empty codes from the routes).
func (r *roomRepo) GetRoomByCode(ctx context.Context, code string) (*models.Room, error) {
	var room models.Room
	err := r.db.QueryOne(ctx, &room, "SELECT * FROM millionaire_rooms WHERE code = $1", code)
	return &room, err
}

func (r *roomRepo) UpdateStatus(ctx context.Context, id string, status string) error {
	_, err := r.db.Exec(ctx, "UPDATE millionaire_rooms SET status = $1 WHERE id = $2", status, id)
	return err
}

func (r *roomRepo) DeleteRoom(ctx context.Context, id string) error {
	return r.db.Delete(ctx, RoomsTable, id)
}

// IsUniqueViolation reports whether err is Postgres' 23505 (unique
// constraint / unique index violation). ksql bubbles the driver error,
// which for the kpgx adapter is *pgconn.PgError; lib/pq's *pq.Error covers
// the stdlib-driver case, and a last-resort substring check guards a
// wrapped error after a future ksql change.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return true
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return true
	}
	return strings.Contains(err.Error(), "23505")
}
