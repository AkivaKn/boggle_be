package models

import "time"

type Room struct {
	ID        string    `ksql:"id" json:"id"`
	CreatedAt time.Time `ksql:"created_at" json:"created_at"`
}

type Board struct {
	ID        string    `ksql:"id" json:"id"`
	RoomID    string    `ksql:"room_id" json:"room_id"`
	Letters   string    `ksql:"letters" json:"letters"`
	EndsAt    time.Time `ksql:"ends_at" json:"ends_at"`
	CreatedAt time.Time `ksql:"created_at" json:"created_at"`
}