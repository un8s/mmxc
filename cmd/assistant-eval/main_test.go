package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/un8s/mmxc/internal/mmxc"
)

func TestDefaultAssistantEvalFiles(t *testing.T) {
	root := filepath.Join("..", "..")
	cases, err := loadCases(filepath.Join(root, "docs", "assistant_eval_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 10 {
		t.Fatalf("cases = %d, want at least 10", len(cases))
	}
	// The eval reuses the app's prompt builder (no template file to keep in sync).
	// Spot-check that the rendered prompt carries the case inputs and the key tool
	// guidance the scorer relies on.
	prompt := mmxc.BuildAssistantToolPrompt(mmxc.Prompts{}, "2026-06-19", []string{"Primary Checking"}, "How much did I spend?", nil)
	if !strings.Contains(prompt, "How much did I spend?") {
		t.Fatal("prompt is missing the question")
	}
	if !strings.Contains(prompt, "Primary Checking") {
		t.Fatal("prompt is missing the injected account names")
	}
	for _, want := range []string{
		`"tool":"ask_clarification"`,
		`"tool":"compare_summary"`,
		`The top-level action must be only "tool" or "final"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
}

func TestAssistantEvalCasesUseSyntheticNames(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "assistant_eval_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	banned := []string{
		"Amazon",
		"Costco",
		"Grocery Mart",
	}
	// Names from a maintainer's own data must not be listed here, since this file
	// is public. Put them one per line in the gitignored data/eval-banned-names.txt
	// instead; the check then covers them only where that file exists.
	if local, err := os.ReadFile(filepath.Join("..", "..", "data", "eval-banned-names.txt")); err == nil {
		for _, line := range strings.Split(string(local), "\n") {
			if name := strings.TrimSpace(line); name != "" && !strings.HasPrefix(name, "#") {
				banned = append(banned, name)
			}
		}
	}
	for _, name := range banned {
		if strings.Contains(text, name) {
			t.Fatalf("assistant eval cases contain non-synthetic/dev name %q", name)
		}
	}
}

func TestParseDecisionNormalizesLegacyClarification(t *testing.T) {
	decision, err := parseDecision(`{"action":"ask_clarification","question":"Which home loan category?","options":["Shelter: Home Loan Alpha","Shelter: Home Loan Beta"],"answer":""}`)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "tool" || decision.Tool != "ask_clarification" {
		t.Fatalf("decision = %+v, want ask_clarification tool call", decision)
	}
	if decision.Arguments["question"] != "Which home loan category?" {
		t.Fatalf("question = %v", decision.Arguments["question"])
	}
	options, ok := decision.Arguments["options"].([]any)
	if !ok || len(options) != 2 || options[0] != "Shelter: Home Loan Alpha" {
		t.Fatalf("options = %#v, want normalized []any", decision.Arguments["options"])
	}
}

func TestParseDecisionNormalizesClarificationAction(t *testing.T) {
	decision, err := parseDecision(`{"action":"ask_clarification","tool":"","arguments":{"question":"Which home loan category?","options":["Shelter: Home Loan Alpha","Shelter: Home Loan Beta"]},"answer":""}`)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "tool" || decision.Tool != "ask_clarification" {
		t.Fatalf("decision = %+v, want ask_clarification tool call", decision)
	}
	if decision.Arguments["question"] != "Which home loan category?" {
		t.Fatalf("question = %v", decision.Arguments["question"])
	}
}

func TestParseDecisionNormalizesToolNameAction(t *testing.T) {
	decision, err := parseDecision(`{"action":"read_transactions","tool":"","arguments":{"description_contains":"warehouse","limit":20},"answer":""}`)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "tool" || decision.Tool != "read_transactions" {
		t.Fatalf("decision = %+v, want read_transactions tool call", decision)
	}
	if decision.Arguments["description_contains"] != "warehouse" {
		t.Fatalf("description_contains = %v", decision.Arguments["description_contains"])
	}
}

func TestParseDecisionNormalizesDuplicatedToolNameAction(t *testing.T) {
	decision, err := parseDecision(`{"action":"find_account","tool":"find_account","arguments":{"query":"chequing"},"answer":""}`)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "tool" || decision.Tool != "find_account" {
		t.Fatalf("decision = %+v, want normalized tool action", decision)
	}
}

func TestScoreCaseAcceptsLegacySearchAlias(t *testing.T) {
	tc := evalCase{
		Expect: expectedDecision{
			Action: "tool",
			Tool:   "read_transactions",
			Arguments: map[string]any{
				"description_contains": "refund",
			},
		},
	}
	got := modelDecision{Action: "tool", Tool: "read_transactions", Arguments: map[string]any{"search": "refund"}}
	if errs := scoreCase(tc, got, "2026-06-19"); len(errs) != 0 {
		t.Fatalf("score errors = %v, want legacy search alias match", errs)
	}
}

func TestScoreCaseAcceptsAnyOf(t *testing.T) {
	tc := evalCase{
		AnyOf: []expectedDecision{
			{Action: "tool", Tool: "spending_summary", Arguments: map[string]any{"from": "2026-06-01"}},
			{Action: "tool", Tool: "spending_summary", Arguments: map[string]any{"from": "2026-05-01"}},
		},
	}
	got := modelDecision{Action: "tool", Tool: "spending_summary", Arguments: map[string]any{"from": "2026-05-01"}}
	if errs := scoreCase(tc, got, "2026-06-19"); len(errs) != 0 {
		t.Fatalf("score errors = %v, want any_of match", errs)
	}
}

func TestScoreCaseResolvesDatePlaceholders(t *testing.T) {
	tc := evalCase{
		Expect: expectedDecision{
			Action: "tool",
			Tool:   "spending_summary",
			Arguments: map[string]any{
				"from": "{{last_month_start}}",
				"to":   "{{last_month_end}}",
			},
		},
	}
	got := modelDecision{Action: "tool", Tool: "spending_summary", Arguments: map[string]any{"from": "2026-05-01", "to": "2026-05-31"}}
	if errs := scoreCase(tc, got, "2026-06-19"); len(errs) != 0 {
		t.Fatalf("score errors = %v, want placeholder match", errs)
	}
}

func TestScoreCaseAcceptsArgumentAnyOf(t *testing.T) {
	tc := evalCase{
		Expect: expectedDecision{
			Action: "tool",
			Tool:   "spending_summary",
			Arguments: map[string]any{
				"from": "{{current_month_start}}",
				"to": map[string]any{
					"any_of": []any{"{{current_date}}", "{{current_month_end}}"},
				},
			},
		},
	}
	got := modelDecision{Action: "tool", Tool: "spending_summary", Arguments: map[string]any{"from": "2026-06-01", "to": "2026-06-30"}}
	if errs := scoreCase(tc, got, "2026-06-19"); len(errs) != 0 {
		t.Fatalf("score errors = %v, want argument any_of match", errs)
	}
}
