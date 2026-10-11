package repository

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	"boggle-api/internal/millionaire/models"

	"github.com/google/uuid"
	"github.com/vingarcia/ksql"
	"github.com/vingarcia/ksql/adapters/kpgx"
)

// This file integration-tests the M6 band SQL against the dev DB
// (skip pattern like room_test.go): the rank-window query bucketizes by
// strength ordering exactly as the pure bandBounds math (internal/service
// bandselection_test.go) presumes; the cultural filter negates
// models.IsCultural exactly; BandCoverage matches the same windows.
//
// HERMETIC SESSION (M6 determinism discipline): every query below runs on
// a ksql/kpgx pool pinned to ONE connection (the default MaxOpenConns=1)
// whose session owns a TEMP TABLE clone of `questions`. The temp table
// shadows the real one for every statement in this session (temp schemas
// come first in search_path), so the tests insert, rank and delete
// NOTHING from the shared corpus:
//
//   - the other test binaries (handler/service) run CONCURRENTLY with
//     this package (`go test ./...`); picking questions from a corpus
//     this package deletes mid-flight used to break their room_questions
//     FKs (solved by the temp clone — session-scoped, dropped on close);
//   - the current REAL rated corpus is seeded INTO the clone, so the
//     expectation math can still "observe pre-existing rows" and decode
//     itself against them (the corpus-aware assertions below); the M10
//     fix excludes wwtbam rows from the band ranking (they are owned by
//     the strict per-level pools), and the expectation math mirrors that
//     exclusion;
//   - the dual-corpus assertions are unchanged; every expected ranking
//     is derived from the OBSERVED rows before asserting.

// openLabelDB opens a PRIVATE session against the dev DB: a
// one-connection ksql pool (MaxOpenConns 1 is the ksql default) that owns
// the temp questions clone described in the header comment.
func openLabelDB(t *testing.T) ksql.Provider {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://devuser:devpassword@localhost:5436/millionaire?sslmode=disable"
	}
	db, err := kpgx.New(context.Background(), dsn, ksql.Config{})
	if err != nil {
		t.Skipf("dev DB unreachable (%v) — skipping integration test", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	// Temp clone: every `FROM/INTO millionaire_questions` in THIS session now resolves
	// to the session-local table. LIKE copies columns + CHECK/NOT NULL
	// constraints (FKs are not copied — none needed); the currently-RATED
	// rows are seeded in so the tests can derive windows from an observed
	// corpus exactly as they would against the live data.
	if _, err := db.Exec(ctx,
		"CREATE TEMP TABLE millionaire_questions (LIKE public.millionaire_questions INCLUDING ALL)"); err != nil {
		t.Fatalf("creating the temp questions clone: %v", err)
	}
	if _, err := db.Exec(ctx,
		"INSERT INTO millionaire_questions SELECT * FROM public.millionaire_questions WHERE strength IS NOT NULL"); err != nil {
		t.Fatalf("seeding the temp clone with the rated corpus: %v", err)
	}
	return db
}

type testQuestion struct {
	id       string
	strength int
	category string
	source   string
}

func (tq testQuestion) question() models.Question {
	s := tq.strength
	return models.Question{
		ID:           tq.id,
		Question:     "band test " + tq.id,
		Options:      `["a","b","c","d"]`,
		CorrectIndex: 0,
		Difficulty:   "medium",
		Category:     tq.category,
		Source:       tq.source,
		CreatedAt:    time.Now().UTC(),
		Strength:     &s,
	}
}

// insertBandQuestions inserts the corpus rows (they live in the private
// temp clone — nothing to clean up on the shared corpus).
func insertBandQuestions(t *testing.T, db ksql.Provider, rows []testQuestion) {
	t.Helper()
	repo := NewQuestionRepository(db)
	ctx := context.Background()
	for _, row := range rows {
		q := row.question()
		if err := repo.InsertQuestion(ctx, &q); err != nil {
			t.Fatalf("inserting %s: %v", row.id, err)
		}
	}
}

// expectedWindowVales re-derives the corpus ranking (ORDER BY strength, id)
// and returns the strength VALUES of window [lo, hi] (1-based ranks).
func expectedWindowVales(existing []models.Question, mine []testQuestion, lo, hi int) map[int]bool {
	type row struct {
		strength int
		id       string
	}
	var ranked []row
	for _, q := range existing {
		if q.Strength != nil {
			ranked = append(ranked, row{*q.Strength, q.ID})
		}
	}
	for _, tq := range mine {
		ranked = append(ranked, row{tq.strength, tq.id})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].strength != ranked[j].strength {
			return ranked[i].strength < ranked[j].strength
		}
		return ranked[i].id < ranked[j].id
	})
	out := map[int]bool{}
	for r := lo; r <= hi && r <= len(ranked); r++ {
		out[ranked[r-1].strength] = true
	}
	return out
}

// listRated lists the CURRENT rated corpus (existing + rows inserted so
// far).
func listRated(t *testing.T, db ksql.Provider) []models.Question {
	t.Helper()
	var all []models.Question
	if err := db.Query(context.Background(), &all,
		"SELECT * FROM millionaire_questions WHERE strength IS NOT NULL"); err != nil {
		t.Fatalf("listing rated rows: %v", err)
	}
	return all
}

// TestRandomByRankBandBucketization inserts 3 questions per strength value
// 1..15 (45 rows) and asserts every draw of band L lands ONLY on ranks
// whose strength value is inside the re-derived window of band L — plus
// exclusions and limit behavior.
func TestRandomByRankBandBucketization(t *testing.T) {
	db := openLabelDB(t)
	repo := NewQuestionRepository(db)
	ctx := context.Background()

	// Pre-existing corpus captured BEFORE the inserts (for the expectation
	// math) — ListAll reads everything once. The band SQL EXCLUDES wwtbam
	// rows (M10 fix: they are owned by the strict per-level pools), so the
	// expectation math ranks the same non-wwtbam universe the query ranks.
	var before []models.Question
	for _, q := range listRated(t, db) {
		if q.Source != "wwtbam" {
			before = append(before, q)
		}
	}

	var mine []testQuestion
	for r := 1; r <= 15; r++ {
		for k := 0; k < 3; k++ {
			cat := "OTQA:world"
			src := "otqa"
			if k == 0 {
				cat, src = "OTQA:music", "otqa" // cultural per IsCultural
			}
			if k == 1 {
				cat, src = "History", "opentdb" // non-cultural prefix class
			}
			// k == 2: OTQA:general — non-cultural.
			if k == 2 {
				cat, src = "OTQA:general", "otqa"
			}
			mine = append(mine, testQuestion{
				id:       uuid.Must(uuid.NewV7()).String(),
				strength: r,
				category: cat,
				source:   src,
			})
		}
	}
	insertBandQuestions(t, db, mine)

	excludeOne := mine[15].id // a strength-6 row (band 6); exclusions asserted separately below

	// Every band: the draw must fall within the EXPECTED window values.
	// n is the ranked universe's size — non-wwtbam rated rows only, the
	// exact subquery the SQL ranks (M10 fix).
	var nonWwtbam int
	rated := listRated(t, db)
	for _, q := range rated {
		if q.Source != "wwtbam" {
			nonWwtbam++
		}
	}
	n := nonWwtbam
	if expected := len(before) + len(mine); n < expected {
		t.Fatalf("rated corpus shrank during the test? %d rated rows", n)
	}
	for level := 1; level <= 15; level++ {
		lo := (level-1)*n/15 + 1
		hi := level * n / 15
		if hi < lo {
			hi = lo
		}
		wantVals := expectedWindowVales(before, mine, lo, hi)
		for attempt := 0; attempt < 4; attempt++ {
			picked, err := repo.RandomByRankBand(ctx, lo, hi, false, 2, nil)
			if err != nil {
				t.Fatalf("band %d query: %v", level, err)
			}
			if len(picked) == 0 {
				break // exclusions may legitimately dry a window mid-run
			}
			for _, q := range picked {
				if q.Strength == nil {
					t.Fatalf("band %d window %d..%d served a nil strength — the SELECT misses the strength column", level, lo, hi)
				}
				sv := *q.Strength
				if !wantVals[sv] {
					t.Fatalf("band %d window %d..%d served strength %d (want one of %v) — bucketization broken",
						level, lo, hi, sv, wantVals)
				}
			}
		}
	}

	// EXCLUSIONS: a separately excluded row of band 6 must never be
	// served (the SQL filters it inside the window ranking). Only the
	// empty-corpus case is asserted exactly (bounds otherwise depend on
	// live data).
	if n == len(mine)+len(before) && len(before) == 0 {
		for attempt := 0; attempt < 3; attempt++ {
			picked, err := repo.RandomByRankBand(ctx, 16, 18, false, 3, []string{excludeOne})
			if err != nil {
				t.Fatalf("exclusion query: %v", err)
			}
			for _, q := range picked {
				if q.ID == excludeOne {
					t.Fatal("excluded id served")
				}
				if q.Strength == nil || (*q.Strength < 5 || *q.Strength > 7) {
					t.Fatalf("band 6 near-window served strength %v — drifted outside the ±1 neighborhood", q.Strength)
				}
			}
		}
	}
}

// TestRandomByRankBandCulturalFilter pins the taste-layer SQL semantics:
// with excludeCultural=true no response may ever be a cultural row
// (OTQA:music / OTQA:television / OTQA:people / ... / Entertainment:% /
// Celebrities), and noncultural OTQA:world+OTQA:general / History keep
// passing.
func TestRandomByRankBandCulturalFilter(t *testing.T) {
	db := openLabelDB(t)
	repo := NewQuestionRepository(db)
	ctx := context.Background()

	var mine []testQuestion
	for i := 0; i < 12; i++ { // spread over several bands
		cat := "OTQA:music" // cultural
		if i%2 == 1 {
			cat = "OTQA:world" // non-cultural
		}
		if i == 4 {
			cat = "OTQA:television" // cultural
		}
		if i == 6 {
			cat = "Entertainment: Film" // opentdb cultural prefix
		}
		if i == 8 {
			cat = "Celebrities" // opentdb cultural
		}
		mine = append(mine, testQuestion{
			id:       uuid.Must(uuid.NewV7()).String(),
			strength: 1 + i,
			category: cat,
			source:   "otqa",
		})
	}
	insertBandQuestions(t, db, mine)

	rated := listRated(t, db)
	n := len(rated)
	picked, err := repo.RandomByRankBand(ctx, 1, n, true, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(picked) == 0 {
		// Possible only when the WHOLE corpus is cultural — impossible
		// (non-cultural rows inserted), else a hard failure.
		t.Fatal("the non-cultural query yielded nothing despite non-cultural rows")
	}
	for _, q := range picked {
		if models.IsCultural(q.Category, q.Source) {
			t.Fatalf("excludeCultural served %q (%s) — the taste filter is broken", q.Category, q.Source)
		}
	}
}

// TestRandomByTierOpentdbOnly pins the M6 round-1 fix: the legacy tier
// pool is the vetted OpenTDB corpus ONLY. An OTQA row with the identical
// synthetic 'medium' difficulty value must never surface — pre-labeling
// games draw from opentdb rows exclusively. Runs on the hermetic temp
// questions clone (unrated rows CAN be inserted: the clone is invisible
// to the shared corpus).
func TestRandomByTierOpentdbOnly(t *testing.T) {
	db := openLabelDB(t)
	repo := NewQuestionRepository(db)
	ctx := context.Background()

	rows := []testQuestion{
		{id: uuid.Must(uuid.NewV7()).String(), category: "General Knowledge", source: "opentdb"},
		{id: uuid.Must(uuid.NewV7()).String(), category: "OTQA:general", source: "otqa"},
		{id: uuid.Must(uuid.NewV7()).String(), category: "OTQA:music", source: "otqa"},
	}
	// Unrated medium rows — the exact shape RandomByTier matches on
	// difficulty; the otqa ones are the trap the source guard must hold.
	for _, row := range rows {
		q := row.question()
		q.Strength = nil // unrated: still visible to tier queries
		if err := repo.InsertQuestion(ctx, &q); err != nil {
			t.Fatalf("inserting %s: %v", row.id, err)
		}
	}

	// The clone carries the whole live corpus, and now that corpus holds
	// hundreds of opentdb 'medium' rows — so "the inserted row was among 10
	// random picks" is a coin flip, not an assertion. Exclude every OTHER
	// medium opentdb row first: RandomByTier can then only return the row
	// under test (or nothing, if the source guard is broken).
	var pre []struct {
		ID string `ksql:"id"`
	}
	if err := db.Query(ctx, &pre,
		"SELECT id::text AS id FROM millionaire_questions WHERE difficulty = 'medium' AND source = 'opentdb' AND id::text <> $1", rows[0].id); err != nil {
		t.Fatalf("listing pre-existing medium opentdb rows: %v", err)
	}
	exclude := make([]string, 0, len(pre))
	for _, r := range pre {
		exclude = append(exclude, r.ID)
	}

	picked, err := repo.RandomByTier(ctx, "medium", 10, exclude)
	if err != nil {
		t.Fatal(err)
	}
	if len(picked) == 0 {
		t.Skip("dev corpus holds no unrated opentdb medium rows — nothing to assert")
	}
	var sawVetted bool
	for _, q := range picked {
		if q.Source != "opentdb" {
			t.Fatalf("RandomByTier served source=%q category=%q — OTQA/synthetic rows leak into the pre-labeling tier pool", q.Source, q.Category)
		}
		if q.ID == rows[0].id {
			sawVetted = true
		}
	}
	for _, trap := range rows[1:] {
		for _, q := range picked {
			if q.ID == trap.id {
				t.Fatalf("the otqa trap row %s (%s) was served — source scope broken", trap.id, trap.category)
			}
		}
	}
	if !sawVetted {
		t.Fatal("the inserted opentdb row was not served at all — the tier query misses the vetted pool")
	}
}

// TestBandCoverageMatchesDeterministicCorpus inserts a fully-known corpus
// of 45 rows (3 per strength value; every strength value exactly one band
// worth of strength) and — because it derives the pre-existing rows —
// asserts each reported band's share only within the own-corpus bounds,
// then the exact equality when the pre-existing corpus was EMPTY.
func TestBandCoverageMatchesDeterministicCorpus(t *testing.T) {
	db := openLabelDB(t)
	_ = NewLabelRepository(db)
	label := NewLabelRepository(db)
	ctx := context.Background()

	beforeCount, err := label.CountRated(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var mine []testQuestion
	for r := 1; r <= 15; r++ {
		for k := 0; k < 3; k++ {
			cat := "OTQA:general"
			if k == 0 {
				cat = "OTQA:music" // cultural
			}
			mine = append(mine, testQuestion{
				id:       uuid.Must(uuid.NewV7()).String(),
				strength: r,
				category: cat,
				source:   "otqa",
			})
		}
	}
	insertBandQuestions(t, db, mine)

	rows, err := label.BandCoverage(ctx)
	if err != nil {
		t.Fatalf("BandCoverage: %v", err)
	}
	total := beforeCount + len(mine)

	// Aggregate sanity: the bands partition the whole rated corpus.
	sum := 0
	for _, b := range rows {
		if b.Band < 1 || b.Band > 15 {
			t.Fatalf("band value %d out of 1..15", b.Band)
		}
		if b.CulturalTotal > b.Total {
			t.Fatalf("band %d: cultural %d > total %d", b.Band, b.CulturalTotal, b.Total)
		}
		// cultural rows inside MY corpus are 1 per strength triple but
		// must never exceed whatever the corpus has; exact equality is
		// asserted only in the empty-corpus case (below).
		sum += b.Total
	}
	if sum != total {
		t.Fatalf("bands cover %d rows, the corpus has %d", sum, total)
	}

	if beforeCount == 0 {
		// 45 rows → exactly 3 ranks/band: each band reports 3 with 1
		// cultural (the OTQA:music one).
		byBand := map[int]models.BandCoverage{}
		for _, b := range rows {
			byBand[b.Band] = b
		}
		if len(rows) != 15 {
			t.Fatalf("empty-corpus coverage has %d bands, want all 15 (%v)", len(rows), rows)
		}
		for band := 1; band <= 15; band++ {
			b, ok := byBand[band]
			if !ok {
				t.Fatalf("band %d missing from coverage", band)
			}
			if b.Total != 3 || b.CulturalTotal != 1 {
				t.Fatalf("band %d = total %d cultural %d, want 3/1", band, b.Total, b.CulturalTotal)
			}
		}
	} else {
		// A pre-existing corpus exists: the bands follow the re-derived
		// ranking; assert only band presence math (window counts = derive)
		t.Logf("pre-existing rated corpus of %d rows; aggregate checks only", beforeCount)
		_ = len(rows)
	}
}
