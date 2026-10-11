// Package dberr holds the database error classification both games share.
//
// It exists because room-code generation is RETRY-ON-COLLISION in both
// games now: the generator proposes a WORD-WORD-DD candidate, the partial
// unique index rejects a duplicate, and the service regenerates. Recognising
// that rejection is therefore core game logic, not an incidental detail — and
// it must behave identically for Boggle and Millionaire, or one game would
// silently give up where the other retries.
package dberr

import (
	"errors"
	"strings"

	"github.com/jackc/pgconn"
	"github.com/lib/pq"
)

// IsUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505) — the signal that a proposed room code is
// already taken.
//
// Two driver shapes are covered because the pool is ksql/pgx while some
// paths surface lib/pq's stdlib driver, plus a last-resort substring check
// for a wrapped error after a future ksql change.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return true
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return true
	}
	return strings.Contains(err.Error(), "23505")
}
