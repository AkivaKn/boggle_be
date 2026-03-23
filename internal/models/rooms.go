package models

import "time"

type Room struct {
	ID        string    `ksql:"id" json:"id"`
	CreatedAt time.Time `ksql:"created_at" json:"created_at"`
}

type BoardCell struct {
    Letter      string `json:"letter"`
    Orientation int    `json:"orientation"`
}

type Board struct {
	ID        string    `ksql:"id" json:"id"`
	RoomID    string    `ksql:"room_id" json:"room_id"`
	Cells     string    `ksql:"cells" json:"cells"`
	EndsAt    time.Time `ksql:"ends_at" json:"ends_at"`
	CreatedAt time.Time `ksql:"created_at" json:"created_at"`
}
