package models

import "time"

// RoomTotal mirrors the `room_totals` table (migrations/006_room_totals.sql,
// wave-4B). ONE row per (room, seat): the per-room SESSION totals — the
// accumulated census of every completed run of that room across rematches
// ("in one room session we should persist player totals"). Public client
// facts only: the block carried in room_state `totals[]` is exactly these
// six fields.
//
// Lifecycle: the GameService upserts one row per resolved seat at every
// game_over (AccumulateRoomTotals, += / greater-of semantics); a rematch
// NEVER resets them (Replay's ResetRoomPlayers touches the players rows
// only); the rows die ONLY with their room (FK ON DELETE CASCADE).
type RoomTotal struct {
	ID          string    `ksql:"id" json:"id"`     // uuid v7, minted per upsert (kept only by the first insert)
	RoomID      string    `ksql:"room_id" json:"-"` // internal FK — never serialized to clients
	Seat        int       `ksql:"seat" json:"seat"` // 1 | 2
	GamesPlayed int       `ksql:"games_played" json:"games_played"`
	GamesWon    int       `ksql:"games_won" json:"games_won"`     // runs ended status=won (nobody won → 0)
	TotalMoney  int64     `ksql:"total_money" json:"total_money"` // sum of every run's amount (walk/fall bank; a win's final amount)
	BestLevel   int       `ksql:"best_level" json:"best_level"`   // highest level_reached ever reached here (0 = never left L1)
	BestAmount  int       `ksql:"best_amount" json:"best_amount"` // highest single-run amount ever banked here
	UpdatedAt   time.Time `ksql:"updated_at" json:"-"`            // last accumulation instant
}
