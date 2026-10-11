package main

import (
	"context"
	"log"
	"os"

	"boggle-api/internal/millionaire/repository"
	"boggle-api/internal/millionaire/service"

	"github.com/vingarcia/ksql"
	"github.com/vingarcia/ksql/adapters/kpgx"
)

func main() {
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

	// Standing invariant (M0 review): ksql INSERT sends every field — the
	// seeder sets Source/Category/CreatedAt explicitly (see service.buildQuestion)
	// and only inserts (questions are immutable).
	seeder := service.NewSeeder(repository.NewQuestionRepository(db), nil)
	stats, err := seeder.Run(ctx)
	if err != nil {
		log.Fatalf("seeding failed: %v", err)
	}

	failed := false
	for _, tier := range []string{"easy", "medium", "hard"} {
		if st := stats[tier]; st.Total < service.MinTierCount[tier] {
			log.Printf("FAIL: tier %q below minimum: %d < %d", tier, st.Total, service.MinTierCount[tier])
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
	log.Printf("seeding complete")
}
