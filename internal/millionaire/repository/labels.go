package repository

import (
	"context"
	"fmt"

	"boggle-api/internal/millionaire/models"

	"github.com/vingarcia/ksql"
)

// LabelRepository backs the OFFLINE labeling pipeline (cmd/label, SPEC §2
// question engine v2): batching unrated questions, writing agent-returned
// strengths, and the coverage/drift reports. It is intentionally separate
// from the game-side QuestionRepository.
type LabelRepository interface {
	// ListUnrated returns up to `limit` unrated questions in stable id
	// order (the batch builder: consecutive -export N runs partition the
	// corpus without overlap, so the pipeline is resumable).
	ListUnrated(ctx context.Context, limit int) ([]models.Question, error)
	// ListUnratedFiltered is the priority-filter edition of ListUnrated
	// (M10 labels-first: easy-band batches first): same pool id order, but
	// constrained by an optional source ("" / "any" = both corpora,
	// "opentdb" | "otqa") and category include/exclude token lists — each
	// token matches the questions.category column EXACTLY or as a
	// SUBSTRING ('%'||token||'%'), so 'General Knowledge' hits the opentdb
	// category and 'music' would catch 'OTQA:music'. Empty lists filter
	// nothing.
	ListUnratedFiltered(ctx context.Context, limit int, source string, categoriesIn, categoriesOut []string) ([]models.Question, error)
	// ListRatedSample returns up to `limit` random ALREADY-RATED
	// questions (the blind double-rating audit pool).
	ListRatedSample(ctx context.Context, limit int) ([]models.Question, error)
	// CountUnrated / CountRated are the headline coverage numbers the
	// coverage report opens with.
	CountUnrated(ctx context.Context) (int, error)
	CountRated(ctx context.Context) (int, error)
	// ListByID returns the questions with the given ids (any strength
	// state — batch re-reads and audit exports both need full rows).
	ListByID(ctx context.Context, ids []string) ([]models.Question, error)
	// SetStrength writes one agent rating (1–15; validated upstream).
	SetStrength(ctx context.Context, questionID string, strength int) error
	// ResetStrength clears one rating (pipeline rollback, e.g. after the
	// demo batch proof) so the question returns to ListUnrated.
	ResetStrength(ctx context.Context, questionID string) error
	// BandCoverage computes the per-band selection statistics over the
	// CURRENT rated corpus: band L = rank window ((L-1)*n/15, L*n/15] —
	// the exact quantile rule RandomByRankBand samples from.
	BandCoverage(ctx context.Context) ([]models.BandCoverage, error)
}

type labelRepo struct {
	db ksql.Provider
}

func NewLabelRepository(db ksql.Provider) LabelRepository {
	return &labelRepo{db: db}
}

func (r *labelRepo) ListUnrated(ctx context.Context, limit int) ([]models.Question, error) {
	var questions []models.Question
	err := r.db.Query(ctx, &questions,
		"SELECT * FROM millionaire_questions WHERE strength IS NULL ORDER BY id LIMIT $1", limit)
	return questions, err
}

// ListUnratedFiltered builds the priority-filter query. Placeholders stay
// contiguous (pgx extended protocol allows no gaps): $1 = source (optional),
// $2 = include patterns (optional), $3 = exclude patterns (optional), last
// = limit. Patterns are prebuilt in Go ('%'||token||'%' — LIKE ANY covers
// exact membership the same way substring does).
func (r *labelRepo) ListUnratedFiltered(ctx context.Context, limit int, source string, categoriesIn, categoriesOut []string) ([]models.Question, error) {
	if source == "any" {
		source = ""
	}
	query := "SELECT * FROM millionaire_questions WHERE strength IS NULL"
	args := []interface{}{}
	if source != "" {
		args = append(args, source)
		query += fmt.Sprintf(" AND source = $%d", len(args))
	}
	likeOf := func(tokens []string) []string {
		p := make([]string, 0, len(tokens))
		for _, t := range tokens {
			p = append(p, "%"+t+"%")
		}
		return p
	}
	if len(categoriesIn) > 0 {
		args = append(args, likeOf(categoriesIn))
		query += fmt.Sprintf(" AND category LIKE ANY($%d::text[])", len(args))
	}
	if len(categoriesOut) > 0 {
		args = append(args, likeOf(categoriesOut))
		query += fmt.Sprintf(" AND category NOT LIKE ANY($%d::text[])", len(args))
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY id LIMIT $%d", len(args))

	var questions []models.Question
	err := r.db.Query(ctx, &questions, query, args...)
	return questions, err
}

func (r *labelRepo) ListRatedSample(ctx context.Context, limit int) ([]models.Question, error) {
	var questions []models.Question
	err := r.db.Query(ctx, &questions,
		"SELECT * FROM millionaire_questions WHERE strength IS NOT NULL ORDER BY random() LIMIT $1", limit)
	return questions, err
}

func (r *labelRepo) CountUnrated(ctx context.Context) (int, error) {
	var row struct {
		Count int `ksql:"count"`
	}
	err := r.db.QueryOne(ctx, &row,
		"SELECT count(*) AS count FROM millionaire_questions WHERE strength IS NULL")
	return row.Count, err
}

func (r *labelRepo) CountRated(ctx context.Context) (int, error) {
	var row struct {
		Count int `ksql:"count"`
	}
	err := r.db.QueryOne(ctx, &row,
		"SELECT count(*) AS count FROM millionaire_questions WHERE strength IS NOT NULL")
	return row.Count, err
}

func (r *labelRepo) ListByID(ctx context.Context, ids []string) ([]models.Question, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var questions []models.Question
	err := r.db.Query(ctx, &questions,
		"SELECT * FROM millionaire_questions WHERE id::text = ANY($1::text[]) ORDER BY id", ids)
	return questions, err
}

func (r *labelRepo) SetStrength(ctx context.Context, questionID string, strength int) error {
	_, err := r.db.Exec(ctx,
		"UPDATE millionaire_questions SET strength = $2::int WHERE id = $1", questionID, strength)
	if err != nil {
		return err
	}
	return r.mustExist(ctx, questionID, "SetStrength")
}

func (r *labelRepo) ResetStrength(ctx context.Context, questionID string) error {
	_, err := r.db.Exec(ctx,
		"UPDATE millionaire_questions SET strength = NULL WHERE id = $1", questionID)
	if err != nil {
		return err
	}
	return r.mustExist(ctx, questionID, "ResetStrength")
}

// mustExist turns an affected-zero-row write into an explicit error
// (unknown question id in a batch file must fail loudly, not silently).
func (r *labelRepo) mustExist(ctx context.Context, questionID, op string) error {
	row := struct {
		ID string `ksql:"id"`
	}{}
	err := r.db.QueryOne(ctx, &row,
		"SELECT id FROM millionaire_questions WHERE id = $1", questionID)
	if err != nil {
		return fmt.Errorf("%s: question %s not found", op, questionID)
	}
	return nil
}

func (r *labelRepo) BandCoverage(ctx context.Context) ([]models.BandCoverage, error) {
	// Band L = ((rn-1)*15)/n + 1 for rank rn over the rated corpus (same
	// quantile rule the selection uses). Cultural share per band via the
	// exact models.IsCultural SQL equivalent.
	var rows []models.BandCoverage
	err := r.db.Query(ctx, &rows, `
SELECT band,
       count(*) AS total,
       count(*) FILTER (WHERE cultural) AS cultural_total
FROM (
    SELECT row_number() OVER (ORDER BY strength, id) AS rn,
           count(*) OVER () AS n,
           (source = 'otqa' AND category = ANY($1::text[]))
               OR (source = 'opentdb'
                   AND (category LIKE 'Entertainment:%' OR category = 'Celebrities')) AS cultural
    FROM millionaire_questions
    WHERE strength IS NOT NULL
) ranks,
LATERAL (SELECT (FLOOR(((rn - 1) * 15)::numeric / GREATEST(n, 1))::int + 1) AS band) b
GROUP BY band
ORDER BY band`, models.OtqaCulturalCategories)
	return rows, err
}
