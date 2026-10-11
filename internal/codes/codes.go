package codes

import (
	"math/rand/v2"
	"strings"
)

// Room codes (M7, SPEC v1.1 decision log): duel rooms surface a short
// human-friendly code WORD-WORD-DD (e.g. BLUE-TIGER-73) instead of the
// internal UUID; solo rooms carry the empty string (codes are a duel-only
// surface). Generation is server-side; the rooms.code column carries a
// partial unique index (distinct values only), so collisions surface as
// insert errors that the caller retries.
//
// Two deliberate confusable-glyph decisions (documented):
//   - the word lists contain NO letter that renders ambiguously with a
//     digit in the shared fonts (I/1, L/1, O/0) — every word is built
//     from the 23 remaining uppercase letters;
//   - the 2-digit suffix (plan text says 11–99) is drawn as tens/units
//     digits from {2..9} — i.e. 22..99 with no 0 or 1 inside, keeping the
//     "no confusable glyphs" invariant true for the WHOLE code. A random
//     2-digit number from the full 11–99 would reintroduce exactly the
//     glyphs the rule bans.

// codeWordLen bounds the curated vocabulary shape (readable on a phone
// screen, still long enough to be typeable words).
const codeWordLen = 4

// MaxPick caps the collision retry loop: 78 suffix digits ×
// (len(codeWords))²  word pairs — the empty-out probability is negligible,
// so a handful of attempts covers every realistic ledger size. Exported
// because every game's room-creation path retries against its own unique
// index using this same budget.
const MaxPick = 10

// codeWordsMaxPick is the unexported alias kept for the package's own tests.
const codeWordsMaxPick = MaxPick

// codeWords is the curated vocabulary (~150 words; format floor ≥100 is
// pinned by the unit test so a future edit cannot silently break the
// format contract): uppercase words without the glyphs I, L, O (they
// collide with 1/1/0), length ≥ 4, all distinct.
var codeWords = []string{
	"AMBER", "ARENA", "ASPEN", "BADGE", "BANNER",
	"BARGE", "BARN", "BATH", "BEACH", "BEARD",
	"BEAST", "BEAVER", "BENCH", "BERRY", "BRAND",
	"BREED", "BREEZE", "BUNCH", "BUNKER", "BURST",
	"BUTTER", "CANVAS", "CAREER", "CATCH", "CEDAR",
	"CHAMP", "CHANT", "CHASM", "CHESS", "CHEST",
	"CHURCH", "CREEK", "CREST", "CRUNCH", "CRUST",
	"CURVE", "DANCE", "DANCER", "DECADE", "DESERT",
	"DETECT", "DUSK", "DUSTER", "DWARF", "EARTH",
	"EMBER", "FARM", "FEAST", "FENCE", "FERN",
	"FERRY", "FEVER", "FRESH", "FUDGE", "GAME",
	"GARDEN", "GRAPE", "GRASS", "GREEK", "GUESS",
	"GUEST", "HAMMER", "HAWK", "HERB", "HUNCH",
	"HUSKY", "HYENA", "JAGUAR", "JAZZ", "JUMPER",
	"KAYAK", "MAGNET", "MAMBA", "MARCH", "MARS",
	"MASK", "MATCH", "METER", "MUNCH", "MUSTARD",
	"MUSTANG", "NECTAR", "NEPHEW", "NURSE", "PANDA",
	"PANTHER", "PAPER", "PARK", "PATCH", "PEACH",
	"PEPPER", "PUNCH", "PUPPY", "PURSE", "PUMA",
	"QUACK", "QUARTZ", "QUEEN", "QUEST", "RADAR",
	"RANCH", "RAVEN", "REACH", "RESCUE", "RUST",
	"SAND", "SCARF", "SCENT", "SEARCH", "SHADE",
	"SHAKE", "SHARK", "SHARP", "STEAM", "STEM",
	"STUMP", "SUNSET", "SWAMP", "SWAN", "TANK",
	"TAPE", "TASK", "TEETH", "TENT", "THEME",
	"THUMB", "THUNDER", "TRACE", "TRACK", "TRADE",
	"TRAP", "TREND", "TRUST", "TUSK", "WASP",
	"WATCH", "WATER", "WAVE", "WEAVE", "WHEAT",
	"WREN", "WRENCH", "YACHT", "YARN", "YEAST",
	"ZEBRA", "ZEPHYR", "ZEST",
}

// NewRoomCode is the exported seam for deploy tooling (cmd/backfill): one
// WORD-WORD-DD candidate with WORD1 != WORD2 and both digits in 2..9.
// math/rand/v2 package functions are auto-seeded and concurrency-safe;
// uniqueness versus the DB is the caller's job (retry on the unique-index
// violation).
func NewRoomCode() string { return generateRoomCode() }

// generateRoomCode returns one WORD-WORD-DD candidate with WORD1 != WORD2
// and both digits in 2..9. math/rand/v2 package functions are auto-seeded
// and concurrency-safe; uniqueness versus the DB is the caller's job
// (retry on the unique-index violation).
func generateRoomCode() string {
	w1 := codeWords[rand.IntN(len(codeWords))]
	w2 := w1
	for w2 == w1 { // WORD-WORD-77 is legal but reads badly — never ship it
		w2 = codeWords[rand.IntN(len(codeWords))]
	}
	tens := 2 + rand.IntN(8) // {2..9} — no 0/1 glyphs in either digit
	units := 2 + rand.IntN(8)
	var sb strings.Builder
	sb.Grow(20)
	sb.WriteString(w1)
	sb.WriteByte('-')
	sb.WriteString(w2)
	sb.WriteByte('-')
	sb.WriteByte(byte('0' + tens))
	sb.WriteByte(byte('0' + units))
	return sb.String()
}
