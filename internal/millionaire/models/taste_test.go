package models

import "testing"

// This file pins the S2 casual-pool blacklist (v3-ux-plan "Question
// quality" / backend M10): the package-level lists are contract data the
// SQL casual query and the gate tests both consume, so their membership is
// pinned here rather than inferred.

func TestCasualBlacklistOtqaIsCulturalPlusAnimals(t *testing.T) {
	set := map[string]bool{}
	for _, c := range CasualBlacklistOtqa {
		set[c] = true
	}
	// Every cultural subcategory MUST be on the casual blacklist (the pool
	// is non-cultural by construction).
	for _, c := range OtqaCulturalCategories {
		if !set[c] {
			t.Fatalf("cultural category %q missing from CasualBlacklistOtqa — the casual pool would not be non-cultural", c)
		}
	}
	// The mundane-animal niche drops the whole animals subcategory.
	if !set["OTQA:animals"] {
		t.Fatal("OTQA:animals missing from CasualBlacklistOtqa (spec's mundane-animal niche)")
	}
	// Exactly the approved list — no accidental narrowing/widening.
	want := map[string]bool{
		"OTQA:music": true, "OTQA:television": true, "OTQA:movies": true,
		"OTQA:celebrities": true, "OTQA:people": true, "OTQA:video-games": true,
		"OTQA:entertainment": true, "OTQA:rated": true, "OTQA:animals": true,
	}
	if len(set) != len(want) {
		t.Fatalf("CasualBlacklistOtqa = %v, want exactly the approved 9 entries", CasualBlacklistOtqa)
	}
	for c := range want {
		if !set[c] {
			t.Fatalf("CasualBlacklistOtqa missing %q", c)
		}
	}
}

func TestCasualBlacklistOpentdbMembership(t *testing.T) {
	if len(CasualBlacklistOpentdbPrefixes) != 1 || CasualBlacklistOpentdbPrefixes[0] != "Entertainment:" {
		t.Fatalf("CasualBlacklistOpentdbPrefixes = %v, want [Entertainment:]", CasualBlacklistOpentdbPrefixes)
	}
	set := map[string]bool{}
	for _, c := range CasualBlacklistOpentdbExact {
		set[c] = true
	}
	if !set["Science: Gadgets"] || !set["Celebrities"] {
		t.Fatalf("CasualBlacklistOpentdbExact = %v, want {Celebrities, Science: Gadgets}", CasualBlacklistOpentdbExact)
	}
}

func TestIsCasualBlacklisted(t *testing.T) {
	banned := [][2]string{
		{"Entertainment: Video Games", "opentdb"},
		{"Entertainment: Film", "opentdb"},
		{"Entertainment: Japanese Anime & Manga", "opentdb"},
		{"Celebrities", "opentdb"},
		{"Science: Gadgets", "opentdb"},
		{"OTQA:music", "otqa"},
		{"OTQA:video-games", "otqa"},
		{"OTQA:animals", "otqa"},
	}
	for _, b := range banned {
		if !IsCasualBlacklisted(b[0], b[1]) {
			t.Fatalf("%s (%s) must be casual-blacklisted", b[0], b[1])
		}
	}
	allowed := [][2]string{
		{"General Knowledge", "opentdb"},
		{"Geography", "opentdb"},
		{"Sports", "opentdb"},
		{"History", "opentdb"},
		{"Science: Computers", "opentdb"}, // niche, but NOT on the approved S2 list
		{"Religion & Mythology", "opentdb"},
		{"Mythology", "opentdb"},
		{"OTQA:general", "otqa"},
		{"OTQA:geography", "otqa"},
		{"OTQA:for-kids", "otqa"},
		{"OTQA:history", "otqa"},
		{"", "opentdb"},
	}
	for _, a := range allowed {
		if IsCasualBlacklisted(a[0], a[1]) {
			t.Fatalf("%q (%s) must NOT be casual-blacklisted", a[0], a[1])
		}
	}
	// Cultural cross-check: IsCultural ⇒ IsCasualBlacklisted (the casual
	// rule is a strengthening of the taste layer at L1–3).
	for _, c := range [][2]string{
		{"Entertainment: Music", "opentdb"}, {"Celebrities", "opentdb"},
		{"OTQA:television", "otqa"}, {"OTQA:people", "otqa"},
	} {
		if IsCultural(c[0], c[1]) && !IsCasualBlacklisted(c[0], c[1]) {
			t.Fatalf("IsCultural(%q) but not casual-blacklisted — L1–3 would not be non-cultural", c[0])
		}
	}
}
