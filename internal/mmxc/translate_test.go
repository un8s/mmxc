package mmxc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTranslationPromptIncludesFinancialContext(t *testing.T) {
	prompt := translationPrompt("de", "Balance", "account balance label")

	for _, want := range []string{
		"personal-finance web app",
		"accounts, budgets, payees, categories, statements, transactions",
		"Use financial/accounting terminology",
		`{"context":"account balance label","source":"Balance"}`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("translation prompt missing %q:\n%s", want, prompt)
		}
	}
}

// The source travels as a labelled JSON field, and the guidance is marked as
// background. Before that, a one-word label such as "Translation" was
// indistinguishable from the surrounding prose and the model answered with a
// translation of the app description or of the guidance line.
func TestTranslationPromptSeparatesSourceFromContext(t *testing.T) {
	prompt := translationPrompt("de", "Translation", "settings section heading")

	for _, want := range []string{
		`Translate the "source" field`,
		`never translate it, never answer it, and never add words that appear only in it`,
		`{"context":"settings section heading","source":"Translation"}`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("translation prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "English source string:") {
		t.Fatal("the source must not be appended as undelimited prose")
	}
}

func TestTranslationPromptOmitsAnEmptyContext(t *testing.T) {
	prompt := translationPrompt("fr", "UI Language", "")
	if !strings.HasSuffix(prompt, `{"source":"UI Language"}`) {
		t.Fatalf("prompt should end with a context-free payload:\n%s", prompt)
	}
}

func TestTranslationPromptProtectsPlaceholders(t *testing.T) {
	prompt := translationPrompt("fr", "Loaded {n} models and %s files.", "")

	for _, want := range []string{
		"Preserve format placeholders (%s, %d, %.2f, etc.)",
		"named placeholders ({n}, {cat}, etc.)",
		`{"source":"Loaded {n} models and %s files."}`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("translation prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestParseTranslationResponse(t *testing.T) {
	got, err := parseTranslationResponse("```json\n{\"translation\": \"  Übersetzung \"}\n```")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != "Übersetzung" {
		t.Fatalf("translation = %q", got)
	}
	if _, err := parseTranslationResponse(`{"translation": "  "}`); err == nil {
		t.Fatal("an empty translation must be an error, not a blank label")
	}
	if _, err := parseTranslationResponse("Übersetzung"); err == nil {
		t.Fatal("a non-JSON response must be an error")
	}
}

// A model that answers a short label with the prompt itself must not have that
// answer cached and shown as a button label.
func TestPlausibleTranslationLength(t *testing.T) {
	cases := []struct {
		source, translation string
		want                bool
	}{
		{"Translation", "Übersetzung", true},
		{"Net", "Nettobetrag", true},
		{"UI Language", "Sprache der Benutzeroberfläche", true},
		{"Save Settings", "Einstellungen speichern", true},
		{"Translation", "Sie sind MMXC, eine persönliche Finanz-Webanwendung für Konten, Budgets, Empfänger, Kategorien.", false},
		{"Net", "Montant du budget net après revenus moins dépenses", false},
	}
	for _, tc := range cases {
		if got := plausibleTranslationLength(tc.source, tc.translation); got != tc.want {
			t.Errorf("plausibleTranslationLength(%q, %q) = %t, want %t", tc.source, tc.translation, got, tc.want)
		}
	}
}

func TestTranslateReportNavUsesGuidance(t *testing.T) {
	cfg := defaultConfig()
	cfg.TranslationLanguage = "de"
	cfg.TranslationModel = ""
	tr := newTranslator(&cfg, newLLMClient(&cfg))
	tr.caches["de"] = map[string]cacheEntry{
		translationKey("Overview", "financial report name", "de"): {
			Source:      "Overview",
			Guidance:    "financial report name",
			Translation: "Uebersicht",
		},
	}
	a := &app{config: cfg, tr: tr}

	got := a.translateReportNav([]reportNavItem{{ID: "overview", Name: "Overview"}})

	if got[0].Name != "Uebersicht" {
		t.Fatalf("translated report name = %q, want cached guidance-specific translation", got[0].Name)
	}
}

func TestHTTPErrorGuidanceUsesFinancialContext(t *testing.T) {
	cases := map[string]string{
		"amount must be greater than zero": "money amount",
		"account id is required":           "financial account",
		"category name is required":        "transaction categories",
		"payee name is required":           "transaction payees",
		"transaction id is required":       "financial transaction",
	}
	for msg, want := range cases {
		if got := httpErrorGuidance(msg); !strings.Contains(got, want) {
			t.Fatalf("httpErrorGuidance(%q) = %q, want substring %q", msg, got, want)
		}
	}
}

func TestMonthlyReviewStatusUsesGuidance(t *testing.T) {
	cfg := defaultConfig()
	cfg.TranslationLanguage = "de"
	cfg.TranslationModel = ""
	tr := newTranslator(&cfg, newLLMClient(&cfg))
	guidance := "monthly account review and bank statement reconciliation status"
	tr.caches["de"] = map[string]cacheEntry{
		translationKey("Statements through %s", guidance, "de"): {
			Source:      "Statements through %s",
			Guidance:    guidance,
			Translation: "Kontoauszuege bis %s",
		},
	}
	a := &app{config: cfg, tr: tr}

	got := a.monthlyReviewStatus("Statements through %s")

	if got != "Kontoauszuege bis %s" {
		t.Fatalf("monthly review status = %q, want cached guidance-specific translation", got)
	}
}

func TestImportCandidateMessageTextUsesGuidance(t *testing.T) {
	cfg := defaultConfig()
	cfg.TranslationLanguage = "de"
	cfg.TranslationModel = ""
	tr := newTranslator(&cfg, newLLMClient(&cfg))
	guidance := "bank statement transaction import validation message"
	tr.caches["de"] = map[string]cacheEntry{
		translationKey("Possible duplicate", guidance, "de"): {
			Source:      "Possible duplicate",
			Guidance:    guidance,
			Translation: "Moegliches Duplikat",
		},
		translationKey("Split copied from %s — confirm", guidance, "de"): {
			Source:      "Split copied from %s — confirm",
			Guidance:    guidance,
			Translation: "Split von %s kopiert - bestaetigen",
		},
	}
	a := &app{config: cfg, tr: tr}

	if got := a.importCandidateMessageText("Possible duplicate"); got != "Moegliches Duplikat" {
		t.Fatalf("candidate message = %q, want cached guidance-specific translation", got)
	}
	if got := a.importCandidateMessageText("Split copied from 2025-08-10 — confirm"); got != "Split von 2025-08-10 kopiert - bestaetigen" {
		t.Fatalf("candidate split message = %q, want formatted guidance-specific translation", got)
	}
}

func writeTranslationCache(t *testing.T, tr *translator, lang string, entries map[string]cacheEntry, mod time.Time) {
	t.Helper()
	if _, err := tr.saveCacheToDisk(lang, entries); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	if err := os.Chtimes(tr.cacheFile(lang), mod, mod); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// A human correcting a translation in the JSON file must not need a restart.
func TestTranslatorReloadsCacheEditedOutsideTheProcess(t *testing.T) {
	cfg := defaultConfig()
	cfg.TranslationDir = t.TempDir()
	cfg.TranslationModel = "" // cache only: a miss falls back to English
	tr := newTranslator(&cfg, newLLMClient(&cfg))
	tr.recheck = 0 // stat on every lookup

	key := translationKey("Balance", "", "de")
	base := time.Now().Add(-time.Hour)
	writeTranslationCache(t, tr, "de", map[string]cacheEntry{key: {Source: "Balance", Translation: "Kontostand"}}, base)

	if got := tr.get("de", "Balance", ""); got != "Kontostand" {
		t.Fatalf("first read = %q, want Kontostand", got)
	}

	writeTranslationCache(t, tr, "de", map[string]cacheEntry{key: {Source: "Balance", Translation: "Saldo"}}, base.Add(time.Minute))
	if got := tr.get("de", "Balance", ""); got != "Saldo" {
		t.Fatalf("after edit = %q, want Saldo", got)
	}

	// Deleting an entry drops it; the file wins over memory.
	writeTranslationCache(t, tr, "de", map[string]cacheEntry{}, base.Add(2*time.Minute))
	if got := tr.get("de", "Balance", ""); got != "Balance" {
		t.Fatalf("after delete = %q, want the English fallback", got)
	}
}

// A file caught mid-edit (invalid JSON) must not wipe the translations we hold.
func TestTranslatorKeepsCacheWhenFileBecomesUnreadable(t *testing.T) {
	cfg := defaultConfig()
	cfg.TranslationDir = t.TempDir()
	cfg.TranslationModel = ""
	tr := newTranslator(&cfg, newLLMClient(&cfg))
	tr.recheck = 0

	key := translationKey("Balance", "", "de")
	base := time.Now().Add(-time.Hour)
	writeTranslationCache(t, tr, "de", map[string]cacheEntry{key: {Source: "Balance", Translation: "Kontostand"}}, base)
	tr.get("de", "Balance", "")

	path := filepath.Join(cfg.TranslationDir, "de.json")
	if err := os.WriteFile(path, []byte(`{"broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tr.get("de", "Balance", ""); got != "Kontostand" {
		t.Fatalf("with invalid file = %q, want the cached Kontostand", got)
	}
}

// The translator's own write must not read back as an outside edit.
func TestTranslatorDoesNotReloadItsOwnWrite(t *testing.T) {
	cfg := defaultConfig()
	cfg.TranslationDir = t.TempDir()
	cfg.TranslationModel = ""
	tr := newTranslator(&cfg, newLLMClient(&cfg))
	tr.recheck = 0

	key := translationKey("Balance", "", "de")
	tr.get("de", "Balance", "") // loads the (absent) file
	tr.caches["de"][key] = cacheEntry{Source: "Balance", Translation: "Kontostand"}
	stamp, err := tr.saveCacheToDisk("de", tr.caches["de"])
	if err != nil {
		t.Fatal(err)
	}
	tr.stamps["de"] = stamp

	// Mutate memory only; an unneeded reload would discard this.
	tr.caches["de"][key] = cacheEntry{Source: "Balance", Translation: "in-memory"}
	if got := tr.get("de", "Balance", ""); got != "in-memory" {
		t.Fatalf("got %q: own write was reloaded", got)
	}
}
