package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"boggle-api/internal/millionaire/models"
	"boggle-api/internal/millionaire/repository"
)

// This file pins the S2 casual-pool L1–3 rule (v3-ux-plan "Question
// quality", backend M10): levels 1–3 draw casual → widened-casual
// (strength 1–5, logged) → fail-safe opentdb easy GK/Geo/Sports/History/
// Food (logged degraded) — and NEVER the raw tier pool. The four branches
// are driven over the in-memory fake (deterministic); the DB-level gate
// (50 fresh rooms against the REAL repository + dev DB) closes the file.

// casualRow builds a strength-rated fixture question for the casual pool.
func casualRow(id string, strength int, category, source string) models.Question {
	s := strength
	return models.Question{
		ID: id, Question: "casual " + id, Options: `["a","b","c","d"]`,
		CorrectIndex: 0, Difficulty: "medium", Category: category,
		Source: source, Strength: &s, CreatedAt: time.Now().UTC(),
	}
}

// failSafeRow builds an unrated opentdb easy question (the fail-safe pool
// shape: strength NULL until the labeling pipeline rates it).
func failSafeRow(id, category string) models.Question {
	return models.Question{
		ID: id, Question: "failsafe " + id, Options: `["a","b","c","d"]`,
		CorrectIndex: 0, Difficulty: "easy", Category: category,
		Source: "opentdb", CreatedAt: time.Now().UTC(),
	}
}

// (f *baseFakeQuestionRepo) casual methods mirror the SQL contract: window
// (strength ≤ maxStrength), wwtbam excluded (M10 fix — wwtbam rows are
// served strictly at strength == level by their own per-level pools,
// never by the casual blend), blacklist (models.IsCasualBlacklisted) and
// the room's exclusion set apply; deterministic first-hit order like the
// other fake methods.
func (f *baseFakeQuestionRepo) RandomByCasual(ctx context.Context, maxStrength, limit int, exclude []string) ([]models.Question, error) {
	f.casualCalls = append(f.casualCalls, maxStrength)
	var out []models.Question
	for _, q := range f.casual {
		if q.Source == WWTBAMSource {
			continue
		}
		if q.Strength == nil || *q.Strength < 1 || *q.Strength > maxStrength {
			continue
		}
		if models.IsCasualBlacklisted(q.Category, q.Source) {
			continue
		}
		if excludedID(exclude, q.ID) {
			continue
		}
		out = append(out, q)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *baseFakeQuestionRepo) RandomByFailSafeCasual(ctx context.Context, categories []string, limit int, exclude []string) ([]models.Question, error) {
	f.failSafeCalls = append(f.failSafeCalls, 1)
	allowed := map[string]bool{}
	for _, c := range categories {
		allowed[c] = true
	}
	var out []models.Question
	for _, q := range f.failSafe {
		if q.Difficulty != "easy" || q.Source != "opentdb" || !allowed[q.Category] {
			continue
		}
		if excludedID(exclude, q.ID) {
			continue
		}
		out = append(out, q)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// TestCasualSelectionServesStrength1to3 is branch 1: a healthy casual pool
// serves every L1–3 variant; the widened window and the fail-safe pool are
// never queried.
func TestCasualSelectionServesStrength1to3(t *testing.T) {
	repo := &baseFakeQuestionRepo{}
	for i := 1; i <= 30; i++ { // far more than the 6 L1–3 draws
		repo.casual = append(repo.casual, casualRow(
			fmt.Sprintf("cas-%02d", i), (i%3)+1, "General Knowledge", "opentdb"))
	}
	for i := 1; i <= 12; i++ { // legacy decoys for the L4–15 path (rated=0)
		repo.legacy = append(repo.legacy, legacyQ(i, "easy"), legacyQ(i, "medium"), legacyQ(i, "hard"))
	}
	repo.failSafe = append(repo.failSafe, failSafeRow("fs-1", "Geography"))

	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(11)}
	list, err := g.selectQuestions(context.Background(), "room-casual")
	if err != nil {
		t.Fatal(err)
	}
	for level := 1; level <= 3; level++ {
		for _, v := range list[level] {
			if !strings.HasPrefix(v.questionID, "cas-") {
				t.Fatalf("level %d drew %s — outside the casual pool", level, v.questionID)
			}
		}
	}
	for _, call := range repo.casualCalls {
		if call != casualMaxStrength {
			t.Fatalf("casual calls = %v — the widened window must not trigger with a healthy pool", repo.casualCalls)
		}
	}
	if len(repo.failSafeCalls) != 0 {
		t.Fatal("fail-safe pool queried although the casual pool was healthy")
	}
}

// TestCasualSelectionWidenToStrength5 is branch 2: the casual window is
// dry, strength 4–5 non-blacklisted rows exist → the widened window serves
// them (the widening call with maxStrength=5 is pinned).
func TestCasualSelectionWidenToStrength5(t *testing.T) {
	repo := &baseFakeQuestionRepo{}
	for i := 1; i <= 12; i++ { // ALL candidates sit at strength 4–5
		repo.casual = append(repo.casual, casualRow(
			fmt.Sprintf("wide-%02d", i), 3+((i%2)+1), "OTQA:general", "otqa"))
	}
	for i := 1; i <= 12; i++ { // legacy decoys for the L4–15 path
		repo.legacy = append(repo.legacy, legacyQ(i, "easy"), legacyQ(i, "medium"), legacyQ(i, "hard"))
	}
	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(12)}
	list, err := g.selectQuestions(context.Background(), "room-widen")
	if err != nil {
		t.Fatal(err)
	}
	for level := 1; level <= 3; level++ {
		for _, v := range list[level] {
			if !strings.HasPrefix(v.questionID, "wide-") {
				t.Fatalf("level %d drew %s — outside the widened casual pool", level, v.questionID)
			}
		}
	}
	// Blend-agnostic chain assertions (M10 S3): the 1..3 window is queried
	// first and dries (all candidates sit at strength 4–5), and the widen
	// to 5 happens. The exact call ORDER now depends on the blend coin
	// (wwtbam-first re-enters the chain via drawCasual), so order is not
	// pinned — only that the widened window was consulted, per level.
	widened := 0
	for _, call := range repo.casualCalls {
		if call == casualWideMaxStrength {
			widened++
		}
	}
	if widened < 6 {
		t.Fatalf("casual calls = %v — the widened window must be consulted for every L1–3 variant", repo.casualCalls)
	}
	if len(repo.failSafeCalls) != 0 {
		t.Fatal("widened window must satisfy L1–3 when strength 4–5 rows exist")
	}
}

// TestCasualSelectionFailSafeFallback is branch 3: casual pools dry and a
// decoy legacy tier pool sits ready — the fail-safe opentdb pool must
// serve (and the raw tier pool must STILL not be consulted for L1–3).
func TestCasualSelectionFailSafeFallback(t *testing.T) {
	repo := &baseFakeQuestionRepo{}
	for i := 1; i <= 60; i++ {
		repo.legacy = append(repo.legacy, legacyQ(i, "easy"), legacyQ(i, "medium"), legacyQ(i, "hard"))
	}
	repo.failSafe = append(repo.failSafe,
		failSafeRow("fs-gk-1", "General Knowledge"),
		failSafeRow("fs-geo-1", "Geography"),
		failSafeRow("fs-sport-1", "Sports"),
		failSafeRow("fs-hist-1", "History"),
		failSafeRow("fs-gk-2", "General Knowledge"),
		failSafeRow("fs-geo-2", "Geography"),
		failSafeRow("fs-gk-3", "General Knowledge"),
		failSafeRow("fs-hist-2", "History"),
	)
	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(13)}
	list, err := g.selectQuestions(context.Background(), "room-failsafe")
	if err != nil {
		t.Fatal(err)
	}
	for level := 1; level <= 3; level++ {
		for _, v := range list[level] {
			if !strings.HasPrefix(v.questionID, "fs-") {
				t.Fatalf("level %d drew %s — the fail-safe pool must serve when casual pools dry", level, v.questionID)
			}
		}
	}
	if len(repo.casualCalls) < 6 {
		t.Fatalf("both casual windows must have been queried: %v", repo.casualCalls)
	}
	for _, call := range repo.casualCalls {
		if call != casualMaxStrength && call != casualWideMaxStrength {
			t.Fatalf("casual calls = %v — unexpected window", repo.casualCalls)
		}
	}
	if len(repo.failSafeCalls) != 6 {
		t.Fatalf("fail-safe calls = %d — one per L1–3 variant", len(repo.failSafeCalls))
	}
}

// TestCasualSelectionFailSafeCategories — the fail-safe pool drawing must
// pass exactly failSafeCasualCategories (S2's GK/Geo/Sports/History/Food).
func TestCasualSelectionFailSafeCategories(t *testing.T) {
	repo := &baseFakeQuestionRepo{}
	repo.failSafe = append(repo.failSafe, failSafeRow("fs-any-1", "Anything Else"))
	// failSafe is filtered strictly: 'Anything Else' is never served; add
	// eligible rows enough for all 6 L1–3 draws.
	repo.failSafe = append(repo.failSafe,
		failSafeRow("fs-food-1", "Food & Drink"),
		failSafeRow("fs-food-2", "Food & Drink"),
		failSafeRow("fs-gk-1", "General Knowledge"),
		failSafeRow("fs-gk-2", "General Knowledge"),
		failSafeRow("fs-geo-1", "Geography"),
		failSafeRow("fs-sport-1", "Sports"),
	)
	for i := 1; i <= 12; i++ { // legacy decoys for the L4–15 path
		repo.legacy = append(repo.legacy, legacyQ(i, "easy"), legacyQ(i, "medium"), legacyQ(i, "hard"))
	}
	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(14)}
	list, err := g.selectQuestions(context.Background(), "room-fs-cats")
	if err != nil {
		t.Fatal(err)
	}
	for level := 1; level <= 3; level++ {
		for _, v := range list[level] {
			if v.questionID == "fs-any-1" {
				t.Fatalf("level %d served a non-fail-safe-category question", level)
			}
		}
	}
}

// TestCasualSelectionNeverServesBlacklisted — a casual pool full of
// blacklisted rows (Entertainment:*, Science: Gadgets, Celebrities, OTQA
// cultural/animals) cannot leak ANY of them into L1–3: only the eligible
// rows ever surface.
func TestCasualSelectionNeverServesBlacklisted(t *testing.T) {
	repo := &baseFakeQuestionRepo{}
	for i := 1; i <= 12; i++ {
		repo.casual = append(repo.casual, casualRow(
			fmt.Sprintf("bw-%02d", i), 2, "Entertainment: Video Games", "opentdb"))
	}
	repo.casual = append(repo.casual,
		casualRow("bw-gadgets", 2, "Science: Gadgets", "opentdb"),
		casualRow("bw-celeb", 2, "Celebrities", "opentdb"),
		casualRow("bw-otqa-music", 2, "OTQA:music", "otqa"),
		casualRow("bw-otqa-animals", 2, "OTQA:animals", "otqa"),
		casualRow("cw-good-1", 2, "General Knowledge", "opentdb"),
		casualRow("cw-good-2", 3, "OTQA:geography", "otqa"),
		casualRow("cw-good-3", 1, "Sports", "opentdb"),
		casualRow("cw-good-4", 2, "History", "opentdb"),
		casualRow("cw-good-5", 3, "OTQA:general", "otqa"),
		casualRow("cw-good-6", 2, "Geography", "opentdb"),
	)
	for i := 1; i <= 12; i++ { // legacy decoys for the L4–15 path
		repo.legacy = append(repo.legacy, legacyQ(i, "easy"), legacyQ(i, "medium"), legacyQ(i, "hard"))
	}
	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(15)}
	list, err := g.selectQuestions(context.Background(), "room-blacklist")
	if err != nil {
		t.Fatal(err)
	}
	for level := 1; level <= 3; level++ {
		for _, v := range list[level] {
			if !strings.HasPrefix(v.questionID, "cw-good") {
				t.Fatalf("level %d drew %s — a blacklisted row leaked through the casual pool", level, v.questionID)
			}
		}
	}
}

// TestCasualSelectionHonestFailure is branch 4: every casual window AND
// the fail-safe pool dry (with a fat legacy tier pool ready) → selection
// FAILS and the error names the fail-safe exhaustion — the raw tier pool
// must never be the L1–3 answer.
func TestCasualSelectionHonestFailure(t *testing.T) {
	repo := &baseFakeQuestionRepo{}
	for i := 1; i <= 60; i++ { // tier pool full — still forbidden for L1–3
		repo.legacy = append(repo.legacy, legacyQ(i, "easy"), legacyQ(i, "medium"), legacyQ(i, "hard"))
	}
	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(16)}
	_, err := g.selectQuestions(context.Background(), "room-dry")
	if err == nil {
		t.Fatal("selection must fail honestly when casual AND fail-safe pools are dry (tier sampling forbidden at L1–3)")
	}
	if !strings.Contains(err.Error(), "fail-safe") {
		t.Fatalf("err = %v — want the fail-safe exhaustion message", err)
	}
}

// ---------------------------------------------------------------------------
// DB GATE (TASK C): 50 fresh rooms against the REAL question repository on
// the dev DB. Every level-1-to-3 pick must satisfy the S2 rule as the
// repository query contract expresses it:
//
//	not casual-blacklisted (either corpus) AND
//	strength NULL (fail-safe pool) or 1..casualWideMaxStrength=5,
//
// plus the explicit "50 rooms × L1 never from a blacklist category"
// invariant and the room-level invariants (30 distinct picks, 2 per level).
// Runs SERIALLY with the other suites (recent OOM discipline); it only
// READS from the shared corpus — no rooms or room_questions rows are
// created (the selections are in-memory rooms).
// ---------------------------------------------------------------------------

func TestCasualPoolGate50RoomsL1to3Rule(t *testing.T) {
	db := newServiceTestDB(t) // skips ONLY when the dev DB is unreachable (never in-container)
	repo := repository.NewQuestionRepository(db)
	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(17)}
	ctx := context.Background()

	const rooms = 50
	var l1IDs []string
	for room := 1; room <= rooms; room++ {
		roomID := fmt.Sprintf("gate-room-%02d", room)
		list, err := g.selectQuestions(ctx, roomID)
		if err != nil {
			t.Fatalf("room %d: selection failed: %v", room, err)
		}
		seen := map[string]bool{}
		for level := 1; level <= 15; level++ {
			if len(list[level]) != 2 {
				t.Fatalf("room %d level %d: %d variants, want 2", room, level, len(list[level]))
			}
			for _, v := range list[level] {
				if seen[v.questionID] {
					t.Fatalf("room %d: duplicate question %s", room, v.questionID)
				}
				seen[v.questionID] = true
			}
		}

		// Contract re-read for the 6 L1–3 picks: fetch their category /
		// source / strength exactly as the casual queries govern them.
		var picks []string
		for level := 1; level <= 3; level++ {
			for _, v := range list[level] {
				picks = append(picks, v.questionID)
				if level == 1 {
					l1IDs = append(l1IDs, v.questionID)
				}
			}
		}
		var rows []struct {
			ID       string `ksql:"id"`
			Category string `ksql:"category"`
			Source   string `ksql:"source"`
			Strength *int   `ksql:"strength"`
		}
		if err := db.Query(ctx, &rows,
			"SELECT id, category, source, strength FROM millionaire_questions WHERE id::text = ANY($1::text[])", picks); err != nil {
			t.Fatalf("room %d: contract read failed: %v", room, err)
		}
		if len(rows) != len(picks) {
			t.Fatalf("room %d: contract read returned %d of %d rows", room, len(rows), len(picks))
		}
		for _, r := range rows {
			if models.IsCasualBlacklisted(r.Category, r.Source) {
				t.Fatalf("room %d: L1–3 pick %s is blacklisted (%s / %s) — S2 broken", room, r.ID, r.Source, r.Category)
			}
			if r.Strength != nil && (*r.Strength < 1 || *r.Strength > casualWideMaxStrength) {
				t.Fatalf("room %d: L1–3 pick %s carries strength %d — outside the casual order's 1..%d guarantee", room, r.ID, *r.Strength, casualWideMaxStrength)
			}
		}
	}

	// Explicit L1 invariant: NOT ONE of the 50 rooms' level-1 questions
	// may come from a blacklist category.
	if len(l1IDs) != 2*rooms {
		t.Fatalf("collected %d L1 picks, want %d", len(l1IDs), 2*rooms)
	}
	var l1rows []struct {
		ID       string `ksql:"id"`
		Category string `ksql:"category"`
		Source   string `ksql:"source"`
	}
	if err := db.Query(ctx, &l1rows,
		"SELECT id, category, source FROM millionaire_questions WHERE id::text = ANY($1::text[])", l1IDs); err != nil {
		t.Fatal(err)
	}
	for _, r := range l1rows {
		if models.IsCasualBlacklisted(r.Category, r.Source) {
			t.Fatalf("L1 pick %s is from blacklisted %q (%s) — the L1 invariant is broken", r.ID, r.Category, r.Source)
		}
	}
}
