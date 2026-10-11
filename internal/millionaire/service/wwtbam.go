package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"boggle-api/internal/millionaire/models"
	"boggle-api/internal/millionaire/repository"

	"github.com/google/uuid"
)

// This file is the M10 WWTBAM importer (S3): the community-edited
// "MillionaireDB — WWTBAM Question Database" corpus (the 2020 Microids PC
// game files, collected by the credited editors — see README) converted on
// the HOST by data/wwtbam_export.py into one JSONL row per question:
//
//	{"level":1,"question":"...","options":["A","B","C","D"],
//	 "correct":"d","src":"..."}    // letter lowercase; options in A–D order
//
// It inserts EVERY valid row (no sampling — the corpus is already the
// calibrated set), deduped against the whole existing corpus by the
// seeder normalization (cross-source, idempotent: a second run imports 0).
// Stored shape differs from OTQA deliberately:
//   - options are stored in the workbook's original A–D order and
//     correct_index = the LETTER position (0..3), not stored-correct-first;
//   - strength = the LEVEL (1..15) — the real show calibration, the v2
//     difficulty axis (SPEC §2), consumed by the per-level selection pools;
//   - difficulty (the legacy NOT NULL/CHECK column) is a display-only
//     synthetic here like for OTQA: level 1–2 'easy', 3–6 'medium',
//     7–15 'hard';
//   - category = 'WWTBAM:' + the collapsed src tag (the episode/show
//     provenance, mostly 'WWTBAM:Unknown').

const (
	// WWTBAMSource is stored questions.source for this corpus.
	WWTBAMSource = "wwtbam"
	// WWTBAMCategoryPre prefixes the provenance tag in category.
	WWTBAMCategoryPre = "WWTBAM:"
	// WWTBAMTagUnknown replaces a blank Question Source cell.
	WWTBAMTagUnknown = "Unknown"
)

// WWTBAMRow is one data/wwtbam.jsonl line (converter contract above).
type WWTBAMRow struct {
	Level    int      `json:"level"`
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Correct  string   `json:"correct"`
	Src      string   `json:"src"`
}

// WWTBAMStats is the run summary (logged and returned by cmd/import).
type WWTBAMStats struct {
	Imported   int
	Duplicates int
	Skipped    int // parse-level rejects: malformed line / <4 options / bad letter
	Total      int // count of source='wwtbam' rows after the run
}

// wwtbamDifficultyTier maps the level to the legacy difficulty column
// (display-only; strength is the governing axis).
func wwtbamDifficultyTier(level int) string {
	switch {
	case level <= 2:
		return "easy"
	case level <= 6:
		return "medium"
	default:
		return "hard"
	}
}

// wwtbamCategory collapses the src tag into its stored category value
// (whitespace-collapsed, trimmed, blank → Unknown).
func wwtbamCategory(src string) string {
	tag := whitespaceRe.ReplaceAllString(strings.TrimSpace(src), " ")
	if tag == "" {
		tag = WWTBAMTagUnknown
	}
	return WWTBAMCategoryPre + tag
}

// WWTBAMImporter imports the JSONL corpus idempotently (see the file
// comment): re-runs re-derive an identical insert set and find every row
// already present (dedupe is cross-corpus by normalized text), so a second
// run imports 0 new rows and just reports.
type WWTBAMImporter struct {
	repo repository.QuestionRepository
	file string
}

func NewWWTBAMImporter(repo repository.QuestionRepository, file string) *WWTBAMImporter {
	return &WWTBAMImporter{repo: repo, file: file}
}

func (i *WWTBAMImporter) Run(ctx context.Context) (WWTBAMStats, error) {
	f, err := os.Open(i.file)
	if err != nil {
		return WWTBAMStats{}, fmt.Errorf("opening wwtbam JSONL: %w", err)
	}
	defer f.Close()

	existing, err := i.repo.ListAll(ctx)
	if err != nil {
		return WWTBAMStats{}, fmt.Errorf("loading existing questions for dedupe: %w", err)
	}
	seen := make(map[string]bool, len(existing))
	for _, q := range existing {
		seen[normalizeQuestionText(q.Question)] = true
	}
	log.Printf("wwtbam: %d existing questions loaded for dedupe", len(seen))

	var st WWTBAMStats
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var row WWTBAMRow
		if err := json.Unmarshal(line, &row); err != nil {
			st.Skipped++
			log.Printf("wwtbam: malformed JSONL line skipped: %v", err)
			continue
		}
		if err := row.validate(); err != nil {
			st.Skipped++
			log.Printf("wwtbam: invalid row skipped (level %d): %v", row.Level, err)
			continue
		}
		norm := normalizeQuestionText(row.Question)
		if seen[norm] {
			st.Duplicates++
			continue
		}
		id, err := uuid.NewV7()
		if err != nil {
			return st, fmt.Errorf("generating question id: %w", err)
		}
		// Standing invariant (M0 review): every field is set explicitly
		// because ksql INSERT sends them all. Strength = the level.
		level := row.Level
		q := models.Question{
			ID:           id.String(),
			Question:     row.Question,
			Options:      marshalOptions(row.Options), // workbook A–D order
			CorrectIndex: wwtbamLetterIndex(row.Correct),
			Difficulty:   wwtbamDifficultyTier(level),
			Category:     wwtbamCategory(row.Src),
			Source:       WWTBAMSource,
			CreatedAt:    time.Now().UTC(),
			Strength:     &level,
		}
		if err := i.repo.InsertQuestion(ctx, &q); err != nil {
			return st, fmt.Errorf("inserting question (level %d): %w", row.Level, err)
		}
		seen[norm] = true
		st.Imported++
	}
	if err := scanner.Err(); err != nil {
		return st, fmt.Errorf("reading wwtbam JSONL: %w", err)
	}

	total, err := i.repo.CountBySource(ctx, WWTBAMSource)
	if err != nil {
		return st, fmt.Errorf("counting stored wwtbam rows: %w", err)
	}
	st.Total = total
	log.Printf("wwtbam: imported=%d duplicates=%d skipped=%d total=%d", st.Imported, st.Duplicates, st.Skipped, st.Total)
	return st, nil
}

// validate is the parse-level gate mirroring the converter's: exactly 4
// non-empty options, the correct letter inside a–d, level inside 1..15.
func (r WWTBAMRow) validate() error {
	if r.Level < 1 || r.Level > 15 {
		return fmt.Errorf("level %d outside 1..15", r.Level)
	}
	if normalizeQuestionText(r.Question) == "" {
		return fmt.Errorf("empty question")
	}
	if len(r.Options) != 4 {
		return fmt.Errorf("got %d options, want 4", len(r.Options))
	}
	for _, o := range r.Options {
		if strings.TrimSpace(o) == "" {
			return fmt.Errorf("empty option")
		}
	}
	if wwtbamLetterIndex(r.Correct) < 0 {
		return fmt.Errorf("correct %q not a letter a–d", r.Correct)
	}
	return nil
}

// wwtbamLetterIndex maps 'a'..'d' → 0..3 (-1 otherwise). Tolerates the
// workbook's uppercase convention and stray whitespace.
func wwtbamLetterIndex(letter string) int {
	l := strings.ToLower(strings.TrimSpace(letter))
	if len(l) != 1 || l[0] < 'a' || l[0] > 'd' {
		return -1
	}
	return int(l[0] - 'a')
}

// String formats the letter for strconv-free logging of run stats.
func (s WWTBAMStats) String() string {
	return fmt.Sprintf("imported=%d duplicates=%d skipped=%d total=%d",
		s.Imported, s.Duplicates, s.Skipped, s.Total)
}
