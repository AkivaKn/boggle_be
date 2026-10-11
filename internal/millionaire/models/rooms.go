package models

import "time"

type Room struct {
	ID        string    `ksql:"id" json:"id"`
	Code      string    `ksql:"code" json:"code"` // duel-facing join code (WORD-WORD-DD); solo rooms carry=''
	Mode      string    `ksql:"mode" json:"mode"`
	Status    string    `ksql:"status" json:"status"`
	CreatedAt time.Time `ksql:"created_at" json:"created_at"`
}
