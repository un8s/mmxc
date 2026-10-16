package mmxc

import (
	"strings"
	"testing"
)

// fuzzyPayeeThreshold mirrors the cutoff resolveFuzzyPayeeID applies; a score at
// or above it would resolve to the candidate payee.
const fuzzyPayeeThreshold = 82

func TestFuzzyTextScoreMatching(t *testing.T) {
	cases := []struct {
		name      string
		statement string
		candidate string
		want      bool // true if it should resolve (score >= threshold)
	}{
		// Textual misses the old whole-string Levenshtein scorer dropped.
		{"concatenated name", "THE HOME DEPOT", "Homedepot", true},
		{"clean name buried in noise", "CADILLAC CHEVROLET BUIT RIVERTON NY", "Chevrolet Riverton", true},
		// Existing typo behaviour must keep working.
		{"typo", "Amazn", "Amazon", true},
		// Semantic mappings have no textual signal and must NOT false-match.
		{"semantic uber", "UBER", "*Taxi", false},
		{"semantic wikipedia", "WIKIPEDIA", "Wikimedia Foundation", false},
		// A single shared token must not pull an unrelated multi-word payee over.
		{"partial token overlap", "RIVERTON STAR", "Riverton Savings", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			score := fuzzyTextScore(normalizeSearchText(tc.statement), normalizeSearchText(tc.candidate))
			got := score >= fuzzyPayeeThreshold
			if got != tc.want {
				t.Fatalf("%q vs %q: score=%d, resolves=%v, want resolves=%v",
					tc.statement, tc.candidate, score, got, tc.want)
			}
		})
	}
}

func TestGeneralizePayeePattern(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		// Store number, city, and state are normalized; the store number is a
		// volatile pure-number token and drops out.
		{"THE HOME DEPOT #7012 SPRINGFIELD IL", "the home depot springfield"},
		// Mixed alphanumeric reference code drops; merchant core stays.
		{"AMZN MKTP US*A1B2C3D4", "amzn mktp"},
		// Single-token merchant survives.
		{"UBER", "uber"},
		// All-numeric description falls back to its longest token, never empty.
		{"12345 67890", "12345"},
	}
	for _, tc := range cases {
		if got := generalizePayeePattern(tc.raw); got != tc.want {
			t.Errorf("generalizePayeePattern(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestPayeePatternConverges checks that learning the same merchant from two
// statements whose volatile parts differ collapses the stored pattern to the
// shared core, which then matches a third, unseen variant.
func TestPayeePatternConverges(t *testing.T) {
	first := generalizePayeePattern("THE HOME DEPOT #7012 SPRINGFIELD IL")
	second := generalizePayeePattern("THE HOME DEPOT #3344 RIVERTON NY")

	core := sharedSignificantTokens(first, second)
	pattern := strings.Join(core, " ")
	if pattern != "the home depot" {
		t.Fatalf("converged pattern = %q, want %q", pattern, "the home depot")
	}

	// The converged pattern resolves a third location it never saw, and does not
	// match an unrelated merchant that happens to share one word.
	if !importRuleMatches("the home depot 9981 lakewood oh", pattern) {
		t.Fatal("converged pattern should match an unseen Home Depot variant")
	}
	if importRuleMatches("ace hardware lakewood oh", pattern) {
		t.Fatal("converged pattern should not match an unrelated merchant")
	}
}
