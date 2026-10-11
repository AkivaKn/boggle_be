package service

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"boggle-api/internal/millionaire/models"
	"boggle-api/internal/millionaire/repository"

	"github.com/google/uuid"
	"golang.org/x/text/encoding/charmap"
)

// This file is the M6 OpenTriviaQA importer (SPEC §2 question engine v2,
// corpus strategy source B): the OpenTriviaQA corpus is 22 text files
// (github.com/uberspot/OpenTriviaQA, CC BY-SA 4.0) with a MIXED codec —
// 10 of the 22 subcategories ship (predominantly) UTF-8 (animals,
// brain-teasers, entertainment, for-kids, geography, hobbies, music,
// newest, religion-faith, television) and the other 12 ship
// (predominantly) ISO-8859-1 — AND those latin-1 files themselves embed
// raw UTF-8 on individual lines (M6 round-1 live scan: e.g. `people`
// carries 95 `e2 80 xx` quote sequences inside an otherwise-latin-1
// file) and ship WINDOWS-1252 high bytes besides (smart quotes 93/94,
// ellipsis 85 — ISO-8859-1 maps those to bare C1 controls, which no
// stored row may carry). One file per subcategory, line-oriented format
// (see parseOTQAFile); decoding therefore sniffs PER LINE (otqaDecode),
// never per corpus and not even per file.
//
// IMPORT POLICY (user decision: sample first, scale on demand):
//   - a stratified SAMPLE per category (see otqaTasteClass: non-cultural
//     ~300, cultural ~150, mixed ~150), taking the first `cap` questions
//     each file yields
//     in file order (deterministic → idempotent re-runs import 0);
//   - taste classes per SPEC: cultural = music, television, movies,
//     celebrities, people, video-games, entertainment, rated;
//     non-cultural = general, world, science-technology, sports, history,
//     animals, geography, humanities, religion-faith, for-kids,
//     brain-teasers; mixed = newest, literature (capped either way).
//     DECISION on the unlisted subcategory "hobbies": NON-cultural
//     (pastime/craft content — see models.IsCultural).
//   - dedupe vs the whole existing corpus and within the run via the
//     seeder normalization (text-codec-safe across both sources);
//   - questions with fewer than 4 DISTINCT options are skipped; files
//     with MORE than 4 distractors are truncated to correct + first 3
//     distractors (file order);
//   - every stored row: source='otqa', Category='OTQA:<subcategory>',
//     strength NULL (the labeling pipeline rates it later), difficulty
//     'medium' — a synthetic value that merely satisfies the legacy
//     NOT NULL/CHECK column (v2 governs difficulty by strength, the
//     legacy column is irrelevant for OTQA rows; documented in the SPEC
//     note and counted nowhere: tier minimums are opentdb-only).

// OTQA dir names and counts (census over /tmp/opencode/otqa/categories,
// ≥4-distinct-option yield after within-file dedupe; re-measured
// post-codec-fix with the importer's own ParseYield stat):
//	animals 1015, brain-teasers 155, celebrities 2772, entertainment 221,
//	for-kids 584, general 2979, geography 777, history 1445, hobbies 1124,
//	humanities 948, literature 1123, movies 3746, music 4923, newest 2550,
//	people 2400, rated 1945, religion-faith 520, science-technology 2107,
//	sports 2397, television 4344, video-games 454, world 4158 → 42687.

const (
	otqaSource        = "otqa"
	otqaCategoryPre   = "OTQA:"
	otqaDifficultless = "medium" // synthetic legacy-difficulty for OTQA rows

	otqaSampleNonCultural = 300 // non-cultural categories weight heavier
	otqaSampleCultural    = 150 // cultural cap (taste layer targets ≤30%)
	otqaSampleMixed       = 150 // newest / literature capped either way
)

// otqaTasteClass maps a subcategory to its taste class and SAMPLE CAP
// (the user script: non-cultural ~300, cultural ~150, mixed ~150). An
// unknown subcategory caps at 0 — skipped with a warning — so a surprise
// file can never silently flood a taste class; add it here deliberately
// when the corpus grows.
func otqaTasteClass(subcategory string) (class string, cap int) {
	switch subcategory {
	case "music", "television", "movies", "celebrities", "people",
		"video-games", "entertainment", "rated":
		return "cultural", otqaSampleCultural
	case "newest", "literature":
		return "mixed", otqaSampleMixed
	case "general", "world", "science-technology", "sports", "history",
		"animals", "geography", "humanities", "religion-faith", "for-kids",
		"brain-teasers", "hobbies":
		return "non-cultural", otqaSampleNonCultural
	default:
		return "unknown", 0
	}
}

// OTQACategoryStats is the per-category outcome of an import run (logged
// per line, returned for the caller's summary).
type OTQACategoryStats struct {
	Category   string // subcategory file name (without the OTQA: prefix)
	Class      string // cultural | non-cultural | mixed
	Imported   int
	Duplicates int
	Skipped    int // parse-level rejects: <4 distinct options / no answer / empty
	Total      int // count of that category in the DB after the run
	Cap        int // the sample cap applied
	ParseYield int // ≥4-option unique questions the file yields (even beyond cap)
}

// OTQAImporter imports the OpenTriviaQA sample into the questions table,
// idempotently: re-runs re-derive the identical sample from the files and
// find every question already present (dedupe is cross-corpus by the
// normalized text), so a second run imports 0 new rows and just reports.
type OTQAImporter struct {
	repo repository.QuestionRepository
	dir  string
}

func NewOTQAImporter(repo repository.QuestionRepository, dir string) *OTQAImporter {
	return &OTQAImporter{repo: repo, dir: dir}
}

func (i *OTQAImporter) Run(ctx context.Context) ([]OTQACategoryStats, error) {
	existing, err := i.repo.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading existing questions for dedupe: %w", err)
	}
	seen := make(map[string]bool, len(existing))
	for _, q := range existing {
		seen[normalizeQuestionText(q.Question)] = true
	}
	log.Printf("otqa: %d existing questions loaded for dedupe", len(seen))

	files, err := filepath.Glob(filepath.Join(i.dir, "*"))
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", i.dir, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no category files in %s — stage the OpenTriviaQA categories first (README: docker cp)", i.dir)
	}

	var allStats []OTQACategoryStats
	for _, path := range files {
		cat := filepath.Base(path)
		class, cap := otqaTasteClass(cat)
		if cap == 0 {
			log.Printf("otqa: %s: unknown subcategory — skipped (add a taste class to classifyOTQA deliberately)", cat)
			continue
		}
		st, err := i.importCategory(ctx, path, cat, class, cap, seen)
		if err != nil {
			return allStats, fmt.Errorf("importing %s: %w", cat, err)
		}
		log.Printf("otqa: %s (%s): imported=%d duplicates=%d skipped=%d total=%d (sample cap %d, file yield %d)",
			cat, class, st.Imported, st.Duplicates, st.Skipped, st.Total, st.Cap, st.ParseYield)
		allStats = append(allStats, st)
	}
	return allStats, nil
}

// importCategory parses one category file, selects the deterministic
// sample (first `cap` unique-form-ok questions in file order) and inserts
// the previously-unseen ones. Statistics count parse-level rejects from
// the WHOLE file and dedupe hits from the SAMPLE only (a re-run reports
// the whole sample as duplicates — the honest idempotency signal).
func (i *OTQAImporter) importCategory(ctx context.Context, path, category, class string, cap int, seen map[string]bool) (OTQACategoryStats, error) {
	st := OTQACategoryStats{Category: category, Class: class, Cap: cap}

	blocks, err := parseOTQAFile(path)
	if err != nil {
		return st, err
	}

	total, err := i.repo.CountByCategory(ctx, otqaCategoryPre+category)
	if err != nil {
		return st, fmt.Errorf("counting existing %s rows: %w", category, err)
	}
	st.Total = total

	// Pass 1: parse-level filter + within-file normalized dedupe. The
	// SAMPLE is the first `cap` uniques in file order, but ParseYield
	// counts EVERY ≥4-distinct-option unique question in the whole file
	// (per its docstring: the file's true yield, even beyond the cap) —
	// the loop therefore never breaks, it only stops filling the sample.
	type sampled struct {
		question   string   // stored text (in file order)
		options    []string // exactly 4: [correct, d1, d2, d3]
		normalized string
	}
	var sample []sampled
	yieldSeen := map[string]bool{} // ≥4-option uniques across the WHOLE file
	for _, b := range blocks {
		question, options, ok := b.build()
		if !ok || normalizeQuestionText(question) == "" {
			st.Skipped++
			continue
		}
		norm := normalizeQuestionText(question)
		if yieldSeen[norm] {
			continue // within-file duplicate: counted once, first occurrence
		}
		yieldSeen[norm] = true
		st.ParseYield++
		if len(sample) >= cap {
			continue // deterministic truncation: first `cap` in file order
		}
		sample = append(sample, sampled{question: question, options: options, normalized: norm})
	}

	// Pass 2: dedupe vs the whole corpus + insert the unseen.
	for _, s := range sample {
		if seen[s.normalized] {
			st.Duplicates++
			continue
		}
		id, err := uuid.NewV7()
		if err != nil {
			return st, fmt.Errorf("generating question id: %w", err)
		}
		// Standing invariant (M0 review): every field is set explicitly
		// because ksql INSERT sends them all. Strength stays NULL — the
		// labeling pipeline (cmd/label) writes it.
		q := models.Question{
			ID:           id.String(),
			Question:     s.question,
			Options:      marshalOptions(s.options),
			CorrectIndex: 0, // options are stored [correct, d1..d3]
			Difficulty:   otqaDifficultless,
			Category:     otqaCategoryPre + category,
			Source:       otqaSource,
			CreatedAt:    time.Now().UTC(),
		}
		if err := i.repo.InsertQuestion(ctx, &q); err != nil {
			return st, fmt.Errorf("inserting question: %w", err)
		}
		seen[s.normalized] = true
		st.Imported++
	}

	st.Total += st.Imported
	return st, nil
}

// --- parser ----------------------------------------------------------------

// otqaBlock is one parsed question block of an OpenTriviaQA file.
type otqaBlock struct {
	question string   // '#'/'Q' text + continuation lines (space-joined)
	options  []string // lettered options in file order (raw, undeduped)
	answer   string   // the '^' line ('' when missing → block rejected)
}

// build materializes the block into stored form (≥4 DISTINCT options
// required, stored as [correct, first-3 distractors in file order]), or
// reports ok=false. The correct text always equals one of the lettered
// options (census: 0/49716 mismatches in the shipped corpus); if it ever
// fails to match, that block is a skip — the stored form must be sound.
func (b otqaBlock) build() (string, []string, bool) {
	text := strings.TrimSpace(b.question)
	if text == "" || b.answer == "" {
		return "", nil, false
	}
	seen := map[string]bool{}
	var distinct []string
	for _, o := range b.options {
		o = strings.TrimSpace(o)
		if o == "" || seen[o] {
			continue
		}
		seen[o] = true
		distinct = append(distinct, o)
	}
	correct := strings.TrimSpace(b.answer)
	if !seen[correct] {
		return "", nil, false
	}
	var distractors []string
	for _, o := range distinct {
		if o != correct {
			distractors = append(distractors, o)
		}
	}
	if len(distractors) < 3 {
		return "", nil, false // < 4 distinct options in total
	}
	options := append([]string{correct}, distractors[:3]...)
	return text, options, true
}

var otqaChoiceRe = regexp.MustCompile(`^[A-Z] `)

// parseOTQAFile reads one category file and yields the decoded question
// blocks. Codec: PER-LINE sniff (see otqaDecode) — never an assumption.
func parseOTQAFile(path string) ([]otqaBlock, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading category file: %w", err)
	}
	decoded, err := otqaDecode(raw)
	if err != nil {
		return nil, fmt.Errorf("decoding category file: %w", err)
	}
	return decodeOTQA(bytes.NewReader(decoded))
}

// otqaDecode applies the corpus codec with a PER-LINE sniff over the raw
// bytes of one category file and returns the decoded UTF-8 text:
//
//   - strict UTF-8 validation per LINE first (unicode/utf8.Valid — the
//     corollary of "the corpus is not single-codec": 10 of the 22
//     shipped files are UTF-8 — e.g. animals carries `lion’s` as the
//     raw bytes e2 80 99 — and the latin-1 files embed UTF-8 lines too,
//     like `people` serving `â<80><99>`-makers such as byte runs e2 80 9c
//     that decode cleanly as U+201C). A UTF-8-valid line passes through
//     byte-for-byte;
//   - ISO-8859-1-flavored → UTF-8 decoding ONLY for lines that FAIL the
//     validation (the genuinely latin-1 lines: accented text as single
//     high bytes such as é → e9).
//
// The strict-UTF-8-first order is the safe direction at line granularity:
// latin-1 high bytes standing alone (or in non-multibyte runs) are
// invalid UTF-8 as a rule, while every legitimately UTF-8 line validates
// in full. Any corpus-content risk (a latin-1 byte pair that is also a
// valid UTF-8 sequence, or a line that blends both codecs) is inherent
// to a double-encoded import file and confined to that one line. Census
// at fix time: all 22 shipped files classify line-wise exactly as
// expected — the sniff is total for the corpus and keeps surprise files
// from ever decoding into mojibake rows again.
//
// The fallback codec is WINDOWS-1252, not ISO-8859-1, on evidence (the
// post-fix DB scan): the corpus ships cp1252 punctuation — smart quotes
// 93/94 (→ “…realistic…”), the 85 ellipsis, etc. — which ISO-8859-1 maps
// to bare C1 CONTROLS (U+0093, mojibake-grade garbage); cp1252 maps the
// exact same bytes to the intended punctuation. cp1252 agrees with
// ISO-8859-1 on A0..FF, so accented text is unaffected, and its 0x80..9F
// interpretations are always more faithful than C1 controls (C1 never
// appears as question content).
func otqaDecode(raw []byte) ([]byte, error) {
	// Fast path: a wholly-UTF-8 file (10 of 22) passes through untouched.
	if utf8.Valid(raw) {
		return raw, nil
	}
	var out []byte
	for len(raw) > 0 {
		var line, rest []byte
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			line, rest = raw[:i], raw[i+1:]
		} else {
			line, rest = raw, nil
		}
		if utf8.Valid(line) {
			out = append(out, line...)
		} else {
			decoded, err := charmap.Windows1252.NewDecoder().Bytes(line)
			if err != nil {
				return nil, err
			}
			out = append(out, decoded...)
		}
		if rest != nil {
			out = append(out, '\n') // a real break: exactly one, preserved
		}
		raw = rest
	}
	return out, nil
}

// decodeOTQA parses the line semantics of one OpenTriviaQA file. The
// reader r must already yield UTF-8 text — the per-line codec sniff is
// the caller's job (parseOTQAFile → otqaDecode).
// Line rules mirror the corpus's own util/converter.py (the official
// parser):
//   - "#Q " starts a question;
//   - "^ " is the correct answer (its exact text also appears as one of
//     the lettered options — a corpus convention);
//   - "A " .. "F " (single uppercase letter + space) are options;
//   - any other non-empty line is a QUESTION CONTINUATION (appended with
//     a single space; the shipped corpus holds multi-line phrasings);
//   - a blank line ends the question once it has an answer (blocks that
//     end prematurely — no answer / options — are rejected by build()).
func decodeOTQA(r io.Reader) ([]otqaBlock, error) {
	var blocks []otqaBlock
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var cur *otqaBlock
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "#Q "):
			if cur != nil {
				blocks = append(blocks, *cur)
			}
			cur = &otqaBlock{question: line[3:]}
		case strings.HasPrefix(line, "^ "):
			if cur != nil {
				cur.answer = line[2:]
			}
		case otqaChoiceRe.MatchString(line):
			if cur != nil {
				cur.options = append(cur.options, line[2:])
			}
		case line == "":
			// Question terminator iff an answer was already found; stray
			// blank lines (mid-question, e.g. before a continuation) and
			// pre-open lines are ignored ("incorrectly placed empty
			// lines" in the official converter).
			if cur != nil && cur.answer != "" {
				blocks = append(blocks, *cur)
				cur = nil
			}
		default:
			if cur != nil {
				cur.question = strings.TrimSpace(cur.question + " " + line)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading category file: %w", err)
	}
	if cur != nil {
		blocks = append(blocks, *cur) // EOF finalizer
	}
	return blocks, nil
}
