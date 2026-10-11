package codes

import (
	"strings"
	"testing"
)

// TestRoomCodeFormat pins the M7 code format contract: the curated
// vocabulary is uppercase, long enough to be readable, built only from
// non-confusable glyphs (no I/L/O letters — they collide with 1/1/0),
// and every word is distinct.
func TestRoomCodeFormat(t *testing.T) {
	if len(codeWords) < 100 {
		t.Fatalf("curated vocabulary = %d words, want >= ~100", len(codeWords))
	}
	seen := map[string]bool{}
	for i, w := range codeWords {
		if w != strings.ToUpper(w) {
			t.Fatalf("codeWords[%d] = %q is not uppercase", i, w)
		}
		if len(w) < codeWordLen {
			t.Fatalf("codeWords[%d] = %q shorter than %d", i, w, codeWordLen)
		}
		for _, r := range w {
			if r == 'I' || r == 'L' || r == 'O' {
				t.Fatalf("codeWords[%d] = %q contains confusable glyph %q", i, w, r)
			}
			if r < 'A' || r > 'Z' {
				t.Fatalf("codeWords[%d] = %q is not A-Z: %q", i, w, r)
			}
		}
		if seen[w] {
			t.Fatalf("codeWords[%d] = %q duplicates an earlier word", i, w)
		}
		seen[w] = true
	}
}

// TestGenerateRoomCodeShapeAndSpread generates a batch and asserts the
// format (distinct words, 2 unrestricted-glyph digits) and that both the
// word slots and the digit suffix actually SPREAD — a constant word slot
// or suffix would make code collisions trivial.
func TestGenerateRoomCodeShapeAndSpread(t *testing.T) {
	words := map[string]bool{}
	digits := map[string]bool{}
	const n = 2000
	for i := 0; i < n; i++ {
		code := generateRoomCode()
		parts := strings.Split(code, "-")
		if len(parts) != 3 {
			t.Fatalf("code %q is not WORD-WORD-DD", code)
		}
		if parts[0] == parts[1] {
			t.Fatalf("code %q repeats a word", code)
		}
		for _, p := range parts[:2] {
			if len(p) < codeWordLen || strings.ToUpper(p) != p {
				t.Fatalf("code %q word %q violates the format", code, p)
			}
			for _, r := range p {
				if r == 'I' || r == 'L' || r == 'O' {
					t.Fatalf("code %q word %q contains confusable glyph %q", code, p, r)
				}
			}
		}
		dd := parts[2]
		if len(dd) != 2 {
			t.Fatalf("code %q suffix %q is not 2 digits", code, dd)
		}
		for _, r := range dd {
			if r < '2' || r > '9' {
				t.Fatalf("code %q suffix %q digit %q outside 2..9", code, dd, r)
			}
		}
		words[parts[0]] = true
		words[parts[1]] = true
		digits[dd] = true
	}
	if len(words) < 40 {
		t.Fatalf("word slots only used %d distinct words", len(words))
	}
	if len(digits) < 40 {
		t.Fatalf("suffix only used %d distinct 2-digit values", len(digits))
	}
}

// TestGenerateRoomCodeDistinctness is the flake guard for the same-word
// retry loop: over a full batch no code may read WORD-WORD.
func TestGenerateRoomCodeDistinctness(t *testing.T) {
	for i := 0; i < 5000; i++ {
		code := generateRoomCode()
		parts := strings.Split(code, "-")
		if parts[0] == parts[1] {
			t.Fatalf("generated %q with identical words", code)
		}
	}
}
