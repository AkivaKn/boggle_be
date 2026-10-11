package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"boggle-api/internal/millionaire/models"
	"boggle-api/internal/millionaire/repository"
)

// This file is the M6 labeling pipeline (SPEC §2 question engine v2, point
// 2 — "Strength = agent-rated"): batch files the ORCHESTRATOR hands to
// labeling agents, the agents' JSONL answers, validation, DB writes, blind
// double-rating audits and coverage reports. The pipeline itself NEVER
// runs agents — cmd/label only orchestrates files.
//
// LAYOUT (state dir, default ./label — gitignored):
//
//	label/rubric.md                  authoritative rubric (written on every run)
//	label/state.jsonl                append-only pipeline state (see below)
//	label/batch_<id>.txt             exported batch (rubric header + questions)
//	label/batch_<id>.rated.jsonl     agent answers for a real batch
//	label/batch_<id>.audit.txt       exported blind audit batch
//	label/batch_<id>.audit.rated.jsonl  audit answers (NO strength write)
//
// state.jsonl events (append-only; a rollback REWRITES the file without the
// batch's lines):
//
//	{"kind":"batch", ...}   batch created: {batch_id, audit, batch_file,
//	                        question_ids[], exported_at}
//	{"kind":"rating", ...}  one validated rating: {batch_id, question_id,
//	                        rating, sampled_at, rater, batch_file}
//	                        — audit lines additionally carry `canonical`
//	                        (the DB strength at import time)
//	{"kind":"settled"...}   {batch_id, status: done|rolled_back, settled_at}
//
// Ratings are the pipeline's source of truth for bookkeeping, the DB
// strength column is the source of truth for SELECTION.
const (
	// labelBatchSize is the standard batch (SPEC §2 point 2: ~100 per batch).
	labelBatchSize = 100
	// labelBatchCap bounds the -export N flag — batches of ≤102 lines keep
	// the JSONL ingest validated and agents sane.
	labelBatchCap = 102

	labelStateFileName = "state.jsonl"
	labelRubricFile    = "rubric.md"

	// priorityBatchCap is the larger cap for PRIORITY/filtered exports
	// (M10 labels-first kickoffs import up to 200 rows in one file); the
	// standard ~100-per-batch cap keeps governing unfiltered exports.
	priorityBatchCap = 200

	raterCLI   = "cli"            // CLI-written demo ratings
	raterAgent = "labeling-agent" // real batch ratings
	raterAudit = "audit"          // blind re-ratings
)

// LabelStateEvent is one state.jsonl line (both write and read side).
type LabelStateEvent struct {
	Kind        string   `json:"kind"` // batch | rating | settled
	BatchID     int      `json:"batch_id"`
	Audit       bool     `json:"audit,omitempty"`
	BatchFile   string   `json:"batch_file,omitempty"`
	Status      string   `json:"status,omitempty"` // settled: done | rolled_back
	QuestionIDs []string `json:"question_ids,omitempty"`

	// rating lines:
	QuestionID string    `json:"question_id,omitempty"`
	Rating     int       `json:"rating,omitempty"`
	Canonical  int       `json:"canonical,omitempty"` // audit rating lines only
	Rater      string    `json:"rater,omitempty"`
	SampledAt  time.Time `json:"sampled_at,omitempty"`

	ExportedAt time.Time `json:"exported_at,omitempty"` // batch lines
	SettledAt  time.Time `json:"settled_at,omitempty"`  // settled lines
}

// LabelService carries the pipeline operations over one state dir + DB.
type LabelService struct {
	repo repository.LabelRepository
	dir  string
}

func NewLabelService(repo repository.LabelRepository, stateDir string) *LabelService {
	return &LabelService{repo: repo, dir: stateDir}
}

func (s *LabelService) statePath() string  { return filepath.Join(s.dir, labelStateFileName) }
func (s *LabelService) rubricPath() string { return filepath.Join(s.dir, labelRubricFile) }

// writeRubric keeps label/rubric.md identical to RubricV1 on every run.
// Creates the state dir if needed (a first export must not require it).
func (s *LabelService) writeRubric() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.rubricPath(), []byte(RubricV1), 0o644)
}

// loadState reads + parses the whole event log. A missing file = empty
// state (export creates the pipeline's first batch).
func (s *LabelService) loadState() ([]LabelStateEvent, error) {
	data, err := os.ReadFile(s.statePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var events []LabelStateEvent
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev LabelStateEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("corrupt state line %q: %w", line, err)
		}
		events = append(events, ev)
	}
	return events, nil
}

func (s *LabelService) appendEvents(events []LabelStateEvent) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.statePath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := func(ev LabelStateEvent) error {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		_, err = f.Write(append(b, '\n'))
		return err
	}
	for _, ev := range events {
		if err := w(ev); err != nil {
			return err
		}
	}
	return nil
}

// nextBatchID: monotonic over history (rolled-back batches release their
// QUESTION ids but never their batch number — numbering never reuses).
func nextBatchID(events []LabelStateEvent) int {
	max := 0
	for _, ev := range events {
		if ev.BatchID > max {
			max = ev.BatchID
		}
	}
	return max + 1
}

// batchStatus returns done | rolled_back | open (later settled events win).
func batchStatus(events []LabelStateEvent, id int) string {
	status := "open"
	for _, ev := range events {
		if ev.Kind == "settled" && ev.BatchID == id {
			status = ev.Status
		}
	}
	return status
}

func batchOf(events []LabelStateEvent, id int) *LabelStateEvent {
	var batch *LabelStateEvent
	for i, ev := range events {
		if ev.Kind == "batch" && ev.BatchID == id {
			batch = &events[i]
		}
	}
	return batch
}

// batchByFile finds a batch event by its recorded batch FILE name (base
// name with extension, e.g. batch_priority_1.txt) — the NAMED priority
// export convention (M10): their agents return <name>.rated.jsonl and the
// state file carries the file→(batch id) mapping.
func batchByFile(events []LabelStateEvent, fileName string) *LabelStateEvent {
	for i, ev := range events {
		if ev.Kind == "batch" && ev.BatchFile == fileName {
			return &events[i]
		}
	}
	return nil
}

// Export writes the next unrated batch (no priority filters): see
// ExportFiltered — this wrapper keeps the M6-API callers/test shape.
func (s *LabelService) Export(ctx context.Context, n int) (string, int, error) {
	path, count, _, err := s.ExportFiltered(ctx, n, ExportFilter{})
	return path, count, err
}

// ExportFilter carries the M10 priority-filter options for the labels-first
// easy-band batches (v3-ux-plan "Question quality" S1/S2):
//
//   - Source: "" / "any" = both corpora (unchanged M6 behavior);
//     "opentdb" | "otqa" restrict the pool's source column.
//   - CategoriesIn / CategoriesEx: category include/exclude TOKENS (exact or
//     substring match on questions.category — e.g. "General Knowledge,
//     Geography,Sports,History" for opentdb, "OTQA:music" for otqa).
//   - Out: optional custom batch base name for NAMED priority exports
//     (e.g. "batch_priority_1" produces batch_priority_1.txt whose agents
//     return batch_priority_1.rated.jsonl — import/rollback resolve the
//     name against the recorded state). Empty = standard batch_<NNN>.txt
//     numbering.
type ExportFilter struct {
	Source       string
	CategoriesIn []string
	CategoriesEx []string
	Out          string
}

var priorityNameRe = regexp.MustCompile(`^batch_[a-z0-9_]+$`)

// priorityMode reports whether the batch is a priority/filtered one: only
// then may it exceed the standard ~100 batch cap (up to priorityBatchCap),
// because a priority export is a curated one-shot kickoff file, while
// regular consecutive exports stay ≤102 for agent sanity.
func (f ExportFilter) priorityMode() bool {
	return f.Out != "" ||
		(f.Source != "" && f.Source != "any") ||
		len(f.CategoriesIn) > 0 || len(f.CategoriesEx) > 0
}

// ExportFiltered writes the next unrated batch through the optional
// priority filters. Resumability semantics are UNCHANGED (the pool is
// `strength IS NULL ORDER BY id`, so consecutive exports — filtered or
// not — never overlap). When any filter or a custom Out name is given:
//   - the batch may hold up to priorityBatchCap questions;
//   - the export ECHOES the batch breakdown (per-source + per-category
//     counts) both into the batch file's ASSIGNMENT section (so the
//     labeling agent sees its priors) and to the caller (cmd/label prints
//     it).
func (s *LabelService) ExportFiltered(ctx context.Context, n int, f ExportFilter) (string, int, []string, error) {
	if err := s.writeRubric(); err != nil {
		return "", 0, nil, err
	}
	if n <= 0 {
		n = labelBatchSize
	}
	maxN := labelBatchCap
	if f.priorityMode() {
		maxN = priorityBatchCap
	}
	if n > maxN {
		return "", 0, nil, fmt.Errorf("batch size %d exceeds the %d cap", n, maxN)
	}
	if f.Source != "" && f.Source != "any" && f.Source != "opentdb" && f.Source != "otqa" {
		return "", 0, nil, fmt.Errorf("unknown source filter %q (use any | opentdb | otqa)", f.Source)
	}
	if f.Out != "" && !priorityNameRe.MatchString(f.Out) {
		return "", 0, nil, fmt.Errorf("batch out name %q does not match batch_<lowercase-id>", f.Out)
	}

	events, err := s.loadState()
	if err != nil {
		return "", 0, nil, err
	}
	if f.Out != "" {
		if prior := batchByFile(events, f.Out+".txt"); prior != nil {
			return "", 0, nil, fmt.Errorf("%s.txt was already exported (batch %d) — use a fresh name for the next priority batch", f.Out, prior.BatchID)
		}
	}

	var questions []models.Question
	if f.Source == "" || f.Source == "any" {
		if len(f.CategoriesIn) == 0 && len(f.CategoriesEx) == 0 {
			// Unfiltered: plain ListUnrated (exact M6 query semantics).
			questions, err = s.repo.ListUnrated(ctx, n)
		} else {
			questions, err = s.repo.ListUnratedFiltered(ctx, n, "", f.CategoriesIn, f.CategoriesEx)
		}
	} else {
		questions, err = s.repo.ListUnratedFiltered(ctx, n, f.Source, f.CategoriesIn, f.CategoriesEx)
	}
	if err != nil {
		return "", 0, nil, fmt.Errorf("listing unrated questions: %w", err)
	}
	if len(questions) == 0 {
		if f.priorityMode() {
			return "", 0, nil, fmt.Errorf("no unrated questions match the priority filters (source=%s, in=%v, out=%v)", f.Source, f.CategoriesIn, f.CategoriesEx)
		}
		return "", 0, nil, fmt.Errorf("no unrated questions left — the corpus is fully rated (re-run cmd/label -coverage)")
	}

	id := nextBatchID(events)
	name := fmt.Sprintf("batch_%03d.txt", id)
	returnFile := fmt.Sprintf("batch_%03d.rated.jsonl", id)
	if f.Out != "" {
		name = f.Out + ".txt"
		returnFile = f.Out + ".rated.jsonl"
	}
	path := filepath.Join(s.dir, name)
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return "", 0, nil, err
	}
	breakdown := exportBreakdown(questions)
	body, err := renderBatch(RubricV1, name, returnFile, questions, false, breakdown...)
	if err != nil {
		return "", 0, nil, err
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", 0, nil, err
	}
	if err := os.WriteFile(s.rubricPath(), []byte(RubricV1), 0o644); err != nil {
		return "", 0, nil, err
	}

	ids := make([]string, 0, len(questions))
	for _, q := range questions {
		ids = append(ids, q.ID)
	}
	if err := s.appendEvents([]LabelStateEvent{{
		Kind:        "batch",
		BatchID:     id,
		Audit:       false,
		BatchFile:   name,
		QuestionIDs: ids,
		ExportedAt:  time.Now().UTC(),
	}}); err != nil {
		return "", 0, nil, err
	}
	if f.priorityMode() {
		log.Printf("label: exported %s with %d questions (priority filters: source=%s, in=%v, out=%v) — hand to a labeling agent, then cmd/label -import %s",
			name, len(questions), f.Source, f.CategoriesIn, f.CategoriesEx, returnFile)
	} else {
		log.Printf("label: exported batch_%03d.txt with %d questions (NOT yet rated — hand to a labeling agent, then cmd/label -import batch_%03d.rated.jsonl)", id, len(questions), id)
	}
	return path, len(questions), breakdown, nil
}

// exportBreakdown computes the exported batch's composition (per source +
// per category counts) — sources first (alphabetical), then categories
// (alphabetical); the same lines travel in the batch file so agent prompts
// stay self-contained.
func exportBreakdown(questions []models.Question) []string {
	sources := map[string]int{}
	categories := map[string]int{}
	for _, q := range questions {
		sources[q.Source]++
		categories[q.Category]++
	}
	var srcNames, catNames []string
	for s := range sources {
		srcNames = append(srcNames, s)
	}
	for c := range categories {
		catNames = append(catNames, c)
	}
	sort.Strings(srcNames)
	sort.Strings(catNames)

	lines := []string{
		fmt.Sprintf("Batch breakdown: %d questions selected (pipeline unrated pool, ORDER BY id).",
			len(questions)),
	}
	for _, s := range srcNames {
		lines = append(lines, fmt.Sprintf("  source %s: %d", s, sources[s]))
	}
	for _, c := range catNames {
		lines = append(lines, fmt.Sprintf("  category %s: %d", c, categories[c]))
	}
	return lines
}

// ExportAudit re-emits K already-rated questions as a BLIND audit batch:
// the stored ratings are never shown in the file, the answers are marked
// rater='audit' and recorded against the canonical strength WITHOUT
// touching the DB (double-rating drift stats flow through -audit-report).
func (s *LabelService) ExportAudit(ctx context.Context, k int) (string, int, error) {
	if err := s.writeRubric(); err != nil {
		return "", 0, err
	}
	if k <= 0 || k > labelBatchCap {
		return "", 0, fmt.Errorf("audit batch size must be 1..%d", labelBatchCap)
	}
	events, err := s.loadState()
	if err != nil {
		return "", 0, err
	}
	questions, err := s.repo.ListRatedSample(ctx, k)
	if err != nil {
		return "", 0, fmt.Errorf("sampling rated questions for audit: %w", err)
	}
	if len(questions) == 0 {
		return "", 0, fmt.Errorf("no rated questions yet — run real batches first")
	}
	id := nextBatchID(events)
	name := fmt.Sprintf("batch_%03d.audit.txt", id)
	path := filepath.Join(s.dir, name)
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return "", 0, err
	}
	body, err := renderBatch(RubricV1, name, fmt.Sprintf("batch_%03d.audit.rated.jsonl", id), questions, true)
	if err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", 0, err
	}
	ids := make([]string, 0, len(questions))
	for _, q := range questions {
		ids = append(ids, q.ID)
	}
	if err := s.appendEvents([]LabelStateEvent{{
		Kind:        "batch",
		BatchID:     id,
		Audit:       true,
		BatchFile:   name,
		QuestionIDs: ids,
		ExportedAt:  time.Now().UTC(),
	}}); err != nil {
		return "", 0, err
	}
	log.Printf("label: exported AUDIT batch_%03d.audit.txt (%d questions, blind) — have a DIFFERENT agent rate it, then cmd/label -import label/batch_%03d.audit.rated.jsonl", id, len(questions), id)
	return path, len(questions), nil
}

// renderBatch renders one batch file: the rubric verbatim, the assignment
// block (blind for audits — the stored rating must not be visible; extra
// `notes` lines (the exported batch breakdown) travel right below the
// assignment) and the numbered questions with their ids.
func renderBatch(rubric, fileName, returnName string, questions []models.Question, blind bool, notes ...string) (string, error) {
	var sb strings.Builder
	sb.WriteString(rubric)
	sb.WriteString("\n=== ASSIGNMENT ===\n")
	fmt.Fprintf(&sb, "Batch file: %s (%d questions)\n", fileName, len(questions))
	if blind {
		fmt.Fprintf(&sb, "Return file: %s — AUDIT (blind re-rate): the questions already carry stored ratings; rate them FRESH from the rubric, do not attempt to match the stored ones (you cannot see them).\n", returnName)
	} else {
		fmt.Fprintf(&sb, "Return file: %s\n", returnName)
	}
	for _, note := range notes {
		sb.WriteString(note)
		sb.WriteString("\n")
	}
	sb.WriteString("Output: JSON Lines, one object per question:\n")
	sb.WriteString(`  {"question_id": "<the id shown below>", "rating": <1-15>}`)
	sb.WriteString("\nCover EVERY question in the batch exactly once. Do not alter the ids.\n")
	sb.WriteString("\n=== QUESTIONS ===\n")
	for i, q := range questions {
		var options []string
		if err := json.Unmarshal([]byte(q.Options), &options); err != nil || len(options) != 4 {
			return "", fmt.Errorf("question %s options malformed: %w", q.ID, err)
		}
		fmt.Fprintf(&sb, "\nQ-%02d id=%s\n%s\n", i+1, q.ID, strings.TrimSpace(q.Question))
		fmt.Fprintf(&sb, "  options: %s\n", strings.Join(options, " | "))
	}
	return sb.String(), nil
}

// ratedLine is one expected line of an agent's rated JSONL file.
type ratedLine struct {
	QuestionID string `json:"question_id"`
	Rating     int    `json:"rating"`
	Rater      string `json:"rater,omitempty"` // optional override; CLI defaults it
}

var validRaterNames = map[string]bool{raterAgent: true, raterCLI: true, raterAudit: true}

// ratedFileRe parses the return-file name convention:
// batch_<id>.rated.jsonl or batch_<id>.audit.rated.jsonl.
var ratedFileRe = regexp.MustCompile(`^batch_(\d+)(?:\.audit)?\.rated\.jsonl$`)

// namedPriorityBatchFile reports whether a recorded batch file is an M10
// NAMED priority export (batch_<name>.txt with a non-digit-leading name —
// the -out convention). Named exports only exist in priority mode
// (ExportFilter.priorityMode is implied by Out != ""), so their return
// files may carry up to priorityBatchCap rows. Numbered batches (legacy
// exports or filtered exports without -out) and AUDIT batches return
// false.
func namedPriorityBatchFile(batchFile string) bool {
	stem := strings.TrimSuffix(batchFile, ".txt")
	if stem == batchFile || !strings.HasPrefix(stem, "batch_") || len(stem) == len("batch_") {
		return false
	}
	if c := stem[len("batch_")]; c >= '0' && c <= '9' {
		return false // numbered (legacy or audit) batches
	}
	return priorityNameRe.MatchString(stem)
}

// capFor returns the line-count cap an import of the given batch must
// satisfy — derived per batch kind, i.e. per the export contract the
// batch came from:
//   - NAMED priority batches (batch_<name>.txt, the M10 -out convention):
//     priorityBatchCap — a named export is always a priority export;
//   - NUMBERED priority batches (filters without -out) are recognizable
//     only through the state record: a batch recorded with more than
//     labelBatchCap questions can only come from a priority export, so
//     its return file gets priorityBatchCap too;
//   - legacy numbered batches and AUDIT batches keep labelBatchCap
//     (their export contracts never exceed it — ExportAudit bounds K by
//     labelBatchCap).
//
// batch may be nil (name not yet resolved): such names keep the legacy
// cap.
func capFor(batchName string, batch *LabelStateEvent) int {
	if namedPriorityBatchFile(batchName) || (batch != nil && len(batch.QuestionIDs) > labelBatchCap) {
		return priorityBatchCap
	}
	return labelBatchCap
}

// Import validates an agent's rated JSONL against its batch and applies
// it:
//   - ratings must be integers 1..15 (json floats fail the decode);
//   - every question_id must exist AND belong to the batch;
//   - FULL batch coverage is required (size ≤ the batch's own cap —
//     capFor derives it per batch kind: priority exports 200, legacy
//     numbered and audit batches 102 — validated against the recorded
//     batch); duplicates in the file are rejected;
//   - a real batch writes questions.strength and settles the batch;
//   - an AUDIT batch records audit rating lines (with the canonical
//     strength embedded) and settles — it NEVER writes the DB.
//
// Idempotent: re-importing a settled batch is a reported no-op (exit 0).
func (s *LabelService) Import(ctx context.Context, ratedPath string) (int, error) {
	if err := s.writeRubric(); err != nil {
		return 0, err
	}
	base := filepath.Base(ratedPath)
	m := ratedFileRe.FindStringSubmatch(base)
	named := false
	if m == nil {
		// M10 named priority batches: batch_<lowercase>.rated.jsonl
		// resolves against the state file's recorded BatchFile. The
		// numeric convention keeps parsing directly from the id.
		stem := strings.TrimSuffix(base, ".rated.jsonl")
		if m == nil && stem != base &&
			len(stem) > len("batch_") && strings.HasPrefix(stem, "batch_") {
			bn := strings.TrimPrefix(stem, "batch_")
			isLowerName := true
			for _, r := range bn {
				if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
					isLowerName = false
					break
				}
			}
			if isLowerName {
				first := bn[0]
				named = !(first >= '0' && first <= '9') // numeric names go through ratedFileRe only
			}
		}
		if !named {
			return 0, fmt.Errorf("%s does not match batch_<id>.rated.jsonl or batch_<id>.audit.rated.jsonl", base)
		}
	}
	var batchID int
	if m != nil {
		fmt.Sscanf(m[1], "%d", &batchID)
	}

	events, err := s.loadState()
	if err != nil {
		return 0, err
	}
	if named {
		b := batchByFile(events, strings.TrimSuffix(base, ".rated.jsonl")+".txt")
		if b == nil {
			return 0, fmt.Errorf("%s was never exported by this pipeline — import the file against the state dir that exported it", base)
		}
		batchID = b.BatchID
	}
	batch := batchOf(events, batchID)
	if batch == nil {
		return 0, fmt.Errorf("batch %d was never exported by this pipeline — import the file against the state dir that exported it", batchID)
	}
	switch status := batchStatus(events, batchID); status {
	case "done":
		log.Printf("label: batch_%03d already imported (settled) — no-op", batchID)
		return 0, nil
	case "rolled_back":
		return 0, fmt.Errorf("batch_%03d was rolled back and must not be imported", batchID)
	default: // open
	}

	lines, err := readRatedFile(ratedPath)
	if err != nil {
		return 0, err
	}
	maxLines := capFor(batch.BatchFile, batch)
	if len(lines) > maxLines {
		return 0, fmt.Errorf("rated file has %d lines, batch cap is %d", len(lines), maxLines)
	}
	batchIDs := map[string]bool{}
	for _, id := range batch.QuestionIDs {
		batchIDs[id] = true
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if l.QuestionID == "" {
			return 0, fmt.Errorf("rated line missing question_id: %v", l)
		}
		if l.Rating < 1 || l.Rating > 15 {
			return 0, fmt.Errorf("%s: rating %d out of range 1..15", l.QuestionID, l.Rating)
		}
		if !batchIDs[l.QuestionID] {
			return 0, fmt.Errorf("%s: question does not belong to batch_%03d", l.QuestionID, batchID)
		}
		if seen[l.QuestionID] {
			return 0, fmt.Errorf("%s: duplicate rating for the same question", l.QuestionID)
		}
		seen[l.QuestionID] = true
	}
	if missing := len(batch.QuestionIDs) - len(seen); missing > 0 {
		return 0, fmt.Errorf("batch_%03d incomplete: %d of %d questions rated (full-coverage import required — recollect from the agent)", batchID, len(seen), len(batch.QuestionIDs))
	}

	// Validated. Resolve the batch's CURRENT canonical strengths (audit
	// lines embed them; real batches write them to the DB).
	idOrder := append([]string(nil), batch.QuestionIDs...)
	questions, err := s.repo.ListByID(ctx, idOrder)
	if err != nil {
		return 0, fmt.Errorf("reading batch questions: %w", err)
	}
	byID := map[string]models.Question{}
	for _, q := range questions {
		byID[q.ID] = q
	}

	sampleAt := time.Now().UTC()
	var updates []LabelStateEvent
	for _, r := range lines {
		rater := r.Rater
		if rater == "" {
			if batch.Audit {
				rater = raterAudit
			} else {
				rater = raterAgent
			}
		}
		if !validRaterNames[rater] {
			return 0, fmt.Errorf("%s: unknown rater %q (use %s | %s | %s)", r.QuestionID, rater, raterAgent, raterCLI, raterAudit)
		}
		if batch.Audit {
			q, ok := byID[r.QuestionID]
			if !ok {
				return 0, fmt.Errorf("%s: audit batch references a vanished question", r.QuestionID)
			}
			updates = append(updates, s.auditRatingEvent(batch, r.QuestionID, r.Rating, canonicalOf(q), rater, sampleAt))
		} else {
			updates = append(updates, LabelStateEvent{
				Kind:       "rating",
				BatchID:    batch.BatchID,
				BatchFile:  batch.BatchFile,
				QuestionID: r.QuestionID,
				Rating:     r.Rating,
				Rater:      rater,
				SampledAt:  sampleAt,
			})
		}
	}

	if !batch.Audit {
		// DB write before the state append: a failure here leaves the
		// batch open and re-running the import is harmless (SetStrength
		// is idempotent — same value, and the validations bind the write
		// to the batch's own ids).
		sort.Strings(idOrder)
		for _, id := range idOrder {
			rating := ratingOf(updates, id)
			if err := s.repo.SetStrength(ctx, id, rating); err != nil {
				return 0, fmt.Errorf("writing strength for %s: %w", id, err)
			}
		}
	}
	if err := s.appendEvents(updates); err != nil {
		return 0, err
	}
	if err := s.appendEvents([]LabelStateEvent{{
		Kind:      "settled",
		Status:    "done",
		BatchID:   batch.BatchID,
		SettledAt: time.Now().UTC(),
	}}); err != nil {
		return 0, err
	}
	action := "imported (rates written to questions.strength)"
	if batch.Audit {
		action = "imported (AUDIT ratings recorded, DB untouched)"
	}
	log.Printf("label: batch_%03d %s — %d ratings", batchID, action, len(lines))
	return len(lines), nil
}

func canonicalOf(q models.Question) int {
	if q.Strength == nil {
		return 0
	}
	return *q.Strength
}

func ratingOf(events []LabelStateEvent, questionID string) int {
	for _, ev := range events {
		if ev.Kind == "rating" && ev.QuestionID == questionID {
			return ev.Rating
		}
	}
	return 0
}

func (s *LabelService) auditRatingEvent(batch *LabelStateEvent, questionID string, rating, canonical int, rater string, at time.Time) LabelStateEvent {
	return LabelStateEvent{
		Kind:       "rating",
		BatchID:    batch.BatchID,
		BatchFile:  batch.BatchFile,
		QuestionID: questionID,
		Rating:     rating,
		Canonical:  canonical,
		Rater:      rater,
		SampledAt:  at,
	}
}

// readRatedFile decodes a rated JSONL file (tolerating blank lines).
func readRatedFile(path string) ([]ratedLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var lines []ratedLine
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var l ratedLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			return nil, fmt.Errorf("rated file line %d is not a valid {question_id, rating} object: %w", i+1, err)
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("rated file is empty")
	}
	return lines, nil
}

// AuditReport compares every AUDIT rating against its canonical strength:
// same-band % (exact equality) and ±1 band %. Useful drift output needs a
// reasonable sample (SPEC §2: ~10% re-rated blind).
func (s *LabelService) AuditReport(ctx context.Context) error {
	events, err := s.loadState()
	if err != nil {
		return err
	}
	batches := map[int]*LabelStateEvent{}
	for i, ev := range events {
		if ev.Kind == "batch" && ev.Audit {
			batches[ev.BatchID] = &events[i]
		}
	}
	type pair struct{ canonical, rating int }
	var pairs []pair
	var open int
	for _, ev := range events {
		if ev.Kind != "rating" || ev.Canonical == 0 {
			continue
		}
		if batches[ev.BatchID] == nil {
			continue // non-audit line carrying canonical — impossible, be strict
		}
		pairs = append(pairs, pair{ev.Canonical, ev.Rating})
	}
	// batches exported but not yet imported:
	for _, ev := range events {
		if ev.Kind == "batch" && ev.Audit && batchStatus(events, ev.BatchID) == "open" {
			open++
		}
	}
	if len(pairs) == 0 {
		return fmt.Errorf("no audit ratings in %s yet (%d open audit batches) — export a blind batch, have an agent rate it, then -import it", s.statePath(), open)
	}
	same, near, off := 0, 0, 0
	for _, p := range pairs {
		d := p.canonical - p.rating
		if d < 0 {
			d = -d
		}
		switch {
		case d == 0:
			same++
		case d <= 1:
			near++
		default:
			off++
		}
	}
	total := len(pairs)
	fmt.Printf("audit agreement: %d pairs, same band %d (%.1f%%), within ±1 band %d (%.1f%%), drifted >1 %d (%.1f%%) [gate: same+±1 ≥ 75%% per SPEC §2]\n",
		total, same, pct(same, total), same+near, pct(same+near, total), off, pct(off, total))
	return nil
}

func pct(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

// Coverage prints the per-band import/selection statistics (per-band rated
// counts + cultural share — the seeder's per-TIER minimums shift to
// per-BAND coverage with v2).
func (s *LabelService) Coverage(ctx context.Context) error {
	unrated, rated, err := s.counts(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("coverage: %d rated, %d unrated of the corpus; cultural share limited per room by the selection cap (≤30%%, L1–3 non-cultural)\n", rated, unrated)
	if rated == 0 {
		fmt.Println("no rated questions yet — export batches and run the agents")
		return nil
	}
	bands, err := s.repo.BandCoverage(ctx)
	if err != nil {
		return fmt.Errorf("band coverage: %w", err)
	}
	fmt.Println("band  rated  cultural  cultural%")
	for _, b := range bands {
		fmt.Printf("%4d %6d %9d %10.1f%%\n", b.Band, b.Total, b.CulturalTotal, pct(b.CulturalTotal, b.Total))
	}
	return nil
}

func (s *LabelService) counts(ctx context.Context) (unrated, rated int, err error) {
	unrated, err = s.repo.CountUnrated(ctx)
	if err != nil {
		return 0, 0, err
	}
	rated, err = s.repo.CountRated(ctx)
	if err != nil {
		return 0, 0, err
	}
	return unrated, rated, nil
}

// RollbackBatch reverses a batch: real batches RESET questions.strength
// to NULL for all its ids; the batch's lines are DELETED from state.jsonl
// (the exported batch file is removed too). Used after the end-to-end demo
// (and for genuine mistakes) so the pipeline stays unpolluted. The name
// may be the base id ("batch_001"), the full batch name or the rated file
// name — anything that encodes the batch id.
func (s *LabelService) RollbackBatch(ctx context.Context, batchFile string) error {
	base := filepath.Base(strings.TrimSpace(batchFile))
	m := regexp.MustCompile(`^batch_(\d+)(?:\.audit)?(?:\.txt|\.rated\.jsonl|\.audit\.rated\.jsonl)?$`).FindStringSubmatch(base)
	var batchID int
	if m != nil {
		fmt.Sscanf(m[1], "%d", &batchID)
	}

	events, err := s.loadState()
	if err != nil {
		return err
	}

	// M10 NAMED priority batches resolve by their recorded batch file:
	// accept "batch_priority_1" / ".txt" / ".rated.jsonl" spellings.
	if m == nil {
		stem := strings.TrimSuffix(base, ".rated.jsonl")
		stem = strings.TrimSuffix(stem, ".txt")
		ok := stem != "" && strings.HasPrefix(stem, "batch_")
		if ok {
			for _, r := range strings.TrimPrefix(stem, "batch_") {
				if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
					ok = false
					break
				}
			}
		}
		if !ok {
			return fmt.Errorf("%s does not look like a batch file name", base)
		}
		b := batchByFile(events, stem+".txt")
		if b == nil {
			return fmt.Errorf("%s is not recorded in this state dir", stem)
		}
		batchID = b.BatchID
	}

	batch := batchOf(events, batchID)
	if batch == nil {
		return fmt.Errorf("batch_%03d is not recorded in this state dir", batchID)
	}

	if !batch.Audit {
		for _, id := range batch.QuestionIDs {
			if err := s.repo.ResetStrength(ctx, id); err != nil {
				return fmt.Errorf("resetting strength for %s: %w", id, err)
			}
		}
	}

	// Rewrite state.jsonl without every event of this batch.
	var kept []string
	for _, ev := range events {
		if ev.BatchID == batchID {
			continue
		}
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		kept = append(kept, string(b))
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	if len(kept) == 0 {
		err = os.Remove(s.statePath())
	} else {
		err = os.WriteFile(s.statePath(), []byte(strings.Join(kept, "\n")+"\n"), 0o644)
	}
	if err != nil {
		return err
	}

	// Remove the exported artifact best-effort (it references an undone
	// batch; a stale .txt could trick a labeling agent).
	_ = os.Remove(filepath.Join(s.dir, batch.BatchFile))
	log.Printf("label: batch_%03d rolled back (%d strength columns reset, state lines removed)", batchID, len(batch.QuestionIDs))
	return nil
}
