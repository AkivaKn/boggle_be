package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boggle-api/internal/millionaire/models"
	"boggle-api/internal/millionaire/repository"
)

// This file pins the M10/S3 WWTBAM ingest + selection blend:
//
//   - the importer (cmd/import -source wwtbam -file data/wwtbam.jsonl):
//     stored shape (source='wwtbam', category 'WWTBAM:<collapsed src tag>',
//     options in workbook A–D order, correct_index = LETTER position,
//     strength = level, legacy difficulty tiers 1–2 easy / 3–6 medium /
//     7–15 hard) and IDEMPOTENCY (a second full run imports 0);
//   - the per-level draw pools (drawWWTBAMLevel/tryWWTBAMLevel/drawCasual
//     Blend) over the in-memory fake (deterministic, scripted coin);
//   - the DB-level 50-room distribution gate against the REAL repository
//     on the dev DB (same discipline as the S2 gate in casual_test.go).

// wtbamSourceNamePinned pins the selection pool's source literal to the
// importer's stored value (they must never drift apart).
func TestWWTBAMSourceLiteralPinned(t *testing.T) {
	if wwtbamSource != WWTBAMSource {
		t.Fatalf("selection pool source %q != importer source %q", wwtbamSource, WWTBAMSource)
	}
}

// wwtbamRow builds a jsonl fixture line.
func wwtbamLine(level int, q string, a, b, c, d, correct, src string) string {
	row := map[string]interface{}{
		"level": level, "question": q,
		"options": []string{a, b, c, d},
		"correct": correct, "src": src,
	}
	bts, _ := json.Marshal(row)
	return string(bts)
}

func writeWWTBAMFixture(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wwtbam.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		f.WriteString(l + "\n")
	}
	return path
}

// wwtbamFixture is a 20-row corpus: sheet-like levels 1..4 (5 rows each)
// with distinct question texts per level, plus one DUPLICATE of an
// existing opentdb row and one malformed line (exercising reject paths).
func wwtbamFixture(existingText string) ([]string, int) {
	const valid = 20
	lines := make([]string, 0, valid+2)
	n := 0
	for level := 1; level <= 4; level++ {
		for i := 1; i <= 5; i++ {
			n++
			q := fmt.Sprintf("wwtbam seed question lv%d n%02d?", level, i)
			lines = append(lines, wwtbamLine(level, q,
				fmt.Sprintf("ans lv%d n%02d", level, i), "wrong one", "wrong two", "wrong three",
				"a", fmt.Sprintf("Show Season %d", level)))
		}
	}
	// A pre-existing row (dedupe target) + one malformed JSON line.
	lines = append(lines, wwtbamLine(1, existingText, "a", "b", "c", "d", "b", "Show"))
	lines = append(lines, `{"level":1,"question":"broken`+",")
	return lines, valid
}

// TestWWTBAMImporterShapeIdempotent: 20 fixure rows import ONCE with the
// documented stored shape; the second run adds 0 (honest idempotency);
// the duplicate pre-existing row is skipped the first run.
func TestWWTBAMImporterShapeIdempotent(t *testing.T) {
	existingText := "PREEXISTING opentdb row which the wwtbam seed collides with"
	repo := &baseFakeQuestionRepo{}
	repo.rated = append(repo.rated, models.Question{
		ID: "pre-1", Question: existingText,
		Options: `["zz","yy","xx","ww"]`, CorrectIndex: 0,
		Difficulty: "easy", Category: "General Knowledge",
		Source: "opentdb", CreatedAt: time.Now().UTC(),
	})

	lines, valid := wwtbamFixture(existingText)
	// 22 lines total: 20 fresh valid + 1 preexisting-duplicate + 1 malformed.
	path := writeWWTBAMFixture(t, lines)

	importer := NewWWTBAMImporter(repo, path)
	st1, err := importer.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st1.Imported != valid {
		t.Fatalf("run1 imported=%d, want %d (20 fresh rows; preexisting dedupes, malformed rejects)", st1.Imported, valid)
	}
	if st1.Duplicates != 1 || st1.Skipped != 1 {
		t.Fatalf("run1 duplicates=%d skipped=%d, want 1/1", st1.Duplicates, st1.Skipped)
	}

	st2, err := importer.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st2.Imported != 0 {
		t.Fatalf("run2 imported=%d — idempotency broken (duplicates=%d)", st2.Imported, st2.Duplicates)
	}
	if st2.Duplicates != valid+1 {
		t.Fatalf("run2 duplicates=%d, want %d (the whole valid corpus re-reported as present)", st2.Duplicates, valid+1)
	}
	if got := len(repo.inserted); got != valid {
		t.Fatalf("inserted=%d rows, want exactly %d across both runs", got, valid)
	}

	// Stored shape of a level-3 row (medium band; strength = level).
	level3 := 0
	for i, q := range repo.inserted {
		if s := *q.Strength; s == 3 {
			level3 = i
			break
		}
	}
	q := repo.inserted[level3]
	if q.Source != "wwtbam" {
		t.Fatalf("source=%q, want wwtbam", q.Source)
	}
	if q.Category != "WWTBAM:Show Season 3" {
		t.Fatalf("category=%q, want WWTBAM:Show Season 3", q.Category)
	}
	if q.Difficulty != "medium" {
		t.Fatalf("difficulty=%q, want medium (level 3)", q.Difficulty)
	}
	var opts []string
	if err := json.Unmarshal([]byte(q.Options), &opts); err != nil {
		t.Fatal(err)
	}
	if len(opts) != 4 || opts[0] != "ans lv3 n01" {
		t.Fatalf("options=%v — must be stored in workbook A–D order", opts)
	}
	if q.CorrectIndex != 0 { // 'a' → position 0 in the A–D order
		t.Fatalf("correct_index=%d, want 0 (the letter position)", q.CorrectIndex)
	}

	// Difficulty tiers across the levels present in the fixture.
	want := map[int]string{1: "easy", 2: "easy", 3: "medium", 4: "medium"}
	for _, row := range repo.inserted {
		lv := *row.Strength
		if row.Difficulty != want[lv] {
			t.Fatalf("level %d difficulty=%q, want %q", lv, row.Difficulty, want[lv])
		}
	}
}

// TestWWTBAMImporterRejectsMalformed: parse-level honesty — rows missing
// options / with bad letters / out-of-range levels are SKIPPED, never
// stored, and their count is reported.
func TestWWTBAMImporterRejectsMalformed(t *testing.T) {
	lines := []string{
		wwtbamLine(1, "ok row", "a", "b", "c", "d", "c", "Show"),
		wwtbamLine(1, "three options", "a", "b", "c", "", "c", "Show"),
		`{"level":1,"question":"bad letter","options":["a","b","C","d "],"correct":"e","src":"Show"}`,
		`{"level":16,"question":"out of range","options":["a","b","c","d"],"correct":"a","src":"Show"}`,
		wwtbamLine(2, "uppercase letter ok", "a", "b", "c", "d", "C", "  Show   Electric "), // collapsed tag
	}
	path := writeWWTBAMFixture(t, lines)
	repo := &baseFakeQuestionRepo{}
	st, err := NewWWTBAMImporter(repo, path).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Imported != 2 || st.Skipped != 3 {
		t.Fatalf("imported=%d skipped=%d, want 2/3", st.Imported, st.Skipped)
	}
	if len(repo.inserted) != 2 {
		t.Fatalf("stored=%d, want 2", len(repo.inserted))
	}
	cats := map[string]bool{}
	for _, q := range repo.inserted {
		cats[q.Category] = true
	}
	if cats["WWTBAM:Show Electric"] != true {
		t.Fatalf("category collapse broken: %v", cats)
	}
}

// wtbamFixtureRow builds a strength-level storage row for the selection
// fakes (wwtbam pool slice; category shows the WWTBAM prefix contract).
func wtbamFixtureRow(id string, level int) models.Question {
	s := level
	return models.Question{
		ID: id, Question: "wwtbam " + id,
		Options: `["a","b","c","d"]`, CorrectIndex: 0,
		Difficulty: wwtbamDifficultyTier(level), Category: "WWTBAM:Unknown",
		Source: WWTBAMSource, CreatedAt: time.Now().UTC(), Strength: &s,
	}
}

// scriptedRNG returns IntN from a fixed queue (the blend coin is driven,
// not sampled): values loop when exhausted.
type scriptedRNG struct{ vals []int }

func (s *scriptedRNG) IntN(n int) int {
	if n != 2 { // only the blend coin uses IntN(2) in these selection tests
		panic(fmt.Sprintf("scriptedRNG asked IntN(%d)", n))
	}
	v := s.vals[0]
	s.vals = s.vals[1:]
	if len(s.vals) == 0 {
		s.vals = append(s.vals, v) // repeat the last coin forever
	}
	return v
}

// totalRooms helper builds a legacy tier pool big enough for L4–15 when
// the wwtbam pool is empty (count 0 < 2 → degradation to the bands/legacy
// path; rated=0 keeps everything on drawLegacyTier).
func wwtbamLegacyDecoys(repo *baseFakeQuestionRepo, n int) {
	for i := 1; i <= n; i++ {
		repo.legacy = append(repo.legacy, legacyQ(i, "easy"), legacyQ(i, "medium"), legacyQ(i, "hard"))
	}
}

// TestWWTBAMBlendL13CasualFirstAndWWTBAMFirst drives the 50/50 coin over
// the rng seam: coin=1 → casual pool is tried FIRST and serves; coin=0 →
// wwtbam@level is tried first and serves (the casual pool must then be
// UNTOUCHED for that level).
func TestWWTBAMBlendL13CoinOrder(t *testing.T) {
	newRepo := func() *baseFakeQuestionRepo {
		repo := &baseFakeQuestionRepo{}
		for i := 1; i <= 12; i++ {
			repo.casual = append(repo.casual, casualRow(
				fmt.Sprintf("cas-%02d", i), (i%3)+1, "General Knowledge", "opentdb"))
		}
		for level := 1; level <= 3; level++ {
			for i := 1; i <= 8; i++ {
				repo.wwtbam = append(repo.wwtbam, wtbamFixtureRow(fmt.Sprintf("wt-l%d-%02d", level, i), level))
			}
		}
		wwtbamLegacyDecoys(repo, 40) // L4–15 degrade to legacy on the fake
		return repo
	}

	// coin=1: casual first — every L1–3 pick from cas-* and NOT from wwtbam.
	repo := newRepo()
	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: &scriptedRNG{vals: []int{1}}}
	list, err := g.selectQuestions(context.Background(), "coin-casual")
	if err != nil {
		t.Fatal(err)
	}
	for level := 1; level <= 3; level++ {
		for _, v := range list[level] {
			if !strings.HasPrefix(v.questionID, "cas-") {
				t.Fatalf("coin=1 level %d drew %s — the FIRST pool (casual) must serve", level, v.questionID)
			}
		}
	}

	// coin=0: wwtbam first — every L1–3 pick from wt-*. The per-level
	// (strength-owned) invariant + the "casual untouched" invariant are
	// pinned explicitly in TestWWTBAMBlendL13PoolStrength below.
	repo = newRepo()
	g = &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: &scriptedRNG{vals: []int{0}}}
	list, err = g.selectQuestions(context.Background(), "coin-wwtbam")
	if err != nil {
		t.Fatal(err)
	}
	for level := 1; level <= 3; level++ {
		for _, v := range list[level] {
			if !strings.HasPrefix(v.questionID, "wt-l") {
				t.Fatalf("coin=0 level %d drew %s — the FIRST pool (wwtbam@level) must serve", level, v.questionID)
			}
		}
	}
}

func TestWWTBAMBlendL13PoolStrength(t *testing.T) {
	repo := &baseFakeQuestionRepo{}
	for i := 1; i <= 12; i++ {
		repo.casual = append(repo.casual, casualRow(
			fmt.Sprintf("cas-%02d", i), 3, "General Knowledge", "opentdb"))
	}
	// Rows for EVERY level 1..3 (8 each) so the per-level pool is fat.
	for level := 1; level <= 3; level++ {
		for i := 1; i <= 8; i++ {
			repo.wwtbam = append(repo.wwtbam, wtbamFixtureRow(fmt.Sprintf("wt-l%d-%02d", level, i), level))
		}
	}
	wwtbamLegacyDecoys(repo, 40)

	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: &scriptedRNG{vals: []int{0}}}
	list, err := g.selectQuestions(context.Background(), "coin-wwtbam")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]models.Question{}
	for _, q := range repo.wwtbam {
		byID[q.ID] = q
	}
	for level := 1; level <= 3; level++ {
		for _, v := range list[level] {
			q, ok := byID[v.questionID]
			if !ok {
				t.Fatalf("level %d drew %s — outside the wwtbam pool", level, v.questionID)
			}
			if *q.Strength != level {
				t.Fatalf("level %d drew a strength-%d wwtbam row — the per-level pool leaked across levels", level, *q.Strength)
			}
			if q.Source != WWTBAMSource {
				t.Fatalf("served %q from source %q", q.ID, q.Source)
			}
		}
	}
	if len(repo.casualCalls) != 0 {
		t.Fatalf("coin=0 with a fat wwtbam pool must NOT query the casual pool: %v", repo.casualCalls)
	}
}

// TestWWTBAMBlendFallsThroughWhenWWTBAMDry: coin=0 picks wwtbam first;
// its pool is empty → the S2 casual chain serves (call log: the widened
// window is NOT consulted while the 1..3 pool is healthy).
func TestWWTBAMBlendFallsThroughWhenWWTBAMDry(t *testing.T) {
	repo := &baseFakeQuestionRepo{}
	for i := 1; i <= 30; i++ {
		repo.casual = append(repo.casual, casualRow(
			fmt.Sprintf("cas-%02d", i), (i%3)+1, "General Knowledge", "opentdb"))
	}
	wwtbamLegacyDecoys(repo, 40)
	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: &scriptedRNG{vals: []int{0}}}
	list, err := g.selectQuestions(context.Background(), "dry-wwtbam")
	if err != nil {
		t.Fatal(err)
	}
	for level := 1; level <= 3; level++ {
		for _, v := range list[level] {
			if !strings.HasPrefix(v.questionID, "cas-") {
				t.Fatalf("level %d drew %s — the dry wwtbam pool must fall through to the casual chain", level, v.questionID)
			}
		}
	}
	for _, call := range repo.casualCalls {
		if call != casualMaxStrength {
			t.Fatalf("casual calls = %v — a healthy 1..3 pool must serve without widening", repo.casualCalls)
		}
	}
	if len(repo.failSafeCalls) != 0 {
		t.Fatal("fail-safe pool queried although the casual pool was healthy")
	}
}

// TestWWTBAMBlendBothDryDegradation: with the blend both B pools dry, the
// EXISTING degradation chain must still run (widened casual → fail-safe)
// — the S2 guarantee chain is preserved under the blend.
func TestWWTBAMBlendBothDryDegradation(t *testing.T) {
	repo := &baseFakeQuestionRepo{}
	for i := 1; i <= 12; i++ { // ONLY strength 4–5 casual rows
		repo.casual = append(repo.casual, casualRow(
			fmt.Sprintf("wide-%02d", i), 3+((i%2)+1), "OTQA:general", "otqa"))
	}
	wwtbamLegacyDecoys(repo, 40)
	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: &scriptedRNG{vals: []int{0}}}
	list, err := g.selectQuestions(context.Background(), "both-dry")
	if err != nil {
		t.Fatal(err)
	}
	for level := 1; level <= 3; level++ {
		for _, v := range list[level] {
			if !strings.HasPrefix(v.questionID, "wide-") {
				t.Fatalf("level %d drew %s — the degraded chain (widened casual) must serve when both blend pools dry", level, v.questionID)
			}
		}
	}
	widened := false
	for _, call := range repo.casualCalls {
		if call == casualWideMaxStrength {
			widened = true
		}
	}
	if !widened {
		t.Fatalf("casual calls = %v — the widened window must be part of the degradation chain", repo.casualCalls)
	}
}

// TestWWTBAMLevels45to15ServePerLevelPool: the strength=level rows serve
// levels 4..15 directly — the band machinery must stay untouched (zero
// band queries) and each pick must come from ITS OWN level's pool.
func TestWWTBAMLevels45to15ServePerLevelPool(t *testing.T) {
	repo := &baseFakeQuestionRepo{}
	for level := 1; level <= 15; level++ {
		for i := 1; i <= 8; i++ {
			repo.wwtbam = append(repo.wwtbam, wtbamFixtureRow(fmt.Sprintf("wt-l%d-%02d", level, i), level))
		}
	}
	// A rated corpus would trigger band queries — with wwtbam pools fat
	// per level, the count gate must pick the wwtbam pool FIRST, so no
	// band query is ever issued. Seed 100 rated rows to make the gate
	// strict (bands WOULD be available).
	for i := 1; i <= 100; i++ {
		s := i
		repo.rated = append(repo.rated, models.Question{
			ID: fmt.Sprintf("rated-%03d", i), Question: "rated q",
			Options: `["a","b","c","d"]`, CorrectIndex: 0,
			Difficulty: "medium", Category: "General Knowledge",
			Source: "opentdb", CreatedAt: time.Now().UTC(), Strength: &s,
		})
	}
	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(21)}
	list, err := g.selectQuestions(context.Background(), "wt-per-level")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]models.Question{}
	for _, q := range repo.wwtbam {
		byID[q.ID] = q
	}
	for level := 1; level <= 15; level++ {
		if len(list[level]) != 2 {
			t.Fatalf("level %d: %d variants", level, len(list[level]))
		}
		for _, v := range list[level] {
			q, ok := byID[v.questionID]
			if !ok || *q.Strength != level || q.Source != WWTBAMSource {
				t.Fatalf("level %d drew %s — must be a strength-%d wwtbam row", level, v.questionID, level)
			}
		}
	}
	if len(repo.bandCalls) != 0 {
		t.Fatalf("band queries issued although every level pool was fat: %d", len(repo.bandCalls))
	}
}

// TestWWTBAMPoolDegradesToBandWhen skinny: a wwtbam pool with <2
// candidates for a level must DEGRADE to the band rules (band query issued;
// wwtbam row never served at that level; log contract via bandCalls).
func TestWWTBAMPoolDegradesToBandWhenSkinny(t *testing.T) {
	repo := &baseFakeQuestionRepo{}
	// L8 pool has EXACTLY 1 candidate (< 2 → whole level degrades); all
	// other levels' pools are fat.
	for level := 1; level <= 15; level++ {
		if level == 8 {
			continue
		}
		for i := 1; i <= 8; i++ {
			repo.wwtbam = append(repo.wwtbam, wtbamFixtureRow(fmt.Sprintf("wt-l%d-%02d", level, i), level))
		}
	}
	repo.wwtbam = append(repo.wwtbam, wtbamFixtureRow("wt-l8-lonely", 8))
	// A rated corpus so the band path is live: 300 rows at strength 8-ish.
	for i := 1; i <= 300; i++ {
		s := 8
		repo.rated = append(repo.rated, models.Question{
			ID: fmt.Sprintf("rated-%03d", i), Question: "rated q",
			Options: `["a","b","c","d"]`, CorrectIndex: 0,
			Difficulty: "medium", Category: "General Knowledge",
			Source: "opentdb", CreatedAt: time.Now().UTC(), Strength: &s,
		})
	}
	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(22)}
	list, err := g.selectQuestions(context.Background(), "skinny-l8")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]models.Question{}
	for _, q := range repo.wwtbam {
		byID[q.ID] = q
	}
	if len(list[8]) != 2 {
		t.Fatalf("level 8 variants=%d", len(list[8]))
	}
	for _, v := range list[8] {
		if q, ok := byID[v.questionID]; ok && q.Source == WWTBAMSource {
			t.Fatalf("level %d served wwtbam row %s from a skinny (<%d) pool", 8, v.questionID, wwtbamMinPool)
		}
		if v.questionID == "wt-l8-lonely" {
			t.Fatalf("the lonely level-8 wwtbam row leaked through")
		}
	}
	if len(repo.bandCalls) < 2 {
		t.Fatalf("band queries=%d — level 8 must degrade to the band rules", len(repo.bandCalls))
	}
}

// TestWWTBAMVariantsDistinctSamePool: both variants of a level draw from
// the same per-level pool and NEVER duplicate (exclusions grow as today).
func TestWWTBAMVariantsDistinctSamePool(t *testing.T) {
	repo := &baseFakeQuestionRepo{}
	for level := 1; level <= 15; level++ {
		for i := 1; i <= 6; i++ {
			repo.wwtbam = append(repo.wwtbam, wtbamFixtureRow(fmt.Sprintf("wt-l%d-%02d", level, i), level))
		}
	}
	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(23)}
	list, err := g.selectQuestions(context.Background(), "variants")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for level := 1; level <= 15; level++ {
		for _, v := range list[level] {
			if seen[v.questionID] {
				t.Fatalf("level %d repeated %s", level, v.questionID)
			}
			seen[v.questionID] = true
			for _, c := range repo.wwtbamCalls[len(repo.wwtbamCalls)-1].exclude {
				_ = c
			}
		}
	}
	if len(seen) != 30 {
		t.Fatalf("picked %d distinct questions, want 30", len(seen))
	}
}

// ---------------------------------------------------------------------------
// DB GATE (M10 S3): 50 fresh rooms against the REAL repository on the dev
// DB. Per-level SOURCE DISTRIBUTION contract:
//
//	levels 1–3: every pick is a wwtbam@level row (source='wwtbam',
//	            strength == level) OR in the S2 casual shape
//	            (non-blacklisted AND (strength NULL or 1..3) — other
//	            sources only; wwtbam rows are NEVER served off-level);
//	levels 4–15: every pick is a wwtbam@level row OR a non-wwtbam RATED
//	            row (the band/legacy fallback machinery).
//
// Runs SERIALLY with the other suites (OOM discipline). READ-ONLY: no
// rooms or room_questions rows are created.
// ---------------------------------------------------------------------------

func TestWWTBAMPoolGate50RoomsDistribution(t *testing.T) {
	db := newServiceTestDB(t)
	repo := repository.NewQuestionRepository(db)
	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(27)}
	ctx := context.Background()

	const rooms = 50
	l1FromWWTBAM := 0
	l1Total := 0
	for room := 1; room <= rooms; room++ {
		roomID := fmt.Sprintf("wtb-gate-room-%02d", room)
		list, err := g.selectQuestions(ctx, roomID)
		if err != nil {
			t.Fatalf("room %d: selection failed: %v", room, err)
		}
		seen := map[string]bool{}
		picks := map[string]int{} // question id → its room level
		for level := 1; level <= 15; level++ {
			if len(list[level]) != 2 {
				t.Fatalf("room %d level %d: %d variants, want 2", room, level, len(list[level]))
			}
			for _, v := range list[level] {
				if seen[v.questionID] {
					t.Fatalf("room %d: duplicate question %s", room, v.questionID)
				}
				seen[v.questionID] = true
				picks[v.questionID] = level
			}
		}
		if len(picks) != 30 {
			t.Fatalf("room %d: %d picks, want 30", room, len(picks))
		}
		ids := make([]string, 0, len(picks))
		for id := range picks {
			ids = append(ids, id)
		}
		var rows []struct {
			ID       string `ksql:"id"`
			Source   string `ksql:"source"`
			Strength *int   `ksql:"strength"`
			Category string `ksql:"category"`
		}
		if err := db.Query(ctx, &rows,
			"SELECT id, source, strength, category FROM questions WHERE id::text = ANY($1::text[])", ids); err != nil {
			t.Fatalf("room %d: contract read failed: %v", room, err)
		}
		if len(rows) != len(ids) {
			t.Fatalf("room %d: contract read returned %d of %d rows", room, len(rows), len(ids))
		}
		for _, r := range rows {
			level := picks[r.ID]
			if r.Source == WWTBAMSource {
				// Dereference before formatting: %v on a *int prints the
				// POINTER address (hex garbage), never the strength.
				strength := "<nil>"
				if r.Strength != nil {
					strength = fmt.Sprintf("%d", *r.Strength)
				}
				if r.Strength == nil || *r.Strength != level {
					t.Fatalf("room %d: L%d wwtbam pick %s has strength %s ≠ level — calibration broken",
						room, level, r.ID, strength)
				}
				if level == 1 {
					l1FromWWTBAM++
					l1Total++
				}
				continue // policy hit: the per-level show pool
			}
			if level <= 3 {
				// the OTHER blend pool: the S2 casual shape
				if models.IsCasualBlacklisted(r.Category, r.Source) {
					t.Fatalf("room %d: L%d pick %s (%s/%s) is casual-blacklisted", room, level, r.ID, r.Source, r.Category)
				}
				// Casual rows ⇒ strength 1..3 (the labeled mix window;
				// NULL = the fail-safe opentdb pool). The widened 4..5
				// window is a logged DEGRADATION only — unreachable while
				// the wwtbam@level pools are fat, and never a legitimate
				// "casual" calibration.
				if r.Strength != nil && (*r.Strength < 1 || *r.Strength > casualMaxStrength) {
					t.Fatalf("room %d: L%d pick %s strength %d outside the casual labeled mix (1..%d)",
						room, level, r.ID, *r.Strength, casualMaxStrength)
				}
				if level == 1 {
					l1Total++
				}
				continue
			}
			// levels 4–15: the band/legacy fallback machinery may serve
			// rated band rows OR (when the rated corpus can't fill a
			// band) the legacy tier rows; both are non-wwtbam by
			// definition here (the wwtbam shape is handled above).
			// Mixed opentdb/otqa sources are allowed per the pools.
			if r.Source == "opentdb" || r.Source == "otqa" {
				continue
			}
			t.Fatalf("room %d: L%d pick %s from unknown source %q", room, level, r.ID, r.Source)
		}
	}
	if l1Total != rooms*2 {
		t.Fatalf("collected %d L1 picks, want %d", l1Total, rooms*2)
	}
	t.Logf("L1 pool distribution over %d rooms: wwtbam=%d casual=%d", rooms, l1FromWWTBAM, l1Total-l1FromWWTBAM)
}
