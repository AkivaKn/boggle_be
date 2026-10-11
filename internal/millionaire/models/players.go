package models

import "time"

// Player mirrors the `players` table (migrations/001_create_schema.sql).
// LifelinesLeft is stored as a JSON array string (Boggle Board.Cells pattern
// for jsonb columns); it must always be set explicitly before insert because
// ksql INSERT sends every field and an empty string is not valid jsonb.
type Player struct {
	ID            string    `ksql:"id" json:"id"`
	RoomID        string    `ksql:"room_id" json:"room_id"`
	Seat          int       `ksql:"seat" json:"seat"`
	Token         string    `ksql:"token" json:"token"` // uuid, REST identity
	Name          string    `ksql:"name" json:"name"`
	Status        string    `ksql:"status" json:"status"`                 // active|fallen|walked|won
	LifelinesLeft string    `ksql:"lifelines_left" json:"lifelines_left"` // jsonb, e.g. ["fifty_fifty","audience","switch"]
	Amount        int       `ksql:"amount" json:"amount"`
	LevelReached  int       `ksql:"level_reached" json:"level_reached"`
	CreatedAt     time.Time `ksql:"created_at" json:"created_at"`
}

// DefaultLifelinesLeft is the JSON value every player starts with.
const DefaultLifelinesLeft = `["fifty_fifty","audience","switch"]`
