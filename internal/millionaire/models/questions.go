package models

import "time"

// Question mirrors the `questions` table (migrations/001_create_schema.sql).
// Options is stored as a JSON array string to match the Boggle Board.Cells
// pattern for jsonb columns (ksql passes it through as a plain string).
type Question struct {
	ID           string    `ksql:"id" json:"id"`
	Question     string    `ksql:"question" json:"question"`
	Options      string    `ksql:"options" json:"options"` // jsonb: exactly 4 strings
	CorrectIndex int       `ksql:"correct_index" json:"correct_index"`
	Difficulty   string    `ksql:"difficulty" json:"difficulty"` // easy|medium|hard
	Category     string    `ksql:"category" json:"category"`
	Source       string    `ksql:"source" json:"source"` // opentdb | otqa
	CreatedAt    time.Time `ksql:"created_at" json:"created_at"`
	// Strength is the v2 per-question difficulty (SPEC §2 question engine
	// v2): rated 1–15 OFFLINE by labeling agents; NULL = unrated (not yet
	// through the cmd/label pipeline). Selection reads only rated rows
	// once the coverage gate is met; unrated rows remain selected only via
	// the legacy tier fallback. Nil → SQL NULL.
	Strength *int `ksql:"strength" json:"strength,omitempty"`
}
