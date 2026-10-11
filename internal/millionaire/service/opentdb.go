package service

import (
	"context"
	"encoding/csv"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"

	"boggle-api/internal/millionaire/models"
	"boggle-api/internal/millionaire/repository"

	"github.com/google/uuid"
)

// This file is the OpenTDB importer (M10 casual-pool sibling of the WWTBAM
// importer). It reads the vendored, decoded slice of the OpenTDB corpus
// (data/opentdb.csv) plus its curated difficulty ratings
// (data/opentdb_strengths.csv), and inserts every row with a REAL strength so
// the levels 1–3 50/50 blend (drawCasualBlend) has a non-wwtbam pool to draw
// from — without it the casual side of the coin is always empty and the blend
// silently runs at 100/0.
//
// Source and licence
//
//	OpenTDB — https://opentdb.com — is licensed CC BY-SA 4.0. The per-category
//	CSVs come from the QuartzWarrior/OTDB-Source mirror; data/opentdb_build.py
//	decodes them into data/opentdb.csv (see README "OpenTDB corpus").
//
// SHAPE ADAPTER (the only adaptation this file makes)
//	Upstream rows carry a QUESTION and an ANSWER but no options, while
//	millionaire_questions.options requires exactly four strings. The three
//	distractors are therefore sampled from the other answers of the SAME
//	category, rejecting near-duplicates of the correct answer (case-folded
//	equality, or one answer containing the other, which would make two
//	options defensible). Deterministic per run (seeded rng) and idempotent
//	across runs because the dedupe is by question text: a second run finds
//	every row present and imports 0.
//
// CATEGORY FILTER
//	Only the categories models.CasualBlacklist* allows are vendored: no
//	`Entertainment:*`, no `Celebrities`, no `Science: Gadgets`, no
//	`Mythology`. The filter lives in data/opentdb_build.py so the shipped CSV
//	is already compliant; the importer does not need to re-apply it.

const (
	// OpenTDBSource is stored questions.source for this corpus. It is the
	// source the casual pool keys on (repository.RandomByCasual).
	OpenTDBSource = "opentdb"
)

// OpenTDBRow is one data/opentdb.csv line.
type OpenTDBRow struct {
	Question string
	Answer   string
	Category string
}

// OpenTDBStats is the run summary (logged and returned by cmd/import).
type OpenTDBStats struct {
	Imported   int
	Duplicates int
	Skipped    int // unrated, empty, or fewer than 3 usable distractors
	Unrated    int // present in data/opentdb.csv but absent from the ratings file
	Total      int // count of source='opentdb' rows after the run
}

// OpenTDBImporter imports the vendored OpenTDB CSV idempotently: re-runs
// re-derive an identical insert set and find every row already present
// (dedupe is cross-corpus by normalized text), so a second run imports 0.
type OpenTDBImporter struct {
	repo repository.QuestionRepository
	file string
	// strengths is the curated "question,strength" CSV. Optional: without it
	// every row imports UNRATED, which is invisible to the casual pool — the
	// file is what makes this corpus count at levels 1–3.
	strengths string
}

func NewOpenTDBImporter(repo repository.QuestionRepository, file, strengths string) *OpenTDBImporter {
	return &OpenTDBImporter{
		repo:      repo,
		file:      file,
		strengths: strengths,
	}
}

func (i *OpenTDBImporter) Run(ctx context.Context) (OpenTDBStats, error) {
	ratings, err := loadOpenTDBStrengths(i.strengths)
	if err != nil {
		return OpenTDBStats{}, err
	}

	rows, err := readOpenTDBRows(i.file)
	if err != nil {
		return OpenTDBStats{}, err
	}

	existing, err := i.repo.ListAll(ctx)
	if err != nil {
		return OpenTDBStats{}, fmt.Errorf("loading existing questions for dedupe: %w", err)
	}
	seen := make(map[string]bool, len(existing))
	for _, q := range existing {
		seen[normalizeQuestionText(q.Question)] = true
	}
	log.Printf("opentdb: %d existing questions loaded for dedupe, %d ratings loaded", len(seen), len(ratings))

	// Distractor pools, keyed by category.
	pools := make(map[string][]string)
	for _, r := range rows {
		pools[r.Category] = append(pools[r.Category], r.Answer)
	}

	var st OpenTDBStats
	for _, r := range rows {
		strength, ok := ratings[normalizeQuestionText(r.Question)]
		if !ok {
			st.Unrated++
			continue
		}
		if normalizeQuestionText(r.Question) == "" || normalizeAnswer(r.Answer) == "" {
			st.Skipped++
			continue
		}
		norm := normalizeQuestionText(r.Question)
		if seen[norm] {
			st.Duplicates++
			continue
		}
		options, correct := i.buildOptions(r, pools[r.Category])
		if len(options) != 4 {
			st.Skipped++
			log.Printf("opentdb: skipped (fewer than 3 distractors in %q): %s", r.Category, r.Question)
			continue
		}
		id, err := uuid.NewV7()
		if err != nil {
			return st, fmt.Errorf("generating question id: %w", err)
		}
		// Every field is set explicitly because ksql INSERT sends them all.
		str := strength
		q := models.Question{
			ID:           id.String(),
			Question:     r.Question,
			Options:      marshalOptions(options),
			CorrectIndex: correct,
			Difficulty:   opentdbDifficultyTier(strength),
			Category:     r.Category,
			Source:       OpenTDBSource,
			CreatedAt:    time.Now().UTC(),
			Strength:     &str,
		}
		if err := i.repo.InsertQuestion(ctx, &q); err != nil {
			return st, fmt.Errorf("inserting question (%s): %w", r.Question, err)
		}
		seen[norm] = true
		st.Imported++
	}

	total, err := i.repo.CountBySource(ctx, OpenTDBSource)
	if err != nil {
		return st, fmt.Errorf("counting stored opentdb rows: %w", err)
	}
	st.Total = total
	log.Printf("opentdb: imported=%d duplicates=%d skipped=%d unrated=%d total=%d",
		st.Imported, st.Duplicates, st.Skipped, st.Unrated, st.Total)
	return st, nil
}

// buildOptions assembles the four options and the index of the correct one.
// The correct answer is placed FIRST and then the slice is rotated by a random
// amount, so its position is neither always 0 nor correlated with the sample.
//
// The rng is derived from the QUESTION TEXT (opentdbQuestionRNG), not from a
// shared stream: a row's option set must be a pure function of the corpus, or
// a second run over the same file would draw a different sample, skip a row
// it inserted last time, and break the idempotency this importer promises.
func (i *OpenTDBImporter) buildOptions(r OpenTDBRow, pool []string) ([]string, int) {
	rng := opentdbQuestionRNG(r.Question)
	correct := strings.TrimSpace(r.Answer)
	chosen := make([]string, 0, 4)
	seen := map[string]bool{normalizeAnswer(correct): true}

	for attempt := 0; len(chosen) < 3 && attempt < len(pool)*3; attempt++ {
		cand := pool[rng.Intn(len(pool))]
		key := normalizeAnswer(cand)
		if key == "" || seen[key] || key == normalizeAnswer(correct) {
			continue
		}
		// Reject containment either way: "Paris" against "Paris, France"
		// would leave two defensible options.
		if strings.Contains(key, normalizeAnswer(correct)) || strings.Contains(normalizeAnswer(correct), key) {
			continue
		}
		seen[key] = true
		chosen = append(chosen, strings.TrimSpace(cand))
	}
	if len(chosen) < 3 {
		return nil, -1
	}

	options := append([]string{correct}, chosen...)
	// options[0] is the answer; rotating by `shift` moves it to index shift.
	shift := rng.Intn(4)
	rotated := make([]string, 4)
	for k := 0; k < 4; k++ {
		rotated[(k+shift)%4] = options[k]
	}
	return rotated, shift
}

// opentdbQuestionRNG returns the deterministic, question-keyed sampler the
// distractor draw uses (see buildOptions).
func opentdbQuestionRNG(question string) *rand.Rand {
	h := fnv.New64a()
	_, _ = h.Write([]byte(question))
	return rand.New(rand.NewSource(int64(h.Sum64())))
}

// opentdbDifficultyTier maps the curated strength to the legacy difficulty
// column (display-only; strength is the governing axis).
func opentdbDifficultyTier(strength int) string {
	switch {
	case strength <= 5:
		return "easy"
	case strength <= 10:
		return "medium"
	default:
		return "hard"
	}
}

// normalizeAnswer is the option-identity comparison (case/whitespace folded).
func normalizeAnswer(a string) string {
	return strings.ToLower(strings.Join(strings.Fields(a), " "))
}

// readOpenTDBRows parses the vendored "question,answer,category" CSV.
func readOpenTDBRows(path string) ([]OpenTDBRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening opentdb CSV: %w", err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = 3
	r.LazyQuotes = true
	var out []OpenTDBRow
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading opentdb CSV %s: %w", path, err)
		}
		if strings.EqualFold(strings.TrimSpace(rec[0]), "question") {
			continue // header
		}
		out = append(out, OpenTDBRow{
			Question: strings.TrimSpace(rec[0]),
			Answer:   strings.TrimSpace(rec[1]),
			Category: strings.TrimSpace(rec[2]),
		})
	}
	return out, nil
}

// loadOpenTDBStrengths parses the curated "question,strength" CSV into a
// normalized-text lookup. An empty path yields an empty map (everything then
// imports unrated).
func loadOpenTDBStrengths(path string) (map[string]int, error) {
	out := map[string]int{}
	if path == "" {
		return out, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening opentdb strengths CSV: %w", err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = 2
	r.LazyQuotes = true
	line := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading opentdb strengths CSV %s: %w", path, err)
		}
		line++
		if line == 1 && strings.EqualFold(strings.TrimSpace(rec[0]), "question") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(rec[1]))
		if err != nil || n < 1 || n > 15 {
			return nil, fmt.Errorf("invalid strength %q for %q", rec[1], rec[0])
		}
		out[normalizeQuestionText(rec[0])] = n
	}
	return out, nil
}

// String formats the stats for strconv-free logging of run stats.
func (s OpenTDBStats) String() string {
	return fmt.Sprintf("imported=%d duplicates=%d skipped=%d unrated=%d total=%d",
		s.Imported, s.Duplicates, s.Skipped, s.Unrated, s.Total)
}
