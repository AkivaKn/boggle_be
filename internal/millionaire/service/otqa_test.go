package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"boggle-api/internal/millionaire/models"
)

// This file covers the M6 OpenTriviaQA importer: the latin-1 decode (the
// corpus codec — the REQUIRED accented-line proof), the #Q/^/letter format
// rules (mirroring the corpus's own util/converter.py), the ≥4-distinct-
// options gate with correct/truncate behavior, and the importer's
// sample caps + cross-corpus dedupe + idempotency against an in-memory
// fake repository.

// --- fixtures --------------------------------------------------------------

// latin1 re-encodes a UTF-8 Go string into ISO-8859-1 bytes (as a byte
// string) so tests can hand the parser the corpus's RAW bytes.
func latin1(t *testing.T, utf8Text string) string {
	t.Helper()
	out := make([]byte, 0, len(utf8Text))
	for _, r := range utf8Text {
		if r < 0x80 {
			out = append(out, byte(r))
			continue
		}
		if r > 0xFF {
			t.Fatalf("fixture text contains %q which is not ISO-8859-1 mappable", r)
		}
		out = append(out, byte(r))
	}
	return string(out)
}

// otqaFixtureDir writes latin-1 category files into a temp dir and returns
// the DIR (the importer reads categories directories).
func otqaFixtureDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// otqaQuestionText builds one full valid block: the correct text is echoed
// among the lettered options (last position), as the corpus convention has
// it — `^` must equal exactly one option.
func otqaQuestionText(prompt, correct string, distractors ...string) string {
	letters := []string{"A", "B", "C", "D", "E", "F"}
	var sb strings.Builder
	sb.WriteString("#Q " + prompt + "\n")
	sb.WriteString("^ " + correct + "\n")
	opts := append(append([]string(nil), distractors...), correct)
	for i, o := range opts {
		sb.WriteString(letters[i] + " " + o + "\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

// --- parser ----------------------------------------------------------------

// TestOTQAParserLatin1Decode is the REQUIRED latin-1 proof: a question
// carrying an accented word arrives on disk as RAW ISO-8859-1 bytes
// (latin1() re-encodes the source é to the single byte 0xE9) and the
// decoded block must restore proper UTF-8 (é, valid throughout).
func TestOTQAParserLatin1Decode(t *testing.T) {
	dir := otqaFixtureDir(t, map[string]string{
		"latintest": latin1(t, "#Q Classic café grilling: which beverage is served décaféinized?\n") +
			"^ Espresso\n" +
			"A Espresso\n" + "B Cold brew\n" + "C Graham cracker crumbs\n" + "D Black bread\n" + "\n",
	})
	blocks, err := parseOTQAFile(filepath.Join(dir, "latintest"))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	b := blocks[0]
	if !strings.Contains(b.question, "café") || !strings.Contains(b.question, "décaféinized") {
		t.Fatalf("decoded question %q missing the accented words — latin-1 decode broken", b.question)
	}
	if !utf8valid(b.question) || !utf8valid(b.answer) {
		t.Fatalf("decoded block not valid UTF-8: %q / %q", b.question, b.answer)
	}
	if q, options, ok := b.build(); !ok {
		t.Fatal("block must build")
	} else if !utf8valid(q) || len(options) != 4 {
		t.Fatalf("built block invalid: %q %v", q, options)
	}
}

// utf8valid dodges the direct strings/utf8 import dance in two places.
func utf8valid(s string) bool {
	// Ranging a string yields RUNES; invalid UTF-8 surfaces as U+FFFD.
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

// TestOTQAParserLatin1AccentInOptions proves option text decodes too: an
// accented CORRECT answer survives decode + build as options[0].
func TestOTQAParserLatin1AccentInOptions(t *testing.T) {
	correct := "Pâté dé résistance" // encoded as RAW latin-1 bytes below
	dir := otqaFixtureDir(t, map[string]string{
		"accents": latin1(t, "#Q Which French dish is this?\n") +
			"^ " + latin1(t, correct) + "\n" +
			"A " + latin1(t, correct) + "\n" +
			"B Casserole\n" +
			"C Baked beans\n" +
			"D Riz pourri\n" + "\n",
	})
	blocks, err := parseOTQAFile(filepath.Join(dir, "accents"))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d", len(blocks))
	}
	q, options, ok := blocks[0].build()
	if !ok {
		t.Fatal("block should build")
	}
	if q != "Which French dish is this?" {
		t.Fatalf("q = %q", q)
	}
	if options[0] != "Pâté dé résistance" { // stored form is [correct, d1, d2, d3]
		t.Fatalf("options[0] = %q, want the decoded correct answer", options[0])
	}
}

// TestOTQAParserUTF8ShippedLine is the M6 round-1 [high] regression: a
// REAL UTF-8-shipped OpenTriviaQA line — the animals category question,
// shipped with ’ as the raw bytes e2 80 99 — must decode CLEAN. The old
// always-latin-1 decoder produced the mojibake `lionâ<80><99>s` (U+00E2 +
// C1 controls) DB-wide (~112 rows). Assertions: ’ survives; NO C1 control
// characters (U+0080–U+009F) anywhere in the block — those are the exact
// fingerprint that the strict-UTF-8-first sniff kills. Paired with a
// genuinely latin-1 fixture (must decode through the Windows-1252
// fallback) and a MIXED fixture (the live-scan reality: latin-1 files
// embed whole UTF-8 lines, e.g. `people` carrying raw e2 80 9c “-quotes —
// the per-LINE sniff must handle both encodings inside ONE file).
func TestOTQAParserUTF8ShippedLine(t *testing.T) {
	// Real corpus bytes: OpenTriviaQA / categories / animals, the
	// `lion's diet` question block, byte-for-byte as shipped upstream.
	realUTF8 := "#Q Approximately what amount of meat does an adult male lion" +
		"\xe2\x80\x99" + "s diet require per day?\n" +
		"^ 15 lbs. (6.8 kg)\n" +
		"A 10 lbs. (4.5 kg)\n" +
		"B 5 lbs. (2.2 kg)\n" +
		"C 15 lbs. (6.8 kg)\n" +
		"D 25 lbs. (11 kg)\n\n"
	dir := otqaFixtureDir(t, map[string]string{
		"utf8animals": realUTF8,
		"latinfile":   latin1(t, "#Q Classic café grilling?\n") + "^ Espresso\nA Espresso\nB Cold brew\nC Décaféiné crumb\nD Black bread\n\n",
		// Mixed file (real-corpus shape): a whole UTF-8 question line +
		// a latin-1 continuation line (raw 0xE9) in the SAME file — the
		// per-LINE sniff must resolve each line independently.
		"mixedfile": "#Q Which character is \xe2\x80\x9c’\xe2\x80\x9d served?\n" +
			latin1(t, "Café rendition named:\n") + // continuation, latin-1 é
			"^ Espresso\nA Espresso\nB Cold brew\nC Decaf crumb\nD Black bread\n\n",
		// cp1252 punctuation file (the residual live-scan reality): raw
		// 93/94 byte pairs that ISO-8859-1 would decode as C1 controls —
		// the Windows-1252 fallback must map them to “ ”.
		"cp1252file": "#Q The Nobel citation: \x93realistic and imaginative\x94 lands.\n" +
			"^ John Steinbeck\nA John Steinbeck\nB John Tolkien\nC John Wayne\nD John Milton\n\n",
	})

	// 1) The real UTF-8-shipped file decodes clean (strict-UTF-8 path).
	blocks, err := parseOTQAFile(filepath.Join(dir, "utf8animals"))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	b := blocks[0]
	if !strings.Contains(b.question, "lion’s diet") {
		t.Fatalf("decoded question %q lost the right single quote — UTF-8 sniff broken", b.question)
	}
	if got := fmt.Sprint(b.options); strings.Contains(got, "â") || hasC1(b.question) || hasC1(got) {
		t.Fatalf("mojibake fingerprint in decoded block: %q / %v", b.question, b.options)
	}
	if q, _, ok := b.build(); !ok {
		t.Fatal("the real block must build")
	} else if hasC1(q) {
		t.Fatalf("built stored text carries C1 controls: %q", q)
	}

	// 2) A genuinely latin-1 file still decodes through the fallback.
	latin, err := parseOTQAFile(filepath.Join(dir, "latinfile"))
	if err != nil {
		t.Fatal(err)
	}
	if len(latin) != 1 || !strings.Contains(latin[0].question, "café") {
		t.Fatalf("latin-1 fallback broken: %d blocks / %q", len(latin), latin[0].question)
	}
	if hasC1(latin[0].question) {
		t.Fatalf("latin-1 file decoded with C1 mangling: %q", latin[0].question)
	}

	// 3) The mixed file: BOTH codecs inside one file decode per line.
	mixed, err := parseOTQAFile(filepath.Join(dir, "mixedfile"))
	if err != nil {
		t.Fatal(err)
	}
	if len(mixed) != 1 {
		t.Fatalf("mixed blocks = %d, want 1", len(mixed))
	}
	wantMixed := "Which character is “’” served? Café rendition named:"
	if mixed[0].question != wantMixed {
		t.Fatalf("mixed line sniff broken: got %q, want %q", mixed[0].question, wantMixed)
	}
	if hasC1(mixed[0].question) {
		t.Fatalf("mixed file decoded with C1 mangling: %q", mixed[0].question)
	}

	// 4) The cp1252 file: smart-quote byte pairs decode as “ ”, never as
	// bare C1 controls (0x93 → U+201C; the old ISO-8859-1 fallback
	// emitted U+0093).
	cp, err := parseOTQAFile(filepath.Join(dir, "cp1252file"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cp) != 1 {
		t.Fatalf("cp1252 blocks = %d, want 1", len(cp))
	}
	if !strings.Contains(cp[0].question, "“realistic and imaginative”") || hasC1(cp[0].question) {
		t.Fatalf("cp1252 fallback broken: %q", cp[0].question)
	}
}

// hasC1 reports the mojibake fingerprint: any U+0080–U+009F control
// character that no legitimate decoded corpus text contains (UTF-8 text
// legitimately holds code points ABOVE 0x9F, e.g. ’ U+2019 — those are
// fine).
func hasC1(s string) bool {
	for _, r := range s {
		if r >= 0x80 && r <= 0x9F {
			return true
		}
	}
	return false
}

// TestOTQAParserOptionCounts exercises the corpus's 2–6 options spectrum:
// only ≥4 DISTINCT options pass the gate — 2/3 skipped, 5/6 truncated to
// correct + first 3 distinct distractors, duplicated options collapse.
func TestOTQAParserOptionCounts(t *testing.T) {
	cases := []struct {
		name    string
		options []string // raw lettered options (correct echoed somewhere)
		answer  string
		ok      bool
	}{
		{"2 options skipped", []string{"Correct", "Wrong"}, "Correct", false},
		{"3 options skipped", []string{"D1", "D2", "Correct"}, "Correct", false},
		{"4 options ok", []string{"D1", "D2", "D3", "Correct"}, "Correct", true},
		{"5 options truncated", []string{"D1", "D2", "D3", "D4", "Correct"}, "Correct", true},
		{"6 options truncated", []string{"D1", "D2", "D3", "D4", "D5", "Correct"}, "Correct", true},
		{"duplicate distractors collapse below 4", []string{"D1", "D1", "D2", "Correct"}, "Correct", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := otqaBlock{question: "x?", answer: c.answer, options: c.options}
			_, options, ok := b.build()
			if ok != c.ok {
				t.Fatalf("build ok=%v, want %v", ok, c.ok)
			}
			if c.ok && (len(options) != 4 || options[0] != c.answer) {
				t.Fatalf("options = %v, want [correct, d1, d2, d3]", options)
			}
		})
	}
}

// TestOTQAParserTruncationKeepsFirstDistractors pins the >4 option
// verdict precisely: correct + FIRST 3 distinct distractors in FILE
// order — even when the correct option comes late (it must be KEPT while
// later distractors drop) and duplicates never count toward the three.
func TestOTQAParserTruncationKeepsFirstDistractors(t *testing.T) {
	cases := []struct {
		name    string
		options []string
		want    string
	}{
		{"correct early", []string{"Correct", "D1", "D2", "D3", "D4"}, "[Correct D1 D2 D3]"},
		{"correct late", []string{"D1", "D2", "D3", "D4", "Correct"}, "[Correct D1 D2 D3]"},
		{"duplicates skipped in order", []string{"D1", "Correct", "D1", "D2", "D3", "D4"}, "[Correct D1 D2 D3]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := otqaBlock{question: "x?", answer: "Correct", options: c.options}
			_, options, ok := b.build()
			if !ok {
				t.Fatal("must build")
			}
			if got := fmt.Sprint(options); got != c.want {
				t.Fatalf("options = %v, want %s", options, c.want)
			}
		})
	}
}

// TestOTQAParserContinuationLines mirrors the corpus's multi-line blocks:
// non-#Q/^/letter lines are question CONTINUATIONS (space-joined), stray
// blank lines mid-question are ignored, and a blank line WITH an already
// found answer terminates the block (official converter semantics).
func TestOTQAParserContinuationLines(t *testing.T) {
	file := "#Q Finish the proverb:\n" + // the stray blank (no answer) is ignored
		"Poets are born, ________.\n" + // continuation #1
		"\n" + // another stray blank mid-question
		"and the stars are lit.\n" + // continuation #2
		"^ ...not made.\n" +
		"A ...not made.\n" +
		"B ...but can also be made.\n" +
		"C ...but that one fails.\n" +
		"D ...long live the poets!\n" +
		"\n" + // TERMINATOR (answer already found)
		"#Q orphan block\n" +
		"^ Missing options entirely\n" +
		"\n"
	blocks, err := decodeOTQA(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d (%+v)", len(blocks), blocks)
	}
	wantQ := "Finish the proverb: Poets are born, ________. and the stars are lit."
	if got := blocks[0].question; got != wantQ {
		t.Fatalf("continuations = %q, want %q", got, wantQ)
	}
	if _, _, ok := blocks[0].build(); !ok {
		t.Fatal("proverb block should build")
	}
	if _, _, ok := blocks[1].build(); ok {
		t.Fatal("answer-without-options block must be rejected by build")
	}
}

// TestOTQAParserSkipsDegenerateBlocks — empty question, missing answer and
// correct-not-among-options blocks never reach the importer's inserts.
func TestOTQAParserSkipsDegenerateBlocks(t *testing.T) {
	cases := []otqaBlock{
		{question: "", answer: "Yes", options: []string{"Yes", "A", "B", "C"}},
		{question: "No answer", answer: "", options: []string{"A", "B", "C", "D"}},
		{question: "Correct not an option", answer: "Correct", options: []string{"A", "B", "C", "D"}},
	}
	for _, b := range cases {
		if _, _, ok := b.build(); ok {
			t.Fatalf("block %+v must not build", b)
		}
	}
}

// --- importer --------------------------------------------------------------

// baseFakeQuestionRepo implements repository.QuestionRepository for the
// importer/selection tests (an in-memory corpus with a canonical ranked
// view for band queries).
type baseFakeQuestionRepo struct {
	inserted []models.Question
	rated    []models.Question // pre-rated corpus rows (Strength set)
	legacy   []models.Question // RandomByTier fallback pool (unrated)

	// M10 S2 casual-pool fixture rows + call records (see casual_test.go):
	// casual / failSafe are the pools the fake's casual methods scan, so
	// the four L1–3 branches can be driven deterministically.
	casual        []models.Question // strength 1..n rows (blacklist rules apply via the filter)
	failSafe      []models.Question // opentdb easy rows over the fail-safe categories
	casualCalls   []int             // maxStrength of each RandomByCasual call
	failSafeCalls []int             // RandomByFailSafeCasual call log

	// M10 S3 WWTBAM-pool fixture rows + call records (see wwtbam_test.go):
	// wwtbam is the per-level pool the fake's source-level methods scan
	// (any source; the queries key on (source, strength)).
	wwtbam      []models.Question
	wwtbamCalls []wwtbamCall
	bandCalls   []bandCall
}

type bandCall struct {
	lo, hi          int
	excludeCultural bool
	limit           int
	exclude         []string
}

type wwtbamCall struct {
	source  string
	level   int
	limit   int
	exclude []string
}

func (f *baseFakeQuestionRepo) InsertQuestion(ctx context.Context, q *models.Question) error {
	f.inserted = append(f.inserted, *q)
	return nil
}

func (f *baseFakeQuestionRepo) CountByTier(ctx context.Context, difficulty string) (int, error) {
	n := 0
	for _, q := range f.legacy {
		if q.Difficulty == difficulty {
			n++
		}
	}
	return n, nil
}

func (f *baseFakeQuestionRepo) CountByCategory(ctx context.Context, category string) (int, error) {
	n := 0
	for _, q := range f.inserted {
		if q.Category == category {
			n++
		}
	}
	return n, nil
}

func (f *baseFakeQuestionRepo) CountRated(ctx context.Context) (int, error) {
	return len(f.rated), nil
}

func (f *baseFakeQuestionRepo) ListAll(ctx context.Context) ([]models.Question, error) {
	all := append(append([]models.Question(nil), f.rated...), f.legacy...)
	all = append(all, f.inserted...)
	return all, nil
}

func (f *baseFakeQuestionRepo) RandomByTier(ctx context.Context, difficulty string, limit int, exclude []string) ([]models.Question, error) {
	var out []models.Question
	for _, q := range f.legacy {
		if q.Difficulty != difficulty || excludedID(exclude, q.ID) {
			continue
		}
		out = append(out, q)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func excludedID(exclude []string, id string) bool {
	for _, e := range exclude {
		if e == id {
			return true
		}
	}
	return false
}

// canonical returns the fake's ranked list (ORDER BY strength, id — the
// exact SQL window ordering).
func (f *baseFakeQuestionRepo) canonical() []models.Question {
	out := append([]models.Question(nil), f.rated...)
	sort.SliceStable(out, func(i, j int) bool {
		if *out[i].Strength != *out[j].Strength {
			return *out[i].Strength < *out[j].Strength
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (f *baseFakeQuestionRepo) RandomByRankBand(ctx context.Context, lo, hi int, excludeCultural bool, limit int, exclude []string) ([]models.Question, error) {
	f.bandCalls = append(f.bandCalls, bandCall{lo, hi, excludeCultural, limit, append([]string(nil), exclude...)})
	// Mirror the SQL shape: rank the non-excluded rows in strength order
	// (the exclusions shrink the ranking INSIDE the window subquery), then
	// apply the taste filter to the window's occupants (the SQL does this
	// outside the ROW_NUMBER, leaving cultural holes in the window) and
	// return the first `limit` survivors. Deterministic, like the other
	// fake methods.
	var out []models.Question
	rank := 0
	for _, q := range f.canonical() {
		if q.Source == WWTBAMSource {
			// Mirror the SQL shape (M10 fix): wwtbam rows never rank into
			// a band — they are owned by the strict per-level pools.
			continue
		}
		if excludedID(exclude, q.ID) {
			continue
		}
		rank++
		if rank < lo {
			continue
		}
		if rank > hi {
			break
		}
		if excludeCultural && models.IsCultural(q.Category, q.Source) {
			continue
		}
		out = append(out, q)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// --- WWTBAM per-level pool (M10 S3) ----------------------------------------

// CountBySourceLevel mirrors the SQL contract: exactly-strength rows of
// one source counted (NULL-strength rows are not on the axis).
func (f *baseFakeQuestionRepo) CountBySourceLevel(ctx context.Context, source string, level int) (int, error) {
	n := 0
	for _, q := range f.wwtbam {
		if q.Source == source && q.Strength != nil && *q.Strength == level {
			n++
		}
	}
	return n, nil
}

// RandomBySourceLevel mirrors the SQL contract: random rows of one source
// at exactly one strength, minus the exclusions (deterministic first-hit
// order like the other fake methods).
func (f *baseFakeQuestionRepo) RandomBySourceLevel(ctx context.Context, source string, level int, limit int, exclude []string) ([]models.Question, error) {
	f.wwtbamCalls = append(f.wwtbamCalls, wwtbamCall{source, level, limit, append([]string(nil), exclude...)})
	var out []models.Question
	for _, q := range f.wwtbam {
		if q.Source != source || q.Strength == nil || *q.Strength != level {
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

// CountBySource counts every fixture row of one source (importer total).
func (f *baseFakeQuestionRepo) CountBySource(ctx context.Context, source string) (int, error) {
	n := 0
	for _, q := range append(append(append(append([]models.Question{}, f.wwtbam...), f.rated...), f.legacy...), f.inserted...) {
		if q.Source == source {
			n++
		}
	}
	return n, nil
}

// TestOTQAImporterSampleCapsIdempotent proves, against the fake repo:
//   - the stratified SAMPLE caps (non-cultural 300 / cultural 150 / mixed
//     150) with deterministic first-N truncation;
//   - within-file dedupe; parse-level rejects (<4 options) skipped;
//   - cross-corpus dedupe vs an existing opentdb row;
//   - IDENTITY of shape: source='otqa', Category='OTQA:<sub>', correct
//     index 0 (stored [correct, d1..d3]), strength NULL;
//   - idempotency: a second FULL run imports 0 (every sample question is
//     now a duplicate) — the honest no-op signal.
func TestOTQAImporterSampleCapsIdempotent(t *testing.T) {
	var general strings.Builder // non-cultural: yields 310 → imports 300
	for i := 1; i <= 310; i++ {
		general.WriteString(otqaQuestionText(fmt.Sprintf("general question number %d?", i), fmt.Sprintf("Answer %d", i), "X1", "X2", "X3"))
	}
	var music strings.Builder // cultural: 5 valid + 2 in-file dupes + 1 malformed
	for i := 1; i <= 5; i++ {
		music.WriteString(otqaQuestionText(fmt.Sprintf("music question %d?", i), fmt.Sprintf("Song %d", i), "Note A", "Note B", "Note C"))
	}
	for i := 1; i <= 2; i++ {
		music.WriteString(otqaQuestionText(fmt.Sprintf("music question %d?", i), fmt.Sprintf("Song %d", i), "Note A", "Note B", "Note C"))
	}
	music.WriteString("#Q music broken block\n^ Song Zero\nA one\nB two\nC three\n\n") // <4 options
	var newest strings.Builder                                                         // mixed: yields 220 → imports 150
	for i := 1; i <= 220; i++ {
		newest.WriteString(otqaQuestionText(fmt.Sprintf("newest question %d?", i), fmt.Sprintf("Headline %d", i), "N1", "N2", "N3"))
	}
	var world strings.Builder // one duplicated opentdb question + none new
	world.WriteString(otqaQuestionText(
		"PREEXISTING which should collide with the otqa world duplicate?", "a", "b", "c", "d"))

	repo := &baseFakeQuestionRepo{}
	repo.legacy = append(repo.legacy, models.Question{
		ID: "pre-1", Question: "PREEXISTING which should collide with the otqa world duplicate?",
		Options: `["a","b","c","d"]`, CorrectIndex: 0, Difficulty: "easy",
		Category: "General Knowledge", Source: "opentdb", CreatedAt: time.Now().UTC(),
	})

	dir := otqaFixtureDir(t, map[string]string{
		"general": general.String(),
		"music":   music.String(),
		"newest":  newest.String(),
		"world":   world.String(),
	})
	importer := NewOTQAImporter(repo, dir)
	stats, err := importer.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byCat := map[string]OTQACategoryStats{}
	for _, st := range stats {
		byCat[st.Category] = st
	}

	if g := byCat["general"]; g.Imported != 300 {
		t.Fatalf("general imported=%d, want the non-cultural cap 300 (yield 310)", g.Imported)
	}
	if m := byCat["music"]; m.Imported != 5 {
		t.Fatalf("music imported=%d, want 5 (yield 5; within-file dupes don't feed the sample)", m.Imported)
	}
	if m := byCat["music"]; m.Skipped < 1 {
		t.Fatalf("music skipped=%d, want ≥1 (the <4-option block)", m.Skipped)
	}
	if n := byCat["newest"]; n.Imported != 150 {
		t.Fatalf("newest imported=%d, want the mixed cap 150", n.Imported)
	}
	// ParseYield counts the WHOLE-FILE ≥4-option unique yield (even
	// beyond the sample cap — the docstring's promise; M6 round-1 review
	// fixed the old break-at-cap that reported the capped count).
	if g := byCat["general"]; g.ParseYield != 310 {
		t.Fatalf("general ParseYield=%d, want the full unique yield 310 (cap 300)", g.ParseYield)
	}
	if n := byCat["newest"]; n.ParseYield != 220 {
		t.Fatalf("newest ParseYield=%d, want the full unique yield 220 (cap 150)", n.ParseYield)
	}
	if m := byCat["music"]; m.ParseYield != 5 {
		t.Fatalf("music ParseYield=%d, want 5 (uniques only; within-file dupes never count)", m.ParseYield)
	}
	if w := byCat["world"]; w.Imported != 0 || w.Duplicates != 1 {
		t.Fatalf("world imported=%d duplicates=%d, want 0/1 (cross-corpus dedupe)", w.Imported, w.Duplicates)
	}

	// Shape of every stored row.
	for _, q := range repo.inserted {
		if q.Source != otqaSource || !strings.HasPrefix(q.Category, otqaCategoryPre) {
			t.Fatalf("inserted row %+v: wrong source/category", q)
		}
		if q.CorrectIndex != 0 {
			t.Fatalf("inserted row %s: correct_index %d, want 0", q.ID, q.CorrectIndex)
		}
		if q.Strength != nil {
			t.Fatalf("inserted row %s: strength must be NULL (labeling rates it later)", q.ID)
		}
		if !validUTF8Row(q) {
			t.Fatalf("inserted row %s: decoded text/options not valid UTF-8", q.ID)
		}
	}
	if want := 300 + 5 + 150 + 0; len(repo.inserted) != want {
		t.Fatalf("total inserted = %d, want %d", len(repo.inserted), want)
	}

	// IDEMPOTENCY: a second FULL run against the same (already populated)
	// repository must import exactly 0 — the sample re-derives identically
	// from the files and every normalized text is already present.
	rowsBefore := len(repo.inserted)
	secondRun, err := importer.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantDup := map[string]int{"general": 300, "music": 5, "newest": 150, "world": 1}
	for _, st := range secondRun {
		if st.Imported != 0 || st.Duplicates != wantDup[st.Category] {
			t.Fatalf("second run %s: imported=%d duplicates=%d, want 0/%d",
				st.Category, st.Imported, st.Duplicates, wantDup[st.Category])
		}
	}
	if len(repo.inserted) != rowsBefore {
		t.Fatalf("second run inserted %d new rows", len(repo.inserted)-rowsBefore)
	}
}

func validUTF8Row(q models.Question) bool {
	return utf8valid(q.Question) && strings.Contains(q.Options, `"`)
}
