package repository

import (
	"context"

	"boggle-api/internal/millionaire/models"

	"github.com/vingarcia/ksql"
)

type PlayerRepository interface {
	InsertPlayer(ctx context.Context, p *models.Player) error
	GetPlayerByToken(ctx context.Context, roomID string, token string) (*models.Player, error)
	UpdateStatus(ctx context.Context, playerID string, status string) error
	UpdateAmount(ctx context.Context, playerID string, amount int) error
	UpdateLevelReached(ctx context.Context, playerID string, levelReached int) error
	UpdateLifelinesLeft(ctx context.Context, playerID string, lifelinesLeft string) error
	// SpendLifeline atomically consumes one use of `lifeline`: a single
	// guarded UPDATE (M5-review finding 2) removes the lifeline from the
	// jsonb array ONLY when it is still there:
	//
	//	UPDATE millionaire_players
	//	SET lifelines_left = lifelines_left - $2::jsonb
	//	WHERE id = $1 AND lifelines_left @> $2::jsonb
	//
	// and returns the rows affected (0 = already spent → the caller maps
	// that to 409). The DB row is the source of truth, so parallel
	// identical POSTs deterministically produce exactly one winner.
	SpendLifeline(ctx context.Context, playerID, lifeline string) (int64, error)
	GetPlayersByRoom(ctx context.Context, roomID string) ([]models.Player, error)
	// ResetRoomPlayers re-arms EVERY seat of the room for a fresh run
	// (wave-4 in-room rematch): status back to `active`, amount and
	// level_reached to 0, lifelines_left to the default set. One guarded
	// statement so a rematch never leaves a half-reset roster.
	ResetRoomPlayers(ctx context.Context, roomID string, lifelinesLeft string) error
}

type playerRepo struct {
	db ksql.Provider
}

func NewPlayerRepository(db ksql.Provider) PlayerRepository {
	return &playerRepo{db: db}
}

func (r *playerRepo) InsertPlayer(ctx context.Context, p *models.Player) error {
	return r.db.Insert(ctx, PlayersTable, p)
}

// GetPlayerByToken resolves a rejoin: the token must belong to a player of
// exactly this room (SPEC §2 join contract).
func (r *playerRepo) GetPlayerByToken(ctx context.Context, roomID string, token string) (*models.Player, error) {
	var p models.Player
	err := r.db.QueryOne(ctx, &p,
		"SELECT * FROM millionaire_players WHERE room_id = $1 AND token = $2", roomID, token)
	return &p, err
}

func (r *playerRepo) UpdateStatus(ctx context.Context, playerID string, status string) error {
	_, err := r.db.Exec(ctx, "UPDATE millionaire_players SET status = $1 WHERE id = $2", status, playerID)
	return err
}

func (r *playerRepo) UpdateAmount(ctx context.Context, playerID string, amount int) error {
	_, err := r.db.Exec(ctx, "UPDATE millionaire_players SET amount = $1 WHERE id = $2", amount, playerID)
	return err
}

func (r *playerRepo) UpdateLevelReached(ctx context.Context, playerID string, levelReached int) error {
	_, err := r.db.Exec(ctx, "UPDATE millionaire_players SET level_reached = $1 WHERE id = $2", levelReached, playerID)
	return err
}

func (r *playerRepo) UpdateLifelinesLeft(ctx context.Context, playerID string, lifelinesLeft string) error {
	_, err := r.db.Exec(ctx, "UPDATE millionaire_players SET lifelines_left = $1 WHERE id = $2", lifelinesLeft, playerID)
	return err
}

// SpendLifeline performs the guarded atomic spend (see the interface
// comment). Two parameter forms of the SAME whitelisted name: raw text
// for the array-element removal (`jsonb - text`) and the double-quoted
// jsonb literal for the guard (`jsonb @> jsonb`). No untrusted content
// is embedded — `name` must be one of the whitelisted lifeline names
// (game.ValidLifelines upstream).
func (r *playerRepo) SpendLifeline(ctx context.Context, playerID, name string) (int64, error) {
	res, err := r.db.Exec(ctx,
		"UPDATE millionaire_players SET lifelines_left = lifelines_left - $2::text WHERE id = $1 AND lifelines_left @> $3::jsonb",
		playerID, name, `"`+name+`"`)
	if err != nil {
		return -1, err
	}
	return res.RowsAffected()
}

func (r *playerRepo) GetPlayersByRoom(ctx context.Context, roomID string) ([]models.Player, error) {
	var players []models.Player
	err := r.db.Query(ctx, &players, "SELECT * FROM millionaire_players WHERE room_id = $1 ORDER BY seat", roomID)
	return players, err
}

// ResetRoomPlayers performs the rematch reset for the whole roster (see
// the interface comment). `lifelinesLeft` is the jsonb default set, passed
// as text and cast — never user-controlled content.
func (r *playerRepo) ResetRoomPlayers(ctx context.Context, roomID string, lifelinesLeft string) error {
	_, err := r.db.Exec(ctx,
		"UPDATE millionaire_players SET status = 'active', amount = 0, level_reached = 0, lifelines_left = $1::jsonb WHERE room_id = $2",
		lifelinesLeft, roomID)
	return err
}
