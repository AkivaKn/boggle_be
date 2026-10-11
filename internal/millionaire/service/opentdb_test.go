package service

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"boggle-api/internal/millionaire/models"
	"boggle-api/internal/millionaire/repository"
)

// The OpenTDB adapter is the only importer that has to INVENT its distractor
// options (upstream rows carry question + answer only, while
// millionaire_questions.options requires exactly four). These tests pin the
// two ways that can go wrong: an option set that leaks the answer, and a
// corpus that does not fit the four-option schema.

// TestOpenTDBBuildOptionsFourUniqueNoAnswerLeak pins the core contract:
// exactly four options, the correct one present exactly once, no answer
// repeated, and no distractor that contains the answer or vice versa.
func TestOpenTDBBuildOptionsFourUniqueNoAnswerLeak(t *testing.T) {
	pool := []string{"Berlin", "Paris", "Paris, France", "paris", "Madrid", "Rome", "Lisbon"}
	for _, answer := range []string{"Paris", "Berlin", "Rome"} {
		imp := NewOpenTDBImporter(nil, "", "")
		options, correct := imp.buildOptions(OpenTDBRow{Answer: answer}, pool)
		if len(options) != 4 {
			t.Fatalf("answer %q: got %d options, want 4 (%v)", answer, len(options), options)
		}
		if correct < 0 || correct > 3 {
			t.Fatalf("answer %q: correct_index %d out of 0..3", answer, correct)
		}
		if options[correct] != answer {
			t.Fatalf("answer %q: options[%d] is %q, not the answer", answer, correct, options[correct])
		}
		seen := map[string]int{}
		for _, o := range options {
			seen[normalizeAnswer(o)]++
		}
		if seen[normalizeAnswer(answer)] != 1 {
			t.Fatalf("answer %q appears %d times in %v — the answer leaks", answer, seen[normalizeAnswer(answer)], options)
		}
		for opt := range seen {
			if seen[opt] > 1 {
				t.Fatalf("duplicate option %q in %v", opt, options)
			}
			if opt != normalizeAnswer(answer) &&
				(strings.Contains(opt, normalizeAnswer(answer)) || strings.Contains(normalizeAnswer(answer), opt)) {
				t.Fatalf("option %q is a substring match of the answer %q — two defensible options", opt, answer)
			}
		}
	}
}

// TestOpenTDBBuildOptionsInsufficientPool pins the honest failure: fewer than
// three usable distractors returns no options rather than padding a
// four-option question with filler.
func TestOpenTDBBuildOptionsInsufficientPool(t *testing.T) {
	imp := NewOpenTDBImporter(nil, "", "")
	options, correct := imp.buildOptions(OpenTDBRow{Answer: "Paris"}, []string{"Paris", "paris", "Paris, France", ""})
	if options != nil || correct != -1 {
		t.Fatalf("got options=%v correct=%d, want nil/-1", options, correct)
	}
}

// TestOpenTDBRunImportsRatedRowsOnly is the end-to-end shape test on a fake
// repo: rated rows import with their strength and a derived difficulty, and an
// UNRATED row is skipped (an unrated row would be invisible to the casual
// pool, which is the whole reason the ratings file exists).
func TestOpenTDBRunImportsRatedRowsOnly(t *testing.T) {
	dir := t.TempDir()
	corpus := filepath.Join(dir, "opentdb.csv")
	ratings := filepath.Join(dir, "strengths.csv")
	body := "question,answer,category\n" +
		"Capital of France?,Paris,Geography\n" +
		"Capital of Spain?,Madrid,Geography\n" +
		"Capital of Italy?,Rome,Geography\n" +
		"Capital of Peru?,Lima,Geography\n" +
		"Capital of Kenya?,Nairobi,Geography\n" +
		"Which planet is red?,Mars,Geography\n"
	if err := os.WriteFile(corpus, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	str := "question,strength\n" +
		"Capital of France?,2\n" +
		"Capital of Spain?,3\n" +
		"Capital of Italy?,4\n" +
		"Capital of Peru?,5\n" +
		"Capital of Kenya?,6\n"
	if err := os.WriteFile(ratings, []byte(str), 0o644); err != nil {
		t.Fatal(err)
	}

	repo := &fakeOTDBRepo{}
	st, err := NewOpenTDBImporter(repo, corpus, ratings).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Imported != 5 {
		t.Fatalf("imported %d, want 5 (the unrated row must be skipped)", st.Imported)
	}
	if st.Unrated != 1 {
		t.Fatalf("unrated %d, want 1", st.Unrated)
	}
	if st.Duplicates != 0 || st.Skipped != 0 {
		t.Fatalf("duplicates=%d skipped=%d, want 0/0", st.Duplicates, st.Skipped)
	}
	if len(repo.rows) != 5 {
		t.Fatalf("repo holds %d rows, want 5", len(repo.rows))
	}
	wantDifficulty := map[string]string{"2": "easy", "3": "easy", "4": "easy", "5": "easy", "6": "medium"}
	for _, q := range repo.rows {
		if q.Source != OpenTDBSource {
			t.Fatalf("row %q stored with source %q", q.Question, q.Source)
		}
		if q.Strength == nil {
			t.Fatalf("row %q imported UNRATED — invisible to the casual pool", q.Question)
		}
		if got, want := opentdbDifficultyTier(*q.Strength), wantDifficulty[strconv.Itoa(*q.Strength)]; got != want {
			t.Fatalf("row %q strength %d: difficulty %q, want %q", q.Question, *q.Strength, got, want)
		}
		if q.CorrectIndex < 0 || q.CorrectIndex > 3 {
			t.Fatalf("row %q correct_index %d out of range", q.Question, q.CorrectIndex)
		}
	}
}

// TestOpenTDBRunIsIdempotentOnSecondRun pins the dedupe: the second run over
// the same corpus imports 0.
func TestOpenTDBRunIsIdempotentOnSecondRun(t *testing.T) {
	dir := t.TempDir()
	corpus := filepath.Join(dir, "opentdb.csv")
	ratings := filepath.Join(dir, "strengths.csv")
	rows := ""
	str := "question,strength\n"
	for _, r := range [][2]string{
		{"Capital of France?", "Paris"}, {"Capital of Spain?", "Madrid"},
		{"Capital of Italy?", "Rome"}, {"Capital of Peru?", "Lima"},
	} {
		rows += r[0] + "," + r[1] + ",Geography\n"
		str += r[0] + ",3\n"
	}
	if err := os.WriteFile(corpus, []byte("question,answer,category\n"+rows), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ratings, []byte(str), 0o644); err != nil {
		t.Fatal(err)
	}

	repo := &fakeOTDBRepo{}
	imp := NewOpenTDBImporter(repo, corpus, ratings)
	if _, err := imp.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := imp.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Imported != 0 || st.Duplicates != 4 {
		t.Fatalf("second run imported=%d duplicates=%d, want 0/4", st.Imported, st.Duplicates)
	}
}

// fakeOTDBRepo is the minimal QuestionRepository surface OpenTDBImporter uses.
type fakeOTDBRepo struct {
	repository.QuestionRepository // embedded for the methods the importer never calls
	rows                          []models.Question
}

func (f *fakeOTDBRepo) ListAll(ctx context.Context) ([]models.Question, error) {
	return append([]models.Question(nil), f.rows...), nil
}

func (f *fakeOTDBRepo) InsertQuestion(ctx context.Context, q *models.Question) error {
	f.rows = append(f.rows, *q)
	return nil
}

func (f *fakeOTDBRepo) CountBySource(ctx context.Context, source string) (int, error) {
	n := 0
	for _, q := range f.rows {
		if q.Source == source {
			n++
		}
	}
	return n, nil
}
