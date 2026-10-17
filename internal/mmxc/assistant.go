package mmxc

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const assistantFeedbackFile = "data/assistant-feedback.txt"

type assistantPageData struct {
	Prompt                string
	Answer                string
	IntentJSON            string
	QuerySummary          string
	Warnings              []string
	Totals                assistantTotals
	Transactions          []transaction
	ToolTranscript        string
	ClarificationQuestion string
	ClarificationOptions  []string
	Budget                *assistantBudgetStatus
	Months                []assistantMonthTotal
	FeedbackSaved         bool
}

type assistantToolDecision struct {
	Action    string          `json:"action"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Answer    string          `json:"answer"`
}

type assistantLegacyClarificationDecision struct {
	Action   string   `json:"action"`
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Answer   string   `json:"answer"`
}

type assistantToolFilterArgs struct {
	From                string  `json:"from"`
	To                  string  `json:"to"`
	Account             string  `json:"account"`
	Payee               string  `json:"payee"`
	Category            string  `json:"category"`
	Uncategorized       bool    `json:"uncategorized"`
	DescriptionContains string  `json:"description_contains"`
	Search              string  `json:"search"`
	Type                string  `json:"type"`
	Status              string  `json:"status"`
	Sort                string  `json:"sort"`
	Limit               int     `json:"limit"`
	AmountMin           float64 `json:"amount_min"`
	AmountMax           float64 `json:"amount_max"`
	// GroupBy "month" makes spending_summary also return per-month totals.
	GroupBy string `json:"group_by"`
}

// assistantMonthTotal is one row of a spending_summary grouped by month.
type assistantMonthTotal struct {
	Month       string  `json:"month"`
	Count       int     `json:"count"`
	Withdrawals float64 `json:"withdrawals"`
	Deposits    float64 `json:"deposits"`
	Net         float64 `json:"net"`
}

// assistantMaxGroupedMonths bounds a month breakdown, so one question cannot
// ask for an unbounded table.
const assistantMaxGroupedMonths = 36

type assistantCompareSummaryArgs struct {
	PeriodALabel        string  `json:"period_a_label"`
	PeriodAFrom         string  `json:"period_a_from"`
	PeriodATo           string  `json:"period_a_to"`
	PeriodBLabel        string  `json:"period_b_label"`
	PeriodBFrom         string  `json:"period_b_from"`
	PeriodBTo           string  `json:"period_b_to"`
	Account             string  `json:"account"`
	Payee               string  `json:"payee"`
	Category            string  `json:"category"`
	Uncategorized       bool    `json:"uncategorized"`
	DescriptionContains string  `json:"description_contains"`
	Search              string  `json:"search"`
	Type                string  `json:"type"`
	Status              string  `json:"status"`
	AmountMin           float64 `json:"amount_min"`
	AmountMax           float64 `json:"amount_max"`
}

type assistantToolRunResult struct {
	Answer                string
	QuerySummary          string
	Transcript            string
	Tool                  string
	Arguments             json.RawMessage
	Warnings              []string
	Transactions          []transaction
	Totals                assistantTotals
	ClarificationQuestion string
	ClarificationOptions  []string
	Budget                *assistantBudgetStatus
	Months                []assistantMonthTotal
}

type assistantClarificationArgs struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
}

type assistantTotals struct {
	Count       int
	Withdrawals float64
	Deposits    float64
	Net         float64
	Previous    float64
	Delta       float64
}

func (a *app) assistant(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.assistantReady() {
		a.httpError(w, a.assistantUnavailableMessage(), http.StatusServiceUnavailable)
		return
	}
	data := pageData{Title: "Ask", Active: "assistant", Info: a.info(), ReturnTo: localReturnURL(r)}
	accounts, err := a.listAccounts()
	if err != nil {
		a.renderError(w, "Ask", "assistant", err)
		return
	}
	categories, err := a.listCategories()
	if err != nil {
		a.renderError(w, "Ask", "assistant", err)
		return
	}
	payees, err := a.listPayees()
	if err != nil {
		a.renderError(w, "Ask", "assistant", err)
		return
	}
	data.Accounts = accounts
	data.Categories = categories
	data.Payees = payees
	data.Assistant.FeedbackSaved = r.URL.Query().Get("feedback") == "saved"
	if prompt := strings.TrimSpace(r.URL.Query().Get("q")); prompt != "" {
		data.Assistant.Prompt = prompt
		result, err := a.assistantResultFromRequest(prompt, r.URL.Query())
		if err != nil {
			data.Assistant.Warnings = []string{err.Error()}
		} else {
			applyAssistantResult(&data, result)
		}
	}
	if idText := r.URL.Query().Get("edit_trans_id"); idText != "" && a.config.WriteEnabled {
		if id, err := strconv.Atoi(idText); err == nil {
			if tx, err := a.getTransaction(id); err == nil {
				data.EditTransaction = &tx
			}
		}
	}
	a.render(w, "assistant", data)
}

func (a *app) askAssistant(w http.ResponseWriter, r *http.Request) {
	a.askAssistantTools(w, r)
}

func (a *app) assistantFeedback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		a.httpError(w, "prompt is required", http.StatusBadRequest)
		return
	}
	if len(prompt) > 1000 {
		a.httpError(w, "prompt is too long", http.StatusBadRequest)
		return
	}
	if err := appendAssistantFeedbackQuestion(assistantFeedbackFile, prompt); err != nil {
		a.renderError(w, "Ask", "assistant", err)
		return
	}
	returnTo := sanitizeLocalPath(r.FormValue("return_to"))
	if returnTo == "" {
		returnTo = "/assistant"
	}
	http.Redirect(w, r, assistantFeedbackSavedURL(returnTo), http.StatusSeeOther)
}

func (a *app) askAssistantTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.assistantReady() {
		a.httpError(w, a.assistantUnavailableMessage(), http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		http.Redirect(w, r, "/assistant", http.StatusSeeOther)
		return
	}
	if len(prompt) > 1000 {
		a.httpError(w, "prompt is too long", http.StatusBadRequest)
		return
	}
	transcript, err := parseAssistantToolTranscript(r.FormValue("transcript"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if answer := strings.TrimSpace(r.FormValue("clarification_answer")); answer != "" {
		if len(answer) > 500 {
			a.httpError(w, "clarification answer is too long", http.StatusBadRequest)
			return
		}
		transcript = append(transcript, map[string]any{
			"user":   "clarification",
			"answer": answer,
		})
	}
	data := pageData{Title: "Ask", Active: "assistant", Info: a.info(), ReturnTo: localReturnURL(r)}
	accounts, _ := a.listAccounts()
	categories, _ := a.listCategories()
	payees, _ := a.listPayees()
	data.Accounts = accounts
	data.Categories = categories
	data.Payees = payees
	data.Assistant.Prompt = prompt
	result, err := a.runAssistantToolPOC(prompt, transcript)
	if err != nil {
		applyAssistantResult(&data, result)
		data.Assistant.Warnings = append(data.Assistant.Warnings, err.Error())
		if result.Tool != "" {
			data.ReturnTo = assistantReplayURL(prompt, result.Tool, result.Arguments)
		}
		a.render(w, "assistant", data)
		return
	}
	applyAssistantResult(&data, result)
	data.ReturnTo = assistantReplayURL(prompt, result.Tool, result.Arguments)
	a.render(w, "assistant", data)
}

func appendAssistantFeedbackQuestion(path, question string) error {
	question = strings.TrimSpace(question)
	if question == "" {
		return errors.New("feedback question is required")
	}
	question = strings.Join(strings.Fields(question), " ")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(question + "\n")
	return err
}

func assistantFeedbackSavedURL(returnTo string) string {
	u, err := url.Parse(returnTo)
	if err != nil || u.Path == "" {
		return "/assistant?feedback=saved"
	}
	q := u.Query()
	q.Set("feedback", "saved")
	u.RawQuery = q.Encode()
	return u.String()
}

func (a *app) runAssistantToolPOC(question string, transcript []map[string]any) (assistantToolRunResult, error) {
	var lastTool string
	var lastArguments json.RawMessage
	var lastResult any
	var lastTransactions []transaction
	var lastTotals assistantTotals
	var warnings []string
	for step := 0; step < 4; step++ {
		prompt := a.assistantToolPrompt(question, transcript)
		decision, resp, err := a.assistantToolDecision(prompt)
		if err != nil {
			return assistantToolRunResult{}, err
		}
		if decision.Action == "final" {
			transcriptText := assistantToolTranscriptJSON(transcript)
			return assistantToolRunResult{
				Answer:       strings.TrimSpace(decision.Answer),
				QuerySummary: assistantToolSummary(lastTool, lastResult),
				Transcript:   transcriptText,
				Tool:         lastTool,
				Arguments:    lastArguments,
				Warnings:     warnings,
				Transactions: lastTransactions,
				Totals:       lastTotals,
				Budget:       assistantBudgetResult(lastResult),
				Months:       assistantMonthsResult(lastResult),
			}, nil
		}
		if decision.Action != "tool" {
			return assistantToolRunResult{}, fmt.Errorf("unsupported tool action %q", decision.Action)
		}
		var carriedWarnings []string
		decision.Arguments, carriedWarnings = assistantCarryForwardLookupArguments(decision.Tool, decision.Arguments, transcript)
		result, txs, totals, toolWarnings, err := a.executeAssistantTool(question, decision.Tool, decision.Arguments)
		if err != nil {
			var repaired assistantToolDecision
			repaired, resp, err = a.repairAssistantToolDecision(prompt, resp, err)
			if err != nil {
				return assistantToolRunResult{}, err
			}
			if repaired.Action == "final" {
				transcriptText := assistantToolTranscriptJSON(transcript)
				return assistantToolRunResult{
					Answer:       strings.TrimSpace(repaired.Answer),
					QuerySummary: assistantToolSummary(lastTool, lastResult),
					Transcript:   transcriptText,
					Tool:         lastTool,
					Arguments:    lastArguments,
					Warnings:     warnings,
					Transactions: lastTransactions,
					Totals:       lastTotals,
					Budget:       assistantBudgetResult(lastResult),
					Months:       assistantMonthsResult(lastResult),
				}, nil
			}
			if repaired.Action != "tool" {
				return assistantToolRunResult{}, fmt.Errorf("unsupported tool action %q after repair", repaired.Action)
			}
			decision = repaired
			decision.Arguments, carriedWarnings = assistantCarryForwardLookupArguments(decision.Tool, decision.Arguments, transcript)
			result, txs, totals, toolWarnings, err = a.executeAssistantTool(question, decision.Tool, decision.Arguments)
			if err != nil {
				return assistantToolRunResult{}, err
			}
		}
		lastTool = decision.Tool
		lastArguments = append(json.RawMessage(nil), decision.Arguments...)
		lastResult = result
		lastTransactions = txs
		lastTotals = totals
		warnings = append(warnings, carriedWarnings...)
		warnings = append(warnings, toolWarnings...)
		transcript = append(transcript, map[string]any{
			"tool":      decision.Tool,
			"arguments": assistantRawJSON(decision.Arguments),
			"result":    result,
		})
		if decision.Tool == "ask_clarification" {
			question, options := assistantClarificationFields(result)
			return assistantToolRunResult{
				Answer:                assistantClarificationAnswer(result),
				QuerySummary:          assistantToolSummary(decision.Tool, result),
				Transcript:            assistantToolTranscriptJSON(transcript),
				Tool:                  decision.Tool,
				Arguments:             append(json.RawMessage(nil), decision.Arguments...),
				Warnings:              warnings,
				ClarificationQuestion: question,
				ClarificationOptions:  options,
			}, nil
		}
	}
	return assistantToolRunResult{
		QuerySummary: assistantToolSummary(lastTool, lastResult),
		Transcript:   assistantToolTranscriptJSON(transcript),
		Tool:         lastTool,
		Arguments:    lastArguments,
		Warnings:     warnings,
		Transactions: lastTransactions,
		Totals:       lastTotals,
		Budget:       assistantBudgetResult(lastResult),
		Months:       assistantMonthsResult(lastResult),
	}, errors.New("assistant did not produce a final answer after tool calls")
}

func applyAssistantResult(data *pageData, result assistantToolRunResult) {
	data.Assistant.Answer = result.Answer
	data.Assistant.QuerySummary = result.QuerySummary
	data.Assistant.IntentJSON = result.Transcript
	data.Assistant.ToolTranscript = result.Transcript
	data.Assistant.Warnings = result.Warnings
	data.Assistant.Totals = result.Totals
	data.Assistant.Transactions = result.Transactions
	data.Assistant.ClarificationQuestion = result.ClarificationQuestion
	data.Assistant.ClarificationOptions = result.ClarificationOptions
	data.Assistant.Budget = result.Budget
	data.Assistant.Months = result.Months
	data.Transactions = result.Transactions
}

func (a *app) assistantResultFromRequest(prompt string, q url.Values) (assistantToolRunResult, error) {
	tool := strings.TrimSpace(q.Get("tool"))
	args := strings.TrimSpace(q.Get("args"))
	if tool == "" {
		return a.runAssistantToolPOC(prompt, nil)
	}
	if !isAssistantToolName(tool) || tool == "ask_clarification" || tool == "find_account" || tool == "find_category" || tool == "find_payee" {
		return assistantToolRunResult{}, fmt.Errorf("cannot replay assistant tool %q", tool)
	}
	result, txs, totals, warnings, err := a.executeAssistantTool(prompt, tool, json.RawMessage(args))
	if err != nil {
		return assistantToolRunResult{}, err
	}
	transcript := []map[string]any{{
		"tool":      tool,
		"arguments": assistantRawJSON(json.RawMessage(args)),
		"result":    result,
	}}
	return assistantToolRunResult{
		QuerySummary: assistantToolSummary(tool, result),
		Transcript:   assistantToolTranscriptJSON(transcript),
		Tool:         tool,
		Arguments:    json.RawMessage(args),
		Warnings:     warnings,
		Transactions: txs,
		Totals:       totals,
		Budget:       assistantBudgetResult(result),
		Months:       assistantMonthsResult(result),
	}, nil
}

func assistantReplayURL(prompt, tool string, args json.RawMessage) string {
	q := url.Values{}
	if prompt = strings.TrimSpace(prompt); prompt != "" {
		q.Set("q", prompt)
	}
	if tool = strings.TrimSpace(tool); tool != "" && len(args) > 0 {
		q.Set("tool", tool)
		q.Set("args", string(args))
	}
	if encoded := q.Encode(); encoded != "" {
		return "/assistant?" + encoded
	}
	return "/assistant"
}

func (a *app) assistantToolDecision(prompt string) (assistantToolDecision, string, error) {
	resp, err := a.llm.generate(a.config.LLMModel, prompt, nil, llmOptions{Temperature: temperature(0.1), JSONMode: true})
	if err != nil {
		return assistantToolDecision{}, resp, err
	}
	decision, err := parseAssistantToolDecision(resp)
	if err != nil {
		return a.repairAssistantToolDecision(prompt, resp, err)
	}
	if decision.Action != "tool" && decision.Action != "final" {
		return a.repairAssistantToolDecision(prompt, resp, fmt.Errorf("unsupported tool action %q", decision.Action))
	}
	return decision, resp, nil
}

func (a *app) repairAssistantToolDecision(originalPrompt, badResponse string, cause error) (assistantToolDecision, string, error) {
	repairPrompt := assistantToolRepairPrompt(originalPrompt, badResponse, cause)
	resp, err := a.llm.generate(a.config.LLMModel, repairPrompt, nil, llmOptions{Temperature: temperature(0), JSONMode: true})
	if err != nil {
		return assistantToolDecision{}, resp, err
	}
	decision, err := parseAssistantToolDecision(resp)
	if err != nil {
		return assistantToolDecision{}, resp, err
	}
	if decision.Action != "tool" && decision.Action != "final" {
		return assistantToolDecision{}, resp, fmt.Errorf("unsupported tool action %q after repair", decision.Action)
	}
	return decision, resp, nil
}

func parseAssistantToolDecision(text string) (assistantToolDecision, error) {
	text = strings.TrimSpace(text)
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return assistantToolDecision{}, errors.New("LLM response did not contain a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(text[start : end+1])))
	dec.DisallowUnknownFields()
	var decision assistantToolDecision
	if err := dec.Decode(&decision); err != nil {
		if normalized, ok := parseLegacyAssistantClarificationDecision(text[start : end+1]); ok {
			return normalized, nil
		}
		return assistantToolDecision{}, fmt.Errorf("parse assistant tool decision: %w", err)
	}
	decision.Action = strings.TrimSpace(decision.Action)
	decision.Tool = strings.TrimSpace(decision.Tool)
	decision.Answer = strings.TrimSpace(decision.Answer)
	decision = normalizeAssistantToolDecision(decision)
	return decision, nil
}

func normalizeAssistantToolDecision(decision assistantToolDecision) assistantToolDecision {
	if isAssistantToolName(decision.Action) && (decision.Tool == "" || decision.Tool == decision.Action) {
		decision.Tool = decision.Action
		decision.Action = "tool"
	}
	return decision
}

func isAssistantToolName(name string) bool {
	switch name {
	case "find_account", "find_category", "find_payee", "spending_summary", "compare_summary", "read_transactions", "budget_status", "ask_clarification":
		return true
	default:
		return false
	}
}

func assistantToolRepairPrompt(originalPrompt, badResponse string, cause error) string {
	return fmt.Sprintf(`%s

The previous response was invalid for this schema.
Error: %s
Previous response:
%s

Return one corrected JSON object only. The top-level keys must be exactly action, tool, arguments, and answer. action must be "tool" or "final". If action is "tool", tool must be one of find_account, find_category, find_payee, spending_summary, compare_summary, read_transactions, budget_status, or ask_clarification, and answer must be empty. Do not include unknown fields.`, originalPrompt, cause, badResponse)
}

func parseLegacyAssistantClarificationDecision(raw string) (assistantToolDecision, bool) {
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return assistantToolDecision{}, false
	}
	allowed := map[string]bool{
		"action":   true,
		"question": true,
		"options":  true,
		"answer":   true,
	}
	for key := range fields {
		if !allowed[key] {
			return assistantToolDecision{}, false
		}
	}
	var legacy assistantLegacyClarificationDecision
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&legacy); err != nil {
		return assistantToolDecision{}, false
	}
	if strings.TrimSpace(legacy.Action) != "ask_clarification" {
		return assistantToolDecision{}, false
	}
	args, err := json.Marshal(assistantClarificationArgs{
		Question: strings.TrimSpace(legacy.Question),
		Options:  legacy.Options,
	})
	if err != nil {
		return assistantToolDecision{}, false
	}
	return assistantToolDecision{
		Action:    "tool",
		Tool:      "ask_clarification",
		Arguments: args,
		Answer:    strings.TrimSpace(legacy.Answer),
	}, true
}

func (a *app) assistantToolPrompt(question string, transcript []map[string]any) string {
	accounts, _ := a.listAccounts()
	var accountNames []string
	for _, account := range accounts {
		accountNames = append(accountNames, account.Name)
	}
	return BuildAssistantToolPrompt(a.prompts(), time.Now().Format("2006-01-02"), accountNames, question, transcript)
}

// BuildAssistantToolPrompt renders the assistant tool-selection prompt from
// explicit inputs (prompt overrides, current date, account names, question,
// transcript) with no database or clock dependency, so the assistant eval
// (cmd/assistant-eval) can reuse the exact production prompt instead of a
// hand-synced copy. The app's assistantToolPrompt method gathers the accounts
// and current date, then delegates here.
//
// The schema, tool list and allowed argument values below are the contract
// parseAssistantToolDecision and the tool executors depend on, so they stay in
// code; the guidance between them comes from the overridable assistant prompt
// file (see prompts.go).
func BuildAssistantToolPrompt(prompts Prompts, currentDate string, accountNames []string, question string, transcript []map[string]any) string {
	return fmt.Sprintf(`You are using MMXC read-only finance tools. Return exactly one JSON object.
For action "tool", answer must be an empty string because MMXC will execute the tool and supply the real result.
The top-level action must be only "tool" or "final". Tool names such as find_account, find_category, find_payee, spending_summary, compare_summary, read_transactions, budget_status, and ask_clarification are never valid as the top-level action.
The top-level keys must be exactly action, tool, arguments, and answer. Put tool-specific fields such as question and options inside arguments.

Tools:
- find_account {"query": "text"}: finds account names and returns account metadata such as type and status.
- find_category {"query": "text"}: finds category names.
- find_payee {"query": "text"}: finds payee names.
- spending_summary {"from":"","to":"","account":"","payee":"","category":"","uncategorized":false,"description_contains":"","type":"Withdrawal","status":"","amount_min":0,"amount_max":0,"group_by":""}: totals matching transactions, including count. With group_by "month" it also returns months, one row per calendar month from from to to, each with its own count and totals.
- compare_summary {"period_a_label":"","period_a_from":"YYYY-MM-DD","period_a_to":"YYYY-MM-DD","period_b_label":"","period_b_from":"YYYY-MM-DD","period_b_to":"YYYY-MM-DD","account":"","payee":"","category":"","uncategorized":false,"description_contains":"","type":"Withdrawal","status":"","amount_min":0,"amount_max":0}: compares totals for the same filters across two concrete periods.
- read_transactions {"from":"","to":"","account":"","payee":"","category":"","uncategorized":false,"description_contains":"","type":"","status":"","sort":"date_desc","limit":20,"amount_min":0,"amount_max":0}: returns bounded transaction rows.
- budget_status {"year":"YYYY","category":""}: budget versus actual for one budget year, per budgeted category (including its subcategories when category is set) plus expense and income totals. Amounts are positive; actual is net spending for expense budgets and net income for income budgets; remaining is budget minus actual and is negative once over budget; over_by is the excess; budget_to_date is the budget prorated through Current date (whole months for monthly and longer periods).
- ask_clarification {"question":"short question","options":["choice"]}: asks the user to choose when the question is ambiguous or a tool lookup returns multiple plausible matches.

%s

Argument rules:
from and to are YYYY-MM-DD dates or empty. compare_summary period_a_from, period_a_to, period_b_from, and period_b_to are required YYYY-MM-DD dates; never leave them empty.
type must be "", "Withdrawal", "Deposit", or "Transfer". Never use "expense" or "income" as type.
status must be "", "Reconciled", "Void", "Follow up", "Duplicate", or "__blank".
sort must be "", "date_desc", "date_asc", "amount_desc", or "amount_asc".
budget_status year is a four-digit budget year such as "2026"; category is empty for all budgeted categories.
group_by must be "" or "month". Only spending_summary accepts it. group_by "month" requires from and to, covering at most 36 months.
limit, amount_min, and amount_max must be JSON numbers, never strings. Use 0 for unused numeric values.
If the question is not about read-only finance data or cannot be answered with these tools, return final with an answer containing "Cannot answer".
Current date: %s
Accounts: %s
Previous tool calls and results: %s

Return either:
{"action":"tool","tool":"spending_summary","arguments":{"from":"","to":"","account":"","payee":"","category":"","uncategorized":false,"description_contains":"","type":"Withdrawal","status":"","amount_min":0,"amount_max":0,"group_by":""},"answer":""}
or:
{"action":"tool","tool":"compare_summary","arguments":{"period_a_label":"this month","period_a_from":"","period_a_to":"","period_b_label":"last month","period_b_from":"","period_b_to":"","account":"","payee":"","category":"","uncategorized":false,"description_contains":"","type":"Withdrawal","status":"","amount_min":0,"amount_max":0},"answer":""}
or:
{"action":"tool","tool":"read_transactions","arguments":{"from":"","to":"","account":"","payee":"","category":"","uncategorized":false,"description_contains":"","type":"","status":"","sort":"date_desc","limit":20,"amount_min":0,"amount_max":0},"answer":""}
or:
{"action":"tool","tool":"budget_status","arguments":{"year":"","category":""},"answer":""}
or:
{"action":"tool","tool":"ask_clarification","arguments":{"question":"Which category should I use?","options":["Choice A","Choice B"]},"answer":""}
or:
{"action":"final","tool":"","arguments":{},"answer":"short answer grounded only in tool results"}

Question: %s`,
		prompts.guidance(promptAssistant),
		currentDate,
		strings.Join(accountNames, ", "),
		assistantToolTranscriptJSON(transcript),
		question)
}

func (a *app) executeAssistantTool(question, name string, raw json.RawMessage) (any, []transaction, assistantTotals, []string, error) {
	switch name {
	case "ask_clarification":
		var args assistantClarificationArgs
		if err := decodeAssistantToolArgs(raw, &args); err != nil {
			return nil, nil, assistantTotals{}, nil, err
		}
		result, err := assistantToolClarification(args)
		return result, nil, assistantTotals{}, nil, err
	case "find_account":
		var args struct {
			Query string `json:"query"`
		}
		if err := decodeAssistantToolArgs(raw, &args); err != nil {
			return nil, nil, assistantTotals{}, nil, err
		}
		result, err := a.assistantToolFindAccount(args.Query)
		return result, nil, assistantTotals{}, nil, err
	case "find_category":
		var args struct {
			Query string `json:"query"`
		}
		if err := decodeAssistantToolArgs(raw, &args); err != nil {
			return nil, nil, assistantTotals{}, nil, err
		}
		result, err := a.assistantToolFindCategory(args.Query)
		return result, nil, assistantTotals{}, nil, err
	case "find_payee":
		var args struct {
			Query string `json:"query"`
		}
		if err := decodeAssistantToolArgs(raw, &args); err != nil {
			return nil, nil, assistantTotals{}, nil, err
		}
		result, err := a.assistantToolFindPayee(args.Query)
		return result, nil, assistantTotals{}, nil, err
	case "compare_summary":
		var args assistantCompareSummaryArgs
		if err := decodeAssistantToolArgs(raw, &args); err != nil {
			return nil, nil, assistantTotals{}, nil, err
		}
		result, txs, totals, warnings, err := a.assistantToolCompareSummary(question, args)
		return result, txs, totals, warnings, err
	case "budget_status":
		var args assistantBudgetStatusArgs
		if err := decodeAssistantToolArgs(raw, &args); err != nil {
			return nil, nil, assistantTotals{}, nil, err
		}
		result, err := a.assistantToolBudgetStatus(args, time.Now())
		if err != nil {
			return nil, nil, assistantTotals{}, nil, err
		}
		return result, nil, assistantTotals{}, nil, nil
	case "spending_summary", "read_transactions":
		var args assistantToolFilterArgs
		if err := decodeAssistantToolArgs(raw, &args); err != nil {
			return nil, nil, assistantTotals{}, nil, err
		}
		groupBy := strings.TrimSpace(args.GroupBy)
		switch {
		case groupBy != "" && groupBy != "month":
			return nil, nil, assistantTotals{}, nil, fmt.Errorf("group_by must be empty or \"month\", not %q", groupBy)
		case groupBy != "" && name != "spending_summary":
			return nil, nil, assistantTotals{}, nil, errors.New("group_by is only supported by spending_summary")
		}
		filter, warnings, err := a.assistantToolFilter(question, args, name == "read_transactions")
		if err != nil {
			return nil, nil, assistantTotals{}, nil, err
		}
		txs, err := a.listTransactions(filter)
		if err != nil {
			return nil, nil, assistantTotals{}, nil, err
		}
		totals, err := a.assistantTotals(txs, filter)
		if err != nil {
			return nil, nil, assistantTotals{}, nil, err
		}
		if name == "read_transactions" {
			result := map[string]any{"count": len(txs), "transactions": assistantToolTransactionRows(txs)}
			return result, txs, totals, warnings, nil
		}
		result := map[string]any{
			"count":       totals.Count,
			"withdrawals": roundMoney(totals.Withdrawals),
			"deposits":    roundMoney(totals.Deposits),
			"net":         roundMoney(totals.Net),
		}
		if groupBy == "month" {
			months, err := a.assistantMonthTotals(txs, filter)
			if err != nil {
				return nil, nil, assistantTotals{}, nil, err
			}
			result["months"] = months
		}
		return result, txs, totals, warnings, nil
	default:
		return nil, nil, assistantTotals{}, nil, fmt.Errorf("unsupported assistant tool %q", name)
	}
}

func assistantToolClarification(args assistantClarificationArgs) (map[string]any, error) {
	question := strings.TrimSpace(args.Question)
	if question == "" {
		return nil, errors.New("clarification question is required")
	}
	var options []string
	seen := make(map[string]bool)
	for _, option := range args.Options {
		option = strings.TrimSpace(option)
		if option == "" || seen[strings.ToLower(option)] {
			continue
		}
		seen[strings.ToLower(option)] = true
		options = append(options, option)
		if len(options) >= 8 {
			break
		}
	}
	return map[string]any{"question": question, "options": options}, nil
}

func decodeAssistantToolArgs(raw json.RawMessage, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("parse tool arguments: %w", err)
	}
	return nil
}

func (a *app) assistantToolCompareSummary(question string, args assistantCompareSummaryArgs) (map[string]any, []transaction, assistantTotals, []string, error) {
	if strings.TrimSpace(args.PeriodAFrom) == "" || strings.TrimSpace(args.PeriodATo) == "" || strings.TrimSpace(args.PeriodBFrom) == "" || strings.TrimSpace(args.PeriodBTo) == "" {
		return nil, nil, assistantTotals{}, nil, errors.New("compare_summary requires period_a_from, period_a_to, period_b_from, and period_b_to")
	}
	base := assistantToolFilterArgs{
		Account:             args.Account,
		Payee:               args.Payee,
		Category:            args.Category,
		Uncategorized:       args.Uncategorized,
		DescriptionContains: args.DescriptionContains,
		Search:              args.Search,
		Type:                args.Type,
		Status:              args.Status,
		AmountMin:           args.AmountMin,
		AmountMax:           args.AmountMax,
	}
	periodAArgs := base
	periodAArgs.From = args.PeriodAFrom
	periodAArgs.To = args.PeriodATo
	periodBArgs := base
	periodBArgs.From = args.PeriodBFrom
	periodBArgs.To = args.PeriodBTo

	periodAFilter, warningsA, err := a.assistantToolFilter(question, periodAArgs, false)
	if err != nil {
		return nil, nil, assistantTotals{}, nil, err
	}
	periodBFilter, warningsB, err := a.assistantToolFilter(question, periodBArgs, false)
	if err != nil {
		return nil, nil, assistantTotals{}, nil, err
	}
	periodATxs, err := a.listTransactions(periodAFilter)
	if err != nil {
		return nil, nil, assistantTotals{}, nil, err
	}
	periodBTxs, err := a.listTransactions(periodBFilter)
	if err != nil {
		return nil, nil, assistantTotals{}, nil, err
	}
	periodATotals, err := a.assistantTotals(periodATxs, periodAFilter)
	if err != nil {
		return nil, nil, assistantTotals{}, nil, err
	}
	periodBTotals, err := a.assistantTotals(periodBTxs, periodBFilter)
	if err != nil {
		return nil, nil, assistantTotals{}, nil, err
	}
	valueA := assistantCompareValue(periodATotals, args.Type)
	valueB := assistantCompareValue(periodBTotals, args.Type)
	result := map[string]any{
		"period_a": map[string]any{
			"label":       strings.TrimSpace(args.PeriodALabel),
			"from":        periodAArgs.From,
			"to":          periodAArgs.To,
			"count":       periodATotals.Count,
			"withdrawals": roundMoney(periodATotals.Withdrawals),
			"deposits":    roundMoney(periodATotals.Deposits),
			"net":         roundMoney(periodATotals.Net),
			"value":       roundMoney(valueA),
		},
		"period_b": map[string]any{
			"label":       strings.TrimSpace(args.PeriodBLabel),
			"from":        periodBArgs.From,
			"to":          periodBArgs.To,
			"count":       periodBTotals.Count,
			"withdrawals": roundMoney(periodBTotals.Withdrawals),
			"deposits":    roundMoney(periodBTotals.Deposits),
			"net":         roundMoney(periodBTotals.Net),
			"value":       roundMoney(valueB),
		},
		"delta": roundMoney(valueA - valueB),
	}
	warnings := append([]string{}, warningsA...)
	warnings = append(warnings, warningsB...)
	txs := append([]transaction{}, periodATxs...)
	txs = append(txs, periodBTxs...)
	return result, txs, periodATotals, warnings, nil
}

func assistantCompareValue(totals assistantTotals, transType string) float64 {
	switch transType {
	case "Deposit":
		return totals.Deposits
	case "Withdrawal":
		return totals.Withdrawals
	default:
		return totals.Net
	}
}

func assistantCarryForwardLookupArguments(tool string, raw json.RawMessage, transcript []map[string]any) (json.RawMessage, []string) {
	// budget_status filters by category only; adding account or payee would
	// fail its strict argument decoding.
	categoryOnly := tool == "budget_status"
	if tool != "spending_summary" && tool != "compare_summary" && tool != "read_transactions" && !categoryOnly {
		return raw, nil
	}
	var args map[string]any
	if len(raw) == 0 || string(raw) == "null" {
		args = make(map[string]any)
	} else if err := json.Unmarshal(raw, &args); err != nil {
		return raw, nil
	}
	var warnings []string
	if !categoryOnly && assistantStringArg(args, "account") == "" && assistantDescriptionArg(args) == "" {
		if name := assistantSingleLookupName(transcript, "find_account"); name != "" {
			args["account"] = name
			warnings = append(warnings, fmt.Sprintf("Used exact account %q from the prior lookup.", name))
		}
	}
	if !categoryOnly && assistantStringArg(args, "payee") == "" && assistantDescriptionArg(args) == "" {
		if name := assistantSingleLookupName(transcript, "find_payee"); name != "" {
			args["payee"] = name
			warnings = append(warnings, fmt.Sprintf("Used exact payee %q from the prior lookup.", name))
		}
	}
	if assistantStringArg(args, "category") == "" && assistantDescriptionArg(args) == "" {
		if name := assistantSingleLookupName(transcript, "find_category"); name != "" {
			args["category"] = name
			warnings = append(warnings, fmt.Sprintf("Used exact category %q from the prior lookup.", name))
		}
	}
	data, err := json.Marshal(args)
	if err != nil {
		return raw, warnings
	}
	return data, warnings
}

func assistantStringArg(args map[string]any, key string) string {
	value, ok := args[key]
	if !ok || value == nil {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func assistantDescriptionArg(args map[string]any) string {
	if value := assistantStringArg(args, "description_contains"); value != "" {
		return value
	}
	return assistantStringArg(args, "search")
}

func assistantSingleLookupName(transcript []map[string]any, tool string) string {
	for i := len(transcript) - 1; i >= 0; i-- {
		entry := transcript[i]
		if entryTool, _ := entry["tool"].(string); entryTool != tool {
			continue
		}
		names := assistantLookupResultNames(entry["result"])
		if len(names) == 1 {
			return names[0]
		}
		return ""
	}
	return ""
}

func assistantLookupResultNames(result any) []string {
	var names []string
	switch rows := result.(type) {
	case []map[string]any:
		for _, row := range rows {
			if name, _ := row["name"].(string); strings.TrimSpace(name) != "" {
				names = append(names, strings.TrimSpace(name))
			}
		}
	case []any:
		for _, raw := range rows {
			row, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if name, _ := row["name"].(string); strings.TrimSpace(name) != "" {
				names = append(names, strings.TrimSpace(name))
			}
		}
	}
	return names
}

func (a *app) assistantToolFilter(question string, args assistantToolFilterArgs, listMode bool) (transactionFilter, []string, error) {
	_ = question
	payeeName := strings.TrimSpace(args.Payee)
	categoryName := strings.TrimSpace(args.Category)
	descriptionContains := strings.TrimSpace(args.DescriptionContains)
	if descriptionContains == "" {
		descriptionContains = strings.TrimSpace(args.Search)
	}
	uncategorized := args.Uncategorized || assistantUncategorizedCategoryAlias(categoryName)
	if uncategorized {
		categoryName = ""
	}
	from := strings.TrimSpace(args.From)
	to := strings.TrimSpace(args.To)
	if from != "" {
		if _, err := time.Parse("2006-01-02", from); err != nil {
			return transactionFilter{}, nil, fmt.Errorf("from date must be YYYY-MM-DD")
		}
	}
	if to != "" {
		if _, err := time.Parse("2006-01-02", to); err != nil {
			return transactionFilter{}, nil, fmt.Errorf("to date must be YYYY-MM-DD")
		}
	}
	transType := strings.TrimSpace(args.Type)
	if transType != "" && transType != "Withdrawal" && transType != "Deposit" && transType != "Transfer" {
		return transactionFilter{}, nil, fmt.Errorf("unsupported transaction type %q", transType)
	}
	status := strings.TrimSpace(args.Status)
	if status != "" && !validTransactionStatus(status) && status != "__blank" {
		return transactionFilter{}, nil, fmt.Errorf("unsupported transaction status %q", status)
	}
	sort := strings.TrimSpace(args.Sort)
	switch sort {
	case "", "date_desc":
		sort = "date_desc"
	case "date_asc", "amount_desc", "amount_asc":
	default:
		return transactionFilter{}, nil, fmt.Errorf("unsupported assistant sort %q", sort)
	}
	limit := args.Limit
	if listMode {
		if limit <= 0 {
			limit = 20
		}
		if limit > 50 {
			limit = 50
		}
	} else {
		if limit <= 0 {
			limit = 500
		}
		if limit > 1000 {
			limit = 1000
		}
		if transType == "" {
			transType = "Withdrawal"
		}
	}
	filter := transactionFilter{
		From:      from,
		To:        to,
		Type:      transType,
		Status:    status,
		Search:    descriptionContains,
		AmountMin: args.AmountMin,
		AmountMax: args.AmountMax,
		Sort:      sort,
		Limit:     limit,
	}
	var warnings []string
	if accountName := strings.TrimSpace(args.Account); accountName != "" {
		id, err := a.resolveAssistantToolAccountID(accountName)
		if err != nil {
			return transactionFilter{}, nil, err
		}
		filter.AccountID = id
	}
	if payeeName != "" {
		id, err := a.resolveAssistantToolPayeeID(payeeName)
		if err != nil {
			return transactionFilter{}, nil, err
		}
		filter.PayeeID = id
	}
	if categoryName != "" {
		id, err := a.resolveAssistantToolCategoryID(categoryName)
		if err != nil {
			return transactionFilter{}, nil, err
		}
		filter.CategoryID = id
		filter.IncludeCategoryChildren = true
		warnings = append(warnings, "Category totals use only matched split lines plus matching non-split rows.")
	}
	if uncategorized {
		filter.Uncategorized = true
		warnings = append(warnings, "Uncategorized split totals use only uncategorized split lines.")
	}
	return filter, warnings, nil
}

func assistantUncategorizedCategoryAlias(name string) bool {
	name = strings.TrimSpace(name)
	return strings.EqualFold(name, "Uncategorized") || name == "__blank"
}

func (a *app) resolveAssistantToolAccountID(name string) (int, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("account is required")
	}
	accounts, err := a.listAccounts()
	if err != nil {
		return 0, err
	}
	for _, account := range accounts {
		if strings.EqualFold(account.Name, name) {
			return account.ID, nil
		}
	}
	return 0, fmt.Errorf("account %q was not found; use an exact account name", name)
}

func (a *app) assistantToolFindAccount(query string) ([]map[string]any, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("account query is required")
	}
	accounts, err := a.listAccounts()
	if err != nil {
		return nil, err
	}
	needle := searchKey(query)
	var out []map[string]any
	for _, account := range accounts {
		if strings.Contains(assistantAccountSearchText(account), needle) {
			out = append(out, map[string]any{
				"id":                account.ID,
				"name":              account.Name,
				"type":              account.Type,
				"status":            account.Status,
				"currency":          account.CurrencyCode,
				"favorite":          account.Favorite,
				"balance":           roundMoney(account.Balance),
				"transaction_count": account.TransactionN,
			})
		}
		if len(out) >= 10 {
			break
		}
	}
	return out, nil
}

func assistantAccountSearchText(account account) string {
	text := account.Name + " " + account.Type + " " + account.Status + " " + account.HeldAt + " " + account.CurrencyCode
	if strings.EqualFold(account.Type, "Checking") {
		text += " Chequing"
	}
	return searchKey(text)
}

func (a *app) resolveAssistantToolPayeeID(name string) (int, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("payee is required")
	}
	payees, err := a.listPayees()
	if err != nil {
		return 0, err
	}
	for _, payee := range payees {
		if strings.EqualFold(payee.Name, name) {
			return payee.ID, nil
		}
	}
	return 0, fmt.Errorf("payee %q was not found; use find_payee before filtering by payee", name)
}

func (a *app) resolveAssistantToolCategoryID(name string) (int, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("category is required")
	}
	cats, err := a.listCategories()
	if err != nil {
		return 0, err
	}
	for _, cat := range cats {
		path := cat.Name
		if cat.ParentID > 0 {
			path = cat.Parent + ": " + cat.Name
		}
		if strings.EqualFold(path, name) {
			return cat.ID, nil
		}
	}
	return 0, fmt.Errorf("category %q was not found; use find_category before filtering by category", name)
}

func (a *app) assistantToolFindCategory(query string) ([]map[string]any, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("category query is required")
	}
	cats, err := a.listCategories()
	if err != nil {
		return nil, err
	}
	needle := searchKey(query)
	var out []map[string]any
	for _, cat := range cats {
		path := cat.Name
		if cat.ParentID > 0 {
			path = cat.Parent + ": " + cat.Name
		}
		if strings.Contains(searchKey(path), needle) {
			out = append(out, map[string]any{"id": cat.ID, "name": path, "active": cat.Active})
		}
		if len(out) >= 10 {
			break
		}
	}
	return out, nil
}

func (a *app) assistantToolFindPayee(query string) ([]map[string]any, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("payee query is required")
	}
	payees, err := a.listPayees()
	if err != nil {
		return nil, err
	}
	needle := searchKey(query)
	var out []map[string]any
	for _, payee := range payees {
		if strings.Contains(searchKey(payee.Name), needle) {
			out = append(out, map[string]any{"id": payee.ID, "name": payee.Name, "active": payee.Active, "category": payee.Category})
		}
		if len(out) >= 10 {
			break
		}
	}
	return out, nil
}

func assistantToolTransactionRows(txs []transaction) []map[string]any {
	rows := make([]map[string]any, 0, len(txs))
	for _, tx := range txs {
		rows = append(rows, map[string]any{
			"id":       tx.ID,
			"date":     tx.Date,
			"type":     tx.Type,
			"account":  tx.Account,
			"payee":    tx.Payee,
			"category": tx.Category,
			"amount":   roundMoney(tx.Amount),
			"status":   tx.Status,
			"notes":    tx.Notes,
		})
	}
	return rows
}

func assistantRawJSON(raw json.RawMessage) any {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return map[string]any{}
	}
	return v
}

func assistantToolTranscriptJSON(transcript []map[string]any) string {
	data, err := json.MarshalIndent(transcript, "", "  ")
	if err != nil {
		return "[]"
	}
	return string(data)
}

func parseAssistantToolTranscript(text string) ([]map[string]any, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	if len(text) > 128*1024 {
		return nil, errors.New("assistant transcript is too large")
	}
	var transcript []map[string]any
	dec := json.NewDecoder(strings.NewReader(text))
	if err := dec.Decode(&transcript); err != nil {
		return nil, fmt.Errorf("parse assistant transcript: %w", err)
	}
	if len(transcript) > 20 {
		return nil, errors.New("assistant transcript has too many entries")
	}
	return transcript, nil
}

// assistantMonthTotals splits a spending_summary's transactions by calendar
// month across the whole requested range, months without matches included as
// zero rows, and totals each month with the same rules as the overall total
// (assistantTotals, so split lines count the same way).
func (a *app) assistantMonthTotals(txs []transaction, filter transactionFilter) ([]assistantMonthTotal, error) {
	from, errFrom := time.Parse("2006-01-02", filter.From)
	to, errTo := time.Parse("2006-01-02", filter.To)
	if errFrom != nil || errTo != nil || to.Before(from) {
		return nil, errors.New("group_by \"month\" requires from and to dates, with from not after to")
	}
	first := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)
	last := time.Date(to.Year(), to.Month(), 1, 0, 0, 0, 0, time.UTC)
	if months := (last.Year()-first.Year())*12 + int(last.Month()-first.Month()) + 1; months > assistantMaxGroupedMonths {
		return nil, fmt.Errorf("group_by \"month\" covers at most %d months; this range has %d", assistantMaxGroupedMonths, months)
	}
	byMonth := map[string][]transaction{}
	for _, tx := range txs {
		byMonth[yearMonth(tx.Date)] = append(byMonth[yearMonth(tx.Date)], tx)
	}
	var out []assistantMonthTotal
	for m := first; !m.After(last); m = m.AddDate(0, 1, 0) {
		key := m.Format("2006-01")
		totals, err := a.assistantTotals(byMonth[key], filter)
		if err != nil {
			return nil, err
		}
		out = append(out, assistantMonthTotal{
			Month:       key,
			Count:       totals.Count,
			Withdrawals: roundMoney(totals.Withdrawals),
			Deposits:    roundMoney(totals.Deposits),
			Net:         roundMoney(totals.Net),
		})
	}
	return out, nil
}

// assistantMonthsResult returns the month rows of a grouped spending_summary
// result, or nil for any other result.
func assistantMonthsResult(result any) []assistantMonthTotal {
	m, ok := result.(map[string]any)
	if !ok {
		return nil
	}
	months, _ := m["months"].([]assistantMonthTotal)
	return months
}

func assistantToolSummary(tool string, result any) string {
	if tool == "" {
		return "Assistant did not call a tool."
	}
	data, err := json.Marshal(result)
	if err != nil {
		return "Assistant used " + tool + "."
	}
	return fmt.Sprintf("Assistant used %s and returned %d bytes.", tool, len(data))
}

func assistantClarificationAnswer(result any) string {
	question, options := assistantClarificationFields(result)
	if question == "" {
		return "I need a little more detail to answer that."
	}
	if len(options) == 0 {
		return question
	}
	return question
}

func assistantClarificationFields(result any) (string, []string) {
	m, ok := result.(map[string]any)
	if !ok {
		return "", nil
	}
	question, _ := m["question"].(string)
	if strings.TrimSpace(question) == "" {
		return "", nil
	}
	switch rawOptions := m["options"].(type) {
	case []string:
		return strings.TrimSpace(question), rawOptions
	case []any:
		var options []string
		for _, raw := range rawOptions {
			option, ok := raw.(string)
			if ok && strings.TrimSpace(option) != "" {
				options = append(options, strings.TrimSpace(option))
			}
		}
		return strings.TrimSpace(question), options
	default:
		return strings.TrimSpace(question), nil
	}
}

func (a *app) assistantTotals(txs []transaction, filter transactionFilter) (assistantTotals, error) {
	if filter.Uncategorized {
		return a.assistantUncategorizedTotals(txs)
	}
	if filter.CategoryID > 0 {
		return a.assistantCategoryTotals(txs, filter.CategoryID, filter.IncludeCategoryChildren)
	}
	summary := summarizeTransactions(txs)
	return assistantTotals{Count: summary.Count, Withdrawals: summary.Withdrawals, Deposits: summary.Deposits, Net: summary.Net}, nil
}

func (a *app) assistantCategoryTotals(txs []transaction, categoryID int, includeChildren bool) (assistantTotals, error) {
	var totals assistantTotals
	totals.Count = len(txs)
	for _, tx := range txs {
		amount, err := a.transactionCategoryAmount(tx, categoryID, includeChildren)
		if err != nil {
			return assistantTotals{}, err
		}
		switch tx.Type {
		case "Withdrawal":
			totals.Withdrawals += amount
		case "Deposit":
			totals.Deposits += amount
		}
	}
	totals.Net = totals.Deposits - totals.Withdrawals
	return totals, nil
}

func (a *app) assistantUncategorizedTotals(txs []transaction) (assistantTotals, error) {
	var totals assistantTotals
	totals.Count = len(txs)
	for _, tx := range txs {
		amount, err := a.transactionUncategorizedAmount(tx)
		if err != nil {
			return assistantTotals{}, err
		}
		switch tx.Type {
		case "Withdrawal":
			totals.Withdrawals += amount
		case "Deposit":
			totals.Deposits += amount
		}
	}
	totals.Net = totals.Deposits - totals.Withdrawals
	return totals, nil
}

func (a *app) transactionUncategorizedAmount(tx transaction) (float64, error) {
	if tx.SplitCount > 0 {
		var total sql.NullFloat64
		err := a.db.QueryRow(`
SELECT SUM(s.SPLITTRANSAMOUNT)
  FROM SPLITTRANSACTIONS_V1 s
 WHERE s.TRANSID = ?
   AND COALESCE(s.CATEGID, 0) <= 0`, tx.ID).Scan(&total)
		if err != nil {
			return 0, err
		}
		if total.Valid {
			return total.Float64, nil
		}
		return 0, nil
	}
	return tx.Amount, nil
}

func (a *app) transactionCategoryAmount(tx transaction, categoryID int, includeChildren bool) (float64, error) {
	if tx.SplitCount > 0 {
		clause := "s.CATEGID = ?"
		args := []any{tx.ID, categoryID}
		if includeChildren {
			clause = `s.CATEGID IN (
				WITH RECURSIVE descendants(id) AS (
					SELECT ?
					UNION ALL
					SELECT c.CATEGID FROM CATEGORY_V1 c JOIN descendants d ON c.PARENTID = d.id
				)
				SELECT id FROM descendants
			)`
		}
		var total sql.NullFloat64
		err := a.db.QueryRow("SELECT SUM(s.SPLITTRANSAMOUNT) FROM SPLITTRANSACTIONS_V1 s WHERE s.TRANSID = ? AND "+clause, args...).Scan(&total)
		if err != nil {
			return 0, err
		}
		if total.Valid {
			return total.Float64, nil
		}
		return 0, nil
	}
	ok, err := a.categoryMatches(tx.ID, categoryID, includeChildren)
	if err != nil || !ok {
		return 0, err
	}
	return tx.Amount, nil
}

func (a *app) categoryMatches(transID, categoryID int, includeChildren bool) (bool, error) {
	clause := "t.CATEGID = ?"
	if includeChildren {
		clause = `t.CATEGID IN (
			WITH RECURSIVE descendants(id) AS (
				SELECT ?
				UNION ALL
				SELECT c.CATEGID FROM CATEGORY_V1 c JOIN descendants d ON c.PARENTID = d.id
			)
			SELECT id FROM descendants
		)`
	}
	var n int
	err := a.db.QueryRow("SELECT COUNT(*) FROM CHECKINGACCOUNT_V1 t WHERE t.TRANSID = ? AND "+clause, transID, categoryID).Scan(&n)
	return n > 0, err
}
