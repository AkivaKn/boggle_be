package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"boggle-api/internal/millionaire/repository"
	"boggle-api/internal/millionaire/service"

	"github.com/vingarcia/ksql"
	"github.com/vingarcia/ksql/adapters/kpgx"
)

// cmd/label is the OFFLINE question-strength pipeline driver (M6, SPEC §2
// question engine v2). It never runs labeling agents — the ORCHESTRATOR
// does — it only exports batch files, validates/imports agent answers,
// manages state, and reports coverage/drift:
//
//	cmd/label -export N            label/batch_<id>.txt with the rubric header
//	                               (agents return label/batch_<id>.rated.jsonl)
//	cmd/label -import FILE         validate + write questions.strength
//	cmd/label -export-audit K      blind double-rating batch (no DB write)
//	cmd/label -audit-report        agreement stats (same band %, ±1 band %)
//	cmd/label -coverage            per-band counts + cultural % report
//	cmd/label -rollback-batch F    undo a batch (demo cleanup / mistakes)
//
// M10 PRIORITY EXPORTS (labels-first): -export accepts -source /
// -category-in / -exclude-category-in filters (and -out for a named batch
// file, e.g. -out batch_priority_1); filtered batches may carry up to 200
// rows and ECHO their source/category breakdown for the labeling-agent
// prompt.
//
// State (label/, gitignored): state.jsonl, rubric.md, batch files.
// IMPORTANT: run with the repo root as cwd (state dir ./label).
func main() {
	stateDir := flag.String("state-dir", "label", "pipeline state directory (label/rubric.md, state.jsonl, batch files)")

	var exportN int
	flag.IntVar(&exportN, "export", 0, "export a real batch of N unrated questions (default 100 when >0 is given; e.g. -export 100, demo: -export 20)")
	var exportAuditK int
	flag.IntVar(&exportAuditK, "export-audit", 0, "export a blind audit batch of K already-rated questions")
	var importFile string
	flag.StringVar(&importFile, "import", "", "import an agent-returned rated JSONL (label/batch_<id>[.audit].rated.jsonl)")
	auditReport := flag.Bool("audit-report", false, "print the audit agreement report (same band %, ±1 band %)")
	coverage := flag.Bool("coverage", false, "print the per-band coverage report (counts + cultural %)")
	var rollbackBatch string
	flag.StringVar(&rollbackBatch, "rollback-batch", "", "roll a batch back: reset its strengths to NULL and remove its state lines (cleanup/demo)")
	// M10 priority filters (labels-first easy-band batches):
	exportSource := flag.String("source", "any", "priority filter: restrict the export source (any | opentdb | otqa)")
	exportCategoryIn := flag.String("category-in", "", "priority filter: comma-list of category tokens included (exact or substring match on questions.category)")
	exportCategoryEx := flag.String("exclude-category-in", "", "priority filter: comma-list of category tokens excluded")
	exportOut := flag.String("out", "", "named priority batch base (e.g. batch_priority_1 → batch_priority_1.txt; agents return batch_priority_1.rated.jsonl)")
	flag.Parse()

	splitCats := func(s string) []string {
		var out []string
		for _, t := range strings.Split(s, ",") {
			if t = strings.TrimSpace(t); t != "" {
				out = append(out, t)
			}
		}
		return out
	}

	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = "postgres://devuser:devpassword@db:5432/millionaire?sslmode=disable"
	}

	ctx := context.Background()
	db, err := kpgx.New(ctx, connStr, ksql.Config{})
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer db.Close()

	svc := service.NewLabelService(repository.NewLabelRepository(db), *stateDir)

	switch {
	case exportN > 0:
		path, count, breakdown, err := svc.ExportFiltered(ctx, exportN, service.ExportFilter{
			Source:       *exportSource,
			CategoriesIn: splitCats(*exportCategoryIn),
			CategoriesEx: splitCats(*exportCategoryEx),
			Out:          *exportOut,
		})
		exitOn(err)
		fmt.Printf("exported %s (%d questions)\n", path, count)
		for _, line := range breakdown {
			fmt.Println(line)
		}
	case exportAuditK > 0:
		path, count, err := svc.ExportAudit(ctx, exportAuditK)
		exitOn(err)
		fmt.Printf("exported %s (%d questions, blind audit)\n", path, count)
	case importFile != "":
		n, err := svc.Import(ctx, importFile)
		exitOn(err)
		fmt.Printf("imported %d rating(s) from %s\n", n, importFile)
	case *auditReport:
		exitOn(svc.AuditReport(ctx))
	case *coverage:
		exitOn(svc.Coverage(ctx))
	case rollbackBatch != "":
		exitOn(svc.RollbackBatch(ctx, rollbackBatch))
		fmt.Printf("batch rolled back (state lines removed; strengths NULL again)\n")
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func exitOn(err error) {
	if err != nil {
		log.Fatalf("error: %v", err)
	}
}
