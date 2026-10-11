package repository

import (
	"context"
	"testing"

	"boggle-api/internal/millionaire/models"
)

// This file integration-tests the S2 casual-pool SQL against the dev DB on
// the hermetic temp questions clone (same discipline as band_test.go): trap
// rows (casual-blacklisted categories, out-of-window strengths, foreign
// sources/difficulties) must never surface while the eligible rows do.

// casualTestRows inserts trap + eligible rows into the temp clone and
// returns them for the assertions.
func casualTestRows() (good, traps []testQuestion) {
	// Eligible casual rows: rated 1..3, non-blacklisted.
	good = []testQuestion{
		{id: "11111111-1111-7111-a111-111111111111", strength: 2, category: "General Knowledge", source: "opentdb"},
		{id: "22222222-2222-7222-a222-222222222222", strength: 2, category: "OTQA:world", source: "otqa"},
	}
	// Every blacklist dimension as its own trap:
	traps = []testQuestion{
		{id: "33333333-3333-7333-a333-333333333333", strength: 1, category: "Entertainment: Video Games", source: "opentdb"},
		{id: "44444444-4444-7444-a444-444444444444", strength: 1, category: "Science: Gadgets", source: "opentdb"},
		{id: "55555555-5555-7555-a555-555555555555", strength: 1, category: "Celebrities", source: "opentdb"},
		{id: "66666666-6666-7666-a666-666666666666", strength: 1, category: "OTQA:animals", source: "otqa"},
		{id: "77777777-7777-7777-a777-777777777777", strength: 1, category: "OTQA:music", source: "otqa"},
		{id: "88888888-8888-7888-a888-888888888888", strength: 4, category: "General Knowledge", source: "opentdb"},
		{id: "99999999-9999-7999-a999-999999999999", strength: 9, category: "OTQA:general", source: "otqa"},
	}
	return good, traps
}

// TestRandomByCasualBlacklistAndWindow pins the casual query: draws with
// maxStrength=3 may only serve strength 1–3 non-blacklisted rows; traps
// never surface; the widened maxStrength=5 admits the strength-4 row but
// still holds the blacklist and the upper window.
func TestRandomByCasualBlacklistAndWindow(t *testing.T) {
	db := openLabelDB(t)
	repo := NewQuestionRepository(db)
	ctx := context.Background()

	good, traps := casualTestRows()
	// The pre-existing rated corpus may hold casual-eligible rows too —
	// relative claims (both MY eligible rows surface) hold only when it
	// was empty of casual-eligible material; rule/trap claims hold always.
	var preEligible int
	for _, q := range listRated(t, db) {
		if q.Strength != nil && *q.Strength <= 3 &&
			!models.IsCasualBlacklisted(q.Category, q.Source) {
			preEligible++
		}
	}
	insertBandQuestions(t, db, append(append([]testQuestion{}, good...), traps...))

	trapIDs := map[string]bool{}
	for _, tr := range traps {
		trapIDs[tr.id] = true
	}
	goodIDs := map[string]bool{}
	for _, g := range good {
		goodIDs[g.id] = true
	}

	seen := map[string]bool{}
	for attempt := 0; attempt < 12; attempt++ {
		picked, err := repo.RandomByCasual(ctx, 3, 2, nil)
		if err != nil {
			t.Fatalf("RandomByCasual(max=3): %v", err)
		}
		for _, q := range picked {
			if q.Strength == nil || *q.Strength < 1 || *q.Strength > 3 {
				t.Fatalf("casual throwaway: strength %v outside 1..3 — the window filter is broken", q.Strength)
			}
			if models.IsCasualBlacklisted(q.Category, q.Source) {
				t.Fatalf("casual served blacklisted %q (%s) — S2 broken", q.Category, q.Source)
			}
			if trapIDs[q.ID] {
				t.Fatalf("casual served trap row %s (%s)", q.ID, q.Category)
			}
			seen[q.ID] = true
		}
	}
	if preEligible == 0 {
		// Nothing but MY rows could have been served: both eligible rows
		// must have surfaced over 24 draws.
		for _, g := range good {
			if !seen[g.id] {
				t.Fatalf("eligible casual row %s (%s) never surfaced over the draws", g.id, g.category)
			}
		}
	} else {
		t.Logf("%d pre-existing casual-eligible rows — relative-surfacing claim skipped", preEligible)
	}

	// Exclusions: my two eligible rows excluded → empty pool within the
	// deterministic corpus (pre-existing corpus excluded via its own ids
	// is not attempted — the flag is the assertable bit).
	if preEligible == 0 {
		excl := []string{good[0].id, good[1].id}
		picked, err := repo.RandomByCasual(ctx, 3, 2, excl)
		if err != nil {
			t.Fatal(err)
		}
		if len(picked) != 0 {
			t.Fatalf("casual with all eligible rows excluded served %v", picked)
		}
	}

	// Widened window: strength-4 GK row may surface; strength-9 and traps
	// still never.
	for attempt := 0; attempt < 6; attempt++ {
		picked, err := repo.RandomByCasual(ctx, 5, 2, nil)
		if err != nil {
			t.Fatalf("RandomByCasual(max=5): %v", err)
		}
		for _, q := range picked {
			if q.Strength == nil || *q.Strength < 1 || *q.Strength > 5 {
				t.Fatalf("widened casual served strength %v outside 1..5", q.Strength)
			}
			if models.IsCasualBlacklisted(q.Category, q.Source) {
				t.Fatalf("widened casual served blacklisted %q (%s)", q.Category, q.Source)
			}
			for _, tr := range traps[:5] { // the blacklist traps (indices 5/6 are the window traps)
				if tr.id == q.ID {
					t.Fatalf("widened casual served blacklisted trap %s", tr.id)
				}
			}
		}
	}
}

// TestRandomByFailSafeCasualTraps pins the S2 last-resort pool: opentdb
// EASY rows from the fail-safe categories ONLY — an otqa row, a medium/hard
// tier, or an off-set category (Entertainment: Film) must never surface.
func TestRandomByFailSafeCasualTraps(t *testing.T) {
	db := openLabelDB(t)
	repo := NewQuestionRepository(db)
	ctx := context.Background()

	failSafe := map[string]bool{
		"General Knowledge": true, "Geography": true, "Sports": true,
		"History": true, "Food & Drink": true,
	}

	// Capture the pre-seeded (rated) corpus' matching rows so the
	// relative-surfacing claim can degrade gracefully.
	var preMatching int
	for _, q := range listRated(t, db) {
		if q.Difficulty == "easy" && q.Source == "opentdb" && failSafe[q.Category] {
			preMatching++
		}
	}

	rows := []testQuestion{
		{id: "aaaaaaa1-aaaa-7aaa-baaa-aaaaaaaaaaa1", strength: 5, category: "General Knowledge", source: "opentdb"},   // eligible
		{id: "aaaaaaa2-aaaa-7aaa-baaa-aaaaaaaaaaa2", strength: 5, category: "OTQA:general", source: "otqa"},           // wrong source
		{id: "aaaaaaa3-aaaa-7aaa-baaa-aaaaaaaaaaa3", strength: 5, category: "Entertainment: Film", source: "opentdb"}, // off-set category
		{id: "aaaaaaa4-aaaa-7aaa-baaa-aaaaaaaaaaa4", strength: 5, category: "Geography", source: "opentdb"},           // eligible
		{id: "aaaaaaa5-aaaa-7aaa-baaa-aaaaaaaaaaa5", strength: 5, category: "Sports", source: "opentdb"},              // tier trap (hard)
	}
	for i, row := range rows {
		q := row.question()
		switch i {
		case 1:
			q.Difficulty = "medium" // tier trap within a foreign source
		default:
			q.Difficulty = "easy"
		case 4:
			q.Difficulty = "hard" // tier trap INSIDE the fail-safe set
		}
		if err := repo.InsertQuestion(ctx, &q); err != nil {
			t.Fatalf("inserting %s: %v", q.ID, err)
		}
	}

	eligible := map[string]bool{rows[0].id: true, rows[3].id: true}
	trapIDs := map[string]bool{rows[1].id: true, rows[2].id: true, rows[4].id: true}
	seen := map[string]bool{}
	for attempt := 0; attempt < 12; attempt++ {
		picked, err := repo.RandomByFailSafeCasual(ctx,
			[]string{"General Knowledge", "Geography", "Sports", "History", "Food & Drink"}, 2, nil)
		if err != nil {
			t.Fatalf("RandomByFailSafeCasual: %v", err)
		}
		for _, q := range picked {
			if q.Difficulty != "easy" || q.Source != "opentdb" || !failSafe[q.Category] {
				t.Fatalf("fail-safe pool served %+v — outside the easy/opentdb/known-category contract", q)
			}
			if trapIDs[q.ID] {
				t.Fatalf("fail-safe pool served trap %s (%s %s)", q.ID, q.Source, q.Category)
			}
			seen[q.ID] = true
		}
	}
	if preMatching == 0 {
		for id := range eligible {
			if !seen[id] {
				t.Fatalf("eligible fail-safe row %s never surfaced", id)
			}
		}
	}

	// Exclusions: excluding both my eligible rows must never serve them.
	picked, err := repo.RandomByFailSafeCasual(ctx,
		[]string{"General Knowledge", "Geography", "Sports", "History", "Food & Drink"},
		5, []string{rows[0].id, rows[3].id})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range picked {
		if q.ID == rows[0].id || q.ID == rows[3].id {
			t.Fatalf("fail-safe pool served an explicitly excluded row %s", q.ID)
		}
	}
}
