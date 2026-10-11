package service

import (
	"encoding/json"
	"math/rand/v2"
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeQuestionText(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "html entities unescaped",
			in:   "&quot;A&quot; &amp; &lt;B&gt;",
			want: `"a" & <b>`,
		},
		{
			name: "case folded",
			in:   "Who Wrote Hamlet?",
			want: "who wrote hamlet?",
		},
		{
			name: "whitespace collapsed",
			in:   "  who   wrote\t\nHamlet?  ",
			want: "who wrote hamlet?",
		},
		{
			name: "nbsp variants collapse like spaces (M1-review finding 3)",
			in:   "Who\u00A0Wrote\u00A0Hamlet?", // &nbsp; after html-unescape
			want: "who wrote hamlet?",
		},
		{
			name: "U+0085 variants collapse like spaces",
			in:   "Who Wrote\u0085Hamlet?",
			want: "who wrote hamlet?",
		},
		{
			name: "already normalized",
			in:   "already   normalized?",
			want: "already normalized?",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeQuestionText(tc.in); got != tc.want {
				t.Errorf("normalizeQuestionText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAssembleOptions(t *testing.T) {
	for i := 0; i < 100; i++ {
		rng := rand.New(rand.NewPCG(uint64(i), 7))
		options, correctIndex := assembleOptions("4", []string{"3", "5", "6"}, rng)
		if len(options) != 4 {
			t.Fatalf("got %d options, want 4: %v", len(options), options)
		}
		if options[correctIndex] != "4" {
			t.Fatalf("options=%v correctIndex=%d but options[correctIndex]=%q, want \"4\"",
				options, correctIndex, options[correctIndex])
		}
	}
}

func TestBuildQuestion(t *testing.T) {
	t.Run("imported valid multiple-choice result", func(t *testing.T) {
		res := opentdbResult{
			Category:         "Science: Mathematics",
			Type:             "multiple",
			Difficulty:       "easy",
			Question:         "What is 2 + 2?",
			CorrectAnswer:    "4",
			IncorrectAnswers: []string{"3", "5", "6"},
		}
		seen := map[string]bool{}
		q, status, err := buildQuestion(res, seen, rand.New(rand.NewPCG(1, 2)))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != builtImported {
			t.Fatalf("status = %q, want %q", status, builtImported)
		}

		// correct_index must point at the correct answer after the shuffle.
		var options []string
		if err := json.Unmarshal([]byte(q.Options), &options); err != nil {
			t.Fatalf("Options is not valid JSON: %v", err)
		}
		if len(options) != 4 {
			t.Fatalf("got %d options, want 4", len(options))
		}
		if options[q.CorrectIndex] != "4" {
			t.Fatalf("options=%v correctIndex=%d: options[correctIndex]=%q, want \"4\"",
				options, q.CorrectIndex, options[q.CorrectIndex])
		}

		// Standing invariant: every field set explicitly (ksql sends them all).
		if q.Source != "opentdb" {
			t.Errorf("Source = %q, want opentdb", q.Source)
		}
		if q.Category != "Science: Mathematics" {
			t.Errorf("Category = %q", q.Category)
		}
		if q.Difficulty != "easy" {
			t.Errorf("Difficulty = %q", q.Difficulty)
		}
		if q.CreatedAt.IsZero() {
			t.Error("CreatedAt must be set explicitly")
		}
		if _, err := uuid.Parse(q.ID); err != nil {
			t.Errorf("ID %q is not a uuid: %v", q.ID, err)
		}

		if !seen["what is 2 + 2?"] {
			t.Error("normalized text must be recorded in seen for dedupe")
		}
	})

	t.Run("duplicate normalized text", func(t *testing.T) {
		res := opentdbResult{
			Type:             "multiple",
			Difficulty:       "easy",
			Question:         "what   is 2 + 2?", // same as seen, different spacing
			CorrectAnswer:    "4",
			IncorrectAnswers: []string{"3", "5", "6"},
		}
		seen := map[string]bool{"what is 2 + 2?": true}
		_, status, err := buildQuestion(res, seen, rand.New(rand.NewPCG(1, 2)))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != builtDuplicate {
			t.Fatalf("status = %q, want %q", status, builtDuplicate)
		}
	})

	t.Run("nbsp-encoded duplicate of a space-encoded question", func(t *testing.T) {
		// The DB already holds the plain variant; OpenTDB re-serves the same
		// question with &nbsp; entities. After unescape the U+00A0 bytes must
		// normalize to spaces so the two encode identically (M1-review finding 3).
		seen := map[string]bool{"who wrote hamlet?": true}
		res := opentdbResult{
			Type:             "multiple",
			Difficulty:       "easy",
			Question:         "Who&nbsp;Wrote&nbsp;Hamlet?",
			CorrectAnswer:    "Shakespeare",
			IncorrectAnswers: []string{"Marlowe", "Jonson", "Wilde"},
		}
		_, status, err := buildQuestion(res, seen, rand.New(rand.NewPCG(1, 2)))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != builtDuplicate {
			t.Fatalf("status = %q, want %q (nbsp variant must dedupe)", status, builtDuplicate)
		}
	})

	t.Run("nbsp variant imported first still dedupes the plain variant", func(t *testing.T) {
		seen := map[string]bool{}
		first := opentdbResult{
			Type:             "multiple",
			Difficulty:       "easy",
			Question:         "Who&nbsp;Wrote&nbsp;Hamlet?",
			CorrectAnswer:    "Shakespeare",
			IncorrectAnswers: []string{"Marlowe", "Jonson", "Wilde"},
		}
		_, status, err := buildQuestion(first, seen, rand.New(rand.NewPCG(1, 2)))
		if err != nil || status != builtImported {
			t.Fatalf("first variant: status=%q err=%v, want imported", status, err)
		}
		plain := first
		plain.Question = "Who   wrote Hamlet?" // same text, plain spaces
		_, status, err = buildQuestion(plain, seen, rand.New(rand.NewPCG(1, 2)))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != builtDuplicate {
			t.Fatalf("status = %q, want %q", status, builtDuplicate)
		}
	})

	t.Run("empty normalized text skipped, not duplicated (M1-review finding 2)", func(t *testing.T) {
		for _, raw := range []string{"", "   ", "&nbsp;"} {
			res := opentdbResult{
				Type:             "multiple",
				Difficulty:       "easy",
				Question:         raw,
				CorrectAnswer:    "4",
				IncorrectAnswers: []string{"3", "5", "6"},
			}
			_, status, err := buildQuestion(res, map[string]bool{}, rand.New(rand.NewPCG(1, 2)))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != builtSkipped {
				t.Fatalf("question %q: status = %q, want %q", raw, status, builtSkipped)
			}
		}
	})

	t.Run("boolean type skipped", func(t *testing.T) {
		res := opentdbResult{
			Type:             "boolean",
			Difficulty:       "easy",
			Question:         "Is Go fun?",
			CorrectAnswer:    "True",
			IncorrectAnswers: []string{"False"},
		}
		_, status, err := buildQuestion(res, map[string]bool{}, rand.New(rand.NewPCG(1, 2)))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != builtSkipped {
			t.Fatalf("status = %q, want %q", status, builtSkipped)
		}
	})

	t.Run("fewer than 4 options skipped", func(t *testing.T) {
		res := opentdbResult{
			Type:             "multiple",
			Difficulty:       "medium",
			Question:         "Broken result",
			CorrectAnswer:    "a",
			IncorrectAnswers: []string{"b"},
		}
		_, status, err := buildQuestion(res, map[string]bool{}, rand.New(rand.NewPCG(1, 2)))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != builtSkipped {
			t.Fatalf("status = %q, want %q", status, builtSkipped)
		}
	})
}
