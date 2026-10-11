package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"boggle-api/internal/millionaire/models"
	"boggle-api/internal/millionaire/repository"

	"github.com/google/uuid"
)

const (
	openTDBURL        = "https://opentdb.com/api.php"
	openTDBTokenURL   = "https://opentdb.com/api_token.php"
	openTDBBatchSize  = 50
	openTDBMaxRetries = 5
	throttleBackoff   = 5 * time.Second

	// maxDryBatches stops the import loop when OpenTDB keeps returning
	// questions we already have (pool drained) even without a session token.
	maxDryBatches = 10
)

// Default tier targets. Calibrated during M1 seeding to OpenTDB's actual
// unique supply of multiple-choice questions per tier: fetching more keeps
// returning duplicates (~97% for hard), so the targets are set to levels the
// API can sustain idempotently. The M1 gate needs ≥1,000 easy/medium; the
// hard pool caps below that (~975) and is reported honestly.
var defaultTierTargets = map[string]int{
	"easy":   1150,
	"medium": 1200,
	"hard":   950,
}

// MinTierCount is the minimum per-tier question count after seeding;
// cmd/seed exits non-zero when any tier ends below its entry (M1-review
// finding 4: SPEC §2 floor is ≥1,000 easy/medium; hard's unique OpenTDB
// pool sustains only ~400). Seeding targets (defaultTierTargets) are
// calibrated above these floors.
var MinTierCount = map[string]int{
	"easy":   1000,
	"medium": 1000,
	"hard":   400,
}

// errBadToken marks OpenTDB response codes 3 (token not found) and 4 (token
// empty): the session token went stale and the caller should reset it once —
// or continue token-less — instead of failing the run (M1-review finding 1).
var errBadToken = errors.New("OpenTDB session token invalid")

type opentdbResponse struct {
	ResponseCode int             `json:"response_code"`
	Results      []opentdbResult `json:"results"`
}

type opentdbResult struct {
	Category         string   `json:"category"`
	Type             string   `json:"type"`
	Difficulty       string   `json:"difficulty"`
	Question         string   `json:"question"`
	CorrectAnswer    string   `json:"correct_answer"`
	IncorrectAnswers []string `json:"incorrect_answers"`
}

// buildQuestion statuses.
const (
	builtImported  = "imported"
	builtDuplicate = "duplicate"
	builtSkipped   = "skipped"
)

// TierStats is the per-tier outcome of a seeding run.
type TierStats struct {
	Imported   int
	Duplicates int
	Skipped    int
	Total      int  // count of that tier in the DB after the run
	Exhausted  bool // OpenTDB ran out of unique questions for this tier
}

type Seeder struct {
	repo    repository.QuestionRepository
	targets map[string]int
	rng     *rand.Rand
	client  *http.Client
	apiURL  string
}

func NewSeeder(repo repository.QuestionRepository, targets map[string]int) *Seeder {
	if targets == nil {
		targets = defaultTierTargets
	}
	seed := time.Now().UnixNano()
	return &Seeder{
		repo:    repo,
		targets: targets,
		rng:     rand.New(rand.NewPCG(uint64(seed), uint64(seed>>32))),
		client:  &http.Client{Timeout: 20 * time.Second},
		apiURL:  openTDBURL,
	}
}

// Run imports questions tier by tier, idempotently: existing rows are counted
// first and only the shortfall is fetched. Dedupe is by normalized question
// text, both against the DB and within the run.
func (s *Seeder) Run(ctx context.Context) (map[string]TierStats, error) {
	token := s.fetchSessionToken(ctx) // optional; empty on failure

	seen, err := s.loadExistingTexts(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading existing questions for dedupe: %w", err)
	}
	log.Printf("seeder: %d existing questions loaded for dedupe", len(seen))

	stats := map[string]TierStats{}
	for _, tier := range []string{"easy", "medium", "hard"} {
		st, err := s.seedTier(ctx, tier, seen, token)
		if err != nil {
			return stats, fmt.Errorf("seeding tier %q: %w", tier, err)
		}
		stats[tier] = st
		log.Printf("seeder: %s: imported=%d duplicates=%d skipped=%d total=%d (target %d, exhausted=%v)",
			tier, st.Imported, st.Duplicates, st.Skipped, st.Total, s.targets[tier], st.Exhausted)
	}
	return stats, nil
}

func (s *Seeder) seedTier(ctx context.Context, tier string, seen map[string]bool, token string) (TierStats, error) {
	existing, err := s.repo.CountByTier(ctx, tier)
	if err != nil {
		return TierStats{}, fmt.Errorf("counting %s questions: %w", tier, err)
	}
	st := TierStats{Total: existing}
	if existing >= s.targets[tier] {
		log.Printf("seeder: %s already at %d (target %d) — nothing to do", tier, existing, s.targets[tier])
		return st, nil
	}

	shortfall := ((s.targets[tier] - existing) + openTDBBatchSize - 1) / openTDBBatchSize * openTDBBatchSize
	log.Printf("seeder: %s has %d/%d — importing up to %d", tier, existing, s.targets[tier], shortfall)

	dryBatches := 0
	for fetched := 0; fetched < shortfall; fetched += openTDBBatchSize {
		results, exhausted, err := s.fetchBatch(ctx, tier, token)
		if errors.Is(err, errBadToken) {
			// M1-review finding 1: a stale token (response codes 3/4) must
			// not fail the run. One reset attempt, then retry; if the retry
			// is ALSO rejected (M2 review finding 5), fall back to a
			// token-less continuation instead of failing the run.
			log.Printf("seeder: OpenTDB rejected session token (%v) — resetting once", err)
			token = s.resetSessionToken(ctx, token)
			results, exhausted, err = s.fetchBatch(ctx, tier, token)
			if errors.Is(err, errBadToken) {
				log.Printf("seeder: retry after reset also rejected (%v) — continuing without a token", err)
				token = ""
				results, exhausted, err = s.fetchBatch(ctx, tier, token)
			}
		}
		if err != nil {
			return st, err
		}
		if exhausted {
			st.Exhausted = true
			log.Printf("seeder: %s: OpenTDB pool exhausted, stopping gracefully", tier)
			break
		}

		importedThisBatch := 0
		for _, res := range results {
			q, status, err := buildQuestion(res, seen, s.rng)
			if err != nil {
				return st, err
			}
			switch status {
			case builtDuplicate:
				st.Duplicates++
			case builtSkipped:
				st.Skipped++
			case builtImported:
				// Standing invariant (M0 review): ksql INSERT sends every
				// field, so Source/Category/CreatedAt are all set in
				// buildQuestion — questions are inserted once, never updated.
				if err := s.repo.InsertQuestion(ctx, &q); err != nil {
					return st, fmt.Errorf("inserting question: %w", err)
				}
				st.Imported++
				importedThisBatch++
			}
		}

		if importedThisBatch == 0 {
			dryBatches++
			if dryBatches >= maxDryBatches {
				st.Exhausted = true
				log.Printf("seeder: %s: %d consecutive batches with no new unique questions — stopping", tier, dryBatches)
				break
			}
		} else {
			dryBatches = 0
		}

		// Polite pacing: OpenTDB recommends ~1 request per 5 seconds.
		select {
		case <-time.After(throttleBackoff):
		case <-ctx.Done():
			return st, ctx.Err()
		}
	}

	st.Total = existing + st.Imported
	return st, nil
}

// fetchBatch fetches one batch of questions, backing off on throttle
// (response_code 5) and reporting pool exhaustion (response_code 1) gracefully.
func (s *Seeder) fetchBatch(ctx context.Context, difficulty, token string) ([]opentdbResult, bool, error) {
	url := fmt.Sprintf("%s?amount=%d&difficulty=%s&type=multiple", s.apiURL, openTDBBatchSize, difficulty)
	if token != "" {
		url += "&token=" + token
	}

	for attempt := 1; attempt <= openTDBMaxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, false, err
		}
		resp, err := s.client.Do(req)
		if err != nil {
			return nil, false, fmt.Errorf("fetching OpenTDB: %w", err)
		}
		var parsed opentdbResponse
		err = json.NewDecoder(resp.Body).Decode(&parsed)
		resp.Body.Close()
		if err != nil {
			return nil, false, fmt.Errorf("decoding OpenTDB response: %w", err)
		}

		switch parsed.ResponseCode {
		case 0:
			return parsed.Results, false, nil
		case 1: // no results: pool exhausted for this filter
			return nil, true, nil
		case 3, 4: // token not found / token empty
			return nil, false, fmt.Errorf("response_code %d: %w", parsed.ResponseCode, errBadToken)
		case 5: // throttled
			log.Printf("seeder: OpenTDB throttled (attempt %d/%d), backing off %s",
				attempt, openTDBMaxRetries, throttleBackoff)
			select {
			case <-time.After(throttleBackoff):
			case <-ctx.Done():
				return nil, false, ctx.Err()
			}
		default:
			return nil, false, fmt.Errorf("OpenTDB response_code %d", parsed.ResponseCode)
		}
	}
	return nil, false, fmt.Errorf("OpenTDB still throttling after %d retries", openTDBMaxRetries)
}

// fetchSessionToken requests an OpenTDB session token so the API stops
// repeating questions before its pool is exhausted. Best-effort: an empty
// token means "proceed without one".
func (s *Seeder) fetchSessionToken(ctx context.Context) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openTDBTokenURL+"?command=request", nil)
	if err != nil {
		return ""
	}
	resp, err := s.client.Do(req)
	if err != nil {
		log.Printf("seeder: could not request OpenTDB session token (%v) — proceeding without", err)
		return ""
	}
	defer resp.Body.Close()
	var parsed struct {
		ResponseCode int    `json:"response_code"`
		Token        string `json:"response_token"`
	}
	if json.NewDecoder(resp.Body).Decode(&parsed) != nil || parsed.ResponseCode != 0 {
		return ""
	}
	log.Printf("seeder: using OpenTDB session token")
	return parsed.Token
}

// resetSessionToken asks OpenTDB to clear the used-question memory of the
// session token (command=reset — response_code 0 keeps the same token). On
// any failure it returns "" so the run continues token-less rather than
// aborting (M1-review finding 1).
func (s *Seeder) resetSessionToken(ctx context.Context, token string) string {
	url := openTDBTokenURL + "?command=reset&token=" + url.QueryEscape(token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	resp, err := s.client.Do(req)
	if err != nil {
		log.Printf("seeder: OpenTDB token reset failed (%v) — continuing without a token", err)
		return ""
	}
	defer resp.Body.Close()
	var parsed struct {
		ResponseCode int `json:"response_code"`
	}
	if json.NewDecoder(resp.Body).Decode(&parsed) != nil || parsed.ResponseCode != 0 {
		log.Printf("seeder: OpenTDB token reset rejected (code %d) — continuing without a token", parsed.ResponseCode)
		return ""
	}
	log.Printf("seeder: OpenTDB session token reset ok")
	return token
}

func (s *Seeder) loadExistingTexts(ctx context.Context) (map[string]bool, error) {
	existing, err := s.repo.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(existing))
	for _, q := range existing {
		seen[normalizeQuestionText(q.Question)] = true
	}
	return seen, nil
}

// buildQuestion converts an OpenTDB result into a models.Question.
// status tells the caller what happened:
//   - builtImported:  the question was assembled and is not yet in `seen`
//   - builtDuplicate: normalized text already seen (DB or earlier this run)
//   - builtSkipped:   not multiple-choice, fewer than 4 options, or bad id
//
// The question text is recorded normalized (html-unescape + lowercase +
// collapsed whitespace) for dedupe, and stored html-unescaped.
func buildQuestion(res opentdbResult, seen map[string]bool, rng *rand.Rand) (models.Question, string, error) {
	if res.Type != "multiple" || len(res.IncorrectAnswers) != 3 {
		return models.Question{}, builtSkipped, nil
	}

	norm := normalizeQuestionText(res.Question)
	if norm == "" {
		// M1-review finding 2: an empty normalized text is garbage input,
		// not a duplicate — route it to skipped so stats stay honest.
		return models.Question{}, builtSkipped, nil
	}
	if seen[norm] {
		return models.Question{}, builtDuplicate, nil
	}

	options, correctIndex := assembleOptions(
		html.UnescapeString(res.CorrectAnswer),
		unescapeAll(res.IncorrectAnswers),
		rng,
	)
	if len(options) != 4 || correctIndex < 0 {
		return models.Question{}, builtSkipped, nil
	}

	id, err := uuid.NewV7()
	if err != nil {
		return models.Question{}, "", fmt.Errorf("generating question id: %w", err)
	}

	// Standing invariant (M0 review): every field is set explicitly because
	// ksql INSERT sends them all.
	q := models.Question{
		ID:           id.String(),
		Question:     html.UnescapeString(res.Question),
		Options:      marshalOptions(options),
		CorrectIndex: correctIndex,
		Difficulty:   res.Difficulty,
		Category:     html.UnescapeString(res.Category),
		Source:       "opentdb",
		CreatedAt:    time.Now().UTC(),
	}
	seen[norm] = true
	return q, builtImported, nil
}

// assembleOptions shuffles the correct answer plus the incorrect ones and
// returns the options together with the correct answer's new index.
func assembleOptions(correct string, incorrect []string, rng *rand.Rand) ([]string, int) {
	options := make([]string, 0, 4)
	options = append(options, incorrect...)
	options = append(options, correct)
	rng.Shuffle(len(options), func(i, j int) {
		options[i], options[j] = options[j], options[i]
	})
	for i, o := range options {
		if o == correct {
			return options, i
		}
	}
	return options, -1
}

var whitespaceRe = regexp.MustCompile(`\s+`)

// normalizeQuestionText canonicalizes a question for dedupe: html-unescape,
// lowercase, collapse whitespace. U+00A0 (`&nbsp;`) and U+0085 are produced
// by html.UnescapeString and are NOT matched by Go's `\s`, so they are
// substituted before the \s+ collapse (M1-review finding 3: two stored rows
// contained nbsp variants of otherwise identical questions).
func normalizeQuestionText(s string) string {
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\u00A0", " ")
	s = strings.ReplaceAll(s, "\u0085", " ")
	s = strings.ToLower(s)
	s = whitespaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func unescapeAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = html.UnescapeString(s)
	}
	return out
}

func marshalOptions(options []string) string {
	data, _ := json.Marshal(options)
	return string(data)
}
