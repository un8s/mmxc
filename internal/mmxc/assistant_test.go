package mmxc

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAssistantToolDecisionRejectsUnknownFields(t *testing.T) {
	_, err := parseAssistantToolDecision(`{"action":"tool","tool":"spending_summary","arguments":{},"sql":"select * from CHECKINGACCOUNT_V1","answer":""}`)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("parseAssistantToolDecision unknown field error = %v, want unknown field", err)
	}
}

func TestParseAssistantToolDecisionNormalizesLegacyClarification(t *testing.T) {
	decision, err := parseAssistantToolDecision(`{"action":"ask_clarification","question":"Which category?","options":["A","B"],"answer":""}`)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "tool" || decision.Tool != "ask_clarification" {
		t.Fatalf("decision = %+v, want ask_clarification tool call", decision)
	}
	var args assistantClarificationArgs
	if err := decodeAssistantToolArgs(decision.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args.Question != "Which category?" || len(args.Options) != 2 || args.Options[0] != "A" || args.Options[1] != "B" {
		t.Fatalf("args = %+v, want normalized clarification args", args)
	}
}

func TestParseAssistantToolDecisionNormalizesClarificationAction(t *testing.T) {
	decision, err := parseAssistantToolDecision(`{"action":"ask_clarification","tool":"","arguments":{"question":"Which category?","options":["A","B"]},"answer":""}`)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "tool" || decision.Tool != "ask_clarification" {
		t.Fatalf("decision = %+v, want ask_clarification tool call", decision)
	}
}

func TestParseAssistantToolDecisionNormalizesToolNameAction(t *testing.T) {
	decision, err := parseAssistantToolDecision(`{"action":"read_transactions","tool":"","arguments":{"search":"warehouse","limit":20},"answer":""}`)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "tool" || decision.Tool != "read_transactions" {
		t.Fatalf("decision = %+v, want read_transactions tool call", decision)
	}
}

func TestParseAssistantToolDecisionNormalizesDuplicatedToolNameAction(t *testing.T) {
	decision, err := parseAssistantToolDecision(`{"action":"find_account","tool":"find_account","arguments":{"query":"chequing"},"answer":""}`)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "tool" || decision.Tool != "find_account" {
		t.Fatalf("decision = %+v, want normalized tool action", decision)
	}
}

func TestAssistantToolRepairPromptIncludesCorrectionContext(t *testing.T) {
	prompt := assistantToolRepairPrompt("ORIGINAL PROMPT", `{"action":"bad"}`, errors.New("unsupported tool action"))
	for _, want := range []string{
		"ORIGINAL PROMPT",
		"unsupported tool action",
		`{"action":"bad"}`,
		`action must be "tool" or "final"`,
		"Do not include unknown fields.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("repair prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestAssistantToolPromptIncludesCountAndPayeeCategoryGuidance(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	prompt := a.assistantToolPrompt("How often did I buy at Greenleaf?", nil)
	for _, want := range []string{
		`find_account {"query": "text"}`,
		"chequing",
		"Use account filters only with exact names",
		`"how often"`,
		"spending_summary.count is the matching transaction count",
		"Do not use read_transactions to count matching transactions",
		"do not add a category filter unless the user explicitly asked for that category",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("assistant tool prompt missing %q", want)
		}
	}
}

func TestAppendAssistantFeedbackQuestionStoresOnlyQuestionLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant-feedback.txt")
	if err := appendAssistantFeedbackQuestion(path, "  How much did I spend\non groceries?\t "); err != nil {
		t.Fatal(err)
	}
	if err := appendAssistantFeedbackQuestion(path, "Why is rent missing?"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	want := "How much did I spend on groceries?\nWhy is rent missing?\n"
	if got != want {
		t.Fatalf("feedback file = %q, want %q", got, want)
	}
}

func TestAssistantFeedbackSavedURLPreservesReturnTarget(t *testing.T) {
	got := assistantFeedbackSavedURL("/assistant?q=rent")
	if got != "/assistant?feedback=saved&q=rent" && got != "/assistant?q=rent&feedback=saved" {
		t.Fatalf("saved URL = %q, want feedback query on assistant URL", got)
	}
}

func TestExecuteAssistantFindAccountToolMatchesChequingType(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	result, txs, totals, warnings, err := a.executeAssistantTool(
		"Show my chequing account transactions.",
		"find_account",
		json.RawMessage(`{"query":"chequing"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 0 || totals.Count != 0 || len(warnings) != 0 {
		t.Fatalf("unexpected side effects: txs=%d totals=%+v warnings=%v", len(txs), totals, warnings)
	}
	rows, ok := result.([]map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want []map[string]any", result)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %#v, want one chequing account", rows)
	}
	if rows[0]["name"] != "Everyday Chequing" || rows[0]["type"] != "Checking" {
		t.Fatalf("account row = %#v, want Everyday Chequing Checking", rows[0])
	}
}

func TestDecodeAssistantToolArgsRejectsUnknownFields(t *testing.T) {
	var args assistantToolFilterArgs
	err := decodeAssistantToolArgs(json.RawMessage(`{"date_preset":"all_time"}`), &args)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("decodeAssistantToolArgs unknown field error = %v, want unknown field", err)
	}
}

func TestAssistantToolFilterUsesDescriptionContains(t *testing.T) {
	filter, _, err := (&app{}).assistantToolFilter(
		"Show refund deposits",
		assistantToolFilterArgs{DescriptionContains: "refund", Type: "Deposit"},
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if filter.Search != "refund" || filter.Type != "Deposit" {
		t.Fatalf("filter search/type = %q/%q, want refund/Deposit", filter.Search, filter.Type)
	}
}

func TestAssistantToolFilterAcceptsLegacySearchAlias(t *testing.T) {
	filter, _, err := (&app{}).assistantToolFilter(
		"Show refund deposits",
		assistantToolFilterArgs{Search: "refund", Type: "Deposit"},
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if filter.Search != "refund" || filter.Type != "Deposit" {
		t.Fatalf("filter search/type = %q/%q, want refund/Deposit", filter.Search, filter.Type)
	}
}

func TestAssistantToolFilterPreservesExplicitDates(t *testing.T) {
	filter, _, err := (&app{}).assistantToolFilter(
		"Show transactions in November 2025",
		assistantToolFilterArgs{From: "2025-05-05", To: "2025-05-12", Sort: "date_desc", Limit: 20},
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if filter.From != "2025-05-05" || filter.To != "2025-05-12" {
		t.Fatalf("filter dates = %q to %q, want explicit tool dates", filter.From, filter.To)
	}
}

func TestAssistantReplayURL(t *testing.T) {
	got := assistantReplayURL(
		"Show groceries",
		"read_transactions",
		json.RawMessage(`{"category":"Food: Groceries","type":"Withdrawal","limit":20}`),
	)
	if !strings.HasPrefix(got, "/assistant?") {
		t.Fatalf("replay URL = %q, want assistant URL", got)
	}
	if !strings.Contains(got, "q=Show+groceries") || !strings.Contains(got, "tool=read_transactions") || !strings.Contains(got, "args=") {
		t.Fatalf("replay URL = %q, missing prompt/tool/args", got)
	}
}

func TestExecuteAssistantSpendingSummaryTool(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	result, txs, totals, warnings, err := a.executeAssistantTool(
		"How much did I spend on groceries?",
		"spending_summary",
		json.RawMessage(`{"category":"Food: Groceries","type":"Withdrawal"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 2 {
		t.Fatalf("transactions = %d, want 2", len(txs))
	}
	if totals.Withdrawals != 45 {
		t.Fatalf("withdrawals = %.2f, want 45.00", totals.Withdrawals)
	}
	summary, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want map", result)
	}
	if summary["withdrawals"] != 45.0 {
		t.Fatalf("summary withdrawals = %#v, want 45", summary["withdrawals"])
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "Category totals") {
		t.Fatalf("warnings = %v, want category split warning", warnings)
	}
}

func TestExecuteAssistantCompareSummaryTool(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	result, txs, totals, warnings, err := a.executeAssistantTool(
		"Compare groceries in August vs July.",
		"compare_summary",
		json.RawMessage(`{"period_a_label":"August","period_a_from":"2025-08-01","period_a_to":"2025-08-31","period_b_label":"July","period_b_from":"2025-07-01","period_b_to":"2025-07-31","category":"Food: Groceries","type":"Withdrawal"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 2 {
		t.Fatalf("transactions = %d, want period A then period B rows", len(txs))
	}
	if txs[0].Date != "2025-08-10" || txs[1].Date != "2025-07-05" {
		t.Fatalf("transaction dates = %s, %s, want period A then period B", txs[0].Date, txs[1].Date)
	}
	if totals.Withdrawals != 40 {
		t.Fatalf("period A withdrawals = %.2f, want 40.00", totals.Withdrawals)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "Category totals") {
		t.Fatalf("warnings = %v, want category split warning", warnings)
	}
	compare, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want map", result)
	}
	periodA, ok := compare["period_a"].(map[string]any)
	if !ok {
		t.Fatalf("period_a type = %T, want map", compare["period_a"])
	}
	periodB, ok := compare["period_b"].(map[string]any)
	if !ok {
		t.Fatalf("period_b type = %T, want map", compare["period_b"])
	}
	if periodA["withdrawals"] != 40.0 || periodB["withdrawals"] != 5.0 || compare["delta"] != 35.0 {
		t.Fatalf("compare result = %#v, want 40 vs 5 delta 35", compare)
	}
}

func TestExecuteAssistantCompareSummaryToolRequiresPeriodDates(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	_, _, _, _, err = a.executeAssistantTool(
		"Compare groceries.",
		"compare_summary",
		json.RawMessage(`{"period_a_label":"A","period_a_from":"","period_a_to":"2025-08-31","period_b_label":"B","period_b_from":"2025-07-01","period_b_to":"2025-07-31","category":"Food: Groceries","type":"Withdrawal"}`),
	)
	if err == nil || !strings.Contains(err.Error(), "compare_summary requires") {
		t.Fatalf("compare error = %v, want required dates", err)
	}
}

func TestExecuteAssistantReadTransactionsToolCapsLimit(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	_, txs, _, _, err := a.executeAssistantTool(
		"Show transactions",
		"read_transactions",
		json.RawMessage(`{"limit":500}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) > 50 {
		t.Fatalf("transactions = %d, want capped at 50", len(txs))
	}
}

func TestExecuteAssistantReadTransactionsToolForLastPaymentUsesOpenDateRange(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	_, txs, _, _, err := a.executeAssistantTool(
		"When was the last grocery payment?",
		"read_transactions",
		json.RawMessage(`{"category":"Food: Groceries","type":"Withdrawal","sort":"date_desc","limit":1}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 1 {
		t.Fatalf("transactions = %d, want 1", len(txs))
	}
}

func TestExecuteAssistantToolRequiresExactPayeeName(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	_, _, _, _, err = a.executeAssistantTool(
		"Show Grocery payments",
		"read_transactions",
		json.RawMessage(`{"payee":"Grocery","type":"Withdrawal"}`),
	)
	if err == nil || !strings.Contains(err.Error(), `payee "Grocery" was not found`) {
		t.Fatalf("partial payee error = %v, want exact-name failure", err)
	}

	_, _, _, _, err = a.executeAssistantTool(
		"Show Grocery Mart payments",
		"read_transactions",
		json.RawMessage(`{"payee":"Grocery Mart","type":"Withdrawal"}`),
	)
	if err != nil {
		t.Fatalf("exact payee should resolve: %v", err)
	}
}

func TestExecuteAssistantToolRequiresExactCategoryPath(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	_, _, _, _, err = a.executeAssistantTool(
		"Show groceries",
		"read_transactions",
		json.RawMessage(`{"category":"Groceries","type":"Withdrawal"}`),
	)
	if err == nil || !strings.Contains(err.Error(), `category "Groceries" was not found`) {
		t.Fatalf("bare category error = %v, want exact path failure", err)
	}

	_, _, _, _, err = a.executeAssistantTool(
		"Show groceries",
		"read_transactions",
		json.RawMessage(`{"category":"Food: Groceries","type":"Withdrawal"}`),
	)
	if err != nil {
		t.Fatalf("exact category path should resolve: %v", err)
	}
}

func TestExecuteAssistantToolSupportsUncategorizedFilter(t *testing.T) {
	a, err := newApp(config{
		DBPath:       fixtureDB(t),
		WriteEnabled: false,
		BackupDir:    "backups",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	_, _, _, warnings, err := a.executeAssistantTool(
		"Which transactions are uncategorized?",
		"read_transactions",
		json.RawMessage(`{"uncategorized":true,"sort":"date_desc","limit":20}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "Uncategorized") {
		t.Fatalf("warnings = %v, want uncategorized warning", warnings)
	}

	_, _, _, _, err = a.executeAssistantTool(
		"Which transactions are uncategorized?",
		"read_transactions",
		json.RawMessage(`{"category":"__blank","sort":"date_desc","limit":20}`),
	)
	if err != nil {
		t.Fatalf("__blank category alias should be treated as uncategorized: %v", err)
	}
}

func TestAssistantCarryForwardSinglePayeeLookup(t *testing.T) {
	transcript := []map[string]any{
		{
			"tool": "find_payee",
			"result": []map[string]any{
				{"id": 274, "name": "TaleCast", "active": true},
			},
		},
	}
	raw, warnings := assistantCarryForwardLookupArguments(
		"spending_summary",
		json.RawMessage(`{"type":"Withdrawal","amount_min":0,"amount_max":0}`),
		transcript,
	)
	var args assistantToolFilterArgs
	if err := decodeAssistantToolArgs(raw, &args); err != nil {
		t.Fatal(err)
	}
	if args.Payee != "TaleCast" {
		t.Fatalf("payee = %q, want TaleCast", args.Payee)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], `payee "TaleCast"`) {
		t.Fatalf("warnings = %v, want carry-forward warning", warnings)
	}
}

func TestAssistantCarryForwardSingleAccountLookup(t *testing.T) {
	transcript := []map[string]any{
		{
			"tool": "find_account",
			"result": []map[string]any{
				{"id": 12, "name": "Everyday Chequing", "type": "Checking"},
			},
		},
	}
	raw, warnings := assistantCarryForwardLookupArguments(
		"read_transactions",
		json.RawMessage(`{"sort":"date_desc","limit":20}`),
		transcript,
	)
	var args assistantToolFilterArgs
	if err := decodeAssistantToolArgs(raw, &args); err != nil {
		t.Fatal(err)
	}
	if args.Account != "Everyday Chequing" {
		t.Fatalf("account = %q, want Everyday Chequing", args.Account)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], `account "Everyday Chequing"`) {
		t.Fatalf("warnings = %v, want carry-forward warning", warnings)
	}
}

func TestAssistantCarryForwardSinglePayeeLookupFromParsedTranscript(t *testing.T) {
	transcript, err := parseAssistantToolTranscript(`[{"tool":"find_payee","result":[{"id":274,"name":"TaleCast","active":true}]}]`)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := assistantCarryForwardLookupArguments(
		"spending_summary",
		json.RawMessage(`{"type":"Withdrawal"}`),
		transcript,
	)
	var args assistantToolFilterArgs
	if err := decodeAssistantToolArgs(raw, &args); err != nil {
		t.Fatal(err)
	}
	if args.Payee != "TaleCast" {
		t.Fatalf("payee = %q, want TaleCast", args.Payee)
	}
}

func TestAssistantCarryForwardDoesNotOverrideExistingFilter(t *testing.T) {
	transcript := []map[string]any{
		{
			"tool": "find_payee",
			"result": []map[string]any{
				{"name": "TaleCast"},
			},
		},
	}
	raw, warnings := assistantCarryForwardLookupArguments(
		"spending_summary",
		json.RawMessage(`{"payee":"Amazon","type":"Withdrawal"}`),
		transcript,
	)
	var args assistantToolFilterArgs
	if err := decodeAssistantToolArgs(raw, &args); err != nil {
		t.Fatal(err)
	}
	if args.Payee != "Amazon" {
		t.Fatalf("payee = %q, want Amazon", args.Payee)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

func TestAssistantCarryForwardIgnoresAmbiguousLookup(t *testing.T) {
	transcript := []map[string]any{
		{
			"tool": "find_payee",
			"result": []map[string]any{
				{"name": "TaleCast"},
				{"name": "TaleCast Trial"},
			},
		},
	}
	raw, _ := assistantCarryForwardLookupArguments(
		"spending_summary",
		json.RawMessage(`{"type":"Withdrawal"}`),
		transcript,
	)
	var args assistantToolFilterArgs
	if err := decodeAssistantToolArgs(raw, &args); err != nil {
		t.Fatal(err)
	}
	if args.Payee != "" {
		t.Fatalf("payee = %q, want empty for ambiguous lookup", args.Payee)
	}
}

func TestExecuteAssistantClarificationTool(t *testing.T) {
	result, txs, totals, warnings, err := (&app{}).executeAssistantTool(
		"When was the last mortgage payment?",
		"ask_clarification",
		json.RawMessage(`{"question":"Which Mortgage category should I use?","options":["Housing: Mortgage","Housing: Mortgage","Rental: Mortgage Fees",""]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 0 || totals.Count != 0 || len(warnings) != 0 {
		t.Fatalf("unexpected side effects: txs=%d totals=%+v warnings=%v", len(txs), totals, warnings)
	}
	clarification, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want map", result)
	}
	options, ok := clarification["options"].([]string)
	if !ok {
		t.Fatalf("options type = %T, want []string", clarification["options"])
	}
	if len(options) != 2 || options[0] != "Housing: Mortgage" || options[1] != "Rental: Mortgage Fees" {
		t.Fatalf("options = %#v, want deduplicated category choices", options)
	}
	answer := assistantClarificationAnswer(result)
	if answer != "Which Mortgage category should I use?" {
		t.Fatalf("clarification answer = %q", answer)
	}
	question, fieldsOptions := assistantClarificationFields(result)
	if question != "Which Mortgage category should I use?" || len(fieldsOptions) != 2 {
		t.Fatalf("clarification fields = %q %#v, want question and two options", question, fieldsOptions)
	}
}

func TestExecuteAssistantClarificationToolRequiresQuestion(t *testing.T) {
	_, _, _, _, err := (&app{}).executeAssistantTool(
		"When was the last mortgage payment?",
		"ask_clarification",
		json.RawMessage(`{"question":"","options":["Housing: Mortgage"]}`),
	)
	if err == nil || !strings.Contains(err.Error(), "question is required") {
		t.Fatalf("clarification error = %v, want required question", err)
	}
}

func TestParseAssistantToolTranscript(t *testing.T) {
	transcript, err := parseAssistantToolTranscript(`[{"tool":"ask_clarification","result":{"question":"Which category?","options":["A","B"]}},{"user":"clarification","answer":"A"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 2 {
		t.Fatalf("transcript entries = %d, want 2", len(transcript))
	}
	if transcript[1]["answer"] != "A" {
		t.Fatalf("clarification answer = %#v, want A", transcript[1]["answer"])
	}
}

func TestParseAssistantToolTranscriptRejectsInvalidJSON(t *testing.T) {
	_, err := parseAssistantToolTranscript(`{"tool":"not an array"}`)
	if err == nil || !strings.Contains(err.Error(), "parse assistant transcript") {
		t.Fatalf("parse transcript error = %v, want parse error", err)
	}
}

// spending_summary with group_by "month" returns every month of the range,
// empty ones as zero rows, and the months add up to the overall total.
func TestAssistantSpendingSummaryGroupedByMonth(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	result, _, totals, _, err := a.executeAssistantTool("How much did I spend each month?", "spending_summary",
		json.RawMessage(`{"from":"2025-07-01","to":"2025-10-31","group_by":"month"}`))
	if err != nil {
		t.Fatal(err)
	}
	months := assistantMonthsResult(result)
	var got []string
	count, withdrawals, deposits := 0, 0.0, 0.0
	for _, m := range months {
		got = append(got, m.Month)
		count += m.Count
		withdrawals += m.Withdrawals
		deposits += m.Deposits
	}
	if strings.Join(got, ",") != "2025-07,2025-08,2025-09,2025-10" {
		t.Fatalf("months = %v, want July through October 2025", got)
	}
	if months[2].Count != 0 || months[2].Withdrawals != 0 {
		t.Fatalf("September has no transactions but got %+v", months[2])
	}
	if count != totals.Count || roundMoney(withdrawals) != roundMoney(totals.Withdrawals) || roundMoney(deposits) != roundMoney(totals.Deposits) {
		t.Fatalf("months add up to count %d / out %.2f / in %.2f, total is %d / %.2f / %.2f", count, withdrawals, deposits, totals.Count, totals.Withdrawals, totals.Deposits)
	}
	if count == 0 {
		t.Fatal("fixture range returned no transactions; the test would prove nothing")
	}
}

func TestAssistantGroupByRejectsBadUse(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), BackupDir: "backups"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	cases := map[string]struct{ tool, args string }{
		"unknown grouping":    {"spending_summary", `{"from":"2025-07-01","to":"2025-10-31","group_by":"week"}`},
		"other tool":          {"read_transactions", `{"from":"2025-07-01","to":"2025-10-31","group_by":"month"}`},
		"no dates":            {"spending_summary", `{"group_by":"month"}`},
		"more than 36 months": {"spending_summary", `{"from":"2020-01-01","to":"2025-12-31","group_by":"month"}`},
		"from after to":       {"spending_summary", `{"from":"2025-10-01","to":"2025-07-31","group_by":"month"}`},
	}
	for name, tc := range cases {
		if _, _, _, _, err := a.executeAssistantTool("q", tc.tool, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// Without group_by the result carries no months.
	result, _, _, _, err := a.executeAssistantTool("q", "spending_summary", json.RawMessage(`{"from":"2025-07-01","to":"2025-10-31"}`))
	if err != nil {
		t.Fatal(err)
	}
	if months := assistantMonthsResult(result); months != nil {
		t.Fatalf("ungrouped summary returned months %v", months)
	}
}
