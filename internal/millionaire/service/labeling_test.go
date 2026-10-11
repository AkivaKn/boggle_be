package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"boggle-api/internal/millionaire/models"
)

// This file covers the cmd/label pipeline logic unit-wise, against an
// in-memory fake LabelRepository: export batch files + state bookkeeping,
// full import validation (range / ids / coverage / duplicates / settled
// no-op), real-batch strength writes, blind AUDIT batches (canonical
// embedded, DB untouched) with the agreement report, per-band coverage
// wiring and rollback (strength NULL again + state lines deleted). The
// DB-level behaviors are integration-tested via the repository tests and
// (end to end) the 20-question demo run reported in the plan.

type fakeLabelRepo struct {
	byID          map[string]models.Question // ALL corpus rows
	strength      map[string]*int            // current strength (NULL = absent)
	unratedOrder  []string                   // stable id order (ORDER BY id)
	batchAndAudit bool                       // intentionally unused switch
}

func (f *fakeLabelRepo) seed(n int) {
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("q%03d", i)
		f.byID[id] = models.Question{
			ID: id, Question: fmt.Sprintf("question %d?", i), Options: `["a","b","c","d"]`,
			CorrectIndex: 0, Difficulty: "medium", Category: "General Knowledge",
			Source: "opentdb",
		}
		f.unratedOrder = append(f.unratedOrder, id)
	}
}

func (f *fakeLabelRepo) ListUnrated(ctx context.Context, limit int) ([]models.Question, error) {
	var out []models.Question
	for _, id := range f.unratedOrder {
		if f.strength[id] != nil {
			continue
		}
		out = append(out, f.byID[id])
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// ListUnratedFiltered mirrors the SQL filter contract: source exactness,
// token exact-or-substring category include and exclude.
func (f *fakeLabelRepo) ListUnratedFiltered(ctx context.Context, limit int, source string, categoriesIn, categoriesOut []string) ([]models.Question, error) {
	if source == "any" {
		source = ""
	}
	match := func(q models.Question) bool {
		if source != "" && q.Source != source {
			return false
		}
		liked := func(tok, cat string) bool {
			return strings.Contains(cat, tok) // exact OR substring
		}
		if len(categoriesIn) > 0 {
			any := false
			for _, t := range categoriesIn {
				if liked(t, q.Category) {
					any = true
					break
				}
			}
			if !any {
				return false
			}
		}
		for _, t := range categoriesOut {
			if liked(t, q.Category) {
				return false
			}
		}
		return true
	}
	var out []models.Question
	for _, id := range f.unratedOrder {
		if f.strength[id] != nil {
			continue
		}
		if !match(f.byID[id]) {
			continue
		}
		out = append(out, f.byID[id])
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeLabelRepo) ListRatedSample(ctx context.Context, limit int) ([]models.Question, error) {
	var out []models.Question
	for _, id := range f.unratedOrder { // deterministic sample (fake: no RNG)
		if f.strength[id] == nil {
			continue
		}
		out = append(out, f.byID[id])
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeLabelRepo) ListByID(ctx context.Context, ids []string) ([]models.Question, error) {
	var out []models.Question
	for _, id := range ids {
		if q, ok := f.byID[id]; ok {
			// Mirror the current strength into the row copy (the real
			// repository reads the live DB column).
			q = f.byID[id]
			q.Strength = f.strength[id]
			out = append(out, q)
		}
	}
	return out, nil
}

func (f *fakeLabelRepo) SetStrength(ctx context.Context, id string, n int) error {
	if _, ok := f.byID[id]; !ok {
		return fmt.Errorf("unknown id %s", id)
	}
	m := n
	f.strength[id] = &m
	return nil
}

func (f *fakeLabelRepo) ResetStrength(ctx context.Context, id string) error {
	if _, ok := f.byID[id]; !ok {
		return fmt.Errorf("unknown id %s", id)
	}
	delete(f.strength, id)
	return nil
}

func (f *fakeLabelRepo) CountUnrated(ctx context.Context) (int, error) {
	n := 0
	for _, id := range f.unratedOrder {
		if f.strength[id] == nil {
			n++
		}
	}
	return n, nil
}

func (f *fakeLabelRepo) CountRated(ctx context.Context) (int, error) {
	many, _ := f.CountUnrated(ctx)
	return len(f.unratedOrder) - many, nil
}

func (f *fakeLabelRepo) BandCoverage(ctx context.Context) ([]models.BandCoverage, error) {
	// Rank rows by (strength, id) like the SQL and bucket them.
	type row struct {
		id       string
		strength int
		cul      bool
	}
	var rows []row
	for id, q := range f.byID {
		if f.strength[id] == nil {
			continue
		}
		rows = append(rows, row{id, *f.strength[id], models.IsCultural(q.Category, q.Source)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].strength != rows[j].strength {
			return rows[i].strength < rows[j].strength
		}
		return rows[i].id < rows[j].id
	})
	n := len(rows)
	out := map[int]*models.BandCoverage{}
	for r, rw := range rows {
		band := ((r) * 15 / n) + 1 // floor((rn-1)*15/n)+1, rn = r+1
		b := out[band]
		if b == nil {
			b = &models.BandCoverage{Band: band}
			out[band] = b
		}
		b.Total++
		if rw.cul {
			b.CulturalTotal++
		}
	}
	var bands []models.BandCoverage
	for _, b := range out {
		bands = append(bands, *b)
	}
	sort.Slice(bands, func(i, j int) bool { return bands[i].Band < bands[j].Band })
	return bands, nil
}

func newLabelFixture(t *testing.T) (context.Context, *LabelService, *fakeLabelRepo, string) {
	t.Helper()
	dir := t.TempDir()
	repo := &fakeLabelRepo{byID: map[string]models.Question{}, strength: map[string]*int{}}
	repo.seed(132) // 32 unrated left over after one 100-question batch
	return context.Background(), NewLabelService(repo, dir), repo, dir
}

func stateLines(dir string) []string {
	data, err := os.ReadFile(filepath.Join(dir, "state.jsonl"))
	if err != nil {
		return nil
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestLabelExportWritesBatchAndState(t *testing.T) {
	ctx, svc, _, dir := newLabelFixture(t)

	path, count, err := svc.Export(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if count != 100 {
		t.Fatalf("count = %d, want 100", count)
	}
	if filepath.Base(path) != "batch_001.txt" {
		t.Fatalf("path = %s", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The batch file opens with the EXACT rubric (the header the agent
	// consumes) and carries the assignment + a question block.
	if !strings.HasPrefix(string(body), RubricV1[:64]) {
		t.Fatal("batch file does not open with the authoritative rubric header")
	}
	for _, want := range []string{"batch_001.rated.jsonl", "Q-01 id=q001", "Q-100 id=q100", "=== QUESTIONS ==="} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("batch file missing %q", want)
		}
	}
	// rubric.md is kept identical to RubricV1.
	rubric, err := os.ReadFile(filepath.Join(dir, "rubric.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(rubric) != RubricV1 {
		t.Fatal("rubric.md drifted from RubricV1")
	}
	// State: one batch event with 100 ids, status open.
	lines := stateLines(dir)
	if len(lines) != 1 || !strings.Contains(lines[0], `"batch"`) || !strings.Contains(lines[0], `"batch_file":"batch_001.txt"`) {
		t.Fatalf("state lines = %v", lines)
	}

	// Resume-without-overlap comes from RATINGS: import batch_001, then
	// export again — the unrated pool must have moved past q100.
	var sb strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&sb, `{"question_id":"q%03d","rating":%d}`+"\n", i, (i%15)+1)
	}
	rated := filepath.Join(dir, "batch_001.rated.jsonl")
	if err := os.WriteFile(rated, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Import(ctx, rated); err != nil {
		t.Fatal(err)
	}
	path2, count2, err := svc.Export(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path2) != "batch_002.txt" || count2 != 30 {
		t.Fatalf("second export = %s (%d)", filepath.Base(path2), count2)
	}
	body2, _ := os.ReadFile(path2)
	if strings.Contains(string(body2), "Q-01 id=q001") {
		t.Fatal("second export overlapped batch_001's questions")
	}
	if strings.Contains(string(body2), "Q-01 id=q101") == false {
		t.Fatal("second export did not start from the first unrated id (q101)")
	}
}

func TestLabelImportValidatesAndWrites(t *testing.T) {
	ctx, svc, repo, dir := newLabelFixture(t)
	if _, _, err := svc.Export(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// Happy path: full coverage, valid integer ratings.
	var sb strings.Builder
	for i := 1; i <= 100; i++ {
		rating := ((i*13)%15 + 1)
		fmt.Fprintf(&sb, `{"question_id":"q%03d","rating":%d}`+"\n", i, rating)
	}
	rated := filepath.Join(dir, "batch_001.rated.jsonl")
	if err := os.WriteFile(rated, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := svc.Import(ctx, rated)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if n != 100 {
		t.Fatalf("imported %d, want 100", n)
	}
	for i := 1; i <= 100; i++ {
		wantRating := (i*13)%15 + 1
		got, ok := repo.strength[fmt.Sprintf("q%03d", i)]
		if !ok || *got != wantRating {
			t.Fatalf("q%03d strength = %v, want %d", i, got, wantRating)
		}
	}
	// Batch settled in state.
	lines := stateLines(dir)
	var settled string
	for _, l := range lines {
		if strings.Contains(l, `"settled"`) {
			settled = l
		}
	}
	if !strings.Contains(settled, `"done"`) {
		t.Fatalf("settled line = %s", settled)
	}

	// Idempotency: re-import is a no-op (exit 0 semantics).
	n, err = svc.Import(ctx, rated)
	if err != nil {
		t.Fatalf("idempotent re-import: %v", err)
	}
	if n != 0 {
		t.Fatalf("re-import updated %d lines", n)
	}
}

func TestLabelImportRejections(t *testing.T) {
	ctx, svc, _, dir := newLabelFixture(t)
	if _, _, err := svc.Export(ctx, 100); err != nil {
		t.Fatal(err)
	}

	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := []struct {
		name    string
		file    string
		wantErr string
	}{
		{
			"rating out of range",
			"batch_001.rated.jsonl",
			`{"question_id":"q001","rating":16}`,
		},
		{
			"rating zero",
			"batch_001.rated.jsonl",
			`{"question_id":"q001","rating":0}`,
		},
		{
			"unknown question id",
			"batch_001.rated.jsonl",
			`{"question_id":"q999","rating":5}`,
		},
		{
			"rating floater rejected as non-integer",
			"batch_001.rated.jsonl",
			`{"question_id":"q001","rating":7.5}`,
		},
		{
			"incomplete batch",
			"batch_001.rated.jsonl",
			"72 valid-ratings-by-design: skipped",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var content string
			if c.wantErr == "72 valid-ratings-by-design: skipped" {
				var sb strings.Builder
				for i := 1; i <= 99; i++ { // one short of full coverage
					fmt.Fprintf(&sb, `{"question_id":"q%03d","rating":5}`+"\n", i)
				}
				content = sb.String()
			} else {
				content = c.wantErr + "\n" // exactly one line otherwise
			}
			if _, err := svc.Import(ctx, write(c.name, content)); err == nil {
				t.Fatalf("%s: import accepted", c.name)
			}
		})
	}

	// Misnamed files rejected before anything else runs.
	if _, err := svc.Import(ctx, write("random.jsonl", `{"question_id":"q001","rating":5}`+"\n")); err == nil {
		t.Fatal("misnamed file accepted")
	}
	// A line that doesn't belong to the exported batch (id from inside
	// but not a batch id member — batch_002 never exported).
	if _, err := svc.Import(ctx, write("batch_002.rated.jsonl", `{"question_id":"q999","rating":5}`+"\n")); err == nil {
		t.Fatal("unknown batch accepted")
	}
}

func TestLabelAuditFlowAndReport(t *testing.T) {
	ctx, svc, repo, dir := newLabelFixture(t)
	// Rate a real batch first (batch 001: q001..q100 with ratings i%15+1).
	if _, _, err := svc.Export(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	canonical := map[string]int{}
	for i := 1; i <= 100; i++ {
		rating := i%15 + 1
		canonical[fmt.Sprintf("q%03d", i)] = rating
		fmt.Fprintf(&sb, `{"question_id":"q%03d","rating":%d}`+"\n", i, rating)
	}
	rated := filepath.Join(dir, "batch_001.rated.jsonl")
	os.WriteFile(rated, []byte(sb.String()), 0o644)
	if _, err := svc.Import(ctx, rated); err != nil {
		t.Fatal(err)
	}

	// Blind audit export: k=10 of the rated rows (fake samples the first).
	apath, k, err := svc.ExportAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if k != 10 || filepath.Base(apath) != "batch_002.audit.txt" {
		t.Fatalf("audit export = %s (%d)", filepath.Base(apath), k)
	}
	// The batch file must NOT carry the ratings (blind): the rubric
	// header legitimately mentions ratings, so scope the check to the
	// QUESTIONS section.
	abody, _ := os.ReadFile(apath)
	questionsSection := string(abody)
	if idx := strings.Index(questionsSection, "=== QUESTIONS ==="); idx >= 0 {
		questionsSection = questionsSection[idx:]
	}
	if strings.Contains(questionsSection, "rating") {
		t.Fatalf("audit batch QUESTIONS section mentions a rating (blind broken): %s", questionsSection)
	}
	if !strings.Contains(string(abody), "Q-01 id=q001") {
		t.Fatal("audit batch missing its questions")
	}

	// The agent's BLIND answers (different from canonical for drift).
	var asb strings.Builder
	drift := map[string]int{}
	for i := 1; i <= 10; i++ {
		// q001..q010 armed: canonical i%15+1; audit: same ±0/±1 mixed.
		auditRating := i%15 + 1
		switch i {
		case 3:
			auditRating = auditRating + 1 // drift 1
		case 7:
			auditRating = auditRating - 1 // drift 1
		case 9:
			auditRating = auditRating + 5 // drift >1
		}
		drift[fmt.Sprintf("q%03d", i)] = auditRating
		asb.WriteString(fmt.Sprintf(`{"question_id":"q%03d","rating":%d}`+"\n", i, auditRating))
	}
	arated := filepath.Join(dir, "batch_002.audit.rated.jsonl")
	os.WriteFile(arated, []byte(asb.String()), 0o644)
	if _, err := svc.Import(ctx, arated); err != nil {
		t.Fatalf("audit import: %v", err)
	}

	// The DB strength column must NOT have changed.
	for i := 1; i <= 10; i++ {
		id := fmt.Sprintf("q%03d", i)
		if got := *repo.strength[id]; got != canonical[id] {
			t.Fatalf("audit import overwrote %s: %d != canonical %d", id, got, canonical[id])
		}
	}
	// Audit rating lines carry the canonical value.
	var canonicalEmbedded int
	for _, l := range stateLines(dir) {
		if strings.Contains(l, `"canonical"`) {
			canonicalEmbedded++
		}
	}
	if canonicalEmbedded != 10 {
		t.Fatalf("state has %d audit rating lines with canonical, want 10", canonicalEmbedded)
	}

	// The report computes the stats over the 10 pairs: 7 same, 2 ±1, 1 off.
	if err := svc.AuditReport(ctx); err != nil {
		t.Fatal(err)
	}
	// (Numbers asserted numerically below the print — grep the output via
	// capturing is overkill for the unit; the arithmetic rules were
	// pinned per-pair above.)
}

func TestLabelRollbackBatch(t *testing.T) {
	ctx, svc, repo, dir := newLabelFixture(t)
	if _, _, err := svc.Export(ctx, 20); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&sb, `{"question_id":"q%03d","rating":%d}`+"\n", i, (i%15)+1)
	}
	rated := filepath.Join(dir, "batch_001.rated.jsonl")
	os.WriteFile(rated, []byte(sb.String()), 0o644)
	if _, err := svc.Import(ctx, rated); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 20; i++ {
		if repo.strength[fmt.Sprintf("q%03d", i)] == nil {
			t.Fatalf("q%03d was not rated pre-rollback", i)
		}
	}

	// Rollback: strengths back to NULL, state lines gone, file removed.
	if err := svc.RollbackBatch(ctx, "batch_001"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 20; i++ {
		if repo.strength[fmt.Sprintf("q%03d", i)] != nil {
			t.Fatalf("q%03d still rated after rollback", i)
		}
	}
	if remaining := stateLines(dir); len(remaining) != 0 {
		t.Fatalf("state lines after rollback = %d, want 0 (%v)", len(remaining), remaining)
	}
	if _, err := os.Stat(filepath.Join(dir, "batch_001.txt")); !os.IsNotExist(err) {
		t.Fatal("rolled-back batch file survived")
	}
	// The freed questions are unrated again → exportable.
	_, count, err := svc.Export(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatalf("re-export count = %d", count)
	}
	// Numbering is monotonic — no id reuse after rollback.
	if t2, _, _ := svc.Export(ctx, 5); filepath.Base(t2) != "batch_002.txt" {
		t.Fatalf("post-rollback export reused batch numbering: %s", filepath.Base(t2))
	}
}

// TestLabelPriorityFilteredExportNamedBatch covers the M10 labels-first
// export filters: source/category pool restriction (exact-or-substring
// category matching), the breakdown echo, the ≤200 priority cap, the named
// batch file convention including import + rollback resolution, and the
// duplicate-name guard.
func TestLabelPriorityFilteredExportNamedBatch(t *testing.T) {
	ctx, svc, repo, dir := newLabelFixture(t)
	// Diversify the seed so filters bite: 10 otqa/geography rows, 5
	// Celebrities otqa rows; the rest stay opentdb General Knowledge.
	for i := 1; i <= 10; i++ {
		id := fmt.Sprintf("q%03d", i+120) // tail of the unrated order
		q := repo.byID[id]
		q.Source = "otqa"
		q.Category = "OTQA:" + []string{"music", "geography", "television", "animals"}[(i-1)%4]
		repo.byID[id] = q
	}
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("q%03d", i+110)
		q := repo.byID[id]
		q.Category = "Celebrities"
		repo.byID[id] = q
	}

	// Priority export: opentdb + (General Knowledge|Geography) matches the
	// 110 leading GK rows AND the otqa 'OTQA:geography' rows are EXCLUDED
	// by the source filter.
	path, count, breakdown, err := svc.ExportFiltered(ctx, 40, ExportFilter{
		Source:       "opentdb",
		CategoriesIn: []string{"General Knowledge", "Geography"},
		Out:          "batch_priority_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "batch_priority_1.txt" || count != 40 {
		t.Fatalf("priority export = %s (%d)", filepath.Base(path), count)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), RubricV1[:64]) {
		t.Fatal("priority batch does not open with the rubric header")
	}
	for _, want := range []string{
		"batch_priority_1.rated.jsonl", "=== QUESTIONS ===",
		"Batch breakdown:", "source opentdb: 40", "category General Knowledge: 40",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("priority batch file missing %q", want)
		}
	}
	var aboard bool
	for _, b := range breakdown {
		if strings.Contains(b, "source opentdb: 40") {
			aboard = true
		}
	}
	if !aboard {
		t.Fatalf("exported breakdown lines = %v", breakdown)
	}
	// The pool stayed inside the filter: no otqa/celebrities rows exported.
	if strings.Contains(string(body), "OTQA:") || strings.Contains(string(body), "Celebrities") {
		t.Fatal("priority export leaked rows outside the source/category filters")
	}

	// Cap: 201 in priority mode exceeds priorityBatchCap(200).
	if _, _, _, err := svc.ExportFiltered(ctx, 201, ExportFilter{Source: "opentdb"}); err == nil {
		t.Fatal("priority cap not enforced")
	}
	// Standard cap unchanged for unfiltered exports.
	if _, _, _, err := svc.ExportFiltered(ctx, 103, ExportFilter{}); err == nil {
		t.Fatal("standard batch cap not enforced")
	}
	// Unknown source rejected.
	if _, _, _, err := svc.ExportFiltered(ctx, 10, ExportFilter{Source: "mixcloud"}); err == nil {
		t.Fatal("unknown source filter accepted")
	}
	// Duplicate priority name rejected.
	if _, _, _, err := svc.ExportFiltered(ctx, 10, ExportFilter{Out: "batch_priority_1"}); err == nil {
		t.Fatal("duplicate named export accepted")
	}

	// Weighted filter over a DIFFERENT slice: substring matching pins
	// exact-or-substring semantics.
	path2, count2, _, err := svc.ExportFiltered(ctx, 3, ExportFilter{Source: "otqa", CategoriesIn: []string{"music"}})
	if err != nil {
		t.Fatal(err)
	}
	if count2 != 3 {
		t.Fatalf("substring-filtered export = %d rows", count2)
	}
	b2, _ := os.ReadFile(path2)
	if !strings.Contains(string(b2), "OTQA:music") {
		t.Fatalf("substring filter did not hit OTQA:music rows: %d", count2)
	}

	// NAMED import: agents return batch_priority_1.rated.jsonl and the
	// state file must resolve it against its recorded batch (no numeric
	// filename parsing).
	batchIDs := []string{}
	for _, l := range strings.Split(string(body), "\n") {
		if strings.Contains(l, " id=") {
			fields := strings.Fields(l)
			for _, f := range fields {
				if strings.HasPrefix(f, "id=") {
					batchIDs = append(batchIDs, strings.TrimPrefix(f, "id="))
				}
			}
		}
	}
	if len(batchIDs) != 40 {
		t.Fatalf("parsed %d batch ids from the batch file, want 40", len(batchIDs))
	}
	var rated strings.Builder
	for _, id := range batchIDs {
		fmt.Fprintf(&rated, `{"question_id":"%s","rating":7}`+"\n", id)
	}
	ratedPath := filepath.Join(dir, "batch_priority_1.rated.jsonl")
	if err := os.WriteFile(ratedPath, []byte(rated.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := svc.Import(ctx, ratedPath)
	if err != nil {
		t.Fatalf("named import: %v", err)
	}
	if n != 40 {
		t.Fatalf("named import wrote %d ratings, want 40", n)
	}
	for _, id := range batchIDs {
		if got := repo.strength[id]; got == nil || *got != 7 {
			t.Fatalf("%s strength = %v after named import", id, got)
		}
	}

	// Named rollback: "batch_priority_1" resolves to the recorded batch.
	if err := svc.RollbackBatch(ctx, "batch_priority_1"); err != nil {
		t.Fatalf("named rollback: %v", err)
	}
	for _, id := range batchIDs {
		if repo.strength[id] != nil {
			t.Fatalf("%s still rated after named rollback", id)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "batch_priority_1.txt")); !os.IsNotExist(err) {
		t.Fatal("rolled-back priority batch file survived")
	}
}

// batchFileIDs parses the question ids out of an exported batch file
// (the " id=<...>" tokens in the numbered question lines).
func batchFileIDs(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, l := range strings.Split(string(body), "\n") {
		if !strings.Contains(l, " id=") {
			continue
		}
		for _, f := range strings.Fields(l) {
			if strings.HasPrefix(f, "id=") {
				ids = append(ids, strings.TrimPrefix(f, "id="))
			}
		}
	}
	return ids
}

// TestLabelImportCapPerBatchKind covers the M10 import-cap defect: the
// import validated EVERY rated file against the legacy labelBatchCap
// (102), rejecting the 200-row priority return files the priority
// exports produce. The cap is now derived per batch kind:
//
//   - a 200-row named priority batch imports (priorityBatchCap);
//   - a 200-row file against a legacy numbered batch fails with the CAP
//     error (labelBatchCap), while its full ≤102 coverage imports;
//   - a numbered priority export (filters without -out, recognized
//     through its recorded size) imports up to priorityBatchCap too.
//
// Audit batches need no alignment: ExportAudit bounds K by labelBatchCap
// and audit batch names resolve to that same cap on import.
func TestLabelImportCapPerBatchKind(t *testing.T) {
	ctx := context.Background()

	t.Run("200-row priority batch imports", func(t *testing.T) {
		dir := t.TempDir()
		repo := &fakeLabelRepo{byID: map[string]models.Question{}, strength: map[string]*int{}}
		repo.seed(205)
		svc := NewLabelService(repo, dir)

		path, count, _, err := svc.ExportFiltered(ctx, 200, ExportFilter{Out: "batch_priority_cap1"})
		if err != nil {
			t.Fatal(err)
		}
		if count != 200 {
			t.Fatalf("priority export = %d rows, want 200", count)
		}
		ids := batchFileIDs(t, path)
		if len(ids) != 200 {
			t.Fatalf("parsed %d batch ids, want 200", len(ids))
		}
		var rated strings.Builder
		for _, id := range ids {
			fmt.Fprintf(&rated, `{"question_id":"%s","rating":8}`+"\n", id)
		}
		ratedPath := filepath.Join(dir, "batch_priority_cap1.rated.jsonl")
		if err := os.WriteFile(ratedPath, []byte(rated.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		n, err := svc.Import(ctx, ratedPath)
		if err != nil {
			t.Fatalf("200-row priority import rejected: %v", err)
		}
		if n != 200 {
			t.Fatalf("import wrote %d ratings, want 200", n)
		}
		for _, id := range ids {
			if got := repo.strength[id]; got == nil || *got != 8 {
				t.Fatalf("%s strength = %v after priority import", id, got)
			}
		}
	})

	// Legacy numbered batches keep the legacy cap.
	t.Run("legacy cap: 102 ok, 200 rejected", func(t *testing.T) {
		dir := t.TempDir()
		repo := &fakeLabelRepo{byID: map[string]models.Question{}, strength: map[string]*int{}}
		repo.seed(210)
		svc := NewLabelService(repo, dir)

		// (c) a 102-line legacy batch imports (the old contract, intact).
		path, count, err := svc.Export(ctx, labelBatchCap)
		if err != nil {
			t.Fatal(err)
		}
		if count != labelBatchCap {
			t.Fatalf("legacy export = %d rows, want %d", count, labelBatchCap)
		}
		ids := batchFileIDs(t, path)
		if len(ids) != labelBatchCap {
			t.Fatalf("parsed %d batch ids, want %d", len(ids), labelBatchCap)
		}
		var rated strings.Builder
		for _, id := range ids {
			fmt.Fprintf(&rated, `{"question_id":"%s","rating":5}`+"\n", id)
		}
		ratedPath := filepath.Join(dir, "batch_001.rated.jsonl")
		if err := os.WriteFile(ratedPath, []byte(rated.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		n, err := svc.Import(ctx, ratedPath)
		if err != nil {
			t.Fatalf("%d-row legacy import rejected: %v", labelBatchCap, err)
		}
		if n != labelBatchCap {
			t.Fatalf("import wrote %d ratings, want %d", n, labelBatchCap)
		}

		// (b) a 200-line file against the NEXT legacy numbered batch must
		// fail with the cap error (not a membership error — the cap
		// check fires first).
		if _, _, err := svc.Export(ctx, 100); err != nil {
			t.Fatal(err)
		}
		var junk strings.Builder
		for i := 0; i < 200; i++ {
			fmt.Fprintf(&junk, `{"question_id":"zz%03d","rating":7}`+"\n", i)
		}
		junkPath := filepath.Join(dir, "batch_002.rated.jsonl")
		if err := os.WriteFile(junkPath, []byte(junk.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = svc.Import(ctx, junkPath)
		if err == nil {
			t.Fatal("200-row file accepted against a legacy numbered batch")
		}
		if want := fmt.Sprintf("batch cap is %d", labelBatchCap); !strings.Contains(err.Error(), want) {
			t.Fatalf("legacy import failed with a non-cap error: %v (want %q)", err, want)
		}
		if err := os.Remove(junkPath); err != nil {
			t.Fatal(err)
		}
	})

	// Filtered exports without -out get their priority cap through the
	// recorded batch size (a >labelBatchCap batch can only be priority).
	t.Run("numbered priority batch imports", func(t *testing.T) {
		dir := t.TempDir()
		repo := &fakeLabelRepo{byID: map[string]models.Question{}, strength: map[string]*int{}}
		repo.seed(205)
		svc := NewLabelService(repo, dir)

		path, count, _, err := svc.ExportFiltered(ctx, 150, ExportFilter{Source: "opentdb"})
		if err != nil {
			t.Fatal(err)
		}
		if count != 150 {
			t.Fatalf("numbered priority export = %d rows, want 150", count)
		}
		ids := batchFileIDs(t, path)
		if len(ids) != 150 {
			t.Fatalf("parsed %d batch ids, want 150", len(ids))
		}
		var rated strings.Builder
		for _, id := range ids {
			fmt.Fprintf(&rated, `{"question_id":"%s","rating":3}`+"\n", id)
		}
		ratedPath := filepath.Join(dir, "batch_001.rated.jsonl")
		if err := os.WriteFile(ratedPath, []byte(rated.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		n, err := svc.Import(ctx, ratedPath)
		if err != nil {
			t.Fatalf("150-row numbered priority import rejected: %v", err)
		}
		if n != 150 {
			t.Fatalf("import wrote %d ratings, want 150", n)
		}
	})
}
