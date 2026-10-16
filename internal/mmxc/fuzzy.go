package mmxc

import "strings"

func normalizePayeePattern(pattern string) string {
	pattern = strings.Join(strings.Fields(pattern), " ")
	if len(pattern) < 3 {
		return ""
	}
	return pattern
}

func normalizeSearchText(text string) string {
	text = strings.ToLower(strings.Join(strings.Fields(text), " "))
	var b strings.Builder
	lastSpace := false
	for _, r := range text {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			lastSpace = false
			continue
		}
		if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// searchKey folds a string into the canonical form used for transaction
// filtering: lowercased, accents stripped, and every run of non-alphanumeric
// characters collapsed to a single space. Applying it to both the column and the
// needle (via the SQLite "searchkey" function) makes the Search/Payee/Category
// filters case-, accent-, and punctuation-insensitive ("cafe" matches "Café",
// "o brien" matches "O'Brien").
func searchKey(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	lastSpace := false
	for _, r := range s {
		r = foldAccentRune(r)
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			lastSpace = false
			continue
		}
		if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// foldAccentRune maps common Latin-1 / Latin Extended-A accented letters to
// their ASCII base so accented spellings fold together during search.
func foldAccentRune(r rune) rune {
	switch r {
	case 'à', 'á', 'â', 'ã', 'ä', 'å', 'æ':
		return 'a'
	case 'ç':
		return 'c'
	case 'è', 'é', 'ê', 'ë':
		return 'e'
	case 'ì', 'í', 'î', 'ï':
		return 'i'
	case 'ñ':
		return 'n'
	case 'ò', 'ó', 'ô', 'õ', 'ö', 'ø':
		return 'o'
	case 'ù', 'ú', 'û', 'ü':
		return 'u'
	case 'ý', 'ÿ':
		return 'y'
	}
	return r
}

func fuzzyTextScore(a, b string) int {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 100
	}
	score := 0
	if s := containmentScore(a, b); s > score {
		score = s
	}
	// Space-stripped containment catches concatenated merchant names where the
	// statement spells out what the payee record runs together: "the home depot"
	// vs "homedepot".
	if s := containmentScore(stripSpaces(a), stripSpaces(b)); s > score {
		score = s
	}
	// Token-subset catches a clean multi-word payee buried in a noisy statement
	// description: "chevrolet riverton" inside "cadillac chevrolet buit riverton
	// ny".
	if s := tokenSubsetScore(a, b); s > score {
		score = s
	}
	distance := levenshteinDistance(a, b)
	longer := len(a)
	if len(b) > longer {
		longer = len(b)
	}
	if longer > 0 {
		if s := 100 - distance*100/longer; s > score {
			score = s
		}
	}
	if score < 0 {
		return 0
	}
	return score
}

// containmentScore scores one string fully containing the other, weighted by how
// much of the longer string the shorter one covers (so a tiny fragment inside a
// long string scores low).
func containmentScore(a, b string) int {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 100
	}
	if !strings.Contains(a, b) && !strings.Contains(b, a) {
		return 0
	}
	shorter, longer := len(a), len(b)
	if longer < shorter {
		shorter, longer = longer, shorter
	}
	// Guard against short fragments collapsing into long strings once spaces are
	// removed (e.g. a 3-letter name landing inside an unrelated description).
	if shorter < 4 {
		return 0
	}
	score := 86 + shorter*14/longer
	if score > 100 {
		return 100
	}
	return score
}

// tokenSubsetScore rewards b's significant tokens (length >= 3) all appearing in
// a. It only fires for multi-token b so a single common word can't pull an
// unrelated payee over the match threshold; partial overlap stays below it.
func tokenSubsetScore(a, b string) int {
	bTokens := significantTokens(b)
	if len(bTokens) < 2 {
		return 0
	}
	aTokens := map[string]bool{}
	for _, t := range strings.Fields(a) {
		aTokens[t] = true
	}
	matched := 0
	for _, t := range bTokens {
		if aTokens[t] {
			matched++
		}
	}
	if matched == 0 {
		return 0
	}
	return matched * 90 / len(bTokens)
}

func significantTokens(s string) []string {
	var tokens []string
	for _, t := range strings.Fields(s) {
		if len(t) >= 3 {
			tokens = append(tokens, t)
		}
	}
	return tokens
}

func stripSpaces(s string) string {
	return strings.ReplaceAll(s, " ", "")
}

// generalizePayeePattern turns a raw statement description into a learnable
// pattern: lowercased significant tokens with volatile parts (store/reference
// numbers, dates, amounts) dropped, so a pattern learned from one statement
// still matches future statements where only those volatile parts change.
func generalizePayeePattern(raw string) string {
	norm := normalizeSearchText(raw)
	var kept []string
	for _, tok := range strings.Fields(norm) {
		if len(tok) < 3 || isVolatileToken(tok) {
			continue
		}
		kept = append(kept, tok)
	}
	if len(kept) > 0 {
		return strings.Join(kept, " ")
	}
	// Never learn an empty pattern (e.g. an all-numeric description): fall back
	// to the longest token so there is still something to match on.
	longest := ""
	for _, tok := range strings.Fields(norm) {
		if len(tok) > len(longest) {
			longest = tok
		}
	}
	return longest
}

// isVolatileToken reports whether a token is the kind that changes between
// otherwise-identical statement lines: a pure number (store #, date, amount) or
// a longer mixed alphanumeric reference code. Short brandy tokens with a digit
// (e.g. "3m") are kept.
func isVolatileToken(tok string) bool {
	letters, digits := 0, 0
	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r >= 'a' && r <= 'z':
			letters++
		}
	}
	if digits == 0 {
		return false
	}
	if letters == 0 {
		return true
	}
	return len(tok) >= 5
}

// sharedSignificantTokens returns the significant tokens common to a and b, in
// the order they appear in a. Used to collapse a payee's learned patterns toward
// the merchant core they have in common.
func sharedSignificantTokens(a, b string) []string {
	inB := map[string]bool{}
	for _, t := range significantTokens(normalizeSearchText(b)) {
		inB[t] = true
	}
	seen := map[string]bool{}
	var common []string
	for _, t := range significantTokens(normalizeSearchText(a)) {
		if inB[t] && !seen[t] {
			common = append(common, t)
			seen[t] = true
		}
	}
	return common
}

func levenshteinDistance(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := 0; j <= len(b); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			curr[j] = minInt(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func minInt(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
