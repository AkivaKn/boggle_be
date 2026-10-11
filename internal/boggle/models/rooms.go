package models

import "time"

type Room struct {
	ID string `ksql:"id" json:"id"`
	// M-A2: the human-friendly room code (WORD-WORD-DD) a friend types to
	// join. Every Boggle room gets one (Boggle has no solo mode), so it is
	// the primary way to address a room; the uuid stays the internal key and
	// the REST resolver keeps a uuid fallback for the creator's own link.
	Code      string    `ksql:"code" json:"code"`
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
