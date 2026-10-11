package models

import "strings"

// OtqaCulturalCategories are the OpenTriviaQA subcategories the taste layer
// classes as CULTURAL (SPEC §2 game engine v2 / task mapping): pop-culture
// subjects that must never dominate early levels.
var OtqaCulturalCategories = []string{
	"OTQA:music", "OTQA:television", "OTQA:movies", "OTQA:celebrities",
	"OTQA:people", "OTQA:video-games", "OTQA:entertainment", "OTQA:rated",
}

// culturalOtqa is a lookup set over OtqaCulturalCategories.
var culturalOtqa = func() map[string]bool {
	m := make(map[string]bool, len(OtqaCulturalCategories))
	for _, c := range OtqaCulturalCategories {
		m[c] = true
	}
	return m
}()

// IsCultural is the single taste-layer predicate (SPEC §2: category only
// governs caps, never difficulty). It covers both corpora:
//   - OTQA rows store category `OTQA:<subcategory>` (source="otqa"):
//     the OTQA cultural list above. DECISION on the unlisted subcategory
//     "hobbies": NOT cultural (pastime/craft content — general knowledge
//     material, nearest class non-cultural; keeps early levels clear).
//   - OpenTDB Entertainment:* and Celebrities are cultural.
func IsCultural(category, source string) bool {
	if source == "otqa" {
		return culturalOtqa[category]
	}
	if source == "opentdb" {
		if category == "Celebrities" {
			return true
		}
		return strings.HasPrefix(category, "Entertainment:")
	}
	return false
}

// ---------------------------------------------------------------------------
// S2 casual-pool rule (v3-ux-plan "Question quality" / backend M10): levels
// 1–3 are pub-quiz round 1 — every pick must be strength 1–3 ∧ non-cultural
// ∧ NOT on the NICHE LIST (video games, anime/cartoons, theology,
// celebrities, gadgets, mundane animals, subcultures). The niche list is
// enforced as an explicit CATEGORY blacklist per corpus (SQL-side, see
// repository.RandomByCasual), because mundane-subject nuances (e.g.
// "animals minus mundane") cannot be expressed in the WHERE clause: the
// blacklist drops the whole category instead. Cultural overlap is
// deliberate: the lists below are a SUPERSET of IsCultural, so the casual
// pool is non-cultural by construction.
// ---------------------------------------------------------------------------

// CasualBlacklistOtqa lists the OTQA subcategories excluded from the casual
// pool: every cultural subcategory (pop culture must not dominate round 1)
// plus video-games' mundane-animal niche → the whole `animals` category is
// blacklisted too (the spec's "mundane-animal" item; an L1 animals question
// reads as a niche quiz, not round-1 general knowledge).
var CasualBlacklistOtqa = append(append([]string{}, OtqaCulturalCategories...),
	"OTQA:animals")

// CasualBlacklistOpentdbPrefixes: opentdb categories matched by PREFIX that
// the casual pool must skip — "Entertainment:*" carries the anime/cartoon/
// film/music/video-game niches in one sweep.
var CasualBlacklistOpentdbPrefixes = []string{"Entertainment:"}

// CasualBlacklistOpentdbExact: opentdb categories matched EXACTLY —
// Celebrities is the spec's "celebrities" niche (also cultural per
// IsCultural), Science: Gadgets the "gadgets" niche. NOTE (documented
// decision): religion/mythology is NOT blacklisted even though the spec's
// niche wording lists theology — that category is pending a user product
// decision and the approved S2 lists (v3-ux-plan) omit it.
var CasualBlacklistOpentdbExact = []string{"Celebrities", "Science: Gadgets"}

// IsCasualBlacklisted is the Go-side mirror of the SQL blacklist the casual
// pool query applies (pin-tested in models + repository trap tests): true
// when the (category, source) pair may NEVER serve levels 1–3.
func IsCasualBlacklisted(category, source string) bool {
	switch source {
	case "otqa":
		for _, c := range CasualBlacklistOtqa {
			if category == c {
				return true
			}
		}
	case "opentdb":
		if category == "Celebrities" || category == "Science: Gadgets" {
			return true
		}
		if strings.HasPrefix(category, "Entertainment:") {
			return true
		}
	}
	return false
}
