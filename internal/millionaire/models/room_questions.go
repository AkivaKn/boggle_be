package models

import "time"

// RoomQuestion mirrors the `room_questions` table (migrations/001_create_schema.sql).
// Variant 1 is the primary question of a level, variant 2 the switch-lifeline alternate.
type RoomQuestion struct {
	RoomID     string `ksql:"room_id" json:"room_id"`
	Level      int    `ksql:"level" json:"level"`
	QuestionID string `ksql:"question_id" json:"question_id"`
	Variant    int    `ksql:"variant" json:"variant"`
	// ShuffledOptions is the server-shuffled option order for THIS room
	// (invariant 4: the correct index is not uniform across rooms);
	// ShuffledCorrectIndex is the remapped index in that order. Stored as
	// a JSON array string (Question.Options jsonb pattern).
	ShuffledOptions      string    `ksql:"shuffled_options" json:"shuffled_options"`
	ShuffledCorrectIndex int       `ksql:"shuffled_correct_index" json:"shuffled_correct_index"`
	CreatedAt            time.Time `ksql:"created_at" json:"created_at"`
}
