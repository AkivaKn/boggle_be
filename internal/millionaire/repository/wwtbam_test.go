package repository

import (
	"context"
	"testing"
	"time"

	"boggle-api/internal/millionaire/models"

	"github.com/vingarcia/ksql"
)

// This file integration-tests the M10/S3 WWTBAM per-level pool SQL
// (CountBySourceLevel / RandomBySourceLevel) against the dev DB on the
// hermetic temp questions clone (same discipline as band_test.go /
// casual_test.go): a question's per-level pool is (source, strength) —
// foreign strengths, foreign sources and NULL-strength rows must never
// surface, exact-level rows must.

// wwtbamPoolRow builds one fixture question for the temp clone.
func wwtbamPoolRow(id, source string, level int, difficulty string) models.Question {
	l := level
	return models.Question{
		ID:           id,
		Question:     "wwtbam pool test " + id,
		Options:      `["a","b","c","d"]`,
		CorrectIndex: 0,
		Difficulty:   difficulty,
		Category:     "WWTBAM:Unknown",
		Source:       source,
		CreatedAt:    time.Now().UTC(),
		Strength:     &l,
	}
}

// insertPoolQuestions inserts full Question rows onto the temp clone
// (they live in the private session — nothing to clean up).
func insertPoolQuestions(db ksql.Provider, rows []models.Question) error {
	repo := NewQuestionRepository(db)
	ctx := context.Background()
	for i := range rows {
		if err := repo.InsertQuestion(ctx, &rows[i]); err != nil {
			return err
		}
	}
	return nil
}

// TestWWTBAMPerLevelPoolQueries pins the (source, strength) pool: only
// exact-level rows of the queried source are servable/countable,
// NULL-strength and foreign-source rows are invisible, exclusions apply.
func TestWWTBAMPerLevelPoolQueries(t *testing.T) {
	db := openLabelDB(t)
	repo := NewQuestionRepository(db)
	ctx := context.Background()

	// The clone is seeded with the currently-RATED corpus, which holds
	// wwtbam rows post-import — every expectation is therefore RELATIVE
	// to the pre-existing per-level counts (the fixtures must surface
	// exactly on top of them).
	pre := map[int]int{}
	preAll := 0
	for _, q := range listRated(t, db) {
		if q.Source == "wwtbam" {
			preAll++
			if q.Strength != nil {
				pre[*q.Strength]++
			}
		}
	}

	pool := []models.Question{
		wwtbamPoolRow("81111111-1111-7111-a111-111111111111", "wwtbam", 7, "hard"),
		wwtbamPoolRow("82222222-2222-7222-a222-222222222222", "wwtbam", 7, "hard"),
		wwtbamPoolRow("83333333-3333-7333-a333-333333333333", "wwtbam", 8, "hard"),
		// Traps: wrong source (opentdb impostor, invisible to the query) /
		// NULL-strength wwtbam row (invisible on the strength axis).
		wwtbamPoolRow("84444444-4444-7444-a444-444444444444", "opentdb", 7, "hard"),
		wtbamNullRow("85555555-5555-7555-a555-555555555555", "hard"),
	}
	if err := insertPoolQuestions(db, pool); err != nil {
		t.Fatalf("inserting pool fixtures: %v", err)
	}

	// Count: exactly MY fixture rows on top of the pre-existing counts.
	n7, err := repo.CountBySourceLevel(ctx, "wwtbam", 7)
	if err != nil {
		t.Fatal(err)
	}
	if n7 != 2+pre[7] {
		t.Fatalf("CountBySourceLevel(wwtbam, 7) = %d, want %d (%d pre-existing)", n7, 2+pre[7], pre[7])
	}
	if n8, _ := repo.CountBySourceLevel(ctx, "wwtbam", 8); n8 != 1+pre[8] {
		t.Fatalf("CountBySourceLevel(wwtbam, 8) = %d, want %d (%d pre-existing)", n8, 1+pre[8], pre[8])
	}
	if n6, _ := repo.CountBySourceLevel(ctx, "wwtbam", 6); n6 != pre[6] {
		t.Fatalf("CountBySourceLevel(wwtbam, 6) = %d, want %d (no level-6 fixture; traps must not count)", n6, pre[6])
	}
	// CountBySource counts every wwtbam row regardless of level, including
	// the NULL-strength one (four fixture rows carry source='wwtbam').
	if all, _ := repo.CountBySource(ctx, "wwtbam"); all != 4+preAll {
		t.Fatalf("CountBySource(wwtbam) = %d, want %d (incl. the NULL-strength row)", all, 4+preAll)
	}

	// Draw: every pick is an exact-level-7 wwtbam row.
	for attempt := 0; attempt < 10; attempt++ {
		picked, err := repo.RandomBySourceLevel(ctx, "wwtbam", 7, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(picked) == 0 {
			t.Fatal("RandomBySourceLevel drew nothing from a non-empty pool")
		}
		if picked[0].Source != "wwtbam" || picked[0].Strength == nil || *picked[0].Strength != 7 {
			t.Fatalf("drew out-of-pool row %+v", picked[0])
		}
	}

	// Exclusion: the named id disappears; only eligible rows remain.
	for attempt := 0; attempt < 6; attempt++ {
		picked, err := repo.RandomBySourceLevel(ctx, "wwtbam", 7, 1,
			[]string{"81111111-1111-7111-a111-111111111111"})
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range picked {
			if q.ID == "81111111-1111-7111-a111-111111111111" {
				t.Fatal("the excluded id surfaced in a source-level draw")
			}
			if q.Strength == nil || *q.Strength != 7 || q.Source != "wwtbam" {
				t.Fatalf("excluded draw served out-of-pool row %+v", q)
			}
		}
	}
}

// wtbamNullRow builds a wwtbam-shaped row WITHOUT a strength (invisible on
// the strength axis).
func wtbamNullRow(id string, difficulty string) models.Question {
	q := wwtbamPoolRow(id, "wwtbam", 99, difficulty)
	q.Strength = nil
	return q
}
