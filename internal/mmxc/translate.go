package mmxc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// cacheRecheckInterval bounds how often a language's cache file is stat'ed. A
// page makes hundreds of translator lookups, so the file is checked at most
// this often per language; hand edits show up on a page load within a second.
const cacheRecheckInterval = time.Second

// cacheStamp identifies the version of a cache file that is in memory, so an
// edit made outside the process (a human correcting a translation) is noticed.
type cacheStamp struct {
	mod  time.Time
	size int64
}

// cacheEntry is one record in a translation cache file. Storing source and
// guidance alongside the translation makes the JSON file self-documenting so
// a human correcting a machine translation can see what the original said.
type cacheEntry struct {
	Source      string `json:"source"`
	Guidance    string `json:"guidance,omitempty"`
	Translation string `json:"translation"`
}

// translator caches LLM-generated UI translations per language. The cache is
// persisted to disk as a JSON file (one per language) under TranslationDir.
// Its settings (model, cache directory) are read at call time through a config
// pointer that is swapped as a whole (setConfig), never written through.
type translator struct {
	mu          sync.Mutex
	caches      map[string]map[string]cacheEntry // lang → (key → entry)
	stamps      map[string]cacheStamp            // lang → file version held in caches
	checked     map[string]time.Time             // lang → when the file was last stat'ed
	recheck     time.Duration
	cfg         atomic.Pointer[config]
	llm         *llmClient
	aiAvailable func() bool
}

func newTranslator(cfg *config, llm *llmClient) *translator {
	t := &translator{
		caches:  make(map[string]map[string]cacheEntry),
		stamps:  make(map[string]cacheStamp),
		checked: make(map[string]time.Time),
		recheck: cacheRecheckInterval,
		llm:     llm,
	}
	t.cfg.Store(cfg)
	return t
}

func (t *translator) setConfig(cfg *config) { t.cfg.Store(cfg) }

// get returns the translation of source into lang. It is cache-first: when AI
// is not configured or reachable, cached translations are still used and cache
// misses fall back to the English source string.
func (t *translator) get(lang, source, guidance string) string {
	if lang == "" || lang == "en" {
		return source
	}
	key := translationKey(source, guidance, lang)

	t.mu.Lock()
	t.ensureCache(lang)
	if e, ok := t.caches[lang][key]; ok {
		t.mu.Unlock()
		return e.Translation
	}
	t.mu.Unlock()

	model := strings.TrimSpace(t.cfg.Load().TranslationModel)
	if model == "" || (t.aiAvailable != nil && !t.aiAvailable()) {
		return source
	}
	result, err := t.callTranslate(lang, source, guidance)
	if err != nil {
		log.Printf("translate %s→%s: %v (falling back to English)", lang, short(source), err)
		return source
	}
	if !validateTranslationPlaceholders(source, result) {
		log.Printf("translate %s→%s: placeholder mismatch in %q, falling back to English", lang, short(source), result)
		return source
	}
	if !plausibleTranslationLength(source, result) {
		log.Printf("translate %s→%s: response is too long to be a translation (%q), falling back to English", lang, short(source), short(result))
		return source
	}

	t.mu.Lock()
	// Another goroutine may have stored a result while we were calling the LLM.
	if e, ok := t.caches[lang][key]; ok {
		t.mu.Unlock()
		return e.Translation
	}
	t.caches[lang][key] = cacheEntry{Source: source, Guidance: guidance, Translation: result}
	cacheCopy := make(map[string]cacheEntry, len(t.caches[lang]))
	for k, v := range t.caches[lang] {
		cacheCopy[k] = v
	}
	t.mu.Unlock()

	stamp, err := t.saveCacheToDisk(lang, cacheCopy)
	if err != nil {
		log.Printf("translate: save cache %s: %v", lang, err)
		return result
	}
	// Record our own write so it is not mistaken for an outside edit.
	t.mu.Lock()
	t.stamps[lang] = stamp
	t.mu.Unlock()
	return result
}

// ensureCache makes the in-memory cache for lang current with its file: it
// loads it on first use and reloads it when the file was changed outside the
// process, so a hand-corrected translation takes effect without a restart. The
// file wins over memory, so deleting an entry from it forces a retranslation.
// Must be called under t.mu.
func (t *translator) ensureCache(lang string) {
	_, loaded := t.caches[lang]
	now := time.Now()
	if loaded && now.Sub(t.checked[lang]) < t.recheck {
		return
	}
	t.checked[lang] = now

	fi, err := os.Stat(t.cacheFile(lang))
	if err != nil {
		if !loaded {
			t.caches[lang] = make(map[string]cacheEntry)
		} else if _, ok := t.stamps[lang]; ok {
			// The file we had read or written is gone.
			t.caches[lang] = make(map[string]cacheEntry)
			delete(t.stamps, lang)
		}
		return
	}
	stamp := cacheStamp{mod: fi.ModTime(), size: fi.Size()}
	if loaded && t.stamps[lang] == stamp {
		return
	}
	t.stamps[lang] = stamp
	m, ok := t.loadCacheFromDisk(lang)
	if !ok && loaded {
		return // unreadable (e.g. mid-edit): keep what we have until it changes again
	}
	t.caches[lang] = m
}

func (t *translator) cacheFile(lang string) string {
	return filepath.Join(t.cfg.Load().TranslationDir, lang+".json")
}

// loadCacheFromDisk reads lang's cache file; ok is false when it could not be
// read or parsed, in which case the returned map is empty.
func (t *translator) loadCacheFromDisk(lang string) (m map[string]cacheEntry, ok bool) {
	data, err := os.ReadFile(t.cacheFile(lang))
	if err != nil {
		return make(map[string]cacheEntry), false
	}
	if err := json.Unmarshal(data, &m); err != nil {
		log.Printf("translate: load cache %s: %v", lang, err)
		return make(map[string]cacheEntry), false
	}
	if m == nil {
		m = make(map[string]cacheEntry)
	}
	return m, true
}

// saveCacheToDisk writes lang's cache atomically (temp file + rename) so a
// concurrent reload never sees a half-written file, and returns the written
// file's stamp.
func (t *translator) saveCacheToDisk(lang string, cache map[string]cacheEntry) (cacheStamp, error) {
	dir := t.cfg.Load().TranslationDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return cacheStamp{}, err
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return cacheStamp{}, err
	}
	tmp, err := os.CreateTemp(dir, lang+".json.*")
	if err != nil {
		return cacheStamp{}, err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return cacheStamp{}, err
	}
	if err := tmp.Close(); err != nil {
		return cacheStamp{}, err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return cacheStamp{}, err
	}
	if err := os.Rename(tmp.Name(), t.cacheFile(lang)); err != nil {
		return cacheStamp{}, err
	}
	fi, err := os.Stat(t.cacheFile(lang))
	if err != nil {
		return cacheStamp{}, err
	}
	return cacheStamp{mod: fi.ModTime(), size: fi.Size()}, nil
}

// callTranslate asks the shared LLM client for one translation. It runs in
// JSON mode like every other call site: a bare-string response invites the
// model to answer with alternatives, an explanation, or — for a one-word
// source — a translation of the prompt instead of the string.
func (t *translator) callTranslate(lang, source, guidance string) (string, error) {
	model := strings.TrimSpace(t.cfg.Load().TranslationModel)
	if model == "" {
		return "", fmt.Errorf("translation model is not configured")
	}
	prompt := translationPrompt(lang, source, guidance)
	log.Printf("translate request: lang=%s model=%s source=%dB", lang, model, len(source))
	raw, err := t.llm.generate(model, prompt, nil, llmOptions{Temperature: temperature(0), JSONMode: true})
	if err != nil {
		return "", err
	}
	return parseTranslationResponse(raw)
}

func parseTranslationResponse(raw string) (string, error) {
	objText, err := extractJSONObject(raw)
	if err != nil {
		return "", err
	}
	var out struct {
		Translation string `json:"translation"`
	}
	if err := json.Unmarshal([]byte(objText), &out); err != nil {
		return "", fmt.Errorf("parse translation response: %w", err)
	}
	result := strings.TrimSpace(out.Translation)
	if result == "" {
		return "", fmt.Errorf("empty translation response")
	}
	return result, nil
}

// translationPrompt asks for the translation of one labelled field rather than
// of the whole prompt. The source and its guidance travel as JSON fields
// because a plain "English source string:" trailer left a short label such as
// "Translation" or "UI Language" indistinguishable from the surrounding prose:
// the model translated the app description or the guidance line and that
// paragraph was cached and rendered as the section heading.
func translationPrompt(lang, source, guidance string) string {
	var sb strings.Builder
	sb.WriteString("Translate the \"source\" field of the JSON object below into ")
	sb.WriteString(languageName(lang))
	sb.WriteString(".\n")
	sb.WriteString("You are translating MMXC, a personal-finance web app for accounts, budgets, payees, categories, statements, transactions, deposits, withdrawals, transfers, balances, and reconciliation.\n")
	sb.WriteString("Use financial/accounting terminology where a word is ambiguous.\n")
	sb.WriteString("Keep UI labels, buttons, table headings, validation messages, and short status text concise and natural for software UI.\n")
	sb.WriteString("Preserve product names and technical names such as MMXC, MMEX, LLM, AI, URL, PDF, CSS, JSON, and YYYY-MM-DD unless the target language convention clearly localizes them.\n")
	sb.WriteString("Preserve format placeholders (%s, %d, %.2f, etc.) and named placeholders ({n}, {cat}, etc.) exactly as they appear.\n")
	sb.WriteString("The \"context\" field says where the string appears. Use it only to pick the right sense of an ambiguous word: never translate it, never answer it, and never add words that appear only in it.\n")
	sb.WriteString("The translation renders the source alone, so it stays about as short as the source.\n")
	sb.WriteString("Reply with one JSON object: {\"translation\": \"<the translated source>\"}\n\n")
	payload := map[string]string{"source": source}
	if guidance != "" {
		payload["context"] = guidance
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// A map of strings cannot fail to encode; fall back to the bare source
		// rather than dropping the request.
		encoded = []byte(source)
	}
	sb.Write(encoded)
	return sb.String()
}

// translationKey returns a short hash used as the cache key for a (source,
// guidance, lang) triple. Using a hash keeps the JSON file keys stable even
// when source text contains special characters, and automatically handles the
// disambiguation role of guidance (identical English strings with different
// guidance get different keys).
func translationKey(source, guidance, lang string) string {
	h := sha256.Sum256([]byte(source + "\x00" + guidance + "\x00" + lang))
	return hex.EncodeToString(h[:8])
}

// maxTranslationGrowth bounds how much longer a translation may be than its
// source. A model that ignores the instructions and returns the prompt (or the
// guidance) instead of the string blows past this by an order of magnitude,
// while genuine expansion — German compounds, French articles — stays well
// inside it even for one-word labels, thanks to the constant slack.
const (
	maxTranslationGrowth = 4
	translationSlack     = 24
)

// plausibleTranslationLength rejects a response far too long to be a
// translation of source. Without it a bad response is cached to disk and the
// UI shows a paragraph where a button label belongs.
func plausibleTranslationLength(source, translation string) bool {
	return len([]rune(translation)) <= maxTranslationGrowth*len([]rune(source))+translationSlack
}

var placeholderRE = regexp.MustCompile(`%[-+ #0]?[0-9]*(?:\.[0-9]+)?[sdfevqtbxXoOpTU]`)

// validateTranslationPlaceholders checks that the translation contains the
// same sequence of format placeholders as the source string. A mismatch means
// the model garbled or dropped a placeholder, which would cause a runtime
// panic when the string is later passed to fmt.Sprintf.
func validateTranslationPlaceholders(source, translation string) bool {
	sp := placeholderRE.FindAllString(source, -1)
	tp := placeholderRE.FindAllString(translation, -1)
	if len(sp) != len(tp) {
		return false
	}
	for i, p := range sp {
		if p != tp[i] {
			return false
		}
	}
	return true
}

// languageName maps an ISO 639-1 code to a full English language name for
// use in the LLM prompt. Unknown codes are returned as-is.
func languageName(code string) string {
	names := map[string]string{
		"ar": "Arabic",
		"cs": "Czech",
		"da": "Danish",
		"de": "German",
		"el": "Greek",
		"es": "Spanish",
		"fi": "Finnish",
		"fr": "French",
		"hu": "Hungarian",
		"it": "Italian",
		"ja": "Japanese",
		"ko": "Korean",
		"nl": "Dutch",
		"no": "Norwegian",
		"pl": "Polish",
		"pt": "Portuguese",
		"ro": "Romanian",
		"ru": "Russian",
		"sv": "Swedish",
		"tr": "Turkish",
		"uk": "Ukrainian",
		"zh": "Chinese (Simplified)",
	}
	if name, ok := names[code]; ok {
		return name
	}
	return code
}
