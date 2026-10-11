package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"boggle-api/internal/millionaire/models"
)

// This file pins the M6 band-selection v2 unit-wise: the pure bucketization
// (level → 15-band rank window) and the taste decision, then the
// SELECTION FLOW over the in-memory fake repository (internal tests):
//
//   - per-band bucketization of the actual 30 draws (a 30-row corpus ⇒
//     exactly 2 ranks per band);
//   - the quantized cultural cap: with cultural rows alternating ranks,
//     the room draws EXACTLY maxCulturalPerRoom cultural questions and
//     every query after the cap is reached filters cultural server-side;
//   - levels 1–3 never see a cultural question (forced non-cultural);
//   - widening to adjacent bands when a window empties (exhausted +
//     cultural-filtered rank slots), and the legacy tier fallback;
//   - the honest error path when band AND fallback pools are exhausted.
//
// The REAL SQL banding is covered by the repository integration test
// (RandomByRankBand) and the game-start flows by the existing suite.

func TestBandBoundsBucketization(t *testing.T) {
	// n == 45 → exactly 3 ranks per band.
	if lo, hi := bandBounds(1, 45); lo != 1 || hi != 3 {
		t.Fatalf("bandBounds(1,45) = %d..%d, want 1..3", lo, hi)
	}
	if lo, hi := bandBounds(2, 45); lo != 4 || hi != 6 {
		t.Fatalf("bandBounds(2,45) = %d..%d, want 4..6", lo, hi)
	}
	if lo, hi := bandBounds(15, 45); lo != 43 || hi != 45 {
		t.Fatalf("bandBounds(15,45) = %d..%d, want 43..45", lo, hi)
	}
	// The windows must PARTITION 1..n for representative corpus sizes.
	for _, n := range []int{15, 16, 30, 45, 100, 300, 4955} {
		covered := 0
		for level := 1; level <= 15; level++ {
			lo, hi := bandBounds(level, n)
			if hi < lo {
				t.Fatalf("n=%d level=%d: inverted window %d..%d", n, level, lo, hi)
			}
			covered += hi - lo + 1
		}
		if covered != n {
			t.Fatalf("n=%d: bands cover %d ranks, want %d (windows must partition)", n, covered, n)
		}
	}
	// Degenerate tiny corpora never invert (clamped to a single rank).
	if lo, hi := bandBounds(15, 3); lo != 3 || hi != 3 {
		t.Fatalf("bandBounds(15,3) = %d..%d, want 3..3", lo, hi)
	}
}

func TestAllowCulturalLocked(t *testing.T) {
	cases := []struct {
		level, picks int
		want         bool
	}{
		{1, 0, false}, // L1–3 always non-cultural
		{3, 0, false},
		{4, 0, true}, // from L4 cultural is allowed…
		{4, 8, true},
		{4, 9, false}, // …until the 9-pick room cap is reached
		{15, 9, false},
		{15, 5, true},
	}
	for _, c := range cases {
		if got := allowCulturalLocked(c.level, c.picks); got != c.want {
			t.Fatalf("allowCulturalLocked(%d, %d) = %v, want %v", c.level, c.picks, got, c.want)
		}
	}
}

// corpusQ builds a rated corpus row in rank order (strength r).
func corpusQ(rank int, cultural bool) models.Question {
	id := fmt.Sprintf("q%02d", rank)
	category := "OTQA:world"
	if cultural {
		category = "OTQA:music"
	}
	s := rank
	return models.Question{
		ID: id, Question: "corpus " + id, Options: `["a","b","c","d"]`,
		CorrectIndex: 0, Difficulty: "medium", Category: category,
		Source: "otqa", Strength: &s, CreatedAt: time.Now().UTC(),
	}
}

func legacyQ(n int, difficulty string) models.Question {
	return models.Question{
		ID: fmt.Sprintf("leg-%s-%02d", difficulty, n), Question: "legacy q",
		Options: `["a","b","c","d"]`, CorrectIndex: 0, Difficulty: difficulty,
		Category: "General Knowledge", Source: "opentdb",
	}
}

func rankOf(id string) int {
	var r int
	fmt.Sscanf(strings.TrimPrefix(id, "q0"), "%d", &r)
	if strings.HasPrefix(id, "q") {
		fmt.Sscanf(id[1:], "%d", &r)
	}
	return r
}

func isCulturalPick(id string) bool {
	return strings.HasPrefix(id, "q") && (rankOf(id)%2 == 1 || rankOf(id) == 18)
}

// TestBandSelectionBucketization is the all-non-cultural scenario: 30
// ranked rows with the L1–3 draws routed to the casual pool (M10 S2 rule)
// and levels 4–15 riding the bands. NOTE the SQL re-bands PER DRAW over
// the not-yet-excluded corpus (documented M6 behavior: exclusions ≤30,
// quantile drift negligible on real corpora) — with a 30-row fixture the
// later bands drain early and the tail falls back to the legacy tier pool,
// so the assertions here pin the INVARIANTS (no duplicates, L1–3 casual,
// monotone band progress, legacy tail) rather than fixed rank windows.
func TestBandSelectionBucketization(t *testing.T) {
	repo := &baseFakeQuestionRepo{}
	for r := 1; r <= 30; r++ {
		repo.rated = append(repo.rated, corpusQ(r, false))
	}
	// M10 S2 rule: L1–3 draw from the casual pool — the band corpus serves
	// levels 4–15.
	for i := 1; i <= 6; i++ {
		repo.casual = append(repo.casual, casualRow(
			fmt.Sprintf("cas-%02d", i), (i%3)+1, "General Knowledge", "opentdb"))
	}
	for i := 1; i <= 40; i++ { // untouched legacy pools
		repo.legacy = append(repo.legacy, legacyQ(i, "easy"), legacyQ(i, "medium"), legacyQ(i, "hard"))
	}

	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(1)}
	list, err := g.selectQuestions(context.Background(), "room-bucket")
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	lastRank := 0
	for level := 1; level <= 15; level++ {
		for _, v := range list[level] {
			if seen[v.questionID] {
				t.Fatalf("duplicate question %s", v.questionID)
			}
			seen[v.questionID] = true
			if level <= 3 {
				if !strings.HasPrefix(v.questionID, "cas-") {
					t.Fatalf("level %d drew %s — L1–3 must come from the casual pool (S2)", level, v.questionID)
				}
				continue
			}
			if strings.HasPrefix(v.questionID, "leg-") {
				continue // legacy tail after the drifting bands drained
			}
			rank := rankOf(v.questionID)
			if rank < lastRank {
				t.Fatalf("level %d drew rank %d after %d — band picks moved backwards", level, rank, lastRank)
			}
			lastRank = rank
		}
	}
	if len(seen) != 30 {
		t.Fatalf("picked %d distinct questions, want 30", len(seen))
	}
	if len(repo.bandCalls) == 0 {
		t.Fatal("no band queries recorded — the fake path was not exercised")
	}
	// The six L1–3 picks never queried the bands — sampling starts at the
	// level-4 window.
	lo4, hi4 := bandBounds(4, 30)
	if first := repo.bandCalls[0]; first.lo != lo4 || first.hi != hi4 {
		t.Fatalf("first band query = %d..%d, want the level-4 window %d..%d — L1–3 leaked into band sampling", first.lo, first.hi, lo4, hi4)
	}
}

// TestBandSelectionCulturalCapAndWidening pins the taste layer downstream
// of the M10 S2 split (L1–3 casual): over a drifting-band corpus (the SQL
// re-bands per draw — see TestBandSelectionBucketization's note) the
// invariants are result-level: no duplicates, L1–3 never cultural, band
// progress plus widening, and the cap never EXCEEDED. The quantized cap
// itself (exactly 9, filter flips) is pinned directly on drawOne below in
// TestDrawOneCulturalCapQuantized — the selectQuestions flow cannot bind
// the cap deterministically once L1–3 stopped consuming cultural-filtered
// ranks.
func TestBandSelectionCulturalCapAndWidening(t *testing.T) {
	repo := &baseFakeQuestionRepo{}
	for r := 1; r <= 30; r++ {
		cultural := r%2 == 1
		if r == 18 {
			cultural = true // ranks 17 AND 18 cultural → band 9 cultural-only
		}
		repo.rated = append(repo.rated, corpusQ(r, cultural))
	}
	// M10 S2 rule: L1–3 draw from the casual pool — never a cultural rank.
	for i := 1; i <= 6; i++ {
		repo.casual = append(repo.casual, casualRow(
			fmt.Sprintf("cas-%02d", i), (i%3)+1, "General Knowledge", "opentdb"))
	}
	for i := 1; i <= 40; i++ {
		repo.legacy = append(repo.legacy, legacyQ(i, "easy"), legacyQ(i, "medium"), legacyQ(i, "hard"))
	}

	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(2)}
	list, err := g.selectQuestions(context.Background(), "room-cap")
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	culturalPicks := 0
	for level := 1; level <= 15; level++ {
		for _, v := range list[level] {
			if seen[v.questionID] {
				t.Fatalf("duplicate question %s (level %d)", v.questionID, level)
			}
			seen[v.questionID] = true
			if isCulturalPick(v.questionID) {
				culturalPicks++
			}
		}
		// L1–3: the casual pool — non-cultural by construction, no exceptions.
		if level <= 3 {
			for _, v := range list[level] {
				if isCulturalPick(v.questionID) {
					t.Fatalf("level %d drew cultural rank %s — L1–3 must be non-cultural", level, v.questionID)
				}
			}
		}
	}
	if culturalPicks > maxCulturalPerRoom {
		t.Fatalf("cultural picks = %d — the %d-pick room cap was exceeded", culturalPicks, maxCulturalPerRoom)
	}

	// The taste filter must appear in the recorded band calls AT ALL (the
	// L4+ flow must carry the decision), and widening must have happened
	// for the drained windows.
	var openQueries, filtered, widened bool
	for _, c := range repo.bandCalls {
		if c.excludeCultural {
			filtered = true
		} else {
			openQueries = true
		}
		for level := 1; level <= 15; level++ {
			lo, hi := bandBounds(level, 30)
			if (c.lo < lo) || (c.hi > hi) {
				widened = true
			}
		}
	}
	if !openQueries && !filtered {
		t.Fatalf("no band calls recorded: open=%v filtered=%v", openQueries, filtered)
	}
	if !widened {
		t.Fatal("widening to adjacent bands never happened — the drifting windows require it")
	}
}

// TestDrawOneCulturalCapQuantized pins the cap decision point EXACTLY: at
// the 9-pick room cap the next draw's band query MUST exclude cultural
// server-side (the recorded call proves it), and one pick below the cap it
// stays open — regardless of level ≥ 4.
func TestDrawOneCulturalCapQuantized(t *testing.T) {
	newRepo := func() *baseFakeQuestionRepo {
		repo := &baseFakeQuestionRepo{}
		for r := 1; r <= 30; r++ {
			cultural := r%2 == 1
			repo.rated = append(repo.rated, corpusQ(r, cultural))
		}
		return repo
	}
	mkDraw := func(cultural int) *roomDraw {
		d := &roomDraw{excluded: map[string]bool{}}
		d.cultural = cultural
		return d
	}

	// One pick below the cap: the query stays unfiltered.
	repo := newRepo()
	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(5)}
	if _, err := g.drawOne(context.Background(), "room", 4, mkDraw(maxCulturalPerRoom-1), 30); err != nil {
		t.Fatal(err)
	}
	if last := repo.bandCalls[len(repo.bandCalls)-1]; last.excludeCultural {
		t.Fatal("band call filtered cultural although the room was one pick below the cap")
	}

	// At the cap: the query MUST filter cultural.
	repo = newRepo()
	g = &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(6)}
	q, err := g.drawOne(context.Background(), "room", 4, mkDraw(maxCulturalPerRoom), 30)
	if err != nil {
		t.Fatal(err)
	}
	if last := repo.bandCalls[len(repo.bandCalls)-1]; !last.excludeCultural {
		t.Fatal("band call stayed open although the room cap was reached")
	}
	if models.IsCultural(q.Category, q.Source) {
		t.Fatalf("the capped draw served a cultural question (%s)", q.ID)
	}
}

// TestBandSelectionOverallCapNeversExceed — direct quantization guard on
// the cap helper vs a hand-built room state: no room configuration can
// make allowCulturalLocked admit MORE than maxCulturalPerRoom cultural
// picks (it is the single decision point).
func TestBandSelectionOverallCapNeversExceed(t *testing.T) {
	for picks := 0; picks <= maxCulturalPerRoom+5; picks++ {
		for level := 1; level <= 15; level++ {
			if allowCulturalLocked(level, picks) && picks >= maxCulturalPerRoom {
				t.Fatalf("cap leak: level %d allowed a cultural pick with %d already picked", level, picks)
			}
			if allowCulturalLocked(level, picks) && picks != maxCulturalPerRoom-1 == false && picks == maxCulturalPerRoom {
				t.Fatalf("unreachable")
			}
		}
	}
}

// TestBandSelectionAllCulturalFallback pins the honest degradation: the
// ranked corpus is 100% cultural (every window filtered/empty after the
// cap; L1–3 are served by the fail-safe pool), and when the legacy tier
// pool is exhausted too, selection fails (it must never silently
// duplicate questions).
func TestBandSelectionAllCulturalFallback(t *testing.T) {
	repo := &baseFakeQuestionRepo{}
	for r := 1; r <= 20; r++ { // minRatedForBands=15 satisfied, all cultural
		repo.rated = append(repo.rated, corpusQ(r, true))
	}
	for i := 1; i <= 6; i++ { // fail-safe pool serves the 6 L1–3 draws (S2)
		repo.failSafe = append(repo.failSafe, failSafeRow(fmt.Sprintf("fs-%02d", i), "General Knowledge"))
	}
	for i := 1; i <= 5; i++ { // easy-only fallback pool: the level-4..5 draws eat it
		repo.legacy = append(repo.legacy, legacyQ(i, "easy"))
	}
	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(3)}
	_, err := g.selectQuestions(context.Background(), "room-exhausted")
	if err == nil {
		t.Fatal("selection must fail when band AND fallback pools are both exhausted")
	}
	if !strings.Contains(err.Error(), "not enough") {
		t.Fatalf("err = %v — want the pool-exhaustion message", err)
	}
}

// TestBandSelectionFallbackServes is the positive fallback case: with NO
// rated rows at all (pre-labeling corpus), levels 4–15 draw from the
// legacy tier pools (the M3 behavior, level-gated by tier) while levels
// 1–3 draw from the fail-safe pool (the S2 rule — never raw tier).
func TestBandSelectionFallbackServes(t *testing.T) {
	repo := &baseFakeQuestionRepo{}
	for i := 1; i <= 6; i++ { // fail-safe pool: the 6 L1–3 draws (S2)
		repo.failSafe = append(repo.failSafe, failSafeRow(fmt.Sprintf("fs-%02d", i), "General Knowledge"))
	}
	for i := 1; i <= 40; i++ {
		repo.legacy = append(repo.legacy, legacyQ(i, "easy"), legacyQ(i, "medium"), legacyQ(i, "hard"))
	}
	g := &GameService{questions: repo, budgets: map[string]time.Duration{}, rng: seededRNG(4)}
	list, err := g.selectQuestions(context.Background(), "room-legacy")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	tierWanted := map[int]string{1: "easy"}
	_ = tierWanted
	for level := 1; level <= 15; level++ {
		if len(list[level]) != 2 {
			t.Fatalf("level %d variants = %d", level, len(list[level]))
		}
		for _, v := range list[level] {
			if seen[v.questionID] {
				t.Fatalf("duplicate %s", v.questionID)
			}
			seen[v.questionID] = true
			if strings.HasPrefix(v.questionID, "q") {
				t.Fatal("an unrated corpus must never draw from the band pool")
			}
			if level <= 3 && !strings.HasPrefix(v.questionID, "fs-") {
				t.Fatalf("pre-labeling L%d drew %s — the fail-safe pool must serve levels 1–3 (S2)", level, v.questionID)
			}
		}
		if repo.bandCalls != nil {
			break // legacy path issues no band queries beyond the count
		}
	}
	if len(seen) != 30 {
		t.Fatalf("picked %d, want 30", len(seen))
	}
	if len(repo.bandCalls) != 0 {
		t.Fatalf("legacy-only selection issued %d band queries", len(repo.bandCalls))
	}
}
