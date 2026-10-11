package repository

import (
	"context"

	"boggle-api/internal/millionaire/models"

	"github.com/vingarcia/ksql"
)

type RoomQuestionRepository interface {
	InsertBatch(ctx context.Context, batch []models.RoomQuestion) error
	// DeleteByRoom removes every layout row of the room (wave-4 rematch:
	// a replayed room's composite-keyed rows must be REPLACED, never
	// duplicated; run inside the same transaction as the fresh insert).
	DeleteByRoom(ctx context.Context, roomID string) error
	GetByRoomID(ctx context.Context, roomID string) ([]models.RoomQuestion, error)
	GetByRoomIDAndLevel(ctx context.Context, roomID string, level int) ([]models.RoomQuestion, error)
}

type roomQuestionRepo struct {
	db ksql.Provider
}

func NewRoomQuestionRepository(db ksql.Provider) RoomQuestionRepository {
	return &roomQuestionRepo{db: db}
}

func (r *roomQuestionRepo) InsertBatch(ctx context.Context, batch []models.RoomQuestion) error {
	// ksql has no bulk insert; questions for a room arrive as 30 rows
	// (15 levels x 2 variants) so a simple loop is fine.
	for i := range batch {
		if err := r.db.Insert(ctx, RoomQuestionsTable, &batch[i]); err != nil {
			return err
		}
	}
	return nil
}

// DeleteByRoom removes every layout row of the room (see the interface
// comment). Plain SQL — ksql's Delete is single-PK keyed.
func (r *roomQuestionRepo) DeleteByRoom(ctx context.Context, roomID string) error {
	_, err := r.db.Exec(ctx, "DELETE FROM room_questions WHERE room_id = $1", roomID)
	return err
}

func (r *roomQuestionRepo) GetByRoomID(ctx context.Context, roomID string) ([]models.RoomQuestion, error) {
	var rqs []models.RoomQuestion
	err := r.db.Query(ctx, &rqs, "SELECT * FROM room_questions WHERE room_id = $1 ORDER BY level, variant", roomID)
	return rqs, err
}

func (r *roomQuestionRepo) GetByRoomIDAndLevel(ctx context.Context, roomID string, level int) ([]models.RoomQuestion, error) {
	var rqs []models.RoomQuestion
	err := r.db.Query(ctx, &rqs,
		"SELECT * FROM room_questions WHERE room_id = $1 AND level = $2 ORDER BY variant", roomID, level)
	return rqs, err
}
