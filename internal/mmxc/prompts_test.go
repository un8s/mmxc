package mmxc

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writePromptOverride(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParsePromptFileStripsLeadingComments(t *testing.T) {
	p := parsePromptFile("# mmxc-prompt-version: 3\r\n# copied for my bank\r\n\r\nFirst rule.\r\n# not a comment once guidance started\r\n")
	if p.version != 3 {
		t.Fatalf("version = %d, want 3", p.version)
	}
	if want := "First rule.\n# not a comment once guidance started"; p.text != want {
		t.Fatalf("text = %q, want %q", p.text, want)
	}
	if p := parsePromptFile("Only guidance."); p.version != 0 || p.text != "Only guidance." {
		t.Fatalf("no header: got %+v", p)
	}
}

func TestBuiltinPromptsCarryCurrentVersion(t *testing.T) {
	for _, name := range promptNames {
		p := builtinPrompts[name]
		if p.version == 0 || p.text == "" {
			t.Fatalf("built-in %s = %+v", name, p)
		}
		if strings.HasPrefix(p.text, "#") {
			t.Fatalf("built-in %s still starts with a comment line", name)
		}
	}
}

func TestPromptsWithoutOverrideUseBuiltin(t *testing.T) {
	for _, prompts := range []Prompts{{}, {Dir: t.TempDir()}} {
		for _, status := range prompts.Status() {
			if status.Custom || status.Path != "" || status.Warning != "" {
				t.Fatalf("Dir %q: status = %+v, want built-in", prompts.Dir, status)
			}
			if got := prompts.guidance(status.Name); got != builtinPrompts[status.Name].text {
				t.Fatalf("Dir %q: %s guidance is not the built-in text", prompts.Dir, status.Name)
			}
		}
	}
}

func TestAssistantPromptOverrideKeepsContract(t *testing.T) {
	dir := t.TempDir()
	version := builtinPrompts[promptAssistant].version
	writePromptOverride(t, dir, promptAssistant, "# mmxc-prompt-version: "+strconv.Itoa(version)+"\nPrefer the Groceries category for Greenleaf.\n")
	prompts := Prompts{Dir: dir}

	prompt := BuildAssistantToolPrompt(prompts, "2026-06-19", []string{"Primary Checking"}, "How much at Greenleaf?", nil)
	if !strings.Contains(prompt, "Prefer the Groceries category for Greenleaf.") {
		t.Fatal("override guidance missing from prompt")
	}
	if strings.Contains(prompt, "mmxc-prompt-version") {
		t.Fatal("version header leaked into the prompt")
	}
	if strings.Contains(prompt, "Use compare_summary for compare questions") {
		t.Fatal("built-in guidance still present although overridden")
	}
	for _, contract := range []string{
		"The top-level keys must be exactly action, tool, arguments, and answer.",
		"- read_transactions {",
		`sort must be "", "date_desc", "date_asc", "amount_desc", or "amount_asc".`,
		"Current date: 2026-06-19",
		"Accounts: Primary Checking",
		"Question: How much at Greenleaf?",
	} {
		if !strings.Contains(prompt, contract) {
			t.Fatalf("contract line %q missing from overridden prompt", contract)
		}
	}
	status := prompts.AssistantStatus()
	if !status.Custom || status.Warning != "" || status.Path != filepath.Join(dir, "assistant.txt") {
		t.Fatalf("status = %+v", status)
	}
}

func TestStatementPromptOverrideKeepsContract(t *testing.T) {
	dir := t.TempDir()
	writePromptOverride(t, dir, promptStatement, "# mmxc-prompt-version: 1\nDo not mistake a Blue Token for an account number.\n")
	prompt := statementTextPrompt(Prompts{Dir: dir}, "2026-06-19", "sample statement text", []string{"Food: Groceries"})
	for _, want := range []string{
		"Do not mistake a Blue Token for an account number.",
		`"accounts": [`,
		"description must be the original transaction description",
		"use the current year (current date: 2026-06-19)",
		"Existing categories",
		"Statement text:\nsample statement text",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt is missing %q", want)
		}
	}
	if strings.Contains(statementTextPrompt(Prompts{}, "2026-06-19", "sample statement text", nil), "Blue Token") {
		t.Fatal("bank-specific wording leaked into the built-in statement prompt")
	}
}

func TestPromptOverrideWarnings(t *testing.T) {
	dir := t.TempDir()
	prompts := Prompts{Dir: dir}

	writePromptOverride(t, dir, promptAssistant, "Guidance without a header.")
	if s := prompts.AssistantStatus(); !s.Custom || !strings.Contains(s.Warning, "has no") {
		t.Fatalf("missing header: %+v", s)
	}

	writePromptOverride(t, dir, promptAssistant, "# mmxc-prompt-version: 999\nGuidance.")
	if s := prompts.AssistantStatus(); !s.Custom || !strings.Contains(s.Warning, "version 999") {
		t.Fatalf("stale version: %+v", s)
	}

	// An empty override would strip all guidance; the built-in is used instead.
	writePromptOverride(t, dir, promptAssistant, "# mmxc-prompt-version: 1\n\n")
	if s := prompts.AssistantStatus(); s.Custom || !strings.Contains(s.Warning, "is empty") {
		t.Fatalf("empty override: %+v", s)
	}
	if prompts.guidance(promptAssistant) != builtinPrompts[promptAssistant].text {
		t.Fatal("empty override did not fall back to the built-in guidance")
	}
}

func TestDownloadBuiltinPrompt(t *testing.T) {
	a := &app{}
	rec := httptest.NewRecorder()
	a.downloadBuiltinPrompt(rec, httptest.NewRequest(http.MethodGet, "/settings/prompt?name=statement", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.HasPrefix(rec.Body.String(), "# mmxc-prompt-version:") {
		t.Fatalf("body does not start with the version header: %q", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, `filename="statement.txt"`) {
		t.Fatalf("Content-Disposition = %q", got)
	}

	rec = httptest.NewRecorder()
	a.downloadBuiltinPrompt(rec, httptest.NewRequest(http.MethodGet, "/settings/prompt?name=../ui.go", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown prompt status = %d, want 404", rec.Code)
	}
}
