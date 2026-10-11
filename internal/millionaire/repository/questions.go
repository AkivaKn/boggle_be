package repository

import (
	"context"
	"fmt"

	"boggle-api/internal/millionaire/models"

	"github.com/vingarcia/ksql"
)

type QuestionRepository interface {
	InsertQuestion(ctx context.Context, q *models.Question) error
	// CountByTier counts questions of one difficulty in the OPENTDB corpus
	// only (M6: per-tier minimums stay an opentdb-source invariant; OTQA
	// rows carry a synthetic difficulty and are governed by strength).
	CountByTier(ctx context.Context, difficulty string) (int, error)
	ListAll(ctx context.Context) ([]models.Question, error)
	// RandomByTier returns `limit` random questions of one difficulty,
	// excluding the given question ids (used to avoid re-issuing questions
	// already picked for a room's 15-level list). OPENTDB-only (M6 round-1
	// review): the pre-labeling legacy tier pool must serve the vetted
	// OpenTDB corpus only — the 4,955 synthetic-'medium' OTQA rows share
	// the difficulty VALUE and must never surface in pre-labeling games
	// (they join the pool later, strength-rated, via RandomByRankBand).
	RandomByTier(ctx context.Context, difficulty string, limit int, excludeQuestionIDs []string) ([]models.Question, error)
	// CountByCategory counts questions under one exact category value
	// (M6: the OTQA importer logs per-category totals).
	CountByCategory(ctx context.Context, category string) (int, error)
	// CountRated counts rows with a non-NULL strength (band-selection
	// universe; M6 question engine v2).
	CountRated(ctx context.Context) (int, error)
	// RandomByRankBand draws up to `limit` random rated questions whose
	// RANK (ordered by strength) falls in the 1-based lo..hi window — the
	// quantile band of one level. `excludeCultural` lifts the taste layer:
	// cultural questions are filtered out server-side. WWTBAM rows are
	// EXCLUDED (M10 fix): their calibration axis is their own show level,
	// served exclusively by the strict per-level pools (RandomBySourceLevel
	// / CountBySourceLevel); the rank windows only bound rows BETWEEN
	// strengths, never exactly AT one, so a wwtbam row in a band would be
	// served at a level ≠ its strength.
	RandomByRankBand(ctx context.Context, lo, hi int, excludeCultural bool, limit int, excludeQuestionIDs []string) ([]models.Question, error)
	// RandomByCasual draws up to `limit` random questions from the CASUAL
	// pool (S2 rule, levels 1–3): strength BETWEEN 1 AND maxStrength (3 =
	// the casual window, 5 = the logged widened window) with the
	// niche/blacklist categories filtered out — both corpora's cultural
	// categories are on the blacklist (models.CasualBlacklist*), so the
	// pool is non-cultural by construction. Excludes this room's picks so
	// variants never duplicate. WWTBAM rows are EXCLUDED (M10 fix): the
	// show corpus is governed strictly by strength == level (its dedicated
	// per-level pools); with the corpus unrated elsewhere, a source-blind
	// casual window served wwtbam rows at ANY of levels 1–3 — the exact
	// calibration defect the pool gate caught.
	RandomByCasual(ctx context.Context, maxStrength int, limit int, excludeQuestionIDs []string) ([]models.Question, error)
	// RandomByFailSafeCasual is the S2 last resort when the casual pool
	// (and its widened window) cannot yield a candidate: the vetted
	// opentdb EASY tier restricted to the fail-safe categories (General
	// Knowledge / Geography / Sports / History / Food & Drink — always
	// non-blacklisted). Raw tier sampling never serves levels 1–3.
	RandomByFailSafeCasual(ctx context.Context, categories []string, limit int, excludeQuestionIDs []string) ([]models.Question, error)
	// CountBySourceLevel counts the questions whose STRENGTH is exactly
	// one level for one source (M10 S3 WWTBAM ingest: strength = the real
	// show level 1..15, so the per-level pool size IS the count). Rows
	// with a NULL strength never count (the axis is strength, not the
	// synthetic legacy difficulty).
	CountBySourceLevel(ctx context.Context, source string, level int) (int, error)
	// CountBySource counts ALL rows of one source regardless of strength
	// (the importer's run total / the corpus census).
	CountBySource(ctx context.Context, source string) (int, error)
	// RandomBySourceLevel draws up to `limit` random questions of one
	// source at exactly one strength level, excluding the given question
	// ids (the room's consumed picks — same discipline as RandomByTier).
	RandomBySourceLevel(ctx context.Context, source string, level int, limit int, excludeQuestionIDs []string) ([]models.Question, error)
}

type questionRepo struct {
	db ksql.Provider
}

func NewQuestionRepository(db ksql.Provider) QuestionRepository {
	return &questionRepo{db: db}
}

func (r *questionRepo) InsertQuestion(ctx context.Context, q *models.Question) error {
	return r.db.Insert(ctx, QuestionsTable, q)
}

func (r *questionRepo) CountByTier(ctx context.Context, difficulty string) (int, error) {
	var row struct {
		Count int `ksql:"count"`
	}
	err := r.db.QueryOne(ctx, &row,
		"SELECT count(*) AS count FROM questions WHERE difficulty = $1 AND source = 'opentdb'", difficulty)
	return row.Count, err
}

func (r *questionRepo) CountByCategory(ctx context.Context, category string) (int, error) {
	var row struct {
		Count int `ksql:"count"`
	}
	err := r.db.QueryOne(ctx, &row, "SELECT count(*) AS count FROM questions WHERE category = $1", category)
	return row.Count, err
}

func (r *questionRepo) CountRated(ctx context.Context) (int, error) {
	var row struct {
		Count int `ksql:"count"`
	}
	err := r.db.QueryOne(ctx, &row,
		"SELECT count(*) AS count FROM questions WHERE strength IS NOT NULL")
	return row.Count, err
}

func (r *questionRepo) ListAll(ctx context.Context) ([]models.Question, error) {
	var questions []models.Question
	err := r.db.Query(ctx, &questions, "SELECT * FROM questions")
	return questions, err
}

func (r *questionRepo) RandomByTier(ctx context.Context, difficulty string, limit int, excludeQuestionIDs []string) ([]models.Question, error) {
	var questions []models.Question
	// An empty Go slice arrives as NULL, and `id <> ALL(NULL)` is never
	// true — so the filter is only issued when there is something to
	// exclude (M3: the game picker grows the list as it samples levels).
	// Placeholders stay contiguous (pgx extended protocol allows no gaps):
	// $1 = difficulty, optional $2 = text[] excludes, last = limit.
	// source = 'opentdb': M6 round-1 review — the legacy tier pool is the
	// VETTED OpenTDB corpus only; OTQA rows carry the same synthetic
	// difficulty and must never be picked pre-labeling.
	query := "SELECT * FROM questions WHERE difficulty = $1 AND source = 'opentdb'"
	args := []interface{}{difficulty}
	if len(excludeQuestionIDs) > 0 {
		// Explicit text[] cast: pgx sends the slice untyped otherwise
		// ("could not determine data type of parameter $2").
		query += " AND id::text <> ALL($2::text[])"
		args = append(args, excludeQuestionIDs)
	}
	query += fmt.Sprintf(" ORDER BY random() LIMIT $%d", len(args)+1)
	args = append(args, limit)
	err := r.db.Query(ctx, &questions, query, args...)
	return questions, err
}

// bandRandomByRank selects one random rated question whose rank (ordered
// by strength, then id for determinism) lies in the 1-based [lo, hi]
// window, computed over the NOT-yet-EXCLUDED rated corpus. `excludeIDs`
// are filtered INSIDE the window subquery, so the exclusion shrinks the
// ranking right away (fresh banding per query; excluded ids are ≤ 30 —
// the quantile drift is negligible).
//
// Taste layer when excludeCultural: the cultural predicate is the exact
// Go-side classification (repository.IsCulturalCategory / source prefixes):
//   - OTQA cultural subcategories (models.OtqaCulturalCategories) — exact;
//   - OpenTDB Entertainment:% / Celebrities                — prefix match.
func (r *questionRepo) RandomByRankBand(ctx context.Context, lo, hi int, excludeCultural bool, limit int, excludeQuestionIDs []string) ([]models.Question, error) {
	// Placeholders stay contiguous (pgx extended protocol allows no gaps):
	// $1/$2 = lo/hi rank window, optional $3 = text[] excludes, optional
	// $4 = the OTQA cultural category list (explicit text[] casts because
	// pgx sends slices untyped otherwise), last = limit.
	query := `SELECT id, question, options, correct_index, difficulty, category, source, created_at, strength
FROM (
    SELECT id, question, options, correct_index, difficulty, category, source, created_at, strength,
           row_number() OVER (ORDER BY strength, id) AS rn
    FROM questions
    WHERE strength IS NOT NULL
      -- M10 fix: wwtbam rows live ONLY in their strict per-level pools
      -- (strength == level); a rank band never guarantees that.
      AND source <> 'wwtbam'` + "\n"

	args := []interface{}{lo, hi}
	if len(excludeQuestionIDs) > 0 {
		args = append(args, excludeQuestionIDs) // $3 — excludes
		query += fmt.Sprintf("        AND id::text <> ALL($%d::text[])\n", len(args))
	}
	query += ") ranked\nWHERE ranked.rn BETWEEN $1 AND $2\n" // $1 = lo, $2 = hi
	if excludeCultural {
		args = append(args, models.OtqaCulturalCategories)
		query += fmt.Sprintf(`  AND NOT (
        (source = 'otqa' AND category = ANY($%d::text[]))
        OR (source = 'opentdb' AND (category LIKE 'Entertainment:%%' OR category = 'Celebrities'))
    )`+"\n", len(args))
	}
	args = append(args, limit)
	query += fmt.Sprintf("ORDER BY random() LIMIT $%d", len(args))

	var questions []models.Question
	err := r.db.Query(ctx, &questions, query, args...)
	return questions, err
}

// RandomByCasual implements the S2 casual-pool query (see the interface
// doc): rated strength 1..maxStrength with the casual blacklist applied
// per corpus. The blacklist is SQL-side (one round trip decides) AND
// mirrored in models.IsCasualBlacklisted for the Go-side gate tests.
//
// Blacklist shape (per corpus):
//   - otqa:     category = ANY(CasualBlacklistOtqa) — exact list;
//   - opentdb:  category LIKE ANY(CasualBlacklistOpentdbPrefixes + '%')
//     OR category = ANY(CasualBlacklistOpentdbExact).
//
// Placeholders stay contiguous (pgx extended protocol allows no gaps):
// $1 = maxStrength, $2 = otqa exact list, $3 = opentdb prefix patterns,
// $4 = opentdb exact list, optional $5 = text[] excludes, last = limit.
func (r *questionRepo) RandomByCasual(ctx context.Context, maxStrength, limit int, excludeQuestionIDs []string) ([]models.Question, error) {
	patterns := make([]string, 0, len(models.CasualBlacklistOpentdbPrefixes))
	for _, p := range models.CasualBlacklistOpentdbPrefixes {
		patterns = append(patterns, p+"%")
	}
	query := `SELECT id, question, options, correct_index, difficulty, category, source, created_at, strength
FROM questions
WHERE strength BETWEEN 1 AND $1
  -- M10 fix: the casual blend must only mix OTHER sources' rows at
  -- levels 1–3 (the S2 rule); wwtbam rows are served strictly at
  -- strength == level by their dedicated per-level pools, never here —
  -- otherwise a strength-1 wwtbam row lands at L2/L3.
  AND source <> 'wwtbam'
  AND NOT (
        (source = 'otqa' AND category = ANY($2::text[]))
        OR (source = 'opentdb' AND
            (category LIKE ANY($3::text[]) OR category = ANY($4::text[])))
    )` + "\n"
	args := []interface{}{maxStrength, models.CasualBlacklistOtqa, patterns,
		models.CasualBlacklistOpentdbExact}
	if len(excludeQuestionIDs) > 0 {
		args = append(args, excludeQuestionIDs)
		query += fmt.Sprintf("  AND id::text <> ALL($%d::text[])\n", len(args))
	}
	query += "ORDER BY random()"
	if limit > 0 {
		args = append(args, limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}

	var questions []models.Question
	err := r.db.Query(ctx, &questions, query, args...)
	return questions, err
}

// RandomByFailSafeCasual is the S2 last-resort pool: vetted opentdb EASY
// rows restricted to the fail-safe categories (GK/Geo/Sports/History/Food —
// none of them casual-blacklisted, so the degradation stays inside the S2
// guarantee). Raw tier sampling deliberately never serves levels 1–3.
// $1 = the fail-safe category list, optional $2 = text[] excludes, last =
// limit.
func (r *questionRepo) RandomByFailSafeCasual(ctx context.Context, categories []string, limit int, excludeQuestionIDs []string) ([]models.Question, error) {
	query := `SELECT id, question, options, correct_index, difficulty, category, source, created_at, strength
FROM questions
WHERE difficulty = 'easy' AND source = 'opentdb' AND category = ANY($1::text[])` + "\n"
	args := []interface{}{categories}
	if len(excludeQuestionIDs) > 0 {
		args = append(args, excludeQuestionIDs)
		query += fmt.Sprintf("  AND id::text <> ALL($%d::text[])\n", len(args))
	}
	query += "ORDER BY random()"
	if limit > 0 {
		args = append(args, limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}

	var questions []models.Question
	err := r.db.Query(ctx, &questions, query, args...)
	return questions, err
}

// CountBySourceLevel counts one source's questions whose strength is
// exactly `level` (see the interface doc: the wwtbam ingest stores
// strength = the show level, 1..15; NULL-strength rows are not on the
// strength axis and never count).
func (r *questionRepo) CountBySourceLevel(ctx context.Context, source string, level int) (int, error) {
	var row struct {
		Count int `ksql:"count"`
	}
	err := r.db.QueryOne(ctx, &row,
		"SELECT count(*) AS count FROM questions WHERE source = $1 AND strength = $2",
		source, level)
	return row.Count, err
}

// RandomBySourceLevel draws up to `limit` random rows of one source at
// exactly one strength level, minus this room's already-picked ids. The
// placeholder shapes mirror RandomByTier: $1 = source, $2 = level,
// optional $3 = text[] excludes (explicit cast: pgx sends slices untyped),
// last = limit.
func (r *questionRepo) RandomBySourceLevel(ctx context.Context, source string, level int, limit int, excludeQuestionIDs []string) ([]models.Question, error) {
	var questions []models.Question
	query := `SELECT id, question, options, correct_index, difficulty, category, source, created_at, strength
FROM questions
WHERE source = $1 AND strength = $2` + "\n"
	args := []interface{}{source, level}
	if len(excludeQuestionIDs) > 0 {
		args = append(args, excludeQuestionIDs)
		query += "  AND id::text <> ALL($3::text[])\n"
	}
	query += "ORDER BY random()"
	if limit > 0 {
		args = append(args, limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	err := r.db.Query(ctx, &questions, query, args...)
	return questions, err
}

// CountBySource counts every row of one source (any strength).
func (r *questionRepo) CountBySource(ctx context.Context, source string) (int, error) {
	var row struct {
		Count int `ksql:"count"`
	}
	err := r.db.QueryOne(ctx, &row,
		"SELECT count(*) AS count FROM questions WHERE source = $1", source)
	return row.Count, err
}
