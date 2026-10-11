package main

import (
	"context"
	"flag"
	"log"
	"os"

	"boggle-api/internal/millionaire/repository"
	"boggle-api/internal/millionaire/service"

	"github.com/vingarcia/ksql"
	"github.com/vingarcia/ksql/adapters/kpgx"
)

// cmd/import is the question importer dispatcher (M6/M10):
//
//   - the default (no -source) runs the M6 OpenTriviaQA importer: it
//     parses the corpus's category files (per-line codec sniff — see
//     service.OTQAImporter) and inserts the SAMPLE-FIRST stratified subset
//     (non-cultural ≤300, cultural ≤150, mixed ≤150 per category),
//     deduped against the whole existing corpus; idempotent.
//
//   - `-source wwtbam` runs the M10/S3 WWTBAM importer over the host-
//     converted JSONL seed (data/wwtbam_export.py → data/wwtbam.jsonl):
//     EVERY valid row (no sampling), strength = the real show level 1..15,
//     category 'WWTBAM:<src tag>', deduped against the whole corpus; the
//     second run imports 0.
//
//   - `-source opentdb` runs the OpenTDB importer over the vendored slice of
//     the OpenTDB corpus (CC BY-SA 4.0 — see README): data/opentdb.csv plus
//     the curated data/opentdb_strengths.csv, which is what gives every row a
//     real strength so the levels 1–3 casual pool is non-empty. Categories
//     the casual blacklist forbids are already excluded from the CSV; the
//     second run imports 0.
//
// Usage inside the container (the repo is bind-mounted at /workspace):
//
//	go run ./cmd/import                   # OTQA sample, reads /tmp/otqa
//	go run ./cmd/import -source wwtbam -file data/wwtbam.jsonl
//	go run ./cmd/import -source opentdb
//
// See README "Question engine v2 — import & labeling" and the
// "WWTBAM corpus" section for the full flows.
func main() {
	source := flag.String("source", "", `importer to run: "wwtbam" (reads -file), "opentdb" (reads -opentdb-file/-opentdb-strengths), or "" for the default OTQA sample importer`)
	dir := flag.String("otqa-dir", "/tmp/otqa", "OTQA only: directory holding the OpenTriviaQA category files (double-encoded corpus — per-line UTF-8/Windows-1252 sniff; one file per category)")
	file := flag.String("file", "data/wwtbam.jsonl", "wwtbam only: the converted JSONL seed (one row per question)")
	opentdbFile := flag.String("opentdb-file", "data/opentdb.csv", "opentdb only: the vendored, decoded OpenTDB CSV (question,answer,category)")
	opentdbStrengths := flag.String("opentdb-strengths", "data/opentdb_strengths.csv", "opentdb only: the curated strength CSV (question,strength) — without it every row imports UNRATED and stays invisible to the levels 1-3 casual pool")
	flag.Parse()

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

	repo := repository.NewQuestionRepository(db)
	switch *source {
	case "":
		importer := service.NewOTQAImporter(repo, *dir)
		stats, err := importer.Run(ctx)
		if err != nil {
			log.Fatalf("OTQA import failed: %v", err)
		}
		// Per-category summary table is already logged per line; total here.
		imported, duplicates, skipped := 0, 0, 0
		for _, st := range stats {
			imported += st.Imported
			duplicates += st.Duplicates
			skipped += st.Skipped
		}
		log.Printf("otqa: categories=%d imported=%d duplicates=%d skipped=%d", len(stats), imported, duplicates, skipped)
	case "wwtbam":
		importer := service.NewWWTBAMImporter(repo, *file)
		stats, err := importer.Run(ctx)
		if err != nil {
			log.Fatalf("WWTBAM import failed: %v", err)
		}
		log.Printf("wwtbam: %s", stats)
	case "opentdb":
		importer := service.NewOpenTDBImporter(repo, *opentdbFile, *opentdbStrengths)
		stats, err := importer.Run(ctx)
		if err != nil {
			log.Fatalf("OpenTDB import failed: %v", err)
		}
		log.Printf("opentdb: %s", stats)
	default:
		log.Fatalf("unknown source %q (supported: \"\", \"wwtbam\", \"opentdb\")", *source)
	}
	log.Printf("import complete")
}
