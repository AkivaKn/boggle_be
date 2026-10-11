package repository

import (
	"context"

	"boggle-api/internal/millionaire/models"

	"github.com/vingarcia/ksql"
)

// RoomTotalsTable — wave-4B per-room session totals (one row per
// room+seat; see models.RoomTotal). ksql is used only for the read; the
// accumulation is a single hand-written upsert (below) so the += and
// greater-of math happens ATOMICALLY inside Postgres, not read-modify-
// write in Go.
var RoomTotalsTable = ksql.NewTable("millionaire_room_totals")

type RoomTotalsRepository interface {
	// AccumulateSeat adds ONE completed run of one seat into the room's
	// session totals: an INSERT that, on the (room_id, seat) conflict,
	// adds this run's delta ON TOP of the stored census —
	// games_played/games_won/total_money sum up, best_level/best_amount
	// take the greater-of, updated_at moves. Never resets on a rematch:
	// deleting the ROOM row is the only teardown (FK ON DELETE CASCADE).
	// t.ID is minted per call and only ever stored by the FIRST insert
	// (the conflict branch keeps the original row's id).
	AccumulateSeat(ctx context.Context, t *models.RoomTotal) error
	// GetRoomTotals returns the room's per-seat session census ordered by
	// seat (room_state `totals[]` builder). EMPTY (no rows) for a room
	// with no history — callers omit the field entirely in that case.
	GetRoomTotals(ctx context.Context, roomID string) ([]models.RoomTotal, error)
}

type roomTotalsRepo struct {
	db ksql.Provider
}

func NewRoomTotalsRepository(db ksql.Provider) RoomTotalsRepository {
	return &roomTotalsRepo{db: db}
}

// AccumulateSeat performs the guarded upsert. All the accumulated
// arithmetic lives in the ON CONFLICT clause (sum / GREATEST) so two
// racing completions of the same room can never lose a run: every
// statement applies its own delta to whatever census is current.
// Constant literals only — no untrusted interpolation.
func (r *roomTotalsRepo) AccumulateSeat(ctx context.Context, t *models.RoomTotal) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO millionaire_room_totals
			(id, room_id, seat, games_played, games_won, total_money, best_level, best_amount, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		ON CONFLICT (room_id, seat) DO UPDATE SET
			games_played = millionaire_room_totals.games_played + EXCLUDED.games_played,
			games_won    = millionaire_room_totals.games_won    + EXCLUDED.games_won,
			total_money  = millionaire_room_totals.total_money  + EXCLUDED.total_money,
			best_level   = GREATEST(millionaire_room_totals.best_level,  EXCLUDED.best_level),
			best_amount  = GREATEST(millionaire_room_totals.best_amount, EXCLUDED.best_amount),
			updated_at   = now()`,
		t.ID, t.RoomID, t.Seat, t.GamesPlayed, t.GamesWon, t.TotalMoney, t.BestLevel, t.BestAmount)
	return err
}

func (r *roomTotalsRepo) GetRoomTotals(ctx context.Context, roomID string) ([]models.RoomTotal, error) {
	var rows []models.RoomTotal
	err := r.db.Query(ctx, &rows,
		"SELECT * FROM millionaire_room_totals WHERE room_id = $1 ORDER BY seat", roomID)
	return rows, err
}
